package sidecar

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sse"
)

// ── test clock ──────────────────────────────────────────────────────────────

// testTimer implements Timer and allows manual firing.
type testTimer struct {
	mu      sync.Mutex
	stopped bool
	fn      func()
}

func (t *testTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (t *testTimer) Fire() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	fn := t.fn
	t.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// testClock implements Clock with deterministic time and manual timer control.
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*testTimer
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &testTimer{fn: f}
	c.timers = append(c.timers, t)
	return t
}

// LastTimer returns the most recently created timer.
func (c *testClock) LastTimer() *testTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) == 0 {
		return nil
	}
	return c.timers[len(c.timers)-1]
}

// TimerCount returns the number of timers created.
func (c *testClock) TimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// ── test helpers ────────────────────────────────────────────────────────────

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newTestSidecar(t *testing.T) (*Sidecar, *testClock) {
	t.Helper()
	clk := newTestClock()
	d := openTestDB(t)

	cfg := Config{
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/test-worktree",
		OpencodeURL: "http://localhost:14000",
		DB:          d,
		Clock:       clk,
	}
	return New(cfg), clk
}

// makeSSE creates an sse.Event with the given type and a JSON data payload
// built from nested properties. The payload structure mirrors opencode's
// SSE events: {"properties": {...}}.
func makeSSE(eventType string, properties any) sse.Event {
	data, _ := json.Marshal(map[string]any{"properties": properties})
	return sse.Event{Type: eventType, Data: string(data)}
}

// getState reads the current agent state from the DB.
func getState(t *testing.T, d *db.DB, session string) string {
	t.Helper()
	s, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		return ""
	}
	return s.State
}

// getEvents reads all events for a session.
func getEvents(t *testing.T, d *db.DB, session string) []db.Event {
	t.Helper()
	events, err := d.AllSessionEvents(session)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	return events
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestSessionStatusBusy_WritesActive(t *testing.T) {
	sc, _ := newTestSidecar(t)

	// Seed idle state first.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "idle", nil, nil)

	evt := makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", state, agent.StateActive)
	}
}

func TestSessionStatusBusy_CancelsIdleTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Fire session.idle to start the debounce timer.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}

	// Now fire session.status busy — should cancel the timer.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Manually try to fire the timer — it should have been stopped.
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after cancelled idle timer, want %q", state, agent.StateActive)
	}
}

func TestSessionStatusRetry_WritesError(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "retry"},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateError) {
		t.Errorf("state = %q, want %q", state, agent.StateError)
	}
}

func TestSessionIdle_DebounceWritesFinished(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}

	// Fire the timer manually (simulates 2s passing).
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after idle debounce, want %q", state, agent.StateFinished)
	}
}

func TestSessionIdle_ManualDenial_WritesInterrupted(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed: active → waiting → permission denied → active.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// permission.asked → waiting
	sc.HandleEvent(makeSSE("permission.asked", map[string]any{
		"permission": "bash",
	}))

	// permission.replied with reject → active (but manualDenial flag set)
	sc.HandleEvent(makeSSE("permission.replied", map[string]any{
		"reply": "reject",
	}))

	// session.idle → start debounce
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q after manual denial idle, want %q", state, agent.StateInterrupted)
	}
}

func TestSessionIdle_DoesNotOverrideInterrupted(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed as interrupted (e.g. pane-died already fired).
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "interrupted", nil, nil)

	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q (should not overwrite interrupted)", state, agent.StateInterrupted)
	}
}

func TestSessionIdle_DoesNotOverrideError(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed as error (e.g. session.error wrote error state, then session.idle fires).
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "error", nil, nil)

	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateError) {
		t.Errorf("state = %q, want %q (should not overwrite error)", state, agent.StateError)
	}
}

func TestSessionCreated_WritesActiveWithTitle(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "idle", nil, nil)

	evt := makeSSE("session.created", map[string]any{
		"info": map[string]string{
			"id":    "oc-session-123",
			"title": "Fix the widget",
		},
	})
	sc.HandleEvent(evt)

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status.State != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", status.State, agent.StateActive)
	}
	if status.Title == nil || *status.Title != "Fix the widget" {
		t.Errorf("title = %v, want %q", status.Title, "Fix the widget")
	}
	if status.OpencodeSID == nil || *status.OpencodeSID != "oc-session-123" {
		t.Errorf("opencodeSID = %v, want %q", status.OpencodeSID, "oc-session-123")
	}
}

func TestSessionUpdated_ResumeFromInterrupted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "interrupted", nil, nil)

	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-session-456",
			"title": "Resumed task",
		},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after resume, want %q", state, agent.StateActive)
	}
}

