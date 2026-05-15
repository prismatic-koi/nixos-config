package iris

// credential_broker.go — D-7 credential-brokering subsystem for iris tool
// subprocesses.
//
// The CredentialBroker is the single point that decides which credentials
// reach each tool subprocess. It replaces the inline credential resolution
// that D-5 added to bash_credentials.go: callers obtain credentials by
// asking the broker, and the broker also reports an audit-only list of
// credential names that is persisted with the corresponding tool_call event.
//
// Design notes:
//
//   - The broker is stateless: it reads the host environment and the file
//     system at the moment of each call. This matches the design-doc
//     statement that "credentials are resolved per-call" (§7.2) and means
//     a daemon restart cannot serve stale credentials.
//
//   - The broker is the only place that should read from host env vars or
//     known credential file paths when assembling a tool subprocess. Mount
//     decisions still live in bash_sandbox_{linux,darwin}.go because they
//     are platform-specific, but those files now query the broker to learn
//     which mounts and env vars they should produce.
//
//   - Names are stable, machine-readable audit identifiers — never values.
//     The format is documented in docs/iris-credential-model.md so that
//     `iris stats credentials` output is greppable and operators can build
//     dashboards on top of it.
//
// Credential matrix (initial — D-7):
//
//   read/edit/write/grep/find/ls  → no credentials
//   bash                          → GITHUB_TOKEN (role-scoped or host fallback),
//                                   AWS_* (file-mount based, conditional)
//
// LLM API keys are explicitly excluded from every tool subprocess. The
// broker does not even read them.

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/prismatic-koi/prism/internal/container"
)

// CredentialBroker resolves credentials for tool subprocesses.
//
// A single broker can be reused across calls; it holds no mutable state and
// is safe for concurrent use. Construct one per-daemon with NewCredentialBroker
// or use a per-call zero value.
type CredentialBroker struct{}

// NewCredentialBroker returns a broker. There is currently no configurable
// state, but the constructor exists so future changes can attach cached
// secret backends, refresh policy, etc. without breaking callers.
func NewCredentialBroker() *CredentialBroker { return &CredentialBroker{} }

// BashResolution is the result of resolving credentials for a bash tool call.
type BashResolution struct {
	// Env is the full environment-variable list to pass to the bash
	// subprocess (KEY=VAL pairs). It includes both credentials and the
	// non-credential shell-environment plumbing (PATH, HOME, AWS_CONFIG_FILE,
	// etc.) — the broker owns the whole bash subprocess env in order to be
	// the single source of truth for what reaches the subprocess.
	Env []string

	// Names is the audit list of credentials injected into Env. Names only,
	// never values. Used for the `credentials_injected` field on the
	// tool_call event and surfaced via `iris stats credentials`.
	//
	// Possible values:
	//   "GITHUB_TOKEN"                — role-scoped PRISM_GITHUB_TOKEN_* hit.
	//   "GITHUB_TOKEN(fallback:host)" — role-scoped missing; host GITHUB_TOKEN used.
	//   "AWS_*"                       — AWS config/credentials files were
	//                                   present on the host and AWS_CONFIG_FILE
	//                                   / AWS_SHARED_CREDENTIALS_FILE were set.
	//
	// When neither a role-scoped token nor a host GITHUB_TOKEN is available,
	// "GITHUB_TOKEN" is omitted from Names entirely (the audit log records
	// the absence by omission).
	Names []string
}

// audit-name constants — kept here so the docs and tests reference the same
// strings the broker emits.
const (
	credNameGitHubToken         = "GITHUB_TOKEN"
	credNameGitHubTokenFallback = "GITHUB_TOKEN(fallback:host)"
	credNameAWS                 = "AWS_*"
)

// llmAPIKeyNames is the closed list of LLM/related provider keys that the
// broker explicitly refuses to forward into any tool subprocess. The list
// is exhaustive across providers prism has ever supported, plus speculative
// keys that the threat model wants kept out regardless of consumer status.
//
// Exported (via ForbiddenLLMKeyNames) so tests outside this package can
// assert the same list without copy-pasting.
var llmAPIKeyNames = []string{
	"ANTHROPIC_API_KEY",
	"OPENROUTER_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GITHUB_COPILOT_TOKEN",
	"DEEPSEEK_API_KEY",
}

// ForbiddenLLMKeyNames returns a copy of the forbidden LLM API key list.
// Use this in tests instead of duplicating the list.
func ForbiddenLLMKeyNames() []string {
	out := make([]string, len(llmAPIKeyNames))
	copy(out, llmAPIKeyNames)
	return out
}

