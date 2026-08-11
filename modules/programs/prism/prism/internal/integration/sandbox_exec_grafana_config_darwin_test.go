//go:build darwin

package integration_test

// sandbox_exec_grafana_config_darwin_test.go — integration coverage for the
// pi grafana MCP config-bundle secrets.d carve-out (issue #2746).
//
// #2211 denies the whole sops secrets.d subtree and re-allows exactly the
// secret NAMES an in-sandbox consumer reads. #2746 adds one name — the
// grafana config bundle — derived from Config.GrafanaConfigPath, which the
// sandbox-exec spawn path copies off the role-filtered GRAFANA_MCP_CONFIG_PATH
// agent env var. The in-sandbox read matters more here than in the #2668
// gitlab case: the pi grafana extension calls readFileSync on that path from
// inside the sandbox, so the read IS the delivery mechanism, and the deny is
// exactly what kept grafana off Darwin sandbox-exec hosts before this change.
//
// This file proves, with a real /usr/bin/sandbox-exec run, that:
//
//  1. TestSandboxExecGrafanaConfig_ReadableWhenConfigured — the configured
//     grafana bundle is readable in-sandbox, while `github_token` sitting in
//     the SAME generation dir stays denied. The control matters: it shows the
//     carve-out is one name, not a widening of the subtree.
//  2. TestSandboxExecGrafanaConfig_ExceptionIsLoadBearing — the paired
//     negative: strip the grafana require-not exception and the same read
//     fails, so (1) is not green because of some broader rule.
//  3. TestSandboxExecGrafanaConfig_DeniedWhenNotConfigured — a Manager with
//     no GrafanaConfigPath (the default on a host without
//     nx.programs.prism.pi.grafana.enable, and on every review role, whose
//     GRAFANA_MCP_CONFIG_PATH is stripped by #2533) cannot read the same
//     file. Existence of the secret never admits it; only a configured
//     consumer does.
//
// The tests run against a FAKE secrets.d tree under the per-user TMPDIR, so
// they need no real Grafana credential on the host and never read one. The
// production deny regexes are anchored at /var/folders, which is where the
// per-user TMPDIR lives, so the fake tree is covered by the real rules with
// no profile mutation.
//
// Secret hygiene: the fake bundle carries sentinel content only. No test in
// this file reads a real credential.
//
// See docs/sandbox-exec-testing.md for the convention (#1192).

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
	// fakeGrafanaSentinel is the content of the fake grafana config bundle.
	// It is not a credential and matches no credential shape.
	fakeGrafanaSentinel = "prism-2746-grafana-sentinel"

	// grafanaConfigSecretName is the secrets.d-relative name the carve-out is
	// expected to admit — the `home` bundle navi, tui, and m4mac all share.
	grafanaConfigSecretName = "grafana_config_home"
)

// setupFakeGrafanaSecret builds a fake sops layout under the per-user TMPDIR:
//
//	<base>/secrets.d/<counter>/grafana_config_home  (sentinel content)
//	<base>/secrets.d/<counter>/github_token         (denied control)
//	<base>/stable/grafana_config_home               → the counter path
//
// and returns the STABLE symlink path — the shape GRAFANA_MCP_CONFIG_PATH
// carries, and the shape collectSecretsDAllowlistNames resolves through.
// Skips when the TMPDIR is not under /var/folders, where the production deny
// regexes are anchored.
func setupFakeGrafanaSecret(t *testing.T, counter string) (stablePath, deniedPath string) {
	t.Helper()

	base, err := os.MkdirTemp("", "prism-2746-grafana-")
	if err != nil {
		t.Fatalf("MkdirTemp for fake grafana secret: %v", err)
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
	concrete := filepath.Join(counterDir, grafanaConfigSecretName)
	// Same dotenv shape the real bundle uses (see
	// pi/extensions/grafana/UPSTREAM.md), with sentinel values.
	bundle := "GRAFANA_URL=https://grafana.invalid\nGRAFANA_API_KEY=" + fakeGrafanaSentinel + "\n"
	if err := os.WriteFile(concrete, []byte(bundle), 0o600); err != nil {
		t.Fatalf("write fake grafana bundle: %v", err)
	}
	deniedPath = filepath.Join(counterDir, "github_token")
	if err := os.WriteFile(deniedPath, []byte("ghp_fake-2746-denied"), 0o600); err != nil {
		t.Fatalf("write fake github_token control: %v", err)
	}

	stableDir := filepath.Join(canonical, "stable")
	if err := os.MkdirAll(stableDir, 0o700); err != nil {
		t.Fatalf("create fake stable symlink dir: %v", err)
	}
	stablePath = filepath.Join(stableDir, grafanaConfigSecretName)
	if err := os.Symlink(concrete, stablePath); err != nil {
		t.Fatalf("symlink stable grafana bundle: %v", err)
	}
	return stablePath, deniedPath
}

// newGrafanaProfileManager is newProfileManagerWithBareRoot plus a configured
// GrafanaConfigPath. BareRoot is required for the positive read: following
// the stable symlink needs the ancestor block's metadata allow (same reason
// the #2211 stable-chain test uses that variant).
func newGrafanaProfileManager(t *testing.T, grafanaConfigPath string) *container.Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-2746-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })

	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}

	return container.New(container.Config{
		SessionName:       "integ-sandbox-exec-grafana-test",
		InstanceID:        "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:          t.TempDir(),
		BareRoot:          bareRoot,
		GitUserName:       "test-user",
		GitUserEmail:      "test@example.com",
		GrafanaConfigPath: grafanaConfigPath,
	})
}

