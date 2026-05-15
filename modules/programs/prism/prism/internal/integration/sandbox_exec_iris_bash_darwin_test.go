//go:build darwin

package integration_test

// sandbox_exec_iris_bash_darwin_test.go — integration coverage for the
// iris D-5 bash tool sandbox-exec SBPL profile.
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
// This file covers the iris bash SBPL profile from
// internal/iris/bash_sandbox_darwin.go::GenerateBashSBPLProfile.
//
// Test pairs:
//
//   TestIrisBash_WorktreeWriteAllowed / TestIrisBash_WorktreeWriteAllowed_Negative
//     Positive: write a file under the worktree (RW profile); succeeds.
//     Negative: remove the worktree write allow; same write fails.
//
//   TestIrisBash_TmpReadWrite / TestIrisBash_TmpReadWrite_Negative
//     Positive: write and read a file in tmpDir; succeeds.
//     Negative: remove the tmpDir allow; same write fails.
//
//   TestIrisBash_EtcSSHDenied / TestIrisBash_EtcSSHDenied_Negative
//     Positive: cat /etc/ssh/ssh_config fails under the profile (deny rule).
//     Negative: remove /etc/ssh deny; same cat succeeds.
//
//   TestIrisBash_NetworkPermitted
//     Content assertion that (allow network*) is present in the profile.
//
//   TestIrisBash_PiCredentialsDenied (security)
//     Profile content check: ~/.claude, ~/.mcp-auth, ~/.pi/agent,
//     ~/.cache/bun, ~/.config/pi are NOT in the generated SBPL profile.
//
//   TestIrisBash_LLMKeysNotInProfile (security)
//     Profile content check: ANTHROPIC_API_KEY, OPENROUTER_API_KEY are
//     not granted by the profile.
//
// All tests skip when:
//   - Not on Darwin (build tag enforces this but checked defensively).
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

// generateIrisBashProfile generates a bash SBPL profile using the production
// generator for the given worktree and tmpDir.
func generateIrisBashProfile(worktree, tmpDir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return iris.GenerateBashSBPLProfile(home, worktree, tmpDir, "", "", "", false)
}

// writeIrisBashProfile writes a SBPL profile to a temp file, augments it
// with test-harness extras, and returns the path.
func writeIrisBashProfile(t *testing.T, profile string) string {
	t.Helper()
	augmented := augmentProfileForTest(profile)
	f, err := os.CreateTemp("", "iris-bash-test-*.sb")
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

// mutateBashProfile applies mutate to the profile, augments, writes to a
// temp file, and returns the path.  Fails the test if mutate is a no-op.
func mutateBashProfile(t *testing.T, profile string, mutate func(string) string) string {
	t.Helper()
	mutated := mutate(profile)
	if mutated == profile {
		t.Fatalf("mutateBashProfile: mutate returned identical content — the substitution did not match.\nProfile:\n%s", profile)
	}
	return writeIrisBashProfile(t, mutated)
}

// ── Profile content assertions ────────────────────────────────────────────────

// TestIrisBash_ProfileContent_WorktreeRW asserts that the bash profile contains
// a RW allow for the worktree.
func TestIrisBash_ProfileContent_WorktreeRW(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-wt-rw-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

	rwRule := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))"
	if !strings.Contains(profile, rwRule) {
		t.Errorf("bash profile missing worktree RW rule.\nWant substring: %q\nProfile:\n%s", rwRule, profile)
	}
}

// TestIrisBash_ProfileContent_DenyRules asserts that sensitive /etc subtrees
// are denied.
func TestIrisBash_ProfileContent_DenyRules(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-deny-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

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
			t.Errorf("bash profile missing deny rule: %q\nProfile:\n%s", exp, profile)
		}
	}

	denyHeader := "(deny file-read* file-write*\n"
	if !strings.Contains(profile, denyHeader) {
		t.Errorf("bash profile missing (deny file-read* file-write*) block.\nProfile:\n%s", profile)
	}
}

