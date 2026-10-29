package cmd

// Tests for issue #3345: prism restore --session restarts one live session,
// so the recovery of a dead agent pane restarts the sidecar with the agent.

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestRestoreOneSession_RestartsLiveSession restarts a session whose tmux
// session exists. A plain restore skips such a session. restore --session
// must close it, stop the old sidecar, and create the session again, with
// the row still live.
func TestRestoreOneSession_RestartsLiveSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubNvimOnPath(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	keepDeadPanes(s)
	withRestoreConfig(t, config.Config{PIExtensionDir: "/test/prism-pi-extension"})
	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	d := openRestoreTestDB(t)

	const sessionName = "testrepo@dead-pane-3345"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	t.Cleanup(func() { session.KillSidecar(sessionName) })
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	firstID, err := s.output("display-message", "-t", sessionName, "-p", "#{session_id}")
	if err != nil {
		t.Fatalf("read session id: %v", err)
	}

	var stopped []string
	prev := restoreStopSidecar
	restoreStopSidecar = func(name string, _ time.Duration) (int, bool, bool) {
		stopped = append(stopped, name)
		return 0, true, true
	}
	t.Cleanup(func() { restoreStopSidecar = prev })

	if err := restoreOneSession(d, sessionName, false); err != nil {
		t.Fatalf("restoreOneSession: %v", err)
	}

	if !s.hasSession(sessionName) {
		t.Fatalf("tmux session %q is missing after the restart", sessionName)
	}
	secondID, err := s.output("display-message", "-t", sessionName, "-p", "#{session_id}")
	if err != nil {
		t.Fatalf("read session id: %v", err)
	}
	if secondID == firstID {
		t.Errorf("tmux session id = %s before and after, want a new tmux session", firstID)
	}
	if len(stopped) != 1 || stopped[0] != sessionName {
		t.Errorf("sidecar stops = %v, want one stop of %q", stopped, sessionName)
	}
	if isEnded(t, d, sessionName) {
		t.Errorf("agent_status row of %q is ended, want it live after the restart", sessionName)
	}
	if got := windowNames(t, s, sessionName); strings.Join(got, ",") != "edit,agent,term" {
		t.Errorf("windows = %v, want edit, agent, term", got)
	}
}

// TestRestoreOneSession_RefusesSessionWithNoLiveRow checks that the restart
// does not start a session that prism.db does not hold as live, and does not
// touch its tmux session.
func TestRestoreOneSession_RefusesSessionWithNoLiveRow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	d := openRestoreTestDB(t)

	const ended = "testrepo@ended-3345"
	seedStatus(t, d, ended, t.TempDir(), nil)
	if err := d.SetEnded(ended); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	s.newSession(ended)

	for _, name := range []string{ended, "testrepo@unknown-3345"} {
		err := restoreOneSession(d, name, false)
		if err == nil || !strings.Contains(err.Error(), "no live session") {
			t.Errorf("restoreOneSession(%q) = %v, want a no-live-session error", name, err)
		}
	}
	if !s.hasSession(ended) {
		t.Errorf("tmux session %q was closed, want it untouched", ended)
	}
}

// TestRestoreOneSession_DryRunChangesNothing checks that --dry-run keeps the
// tmux session.
func TestRestoreOneSession_DryRunChangesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	d := openRestoreTestDB(t)

	const sessionName = "testrepo@dry-run-3345"
	seedStatus(t, d, sessionName, t.TempDir(), nil)
	s.newSession(sessionName)

	stdout, _ := captureStdoutStderr(t, func() {
		if err := restoreOneSession(d, sessionName, true); err != nil {
			t.Errorf("restoreOneSession dry run: %v", err)
		}
	})
	if !strings.Contains(stdout, "would restart: "+sessionName) {
		t.Errorf("stdout = %q, want the would-restart line", stdout)
	}
	if !s.hasSession(sessionName) {
		t.Errorf("tmux session %q was closed by a dry run", sessionName)
	}
}
