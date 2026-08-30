//go:build darwin

package integration_test

// sandbox_exec_gitlab_token_darwin_test.go — integration coverage for the
// gitlab_token secrets.d carve-out.
//
// The secrets.d deny covers the whole sops secrets.d subtree and re-allows
// exactly the secret NAMES an in-sandbox consumer reads. This carve-out adds
// one name, `gitlab_token`, derived from Config.GitLabTokenPath. This file
// proves, with
// a real /usr/bin/sandbox-exec run, that:
//
//  1. TestSandboxExecGitLabToken_ReadableWhenConfigured — the configured
//     GitLab token is readable in-sandbox, while `github_token` sitting in
//     the SAME generation dir stays denied. The control matters: it shows the
//     carve-out is one name, not a widening of the subtree.
//  2. TestSandboxExecGitLabToken_ExceptionIsLoadBearing — the paired
//     negative: strip the gitlab_token require-not exception and the same
//     read fails, so (1) is not green because of some broader rule.
//  3. TestSandboxExecGitLabToken_DeniedWhenNotConfigured — a Manager with no
//     GitLabTokenPath (the default on a host without
//     nx.programs.gitlab-cli.enable) cannot read the same file. Existence of
//     the secret never admits it. Only a configured consumer does.
//
// The tests run against a FAKE secrets.d tree under the per-user TMPDIR, so
// they need no real GitLab secret on the host and never read one. The
// production deny regexes are anchored at /var/folders, which is where the
// per-user TMPDIR lives, so the fake tree is covered by the real rules with
// no profile mutation.
//
// Secret hygiene: the fake secrets carry sentinel content only. No test in
// this file reads a real credential.
//
// See docs/sandbox-exec-testing.md for the convention.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

const (
	// fakeGitLabSentinel is the content of the fake gitlab_token file. It is
	// not a credential and matches no credential shape.
	fakeGitLabSentinel = "prism-2668-gitlab-sentinel"

	// gitlabTokenSecretName is the secrets.d-relative name the carve-out is
	// expected to admit.
	gitlabTokenSecretName = "gitlab_token"
)

// setupFakeGitLabSecret builds a fake sops layout under the per-user TMPDIR:
//
//	<base>/secrets.d/<counter>/gitlab_token   (sentinel content)
//	<base>/secrets.d/<counter>/github_token   (denied control)
//	<base>/stable/gitlab_token                → the counter path (sops symlink)
//
// and returns the STABLE symlink path — the shape config.json carries, and
// the shape collectSecretsDAllowlistNames resolves through. Skips when the
// TMPDIR is not under /var/folders, where the production deny regexes are
// anchored.
func setupFakeGitLabSecret(t *testing.T, counter string) (stablePath, deniedPath string) {
	t.Helper()

	base, err := os.MkdirTemp("", "prism-2668-gitlab-")
	if err != nil {
		t.Fatalf("MkdirTemp for fake gitlab secret: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", base, err)
	}
	if !strings.HasPrefix(canonical, "/private/var/folders/") && !strings.HasPrefix(canonical, "/var/folders/") {
		t.Skipf("TMPDIR-derived base %s is not under /var/folders — the production secrets.d deny regexes would not apply", canonical)
	}

	counterDir := filepath.Join(canonical, "secrets.d", counter)
	if err := os.MkdirAll(counterDir, 0o700); err != nil {
		t.Fatalf("create fake counter dir: %v", err)
	}
	concrete := filepath.Join(counterDir, gitlabTokenSecretName)
	if err := os.WriteFile(concrete, []byte(fakeGitLabSentinel), 0o600); err != nil {
		t.Fatalf("write fake gitlab_token: %v", err)
	}
	deniedPath = filepath.Join(counterDir, "github_token")
	if err := os.WriteFile(deniedPath, []byte("ghp_fake-2668-denied"), 0o600); err != nil {
		t.Fatalf("write fake github_token control: %v", err)
	}

	stableDir := filepath.Join(canonical, "stable")
	if err := os.MkdirAll(stableDir, 0o700); err != nil {
		t.Fatalf("create fake stable symlink dir: %v", err)
	}
	stablePath = filepath.Join(stableDir, gitlabTokenSecretName)
	if err := os.Symlink(concrete, stablePath); err != nil {
		t.Fatalf("symlink stable gitlab_token: %v", err)
	}
	return stablePath, deniedPath
}