// TestSandboxExecGrafanaConfig_ReadableWhenConfigured is the positive case: a
// configured, sops-backed grafana bundle is readable in-sandbox — the read
// the pi grafana extension performs — and the github_token in the same
// generation dir is not.
func TestSandboxExecGrafanaConfig_ReadableWhenConfigured(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, deniedPath := setupFakeGrafanaSecret(t, "700")

	m := newGrafanaProfileManager(t, stablePath)
	prepared, _ := preparePositiveProfile(t, m)

	if !strings.Contains(prepared.content, `/secrets\.d/[0-9]+/`+grafanaConfigSecretName+`$`) {
		t.Fatalf("profile carries no grafana exception — the generator did not derive it from GrafanaConfigPath.\nProfile:\n%s", prepared.content)
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Positive: the grafana bundle is readable through its stable symlink.
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(stablePath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("in-sandbox read of the configured grafana bundle failed under the production profile.\nExit: %v\nOutput: %s", runErr, out)
	} else if !strings.Contains(string(out), fakeGrafanaSentinel) {
		t.Errorf("grafana bundle read exited 0 but the sentinel is missing.\nOutput: %s", out)
	} else {
		t.Logf("ka pai — the configured grafana bundle is readable in-sandbox")
	}

	// Control: the neighbouring github_token stays denied. This is what
	// makes the carve-out one name rather than a subtree widening.
	runErr, errOut := sandboxCatDiscard(profilePath, nixBash, deniedPath)
	if runErr == nil {
		t.Errorf("in-sandbox read of github_token SUCCEEDED alongside the grafana carve-out — the exception widened the subtree (issue #2211 AC #2)")
	} else if !strings.Contains(errOut, "Operation not permitted") {
		t.Errorf("github_token read failed but not with EPERM.\nExit: %v\nStderr: %s", runErr, errOut)
	}
}

// TestSandboxExecGrafanaConfig_ExceptionIsLoadBearing is the paired negative:
// with the grafana require-not exception stripped, the read that succeeds
// above must fail. Without this, the positive test could be green because of
// some broader allow.
func TestSandboxExecGrafanaConfig_ExceptionIsLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, _ := setupFakeGrafanaSecret(t, "700")

	m := newGrafanaProfileManager(t, stablePath)

	// Re-target ONLY the grafana exception at a name that matches nothing,
	// leaving every other exception (and the deny itself) intact.
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			`/secrets\.d/[0-9]+/`+grafanaConfigSecretName+`$`,
			`/secrets\.d/[0-9]+/prism-2746-disabled$`)
	})

	runErr, out := sandboxCatDiscard(mutatedPath, nixBash, stablePath)
	if runErr == nil {
		t.Errorf("in-sandbox read of the grafana bundle succeeded WITHOUT its require-not exception — "+
			"the exception is not load-bearing and the positive test is a no-op.\nOutput: %s", out)
	} else {
		t.Logf("ka pai — the grafana bundle is unreadable once its exception is stripped (exit: %v)", runErr)
	}
}

// TestSandboxExecGrafanaConfig_DeniedWhenNotConfigured verifies the default
// posture is unchanged: with no GrafanaConfigPath configured, the same file
// is unreadable in-sandbox. A secret that merely EXISTS on the host is never
// admitted — only a configured in-sandbox consumer admits it. This is also
// the review-role posture, since #2533 strips GRAFANA_MCP_CONFIG_PATH for
// those roles and the field is sourced from that same filtered map.
func TestSandboxExecGrafanaConfig_DeniedWhenNotConfigured(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	stablePath, _ := setupFakeGrafanaSecret(t, "700")

	// Same fixture, but the Manager has no GrafanaConfigPath.
	m := newGrafanaProfileManager(t, "")
	prepared, _ := preparePositiveProfile(t, m)

	if strings.Contains(prepared.content, grafanaConfigSecretName) {
		t.Fatalf("profile mentions %s with no GrafanaConfigPath configured.\nProfile:\n%s", grafanaConfigSecretName, prepared.content)
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)
	runErr, out := sandboxCatDiscard(profilePath, nixBash, stablePath)
	if runErr == nil {
		t.Errorf("in-sandbox read of the grafana bundle succeeded with NO GrafanaConfigPath configured — "+
			"the secrets.d deny regressed.\nOutput: %s", out)
	} else if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("read failed but not with EPERM.\nExit: %v\nStderr: %s", runErr, out)
	} else {
		t.Logf("ka pai — the grafana bundle stays denied on a host that does not configure it")
	}
}
