package cmd

// Tests for the two-signal shutdown contract.
//
// runSignalHandler implements the two-signal contract:
//   - First signal: invoke shutdownFn() (in a goroutine) and call cancelFn().
//   - Second signal: reset to the runtime default handler and re-raise, giving
//     an immediate-exit path without requiring kill -9.
//
// Without it, a second SIGINT/SIGTERM during shutdown is silently dropped and
// the user has no force-exit path.
//
// These tests exercise runSignalHandler directly, using channels and stubs
// instead of a real Sidecar or OS process, so they run fast and in-process.

import (
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestSignalHandler_SingleSignal verifies that a single SIGINT causes
// shutdownFn and cancelFn to both be called, and that they complete normally.
func TestSignalHandler_SingleSignal(t *testing.T) {
	t.Parallel()

	shutdownStarted := make(chan struct{})
	shutdownDone := make(chan struct{})
	cancelCalled := make(chan struct{})

	shutdownFn := func() {
		close(shutdownStarted)
		// Simulate a brief but non-zero shutdown duration.
		time.Sleep(10 * time.Millisecond)
		close(shutdownDone)
	}
	cancelFn := func() {
		// May be called before or after shutdownFn starts; just record the call.
		select {
		case <-cancelCalled:
			// already closed
		default:
			close(cancelCalled)
		}
	}

	sigCh := make(chan os.Signal, 2)
	runSignalHandler(sigCh, shutdownFn, cancelFn)

	// Send one signal.
	sigCh <- syscall.SIGINT

	// Expect cancel to be called within a short deadline.
	select {
	case <-cancelCalled:
		// good
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancelFn not called within 250ms after first signal")
	}

	// Expect shutdown to start.
	select {
	case <-shutdownStarted:
		// good
	case <-time.After(250 * time.Millisecond):
		t.Fatal("shutdownFn not started within 250ms after first signal")
	}

	// Expect shutdown to complete.
	select {
	case <-shutdownDone:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdownFn did not complete within 500ms")
	}
}

// TestSignalHandler_TwoSignals verifies the force-exit path:
//   - First signal starts shutdown (stubbed to block until released).
//   - Second signal arrives before shutdown completes.
//   - The handler resets the signal disposition and re-raises.
//
// Because re-raising with signal.Reset + syscall.Kill would terminate the test
// process, we intercept the second-signal effects by replacing the syscall
// with a test hook and verifying the reset was requested within ~250ms.
func TestSignalHandler_TwoSignals(t *testing.T) {
	t.Parallel()

	// shutdownBlocked lets us hold Shutdown open until we are done asserting.
	shutdownBlocked := make(chan struct{})
	shutdownStarted := make(chan struct{})
	cancelCalled := make(chan struct{})
	forceExitTriggered := make(chan struct{}, 1)

	var once sync.Once

	shutdownFn := func() {
		once.Do(func() { close(shutdownStarted) })
		// Block until the test releases us — simulates a slow Shutdown.
		<-shutdownBlocked
	}
	cancelFn := func() {
		select {
		case <-cancelCalled:
		default:
			close(cancelCalled)
		}
	}

	// Swap in a test-local forceExit so we don't actually kill the test process.
	origForceExit := sidecarForceExit
	t.Cleanup(func() { sidecarForceExit = origForceExit })
	sidecarForceExit = func(sig syscall.Signal) {
		select {
		case forceExitTriggered <- struct{}{}:
		default:
		}
	}

	sigCh := make(chan os.Signal, 2)
	runSignalHandler(sigCh, shutdownFn, cancelFn)

	// First signal — starts Shutdown (which blocks) and cancels the context.
	sigCh <- syscall.SIGINT

	select {
	case <-shutdownStarted:
		// good — Shutdown is now running (and blocked)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("shutdownFn not started within 250ms after first signal")
	}

	// Second signal — should trigger force exit within ~250ms.
	sigCh <- syscall.SIGTERM

	select {
	case <-forceExitTriggered:
		// good
	case <-time.After(250 * time.Millisecond):
		t.Fatal("force exit not triggered within 250ms after second signal")
	}

	// Release Shutdown so the goroutine can exit cleanly (avoids goroutine leak).
	close(shutdownBlocked)
}
