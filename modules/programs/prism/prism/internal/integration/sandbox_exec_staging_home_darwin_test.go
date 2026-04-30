//go:build darwin

package integration_test

// sandbox_exec_staging_home_darwin_test.go — integration coverage for the
// staging-HOME read/write rules in the SBPL profile (issue #1192 AC #4):
//
//   - Staging HOME write allow: a binary inside the sandbox can write a
//     regular file under $HOME (the staging HOME path).
//   - Staging-HOME credential read via symlink: the sandbox can dereference
//     a staged symlink at $HOME/.config/aws/credentials → host-side resolved
//     path, and read the target file.
//
// Each positive test has a paired negative test that mutates the production
// profile to remove the rule under test, asserts the operation fails, and
// thereby proves the positive is not green by accident (per the convention
// in docs/sandbox-exec-testing.md).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxExecProfile_StagingHomeWritable is the positive integration
// test for the staging-HOME write allow. It generates the production
// profile, prepares the staging HOME, and runs `bash -c 'echo hi > $HOME/foo'`
// inside the sandbox. Asserts the file is created (exit 0).
func TestSandboxExecProfile_StagingHomeWritable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	target := filepath.Join(stagingHome, "prism-1192-write-probe.tmp")
	t.Cleanup(func() { _ = os.Remove(target) })

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("write to staging HOME failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nTarget: %s\nProfile: %s",
			runErr, string(out), target, testProfilePath)
	}

	// Verify the file was actually created on the host.
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("write probe exited 0 but target file is missing: %v", statErr)
	}
}

// TestSandboxExecProfile_StagingHomeWriteDenied is the paired negative test.
// It removes the single (subpath "<stagingHome>") line from the
// staging-HOME write allow block and asserts the same write operation
// fails with a non-zero exit.
//
// Mutation strategy: targeted single-line ReplaceAll on the indented
// `  (subpath "<stagingHome>")\n` form. This leaves the rest of the
// (allow file-read* file-write* ...) block intact (worktree, bare repo,
// host-API socket dir, opencode shared dirs) so the profile remains
// syntactically valid SBPL — the only behaviour change is that the
// staging-HOME path itself is no longer covered by the write allow,
// which is exactly the rule we are testing. Removing the entire block
// would make the profile malformed (orphaned subpath lines from the
// trailing entries in the block) and sandbox-exec would reject the
// profile at parse time, making the test pass for the wrong reason.
//
// This proves StagingHomeWritable is not green by accident — the staging
// HOME is writable specifically because of the (subpath "<stagingHome>")
// entry in the write allow block, not because of some unrelated rule.
func TestSandboxExecProfile_StagingHomeWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	stagingHomeRule := "  (subpath " + sbplQuoteForTest(stagingHome) + ")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, stagingHomeRule, "")
	})

	// Sanity check: the mutated profile must still be syntactically valid
	// SBPL — assert the close-paren of the parent (allow ...) block is
	// still present and balanced. This guards against a future generator
	// change that puts stagingHome in a different block shape: if the
	// ReplaceAll above ever truncates the block close, parens would not
	// balance and sandbox-exec would reject the profile, making the test
	// pass for the wrong reason.
	if mutatedContent, readErr := os.ReadFile(mutatedPath); readErr != nil {
		t.Fatalf("read mutated profile: %v", readErr)
	} else if open, close := strings.Count(string(mutatedContent), "("), strings.Count(string(mutatedContent), ")"); open != close {
		t.Fatalf("mutated profile has unbalanced parentheses (%d open, %d close) — mutation produced malformed SBPL.\nProfile: %s",
			open, close, mutatedPath)
	}

	target := filepath.Join(stagingHome, "prism-1192-write-probe-denied.tmp")
	t.Cleanup(func() { _ = os.Remove(target) })

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c", "echo hi > "+shQuote(target))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write to staging HOME succeeded WITHOUT the staging-HOME (subpath ...) entry.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — write correctly denied without staging-HOME allow (exit: %v)", runErr)
	}
}

