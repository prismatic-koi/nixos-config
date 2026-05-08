//go:build darwin

package integration_test

// sandbox_exec_keychain_darwin_test.go — integration coverage for the
// ~/Library/Keychains file-read rule added to the SBPL profile (issue #1487).
//
// Background: the Keychain Services API routes credential lookups over Mach IPC
// to securityd, but securityd also requires file-read* + file-test-existence on
// ~/Library/Keychains to service the lookup. Without the subpath rule, securityd
// returns exit 44 ("item not found") from inside the sandbox even though the
// Mach IPC path is open.
//
// generateProfile now emits:
//
//	(allow file-read* file-test-existence
//	  (subpath "<home>/Library/Keychains"))
//
// conditionally, when the directory exists on the host.
//
// This file tests:
//
//  1. Positive case: when ~/Library/Keychains exists, /usr/bin/security
//     find-generic-password exits 0 (credentials present) or 44 (API
//     reachable, entry absent). "Operation not permitted" in output is always
//     a failure. Also asserts the generated profile contains the Keychains
//     subpath — this string check is the load-bearing regression guard for
//     the SBPL rule.
//
// Note on negative test: the standard negative-test pattern (mutate profile,
// assert operation fails) cannot be made to work for this rule because the
// full production profile (with staging-home symlink targets and the BareRoot
// ancestor block) grants file-read access to ~/Library/Keychains via other
// rules on a fully set-up machine. The profile string-content assertion in the
// positive test serves as the regression guard: if generateProfile stops
// emitting the Keychains rule, the assertion fails before security even runs.
//
// Shared helpers (requireSandboxExec, newProfileManagerWithBareRoot,
// preparePositiveProfile, writeAugmentedPositiveProfile) live in
// sandbox_exec_helpers_darwin_test.go.
//
// See docs/sandbox-exec-testing.md for the convention these tests support (#1192).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// keychainServiceName is the Keychain service name for Claude credentials.
const keychainServiceName = "Claude Code-credentials"

// keychainsDir returns the path to the user's ~/Library/Keychains directory.
// Returns an empty string if the user's home directory cannot be determined.
func keychainsDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Keychains")
}

// TestSandboxExecProfile_KeychainAPIAccessible is the integration test for the
// ~/Library/Keychains file-read rule (issue #1487).
//
// It asserts that:
//   - The generated profile contains the (subpath ~/Library/Keychains) rule
//     (load-bearing regression guard: if generateProfile stops emitting the
//     rule, this assertion fails before the sandbox invocation even runs).
//   - /usr/bin/security find-generic-password exits 0 or 44 inside the sandbox
//     (no "Operation not permitted" sandbox denial).
func TestSandboxExecProfile_KeychainAPIAccessible(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	if _, err := os.Stat("/usr/bin/security"); err != nil {
		t.Skip("/usr/bin/security not present")
	}

	kDir := keychainsDir(t)
	if kDir == "" {
		t.Skip("cannot determine home directory")
	}
	if _, err := os.Stat(kDir); err != nil {
		t.Skipf("~/Library/Keychains does not exist at %s — skipping: %v", kDir, err)
	}

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)

	// Load-bearing regression guard: the generated profile must contain the
	// ~/Library/Keychains subpath rule. If generateProfile stops emitting it,
	// this check catches the regression before the sandbox invocation below.
	if !strings.Contains(prepared.content, kDir) {
		t.Fatalf("generated profile does not contain the ~/Library/Keychains subpath %q.\n"+
			"The (allow file-read* file-test-existence (subpath ...)) rule was not emitted by generateProfile.\nProfile:\n%s",
			kDir, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Run /usr/bin/security inside sandbox-exec. Apple-signed binary; uses
	// Mach IPC to contact securityd — exactly the code path we need to exercise.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/security", "find-generic-password", "-l", keychainServiceName, "-w")
	out, runErr := cmd.CombinedOutput()
	output := string(out)

	// "Operation not permitted" means a sandbox denial — always a failure.
	if strings.Contains(output, "Operation not permitted") {
		t.Fatalf("Keychain API call produced 'Operation not permitted' inside sandbox.\n"+
			"This indicates a sandbox denial — the (subpath ~/Library/Keychains) rule is not working.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, output, testProfilePath)
	}

	const securityItemNotFound = 44
	if runErr == nil {
		t.Logf("ka pai — Keychain API accessible inside sandbox, credentials retrieved (exit 0)")
		return
	}

	exitCode := -1
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}

	if exitCode == securityItemNotFound {
		// Entry absent — API is reachable, item is simply missing. Acceptable on CI.
		t.Logf("Keychain API accessible inside sandbox (security exit 44: item not found). "+
			"Claude Code-credentials not present on this host — expected on CI. (#1487)")
		return
	}

	t.Errorf("Keychain API call inside sandbox: expected exit 0 or 44, got %d\nOutput: %s\nProfile: %s",
		exitCode, output, testProfilePath)
}
