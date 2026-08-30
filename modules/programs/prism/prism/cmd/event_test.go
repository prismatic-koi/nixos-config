// Package cmd unit tests for eventTmuxSessionEndCmd liveness check.
//
// TestEventTmuxSessionEnd_LiveSession verifies that when the named session still
// exists in tmux, the handler exits 0 without writing ended_at to the DB.
//
// TestEventTmuxSessionEnd_DeadSession verifies that when the named session no
// longer exists in tmux, the handler writes ended_at to the DB as normal.
//
// TestEventTmuxSessionEnd_EmptySession verifies that an empty session name
// causes the handler to return an error without touching tmux or the DB.
package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// setupEventTestDB seeds a temp DB with a single active session row and
// returns the DB path. The DB is closed before returning so openDB can reopen it.
// It also unsets PRISM_HOST_API so that direct-DB-path tests exercise the
// local DB rather than attempting to proxy (proxy tests live in
// event_proxy_test.go).
func setupEventTestDB(t *testing.T, session string) string {
	t.Helper()
	// Wipe any rootCmd flag values left behind by a previous test (or a
	// previous iteration under `go test -count=N`) before this test drives
	// the cobra tree via rootCmd.SetArgs / rootCmd.Execute.
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, "myrepo", "/code/myrepo/branch", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()
	return dbFile
}

// TestEventTmuxSessionEnd_LiveSession verifies that the handler exits 0 without
// writing ended_at when the session still exists in tmux.
//
// This covers the spurious display-popup dismissal scenario: session-closed
// fires but the outer session is still alive.
func TestEventTmuxSessionEnd_LiveSession(t *testing.T) {
	// Uses withCmdServer (mutates TmuxBin) — must not be parallel.
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH — skipping integration test")
	}

	const session = "myrepo@live-branch"

	dbFile := setupEventTestDB(t, session)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Spin up an isolated tmux server and redirect TmuxBin.
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Create the session so it is still alive in tmux.
	s.newSession(session)

	if !tmux.HasSession(session) {
		t.Fatalf("pre-condition failed: HasSession(%q) = false", session)
	}

	// Execute the command — should exit 0 without writing ended_at.
	rootCmd.SetArgs([]string{"event", "tmux-session-end", "--session", session})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil (session is live)", err)
	}

	// Re-open the DB and verify ended_at is NOT set.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("status row missing from DB")
	}
	if status.EndedAt != nil {
		t.Errorf("ended_at is set (%v) for a live session — spurious write detected", *status.EndedAt)
	}
}

// TestEventTmuxSessionEnd_DeadSession verifies that the handler writes ended_at
// when the session no longer exists in tmux.
func TestEventTmuxSessionEnd_DeadSession(t *testing.T) {
	// Uses withCmdServer (mutates TmuxBin) — must not be parallel.
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH — skipping integration test")
	}

	const session = "myrepo@dead-branch"

	dbFile := setupEventTestDB(t, session)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Spin up an isolated tmux server and redirect TmuxBin.
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Do NOT create the session — it is intentionally absent from tmux.
	if tmux.HasSession(session) {
		t.Fatalf("pre-condition failed: HasSession(%q) = true (session should not exist)", session)
	}

	// Execute the command — should write ended_at.
	rootCmd.SetArgs([]string{"event", "tmux-session-end", "--session", session})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	// Verify ended_at IS set.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("status row missing from DB")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — session-end was not written for a dead session")
	}
}

// TestEventTmuxSessionEnd_EmptySession verifies that passing a whitespace-only
// session name causes the handler to return an error without invoking
// tmux has-session or writing to the DB.
//
// Note: cobra's MarkFlagRequired only checks presence, not content, so an
// explicitly-passed empty string reaches the handler and is rejected there.
func TestEventTmuxSessionEnd_EmptySession(t *testing.T) {
	// This test does not require a live tmux server: the empty-session guard
	// must reject the request before any tmux or DB call.

	// Point openDB at an isolated empty DB so openDB doesn't fall through to
	// the production path.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Cobra will reject a completely missing --session flag (required), so we
	// test only the case where the flag is explicitly set to an empty string or
	// whitespace.  These all reach the handler and must be rejected by our guard.
	for _, session := range []string{"", "   "} {
		rootCmd.SetArgs([]string{"event", "tmux-session-end", "--session", session})
		if err := rootCmd.Execute(); err == nil {
			t.Errorf("Execute with session=%q returned nil, want error", session)
		}
	}
}

