package cmd

// Tests for the shared --wait helpers. The per-command wait paths
// are tested in merge_wait_test.go, review_wait_test.go, and
// spawn_wait_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestBackoffSchedule_GrowsAndCaps asserts that the backoff helper's mean
// duration grows roughly geometrically until it hits the cap, then stays
// flat. The jitter spec is ±50% so we check averages over many calls.
//
// The intent is to make the AC ("verifiable by reading the code") provable
// by a unit test: any change that swaps backoff for a constant interval
// will fail this test.
func TestBackoffSchedule_GrowsAndCaps(t *testing.T) {
	base := 100 * time.Millisecond
	max := 1600 * time.Millisecond

	// The FIRST call should average ~base, the saturated call should
	// average ~max, and the sequence should grow strictly in expected
	// value. This is the "exponential growth + capped" half of the AC.
	const samples = 500
	step1Total, stepLastTotal := time.Duration(0), time.Duration(0)
	for i := 0; i < samples; i++ {
		f := backoffSchedule(base, max)
		step1Total += f()
		var last time.Duration
		for j := 0; j < 10; j++ {
			last = f()
		}
		stepLastTotal += last
	}
	mean1 := step1Total / samples
	meanLast := stepLastTotal / samples
	if mean1 < base/2 || mean1 > 3*base/2 {
		t.Errorf("first-step mean = %v, expected ~%v ± 50%%", mean1, base)
	}
	if meanLast < max/2 || meanLast > 3*max/2 {
		t.Errorf("saturated mean = %v, expected ~%v ± 50%%", meanLast, max)
	}
	if meanLast <= mean1 {
		t.Errorf("expected meanLast (%v) > mean1 (%v) — backoff is not growing", meanLast, mean1)
	}
}

// TestBackoffSchedule_JitterIsBoundedAndRandom checks that jitter is in
// [0.5x, 1.5x] of the unjittered value, never outside.
func TestBackoffSchedule_JitterIsBoundedAndRandom(t *testing.T) {
	base := 100 * time.Millisecond
	max := 100 * time.Millisecond
	next := backoffSchedule(base, max)
	saw := map[int64]int{}
	for i := 0; i < 200; i++ {
		d := next()
		if d < base/2 || d > 3*base/2 {
			t.Fatalf("jitter out of range: %v not in [%v,%v]", d, base/2, 3*base/2)
		}
		saw[int64(d)]++
	}
	if len(saw) < 50 {
		// With ±50% jitter and 200 samples we expect dozens of distinct
		// values. Anything less suggests jitter was dropped.
		t.Errorf("expected jitter to produce many distinct values; only saw %d", len(saw))
	}
}

// TestPollWait_ReturnsImmediatelyOnDone exercises the idempotent-observation
// a probe that is already done returns 0 immediately, no sleep.
func TestPollWait_ReturnsImmediatelyOnDone(t *testing.T) {
	start := time.Now()
	err := pollWait(context.Background(), 1*time.Second,
		100*time.Millisecond, 1*time.Second,
		func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("expected immediate return, took %v", elapsed)
	}
}

// TestPollWait_TimeoutEmitsTimeoutCode asserts that a probe that never
// finishes returns waitExitTimeout once the timeout elapses.
func TestPollWait_TimeoutEmitsTimeoutCode(t *testing.T) {
	err := pollWait(context.Background(), 50*time.Millisecond,
		10*time.Millisecond, 20*time.Millisecond,
		func() (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var ec *exitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected *exitCodeError, got %T: %v", err, err)
	}
	if ec.code != waitExitTimeout {
		t.Errorf("expected exit code %d (timeout), got %d", waitExitTimeout, ec.code)
	}
}

// TestPollWait_CtxCancelDoesNotCancelJob — the AC requires that Ctrl-C /
// context cancellation interrupts the wait loop, but the underlying job is
// not affected. Here we verify that ctx cancellation surfaces as a
// user-interrupt exit code. The probe records that it was called only as
// many times as poll iterations elapsed before the cancel — proving the
// loop returns promptly rather than running the job to completion.
func TestPollWait_CtxCancelReturnsUserInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	probeCalls := 0
	err := pollWait(ctx, 5*time.Second,
		10*time.Millisecond, 50*time.Millisecond,
		func() (bool, error) {
			probeCalls++
			return false, nil
		})
	if err == nil {
		t.Fatal("expected user-interrupt error, got nil")
	}
	var ec *exitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected *exitCodeError, got %T: %v", err, err)
	}
	if ec.code != waitExitUserInterrupt {
		t.Errorf("expected user-interrupt code %d, got %d", waitExitUserInterrupt, ec.code)
	}
	if probeCalls > 10 {
		t.Errorf("expected ≤10 probe calls before cancel, got %d (loop did not exit promptly)", probeCalls)
	}
}

// TestExitCodeOf returns the embedded code for *exitCodeError, 0 otherwise.
func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Errorf("nil err: got %d, want 0", got)
	}
	if got := exitCodeOf(errors.New("plain")); got != 0 {
		t.Errorf("plain err: got %d, want 0", got)
	}
	if got := exitCodeOf(newExitErr(7, "x")); got != 7 {
		t.Errorf("exitCodeError: got %d, want 7", got)
	}
}

// TestFormatDurationShort renders durations without nanoseconds.
func TestFormatDurationShort(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
	}
	for _, tc := range cases {
		if got := formatDurationShort(tc.d); got != tc.want {
			t.Errorf("formatDurationShort(%v): got %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestNewExitErr_PrintsMessage asserts that a non-empty message is preserved
// on the error so callers (including main.go) can print it.
func TestNewExitErr_PrintsMessage(t *testing.T) {
	err := newExitErr(2, "oops something")
	if !strings.Contains(err.Error(), "oops something") {
		t.Errorf("expected message preserved, got %q", err.Error())
	}
}
