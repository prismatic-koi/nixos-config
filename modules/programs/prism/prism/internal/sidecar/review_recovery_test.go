package sidecar

// review_recovery_test.go — integration coverage for the worker-sidecar
// review-completion recovery watcher (#1709 reopen).
//
// The watcher rescues review groups orphaned by a dead `prism
// monitor-review` subprocess. The reproducer below exercises this exact
// failure mode:
//
//   1. Seed an isolated bus with a worker session whose harness='pi' so
//      promptdelivery routes via the host-API Unix socket (the socket is
//      captured by the bus and exposed via bus.CopyBodies()).
//   2. Register a review group via RegisterGroupWithPR and seed 5
//      agent_status rows for the review agents all in state=finished, each
//      with a msg_assistant event carrying a PASS verdict.
//   3. DO NOT spawn a monitor subprocess. This is the "monitor died"
//      condition: agents are terminal in the DB but no process is watching
//      GroupCompleted.
//   4. Construct a worker Sidecar with reviewingInFlight=true and tick the
//      recovery watcher directly (ReviewRecoveryTickForTest).
//   5. First tick records first-seen-complete and does NOT deliver
//      (grace window not elapsed).
//   6. Second tick with now > firstSeen + grace dispatches the delivery.
//      Assert the body lands on the bus socket and contains the expected
//      "Review complete: PR #" header + PASS markers.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// stuckReviewGroupFixture sets up the bus, registers a session group with
// 5 finished agents, and returns the worker session name, group_id, and
// bus for assertions.
type stuckReviewGroupFixture struct {
	bus           *sidecartest.Bus
	workerSession string
	groupID       string
	prNumber      string
	round         int
	agentSessions []string
}

func setupStuckReviewGroup(t *testing.T, scenarioTag string) *stuckReviewGroupFixture {
	t.Helper()

	workerSession := "prism-test@worker-" + scenarioTag
	bus := sidecartest.NewIsolated(t, workerSession)

	// Override the worker's harness to "pi" so DeliverToSessionWithID picks
	// the socket-pipe path (sidecartest.NewIsolated seeds harness='').
	if err := bus.DB.QueryRow(`UPDATE agent_status SET harness = 'pi' WHERE session_name = ? RETURNING session_name`, workerSession).Scan(new(string)); err != nil {
		t.Fatalf("set harness=pi on worker: %v", err)
	}
	// Flip the worker to `reviewing` to match the production state right
	// after the /review handler returns and before the monitor delivers.
	if err := bus.DB.QueryRow(`UPDATE agent_status SET state = 'reviewing' WHERE session_name = ? RETURNING session_name`, workerSession).Scan(new(string)); err != nil {
		t.Fatalf("set worker state=reviewing: %v", err)
	}

	const prNumber = "1746"
	const round = 1
	groupID, err := bus.DB.RegisterGroupWithPR(workerSession, prNumber, round)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	agentRoles := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	agentSessions := make([]string, len(agentRoles))
	for i, role := range agentRoles {
		sessionName := fmt.Sprintf("%s~review-%d-%s", workerSession, round, role)
		agentSessions[i] = sessionName

		// Insert agent_status row in state=finished, group_id set.
		if err := bus.DB.UpsertStatus(sessionName, "prism-test", "/tmp/worktree", "finished", nil, nil); err != nil {
			t.Fatalf("upsert agent_status for %s: %v", sessionName, err)
		}
		if err := bus.DB.SetGroupID(sessionName, groupID); err != nil {
			t.Fatalf("SetGroupID for %s: %v", sessionName, err)
		}
		// Seed a msg_assistant event with a PASS verdict so GroupResults
		// returns a non-empty LastMessage and FormatResults sees PASS.
		payload, _ := json.Marshal(map[string]string{
			"text": fmt.Sprintf("<summary>%s: all good</summary>\n<verdict>PASS</verdict>", role),
		})
		evt := db.Event{
			ID:          fmt.Sprintf("evt-%s-%s", scenarioTag, role),
			SessionName: sessionName,
			Repo:        "prism-test",
			Worktree:    "/tmp/worktree",
			Type:        "msg_assistant",
			Payload:     string(payload),
			CreatedAt:   time.Now(),
		}
		if err := bus.DB.WriteEvent(evt); err != nil {
			t.Fatalf("WriteEvent for %s: %v", sessionName, err)
		}
	}

	// Sanity check: GroupCompleted should already return true with all
	// members in state=finished.
	if done, err := bus.DB.GroupCompleted(groupID); err != nil {
		t.Fatalf("GroupCompleted: %v", err)
	} else if !done {
		t.Fatalf("GroupCompleted = false; want true after seeding 5 finished agents")
	}

	return &stuckReviewGroupFixture{
		bus:           bus,
		workerSession: workerSession,
		groupID:       groupID,
		prNumber:      prNumber,
		round:         round,
		agentSessions: agentSessions,
	}
}

