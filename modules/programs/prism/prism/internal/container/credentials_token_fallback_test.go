package container_test

// credentials_token_fallback_test.go — unit tests for the GITHUB_TOKEN file
// fallback added in #2029.
//
// On Darwin a darwin-rebuild switch tears down and re-bootstraps the sops-nix
// launchd agent, which re-decrypts the GitHub token asynchronously. A shell
// that sources hm-session-vars.sh during that window freezes GITHUB_TOKEN=""
// (empty, not unset) into the sticky tmux server env. credentialEnvVars now
// reads the sops secret file directly (cfg.GitHubTokenPath) as a last-resort
// fallback so agents get a working token regardless of the inherited env state.
//
// Precedence asserted here:
//   PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> > inherited GITHUB_TOKEN > file fallback
//
// These are pure unit tests: they set env vars with t.Setenv, point the config
// at a temp file with t.TempDir(), and assert the returned env slice. No
// sidecar harness is constructed (per AGENTS.md #1608, the sidecar isolation
// helper is only needed for tests that build a sidecar.Sidecar).

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// clearTokenEnv blanks every env var credentialEnvVars consults so each test
// starts from a known-empty baseline. t.Setenv restores the prior value at
// test end.
func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GITHUB_TOKEN",
		// GITLAB_TOKEN is cleared for the same reason as GITHUB_TOKEN: the
		// developer running the suite has a real one in their environment,
		// and an ambient value would make the "no source configured" cases
		// pass or fail for the wrong reason (issue #2668).
		"GITLAB_TOKEN",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR",
		"PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER",
		"PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR",
		"ANTHROPIC_API_KEY",
		"OPENROUTER_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

// findGitHubToken invokes m.CredentialEnvVars and returns the value of the
// GITHUB_TOKEN=… entry, and whether such an entry was present at all. A
// present-but-empty entry (GITHUB_TOKEN=) returns ("", true) so tests can
// assert it is never produced.
//
// Treats a non-nil err from CredentialEnvVars as a fatal setup failure via
// t.Fatal. Tests that specifically exercise the error path do NOT go through
// this helper — they call m.CredentialEnvVars directly and inspect the error.
func findGitHubToken(t *testing.T, m *container.Manager) (string, bool) {
	t.Helper()
	vars, err := m.CredentialEnvVars()
	if err != nil {
		t.Fatalf("CredentialEnvVars returned error: %v", err)
	}
	for _, v := range vars {
		if len(v) >= len("GITHUB_TOKEN=") && v[:len("GITHUB_TOKEN=")] == "GITHUB_TOKEN=" {
			return v[len("GITHUB_TOKEN="):], true
		}
	}
	return "", false
}

// writeTokenFile writes contents to a fresh temp file and returns its path.
func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github_token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	return path
}

// TestCredentialEnvVars_FileFallback_EmptyEnv covers the primary #2029 case:
// GITHUB_TOKEN is empty in the process env but the sops secret file exists and
// is non-empty, so the file contents (trimmed) are injected.
func TestCredentialEnvVars_FileFallback_EmptyEnv(t *testing.T) {
	clearTokenEnv(t)
	path := writeTokenFile(t, "ghp_fromfile\n")

	m := container.New(container.Config{GitHubTokenPath: path})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected GITHUB_TOKEN to be injected from the file fallback, got none")
	}
	if tok != "ghp_fromfile" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (trailing newline must be stripped)", tok, "ghp_fromfile")
	}
}

// TestCredentialEnvVars_TrailingNewlineStripped asserts the byte-for-byte value
// matches what gh/git expect — a trailing newline (and surrounding whitespace)
// is stripped before injection.
func TestCredentialEnvVars_TrailingNewlineStripped(t *testing.T) {
	clearTokenEnv(t)
	path := writeTokenFile(t, "  ghp_padded  \n")

	m := container.New(container.Config{GitHubTokenPath: path})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected GITHUB_TOKEN to be injected, got none")
	}
	if tok != "ghp_padded" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q", tok, "ghp_padded")
	}
}

// TestCredentialEnvVars_InheritedTokenWins asserts that a non-empty inherited
// GITHUB_TOKEN is used as-is and the file is NOT read (no happy-path change).
func TestCredentialEnvVars_InheritedTokenWins(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromenv")
	// Point the config at a file with a DIFFERENT value; it must be ignored.
	path := writeTokenFile(t, "ghp_fromfile\n")

	m := container.New(container.Config{GitHubTokenPath: path})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected GITHUB_TOKEN to be injected from the inherited env, got none")
	}
	if tok != "ghp_fromenv" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (inherited env must win over the file)", tok, "ghp_fromenv")
	}
}

// TestCredentialEnvVars_RoleSpecificWins asserts the role-specific PAT takes
// precedence over both the inherited GITHUB_TOKEN and the file fallback
// (existing 4-PAT behaviour unchanged). A real bare repo is created so the
// account ("prismatic-koi") is derived from the origin remote URL.
func TestCredentialEnvVars_RoleSpecificWins(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	clearTokenEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromenv")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "ghp_role_specific")
	path := writeTokenFile(t, "ghp_fromfile\n")

	bareRoot := t.TempDir()
	bareDir := filepath.Join(bareRoot, ".bare")
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bareDir, "remote", "add", "origin",
		"git@github.com:prismatic-koi/nixos-config.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
	container.ClearGithubAccountCacheForTest()

	m := container.New(container.Config{
		BareRoot:        bareRoot,
		AgentRole:       "worker",
		GitHubTokenPath: path,
	})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected GITHUB_TOKEN to be injected, got none")
	}
	if tok != "ghp_role_specific" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (role-specific PAT must win)", tok, "ghp_role_specific")
	}
}

// TestCredentialEnvVars_MissingFile_NoInjection covers the negative edge case:
// the inherited GITHUB_TOKEN is empty AND the secret file is missing, so no
// GITHUB_TOKEN= entry is injected at all (an empty var is never produced).
func TestCredentialEnvVars_MissingFile_NoInjection(t *testing.T) {
	clearTokenEnv(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	m := container.New(container.Config{GitHubTokenPath: missing})
	if tok, ok := findGitHubToken(t, m); ok {
		t.Errorf("expected no GITHUB_TOKEN entry for a missing file, got GITHUB_TOKEN=%q", tok)
	}
}

// TestCredentialEnvVars_EmptyFile_NoInjection covers the edge case where the
// file exists but is empty (or whitespace-only): no GITHUB_TOKEN= is injected.
func TestCredentialEnvVars_EmptyFile_NoInjection(t *testing.T) {
	clearTokenEnv(t)
	path := writeTokenFile(t, "\n  \n")

	m := container.New(container.Config{GitHubTokenPath: path})
	if tok, ok := findGitHubToken(t, m); ok {
		t.Errorf("expected no GITHUB_TOKEN entry for an empty/whitespace file, got GITHUB_TOKEN=%q", tok)
	}
}

// TestCredentialEnvVars_NoPath_NoInjection asserts that when no path is
// configured and the env is empty, no GITHUB_TOKEN= is produced (matches the
// pre-#2029 behaviour for non-Darwin / unconfigured hosts).
func TestCredentialEnvVars_NoPath_NoInjection(t *testing.T) {
	clearTokenEnv(t)

	m := container.New(container.Config{})
	if tok, ok := findGitHubToken(t, m); ok {
		t.Errorf("expected no GITHUB_TOKEN entry with no path and empty env, got GITHUB_TOKEN=%q", tok)
	}
}
