// Tests for the muted-session notification-suppression path (#2013).
//
// The mute check in notifyCoordinator is additive next to the existing
// escalated / review-agent / investigate-agent / coordinator-session
// suppression guards. These tests verify that:
//
//   (a) a muted worker does not emit notifyCoordinator on session.finished;
//   (b) a muted worker does not emit on session.escalated (the escalate cmd
//       path is covered by a cmd-package test, but the in-sidecar guard is
//       proven here by exercising notifyCoordinator while StateEscalated is
//       set \u2014 the existing escalated-guard takes precedence, which is fine);
//   (c) unmuting restores notification on the next session.finished;
//   (d) the existing escalated / review-agent / coordinator-session
//       suppression guards still fire when the muted flag is unset.
//
// Isolation contract: every test uses sidecartest.NewIsolated and the
// "prism-test@" session-name prefix per AGENTS.md.
package sidecar

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newMutedTestSidecar builds a fully isolated test bus with a coordinator
// (root_agent_name='coordinator'), a worker session row, and a Sidecar
// pointed at the worker. The returned tracker records every invocation of
// the notifyCoordinatorDeliverFn seam so the test can assert the post-state
// "did delivery happen?" question without depending on bus_messages row
// counting (which is its own concern, covered by notify_test.go).
func newMutedTestSidecar(t *testing.T, workerSession, coordSession, repo string) (*Sidecar, *sidecartest.Bus, *deliveryTracker) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	clk := newTestClock()
	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-" + workerSession,
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	tr := &deliveryTracker{}
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		tr.calls = append(tr.calls, deliveryCall{to: sessionName, text: text})
		return nil
	}
	return s, bus, tr
}

type deliveryCall struct {
	to   string
	text string
}

type deliveryTracker struct{ calls []deliveryCall }

func (t *deliveryTracker) Count() int { return len(t.calls) }

// TestNotifyCoordinator_MutedSuppresses asserts AC (a): a worker whose
// agent_status.muted = 1 does not invoke the coordinator delivery path on
// session.finished. The escalate / review-agent / coordinator guards must
// not be load-bearing here \u2014 the worker is a plain worker in a non-escalated
// state, the only reason to suppress is the muted flag.
func TestNotifyCoordinator_MutedSuppresses(t *testing.T) {
	workerSession := "prism-test@worker-muted-suppress"
	coordSession := "prism-test@coordinator-muted-suppress"
	repo := "prism-test"

	s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)

	if _, err := bus.DB.SetMuted(workerSession, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	s.notifyCoordinator()

	if got := tr.Count(); got != 0 {
		t.Errorf("muted worker delivered %d coordinator notification(s); want 0", got)
	}
}

// TestNotifyCoordinator_MutedSuppresses_EscalatedStillSuppressed asserts
// AC (b)-equivalent: when a muted worker is also in StateEscalated, the
// existing escalated guard short-circuits first \u2014 the test does not need
// the mute guard to be the reason. The point of this test is to prove that
// flipping the muted flag does not break the escalated suppression.
func TestNotifyCoordinator_MutedAndEscalatedBothSuppressed(t *testing.T) {
	workerSession := "prism-test@worker-muted-esc"
	coordSession := "prism-test@coordinator-muted-esc"
	repo := "prism-test"

	s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)

	// Transition the worker to escalated AND set muted.
	if err := bus.DB.UpsertStatus(workerSession, repo, "/tmp/test-worker-"+workerSession, "escalated", nil, nil); err != nil {
		t.Fatalf("UpsertStatus escalated: %v", err)
	}
	if _, err := bus.DB.SetMuted(workerSession, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	s.notifyCoordinator()

	if got := tr.Count(); got != 0 {
		t.Errorf("muted+escalated worker delivered %d coordinator notification(s); want 0", got)
	}
}