// TestIrisBash_NetworkPermitted asserts that (allow network*) is present.
func TestIrisBash_NetworkPermitted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-net-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

	if !strings.Contains(profile, "(allow network*)") {
		t.Errorf("bash profile does not contain '(allow network*)'\n"+
			"Network is required per design doc §5.\nProfile:\n%s", profile)
	}
}

// TestIrisBash_PiCredentialsDenied asserts that pi-process credential paths
// are NOT present in the generated bash SBPL profile.
//
// This is the [security] AC from issue #1636: pi-process credential paths
// (~/.claude, ~/.mcp-auth, ~/.pi/agent/*, ~/.cache/bun, ~/.config/pi/*)
// must NOT appear in the bash subprocess's mount set.
func TestIrisBash_PiCredentialsDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir: %v", err)
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-pi-creds-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

	piCredPaths := []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".mcp-auth"),
		filepath.Join(home, ".pi", "agent"),
		filepath.Join(home, ".cache", "bun"),
		filepath.Join(home, ".config", "pi"),
	}

	for _, path := range piCredPaths {
		if strings.Contains(profile, path) {
			t.Errorf("bash SBPL profile contains pi-process credential path %q;\n"+
				"this path must NOT appear in the bash sandbox.\n"+
				"Profile excerpt containing the path:\n%s",
				path, extractContext(profile, path))
		}
	}
}

// TestIrisBash_LLMKeysNotInProfile is a belt-and-suspenders check that the
// SBPL profile does not somehow inject LLM API keys.
func TestIrisBash_LLMKeysNotInProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	// Set LLM keys in the host env — they must not appear in the profile.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-excluded")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test-excluded")

	worktree, err := os.MkdirTemp("", "iris-d5-bash-llm-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		if strings.Contains(profile, key) {
			t.Errorf("bash SBPL profile contains %q; LLM API keys must be absent from the profile.\nProfile:\n%s",
				key, profile)
		}
	}
}

// ── Positive integration tests ────────────────────────────────────────────────

