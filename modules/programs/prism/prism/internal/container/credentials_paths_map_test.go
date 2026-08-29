package container_test

// credentials_paths_map_test.go — tests for the file-paths-map credential
// resolution.
//
// The primary source of truth for GitHub token resolution is now
// cfg.GitHubTokenPaths[<ACCOUNT>_<ROLE>] — an absolute path to a sops-decrypted
// file, read at spawn time. The env-var chain (PRISM_GITHUB_TOKEN_<KEY> then
// GITHUB_TOKEN) is retained as a fallback for legacy / host-mode paths, but
// with a hard guard against unexpanded shell command-substitution literals
// (`$(cat …)`), which was the shape that broke every session under the
// boot-restore path.
//
// Test conventions per repo AGENTS.md: t.Setenv for env-var isolation,
// t.TempDir() for token files, exec.Command("git") for real bare-repo setup.
// No sidecar harness is constructed — these are pure Manager-level unit tests.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// setupBareRepo creates a bare git repo under bareRoot/.bare with the given
// origin remote URL and returns bareRoot. Fails the test on any setup error.
func setupBareRepo(t *testing.T, remoteURL string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	bareRoot := t.TempDir()
	bareDir := filepath.Join(bareRoot, ".bare")
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bareDir, "remote", "add", "origin", remoteURL).CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
	container.ClearGithubAccountCacheForTest()
	return bareRoot
}

// TestCredentialEnvVars_PathsMap_PrimaryWins verifies that when a file path
// is configured for the (account, role) pair AND the file is readable, the
// file contents win over BOTH the role-specific PRISM_GITHUB_TOKEN env var
// AND the inherited GITHUB_TOKEN env var. This is the primary happy path.
func TestCredentialEnvVars_PathsMap_PrimaryWins(t *testing.T) {
	clearTokenEnv(t)
	// Even a valid-looking env var must be OVERRIDDEN by the file.
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "ghp_env_should_lose")
	t.Setenv("GITHUB_TOKEN", "ghp_fallback_should_lose")

	filePath := writeTokenFile(t, "ghp_from_file\n")
	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")

	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
		GitHubTokenPaths: map[string]string{
			"PRISMATIC_KOI_WORKER": filePath,
		},
	})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected GITHUB_TOKEN to be injected from the file, got none")
	}
	if tok != "ghp_from_file" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (file must win over env)", tok, "ghp_from_file")
	}
}

// TestCredentialEnvVars_PathsMap_RoleSelection covers the four (account, role)
// combinations independently and asserts each picks the right file. This is
// the analogue of the existing TestCredentialEnvVars_AccountRoleTokenSelection
// but for the file-paths map instead of the env-var chain.
func TestCredentialEnvVars_PathsMap_RoleSelection(t *testing.T) {
	cases := []struct {
		name      string
		remoteURL string
		role      string
		key       string
	}{
		{"prismatic-koi worker", "git@github.com:prismatic-koi/nixos-config.git", "worker", "PRISMATIC_KOI_WORKER"},
		{"prismatic-koi coordinator", "git@github.com:prismatic-koi/nixos-config.git", "coordinator", "PRISMATIC_KOI_COORDINATOR"},
		{"thankyou-payroll worker", "https://github.com/thankyou-payroll/some-repo.git", "worker", "THANKYOU_PAYROLL_WORKER"},
		{"thankyou-payroll coordinator", "https://github.com/thankyou-payroll/some-repo.git", "coordinator", "THANKYOU_PAYROLL_COORDINATOR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTokenEnv(t)
			// Populate ALL four keys so we can prove the RIGHT one is picked.
			paths := map[string]string{
				"PRISMATIC_KOI_WORKER":         writeTokenFile(t, "tok_pk_worker"),
				"PRISMATIC_KOI_COORDINATOR":    writeTokenFile(t, "tok_pk_coord"),
				"THANKYOU_PAYROLL_WORKER":      writeTokenFile(t, "tok_typ_worker"),
				"THANKYOU_PAYROLL_COORDINATOR": writeTokenFile(t, "tok_typ_coord"),
			}
			wantByKey := map[string]string{
				"PRISMATIC_KOI_WORKER":         "tok_pk_worker",
				"PRISMATIC_KOI_COORDINATOR":    "tok_pk_coord",
				"THANKYOU_PAYROLL_WORKER":      "tok_typ_worker",
				"THANKYOU_PAYROLL_COORDINATOR": "tok_typ_coord",
			}
			bareRoot := setupBareRepo(t, tc.remoteURL)

			m := container.New(container.Config{
				BareRoot:         bareRoot,
				AgentRole:        tc.role,
				GitHubTokenPaths: paths,
			})
			tok, ok := findGitHubToken(t, m)
			if !ok {
				t.Fatal("expected GITHUB_TOKEN to be injected, got none")
			}
			if want := wantByKey[tc.key]; tok != want {
				t.Errorf("got GITHUB_TOKEN=%q, want %q (key %s)", tok, want, tc.key)
			}
		})
	}
}

