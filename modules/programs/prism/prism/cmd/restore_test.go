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

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// skipRestoreOnGHA skips a TestRestoreSession_* test on a GitHub Actions
// ubuntu-latest runner.
//
// These tests drive a real tmux server and call restoreSession(), which
// spawns the per-session "agent" window with a start command that, in
// bwrap-isolation mode, ultimately exec(2)s bwrap. On a GHA runner step,
// unprivileged user-namespace uid-map setup is blocked by the runner's
// apparmor profile, so the bwrap exec fails immediately with "setting up
// uid map: Permission denied". The window dies before tmux can record it
// in #{pane_start_command}, leaving the session with only [edit, term] and
// the assertions on the agent window fail.
//
// The skip is loud and named per issue #1510 — a follow-up that explores
// self-hosted runners, privileged containers, or alternative test shapes
// that would let these tests run on CI rather than only in a host shell
// or a Nix dev shell.
func skipRestoreOnGHA(t *testing.T) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Skipf("skipping on GitHub Actions ubuntu-latest: %s — see #1510",
			"unprivileged userns uid-map setup is disallowed (kernel.apparmor_restrict_unprivileged_userns=1)")
	}
}

// withRestoreConfig overrides loadRestoreConfig for the duration of the test
// and restores the previous value on cleanup. It is used to exercise the
// container-mode and host-mode restore paths without touching the
// process-wide config.Load() singleton cache.
func withRestoreConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	prev := loadRestoreConfig
	loadRestoreConfig = func() config.Config { return cfg }
	t.Cleanup(func() { loadRestoreConfig = prev })
}

// withCreateSessionHook installs onRestoreSessionCreate for the duration of the
// test and restores it to nil on cleanup. The hook is called with the opts
// snapshot just before session.Create is invoked inside restoreProjectSession.
func withCreateSessionHook(t *testing.T, fn func(opts session.Opts)) {
	t.Helper()
	prev := onRestoreSessionCreate
	onRestoreSessionCreate = fn
	t.Cleanup(func() { onRestoreSessionCreate = prev })
}

// stubNvimOnPath writes a no-op `nvim` shim (`#!/bin/sh\nexit 0\n`) into a
// fresh t.TempDir() and prepends that directory to PATH for the duration of
// the test. setupFullLayout (internal/session/session.go) unconditionally
// sends NvimCmd into the session's edit window via a real tmux.SendKeys —
// against the real tmux server these restore tests drive (cmdTestServer),
// that really execs the `nvim` binary. That real, unmanaged nvim process is
// the writer of `.local/state/nvim` under the test's fake HOME (issue
// #2719): it can still be writing its state dir when a later t.TempDir()
// cleanup for HOME runs RemoveAll, racing on "directory not empty". These
// tests only assert on the agent window (window 1), so the edit window's
// content is never under test — stubbing nvim removes the writer instead of
// waiting on or hiding the race.
func stubNvimOnPath(t *testing.T) {
	t.Helper()
	nvimBinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nvimBinDir, "nvim"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake nvim: %v", err)
	}
	t.Setenv("PATH", nvimBinDir+":"+os.Getenv("PATH"))
}

// agentPaneStartCmd reads #{pane_start_command} from the agent window (window 1)
// of the named session. The agent window is created with "new-window ... sh -c
// <cmd>", so the command is embedded in the pane's start command synchronously
// at window creation — no polling is required. Returns the start command string
// so callers can assert substrings.
func agentPaneStartCmd(t *testing.T, s *cmdTestServer, sessionName string) string {
	t.Helper()
	out, err := s.output("display-message", "-t", sessionName+":1", "-p", "#{pane_start_command}")
	if err != nil {
		t.Fatalf("display-message pane_start_command for %q: %v", sessionName, err)
	}
	return out
}

