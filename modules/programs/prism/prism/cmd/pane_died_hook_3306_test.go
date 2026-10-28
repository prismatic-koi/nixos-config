package cmd

// End-to-end tests of the global pane-died hook (#3306). Each test sources
// the `remain-on-exit` line and the pane-died hook line of
// modules/programs/prism/tmux.nix on a test tmux server, with a test-built
// prism binary in place of ${prism}. The hook then runs the real
// `prism event pane-died` against the test database.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// installPaneDiedHook sources the remain-on-exit line and the pane-died
// hook line of tmux.nix on s, and returns the hook log file.
func installPaneDiedHook(t *testing.T, s *cmdTestServer, bin, root string) string {
	t.Helper()
	remain := tmuxNixLine(t, "set -g remain-on-exit ")
	if remain != "set -g remain-on-exit on" {
		t.Fatalf("tmux.nix has %q, want remain-on-exit on: these tests and the hook expect it", remain)
	}
	return installTmuxNixHook(t, s, bin, root, tmuxNixLine(t, "set-hook -g pane-died "), remain)
}

// paneDiedFixture is a test tmux server with the pane-died hook, and the
// test database of the binary that the hook runs.
type paneDiedFixture struct {
	s       *cmdTestServer
	d       *db.DB
	logFile string
	root    string
}

