package sidecar

// sidecar_reviewing_state_test.go
//
// Verifies that the sidecar emits a `reviewing_state` frame to the PI
// extension on every transition of reviewingInFlight plus once immediately
// after handshake completion. This is the authoritative-from-sidecar signal
// that replaces the extension's fragile bash-substring set-trigger for
// pendingReviewCall.
//
// See modules/programs/prism/pi/extensions/prism.ts for the consumer side.

import (
	"strings"
	"testing"
	"time"
)

// TestReviewingState_EmittedOnHandshake verifies the sidecar emits a
// reviewing_state{in_flight: false} frame immediately after hello_ack on
// a fresh session (no review in flight).
func TestReviewingState_EmittedOnHandshake(t *testing.T) {
	sockPath := shortSockPath(t)
	d := openTestDB(t)
	_ = d
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	// dialAndHandshake itself now consumes and asserts the reviewing_state
	// frame; success here means the frame was emitted and parsed correctly.
	conn, ack := dialAndHandshake(t, sockPath)
	if ack["type"] != "hello_ack" {
		t.Fatalf("ack type = %v, want hello_ack", ack["type"])
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestReviewingState_EmittedOnSetTrue verifies that when the /review handler
// sets reviewingInFlight=true, the sidecar pushes a reviewing_state{in_flight:
// true} frame to the connected PI extension. The frame is what drives the
// extension's pendingReviewCall guard ON, replacing the prior bash-substring
// set-trigger that false-matched on any bash command containing "prism review"
// (the root cause).
func TestReviewingState_EmittedOnSetTrue(t *testing.T) {
	sockPath := shortSockPath(t)
	d := openTestDB(t)
	_ = d
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	defer conn.Close()

	// Flip reviewingInFlight in-memory the way the /review handler does and
	// also drive the in-test transition via writeReviewingState directly.
	// We exercise the wire emission separately from the /review HTTP path
	// because /review's full path needs DB rows, agents, and a subprocess
	// spawn — out of scope for this guard-frame test.
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()
	sc.writeReviewingState(true)

	frame := readJSON(t, conn)
	if frame["type"] != "reviewing_state" {
		t.Fatalf("frame type = %v, want reviewing_state", frame["type"])
	}
	if got, _ := frame["in_flight"].(bool); !got {
		t.Errorf("frame in_flight = %v, want true", frame["in_flight"])
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	_ = wait()
}

// TestReviewingState_EmittedOnClear verifies that when the /prompt
// review-complete handler clears reviewingInFlight=false (or
// flushPendingReplay does so after a buffered review-complete enqueue), the
// sidecar pushes a reviewing_state{in_flight: false} frame so the extension
// can release its pendingReviewCall guard even if the inbound prompt frame
// itself is lost or processed in an unexpected order.
func TestReviewingState_EmittedOnClear(t *testing.T) {
	sockPath := shortSockPath(t)
	d := openTestDB(t)
	_ = d
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	defer conn.Close()

	// Pre-state: simulate that a review is in flight.
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()
	sc.writeReviewingState(true)

	// Drain the in_flight=true frame so the next read sees the clear.
	first := readJSON(t, conn)
	if first["type"] != "reviewing_state" {
		t.Fatalf("first frame type = %v, want reviewing_state", first["type"])
	}

	// Clear the flag and emit the cleared frame as the host_api.go and
	// flushPendingReplay paths now do.
	sc.mu.Lock()
	sc.reviewingInFlight = false
	sc.mu.Unlock()
	sc.writeReviewingState(false)

	cleared := readJSON(t, conn)
	if cleared["type"] != "reviewing_state" {
		t.Fatalf("cleared frame type = %v, want reviewing_state", cleared["type"])
	}
	if got, ok := cleared["in_flight"].(bool); !ok || got {
		t.Errorf("cleared frame in_flight = %v, want false", cleared["in_flight"])
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	_ = wait()
}

// TestReviewingState_FrameShape locks the on-the-wire JSON shape so an
// accidental rename of either field is caught by tests rather than by a
// stuck-active worker in production. The shape is asserted by direct call
// into writeReviewingState plus an inspection of the bytes enqueued on the
// outbound pipe channel.
func TestReviewingState_FrameShape(t *testing.T) {
	d := openTestDB(t)
	sc := newDedupTestSidecar(t, "prism-test@reviewing-state-shape", d)

	pipeCh := make(chan []byte, 4)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	if !sc.writeReviewingState(false) {
		t.Fatalf("writeReviewingState(false) returned false")
	}
	if !sc.writeReviewingState(true) {
		t.Fatalf("writeReviewingState(true) returned false")
	}

	select {
	case frame := <-pipeCh:
		if !strings.Contains(string(frame), `"type":"reviewing_state"`) {
			t.Errorf("first frame missing type:reviewing_state — got %q", frame)
		}
		if !strings.Contains(string(frame), `"in_flight":false`) {
			t.Errorf("first frame missing in_flight:false — got %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first frame on pipe channel")
	}

	select {
	case frame := <-pipeCh:
		if !strings.Contains(string(frame), `"type":"reviewing_state"`) {
			t.Errorf("second frame missing type:reviewing_state — got %q", frame)
		}
		if !strings.Contains(string(frame), `"in_flight":true`) {
			t.Errorf("second frame missing in_flight:true — got %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for second frame on pipe channel")
	}
}
