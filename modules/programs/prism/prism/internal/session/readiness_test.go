package session_test

// Tests for the readiness gate (#1051 Piece A). These use a real prism DB
// and exercise the actual signals SpawnSession's gate consumes:
//
//   - "state_change" event written to agent_events (the cleanest readiness
//     marker — only ever written by the sidecar after the first SSE event
//     from opencode).
//   - agent_status.harness_session_id set non-NULL (the secondary signal).
//
// We do NOT spin up a real sidecar or tmux session here; the readiness gate
// is purely a DB poll. Tests inject the readiness signals directly to
// validate the polling behaviour, the timeout path, and the error type.

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

func openReadinessTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedSession creates a minimal agent_status row so CurrentStatus / QueryEvents
// against it return without error. The state is "idle" — same as what
// SpawnSession's UpsertStatusSeedRootAgentName writes at spawn time.
func seedSession(t *testing.T, d *db.DB, sessionName string) {
	t.Helper()
	if err := d.UpsertStatusSeedRootAgentName(sessionName, "test", "/tmp", "idle", nil, nil, "worker"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", sessionName, err)
	}
}

// ── WaitForReady — happy path ─────────────────────────────────────────────────

// TestWaitForReady_StateChangeEventSatisfiesGate verifies that an agent_events
// row of type "state_change" causes WaitForReady to return nil. This is the
// primary signal SpawnSession's gate uses, and it's the one the sidecar emits
// when the first SSE event from opencode arrives.
func TestWaitForReady_StateChangeEventSatisfiesGate(t *testing.T) {
	d := openReadinessTestDB(t)
	const sess = "myrepo@feature"
	seedSession(t, d, sess)

	// Simulate the sidecar receiving the first SSE event by inserting a
	// state_change event directly. The payload shape mirrors what
	// internal/sidecar/sidecar.go writeEvent produces.
	evt := db.Event{
		ID:          "evt-1",
		SessionName: sess,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
	}
	if err := d.WriteEvent(evt); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	if err := session.WaitForReady(d, sess, 2*time.Second); err != nil {
		t.Errorf("WaitForReady: got %v, want nil (state_change should satisfy the gate)", err)
	}
}

// TestWaitForReady_HarnessSessionIDSatisfiesGate verifies that a non-NULL
// harness_session_id on agent_status causes WaitForReady to return nil. This
// is the secondary signal — the sidecar writes it on session.created.
func TestWaitForReady_HarnessSessionIDSatisfiesGate(t *testing.T) {
	d := openReadinessTestDB(t)
	const sess = "myrepo@hsid"
	seedSession(t, d, sess)

	if err := d.UpdateHarnessSessionID(sess, "ses_abc123"); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	if err := session.WaitForReady(d, sess, 2*time.Second); err != nil {
		t.Errorf("WaitForReady: got %v, want nil (harness_session_id should satisfy the gate)", err)
	}
}

// TestWaitForReady_ReturnsBeforeDeadline verifies that the gate returns
// promptly once the signal arrives — not at the end of the timeout window.
// We insert the state_change after a small delay and assert WaitForReady
// returned within a generous bound that is still well under the configured
// timeout.
func TestWaitForReady_ReturnsBeforeDeadline(t *testing.T) {
	d := openReadinessTestDB(t)
	const sess = "myrepo@quick"
	seedSession(t, d, sess)

	// Insert the readiness signal after 200ms — well before the 5s timeout.
	go func() {
		time.Sleep(200 * time.Millisecond)
		evt := db.Event{
			ID:          "evt-quick",
			SessionName: sess,
			Repo:        "test",
			Worktree:    "/tmp",
			Type:        "state_change",
			Payload:     `{"state":"active"}`,
		}
		_ = d.WriteEvent(evt)
	}()

	start := time.Now()
	err := session.WaitForReady(d, sess, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	// Allow a generous bound — the poll interval is 250ms so the worst-case
	// detection latency is ~450ms after the signal arrives.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("WaitForReady took %v, expected to return well before the 5s timeout", elapsed)
	}
}

// ── WaitForReady — timeout path ───────────────────────────────────────────────

// TestWaitForReady_TimesOutWhenNoSignalArrives verifies the timeout branch:
// when no state_change event is written and harness_session_id stays NULL,
// WaitForReady returns *ReadinessTimeoutError after the configured deadline.
func TestWaitForReady_TimesOutWhenNoSignalArrives(t *testing.T) {
	d := openReadinessTestDB(t)
	const sess = "myrepo@stuck"
	seedSession(t, d, sess)

	const timeout = 500 * time.Millisecond
	start := time.Now()
	err := session.WaitForReady(d, sess, timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitForReady: got nil, want *ReadinessTimeoutError")
	}
	if !session.IsReadinessTimeout(err) {
		t.Errorf("WaitForReady error = %v (type %T), want *ReadinessTimeoutError", err, err)
	}
	// Sanity-check: elapsed must be at least the configured timeout (no
	// premature returns), and not absurdly larger (no run-away polling).
	if elapsed < timeout {
		t.Errorf("WaitForReady returned in %v, expected at least %v", elapsed, timeout)
	}
	if elapsed > timeout+1*time.Second {
		t.Errorf("WaitForReady returned in %v, expected close to %v", elapsed, timeout)
	}
}

