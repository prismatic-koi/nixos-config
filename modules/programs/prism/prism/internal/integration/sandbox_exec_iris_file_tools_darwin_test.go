//go:build darwin

package integration_test

// sandbox_exec_iris_file_tools_darwin_test.go — integration coverage for the
// iris D-4 per-tool sandbox-exec SBPL profile.
//
// Per the sandbox-exec testing convention (docs/sandbox-exec-testing.md,
// issue #1192), every change to a sandbox-exec profile generator must be
// paired with:
//
//  1. A positive integration test that invokes /usr/bin/sandbox-exec against
//     a Nix-built test binary with the generated profile and asserts expected
//     behaviour.
//
//  2. A negative test that mutates the profile (removes a specific allow rule)
//     and asserts the same operation fails — proving the positive is not a
//     no-op.
//
// This file covers the iris file-tool SBPL profile from
// internal/iris/file_sandbox_darwin.go::generateFileToolSBPLProfile.
//
// Test pairs:
//
//   TestIrisFileTool_WorktreeReadAllowed / TestIrisFileTool_WorktreeReadAllowed_Negative
//     Positive: cat a sentinel file under the worktree; succeeds.
//     Negative: remove the worktree allow rule; same cat fails.
//
//   TestIrisFileTool_WorktreeWriteAllowed / TestIrisFileTool_WorktreeWriteAllowed_Negative
//     Positive: write a file under the worktree (RW profile); succeeds.
//     Negative: remove the worktree write allow; same write fails.
//
//   TestIrisFileTool_TmpReadWrite / TestIrisFileTool_TmpReadWrite_Negative
//     Positive: write and read a file in /tmp (via tmpDir allow); succeeds.
//     Negative: remove the /tmp allow; same write fails.
//
//   TestIrisFileTool_EtcSSHDenied / TestIrisFileTool_EtcSSHDenied_Negative
//     Positive: cat /etc/ssh/ssh_config fails under the profile (deny rule).
//     Negative: remove the /etc/ssh deny; same cat succeeds.
//
//   TestIrisFileTool_NetworkPermitted
//     Positive: curl/nc-style check that outbound TCP is not blocked.
//     (No negative test needed — (allow network*) is the last line.)
//
// Profile content assertions (substring checks, necessary but not sufficient):
//   TestIrisFileTool_ProfileContent_WorktreeRO
//   TestIrisFileTool_ProfileContent_WorktreeRW
//   TestIrisFileTool_ProfileContent_DenyRules
//
// All tests skip when:
//   - Not on Darwin (build tag enforces this but check defensively).
//   - /usr/bin/sandbox-exec is absent.
//   - bash does not resolve to a /nix/store/... path.
//   - Inside the Nix build sandbox (NIX_BUILD_TOP is set).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
)

// generateIrisROProfile generates a RO-worktree iris file-tool SBPL profile
// using the production generator for the given worktree and tmpDir.
func generateIrisROProfile(worktree, tmpDir string) string {
	return iris.GenerateFileToolSBPLProfile(worktree, tmpDir, true)
}

// generateIrisRWProfile generates a RW-worktree iris file-tool SBPL profile.
func generateIrisRWProfile(worktree, tmpDir string) string {
	return iris.GenerateFileToolSBPLProfile(worktree, tmpDir, false)
}

// writeIrisProfile writes a SBPL profile to a temp file, augments it with
// test-harness extras, and returns the path.  Registers a cleanup for the
// temp file.
func writeIrisProfile(t *testing.T, profile string) string {
	t.Helper()
	augmented := augmentProfileForTest(profile)
	f, err := os.CreateTemp("", "iris-file-tool-test-*.sb")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	if _, err := f.WriteString(augmented); err != nil {
		f.Close()
		os.Remove(path)
		t.Fatalf("write profile: %v", err)
	}
	f.Close()
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// mutateThenWrite applies mutate to the profile, augments, writes to a temp
// file, and returns the path.
func mutateThenWrite(t *testing.T, profile string, mutate func(string) string) string {
	t.Helper()
	mutated := mutate(profile)
	if mutated == profile {
		t.Fatalf("mutateThenWrite: mutate returned identical content — the substitution did not match.\nProfile:\n%s", profile)
	}
	return writeIrisProfile(t, mutated)
}

// --- Profile content assertions ---

// TestIrisFileTool_ProfileContent_WorktreeRO asserts that the RO profile
// contains the expected allow rules for the worktree (file-read* only) and
// the expected deny rules for sensitive /etc subtrees.
func TestIrisFileTool_ProfileContent_WorktreeRO(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d4-worktree-ro-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisROProfile(worktree, "")

	// RO worktree rule: file-read* (no file-write*).
	roRule := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))"
	if !strings.Contains(profile, roRule) {
		t.Errorf("RO profile missing worktree read-only rule.\nWant substring: %q\nProfile:\n%s", roRule, profile)
	}

	// Must NOT contain a write allow for the worktree.
	rwRule := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))"
	if strings.Contains(profile, rwRule) {
		t.Errorf("RO profile unexpectedly contains a write allow for the worktree.\nProfile:\n%s", profile)
	}
}