func TestSessionUpdated_ResumeFromFinished(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "finished", nil, nil)

	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-session-789",
			"title": "Resumed after finish",
		},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after resume from finished, want %q", state, agent.StateActive)
	}
}

func TestSessionUpdated_CompactionStarted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	compactingTime := 1234.0
	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id": "oc-session-100",
			"time": map[string]*float64{
				"compacting": &compactingTime,
			},
		},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateCompacting) {
		t.Errorf("state = %q, want %q", state, agent.StateCompacting)
	}

	// Verify compaction event was written.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "compaction" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected compaction event to be written")
	}
}

func TestSessionUpdated_DoesNotOverrideActiveState(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-session-200",
			"title": "Updated title",
		},
	})
	sc.HandleEvent(evt)

	// State should remain active (not change), but title should update.
	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status.State != string(agent.StateActive) {
		t.Errorf("state = %q, want %q (should not change)", status.State, agent.StateActive)
	}
	if status.Title == nil || *status.Title != "Updated title" {
		t.Errorf("title = %v, want %q", status.Title, "Updated title")
	}
}

func TestSessionError_WritesError(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.error", map[string]any{
		"error": map[string]string{"name": "APIError"},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateError) {
		t.Errorf("state = %q, want %q", state, agent.StateError)
	}
}

func TestSessionError_MessageAborted_WritesInterrupted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.error", map[string]any{
		"error": map[string]string{"name": "MessageAbortedError"},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q", state, agent.StateInterrupted)
	}
}

func TestSessionError_MessageAborted_CancelsIdleTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Start an idle timer first.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()

	// Then MessageAbortedError should cancel it.
	sc.HandleEvent(makeSSE("session.error", map[string]any{
		"error": map[string]string{"name": "MessageAbortedError"},
	}))

	// Try to fire the timer — should be stopped.
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q (timer should have been cancelled)", state, agent.StateInterrupted)
	}
}

func TestSessionCompacted_WritesFinished(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "compacting", nil, nil)

	sc.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q, want %q", state, agent.StateFinished)
	}
}

func TestSessionCompacted_DoesNotOverrideInterrupted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "interrupted", nil, nil)

	sc.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q (should not overwrite interrupted)", state, agent.StateInterrupted)
	}
}

func TestSessionDeleted_SetsEndedAt(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.deleted", map[string]any{
		"info": map[string]string{"id": "oc-session-del"},
	})
	sc.HandleEvent(evt)

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected status row to exist")
	}
	if status.EndedAt == nil {
		t.Error("expected ended_at to be set")
	}
}

func TestPermissionAsked_WritesWaiting(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("permission.asked", map[string]any{
		"permission": "bash",
		"tool":       map[string]string{"messageID": "msg-1"},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateWaiting) {
		t.Errorf("state = %q, want %q", state, agent.StateWaiting)
	}

	// Check permission_ask event was written.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "permission_ask" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected permission_ask event to be written")
	}
}

func TestPermissionReplied_Approve_WritesActive(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "waiting", nil, nil)

	evt := makeSSE("permission.replied", map[string]any{
		"reply": "approve",
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", state, agent.StateActive)
	}
}

func TestPermissionReplied_Reject_SetsManualDenial(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "waiting", nil, nil)

	// Reject permission.
	sc.HandleEvent(makeSSE("permission.replied", map[string]any{
		"reply": "reject",
	}))

	// State should be active (permission.replied always writes active).
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after reject, want %q", state, agent.StateActive)
	}

	// Verify permission_denied event was written.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "permission_denied" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected permission_denied event to be written")
	}

	// Now idle should write interrupted (not finished) due to manual denial.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	state = getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q", state, agent.StateInterrupted)
	}
}

func TestManualDenial_ClearedByBusy(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "waiting", nil, nil)

	// Reject → sets manualDenial
	sc.HandleEvent(makeSSE("permission.replied", map[string]any{
		"reply": "reject",
	}))

	// Busy → clears manualDenial
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Idle → should write finished (not interrupted) since denial was cleared.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q, want %q (manual denial should have been cleared by busy)", state, agent.StateFinished)
	}
}

func TestMessageUpdated_UserMessage(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// First, accumulate text via message.part.updated.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-user-1",
			"text":      "Hello, please fix the bug",
		},
	}))

	// Then fire message.updated for the user message.
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":    "msg-user-1",
			"role":  "user",
			"agent": "coordinator",
			"model": map[string]string{
				"providerID": "anthropic",
				"modelID":    "claude-4",
			},
		},
	}))

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "msg_user" {
			found = true
			var payload map[string]string
			if err := json.Unmarshal([]byte(e.Payload), &payload); err == nil {
				if payload["messageId"] != "msg-user-1" {
					t.Errorf("messageId = %q, want %q", payload["messageId"], "msg-user-1")
				}
				if payload["text"] != "Hello, please fix the bug" {
					t.Errorf("text = %q", payload["text"])
				}
			}
			break
		}
	}
	if !found {
		t.Error("expected msg_user event")
	}
}

