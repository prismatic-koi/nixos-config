package sidecar

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// expirableDeadline is a context whose deadline expires when expire is
// called, not at a wall-clock time. Err reports context.DeadlineExceeded, the
// same as a real deadline.
type expirableDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newExpirableDeadline(parent context.Context) *expirableDeadline {
	return &expirableDeadline{Context: parent, done: make(chan struct{})}
}

func (c *expirableDeadline) Done() <-chan struct{} { return c.done }

func (c *expirableDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *expirableDeadline) expire() { c.once.Do(func() { close(c.done) }) }

func runReviewWithStub(t *testing.T, script string) string {
	t.Helper()
	old := reviewExecTimeout
	reviewExecTimeout = 300 * time.Millisecond
	t.Cleanup(func() { reviewExecTimeout = old })

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sc := New(Config{
		SessionName:     "test-repo@feature",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@feature",
		HarnessURL:      "http://localhost:14000",
		DB:              openTestDB(t),
		Clock:           newTestClock(),
		AgentRole:       "worker",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	})
	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// A child killed by the exec deadline after it printed the acknowledgement
// has registered the round, so the response must report success.
//
// The order of events does not depend on the scheduler. The stub opens a
// FIFO after it prints the acknowledgement, and the deadline expires only
// when that open completes. The drain grace is longer than the test, so the
// response ends when the killed child closes its output, and the handler
// must decide from the deadline result.
func TestHostAPI_Review_DeadlineAfterAckIsStarted(t *testing.T) {
	ackFIFO := filepath.Join(t.TempDir(), "ack-printed")
	if err := syscall.Mkfifo(ackFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Setenv("PRISM_TEST_REVIEW_ACK_FIFO", ackFIFO)

	oldGrace := reviewAckDrainGrace
	reviewAckDrainGrace = time.Hour
	t.Cleanup(func() { reviewAckDrainGrace = oldGrace })

	ackPrinted := make(chan struct{})
	oldExecContext := reviewExecContext
	reviewExecContext = func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx := newExpirableDeadline(parent)
		go func() {
			defer ctx.expire()
			// Open blocks until the stub opens the FIFO to write.
			f, err := os.Open(ackFIFO)
			if err != nil {
				return
			}
			_ = f.Close()
			close(ackPrinted)
		}()
		return ctx, ctx.expire
	}
	t.Cleanup(func() {
		reviewExecContext = oldExecContext
		// Release the reader goroutine if the stub never opened the FIFO.
		if f, err := os.OpenFile(ackFIFO, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})

	body := runReviewWithStub(t, "#!/bin/sh\n"+
		"echo 'Review in progress — PR #1, round 1 (group: g)'\n"+
		": > \"$PRISM_TEST_REVIEW_ACK_FIFO\" || exit 1\n"+
		"exec sleep 60\n")

	select {
	case <-ackPrinted:
	default:
		t.Fatalf("the stub did not open the FIFO, so the deadline did not expire after the acknowledgement; body = %q", body)
	}
	if !strings.Contains(body, ReviewSentinelPassed) || strings.Contains(body, ReviewSentinelFailed) {
		t.Errorf("want passed sentinel only; body = %q", body)
	}
}

// A child killed by the exec deadline before the acknowledgement started no
// round, and the response must name the deadline.
func TestHostAPI_Review_DeadlineBeforeAckNamesCause(t *testing.T) {
	body := runReviewWithStub(t, "#!/bin/sh\nexec sleep 5\n")
	if !strings.Contains(body, ReviewSentinelFailed) {
		t.Fatalf("want failed sentinel; body = %q", body)
	}
	if !strings.Contains(body, ReviewCauseMarker) || !strings.Contains(body, "deadline") {
		t.Errorf("want cause line naming the deadline; body = %q", body)
	}
}

// The response must end at the acknowledgement while the child still runs,
// and must not wait for the exec deadline.
func TestHostAPI_Review_ResponseEndsAtAckWhileChildRuns(t *testing.T) {
	old := reviewExecTimeout
	reviewExecTimeout = 20 * time.Second
	t.Cleanup(func() { reviewExecTimeout = old })
	oldGrace := reviewAckDrainGrace
	reviewAckDrainGrace = 100 * time.Millisecond
	t.Cleanup(func() { reviewAckDrainGrace = oldGrace })

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	script := "#!/bin/sh\necho 'Review in progress — PR #1, round 1 (group: g)'\necho 'Spawned: 5, Failed: 0'\nexec sleep 30\n"
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sc := New(Config{
		SessionName:     "test-repo@feature",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-repo@feature",
		HarnessURL:      "http://localhost:14000",
		DB:              openTestDB(t),
		Clock:           newTestClock(),
		AgentRole:       "worker",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	})
	start := time.Now()
	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"1"}`)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("response took %v; it must end at the ack, not at the deadline", elapsed)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Spawned: 5") || !strings.HasSuffix(strings.TrimSpace(body), ReviewSentinelPassed) {
		t.Errorf("want full ack then passed sentinel; body = %q", body)
	}
}
