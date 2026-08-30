//go:build darwin

package integration_test

// sandbox_exec_profiles_json_darwin_test.go — integration coverage for the
// single-file read-only allow on ~/.config/prism/profiles.json.
//
// The prism CLI's `prism profile list`, `prism profile show`, and the
// `available_profiles` section of `prism agent-context` open this file
// directly via internal/config/profiles.go::LoadProfiles. Without this allow
// the read fails EPERM and the user sees a misleading "profiles: <path>
// not found — run the system rebuild" message from inside any sandbox-exec
// session, even though the file exists on the host (the mutation surface
// `prism profile use` routes through the host API instead and is
// unaffected).
//
// Per docs/sandbox-exec-testing.md the coverage is a positive/negative pair
// plus a write-denied negative for the RO/security guarantee:
//
//   - TestSandboxExecProfile_PrismProfilesJSONReadable proves the generated
//     profile permits opening the file for reading. The test NEVER creates
//     or modifies the real host file (it is the user's actual prism
//     profiles config), so it asserts on whichever state the host is in:
//     when the file exists the read must exit 0; when it is absent the
//     open must be attempted and fail with ENOENT ("No such file or
//     directory") — NOT EPERM ("Operation not permitted"). Both branches
//     prove the sandbox allowed the open. The ENOENT branch also covers
//     the missing-file case: a missing host file follows LoadProfiles'
//     normal missing-file path (preserving the existing error message)
//     instead of failing for sandbox reasons.
//
//   - TestSandboxExecProfile_PrismProfilesJSONDeniedWithoutRule mutates the
//     profile to remove the section-5h allow block and asserts the same
//     read fails with "Operation not permitted" — proving the positive
//     test is not green by accident (for example, via some broader allow).
//
//   - TestSandboxExecProfile_PrismProfilesJSONWriteDenied proves the
//     security AC: under the PRODUCTION profile, writing to profiles.json
//     from inside the sandbox fails — RO must not silently become RW.
//     Skipped when the file is absent on the host (no target to attempt a
//     write against without disturbing the user's real config).
//
// All three tests use newProfileManagerWithBareRoot: production worker
// sessions always have BareRoot set, so the ancestor metadata block is
// present, and the rule under test grants file-read-data which the
// BareRoot ancestor block (file-test-existence/file-read-metadata only)
// cannot mask in the negative test. This mirrors the trusted-settings
// sibling test in sandbox_exec_nix_trusted_settings_darwin_test.go.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// prismProfilesJSONPath returns the host path of the prism declarative
// profiles config, derived the same way generateProfile derives it
// (os.UserHomeDir() + .config/prism/profiles.json).
func prismProfilesJSONPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	return filepath.Join(home, ".config", "prism", "profiles.json")
}

// TestSandboxExecProfile_PrismProfilesJSONReadable is the positive half:
// under the unmodified production profile, opening
// ~/.config/prism/profiles.json for reading must not be denied by the
// sandbox.
func TestSandboxExecProfile_PrismProfilesJSONReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONPath(t)

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
			"The prism profiles.json allow (issue #2286) is missing or not load-bearing —\n"+
			"`prism profile list` / `prism profile show` / `prism agent-context` will fail inside worker sandboxes.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}

	// Branch on actual host state — the test must never create or modify
	// the user's real profiles.json.
	if _, statErr := os.Stat(target); statErr == nil {
		// File exists on the host: the sandboxed read must succeed outright.
		if runErr != nil {
			t.Errorf("host file %s exists but the sandboxed read failed.\nExit: %v\nOutput: %s\nProfile: %s",
				target, runErr, string(out), profilePath)
		}
	} else if os.IsNotExist(statErr) {
		// File absent on the host: the open must have been attempted and
		// failed with ENOENT — the sane missing-file path LoadProfiles
		// tolerates (the missing-file case: the existing host error message
		// must be preserved on a fresh install).
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

// TestSandboxExecProfile_PrismProfilesJSONDeniedWithoutRule is the paired
// negative test: with the section-5h allow block removed from the generated
// profile, the same read must fail with EPERM. This proves the positive
// test exercises the rule itself rather than passing by accident (for
// example, via some broader allow).
func TestSandboxExecProfile_PrismProfilesJSONDeniedWithoutRule(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONPath(t)

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
		t.Errorf("read of %s succeeded WITHOUT the profiles.json allow rule — some broader allow\n"+
			"covers the path and the positive test is not isolating the rule under test.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	}
	if !strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("read of %s without the allow rule did not fail with EPERM.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}
}

// TestSandboxExecProfile_PrismProfilesJSONWriteDenied is the security
// guarantee: under the PRODUCTION profile, writing to profiles.json
// from inside the sandbox fails — RO must not silently become RW. This
// guards against a future change that widens the (literal ...) RO rule
// into a (subpath ...) RW grant or promotes the allow to include
// file-write*.
//
// The test does not need to (and must not) actually overwrite the user's
// real profiles.json: the sandbox deny must fire BEFORE any host-side
// write. We attempt to append a sentinel byte and assert the append fails;
// then we double-check the file is unchanged. Skipped when the file is
// absent on the host (no target to write against. The bwrap-side
// fresh-install case is covered separately by the OptionalIfMissing
// unit-test pair in bwrap_test.go).
func TestSandboxExecProfile_PrismProfilesJSONWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONPath(t)

	infoBefore, statErr := os.Stat(target)
	if os.IsNotExist(statErr) {
		t.Skipf("~/.config/prism/profiles.json absent on this host; write-denied AC covered for fresh installs by the bwrap OptionalIfMissing unit tests")
	}
	if statErr != nil {
		t.Fatalf("stat %s: %v", target, statErr)
	}

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Append a sentinel via shell redirection — appending (>>) avoids any
	// truncation if the sandbox somehow lets the open() through but blocks
	// the write(). Either way the assertion fails iff a write succeeds.
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "echo prism-2286-write-probe >> "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to %s succeeded under the production profile — the RO grant must not silently become RW.\nOutput: %s\nProfile: %s",
			target, string(out), profilePath)
	}

	// Belt-and-braces: confirm the host file is unchanged. If the sandbox
	// deny fired correctly, size and mtime are identical to the
	// pre-attempt snapshot.
	infoAfter, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatalf("stat %s after probe: %v", target, statErr)
	}
	if infoAfter.Size() != infoBefore.Size() {
		t.Errorf("profiles.json size changed across the write probe: before=%d after=%d — the sandbox allowed a write",
			infoBefore.Size(), infoAfter.Size())
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Errorf("profiles.json mtime changed across the write probe: before=%v after=%v — the sandbox allowed a write",
			infoBefore.ModTime(), infoAfter.ModTime())
	}
}
