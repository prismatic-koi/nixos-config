//go:build linux

package container

// bare_readonly_bwrap_test.go — Linux-only integration coverage for the
// read-only overlays over the read-write .bare bind (issue #3315).
//
// The sibling unit tests in bwrap_test.go assert the emitted argv. Argv
// assertions are necessary but not sufficient — a bind sequence that the
// kernel rejects, or an overlay order that does not actually shadow, would
// keep every substring test green. These tests spawn a REAL bwrap sandbox
// with the PRODUCTION bind sequence (the read-write .bare bind, the
// bwrapBareReadOnlyOverlays overlays, then the own-worktree read-write bind,
// in the same order BuildArgs composes them) and observe the behaviour the
// acceptance criteria name:
//
//   - .bare/config, .bare/hooks, and .bare/info are read-only: a write or a
//     create under them fails;
//   - a sibling worktree's git dir under .bare/worktrees is read-only;
//   - the session's OWN worktree git dir stays read-write;
//   - the object store under .bare/objects stays read-write (so commit and
//     fetch still work).
//
// The negative test drops the overlays and proves the same writes SUCCEED,
// so the positive tests are not green by accident.
//
// Skips (never fails) when bwrap is absent or the live probe cannot start a
// sandbox. requireUsableBwrapForUsageTest lists the environments.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBareLayout builds a minimal bare+worktree layout under a temp dir and
// returns the .bare dir and the session's own worktree git dir
// (.bare/worktrees/own). It also creates a sibling worktree git dir
// (.bare/worktrees/sibling) and the object store.
func fakeBareLayout(t *testing.T) (bareDir, ownGitDir, siblingGitDir string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, ".bare")
	ownGitDir = filepath.Join(bareDir, "worktrees", "own")
	siblingGitDir = filepath.Join(bareDir, "worktrees", "sibling")
	for _, d := range []string{
		filepath.Join(bareDir, "hooks"),
		filepath.Join(bareDir, "info"),
		filepath.Join(bareDir, "objects"),
		ownGitDir,
		siblingGitDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(bareDir, "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	// The redirect files the overlays pin: the benign top-level commondir, and
	// the own worktree's commondir / gitdir / config.worktree.
	if err := os.WriteFile(filepath.Join(bareDir, "commondir"), []byte(".\n"), 0o644); err != nil {
		t.Fatalf("WriteFile commondir: %v", err)
	}
	for _, f := range []string{"commondir", "gitdir", "config.worktree"} {
		if err := os.WriteFile(filepath.Join(ownGitDir, f), []byte("../..\n"), 0o644); err != nil {
			t.Fatalf("WriteFile own %s: %v", f, err)
		}
	}
	return bareDir, ownGitDir, siblingGitDir
}

// productionBareBinds composes the .bare bind sequence exactly as BuildArgs
// does: the read-write .bare bind, the read-only overlays, the own worktree
// read-write bind, then the own-worktree redirect-file pins last (so they win
// for just those files).
func productionBareBinds(bareDir, ownGitDir string) []string {
	args := []string{"--bind", bareDir, bareDir}
	args = append(args, bwrapBareReadOnlyOverlays(bareDir)...)
	args = append(args, "--bind", ownGitDir, ownGitDir)
	return append(args, bwrapOwnWorktreeReadOnlyPins(ownGitDir)...)
}

// runBareProbe runs script in a minimal bwrap sandbox with the supplied .bare
// mount args. /nix and /bin are read-only so /bin/sh and coreutils resolve;
// the host root is not bound, so the only route to .bare is the binds under
// test. Returns the combined output and the run error.
func runBareProbe(t *testing.T, bwrapBin string, mountArgs []string, script string) (string, error) {
	t.Helper()
	args := []string{
		"--ro-bind", "/nix", "/nix",
		"--ro-bind", "/bin", "/bin",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid",
		"--die-with-parent",
		"--setenv", "PATH", nixCoreutilsBinDir(t),
	}
	args = append(args, mountArgs...)
	args = append(args, "/bin/sh", "-c", script)
	out, err := exec.Command(bwrapBin, args...).CombinedOutput()
	return string(out), err
}

// TestBwrapBareReadOnly_OverlaysLockCodeExecutionPaths runs a real bwrap with
// the production bind sequence and checks that the code-execution paths are
// read-only while the own worktree and the object store stay writable.
func TestBwrapBareReadOnly_OverlaysLockCodeExecutionPaths(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	bareDir, ownGitDir, siblingGitDir := fakeBareLayout(t)

	// The script tries each write and prints OK/FAIL per case. Each write runs
	// in a subshell with `printf`, never the `:` special built-in: a redirect
	// failure on a POSIX special built-in exits the whole script (bash in POSIX
	// mode is /bin/sh on the CI runner), which would mask every later case
	// after the first read-only write. A subshell + regular built-in isolates
	// each case so one blocked write does not stop the others.
	script := strings.Join([]string{
		// Read-only: these must FAIL.
		`if ( printf x >> ` + sh(filepath.Join(bareDir, "config")) + ` ) 2>/dev/null; then echo config-write OK; else echo config-write FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(bareDir, "hooks", "post-checkout")) + ` ) 2>/dev/null; then echo hooks-create OK; else echo hooks-create FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(bareDir, "info", "attributes")) + ` ) 2>/dev/null; then echo info-create OK; else echo info-create FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(siblingGitDir, "config.worktree")) + ` ) 2>/dev/null; then echo sibling-create OK; else echo sibling-create FAIL; fi`,
		// Top-level commondir and the own worktree redirect files: must FAIL.
		`if ( printf x >> ` + sh(filepath.Join(bareDir, "commondir")) + ` ) 2>/dev/null; then echo commondir-write OK; else echo commondir-write FAIL; fi`,
		`if ( printf x >> ` + sh(filepath.Join(ownGitDir, "commondir")) + ` ) 2>/dev/null; then echo own-commondir-write OK; else echo own-commondir-write FAIL; fi`,
		`if ( printf x >> ` + sh(filepath.Join(ownGitDir, "gitdir")) + ` ) 2>/dev/null; then echo own-gitdir-write OK; else echo own-gitdir-write FAIL; fi`,
		`if ( printf x >> ` + sh(filepath.Join(ownGitDir, "config.worktree")) + ` ) 2>/dev/null; then echo own-cfgwt-write OK; else echo own-cfgwt-write FAIL; fi`,
		// Read-write: these must succeed.
		`if ( printf '' > ` + sh(filepath.Join(ownGitDir, "HEAD.lock")) + ` ) 2>/dev/null; then echo own-create OK; else echo own-create FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(bareDir, "objects", "tmp-pack")) + ` ) 2>/dev/null; then echo objects-create OK; else echo objects-create FAIL; fi`,
	}, "\n")

	out, err := runBareProbe(t, bwrapBin, productionBareBinds(bareDir, ownGitDir), script)
	if err != nil {
		t.Fatalf("bwrap probe failed to run: %v\n%s", err, out)
	}

	wantReadOnly := []string{
		"config-write FAIL", "hooks-create FAIL", "info-create FAIL", "sibling-create FAIL",
		"commondir-write FAIL", "own-commondir-write FAIL", "own-gitdir-write FAIL", "own-cfgwt-write FAIL",
	}
	wantWritable := []string{"own-create OK", "objects-create OK"}
	for _, w := range wantReadOnly {
		if !strings.Contains(out, w) {
			t.Errorf("expected %q (path should be read-only); output:\n%s", w, out)
		}
	}
	for _, w := range wantWritable {
		if !strings.Contains(out, w) {
			t.Errorf("expected %q (path should be writable); output:\n%s", w, out)
		}
	}
}

