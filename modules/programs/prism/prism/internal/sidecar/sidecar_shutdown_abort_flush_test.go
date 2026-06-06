package sidecar

// Tests for the Shutdown abort-frame flush path (issue #1849).
//
// Background: the socket-pipe transport's clean-shutdown path enqueues a final
// {"type":"abort"} frame onto harnessPipeOutCh so the PI extension can flush
// state to disk before the connection is torn down. The original implementation
// followed the enqueue with an unconditional 100ms time.Sleep to give the writer
// goroutine "a brief moment" to flush — that added 100ms of wall latency to
// every SIGTERM regardless of whether the writer had already drained.
//
// The current implementation replaces the sleep with an ack signal: the writer
// goroutine closes harnessPipeAbortAck after the abort frame's write attempt
// completes. Shutdown selects on the ack or on time.After(ShutdownDrainTimeout)
// — defaulting to DefaultShutdownDrainTimeout (250ms) — so:
//
//   - Healthy connection: Shutdown returns within a scheduler tick of the
//     flush (well under 100ms).
//   - Unhealthy connection (writer blocked on a stalled conn / queue full):
//     Shutdown returns within ShutdownDrainTimeout.
//
// These tests assert both bounds.

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// TestShutdown_AbortFlush_HealthyPath_FastAck verifies that on a healthy
// connection (writer goroutine responsive, conn able to accept writes),
// Shutdown returns very quickly after the abort frame is acked by the writer
// — well under the previous 100ms hard sleep and well under the 250ms
// ShutdownDrainTimeout bound. Issue #1849 AC: "On a healthy connection,
// Shutdown returns within ~10ms of the abort frame being flushed".
func TestShutdown_AbortFlush_HealthyPath_FastAck(t *testing.T) {
	// In the nix build sandbox ($NIX_BUILD_TOP set), bwrap I/O overhead
	// inflates the measured latency past the 50ms budget without indicating
	// a real regression (observed: 104ms). The latency assertion's intent is
	// "well under the old 100ms hard sleep on a host", and the nix sandbox
	// is not a host. Tracked in issue #2169 § Cluster 3.
	if os.Getenv("NIX_BUILD_TOP") != "" {
		t.Skip("skipping latency-budget test in nix build sandbox: bwrap I/O overhead inflates measured latency past the 50ms budget; see issue #2169")
	}
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	// Drain frames from the conn in the background so the writer's
	// c.Write(abortFrame) returns immediately. Without a concurrent reader the
	// kernel send buffer would absorb the small frame anyway, but explicitly
	// reading guarantees we're not measuring buffer-fill latency on a slow CI
	// runner.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			if _, err := rd.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()

	// Wait until the sidecar has installed the outbound channel — Shutdown
	// only enters the abort-flush block when harnessPipeOutCh is non-nil, and
	// runStartupSocketPipe sets it after the listener binds. dialAndHandshake
	// already proves the listener is up and the handshake completed, so the
	// channel is in place. Belt-and-braces: poll briefly.
	waitForOutChNonNil(t, sc, 2*time.Second)

	start := time.Now()
	sc.Shutdown()
	elapsed := time.Since(start)

	// 50ms is a conservative cap for a single in-process channel send +
	// writer-goroutine ack close; in practice this completes in well under
	// 1ms. We deliberately do NOT assert < 10ms because CI runners under load
	// can easily blow past that without indicating a real regression. The
	// important invariant is "well under the old 100ms hard sleep".
	if elapsed > 50*time.Millisecond {
		t.Errorf("Shutdown took %s on a healthy connection; expected well under 50ms (old hard-sleep was 100ms)", elapsed)
	}
	t.Logf("healthy-path Shutdown latency: %s", elapsed)

	_ = conn.Close()
	<-readerDone
	_ = wait()
}

