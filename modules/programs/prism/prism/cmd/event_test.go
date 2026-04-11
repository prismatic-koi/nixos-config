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

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// setupEventTestDB seeds a temp DB with a single active session row and
// returns the DB path. The DB is closed before returning so openDB can reopen it.
func setupEventTestDB(t *testing.T, session string) string {
	t.Helper()
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
// AC-7: new test for the non-worktree session path.
func TestEventTmuxSessionStart_NonWorktreeSession(t *testing.T) {
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
// AC-8: meta-sessions must not appear in agent_status.
func TestEventTmuxSessionStart_SkipsMetaSessions(t *testing.T) {
	for _, session := range []string{"scratchpad", "prism-dashboard"} {
		t.Run(session, func(t *testing.T) {
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
// Regression guard for issue #576: state-change used to silently drop events
// for any session whose worktree had no .bare ancestor, leaving the dashboard
// stuck showing the session as "idle".
func TestEventStateChange_TracksNonWorktreeSession(t *testing.T) {
	const session = "obsidian"

	// Worktree path with no .bare ancestor — deriveRepo returns "".
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
// Regression guard for issue #576 — the precise name-based skip must still
// exclude meta-sessions even though the broader repo=="" guard is gone.
func TestEventStateChange_SkipsMetaSessions(t *testing.T) {
	for _, session := range []string{"scratchpad", "prism-dashboard"} {
		t.Run(session, func(t *testing.T) {
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
// resolves repo via deriveRepo and writes it to both agent_status and the
// state_change event row.
//
// Regression guard for issue #576 — relaxing the guard must not break the
// existing worktree flow.
func TestEventStateChange_WorktreeSession(t *testing.T) {
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
// This is the primary user-visible bug from issue #576 — the row existed but
// was never updated, so the dashboard showed "idle" forever.
func TestEventStateChange_UpdatesExistingNonWorktreeRow(t *testing.T) {
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

// TestEventTmuxSessionStart_NonWorktreeSession_ClearsEnded verifies that when
// an agent_status row already exists for a non-worktree session with ended_at
// set, a new tmux-session-start call clears ended_at (making the session
// visible to AllActiveStatus again).
//
// AC-9: regression protection for PR #475 — ClearEnded must fire for obsidian.
func TestEventTmuxSessionStart_NonWorktreeSession_ClearsEnded(t *testing.T) {
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