func TestMessageUpdated_DedupesMultipleFires(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Accumulate text.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-dup",
			"text":      "Some text",
		},
	}))

	msg := makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":    "msg-dup",
			"role":  "user",
			"agent": "worker",
		},
	})

	// Fire twice.
	sc.HandleEvent(msg)
	sc.HandleEvent(msg)

	// Should only have one msg_user event.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	count := 0
	for _, e := range events {
		if e.Type == "msg_user" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("msg_user count = %d, want 1", count)
	}
}

func TestMessagePartUpdated_ToolCall(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	start := 1000.0
	end := 2500.0
	evt := makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "tool",
			"messageID": "msg-tool-1",
			"tool":      "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]string{"command": "ls -la"},
				"output": "file1.txt\nfile2.txt",
				"time":   map[string]*float64{"start": &start, "end": &end},
			},
		},
	})
	sc.HandleEvent(evt)

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var toolCalls, toolResults int
	for _, e := range events {
		if e.Type == "tool_call" {
			toolCalls++
			var payload map[string]any
			json.Unmarshal([]byte(e.Payload), &payload)
			if payload["tool"] != "bash" {
				t.Errorf("tool = %v, want bash", payload["tool"])
			}
		}
		if e.Type == "tool_result" {
			toolResults++
		}
	}
	if toolCalls != 1 {
		t.Errorf("tool_call count = %d, want 1", toolCalls)
	}
	if toolResults != 1 {
		t.Errorf("tool_result count = %d, want 1", toolResults)
	}
}

func TestMessagePartUpdated_Thinking(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	endTime := 5000.0
	evt := makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "reasoning",
			"messageID": "msg-think-1",
			"text":      "Let me analyze the problem...",
			"time":      map[string]*float64{"end": &endTime},
		},
	})
	sc.HandleEvent(evt)

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "thinking" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected thinking event")
	}
}

func TestSessionStatusBusy_DoesNotOverrideCompacting(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Start compaction via session.updated.
	compactingTime := 1234.0
	sc.HandleEvent(makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id": "oc-100",
			"time": map[string]*float64{
				"compacting": &compactingTime,
			},
		},
	}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateCompacting) {
		t.Fatalf("state = %q, want %q", state, agent.StateCompacting)
	}

	// Busy during compaction should not override compacting.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	state = getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateCompacting) {
		t.Errorf("state = %q after busy during compaction, want %q", state, agent.StateCompacting)
	}
}

func TestShutdown_WritesInterrupted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.Shutdown()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q after shutdown, want %q", state, agent.StateInterrupted)
	}
}

func TestShutdown_CancelsIdleTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Start idle timer.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()

	// Shutdown should cancel the timer and write interrupted.
	sc.Shutdown()

	// Timer fire should be a no-op.
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q, want %q", state, agent.StateInterrupted)
	}
}

func TestShutdown_DoesNotOverrideFinished(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "finished", nil, nil)

	// Set lastState to finished to match the actual state.
	sc.mu.Lock()
	sc.lastState = agent.StateFinished
	sc.mu.Unlock()

	sc.Shutdown()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after shutdown on finished, want %q", state, agent.StateFinished)
	}
}

func TestShutdown_DoesNotOverrideDeleted(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Simulate session.deleted.
	sc.HandleEvent(makeSSE("session.deleted", map[string]any{
		"info": map[string]string{"id": "oc-del"},
	}))

	sc.Shutdown()

	// Should still be deleted state (the DB state), not overwritten to interrupted.
	// The sidecar's lastState is "deleted" so Shutdown() skips the write.
	// Note: the DB state may differ from lastState if deleted wrote to DB.
	sc.mu.Lock()
	lastState := sc.lastState
	sc.mu.Unlock()
	if lastState != agent.StateDeleted {
		t.Errorf("lastState = %q, want %q", lastState, agent.StateDeleted)
	}
}

func TestStateChangeDeduplication(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "idle", nil, nil)

	// Fire two consecutive busy events.
	for range 3 {
		sc.HandleEvent(makeSSE("session.status", map[string]any{
			"status": map[string]string{"type": "busy"},
		}))
	}

	// Should only have one state_change event.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	count := 0
	for _, e := range events {
		if e.Type == "state_change" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("state_change count = %d, want 1 (dedup should suppress duplicates)", count)
	}
}

