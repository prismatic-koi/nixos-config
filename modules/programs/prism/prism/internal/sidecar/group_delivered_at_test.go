package sidecar

// group_delivered_at_test.go — integration coverage for the
// delivered_at column.
//
// The unit tests under internal/db/ verify that GroupCompleted short-
// circuits on delivered_at and that SetGroupDeliveredAt is first-write-
// wins. This file exercises the end-to-end write path:
//
//  1. positive — DeliverGroupResults against a fake host-API /prompt
//     returning 200 must set delivered_at, and ActiveReviewGroupForParent
//     must immediately return "" (no active group) even though the
//     agent_status rows are still in non-terminal states.
//  2. negative — when delivery fails (the worker's host-API socket is
//     unreachable), delivered_at must remain NULL so the verdict-rerun
//     path remains available.
//
// Both scenarios use sidecartest.NewIsolated per the isolation convention so
// no host-side prism state is touched.

import (
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// setupDeliveredAtFixture seeds a worker session, registers a review group
// with 5 active/idle members (no terminal states), and returns the DB and
// fixture details. Crucially, members are NOT in terminal states — so the
// GroupCompleted predicate returns false on member states alone. The test asserts
// that delivered_at flips the predicate to true regardless.
func setupDeliveredAtFixture(t *testing.T, scenarioTag string) (d *db.DB, workerSession, groupID string, sockPath string) {
	t.Helper()

	workerSession = "prism-test@worker-" + scenarioTag
	bus := sidecartest.NewIsolated(t, workerSession)
	d = bus.DB

	// Flip the worker to harness='pi' and state='reviewing' so the
	// promptdelivery socket-pipe path is exercised.
	if err := d.QueryRow(
		`UPDATE agent_status SET harness = 'pi', state = 'reviewing' WHERE session_name = ? RETURNING session_name`,
		workerSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("set harness=pi, state=reviewing: %v", err)
	}

	const prNumber = "2259"
	const round = 1
	var err error
	groupID, err = d.RegisterGroupWithPR(workerSession, prNumber, round)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	// Seed 5 members in NON-terminal states (idle + active). The whole
	// point is that delivered_at must work as a terminal
	// signal even when the agent_status rollup says the group is still
	// in-progress (that is the wedged-at-idle reproducer from the issue).
	memberStates := []struct {
		role  string
		state string
	}{
		{"review-goal", "idle"},
		{"review-code", "idle"},
		{"review-security", "active"},
		{"review-qa", "idle"},
		{"review-context", "active"},
	}
	for _, m := range memberStates {
		sessName := workerSession + "~review-1-" + m.role
		if err := d.UpsertStatus(sessName, "prism-test", "/tmp/worktree", m.state, nil, nil); err != nil {
			t.Fatalf("upsert agent_status for %s: %v", sessName, err)
		}
		if err := d.SetGroupID(sessName, groupID); err != nil {
			t.Fatalf("SetGroupID for %s: %v", sessName, err)
		}
	}

	// Sanity check: pre-delivery GroupCompleted MUST be false (members
	// are non-terminal). This is the wedged-at-idle reproducer.
	if done, err := d.GroupCompleted(groupID); err != nil {
		t.Fatalf("GroupCompleted pre-delivery: %v", err)
	} else if done {
		t.Fatalf("GroupCompleted pre-delivery returned true; expected false (members are non-terminal)")
	}
	// Sanity check: ActiveReviewGroupForParent must return groupID
	// because at least one member is non-terminal and delivered_at is
	// NULL — that is, the in-progress guard is active.
	if active, err := review.ActiveReviewGroupForParent(d, workerSession); err != nil {
		t.Fatalf("ActiveReviewGroupForParent pre-delivery: %v", err)
	} else if active != groupID {
		t.Fatalf("ActiveReviewGroupForParent pre-delivery = %q, want %q (in-progress guard should be active)", active, groupID)
	}

	sockPath, err = session.SidecarHostAPIPath(workerSession)
	if err != nil {
		t.Fatalf("resolve sock path: %v", err)
	}
	return d, workerSession, groupID, sockPath
}

// TestDeliverGroupResults_SetsDeliveredAt_UnblocksGuard is the positive
// integration test. It mounts a fake /prompt server on the
// worker's host-API Unix socket that returns 200, calls
// DeliverGroupResults, and verifies:
//
//   - delivered_at is set on the session_groups row.
//   - ActiveReviewGroupForParent immediately returns "" (no active group)
//     even though the 5 agent_status members are still idle/active.
//   - GroupCompleted returns true via the short-circuit.
//
// This is the wedged-at-idle reproducer.
func TestDeliverGroupResults_SetsDeliveredAt_UnblocksGuard(t *testing.T) {
	d, workerSession, groupID, sockPath := setupDeliveredAtFixture(t, "delivered-at-positive")

	// Mount a fake /prompt server on the worker's host-API socket. The
	// promptdelivery socket-pipe path dials this socket; a 200 response
	// makes DeliverToSessionWithID return nil, which is the success branch
	// in DeliverGroupResults that writes delivered_at.
	srv, cleanup := startFakePromptServer(t, sockPath, http.StatusOK, `{"replayed":false}`)
	defer cleanup()
	_ = srv

	res, err := review.DeliverGroupResults(d, groupID, review.RecoveryDeliveryID(groupID))
	if err != nil {
		t.Fatalf("DeliverGroupResults: %v", err)
	}
	if !res.Delivered {
		t.Fatalf("DeliverGroupResults result: Delivered=false, want true")
	}

	// delivered_at must now be non-NULL on the session_groups row.
	var deliveredAt *int64
	if err := d.QueryRow(
		`SELECT delivered_at FROM session_groups WHERE group_id = ?`, groupID,
	).Scan(&deliveredAt); err != nil {
		t.Fatalf("read delivered_at: %v", err)
	}
	if deliveredAt == nil {
		t.Fatal("delivered_at is NULL after successful DeliverGroupResults; want non-NULL")
	}
	// Within a reasonable window of "now" — tiny sanity check that the
	// value is an epoch-ms not some random integer.
	nowMs := time.Now().UnixMilli()
	if *deliveredAt < nowMs-int64(time.Minute/time.Millisecond) || *deliveredAt > nowMs+int64(time.Minute/time.Millisecond) {
		t.Errorf("delivered_at = %d, want within ±60s of now=%d (epoch-ms)", *deliveredAt, nowMs)
	}

	// ActiveReviewGroupForParent must now return "" — the in-progress
	// guard is released even though the 5 members are still in idle/active
	// states. This is the wedged-at-idle fix in action.
	active, err := review.ActiveReviewGroupForParent(d, workerSession)
	if err != nil {
		t.Fatalf("ActiveReviewGroupForParent post-delivery: %v", err)
	}
	if active != "" {
		t.Errorf("ActiveReviewGroupForParent post-delivery = %q, want \"\" (delivered_at must release the in-progress guard regardless of member state)", active)
	}

	// GroupCompleted must also return true — the short-circuit fires
	// before the agent_status-based predicate is consulted.
	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted post-delivery: %v", err)
	}
	if !done {
		t.Error("GroupCompleted post-delivery = false, want true (delivered_at must short-circuit to terminal)")
	}
}

