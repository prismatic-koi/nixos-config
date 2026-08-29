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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// jsonUnmarshal is a thin wrapper around json.Unmarshal for use in test
// helpers that pass raw []byte from the pipe channel.
func jsonUnmarshal(b []byte, v any) error {
	// The pipe frames are JSONL — strip the trailing newline before unmarshalling.
	return json.Unmarshal([]byte(strings.TrimRight(string(b), "\n")), v)
}

// containsAll reports whether s contains all of the provided substrings.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// newPiSidecarForHostAPITest creates a Sidecar configured for the pi harness
// and suitable for hostAPIHandler tests.
func newPiSidecarForHostAPITest(t *testing.T, sessionName, repo, role string, d *db.DB) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
// is in "waiting" state, consistent with `prism prompt` CLI behaviour.
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
// 200 with {"buffered":true} when the pi harness pipe is not connected.
// The delivery is buffered for replay on the next handshake (with
// replay=true on the resumed frame) rather than returning 503 Service
// Unavailable, so a transient disconnect during an escalation cannot lose
// the prompt.
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

	// Expect 200 OK and the delivery is buffered for replay.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (buffered for replay), body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Errorf("body = %q, want it to contain \"buffered\":true", rr.Body.String())
	}

	// Buffer must contain the delivery.
	sc.mu.Lock()
	n := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if n != 1 {
		t.Errorf("pendingReplayDeliveries length = %d, want 1", n)
	}
}

// TestHostAPI_Prompt_ReviewComplete_ClearsReviewingInFlight verifies that a
// /prompt delivery with source="review-complete" clears the reviewingInFlight
// flag, allowing the subsequent turn_start to transition normally to active.
// This is the primary gate for the reviewing-window race.
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

// TestHostAPI_Prompt_DeliverAs_ForwardedToFrame verifies that the deliver_as
// field from the POST /prompt request body is forwarded verbatim into the
// prompt frame enqueued on the harness pipe channel. This is the key AC:
// callers that set deliver_as="followUp" (e.g. notifyCoordinator) must have
// their intent preserved end-to-end, not overridden with a hardcoded "nextTurn".
func TestHostAPI_Prompt_DeliverAs_ForwardedToFrame(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantMode string
	}{
		{
			name:     "followUp is forwarded",
			body:     `{"session":"myrepo@piworker","prompt":"follow","deliver_as":"followUp"}`,
			wantMode: "followUp",
		},
		{
			name:     "steer is forwarded",
			body:     `{"session":"myrepo@piworker","prompt":"steer","deliver_as":"steer"}`,
			wantMode: "steer",
		},
		{
			name:     "nextTurn is forwarded",
			body:     `{"session":"myrepo@piworker","prompt":"next","deliver_as":"nextTurn"}`,
			wantMode: "nextTurn",
		},
		{
			name:     "omitted defaults to nextTurn",
			body:     `{"session":"myrepo@piworker","prompt":"default"}`,
			wantMode: "nextTurn",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			if err := d.UpsertStatus("myrepo@piworker", "myrepo", "/wt", "active", nil, nil); err != nil {
				t.Fatalf("seed DB: %v", err)
			}

			sc := newPiSidecarForHostAPITest(t, "myrepo@piworker", "myrepo", "worker", d)

			// Inject a fake pipe channel so DeliverPrompt has somewhere to write.
			fakePipeCh := make(chan []byte, 10)
			sc.mu.Lock()
			sc.harnessPipeOutCh = fakePipeCh
			sc.mu.Unlock()

			rr := doHostAPI(t, sc, http.MethodPost, "/prompt", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
			}

			var frame map[string]string
			select {
			case raw := <-fakePipeCh:
				if err := jsonUnmarshal(raw, &frame); err != nil {
					t.Fatalf("unmarshal frame: %v", err)
				}
			default:
				t.Fatal("expected a frame in pipe channel, but channel was empty")
			}

			if got := frame["deliver_as"]; got != tc.wantMode {
				t.Errorf("frame deliver_as = %q, want %q", got, tc.wantMode)
			}
		})
	}
}

// TestHostAPI_Prompt_DeliverAs_InvalidRejected verifies that the /prompt
// handler rejects an unknown deliver_as value with HTTP 400 and does not
// enqueue any frame — no deliver_as value bypasses the validation.
func TestHostAPI_Prompt_DeliverAs_InvalidRejected(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpsertStatus("myrepo@piworker", "myrepo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed DB: %v", err)
	}

	sc := newPiSidecarForHostAPITest(t, "myrepo@piworker", "myrepo", "worker", d)

	// Inject a fake pipe channel to detect spurious frame delivery.
	fakePipeCh := make(chan []byte, 10)
	sc.mu.Lock()
	sc.harnessPipeOutCh = fakePipeCh
	sc.mu.Unlock()

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"myrepo@piworker","prompt":"hello","deliver_as":"bogus"}`)

	// Expect 400 Bad Request.
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (invalid deliver_as), body=%s", rr.Code, rr.Body.String())
	}

	// Error body must mention the invalid value and the accepted set.
	body := rr.Body.String()
	if !containsAll(body, "bogus", "steer", "followUp", "nextTurn") {
		t.Errorf("400 body %q should mention invalid value 'bogus' and accepted values", body)
	}

	// No frame must have been enqueued.
	select {
	case raw := <-fakePipeCh:
		t.Errorf("unexpected frame enqueued after invalid deliver_as: %s", raw)
	default:
		// Good — nothing enqueued.
	}
}

// TestHostAPI_Prompt_NonReviewComplete_DoesNotClearReviewingInFlight verifies
// that a /prompt delivery without source="review-complete" (e.g. a coordinator
// follow-up) does NOT clear reviewingInFlight. Clearing on non-review prompts
// would prematurely end the reviewing window and reintroduce the race.
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
