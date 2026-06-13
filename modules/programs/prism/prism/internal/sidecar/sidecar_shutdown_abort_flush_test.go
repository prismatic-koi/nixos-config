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
// completes. Shutdown selects on the ack or on a drain timer armed via
// s.cfg.Clock with ShutdownDrainTimeout — defaulting to
// DefaultShutdownDrainTimeout (250ms) — so:
//
//   - Healthy connection: Shutdown returns as soon as the writer acks the
//     flush, without waiting out the drain timer.
//   - Unhealthy connection (writer blocked on a stalled conn / queue full):
//     Shutdown returns when the drain timer fires.
//
// These tests assert both paths behaviourally on the fake clock (whose timers
// fire only when a test calls Fire) rather than measuring wall-clock latency:
// real-time latency budgets proved scheduler- and I/O-noise-dependent — the
// nix build sandbox pushed the healthy path past a 50ms budget with no code
// regression.

import (
	"bufio"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestShutdown_AbortFlush_HealthyPath_FastAck verifies that on a healthy
// connection (writer goroutine responsive, conn able to accept writes),
// Shutdown returns because the writer acked the abort-frame flush — NOT
// because the drain timeout expired. Issue #1849 AC: "On a healthy
// connection, Shutdown returns within ~10ms of the abort frame being
// flushed" — i.e. the old unconditional 100ms hard sleep is gone.
//
// The assertion is behavioural, on the fake clock, rather than a wall-clock
// latency budget: the sidecar's drain timer is registered on the injected
// testClock, whose timers NEVER fire on their own. Shutdown can therefore
// only return via the writer's ack — if the ack path were broken (or a hard
// sleep-until-timeout were reintroduced on the drain timer), Shutdown would
// block forever and the liveness bound below would catch it. This replaces a
// previous `elapsed < 50ms` assertion that measured scheduler/I-O noise and
// failed in the nix build sandbox (observed 104ms) with no code regression.
func TestShutdown_AbortFlush_HealthyPath_FastAck(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	// Drain frames from the conn in the background so the writer's
	// c.Write(abortFrame) returns immediately. Without a concurrent reader the
	// kernel send buffer would absorb the small frame anyway, but explicitly
	// reading guarantees the writer cannot block on a full buffer on a slow
	// CI runner.
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Shutdown()
	}()

	select {
	case <-done:
		// Healthy path proven: the fake drain timer cannot fire, so Shutdown
		// returning at all means the writer's ack released it.
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return on a healthy connection; the abort-frame ack never arrived (the fake drain timer cannot fire, so only the ack can release Shutdown)")
	}

	// The drain bound must still have been armed while waiting for the ack —
	// it is the guard against an unresponsive writer, and the healthy path
	// must not skip it.
	if tm := clk.WaitForTimerWithDuration(DefaultShutdownDrainTimeout, 2*time.Second); tm == nil {
		t.Errorf("no drain timer with the default %s bound was registered during Shutdown", DefaultShutdownDrainTimeout)
	}

	_ = conn.Close()
	<-readerDone
	_ = wait()
}

// TestShutdown_AbortFlush_UnhealthyPath_BoundedByDrainTimeout verifies that
// when the writer goroutine cannot flush the abort frame (because a prior
// frame is wedged on a stalled conn), Shutdown blocks until the drain timer
// fires and then returns. Issue #1849 AC: "On an unhealthy connection (writer
// goroutine blocked, conn stalled), Shutdown returns within a bounded wait
// time (≤ 250ms, or the documented ShutdownDrainTimeout)".
//
// The bound is asserted behaviourally on the fake clock: Shutdown must (a)
// arm a drain timer with the configured ShutdownDrainTimeout, (b) NOT return
// while the writer is wedged and the timer has not fired — the only other
// release path is the ack, which the wedged writer cannot deliver — and (c)
// return promptly once the test fires the timer manually. This replaces the
// previous real-sleep elapsed-time window (90ms–350ms), making the test
// independent of scheduler and I/O latency.
func TestShutdown_AbortFlush_UnhealthyPath_BoundedByDrainTimeout(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	// A distinctive drain timeout so the timer can be located unambiguously
	// among any other timers registered on the fake clock (idle debounce is
	// 2s, reconnect recovery 60s). The value itself never elapses — the fake
	// clock only fires when the test says so.
	const drainTimeout = 123 * time.Millisecond
	sc.cfg.ShutdownDrainTimeout = drainTimeout
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

	// Now invoke Shutdown in the background. The abort frame will be queued
	// behind the wedged frame; the writer can't process it, so
	// harnessPipeAbortAck will never be closed via the normal path. Shutdown
	// must arm the drain timer and wait for it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Shutdown()
	}()

	// (a) Shutdown reached the bounded wait: the drain timer was armed with
	// the configured timeout.
	tm := clk.WaitForTimerWithDuration(drainTimeout, 2*time.Second)
	if tm == nil {
		t.Fatal("Shutdown never armed the drain timer with the configured ShutdownDrainTimeout")
	}

	// (b) Shutdown must still be blocked: the writer is wedged (no ack
	// possible) and the fake timer has not fired. The 100ms observation
	// window is a detection bound for a violation, not a latency budget —
	// a correct implementation blocks here indefinitely.
	select {
	case <-done:
		t.Fatal("Shutdown returned before the drain timer fired; the bounded wait is not being honoured")
	case <-time.After(100 * time.Millisecond):
		// Still blocked — expected.
	}

	// (c) Fire the drain timer: Shutdown must now complete.
	tm.Fire()
	select {
	case <-done:
		// Bounded-wait branch proven: the timer (and only the timer)
		// released Shutdown.
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the drain timer fired")
	}

	_ = conn.Close()
	// Closing the conn unblocks the writer's c.Write; outCh will drain and
	// runStartupSocketPipe will return.
	_ = wait()
}

// TestShutdown_AbortFlush_DefaultTimeoutApplies verifies that when
// Config.ShutdownDrainTimeout is left at zero, Shutdown arms the drain timer
// with DefaultShutdownDrainTimeout (250ms). The constant itself is asserted
// against the documented AC value, and the timer registration is observed on
// the fake clock — no wall-clock measurement.
func TestShutdown_AbortFlush_DefaultTimeoutApplies(t *testing.T) {
	if DefaultShutdownDrainTimeout != 250*time.Millisecond {
		t.Errorf("DefaultShutdownDrainTimeout = %s, want 250ms (AC: ≤ 250ms)", DefaultShutdownDrainTimeout)
	}

	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	// Leave sc.cfg.ShutdownDrainTimeout at its zero value — the default
	// should kick in. A healthy-path Shutdown still returns via the writer's
	// ack (the fake drain timer never fires on its own).
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.Shutdown()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return on a healthy connection with the default drain timeout")
	}

	// The drain timer must have been armed with the default bound — not 0,
	// which would make the timeout branch a no-op fall-through.
	if tm := clk.WaitForTimerWithDuration(DefaultShutdownDrainTimeout, 2*time.Second); tm == nil {
		t.Errorf("no drain timer with the default %s bound was registered during Shutdown", DefaultShutdownDrainTimeout)
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