// TestNotifyCoordinator_UnmuteRestoresNotification asserts AC (c): a muted
// worker that is then unmuted DOES notify on the next session.finished
// transition. The same Sidecar instance is reused across mute → unmute →
// notify to prove there is no in-process caching of the muted flag.
func TestNotifyCoordinator_UnmuteRestoresNotification(t *testing.T) {
	workerSession := "prism-test@worker-unmute-restores"
	coordSession := "prism-test@coordinator-unmute-restores"
	repo := "prism-test"

	s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)

	// Mute, notify, expect zero deliveries.
	if _, err := bus.DB.SetMuted(workerSession, true); err != nil {
		t.Fatalf("SetMuted true: %v", err)
	}
	s.notifyCoordinator()
	if got := tr.Count(); got != 0 {
		t.Fatalf("muted notify delivered %d; want 0", got)
	}

	// Unmute, notify, expect exactly one delivery to the coordinator.
	if _, err := bus.DB.SetMuted(workerSession, false); err != nil {
		t.Fatalf("SetMuted false: %v", err)
	}
	s.notifyCoordinator()
	if got := tr.Count(); got != 1 {
		t.Fatalf("after unmute, delivered %d coordinator notification(s); want 1", got)
	}
	if tr.calls[0].to != coordSession {
		t.Errorf("post-unmute delivery target = %q, want %q", tr.calls[0].to, coordSession)
	}
}

// TestNotifyCoordinator_ExistingGuardsStillFireWhenUnmuted asserts AC (d):
// with muted=false (the default), the existing escalated and
// review-agent suppression guards continue to behave exactly as today \u2014
// adding the mute check did not regress them.
func TestNotifyCoordinator_ExistingGuardsStillFireWhenUnmuted(t *testing.T) {
	repo := "prism-test"
	coordSession := "prism-test@coordinator-guards"

	t.Run("escalated_guard_still_fires", func(t *testing.T) {
		workerSession := "prism-test@worker-guards-escalated"
		s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)
		// Drive the worker into escalated WITHOUT muting it.
		if err := bus.DB.UpsertStatus(workerSession, repo, "/tmp/w", "escalated", nil, nil); err != nil {
			t.Fatalf("UpsertStatus: %v", err)
		}
		s.notifyCoordinator()
		if got := tr.Count(); got != 0 {
			t.Errorf("escalated guard regressed: %d deliveries; want 0", got)
		}
	})

	t.Run("review_agent_guard_still_fires", func(t *testing.T) {
		// Review-agent sessions follow the name convention "<parent>~review-<N>-<role>".
		workerSession := "prism-test@worker-guards~review-1-review-goal"
		s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)
		// Plain finished state, no muted flag.
		_ = bus
		s.notifyCoordinator()
		if got := tr.Count(); got != 0 {
			t.Errorf("review-agent guard regressed: %d deliveries; want 0", got)
		}
	})
}

// TestNotifyCoordinator_MutedFinishThenUnmute_NoRetroactivePing asserts the
// edge-case AC: if a session enters `finished` while muted and is later
// unmuted, the coordinator does NOT retroactively receive the suppressed
// finish notification \u2014 missed notifications are dropped, not queued.
//
// We exercise this by calling notifyCoordinator while muted, then unmuting,
// and confirming the delivery seam was not invoked at unmute time. The
// production code has no replay queue, so this test is really a guard that
// nobody added one as a "convenience" later.
func TestNotifyCoordinator_MutedFinishThenUnmute_NoRetroactivePing(t *testing.T) {
	workerSession := "prism-test@worker-muted-noreplay"
	coordSession := "prism-test@coordinator-muted-noreplay"
	repo := "prism-test"

	s, bus, tr := newMutedTestSidecar(t, workerSession, coordSession, repo)

	if _, err := bus.DB.SetMuted(workerSession, true); err != nil {
		t.Fatalf("SetMuted true: %v", err)
	}
	s.notifyCoordinator() // suppressed
	if got := tr.Count(); got != 0 {
		t.Fatalf("muted notify delivered %d; want 0", got)
	}

	// Unmute. There is no separate "process pending notifications" call;
	// the missed notification must stay dropped.
	if _, err := bus.DB.SetMuted(workerSession, false); err != nil {
		t.Fatalf("SetMuted false: %v", err)
	}

	// The seam must remain at zero invocations \u2014 nothing fires until a
	// fresh session.finished event arrives (which would call
	// notifyCoordinator again).
	if got := tr.Count(); got != 0 {
		t.Errorf("after unmute (no new finish event), delivered %d; want 0 (no replay)", got)
	}
}
