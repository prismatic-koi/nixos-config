package cmd

// Tests for #3192: the cleanup paths, `prism reset`, and the switch_project
// conversion stop the sidecar with EnsureSidecarStopped before they kill
// tmux or end the row. When the stop fails, the session keeps its row, its
// port, its tmux session, and its worktree, and the output names it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// tmuxCalls reads the argument lists that the spy tmux recorded.
func tmuxCalls(t *testing.T, logFile string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	var calls [][]string
	for _, block := range strings.Split(string(data), "\n\n") {
		if block = strings.Trim(block, "\n"); block != "" {
			calls = append(calls, strings.Split(block, "\n"))
		}
	}
	return calls
}

// tmuxKilled reports whether the spy tmux got kill-session for name, or
// kill-server. tmux.KillSession renames the session before the kill, so a
// kill of the new name counts as a kill of name.
func tmuxKilled(t *testing.T, logFile, name string) bool {
	t.Helper()
	targets := map[string]bool{name: true, "=" + name: true}
	for _, c := range tmuxCalls(t, logFile) {
		if c[0] == "kill-server" {
			return true
		}
		if c[0] == "rename-session" && len(c) >= 4 && targets[c[2]] {
			targets["="+c[3]] = true
		}
		if c[0] == "kill-session" && len(c) >= 3 && targets[c[2]] {
			return true
		}
	}
	return false
}

// withCleanupStops replaces the sidecar stops of the cleanup and
// switch_project paths with a fake that returns ok(session). At each stop
// the fake checks that the row of the session is still open and that tmux
// did not kill the session yet. It returns the stopped sessions in order.
func withCleanupStops(t *testing.T, dbFile, tmuxLog string, ok func(string) bool) *[]string {
	t.Helper()
	var calls []string
	fake := func(name string) bool {
		calls = append(calls, name)
		if readStatus(t, dbFile, name).EndedAt != nil {
			t.Errorf("row of %q ended before its sidecar stop", name)
		}
		if tmuxKilled(t, tmuxLog, name) {
			t.Errorf("tmux session %q killed before its sidecar stop", name)
		}
		return ok(name)
	}
	prevCleanup, prevSwitch := cleanupStopSidecar, switchProjectStopSidecar
	cleanupStopSidecar, switchProjectStopSidecar = fake, fake
	t.Cleanup(func() { cleanupStopSidecar, switchProjectStopSidecar = prevCleanup, prevSwitch })
	return &calls
}

// withChildCleanup replaces cleanupReviewChildren. The fake ends the review
// children of the parent, except stuck, and returns stuck: the children
// whose sidecar did not stop. It returns the parents it was called for.
func withChildCleanup(t *testing.T, stuck ...string) *[]string {
	t.Helper()
	var parents []string
	prev := cleanupReviewChildren
	cleanupReviewChildren = func(d *db.DB, parent, _ string) []string {
		parents = append(parents, parent)
		rows, err := d.AllStatusesWithPrefix(parent + "~review-")
		if err != nil {
			t.Errorf("AllStatusesWithPrefix: %v", err)
		}
		for _, r := range rows {
			if slices.Contains(stuck, r.SessionName) {
				continue
			}
			_ = d.ReleasePort(r.SessionName)
			_ = d.SetEnded(r.SessionName)
		}
		return stuck
	}
	t.Cleanup(func() { cleanupReviewChildren = prev })
	return &parents
}

// cleanupCaller runs one session-ending path for session. worktree is a
// directory that the path owns, or "" when the path must not touch git.
type cleanupCaller struct {
	name string
	run  func(t *testing.T, session, worktree string) error
	// ownsWorktree is true when the path removes the worktree on success.
	ownsWorktree bool
	// reportsError is true when the path returns an error for a failed
	// stop. The other paths write a warning only.
	reportsError bool
}