// TestIrisFileTool_ProfileContent_WorktreeRW asserts that the RW profile
// contains the expected allow rules for the worktree (file-read* + file-write*).
func TestIrisFileTool_ProfileContent_WorktreeRW(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d4-worktree-rw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisRWProfile(worktree, "")

	rwRule := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))"
	if !strings.Contains(profile, rwRule) {
		t.Errorf("RW profile missing worktree read-write rule.\nWant substring: %q\nProfile:\n%s", rwRule, profile)
	}
}

// TestIrisFileTool_ProfileContent_DenyRules asserts that the deny rules for
// sensitive /etc subtrees are present in the generated profile.
func TestIrisFileTool_ProfileContent_DenyRules(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d4-deny-rules-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisROProfile(worktree, "")

	expected := []string{
		"  (subpath \"/etc/wireguard\")",
		"  (subpath \"/etc/wpa_supplicant\")",
		"  (subpath \"/etc/ssh\")",
		"  (subpath \"/private/etc/wireguard\")",
		"  (subpath \"/private/etc/wpa_supplicant\")",
		"  (subpath \"/private/etc/ssh\"))",
	}
	for _, exp := range expected {
		if !strings.Contains(profile, exp) {
			t.Errorf("profile missing deny rule: %q\nProfile:\n%s", exp, profile)
		}
	}

	denyHeader := "(deny file-read* file-write*\n"
	if !strings.Contains(profile, denyHeader) {
		t.Errorf("profile missing (deny file-read* file-write*) block.\nProfile:\n%s", profile)
	}
}

// --- Positive integration tests ---

// TestIrisFileTool_WorktreeReadAllowed verifies that a file under the worktree
// can be read by a Nix-built binary under the RO iris file-tool profile.
func TestIrisFileTool_WorktreeReadAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-wt-read-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	sentinel := filepath.Join(worktree, "sentinel.txt")
	const sentinelContent = "iris-d4-worktree-read-sentinel"
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	profile := generateIrisROProfile(worktree, "")
	profilePath := writeIrisProfile(t, profile)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("read of sentinel %q failed under RO iris profile.\nExit: %v\nOutput: %s\nProfile: %s",
			sentinel, runErr, string(out), profilePath)
	}
	if !strings.Contains(string(out), sentinelContent) {
		t.Errorf("read output does not contain sentinel content.\nOutput: %q", string(out))
	}
}

// TestIrisFileTool_WorktreeReadAllowed_Negative verifies that removing the
// worktree allow rule causes reads to fail, proving the positive is non-trivial.
func TestIrisFileTool_WorktreeReadAllowed_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-wt-read-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	sentinel := filepath.Join(worktree, "sentinel.txt")
	const sentinelContent = "iris-d4-worktree-read-sentinel-neg"
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	profile := generateIrisROProfile(worktree, "")

	// Remove the worktree read allow rule from the profile.
	worktreeRule := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))\n"
	profilePath := mutateThenWrite(t, profile, func(p string) string {
		return strings.ReplaceAll(p, worktreeRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of sentinel %q succeeded even with worktree allow rule removed.\n"+
			"The positive test was a no-op — investigate the profile.\nOutput: %s\nProfile: %s",
			sentinel, string(out), profilePath)
	} else {
		t.Logf("ka pai — read correctly blocked without worktree allow (exit: %v)", runErr)
	}
}

// TestIrisFileTool_WorktreeWriteAllowed verifies that a file can be written
// under the worktree when the profile is RW.
func TestIrisFileTool_WorktreeWriteAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-wt-write-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	target := filepath.Join(worktree, "write-test.txt")
	const writeContent = "iris-d4-write-test"

	profile := generateIrisRWProfile(worktree, "")
	profilePath := writeIrisProfile(t, profile)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo -n "+shQuote(writeContent)+" > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("write to %q failed under RW iris profile.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}

	// Verify the file was actually written.
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Errorf("post-write ReadFile %q: %v", target, readErr)
	} else if string(got) != writeContent {
		t.Errorf("post-write content = %q, want %q", string(got), writeContent)
	}
}

