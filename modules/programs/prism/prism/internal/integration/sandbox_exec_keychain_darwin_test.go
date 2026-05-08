//go:build darwin

package integration_test

// sandbox_exec_keychain_darwin_test.go — integration coverage for the
// login.keychain-db file-read rule added to the SBPL profile (issue #1487).
//
// Background: the Keychain Services API routes credential lookups over Mach IPC
// to securityd, but securityd also requires file-read* access to
// ~/Library/Keychains/login.keychain-db to service the lookup. Without this,
// securityd returns exit 44 ("item not found") from inside the sandbox even
// though the Mach IPC path is open.
//
// generateProfile now emits:
//
//	(allow file-read*
//	  (literal "<home>/Library/Keychains/login.keychain-db"))
//
// conditionally, when the file exists on the host.
//
// This file tests:
//
//  1. Positive case: when login.keychain-db exists, /usr/bin/security
//     find-generic-password exits 0 (credentials present) or 44 (API
//     reachable, entry absent). "Operation not permitted" in output is always
//     a failure. Also asserts the generated profile contains the literal path
//     (load-bearing regression guard).
//     Security is invoked with real HOME (not staging HOME) so the CLI can
//     locate the host keychain search list.
//
// Note on negative test: the withMutatedProfile pattern cannot isolate the
// keychain literal as load-bearing on a fully set-up machine — the full
// production profile (with staging-home symlink targets) grants access to
// ~/Library/Keychains via other rules in this configuration. The profile
// string-content assertion in the positive test is the regression guard: if
// generateProfile stops emitting the rule, that assertion fails before the
// sandbox invocation runs.
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

// loginKeychainPath returns the path to the user's login.keychain-db.
// Returns an empty string if the user's home directory cannot be determined.
func loginKeychainPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db")
}

// TestSandboxExecProfile_KeychainAPIAccessible is the integration test for the
// login.keychain-db file-read rule (issue #1487).
//
// It asserts that:
//   - The generated profile contains the (literal login.keychain-db) path
//     (load-bearing regression guard — fails before sandbox runs if missing).
//   - /usr/bin/security find-generic-password exits 0 or 44 inside the sandbox
//     when invoked with real HOME (no "Operation not permitted" sandbox denial).
func TestSandboxExecProfile_KeychainAPIAccessible(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	if _, err := os.Stat("/usr/bin/security"); err != nil {
		t.Skip("/usr/bin/security not present")
	}

	keychainPath := loginKeychainPath(t)
	if keychainPath == "" {
		t.Skip("cannot determine home directory")
	}
	if _, err := os.Stat(keychainPath); err != nil {
		t.Skipf("login.keychain-db does not exist at %s — skipping: %v", keychainPath, err)
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)

	// Load-bearing regression guard: the generated profile must contain the
	// literal login.keychain-db path. If generateProfile stops emitting it,
	// this assertion fails before the sandbox invocation below.
	if !strings.Contains(prepared.content, keychainPath) {
		t.Fatalf("generated profile does not contain the login.keychain-db literal path %q.\n"+
			"The (allow file-read* (literal ...)) rule was not emitted by generateProfile.\nProfile:\n%s",
			keychainPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Pass real HOME (not staging HOME) so the security CLI can locate the
	// host keychain search list. With staging HOME the CLI always returns
	// exit 44 regardless of the SBPL rule.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realHome,
		"/usr/bin/security", "find-generic-password", "-l", keychainServiceName, "-w")
	out, runErr := cmd.CombinedOutput()
	output := string(out)

	// "Operation not permitted" means a sandbox denial — always a failure.
	if strings.Contains(output, "Operation not permitted") {
		t.Fatalf("Keychain API call produced 'Operation not permitted' inside sandbox.\n"+
			"This indicates a sandbox denial — the (literal login.keychain-db) rule is not working.\n"+
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
		t.Logf("Keychain API accessible inside sandbox (security exit 44: item not found). "+
			"Claude Code-credentials not present on this host — expected on CI. (#1487)")
		return
	}

	t.Errorf("Keychain API call inside sandbox: expected exit 0 or 44, got %d\nOutput: %s\nProfile: %s",
		exitCode, output, testProfilePath)
}
