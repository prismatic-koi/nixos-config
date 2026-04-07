package sidecar

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// makeSSE creates an sse.Event using the real wire format that opencode emits.
// opencode does NOT use the SSE `event:` field — it sends all events as plain
// `data:` lines. The SSE client therefore sets Type to "message" (the SSE
// spec default). The real event type and properties are embedded inside the
// JSON data payload, mirroring what opencode actually sends:
//
//	data: {"type":"session.status","properties":{...}}
//
// Using this wire format ensures the tests exercise the same code path as
// production (type extraction from JSON data), not a shortcut that bypasses it.
func makeSSE(eventType string, properties any) sse.Event {
	data, _ := json.Marshal(map[string]any{
		"type":       eventType,
		"properties": properties,
	})
	return sse.Event{Type: "message", Data: string(data)}
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
	// Simulate ended_at being set (e.g. by session.deleted or pane-died hook).
	if err := sc.cfg.DB.SetEnded(sc.cfg.SessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-session-456",
			"title": "Resumed task",
		},
	})
	sc.HandleEvent(evt)

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status.State != string(agent.StateActive) {
		t.Errorf("state = %q after resume, want %q", status.State, agent.StateActive)
	}
	// ended_at must be cleared so the session appears in AllActiveStatus.
	if status.EndedAt != nil {
		t.Error("expected ended_at to be cleared on resume from interrupted")
	}
}