var cleanupCallers = []cleanupCaller{
	{
		name: "headlessCleanupWithJSONTo",
		run: func(_ *testing.T, session, worktree string) error {
			bareRoot := ""
			if worktree != "" {
				bareRoot = filepath.Dir(worktree)
			}
			return headlessCleanupWithJSONTo(session, "branch", worktree, bareRoot, false, io.Discard)
		},
		ownsWorktree: true,
		reportsError: true,
	},
	{
		name: "headlessCloseSessionWithJSONTo",
		run: func(_ *testing.T, session, _ string) error {
			return headlessCloseSessionWithJSONTo(session, false, io.Discard)
		},
		reportsError: true,
	},
	{
		name: "closeSession",
		run: func(_ *testing.T, session, _ string) error {
			return closeSession(session)
		},
		reportsError: true,
	},
	{
		name: "doCleanup",
		run: func(t *testing.T, session, worktree string) error {
			m := cleanupModel{session: session, worktreeName: "branch", worktreePath: worktree, bareRoot: filepath.Dir(worktree)}
			done, ok := m.doCleanup()().(cleanupDoneMsg)
			if !ok {
				t.Fatal("doCleanup did not return a cleanupDoneMsg")
			}
			return done.err
		},
		ownsWorktree: true,
		reportsError: true,
	},
	{
		name: "stopSessionForConvert",
		run: func(_ *testing.T, session, _ string) error {
			return stopSessionForConvert(session)
		},
		reportsError: true,
	},
}

// setupCleanupCaller seeds an open row with a port for session, a spy tmux,
// a fake podman runner, and a fake review-child cleanup.
func setupCleanupCaller(t *testing.T, session string) (dbFile, tmuxLog string) {
	t.Helper()
	dbFile = seedHookRows(t, session)
	tmuxLog = withSpyTmux(t)
	installFakeRunner(t)
	withChildCleanup(t)
	return dbFile, tmuxLog
}

// AC: when the stop fails, the row, the port, the tmux session, and the
// worktree stay, and the output names the session.
func TestSessionEndingPaths_StopFailureKeepsSession_3192(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			const session = "myrepo@stop-fails"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			stops := withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return false })
			worktree := t.TempDir()

			var err error
			stderr := captureStderrDuringFn(t, func() { err = c.run(t, session, worktree) })

			if !slices.Equal(*stops, []string{session}) {
				t.Errorf("sidecar stops = %v, want [%s]", *stops, session)
			}
			st := readStatus(t, dbFile, session)
			if st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
			if tmuxKilled(t, tmuxLog, session) {
				t.Error("tmux session killed, want kept")
			}
			if _, statErr := os.Stat(worktree); statErr != nil {
				t.Errorf("worktree: %v, want kept", statErr)
			}
			if !strings.Contains(stderr, strconv.Quote(session)) {
				t.Errorf("stderr does not name %q:\n%s", session, stderr)
			}
			if c.reportsError {
				if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), strconv.Quote(session)) {
					t.Errorf("error = %v, want an errSidecarNotStopped error that names %q", err, session)
				}
			} else if err != nil {
				t.Errorf("error = %v, want nil", err)
			}
		})
	}
}

// AC: the sidecar stops before the tmux session and the row go. The fake
// stop checks the order; this test checks that both go after it.
func TestSessionEndingPaths_StopBeforeTmuxAndRow_3192(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const session = "myrepo@stop-works~investigate-x"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			stops := withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })

			if err := c.run(t, session, ""); err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			if !slices.Equal(*stops, []string{session}) {
				t.Errorf("sidecar stops = %v, want [%s]", *stops, session)
			}
			if readStatus(t, dbFile, session).EndedAt == nil {
				t.Error("row open, want ended")
			}
			if !tmuxKilled(t, tmuxLog, session) {
				t.Error("tmux session not killed")
			}
		})
	}
}

