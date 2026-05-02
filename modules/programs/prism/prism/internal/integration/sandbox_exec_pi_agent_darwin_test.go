//go:build darwin

package integration_test

// sandbox_exec_pi_agent_darwin_test.go — integration tests for PI agent
// credential files in the SBPL profile (issue #1305).
//
// PI stores auth credentials in ~/.pi/agent/auth.json. StagePIAgentConfigDir
// copies auth.json, settings.json, and themes/ from ~/.pi/agent/ directly into
// the per-session staging directory. The staging directory is already covered
// by the stagingHome (subpath ...) allow rule, so no additional SBPL rule is
// needed for the PI credential files.
//
// The positive test verifies that sandbox-exec can read auth.json from the
// staging directory after StagePIAgentConfigDir copies it there. The negative
// test removes the stagingHome subpath allow and asserts that the read fails,
// proving the positive test is not a no-op.
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

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// TestSandboxExecPI_AgentDirReadable is the positive integration test for
// PI agent credential files. It:
//
//  1. Creates a fake ~/.pi/agent directory under HOME with a sentinel auth.json.
//  2. Calls StagePIAgentConfigDir, which copies auth.json into the staging dir.
//  3. Verifies the production SBPL profile covers the staging dir path.
//  4. Runs `cat <stagingDir>/auth.json` inside sandbox-exec and asserts exit 0
//     and the sentinel content in the output.
func TestSandboxExecPI_AgentDirReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// BareRoot variant so the stagingHome subpath rule is present.
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Plant a fake ~/.pi/agent directory under HOME with a sentinel auth file.
	// We use HOME (not /private/var/folders) so the staging dir is the
	// specific path covered by the (subpath stagingHome) rule rather than
	// being readable via a broad system-paths allow.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	piAgentDir, err := os.MkdirTemp(home, ".prism-1305-pi-agent-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(piAgentDir) })

	const sentinel = `{"token":"sentinel-1305"}`
	if err := os.WriteFile(filepath.Join(piAgentDir, "auth.json"), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}

	// Override HOME so StagePIAgentConfigDir picks up our fake ~/.pi/agent.
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", home)
	_ = origHome

	// StagePIAgentConfigDir resolves ~/.pi/agent from HOME. Point it at our
	// fake dir by temporarily renaming the real one and placing our fake one
	// at the expected location, or — simpler — we create the fake dir at the
	// exact path ~/.pi/agent that StagePIAgentConfigDir reads.
	realPiAgentPath := filepath.Join(home, ".pi", "agent")
	// Stash the real directory if it exists so we can restore it.
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
	// Place our fake dir at ~/.pi/agent.
	if err := os.MkdirAll(filepath.Join(home, ".pi"), 0o700); err != nil {
		t.Fatalf("mkdir ~/.pi: %v", err)
	}
	if err := os.Rename(piAgentDir, realPiAgentPath); err != nil {
		t.Fatalf("rename fake pi agent dir to ~/.pi/agent: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(realPiAgentPath)
		if stashed {
			_ = os.Rename(stashPath, realPiAgentPath)
		}
	})
	// Update piAgentDir to the new location.
	piAgentDir = realPiAgentPath

	// Call the real StagePIAgentConfigDir so auth.json is copied into the
	// staging dir. We use a synthetic session name; XDG_STATE_HOME is not
	// overridden so staging lands under the real state dir.
	piStagingHostDir, _, stageErr := container.StagePIAgentConfigDir(
		config.RoleSlot{}, "integration-test-1305@pi-agent-readable")
	if stageErr != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", stageErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(piStagingHostDir) })

	// auth.json must have been copied into the staging dir.
	stagedAuthPath := filepath.Join(piStagingHostDir, "auth.json")
	if _, statErr := os.Stat(stagedAuthPath); statErr != nil {
		t.Fatalf("auth.json not found in staging dir %q: %v", piStagingHostDir, statErr)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must contain a (subpath ...) allow that covers stagingHome
	// (where auth.json now lives).
	if !strings.Contains(prepared.content, stagingHome) {
		t.Fatalf("generated profile does not reference stagingHome %q.\nProfile:\n%s",
			stagingHome, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run: sandbox-exec reads auth.json from the staging dir.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(stagedAuthPath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read auth.json from staging dir failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "sentinel-1305") {
		t.Errorf("read succeeded but output does not contain the sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecPI_AgentDirDeniedWithoutSubpathAllow is the paired negative
// test. It removes the (subpath "<stagingHome>") allow line from the generated
// profile, then asserts that reading auth.json from the staging dir fails.
// This proves the positive test is not green by accident.
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

	// Create a minimal auth.json directly in a temp staging dir to test with.
	piStagingHostDir, _, stageErr := container.StagePIAgentConfigDir(
		config.RoleSlot{}, "integration-test-1305@pi-agent-denied")
	if stageErr != nil {
		t.Fatalf("StagePIAgentConfigDir: %v", stageErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(piStagingHostDir) })

	// Write a sentinel directly into the staging dir (source ~/.pi/agent may
	// not exist in CI; we only need the file to be present for the read test).
	stagedAuthPath := filepath.Join(piStagingHostDir, "auth.json")
	if err := os.WriteFile(stagedAuthPath, []byte(`{"token":"sentinel-1305-neg"}`), 0o600); err != nil {
		t.Fatalf("write staged auth.json: %v", err)
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// Remove the (subpath "<stagingHome>") line that covers the staging dir.
		toRemove := "  (subpath " + sbplQuoteForTest(stagingHome) + ")\n"
		return strings.ReplaceAll(p, toRemove, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(stagedAuthPath))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read auth.json succeeded WITHOUT the (subpath ...) allow for stagingHome.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — pi agent auth.json read correctly denied without stagingHome subpath allow (exit: %v)", runErr)
	}
}