// TestEventTmuxSessionStart_NonWorktreeSession verifies that a non-worktree
// session (like "obsidian") gets an agent_status row with repo="", state="idle",
// and ended_at=NULL when tmux-session-start fires.
//
// Covers the non-worktree session path.
func TestEventTmuxSessionStart_NonWorktreeSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	// Does not need a live tmux server — no tmux calls in this path.
	const session = "obsidian"

	// Use a temp dir as the worktree: no .bare marker present.
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row for obsidian, got nil")
	}
	if status.Repo != "" {
		t.Errorf("repo = %q, want empty string", status.Repo)
	}
	if status.State != "idle" {
		t.Errorf("state = %q, want \"idle\"", status.State)
	}
	if status.EndedAt != nil {
		t.Errorf("ended_at = %v, want NULL", *status.EndedAt)
	}
}

// TestEventTmuxSessionStart_SkipsMetaSessions verifies that "scratchpad" and
// "prism-dashboard" are still silently skipped and do NOT produce an
// agent_status row.
//
// Meta-sessions must not appear in agent_status.
func TestEventTmuxSessionStart_SkipsMetaSessions(t *testing.T) {
	for _, session := range []string{"scratchpad", "prism-dashboard"} {
		t.Run(session, func(t *testing.T) {
			t.Setenv("PRISM_HOST_API", "")
			worktree := t.TempDir()
			dbFile := filepath.Join(t.TempDir(), "prism.db")
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			d.Close()

			SetTestDBPath(dbFile)
			t.Cleanup(func() { SetTestDBPath("") })

			rootCmd.SetArgs([]string{
				"event", "tmux-session-start",
				"--session", session,
				"--worktree", worktree,
			})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("Execute returned error %v, want nil (silent skip)", err)
			}

			d2, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("re-open db: %v", err)
			}
			defer d2.Close()

			status, err := d2.CurrentStatus(session)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if status != nil {
				t.Errorf("session %q: expected no agent_status row, got one (state=%q)", session, status.State)
			}
		})
	}
}

// TestEventStateChange_TracksNonWorktreeSession verifies that firing
// state-change for a non-worktree session (e.g. "obsidian") writes/updates a
// row in agent_status with repo="" and the supplied state, and appends a
// state_change event row to the events table.
//
// Regression guard: without this, state-change silently drops events for any
// session whose worktree has no .bare ancestor, leaving the dashboard stuck
// showing the session as "idle".
func TestEventStateChange_TracksNonWorktreeSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "obsidian"

	// Worktree path with no .bare ancestor — repoFromWorktreePath returns "".
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "state-change",
		"--session", session,
		"--state", "active",
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row for obsidian, got nil")
	}
	if status.Repo != "" {
		t.Errorf("repo = %q, want empty string", status.Repo)
	}
	if status.State != "active" {
		t.Errorf("state = %q, want \"active\"", status.State)
	}

	events, err := d2.QueryEvents(session, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "state_change" {
		t.Errorf("event type = %q, want \"state_change\"", ev.Type)
	}
	if ev.SessionName != session {
		t.Errorf("event session_name = %q, want %q", ev.SessionName, session)
	}
	if ev.Repo != "" {
		t.Errorf("event repo = %q, want empty string", ev.Repo)
	}
	if !strings.Contains(ev.Payload, `"active"`) {
		t.Errorf("event payload = %q, want payload containing \"active\"", ev.Payload)
	}
}

// TestEventStateChange_SkipsMetaSessions verifies that "scratchpad" and
// "prism-dashboard" state-change invocations return nil without writing an
// agent_status row or an events row.
//
// Regression guard — the precise name-based skip must exclude
// meta-sessions even though the broader repo=="" guard is gone.
func TestEventStateChange_SkipsMetaSessions(t *testing.T) {
	for _, session := range []string{"scratchpad", "prism-dashboard"} {
		t.Run(session, func(t *testing.T) {
			t.Setenv("PRISM_HOST_API", "")
			worktree := t.TempDir()

			dbFile := filepath.Join(t.TempDir(), "prism.db")
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			d.Close()

			SetTestDBPath(dbFile)
			t.Cleanup(func() { SetTestDBPath("") })

			rootCmd.SetArgs([]string{
				"event", "state-change",
				"--session", session,
				"--state", "active",
				"--worktree", worktree,
			})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("Execute returned error %v, want nil (silent skip)", err)
			}

			d2, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("re-open db: %v", err)
			}
			defer d2.Close()

			status, err := d2.CurrentStatus(session)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if status != nil {
				t.Errorf("session %q: expected no agent_status row, got one (state=%q)", session, status.State)
			}

			events, err := d2.QueryEvents(session, 10, nil, nil, nil)
			if err != nil {
				t.Fatalf("QueryEvents: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("session %q: expected 0 events, got %d", session, len(events))
			}
		})
	}
}