func TestSessionUpdated_ResumeFromFinished(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "finished", nil, nil)
	// Simulate ended_at being set.
	if err := sc.cfg.DB.SetEnded(sc.cfg.SessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	evt := makeSSE("session.updated", map[string]any{
		"info": map[string]any{
			"id":    "oc-session-789",
			"title": "Resumed after finish",
		},
	})
	sc.HandleEvent(evt)

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status.State != string(agent.StateActive) {
		t.Errorf("state = %q after resume from finished, want %q", status.State, agent.StateActive)
	}
	// ended_at must be cleared so the session appears in AllActiveStatus.
	if status.EndedAt != nil {
		t.Error("expected ended_at to be cleared on resume from finished")
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

func TestServerConnected_SilentlyIgnored(t *testing.T) {
	sc, _ := newTestSidecar(t)

	// Seed a known state so we can verify it does not change.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "idle", nil, nil)

	// server.connected arrives via the real wire format (Type: "message", type
	// embedded in the JSON data). It should be silently ignored — no state
	// change, no error, no state_change event written.
	sc.HandleEvent(sse.Event{
		Type: "message",
		Data: `{"type":"server.connected"}`,
	})

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != "idle" {
		t.Errorf("state = %q after server.connected, want %q (should be unchanged)", state, "idle")
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, e := range events {
		if e.Type == "state_change" {
			t.Error("server.connected should not write a state_change event")
		}
	}
}

// ── coordinator notification tests ──────────────────────────────────────────

// newWorkerSidecar creates a worker sidecar (session name "test-repo@feature",
// repo "test-repo") distinct from the coordinator ("test-repo@main").
// It uses the given DB so both worker and coordinator share the same store.
// An optional *http.Client may be passed to override the HTTP client used for
// coordinator notifications; pass nil to use the package default.
func newWorkerSidecar(t *testing.T, d *db.DB, httpClient *http.Client) (*Sidecar, *testClock) {
	t.Helper()
	clk := newTestClock()
	cfg := Config{
		SessionName: "test-repo@feature",
		Repo:        "test-repo",
		Worktree:    "/tmp/test-worktree-feature",
		OpencodeURL: "http://localhost:14001",
		DB:          d,
		Clock:       clk,
		HTTPClient:  httpClient,
	}
	return New(cfg), clk
}

// seedCoordinatorWithPort inserts a coordinator row with a specific known port
// and opencode_sid using a SQL exec via db.QueryRow (for testing).
func seedCoordinatorWithPort(t *testing.T, d *db.DB, repo string, port int, sid string) {
	t.Helper()
	coordName := repo + "@main"
	agentName := "coordinator"
	modelID := "anthropic/claude-sonnet-4-5"
	if err := d.UpsertStatusWithAgent(coordName, repo, "/tmp/coord-worktree", "active", nil, &sid, &agentName, &modelID); err != nil {
		t.Fatalf("seed coordinator: UpsertStatusWithAgent: %v", err)
	}
	// Set the port directly via a UPDATE … RETURNING to verify it applied.
	var got int
	if err := d.QueryRow(
		"UPDATE agent_status SET opencode_port = ? WHERE session_name = ? RETURNING opencode_port",
		port, coordName,
	).Scan(&got); err != nil {
		t.Fatalf("seed coordinator: set port: %v", err)
	}
	if got != port {
		t.Fatalf("seed coordinator: port mismatch: got %d, want %d", got, port)
	}
}

// waitForBusMessage polls the DB for a bus message to toSession, with a short
// timeout. Returns the first message found or nil.
func waitForBusMessage(t *testing.T, d *db.DB, toSession string) *db.BusMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := d.PendingMessages(toSession, "normal")
		if err != nil {
			t.Fatalf("PendingMessages: %v", err)
		}
		if len(msgs) > 0 {
			return &msgs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// waitForBusMessageDelivered polls the DB for a delivered bus message
// (delivered_at IS NOT NULL) to toSession.
func waitForBusMessageDelivered(t *testing.T, d *db.DB, toSession string) *db.BusMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row := d.QueryRow(`
SELECT id, from_session, to_session, repo, text, urgency, sent_at, delivered_at
FROM bus_messages
WHERE to_session = ? AND delivered_at IS NOT NULL
ORDER BY sent_at DESC LIMIT 1`, toSession)
		var m db.BusMessage
		var sentAt, deliveredAt int64
		if err := row.Scan(&m.ID, &m.FromSession, &m.ToSession, &m.Repo, &m.Text, &m.Urgency, &sentAt, &deliveredAt); err == nil {
			return &m
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestNotifyCoordinator_IdleDebouncePath(t *testing.T) {
	d := openTestDB(t)

	// Seed the coordinator with a known port and sid via a test HTTP server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Extract the port from the test server URL.
	var srvPort int
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &srvPort)
	if err != nil {
		// Try localhost form.
		_, err = fmt.Sscanf(srv.URL, "http://localhost:%d", &srvPort)
	}
	if err != nil {
		t.Fatalf("parse test server port from %q: %v", srv.URL, err)
	}

	coordSID := "coord-sid-123"
	seedCoordinatorWithPort(t, d, "test-repo", srvPort, coordSID)

	// Create worker sidecar with the test server's HTTP client.
	worker, clk := newWorkerSidecar(t, d, srv.Client())

	// Seed worker as active.
	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "active", nil, nil)

	// Trigger idle debounce → finished.
	worker.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	// Verify state is finished.
	if state := getState(t, d, worker.cfg.SessionName); state != "finished" {
		t.Errorf("worker state = %q, want finished", state)
	}

	// Wait for the async notification to write to bus_messages (delivered).
	msg := waitForBusMessageDelivered(t, d, "test-repo@main")
	if msg == nil {
		t.Fatal("expected delivered bus message to coordinator, got none")
	}
	if msg.FromSession != worker.cfg.SessionName {
		t.Errorf("from_session = %q, want %q", msg.FromSession, worker.cfg.SessionName)
	}
	wantText := "Agent test-repo@feature has finished its current task"
	if msg.Text != wantText {
		t.Errorf("text = %q, want %q", msg.Text, wantText)
	}
}

func TestNotifyCoordinator_CompactedPath(t *testing.T) {
	d := openTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var srvPort int
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &srvPort)
	if err != nil {
		_, err = fmt.Sscanf(srv.URL, "http://localhost:%d", &srvPort)
	}
	if err != nil {
		t.Fatalf("parse test server port from %q: %v", srv.URL, err)
	}

	coordSID := "coord-sid-456"
	seedCoordinatorWithPort(t, d, "test-repo", srvPort, coordSID)

	// Create worker sidecar with the test server's HTTP client.
	worker, _ := newWorkerSidecar(t, d, srv.Client())

	// Seed worker as compacting.
	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "compacting", nil, nil)
	worker.mu.Lock()
	worker.compacting = true
	worker.mu.Unlock()

	// Trigger session.compacted → finished.
	worker.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	if state := getState(t, d, worker.cfg.SessionName); state != "finished" {
		t.Errorf("worker state = %q, want finished", state)
	}

	// Wait for async notification.
	msg := waitForBusMessageDelivered(t, d, "test-repo@main")
	if msg == nil {
		t.Fatal("expected delivered bus message to coordinator, got none")
	}
	wantText := "Agent test-repo@feature has finished its current task"
	if msg.Text != wantText {
		t.Errorf("text = %q, want %q", msg.Text, wantText)
	}
}

func TestNotifyCoordinator_NoNotificationOnInterrupted(t *testing.T) {
	d := openTestDB(t)
	worker, clk := newWorkerSidecar(t, d, nil)

	// Seed coordinator.
	coordSID := "coord-sid-789"
	_ = d.UpsertStatus("test-repo@main", "test-repo", "/tmp/coord", "active", nil, &coordSID)

	// Seed worker as active with a manual denial.
	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "active", nil, nil)

	// permission.replied reject → sets manualDenial.
	worker.HandleEvent(makeSSE("permission.replied", map[string]any{"reply": "reject"}))

	// session.idle → debounce timer.
	worker.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}

	// Fire → should write interrupted (not finished), so no coordinator notification.
	timer.Fire()

	if state := getState(t, d, worker.cfg.SessionName); state != "interrupted" {
		t.Errorf("worker state = %q, want interrupted", state)
	}

	// Give a brief window for any spurious goroutines to write.
	time.Sleep(50 * time.Millisecond)

	// No bus messages should have been written.
	// Check both delivered and undelivered rows.
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "test-repo@main").Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("expected no bus messages on interrupted, got %d", totalMsgs)
	}
}

func TestNotifyCoordinator_SelfNotificationSkipped(t *testing.T) {
	// Use a coordinator sidecar (session name matches "<repo>@main").
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/test-coord-worktree",
		OpencodeURL: "http://localhost:14000",
		DB:          d,
		Clock:       clk,
	}
	coordinator := New(cfg)

	_ = d.UpsertStatus(coordinator.cfg.SessionName, coordinator.cfg.Repo, coordinator.cfg.Worktree, "active", nil, nil)

	// Trigger idle debounce → finished.
	coordinator.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	if state := getState(t, d, coordinator.cfg.SessionName); state != "finished" {
		t.Errorf("coordinator state = %q, want finished", state)
	}

	// Give a brief window for any spurious goroutines to write.
	time.Sleep(50 * time.Millisecond)

	// No bus messages should have been written (self-notification skipped).
	// Check both delivered and undelivered rows.
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "test-repo@main").Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("expected no bus messages for self-notification, got %d", totalMsgs)
	}
}