// AC: a session that has no running sidecar is cleaned up as before, with
// no error. These cases use the real EnsureSidecarStopped.
func TestSessionEndingPaths_NoRunningSidecar_3192(t *testing.T) {
	deadPID := func(t *testing.T) int {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", "exit 0")
		if err := cmd.Run(); err != nil {
			t.Fatalf("run sh: %v", err)
		}
		return cmd.Process.Pid
	}
	for _, c := range cleanupCallers {
		for _, pidCase := range []string{"no PID file", "PID already gone"} {
			t.Run(c.name+"/"+pidCase, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				const session = "myrepo@no-sidecar~investigate-x"
				dbFile, _ := setupCleanupCaller(t, session)
				pidPath, err := prismSession.SidecarPIDPath(session)
				if err != nil {
					t.Fatalf("SidecarPIDPath: %v", err)
				}
				if pidCase == "PID already gone" {
					if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
						t.Fatalf("mkdir: %v", err)
					}
					if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID(t))+"\n"), 0o644); err != nil {
						t.Fatalf("write PID file: %v", err)
					}
				}

				if err := c.run(t, session, ""); err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				if readStatus(t, dbFile, session).EndedAt == nil {
					t.Error("row open, want ended")
				}
				if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
					t.Errorf("PID file: %v, want removed", err)
				}
			})
		}
	}
}

// A running sidecar exits before the cleanup ends its row. This case uses
// the real EnsureSidecarStopped and a stub sidecar process.
func TestHeadlessCloseSession_StopsRunningSidecar_3192(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const session = "myrepo@running-sidecar~investigate-x"
	dbFile, _ := setupCleanupCaller(t, session)
	done := startNamedSidecarStub(t, session)
	time.Sleep(100 * time.Millisecond)

	if err := headlessCloseSessionWithJSONTo(session, false, io.Discard); err != nil {
		t.Fatalf("headlessCloseSessionWithJSONTo: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("the sidecar still runs after the cleanup returned")
	}
	if readStatus(t, dbFile, session).EndedAt == nil {
		t.Error("row open, want ended")
	}
}

// The --json envelope of a failed stop reports the lifecycle fields as
// skipped and session_killed as false.
func TestHeadlessCleanup_StopFailureJSON_3192(t *testing.T) {
	const session = "myrepo@stop-fails-json"
	dbFile, tmuxLog := setupCleanupCaller(t, session)
	withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return false })

	var out bytes.Buffer
	err := headlessCleanupWithJSONTo(session, "branch", "", "", true, &out)
	if !errors.Is(err, errSidecarNotStopped) {
		t.Fatalf("error = %v, want errSidecarNotStopped", err)
	}
	var got map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &got); jsonErr != nil {
		t.Fatalf("envelope %q: %v", out.String(), jsonErr)
	}
	if got["session_killed"] != false {
		t.Errorf("session_killed = %v, want false", got["session_killed"])
	}
	for _, field := range []string{"ended_at_stamped", "harness_port_released", "harness_session_id_cleared"} {
		if got[field] != "skipped: sidecar did not stop" {
			t.Errorf("%s = %v, want the skip report", field, got[field])
		}
	}
}

