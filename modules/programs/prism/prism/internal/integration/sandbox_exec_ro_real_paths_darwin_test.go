//go:build darwin

package integration_test

// sandbox_exec_ro_real_paths_darwin_test.go — integration coverage for the
// read-only real-path grants of Step 3f of #2132 (issue #2245):
//
//   - section 5f: ~/.cache/prism/clipboard (images staged by
//     `prism clipboard paste-image`, read by the agent at the absolute host
//     path). The ~/.config/prism/agents half of 5f is covered by
//     sandbox_exec_role_prompt_darwin_test.go.
//   - section 5g: ~/.nix-profile — a host SYMLINK; the grant carries a
//     literal allow on the link node plus an RO rule on the
//     EvalSymlinks-resolved target so access works through resolution.
//
// Each capability gets an RO positive AND a write-denied negative (RO must
// not silently become RW). The clipboard additionally gets a whole-block
// strip negative proving the 5f block is load-bearing.
//
// Note on 5g and the strip negative: on hm/nix-darwin hosts the
// ~/.nix-profile chain resolves into /nix/store (covered by the broad §2
// /nix allow) and link-node metadata is covered by the BareRoot-ancestor
// (subpath HOME) metadata allow — so a stripped 5g block does NOT
// deterministically deny the read on such hosts; the block is
// defence-in-depth for layouts where the profile dir lives outside /nix.
// A strip negative would therefore be flaky-by-layout and is deliberately
// omitted; the 5g emission shape is pinned at unit level
// (TestGenerateProfile_NixProfileROGrant) and the security property (no
// writes) is integration-tested here.
//
// Per docs/sandbox-exec-testing.md (issue #1192); #2207 capability-probe
// gating via requireSandboxExec.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const clipboardSentinelContent = "prism-2245-3f-clipboard-sentinel"

// roGrantBlock5f returns the exact section-5f allow block emitted by
// generateProfile (clipboard + role prompts), for presence assertions and
// whole-block strips.
func roGrantBlock5f(t *testing.T) string {
	t.Helper()
	home := realUserHome(t)
	return "(allow file-read* file-test-existence\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".cache", "prism", "clipboard")) + ")\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, ".config", "prism", "agents")) + "))\n"
}

// prepareClipboardSentinel creates ~/.cache/prism/clipboard under the real
// HOME (when absent) and plants a sentinel file inside it. Cleanup removes
// only what the test created.
func prepareClipboardSentinel(t *testing.T) (clipboardDir, sentinelPath string) {
	t.Helper()
	home := realUserHome(t)
	clipboardDir = filepath.Join(home, ".cache", "prism", "clipboard")
	dirExisted := false
	if _, statErr := os.Stat(clipboardDir); statErr == nil {
		dirExisted = true
	}
	if mkErr := os.MkdirAll(clipboardDir, 0o700); mkErr != nil {
		t.Skipf("cannot create ~/.cache/prism/clipboard for test: %v", mkErr)
	}
	sentinelPath = filepath.Join(clipboardDir, ".prism-2245-3f-clipboard-test")
	if wErr := os.WriteFile(sentinelPath, []byte(clipboardSentinelContent), 0o600); wErr != nil {
		t.Skipf("cannot plant clipboard sentinel (may be running inside a restricted sandbox): %v", wErr)
	}
	if dirExisted {
		t.Cleanup(func() { _ = os.Remove(sentinelPath) })
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(clipboardDir) })
	}
	return clipboardDir, sentinelPath
}

// TestSandboxExecClipboard_RealPathReadable is the RO positive for the
// clipboard half of section 5f: a staged image (sentinel) is readable
// in-sandbox at the REAL host path under the production profile, with no
// staging symlink involved.
func TestSandboxExecClipboard_RealPathReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareClipboardSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// No clipboard staging entry (nor a .cache staging dir at all).
	if _, lstatErr := os.Lstat(filepath.Join(stagingHome, ".cache")); lstatErr == nil {
		t.Fatalf("staging HOME has a .cache entry — the 3e/3f staging symlinks were removed in #2245 and must not be recreated")
	}

	prepared, _ := preparePositiveProfile(t, m)

	block := roGrantBlock5f(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-5f RO block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(sentinelPath))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("in-sandbox read of the clipboard sentinel at its real path failed.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s\nTarget: %s",
			runErr, string(out), testProfilePath, sentinelPath)
	}
	if !strings.Contains(string(out), clipboardSentinelContent) {
		t.Errorf("clipboard read did not return the sentinel content.\nGot: %s", string(out))
	}
}

