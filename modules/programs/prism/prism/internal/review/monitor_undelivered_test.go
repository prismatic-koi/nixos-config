package review_test

// Tests for #3345: a review-complete prompt that the monitor cannot deliver
// is not lost silently.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// TestMonitorFunc_DeliveryFailure_ReportsUndelivered covers a worker whose
// sidecar does not answer. The monitor wrote active before it tried to
// deliver. After the retries fail, the worker must show error (AC2), keep the
// prompt for a new sidecar, and the coordinator must get a notice (AC3).
func TestMonitorFunc_DeliveryFailure_ReportsUndelivered(t *testing.T) {
	const (
		coordinator = "prism-test@main"
		worker      = "prism-test@dead-sidecar"
		prNumber    = "99345"
	)
	bus := sidecartest.NewIsolated(t, coordinator)
	d := bus.DB
	if err := d.QueryRow(
		"UPDATE agent_status SET root_agent_name = 'coordinator' WHERE session_name = ? RETURNING session_name", coordinator,
	).Scan(new(string)); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}

	// A pi worker in reviewing. No sidecar listens on its host-API socket.
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

	fallbackPath := filepath.Join(os.TempDir(), fmt.Sprintf("prism-review-%s-round-1-result.md", prNumber))
	t.Cleanup(func() { _ = os.Remove(fallbackPath) })

	err = review.MonitorFunc(review.MonitorOpts{
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
	})
	if err == nil {
		t.Fatal("MonitorFunc returned nil, want the delivery error")
	}

	st, err := d.CurrentStatus(worker)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(worker): %v", err)
	}
	if st.State != "error" {
		t.Errorf("worker state after the failed delivery = %q, want error", st.State)
	}
	var notes int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM agent_events WHERE session_name = ? AND type = 'state_change' AND payload LIKE '%review-complete not delivered%'`, worker,
	).Scan(&notes); err != nil || notes != 1 {
		t.Errorf("state_change events with the undelivered note = %d (err %v), want 1", notes, err)
	}

	rows, err := d.LoadPendingReplayDeliveries(worker)
	if err != nil {
		t.Fatalf("LoadPendingReplayDeliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].DeliveryID != review.RecoveryDeliveryID(groupID) || rows[0].Source != "review-complete" ||
		!strings.Contains(rows[0].Text, "PR #"+prNumber) {
		t.Errorf("pending replay rows = %+v, want the review-complete prompt kept for a new sidecar", rows)
	}

	for _, want := range []string{
		"did not receive the review-complete prompt of review round 1 for PR #" + prNumber,
		"The result is in " + fallbackPath,
		"prism restore --session " + worker,
	} {
		if !bus.BodyContains(want) {
			t.Errorf("no coordinator notice contains %q; bodies: %q", want, bus.CopyBodies())
		}
	}
}
