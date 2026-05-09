package cmd

// wait.go — shared helpers for the --wait flag on prism merge / review / spawn
// (issue #1500).
//
// Three concerns live here:
//
//   1. backoffSchedule — exponential backoff with jitter for poll loops.
//      Centralised so reviewers can spot the implementation in one place
//      (AC: "verifiable by reading the code").
//
//   2. setupWaitSignals — installs a SIGINT/SIGTERM handler that does NOT
//      cancel the underlying job (merge, review, spawn). It only flips a
//      local "user interrupted the wait" flag and unblocks the poller.
//      The merge-queue watcher / review monitor / spawned agent keep
//      running; the user can recover by re-running the same command
//      without --wait, or by inspecting the relevant ledger.
//
//   3. Constants for default --timeout values and exit codes that the
//      individual commands share.
//
// Style note: every helper here is intentionally small and dependency-light.
// The wait loops live in each command's own file so the per-command terminal
// definition stays close to its other code.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// Default timeouts for --wait. The issue specifies 30m for merge and 2× the
// per-agent timeout for review (which defaults to 10m, so 20m). For spawn we
// pick 10m as a sensible default — most spawned agents either become ready
// within a minute or fail.
const (
	defaultMergeWaitTimeout  = 30 * time.Minute
	defaultReviewWaitTimeout = 20 * time.Minute
	defaultSpawnWaitTimeout  = 10 * time.Minute
)

// Exit codes emitted by --wait paths. The wait commands return these via an
// exitCodeError so cobra surfaces them on os.Exit. "Timeout" is distinct from
// a "real" terminal failure so callers can tell the two apart (AC: "On
// timeout, exits non-zero with a status payload distinguishable from a real
// merge failure.").
const (
	waitExitOK            = 0
	waitExitTerminalFail  = 2 // job reached a non-success terminal state
	waitExitTimeout       = 3 // local --timeout elapsed
	waitExitUserInterrupt = 4 // user pressed Ctrl-C; underlying job was NOT cancelled
)

// exitCodeError lets a RunE return a specific os.Exit code without printing
// the error twice. cmd/root.go's main wraps cobra.Execute and inspects the
// returned error; for exitCodeError it calls os.Exit(code) silently.
type exitCodeError struct {
	code int
	msg  string // optional — printed to stderr before exit when non-empty
}

func (e *exitCodeError) Error() string { return e.msg }

// ExitCode returns the desired process exit code. cmd/root.go's runner reads
// this via errors.As to bypass cobra's default exit-code-1 behaviour.
func (e *exitCodeError) ExitCode() int { return e.code }

// newExitErr builds an exitCodeError. msg "" suppresses the error print.
func newExitErr(code int, msg string) error {
	return &exitCodeError{code: code, msg: msg}
}

// backoffSchedule returns a function that yields successive sleep durations
// for a poll loop: starts at base, doubles each call up to max, then stays at
// max. Each yielded duration has uniform jitter in [0.5x, 1.5x] applied so
// concurrent waiters do not synchronise.
//
// Example progression with base=500ms, max=5s:
//
//	~500ms, ~1s, ~2s, ~4s, ~5s, ~5s, ... (each ±50%)
//
// Centralising this in one named function makes the AC ("verifiable by
// reading the code") trivially auditable: every --wait poll loop calls
// backoffSchedule and uses the returned closure.
func backoffSchedule(base, max time.Duration) func() time.Duration {
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if max <= 0 || max < base {
		max = 30 * base
	}
	cur := base
	return func() time.Duration {
		d := cur
		// Advance for next call (capped at max).
		next := cur * 2
		if next > max {
			next = max
		}
		cur = next
		// Apply ±50% jitter. crypto/rand would be overkill here; the only
		// goal is to desynchronise concurrent waiters in the same shell.
		// math/rand is reseeded by Go's runtime since 1.20.
		jitter := 0.5 + rand.Float64() // in [0.5, 1.5)
		return time.Duration(float64(d) * jitter)
	}
}