// TestCredentialEnvVars_PathsMap_MissingFileIsHardError is the load-bearing
// AC assertion: when the KEY IS PRESENT in GitHubTokenPaths but the file at
// that path is missing / unreadable, credentialEnvVars must return an error
// (not silently fall through to an env-var fallback), and the error must name
// the path (never the value). This is the "spawn fails loudly" contract.
func TestCredentialEnvVars_PathsMap_MissingFileIsHardError(t *testing.T) {
	clearTokenEnv(t)
	// Populate the env-var fallbacks so we can prove they are NOT consulted
	// when the file path is committed but broken.
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "ghp_env_would_have_worked")
	t.Setenv("GITHUB_TOKEN", "ghp_inherited_would_have_worked")

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")

	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
		GitHubTokenPaths: map[string]string{
			"PRISMATIC_KOI_WORKER": missing,
		},
	})
	vars, err := m.CredentialEnvVars()
	if err == nil {
		t.Fatalf("expected error for missing configured token file, got nil (vars=%v)", container.RedactedArgsForTest(vars))
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error must name the path %q, got: %v", missing, err)
	}
	if strings.Contains(err.Error(), "ghp_env_would_have_worked") ||
		strings.Contains(err.Error(), "ghp_inherited_would_have_worked") {
		t.Errorf("error must NOT name any token value, got: %v", err)
	}
}

// TestCredentialEnvVars_PathsMap_EmptyFileIsHardError covers the boundary
// between "file exists, is empty" and "file is missing" — the former is
// treated as a hard error too, since an empty token is never a valid GitHub
// credential and silently falling through would hide the misconfiguration.
func TestCredentialEnvVars_PathsMap_EmptyFileIsHardError(t *testing.T) {
	clearTokenEnv(t)
	emptyFile := writeTokenFile(t, "   \n\n")
	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")

	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
		GitHubTokenPaths: map[string]string{
			"PRISMATIC_KOI_WORKER": emptyFile,
		},
	})
	_, err := m.CredentialEnvVars()
	if err == nil {
		t.Fatal("expected error for empty configured token file, got nil")
	}
	if !strings.Contains(err.Error(), emptyFile) {
		t.Errorf("error must name the path %q, got: %v", emptyFile, err)
	}
}

// TestCredentialEnvVars_PathsMap_MissingKeyFallsThrough covers the case where
// the map is populated but does NOT contain the (account, role) key for this
// spawn: the resolver falls through to the env-var chain (unchanged legacy
// behaviour, so the migration is safe).
func TestCredentialEnvVars_PathsMap_MissingKeyFallsThrough(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "ghp_env_wins")

	// Map is populated but only for a DIFFERENT role.
	otherPath := writeTokenFile(t, "tok_for_coord_not_worker")
	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")

	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
		GitHubTokenPaths: map[string]string{
			"PRISMATIC_KOI_COORDINATOR": otherPath,
		},
	})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected env-var fallback, got none")
	}
	if tok != "ghp_env_wins" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (env-var fallback for absent key)", tok, "ghp_env_wins")
	}
}