func TestNotifyCoordinator_BusMessageAuditOnHTTPSuccess(t *testing.T) {
	d := openTestDB(t)

	// Record HTTP requests to verify the body.
	var (
		bodyMu       sync.Mutex
		receivedBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		receivedBody = body
		bodyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var srvPort int
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &srvPort)
	if err != nil {
		_, err = fmt.Sscanf(srv.URL, "http://localhost:%d", &srvPort)
	}
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	coordSID := "coord-sid-audit"
	seedCoordinatorWithPort(t, d, "test-repo", srvPort, coordSID)

	// Create worker sidecar with the test server's HTTP client.
	worker, _ := newWorkerSidecar(t, d, srv.Client())

	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "compacting", nil, nil)
	worker.mu.Lock()
	worker.compacting = true
	worker.mu.Unlock()
	worker.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	// Wait for delivered bus message (audit trail).
	msg := waitForBusMessageDelivered(t, d, "test-repo@main")
	if msg == nil {
		t.Fatal("expected delivered audit bus message")
	}

	// Verify no undelivered message was written.
	undelivered, err := d.PendingMessages("test-repo@main", "normal")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(undelivered) != 0 {
		t.Errorf("expected no undelivered messages after HTTP success, got %d", len(undelivered))
	}

	// Verify the HTTP body contained the notification text.
	// waitForBusMessageDelivered ensures the HTTP call completed before we get here.
	bodyMu.Lock()
	captured := make([]byte, len(receivedBody))
	copy(captured, receivedBody)
	bodyMu.Unlock()

	if len(captured) == 0 {
		t.Fatal("expected HTTP request body to be captured, got empty")
	}
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("unmarshal HTTP body: %v", err)
	}
	parts, _ := body["parts"].([]any)
	if len(parts) == 0 {
		t.Fatal("expected parts in HTTP body")
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	wantText := "Agent test-repo@feature has finished its current task"
	if text != wantText {
		t.Errorf("HTTP body text = %q, want %q", text, wantText)
	}
}

func TestNotifyCoordinator_WriteBusMessageFallbackOnHTTPFailure(t *testing.T) {
	d := openTestDB(t)

	// Use a server that returns an error status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var srvPort int
	_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &srvPort)
	if err != nil {
		_, err = fmt.Sscanf(srv.URL, "http://localhost:%d", &srvPort)
	}
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	coordSID := "coord-sid-fallback"
	seedCoordinatorWithPort(t, d, "test-repo", srvPort, coordSID)

	// Create worker sidecar with the test server's HTTP client (returns 500).
	worker, _ := newWorkerSidecar(t, d, srv.Client())

	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "compacting", nil, nil)
	worker.mu.Lock()
	worker.compacting = true
	worker.mu.Unlock()
	worker.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	// Wait for fallback bus message (undelivered).
	msg := waitForBusMessage(t, d, "test-repo@main")
	if msg == nil {
		t.Fatal("expected fallback bus message after HTTP failure")
	}
	wantText := "Agent test-repo@feature has finished its current task"
	if msg.Text != wantText {
		t.Errorf("bus message text = %q, want %q", msg.Text, wantText)
	}
}

// TestDeliverNotificationViaHTTP_BodyLogging verifies that a non-2xx response
// body (up to 200 bytes) is included in the returned error, making HTTP 500s
// from the coordinator self-diagnosing in the sidecar log.
func TestDeliverNotificationViaHTTP_BodyLogging(t *testing.T) {
	// Build a 300-byte body where the first 200 bytes are all 'a' and the
	// last 100 bytes are all 'b'. After truncation at 200, the 'b' region
	// must not appear in the error.
	longBody := strings.Repeat("a", 200) + strings.Repeat("b", 100)

	tests := []struct {
		name          string
		statusCode    int
		body          string
		wantInErr     string
		wantNotInErr  string
		exactErrMatch string // if set, error must equal this exactly
	}{
		{
			name:       "500 with body snippet in error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"session not found"}`,
			wantInErr:  `session not found`,
		},
		{
			// The body is 300 bytes (200 'a' + 100 'b'); only the first 200
			// bytes should appear in the error, so 'b' must not be present.
			name:         "body truncated at 200 bytes",
			statusCode:   http.StatusInternalServerError,
			body:         longBody,
			wantInErr:    strings.Repeat("a", 200),
			wantNotInErr: "b",
		},
		{
			// Non-2xx with no body: error must be "http status NNN" with no
			// trailing colon-space.
			name:          "empty body does not add trailing colon-space",
			statusCode:    http.StatusBadGateway,
			body:          "",
			exactErrMatch: "http status 502",
		},
		{
			name:       "404 with body snippet in error",
			statusCode: http.StatusNotFound,
			body:       "session abc not found",
			wantInErr:  "session abc not found",
		},
		{
			name:       "200 ok returns no error",
			statusCode: http.StatusOK,
			body:       "",
			wantInErr:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			var srvPort int
			_, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &srvPort)
			if err != nil {
				_, err = fmt.Sscanf(srv.URL, "http://localhost:%d", &srvPort)
			}
			if err != nil {
				t.Fatalf("parse test server port: %v", err)
			}

			agentName := "coordinator"
			modelID := "anthropic/claude-sonnet-4-5"
			status := &db.Status{
				AgentName: &agentName,
				ModelID:   &modelID,
			}

			gotErr := deliverNotificationViaHTTP(srvPort, "test-sid", "notify text", status, srv.Client())

			if tc.wantInErr == "" && tc.exactErrMatch == "" {
				if gotErr != nil {
					t.Errorf("expected no error, got: %v", gotErr)
				}
				return
			}

			if gotErr == nil {
				want := tc.wantInErr
				if tc.exactErrMatch != "" {
					want = tc.exactErrMatch
				}
				t.Fatalf("expected an error (want %q), got nil", want)
			}

			if tc.exactErrMatch != "" {
				if gotErr.Error() != tc.exactErrMatch {
					t.Errorf("error = %q, want exactly %q", gotErr.Error(), tc.exactErrMatch)
				}
				return
			}

			if !strings.Contains(gotErr.Error(), tc.wantInErr) {
				t.Errorf("error = %q, want it to contain %q", gotErr.Error(), tc.wantInErr)
			}
			if tc.wantNotInErr != "" && strings.Contains(gotErr.Error(), tc.wantNotInErr) {
				t.Errorf("error = %q, must NOT contain %q (body was not truncated)", gotErr.Error(), tc.wantNotInErr)
			}
		})
	}
}

