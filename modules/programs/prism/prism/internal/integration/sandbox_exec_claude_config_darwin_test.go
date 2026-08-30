//go:build darwin

package integration_test

// sandbox_exec_claude_config_darwin_test.go — integration coverage for the
// claude config XDG relocation.
//
// The staging-HOME .claude write-through symlink is gone. claude-code
// resolves its config dir (and .claude.json) via the CLAUDE_CONFIG_DIR env
// var at the host XDG path ~/.config/claude. Unlike the aws/kube XDG configs
// (Steps 3a/3b), ~/.config/claude is a plain host directory — NOT a sops
// symlink — so the secrets.d allowlist plays no part here: the
// in-sandbox read/write capability is the explicit RW
// (subpath ~/.config/claude) grant emitted by generateProfile.
//
// This file tests:
//
//  1. Positive: a process inside sandbox-exec under the production profile
//     can create, read back, and remove a file under ~/.config/claude — a
//     real RW round-trip at the host XDG path.
//
//  2. Negative: stripping the (subpath ~/.config/claude) RW allow block
//     makes the same write fail — proving the explicit grant is
//     load-bearing (sandbox-exec testing convention). Because the path is
//     not sops-backed there is no allowlist fallback to mask the
//     regression.
//
// Capability-probe gating applies via requireSandboxExec. Shared
// helpers live in sandbox_exec_helpers_darwin_test.go.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeXDGDirForTest returns the host XDG claude config dir
// (~/.config/claude). When the dir does not exist it is created and a
// cleanup is registered to remove it again (only when this test created
// it) — the positive test needs a real directory to write into, and the
// production grant is on exactly this path.
func claudeXDGDirForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	dir := filepath.Join(home, ".config", "claude")
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			t.Fatalf("create %s: %v", dir, mkErr)
		}
		t.Cleanup(func() { _ = os.Remove(dir) }) // os.Remove: only if empty
	}
	return dir
}

// claudeRoundTripScript builds a bash script that writes a sentinel file
// under dir, reads it back, and removes it. Each step must succeed for the
// script to exit 0.
func claudeRoundTripScript(target string) string {
	q := shQuote(target)
	return "echo prism-2243-probe > " + q + " && cat " + q + " && rm " + q
}

// claudeAllowBlock returns the exact RW allow block generateProfile emits
// for the claude config dir. Mirrors the generator's emission format. The
// negative test removes the whole block. Removing only the (subpath ...)
// line leaves a filter-less (allow ...) clause, which SBPL treats as
// allow-everything — masking the regression in the wrong direction.
func claudeAllowBlock(claudeDir string) string {
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(claudeDir) + "))\n"
}

// TestSandboxExecClaudeConfig_XDGDirReadWrite is the positive integration
// test for the RW grant: under the production profile, a sandboxed
// process can create, read, and remove a file under the host
// ~/.config/claude. This is the capability claude-code needs at the path
// CLAUDE_CONFIG_DIR carries (config writes, history, token refreshes).
func TestSandboxExecClaudeConfig_XDGDirReadWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	claudeDir := claudeXDGDirForTest(t)

	m := newProfileManager(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must carry the explicit RW allow block — the sole grant
	// for this path (not sops-backed, no allowlist involvement).
	if !strings.Contains(prepared.content, claudeAllowBlock(claudeDir)) {
		t.Fatalf("profile does not carry the ~/.config/claude RW allow block (issue #2243).\nProfile:\n%s", prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	target := filepath.Join(claudeDir, fmt.Sprintf("prism-integ-2243-%d.tmp", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(target) }) // belt-and-braces if rm in-sandbox fails

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", claudeRoundTripScript(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("write/read/remove round-trip under %s failed in-sandbox under the production profile (issue #2243 AC).\nExit: %v\nOutput: %s\nProfile: %s",
			claudeDir, runErr, string(out), testProfilePath)
	}
	t.Logf("ka pai — in-sandbox RW round-trip under %s succeeded via the #2243 subpath grant", claudeDir)
}

// TestSandboxExecClaudeConfig_WriteDeniedWithoutSubpathGrant is the paired
// negative test (sandbox-exec testing convention). It strips the entire
// ~/.config/claude RW allow block from the profile and asserts the
// same write fails — proving the positive is not green by accident: the
// explicit subpath grant is the load-bearing capability for the path (there
// is no sops allowlist or broader allow to fall back on).
func TestSandboxExecClaudeConfig_WriteDeniedWithoutSubpathGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	claudeDir := claudeXDGDirForTest(t)

	m := newProfileManager(t)

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, claudeAllowBlock(claudeDir), "")
	})

	target := filepath.Join(claudeDir, fmt.Sprintf("prism-integ-2243-neg-%d.tmp", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(target) }) // in case the write unexpectedly lands

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "echo prism-2243-neg > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write under %s succeeded WITHOUT the (subpath ~/.config/claude) RW grant.\n"+
			"The grant is not load-bearing — investigate.\nOutput: %s\nMutated profile: %s",
			claudeDir, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — write under %s correctly denied without the #2243 subpath grant (exit: %v)", claudeDir, runErr)
	}
}