// TestCredentialEnvVars_ShellLiteralRejected_RoleSpecific asserts the
// $(-literal guard on the role-specific env var. This is the DEFENCE IN DEPTH:
// even if some future path leaves a broken `$(cat …)` string
// in PRISM_GITHUB_TOKEN_*, it must never be forwarded to gh.
func TestCredentialEnvVars_ShellLiteralRejected_RoleSpecific(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
		"$(cat /run/secrets/github_token_prismatic_koi_worker)")
	t.Setenv("GITHUB_TOKEN", "ghp_valid_fallback")

	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")
	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
	})
	tok, ok := findGitHubToken(t, m)
	if !ok {
		t.Fatal("expected fallback GITHUB_TOKEN when role-specific var is a shell literal, got none")
	}
	if tok != "ghp_valid_fallback" {
		t.Errorf("got GITHUB_TOKEN=%q, want %q (shell-literal env must be ignored)",
			tok, "ghp_valid_fallback")
	}
}

// TestCredentialEnvVars_ShellLiteralRejected_GitHubToken asserts the same
// $(-literal guard on the inherited GITHUB_TOKEN. When BOTH the role-specific
// and inherited vars are broken, no GITHUB_TOKEN= should be injected at all
// — better a missing token (which gh reports as "unauthenticated") than a
// literal `$(cat …)` string sent to GitHub as an API bearer token.
func TestCredentialEnvVars_ShellLiteralRejected_GitHubToken(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
		"$(cat /run/secrets/github_token_prismatic_koi_worker)")
	t.Setenv("GITHUB_TOKEN", "$(cat /run/secrets/github_token)")

	bareRoot := setupBareRepo(t, "git@github.com:prismatic-koi/nixos-config.git")
	m := container.New(container.Config{
		BareRoot:  bareRoot,
		AgentRole: "worker",
	})
	vars, err := m.CredentialEnvVars()
	if err != nil {
		t.Fatalf("credentialEnvVars: %v", err)
	}
	for _, kv := range vars {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Errorf("shell-literal GITHUB_TOKEN must NOT be injected; got %q", kv)
		}
	}
}