// TestEventStateChange_WorktreeSession verifies the original worktree-backed
// happy path: a state-change invocation against a path with a .bare ancestor
// resolves repo via repoFromWorktreePath and writes it to both agent_status and the
// state_change event row.
//
// Regression guard — relaxing the guard must not break the existing
// worktree flow.
func TestEventStateChange_WorktreeSession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "myrepo@main"

	// Build a fake bare repo layout: <tmp>/myrepo/{.bare, worktree}
	// deriveBareRoot will walk up from the worktree path and find .bare,
	// yielding repo name "myrepo".
	root := t.TempDir()
	bareRoot := filepath.Join(root, "myrepo")
	worktree := filepath.Join(bareRoot, "main")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.Mkdir(filepath.Join(bareRoot, ".bare"), 0o755); err != nil {
		t.Fatalf("mkdir .bare: %v", err)
	}

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "state-change",
		"--session", session,
		"--state", "active",
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row, got nil")
	}
	if status.Repo != "myrepo" {
		t.Errorf("repo = %q, want \"myrepo\"", status.Repo)
	}
	if status.State != "active" {
		t.Errorf("state = %q, want \"active\"", status.State)
	}

	events, err := d2.QueryEvents(session, 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != "state_change" {
		t.Errorf("event type = %q, want \"state_change\"", ev.Type)
	}
	if ev.Repo != "myrepo" {
		t.Errorf("event repo = %q, want \"myrepo\"", ev.Repo)
	}
	if !strings.Contains(ev.Payload, `"active"`) {
		t.Errorf("event payload = %q, want payload containing \"active\"", ev.Payload)
	}
}

// TestEventStateChange_UpdatesExistingNonWorktreeRow verifies that when
// tmux-session-start has already created an agent_status row for a
// non-worktree session, a subsequent state-change for that session updates
// the existing row's state column rather than inserting a duplicate.
//
// This is the primary user-visible bug — the row existed but was never
// updated, so the dashboard showed "idle" forever.
func TestEventStateChange_UpdatesExistingNonWorktreeRow(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "obsidian"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Simulate the row that tmux-session-start would have written.
	if err := d.UpsertStatus(session, "", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "state-change",
		"--session", session,
		"--state", "active",
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	// There should still be exactly one row for this session, with its state
	// updated from "idle" to "active".
	statuses, err := d2.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	count := 0
	for _, s := range statuses {
		if s.SessionName == session {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d agent_status rows for %q, want 1", count, session)
	}

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row, got nil")
	}
	if status.State != "active" {
		t.Errorf("state = %q, want \"active\" (row was not updated)", status.State)
	}
	if status.Repo != "" {
		t.Errorf("repo = %q, want empty string", status.Repo)
	}
}

// TestEventTmuxSessionStart_AgentRole verifies that when --agent-role is passed
// to tmux-session-start, the resulting agent_status row has root_agent_name set
// to the provided role value immediately (before the sidecar writes it).
func TestEventTmuxSessionStart_AgentRole(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "myrepo@feature"
	const agentRole = "worker"

	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
		"--agent-role", agentRole,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row, got nil")
	}
	if status.RootAgentName == nil {
		t.Fatal("RootAgentName: got nil, want \"worker\"")
	}
	if *status.RootAgentName != agentRole {
		t.Errorf("RootAgentName: got %q, want %q", *status.RootAgentName, agentRole)
	}
	if status.State != "idle" {
		t.Errorf("State: got %q, want \"idle\"", status.State)
	}
}

// TestEventTmuxSessionStart_NoAgentRole verifies that when --agent-role is
// omitted from tmux-session-start, root_agent_name remains NULL (unchanged
// from prior value or NULL on fresh insert). Existing callers that don't pass
// --agent-role continue to behave as before.
func TestEventTmuxSessionStart_NoAgentRole(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "myrepo@no-role-branch"

	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
		"--agent-role", "", // explicitly empty to reset any prior test's flag value
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row, got nil")
	}
	// Without --agent-role (empty string), root_agent_name must be NULL on fresh insert.
	if status.RootAgentName != nil {
		t.Errorf("RootAgentName: got %q, want nil (no --agent-role provided)", *status.RootAgentName)
	}
}