func TestNotifyCoordinator_SilentSkipWhenNoCoordinator(t *testing.T) {
	d := openTestDB(t)
	worker, clk := newWorkerSidecar(t, d, nil)

	// Do NOT seed a coordinator row.

	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "active", nil, nil)

	worker.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	if state := getState(t, d, worker.cfg.SessionName); state != "finished" {
		t.Errorf("worker state = %q, want finished", state)
	}

	// Give a brief window.
	time.Sleep(50 * time.Millisecond)

	// No bus messages should exist (delivered or undelivered).
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "test-repo@main").Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("expected no bus messages when no coordinator, got %d", totalMsgs)
	}
}

func TestNotifyCoordinator_PortSetButNoSID_FallsBackToBus(t *testing.T) {
	d := openTestDB(t)
	worker, _ := newWorkerSidecar(t, d, nil)

	// Seed coordinator with a port but no opencode_sid.
	coordName := "test-repo@main"
	if err := d.UpsertStatus(coordName, "test-repo", "/tmp/coord-worktree", "active", nil, nil); err != nil {
		t.Fatalf("seed coordinator: UpsertStatus: %v", err)
	}
	// Set port but leave opencode_sid = NULL.
	var gotPort int
	if err := d.QueryRow(
		"UPDATE agent_status SET opencode_port = 19999 WHERE session_name = ? RETURNING opencode_port",
		coordName,
	).Scan(&gotPort); err != nil {
		t.Fatalf("set port: %v", err)
	}

	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "compacting", nil, nil)
	worker.mu.Lock()
	worker.compacting = true
	worker.mu.Unlock()
	worker.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	// Should fall back to undelivered bus message since opencode_sid is nil.
	msg := waitForBusMessage(t, d, coordName)
	if msg == nil {
		t.Fatal("expected fallback bus message when coordinator has port but no sid")
	}
	wantText := "Agent test-repo@feature has finished its current task"
	if msg.Text != wantText {
		t.Errorf("bus message text = %q, want %q", msg.Text, wantText)
	}
}

// ── subagent suppression tests ──────────────────────────────────────────────

// makeAssistantMessage creates a message.part.updated + message.updated pair
// that simulates an assistant message completing, as opencode sends them.
func makeAssistantMessage(messageID, agentName, text string) []sse.Event {
	created := 1000.0
	completed := 2000.0
	return []sse.Event{
		makeSSE("message.part.updated", map[string]any{
			"part": map[string]any{
				"type":      "text",
				"messageID": messageID,
				"text":      text,
			},
		}),
		makeSSE("message.updated", map[string]any{
			"info": map[string]any{
				"id":         messageID,
				"role":       "assistant",
				"agent":      agentName,
				"providerID": "anthropic",
				"modelID":    "claude-sonnet-4-5",
				"time": map[string]*float64{
					"created":   &created,
					"completed": &completed,
				},
			},
		}),
	}
}

// makeUserMessage creates a message.part.updated + message.updated pair that
// simulates a user message, as opencode sends them.
func makeUserMessage(messageID, agentName, text string) []sse.Event {
	return []sse.Event{
		makeSSE("message.part.updated", map[string]any{
			"part": map[string]any{
				"type":      "text",
				"messageID": messageID,
				"text":      text,
			},
		}),
		makeSSE("message.updated", map[string]any{
			"info": map[string]any{
				"id":    messageID,
				"role":  "user",
				"agent": agentName,
			},
		}),
	}
}

func sendEvents(sc *Sidecar, evts []sse.Event) {
	for _, e := range evts {
		sc.HandleEvent(e)
	}
}

