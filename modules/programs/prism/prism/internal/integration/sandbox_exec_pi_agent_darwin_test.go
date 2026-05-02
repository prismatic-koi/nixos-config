//go:build darwin

package integration_test

// sandbox_exec_pi_agent_darwin_test.go — integration tests for the ~/.pi/agent
// credential directory in the SBPL profile (issue #1305).
//
// PI stores auth credentials in ~/.pi/agent/auth.json. PrepareSandboxExecHome
// creates a symlink at <stagingHome>/.pi/agent → host ~/.pi/agent, and
// collectStagingHomeSymlinkTargets resolves the symlink and emits a
// (subpath "<resolved>") allow rule so PI can read auth.json inside the
// sandbox.
//
// Each positive test is paired with a negative test that removes the covering
// SBPL rule and asserts failure — proving the positive test is not a no-op.
// See docs/sandbox-exec-testing.md for the convention (issue #1192).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostPIAgentDir creates a fake ~/.pi/agent directory directly under the
// user's HOME (not under /private/var/folders, which is broadly allowed by
// the system-paths read rule). Returns the absolute path to the created dir.
// A cleanup is registered to remove it.
//
// We place the dir under HOME deliberately so that the (subpath ...)
// allow emitted by the profile generator is the specific covering rule —
// a path under /private/var/folders would remain readable even without it.
func hostPIAgentDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".prism-1305-pi-agent-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestSandboxExecPI_AgentDirReadable is the positive integration test for the
// ~/.pi/agent credential directory. It:
//
//  1. Creates a fake ~/.pi/agent directory under HOME with a sentinel auth.json.
//  2. Symlinks <stagingHome>/.pi/agent → the fake dir.
//  3. Generates the production SBPL profile and verifies a (subpath ...) allow
//     is emitted for the resolved ~/.pi/agent path.
//  4. Runs `cat <stagingHome>/.pi/agent/auth.json` inside sandbox-exec and
//     asserts exit 0 and the sentinel content in the output.
func TestSandboxExecPI_AgentDirReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// BareRoot variant: symlink traversal under HOME requires the
	// BareRoot-ancestor block's file-read-metadata allow on (subpath HOME).
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Plant a fake ~/.pi/agent directory under HOME with a sentinel auth file.
	piAgentDir := hostPIAgentDir(t)
	const sentinel = `{"token":"sentinel-1305"}`
	authFile := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authFile, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}

	// Create the staging symlink: <stagingHome>/.pi/agent → piAgentDir.
	piStagingDir := filepath.Join(stagingHome, ".pi")
	if err := os.MkdirAll(piStagingDir, 0o700); err != nil {
		t.Fatalf("mkdir stagingHome/.pi: %v", err)
	}
	symlinkPath := filepath.Join(piStagingDir, "agent")
	_ = os.Remove(symlinkPath)
	if err := os.Symlink(piAgentDir, symlinkPath); err != nil {
		t.Fatalf("symlink stagingHome/.pi/agent: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must contain a (subpath ...) allow for the resolved
	// piAgentDir path.
	resolved, err := filepath.EvalSymlinks(piAgentDir)
	if err != nil {
		resolved = piAgentDir
	}
	if !strings.Contains(prepared.content, resolved) {
		t.Fatalf("generated profile does not reference the resolved ~/.pi/agent path %q.\n"+
			"The symlink-target resolver did not pick up the staging .pi/agent symlink.\nProfile:\n%s",
			resolved, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run: sandbox-exec reads auth.json via the staging HOME symlink chain.
	authViaStagingPath := filepath.Join(stagingHome, ".pi", "agent", "auth.json")
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(authViaStagingPath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read ~/.pi/agent/auth.json via staging HOME failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "sentinel-1305") {
		t.Errorf("read succeeded but output does not contain the sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecPI_AgentDirDeniedWithoutSubpathAllow is the paired negative
// test. It removes the (subpath "<resolved>") allow line for the ~/.pi/agent
// directory from the generated profile, then asserts that reading auth.json
// via the staging HOME symlink fails. This proves the positive test is not
// green by accident.
func TestSandboxExecPI_AgentDirDeniedWithoutSubpathAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	piAgentDir := hostPIAgentDir(t)
	authFile := filepath.Join(piAgentDir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"token":"sentinel-1305-neg"}`), 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}

	piStagingDir := filepath.Join(stagingHome, ".pi")
	if err := os.MkdirAll(piStagingDir, 0o700); err != nil {
		t.Fatalf("mkdir stagingHome/.pi: %v", err)
	}
	symlinkPath := filepath.Join(piStagingDir, "agent")
	_ = os.Remove(symlinkPath)
	if err := os.Symlink(piAgentDir, symlinkPath); err != nil {
		t.Fatalf("symlink stagingHome/.pi/agent: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(piAgentDir)
	if err != nil {
		resolved = piAgentDir
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// Remove the (subpath "<resolved>") line that covers the pi agent dir.
		toRemove := "  (subpath " + sbplQuoteForTest(resolved) + ")\n"
		return strings.ReplaceAll(p, toRemove, "")
	})

	authViaStagingPath := filepath.Join(stagingHome, ".pi", "agent", "auth.json")
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(authViaStagingPath))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read ~/.pi/agent/auth.json succeeded WITHOUT the (subpath ...) allow for the resolved target.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — pi agent dir read correctly denied without subpath allow (exit: %v)", runErr)
	}
}
