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
// Each test runs with a test-owned HOME (useTestHome) and a planted
// profiles.json in it, so no test reads or writes the real profiles config
// of the user. A real home-manager profiles.json is a symlink into
// /nix/store, where a broader allow covers the read, so the negative test
// needs a regular file.
//
//   - TestSandboxExecProfile_PrismProfilesJSONReadable proves the generated
//     profile permits opening the file for reading: the read must exit 0.
//
//   - TestSandboxExecProfile_PrismProfilesJSONDeniedWithoutRule mutates the
//     profile to remove the section-5h allow block and asserts the same
//     read fails with "Operation not permitted" — proving the positive
//     test is not green by accident (for example, via some broader allow).
//
//   - TestSandboxExecProfile_PrismProfilesJSONWriteDenied proves the
//     security AC: under the PRODUCTION profile, writing to profiles.json
//     from inside the sandbox fails — RO must not silently become RW.
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

// prismProfilesJSONTarget returns the path of the prism declarative profiles
// config, derived the same way generateProfile derives it
// (os.UserHomeDir() + .config/prism/profiles.json). Call useTestHome first,
// so the path is in the test home.
func prismProfilesJSONTarget(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	return filepath.Join(home, ".config", "prism", "profiles.json")
}

// prismProfilesJSONPath returns prismProfilesJSONTarget and plants a regular
// file there.
func prismProfilesJSONPath(t *testing.T) string {
	t.Helper()
	path := prismProfilesJSONTarget(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("plant %s: %v", path, err)
	}
	return path
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
	useTestHome(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONPath(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()

	if strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("read of %s was denied by the sandbox (EPERM) under the production profile.\n"+
			"The prism profiles.json allow (issue #2286) is missing or not load-bearing —\n"+
			"`prism profile list` / `prism profile show` / `prism agent-context` will fail inside worker sandboxes.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}

	if runErr != nil {
		t.Errorf("the sandboxed read of %s failed.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
	}
}

// TestSandboxExecProfile_PrismProfilesJSONMissingKeepsENOENT is the
// missing-file case of #2286: with no profiles.json, the sandboxed open must
// fail with ENOENT ("No such file or directory"), the error LoadProfiles
// already handles, and not with EPERM ("Operation not permitted").
func TestSandboxExecProfile_PrismProfilesJSONMissingKeepsENOENT(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	useTestHome(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONTarget(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", "cat "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("cat of missing file %s exited 0.\nOutput: %s", target, string(out))
	}
	if !strings.Contains(string(out), "No such file or directory") {
		t.Errorf("cat of missing file %s did not fail with ENOENT, so the sandbox changed the result.\nExit: %v\nOutput: %s\nProfile: %s",
			target, runErr, string(out), profilePath)
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
	useTestHome(t)
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
// We attempt to append a sentinel byte to the planted file and assert the
// append fails; then we double-check the file is unchanged.
func TestSandboxExecProfile_PrismProfilesJSONWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	useTestHome(t)
	nixBash := requireNixBash(t)
	target := prismProfilesJSONPath(t)

	infoBefore, statErr := os.Stat(target)
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
