package sidecar

// Tests for the requirement that reviewingInFlight must not be cleared on the
// same-session /prompt review-complete branch until the prompt frame is
// actually on the wire — either synchronously (DeliverPrompt success) or
// after flushPendingReplay re-enqueues a buffered entry.
//
// Two scenarios are covered:
//
//   1. Happy path (synchronous success): PI extension connected,
//      /prompt review-complete returns 200 with no buffered marker, and
//      reviewingInFlight is false after the handler returns.
//
//   2. Buffered-for-replay path: PI extension disconnected, /prompt
//      review-complete returns 200 with buffered:true, the entry carries
//      Source="review-complete", reviewingInFlight remains true through
//      the disconnect window (so a state_change{finished} arriving during
//      that window is suppressed by handleSessionFinished), and
//      flushPendingReplay clears the flag after the replayed enqueue
//      succeeds.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// TestHostAPI_Prompt_ReviewComplete_SyncSuccess_ClearsAfterDeliver is the
// happy-path test. It verifies that when DeliverPrompt
// succeeds synchronously, reviewingInFlight ends up false after the handler
// returns. This mirrors the existing
// TestHostAPI_Prompt_ReviewComplete_ClearsReviewingInFlight test but is kept
// here too so this fix has its own focused regression target — both the
// invert-order branch and the existing semantics are exercised in one place.
func TestHostAPI_Prompt_ReviewComplete_SyncSuccess_ClearsAfterDeliver(t *testing.T) {
	d := openTestDB(t)
	sessionName := "myrepo@piworker"
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed DB reviewing state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Connected pipe — DeliverPrompt will succeed synchronously.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"review results here","source":"review-complete"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Errorf("synchronous-success response should not carry buffered:true; body=%s", rr.Body.String())
	}

	// Flag is false on return.
	sc.mu.Lock()
	inFlight := sc.reviewingInFlight
	sc.mu.Unlock()
	if inFlight {
		t.Error("reviewingInFlight = true after synchronous review-complete delivery, want false")
	}

	// Sanity: frame really was enqueued.
	select {
	case <-fakePipeCh:
	default:
		t.Error("expected a frame in the pipe channel after synchronous review-complete delivery")
	}
}

