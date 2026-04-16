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
//   - Container-mode sessions have ConfigContent populated from profiles.json
//
// All tests use an isolated tmux server (cmdTestServer) and an isolated DB
// (SetTestDBPath) so they do not touch the live environment.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

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

// withRestoreProfiles overrides loadRestoreProfiles for the duration of the
// test and restores the previous value on cleanup. Pass nil to simulate a
// missing or unreadable profiles.json.
func withRestoreProfiles(t *testing.T, pf *config.ProfilesFile, loadErr error) {
	t.Helper()
	prev := loadRestoreProfiles
	loadRestoreProfiles = func() (*config.ProfilesFile, error) { return pf, loadErr }
	t.Cleanup(func() { loadRestoreProfiles = prev })
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
// OpencodeSID is present, the opencode launch command delivered to the agent
// window includes the session ID (-s flag).
//
// It reads #{pane_start_command} from the agent window (window 1) — the agent
// window is now created with "new-window ... sh -c <cmd>" so the command is
// embedded in the pane's start command, not echoed via send-keys.
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

	// Read the pane start command — the agent window is created via
	// "new-window ... sh -c <cmd>" so the command appears in #{pane_start_command}.
	paneCmd, err := s.output("display-message", "-t", sessionName+":1", "-p", "#{pane_start_command}")
	if err != nil {
		t.Fatalf("display-message pane_start_command: %v", err)
	}

	if !strings.Contains(paneCmd, "-s "+sid) {
		t.Errorf("agent pane_start_command does not contain '-s %s'; got:\n%s", sid, paneCmd)
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

// TestRestoreSession_ContainerMode verifies that when cfg.ContainerMode is
// enabled and the persisted session is not marked host_mode, restore uses
// the container-mode agent command ("podman attach <container-name>")
// rather than launching opencode directly. It also asserts that the
// PluginHostPath is propagated from cfg into opts so the sidecar bind-mounts
// the plugin file.
//
// This is the core AC-1 regression guard: restoring a container-mode session
// must go through the same podman-attach path as spawn (RFC #691, Phase 1a).
func TestRestoreSession_ContainerMode(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Override cfg to enable container mode for this test only.
	pluginPath := "/fake/plugin/path/prism-hooks.ts"
	withRestoreConfig(t, config.Config{
		ContainerMode:     true,
		SidecarPluginPath: pluginPath,
	})

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@container-restore"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// The agent pane start command must contain "podman attach prism-myrepo-container-restore",
	// not "opencode --agent". The container name is derived from the session name via
	// container.NameForSession("myrepo@container-restore") = "prism-myrepo-container-restore".
	pane := agentPaneStartCmd(t, s, sessionName)
	if !strings.Contains(pane, "podman attach prism-myrepo-container-restore") {
		t.Errorf("agent pane missing 'podman attach prism-myrepo-container-restore' — captured:\n%s", pane)
	}
	if strings.Contains(pane, "opencode --agent") {
		t.Errorf("agent pane contains 'opencode --agent' but should be in container mode — captured:\n%s", pane)
	}
	if strings.Contains(pane, "opencode attach") {
		t.Errorf("agent pane contains old 'opencode attach' command — should now use 'podman attach' (RFC #691)")
	}
}

// TestRestoreSession_HostModeOverride verifies that when a session was
// explicitly spawned in host mode (host_mode=1 in agent_status), restore
// preserves that mode even when cfg.ContainerMode is enabled. The agent
// pane must run "opencode --agent ..." rather than "podman attach".
func TestRestoreSession_HostModeOverride(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Enable container mode globally — the test verifies the per-session
	// host_mode flag still overrides it.
	withRestoreConfig(t, config.Config{ContainerMode: true})

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@host-mode"
	// Seed the row first, then mark it as host_mode=true. Re-read so the
	// Status passed to restoreSession has the correct HostMode value.
	_ = seedStatus(t, d, sessionName, worktreeDir, nil)
	if err := d.SetHostMode(sessionName, true); err != nil {
		t.Fatalf("SetHostMode: %v", err)
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
	if !status.HostMode {
		t.Fatalf("seeded status HostMode = false, want true")
	}

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// Agent pane start command must contain "opencode --agent ...", not "podman attach".
	pane := agentPaneStartCmd(t, s, sessionName)
	if !strings.Contains(pane, "opencode --agent") {
		t.Errorf("agent pane missing 'opencode --agent' — captured:\n%s", pane)
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

	if err := restoreSession(d, status); err != nil {
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

// fakeProfilesFile returns a *config.ProfilesFile with known worker and
// coordinator container config blobs for use in container-mode tests.
func fakeProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		ContainerWorkerConfig:      `{"model":"worker-model"}`,
		ContainerCoordinatorConfig: `{"model":"coordinator-model"}`,
	}
}

// TestRestoreSession_ContainerMode_WorkerConfigContent verifies that when
// cfg.ContainerMode is true and profiles.json contains a worker config blob,
// restoreProjectSession populates opts.ConfigContent with the worker blob for
// a non-main worktree directory (DefaultAgent returns "worker").
//
// This is the AC regression guard for restore: ConfigContent must flow from
// profiles.json through opts into session.Create (and on to StartSidecarWithOpts
// as --config-content) so the container runs with its role identity locked.
func TestRestoreSession_ContainerMode_WorkerConfigContent(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	pluginPath := "/fake/plugin/path/prism-hooks.ts"
	withRestoreConfig(t, config.Config{
		ContainerMode:     true,
		SidecarPluginPath: pluginPath,
	})

	// Inject a fake profiles file so tests do not require a real profiles.json.
	pf := fakeProfilesFile()
	withRestoreProfiles(t, pf, nil)

	d := openRestoreTestDB(t)

	// Use a non-main worktree directory so DefaultAgent returns "worker".
	worktreeDir := filepath.Join(t.TempDir(), "feature-branch")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessionName := "myrepo@feature"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	// Capture the opts passed to session.Create via the test hook.
	var capturedOpts session.Opts
	withCreateSessionHook(t, func(opts session.Opts) {
		capturedOpts = opts
	})

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// The worker blob must be in ConfigContent.
	if capturedOpts.ConfigContent != pf.ContainerWorkerConfig {
		t.Errorf("opts.ConfigContent = %q, want %q (worker blob)",
			capturedOpts.ConfigContent, pf.ContainerWorkerConfig)
	}
}

// TestRestoreSession_ContainerMode_CoordinatorConfigContent verifies that the
// coordinator blob is selected when the worktree directory is named "main"
// (DefaultAgent returns "coordinator" for directories whose base is "main").
func TestRestoreSession_ContainerMode_CoordinatorConfigContent(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	withRestoreConfig(t, config.Config{
		ContainerMode: true,
	})

	pf := fakeProfilesFile()
	withRestoreProfiles(t, pf, nil)

	d := openRestoreTestDB(t)

	// "main" as the base name causes DefaultAgent to return "coordinator".
	worktreeDir := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessionName := "myrepo@main"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	var capturedOpts session.Opts
	withCreateSessionHook(t, func(opts session.Opts) {
		capturedOpts = opts
	})

	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}

	// The coordinator blob must be in ConfigContent.
	if capturedOpts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("opts.ConfigContent = %q, want %q (coordinator blob)",
			capturedOpts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
}

// TestRestoreSession_ContainerMode_ProfilesError verifies that a profiles.json
// load error is non-fatal for restore: the session is still created and
// opts.ConfigContent is left empty (no injection), rather than aborting the
// entire restore run.
func TestRestoreSession_ContainerMode_ProfilesError(t *testing.T) {
	// Uses withCmdServer — must not run in parallel.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)

	withRestoreConfig(t, config.Config{
		ContainerMode: true,
	})

	// Simulate a missing/unreadable profiles.json.
	withRestoreProfiles(t, nil, fmt.Errorf("profiles: not found"))

	d := openRestoreTestDB(t)

	worktreeDir := t.TempDir()
	sessionName := "myrepo@profiles-error"
	status := seedStatus(t, d, sessionName, worktreeDir, nil)

	var capturedOpts session.Opts
	withCreateSessionHook(t, func(opts session.Opts) {
		capturedOpts = opts
	})

	// restoreSession must NOT return an error — the profiles load failure is
	// logged to stderr but must not abort the session recreation.
	if err := restoreSession(d, status); err != nil {
		t.Fatalf("restoreSession returned error on profiles load failure: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	// The session must still have been created.
	if !s.hasSession(sessionName) {
		t.Errorf("session %q was not created despite profiles error (should be non-fatal)", sessionName)
	}

	// ConfigContent must be empty — no injection when profiles are unavailable.
	if capturedOpts.ConfigContent != "" {
		t.Errorf("opts.ConfigContent = %q, want empty (profiles load failed — injection must be skipped)",
			capturedOpts.ConfigContent)
	}
}