// TestEventTmuxSessionStart_AgentRole_PreservesExisting verifies that when a
// row already exists with root_agent_name set (e.g. from a prior seed), a
// subsequent tmux-session-start without --agent-role preserves the existing
// root_agent_name value (COALESCE in UpsertStatus leaves it unchanged).
func TestEventTmuxSessionStart_AgentRole_PreservesExisting(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "myrepo@preserve-role-branch"

	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Pre-seed a row with root_agent_name already set.
	if err := d.UpsertStatusSeedRootAgentName(session, "myrepo", worktree, "idle", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Fire tmux-session-start without --agent-role (explicitly empty to reset
	// any prior test's cobra flag value).
	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
		"--agent-role", "",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row, got nil")
	}
	// Existing root_agent_name must be preserved.
	if status.RootAgentName == nil || *status.RootAgentName != "coordinator" {
		t.Errorf("RootAgentName: got %v, want preserved \"coordinator\"", status.RootAgentName)
	}
}

// TestEventTmuxSessionStart_NonWorktreeSession_ClearsEnded verifies that when
// an agent_status row already exists for a non-worktree session with ended_at
// set, a new tmux-session-start call clears ended_at (making the session
// visible to AllActiveStatus again).
//
// Regression protection: ClearEnded must fire for obsidian.
func TestEventTmuxSessionStart_NonWorktreeSession_ClearsEnded(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "obsidian"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Pre-seed a row with ended_at set (simulating a cleanup cycle).
	if err := d.UpsertStatus(session, "", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Verify pre-condition: ended_at is set.
	pre, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus (pre): %v", err)
	}
	if pre == nil || pre.EndedAt == nil {
		t.Fatal("pre-condition failed: ended_at should be set after SetEnded")
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute returned error %v, want nil", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus (post): %v", err)
	}
	if status == nil {
		t.Fatal("expected agent_status row after tmux-session-start, got nil")
	}
	if status.EndedAt != nil {
		t.Errorf("ended_at = %v, want NULL — ClearEnded did not fire", *status.EndedAt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// tmux-session-start must mint a fresh instance_id when the previous
// incarnation's sessions row has ended_at set.
//
// Without this, the handler reuses agent_status.instance_id unconditionally,
// so a long-lived coordinator session once spawned with
// `prism spawn --profile X` carries that profile pin forward forever via the
// spawn_inputs.profile_name column — even after `prism profile use Y` and
// many respawns. Detecting the ended-previous-incarnation case and minting a
// fresh UUID makes profile resolution at agent-run time fall through to
// state-file / nix-default (the user's currently-active profile).
//
// The prior spawn_inputs rows belonging to ended incarnations must NOT be
// mutated or deleted by the respawn path — they are the audit trail that
// `prism stats` / archive queries aggregate by profile_name.
// ─────────────────────────────────────────────────────────────────────────────

// TestEventTmuxSessionStart_RespawnAfterEnd_MintsFreshInstance verifies that
// when the previous sessions row has ended_at set, tmux-session-start mints
// a fresh instance_id and writes it into agent_status.
func TestEventTmuxSessionStart_RespawnAfterEnd_MintsFreshInstance(t *testing.T) {
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	const session = "prism-test@2253-respawn-after-end"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Pre-seed the bug scenario: a previous incarnation exists in
	// agent_status + sessions, was cleanly closed (ended_at set), and
	// recorded a spawn_inputs.profile_name pin.
	oldIID := uuid.New().String()
	if err := d.UpsertStatus(session, "prism-test", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, oldIID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  oldIID,
		SessionName: session,
		Repo:        "prism-test",
		Worktree:    worktree,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	pinned := "anthropic-opus"
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:  oldIID,
		ProfileName: &pinned,
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}
	if err := d.UpdateSessionEnded(oldIID, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	// agent_status.instance_id must now be a freshly-minted UUID,
	// not the old (ended) one.
	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil || status.InstanceID == nil {
		t.Fatal("expected agent_status row with non-nil instance_id")
	}
	newIID := *status.InstanceID
	if newIID == oldIID {
		t.Fatalf("instance_id was reused (%q) — fresh mint required when previous incarnation has ended_at set", oldIID)
	}

	// MostRecentSessionForName must return the new (live) sessions row,
	// not the old one — so SpawnTimeForSession's lookup chain naturally
	// falls through to state-file / nix-default profile resolution.
	mostRecent, err := d2.MostRecentSessionForName(session)
	if err != nil {
		t.Fatalf("MostRecentSessionForName: %v", err)
	}
	if mostRecent == nil || mostRecent.InstanceID != newIID {
		t.Fatalf("MostRecentSessionForName: got %+v, want row with instance_id=%q", mostRecent, newIID)
	}
	if mostRecent.EndedAt != nil {
		t.Errorf("new sessions row has ended_at=%v, want NULL", *mostRecent.EndedAt)
	}

	// The new instance_id must have NO spawn_inputs row — that is what
	// makes profile resolution fall through to the active profile.
	newSI, err := d2.SpawnInputsByInstanceID(newIID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID(new): %v", err)
	}
	if newSI != nil {
		t.Errorf("new instance_id %q has spawn_inputs row %+v, want none", newIID, newSI)
	}

	// The prior spawn_inputs row for the ENDED incarnation must not be mutated
	// or deleted by the respawn path. `prism stats` and archive queries
	// depend on it.
	oldSI, err := d2.SpawnInputsByInstanceID(oldIID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID(old): %v", err)
	}
	if oldSI == nil {
		t.Fatal("old spawn_inputs row was deleted — audit trail must be preserved (#2090)")
	}
	if oldSI.ProfileName == nil || *oldSI.ProfileName != pinned {
		t.Errorf("old spawn_inputs.profile_name = %v, want preserved %q (must not be mutated)", oldSI.ProfileName, pinned)
	}
	oldSess, err := d2.SessionByInstanceID(oldIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID(old): %v", err)
	}
	if oldSess == nil {
		t.Fatal("old sessions row was deleted — audit trail must be preserved")
	}
	if oldSess.EndedAt == nil {
		t.Error("old sessions row's ended_at was cleared — must not be mutated")
	}
}

// TestEventTmuxSessionStart_LiveIncarnation_PreservesInstance verifies that
// when the previous sessions row has ended_at unset (within-incarnation
// agent-pane restart), tmux-session-start reuses the existing instance_id so
// the spawn_inputs.profile_name pin keeps resolving.
func TestEventTmuxSessionStart_LiveIncarnation_PreservesInstance(t *testing.T) {
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	const session = "prism-test@2253-live-incarnation"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	liveIID := uuid.New().String()
	if err := d.UpsertStatus(session, "prism-test", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, liveIID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  liveIID,
		SessionName: session,
		Repo:        "prism-test",
		Worktree:    worktree,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	pinned := "anthropic-opus"
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:  liveIID,
		ProfileName: &pinned,
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}
	// Intentionally do NOT call UpdateSessionEnded — the incarnation is live.
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil || status.InstanceID == nil {
		t.Fatal("expected agent_status row with non-nil instance_id")
	}
	if *status.InstanceID != liveIID {
		t.Fatalf("instance_id = %q, want preserved %q — live incarnation must keep its instance_id", *status.InstanceID, liveIID)
	}

	// The spawn_inputs pin must still resolve through the unchanged
	// instance_id — this is what guarantees `prism spawn --profile X`
	// keeps X across agent-pane restarts.
	si, err := d2.SpawnInputsByInstanceID(liveIID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil || si.ProfileName == nil || *si.ProfileName != pinned {
		t.Errorf("spawn_inputs.profile_name lookup via reused instance_id failed: si=%+v, want %q", si, pinned)
	}
}

// TestEventTmuxSessionStart_LegacyAgentStatusNoSessionsRow verifies that
// when agent_status carries an instance_id that has no matching sessions row
// (a legacy shape), the respawn path falls through to the existing reuse
// behaviour — the instance_id is preserved and a sessions row is created
// for it via INSERT OR IGNORE. Because no spawn_inputs row exists for the
// reused instance_id, profile resolution still naturally falls through to
// state-file / nix-default at agent-run time.
func TestEventTmuxSessionStart_LegacyAgentStatusNoSessionsRow(t *testing.T) {
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	const session = "prism-test@2253-legacy-no-sessions-row"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	legacyIID := uuid.New().String()
	if err := d.UpsertStatus(session, "prism-test", worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, legacyIID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	// Intentionally no InsertSession — the legacy shape with no sessions row.
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil || status.InstanceID == nil {
		t.Fatal("expected agent_status row with non-nil instance_id")
	}
	if *status.InstanceID != legacyIID {
		t.Fatalf("instance_id = %q, want preserved legacy %q — nil prevSess must preserve reuse behaviour", *status.InstanceID, legacyIID)
	}

	// A sessions row should now exist for the (reused) legacy instance_id.
	sess, err := d2.SessionByInstanceID(legacyIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Error("expected sessions row inserted for legacy instance_id, got nil")
	}

	// And no spawn_inputs row exists — so SpawnTimeForSession returns ""
	// and profile resolution falls through to state-file / nix-default,
	// unchanged from today.
	si, err := d2.SpawnInputsByInstanceID(legacyIID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si != nil {
		t.Errorf("unexpected spawn_inputs row for legacy session: %+v", si)
	}
}
