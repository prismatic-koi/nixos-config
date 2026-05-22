package cmd

// Portable (all-GOOS) unit tests for supervise.go (SuperviseChild).
//
// SuperviseChild owns three concerns for both the bwrap (Linux) and
// sandbox-exec (Darwin) agent-run dispatch paths:
//
//  1. Hand the terminal foreground process group to the child via
//     tcsetpgrpForeground, and restore the original pgid when Wait
//     returns.
//  2. Forward SIGTERM/SIGINT/SIGHUP (and optionally SIGWINCH when
//     opts.ForwardWinch is true) to the child's process group.
//  3. Early-return when the supplied *exec.Cmd is nil or has not been
//     started (cmd.Process == nil).
//
// This file holds only the platform-agnostic early-return tests
// (concern #3). The Linux-only tests for concerns #1 and #2 — which
// rely on /dev/ptmx + TIOCSPTLCK/TIOCGPTN ioctls and the
// Setctty/Ctty fields of syscall.SysProcAttr that exist only on
// Linux — live in supervise_linux_test.go behind a //go:build linux
// tag.
//
// The superviseHelperEnvVar constant is also declared here (rather
// than in the Linux-only file) because the package-wide TestMain
// dispatcher in killsidecar_test.go references it unconditionally.
// The runSuperviseHelper function it dispatches to has a platform
// split: the real implementation lives in supervise_linux_test.go,
// and a stub returning a "not supported on this GOOS" sentinel lives
// in supervise_other_test.go.

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// superviseHelperEnvVar gates the helper-subprocess entry point inside
// the package-wide TestMain (see killsidecar_test.go). When set to
// "1", the test binary skips the usual go-test entry and instead
// executes the SuperviseChild helper described in runSuperviseHelper.
//
// The constant is declared in this portable file so the TestMain
// dispatcher in killsidecar_test.go can reference it on all GOOS; the
// helper body itself is only meaningful on Linux (see
// supervise_linux_test.go).
const superviseHelperEnvVar = "PRISM_TEST_SUPERVISE_HELPER"

// ── early-return paths (portable, all GOOS) ──────────────────────────────────

// TestSuperviseChild_NilCmdReturnsImmediately verifies that passing a
// nil *exec.Cmd causes SuperviseChild to return nil without spawning
// the forwarder goroutine or attempting tcsetpgrp. This is the guard
// at the top of SuperviseChild and is identical behaviour on Linux
// and Darwin.
func TestSuperviseChild_NilCmdReturnsImmediately(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- SuperviseChild(nil, int(os.Stdin.Fd()), SuperviseOpts{ForwardWinch: true})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("SuperviseChild(nil, ...) = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SuperviseChild(nil, ...) did not return within 1s")
	}
}

// TestSuperviseChild_UnstartedCmdReturnsImmediately verifies that
// passing an *exec.Cmd that has not been started (cmd.Process == nil)
// also takes the early-return path. This protects callers who
// construct a Cmd but then bail on Start before calling
// SuperviseChild.
func TestSuperviseChild_UnstartedCmdReturnsImmediately(t *testing.T) {
	cmd := exec.Command("true") // not started — cmd.Process is nil
	if cmd.Process != nil {
		t.Fatalf("precondition: cmd.Process should be nil before Start, got %v", cmd.Process)
	}

	done := make(chan error, 1)
	go func() {
		done <- SuperviseChild(cmd, int(os.Stdin.Fd()), SuperviseOpts{})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("SuperviseChild(unstarted cmd, ...) = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SuperviseChild(unstarted cmd, ...) did not return within 1s")
	}
}
