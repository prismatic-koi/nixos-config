//go:build darwin

package integration_test

// sandbox_exec_pi_agent_darwin_test.go — integration tests for PI agent
// credential files in the SBPL profile (issue #1305, refreshed for #2034).
//
// PI stores auth credentials in ~/.pi/agent/auth.json. Since design #2031
// PR3 (#2034), the per-session pi-agent staging dir has been collapsed into
// a single shared mount; PI reads ~/.pi/agent directly inside the sandbox.
// The SBPL profile emits
//
//	(allow file-read* file-write* … (subpath ~/.pi/agent))
//
// for Harness == "pi" sessions, which covers auth.json reads + writes (OAuth
// token refresh write-through) and proper-lockfile auth.json.lock mkdir.
//
// The positive test verifies that sandbox-exec can read auth.json from the
// real ~/.pi/agent path. The negative test removes the subpath allow and
// asserts the read fails, proving the positive is not a no-op.
//
// Each positive test is paired with a negative test per the convention in
// docs/sandbox-exec-testing.md (issue #1192).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// TestSandboxExecPI_AgentDirReadable is the positive integration test for the
// PI agent credential dir. It:
//
//  1. Plants a fake ~/.pi/agent/auth.json with a sentinel.
//  2. Calls EnsurePIAgentConfigDir to confirm the shared host path resolves.
//  3. Verifies the production SBPL profile emits the (subpath ~/.pi/agent) allow.
//  4. Runs `cat ~/.pi/agent/auth.json` inside sandbox-exec and asserts the
//     sentinel content in the output.
func TestSandboxExecPI_AgentDirReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Use a Harness="pi" Manager so generateProfile emits the
	// (subpath ~/.pi/agent) allow.
	m := newProfileManagerWithBareRootAndPi(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	const sentinel = `{"token":"sentinel-1305"}`
	realPiAgentPath := filepath.Join(home, ".pi", "agent")

	// Stash any real ~/.pi/agent so we can restore it.
	stashed := false
	stashPath := filepath.Join(home, ".pi", "agent.prism-test-stash")
	if _, statErr := os.Lstat(realPiAgentPath); statErr == nil {
		if renErr := os.Rename(realPiAgentPath, stashPath); renErr == nil {
			stashed = true
			t.Cleanup(func() {
				_ = os.RemoveAll(realPiAgentPath)
				_ = os.Rename(stashPath, realPiAgentPath)
			})
		}
	}
	if err := os.MkdirAll(realPiAgentPath, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	t.Cleanup(func() {
		if !stashed {
			_ = os.RemoveAll(realPiAgentPath)
		}
	})
	if err := os.WriteFile(filepath.Join(realPiAgentPath, "auth.json"), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}

	// Sanity-check EnsurePIAgentConfigDir returns the real path.
	hostDir, _, ensureErr := container.EnsurePIAgentConfigDir()
	if ensureErr != nil {
		t.Fatalf("EnsurePIAgentConfigDir: %v", ensureErr)
	}
	if hostDir != realPiAgentPath {
		t.Fatalf("EnsurePIAgentConfigDir hostDir = %q, want %q", hostDir, realPiAgentPath)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must contain a (subpath ...) allow that covers ~/.pi/agent.
	if !strings.Contains(prepared.content, realPiAgentPath) {
		t.Fatalf("generated profile does not reference ~/.pi/agent %q.\nProfile:\n%s",
			realPiAgentPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run: sandbox-exec reads auth.json directly from the shared host path.
	authPath := filepath.Join(realPiAgentPath, "auth.json")
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(authPath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read ~/.pi/agent/auth.json failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "sentinel-1305") {
		t.Errorf("read succeeded but output does not contain the sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecPI_AgentDirDeniedWithoutSubpathAllow is the paired negative
// test. It removes the (subpath ~/.pi/agent) allow line from the generated
// profile, then asserts that reading auth.json from the shared dir fails.
// This proves the positive test is not green by accident.
func TestSandboxExecPI_AgentDirDeniedWithoutSubpathAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManagerWithBareRootAndPi(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	realPiAgentPath := filepath.Join(home, ".pi", "agent")

	// Stash and plant a fresh sentinel.
	stashed := false
	stashPath := filepath.Join(home, ".pi", "agent.prism-test-stash-neg")
	if _, statErr := os.Lstat(realPiAgentPath); statErr == nil {
		if renErr := os.Rename(realPiAgentPath, stashPath); renErr == nil {
			stashed = true
			t.Cleanup(func() {
				_ = os.RemoveAll(realPiAgentPath)
				_ = os.Rename(stashPath, realPiAgentPath)
			})
		}
	}
	if err := os.MkdirAll(realPiAgentPath, 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi/agent: %v", err)
	}
	t.Cleanup(func() {
		if !stashed {
			_ = os.RemoveAll(realPiAgentPath)
		}
	})
	authPath := filepath.Join(realPiAgentPath, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"token":"sentinel-1305-neg"}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// Remove the (subpath "~/.pi/agent") line that covers the dir.
		toRemove := "  (subpath " + sbplQuoteForTest(realPiAgentPath) + "))\n"
		return strings.ReplaceAll(p, toRemove, "  (subpath \"/nonexistent-prism-test-deny\"))\n")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(authPath))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read ~/.pi/agent/auth.json succeeded WITHOUT the (subpath ~/.pi/agent) allow.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — pi agent auth.json read correctly denied without ~/.pi/agent subpath allow (exit: %v)", runErr)
	}
}
