//go:build darwin

package integration_test

// sandbox_exec_usage_dir_darwin_test.go — integration coverage for the
// read-only (subpath ...) allow on the prism usage snapshot directory
// ($XDG_STATE_HOME/prism/usage).
//
// The bottom-bar usage segment reads current.json out of that directory
// (pi/extensions/prism.ts::readUsageSnapshot). Without this allow the open
// fails under deny-default and the reader — which degrades silently by
// design — renders nothing, so the feature is invisible in every sandboxed
// session. `prism account usage` from inside a session reads the same
// directory via internal/usage.ReadAll.
//
// Per docs/sandbox-exec-testing.md the coverage is a positive/negative pair
// plus a write-denied negative for the read-only guarantee:
//
//   - TestSandboxExecProfile_UsageSnapshotReadable proves the generated
//     profile permits reading current.json through the grant.
//   - TestSandboxExecProfile_UsageSnapshotDeniedWithoutRule retargets the
//     section-5j subpath at a non-existent sibling and asserts the same read
//     fails with EPERM — proving the positive test is not green by accident
//     via some broader allow.
//   - TestSandboxExecProfile_UsageSnapshotWriteDenied proves the security
//     property: under the PRODUCTION profile a write into the directory
//     fails, so a compromised session cannot forge usage figures on the
//     host.
//   - TestSandboxExecProfile_UsageSnapshotParentNotReadable proves the
//     leaf-only property: prism.db, sitting in the PARENT directory, stays
//     unreadable.
//
// Fixture note. These tests must NOT point at the user's real
// ~/.local/state/prism/usage — they create and delete files, and the real
// directory holds live snapshots. They redirect $XDG_STATE_HOME to a temp
// directory created under the REAL HOME rather than under t.TempDir():
// t.TempDir() lives inside the Darwin per-user TMPDIR, which section 3b of
// the profile already grants read-WRITE for bun's dylib extraction. A
// fixture there would be readable and writable no matter what section 5j
// says, making every assertion below vacuous.
//
// All tests use newProfileManagerWithBareRoot (then override
// $XDG_STATE_HOME): production worker sessions always have BareRoot set, so
// the ancestor metadata block is present. That block grants
// file-test-existence/file-read-metadata only — never file-read-data — so it
// cannot mask the read the positive test performs. This mirrors the
// profiles.json and trusted-settings sibling tests.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

const usageSnapshotPayload = `{"captured_at":"2026-08-03T00:00:00Z","account":"work"}`

// newUsageFixtureManager builds a BareRoot profile manager whose
// $XDG_STATE_HOME is a throwaway directory under the real HOME, and returns
// the manager together with the resolved usage directory. The directory is
// created but left EMPTY — callers plant whatever files they need.
func newUsageFixtureManager(t *testing.T) (m *container.Manager, usageDir string) {
	t.Helper()

	// newProfileManagerWithBareRoot sets $XDG_STATE_HOME to a t.TempDir();
	// override it AFTERWARDS so ours wins. Nothing state-home-derived is
	// captured at construction time — container.New stores only the Config,
	// and both the session work dir and the profile are resolved lazily
	// inside PrepareSandboxExec.
	built := newProfileManagerWithBareRoot(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	stateHome, err := os.MkdirTemp(home, ".prism-2572-state-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for the fixture XDG_STATE_HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	usageDir = filepath.Join(stateHome, "prism", "usage")
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll fixture usage dir: %v", err)
	}
	return built, usageDir
}

// TestSandboxExecProfile_UsageSnapshotReadable is the positive half: under
// the unmodified production profile, reading current.json out of the usage
// directory must succeed.
func TestSandboxExecProfile_UsageSnapshotReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m, usageDir := newUsageFixtureManager(t)
	snapshot := filepath.Join(usageDir, "current.json")
	if err := os.WriteFile(snapshot, []byte(usageSnapshotPayload), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(snapshot))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read of %s failed under the production profile.\n"+
			"The usage-dir allow (issue #2572) is missing or not load-bearing — the bottom-bar\n"+
			"usage segment renders nothing inside every sandbox-exec session.\nExit: %v\nOutput: %s\nProfile: %s",
			snapshot, runErr, string(out), profilePath)
	}
	if !strings.Contains(string(out), `"account":"work"`) {
		t.Errorf("snapshot content not visible inside the sandbox.\nGot: %q\nProfile: %s", string(out), profilePath)
	}
}

