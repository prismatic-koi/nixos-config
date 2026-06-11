//go:build darwin

package integration_test

// Integration tests for the prism agent role-prompt directory under
// sandbox-exec (issue #2032; real-path grant since #2245, Step 3f of #2132).
// The prism PI extension reads ~/.config/prism/agents/<role>.md at
// before_agent_start and injects it as the role system prompt. The former
// staging-HOME symlink is gone — the capability is the explicit RO
// (subpath ~/.config/prism/agents) grant in the section-5f block emitted by
// generateProfile, evaluated at the REAL host path.
//
// Per the #1192 convention this file carries:
//
//   - a positive (worker.md readable at the real path under the production
//     profile),
//   - a whole-block strip negative (removing the ENTIRE 5f block makes the
//     same read fail — per the #2243 lesson, stripping only the agents
//     (subpath ...) line would leave the clipboard line carrying the block),
//   - a write-denied negative (RO must not silently become RW).
//
// Build tag: darwin (sandbox-exec is Darwin-only).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rolePromptSentinelContent is the sentinel written into the test worker.md
// so the positive test can assert the bytes round-trip through the sandbox.
const rolePromptSentinelContent = "PRISM-2032-ROLE-PROMPT-SENTINEL"

// prepareRolePromptSentinel creates ~/.config/prism/agents/worker.md under
// the user's real HOME (NOT t.TempDir, which resolves under
// /private/var/folders and is broadly allowed) and plants a sentinel inside
// it. Returns the real agents-dir path and the worker.md path.
//
// Skips when the directory cannot be created (e.g. running inside a
// restricted sandbox that denies writes to ~/.config).
func prepareRolePromptSentinel(t *testing.T) (agentsDir, workerMD string) {
	t.Helper()
	home := realUserHome(t)
	agentsDir = filepath.Join(home, ".config", "prism", "agents")

	agentsDirExisted := false
	if _, statErr := os.Stat(agentsDir); statErr == nil {
		agentsDirExisted = true
	}
	if mkErr := os.MkdirAll(agentsDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.config/prism/agents for test: %v", mkErr)
	}

	workerMD = filepath.Join(agentsDir, "worker.md")
	workerMDExisted := false
	if _, statErr := os.Stat(workerMD); statErr == nil {
		workerMDExisted = true
	}
	if workerMDExisted {
		// Pre-existing worker.md (e.g. the real deployed prompt) — do not
		// clobber it. Use a test-specific filename instead; the grant under
		// test is the directory subpath, so any file inside it carries the
		// same signal.
		workerMD = filepath.Join(agentsDir, ".prism-2245-test-role.md")
	}
	if wErr := os.WriteFile(workerMD, []byte(rolePromptSentinelContent), 0o600); wErr != nil {
		t.Skipf("cannot plant role-prompt sentinel (may be running inside a restricted sandbox): %v", wErr)
	}

	if agentsDirExisted {
		mdPath := workerMD
		t.Cleanup(func() { _ = os.Remove(mdPath) })
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(home, ".config", "prism")) })
	}
	return agentsDir, workerMD
}

// TestSandboxExecProfile_RolePromptReadable is the positive integration test:
// the role-prompt markdown is readable at its REAL host path under the
// production profile via the section-5f RO grant — the same path shape the
// PI extension will resolve once Step 5 of #2132 flips $HOME/XDG to the real
// home.
func TestSandboxExecProfile_RolePromptReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, workerMD := prepareRolePromptSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// The staging HOME must NOT contain a .config entry — the agents staging
	// symlink was removed in #2245 (Step 3f).
	if _, lstatErr := os.Lstat(filepath.Join(stagingHome, ".config")); lstatErr == nil {
		t.Fatalf("staging HOME has a .config entry — the agents staging symlink was removed in #2245 and must not be recreated")
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The production profile must carry the whole 5f block.
	block := roGrantBlock5f(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-5f RO block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read the role prompt at the REAL path from inside the sandbox.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(workerMD))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("reading the role prompt at its real path failed under the production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, workerMD)
	}
	if !strings.Contains(string(out), rolePromptSentinelContent) {
		t.Errorf("role-prompt read did not return the sentinel content.\nGot: %s", string(out))
	}
}

// TestSandboxExecProfile_RolePromptDeniedWithoutGrantBlock is the paired
// strip negative: removing the ENTIRE section-5f block makes the same
// real-path read fail — proving the block is load-bearing and the positive
// is not green by accident (#1192; whole-block strip per the #2243 lesson).
func TestSandboxExecProfile_RolePromptDeniedWithoutGrantBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, workerMD := prepareRolePromptSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	block := roGrantBlock5f(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, block, "", 1)
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(workerMD))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("role-prompt read succeeded WITHOUT the section-5f block.\n"+
			"The 5f grant is not load-bearing — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — role-prompt read correctly denied without the 5f block (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_RolePromptWriteDenied is the write-denied negative:
// under the PRODUCTION profile, writing into the real agents dir fails — the
// 5f grant is read-only and must not silently become RW (agents never write
// their own role prompts).
func TestSandboxExecProfile_RolePromptWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	agentsDir, _ := prepareRolePromptSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	writeTarget := filepath.Join(agentsDir, ".prism-2245-write-denied.md")
	t.Cleanup(func() { _ = os.Remove(writeTarget) }) // in case the deny fails
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"echo prism-2245-write > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write into the RO agents dir succeeded under the production profile — RO silently became RW.\n"+
			"Output: %s\nProfile: %s\nTarget: %s", string(out), testProfilePath, writeTarget)
	} else {
		t.Logf("ka pai — agents-dir write correctly denied under the production profile (exit: %v)", runErr)
	}
}