func newPaneDiedFixture(t *testing.T) *paneDiedFixture {
	t.Helper()
	bin, root := isolatedPrismBinary(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	return &paneDiedFixture{
		s:       s,
		d:       openHookTestDB(t, root),
		logFile: installPaneDiedHook(t, s, bin, root),
		root:    root,
	}
}

// handshake writes the state_change{active} event that the sidecar writes at
// the harness handshake. The row stays idle until the first turn.
func (f *paneDiedFixture) handshake(t *testing.T, name string) {
	t.Helper()
	if err := f.d.WriteEvent(db.Event{
		ID: uuid.New().String(), SessionName: name, Type: "state_change",
		Payload: `{"state":"active"}`, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent(%q): %v", name, err)
	}
}

// seed writes open rows for name in state, and returns the instance ID.
func (f *paneDiedFixture) seed(t *testing.T, name, state string) string {
	t.Helper()
	iid := seedHookSession(t, f.d, name)
	if state == "idle" {
		return iid
	}
	// Each state is a valid transition from active.
	for _, st := range []string{"active", state} {
		if err := f.d.UpsertStatus(name, "myrepo", "/code/myrepo/branch", st, nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q, %q): %v", name, st, err)
		}
	}
	return iid
}

// startAgent makes the tmux session name and its agent window, as
// setupFullLayout does: tmux.NewWindowPane, then tmux.MarkAgentPane. The
// agent process waits until release(name) is called, then runs exitCmd.
// startAgent returns the pane ID.
func (f *paneDiedFixture) startAgent(t *testing.T, name, exitCmd string) string {
	t.Helper()
	f.s.newSession(name)
	cmd := "while [ ! -e " + f.gate(name) + " ]; do sleep 0.05; done; " + exitCmd
	paneID, err := tmux.NewWindowPane(name, 1, "agent", "/tmp", cmd, nil)
	if err != nil {
		t.Fatalf("NewWindowPane(%q): %v", name, err)
	}
	if err := tmux.MarkAgentPane(paneID); err != nil {
		t.Fatalf("MarkAgentPane(%q): %v", paneID, err)
	}
	return paneID
}

// gate returns the file that lets the waiting process of name exit.
func (f *paneDiedFixture) gate(name string) string {
	return filepath.Join(f.root, "gate-"+strings.ReplaceAll(name, "@", "_"))
}

// release lets the waiting processes of name exit.
func (f *paneDiedFixture) release(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(f.gate(name), nil, 0o600); err != nil {
		t.Fatalf("release %q: %v", name, err)
	}
}

// waitRun waits until the hook log has a finished run for session name
// whose arguments contain want, and returns the line.
//
// tmux 3.4, the version of the CI runner, can miss the SIGCHLD of a pane
// process. The pane is then dead with no status, and tmux runs no pane-died
// hook until another child of the server exits. So each second with no run,
// waitRun starts a run-shell job that exits at once, and tmux reaps the pane.
func (f *paneDiedFixture) waitRun(t *testing.T, name, want string) string {
	t.Helper()
	prefix := "event pane-died --session=" + name + " "
	deadline := time.Now().Add(15 * time.Second)
	nextReap := time.Now().Add(time.Second)
	for {
		b, _ := os.ReadFile(f.logFile)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, prefix) && strings.Contains(line, want) && strings.Contains(line, " rc=") {
				return line
			}
		}
		if time.Now().After(deadline) {
			stderr, _ := os.ReadFile(f.logFile + ".stderr")
			t.Fatalf("no hook run for %q with %q in 15s. hook log:\n%s\nhook stderr:\n%s", name, want, b, stderr)
		}
		if time.Now().After(nextReap) {
			_ = f.s.run("run-shell", "-b", "true")
			nextReap = time.Now().Add(time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireState fails t unless the agent_status state of name is want and
// the row has exactly wantEvents state_change events with state
// "interrupted".
func (f *paneDiedFixture) requireState(t *testing.T, name, want string, wantEvents int) {
	t.Helper()
	st, err := f.d.CurrentStatus(name)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", name, st, err)
	}
	if st.State != want {
		t.Errorf("state of %q = %q, want %q", name, st.State, want)
	}
	evts, err := f.d.QueryEvents(name, 100, nil, nil, []string{"state_change"})
	if err != nil {
		t.Fatalf("QueryEvents(%q): %v", name, err)
	}
	got := 0
	for _, e := range evts {
		if strings.Contains(e.Payload, `"interrupted"`) {
			got++
		}
	}
	if got != wantEvents {
		t.Errorf("state_change events to interrupted for %q = %d, want %d", name, got, wantEvents)
	}
}

// An exit of the agent process runs the hook with the session, the window,
// and the exit status of the dead pane, also when other sessions exist. The
// hook sets the session to interrupted. A zero exit keeps "finished", and a
// non-zero exit or a signal overrides it. A row stays idle after the harness
// handshake until the first turn, for example a new coordinator before its
// first prompt and each restored session. An exit there also sets
// interrupted.
func TestPaneDiedHook_AgentExitSetsInterrupted_3306(t *testing.T) {
	f := newPaneDiedFixture(t)

	cases := []struct {
		name, state, exitCmd, wantArgs, wantState string
		wantEvents                                int
	}{
		{"prism-test@crash", "active", "exit 3", "--window=agent --exit-code=3 --agent-pane=1 rc=0", "interrupted", 1},
		{"prism-test@finished-crash", "finished", "exit 3", "--exit-code=3 --agent-pane=1 rc=0", "interrupted", 1},
		{"prism-test@quit", "active", "exit 0", "--exit-code=0 --agent-pane=1 rc=0", "interrupted", 1},
		{"prism-test@reviewing-quit", "reviewing", "exit 0", "--exit-code=0 --agent-pane=1 rc=0", "interrupted", 1},
		{"prism-test@finished-quit", "finished", "exit 0", "--exit-code=0 --agent-pane=1 rc=0", "finished", 0},
		{"prism-test@idle-crash", "idle", "exit 3", "--exit-code=3 --agent-pane=1 rc=0", "interrupted", 1},
		{"prism-test@idle-quit", "idle", "exit 0", "--exit-code=0 --agent-pane=1 rc=0", "interrupted", 1},
	}
	for _, tc := range cases {
		f.seed(t, tc.name, tc.state)
		if tc.state == "idle" {
			f.handshake(t, tc.name)
		}
		f.startAgent(t, tc.name, tc.exitCmd)
	}
	for _, tc := range cases {
		f.release(t, tc.name)
		f.waitRun(t, tc.name, tc.wantArgs)
	}
	for _, tc := range cases {
		f.requireState(t, tc.name, tc.wantState, tc.wantEvents)
	}
}

// A signal ends the agent process. #{pane_dead_status} is then empty, and
// the hook passes an empty --exit-code, which counts as a non-zero exit.
func TestPaneDiedHook_SignalOverridesFinished_3306(t *testing.T) {
	f := newPaneDiedFixture(t)

	const name = "prism-test@signal"
	f.seed(t, name, "finished")
	paneID := f.startAgent(t, name, "exec sleep 300")
	f.release(t, name)
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := f.s.output("display-message", "-p", "-t", paneID, "#{pane_current_command} #{pane_pid}")
		if fields := strings.Fields(out); err == nil && len(fields) == 2 && fields[0] == "sleep" {
			if pid, err = strconv.Atoi(fields[1]); err != nil {
				t.Fatalf("pane_pid %q: %v", fields[1], err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent pane %s does not run sleep: %q, %v", paneID, out, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}

	f.waitRun(t, name, "--window=agent --exit-code= --agent-pane=1 rc=0")
	f.requireState(t, name, "interrupted", 1)
}

// The hook acts only for the marked agent pane. A user split in the agent
// window and a pane of another window have no mark, so their exit changes
// nothing. The agent pane keeps its mark when the prefix+Space binding
// moves it to the edit window. A session in interrupted, deleted, or error
// state, an ended row, and an idle row with no state_change event (the agent
// exited before the harness handshake) are left as they are.
func TestPaneDiedHook_LeavesOtherPanesAndStates_3306(t *testing.T) {
	f := newPaneDiedFixture(t)

	const split = "prism-test@split"
	f.seed(t, split, "active")
	f.startAgent(t, split, "exit 3")
	gate := f.gate(split + "-other")
	wait := "while [ ! -e " + gate + " ]; do sleep 0.05; done; exit 4"
	if err := f.s.run("split-window", "-d", "-t", "="+split+":agent", wait); err != nil {
		t.Fatalf("split-window: %v", err)
	}
	if err := f.s.run("new-window", "-d", "-t", "="+split+":2", "-n", "term", wait); err != nil {
		t.Fatalf("new-window term: %v", err)
	}
	f.release(t, split+"-other")
	f.waitRun(t, split, "--window=agent --exit-code=4 --agent-pane= rc=0")
	f.waitRun(t, split, "--window=term --exit-code=4 --agent-pane= rc=0")
	f.requireState(t, split, "active", 0)

	const moved = "prism-test@moved"
	f.seed(t, moved, "active")
	f.startAgent(t, moved, "exit 3")
	if err := f.s.run("rename-window", "-t", "="+moved+":0", "edit"); err != nil {
		t.Fatalf("rename-window: %v", err)
	}
	if err := f.s.run("join-pane", "-h", "-s", "="+moved+":agent.0", "-t", "="+moved+":edit"); err != nil {
		t.Fatalf("join-pane: %v", err)
	}
	f.release(t, moved)
	f.waitRun(t, moved, "--window=edit --exit-code=3 --agent-pane=1 rc=0")
	f.requireState(t, moved, "interrupted", 1)

	kept := []string{"idle", "interrupted", "deleted", "error"}
	for _, state := range kept {
		name := "prism-test@" + state
		f.seed(t, name, state)
		f.startAgent(t, name, "exit 3")
	}
	const ended = "prism-test@ended"
	f.seed(t, ended, "active")
	if err := f.d.SetEnded(ended); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	f.startAgent(t, ended, "exit 3")

	for _, name := range []string{"prism-test@idle", "prism-test@interrupted", "prism-test@deleted", "prism-test@error", ended} {
		f.release(t, name)
		f.waitRun(t, name, "--window=agent --exit-code=3 --agent-pane=1 rc=0")
	}
	for _, state := range kept {
		f.requireState(t, "prism-test@"+state, state, 0)
	}
	f.requireState(t, ended, "active", 0)
}

// prism kills a pane with tmux.KillSession, and a user with the
// `x kill-pane` and `X kill-window` keys of tmux.nix. tmux removes the pane
// and runs no pane-died hook, so the rows do not change.
func TestPaneDiedHook_KilledPaneRunsNoHook_3306(t *testing.T) {
	f := newPaneDiedFixture(t)

	killed := map[string]func(name string) error{
		"prism-test@kill-session": tmux.KillSession,
		"prism-test@kill-window":  func(name string) error { return f.s.run("kill-window", "-t", "="+name+":agent") },
		"prism-test@kill-pane":    func(name string) error { return f.s.run("kill-pane", "-t", "="+name+":agent.0") },
	}
	iids := map[string]string{}
	for name := range killed {
		iids[name] = f.seed(t, name, "active")
		f.startAgent(t, name, "exit 3")
		if err := f.s.run("new-window", "-d", "-t", "="+name+":2", "-n", "term"); err != nil {
			t.Fatalf("new-window term in %q: %v", name, err)
		}
	}
	for name, kill := range killed {
		if err := kill(name); err != nil {
			t.Fatalf("kill %q: %v", name, err)
		}
	}

	// tmux runs the hooks one at a time, in order. When the run for this
	// later exit ends, a run for a killed pane has already been logged.
	const sentinel = "prism-test@sentinel"
	f.seed(t, sentinel, "active")
	f.startAgent(t, sentinel, "exit 3")
	f.release(t, sentinel)
	f.waitRun(t, sentinel, "--exit-code=3 --agent-pane=1 rc=0")

	b, _ := os.ReadFile(f.logFile)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, "event pane-died --session="+sentinel+" ") {
			t.Errorf("hook run %q, want a run for %q only", line, sentinel)
		}
	}
	for name, iid := range iids {
		requireRowsOpen(t, f.d, name, iid)
		f.requireState(t, name, "active", 0)
	}
}

// kill-server removes each pane and runs no pane-died hook, so `prism
// restore` finds the rows as they were.
func TestPaneDiedHook_KillServerRunsNoHook_3306(t *testing.T) {
	f := newPaneDiedFixture(t)

	const name = "prism-test@kill-server"
	iid := f.seed(t, name, "active")
	f.startAgent(t, name, "exit 3")

	if err := f.s.run("kill-server"); err != nil {
		t.Fatalf("kill-server: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.s.run("list-sessions") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the tmux server is alive 10s after kill-server")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The agent process gets SIGHUP when its pane closes. Give a hook run,
	// if tmux started one, time to reach the log.
	time.Sleep(time.Second)

	if b, err := os.ReadFile(f.logFile); err == nil && len(b) > 0 {
		t.Errorf("kill-server ran the pane-died hook:\n%s", b)
	}
	requireRowsOpen(t, f.d, name, iid)
	f.requireState(t, name, "active", 0)
}

func TestPaneExitCode_3306(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		want    int
		wantErr bool
	}{
		{"", -1, false},
		{"0", 0, false},
		{"3", 3, false},
		{"-1", -1, false},
		{"x", 0, true},
	} {
		got, err := paneExitCode(tc.arg)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("paneExitCode(%q) = %d, %v; want %d, error %v", tc.arg, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestIsAgentPaneExit_3306(t *testing.T) {
	for _, tc := range []struct {
		window, agentPane string
		given, want       bool
	}{
		{"agent", "1", true, true},
		{"edit", "1", true, true},
		{"agent", "", true, false},
		{"term", "", true, false},
		{"agent", "", false, true},
		{"term", "", false, false},
	} {
		if got := isAgentPaneExit(tc.window, tc.agentPane, tc.given); got != tc.want {
			t.Errorf("isAgentPaneExit(%q, %q, %v) = %v, want %v", tc.window, tc.agentPane, tc.given, got, tc.want)
		}
	}
}