// TestHostAPI_Prompt_ReviewComplete_Buffered_KeepsFlagAndSuppressesFinish is
// the regression test for the buffered-for-replay
// branch. The full timeline:
//
//  1. Worker sidecar is mid-review (reviewingInFlight=true, DB=reviewing).
//  2. PI extension is disconnected (harnessPipeOutCh is nil).
//  3. Monitor POSTs /prompt with source=review-complete.
//     - Response is 200 buffered:true.
//     - The buffered entry's Source is "review-complete".
//     - reviewingInFlight is STILL true (the invariant). Clearing it before
//     DeliverPrompt is even tried reopens the suppression-evasion race.
//  4. During the disconnect window, the worker's runtime emits a
//     state_change{finished} (simulated by calling handleSessionFinished
//     directly — equivalent to the path a real frame takes through
//     handlePipeFrame). The finished-debounce timer is created.
//  5. Fire the debounce: handleSessionFinished must observe
//     reviewingInFlight=true and SUPPRESS the transition. The DB state must
//     not become StateFinished — which also implies notifyCoordinator (the
//     branch that calls it inside the suppression-gated block) is NOT
//     invoked. This is the same suppression invariant guarded elsewhere.
//     This test re-asserts it for the buffered review-complete path.
//  6. Reconnect: install a fresh pipe channel and call flushPendingReplay
//     directly. The replayed frame is enqueued, and flushPendingReplay
//     clears reviewingInFlight (Source="review-complete" branch).
//  7. After flush, reviewingInFlight is false — the post-replay turn_start
//     will transition normally to active.
//
// The test uses a synthetic clock (newSocketPipeSidecarWithClock would also
// work; we use the simpler in-memory hostAPI pattern instead because the
// debounce timer and flushPendingReplay can both be driven directly).
func TestHostAPI_Prompt_ReviewComplete_Buffered_KeepsFlagAndSuppressesFinish(t *testing.T) {
	d := openTestDB(t)
	sessionName := "myrepo@piworker"
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed DB reviewing state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)
	clk := sc.cfg.Clock.(*testClock)

	// Step 1 + 2: reviewingInFlight=true, PI disconnected.
	sc.mu.Lock()
	sc.reviewingInFlight = true
	// harnessPipeOutCh remains nil — DeliverPrompt will return false.
	sc.mu.Unlock()

	// Step 3: POST /prompt with source=review-complete.
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"review results here","source":"review-complete","delivery_id":"d-1843"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("body = %q, want it to contain \"buffered\":true (PI disconnected so entry must be buffered)", rr.Body.String())
	}

	// Buffered entry must carry Source="review-complete" so the flush path
	// knows which entry's enqueue clears the flag.
	sc.mu.Lock()
	if got := len(sc.pendingReplayDeliveries); got != 1 {
		sc.mu.Unlock()
		t.Fatalf("pendingReplayDeliveries length = %d, want 1", got)
	}
	bufferedSource := sc.pendingReplayDeliveries[0].Source
	bufferedID := sc.pendingReplayDeliveries[0].DeliveryID
	sc.mu.Unlock()
	if bufferedSource != "review-complete" {
		t.Errorf("buffered entry Source = %q, want %q (tag must propagate so flush path can clear reviewingInFlight)", bufferedSource, "review-complete")
	}
	if bufferedID != "d-1843" {
		t.Errorf("buffered entry DeliveryID = %q, want %q (sanity)", bufferedID, "d-1843")
	}

	// reviewingInFlight must still be true after the buffered-path
	// /prompt returns. Clearing it before DeliverPrompt is even called exposes
	// the suppression-evasion race.
	sc.mu.Lock()
	inFlightAfterBuffer := sc.reviewingInFlight
	sc.mu.Unlock()
	if !inFlightAfterBuffer {
		t.Fatal("reviewingInFlight cleared after buffered review-complete /prompt — #1843 regression (this is exactly the bug the issue describes)")
	}

	// Step 4: simulate state_change{finished} arriving during the
	// disconnect window. handleSessionFinished installs a finished-debounce
	// timer; we wait for it (the test clock's WaitForTimerCount captures the
	// AfterFunc registration) and then fire it manually.
	sc.handleSessionFinished()

	timer := clk.WaitForTimerCount(1, 2*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer created after handleSessionFinished")
	}

	// Step 5: fire the debounce. The closure in
	// handleSessionFinished sees reviewingInFlight=true and suppresses the
	// transition. Without the fix, the pre-cleared flag would let the
	// closure overwrite DB state to StateFinished and call
	// goNotify(notifyCoordinator).
	timer.Fire()
	// Give the closure a brief window to run (it acquires s.mu, which is
	// not held here, so this is just a scheduling yield).
	time.Sleep(100 * time.Millisecond)

	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st == string(agent.StateFinished) {
		t.Errorf("session transitioned to StateFinished during the buffered-replay window — #1843 regression (state=%q, want anything but %q)", st, agent.StateFinished)
	}

	// Sanity: reviewingInFlight must still be true (the suppression path
	// does not clear it).
	sc.mu.Lock()
	inFlightAfterDebounce := sc.reviewingInFlight
	sc.mu.Unlock()
	if !inFlightAfterDebounce {
		t.Error("reviewingInFlight cleared by finished-debounce suppression path (should be unchanged)")
	}

	// Step 6: reconnect — install a fresh pipe channel and flush the buffer.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	sc.flushPendingReplay()

	// The replayed frame should be on the pipe channel.
	select {
	case <-fakePipeCh:
	default:
		t.Error("expected a replayed frame on the pipe channel after flushPendingReplay")
	}

	// Step 7: closing condition — reviewingInFlight is now false
	// (the Source="review-complete" branch in flushPendingReplay cleared it
	// after the successful re-enqueue).
	sc.mu.Lock()
	inFlightAfterFlush := sc.reviewingInFlight
	bufferLen := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if inFlightAfterFlush {
		t.Error("reviewingInFlight still true after flushPendingReplay re-enqueued the review-complete entry — flush path must clear it (AC #2)")
	}
	if bufferLen != 0 {
		t.Errorf("pendingReplayDeliveries length after flush = %d, want 0", bufferLen)
	}
}

// TestFlushPendingReplay_NonReviewSourceDoesNotClearFlag is a defensive
// regression test: flushPendingReplay must clear reviewingInFlight ONLY for
// entries whose Source is "review-complete". A buffered coordinator
// follow-up (Source=="") that happens to flush while a review is in flight
// must leave the flag alone — otherwise it reintroduces the class of
// "non-review prompt prematurely ends the reviewing window".
func TestFlushPendingReplay_NonReviewSourceDoesNotClearFlag(t *testing.T) {
	d := openTestDB(t)
	sessionName := "myrepo@piworker"
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed DB reviewing state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.pendingReplayDeliveries = []pendingReplayDelivery{
		{
			DeliveryID: "follow-up-1",
			Text:       "coordinator follow-up while review still in flight",
			DeliverAs:  "nextTurn",
			Source:     "", // non-review-complete
		},
	}
	sc.mu.Unlock()

	// Install a pipe channel so deliverPromptFrame succeeds.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	sc.flushPendingReplay()

	// Drain the channel (sanity).
	select {
	case <-fakePipeCh:
	default:
		t.Fatal("expected the follow-up frame to be enqueued")
	}

	// The flag must still be true — only Source=="review-complete" clears it.
	sc.mu.Lock()
	inFlight := sc.reviewingInFlight
	sc.mu.Unlock()
	if !inFlight {
		t.Error("flushPendingReplay cleared reviewingInFlight for a non-review-complete entry — must only clear for Source==\"review-complete\" (#1843 / #1372 invariant)")
	}
}