func TestFullLifecycle_ActiveToFinished(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// 1. Seed idle state (tmux-session-start).
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "idle", nil, nil)

	// 2. session.created → active.
	sc.HandleEvent(makeSSE("session.created", map[string]any{
		"info": map[string]any{"id": "oc-1", "title": "My task"},
	}))
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != "active" {
		t.Fatalf("after session.created: state = %q, want active", state)
	}

	// 3. session.status busy (agent working) — should stay active.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != "active" {
		t.Fatalf("after busy: state = %q, want active", state)
	}

	// 4. permission.asked → waiting.
	sc.HandleEvent(makeSSE("permission.asked", map[string]any{
		"permission": "bash",
	}))
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != "waiting" {
		t.Fatalf("after permission.asked: state = %q, want waiting", state)
	}

	// 5. permission.replied approve → active.
	sc.HandleEvent(makeSSE("permission.replied", map[string]any{
		"reply": "approve",
	}))
	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != "active" {
		t.Fatalf("after permission.replied: state = %q, want active", state)
	}

	// 6. session.idle → start debounce timer.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	// 7. Fire the debounce timer → finished.
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer to be created")
	}
	timer.Fire()

	if state := getState(t, sc.cfg.DB, sc.cfg.SessionName); state != "finished" {
		t.Fatalf("after idle debounce: state = %q, want finished", state)
	}
}

func TestMessageUpdated_AssistantMessage(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Accumulate text.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-1",
			"text":      "Here is the solution...",
		},
	}))

	// Fire message.updated with completed time.
	created := 1000.0
	completed := 3000.0
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-1",
			"role":       "assistant",
			"agent":      "worker",
			"providerID": "anthropic",
			"modelID":    "claude-4",
			"tokens": map[string]any{
				"input":  500,
				"output": 200,
				"cache":  map[string]int{"read": 100, "write": 50},
			},
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, e := range events {
		if e.Type == "msg_assistant" {
			found = true
			var payload map[string]any
			json.Unmarshal([]byte(e.Payload), &payload)
			if payload["messageId"] != "msg-asst-1" {
				t.Errorf("messageId = %v", payload["messageId"])
			}
			if payload["model"] != "anthropic/claude-4" {
				t.Errorf("model = %v", payload["model"])
			}
			if v, ok := payload["inputTokens"]; !ok || v != float64(500) {
				t.Errorf("inputTokens = %v", v)
			}
			if v, ok := payload["durationMs"]; !ok || v != float64(2000) {
				t.Errorf("durationMs = %v", v)
			}
			break
		}
	}
	if !found {
		t.Error("expected msg_assistant event")
	}
}

func TestMessageUpdated_SkipsEmptyText(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Fire message.updated without accumulating text first.
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":   "msg-empty",
			"role": "user",
		},
	}))

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, e := range events {
		if e.Type == "msg_user" {
			t.Error("should not write msg_user for empty text message")
		}
	}
}

func TestSessionUpdated_NoRow_WritesActive(t *testing.T) {
	sc, _ := newTestSidecar(t)

	// Do NOT seed any row — simulate session.updated arriving before
	// tmux-session-start has written an idle row.
	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-new",
			"title": "Brand new",
		},
	})
	sc.HandleEvent(evt)

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", state, agent.StateActive)
	}
}

func TestSessionCompacted_CancelsIdleTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Start idle timer.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected timer")
	}

	// Mark compacting.
	sc.mu.Lock()
	sc.compacting = true
	sc.mu.Unlock()
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "compacting", nil, nil)

	// session.compacted should cancel the idle timer.
	sc.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	// Timer fire should be no-op.
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q, want %q", state, agent.StateFinished)
	}
}

func TestQuestionAsked_WritesWaiting(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	sc.HandleEvent(makeSSE("question.asked", map[string]any{}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateWaiting) {
		t.Errorf("state = %q, want %q", state, agent.StateWaiting)
	}
}

func TestQuestionReplied_WritesActive(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "waiting", nil, nil)

	sc.HandleEvent(makeSSE("question.replied", map[string]any{}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", state, agent.StateActive)
	}
}

func TestQuestionRejected_WritesActive(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "waiting", nil, nil)

	sc.HandleEvent(makeSSE("question.rejected", map[string]any{}))

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q, want %q", state, agent.StateActive)
	}
}

func TestSessionDeleted_UpdatesDBState(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	evt := makeSSE("session.deleted", map[string]any{
		"info": map[string]string{"id": "oc-session-del-2"},
	})
	sc.HandleEvent(evt)

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected status row to exist")
	}
	// Verify the state column is updated to "deleted" (not just ended_at).
	if status.State != string(agent.StateDeleted) {
		t.Errorf("state = %q, want %q", status.State, agent.StateDeleted)
	}
	if status.EndedAt == nil {
		t.Error("expected ended_at to be set")
	}
}