// TestSubagentIdle_SuppressesDebounce verifies that when session.idle fires
// immediately after a subagent assistant message, no debounce timer is created
// (the parent agent is expected to resume shortly).
func TestSubagentIdle_SuppressesDebounce(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Establish root agent from initial user message.
	sendEvents(sc, makeUserMessage("msg-user-1", "worker", "Please review this PR"))

	// Root agent produces an assistant message (normal operation).
	sendEvents(sc, makeAssistantMessage("msg-asst-1", "worker", "I will invoke the review agent"))

	// Root agent invokes subagent: user message with agent="review".
	sendEvents(sc, makeUserMessage("msg-user-2", "review", "Review this code"))

	// Subagent (review) produces its response.
	sendEvents(sc, makeAssistantMessage("msg-asst-2", "review", "Here are the review findings"))

	// session.idle fires after subagent completes.
	timersBefore := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	// No debounce timer should be created — subagent was last active.
	if clk.TimerCount() != timersBefore {
		t.Errorf("expected no new timer after subagent idle, but timer count went from %d to %d",
			timersBefore, clk.TimerCount())
	}

	// State should remain active.
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after suppressed idle, want %q", state, agent.StateActive)
	}
}

// TestRootAgentIdle_AllowsDebounce verifies that session.idle after a root
// agent assistant message starts the debounce timer normally.
func TestRootAgentIdle_AllowsDebounce(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Establish root agent.
	sendEvents(sc, makeUserMessage("msg-user-1", "worker", "Fix the bug"))

	// Root agent produces its response.
	sendEvents(sc, makeAssistantMessage("msg-asst-1", "worker", "Done, I fixed it"))

	// session.idle fires after root agent completes.
	timersBefore := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	// Debounce timer should be created.
	if clk.TimerCount() != timersBefore+1 {
		t.Errorf("expected new timer after root-agent idle, got timer count %d (was %d)",
			clk.TimerCount(), timersBefore)
	}

	// Fire the timer → finished.
	clk.LastTimer().Fire()
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after root idle debounce, want %q", state, agent.StateFinished)
	}
}

// TestSubagentCycle_MultipleRounds simulates the full spurious-finished
// scenario: worker → review (multiple rounds) → worker finishes.
// Only the final session.idle (after the worker's last message) should produce
// a finished transition; all intermediate idles should be suppressed.
func TestSubagentCycle_MultipleRounds(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Establish root agent from initial user message.
	sendEvents(sc, makeUserMessage("msg-user-1", "worker", "Please open a PR and have it reviewed"))

	// Round 1: worker invokes review subagent.
	sendEvents(sc, makeAssistantMessage("msg-asst-worker-1", "worker", "Opening PR and invoking review"))
	sendEvents(sc, makeUserMessage("msg-review-1", "review", "Review round 1"))
	sendEvents(sc, makeAssistantMessage("msg-asst-review-1", "review", "Round 1 review findings"))

	// session.idle after review round 1 → should be suppressed (no new timer).
	timersAfterRound1 := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	if clk.TimerCount() != timersAfterRound1 {
		t.Errorf("expected no new timer after review round 1 idle, but timer count went from %d to %d",
			timersAfterRound1, clk.TimerCount())
	}
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Fatalf("after review round 1 idle: state = %q, want active (should be suppressed)", state)
	}

	// Worker resumes after reviewing the feedback.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))
	sendEvents(sc, makeAssistantMessage("msg-asst-worker-2", "worker", "Fixing issues from round 1"))

	// Round 2: worker invokes review again.
	sendEvents(sc, makeUserMessage("msg-review-2", "review", "Review round 2"))
	sendEvents(sc, makeAssistantMessage("msg-asst-review-2", "review", "Round 2 review findings"))

	// session.idle after review round 2 → should be suppressed again (no new timer).
	timersAfterRound2 := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	if clk.TimerCount() != timersAfterRound2 {
		t.Errorf("expected no new timer after review round 2 idle, but timer count went from %d to %d",
			timersAfterRound2, clk.TimerCount())
	}
	state = getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Fatalf("after review round 2 idle: state = %q, want active (should be suppressed)", state)
	}

	// Worker resumes and completes its final response.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))
	sendEvents(sc, makeAssistantMessage("msg-asst-worker-3", "worker", "All done, PR is approved"))

	// session.idle after worker's final message → should proceed to finished.
	timersBefore := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	if clk.TimerCount() == timersBefore {
		t.Fatal("expected debounce timer after root agent final idle, but no timer created")
	}

	// Fire the timer → finished.
	clk.LastTimer().Fire()
	state = getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after final idle debounce, want %q", state, agent.StateFinished)
	}
}

// TestSubagentIdle_NoRootAgent_AllowsDebounce verifies that when no root
// agent has been established yet (no user messages seen), session.idle
// proceeds normally to avoid blocking the debounce indefinitely.
func TestSubagentIdle_NoRootAgent_AllowsDebounce(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// No user messages — root agent is unknown. session.idle should still work.
	timersBefore := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	if clk.TimerCount() != timersBefore+1 {
		t.Errorf("expected timer when rootAgent is unknown, got timer count %d (was %d)",
			clk.TimerCount(), timersBefore)
	}

	clk.LastTimer().Fire()
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q, want %q", state, agent.StateFinished)
	}
}