// TestReadinessTimeoutError_Message verifies the error message format. The
// Ack and progress lines depend on this string ("not ready within Xs").
func TestReadinessTimeoutError_Message(t *testing.T) {
	cases := []struct {
		timeout time.Duration
		want    string
	}{
		{30 * time.Second, "not ready within 30s"},
		{1 * time.Minute, "not ready within 1m"},
		{90 * time.Second, "not ready within 1m30s"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.want, func(t *testing.T) {
			e := &session.ReadinessTimeoutError{SessionName: "x", Timeout: c.timeout}
			if got := e.Error(); got != c.want {
				t.Errorf("Error() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestIsReadinessTimeout_WrappedError verifies that IsReadinessTimeout sees
// through error wrapping (errors.As). The review-side gateReviewAgents wraps
// the error with "readiness gate for X: %w" before storing it in spawnErr.
func TestIsReadinessTimeout_WrappedError(t *testing.T) {
	inner := &session.ReadinessTimeoutError{SessionName: "x", Timeout: 30 * time.Second}
	wrapped := errorWrap("readiness gate for review-goal", inner)

	if !session.IsReadinessTimeout(wrapped) {
		t.Errorf("IsReadinessTimeout(%v) = false, want true (errors.As must see through wrapping)", wrapped)
	}

	// Sanity: a non-timeout error should not match.
	plainErr := errors.New("some other error")
	if session.IsReadinessTimeout(plainErr) {
		t.Error("IsReadinessTimeout(plain error) = true, want false")
	}
	if session.IsReadinessTimeout(nil) {
		t.Error("IsReadinessTimeout(nil) = true, want false")
	}
}

// errorWrap is a tiny test helper that wraps inner with a prefix while
// preserving the wrapped-error chain. Cannot use fmt.Errorf("%w: …", inner)
// directly without importing fmt; the explicit struct keeps this file's
// import list short.
type wrappedErr struct {
	prefix string
	inner  error
}

func (w *wrappedErr) Error() string { return w.prefix + ": " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func errorWrap(prefix string, inner error) error {
	return &wrappedErr{prefix: prefix, inner: inner}
}

// ── WaitForReady — argument validation ────────────────────────────────────────

// TestWaitForReady_RequiresDB verifies the nil-DB guard.
func TestWaitForReady_RequiresDB(t *testing.T) {
	if err := session.WaitForReady(nil, "x", time.Second); err == nil {
		t.Error("WaitForReady(nil, …): got nil, want error")
	}
}

// TestWaitForReady_RequiresSessionName verifies the empty-name guard.
func TestWaitForReady_RequiresSessionName(t *testing.T) {
	d := openReadinessTestDB(t)
	if err := session.WaitForReady(d, "", time.Second); err == nil {
		t.Error("WaitForReady(_, \"\"): got nil, want error")
	}
}

// TestWaitForReady_ZeroTimeoutFallsBackToDefault verifies that a zero or
// negative timeout uses DefaultReadinessTimeout. We run with no signal and
// observe that the wait extends well beyond a brief test window — but we
// don't actually wait the full default (30s); we cancel with a short
// goroutine that inserts the signal after 100ms to short-circuit.
func TestWaitForReady_ZeroTimeoutFallsBackToDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode")
	}
	d := openReadinessTestDB(t)
	const sess = "myrepo@zero-timeout"
	seedSession(t, d, sess)

	// Inject the signal quickly so the test does not wait 30s.
	go func() {
		time.Sleep(150 * time.Millisecond)
		evt := db.Event{
			ID: "z", SessionName: sess, Repo: "t", Worktree: "/t",
			Type: "state_change", Payload: `{"state":"active"}`,
		}
		_ = d.WriteEvent(evt)
	}()

	start := time.Now()
	if err := session.WaitForReady(d, sess, 0); err != nil {
		t.Fatalf("WaitForReady(_, _, 0): %v", err)
	}
	elapsed := time.Since(start)
	// Sanity: the call returned long before the default 30s — proving the
	// default did not somehow cap it short, and that the signal was honoured.
	if elapsed > 3*time.Second {
		t.Errorf("WaitForReady took %v with zero timeout — expected to return on signal, not wait the full default", elapsed)
	}
	// Also: the function did NOT return immediately (which would happen if
	// timeout=0 was treated as "deadline already passed"). It must have
	// polled for at least the time before the signal arrived.
	if elapsed < 100*time.Millisecond {
		t.Errorf("WaitForReady took %v with zero timeout — expected to poll until signal, not return immediately", elapsed)
	}
}
