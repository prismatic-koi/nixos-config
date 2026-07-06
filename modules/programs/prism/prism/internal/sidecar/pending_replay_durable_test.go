package sidecar

// pending_replay_durable_test.go — tests for the durable pending-replay
// buffer introduced in issue #2359 Gap B.
//
// Before this change the pending-replay buffer was in-memory only: if the
// sidecar exited between accepting a /prompt delivery (200 {"buffered":
// true}) and the next successful pipe handshake, the delivery was
// destroyed. The coordinator saw a 0.0-second success from `prism prompt`
// and had no signal that the directive had vanished.
//
// These tests exercise the full durable round trip:
//
//   - Buffered deliveries land in pending_replay_deliveries (persisted).
//   - Reconstructing a Sidecar against the same DB restores the buffer.
//   - flushPendingReplay drains the restored buffer and deletes the DB rows.
//   - A second reconstruct sees an empty buffer (no duplicate replay).
//
// Isolation: openTestDB gives us a per-test DB tempdir; session names use
// the prism-test@ prefix per #1608.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPendingReplayDurable_SurvivesSidecarRestart is the AC for
// #2359 Gap B: a buffered /prompt delivery MUST survive sidecar exit and
// flush on the next successful handshake after restart.
//
// The test simulates a sidecar restart by discarding the first Sidecar
// (never calling flushPendingReplay on it) and constructing a fresh Sidecar
// against the same DB. The freshly-constructed sidecar loads the persisted
// buffer, and flushPendingReplay drains it in FIFO order with replay=true
// set on each frame.
func TestPendingReplayDurable_SurvivesSidecarRestart(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@durable-restart"

	// Sidecar #1: PI disconnected — POST /prompt buffers durably.
	sc1 := newDedupTestSidecar(t, session, d)
	rr := doHostAPI(t, sc1, http.MethodPost, "/prompt",
		`{"session":"prism-test@durable-restart","prompt":"first","delivery_id":"dur-001"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first buffered POST: got status %d, want 200: body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"buffered":true`) {
		t.Fatalf("first buffered POST body missing buffered=true: %q", rr.Body.String())
	}

	// Confirm the DB has the row.
	n, err := d.CountPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending_replay_deliveries after buffer: got %d rows, want 1", n)
	}

	// Discard sc1 without ever flushing — simulates a sidecar exit before
	// the reconnect completes. The DB row must still be present.
	sc1 = nil
	_ = sc1

	// Sidecar #2: fresh construction on the same DB — restore then flush.
	sc2 := newDedupTestSidecar(t, session, d)
	sc2.restorePendingReplayFromDB()

	sc2.mu.Lock()
	restored := len(sc2.pendingReplayDeliveries)
	sc2.mu.Unlock()
	if restored != 1 {
		t.Fatalf("post-restore in-memory buffer: got %d, want 1", restored)
	}

	// Attach an outbound pipe channel — this is what runStartupSocketPipe
	// does after a successful handshake — then drive the flush.
	pipeCh := make(chan []byte, 16)
	sc2.mu.Lock()
	sc2.harnessPipeOutCh = pipeCh
	sc2.mu.Unlock()

	sc2.flushPendingReplay()
	// Give the outbound writer time to enqueue.
	time.Sleep(20 * time.Millisecond)

	frames := drainFrames(pipeCh)
	if len(frames) != 1 {
		t.Fatalf("post-restart flush enqueued %d frame(s), want exactly 1 (AC #6): %v",
			len(frames), framesForLog(frames))
	}
	if !strings.Contains(string(frames[0]), `"replay":true`) {
		t.Errorf("post-restart replay frame missing replay=true marker: %q", frames[0])
	}
	if !strings.Contains(string(frames[0]), `"text":"first"`) {
		t.Errorf("post-restart replay frame missing original text: %q", frames[0])
	}

	// The DB row must be gone after successful flush.
	nAfter, err := d.CountPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries after flush: %v", err)
	}
	if nAfter != 0 {
		t.Errorf("pending_replay_deliveries after successful flush: got %d rows, want 0 (durable dedup lost)", nAfter)
	}

	// Sidecar #3: a third construction must see an empty buffer — the
	// coordinator's directive was delivered exactly once, not twice.
	sc3 := newDedupTestSidecar(t, session, d)
	sc3.restorePendingReplayFromDB()
	sc3.mu.Lock()
	stillRestored := len(sc3.pendingReplayDeliveries)
	sc3.mu.Unlock()
	if stillRestored != 0 {
		t.Errorf("post-flush restore on 3rd sidecar restored %d entries, want 0 (exactly-once contract broken)", stillRestored)
	}
}