// callRestoreSession is a test helper that wraps restoreSession with sensible
// defaults (no stagger) so that existing tests don't need to be updated every
// time the internal signature changes.
//
// Also ensures `loadRestoreConfig` returns a config with a non-empty
// PIExtensionDir if no test has already overridden it — the host-mode pi
// launch path enforces the #2065 fail-fast guard
// (session.ValidatePILaunchOpts) and would otherwise reject every restore
// test that exercises LayoutFull. Tests that explicitly override
// `loadRestoreConfig` via `withRestoreConfig` and want the empty-
// PIExtensionDir failure path must set the field themselves (currently
// none of them do).
func callRestoreSession(d *db.DB, s db.Status) error {
	cfg := loadRestoreConfig()
	if cfg.PIExtensionDir == "" {
		cfg.PIExtensionDir = "/test/prism-pi-extension"
		prev := loadRestoreConfig
		loadRestoreConfig = func() config.Config { return cfg }
		defer func() { loadRestoreConfig = prev }()
	}
	pending := false
	_, err := restoreSession(d, s, &pending, 0)
	return err
}

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
func seedStatus(t *testing.T, d *db.DB, sessionName, worktree string, harnessSessionID *string) db.Status {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "testrepo", worktree, "idle", nil, harnessSessionID); err != nil {
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
	skipRestoreOnGHA(t)
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

	if err := callRestoreSession(d, status); err != nil {
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
	skipRestoreOnGHA(t)
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

	if err := callRestoreSession(d, status); err != nil {
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

	if err := callRestoreSession(d, status); err != nil {
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
	if err := callRestoreSession(d, status); err != nil {
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

	if err := callRestoreSession(d, status); err != nil {
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

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession with empty worktree returned error: %v", err)
	}

	if s.hasSession(sessionName) {
		t.Errorf("session %q was created despite empty worktree — should be marked ended", sessionName)
	}

	if !isEnded(t, d, sessionName) {
		t.Errorf("session %q not marked ended in DB after empty-worktree restore", sessionName)
	}
}

// TestRestoreSession_AllThreeWindows is a table-driven test confirming that
// all three windows (edit/agent/term) are always created in the right order,
// regardless of the session name format.
func TestRestoreSession_AllThreeWindows(t *testing.T) {
	skipRestoreOnGHA(t)
	// Uses withCmdServer — must not run in parallel.
	// Redirect XDG_STATE_HOME so StartSidecar writes its PID file to an
	// isolated temp dir rather than the production ~/.local/state/prism/.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// PRISM_CMD_TEST_STUB=1 makes the test binary exit immediately when
	// re-invoked as a sidecar subprocess, preventing it from calling tmux
	// commands (e.g. has-session scratchpad) against the live tmux server.
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
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

		if err := callRestoreSession(d, status); err != nil {
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

// TestRestoreSession_HostModeOverride verifies that when a session was
// explicitly spawned in host mode (isolation_mode="host" in agent_status),
// restore preserves that mode even when cfg.DefaultIsolationMode is "bwrap".
// The agent pane must run "pi --agent ..." rather than using bwrap.
func TestRestoreSession_HostModeOverride(t *testing.T) {
	skipRestoreOnGHA(t)
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Enable bwrap mode globally — the test verifies the per-session
	// isolation_mode still overrides it.
	withRestoreConfig(t, config.Config{DefaultIsolationMode: config.IsolationBwrap})

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@host-mode"
	// Seed the row first, then mark it as host isolation_mode. Re-read so the
	// Status passed to restoreSession has the correct IsolationMode value.
	_ = seedStatus(t, d, sessionName, worktreeDir, nil)
	if err := d.SetIsolationMode(sessionName, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	var status db.Status
	for _, st := range statuses {
		if st.SessionName == sessionName {
			status = st
			break
		}
	}
	if status.IsolationMode != "host" {
		t.Fatalf("seeded status IsolationMode = %q, want \"host\"", status.IsolationMode)
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// Agent pane start command must contain "pi" but NOT "bwrap".
	// For a non-worktree path (no .bare parent), no --agent flag is added,
	// but the command is still pi directly (host mode).
	pane := agentPaneStartCmd(t, s, sessionName)
	if !strings.Contains(pane, "pi") {
		t.Errorf("agent pane missing .pi. — captured:\n%s", pane)
	}
	if strings.Contains(pane, "podman attach") {
		t.Errorf("agent pane contains 'podman attach' but should be in host mode — captured:\n%s", pane)
	}
}

// TestRestoreSession_KillsStaleSidecarPID verifies that restore calls
// KillSidecar before creating the new session, so that any orphaned PID
// file from a previous lifecycle (e.g. a reboot without clean shutdown)
// is cleared and StartSidecarWithOpts can write a fresh one.
//
// The test writes a fake stale PID file at the path KillSidecar would
// look at, then invokes restore. After restore, the stale PID file must
// have been removed.
func TestRestoreSession_KillsStaleSidecarPID(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@stale-pid"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	// Pre-create a stale sidecar PID file at the path KillSidecar will
	// look at. Use PID 1 so any /proc/1/cmdline check finds a non-prism
	// process and the kill is skipped, leaving the file-removal path to
	// handle cleanup.
	pidPath, err := session.SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	// The stale PID file must have been removed by KillSidecar. The new
	// sidecar that StartSidecarWithOpts tries to launch will itself write
	// a fresh PID file, but the stale content ("1") must be gone —
	// verified indirectly by checking that either the file is absent or
	// the content no longer reads "1".
	//
	// In CI environments where the sidecar binary is not installed,
	// StartSidecarWithOpts fails and does not write a new PID file, so
	// the file will be absent. In environments where it succeeds, the
	// file will contain a different PID. Either outcome is acceptable:
	// the stale PID must not be present.
	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid := strings.TrimSpace(string(data))
		if pid == "1" {
			t.Errorf("stale PID file still contains %q after restore — KillSidecar not called", pid)
		}
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error reading PID file: %v", err)
	}

	// Sanity: the session should have been created.
	if !s.hasSession(sessionName) {
		t.Errorf("session %q was not created", sessionName)
	}
}

// TestStaggerDelay_DefaultApplied verifies that the default stagger delay
// (RestoreStaggerDelayMs == 0) returns the compiled-in default of 500ms.
func TestStaggerDelay_DefaultApplied(t *testing.T) {
	cfg := config.Config{} // zero value — RestoreStaggerDelayMs == 0
	d := cfg.RestoreStaggerDelay()
	want := time.Duration(config.DefaultRestoreStaggerDelay) * time.Millisecond
	if d != want {
		t.Errorf("RestoreStaggerDelay() with zero value = %v, want %v", d, want)
	}
}

// TestStaggerDelay_NegativeDisables verifies that a negative RestoreStaggerDelayMs
// disables the stagger (returns 0 duration).
func TestStaggerDelay_NegativeDisables(t *testing.T) {
	cfg := config.Config{RestoreStaggerDelayMs: -1}
	d := cfg.RestoreStaggerDelay()
	if d != 0 {
		t.Errorf("RestoreStaggerDelay() with -1ms = %v, want 0 (disabled)", d)
	}
}

// TestStaggerDelay_CustomValue verifies that a positive RestoreStaggerDelayMs
// is returned correctly as a time.Duration.
func TestStaggerDelay_CustomValue(t *testing.T) {
	cfg := config.Config{RestoreStaggerDelayMs: 250}
	d := cfg.RestoreStaggerDelay()
	want := 250 * time.Millisecond
	if d != want {
		t.Errorf("RestoreStaggerDelay() with 250ms = %v, want %v", d, want)
	}
}

// ─── restore-attempts-on-prior-failure tests (issue #2315) ───────────────
//
// The circuit breaker was removed in #2315. These tests pin the new
// behaviour: restoreSession must attempt session.Create for every session
// that does not already have a live tmux session and whose worktree directory
// exists, regardless of how many consecutive non-finished terminal
// state_change events its history carries.

// writeRestoreStateChange is a test helper that inserts a state_change event
// for the given session with the given state value into the DB. Used by the
// #2315 restore-attempts-on-prior-failure tests below.
func writeRestoreStateChange(t *testing.T, d *db.DB, sessionName, state string) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "testrepo",
		Worktree:    "/tmp/wt",
		Type:        "state_change",
		Payload:     `{"state":"` + state + `"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent state_change(%s): %v", state, err)
	}
}

// TestRestoreSession_PriorErrorEvents_StillAttempted verifies that a session
// whose history contains 3+ consecutive `state_change: error` events is still
// attempted by restoreSession (no longer skipped). Pre-#2315, the circuit
// breaker would have skipped this session with
// "skipped (circuit breaker): ...".
func TestRestoreSession_PriorErrorEvents_StillAttempted(t *testing.T) {
	skipRestoreOnGHA(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "repo@error-history"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	// Write 5 consecutive `state_change: error` events (more than the legacy
	// breaker threshold of 3). Pre-#2315 this would have been skipped.
	for i := 0; i < 5; i++ {
		writeRestoreStateChange(t, d, sessionName, "error")
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created despite prior error history — circuit-breaker removal regressed", sessionName)
	}

	// The agent_status row must remain active (NOT marked ended). The
	// circuit-breaker code path deliberately left rows active so the user
	// could intervene; the new always-attempt path must preserve that.
	if isEnded(t, d, sessionName) {
		t.Error("session was marked ended despite a successful restore attempt")
	}
}

// TestRestoreSession_PriorInterruptedEvents_StillAttempted verifies that a
// session whose history contains 3+ consecutive `state_change: interrupted`
// events (e.g. from repeated SIGTERM-on-reboot) is still attempted by
// restoreSession. This is the exact failure mode #2315 was filed to address:
// the breaker's query treated `interrupted` (clean SIGTERM at shutdown)
// identically to `error`, so three reboots in a row would lock the session
// out of restore.
func TestRestoreSession_PriorInterruptedEvents_StillAttempted(t *testing.T) {
	skipRestoreOnGHA(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "repo@interrupted-history"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	// Simulate three reboots' worth of SIGTERM-triggered shutdowns.
	for i := 0; i < 3; i++ {
		writeRestoreStateChange(t, d, sessionName, "interrupted")
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created despite prior interrupted history — SIGTERM-on-reboot lockout regressed", sessionName)
	}
}

// TestRestoreSession_HostMode_AppendsSessionFlagWhenFileExists is the
// end-to-end host-mode regression guard for issue #1838 / AC8(d). It seeds
// an agent_status row with a non-NULL harness_session_id, writes a matching
// pi session JSONL under ~/.pi/agent/sessions/<encoded-cwd>/, runs restore,
// and asserts that the resulting tmux agent pane start command contains
// `pi ... --session '<id>'`. This is the load-bearing assertion: if the
// HarnessSessionID plumbing breaks anywhere between agent_status and the
// final tmux launch command, the substring will be absent and this test
// fails.
//
// Host mode (rather than bwrap/sandbox-exec) is chosen because the host
// branch is the one the launch-command path actually exercises —
// container-mode panes run `prism agent-run`, which reads HarnessSessionID
// straight from the DB row inside that subprocess (covered by the
// PIInvocation tests in internal/container/pi_invocation_resume_test.go and
// by the obvious-by-inspection plumbing in cmd/agent_run.go +
// cmd/agent_run_sandbox_exec_darwin.go).
func TestRestoreSession_HostMode_AppendsSessionFlagWhenFileExists(t *testing.T) {
	skipRestoreOnGHA(t)
	// Uses withCmdServer — must not run in parallel.
	// Redirect both XDG_STATE_HOME (prism per-session dirs) and HOME (pi
	// sessions root in host mode) so the test never touches real state.
	// Also clear PI_CODING_AGENT_DIR so the resolver exercises the
	// home-fallback branch deterministically (the developer host sets that
	// env var system-wide; post-#2185 the resolver honours it).
	clearPICodingAgentDir(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// restoreSession drives a REAL tmux server (cmdTestServer), so
	// setupFullLayout's `tmux.SendKeys(name+":0", NvimCmd(directory))`
	// (internal/session/session.go) doesn't just record a string — it
	// types the command into a live shell in a live pane, which really
	// execs the `nvim` binary. That real nvim process is the writer of
	// `.local/state/nvim` under $HOME (issue #2719): it is a background
	// process the test neither owns nor waits on, so it can still be
	// writing its state dir when a later t.TempDir() cleanup (for this
	// fakeHome) runs RemoveAll, racing on "directory not empty". Stub
	// `nvim` on PATH with a no-op shim so window 0 never launches a real
	// editor and there is no writer left to race — this test only
	// asserts on window 1 (the agent pane), so the edit window's content
	// is not under test.
	stubNvimOnPath(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Force host isolation on the restore path so the pane runs the direct
	// `pi ...` command rather than `prism agent-run`.
	withRestoreConfig(t, config.Config{DefaultIsolationMode: config.IsolationHost})

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@host-resume"

	const harnessSessionID = "019e00ed-1234-7890-abcd-ef0123456789"
	hsid := harnessSessionID
	_ = seedStatus(t, d, sessionName, worktreeDir, &hsid)
	if err := d.SetIsolationMode(sessionName, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	var status db.Status
	for _, st := range statuses {
		if st.SessionName == sessionName {
			status = st
			break
		}
	}

	// Write a synthetic pi session JSONL under the host-mode sessions root
	// so ResolvePIResumeSession finds it. The encoded-cwd formula matches
	// pi's own session-manager naming.
	encoded := encodePiCWDForTest(worktreeDir)
	sessionDir := filepath.Join(fakeHome, ".pi", "agent", "sessions", encoded)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir host-mode sessions dir: %v", err)
	}
	sessionFile := filepath.Join(sessionDir, "2026-01-02T03-04-05-000Z_"+harnessSessionID+".jsonl")
	if err := os.WriteFile(sessionFile, []byte("{\"type\":\"session\"}\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// The agent pane's start command must contain `--session '<id>'`.
	pane := agentPaneStartCmd(t, s, sessionName)
	want := "--session '" + harnessSessionID + "'"
	if !strings.Contains(pane, want) {
		t.Errorf("agent pane start command missing %q\ncaptured: %s", want, pane)
	}
}

// TestRestoreSession_HostMode_NoSessionFlag_WhenFileMissing exercises AC4 /
// AC5 on the host-mode launch path: when the agent_status row carries a
// HarnessSessionID but no matching pi JSONL exists on disk,
// buildDirectAgentCmd must omit --session and pi must start a fresh
// conversation. The negative assertion proves the test from
// TestRestoreSession_HostMode_AppendsSessionFlagWhenFileExists isn't a
// no-op (i.e. the --session token doesn't sneak in unconditionally).
func TestRestoreSession_HostMode_NoSessionFlag_WhenFileMissing(t *testing.T) {
	skipRestoreOnGHA(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Empty fake HOME — no pi sessions dir present, so the resolver
	// must fail and the launcher must omit --session.
	t.Setenv("HOME", t.TempDir())

	// Same real-tmux setup as TestRestoreSession_HostMode_AppendsSessionFlagWhenFileExists
	// (config.IsolationHost + real tmux via callRestoreSession ->
	// setupFullLayout -> NvimCmd), so it carries the identical
	// .local/state/nvim teardown race (issue #2719). Stub nvim here too —
	// see stubNvimOnPath's doc comment for the full writer identification.
	stubNvimOnPath(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	withRestoreConfig(t, config.Config{DefaultIsolationMode: config.IsolationHost})

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@host-resume-miss"

	const harnessSessionID = "019e00ed-aaaa-bbbb-cccc-deadbeef0000"
	hsid := harnessSessionID
	_ = seedStatus(t, d, sessionName, worktreeDir, &hsid)
	if err := d.SetIsolationMode(sessionName, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	var status db.Status
	for _, st := range statuses {
		if st.SessionName == sessionName {
			status = st
			break
		}
	}

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	pane := agentPaneStartCmd(t, s, sessionName)
	if strings.Contains(pane, "--session") {
		t.Errorf("agent pane start command unexpectedly contains --session when no JSONL exists\ncaptured: %s", pane)
	}
}

// encodePiCWDForTest mirrors internal/container.encodePiCWD /
// internal/harness/pi.EncodePiCWD. Duplicated locally so the restore test
// stays in the cmd package without an internal-package dependency.
func encodePiCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
}
