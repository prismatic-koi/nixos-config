package container_test

// credentials_gitlab_test.go — GITLAB_TOKEN resolution and injection.
//
// The GitLab token follows the same shape as the GitHub one: the
// sops-decrypted FILE named by config.json is the primary source, and the
// inherited env var is the fallback, guarded against the unexpanded
// `$(cat …)` literal that home-manager renders into the host environment.
// The one deliberate difference is failure handling — see
// TestResolveGitLabToken_BrokenPathIsNotFatal.
//
// SECURITY: every token value here is synthetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// writeGitLabTokenFile writes contents to a fresh temp file and returns its
// path.
func writeGitLabTokenFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitlab_token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writeGitLabTokenFile: %v", err)
	}
	return path
}

// findGitLabToken invokes m.CredentialEnvVars and returns the value of the
// GITLAB_TOKEN=… entry, and whether such an entry was present at all.
func findGitLabToken(t *testing.T, m *container.Manager) (string, bool) {
	t.Helper()
	vars, err := m.CredentialEnvVars()
	if err != nil {
		t.Fatalf("CredentialEnvVars returned error: %v", err)
	}
	const prefix = "GITLAB_TOKEN="
	for _, v := range vars {
		if strings.HasPrefix(v, prefix) {
			return strings.TrimPrefix(v, prefix), true
		}
	}
	return "", false
}

// TestResolveGitLabToken_FileWinsOverEnv covers the primary path: when
// GitLabTokenPath is configured and readable, the file contents win over the
// inherited env var, and the trailing newline is stripped.
func TestResolveGitLabToken_FileWinsOverEnv(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITLAB_TOKEN", "glpat-env-should-lose")

	path := writeGitLabTokenFile(t, "glpat-from-file\n")
	got := container.ResolveGitLabToken(container.Config{GitLabTokenPath: path})
	if got != "glpat-from-file" {
		t.Errorf("ResolveGitLabToken = %q, want %q (file must win, newline trimmed)", got, "glpat-from-file")
	}
}

// TestResolveGitLabToken_EnvFallback covers a host with no gitlab_token_path
// in config.json: the inherited env var is used.
func TestResolveGitLabToken_EnvFallback(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITLAB_TOKEN", "glpat-from-env")

	if got := container.ResolveGitLabToken(container.Config{}); got != "glpat-from-env" {
		t.Errorf("ResolveGitLabToken = %q, want %q", got, "glpat-from-env")
	}
}

// TestResolveGitLabToken_RejectsShellLiteral is the shell-literal guard: when the
// tmux server was started from a non-shell context, the host GITLAB_TOKEN is
// the literal string `$(cat /path/to/secret)`. Injecting that would make
// every glab call fail with a confusing 401 instead of a clean
// "unauthenticated".
func TestResolveGitLabToken_RejectsShellLiteral(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITLAB_TOKEN", "$(cat /Users/u/.config/sops-nix/secrets/gitlab_token)")

	if got := container.ResolveGitLabToken(container.Config{}); got != "" {
		t.Errorf("ResolveGitLabToken = %q, want \"\" for an unexpanded $( literal", got)
	}
}

// TestResolveGitLabToken_BrokenPathIsNotFatal pins the one deliberate
// difference from the GitHub chain: a configured-but-unreadable GitLab path
// falls through to the env var rather than failing the spawn. GitLab is a
// secondary forge — a broken GitLab secret must not stop a GitHub session
// from starting.
func TestResolveGitLabToken_BrokenPathIsNotFatal(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITLAB_TOKEN", "glpat-from-env")

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := container.ResolveGitLabToken(container.Config{GitLabTokenPath: missing}); got != "glpat-from-env" {
		t.Errorf("ResolveGitLabToken = %q, want the env fallback %q", got, "glpat-from-env")
	}

	// And the spawn itself must still succeed.
	m := container.New(container.Config{GitLabTokenPath: missing})
	if _, err := m.CredentialEnvVars(); err != nil {
		t.Errorf("CredentialEnvVars must not fail on a broken GitLab token path: %v", err)
	}
}

