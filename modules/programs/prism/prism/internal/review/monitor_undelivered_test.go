package review_test

// Tests for #3345: a review-complete prompt that the monitor cannot deliver
// is not lost silently.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// undeliveredSetup is a pi worker in reviewing, with a finished round of
// one FAIL verdict, and a coordinator in the same repo.
type undeliveredSetup struct {
	bus      *sidecartest.Bus
	d        *db.DB
	worker   string
	prNumber string
	opts     review.MonitorOpts
	fallback string
}

func newUndeliveredSetup(t *testing.T, worker, prNumber string) *undeliveredSetup {
	t.Helper()
	const coordinator = "prism-test@main"
	bus := sidecartest.NewIsolated(t, coordinator)
	d := bus.DB
	if err := d.QueryRow(
		"UPDATE agent_status SET root_agent_name = 'coordinator' WHERE session_name = ? RETURNING session_name", coordinator,
	).Scan(new(string)); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	if err := d.UpsertStatus(worker, "prism-test", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET harness = 'pi' WHERE session_name = ? RETURNING session_name", worker,
	).Scan(new(string)); err != nil {
		t.Fatalf("set worker harness: %v", err)
	}

	groupID, err := d.RegisterGroupWithPR(worker, prNumber, 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	agentSession := worker + "~review-1-review-goal"
	if err := d.UpsertStatus(agentSession, "prism-test", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("seed review agent: %v", err)
	}
	if err := d.SetGroupID(agentSession, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	seedAssistantEvent(t, d, agentSession, "Blocking issue.\n<verdict>FAIL</verdict>")

	fallback := filepath.Join(os.TempDir(), fmt.Sprintf("prism-review-%s-round-1-result.md", prNumber))
	t.Cleanup(func() { _ = os.Remove(fallback) })

	return &undeliveredSetup{
		bus: bus, d: d, worker: worker, prNumber: prNumber, fallback: fallback,
		opts: review.MonitorOpts{
			GroupID:              groupID,
			WorkerSession:        worker,
			PRNumber:             prNumber,
			Round:                1,
			Agents:               []review.Agent{{Name: "review-goal"}},
			AgentSessions:        []string{agentSession},
			DBPath:               d.Path(),
			PollInterval:         10 * time.Millisecond,
			MaxDeliveryRetries:   0,
			DeliveryRetryBackoff: time.Millisecond,
		},
	}
}

// assertErrorAndNotice checks the worker state, the state_change note, and
// that one coordinator notice holds each of want.
func (s *undeliveredSetup) assertErrorAndNotice(t *testing.T, want ...string) {
	t.Helper()
	st, err := s.d.CurrentStatus(s.worker)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(worker): %v", err)
	}
	if st.State != "error" {
		t.Errorf("worker state after the failed delivery = %q, want error", st.State)
	}
	var notes int
	if err := s.d.QueryRow(
		`SELECT COUNT(*) FROM agent_events WHERE session_name = ? AND type = 'state_change' AND payload LIKE '%review-complete not delivered%'`, s.worker,
	).Scan(&notes); err != nil || notes != 1 {
		t.Errorf("state_change events with the undelivered note = %d (err %v), want 1", notes, err)
	}
	want = append(want,
		"did not receive the review-complete prompt of review round 1 for PR #"+s.prNumber,
		"The session is in state error.",
		"The result is in "+s.fallback,
		"prism restore --session "+s.worker,
	)
	for _, w := range want {
		if !s.bus.BodyContains(w) {
			t.Errorf("no coordinator notice contains %q; bodies: %q", w, s.bus.CopyBodies())
		}
	}
	var audit int
	if err := s.d.QueryRow(
		`SELECT COUNT(*) FROM bus_messages WHERE from_session = ? AND delivered_at IS NOT NULL`, s.worker,
	).Scan(&audit); err != nil || audit != 1 {
		t.Errorf("delivered bus_messages audit rows = %d (err %v), want 1", audit, err)
	}
}

// TestMonitorFunc_DeliveryFailure_ReportsUndelivered covers a worker whose
// sidecar does not answer. The monitor wrote active before it tried to
// deliver. After the retries fail, the worker must show error (AC2), keep the
// prompt for a new sidecar, and the coordinator must get a notice (AC3).
func TestMonitorFunc_DeliveryFailure_ReportsUndelivered(t *testing.T) {
	s := newUndeliveredSetup(t, "prism-test@dead-sidecar", "99345")

	if err := review.MonitorFunc(s.opts); err == nil {
		t.Fatal("MonitorFunc returned nil, want the delivery error")
	}

	s.assertErrorAndNotice(t, "prism keeps the prompt, and a new sidecar of the session sends it")
	rows, err := s.d.LoadPendingReplayDeliveries(s.worker)
	if err != nil {
		t.Fatalf("LoadPendingReplayDeliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].DeliveryID != review.RecoveryDeliveryID(s.opts.GroupID) || rows[0].Source != "review-complete" ||
		!strings.Contains(rows[0].Text, "PR #"+s.prNumber) {
		t.Errorf("pending replay rows = %+v, want the review-complete prompt kept for a new sidecar", rows)
	}
}

// TestMonitorFunc_DeliveryFailure_SidecarAnswers_DoesNotKeepPrompt covers a
// worker sidecar that answers but refuses the delivery. That sidecar loads
// the replay buffer only at its next start, so a kept row can deliver the
// result a second time. The monitor must not keep it, and the notice must
// say so.
func TestMonitorFunc_DeliveryFailure_SidecarAnswers_DoesNotKeepPrompt(t *testing.T) {
	s := newUndeliveredSetup(t, "prism-test@refusing-sidecar", "99346")

	sockPath, err := session.SidecarHostAPIPath(s.worker)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "refused", http.StatusInternalServerError)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	if err := review.MonitorFunc(s.opts); err == nil {
		t.Fatal("MonitorFunc returned nil, want the delivery error")
	}

	s.assertErrorAndNotice(t, "A sidecar of the session answers, so prism does not keep the prompt")
	if rows, err := s.d.LoadPendingReplayDeliveries(s.worker); err != nil || len(rows) != 0 {
		t.Errorf("pending replay rows = %+v (err %v), want none while a sidecar answers", rows, err)
	}
}