// TestPendingReplayDurable_DedupAcrossRestart verifies the AC:
// "a buffered-then-replayed prompt is delivered exactly once — the existing
// delivery_id dedup is preserved across the durable buffer".
//
// Concretely: buffer a delivery, restart, restore, then have the same
// delivery_id re-buffered before the flush. The DB dedup (ON CONFLICT DO
// NOTHING keyed by (session_name, delivery_id)) must ensure only one row
// remains, and the flush must enqueue only one frame.
func TestPendingReplayDurable_DedupAcrossRestart(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@durable-dedup"

	sc1 := newDedupTestSidecar(t, session, d)
	// First buffer with delivery_id X.
	rr1 := doHostAPI(t, sc1, http.MethodPost, "/prompt",
		`{"session":"prism-test@durable-dedup","prompt":"x-body","delivery_id":"same-id"}`)
	if rr1.Code != http.StatusOK {
		t.Fatalf("buffer 1: got status %d, want 200", rr1.Code)
	}

	// Discard sc1 — simulate restart.
	sc1 = nil
	_ = sc1

	sc2 := newDedupTestSidecar(t, session, d)
	sc2.restorePendingReplayFromDB()

	// The sidecar's promptDedup ledger is fresh after restart (it's
	// in-memory only). To simulate a retry that survives past the LRU
	// window, buffer directly through bufferPendingReplay — this is the
	// path a same-ID repeat would take if the ledger had aged out. The DB
	// ON CONFLICT DO NOTHING must keep the DB at one row.
	sc2.bufferPendingReplay(pendingReplayDelivery{
		DeliveryID: "same-id",
		Text:       "x-body-repeat",
		DeliverAs:  "steer",
	})

	n, err := d.CountPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries: %v", err)
	}
	if n != 1 {
		t.Errorf("durable dedup on repeat delivery_id: got %d DB rows, want 1", n)
	}

	// Now flush. Only one frame should land on the pipe.
	pipeCh := make(chan []byte, 16)
	sc2.mu.Lock()
	sc2.harnessPipeOutCh = pipeCh
	sc2.mu.Unlock()
	sc2.flushPendingReplay()
	time.Sleep(20 * time.Millisecond)
	frames := drainFrames(pipeCh)
	// The buffer at flush time contains 2 in-memory entries (restored + repeat),
	// but the local per-pass flushDedup drops the duplicate. One frame is enqueued.
	if len(frames) != 1 {
		t.Fatalf("post-flush frame count: got %d, want 1 (exactly-once contract): %v",
			len(frames), framesForLog(frames))
	}

	// DB rows must be empty.
	nAfter, err := d.CountPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries after flush: %v", err)
	}
	if nAfter != 0 {
		t.Errorf("after flush, DB should be empty; got %d rows", nAfter)
	}
}

// TestPendingReplayDurable_LoadPreservesFIFO verifies the restore path
// preserves the queued_at ordering so a mixed-source buffer replays in the
// order the coordinator sent it. This mirrors the in-memory FIFO semantics
// of pendingReplayCapacity and is critical for review-complete deliveries
// which must land after any preceding escalate replies.
func TestPendingReplayDurable_LoadPreservesFIFO(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@durable-fifo"

	sc1 := newDedupTestSidecar(t, session, d)
	for i, body := range []string{"first", "second", "third"} {
		payload := `{"session":"prism-test@durable-fifo","prompt":"` + body +
			`","delivery_id":"fifo-` + strconv.Itoa(i) + `"}`
		rr := doHostAPI(t, sc1, http.MethodPost, "/prompt", payload)
		if rr.Code != http.StatusOK {
			t.Fatalf("buffer %d: got status %d, want 200: %s", i, rr.Code, rr.Body.String())
		}
		// UnixMilli-precision timestamps: sleep 2ms so successive rows are
		// distinguishable in queued_at ordering.
		time.Sleep(2 * time.Millisecond)
	}

	// Restart and restore.
	sc1 = nil
	_ = sc1
	sc2 := newDedupTestSidecar(t, session, d)
	sc2.restorePendingReplayFromDB()

	sc2.mu.Lock()
	restored := make([]pendingReplayDelivery, len(sc2.pendingReplayDeliveries))
	copy(restored, sc2.pendingReplayDeliveries)
	sc2.mu.Unlock()

	if len(restored) != 3 {
		t.Fatalf("restored: got %d, want 3", len(restored))
	}
	wantOrder := []string{"first", "second", "third"}
	for i, r := range restored {
		if r.Text != wantOrder[i] {
			t.Errorf("restored[%d].Text = %q, want %q (FIFO ordering broken)", i, r.Text, wantOrder[i])
		}
	}
}