// TestResolveGitLabToken_NoSourceYieldsNothing covers the default host: no
// path configured and no env var set, so nothing is injected.
func TestResolveGitLabToken_NoSourceYieldsNothing(t *testing.T) {
	clearTokenEnv(t)

	if got := container.ResolveGitLabToken(container.Config{}); got != "" {
		t.Errorf("ResolveGitLabToken = %q, want \"\" with no source configured", got)
	}
}

// TestCredentialEnvVars_InjectsGitLabToken is the AC assertion: the resolved
// token reaches the sandbox environment as a non-empty GITLAB_TOKEN.
func TestCredentialEnvVars_InjectsGitLabToken(t *testing.T) {
	clearTokenEnv(t)
	path := writeGitLabTokenFile(t, "glpat-injected\n")

	m := container.New(container.Config{GitLabTokenPath: path})
	tok, ok := findGitLabToken(t, m)
	if !ok {
		t.Fatal("expected GITLAB_TOKEN to be injected from the sops file, got none")
	}
	if tok != "glpat-injected" {
		t.Errorf("got GITLAB_TOKEN=%q, want %q", tok, "glpat-injected")
	}
}

// TestCredentialEnvVars_OmitsGitLabTokenWhenUnavailable verifies that a host
// without GitLab configured gets NO GITLAB_TOKEN entry — not an empty one.
// An empty value would look configured to glab and produce a 401 rather than
// a clean "not authenticated".
func TestCredentialEnvVars_OmitsGitLabTokenWhenUnavailable(t *testing.T) {
	clearTokenEnv(t)

	m := container.New(container.Config{})
	if tok, ok := findGitLabToken(t, m); ok {
		t.Errorf("GITLAB_TOKEN=%q injected with no source configured — want no entry at all", tok)
	}
}

// TestCredentialEnvVars_GitLabTokenDoesNotDisturbGitHub is the
// no-regression assertion for the existing GitHub flow: with both tokens
// configured, each env var carries its own value and exactly one entry.
func TestCredentialEnvVars_GitLabTokenDoesNotDisturbGitHub(t *testing.T) {
	clearTokenEnv(t)
	githubPath := writeTokenFile(t, "ghp_github_value\n")
	gitlabPath := writeGitLabTokenFile(t, "glpat-gitlab-value\n")

	m := container.New(container.Config{
		GitHubTokenPath: githubPath,
		GitLabTokenPath: gitlabPath,
	})
	vars, err := m.CredentialEnvVars()
	if err != nil {
		t.Fatalf("CredentialEnvVars: %v", err)
	}

	counts := map[string]int{}
	values := map[string]string{}
	for _, v := range vars {
		name, value, ok := strings.Cut(v, "=")
		if !ok {
			t.Fatalf("malformed env entry (name redacted): %d bytes", len(v))
		}
		counts[name]++
		values[name] = value
	}
	if counts["GITHUB_TOKEN"] != 1 {
		t.Errorf("GITHUB_TOKEN appears %d times, want exactly 1", counts["GITHUB_TOKEN"])
	}
	if counts["GITLAB_TOKEN"] != 1 {
		t.Errorf("GITLAB_TOKEN appears %d times, want exactly 1", counts["GITLAB_TOKEN"])
	}
	if values["GITHUB_TOKEN"] != "ghp_github_value" {
		t.Errorf("GITHUB_TOKEN = %q, want %q — the GitLab addition changed the GitHub flow",
			values["GITHUB_TOKEN"], "ghp_github_value")
	}
	if values["GITLAB_TOKEN"] != "glpat-gitlab-value" {
		t.Errorf("GITLAB_TOKEN = %q, want %q", values["GITLAB_TOKEN"], "glpat-gitlab-value")
	}
}
