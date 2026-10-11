package sidecar

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
func TestHostAPI_Review_DeadlineAfterAckIsStarted(t *testing.T) {
	body := runReviewWithStub(t, "#!/bin/sh\necho 'Review in progress — PR #1, round 1 (group: g)'\nexec sleep 5\n")
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
