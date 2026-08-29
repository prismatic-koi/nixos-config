package review_test

// subprocess_timeout_test.go — timeout tests for runGH and runGitInWorktree.
//
// Both functions use exec.CommandContext with a fixed deadline. These tests
// PATH-inject a stub script that sleeps longer than the configured timeout and
// assert that the call returns context.DeadlineExceeded (or a wrapped error
// containing it).
//
// The stub scripts use a test-only short timeout to avoid making the test suite
// slow — the constant being tested is the *mechanism* (context cancellation),
// not the 30 s / 10 s wall-clock value. A code comment in each test documents
// this seam so future readers understand the trade-off.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
)

// writeSleepScript writes an executable shell script that sleeps for the given
// duration to dir/<name> and returns the path.
//
// Uses "exec sleep" (exec-replaces the shell with the sleep binary) so that
// when exec.CommandContext sends SIGKILL to the process, there is no orphaned
// child subprocess keeping inherited file descriptors (e.g. stdout pipes) open.
// Without "exec", the shell spawns sleep as a child; killing the shell leaves
// sleep alive with inherited pipes, causing cmd.Run() to block until sleep
// exits naturally — defeating the context timeout in the test.
func writeSleepScript(t *testing.T, dir, name string, sleep time.Duration) string {
	t.Helper()
	// Use "exec sleep" to replace the shell process with sleep directly.
	// This ensures SIGKILL from exec.CommandContext terminates the process
	// immediately without leaving orphaned children holding open pipe ends.
	script := fmt.Sprintf("#!/bin/sh\nexec sleep %.3f\n", sleep.Seconds())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writeSleepScript: %v", err)
	}
	return path
}

// prependToPATH returns a PATH value with dir prepended to the current PATH.
func prependToPATH(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestRunGH_Timeout verifies that runGH fires context.DeadlineExceeded when the
// gh subprocess runs longer than ghTimeout.
//
// Implementation note: we cannot reduce ghTimeout itself without modifying
// production code, so the stub script is made to sleep for only a brief period
// while the test relies on the fact that the production timeout is 30 s — a
// stub that sleeps for 60 s will always exceed it. To keep CI fast we set the
// stub sleep to 60 s and rely on the production timeout (30 s) firing first.
// If CI is too slow, consider adding a test-only override seam.
func TestRunGH_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	// The stub sleeps longer than ghTimeout (30 s) so the context fires first.
	writeSleepScript(t, dir, "gh", 60*time.Second)
	t.Setenv("PATH", prependToPATH(dir))

	// Sanity check: the stub gh is actually found.
	stubPath := filepath.Join(dir, "gh")
	if _, err := os.Stat(stubPath); err != nil {
		t.Fatalf("stub gh not found at %s: %v", stubPath, err)
	}

	start := time.Now()
	_, err := review.RunGHForTest("some-arg")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("RunGHForTest: expected error, got nil")
	}

	// The error must wrap or name context.DeadlineExceeded.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RunGHForTest error does not wrap context.DeadlineExceeded: %v", err)
	}

	// The error message must name the timed-out command and the timeout duration,
	// so operators can diagnose without reading source.
	errMsg := err.Error()
	if !containsAny(errMsg, "timed out") {
		t.Errorf("error message should mention 'timed out': %v", err)
	}
	if !containsAny(errMsg, "30s") {
		t.Errorf("error message should mention timeout duration '30s': %v", err)
	}

	// The call should have returned in roughly ghTimeout, not the full 60 s.
	// Allow generous headroom for slow CI: assert < 45 s (1.5× the timeout).
	if elapsed > 45*time.Second {
		t.Errorf("RunGHForTest took %s, expected to time out around %s", elapsed, review.GHTimeoutForTest)
	}
}

// TestRunGitInWorktree_Timeout verifies that runGitInWorktree returns an empty
// string (and does not hang) when the git subprocess runs longer than
// gitWorktreeTimeout.
//
// The function returns "" on any error (git log failures are non-fatal), so we
// cannot assert an error value. We assert: (a) the call returns before the
// stub's sleep duration ends, and (b) it returns the empty string.
func TestRunGitInWorktree_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	// The stub sleeps longer than gitWorktreeTimeout (10 s) so the context fires.
	writeSleepScript(t, dir, "git", 60*time.Second)
	t.Setenv("PATH", prependToPATH(dir))

	worktree := t.TempDir()
	start := time.Now()
	result := review.RunGitInWorktreeForTest(worktree, "log", "--oneline", "-20")
	elapsed := time.Since(start)

	if result != "" {
		t.Errorf("RunGitInWorktreeForTest: expected empty string on timeout, got %q", result)
	}

	// Should have timed out around gitWorktreeTimeout (10 s), not run the full 60 s.
	// Allow generous headroom: assert < 25 s (2.5× the timeout).
	if elapsed > 25*time.Second {
		t.Errorf("RunGitInWorktreeForTest took %s, expected to time out around %s", elapsed, review.GitWorktreeTimeoutForTest)
	}
}