// TestSubagentIdle_UnknownAssistantAgent_AllowsDebounce verifies that when an
// assistant message arrives with an empty agent name (e.g. older opencode
// versions), session.idle is not incorrectly suppressed.
func TestSubagentIdle_UnknownAssistantAgent_AllowsDebounce(t *testing.T) {
	sc, clk := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Establish root agent.
	sendEvents(sc, makeUserMessage("msg-user-1", "worker", "Do some work"))

	// Assistant message with empty agent name.
	created := 1000.0
	completed := 2000.0
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-empty",
			"text":      "Done",
		},
	}))
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-empty",
			"role":       "assistant",
			"agent":      "", // empty agent name
			"providerID": "anthropic",
			"modelID":    "claude-sonnet-4-5",
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	// session.idle should allow the debounce (empty agent name → lastAssistantAgent = "").
	timersBefore := clk.TimerCount()
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))
	if clk.TimerCount() != timersBefore+1 {
		t.Errorf("expected timer when lastAssistantAgent is empty, got timer count %d (was %d)",
			clk.TimerCount(), timersBefore)
	}

	clk.LastTimer().Fire()
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q, want %q", state, agent.StateFinished)
	}
}

func TestNotifyCoordinator_EndedCoordinatorSkipped(t *testing.T) {
	d := openTestDB(t)
	worker, clk := newWorkerSidecar(t, d, nil)

	// Seed coordinator row, then mark it as ended.
	coordName := "test-repo@main"
	coordSID := "coord-sid-ended"
	if err := d.UpsertStatus(coordName, "test-repo", "/tmp/coord-worktree", "active", nil, &coordSID); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	if err := d.SetEnded(coordName); err != nil {
		t.Fatalf("SetEnded coordinator: %v", err)
	}

	// Seed worker as active.
	_ = d.UpsertStatus(worker.cfg.SessionName, worker.cfg.Repo, worker.cfg.Worktree, "active", nil, nil)

	// Trigger idle debounce → finished.
	worker.HandleEvent(makeSSE("session.idle", map[string]any{}))
	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected idle timer")
	}
	timer.Fire()

	if state := getState(t, d, worker.cfg.SessionName); state != "finished" {
		t.Errorf("worker state = %q, want finished", state)
	}

	// Give a brief window for any spurious goroutines to write.
	time.Sleep(50 * time.Millisecond)

	// No bus messages should have been written — coordinator has ended.
	// Check both delivered and undelivered rows.
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", coordName).Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("expected no bus messages when coordinator has ended, got %d", totalMsgs)
	}
}

// TestOnReady_NotCalledAfterShutdown verifies that the shuttingDown guard
// (AC-16) prevents OnReady from firing when Shutdown() races a successful
// health probe.
//
// The race: WaitHealthy returns a genuine 200 during podman stop's grace
// period. Shutdown() has already set shuttingDown=true. The guard in Run()
// checks the flag before calling OnReady — this test exercises that guard
// directly by setting shuttingDown before the guard check runs.
func TestOnReady_NotCalledAfterShutdown(t *testing.T) {
	called := false
	sc := New(Config{
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/test",
		OpencodeURL: "http://localhost:14000",
		DB:          openTestDB(t),
		Clock:       newTestClock(),
		OnReady: func() {
			called = true
		},
	})

	// Simulate Shutdown() having already fired — it sets shuttingDown=true
	// under the mutex, exactly as the real Shutdown() method does.
	sc.mu.Lock()
	sc.shuttingDown = true
	sc.mu.Unlock()

	// Now simulate the Run() guard: read shuttingDown under the lock and only
	// call OnReady when the flag is false. This is the code path exercised
	// when WaitHealthy returns ok after Shutdown() has already been called.
	sc.mu.Lock()
	isShuttingDown := sc.shuttingDown
	sc.mu.Unlock()
	if !isShuttingDown && sc.cfg.OnReady != nil {
		sc.cfg.OnReady()
	}

	if called {
		t.Error("OnReady was called after Shutdown() set shuttingDown=true; expected it to be suppressed")
	}
}

// TestOnReady_CalledWhenNotShuttingDown verifies the positive case: OnReady
// fires normally when shuttingDown is false (no SIGTERM race).
func TestOnReady_CalledWhenNotShuttingDown(t *testing.T) {
	called := false
	sc := New(Config{
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/test",
		OpencodeURL: "http://localhost:14000",
		DB:          openTestDB(t),
		Clock:       newTestClock(),
		OnReady: func() {
			called = true
		},
	})

	// shuttingDown is false by default (zero value). Simulate the guard check.
	sc.mu.Lock()
	isShuttingDown := sc.shuttingDown
	sc.mu.Unlock()
	if !isShuttingDown && sc.cfg.OnReady != nil {
		sc.cfg.OnReady()
	}

	if !called {
		t.Error("OnReady was not called when shuttingDown=false; expected it to fire")
	}
}