// TestIrisFileTool_WorktreeWriteAllowed_Negative verifies that removing the
// worktree write allow causes writes to fail.
func TestIrisFileTool_WorktreeWriteAllowed_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-wt-write-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	target := filepath.Join(worktree, "write-neg-test.txt")
	profile := generateIrisRWProfile(worktree, "")

	// Remove the RW worktree allow rule.
	rwRule := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))\n"
	profilePath := mutateThenWrite(t, profile, func(p string) string {
		return strings.ReplaceAll(p, rwRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to %q succeeded even with worktree write allow rule removed.\n"+
			"The positive write test was a no-op.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	} else {
		t.Logf("ka pai — write correctly blocked without RW worktree allow (exit: %v)", runErr)
	}
}

// TestIrisFileTool_TmpReadWrite verifies that /tmp is accessible (RW) under
// the iris file-tool profile when tmpDir is configured.
func TestIrisFileTool_TmpReadWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-tmp-rw-wt-*")
	if err != nil {
		t.Fatalf("MkdirTemp(worktree): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	tmpDir, err := os.MkdirTemp("", "iris-d4-tmp-rw-session-*")
	if err != nil {
		t.Fatalf("MkdirTemp(tmpDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	profile := generateIrisROProfile(worktree, tmpDir)
	profilePath := writeIrisProfile(t, profile)

	// Write and read back a file in the tmpDir (the session's /tmp backing dir).
	target := filepath.Join(tmpDir, "iris-d4-tmp-sentinel.txt")
	const tmpContent = "iris-d4-tmp-rw-sentinel"

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo -n "+shQuote(tmpContent)+" > "+shQuote(target)+" && cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("write+read to tmpDir %q failed under iris profile.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}
	if !strings.Contains(string(out), tmpContent) {
		t.Errorf("tmp read output does not contain sentinel.\nOutput: %q", string(out))
	}
}

// TestIrisFileTool_TmpReadWrite_Negative verifies that removing the tmpDir
// allow causes writes to tmpDir to fail.
func TestIrisFileTool_TmpReadWrite_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d4-tmp-neg-wt-*")
	if err != nil {
		t.Fatalf("MkdirTemp(worktree): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	tmpDir, err := os.MkdirTemp("", "iris-d4-tmp-neg-session-*")
	if err != nil {
		t.Fatalf("MkdirTemp(tmpDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	target := filepath.Join(tmpDir, "iris-d4-tmp-neg.txt")
	profile := generateIrisROProfile(worktree, tmpDir)

	// Remove the tmpDir subpath allow rule.
	tmpRule := "  (subpath " + sbplQuoteForTest(tmpDir) + ")\n"
	profilePath := mutateThenWrite(t, profile, func(p string) string {
		return strings.ReplaceAll(p, tmpRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to tmpDir %q succeeded even with tmpDir allow rule removed.\n"+
			"The positive tmp test was a no-op.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	} else {
		t.Logf("ka pai — tmpDir write correctly blocked without allow (exit: %v)", runErr)
	}
}

// TestIrisFileTool_EtcSSHDenied verifies that reads of /etc/ssh/ssh_config
// are blocked by the deny rule in the iris file-tool profile.
// Skips if /etc/ssh/ssh_config does not exist on the host (can't distinguish
// sandbox deny from ENOENT).
func TestIrisFileTool_EtcSSHDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	target := "/private/etc/ssh/ssh_config"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("%s does not exist on this host — cannot distinguish sandbox deny from ENOENT: %v", target, err)
	}

	worktree, err := os.MkdirTemp("", "iris-d4-etc-ssh-deny-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisROProfile(worktree, "")
	profilePath := writeIrisProfile(t, profile)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded (exit 0) under iris profile — /etc/ssh deny is not enforced.\n"+
			"Output: %s\nProfile: %s", target, string(out), profilePath)
	} else {
		t.Logf("ka pai — /etc/ssh read correctly blocked (exit: %v)", runErr)
	}
}

// TestIrisFileTool_EtcSSHDenied_Negative verifies that removing the /etc/ssh
// deny allows reading /etc/ssh/ssh_config, proving the deny is load-bearing.
func TestIrisFileTool_EtcSSHDenied_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	target := "/private/etc/ssh/ssh_config"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("%s does not exist on this host — cannot distinguish sandbox deny from ENOENT: %v", target, err)
	}

	worktree, err := os.MkdirTemp("", "iris-d4-etc-ssh-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisROProfile(worktree, "")

	// Remove both the /etc/ssh and /private/etc/ssh deny subpath lines.
	profilePath := mutateThenWrite(t, profile, func(p string) string {
		// Remove /etc/ssh line from the deny block.
		p = strings.ReplaceAll(p, "  (subpath \"/etc/ssh\")\n", "")
		// /private/etc/ssh is the last entry in the block (has closing "))").
		// Promote /private/etc/wpa_supplicant to be the last entry.
		p = strings.ReplaceAll(p,
			"  (subpath \"/private/etc/wpa_supplicant\")\n  (subpath \"/private/etc/ssh\"))\n",
			"  (subpath \"/private/etc/wpa_supplicant\"))\n")
		return p
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("read of %s failed even with /etc/ssh deny rules removed.\n"+
			"The /private/etc allow should permit this read.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	} else {
		t.Logf("ka pai — read succeeded without deny rules (expected)")
	}
}

// TestIrisFileTool_NetworkPermitted verifies that (allow network*) is present
// in the iris file-tool profile, consistent with the design doc §5/§6.3.
// We test this via profile-content assertion (actual network tests are
// environment-dependent and not appropriate for the test suite).
func TestIrisFileTool_NetworkPermitted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d4-net-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisROProfile(worktree, "")

	if !strings.Contains(profile, "(allow network*)") {
		t.Errorf("iris file-tool profile does not contain '(allow network*)'\n"+
			"Network is required to be permitted per design doc §5/§6.3.\nProfile:\n%s", profile)
	}
}
