package container

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// gitBareRootTimeout is the per-call timeout for git subprocess invocations
// inside githubAccountFromBareRoot. Matches the convention introduced for
// internal/review/context.go (#1888). A bare git dir on a network FS or a
// credential helper waiting on a missing TTY should not block indefinitely.
const gitBareRootTimeout = 5 * time.Second

// githubAccountCache caches the result of githubAccountFromBareRoot keyed on
// bareRoot. The remote URL is stable for the lifetime of the worktree, so the
// cache never needs to be invalidated.
var githubAccountCache sync.Map // map[string]string

// Names of the host environment variables that carry a credential.
//
// These are declared once because two consumers must agree on them: the
// injection path below, and the test-only argv redaction that keeps their
// VALUES out of a test failure message (redactedArgs in
// argv_redact_test.go, issue #2581). A credential added here is redacted
// there without a second edit.
const (
	// githubTokenEnvKey is the inherited GitHub token — the final env-var
	// fallback in ResolveGitHubToken, and the name credentialEnvVars injects.
	githubTokenEnvKey = "GITHUB_TOKEN"

	// prismGitHubTokenEnvPrefix prefixes the per-(account, role) tokens,
	// PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>.
	prismGitHubTokenEnvPrefix = "PRISM_GITHUB_TOKEN_"
)

// credentialForwardEnvKeys are the external-tool credentials credentialEnvVars
// forwards verbatim from the host environment to every agent role. See
// credentialEnvVars for the keys that are intentionally NOT forwarded.
var credentialForwardEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"OPENROUTER_API_KEY",
}

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

// GitHubAccountFromBareRoot is the exported wrapper around
// githubAccountFromBareRoot. Used by cmd/sidecar.go to resolve the sidecar's
// own GitHub account for env-sanitisation at startup (issue #2348).
func GitHubAccountFromBareRoot(bareRoot string) string {
	return githubAccountFromBareRoot(bareRoot)
}

// GitHubTokenKey returns the <ACCOUNT>_<ROLE> key used to look up the token
// file path in Config.GitHubTokenPaths (and the matching PRISM_GITHUB_TOKEN_*
// env var). Returns "" when the account or role is unresolvable.
//
// account should be the GitHub account name (as returned by githubAccountFromURL
// / githubAccountFromBareRoot). role is the agent role string ("worker" or
// "coordinator" — case-insensitive; any other value yields "").
func GitHubTokenKey(account, role string) string {
	if account == "" {
		return ""
	}
	accountKey := strings.ToUpper(strings.ReplaceAll(account, "-", "_"))
	roleKey := strings.ToUpper(role)
	if roleKey != "WORKER" && roleKey != "COORDINATOR" {
		return ""
	}
	return accountKey + "_" + roleKey
}

// IsShellExpansionLiteral reports whether s appears to be an unexpanded shell
// command-substitution literal (starts with "$(", after trimming ASCII
// whitespace). This is the defence-in-depth guard against the #2348 root cause:
// if the tmux server is started from a non-shell context, PRISM_GITHUB_TOKEN_*
// env vars rendered as "$(cat /run/secrets/…)" propagate through the process
// tree verbatim rather than being expanded. Any such value must NEVER be
// injected as GITHUB_TOKEN — gh would send it to GitHub, get a 401, and
// silently break every operation.
//
// Trimming whitespace catches leading spaces / newlines from misformatted
// config, but does NOT unwrap surrounding quotes: a value like `"$(cat …)"`
// is treated as valid (opaque token) — a shell would strip the quotes before
// exec, so if quotes are still present the substitution definitely was NOT
// expanded and gh would fail on the quotes anyway. That is a caller bug, not
// something this helper needs to catch.
func IsShellExpansionLiteral(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "$(")
}

// readGitHubTokenFile reads the file at path, trims surrounding whitespace,
// and returns the token contents. Returns an error naming path (never
// containing the file's byte contents) when the file is absent, unreadable,
// or empty after trimming.
//
// The whitespace trim mirrors gh's own tolerance for a trailing newline in a
// token file. Path is included in every error so operators can trace which
// sops secret is broken without having to guess.
func readGitHubTokenFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("read %s: file is empty (after whitespace trim)", path)
	}
	return tok, nil
}