// AC: `prism cleanup` of several sessions continues with the next session
// when one stop fails, and exits non-zero.
func TestCleanup_SeveralSessions_ContinuesAndExitsNonZero_3192(t *testing.T) {
	headless := map[string]func(session string) error{
		"headlessCleanupWithJSONTo": func(session string) error {
			return headlessCleanupWithJSONTo(session, "branch", "", "", false, io.Discard)
		},
		"headlessCloseSessionWithJSONTo": func(session string) error {
			return headlessCloseSessionWithJSONTo(session, false, io.Discard)
		},
	}
	for name, run := range headless {
		t.Run(name+"/child stop fails", func(t *testing.T) {
			const parent = "myrepo@several-child"
			stuck := parent + "~review-1-review-goal"
			stopped := parent + "~review-1-review-code"
			dbFile := seedHookRows(t, parent, stuck, stopped)
			tmuxLog := withSpyTmux(t)
			installFakeRunner(t)
			parents := withChildCleanup(t, stuck)
			withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })

			err := run(parent)

			if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), strconv.Quote(stuck)) || strings.Contains(err.Error(), strconv.Quote(parent)) {
				t.Errorf("error = %v, want an errSidecarNotStopped error that names %q only", err, stuck)
			}
			if !slices.Equal(*parents, []string{parent}) {
				t.Errorf("children cleaned up for %v, want [%s]", *parents, parent)
			}
			if readStatus(t, dbFile, parent).EndedAt == nil {
				t.Error("parent row open, want ended")
			}
			if readStatus(t, dbFile, stopped).EndedAt == nil {
				t.Error("child with a stopped sidecar open, want ended")
			}
			if st := readStatus(t, dbFile, stuck); st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("stuck child: ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
		})
		t.Run(name+"/parent stop fails", func(t *testing.T) {
			const parent = "myrepo@several-parent"
			child := parent + "~review-2-review-qa"
			dbFile := seedHookRows(t, parent, child)
			tmuxLog := withSpyTmux(t)
			installFakeRunner(t)
			parents := withChildCleanup(t)
			withCleanupStops(t, dbFile, tmuxLog, func(s string) bool { return s != parent })

			err := run(parent)

			if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), strconv.Quote(parent)) {
				t.Errorf("error = %v, want an errSidecarNotStopped error that names %q", err, parent)
			}
			if !slices.Equal(*parents, []string{parent}) {
				t.Errorf("children cleaned up for %v, want [%s]", *parents, parent)
			}
			if readStatus(t, dbFile, child).EndedAt == nil {
				t.Error("child row open, want ended: the cleanup continues with the next session")
			}
			if st := readStatus(t, dbFile, parent); st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("parent: ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
		})
	}
}

// AC: the command exits non-zero. rootCmd.Execute returns the error, and
// main exits 1 on it.
func TestCleanupCommand_StopFailureReturnsError_3192(t *testing.T) {
	const session = "obsidian-3192"
	dbFile, tmuxLog := setupCleanupCaller(t, session)
	withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return false })

	rootCmd.SetArgs([]string{"cleanup", "--yes", "--session", session})
	var err error
	captureStdoutDuringFn(t, func() { err = rootCmd.Execute() })
	if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), strconv.Quote(session)) {
		t.Errorf("prism cleanup = %v, want an errSidecarNotStopped error that names %q", err, session)
	}
}

// The interactive TUI closes as soon as the cleanup is done, so the error
// of a failed stop must reach the command.
func TestCleanupWrapper_KeepsDoneError_3192(t *testing.T) {
	stopErr := sidecarsNotStoppedError([]string{"myrepo@tui"})
	w, _ := cleanupWrapper{}.Update(cleanupDoneMsg{stopErr})
	if got := w.(cleanupWrapper).doneErr; !errors.Is(got, errSidecarNotStopped) {
		t.Errorf("doneErr = %v, want the stop error", got)
	}
}

// ── prism reset ──────────────────────────────────────────────────────────────

// withListingSpyTmux is withSpyTmux, but list-sessions prints sessions.
func withListingSpyTmux(t *testing.T, sessions ...string) string {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "tmux-args")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + logFile + "; done\n" +
		"printf '\\n' >> " + logFile + "\n" +
		"case \"$1\" in\n" +
		"  list-sessions) printf '" + strings.Join(sessions, "\\n") + "\\n' ;;\n" +
		"  has-session) exit 1 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write spy tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = filepath.Join(dir, "tmux")
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return logFile
}

// writeSidecarRunFiles writes a PID file and a ready file for each name.
func writeSidecarRunFiles(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		pidPath, err := prismSession.SidecarPIDPath(n)
		if err != nil {
			t.Fatalf("SidecarPIDPath: %v", err)
		}
		readyPath, err := prismSession.SidecarReadyPath(n)
		if err != nil {
			t.Fatalf("SidecarReadyPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for _, p := range []string{pidPath, readyPath} {
			if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
				t.Fatalf("write %s: %v", p, err)
			}
		}
	}
}

