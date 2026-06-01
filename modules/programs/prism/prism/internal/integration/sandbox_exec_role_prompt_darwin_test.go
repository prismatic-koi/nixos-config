//go:build darwin

package integration_test

// Integration tests for the prism agent role-prompt directory under
// sandbox-exec (issue #2032). The prism PI extension reads
// ~/.config/prism/agents/<role>.md at before_agent_start and injects it as the
// role system prompt. For that to work inside a sandbox-exec session,
// PrepareSandboxExecHome must symlink ~/.config/prism/agents into the staging
// HOME, and the generated SBPL profile (via collectStagingHomeSymlinkTargets)
// must grant file-read* on the resolved target.
//
// Per the #1192 convention, the change to PrepareSandboxExecHome is paired with
// a positive integration test (the dir is readable through the staging symlink
// under the production profile) and a negative test (mutating the profile to
// drop the read-allow for the resolved target proves the rule is load-bearing).
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

// roledPromptContent is the sentinel written into the test worker.md so the
// positive test can assert the bytes round-trip through the sandbox.
const rolePromptSentinelContent = "PRISM-2032-ROLE-PROMPT-SENTINEL"

// prepareRolePromptSentinel creates ~/.config/prism/agents/worker.md under the
// user's real HOME (NOT t.TempDir, which resolves under /private/var/folders
// and is broadly allowed) and plants a sentinel inside it. Returns the resolved
// (EvalSymlinks) path of the agents directory — that is the path the SBPL
// read-allow targets — and the path of the worker.md file as seen through the
// staging HOME symlink chain.
//
// Skips when the directory cannot be created (e.g. running inside a restricted
// sandbox that denies writes to ~/.config).
func prepareRolePromptSentinel(t *testing.T) (resolvedAgentsDir string) {
	t.Helper()
	home := realUserHome(t)
	agentsDir := filepath.Join(home, ".config", "prism", "agents")

	agentsDirExisted := false
	if _, statErr := os.Stat(agentsDir); statErr == nil {
		agentsDirExisted = true
	}
	if mkErr := os.MkdirAll(agentsDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.config/prism/agents for test: %v", mkErr)
	}

	workerMD := filepath.Join(agentsDir, "worker.md")
	workerMDExisted := false
	if _, statErr := os.Stat(workerMD); statErr == nil {
		workerMDExisted = true
	}
	if wErr := os.WriteFile(workerMD, []byte(rolePromptSentinelContent), 0o600); wErr != nil {
		t.Skipf("cannot plant worker.md sentinel (may be running inside a restricted sandbox): %v", wErr)
	}

	if workerMDExisted {
		// Pre-existing worker.md (e.g. the real deployed prompt) — do not
		// clobber it on cleanup. We cannot restore the original bytes, so the
		// test deliberately skips when worker.md already exists to avoid
		// trashing a developer's deployed prompt.
		t.Skip("~/.config/prism/agents/worker.md already exists; skipping to avoid overwriting the deployed prompt")
	}
	if agentsDirExisted {
		t.Cleanup(func() { _ = os.Remove(workerMD) })
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(home, ".config", "prism")) })
	}

	resolved, evalErr := filepath.EvalSymlinks(agentsDir)
	if evalErr != nil {
		resolved = agentsDir
	}
	return resolved
}

// TestSandboxExecProfile_RolePromptReadable is the positive integration test
// for issue #2032: the prism agent role-prompt dir, symlinked into the staging
// HOME by PrepareSandboxExecHome, is readable through the staging symlink under
// the production profile. This exercises the same path the PI extension uses to
// read <role>.md at before_agent_start.
func TestSandboxExecProfile_RolePromptReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	resolvedAgentsDir := prepareRolePromptSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// The staging HOME must contain the .config/prism/agents symlink.
	agentsLink := filepath.Join(stagingHome, ".config", "prism", "agents")
	if _, lstatErr := os.Lstat(agentsLink); lstatErr != nil {
		t.Fatalf("staging HOME must have a .config/prism/agents symlink after PrepareSandboxExecHome: %v", lstatErr)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The production profile must grant file-read* on the resolved agents dir.
	expectedRule := "(subpath " + sbplQuoteForTest(resolvedAgentsDir) + ")"
	if !strings.Contains(prepared.content, expectedRule) {
		t.Fatalf("generated profile does not contain a read-allow subpath for the resolved agents dir.\n"+
			"Expected to find: %q\nProfile:\n%s", expectedRule, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read worker.md through the staging HOME symlink from inside the sandbox.
	workerMDViaStaging := filepath.Join(agentsLink, "worker.md")
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(workerMDViaStaging))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("reading worker.md through the staging symlink failed under the production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, workerMDViaStaging)
	}
	if !strings.Contains(string(out), rolePromptSentinelContent) {
		t.Errorf("worker.md read did not return the sentinel content.\nGot: %s", string(out))
	}
}

// TestSandboxExecProfile_RolePromptDeniedWithoutAllow is the paired negative
// test for RolePromptReadable. It removes the read-allow (subpath ...) for the
// resolved agents dir from the profile and asserts the same read fails —
// proving the read-allow rule is load-bearing and the positive is not green by
// accident (#1192).
func TestSandboxExecProfile_RolePromptDeniedWithoutAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	resolvedAgentsDir := prepareRolePromptSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	agentsLink := filepath.Join(stagingHome, ".config", "prism", "agents")

	// Remove the (subpath "<resolvedAgentsDir>") read-allow line. The
	// collectStagingHomeSymlinkTargets RO block emits each target on its own
	// indented line: "  (subpath \"<resolved>\")\n". Stripping that single
	// line simulates the pre-#2032 state where the agents dir was not allowed.
	target := "  (subpath " + sbplQuoteForTest(resolvedAgentsDir) + ")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, target, "", 1)
	})

	workerMDViaStaging := filepath.Join(agentsLink, "worker.md")
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(workerMDViaStaging))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("reading worker.md succeeded WITHOUT the read-allow subpath for the agents dir.\n"+
			"The (subpath \"%s\") rule is not load-bearing — investigate.\n"+
			"Output: %s\nMutated profile: %s", resolvedAgentsDir, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — agents dir read correctly denied without the read-allow subpath (exit: %v)", runErr)
	}
}