// ResolveGitHubToken returns the GitHub token for the (account, role)
// implied by cfg.BareRoot and cfg.AgentRole, using the resolution order:
//
//  1. cfg.GitHubTokenPaths[<ACCOUNT>_<ROLE>] — read the file.  If the KEY IS
//     PRESENT but the file is missing / unreadable / empty, this returns a
//     non-nil error naming the path.  This is the primary path.
//  2. env var PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> — legacy path for hosts that
//     have not yet migrated to the file-paths config.  Rejects values that
//     look like unexpanded shell substitutions (see IsShellExpansionLiteral).
//  3. env var GITHUB_TOKEN — final fallback, same $(-literal guard.
//  4. cfg.GitHubTokenPath (single-token file) — the #2029 Darwin sops-decrypt
//     rescue path.
//
// Returns ("", nil) when NO source is available (no key in map, no env var
// set, no legacy path) — the caller decides whether that is a hard failure.
// Returns ("", err) ONLY when step 1 fired but the file was unreadable —
// this is the "configured but broken" case the AC calls out as a hard fail.
func ResolveGitHubToken(cfg Config) (string, error) {
	account := githubAccountFromBareRoot(cfg.BareRoot)
	key := GitHubTokenKey(account, cfg.AgentRole)

	// 1. File path from GitHubTokenPaths (primary).  A KEY that is present
	//    with a non-empty value is a hard commitment to that path — a
	//    missing / unreadable file is an operator-visible failure, not a
	//    silent fall-through.  See ResolveGitHubToken doc.
	if key != "" && cfg.GitHubTokenPaths != nil {
		if path := cfg.GitHubTokenPaths[key]; path != "" {
			tok, err := readGitHubTokenFile(path)
			if err != nil {
				return "", fmt.Errorf("resolve GitHub token for %s: %w", key, err)
			}
			return tok, nil
		}
	}

	// 2. Legacy per-role env var, guarded against $(-literals.
	if key != "" {
		if tok := os.Getenv(prismGitHubTokenEnvPrefix + key); tok != "" && !IsShellExpansionLiteral(tok) {
			return tok, nil
		}
	}

	// 3. Inherited GITHUB_TOKEN, same guard.
	if tok := os.Getenv(githubTokenEnvKey); tok != "" && !IsShellExpansionLiteral(tok) {
		return tok, nil
	}

	// 4. Legacy single-token file (#2029 Darwin sops-decrypt rescue).
	if cfg.GitHubTokenPath != "" {
		if tok, err := readGitHubTokenFile(cfg.GitHubTokenPath); err == nil {
			return tok, nil
		}
		// A missing file here is NOT a hard error — this is the last-resort
		// rescue path, not a committed primary source.  The lack of a token
		// simply falls through to the "nothing available" result below.
	}

	return "", nil
}

// CredentialEnvVars is the exported version of credentialEnvVars. Used by
// cmd/agent_run_sandbox_exec_darwin.go to inject credential env vars into the
// sandbox-exec env.
func (m *Manager) CredentialEnvVars() ([]string, error) {
	return m.credentialEnvVars()
}

// credentialEnvVars returns the environment variable assignments to inject into
// the container based on the agent role and current host environment.
// Only vars that are set on the host are forwarded — unset vars are skipped.
//
// GitHub token selection (issue #2348, 4-PAT architecture):
// The correct token is chosen based on the GitHub account (derived from the
// repo's origin remote URL) and the agent role. The four supported keys are:
//
//	PRISMATIC_KOI_COORDINATOR    — prismatic-koi + coordinator
//	PRISMATIC_KOI_WORKER         — prismatic-koi + worker
//	THANKYOU_PAYROLL_COORDINATOR — thankyou-payroll + coordinator
//	THANKYOU_PAYROLL_WORKER      — thankyou-payroll + worker
//
// Resolution order (see ResolveGitHubToken):
//
//  1. cfg.GitHubTokenPaths[<KEY>] — read the sops-decrypted file at spawn
//     time.  PRIMARY.  A configured-but-unreadable file is a hard error.
//  2. env var PRISM_GITHUB_TOKEN_<KEY> — legacy path, $(-literal guarded.
//  3. env var GITHUB_TOKEN — final fallback, $(-literal guarded.
//  4. cfg.GitHubTokenPath (single-token file) — #2029 Darwin rescue.
//
// The $(-literal guard is defence in depth against the #2348 root cause: the
// tmux server started from a non-shell context propagates `$(cat …)` env-var
// values verbatim (never expanded), so if we see one we treat it as unset
// rather than sending a literal `$(cat …)` string to gh.
func (m *Manager) credentialEnvVars() ([]string, error) {
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
	for _, k := range credentialForwardEnvKeys {
		if v := os.Getenv(k); v != "" && !IsShellExpansionLiteral(v) {
			vars = append(vars, k+"="+v)
		}
	}

	// GitHub token — file-path first, env-var fallback, with the $(-literal
	// guard applied to both env-var sources.  A configured-but-unreadable
	// file is a hard error (surfaced to the caller so the spawn fails with
	// a diagnostic naming the path — never the value).
	tok, err := ResolveGitHubToken(m.cfg)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		vars = append(vars, githubTokenEnvKey+"="+tok)
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

	return vars, nil
}