// TestBwrapBareReadOnly_WithoutOverlaysWritesSucceed is the no-op proof: with
// only the read-write .bare bind (no overlays), every write that the positive
// test blocks succeeds. So the overlays are what enforce read-only.
func TestBwrapBareReadOnly_WithoutOverlaysWritesSucceed(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	bareDir, _, siblingGitDir := fakeBareLayout(t)

	script := strings.Join([]string{
		`if ( printf x >> ` + sh(filepath.Join(bareDir, "config")) + ` ) 2>/dev/null; then echo config-write OK; else echo config-write FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(bareDir, "hooks", "post-checkout")) + ` ) 2>/dev/null; then echo hooks-create OK; else echo hooks-create FAIL; fi`,
		`if ( printf '' > ` + sh(filepath.Join(siblingGitDir, "config.worktree")) + ` ) 2>/dev/null; then echo sibling-create OK; else echo sibling-create FAIL; fi`,
	}, "\n")

	// Only the read-write .bare bind — no overlays.
	out, err := runBareProbe(t, bwrapBin, []string{"--bind", bareDir, bareDir}, script)
	if err != nil {
		t.Fatalf("bwrap probe failed to run: %v\n%s", err, out)
	}
	for _, w := range []string{"config-write OK", "hooks-create OK", "sibling-create OK"} {
		if !strings.Contains(out, w) {
			t.Errorf("without overlays, expected %q (write should succeed); output:\n%s", w, out)
		}
	}
}

// worktreeGitFileLayout builds a worktree directory with a .git gitfile and
// returns (worktree, gitFile). The worktree is otherwise writable.
func worktreeGitFileLayout(t *testing.T) (worktree, gitFile string) {
	t.Helper()
	worktree = t.TempDir()
	gitFile = filepath.Join(worktree, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /real/git/dir\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}
	return worktree, gitFile
}

// gitFileProbeScript tries to write, rename-over, rename-away, and remove the
// worktree .git gitfile, printing OK/FAIL per case. A worker that can do any
// of these can repoint the gitfile and redirect a host git command.
func gitFileProbeScript(worktree, gitFile string) string {
	evil := filepath.Join(worktree, "evil")
	return strings.Join([]string{
		`if ( printf x >> ` + sh(gitFile) + ` ) 2>/dev/null; then echo git-write OK; else echo git-write FAIL; fi`,
		`printf 'gitdir: /evil\n' > ` + sh(evil) + ` 2>/dev/null || true`,
		`if ( mv -f ` + sh(evil) + ` ` + sh(gitFile) + ` ) 2>/dev/null; then echo git-rename-over OK; else echo git-rename-over FAIL; fi`,
		`if ( mv -f ` + sh(gitFile) + ` ` + sh(filepath.Join(worktree, "git-moved")) + ` ) 2>/dev/null; then echo git-rename-away OK; else echo git-rename-away FAIL; fi`,
		`if ( rm -f ` + sh(gitFile) + ` ) 2>/dev/null; then echo git-remove OK; else echo git-remove FAIL; fi`,
	}, "\n")
}

// TestBwrapBareReadOnly_WorktreeGitFilePinnedReadOnly runs a real bwrap with
// the worktree bound read-write and <worktree>/.git pinned read-only (the
// production shape), and checks a worker cannot write, rename, or remove the
// gitfile (issue #3322).
func TestBwrapBareReadOnly_WorktreeGitFilePinnedReadOnly(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	worktree, gitFile := worktreeGitFileLayout(t)

	// Production shape: read-write worktree bind, then the .git ro pin on top.
	mounts := bwrapWorktreeBind(Config{Worktree: worktree})
	mounts = append(mounts, bwrapWorktreeGitFilePin(Config{Worktree: worktree})...)

	out, err := runBareProbe(t, bwrapBin, mounts, gitFileProbeScript(worktree, gitFile))
	if err != nil {
		t.Fatalf("bwrap probe failed to run: %v\n%s", err, out)
	}
	for _, w := range []string{"git-write FAIL", "git-rename-over FAIL", "git-rename-away FAIL", "git-remove FAIL"} {
		if !strings.Contains(out, w) {
			t.Errorf("expected %q (.git must be immutable); output:\n%s", w, out)
		}
	}
	// The gitfile must still hold its original content afterwards.
	if got, rErr := os.ReadFile(gitFile); rErr != nil || string(got) != "gitdir: /real/git/dir\n" {
		t.Errorf(".git content changed: got %q (err %v)", string(got), rErr)
	}
}

// TestBwrapBareReadOnly_WorktreeGitFileWritableWithoutPin is the paired
// negative: with only the read-write worktree bind (no .git pin), the same
// writes succeed. So the pin is what makes the gitfile immutable.
func TestBwrapBareReadOnly_WorktreeGitFileWritableWithoutPin(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	worktree, gitFile := worktreeGitFileLayout(t)

	// Only the read-write worktree bind — no .git pin.
	mounts := bwrapWorktreeBind(Config{Worktree: worktree})

	out, err := runBareProbe(t, bwrapBin, mounts, gitFileProbeScript(worktree, gitFile))
	if err != nil {
		t.Fatalf("bwrap probe failed to run: %v\n%s", err, out)
	}
	// The first write alone proves the worktree is writable without the pin;
	// that is the no-op proof the pin is load-bearing.
	if !strings.Contains(out, "git-write OK") {
		t.Errorf("without the pin, writing .git should succeed; output:\n%s", out)
	}
}

// sh single-quotes a path for safe inclusion in the /bin/sh -c script. Test
// paths come from t.TempDir() and contain no single quotes, but quote anyway.
func sh(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