// TestSandboxExecClipboard_DeniedWithoutGrantBlock is the strip negative for
// section 5f: with the ENTIRE block removed, the same clipboard read fails —
// proving the 5f block is load-bearing (whole-block strip per the #2243
// lesson: stripping only one (subpath ...) line would leave the other path
// granted).
func TestSandboxExecClipboard_DeniedWithoutGrantBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, sentinelPath := prepareClipboardSentinel(t)

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
		"cat "+shQuote(sentinelPath))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("clipboard read succeeded WITHOUT the section-5f block.\n"+
			"The 5f grant is not the load-bearing rule — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — clipboard read correctly denied without the 5f block (exit: %v)", runErr)
	}
}

// TestSandboxExecClipboard_WriteDenied is the write-denied negative for the
// clipboard half of section 5f: under the PRODUCTION profile (no mutation),
// writing into the real clipboard dir fails — RO must not silently become RW.
func TestSandboxExecClipboard_WriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	clipboardDir, _ := prepareClipboardSentinel(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	writeTarget := filepath.Join(clipboardDir, ".prism-2245-3f-write-denied")
	t.Cleanup(func() { _ = os.Remove(writeTarget) }) // in case the deny fails
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"echo prism-2245-write > "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write into the RO clipboard dir succeeded under the production profile — RO silently became RW.\n"+
			"Output: %s\nProfile: %s\nTarget: %s", string(out), testProfilePath, writeTarget)
	} else {
		t.Logf("ka pai — clipboard write correctly denied under the production profile (exit: %v)", runErr)
	}
}

// TestSandboxExecNixProfile_ReadableThroughSymlink is the RO positive for
// section 5g: ~/.nix-profile (a host symlink) is readable in-sandbox at its
// real path — both the link node itself (readlink) and a directory listing
// through the resolved chain. Skips when the host has no ~/.nix-profile.
func TestSandboxExecNixProfile_ReadableThroughSymlink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home := realUserHome(t)
	nixProfile := filepath.Join(home, ".nix-profile")
	if _, lstatErr := os.Lstat(nixProfile); lstatErr != nil {
		t.Skipf("~/.nix-profile absent on this host: %v", lstatErr)
	}

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// No nix-profile staging entry.
	if _, lstatErr := os.Lstat(filepath.Join(stagingHome, ".nix-profile")); lstatErr == nil {
		t.Fatalf("staging HOME has a .nix-profile entry — removed in #2245 (Step 3f), must not be recreated")
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The 5g literal on the link node must be present.
	literalRule := "(literal " + sbplQuoteForTest(nixProfile) + ")"
	if !strings.Contains(prepared.content, literalRule) {
		t.Fatalf("generated profile does not contain the 5g literal for ~/.nix-profile:\n%s\nProfile:\n%s",
			literalRule, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// readlink exercises the link node; ls -1 through the path exercises
	// resolution to the target.
	script := "readlink " + shQuote(nixProfile) + " && ls -1 " + shQuote(nixProfile)
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c", script)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("in-sandbox readlink/ls of ~/.nix-profile failed under the production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Errorf("readlink/ls produced no output — expected the link target and a profile listing")
	}
}

// TestSandboxExecNixProfile_WriteDenied is the write-denied negative for
// section 5g: under the PRODUCTION profile, writing through ~/.nix-profile
// fails — the grant is read-only and must not silently become RW.
func TestSandboxExecNixProfile_WriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home := realUserHome(t)
	nixProfile := filepath.Join(home, ".nix-profile")
	if _, lstatErr := os.Lstat(nixProfile); lstatErr != nil {
		t.Skipf("~/.nix-profile absent on this host: %v", lstatErr)
	}

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	writeTarget := filepath.Join(nixProfile, ".prism-2245-5g-write-denied")
	t.Cleanup(func() { _ = os.Remove(writeTarget) }) // in case the deny fails
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"touch "+shQuote(writeTarget))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write through ~/.nix-profile succeeded under the production profile — the 5g grant must be RO.\n"+
			"Output: %s\nProfile: %s\nTarget: %s", string(out), testProfilePath, writeTarget)
	} else {
		t.Logf("ka pai — write through ~/.nix-profile correctly denied (exit: %v)", runErr)
	}
}
