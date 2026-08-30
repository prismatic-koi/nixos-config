//go:build darwin

package integration_test

// sandbox_exec_session_work_dir_xdg_state_home_darwin_test.go — Darwin
// integration coverage for the XDG_STATE_HOME branch of
// container.SessionWorkDirPath.
//
// Why this test exists. Inside the nix-build homeless-shelter sandbox
// $HOME=/homeless-shelter is read-only. A path derived from it fails on
// os.MkdirAll. SessionWorkDirPath honours $XDG_STATE_HOME first (XDG Base
// Directory Specification) and falls back to $HOME/.local/state only when it
// is unset. Integration test helpers in sandbox_exec_helpers_darwin_test.go
// redirect XDG_STATE_HOME to a t.TempDir() so the work dir is writable
// regardless of $HOME.
//
// The positive test asserts:
//
//   1. The derived sessionDir is rooted at $XDG_STATE_HOME (not $HOME),
//      proving the new branch is actually taken.
//   2. The generated SBPL profile contains (subpath "<sessionDir>") for
//      the XDG_STATE_HOME-derived path.
//   3. A write into sessionDir from inside /usr/bin/sandbox-exec succeeds.
//
// The paired negative test re-targets the (subpath "<sessionDir>") entry
// at a non-existent sibling path (the same mutation strategy as
// TestSandboxExecSessionWorkDir_DeniedWithoutSubpath) and asserts the
// write fails — proving the rule under test is what grants access to the
// XDG_STATE_HOME-derived path, not a side effect of some unrelated allow.
//
// This is a focused complement to TestSandboxExecSessionWorkDir_*, which
// covers the work-dir RW rule at the API level. Those tests already exercise
// the XDG_STATE_HOME-derived path (because newProfileManager sets
// XDG_STATE_HOME). This test pins the path-derivation contract explicitly.
// If a future change reverts SessionWorkDirPath to HOME-only, this test
// fails with a precise error message rather than an opaque homeless-shelter
// mkdir failure.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSandboxExecSessionWorkDir_XDGStateHomeBranch_GrantsWrites is the
// positive half of the XDG_STATE_HOME-branch coverage. It asserts
// that the sessionDir derived from XDG_STATE_HOME (set by
// newProfileManager) is covered by the (subpath "<sessionDir>") rule in
// the generated profile, and that a write into it from inside the sandbox
// succeeds.
func TestSandboxExecSessionWorkDir_XDGStateHomeBranch_GrantsWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	// newProfileManager sets XDG_STATE_HOME=t.TempDir(). Capture it
	// here so we can assert sessionDir is rooted under it, not under $HOME.
	m := newProfileManager(t)
	xdgStateHome := os.Getenv("XDG_STATE_HOME")
	if xdgStateHome == "" {
		t.Fatalf("XDG_STATE_HOME is empty after newProfileManager — the helper's #2263 redirect is not in effect")
	}

	sessionDir, prepared := sessionWorkDirFixture(t, m)

	// sessionDir must live under XDG_STATE_HOME, not under $HOME. This pins
	// the branch in SessionWorkDirPath.
	if !strings.HasPrefix(sessionDir, xdgStateHome) {
		t.Fatalf("sessionDir %q is not rooted at XDG_STATE_HOME %q — the XDG_STATE_HOME branch in SessionWorkDirPath is not active",
			sessionDir, xdgStateHome)
	}
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" && strings.HasPrefix(sessionDir, filepath.Join(homeDir, ".local", "state")) {
		t.Fatalf("sessionDir %q is rooted under $HOME/.local/state %q — XDG_STATE_HOME should have taken precedence",
			sessionDir, filepath.Join(homeDir, ".local", "state"))
	}

	// The generated profile must contain the (subpath ...) rule for
	// the XDG_STATE_HOME-derived sessionDir. sessionWorkDirFixture already
	// asserts this; the re-check here is the explicit precondition for the
	// write test below.
	want := "(subpath " + sbplQuoteForTest(sessionDir) + ")"
	if !strings.Contains(prepared.content, want) {
		t.Fatalf("generated profile missing %q for the XDG_STATE_HOME-derived sessionDir.\nProfile:\n%s",
			want, prepared.content)
	}

	// A write into sessionDir from inside sandbox-exec must succeed.
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)
	probe := filepath.Join(sessionDir, "prism-2263-xdg-write-probe.tmp")
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "echo hi > "+shQuote(probe))
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("write to XDG_STATE_HOME-derived sessionDir failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	if _, statErr := os.Stat(probe); statErr != nil {
		t.Errorf("write probe exited 0 but probe file is missing: %v", statErr)
	}
}

// TestSandboxExecSessionWorkDir_XDGStateHomeBranch_DeniedWithoutSubpath
// is the paired negative test. It mutates the profile to re-target the
// (subpath "<sessionDir>") entry — the SBPL rule that grants access to
// the XDG_STATE_HOME-derived sessionDir — at a non-existent sibling path,
// and asserts the same write now fails. This proves the positive test is
// not green by accident: the (subpath ...) rule against the
// XDG_STATE_HOME-derived path is the load-bearing grant.
//
// Mutation strategy mirrors TestSandboxExecSessionWorkDir_DeniedWithoutSubpath
// (ReplaceAll on the quoted path rather than line deletion), which keeps
// the SBPL syntactically valid regardless of where the entry sits in its
// allow block.
func TestSandboxExecSessionWorkDir_XDGStateHomeBranch_DeniedWithoutSubpath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	xdgStateHome := os.Getenv("XDG_STATE_HOME")
	if xdgStateHome == "" {
		t.Fatalf("XDG_STATE_HOME is empty after newProfileManager — the helper's #2263 redirect is not in effect")
	}

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	if !strings.HasPrefix(sessionDir, xdgStateHome) {
		t.Fatalf("sessionDir %q is not rooted at XDG_STATE_HOME %q — the negative test would be checking the wrong path",
			sessionDir, xdgStateHome)
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			sbplQuoteForTest(sessionDir),
			sbplQuoteForTest(sessionDir+".prism-2263-disabled"))
	})

	probe := filepath.Join(sessionDir, "prism-2263-xdg-write-probe-denied.tmp")
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "echo hi > "+shQuote(probe))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("write into XDG_STATE_HOME-derived sessionDir succeeded WITHOUT the (subpath ...) rule.\n"+
			"The XDG_STATE_HOME positive test is not isolated — some other allow grants the write.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — write correctly denied without the XDG_STATE_HOME-derived subpath rule (exit: %v)", runErr)
	}
}
