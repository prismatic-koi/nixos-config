package sidecar

// prompt_dedup_test.go — integration tests for the /prompt idempotency
// contract introduced in issue #1685.
//
// The bug fixed by this work: a single `prism escalate` invocation was
// delivering the escalation prompt to the coordinator's harness four times.
// The Go-side delivery path is single-shot end-to-end, so the duplication
// arises further down. To make the bus boundary robust regardless of the
// upstream cause, /prompt is now idempotent: each delivery carries a
// `delivery_id` (UUID minted by the sender); repeats are dropped before any
// frame is enqueued and the response carries {"replayed":true} so the
// sender can observe.
//
// Tests in this file follow the issue #1608 isolation contract: each test
// redirects $XDG_STATE_HOME to a t.TempDir() (via openTestDB / the sidecar
// test scaffolding here doesn't dial any real socket, but the convention is
// applied so future test additions inherit it). Session names use the
// "prism-test@" prefix — never a slug that matches a live coordinator.

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// newDedupTestSidecar constructs a pi-harness sidecar suitable for driving
// the /prompt handler directly via doHostAPI. The session name uses the
// prism-test prefix per the issue #1608 isolation contract.
func newDedupTestSidecar(t *testing.T, sessionName string, d *db.DB) *Sidecar {
	t.Helper()
	if !strings.HasPrefix(sessionName, "prism-test@") {
		t.Fatalf("dedup tests must use prism-test@ session-name prefix, got %q", sessionName)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "coordinator",
		HarnessName: "pi",
		Harness:     pih.New("", "coordinator", ""),
	}
	sc := New(cfg)
	if err := d.UpsertStatus(sessionName, "prism-test", cfg.Worktree, "active", nil, nil); err != nil {
		t.Fatalf("seed DB active state for %q: %v", sessionName, err)
	}
	return sc
}

// drainFrames reads every available frame from ch until it would block,
// then returns the collected frames.
func drainFrames(ch <-chan []byte) [][]byte {
	var out [][]byte
	for {
		select {
		case f := <-ch:
			out = append(out, f)
		default:
			return out
		}
	}
}

// TestPromptDedup_SingleDeliveryEnqueuesExactlyOneFrame is the core
// regression test for the issue #1685 observed bug: one /prompt call →
// exactly one prompt frame on the pipe channel. Without dedup this test
// would pass even before the fix; the value of having it is to lock the
// invariant against future regressions and to anchor the duplicate-delivery
// test below.
func TestPromptDedup_SingleDeliveryEnqueuesExactlyOneFrame(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coordinator", d)

	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test@coordinator","prompt":"hello","delivery_id":"d-001"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	// Allow the writer goroutine path (synchronous in this code path) to settle.
	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("got %d frame(s), want exactly 1: %v", len(frames), framesForLog(frames))
	}
	if !strings.Contains(string(frames[0]), `"text":"hello"`) {
		t.Errorf("frame 0 = %q, missing expected text", frames[0])
	}
}

// TestPromptDedup_RepeatedDeliveryIDIsDropped is the central exactly-once
// contract: four /prompt calls with the same delivery_id (simulating the
// observed four-copies failure) must result in exactly one frame on the
// pipe channel and {"replayed":true} on responses 2–4. AC #2, #4, #8.
func TestPromptDedup_RepeatedDeliveryIDIsDropped(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-dup", d)

	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	body := `{"session":"prism-test@coord-dup","prompt":"escalation body","delivery_id":"d-escalation-001"}`

	// First delivery: must enqueue a frame and respond 200 without replayed.
	rr1 := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first POST: status = %d, body=%s", rr1.Code, rr1.Body.String())
	}
	if strings.Contains(rr1.Body.String(), `"replayed":true`) {
		t.Errorf("first POST body = %q, must NOT carry replayed=true", rr1.Body.String())
	}

	// Subsequent deliveries with the same delivery_id: dropped, response
	// carries replayed=true, NO additional frames enqueued.
	for i := 2; i <= 4; i++ {
		rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST #%d: status = %d, body=%s", i, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"replayed":true`) {
			t.Errorf("POST #%d body = %q, must carry replayed=true", i, rr.Body.String())
		}
	}

	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("got %d frame(s), want exactly 1 after 4 deliveries with same delivery_id: %v",
			len(frames), framesForLog(frames))
	}
}

// TestPromptDedup_DifferentDeliveryIDsEachEnqueue verifies that genuinely
// distinct deliveries (different delivery_ids) still each produce a frame.
// Without this we'd be over-deduping. AC #2 negative case.
func TestPromptDedup_DifferentDeliveryIDsEachEnqueue(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-distinct", d)

	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	for _, id := range []string{"d-1", "d-2", "d-3"} {
		body := `{"session":"prism-test@coord-distinct","prompt":"hello ` + id + `","delivery_id":"` + id + `"}`
		rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s: status = %d, body=%s", id, rr.Code, rr.Body.String())
		}
	}

	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 3 {
		t.Fatalf("got %d frame(s), want 3 (one per distinct delivery_id): %v",
			len(frames), framesForLog(frames))
	}
}