// SanitizeGitHubTokenEnv sanitises the current process's environment so that
// downstream subprocesses inherit valid GitHub token values regardless of how
// this process was launched.
//
// This is the fix for the sidecar half of issue #2348.  The sidecar (and any
// prism subcommand that shells out to gh) inherits its env from whatever
// launched it.  Under the boot-restore path the tmux server was launched from
// a systemd user unit — a non-shell context — so `$(cat /run/secrets/…)`
// values propagated verbatim through the process tree.  When the sidecar then
// spawned `prism review` as a subprocess (via os.Environ()), that subprocess
// ran gh against the literal `$(cat …)` string and every call 401'd.
//
// SanitizeGitHubTokenEnv walks paths (keyed by <ACCOUNT>_<ROLE>) and, for
// each readable file, sets PRISM_GITHUB_TOKEN_<KEY> in the current process
// env to the file contents.  It also refreshes the process's own GITHUB_TOKEN
// from the file that matches (account, role) if provided, so gh calls made
// directly by THIS process succeed too.
//
// A file that is missing or unreadable is logged but is NOT fatal at this
// layer — the sidecar must start even if one token file is broken.  Downstream
// resolution via credentialEnvVars / ResolveGitHubToken will surface the
// per-operation failure with a diagnostic naming the path.
//
// account and role are the pair for THIS process's own GITHUB_TOKEN.  Pass
// account="" (or role="") to only sanitise the four PRISM_GITHUB_TOKEN_* vars
// without touching GITHUB_TOKEN.
//
// This function calls os.Setenv, so it is NOT safe to call from tests that
// run in parallel without t.Setenv-style isolation.  Callers must not invoke
// it from an init() or a goroutine — it should be called exactly once,
// synchronously, near process startup.
func SanitizeGitHubTokenEnv(paths map[string]string, account, role string) {
	for key, path := range paths {
		if path == "" {
			continue
		}
		tok, err := readGitHubTokenFile(path)
		if err != nil {
			log.Printf("container: SanitizeGitHubTokenEnv: %s: %v", key, err)
			continue
		}
		if err := os.Setenv(prismGitHubTokenEnvPrefix+key, tok); err != nil {
			log.Printf("container: SanitizeGitHubTokenEnv: setenv %s%s: %v", prismGitHubTokenEnvPrefix, key, err)
		}
	}
	if key := GitHubTokenKey(account, role); key != "" {
		if path, ok := paths[key]; ok && path != "" {
			if tok, err := readGitHubTokenFile(path); err == nil {
				if setErr := os.Setenv(githubTokenEnvKey, tok); setErr != nil {
					log.Printf("container: SanitizeGitHubTokenEnv: setenv GITHUB_TOKEN: %v", setErr)
				}
			}
			// Errors reading the account/role-specific file are already logged
			// by the loop above; do not double-log here.
		}
	} else if inherited := os.Getenv(githubTokenEnvKey); IsShellExpansionLiteral(inherited) {
		// No (account, role) provided but the inherited GITHUB_TOKEN is a
		// broken shell literal — unset it so downstream gh calls fail
		// cleanly (with an "unauthenticated" error naming gh) rather than
		// sending a literal `$(cat …)` string to GitHub.
		_ = os.Unsetenv(githubTokenEnvKey)
	}
}
