//go:build darwin

package integration_test

// sandbox_exec_unifi_api_key_darwin_test.go — integration coverage for the
// UniFi API key carve-out in the secrets.d allowlist (issue #3472).
//
// A session of a repo in nx.programs.prism.unifi.repos gets the key as a
// file: UNIFI_API_KEY_FILE holds the stable sops path, and the agent reads
// the file in-sandbox. The carve-out adds one name, unifi_api_key, derived
// from Config.UniFi.APIKeyPath. internal/unifi leaves that field empty for a
// session of any other repo and for a review role.
//
// This file proves, with a real /usr/bin/sandbox-exec run, that:
//
//  1. TestSandboxExecUniFiAPIKey_ReadableWhenDelivered — the key is
//     readable in-sandbox through its stable symlink, while github_token in
//     the same generation dir stays denied.
//  2. TestSandboxExecUniFiAPIKey_ExceptionIsLoadBearing — the paired
//     negative: strip the exception and the same read fails.
//  3. TestSandboxExecUniFiAPIKey_DeniedWithNoDelivery — a Manager with no
//     delivery (another repo, or a review role) cannot read the same file.
//  4. TestSandboxExecUniFiAPIKey_WriteDeleteReplaceDenied — the session
//     cannot write, delete, or replace the key file or its stable symlink.
//  5. TestSandboxExecUniFiAPIKey_WriteWorksWithWriteGrant — the paired
//     negative of (4): with a write grant appended, the same write works.
//
// The tests run against a FAKE secrets.d tree under the per-user TMPDIR. The
// production deny regexes are anchored at /var/folders, where the per-user
// TMPDIR is, so the real rules cover the fake tree with no profile mutation.
//
// Secret hygiene: the fake key is a sentinel. No test in this file reads a
// real credential. A read of the key discards its content in-sandbox, except
// in (1), which checks the sentinel.
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
	"github.com/prismatic-koi/prism/internal/unifi"
)

const (
	// fakeUniFiSentinel is the content of the fake key file. It is not a
	// credential and matches no credential shape.
	fakeUniFiSentinel = "prism-3472-unifi-sentinel"

	uniFiAPIKeySecretName = "unifi_api_key"
)

// setupFakeUniFiSecret builds a fake sops layout under the per-user TMPDIR:
//
//	<base>/secrets.d/<counter>/unifi_api_key  (sentinel content)
//	<base>/secrets.d/<counter>/github_token   (denied control)
//	<base>/stable/unifi_api_key               → the counter path
//
// and returns the base, the concrete path, the stable symlink path (the
// shape UNIFI_API_KEY_FILE holds), and the denied control path. Skips when
// the TMPDIR is not under /var/folders.
func setupFakeUniFiSecret(t *testing.T) (base, concrete, stablePath, deniedPath string) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "prism-3472-unifi-")
	if err != nil {
		t.Fatalf("MkdirTemp for fake unifi secret: %v", err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(tmp, func(p string, _ os.FileInfo, _ error) error {
			_ = os.Chmod(p, 0o700)
			return nil
		})
		_ = os.RemoveAll(tmp)
	})
	base, err = filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", tmp, err)
	}
	if !strings.HasPrefix(base, "/private/var/folders/") && !strings.HasPrefix(base, "/var/folders/") {
		t.Skipf("TMPDIR-derived base %s is not under /var/folders — the production secrets.d deny regexes would not apply", base)
	}
	counterDir := filepath.Join(base, "secrets.d", "800")
	if err := os.MkdirAll(counterDir, 0o700); err != nil {
		t.Fatalf("create fake counter dir: %v", err)
	}
	concrete = filepath.Join(counterDir, uniFiAPIKeySecretName)
	if err := os.WriteFile(concrete, []byte(fakeUniFiSentinel+"\n"), 0o400); err != nil {
		t.Fatalf("write fake unifi key: %v", err)
	}
	deniedPath = filepath.Join(counterDir, "github_token")
	if err := os.WriteFile(deniedPath, []byte("ghp_fake-3472-denied"), 0o600); err != nil {
		t.Fatalf("write fake github_token control: %v", err)
	}
	stableDir := filepath.Join(base, "stable")
	if err := os.MkdirAll(stableDir, 0o700); err != nil {
		t.Fatalf("create fake stable symlink dir: %v", err)
	}
	stablePath = filepath.Join(stableDir, uniFiAPIKeySecretName)
	if err := os.Symlink(concrete, stablePath); err != nil {
		t.Fatalf("symlink stable unifi key: %v", err)
	}
	return base, concrete, stablePath, deniedPath
}

// newUniFiProfileManager is newGrafanaProfileManager with a UniFi delivery in
// place of the grafana path. An empty keyPath gives no delivery.
func newUniFiProfileManager(t *testing.T, keyPath string) *container.Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3472-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}
	var d unifi.Delivery
	if keyPath != "" {
		d = unifi.Delivery{APIKeyPath: keyPath, Host: "10.0.0.1"}
	}
	return container.New(container.Config{
		SessionName:  "integ-sandbox-exec-unifi-test",
		InstanceID:   "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:     t.TempDir(),
		BareRoot:     bareRoot,
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
		UniFi:        d,
	})
}

const uniFiExceptionRegex = `/secrets\.d/[0-9]+/` + uniFiAPIKeySecretName + `$`