// TestPromptDedup_EmptyDeliveryIDDisablesDedup verifies that callers that
// omit delivery_id (legacy / pre-#1685 senders) keep the old behaviour:
// every POST produces a frame. This is the backward-compatibility contract.
func TestPromptDedup_EmptyDeliveryIDDisablesDedup(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-legacy", d)

	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	body := `{"session":"prism-test@coord-legacy","prompt":"legacy"}`
	for i := 0; i < 3; i++ {
		rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST #%d: status = %d, body=%s", i, rr.Code, rr.Body.String())
		}
	}

	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 3 {
		t.Fatalf("got %d frame(s), want 3 (no dedup when delivery_id is empty): %v",
			len(frames), framesForLog(frames))
	}
}

// TestPromptDedup_SlowAckDoesNotInduceDuplicates simulates AC #4: the
// coordinator is artificially slow to drain frames (sleeping before reading).
// Even under a slow drain, a single delivery_id must still produce exactly
// one frame, because dedup happens at the /prompt handler before enqueue —
// it is independent of how fast (or slow) the receiver consumes frames.
func TestPromptDedup_SlowAckDoesNotInduceDuplicates(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-slow", d)

	// Buffered channel that simulates a slow receiver: the test goroutine
	// will not read from it until after all the POSTs have been issued.
	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	// Drainer goroutine: sleeps 200 ms before reading, simulating a
	// coordinator that is mid-stream when the escalation arrives.
	var drained [][]byte
	var drainMu sync.Mutex
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		time.Sleep(200 * time.Millisecond)
		for {
			select {
			case f := <-pipeCh:
				drainMu.Lock()
				drained = append(drained, f)
				drainMu.Unlock()
			case <-time.After(50 * time.Millisecond):
				return
			}
		}
	}()

	// Fire 4 deliveries with the same delivery_id, back-to-back, while the
	// drainer is still sleeping. This is the slow-ack repro for issue #1685.
	body := `{"session":"prism-test@coord-slow","prompt":"escalation","delivery_id":"slow-001"}`
	for i := 0; i < 4; i++ {
		rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST #%d: status = %d, body=%s", i, rr.Code, rr.Body.String())
		}
	}

	<-drainerDone

	drainMu.Lock()
	defer drainMu.Unlock()
	if len(drained) != 1 {
		t.Fatalf("slow-ack drain got %d frame(s), want 1 (dedup must hold even under slow drain): %v",
			len(drained), framesForLog(drained))
	}
}