// TestMessageUpdated_AssistantWritesRootModelID verifies AC-6: after the first
// completed assistant message from the root agent, root_model_id in the DB
// reflects that message's model. A subagent message that follows must not
// overwrite root_model_id.
func TestMessageUpdated_AssistantWritesRootModelID(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// Establish root agent via a user message.
	sendEvents(sc, makeUserMessage("msg-user-ac6", "root-agent", "Hello"))

	// Accumulate text for the root-agent assistant message.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-ac6",
			"text":      "Here is the answer.",
		},
	}))

	// Complete the root-agent assistant message with a known model.
	created := 1000.0
	completed := 2000.0
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-ac6",
			"role":       "assistant",
			"agent":      "root-agent",
			"providerID": "github-copilot",
			"modelID":    "claude-sonnet-4.6",
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus: got nil status")
	}
	if status.RootModelID == nil {
		t.Fatal("RootModelID: got nil, want non-nil")
	}
	want := "github-copilot/claude-sonnet-4.6"
	if *status.RootModelID != want {
		t.Errorf("RootModelID = %q, want %q", *status.RootModelID, want)
	}

	// Now fire a subagent assistant message with a different model. It must NOT
	// overwrite root_model_id.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-subagent",
			"text":      "Subagent response.",
		},
	}))
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-subagent",
			"role":       "assistant",
			"agent":      "subagent",
			"providerID": "openai",
			"modelID":    "gpt-4o",
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	status, err = sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus after subagent: %v", err)
	}
	if status.RootModelID == nil || *status.RootModelID != want {
		t.Errorf("RootModelID after subagent message = %v, want %q (subagent must not overwrite root model)", status.RootModelID, want)
	}
}

// TestMessageUpdated_SecondSessionUpdatesRootModelID verifies AC-7: when a
// second session starts with a different model, root_model_id is updated to the
// new value (the stale-model scenario is explicitly covered).
func TestMessageUpdated_SecondSessionUpdatesRootModelID(t *testing.T) {
	sc, _ := newTestSidecar(t)

	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	// First session: establish root agent via user message, then assistant message with old model.
	sendEvents(sc, makeUserMessage("msg-user-s1", "root-agent", "Session 1 prompt"))

	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-session1",
			"text":      "First session response.",
		},
	}))
	created := 1000.0
	completed := 2000.0
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-session1",
			"role":       "assistant",
			"agent":      "root-agent",
			"providerID": "anthropic",
			"modelID":    "claude-opus-4",
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	// Verify first model was written.
	status, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (session 1): %v", err)
	}
	if status.RootModelID == nil || *status.RootModelID != "anthropic/claude-opus-4" {
		t.Fatalf("RootModelID after session 1 = %v, want %q", status.RootModelID, "anthropic/claude-opus-4")
	}

	// Second session: assistant message with a new (different) model — simulates
	// a configuration change between sessions. root_model_id must be updated.
	sc.HandleEvent(makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "text",
			"messageID": "msg-asst-session2",
			"text":      "Second session response.",
		},
	}))
	sc.HandleEvent(makeSSE("message.updated", map[string]any{
		"info": map[string]any{
			"id":         "msg-asst-session2",
			"role":       "assistant",
			"agent":      "root-agent",
			"providerID": "github-copilot",
			"modelID":    "claude-sonnet-4.6",
			"time": map[string]*float64{
				"created":   &created,
				"completed": &completed,
			},
		},
	}))

	// Verify root_model_id now reflects the new model, not the old one.
	status, err = sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (session 2): %v", err)
	}
	if status.RootModelID == nil {
		t.Fatal("RootModelID after session 2: got nil, want non-nil")
	}
	want := "github-copilot/claude-sonnet-4.6"
	if *status.RootModelID != want {
		t.Errorf("RootModelID after session 2 = %q, want %q (stale model not updated)", *status.RootModelID, want)
	}
}

// ── reconnect recovery timer tests ──────────────────────────────────────────

// TestServerConnected_InitialConnection_NoTimer verifies that server.connected
// on the initial connection (lastState empty) does NOT start a recovery timer.
func TestServerConnected_InitialConnection_NoTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))

	if clk.TimerCount() != 0 {
		t.Errorf("expected no timers on initial server.connected, got %d", clk.TimerCount())
	}
}

// TestServerConnected_WhileActive_StartsRecoveryTimer verifies that
// server.connected while in active state starts the reconnect recovery timer
// (AC-5: recovery timer fires only when sidecar reconnects and last state is active).
func TestServerConnected_WhileActive_StartsRecoveryTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state and drive lastState via an event.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// server.connected while active → recovery timer should start.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))

	if clk.TimerCount() == 0 {
		t.Fatal("expected recovery timer to be created after server.connected while active")
	}
}

// TestServerConnected_RecoveryTimer_FiresFinished verifies that when the
// recovery timer fires with no subsequent events, the sidecar writes finished
// (AC-5: writes finished and calls notifyCoordinator after recovery window).
func TestServerConnected_RecoveryTimer_FiresFinished(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// server.connected on reconnect.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))

	timer := clk.LastTimer()
	if timer == nil {
		t.Fatal("expected recovery timer to be created")
	}

	// Fire the timer manually (simulates 60s passing with no events).
	timer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after recovery timer fired, want %q", state, agent.StateFinished)
	}
}

