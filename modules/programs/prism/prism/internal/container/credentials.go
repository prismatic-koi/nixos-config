package container

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// writeClaudeCredentials extracts Claude Code credentials from the macOS
// Keychain and writes them to a temp file. On Linux, Claude Code stores
// credentials in ~/.claude/.credentials.json which is already inside the
// claudeMount bind-mounted directory and requires no special handling. On
// Darwin, credentials are stored in the Keychain under the service name
// "Claude Code-credentials" and never reach disk, so the container never
// sees them via the directory mount alone.
//
// Sets m.claudeCredentialsReady to true on success so that buildRunArgs can
// add the bind-mount. Logs and returns without error on failure — a missing
// Keychain entry (e.g. not logged in) should surface as an auth error from
// the agent harness rather than a hard container launch failure.
func (m *Manager) writeClaudeCredentials() {
	m.claudeCredentialsReady = false
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password",
		"-l", "Claude Code-credentials", "-w").Output()
	if err != nil {
		log.Printf("container: could not extract Claude credentials from macOS Keychain: %v — pi-anthropic-oauth may fail to authenticate", err)
		return
	}
	creds := strings.TrimSpace(string(out))
	if creds == "" {
		log.Printf("container: macOS Keychain returned empty Claude credentials — run `claude login` to authenticate")
		return
	}
	if err := os.WriteFile(m.claudeCredentialsFilePath(), []byte(creds), 0o600); err != nil {
		log.Printf("container: failed to write Claude credentials temp file: %v", err)
		return
	}
	m.claudeCredentialsReady = true
}

// gitBareRootTimeout is the per-call timeout for git subprocess invocations
// inside githubAccountFromBareRoot. Matches the convention introduced for
// internal/review/context.go (#1888). A bare git dir on a network FS or a
// credential helper waiting on a missing TTY should not block indefinitely.
const gitBareRootTimeout = 5 * time.Second

// githubAccountCache caches the result of githubAccountFromBareRoot keyed on
// bareRoot. The remote URL is stable for the lifetime of the worktree, so the
// cache never needs to be invalidated.
var githubAccountCache sync.Map // map[string]string

// githubAccountFromBareRoot returns the GitHub account (organisation or user)
// for the repo by reading the origin remote URL from the bare git dir.
// Returns "" when the bare root is empty, git is unavailable, or the remote
// URL does not match a github.com URL pattern.
//
// Results are cached per bareRoot so repeated calls (e.g. once per spawn)
// do not re-fork git. The git subprocess is bounded by gitBareRootTimeout
// to prevent an indefinite hang on network-FS repos or misbehaving credential
// helpers.
//
// Supported URL formats:
//
//	git@github.com:<account>/<repo>.git   (SSH)
//	https://github.com/<account>/<repo>   (HTTPS, with or without .git)
func githubAccountFromBareRoot(bareRoot string) string {
	if bareRoot == "" {
		return ""
	}
	// Return cached result if available — the remote URL is stable.
	if cached, ok := githubAccountCache.Load(bareRoot); ok {
		return cached.(string)
	}
	account := githubAccountFromBareRootUncached(bareRoot)
	githubAccountCache.Store(bareRoot, account)
	return account
}

// githubAccountFromBareRootUncached performs the actual git subprocess calls
// without consulting the cache. Exported for testing via export_test.go.
func githubAccountFromBareRootUncached(bareRoot string) string {
	// The bare git dir lives at <bareRoot>/.bare — use --git-dir to run git
	// against it directly without needing to be inside a worktree.
	bareDir := filepath.Join(bareRoot, ".bare")
	ctx, cancel := context.WithTimeout(context.Background(), gitBareRootTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--git-dir", bareDir, "remote", "get-url", "origin").Output()
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("container: git --git-dir %s remote get-url origin timed out after %s", bareDir, gitBareRootTimeout)
			return ""
		}
		// Also try the bareRoot itself in case it IS the raw bare git dir.
		ctx2, cancel2 := context.WithTimeout(context.Background(), gitBareRootTimeout)
		defer cancel2()
		out, err = exec.CommandContext(ctx2, "git", "--git-dir", bareRoot, "remote", "get-url", "origin").Output()
		if err != nil {
			if ctx2.Err() != nil {
				log.Printf("container: git --git-dir %s remote get-url origin timed out after %s", bareRoot, gitBareRootTimeout)
			}
			return ""
		}
	}
	return githubAccountFromURL(strings.TrimSpace(string(out)))
}