// TestIrisBash_WorktreeWriteAllowed verifies that a file can be written under
// the worktree when the bash profile allows RW access.
func TestIrisBash_WorktreeWriteAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d5-bash-wt-write-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	target := filepath.Join(worktree, "bash-write-test.txt")
	const writeContent = "iris-d5-bash-write-test"

	profile := generateIrisBashProfile(worktree, "")
	profilePath := writeIrisBashProfile(t, profile)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo -n "+shQuote(writeContent)+" > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("write to %q failed under bash iris profile.\nExit: %v\nOutput: %s\nProfile: %s",
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

// TestIrisBash_WorktreeWriteAllowed_Negative verifies that removing the
// worktree write allow causes writes to fail.
func TestIrisBash_WorktreeWriteAllowed_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d5-bash-wt-write-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	target := filepath.Join(worktree, "bash-write-neg.txt")
	profile := generateIrisBashProfile(worktree, "")

	// Remove the RW worktree allow rule from the profile.
	rwRule := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(worktree) + "))\n"
	profilePath := mutateBashProfile(t, profile, func(p string) string {
		return strings.ReplaceAll(p, rwRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to %q succeeded even with worktree write allow removed.\n"+
			"The positive test was a no-op.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	} else {
		t.Logf("ka pai — write correctly blocked without RW allow (exit: %v)", runErr)
	}
}

// TestIrisBash_TmpReadWrite verifies that /tmp is accessible (RW) when
// tmpDir is configured.
func TestIrisBash_TmpReadWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d5-bash-tmp-rw-wt-*")
	if err != nil {
		t.Fatalf("MkdirTemp(worktree): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	tmpDir, err := os.MkdirTemp("", "iris-d5-bash-tmp-rw-session-*")
	if err != nil {
		t.Fatalf("MkdirTemp(tmpDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	profile := generateIrisBashProfile(worktree, tmpDir)
	profilePath := writeIrisBashProfile(t, profile)

	target := filepath.Join(tmpDir, "iris-d5-bash-tmp-sentinel.txt")
	const tmpContent = "iris-d5-bash-tmp-rw"

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo -n "+shQuote(tmpContent)+" > "+shQuote(target)+" && cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("write+read to tmpDir %q failed.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}
	if !strings.Contains(string(out), tmpContent) {
		t.Errorf("tmp read output does not contain sentinel.\nOutput: %q", string(out))
	}
}

// TestIrisBash_TmpReadWrite_Negative verifies that removing the tmpDir allow
// causes writes to tmpDir to fail.
func TestIrisBash_TmpReadWrite_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	worktree, err := os.MkdirTemp("", "iris-d5-bash-tmp-neg-wt-*")
	if err != nil {
		t.Fatalf("MkdirTemp(worktree): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	tmpDir, err := os.MkdirTemp("", "iris-d5-bash-tmp-neg-session-*")
	if err != nil {
		t.Fatalf("MkdirTemp(tmpDir): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	target := filepath.Join(tmpDir, "iris-d5-bash-tmp-neg.txt")
	profile := generateIrisBashProfile(worktree, tmpDir)

	// Remove the tmpDir subpath allow rule.
	tmpRule := "  (subpath " + sbplQuoteForTest(tmpDir) + ")\n"
	profilePath := mutateBashProfile(t, profile, func(p string) string {
		return strings.ReplaceAll(p, tmpRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to tmpDir %q succeeded even with tmpDir allow removed.\n"+
			"The positive tmp test was a no-op.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	} else {
		t.Logf("ka pai — tmpDir write correctly blocked without allow (exit: %v)", runErr)
	}
}

// TestIrisBash_EtcSSHDenied verifies that /etc/ssh/ssh_config is blocked by
// the deny rule in the bash profile.
func TestIrisBash_EtcSSHDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	target := "/private/etc/ssh/ssh_config"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("%s does not exist on this host: %v", target, err)
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-etc-ssh-deny-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")
	profilePath := writeIrisBashProfile(t, profile)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded under bash iris profile — /etc/ssh deny is not enforced.\n"+
			"Output: %s\nProfile: %s", target, string(out), profilePath)
	} else {
		t.Logf("ka pai — /etc/ssh read correctly blocked (exit: %v)", runErr)
	}
}

// TestIrisBash_EtcSSHDenied_Negative verifies that removing the /etc/ssh
// deny allows reading it, proving the deny is load-bearing.
func TestIrisBash_EtcSSHDenied_Negative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	target := "/private/etc/ssh/ssh_config"
	if _, err := os.Stat(target); err != nil {
		t.Skipf("%s does not exist on this host: %v", target, err)
	}

	worktree, err := os.MkdirTemp("", "iris-d5-bash-etc-ssh-neg-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })

	profile := generateIrisBashProfile(worktree, "")

	// Remove both the /etc/ssh and /private/etc/ssh deny lines (same pattern
	// as the D-4 file-tool test).
	profilePath := mutateBashProfile(t, profile, func(p string) string {
		p = strings.ReplaceAll(p, "  (subpath \"/etc/ssh\")\n", "")
		p = strings.ReplaceAll(p,
			"  (subpath \"/private/etc/wpa_supplicant\")\n  (subpath \"/private/etc/ssh\"))\n",
			"  (subpath \"/private/etc/wpa_supplicant\"))\n")
		return p
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("read of %s failed even with /etc/ssh deny removed.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	} else {
		t.Logf("ka pai — read succeeded without deny rules (expected)")
	}
}

// ── Helper ────────────────────────────────────────────────────────────────────

// extractContext returns up to 200 bytes of context around the first
// occurrence of needle in s, for use in error messages.
func extractContext(s, needle string) string {
	idx := strings.Index(s, needle)
	if idx < 0 {
		return ""
	}
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + len(needle) + 80
	if end > len(s) {
		end = len(s)
	}
	return "..." + s[start:end] + "..."
}