// withResetStops replaces the sidecar stop of prism reset with a fake that
// fails for stuck. At each stop the fake checks that the row is still open
// and that tmux was not killed.
func withResetStops(t *testing.T, dbFile, tmuxLog string, stuck ...string) {
	t.Helper()
	prev := resetStopSidecar
	resetStopSidecar = func(name string) bool {
		if readStatus(t, dbFile, name).EndedAt != nil {
			t.Errorf("row of %q ended before its sidecar stop", name)
		}
		if tmuxKilled(t, tmuxLog, name) {
			t.Errorf("tmux killed before the sidecar stop of %q", name)
		}
		return !slices.Contains(stuck, name)
	}
	t.Cleanup(func() { resetStopSidecar = prev })
}

// AC: `prism reset` stops the sidecar of each session before it ends the
// row of that session. A session whose sidecar does not stop keeps its
// tmux session, its row, its port, and its resume pointer.
func TestRunReset_StopsSidecarsFirstAndKeepsStuckSession_3192(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const stuck, stopped = "myrepo@reset-stuck", "myrepo@reset-stopped"
	dbFile := seedHookRows(t, stuck, stopped)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, n := range []string{stuck, stopped} {
		if err := d.UpdateHarnessSessionID(n, "sid-"+n); err != nil {
			t.Fatalf("UpdateHarnessSessionID: %v", err)
		}
	}
	d.Close()
	writeSidecarRunFiles(t, stuck, stopped)
	tmuxLog := withListingSpyTmux(t, stuck, stopped, "scratchpad")
	withResetStops(t, dbFile, tmuxLog, stuck)

	rootCmd.SetArgs([]string{"reset", "--yes", "--no-launch"})
	var runErr error
	stderr := captureStderrDuringFn(t, func() {
		captureStdoutDuringFn(t, func() { runErr = rootCmd.Execute() })
	})
	if runErr != nil {
		t.Fatalf("prism reset: %v", runErr)
	}

	if !strings.Contains(stderr, strconv.Quote(stuck)) {
		t.Errorf("stderr does not name %q:\n%s", stuck, stderr)
	}
	st := readStatus(t, dbFile, stuck)
	if st.EndedAt != nil || st.HarnessPort == nil || st.HarnessSessionID == nil {
		t.Errorf("stuck row: ended_at=%v port=%v resume=%v, want open with its port and resume pointer", st.EndedAt, st.HarnessPort, st.HarnessSessionID)
	}
	st = readStatus(t, dbFile, stopped)
	if st.EndedAt == nil || st.HarnessSessionID != nil {
		t.Errorf("stopped row: ended_at=%v resume=%v, want ended with no resume pointer", st.EndedAt, st.HarnessSessionID)
	}
	if tmuxKilled(t, tmuxLog, stuck) {
		t.Error("tmux of the stuck session killed, want kept")
	}
	for _, n := range []string{stopped, "scratchpad"} {
		if !tmuxKilled(t, tmuxLog, n) {
			t.Errorf("tmux session %q not killed", n)
		}
	}
	readyStuck, _ := prismSession.SidecarReadyPath(stuck)
	if _, err := os.Stat(readyStuck); err != nil {
		t.Errorf("ready file of the stuck session: %v, want kept", err)
	}
	readyStopped, _ := prismSession.SidecarReadyPath(stopped)
	if _, err := os.Stat(readyStopped); !os.IsNotExist(err) {
		t.Errorf("ready file of the stopped session: %v, want removed", err)
	}
}