// TestSandboxExecProfile_UsageSnapshotDeniedWithoutRule is the paired
// negative test. It re-targets the section-5j (subpath ...) entry at a
// non-existent sibling rather than deleting the line: that keeps the SBPL
// syntactically valid wherever the entry sits, and sandbox-exec silently
// ignores rules for non-existent paths.
func TestSandboxExecProfile_UsageSnapshotDeniedWithoutRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m, usageDir := newUsageFixtureManager(t)
	snapshot := filepath.Join(usageDir, "current.json")
	if err := os.WriteFile(snapshot, []byte(usageSnapshotPayload), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}

	profilePath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			sbplQuoteForTest(usageDir),
			sbplQuoteForTest(usageDir+".prism-2572-disabled"))
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(snapshot))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded WITHOUT the usage-dir allow rule — some broader allow covers\n"+
			"the path and the positive test is not isolating the rule under test.\nOutput: %s\nProfile: %s",
			snapshot, string(out), profilePath)
	}
	if !strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("read of %s without the allow rule did not fail with EPERM.\nExit: %v\nOutput: %s\nProfile: %s",
			snapshot, runErr, string(out), profilePath)
	}
}

// TestSandboxExecProfile_UsageSnapshotWriteDenied is the read-only guarantee
// and its security consequence: under the PRODUCTION profile a write into the
// usage directory must fail. Every legitimate writer goes through the
// sidecar endpoint POST /usage/snapshot, so nothing in-sandbox
// needs write access, and a read-only grant stops a compromised session
// forging usage figures on the host.
func TestSandboxExecProfile_UsageSnapshotWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m, usageDir := newUsageFixtureManager(t)
	snapshot := filepath.Join(usageDir, "current.json")
	if err := os.WriteFile(snapshot, []byte(usageSnapshotPayload), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Overwrite an existing snapshot.
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo forged > "+shQuote(snapshot))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("overwriting %s succeeded under the production profile — the grant must stay read-only.\nOutput: %s\nProfile: %s",
			snapshot, string(out), profilePath)
	}

	// Create a new file in the directory.
	forged := filepath.Join(usageDir, "forged.json")
	cmd = exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo forged > "+shQuote(forged))
	out, runErr = cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("creating %s succeeded under the production profile — the grant must stay read-only.\nOutput: %s\nProfile: %s",
			forged, string(out), profilePath)
	}
	if _, statErr := os.Stat(forged); statErr == nil {
		t.Errorf("%s exists on the host — the sandbox wrote through a read-only grant", forged)
	}

	got, readErr := os.ReadFile(snapshot)
	if readErr != nil {
		t.Fatalf("ReadFile current.json after the write probes: %v", readErr)
	}
	if string(got) != usageSnapshotPayload {
		t.Errorf("current.json changed across the write probes: got %q, want %q", string(got), usageSnapshotPayload)
	}
}

// TestSandboxExecProfile_UsageSnapshotParentNotReadable is the leaf-only
// security property: no path outside the usage directory becomes readable.
//
// The parent $XDG_STATE_HOME/prism holds prism.db (the whole session
// database) and run/ (every session's host-API socket dir, isolated per
// session). A widening of section 5j from the leaf to
// the parent would hand a sandboxed agent all of it, so this test plants a
// sentinel file beside the usage dir and asserts it stays unreadable.
func TestSandboxExecProfile_UsageSnapshotParentNotReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m, usageDir := newUsageFixtureManager(t)
	sibling := filepath.Join(filepath.Dir(usageDir), "prism.db")
	if err := os.WriteFile(sibling, []byte("sentinel-not-for-the-sandbox"), 0o600); err != nil {
		t.Fatalf("WriteFile sentinel prism.db: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(sibling))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded — the usage grant leaked its PARENT directory into the sandbox.\nOutput: %s\nProfile: %s",
			sibling, string(out), profilePath)
	}
	if strings.Contains(string(out), "sentinel-not-for-the-sandbox") {
		t.Errorf("sibling file content leaked into the sandbox: %s", string(out))
	}
}