// ResolveBash returns the environment and audit names for a bash tool call.
//
// role is the session's agent role ("worker", "coordinator", or any other
// string — empty / unknown roles are treated as "no role" and produce no
// role-scoped token, falling through to the host GITHUB_TOKEN if present).
//
// bareRoot is the bare repo root used to derive the GitHub account; when
// empty, the broker cannot select a role-scoped token and falls through.
func (b *CredentialBroker) ResolveBash(role, bareRoot string) BashResolution {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	res := BashResolution{}

	// ── Standard shell environment ─────────────────────────────────────────
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/run/current-system/sw/bin:/nix/var/nix/profiles/default/bin:/usr/bin:/bin"
	}
	res.Env = append(res.Env, "PATH="+pathVal)

	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if val := os.Getenv(key); val != "" {
			res.Env = append(res.Env, key+"="+val)
		}
	}
	if term := os.Getenv("TERM"); term != "" {
		res.Env = append(res.Env, "TERM="+term)
	}

	// Git identity — forwarded so bash can run git commit.
	for _, key := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if val := os.Getenv(key); val != "" {
			res.Env = append(res.Env, key+"="+val)
		}
	}

	// Nix daemon.
	res.Env = append(res.Env, "NIX_CONFIG=store = daemon")

	// ── GitHub token (4-PAT architecture with host fallback) ──────────────
	//
	// We need to distinguish:
	//   (a) role-scoped PRISM_GITHUB_TOKEN_<account>_<role> present  → "GITHUB_TOKEN"
	//   (b) role-scoped absent, host GITHUB_TOKEN present            → "GITHUB_TOKEN(fallback:host)"
	//   (c) neither present                                          → omit entirely
	//
	// container.GithubTokenForBareRootAndRole returns the resolved token but
	// hides which branch was taken. We re-derive the decision here so the
	// audit log can record it precisely.
	if tok, source := b.resolveGitHubToken(role, bareRoot); tok != "" {
		res.Env = append(res.Env, "GITHUB_TOKEN="+tok)
		switch source {
		case githubTokenSourceRoleScoped:
			res.Names = append(res.Names, credNameGitHubToken)
		case githubTokenSourceHostFallback:
			res.Names = append(res.Names, credNameGitHubTokenFallback)
		}
	}

	// ── AWS credentials (file-mount based) ────────────────────────────────
	//
	// AWS env vars are NOT injected. The bash sandbox mounts ~/.aws/config
	// and ~/.aws/credentials (RO) when they exist on the host, and we point
	// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE at the in-sandbox paths
	// so the AWS CLI resolves credentials from the files.
	//
	// We always set the AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env
	// vars (they are harmless when the files do not exist), but we only
	// claim "AWS_*" in the audit list when at least one of the source files
	// is actually present on the host.
	res.Env = append(res.Env, "AWS_CONFIG_FILE="+home+"/.aws/config")
	res.Env = append(res.Env, "AWS_SHARED_CREDENTIALS_FILE="+home+"/.aws/credentials")
	if awsCredentialsPresent(home) {
		res.Names = append(res.Names, credNameAWS)
	}

	// Stable order for audit comparison/grep.
	sort.Strings(res.Names)

	return res
}

// githubTokenSource indicates which branch of the 4-PAT lookup was taken.
type githubTokenSource int

const (
	githubTokenSourceNone githubTokenSource = iota
	githubTokenSourceRoleScoped
	githubTokenSourceHostFallback
)

// resolveGitHubToken re-derives the 4-PAT decision so the broker can record
// which branch was taken in the audit log. Logic mirrors
// container.GithubTokenForBareRootAndRole; the two are kept in sync but
// cannot share the inner decision because the container helper returns only
// the resolved token string.
//
// Behaviour:
//   - When bareRoot is empty or the account cannot be derived, no role-scoped
//     env var is consulted; the function falls through to host GITHUB_TOKEN.
//   - When role is empty / unknown (not "worker" or "coordinator"), no
//     role-scoped env var is consulted; falls through to host.
//   - When the role-scoped var is set but empty, it counts as unset
//     (`t.Setenv(name, "")` in tests sets the empty string, which we
//     intentionally treat the same as missing).
func (b *CredentialBroker) resolveGitHubToken(role, bareRoot string) (string, githubTokenSource) {
	// Use the container package's free function to get the resolved token
	// (so the broker and the container path share the same selection logic),
	// then re-do the decision here to learn which branch was taken.
	resolved := container.GithubTokenForBareRootAndRole(bareRoot, role)
	if resolved == "" {
		return "", githubTokenSourceNone
	}

	// Was the role-scoped var the source?
	if name := roleScopedTokenName(role, bareRoot); name != "" {
		if v := os.Getenv(name); v != "" {
			return resolved, githubTokenSourceRoleScoped
		}
	}
	// Otherwise the host fallback was the source.
	return resolved, githubTokenSourceHostFallback
}

// roleScopedTokenName returns the PRISM_GITHUB_TOKEN_<account>_<role> env-var
// name for a (role, bareRoot) pair, or "" when no role-scoped name applies.
// Mirrors the lookup-key construction in container/credentials.go.
func roleScopedTokenName(role, bareRoot string) string {
	account := container.GithubAccountFromBareRoot(bareRoot)
	if account == "" {
		return ""
	}
	roleKey := upperASCII(role)
	if roleKey != "WORKER" && roleKey != "COORDINATOR" {
		return ""
	}
	accountKey := replaceASCII(upperASCII(account), '-', '_')
	return "PRISM_GITHUB_TOKEN_" + accountKey + "_" + roleKey
}

// awsCredentialsPresent reports whether either the canonical config or
// credentials file is present on the host. We check the sops-managed
// source paths (used by the bash sandbox mount layout), not the
// in-sandbox destination paths.
func awsCredentialsPresent(home string) bool {
	candidates := []string{
		filepath.Join(home, ".config", "aws", "readonly-config"),
		filepath.Join(home, ".config", "aws", "credentials"),
		filepath.Join(home, ".aws", "config"),
		filepath.Join(home, ".aws", "credentials"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// upperASCII uppercases ASCII letters without involving locale or
// allocating in the typical fast-path.
func upperASCII(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func replaceASCII(s string, from, to byte) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == from {
			out[i] = to
		} else {
			out[i] = s[i]
		}
	}
	return string(out)
}
