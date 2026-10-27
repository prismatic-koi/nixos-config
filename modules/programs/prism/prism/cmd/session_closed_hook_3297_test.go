package cmd

// End-to-end tests of the global session-closed hook (#3297). Each test
// installs the hook line of modules/programs/prism/tmux.nix on a test tmux
// server, with a test-built prism binary in place of ${prism}. The hook
// then runs the real `prism event tmux-session-end` against the test
// database.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// sessionClosedHookLine returns the line of tmux.nix that registers the
// session-closed hook. The nix build copies only the prism Go module, so
// the test skips there. Everywhere else the file must exist.
func sessionClosedHookLine(t *testing.T) string {
	t.Helper()
	if os.Getenv("NIX_BUILD_TOP") != "" {
		t.Skip("skipping inside the nix build sandbox (NIX_BUILD_TOP is set): tmux.nix is outside the prism Go module")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "tmux.nix"))
	if err != nil {
		t.Fatalf("read tmux.nix: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "set-hook -g session-closed ") {
			return line
		}
	}
	t.Fatal("tmux.nix has no `set-hook -g session-closed` line")
	return ""
}

// installSessionClosedHook installs the hook line of tmux.nix on s. A
// wrapper runs bin in place of ${prism} and appends "<args> rc=<status>" to
// the returned log file when bin exits.
func installSessionClosedHook(t *testing.T, s *cmdTestServer, bin, root string) string {
	t.Helper()
	line := sessionClosedHookLine(t)
	if !strings.Contains(line, "${prism}") {
		t.Fatalf("hook line %q has no ${prism}", line)
	}
	logFile := filepath.Join(root, "hook.log")
	wrapper := filepath.Join(root, "bin", "prism-hook")
	script := "#!/bin/sh\n" +
		bin + " \"$@\" 2>>" + logFile + ".stderr\n" +
		"rc=$?\n" +
		"printf '%s rc=%s\\n' \"$*\" \"$rc\" >> " + logFile + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write hook wrapper: %v", err)
	}
	conf := filepath.Join(root, "hook.conf")
	if err := os.WriteFile(conf, []byte(strings.ReplaceAll(line, "${prism}", wrapper)+"\n"), 0o600); err != nil {
		t.Fatalf("write hook conf: %v", err)
	}
	if err := s.run("source-file", conf); err != nil {
		t.Fatalf("source the hook line: %v", err)
	}
	return logFile
}

