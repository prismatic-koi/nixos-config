package sidecar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	)

// TestNotifyCoordinator_EscalatedSuppressed verifies AC #3 and #4: while a
// session is in the escalated state, the sidecar must NOT call
// notifyCoordinator with a "has finished" message. The session.escalated bus
// event has already informed the coordinator and a finished notification
// would be a duplicate, false signal.
func TestNotifyCoordinator_EscalatedSuppressed(t *testing.T) {
	d := openTestDB(t)

	coordSID := "coord-sid-escalated-suppressed"

	var notifyCount int
	var notifyMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session" {
			sessions := []map[string]any{{"id": coordSID}}
			data, _ := json.Marshal(sessions)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		notifyMu.Lock()
		notifyCount++
		notifyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvPort := parseSrvPort(t, srv.URL)
	seedCoordinatorWithPort(t, d, "test-repo", srvPort, coordSID)

	clk := newTestClock()
	workerSession := "test-repo@feature"
	cfg := Config{
		SessionName: workerSession,
		Repo:        "test-repo",
		Worktree:    "/tmp/test-worktree-escalated",
		HarnessURL: "http://localhost:14002",
		DB:          d,
		Clock:       clk,
		HTTPClient:  srv.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	worker := New(cfg)

	// Seed worker as escalated — this is the state `prism escalate` left it
	// in just before the harness started winding down.
	_ = d.UpsertStatus(workerSession, "test-repo", "/tmp/test-worktree-escalated",
		string(agent.StateEscalated), nil, nil)

	// Directly invoke notifyCoordinator (the call site we want to verify
	// suppresses). We do not need to drive the full debounce machinery; the
	// suppression guard inside notifyCoordinator must short-circuit before
	// any HTTP call is attempted.
	worker.notifyCoordinator()

	// Allow any in-flight goroutine work to settle, though notifyCoordinator
	// is synchronous from the goroutine that calls it.
	time.Sleep(50 * time.Millisecond)

	// No bus messages must have been written.
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "test-repo@main").Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("escalated session must NOT send coordinator notification, but got %d bus message(s)", totalMsgs)
	}

	notifyMu.Lock()
	count := notifyCount
	notifyMu.Unlock()
	if count != 0 {
		t.Errorf("notifyCoordinator HTTP calls = %d for escalated session, want 0", count)
	}
}
