// Package cmd integration tests for prism restore.
//
// TestRestoreSession_* exercises restoreSession() directly and verifies:
//   - Bare-layout sessions are restored with the correct name and all three windows
//   - Non-bare sessions (obsidian pattern) are restored correctly — session name
//     does not equal filepath.Base(worktree)
//   - A session whose name would diverge from name-derivation still restores
//     with the stored (authoritative) name, not a re-derived one
//   - Already-existing sessions are skipped (idempotent)
//   - Sessions with missing/inaccessible worktrees are skipped and marked ended
//     in the DB, not left as zombies
//
// All tests use an isolated tmux server (cmdTestServer) and an isolated DB
// (SetTestDBPath) so they do not touch the live environment.
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// openRestoreTestDB creates a temp DB for restore tests and returns it.
func openRestoreTestDB(t *testing.T) *db.DB {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedStatus inserts an agent_status row into d and returns a db.Status for it.
func seedStatus(t *testing.T, d *db.DB, sessionName, worktree string, opencodeSession *string) db.Status {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "testrepo", worktree, "idle", nil, opencodeSession); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	for _, s := range statuses {
		if s.SessionName == sessionName {
			return s
		}
	}
	t.Fatalf("seedStatus: row for %q not found after upsert", sessionName)
	panic("unreachable")
}