// githubAccountFromURL parses a git remote URL and returns the GitHub account
// (the path segment immediately after "github.com"). Returns "" if the URL is
// not a recognisable github.com URL.
func githubAccountFromURL(remoteURL string) string {
	// SSH: git@github.com:<account>/<repo>[.git]
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		rest := strings.TrimPrefix(remoteURL, "git@github.com:")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	// HTTPS: https://github.com/<account>/<repo>[.git]
	//        https://x-access-token:TOKEN@github.com/<account>/<repo>[.git]
	if idx := strings.Index(remoteURL, "github.com/"); idx >= 0 {
		rest := remoteURL[idx+len("github.com/"):]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
}

// CredentialEnvVars is the exported version of credentialEnvVars. Used by
// cmd/agent_run.go to inject credential env vars into the sandbox-exec env.
func (m *Manager) CredentialEnvVars() []string {
	return m.credentialEnvVars()
}

// credentialEnvVars returns the environment variable assignments to inject into
// the container based on the agent role and current host environment.
// Only vars that are set on the host are forwarded — unset vars are skipped.
//
// GitHub token selection (4-PAT architecture):
// The correct token is chosen based on the GitHub account (derived from the
// repo's origin remote URL) and the agent role:
//
//	PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR   — prismatic-koi + coordinator
//	PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER        — prismatic-koi + worker
//	PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR — thankyou-payroll + coordinator
//	PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER      — thankyou-payroll + worker
//
// Falls back to host GITHUB_TOKEN if the specific token is not set
// (supports host-mode, --host-mode spawns, and migration period). When that
// is also empty, falls back to reading the sops secret file at
// m.cfg.GitHubTokenPath directly (rescues the Darwin sops decrypt race that
// freezes an empty GITHUB_TOKEN into the tmux server env, #2029). Precedence:
// PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> > inherited GITHUB_TOKEN > file fallback.
func (m *Manager) credentialEnvVars() []string {
	var vars []string

	// External-tool credentials — forwarded for all agent roles.
	// Covers LLM API keys and other API credentials.
	//
	// Keys intentionally NOT forwarded:
	//   ATLASSIAN_SITE/EMAIL/API_TOKEN — atlassian CLI removed; OAuth PKCE used.
	//   OPENAI_API_KEY    — speculative; no OpenAI provider configured.
	//   GEMINI_API_KEY    — speculative; Google auth uses a Gemini OAuth plugin.
	//   GOOGLE_API_KEY    — speculative; same as GEMINI_API_KEY.
	//   GITHUB_COPILOT_TOKEN — speculative; Copilot provider uses its own auth flow.
	//   DEEPSEEK_API_KEY  — speculative; not populated and no consumer in-repo.
	forwardKeys := []string{
		"ANTHROPIC_API_KEY",
		"OPENROUTER_API_KEY",
	}
	for _, k := range forwardKeys {
		if v := os.Getenv(k); v != "" {
			vars = append(vars, k+"="+v)
		}
	}

	// GitHub token — 4-PAT architecture: account × role → specific token.
	// Derive the account from the repo's git remote URL.
	account := githubAccountFromBareRoot(m.cfg.BareRoot)
	role := m.cfg.AgentRole

	// Build the env var name: PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>
	// where account is uppercased with hyphens replaced by underscores.
	var tokenEnvVar string
	if account != "" {
		accountKey := strings.ToUpper(strings.ReplaceAll(account, "-", "_"))
		roleKey := strings.ToUpper(role)
		if roleKey == "WORKER" || roleKey == "COORDINATOR" {
			tokenEnvVar = "PRISM_GITHUB_TOKEN_" + accountKey + "_" + roleKey
		}
	}

	// Try specific token first, then fall back to host GITHUB_TOKEN.
	if tokenEnvVar != "" {
		if tok := os.Getenv(tokenEnvVar); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
			return vars
		}
	}
	// Fallback: use host GITHUB_TOKEN (supports --host-mode and migration period).
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		vars = append(vars, "GITHUB_TOKEN="+tok)
		return vars
	}

	// Last-resort fallback: read the sops secret file directly. On Darwin a
	// darwin-rebuild switch tears down and re-bootstraps the sops-nix launchd
	// agent, which re-decrypts asynchronously. A shell that sources
	// hm-session-vars.sh during that window freezes GITHUB_TOKEN="" (empty, not
	// unset) into the sticky tmux server env, so the inherited value above is
	// empty. Reading the file directly makes the agent independent of a
	// correctly populated inherited environment (#2029). A missing or empty
	// file is treated as "no token" — we never inject an empty GITHUB_TOKEN=.
	// The contents are added only to the returned env slice; they are never
	// logged.
	if m.cfg.GitHubTokenPath != "" {
		if data, err := os.ReadFile(m.cfg.GitHubTokenPath); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				vars = append(vars, "GITHUB_TOKEN="+tok)
			}
		}
	}

	// Note: GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME, and
	// GIT_COMMITTER_EMAIL are intentionally NOT injected. The container now
	// has a generated .gitconfig with a [user] section (sourced from prism
	// config). Env vars override gitconfig and would mask a broken gitconfig.

	// Note: GIT_DIR and GIT_COMMON_DIR are intentionally NOT injected.
	// Instead, Create() writes a corrected .git pointer file and bind-mounts it
	// over /workspace/.git so all tools — including the agent's internal git
	// library which reads .git directly rather than honouring GIT_DIR — resolve
	// the correct container-internal path (#492).
	// GIT_COMMON_DIR breaks ref lookup in the git version used in the container
	// image and is therefore also omitted.

	return vars
}