// TestDeliverGroupResults_FailedDelivery_LeavesDeliveredAtNULL is the
// negative integration test. When the /prompt server returns
// 500 (delivery rejected), DeliverGroupResults must NOT write
// delivered_at — the verdict-rerun path remains available because
// GroupCompleted still derives from the agent_status predicate.
func TestDeliverGroupResults_FailedDelivery_LeavesDeliveredAtNULL(t *testing.T) {
	d, workerSession, groupID, sockPath := setupDeliveredAtFixture(t, "delivered-at-negative")

	// Mount a fake /prompt server that returns 500 on every call.
	srv, cleanup := startFakePromptServer(t, sockPath, http.StatusInternalServerError, `{"error":"simulated failure"}`)
	defer cleanup()
	_ = srv

	res, err := review.DeliverGroupResults(d, groupID, review.RecoveryDeliveryID(groupID))
	if err == nil {
		t.Fatal("DeliverGroupResults: got nil error, want non-nil (delivery should have failed)")
	}
	if res != nil && res.Delivered {
		t.Fatalf("DeliverGroupResults result: Delivered=true on a 500 response; want false")
	}

	// delivered_at must remain NULL on the session_groups row.
	var deliveredAt *int64
	if err := d.QueryRow(
		`SELECT delivered_at FROM session_groups WHERE group_id = ?`, groupID,
	).Scan(&deliveredAt); err != nil {
		t.Fatalf("read delivered_at: %v", err)
	}
	if deliveredAt != nil {
		t.Errorf("delivered_at = %d after failed DeliverGroupResults; want NULL so the verdict-rerun path remains available", *deliveredAt)
	}

	// The in-progress guard must STILL be active (that is, the agent_status-
	// based predicate is consulted normally for the unblocked path).
	active, err := review.ActiveReviewGroupForParent(d, workerSession)
	if err != nil {
		t.Fatalf("ActiveReviewGroupForParent post-failure: %v", err)
	}
	if active != groupID {
		t.Errorf("ActiveReviewGroupForParent post-failure = %q, want %q (delivered_at NULL must leave the agent_status predicate authoritative)", active, groupID)
	}
}

// startFakePromptServer binds an HTTP server to the given Unix socket path
// that returns (statusCode, body) for every request. The server is shut
// down by the returned cleanup function.
func startFakePromptServer(t *testing.T, sockPath string, statusCode int, body string) (*http.Server, func()) {
	t.Helper()

	// Ensure the parent directory exists.
	if idx := strings.LastIndex(sockPath, "/"); idx > 0 {
		if err := os.MkdirAll(sockPath[:idx], 0o700); err != nil {
			t.Fatalf("mkdir parent of sock path: %v", err)
		}
	}
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Close()
		_ = ln.Close()
	}
	// Give the listener a brief settling window.
	time.Sleep(10 * time.Millisecond)
	return srv, cleanup
}