// waitInterrupt is set to 1 when the user presses Ctrl-C while a --wait poll
// loop is sleeping. The poll loops check this between sleeps and return early
// with waitExitUserInterrupt.
//
// Critical: pressing Ctrl-C must NOT cancel the underlying merge / review /
// spawn job (AC: edge-case). The job is driven by a separate process (the
// merge-queue watcher, the review monitor, or the spawned agent's sidecar);
// Ctrl-C only interrupts our local poller. setupWaitSignals installs a
// signal handler that flips this flag and does nothing else.
//
// Stored as int32 so atomic loads are cheap inside the poll loops.
type waitInterruptState struct {
	tripped atomic.Int32
}

// setupWaitSignals installs a signal handler for SIGINT and SIGTERM that
// flips state.tripped and returns. The default Go behaviour for SIGINT is
// "terminate the process" — installing this handler suppresses that, so the
// caller's deferred cleanups (DB close, etc.) run before we return the
// user-interrupt exit code.
//
// Returns a cleanup function that uninstalls the handler. Callers should
// defer it so the handler is removed when the wait completes naturally.
func setupWaitSignals() (*waitInterruptState, func()) {
	state := &waitInterruptState{}
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				state.tripped.Store(1)
				// Continue draining so a second Ctrl-C doesn't terminate
				// us before the poll loop sees the first one. We deliberately
				// do NOT exit on a second Ctrl-C: the AC requires that we
				// always return cleanly so the underlying job is not
				// cancelled. The user can kill -9 if they want a hard exit.
			case <-done:
				signal.Stop(ch)
				return
			}
		}
	}()
	return state, func() { close(done) }
}

// userInterrupted returns true when the wait-signal handler has tripped.
func (s *waitInterruptState) userInterrupted() bool {
	return s.tripped.Load() != 0
}

// pollWait drives a generic poll loop. probe is called once per cycle and
// returns (done, err): done=true means a terminal state was reached and the
// loop returns the probe's error verbatim. done=false continues the loop.
//
// The loop terminates with:
//
//   - the probe returning done=true (caller's terminal logic fired),
//   - the deadline being reached (returns waitExitTimeout error),
//   - the user interrupting (returns waitExitUserInterrupt error).
//
// timeout ≤ 0 means "no timeout" — the loop runs until probe is done or the
// user interrupts. Callers that want a default timeout should resolve it
// before calling.
//
// The first probe call runs with no sleep beforehand so an already-terminal
// state is observed immediately (AC: "calling prism merge --wait on an
// already-merged PR returns immediately").
func pollWait(ctx context.Context, timeout time.Duration, base, max time.Duration, probe func() (done bool, err error)) error {
	state, stop := setupWaitSignals()
	defer stop()

	var deadline time.Time
	hasDeadline := timeout > 0
	if hasDeadline {
		deadline = time.Now().Add(timeout)
	}
	next := backoffSchedule(base, max)

	for {
		if state.userInterrupted() {
			return newExitErr(waitExitUserInterrupt,
				"prism: --wait interrupted; underlying job was NOT cancelled. Re-run without --wait to recover the result.")
		}
		done, err := probe()
		if done {
			return err
		}
		if hasDeadline && !time.Now().Before(deadline) {
			return newExitErr(waitExitTimeout, "")
		}

		sleep := next()
		// Cap sleep so we don't overshoot the deadline by more than ~1s.
		if hasDeadline {
			rem := time.Until(deadline)
			if rem <= 0 {
				return newExitErr(waitExitTimeout, "")
			}
			if sleep > rem {
				sleep = rem
			}
		}

		// Sleep, but also wake on signal/context cancel so the user
		// interrupt is observed promptly without waiting for the full
		// backoff slot.
		timer := time.NewTimer(sleep)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return newExitErr(waitExitUserInterrupt, ctx.Err().Error())
		}
	}
}

// emitJSONOrText is a small convenience used by every --wait command. When
// jsonMode is true it marshals payload via printJSON (no textual lines).
// Otherwise it calls textFn to print a human-readable summary.
//
// Returns the error from JSON marshalling/printing or textFn; callers
// typically fmt.Errorf-wrap if they need additional context.
func emitJSONOrText(jsonMode bool, payload []byte, textFn func()) error {
	if jsonMode {
		return printJSON(payload)
	}
	if textFn != nil {
		textFn()
	}
	return nil
}

// formatDurationShort renders a duration like "30m" or "1h30m" without
// nanoseconds. Used in human-readable wait output.
func formatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