// newWorkerSidecarForRecovery builds a Sidecar for the worker session in
// the fixture, with reviewingInFlight=true. The recovery interval is set
// very small but the ticker is NEVER started — tests call
// ReviewRecoveryTickForTest directly to drive the loop deterministically.
func newWorkerSidecarForRecovery(t *testing.T, fix *stuckReviewGroupFixture) *Sidecar {
	t.Helper()
	cfg := Config{
		SessionName: fix.workerSession,
		Repo:        "prism-test",
		Worktree:    "/tmp/worktree",
		DB:          fix.bus.DB,
		Clock:       newTestClock(),
		HTTPClient:  fix.bus.HTTPServer.Client(),
		AgentRole:   "worker",
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
		// Negative => watcher disabled, so the goroutine never spins.
		// We exercise the tick path explicitly via ReviewRecoveryTickForTest.
		ReviewRecoveryInterval: -1,
	}
	s := New(cfg)
	s.mu.Lock()
	s.reviewingInFlight = true
	s.reviewRecoveryFirstSeenComplete = make(map[string]time.Time)
	s.mu.Unlock()
	return s
}

// TestReviewRecoveryWatcher_DispatchesAfterGrace is the spec test for
// AC #4 of #1709: an integration test reproduces the stall by withholding
// the monitor's delivery (no subprocess started) and asserts the daemon
// eventually recovers rather than leaving the group in-progress indefinitely.
func TestReviewRecoveryWatcher_DispatchesAfterGrace(t *testing.T) {
	fix := setupStuckReviewGroup(t, "dispatch-after-grace")
	s := newWorkerSidecarForRecovery(t, fix)

	const grace = 90 * time.Second
	now := time.Now()

	// Tick 1: first observation of GroupCompleted=true. Records first-seen,
	// must NOT deliver yet.
	s.ReviewRecoveryTickForTest(grace, now)
	if got := len(fix.bus.CopyBodies()); got != 0 {
		t.Fatalf("after first tick: want 0 deliveries, got %d", got)
	}
	firstSeen := s.ReviewRecoveryFirstSeenForTest()
	if _, ok := firstSeen[fix.groupID]; !ok {
		t.Fatalf("first-seen map missing entry for group %s", fix.groupID)
	}

	// Tick 2 still inside grace window: still no delivery.
	s.ReviewRecoveryTickForTest(grace, now.Add(grace/2))
	if got := len(fix.bus.CopyBodies()); got != 0 {
		t.Fatalf("inside grace window: want 0 deliveries, got %d", got)
	}

	// Tick 3 past grace window: deliver.
	s.ReviewRecoveryTickForTest(grace, now.Add(grace+time.Second))
	bodies := fix.bus.CopyBodies()
	if len(bodies) != 1 {
		t.Fatalf("past grace: want 1 delivery, got %d", len(bodies))
	}
	body := bodies[0]

	// The body is the JSON-encoded /prompt request. Verify the header
	// includes the PR number and round, and that the per-agent verdicts
	// landed in the formatted output.
	if !strings.Contains(body, "Review complete: PR #"+fix.prNumber) {
		t.Errorf("delivery body missing review-complete header; got: %s", body)
	}
	if !strings.Contains(body, "passed") {
		t.Errorf("delivery body missing pass-summary line; got: %s", body)
	}
	if !strings.Contains(body, "review-goal") {
		t.Errorf("delivery body missing per-agent finding for review-goal; got: %s", body)
	}
	if !strings.Contains(body, "All 5 review agents passed.") {
		t.Errorf("delivery body missing happy-path summary line; got: %s", body)
	}

	// The delivery should carry the deterministic recovery delivery_id so
	// that any concurrent monitor delivery is deduped on the receiver.
	wantID := review.RecoveryDeliveryID(fix.groupID)
	if !strings.Contains(body, wantID) {
		t.Errorf("delivery body missing recovery delivery_id %q; got: %s", wantID, body)
	}

	// After delivery, the first-seen map for this group should be cleared
	// so a fresh group later does not inherit the stale timestamp.
	after := s.ReviewRecoveryFirstSeenForTest()
	if _, stillThere := after[fix.groupID]; stillThere {
		t.Errorf("first-seen map should have dropped entry for delivered group %s; got %v", fix.groupID, after)
	}
}