func TestSandboxExecUniFiAPIKey_ReadableWhenDelivered(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	_, _, stablePath, deniedPath := setupFakeUniFiSecret(t)

	m := newUniFiProfileManager(t, stablePath)
	prepared, _ := preparePositiveProfile(t, m)
	if !strings.Contains(prepared.content, uniFiExceptionRegex) {
		t.Fatalf("profile carries no unifi_api_key exception — the generator did not derive it from Config.UniFi.\nProfile:\n%s", prepared.content)
	}
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	out, runErr := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", `cat "$0"`, stablePath).CombinedOutput()
	if runErr != nil {
		t.Errorf("in-sandbox read of the key file failed under the production profile.\nExit: %v\nOutput: %s", runErr, out)
	} else if strings.TrimSpace(string(out)) != fakeUniFiSentinel {
		t.Errorf("key read exited 0 but the sentinel is missing.\nOutput: %s", out)
	}

	runErr, errOut := sandboxCatDiscard(profilePath, nixBash, deniedPath)
	if runErr == nil {
		t.Errorf("in-sandbox read of github_token SUCCEEDED next to the unifi carve-out — the exception widened the subtree")
	} else if !strings.Contains(errOut, "Operation not permitted") {
		t.Errorf("github_token read failed but not with EPERM.\nExit: %v\nStderr: %s", runErr, errOut)
	}
}

func TestSandboxExecUniFiAPIKey_ExceptionIsLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	_, _, stablePath, _ := setupFakeUniFiSecret(t)

	m := newUniFiProfileManager(t, stablePath)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, uniFiExceptionRegex, `/secrets\.d/[0-9]+/prism-3472-disabled$`)
	})
	if runErr, out := sandboxCatDiscard(mutatedPath, nixBash, stablePath); runErr == nil {
		t.Errorf("in-sandbox read of the key file succeeded WITHOUT its exception — the positive test is a no-op.\nOutput: %s", out)
	}
}

func TestSandboxExecUniFiAPIKey_DeniedWithNoDelivery(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	_, concrete, stablePath, _ := setupFakeUniFiSecret(t)

	m := newUniFiProfileManager(t, "")
	prepared, _ := preparePositiveProfile(t, m)
	if strings.Contains(prepared.content, uniFiAPIKeySecretName) {
		t.Fatalf("profile mentions %s with no delivery.\nProfile:\n%s", uniFiAPIKeySecretName, prepared.content)
	}
	profilePath := writeAugmentedPositiveProfile(t, prepared)
	for _, p := range []string{stablePath, concrete} {
		runErr, out := sandboxCatDiscard(profilePath, nixBash, p)
		if runErr == nil {
			t.Errorf("in-sandbox read of %s succeeded with no delivery — the secrets.d deny regressed", p)
		} else if !strings.Contains(out, "Operation not permitted") {
			t.Errorf("read of %s failed but not with EPERM.\nExit: %v\nStderr: %s", p, runErr, out)
		}
	}
}

// uniFiWriteScripts are the writes that a session must not do to the key
// file. Each script fails as a whole when one step fails.
func uniFiWriteScripts(base, concrete, stablePath string) map[string]string {
	forged := filepath.Join(base, "forged")
	return map[string]string{
		"write the file":             "echo forged > " + shQuote(concrete),
		"delete the file":            "rm -f " + shQuote(concrete),
		"rename the file away":       "mv " + shQuote(concrete) + " " + shQuote(forged),
		"replace the stable symlink": "ln -sfn " + shQuote(forged) + " " + shQuote(stablePath),
		"delete the stable symlink":  "rm -f " + shQuote(stablePath),
	}
}

func TestSandboxExecUniFiAPIKey_WriteDeleteReplaceDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	base, concrete, stablePath, _ := setupFakeUniFiSecret(t)
	if err := os.Chmod(concrete, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newUniFiProfileManager(t, stablePath)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	for name, script := range uniFiWriteScripts(base, concrete, stablePath) {
		out, runErr := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", script).CombinedOutput()
		if runErr == nil {
			t.Errorf("%s: succeeded in-sandbox under the production profile.\nOutput: %s", name, out)
		}
	}
	if data, err := os.ReadFile(concrete); err != nil || string(data) != fakeUniFiSentinel+"\n" {
		t.Errorf("the key file on the host changed (err=%v)", err)
	}
	if target, err := os.Readlink(stablePath); err != nil || target != concrete {
		t.Errorf("the stable symlink on the host changed: %q %v", target, err)
	}
}

// The paired negative of the write test. An appended write grant on the
// fake tree wins over every earlier rule, so the same writes work. Thus a
// denied write above is the effect of the production profile, and the
// harness can see a write that succeeds.
func TestSandboxExecUniFiAPIKey_WriteWorksWithWriteGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	base, concrete, stablePath, _ := setupFakeUniFiSecret(t)
	if err := os.Chmod(concrete, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newUniFiProfileManager(t, stablePath)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return p + "\n(allow file-read* file-write* (subpath " + sbplQuoteForTest(base) + "))\n"
	})
	script := uniFiWriteScripts(base, concrete, stablePath)["write the file"]
	if out, runErr := exec.Command(sandboxExecPath, "-f", mutatedPath, nixBash, "-c", script).CombinedOutput(); runErr != nil {
		t.Errorf("the write failed with a write grant, so the write test proves nothing.\nExit: %v\nOutput: %s", runErr, out)
	}
}