// TestShutdown_AbortFlush_UnhealthyPath_BoundedByDrainTimeout verifies that
// when the writer goroutine cannot flush the abort frame (because a prior
// frame is wedged on a stalled conn), Shutdown still returns within the
// documented ShutdownDrainTimeout bound. Issue #1849 AC: "On an unhealthy
// connection (writer goroutine blocked, conn stalled), Shutdown returns
// within a bounded wait time (≤ 250ms, or the documented ShutdownDrainTimeout)".
func TestShutdown_AbortFlush_UnhealthyPath_BoundedByDrainTimeout(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	// Use a small ShutdownDrainTimeout so the test runs quickly while still
	// exercising the bounded-wait branch. The default would be 250ms; 100ms
	// here keeps total runtime tight.
	sc.cfg.ShutdownDrainTimeout = 100 * time.Millisecond
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	// Note: we do NOT drain frames from conn. The writer goroutine will
	// successfully write the hello_ack (already done during handshake) but
	// once we stall the writer below, the abort frame cannot be acked.

	waitForOutChNonNil(t, sc, 2*time.Second)

	// Artificially stall the writer goroutine by filling the outbound channel
	// with a large frame that the writer will block trying to write. The
	// kernel send buffer for a Unix socket is bounded (typically 208KB on
	// Linux); we don't read from the peer side, so once the buffer fills the
	// writer's c.Write blocks indefinitely.
	//
	// Direct enqueue via enqueueHarnessPipeFrame ensures we exercise the same
	// channel the writer drains. We push a large payload to maximise the
	// chance of filling the send buffer regardless of platform tuning.
	bigPayload := make([]byte, 512*1024) // 512 KiB
	for i := range bigPayload {
		bigPayload[i] = 'x'
	}
	bigFrame := append([]byte(`{"type":"msg_assistant","text":"`), bigPayload...)
	bigFrame = append(bigFrame, []byte(`"}`+"\n")...)
	// enqueueHarnessPipeFrame uses a non-blocking send; one frame will land,
	// the writer will pick it up and block on c.Write. Subsequent enqueues
	// fill the channel buffer. We only need one to wedge the writer.
	if !sc.enqueueHarnessPipeFrame(bigFrame) {
		t.Fatal("enqueueHarnessPipeFrame returned false on first enqueue")
	}
	// Give the writer a chance to pick up the wedge frame and block on Write.
	time.Sleep(50 * time.Millisecond)

	// Now invoke Shutdown. The abort frame will be queued behind the wedged
	// frame; the writer can't process it, so harnessPipeAbortAck will never
	// be closed via the normal path. Shutdown must fall back to the timeout
	// and proceed.
	start := time.Now()
	sc.Shutdown()
	elapsed := time.Since(start)

	// Lower bound: the timeout must actually be respected (not 0).
	// Upper bound: timeout + slack for goroutine scheduling and the rest of
	// Shutdown's teardown work. 250ms slack is generous but keeps the test
	// stable on slow CI.
	if elapsed < 90*time.Millisecond {
		t.Errorf("Shutdown returned in %s — earlier than the configured 100ms ShutdownDrainTimeout; the bound is not being honoured", elapsed)
	}
	if elapsed > 100*time.Millisecond+250*time.Millisecond {
		t.Errorf("Shutdown took %s; expected ≤ %s (drain timeout 100ms + 250ms slack)", elapsed, 350*time.Millisecond)
	}
	t.Logf("unhealthy-path Shutdown latency: %s (drain timeout 100ms)", elapsed)

	_ = conn.Close()
	// Closing the conn unblocks the writer's c.Write; outCh will drain and
	// runStartupSocketPipe will return.
	_ = wait()
}

// TestShutdown_AbortFlush_DefaultTimeoutApplies verifies that when
// Config.ShutdownDrainTimeout is left at zero, Shutdown uses
// DefaultShutdownDrainTimeout (250ms) as the upper bound. We don't actually
// wait the full 250ms — we just assert that the constant is what's
// documented and that Shutdown honours it on a healthy path (returning fast,
// not waiting the default).
func TestShutdown_AbortFlush_DefaultTimeoutApplies(t *testing.T) {
	if DefaultShutdownDrainTimeout != 250*time.Millisecond {
		t.Errorf("DefaultShutdownDrainTimeout = %s, want 250ms (AC: ≤ 250ms)", DefaultShutdownDrainTimeout)
	}

	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	// Leave sc.cfg.ShutdownDrainTimeout at its zero value — the default
	// should kick in. A healthy-path Shutdown must still return quickly
	// (well under the default) because the writer acks promptly.
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			if _, err := rd.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()
	waitForOutChNonNil(t, sc, 2*time.Second)

	start := time.Now()
	sc.Shutdown()
	elapsed := time.Since(start)
	if elapsed >= DefaultShutdownDrainTimeout {
		t.Errorf("Shutdown took %s on a healthy connection with default timeout; the default should be an upper bound, not a floor", elapsed)
	}

	_ = conn.Close()
	<-readerDone
	_ = wait()
}

// TestShutdown_AbortFlush_AckArrivesAfterFlush verifies that the writer
// goroutine actually delivers the abort frame onto the wire (the ack is not a
// no-op signalled before the write).
func TestShutdown_AbortFlush_AckArrivesAfterFlush(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	// Capture every frame received on the wire while Shutdown runs.
	var (
		mu     sync.Mutex
		frames []map[string]any
	)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if jerr := json.Unmarshal(line, &m); jerr == nil {
					mu.Lock()
					frames = append(frames, m)
					mu.Unlock()
				}
			}
			if err != nil {
				return
			}
		}
	}()
	waitForOutChNonNil(t, sc, 2*time.Second)

	sc.Shutdown()

	// Closing the listener / conn from sidecar's side will cause the reader
	// goroutine to see EOF. Give it a moment to capture any pending frame.
	_ = conn.Close()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine did not finish within 2s of Shutdown")
	}

	mu.Lock()
	defer mu.Unlock()
	sawAbort := false
	for _, f := range frames {
		if f["type"] == "abort" {
			sawAbort = true
			break
		}
	}
	if !sawAbort {
		t.Errorf("abort frame was not observed on the wire; received frames: %v", frames)
	}

	_ = wait()
}

// waitForOutChNonNil polls s.harnessPipeOutCh under the lock until it becomes
// non-nil (i.e. runStartupSocketPipe has installed the writer goroutine) or
// the deadline elapses. Fails the test on timeout.
func waitForOutChNonNil(t *testing.T, s *Sidecar, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		ch := s.harnessPipeOutCh
		s.mu.Unlock()
		if ch != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("harnessPipeOutCh did not become non-nil within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

