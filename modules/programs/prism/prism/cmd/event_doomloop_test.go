package cmd

// Tests for `prism event doom-loop-detected`.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

// TestEventDoomLoopDetected_WritesEvent verifies that
// `prism event doom-loop-detected` writes a doom_loop_detected event to the DB
// with the correct payload fields.
func TestEventDoomLoopDetected_WritesEvent(t *testing.T) {
	const session = "testrepo@main"

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "doom-loop-detected",
		"--session", session,
		"--tool", "bash",
		"--pattern", "bash:go test ./...",
		"--count", "5",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	events, err := d2.QueryDoomLoopEvents(session, 0)
	if err != nil {
		t.Fatalf("QueryDoomLoopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 doom_loop_detected event, got %d", len(events))
	}

	e := events[0]
	if e.Type != "doom_loop_detected" {
		t.Errorf("event type = %q, want \"doom_loop_detected\"", e.Type)
	}
	if e.SessionName != session {
		t.Errorf("session_name = %q, want %q", e.SessionName, session)
	}

	var p payload.DoomLoopDetected
	if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Tool != "bash" {
		t.Errorf("payload.tool = %q, want \"bash\"", p.Tool)
	}
	if p.Pattern != "bash:go test ./..." {
		t.Errorf("payload.pattern = %q, want \"bash:go test ./...\"", p.Pattern)
	}
	if p.Count != 5 {
		t.Errorf("payload.count = %d, want 5", p.Count)
	}
}

// TestEventDoomLoopDetected_UnknownSession verifies that the command writes an
// event even when the session does not have an agent_status row (repo/worktree
// default to empty strings in that case).
func TestEventDoomLoopDetected_UnknownSession(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "doom-loop-detected",
		"--session", "unknown@session",
		"--tool", "bash",
		"--pattern", "bash:go test",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	events, err := d2.QueryDoomLoopEvents("unknown@session", 0)
	if err != nil {
		t.Fatalf("QueryDoomLoopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// repo and worktree should be empty strings when no status row exists.
	if events[0].Repo != "" {
		t.Errorf("repo = %q, want empty string for unknown session", events[0].Repo)
	}
}

// TestEventDoomLoopDetected_DefaultCount verifies that the default count is 5
// when not specified.
func TestEventDoomLoopDetected_DefaultCount(t *testing.T) {
	const session = "testrepo@main"

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "doom-loop-detected",
		"--session", session,
		"--tool", "edit",
		"--pattern", "edit:/workspace/foo.go",
		// no --count → default 5
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	events, err := d2.QueryDoomLoopEvents(session, 0)
	if err != nil {
		t.Fatalf("QueryDoomLoopEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	var p payload.DoomLoopDetected
	if err := json.Unmarshal([]byte(events[0].Payload), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Count != 5 {
		t.Errorf("default count = %d, want 5", p.Count)
	}
}

// TestEventDoomLoopDetected_PayloadContainsFields verifies the raw JSON payload
// contains the expected field names.
func TestEventDoomLoopDetected_PayloadContainsFields(t *testing.T) {
	const session = "testrepo@main"

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "doom-loop-detected",
		"--session", session,
		"--tool", "webfetch",
		"--pattern", "webfetch:https://example.com",
		"--count", "5",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	events, err := d2.QueryDoomLoopEvents(session, 0)
	if err != nil {
		t.Fatalf("QueryDoomLoopEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events found")
	}

	raw := events[0].Payload
	for _, field := range []string{`"tool"`, `"pattern"`, `"count"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("payload missing field %s\ngot: %s", field, raw)
		}
	}
}