// windowNames returns the window names for the given session in creation order,
// using list-windows.
func windowNames(t *testing.T, s *cmdTestServer, session string) []string {
	t.Helper()
	out, err := s.output("list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows %q: %v", session, err)
	}
	var names []string
	for _, n := range strings.Split(out, "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// isEnded returns true when the agent_status row for sessionName has a non-null
// ended_at (i.e. SetEnded was called).
func isEnded(t *testing.T, d *db.DB, sessionName string) bool {
	t.Helper()
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	for _, s := range statuses {
		if s.SessionName == sessionName {
			// row present in AllActiveStatus → ended_at IS NULL → not ended
			return false
		}
	}
	// Not in active list. Query directly to confirm the row exists with ended_at set.
	// Use QueryRow exposed for testing.
	row := d.QueryRow(
		"SELECT ended_at FROM agent_status WHERE session_name = ?", sessionName,
	)
	var endedAt *int64
	if err := row.Scan(&endedAt); err != nil {
		// Row not found at all — not our concern for this assertion.
		return false
	}
	return endedAt != nil
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestRestoreSession_BareLayout verifies that a bare-layout session
// (e.g. nixos-config@main) is restored with exactly the authoritative
// session name and the three-window layout: edit / agent / term.
func TestRestoreSession_BareLayout(t *testing.T) {
	// Uses withCmdServer (mutates TmuxBin) — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "nixos-config@main"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	// Kill any sidecar that setupFullLayout may have launched.
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	windows := windowNames(t, s, sessionName)
	want := []string{"edit", "agent", "term"}
	if len(windows) != len(want) {
		t.Fatalf("window count = %d, want %d; got %v", len(windows), len(want), windows)
	}
	for i, w := range want {
		if windows[i] != w {
			t.Errorf("window[%d] = %q, want %q", i, windows[i], w)
		}
	}
}

// TestRestoreSession_NonBare verifies the "obsidian pattern": a session whose
// name does NOT match filepath.Base(worktree) is restored with the stored
// authoritative name, not a re-derived one.
//
// obsidian session:
//   - SessionName: "obsidian"
//   - Worktree:    "/home/ben/documents/obsidian" (base == "obsidian" here,
//     but the key property is that the name is not derived from any git root)
//
// We simulate a non-bare directory (no .bare) so name-derivation would produce
// a different name if allowed to run. The test uses an unrelated temp directory
// as the worktree to guarantee the derivation path is never taken.
func TestRestoreSession_NonBare(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	// Worktree is a plain temp dir (not inside any bare git repo).
	worktreeDir := t.TempDir()

	// Use a session name that has nothing to do with filepath.Base(worktreeDir).
	// t.TempDir() returns something like /tmp/TestXxx123/001 — so the base
	// would be "001". We pick a completely different name.
	sessionName := "obsidian"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	// Kill any sidecar that setupFullLayout may have launched.
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	// The session must be created under the authoritative name — not under any
	// derived name like filepath.Base(worktreeDir).
	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created — wrong name may have been used", sessionName)
	}

	// Confirm the derived name was NOT created.
	derivedName := filepath.Base(worktreeDir)
	if derivedName != sessionName && s.hasSession(derivedName) {
		t.Errorf("session %q was also created — name was re-derived instead of using authoritative name", derivedName)
	}

	windows := windowNames(t, s, sessionName)
	if len(windows) != 3 {
		t.Errorf("window count = %d, want 3; got %v", len(windows), windows)
	}
}

// TestRestoreSession_NameDivergence verifies that a session whose stored name
// would not match what sessionNameFor() would produce from the worktree is still
// restored with the correct (stored) name.
//
// This is the core regression test for Bug 1: the old code called
// ensureAndSwitchSession(s.Worktree, bareRoot, ...) which re-derived the
// session name from the filesystem. If the stored worktree was corrupted (Bug 2)
// or belonged to a non-bare session, the derived name diverged from s.SessionName
// and the correct session was never created.
func TestRestoreSession_NameDivergence(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()

	// Authoritative session name is deliberately different from what
	// sessionNameFor would produce. sessionNameFor of a plain dir returns
	// filepath.Base(dir) (with dots replaced by underscores). Our stored name
	// is something else entirely.
	sessionName := "special-project@my-branch"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	// Kill any sidecar that setupFullLayout may have launched.
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	// Must exist under the authoritative name.
	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// The name that sessionNameFor would have derived must not exist.
	derivedName := filepath.Base(worktreeDir)
	if s.hasSession(derivedName) {
		t.Errorf("derived session %q was created instead of the authoritative %q", derivedName, sessionName)
	}
}

// TestRestoreSession_Idempotent verifies that restoreSession is a no-op when
// the target session already exists in tmux.
func TestRestoreSession_Idempotent(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	sessionName := "nixos-config@feature"
	// Create the session in tmux before calling restoreSession.
	s.newSession(sessionName)

	worktreeDir := t.TempDir()
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	// Should return nil and not attempt to recreate the session.
	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession on existing session: %v", err)
	}

	// Session must still exist (not killed).
	if !s.hasSession(sessionName) {
		t.Fatalf("session %q disappeared after idempotent restore call", sessionName)
	}

	// Only one session with that name should exist (tmux deduplicates by name
	// anyway, but we verify the window count is what we seeded — not a fresh
	// three-window layout which would indicate the function ignored HasSession).
	out, err := s.output("list-windows", "-t", sessionName, "-F", "#{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	// newSession creates a single unnamed window (tmux default name "0" or "bash").
	// The standard restore layout has 3 windows. If restore created new windows,
	// the count would be ≥3.
	var count int
	for _, n := range strings.Split(out, "\n") {
		if strings.TrimSpace(n) != "" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("window count = %d after idempotent restore, want 1 (original session unmodified)", count)
	}
}

// TestRestoreSession_MissingWorktree verifies that a session with a worktree
// path that does not exist on disk is marked as ended in the DB and is NOT
// created as a tmux session.
func TestRestoreSession_MissingWorktree(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	sessionName := "myrepo@gone-branch"
	// Use a path that definitely does not exist.
	missingPath := filepath.Join(t.TempDir(), "this-dir-does-not-exist")
	status := seedStatus(t, d, sessionName, missingPath, nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession with missing worktree returned error: %v", err)
	}

	// No tmux session should have been created.
	if s.hasSession(sessionName) {
		t.Errorf("session %q was created despite missing worktree — should be marked ended instead", sessionName)
	}

	// The DB row must be marked ended.
	if !isEnded(t, d, sessionName) {
		t.Errorf("session %q not marked ended in DB after missing-worktree restore", sessionName)
	}
}

// TestRestoreSession_EmptyWorktree verifies that a session with an empty
// worktree string is marked as ended in the DB rather than left as a zombie.
func TestRestoreSession_EmptyWorktree(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	sessionName := "myrepo@no-worktree"
	status := seedStatus(t, d, sessionName, "", nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession with empty worktree returned error: %v", err)
	}

	if s.hasSession(sessionName) {
		t.Errorf("session %q was created despite empty worktree — should be marked ended", sessionName)
	}

	if !isEnded(t, d, sessionName) {
		t.Errorf("session %q not marked ended in DB after empty-worktree restore", sessionName)
	}
}

// TestRestoreSession_OpencodeSessionResumed verifies that when a stored
// OpencodeSID is present, the opencode launch command sent to the agent window
// includes the session ID (-s flag).
//
// It captures the actual text typed into the agent pane (window 1) via
// capture-pane and asserts the session ID appears in the captured output.
func TestRestoreSession_OpencodeSessionResumed(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@feature"
	sid := "oc-session-abc123"
	status := seedStatus(t, d, sessionName, worktreeDir, &sid)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	// Kill any sidecar that setupFullLayout may have launched.
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// Poll the agent window (index 1) until the session ID appears in the pane
	// content. send-keys is asynchronous so we allow up to 3 seconds for the
	// text to land in the shell's input buffer / history.
	deadline := time.Now().Add(3 * time.Second)
	var paneContent string
	for time.Now().Before(deadline) {
		out, err := s.output("capture-pane", "-t", sessionName+":1", "-p")
		if err == nil {
			paneContent = out
			if strings.Contains(paneContent, "-s "+sid) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !strings.Contains(paneContent, "-s "+sid) {
		t.Errorf("agent pane does not contain '-s %s'; captured output:\n%s", sid, paneContent)
	}
}

// TestRestoreSession_AllThreeWindows is a table-driven test confirming that
// all three windows (edit/agent/term) are always created in the right order,
// regardless of the session name format.
func TestRestoreSession_AllThreeWindows(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()

	cases := []struct {
		sessionName string
	}{
		{"proj@main"},
		{"proj@feature"},
		{"standalone-session"},
		{"obsidian"},
	}

	for i, tc := range cases {
		// Create a unique subdirectory per case so there's no cross-contamination.
		caseDir := filepath.Join(worktreeDir, "case"+strings.ReplaceAll(tc.sessionName, "@", "_"))
		if err := os.MkdirAll(caseDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		status := seedStatus(t, d, tc.sessionName, caseDir, nil)

		if err := restoreSession(d, status); err != nil {
			t.Fatalf("[%d] %q: restoreSession: %v", i, tc.sessionName, err)
		}
		// Kill any sidecar that setupFullLayout may have launched for this session.
		func(name string) {
			t.Cleanup(func() { session.KillSidecar(name) })
		}(tc.sessionName)

		if !s.hasSession(tc.sessionName) {
			t.Errorf("[%d] %q: session not created", i, tc.sessionName)
			continue
		}

		windows := windowNames(t, s, tc.sessionName)
		want := []string{"edit", "agent", "term"}
		if len(windows) != len(want) {
			t.Errorf("[%d] %q: window count = %d, want %d; got %v",
				i, tc.sessionName, len(windows), len(want), windows)
			continue
		}
		for j, w := range want {
			if windows[j] != w {
				t.Errorf("[%d] %q: window[%d] = %q, want %q", i, tc.sessionName, j, windows[j], w)
			}
		}
	}
}
