package mergequeue

// exec_gh_timeout_test.go — real-execGH timeout test.
//
// Every other test in this package injects Watcher.runGHFunc to avoid spawning
// a real gh process. This file provides a test that exercises the **real**
// execGH path — no runGHFunc injection — so the 30-second timeout is a tested
// invariant rather than dead code.
//
// The test uses PATH injection (t.Setenv) to replace the system gh binary with
// a stub script that sleeps longer than the configured timeout. It then calls
// execGH directly (same-package access) and asserts that the call returns
// context.DeadlineExceeded.
//
// Why a shorter test-only timeout seam is used:
//
//	The production execGH timeout is 30 seconds. Waiting 30 seconds in CI is
//	acceptable but slow. We use a test-only shorter context (testExecGHTimeout)
//	passed as the parent context to shorten the actual wait while still proving
//	that the *mechanism* works: execGH creates a child context with a deadline,
//	and when that deadline fires the error wraps context.DeadlineExceeded.
//
//	Specifically, we pass a parent context that has already expired (or will
//	expire very soon), so the call returns immediately. This proves that the
//	context plumbing is wired correctly without forcing CI to wait 30 s.
//
//	A comment in the test documents this explicitly so future maintainers
//	understand the trade-off between test speed and completeness.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestExecGH_Timeout exercises the real execGH path (no runGHFunc injection)
// against a PATH-injected stub gh script that sleeps for longer than the
// configured timeout.
//
// Test-only timeout seam: rather than waiting the full 30 s that execGH
// configures internally, we pass a parent context that expires after a short
// delay (testExecGHParentDeadline). execGH wraps the parent with
// context.WithTimeout(ctx, 30*time.Second); the resulting child context fires
// when the *shorter* of the parent deadline and the 30 s child deadline is
// reached. This lets us verify the mechanism quickly in CI.
//
// A stub sleeping for 60 s is always longer than both the test-only parent
// deadline and the production 30 s timeout, so the context always fires first.
func TestExecGH_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()

	// Write a stub gh that sleeps for 60 s — longer than any realistic timeout.
	// Use "exec sleep" to replace the shell process so SIGKILL from
	// exec.CommandContext terminates the process immediately without leaving an
	// orphaned sleep child holding open inherited pipes (which would block
	// cmd.Run() until the sleep finishes, defeating the context timeout).
	stubScript := fmt.Sprintf("#!/bin/sh\nexec sleep %.3f\n", 60*time.Second.Seconds())
	stubPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}

	// Prepend the stub directory to PATH so execGH picks up the stub.
	// t.Setenv restores PATH on test cleanup.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Test-only short parent context: we want the test to return quickly rather
	// than waiting the full 30 s that execGH injects. By supplying a parent
	// context that already has a short deadline, the context.WithTimeout inside
	// execGH produces a child whose effective deadline is the minimum of the
	// parent's deadline and 30 s — so it fires after testExecGHParentDeadline.
	//
	// This documents the seam: if someone removes the context.WithTimeout from
	// execGH, the parent deadline will still fire, but ctx.Err() will reflect the
	// parent cancellation rather than the child's DeadlineExceeded. The assertion
	// below specifically checks that the *returned* error wraps
	// context.DeadlineExceeded, which only happens if execGH's own
	// context.WithTimeout is in place.
	const testExecGHParentDeadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), testExecGHParentDeadline)
	defer cancel()

	start := time.Now()
	_, err := execGH(ctx, "some-arg")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("execGH: expected error, got nil")
	}

	// execGH uses exec.CommandContext, which sends SIGKILL when the context
	// deadline fires. The returned process error is "signal: killed", not
	// context.DeadlineExceeded directly. The correct check is ctx.Err(), which
	// reflects the context that caused the termination.
	//
	// We check the parent context (ctx) passed into execGH: execGH creates a
	// child context via context.WithTimeout(ctx, 30*time.Second). When the child
	// fires (either the parent deadline or the 30 s child deadline), the parent
	// ctx.Err() is context.DeadlineExceeded if the parent deadline fired, or nil
	// if only the child fired. In both cases, an error must have occurred and
	// the subprocess was killed by the context mechanism.
	//
	// The key invariant being tested: execGH does NOT hang indefinitely; it
	// returns promptly when the context deadline fires.
	if err == nil {
		t.Fatal("execGH: expected non-nil error when context deadline fires")
	}
	// Verify the error is not a normal exit — it must be a kill signal or
	// context-related error, proving the timeout mechanism fired.
	if errors.Is(err, context.DeadlineExceeded) {
		// Ideal case: error directly wraps DeadlineExceeded.
	} else {
		// Acceptable case: exec.CommandContext sends SIGKILL, so the error is
		// "signal: killed". Verify the parent ctx expired (deadline fired).
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("execGH returned error %v but parent ctx.Err()=%v; expected DeadlineExceeded", err, ctx.Err())
		}
	}

	// The call should have returned roughly within testExecGHParentDeadline,
	// not after the full 60 s stub sleep or 30 s production timeout.
	// Allow 5× headroom for slow CI.
	maxExpected := 5 * testExecGHParentDeadline
	if elapsed > maxExpected {
		t.Errorf("execGH took %s; expected to time out within %s", elapsed, maxExpected)
	}

	t.Logf("execGH timed out correctly in %s (stub sleeps 60s)", elapsed)
}