// waitHookRun waits until the hook log has a line for a run with
// `--session <name>`, and returns that line. A name that ends with "*"
// matches each name with that prefix.
func waitHookRun(t *testing.T, logFile, name string) string {
	t.Helper()
	want := "--session " + strings.TrimSuffix(name, "*")
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, _ := os.ReadFile(logFile)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, want) && (strings.HasSuffix(name, "*") || strings.Contains(line, want+" rc=")) {
				return line
			}
		}
		if time.Now().After(deadline) {
			stderr, _ := os.ReadFile(logFile + ".stderr")
			t.Fatalf("no hook run with %q in 15s. hook log:\n%s\nhook stderr:\n%s", want, b, stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// openHookTestDB opens the database of the test-built binary.
func openHookTestDB(t *testing.T, root string) *db.DB {
	t.Helper()
	d, err := db.Open(rootDBPath(root))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedHookSession writes an open agent_status row and an open sessions row
// for name, and returns the instance ID.
func seedHookSession(t *testing.T, d *db.DB, name string) string {
	t.Helper()
	if err := d.UpsertStatus(name, "myrepo", "/code/myrepo/branch", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", name, err)
	}
	iid := uuid.New().String()
	if err := d.SetInstanceID(name, iid); err != nil {
		t.Fatalf("SetInstanceID(%q): %v", name, err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: name, Repo: "myrepo", Worktree: "/code/myrepo/branch", Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession(%q): %v", name, err)
	}
	return iid
}

// requireRowsOpen fails t unless the agent_status row of name is open and
// keeps iid, and the sessions row of iid is open.
func requireRowsOpen(t *testing.T, d *db.DB, name, iid string) {
	t.Helper()
	st, err := d.CurrentStatus(name)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", name, st, err)
	}
	if st.EndedAt != nil {
		t.Errorf("agent_status row of %q ended at %v, want open", name, st.EndedAt)
	}
	if st.InstanceID == nil || *st.InstanceID != iid {
		t.Errorf("agent_status.instance_id of %q = %v, want %q", name, st.InstanceID, iid)
	}
	if row := sessionRow(t, d, iid); row == nil || row.EndedAt != nil {
		t.Errorf("sessions row of %q = %+v, want open", name, row)
	}
}

// A user `tmux kill-session` closes a session outside prism while other
// sessions exist. The hook must get the name of the closed session and end
// its rows. If a sidecar stop ended the sessions row before the close, the
// hook keeps the ended_at of that stop, so MergesAbandonedByStop still
// finds the merge-queue rows of the stop.
func TestSessionClosedHook_OutsideKillEndsClosedSession_3297(t *testing.T) {
	bin, root := isolatedPrismBinary(t)
	s := newCmdTestServer(t)
	logFile := installSessionClosedHook(t, s, bin, root)

	const (
		closed  = "prism-test@closed"
		stopped = "prism-test@stopped"
		live    = "prism-test@live"
	)
	d := openHookTestDB(t, root)
	closedIID := seedHookSession(t, d, closed)
	stoppedIID := seedHookSession(t, d, stopped)
	liveIID := seedHookSession(t, d, live)
	for _, pr := range []int{82, 81} {
		if _, err := d.EnqueueMerge(pr, "myrepo", stopped, stoppedIID, nil); err != nil {
			t.Fatalf("EnqueueMerge(%d): %v", pr, err)
		}
	}
	if err := d.EndIncarnation(stoppedIID, "finished"); err != nil {
		t.Fatalf("EndIncarnation: %v", err)
	}
	stopEndedAt := sessionRow(t, d, stoppedIID).EndedAt
	for _, n := range []string{closed, stopped, live} {
		s.newSession(n)
	}

	for _, n := range []string{closed, stopped} {
		if err := s.run("kill-session", "-t", "="+n); err != nil {
			t.Fatalf("kill-session %q: %v", n, err)
		}
		if line := waitHookRun(t, logFile, n); !strings.HasSuffix(line, " rc=0") {
			t.Errorf("hook run %q, want exit status 0", line)
		}
	}

	b, _ := os.ReadFile(logFile)
	for _, n := range []string{live, "bootstrap"} {
		if strings.Contains(string(b), "--session "+n+" ") {
			t.Errorf("the hook got the live session %q. hook log:\n%s", n, b)
		}
	}

	for _, tc := range []struct{ name, iid string }{{closed, closedIID}, {stopped, stoppedIID}} {
		st, err := d.CurrentStatus(tc.name)
		if err != nil || st == nil {
			t.Fatalf("CurrentStatus(%q) = %v, %v", tc.name, st, err)
		}
		if st.EndedAt == nil {
			t.Errorf("agent_status row of %q is open, want ended", tc.name)
		}
		if st.InstanceID != nil {
			t.Errorf("agent_status.instance_id of %q = %q, want cleared", tc.name, *st.InstanceID)
		}
		row := sessionRow(t, d, tc.iid)
		if row == nil || row.EndedAt == nil || row.EndState == nil || *row.EndState != "finished" {
			t.Errorf("sessions row of %q = %+v, want ended with end_state finished", tc.name, row)
		}
	}
	causes, err := d.SessionEndCauses([]string{closed})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if !causes[closed].TmuxSessionEnded {
		t.Errorf("no tmux_session_end event for %q", closed)
	}

	if got := sessionRow(t, d, stoppedIID).EndedAt; got == nil || !got.Equal(*stopEndedAt) {
		t.Errorf("sessions.ended_at of %q = %v, want the ended_at of the stop %v", stopped, got, stopEndedAt)
	}
	rows, err := d.MergesAbandonedByStop(stopped, stoppedIID)
	if err != nil {
		t.Fatalf("MergesAbandonedByStop: %v", err)
	}
	if len(rows) != 2 || rows[0].PR != 82 || rows[1].PR != 81 {
		t.Errorf("MergesAbandonedByStop = %v, want PR #82, PR #81", rows)
	}

	requireRowsOpen(t, d, live, liveIID)
}

// kill-server closes every session. `prism restore` resumes each session
// with an open row, so the hook must not end one. The exiting server
// refuses new clients while the hook runs, so the presence check of the
// hook cannot query tmux, and the hook keeps the row.
//
// tmux runs the hook for the first session in name order only. The name
// of the session under test sorts before "bootstrap", so the hook runs
// for it.
func TestSessionClosedHook_KillServerKeepsRows_3297(t *testing.T) {
	bin, root := isolatedPrismBinary(t)
	s := newCmdTestServer(t)
	logFile := installSessionClosedHook(t, s, bin, root)

	const restored = "a-prism-test@restore"
	d := openHookTestDB(t, root)
	iid := seedHookSession(t, d, restored)
	s.newSession(restored)

	if err := s.run("kill-server"); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	if line := waitHookRun(t, logFile, restored); !strings.HasSuffix(line, " rc=0") {
		t.Errorf("hook run %q, want exit status 0", line)
	}

	requireRowsOpen(t, d, restored, iid)
}

// A prism path kills a tmux session with tmux.KillSession and ends the
// rows itself, or starts a new session with the same name. The hook must
// not act on the rows of that session.
func TestSessionClosedHook_PrismKillLeavesRows_3297(t *testing.T) {
	bin, root := isolatedPrismBinary(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	logFile := installSessionClosedHook(t, s, bin, root)

	const name = "prism-test@prism-kill"
	d := openHookTestDB(t, root)
	iid := seedHookSession(t, d, name)
	s.newSession(name)

	if err := tmux.KillSession(name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	line := waitHookRun(t, logFile, "*")
	if !strings.Contains(line, "--session "+tmux.ClosingSessionPrefix) || !strings.HasSuffix(line, " rc=0") {
		t.Errorf("hook run %q, want a %s name and exit status 0", line, tmux.ClosingSessionPrefix)
	}
	if s.hasSession(name) {
		t.Errorf("session %q is still present after KillSession", name)
	}

	requireRowsOpen(t, d, name, iid)
	causes, err := d.SessionEndCauses([]string{name})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[name].TmuxSessionEnded {
		t.Errorf("tmux_session_end event for %q, want none", name)
	}
}
