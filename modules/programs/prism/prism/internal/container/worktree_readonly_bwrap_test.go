//go:build linux

package container

// worktree_readonly_bwrap_test.go — live bwrap coverage for the worktree
// bind (#3079). Each test runs a real bwrap with the bind that
// bwrapWorktreeBind emits for BuildArgs. The sandbox binds only /nix, /bin,
// and the worktree, so the bind under test is the only route to the
// worktree.
//
// The worker test is the paired negative: with WorktreeReadOnly unset, the
// same write succeeds, so the read-only test is not green by accident.
//
// The tests skip when bwrap cannot start (see requireUsableBwrapForUsageTest).
// The go-tests-bwrap-live CI job runs them and fails on a skip.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeFixture returns a worktree with one tracked-looking file in it.
func worktreeFixture(t *testing.T) (worktree, file string) {
	t.Helper()
	worktree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	file = filepath.Join(worktree, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return worktree, file
}

// TestBwrapWorktreeReadOnly_WriteDenied: a review agent or an investigator
// cannot change, create, or remove a file in its worktree, and it can still
// read one.
func TestBwrapWorktreeReadOnly_WriteDenied(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	worktree, file := worktreeFixture(t)
	bind := bwrapWorktreeBind(Config{Worktree: worktree, WorktreeReadOnly: true})

	out, err := runInBwrap(t, bwrapBin, bind, "cat "+shQuoteForTest(file))
	if err != nil || !strings.Contains(out, "package main") {
		t.Fatalf("read of a worktree file failed in the sandbox: %v\nOutput: %s", err, out)
	}

	created := filepath.Join(worktree, "new.go")
	for _, script := range []string{
		"echo changed > " + shQuoteForTest(file),
		"echo created > " + shQuoteForTest(created),
		"rm " + shQuoteForTest(file),
	} {
		out, err := runInBwrap(t, bwrapBin, bind, script)
		if err == nil {
			t.Errorf("%q succeeded in a read-only worktree.\nOutput: %s", script, out)
			continue
		}
		if !strings.Contains(out, "Read-only file system") {
			t.Errorf("%q failed for a reason other than the read-only mount: %v\nOutput: %s", script, err, out)
		}
	}

	if got, err := os.ReadFile(file); err != nil || string(got) != "package main\n" {
		t.Errorf("worktree file on the host after the write probes = (%q, %v), want it unchanged", got, err)
	}
	if _, err := os.Stat(created); err == nil {
		t.Errorf("%s exists on the host: the sandbox wrote through the read-only mount", created)
	}
}

// TestBwrapWorktreeReadOnly_WorkerCanWrite: a worker can change and create a
// file in its worktree, and the host sees the change.
func TestBwrapWorktreeReadOnly_WorkerCanWrite(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	worktree, file := worktreeFixture(t)
	bind := bwrapWorktreeBind(Config{Worktree: worktree})

	created := filepath.Join(worktree, "new.go")
	script := "echo changed > " + shQuoteForTest(file) + " && echo created > " + shQuoteForTest(created)
	if out, err := runInBwrap(t, bwrapBin, bind, script); err != nil {
		t.Fatalf("a write to the worktree of a worker failed: %v\nOutput: %s", err, out)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "changed\n" {
		t.Errorf("worktree file on the host = (%q, %v), want %q", got, err, "changed\n")
	}
	if got, err := os.ReadFile(created); err != nil || string(got) != "created\n" {
		t.Errorf("created file on the host = (%q, %v), want %q", got, err, "created\n")
	}
}