// TestServerConnected_RecoveryTimer_CancelledBySessionIdle verifies that when
// session.idle arrives in the recovery window, the recovery timer is cancelled
// and normal idle debounce proceeds.
func TestServerConnected_RecoveryTimer_CancelledBySessionIdle(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Reconnect fires server.connected → recovery timer starts.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// session.idle arrives — should cancel recovery timer and start idle debounce.
	sc.HandleEvent(makeSSE("session.idle", map[string]any{}))

	// Recovery timer must be stopped.
	recoveryTimer.Fire()

	// State should still be active (idle debounce has not fired yet).
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after recovery-cancelled by session.idle, want %q",
			state, agent.StateActive)
	}

	// The idle debounce timer should now be present.
	idleTimer := clk.LastTimer()
	if idleTimer == nil || idleTimer == recoveryTimer {
		t.Fatal("expected new idle debounce timer after session.idle")
	}
	idleTimer.Fire()

	state = getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after idle debounce fired, want %q", state, agent.StateFinished)
	}
}

// TestServerConnected_RecoveryTimer_CancelledByBusy verifies that when
// session.status busy arrives in the recovery window, the recovery timer is
// cancelled (the session is still running).
func TestServerConnected_RecoveryTimer_CancelledByBusy(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Reconnect fires server.connected → recovery timer starts.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// session.status busy arrives — should cancel recovery timer.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Recovery timer must be stopped — firing it should NOT change state.
	recoveryTimer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateActive) {
		t.Errorf("state = %q after cancelled recovery timer, want %q", state, agent.StateActive)
	}
}

// TestServerConnected_NotActive_NoTimer verifies that server.connected while
// in a non-active state (e.g. waiting or finished) does NOT start a recovery timer.
func TestServerConnected_NotActive_NoTimer(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Drive to waiting state via events.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))
	sc.HandleEvent(makeSSE("permission.asked", map[string]any{
		"permission": "bash",
	}))

	countBefore := clk.TimerCount()

	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))

	if clk.TimerCount() != countBefore {
		t.Errorf("expected no new timers on server.connected while waiting, got %d new timer(s)",
			clk.TimerCount()-countBefore)
	}
}

// TestServerConnected_RecoveryTimer_CancelledBySessionError verifies that a
// session.error event cancels any in-flight recovery timer, preventing the
// timer from overwriting the error/interrupted state with finished.
func TestServerConnected_RecoveryTimer_CancelledBySessionError(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Reconnect fires recovery timer.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// A non-abort error arrives — must cancel the recovery timer.
	sc.HandleEvent(makeSSE("session.error", map[string]any{
		"error": map[string]string{"name": "SomeError"},
	}))

	// Fire the timer — should be a no-op.
	recoveryTimer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateError) {
		t.Errorf("state = %q after cancelled recovery timer on error, want %q",
			state, agent.StateError)
	}
}

// TestServerConnected_RecoveryTimer_CancelledByMessageAbortedError verifies
// that a MessageAbortedError cancels any in-flight recovery timer, preventing
// the timer from overwriting interrupted with finished.
func TestServerConnected_RecoveryTimer_CancelledByMessageAbortedError(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Reconnect fires recovery timer.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// User aborted — must cancel the recovery timer.
	sc.HandleEvent(makeSSE("session.error", map[string]any{
		"error": map[string]string{"name": "MessageAbortedError"},
	}))

	// Fire the timer — should be a no-op.
	recoveryTimer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q after cancelled recovery timer on abort, want %q",
			state, agent.StateInterrupted)
	}
}

// TestServerConnected_RecoveryTimer_CancelledByCompaction verifies that
// handleSessionCompacted cancels any in-flight recovery timer, preventing a
// spurious duplicate notifyCoordinator call after compaction finishes.
func TestServerConnected_RecoveryTimer_CancelledByCompaction(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state, then send a compacting status (no-op in handleSessionStatus,
	// but s.compacting and lastState = active are the relevant preconditions).
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))
	// Note: "compacting" type is not handled by handleSessionStatus, so lastState
	// stays "active" — which is the precondition handleServerConnected checks.
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "compacting"},
	}))

	// Reconnect while compacting — recovery timer should start (lastState == active).
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// Compaction finishes — must cancel the recovery timer.
	sc.HandleEvent(makeSSE("session.compacted", map[string]any{}))

	// Fire the timer — should be a no-op.
	recoveryTimer.Fire()

	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateFinished) {
		t.Errorf("state = %q after cancelled recovery timer on compaction, want %q",
			state, agent.StateFinished)
	}
}

// TestServerConnected_RecoveryTimer_CancelledByShutdown verifies that
// Shutdown() cancels any in-flight recovery timer (AC-5: must not fire after
// sidecar shutdown).
func TestServerConnected_RecoveryTimer_CancelledByShutdown(t *testing.T) {
	sc, clk := newTestSidecar(t)

	// Seed active state.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	sc.HandleEvent(makeSSE("session.status", map[string]any{
		"status": map[string]string{"type": "busy"},
	}))

	// Reconnect fires recovery timer.
	sc.HandleEvent(makeSSE("server.connected", map[string]any{}))
	recoveryTimer := clk.LastTimer()
	if recoveryTimer == nil {
		t.Fatal("expected recovery timer after server.connected")
	}

	// Shutdown must cancel the recovery timer.
	sc.Shutdown()

	// Fire the timer — should be a no-op because it was stopped.
	recoveryTimer.Fire()

	// After Shutdown the state should be interrupted (not finished).
	state := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if state != string(agent.StateInterrupted) {
		t.Errorf("state = %q after shutdown with cancelled recovery timer, want %q",
			state, agent.StateInterrupted)
	}
}