// With no failed stop, reset kills the whole tmux server, after the stops.
func TestRunReset_KillsTmuxServerAfterStops_3192(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const session = "myrepo@reset-ok"
	dbFile := seedHookRows(t, session)
	writeSidecarRunFiles(t, session)
	tmuxLog := withListingSpyTmux(t, session)
	withResetStops(t, dbFile, tmuxLog)

	rootCmd.SetArgs([]string{"reset", "--yes", "--no-launch"})
	var runErr error
	captureStdoutDuringFn(t, func() { runErr = rootCmd.Execute() })
	if runErr != nil {
		t.Fatalf("prism reset: %v", runErr)
	}

	calls := tmuxCalls(t, tmuxLog)
	if !slices.ContainsFunc(calls, func(c []string) bool { return c[0] == "kill-server" }) {
		t.Errorf("tmux calls %v, want kill-server", calls)
	}
	if readStatus(t, dbFile, session).EndedAt == nil {
		t.Error("row open, want ended")
	}
}

// The session-closed hook clears instance_id of the row of a session that
// closes outside prism, and that can occur during the reset. Reset takes
// the instance IDs before the kill, so that it still removes the instance
// directories of each reset session (#2960). The directories of a kept
// session stay.
func TestRunReset_SnapshotBeforeTmuxKill_3192(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		stuck []string
	}{
		{"tmux server killed", nil},
		{"tmux sessions killed one by one", []string{"myrepo@snap-stuck"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			clearPICodingAgentDir(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
			sessions := []string{"myrepo@snap-stuck", "myrepo@snap-stopped"}
			dbFile := seedHookRows(t, sessions...)
			workDirs := map[string]string{}
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			for _, n := range sessions {
				id := "iid-" + strings.TrimPrefix(n, "myrepo@")
				if err := d.SetInstanceID(n, id); err != nil {
					t.Fatalf("SetInstanceID: %v", err)
				}
				dir, err := container.SessionWorkDirPath(id)
				if err != nil {
					t.Fatalf("SessionWorkDirPath: %v", err)
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir work dir: %v", err)
				}
				workDirs[n] = dir
			}
			d.Close()
			writeSidecarRunFiles(t, sessions...)
			tmuxLog := withListingSpyTmux(t, sessions...)
			withResetStops(t, dbFile, tmuxLog, tc.stuck...)

			prev := resetKillTmuxStep
			resetKillTmuxStep = func(kept []string) {
				prev(kept)
				hd, err := db.Open(dbFile)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer hd.Close()
				for _, n := range sessions {
					if !slices.Contains(kept, n) {
						_ = hd.ClearInstanceID(n)
					}
				}
			}
			t.Cleanup(func() { resetKillTmuxStep = prev })

			rootCmd.SetArgs([]string{"reset", "--yes", "--no-launch"})
			var runErr error
			captureStderrDuringFn(t, func() {
				captureStdoutDuringFn(t, func() { runErr = rootCmd.Execute() })
			})
			if runErr != nil {
				t.Fatalf("prism reset: %v", runErr)
			}

			for _, n := range sessions {
				_, statErr := os.Stat(workDirs[n])
				if slices.Contains(tc.stuck, n) {
					if statErr != nil {
						t.Errorf("work dir of kept session %q: %v, want kept", n, statErr)
					}
				} else if !os.IsNotExist(statErr) {
					t.Errorf("work dir of reset session %q: %v, want removed", n, statErr)
				}
			}
		})
	}
}

// A session with a PID file whose PID is gone is reset as before. This
// case uses the real EnsureSidecarStopped.
func TestResetStopSidecars_StalePIDFile_3192(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const session = "myrepo@reset-stale"
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run sh: %v", err)
	}
	writeSidecarRunFiles(t, session)
	pidPath, _ := prismSession.SidecarPIDPath(session)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	var kept []string
	var err error
	captureStdoutDuringFn(t, func() { kept, err = resetStopSidecars() })
	if err != nil || len(kept) != 0 {
		t.Fatalf("resetStopSidecars = %v, %v, want no kept session and no error", kept, err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file: %v, want removed", err)
	}
}