// newGitLabProfileManager is newProfileManagerWithBareRoot plus a configured
// GitLabTokenPath. BareRoot is required for the positive read: following the
// stable symlink needs the ancestor block's metadata allow (same reason the
// stable-chain test uses that variant).
func newGitLabProfileManager(t *testing.T, gitlabTokenPath string) *container.Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-2668-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })

	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}

	return container.New(container.Config{
		SessionName:     "integ-sandbox-exec-gitlab-test",
		InstanceID:      "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:        t.TempDir(),
		BareRoot:        bareRoot,
		GitUserName:     "test-user",
		GitUserEmail:    "test@example.com",
		GitLabTokenPath: gitlabTokenPath,
	})
}

// TestSandboxExecGitLabToken_ReadableWhenConfigured is the positive case: a
// configured, sops-backed GitLab token is readable in-sandbox, and the
// github_token in the same generation dir is not.
func TestSandboxExecGitLabToken_ReadableWhenConfigured(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, deniedPath := setupFakeGitLabSecret(t, "700")

	m := newGitLabProfileManager(t, stablePath)
	prepared, _ := preparePositiveProfile(t, m)

	if !strings.Contains(prepared.content, `/secrets\.d/[0-9]+/`+gitlabTokenSecretName+`$`) {
		t.Fatalf("profile carries no gitlab_token exception — the generator did not derive it from GitLabTokenPath.\nProfile:\n%s", prepared.content)
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Positive: the GitLab token is readable through its stable symlink.
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(stablePath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("in-sandbox read of the configured GitLab token failed under the production profile.\nExit: %v\nOutput: %s", runErr, out)
	} else if !strings.Contains(string(out), fakeGitLabSentinel) {
		t.Errorf("GitLab token read exited 0 but the sentinel is missing.\nOutput: %s", out)
	} else {
		t.Logf("ka pai — the configured gitlab_token is readable in-sandbox")
	}

	// Control: the neighbouring github_token stays denied. This is what
	// makes the carve-out one name rather than a subtree widening.
	runErr, errOut := sandboxCatDiscard(profilePath, nixBash, deniedPath)
	if runErr == nil {
		t.Errorf("in-sandbox read of github_token SUCCEEDED alongside the gitlab_token carve-out — the exception widened the subtree (issue #2211 AC #2)")
	} else if !strings.Contains(errOut, "Operation not permitted") {
		t.Errorf("github_token read failed but not with EPERM.\nExit: %v\nStderr: %s", runErr, errOut)
	}
}

// TestSandboxExecGitLabToken_ExceptionIsLoadBearing is the paired negative:
// with the gitlab_token require-not exception stripped, the read that
// succeeds above must fail. Without this, the positive test could be green
// because of some broader allow.
func TestSandboxExecGitLabToken_ExceptionIsLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, _ := setupFakeGitLabSecret(t, "700")

	m := newGitLabProfileManager(t, stablePath)

	// Re-target ONLY the gitlab_token exception at a name that matches
	// nothing, leaving every other exception (and the deny itself) intact.
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			`/secrets\.d/[0-9]+/`+gitlabTokenSecretName+`$`,
			`/secrets\.d/[0-9]+/prism-2668-disabled$`)
	})

	runErr, out := sandboxCatDiscard(mutatedPath, nixBash, stablePath)
	if runErr == nil {
		t.Errorf("in-sandbox read of the GitLab token succeeded WITHOUT its require-not exception — "+
			"the exception is not load-bearing and the positive test is a no-op.\nOutput: %s", out)
	} else {
		t.Logf("ka pai — the GitLab token is unreadable once its exception is stripped (exit: %v)", runErr)
	}
}

// TestSandboxExecGitLabToken_DeniedWhenNotConfigured verifies the default
// posture is unchanged: with no GitLabTokenPath configured, the same file is
// unreadable in-sandbox. A secret that merely EXISTS on the host is never
// admitted — only a configured in-sandbox consumer admits it.
func TestSandboxExecGitLabToken_DeniedWhenNotConfigured(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, _ := setupFakeGitLabSecret(t, "700")

	// Same fixture, but the Manager has no GitLabTokenPath.
	m := newGitLabProfileManager(t, "")
	prepared, _ := preparePositiveProfile(t, m)

	if strings.Contains(prepared.content, gitlabTokenSecretName) {
		t.Fatalf("profile mentions %s with no GitLabTokenPath configured.\nProfile:\n%s", gitlabTokenSecretName, prepared.content)
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)
	runErr, out := sandboxCatDiscard(profilePath, nixBash, stablePath)
	if runErr == nil {
		t.Errorf("in-sandbox read of gitlab_token succeeded with NO GitLabTokenPath configured — "+
			"the secrets.d deny regressed.\nOutput: %s", out)
	} else if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("read failed but not with EPERM.\nExit: %v\nStderr: %s", runErr, out)
	} else {
		t.Logf("ka pai — gitlab_token stays denied on a host that does not configure it")
	}
}