// TestIsShellExpansionLiteral covers the guard predicate directly. Each case
// documents a real-world value shape the guard has to classify correctly.
func TestIsShellExpansionLiteral(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"real token (gh classic)", "ghp_abcdef1234567890", false},
		{"real token (fine-grained)", "github_pat_11ABCDEFG_xxx", false},
		{"literal cat substitution", "$(cat /run/secrets/github_token)", true},
		{"literal cat substitution with leading space", " $(cat /run/secrets/x)", true},
		{"literal cat substitution with leading newline", "\n$(cat /run/secrets/x)", true},
		{"other command substitution", "$(gh auth token)", true},
		{"looks like a token but starts with $", "$secret", false},
		{"backtick substitution — NOT covered", "`cat /run/secrets/x`", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := container.IsShellExpansionLiteral(tc.in); got != tc.want {
				t.Errorf("IsShellExpansionLiteral(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestGitHubTokenKey covers the (account, role) → key derivation and the
// role-value validation that gates it.
func TestGitHubTokenKey(t *testing.T) {
	cases := []struct {
		name    string
		account string
		role    string
		want    string
	}{
		{"prismatic-koi worker", "prismatic-koi", "worker", "PRISMATIC_KOI_WORKER"},
		{"prismatic-koi coordinator (Coordinator)", "prismatic-koi", "Coordinator", "PRISMATIC_KOI_COORDINATOR"},
		{"thankyou-payroll worker (hyphenated account)", "thankyou-payroll", "worker", "THANKYOU_PAYROLL_WORKER"},
		{"empty account", "", "worker", ""},
		{"unknown role", "prismatic-koi", "reviewer", ""},
		{"empty role", "prismatic-koi", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := container.GitHubTokenKey(tc.account, tc.role); got != tc.want {
				t.Errorf("GitHubTokenKey(%q, %q) = %q, want %q", tc.account, tc.role, got, tc.want)
			}
		})
	}
}

// TestSanitizeGitHubTokenEnv_PopulatesFromFiles is the load-bearing test for
// the sidecar half of the fix: SanitizeGitHubTokenEnv reads each
// configured file and writes the token value into the corresponding
// PRISM_GITHUB_TOKEN_* env var, so that subprocesses spawned via os.Environ()
// see valid values regardless of what was inherited.
func TestSanitizeGitHubTokenEnv_PopulatesFromFiles(t *testing.T) {
	// Start from the broken shape observed on the live host.
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
		"$(cat /run/secrets/github_token_prismatic_koi_worker)")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR",
		"$(cat /run/secrets/github_token_prismatic_koi_coordinator)")
	t.Setenv("GITHUB_TOKEN",
		"$(cat /run/secrets/github_token_prismatic_koi_worker)")

	workerPath := writeTokenFile(t, "ghp_worker_from_file")
	coordPath := writeTokenFile(t, "ghp_coord_from_file")
	paths := map[string]string{
		"PRISMATIC_KOI_WORKER":      workerPath,
		"PRISMATIC_KOI_COORDINATOR": coordPath,
	}

	container.SanitizeGitHubTokenEnv(paths, "prismatic-koi", "worker")

	// The four PRISM_GITHUB_TOKEN_* vars should now hold real file contents,
	// NOT the `$(cat …)` literals we started with.
	if got := os.Getenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER"); got != "ghp_worker_from_file" {
		t.Errorf("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER = %q, want %q", got, "ghp_worker_from_file")
	}
	if got := os.Getenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR"); got != "ghp_coord_from_file" {
		t.Errorf("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR = %q, want %q", got, "ghp_coord_from_file")
	}
	// GITHUB_TOKEN should be refreshed from the worker file (matching the
	// account/role pair passed in).
	if got := os.Getenv("GITHUB_TOKEN"); got != "ghp_worker_from_file" {
		t.Errorf("GITHUB_TOKEN = %q, want %q (must be refreshed from the account/role file)",
			got, "ghp_worker_from_file")
	}
}

// TestSanitizeGitHubTokenEnv_UnsetsBrokenGitHubTokenWhenNoAccount covers the
// case where the caller has no (account, role) pair to key off (empty
// bareRoot) but the inherited GITHUB_TOKEN is a shell literal. The sanitiser
// must UNSET the broken var so gh emits a clean "unauthenticated" error
// rather than sending `$(cat …)` to GitHub.
func TestSanitizeGitHubTokenEnv_UnsetsBrokenGitHubTokenWhenNoAccount(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "$(cat /run/secrets/github_token)")

	container.SanitizeGitHubTokenEnv(nil, "", "")

	if got := os.Getenv("GITHUB_TOKEN"); got != "" {
		t.Errorf("GITHUB_TOKEN should be unset when broken and no (account, role) provided; got %q", got)
	}
}

// TestSanitizeGitHubTokenEnv_MissingFileIsNonFatal covers the case where a
// configured file is missing at sidecar startup: the sanitiser logs and moves
// on. The other files are still populated. Downstream per-operation
// resolution (credentialEnvVars) will surface the per-operation failure.
func TestSanitizeGitHubTokenEnv_MissingFileIsNonFatal(t *testing.T) {
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR", "")

	goodPath := writeTokenFile(t, "ghp_good")
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")
	paths := map[string]string{
		"PRISMATIC_KOI_WORKER":      goodPath,
		"PRISMATIC_KOI_COORDINATOR": missingPath,
	}

	// Must not panic; must not fail; must populate the readable ones.
	container.SanitizeGitHubTokenEnv(paths, "", "")

	if got := os.Getenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER"); got != "ghp_good" {
		t.Errorf("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER = %q, want %q", got, "ghp_good")
	}
	// The missing-file case must not have written anything to the env — the
	// existing empty value we set via t.Setenv above must still be empty.
	if got := os.Getenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR"); got != "" {
		t.Errorf("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR should be empty for a missing file; got %q", got)
	}
}