// hostCredentialDir creates a host-side directory that the credential
// sentinel file will be planted in. Crucially it is NOT placed under
// t.TempDir(): on Darwin t.TempDir() returns a path under /private/var/folders/,
// which the production profile already broadly allows under the system-paths
// read rule (subpath "/private/var/folders"). A credential file there would
// remain readable inside the sandbox even with the symlink-target (literal
// ...) allow stripped, defeating the negative test.
//
// Instead we place the credential dir directly under the user's HOME (a
// subdir of HOME is otherwise unreadable inside the sandbox unless an
// explicit symlink-target allow grants it).
func hostCredentialDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".prism-1192-test-cred-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestSandboxExecProfile_StagingCredentialReadable is the positive test for
// "staging-HOME-via-symlink reads of credential targets" (AC #4).
//
// The test plants a sentinel credential file directly under HOME (NOT
// /private/var/folders/, which is broadly allowed by the system-paths rule),
// creates a symlink at $stagingHome/.aws/credentials → that file, then
// generates the production profile (which resolves the symlink and emits a
// (literal "<resolved>") allow for the target). It reads the file from
// inside the sandbox via the staging HOME path and asserts the read
// succeeds.
//
// The negative test in StagingCredentialDeniedWithoutTargetAllow strips
// that (literal ...) allow line from the profile and asserts the same read
// fails — proving the read works specifically because of the symlink-
// target allow, not because of a broader rule.
func TestSandboxExecProfile_StagingCredentialReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Use the BareRoot variant: traversal of the resolved credential path
	// under HOME requires the BareRoot-ancestor block's
	// file-read-metadata allow on (subpath HOME).
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Plant a sentinel credential file under HOME (deliberately not under
	// /private/var/folders, which is broadly allowed) and create a symlink
	// inside the staging .aws/ pointing at it.
	credDir := hostCredentialDir(t)
	hostCredentialFile := filepath.Join(credDir, "credentials")
	const sentinel = "[default]\naws_access_key_id = sentinel-1192\n"
	if err := os.WriteFile(hostCredentialFile, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write host credential file: %v", err)
	}

	stagingCredentialLink := filepath.Join(stagingHome, ".aws", "credentials")
	_ = os.Remove(stagingCredentialLink) // overwrite whatever PrepareSandboxExecHome may have placed
	if err := os.Symlink(hostCredentialFile, stagingCredentialLink); err != nil {
		t.Fatalf("create staging credentials symlink: %v", err)
	}

	prepared, _ := preparePositiveProfile(t, m)
	// The resolved canonical path emitted by the profile generator may
	// differ from hostCredentialFile by the /var → /private/var symlink
	// resolution. Use EvalSymlinks here to compute the canonical form for
	// substring assertions and use the original path for the in-sandbox
	// cat (the kernel resolves the symlink chain transparently for the
	// shell-side syscall).
	resolved, err := filepath.EvalSymlinks(hostCredentialFile)
	if err != nil {
		resolved = hostCredentialFile
	}
	if !strings.Contains(prepared.content, resolved) {
		t.Fatalf("generated profile does not reference the resolved host credential path %q.\n"+
			"The symlink-target resolver did not pick up the staging .aws/credentials symlink.\nProfile:\n%s",
			resolved, prepared.content)
	}
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read the file from inside the sandbox via the staging HOME path. The
	// kernel must dereference $HOME/.aws/credentials → hostCredentialFile,
	// which the profile allows.
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(filepath.Join(stagingHome, ".aws", "credentials")))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read staged credential symlink failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "sentinel-1192") {
		t.Errorf("read succeeded but output does not contain the sentinel marker.\nOutput: %s", string(out))
	}
}

// TestSandboxExecProfile_StagingCredentialDeniedWithoutTargetAllow is the
// paired negative test for StagingCredentialReadable. It mutates the
// generated profile to strip the resolved host credential path from the
// symlink-target allow, then asserts that reading the credential via the
// staging HOME symlink fails.
//
// The mutation strategy is to delete the entire line containing the
// resolved path under the (allow file-read* ...) block emitted by the
// symlink-target collector. We rely on the fact that the resolved path is
// a per-test directory under HOME (planted by hostCredentialDir, which
// places the credential under HOME deliberately so the broadly-allowed
// /private/var/folders subtree does NOT cover it — see the helper's
// commentary). The path is unique to this test invocation.
func TestSandboxExecProfile_StagingCredentialDeniedWithoutTargetAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// Same BareRoot-bearing Manager as the positive test. The mutation under
	// test is the (literal "<resolved>") allow for the credential target,
	// not the BareRoot ancestor block.
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	credDir := hostCredentialDir(t)
	hostCredentialFile := filepath.Join(credDir, "credentials")
	const sentinel = "[default]\naws_access_key_id = sentinel-1192-neg\n"
	if err := os.WriteFile(hostCredentialFile, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write host credential file: %v", err)
	}

	stagingCredentialLink := filepath.Join(stagingHome, ".aws", "credentials")
	_ = os.Remove(stagingCredentialLink)
	if err := os.Symlink(hostCredentialFile, stagingCredentialLink); err != nil {
		t.Fatalf("create staging credentials symlink: %v", err)
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		// Resolve to the canonical form generateProfile emits via
		// filepath.EvalSymlinks (covers the /var → /private/var alias).
		resolved, err := filepath.EvalSymlinks(hostCredentialFile)
		if err != nil {
			resolved = hostCredentialFile
		}
		// Remove the (literal "<resolved>") line from the allow block.
		// Match only the indented "  (literal ..." form so we don't
		// accidentally hit any unrelated occurrences.
		toRemove := "  (literal " + sbplQuoteForTest(resolved) + ")\n"
		return strings.ReplaceAll(p, toRemove, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+stagingHome, nixBash, "-c",
		"cat "+shQuote(filepath.Join(stagingHome, ".aws", "credentials")))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read staged credential succeeded WITHOUT the (literal ...) allow for the resolved target.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — credential read correctly denied without target allow (exit: %v)", runErr)
	}
}

// sbplQuoteForTest is a test-local copy of the SBPL quoter from
// sandbox_exec.go (which is unexported). It must produce identical output
// to container.quoteSBPL for the substring match in
// StagingCredentialDeniedWithoutTargetAllow to find the line.
func sbplQuoteForTest(path string) string {
	escaped := strings.ReplaceAll(path, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}
