//go:build darwin

package integration_test

// sandbox_exec_nix_trusted_settings_darwin_test.go — integration coverage for
// the single-file read-only allow on ~/.local/share/nix/trusted-settings.json
// (issue #2201).
//
// Flake-CLI nix commands consult $XDG_DATA_HOME/nix/trusted-settings.json
// whenever the target flake declares a nixConfig block. Inside a sandbox-exec
// session XDG_DATA_HOME points at the real host ~/.local/share (see
// cmd/agent_run_sandbox_exec_darwin.go), so under deny-default the read
// failed EPERM and nix aborted the entire eval — making every flake CLI
// command unusable on repos with a nixConfig block (e.g. nixos-config).
//
// Per docs/sandbox-exec-testing.md the coverage is a positive/negative pair:
//
//   - TestSandboxExecProfile_NixTrustedSettingsReadable proves the generated
//     profile permits opening the file for reading. The test NEVER creates or
//     modifies the real host file (it is the user's actual nix trust store),
//     so it asserts on whichever state the host is in: when the file exists
//     the read must exit 0; when it is absent the open must be attempted and
//     fail with ENOENT ("No such file or directory") — NOT EPERM ("Operation
//     not permitted"). Both branches prove the sandbox allowed the open,
//     which is exactly the property nix needs (nix's pathExists/readFile
//     tolerate ENOENT but abort on EPERM). The ENOENT branch also covers the
//     issue #2201 edge-case AC: a missing host file follows nix's normal
//     missing-file path instead of crashing.
//
//   - TestSandboxExecProfile_NixTrustedSettingsDeniedWithoutRule mutates the
//     profile to remove the allow block and asserts the same read fails with
//     "Operation not permitted" — proving the positive test is not green by
//     accident.
//
// Both tests use newProfileManagerWithBareRoot: production worker sessions
// always have BareRoot set, so the ancestor metadata block is present, and
// the rule under test grants file-read-data which the BareRoot ancestor
// block (file-test-existence/file-read-metadata only) cannot mask in the
// negative test.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// nixTrustedSettingsPath returns the host path of nix's flake
// trusted-settings file, derived the same way generateProfile derives it
// (os.UserHomeDir() + .local/share/nix/trusted-settings.json).
func nixTrustedSettingsPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	return filepath.Join(home, ".local", "share", "nix", "trusted-settings.json")
}

// TestSandboxExecProfile_NixTrustedSettingsReadable is the positive half:
// under the unmodified production profile, opening
// ~/.local/share/nix/trusted-settings.json for reading must not be denied by
// the sandbox.
func TestSandboxExecProfile_NixTrustedSettingsReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	target := nixTrustedSettingsPath(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()

	// Regardless of whether the host file exists, the sandbox must not be
	// the thing that blocks the open.
	if strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("read of %s was denied by the sandbox (EPERM) under the production profile.\n"+
			"The nix trusted-settings allow (issue #2201) is missing or not load-bearing —\n"+
			"flake-CLI nix commands will abort inside worker sandboxes on repos with a nixConfig block.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}

	// Branch on actual host state — the test must never create or modify
	// the user's real trusted-settings.json.
	if _, statErr := os.Stat(target); statErr == nil {
		// File exists on the host: the sandboxed read must succeed outright.
		if runErr != nil {
			t.Errorf("host file %s exists but the sandboxed read failed.\nExit: %v\nOutput: %s\nProfile: %s",
				target, runErr, string(out), profilePath)
		}
	} else if os.IsNotExist(statErr) {
		// File absent on the host: the open must have been attempted and
		// failed with ENOENT — the sane missing-file path nix tolerates.
		if runErr == nil {
			t.Errorf("cat of missing file %s exited 0 — test environment is inconsistent.\nOutput: %s",
				target, string(out))
		}
		if !strings.Contains(string(out), "No such file or directory") {
			t.Errorf("cat of missing file %s did not fail with ENOENT; the sandbox may be interfering.\nExit: %v\nOutput: %s\nProfile: %s",
				target, runErr, string(out), profilePath)
		}
	} else {
		t.Fatalf("stat %s: %v", target, statErr)
	}
}

// TestSandboxExecProfile_NixTrustedSettingsDeniedWithoutRule is the paired
// negative test: with the trusted-settings allow block removed from the
// generated profile, the same read must fail with EPERM. This proves the
// positive test exercises the rule itself rather than passing by accident
// (e.g. via some broader allow).
func TestSandboxExecProfile_NixTrustedSettingsDeniedWithoutRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	target := nixTrustedSettingsPath(t)

	m := newProfileManagerWithBareRoot(t)

	// Remove the entire self-contained allow block as emitted by
	// generateProfile. withMutatedProfile fails the test if the substitution
	// does not match anything, so format drift in the generator is caught.
	ruleBlock := "(allow file-read* file-test-existence\n" +
		"  (literal " + sbplQuoteForTest(target) + "))\n"
	profilePath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, ruleBlock, "", 1)
	})

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of %s succeeded WITHOUT the trusted-settings allow rule — some broader allow\n"+
			"covers the path and the positive test is not isolating the rule under test.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	}
	if !strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("read of %s without the allow rule did not fail with EPERM.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}
}
