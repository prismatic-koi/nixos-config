package sidecar

// Tests for PI session prompt delivery via the host-API /prompt endpoint.
//
// These tests verify:
//  1. The /prompt same-session socket-pipe path rejects delivery when the
//     session is in "waiting" state (AC: edge-case waiting state guard).
//  2. The /prompt same-session socket-pipe path succeeds when the session is
//     in an active state.
//
// The tests use the hostAPIHandler() directly (no real Unix socket needed).

import (
	"net/http"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// newPiSidecarForHostAPITest creates a Sidecar configured for the pi harness
// and suitable for hostAPIHandler tests.
func newPiSidecarForHostAPITest(t *testing.T, sessionName, repo, role string, d *db.DB) *Sidecar {
	t.Helper()
	clk := newTestClock()
	cfg := Config{
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/" + sessionName,
		DB:          d,
		Clock:       clk,
		AgentRole:   role,
		HarnessName: "pi",
		Harness:     pih.New("", role, ""),
	}
	return New(cfg)
}

// TestHostAPI_Prompt_PiSession_WaitingStateRejected verifies that the /prompt
// same-session socket-pipe path returns HTTP 409 Conflict when the pi session
// is in "waiting" state, consistent with `prism prompt` CLI behaviour (#1364).
func TestHostAPI_Prompt_PiSession_WaitingStateRejected(t *testing.T) {
	d := openTestDB(t)

	sessionName := "myrepo@piworker"
	// Seed the session row with state="waiting".
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "waiting", nil, nil); err != nil {
		t.Fatalf("seed DB waiting state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	// POST to /prompt targeting this session (same-session).
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"hello pi"}`)

	// Expect 409 Conflict (session in waiting state).
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (waiting state rejection), body=%s", rr.Code, rr.Body.String())
	}

	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if errResp["error"] == "" {
		t.Error("expected error field in 409 response")
	}
	if errResp["error"] == "" {
		t.Errorf("error %q should not be empty", errResp["error"])
	}
}

// TestHostAPI_Prompt_PiSession_ActiveState_ConnectedPipe verifies that the
// /prompt same-session socket-pipe path succeeds when the session is active
// and the harness pipe is connected. Uses an actual pipe channel to simulate
// the connected state.
func TestHostAPI_Prompt_PiSession_ActiveState_ConnectedPipe(t *testing.T) {
	d := openTestDB(t)

	sessionName := "myrepo@piworker"
	// Seed the session row with state="active".
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed DB active state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	// Inject a fake pipe channel so DeliverPrompt has somewhere to write.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	// POST to /prompt targeting this session (same-session).
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"hello pi active"}`)

	// Expect 200 OK.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	// Verify the prompt frame was enqueued.
	select {
	case frame := <-fakePipeCh:
		if len(frame) == 0 {
			t.Error("expected non-empty frame in pipe channel")
		}
	default:
		t.Error("expected a frame in pipe channel after /prompt, but channel was empty")
	}
}

// TestHostAPI_Prompt_PiSession_NotConnected verifies that /prompt returns
// 503 Service Unavailable when the pi harness pipe is not connected (no active
// extension connection).
func TestHostAPI_Prompt_PiSession_NotConnected(t *testing.T) {
	d := openTestDB(t)

	sessionName := "myrepo@piworker"
	// Seed the session row with state="active".
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed DB active state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)
	// harnessPipeOutCh is nil (not connected).

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"hello pi disconnected"}`)

	// Expect 503 Service Unavailable (pipe not connected).
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (pipe not connected), body=%s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Prompt_ReviewComplete_ClearsReviewingInFlight verifies that a
// /prompt delivery with source="review-complete" clears the reviewingInFlight
// flag, allowing the subsequent turn_start to transition normally to active.
// This is the primary AC #7 gate for #1372.
func TestHostAPI_Prompt_ReviewComplete_ClearsReviewingInFlight(t *testing.T) {
	d := openTestDB(t)

	sessionName := "myrepo@piworker"
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed DB reviewing state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	// Set reviewingInFlight = true (simulating the /review handler).
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Inject a fake pipe channel so DeliverPrompt succeeds.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	// POST /prompt with source="review-complete".
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"review results here","source":"review-complete"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	// reviewingInFlight must now be false.
	sc.mu.Lock()
	inFlight := sc.reviewingInFlight
	sc.mu.Unlock()
	if inFlight {
		t.Error("reviewingInFlight = true after review-complete delivery, want false")
	}
}

// TestHostAPI_Prompt_NonReviewComplete_DoesNotClearReviewingInFlight verifies
// that a /prompt delivery without source="review-complete" (e.g. a coordinator
// follow-up) does NOT clear reviewingInFlight. Clearing on non-review prompts
// would prematurely end the reviewing window and reintroduce the race (#1372, AC #7).
func TestHostAPI_Prompt_NonReviewComplete_DoesNotClearReviewingInFlight(t *testing.T) {
	d := openTestDB(t)

	sessionName := "myrepo@piworker"
	if err := d.UpsertStatus(sessionName, "myrepo", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("seed DB reviewing state: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, sessionName, "myrepo", "worker", d)

	// Set reviewingInFlight = true.
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Inject a fake pipe channel.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	// POST /prompt without source (coordinator follow-up scenario).
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"coordinator follow-up"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	// reviewingInFlight must still be true — coordinator prompts must not clear it.
	sc.mu.Lock()
	inFlight := sc.reviewingInFlight
	sc.mu.Unlock()
	if !inFlight {
		t.Error("reviewingInFlight = false after non-review-complete delivery, want true (must not be cleared)")
	}
}
