package sidecar

// Tests for issue #2341: the pipe-listener error path in
// runStartupSocketPipe must be diagnostically distinct from a genuine accept
// timeout.
//
// Before the fix, acceptTimedOut and acceptListenerErr shared one case in
// the main reconnect loop. Both produced the same log line ("timed out
// waiting for extension to (re)connect (timeout=...)") and the same
// state_change note ("reconnect timeout"), so a SIGTERM-triggered listener
// close read as a timeout misconfiguration in the sidecar log — the
// diagnostic problem described in #2340.
//
// The two tests below assert that the two paths are now distinguishable:
//
//   - TestSocketPipe_ListenerError_LogAndNote closes the listener under a
//     pending Accept and asserts that the log names the underlying error and
//     does NOT claim a timeout fired, and that the state_change note is
//     "pipe listener error".
//   - TestSocketPipe_GenuineTimeout_LogAndNote lets acceptTimeout elapse
//     without a connection and asserts that the log names the configured
//     timeout duration and that the state_change note is "reconnect
//     timeout" (unchanged from pre-#2341 behaviour).
//
// Both tests also verify that the state=error write lands in the DB before
// the function returns — the #1760 invariant, preserved on both paths.
//
// Isolation follows the #1608 convention: the sidecars are constructed via
// newSocketPipeSidecarWithClock, which sets $XDG_STATE_HOME to a t.TempDir()
// and PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 before calling sidecar.New.

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// captureLogInto swaps sc.cfg.Logger for a buffer-backed logger and returns a
// snapshot function. Modelled after captureLog in sidecar_test.go but without
// the WaitNotifies drain (the runStartupSocketPipe paths tested here do not
// spawn notify goroutines).
func captureLogInto(sc *Sidecar) func() string {
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	sc.cfg.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// lockedWriter serialises writes to an underlying strings.Builder so that
// concurrent log.Logger writes from the sidecar's goroutines and the test's
// snapshot read cannot race under -race.
type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// waitForStateChangeNote polls agent_events for a state_change row whose
// payload JSON contains the given note substring, failing the test after
// deadline. Returns the number of matching rows on success.
func waitForStateChangeNote(t *testing.T, d *db.DB, session, note string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := d.QueryRow(
			`SELECT COUNT(*) FROM agent_events
			   WHERE session_name = ?
			     AND type = 'state_change'
			     AND payload LIKE ?`,
			session, `%"note":"`+note+`"%`,
		).Scan(&n); err != nil {
			t.Fatalf("query agent_events: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for state_change agent_events row with note=%q", note)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoStateChangeNote verifies that no state_change row exists whose
// payload contains the given note substring. Used to prove the two paths are
// disjoint (a listener-error run must not also write "reconnect timeout" and
// vice versa).
func assertNoStateChangeNote(t *testing.T, d *db.DB, session, note string) {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM agent_events
		   WHERE session_name = ?
		     AND type = 'state_change'
		     AND payload LIKE ?`,
		session, `%"note":"`+note+`"%`,
	).Scan(&n); err != nil {
		t.Fatalf("query agent_events: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected no state_change with note=%q, found %d", note, n)
	}
}

// TestSocketPipe_ListenerError_LogAndNote drives the acceptListenerErr path
// by closing the listener while Accept is blocked. It asserts that:
//   - the log names the underlying Accept() error (e.g. "use of closed
//     network connection") and prefixes it with "pipe listener error:";
//   - the log does NOT claim a timeout fired;
//   - the state_change agent_events row carries the new "pipe listener
//     error" note (and NOT the "reconnect timeout" note);
//   - the state=error write is durable in the DB by the time
//     runStartupSocketPipe returns (the #1760 invariant, applied to the
//     listener-error branch).
func TestSocketPipe_ListenerError_LogAndNote(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)

	// Use a long startup timeout so any "timeout" log line in the captured
	// buffer could only come from the wrong (pre-#2341) code path. If the
	// listener-error branch mistakenly rendered the timeout log line, the
	// value would still be visible here.
	sc.cfg.StartupConnectTimeout = 30 * time.Second

	getLogs := captureLogInto(sc)

	// Run the pipe in a goroutine. It will block in Accept() until we close
	// the listener below.
	var (
		mu     sync.Mutex
		runErr error
		done   = make(chan struct{})
	)
	go func() {
		defer close(done)
		e := sc.runStartupSocketPipe(context.Background())
		mu.Lock()
		runErr = e
		mu.Unlock()
	}()

	// Wait for the sidecar to install the listener before we close it.
	// The socket file appearing on disk is the standard readiness signal
	// used elsewhere in this test file.
	deadline := time.Now().Add(3 * time.Second)
	var ln net.Listener
	for {
		sc.mu.Lock()
		ln = sc.harnessPipeListener
		sc.mu.Unlock()
		if ln != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harnessPipeListener never installed by runStartupSocketPipe")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Close the listener — this is the shape SIGTERM -> Shutdown() takes on
	// the real path (#2340). Accept() should return promptly with a
	// "use of closed network connection" error, driving the
	// acceptListenerErr branch.
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	// runStartupSocketPipe should return quickly with an error.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runStartupSocketPipe did not return after listener close")
	}

	mu.Lock()
	gotErr := runErr
	mu.Unlock()
	if gotErr == nil {
		t.Fatal("runStartupSocketPipe returned nil error on listener close, want non-nil")
	}
	if !strings.Contains(gotErr.Error(), "pipe listener error") {
		t.Errorf("returned error = %q, want it to contain \"pipe listener error\"", gotErr.Error())
	}

	logs := getLogs()

	// AC #1: log names the underlying Accept error and does not claim a
	// timeout fired.
	if !strings.Contains(logs, "pipe listener error:") {
		t.Errorf("log missing \"pipe listener error:\" prefix; got:\n%s", logs)
	}
	// The typical error text from a closed Unix listener; assert on the
	// canonical Go net-package substring rather than the full path so the
	// assertion holds on Linux, Darwin, and inside the Nix sandbox alike.
	if !strings.Contains(logs, "use of closed network connection") {
		t.Errorf("log missing underlying Accept error text (\"use of closed network connection\"); got:\n%s", logs)
	}
	if strings.Contains(logs, "timed out waiting for extension") {
		t.Errorf("log claimed a timeout fired on a listener-error path; got:\n%s", logs)
	}

	// AC #2: state_change note distinguishes the two paths.
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "pipe listener error")
	assertNoStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "reconnect timeout")

	// AC #4 (part 1): the state=error write lands in agent_status by the
	// time the function returns — the #1760 invariant is preserved on the
	// listener-error branch.
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != string(agent.StateError) {
		t.Errorf("agent_status.state = %q after listener error, want %q", state, agent.StateError)
	}
}

// TestSocketPipe_GenuineTimeout_LogAndNote drives the acceptTimedOut path by
// starting the sidecar with a short StartupConnectTimeout and never dialling.
// It asserts that the pre-#2341 timeout behaviour is unchanged: log names the
// configured timeout duration, and the state_change note remains
// "reconnect timeout".
func TestSocketPipe_GenuineTimeout_LogAndNote(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)

	// Very short startup timeout so the timer fires quickly and the test
	// stays cheap. The value is echoed back in the log message as
	// "timeout=<duration>", which the assertion below matches on.
	sc.cfg.StartupConnectTimeout = 150 * time.Millisecond

	getLogs := captureLogInto(sc)

	var (
		mu     sync.Mutex
		runErr error
		done   = make(chan struct{})
	)
	go func() {
		defer close(done)
		e := sc.runStartupSocketPipe(context.Background())
		mu.Lock()
		runErr = e
		mu.Unlock()
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runStartupSocketPipe did not return within 3s of the 150ms startup timeout")
	}

	mu.Lock()
	gotErr := runErr
	mu.Unlock()
	if gotErr == nil {
		t.Fatal("runStartupSocketPipe returned nil on genuine timeout, want non-nil")
	}
	if !strings.Contains(gotErr.Error(), "timed out waiting for extension") {
		t.Errorf("returned error = %q, want it to contain \"timed out waiting for extension\"", gotErr.Error())
	}

	logs := getLogs()

	// AC #3: log line names the configured timeout duration.
	if !strings.Contains(logs, "timed out waiting for extension to (re)connect (timeout=150ms)") {
		t.Errorf("log missing timeout line with configured duration; got:\n%s", logs)
	}
	// A genuine-timeout run must NOT surface the listener-error prefix.
	if strings.Contains(logs, "pipe listener error:") {
		t.Errorf("log carried a \"pipe listener error:\" line on a genuine-timeout path; got:\n%s", logs)
	}

	// AC #2 (mirror): the "reconnect timeout" note is unchanged, and the
	// listener-error note MUST NOT appear on this path.
	waitForStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "reconnect timeout")
	assertNoStateChangeNote(t, sc.cfg.DB, sc.cfg.SessionName, "pipe listener error")

	// AC #4 (part 2): the state=error write lands in agent_status by the
	// time the function returns.
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != string(agent.StateError) {
		t.Errorf("agent_status.state = %q after genuine timeout, want %q", state, agent.StateError)
	}
}
