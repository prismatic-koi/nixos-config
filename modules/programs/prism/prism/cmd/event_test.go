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
	"os/exec"
	"path/filepath"
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