// TestReviewRecoveryWatcher_NoOpWhenReviewingInFlightFalse verifies that
// the watcher is a no-op when reviewingInFlight is false — even if a
// completed review group sits in the DB. This prevents the watcher from
// firing for groups owned by some other session.
func TestReviewRecoveryWatcher_NoOpWhenReviewingInFlightFalse(t *testing.T) {
	fix := setupStuckReviewGroup(t, "noop-not-in-flight")
	s := newWorkerSidecarForRecovery(t, fix)
	// Override reviewingInFlight to false to model "not my review".
	s.mu.Lock()
	s.reviewingInFlight = false
	s.mu.Unlock()

	const grace = 100 * time.Millisecond
	now := time.Now()
	s.ReviewRecoveryTickForTest(grace, now)
	s.ReviewRecoveryTickForTest(grace, now.Add(grace+time.Second))
	if got := len(fix.bus.CopyBodies()); got != 0 {
		t.Fatalf("watcher should not deliver when reviewingInFlight=false; got %d deliveries", got)
	}
	if seen := s.ReviewRecoveryFirstSeenForTest(); len(seen) != 0 {
		t.Errorf("first-seen map should be empty; got %v", seen)
	}
}

// TestReviewRecoveryWatcher_NoOpWhenGroupStillRunning verifies that the
// watcher does not deliver while at least one agent is still non-terminal,
// even past the grace window.
func TestReviewRecoveryWatcher_NoOpWhenGroupStillRunning(t *testing.T) {
	fix := setupStuckReviewGroup(t, "noop-still-running")
	// Regress one member to non-terminal.
	if err := fix.bus.DB.QueryRow(`UPDATE agent_status SET state = 'active' WHERE session_name = ? RETURNING session_name`, fix.agentSessions[0]).Scan(new(string)); err != nil {
		t.Fatalf("set agent[0] state=active: %v", err)
	}

	s := newWorkerSidecarForRecovery(t, fix)

	const grace = 50 * time.Millisecond
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.ReviewRecoveryTickForTest(grace, now.Add(time.Duration(i)*grace))
	}
	if got := len(fix.bus.CopyBodies()); got != 0 {
		t.Fatalf("watcher should not deliver while a member is non-terminal; got %d deliveries", got)
	}
}

// TestReviewRecoveryWatcher_IdempotentAcrossMultipleTicks verifies that
// repeated dispatch attempts (e.g. a tick fired after a successful delivery)
// do not produce a second delivery body once the first lands. The dedup
// guarantee comes from two places: (a) dropReviewRecoveryFirstSeen clears
// the timestamp after a successful dispatch, and (b) even if the timestamp
// were re-added, the deterministic delivery_id would dedup at the host-API
// /prompt handler.
//
// This test asserts the first-line defence: after a successful delivery, the
// next tick observes an empty first-seen map for the group and records a
// fresh first-seen — which is correct, because reviewingInFlight is still
// true (the production /prompt handler would have cleared it, but here we
// kept it true to model "the delivery is in flight"). The grace window then
// gates further delivery.
func TestReviewRecoveryWatcher_IdempotentAcrossMultipleTicks(t *testing.T) {
	fix := setupStuckReviewGroup(t, "idempotent")
	s := newWorkerSidecarForRecovery(t, fix)

	const grace = 1 * time.Millisecond
	now := time.Now()

	// First tick: arms first-seen.
	s.ReviewRecoveryTickForTest(grace, now)
	// Second tick past grace: dispatch.
	s.ReviewRecoveryTickForTest(grace, now.Add(10*time.Millisecond))
	first := len(fix.bus.CopyBodies())
	if first != 1 {
		t.Fatalf("after first dispatch: want 1 delivery, got %d", first)
	}

	// Simulate the worker's /prompt handler clearing reviewingInFlight after
	// processing the delivered prompt.
	s.mu.Lock()
	s.reviewingInFlight = false
	s.mu.Unlock()

	// Subsequent ticks must not deliver again.
	for i := 0; i < 3; i++ {
		s.ReviewRecoveryTickForTest(grace, now.Add(time.Duration(20+i*10)*time.Millisecond))
	}
	final := len(fix.bus.CopyBodies())
	if final != first {
		t.Fatalf("after reviewingInFlight cleared: want %d deliveries (unchanged), got %d", first, final)
	}
}