// TestPromptDedup_BufferedReplayOnReconnect verifies AC #5/#7: when /prompt
// arrives while the PI pipe is disconnected, the delivery is buffered and
// replayed on the next handshake with `replay=true` set on the prompt frame.
// The combined contract is exactly-once with a replay marker.
func TestPromptDedup_BufferedReplayOnReconnect(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-replay", d)

	// PI is disconnected: harnessPipeOutCh is nil.
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt",
		`{"session":"prism-test@coord-replay","prompt":"escalation body","delivery_id":"r-001"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("disconnected POST: status = %d, want 200 (buffered), body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Errorf("disconnected POST body = %q, want buffered=true", rr.Body.String())
	}

	// Verify buffer holds the delivery.
	sc.mu.Lock()
	n := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if n != 1 {
		t.Fatalf("pendingReplayDeliveries length = %d, want 1", n)
	}

	// Simulate handshake: install the pipe channel, call flushPendingReplay
	// directly (mirroring the runStartupSocketPipe post-handshake path).
	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	sc.flushPendingReplay()

	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("post-flush drain got %d frame(s), want exactly 1 (AC #6 — at most one replay marker): %v",
			len(frames), framesForLog(frames))
	}
	if !strings.Contains(string(frames[0]), `"replay":true`) {
		t.Errorf("replayed frame = %q, missing replay=true marker", frames[0])
	}
	if !strings.Contains(string(frames[0]), `"text":"escalation body"`) {
		t.Errorf("replayed frame = %q, missing original text", frames[0])
	}

	// Buffer must now be empty.
	sc.mu.Lock()
	nAfter := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if nAfter != 0 {
		t.Errorf("pendingReplayDeliveries after flush = %d, want 0", nAfter)
	}
}

// TestPromptDedup_PartitionDoesNotProduceDuplicateReplayMarkers covers AC #6:
// after a partition heals, the coordinator must see AT MOST ONE replay
// marker. Concretely: a sender retrying with the same delivery_id while the
// PI is disconnected must not cause two buffered entries to flush.
func TestPromptDedup_PartitionDoesNotProduceDuplicateReplayMarkers(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-partition", d)

	// PI is disconnected. Sender hits /prompt 4× with the same delivery_id
	// (simulating an upstream retry layer hammering during a partition).
	body := `{"session":"prism-test@coord-partition","prompt":"escalation","delivery_id":"p-001"}`
	for i := 0; i < 4; i++ {
		rr := doHostAPI(t, sc, http.MethodPost, "/prompt", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST #%d: status = %d, body=%s", i, rr.Code, rr.Body.String())
		}
	}

	// Buffer must contain EXACTLY one entry: the dedup set caught the
	// repeats before they were buffered.
	sc.mu.Lock()
	n := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if n != 1 {
		t.Fatalf("pendingReplayDeliveries length = %d, want 1 (dedup must catch partition-window repeats)", n)
	}

	// Partition heals: install pipe and flush.
	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	sc.flushPendingReplay()

	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("post-heal drain got %d frame(s), want exactly 1 (AC #6): %v",
			len(frames), framesForLog(frames))
	}
	replayCount := 0
	for _, f := range frames {
		if strings.Contains(string(f), `"replay":true`) {
			replayCount++
		}
	}
	if replayCount > 1 {
		t.Errorf("got %d frame(s) with replay=true, AC #6 requires at most 1", replayCount)
	}
}

// TestPromptDedup_ReplayedFrameStillDedups verifies that flushPendingReplay
// uses a per-pass dedup set to prevent forwarding the same delivery_id twice
// in a single flush. If the buffer somehow contains two entries with the same
// delivery_id (a defensive invariant — the /prompt handler's dedup should
// prevent this, but if a bug allows it the flush must still produce exactly
// one frame). Issue #1885 F6.
//
// Implementation note: flushPendingReplay uses a local pass-level set (not
// the global promptDedup) so that legitimate buffered entries are not
// suppressed. The /prompt handler's markSeen call marks the ID when it
// accepts and buffers a delivery; using markSeen again in flush would
// therefore always drop the legitimate entry. Instead, flush tracks which
// IDs it has already forwarded in the current pass via a local map.
func TestPromptDedup_ReplayedFrameStillDedups(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-doublebuf", d)

	// Directly seed the buffer with two copies of the same delivery_id —
	// this can only happen via a bug elsewhere, but flush should still
	// produce exactly one frame (the per-pass dedup set deduplicates within
	// the flush iteration).
	sc.mu.Lock()
	sc.pendingReplayDeliveries = []pendingReplayDelivery{
		{DeliveryID: "dd-001", Text: "x", DeliverAs: "followUp"},
		{DeliveryID: "dd-001", Text: "x", DeliverAs: "followUp"},
	}
	sc.mu.Unlock()

	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	// flushPendingReplay tracks each delivered delivery_id in a local pass
	// set. The first entry is forwarded; the second is a duplicate within
	// the same pass and is dropped — exactly one frame forwarded to PI.
	sc.flushPendingReplay()
	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("flushPendingReplay: expected exactly 1 frame for double-buffered same delivery_id (per-pass dedup), got %d: %v",
			len(frames), framesForLog(frames))
	}
}

// TestFlushPendingReplay_DuplicateDeliveryIDDropsSecond buffers two pending
// replay entries with the same delivery_id (the F6/#1885 scenario: both
// monitor and recovery watcher buffered an entry during a PI disconnect).
// After flushPendingReplay, only the first entry is forwarded; the second
// is dropped by the per-flush dedup check.
func TestFlushPendingReplay_DuplicateDeliveryIDDropsSecond(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@coord-f6-dedup", d)

	// Seed the buffer with two entries sharing the same delivery_id.
	// The first entry is the monitor's delivery; the second is the recovery
	// watcher's delivery. Both arrived during a PI disconnect window.
	sc.mu.Lock()
	sc.pendingReplayDeliveries = []pendingReplayDelivery{
		{DeliveryID: "f6-group-abc", Text: "monitor delivery", DeliverAs: "followUp", Source: "review-complete"},
		{DeliveryID: "f6-group-abc", Text: "recovery delivery", DeliverAs: "followUp", Source: "review-complete"},
	}
	sc.mu.Unlock()

	// Neither ID has been seen yet (the /prompt handler never ran for
	// these in this test scenario — they were injected directly into the
	// buffer). flushPendingReplay uses a local per-pass dedup map: the first
	// entry is forwarded and recorded in the pass map; the second entry has
	// the same delivery_id, which is now in the pass map, so it is dropped.
	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	sc.flushPendingReplay()
	time.Sleep(10 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("flushPendingReplay with duplicate delivery_id: want 1 frame (first forwarded, second dropped), got %d: %v",
			len(frames), framesForLog(frames))
	}
	if !strings.Contains(string(frames[0]), "monitor delivery") {
		t.Errorf("first frame = %q, want it to contain first entry's text", frames[0])
	}

	// Buffer must be empty after flush.
	sc.mu.Lock()
	nAfter := len(sc.pendingReplayDeliveries)
	sc.mu.Unlock()
	if nAfter != 0 {
		t.Errorf("pendingReplayDeliveries after flush = %d, want 0", nAfter)
	}
}

// framesForLog formats a slice of frames for test log readability.
func framesForLog(frames [][]byte) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = strings.TrimRight(string(f), "\n")
	}
	return out
}
