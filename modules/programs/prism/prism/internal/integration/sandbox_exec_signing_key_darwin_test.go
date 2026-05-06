//go:build darwin

package integration_test

// sandbox_exec_signing_key_darwin_test.go — integration coverage for the
// signing-key sops rotation fix (issue #1410).
//
// The staging HOME's signing-key.pub symlink now uses symlinkIfExists (not
// symlinkIfResolvable), so it points at the stable sops intermediate symlink
// (~/.ssh/prismatic-koi-ed25519-signingkey.pub) rather than the fully-resolved
// sops temp path. After a sops secrets.d/<N>/ rotation, the staging HOME
// symlink remains valid because it anchors to the intermediate, not the
// concrete path.
//
// These tests verify that the sandbox can follow the two-hop chain
//
//   <stagingHome>/.ssh/signing-key.pub
//     → <sshIntermediate>                 (simulates the stable sops symlink)
//     → <concreteFile>                    (simulates secrets.d/<N>/signing-key.pub)
//
// and read the final content through sandbox-exec. The concrete file is
// placed under $HOME (via hostCredentialDir) — not under /private/var/folders/
// — so that the relevant SBPL rule is the per-symlink (literal ...) emitted by
// collectStagingHomeSymlinkTargets, not the broad (subpath "/private/var/folders")
// rule. This makes the negative test meaningful: stripping that (literal ...)
// line causes the read to fail.
//
// Two-hop chain design notes
// ──────────────────────────
// The chain traverses two directories outside the staging HOME:
//
//   1. The SSH intermediate dir (simulating ~/.ssh/). On Darwin, sandbox-exec
//      requires file-read-metadata permission to follow symlinks inside a
//      directory. The production profile grants (subpath HOME) for
//      file-test-existence and file-read-metadata via the BareRoot ancestor
//      block, which lets the kernel traverse symlinks in $HOME/.ssh/.
//      We therefore use newProfileManagerWithBareRoot() to activate this block.
//
//   2. The concrete file lives under $HOME (via hostCredentialDir) so the
//      per-symlink (literal <concrete>) allow rule emitted by
//      collectStagingHomeSymlinkTargets is the authoritative grant — not the
//      broader /private/var/folders allow. This makes the negative test
//      sensitive to removal of that specific literal.
//
// Shared helpers: requireSandboxExec, requireNixBash, newProfileManagerWithBareRoot,
// preparePositiveProfile, writeAugmentedPositiveProfile, withMutatedProfile,
// hostCredentialDir, sbplQuoteForTest — all in sandbox_exec_helpers_darwin_test.go.
//
// See docs/sandbox-exec-testing.md for the convention these tests support (#1192).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupSigningKeyTwoHopChain creates a two-hop symlink chain that simulates
// the sops-managed signing key layout. It returns:
//
//   - sshIntermediateDir: directory that contains the intermediate symlink
//     (simulates ~/.ssh/)
//   - concreteFile: the actual key content file (simulates secrets.d/<N>/key.pub)
//   - intermediateSymlink: path of the intermediate symlink
//     (<sshIntermediateDir>/prismatic-koi-ed25519-signingkey.pub)
//
// Layout:
//
//	sshIntermediateDir/prismatic-koi-ed25519-signingkey.pub
//	  → concreteFile
//	    (contains sentinel content)
//
// The concrete file is placed under $HOME (via hostCredentialDir) so the
// per-symlink (literal ...) SBPL rule emitted by collectStagingHomeSymlinkTargets
// is the authoritative read grant — not the broad /private/var/folders allow.
//
// The caller is responsible for creating staging HOME symlinks that point
// at the intermediate symlink (simulating symlinkIfExists behavior).
func setupSigningKeyTwoHopChain(t *testing.T, sentinel string) (sshIntermediateDir, concreteFile, intermediateSymlink string) {
	t.Helper()

	// Concrete file under $HOME (not /private/var/folders) so the
	// per-symlink literal rule is load-bearing for the negative test.
	concreteDir := hostCredentialDir(t)
	concreteFile = filepath.Join(concreteDir, "signing-key.pub")
	if err := os.WriteFile(concreteFile, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write concrete signing key file: %v", err)
	}

	// sshIntermediateDir simulates ~/.ssh/. It must be under HOME so the
	// BareRoot ancestor block (subpath HOME) provides file-read-metadata,
	// letting the kernel follow the symlink stored here.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	sshIntermediateDir, err = os.MkdirTemp(home, ".prism-1410-ssh-*")
	if err != nil {
		t.Fatalf("MkdirTemp for sshIntermediateDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sshIntermediateDir) })

	// The intermediate symlink: simulates ~/.ssh/prismatic-koi-ed25519-signingkey.pub
	intermediateSymlink = filepath.Join(sshIntermediateDir, "prismatic-koi-ed25519-signingkey.pub")
	if err := os.Symlink(concreteFile, intermediateSymlink); err != nil {
		t.Fatalf("create intermediate symlink: %v", err)
	}

	return sshIntermediateDir, concreteFile, intermediateSymlink
}

// TestSandboxExecSigningKey_TwoHopChainReadable is the positive integration
// test for the sops-rotation fix (issue #1410).
//
// It sets up the two-hop chain:
//
//	<stagingHome>/.ssh/signing-key.pub
//	  → <sshIntermediateDir>/prismatic-koi-ed25519-signingkey.pub  (intermediate)
//	  → <concreteFile>                                              (final key content)
//
// Generates the production SBPL profile, then reads the signing key file
// via the staging HOME path inside sandbox-exec. Asserts exit 0 and that
// the sentinel content is visible — confirming the sandbox can follow the
// full two-hop chain.
//
// Uses newProfileManagerWithBareRoot so the BareRoot ancestor block fires
// and grants file-read-metadata on (subpath HOME), enabling the kernel to
// traverse the intermediate symlink inside sshIntermediateDir.
func TestSandboxExecSigningKey_TwoHopChainReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	const sentinel = "ssh-ed25519 AAAA1410 signing-key-sentinel"

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	_, concreteFile, intermediateSymlink := setupSigningKeyTwoHopChain(t, sentinel)

	// Install the staging HOME signing-key.pub symlink → intermediate
	// (simulating the symlinkIfExists behavior from the #1410 fix).
	signingKeyLink := filepath.Join(stagingHome, ".ssh", "signing-key.pub")
	_ = os.Remove(signingKeyLink) // replace whatever PrepareSandboxExecHome placed
	if err := os.Symlink(intermediateSymlink, signingKeyLink); err != nil {
		t.Fatalf("create staging signing-key.pub → intermediate: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must reference the concrete file (resolved by
	// collectStagingHomeSymlinkTargets via filepath.EvalSymlinks).
	resolved, _ := filepath.EvalSymlinks(concreteFile)
	if resolved == "" {
		resolved = concreteFile
	}
	if !strings.Contains(prepared.content, resolved) {
		t.Fatalf("generated profile does not reference the resolved concrete signing key path %q.\n"+
			"collectStagingHomeSymlinkTargets did not pick up the staging signing-key.pub symlink chain.\nProfile:\n%s",
			resolved, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read the signing key file from inside the sandbox via the staging HOME
	// path. The kernel must traverse: signingKeyLink → intermediate → concreteFile.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", fmt.Sprintf("HOME=%s", stagingHome), nixBash, "-c",
		"cat "+shQuote(signingKeyLink))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read signing key via two-hop staging-HOME chain failed under production profile.\n"+
			"This means the sandbox cannot follow the sops-style symlink chain required by #1410.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), sentinel) {
		t.Errorf("signing key read succeeded but output does not contain the sentinel.\nOutput: %s", string(out))
	}
}

// TestSandboxExecSigningKey_TwoHopChainDeniedWithoutConcreteAllow is the
// paired negative test. It removes the (literal <concreteFile>) allow line
// from the profile and asserts that reading the signing key via the staging
// HOME symlink chain fails.
//
// This proves TestSandboxExecSigningKey_TwoHopChainReadable is not green by
// accident: the read works specifically because of the (literal <concreteFile>)
// allow emitted by collectStagingHomeSymlinkTargets, not because of a
// broader rule.
//
// The concrete file is under $HOME (not /private/var/folders) so the broad
// (subpath "/private/var/folders") rule does NOT cover it — only the
// per-symlink literal allow does. Stripping that literal makes the read
// fail, confirming the two-hop chain is load-bearing.
func TestSandboxExecSigningKey_TwoHopChainDeniedWithoutConcreteAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	const sentinel = "ssh-ed25519 AAAA1410-neg signing-key-sentinel-neg"

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	_, concreteFile, intermediateSymlink := setupSigningKeyTwoHopChain(t, sentinel)

	signingKeyLink := filepath.Join(stagingHome, ".ssh", "signing-key.pub")
	_ = os.Remove(signingKeyLink)
	if err := os.Symlink(intermediateSymlink, signingKeyLink); err != nil {
		t.Fatalf("create staging signing-key.pub → intermediate: %v", err)
	}

	// Resolve the concrete path to its canonical form (the same form
	// generateProfile emits via filepath.EvalSymlinks).
	resolved, err := filepath.EvalSymlinks(concreteFile)
	if err != nil {
		resolved = concreteFile
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// Remove the (literal "<resolved>") line from the allow block.
		// Match only the indented form to avoid accidentally hitting
		// unrelated occurrences.
		toRemove := "  (literal " + sbplQuoteForTest(resolved) + ")\n"
		return strings.ReplaceAll(p, toRemove, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", fmt.Sprintf("HOME=%s", stagingHome), nixBash, "-c",
		"cat "+shQuote(signingKeyLink))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("signing key read via staging-HOME chain succeeded WITHOUT the (literal ...) allow for the concrete file.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — signing key read correctly denied without concrete-file allow (exit: %v)", runErr)
	}
}

// TestSandboxExecSigningKey_DanglingSymlinkFails verifies that a dangling
// staging HOME symlink (pointing directly at a concrete sops path that has
// since been rotated away) causes cat to fail — demonstrating why the
// #1410 fix (pointing at the intermediate instead) is necessary.
//
// This is not a profile mutation test; it tests symlink staleness rather
// than an SBPL rule. It documents the failure mode that the fix prevents
// and confirms the test suite can detect it.
func TestSandboxExecSigningKey_DanglingSymlinkFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Create a temp "concrete v1" file, point staging home directly at it
	// (simulating the old symlinkIfResolvable behaviour).
	home, homeErr := os.UserHomeDir()
	if homeErr != nil || home == "" {
		t.Skipf("cannot determine user home: %v", homeErr)
	}
	v1Dir, err := os.MkdirTemp(home, ".prism-1410-v1-*")
	if err != nil {
		t.Fatalf("MkdirTemp for v1Dir: %v", err)
	}
	v1File := filepath.Join(v1Dir, "signing-key.pub")
	if err := os.WriteFile(v1File, []byte("ssh-ed25519 AAAA1410 v1"), 0o600); err != nil {
		t.Fatalf("write v1 concrete key: %v", err)
	}

	// Point staging HOME directly at the concrete v1 path (old behaviour).
	signingKeyLink := filepath.Join(stagingHome, ".ssh", "signing-key.pub")
	_ = os.Remove(signingKeyLink)
	if err := os.Symlink(v1File, signingKeyLink); err != nil {
		t.Fatalf("create staging signing-key.pub → v1 concrete: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Simulate sops rotation: delete v1 dir (key now gone at the concrete path).
	_ = os.RemoveAll(v1Dir)

	// With a dangling staging HOME symlink, cat must fail.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", fmt.Sprintf("HOME=%s", stagingHome), nixBash, "-c",
		"cat "+shQuote(signingKeyLink))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("reading a dangling staging HOME signing-key.pub symlink succeeded — expected failure.\n"+
			"This means the negative baseline for #1410 is not working correctly.\n"+
			"Output: %s\nProfile: %s", string(out), testProfilePath)
	} else {
		t.Logf("ka pai — dangling symlink correctly causes failure (exit: %v): this is the failure the #1410 fix prevents", runErr)
	}
}
