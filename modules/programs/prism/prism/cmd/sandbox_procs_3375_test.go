package cmd

// Tests for #3375: on Darwin, a process that the agent detaches outlives
// the tmux kill, because sandbox-exec has no PID namespace. The cleanup
// paths, the stop before a conversion, and the tmux-session-end hook end
// each process in the sandbox of the session after the tmux kill and
// before the row ends. When a process still runs, the path keeps the row
// and the port, and its error names the process and the session. The
// tests replace the sandbox stop. internal/session and internal/integration
// test the stop itself.

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// withSandboxStops replaces stopSessionSandboxProcs with a fake that
// returns err. At each call the fake checks that the row of the session is
// still open and, when tmuxLog is not empty, that tmux killed the session
// first. It returns the sessions of the calls in order. prism reset runs
// the stops at the same time, so the fake holds a lock.
func withSandboxStops(t *testing.T, dbFile, tmuxLog string, err error) *[]string {
	t.Helper()
	var calls []string
	var mu sync.Mutex
	prev := stopSessionSandboxProcs
	stopSessionSandboxProcs = func(name string) error {
		mu.Lock()
		calls = append(calls, name)
		mu.Unlock()
		if readStatus(t, dbFile, name).EndedAt != nil {
			t.Errorf("row of %q ended before the stop of its sandbox processes", name)
		}
		if tmuxLog != "" && !tmuxKilled(t, tmuxLog, name) {
			t.Errorf("stop of the sandbox processes of %q before its tmux kill: it would also end the agent before tmux does", name)
		}
		return err
	}
	t.Cleanup(func() { stopSessionSandboxProcs = prev })
	return &calls
}

// stillRuns is the error of a sandbox stop that cannot end process 4243.
func stillRuns(session string) error {
	return &prismSession.SandboxProcessError{Session: session, PIDs: []int{4243}}
}

// AC: `prism cleanup`, `prism close`, and the stop before a conversion end
// the processes in the sandbox of the session after the tmux kill and
// before the row ends. The fake stop checks the order.
func TestSessionEndingPaths_StopSandboxProcessesAfterTmuxKill_3375(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const session = "myrepo@detached~investigate-x"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })
			calls := withSandboxStops(t, dbFile, tmuxLog, nil)

			if err := c.run(t, session, ""); err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			if !slices.Equal(*calls, []string{session}) {
				t.Errorf("sandbox stops = %v, want [%s]", *calls, session)
			}
			if readStatus(t, dbFile, session).EndedAt == nil {
				t.Error("row open, want ended")
			}
		})
	}
}

// AC: when a process in the sandbox still runs, the stop is refused. The
// error names the process and the session, and the session keeps its row
// and its port, so that a later cleanup ends the process.
func TestSessionEndingPaths_SandboxProcessStillRunsRefuses_3375(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			const session = "myrepo@detached-runs~investigate-x"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })
			withSandboxStops(t, dbFile, tmuxLog, stillRuns(session))

			var err error
			captureStderrDuringFn(t, func() { err = c.run(t, session, "") })

			var se *prismSession.SandboxProcessError
			if !errors.As(err, &se) {
				t.Errorf("error = %v, want the *SandboxProcessError of the stop", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), strconv.Quote(session))) {
				t.Errorf("error %q does not name the process 4243 and the session %q", err, session)
			}
			if c.name != "stopSessionForConvert" && !errors.Is(err, errSandboxProcessRuns) {
				t.Errorf("error = %v, want errSandboxProcessRuns, so that the TUI cleanup returns it", err)
			}
			if st := readStatus(t, dbFile, session); st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
		})
	}
}

// AC (review of #3375): after a refused stop, the start record of the
// session stays open, so prism confirm-bare, ConvertToBare, and the target
// check of CloneWorktree still refuse. The fake sidecar stop ends the
// sessions row of the incarnation, as Sidecar.Shutdown does, so only the
// kept agent_status row holds the record open.
func TestSessionEndingPaths_RefusedStopKeepsTheStartRecordOpen_3375(t *testing.T) {
	for _, c := range cleanupCallers {
		t.Run(c.name, func(t *testing.T) {
			const session = "myrepo@detached-record~investigate-x"
			const iid = "0f0e0d0c-0b0a-4908-8706-050403023375"
			dbFile, tmuxLog := setupCleanupCaller(t, session)
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			if err := d.InsertSession(db.Session{InstanceID: iid, SessionName: session, Repo: "myrepo", Worktree: "/code/myrepo/branch"}); err != nil {
				t.Fatalf("InsertSession: %v", err)
			}
			if err := d.SetInstanceID(session, iid); err != nil {
				t.Fatalf("SetInstanceID: %v", err)
			}
			d.Close()
			endIncarnation := func(string) error {
				d, err := db.Open(dbFile)
				if err != nil {
					return err
				}
				defer d.Close()
				return d.EndIncarnation(iid, "finished")
			}
			prevCleanup, prevSwitch := cleanupStopSidecar, switchProjectStopSidecar
			cleanupStopSidecar, switchProjectStopSidecar = endIncarnation, endIncarnation
			t.Cleanup(func() { cleanupStopSidecar, switchProjectStopSidecar = prevCleanup, prevSwitch })
			withSandboxStops(t, dbFile, tmuxLog, stillRuns(session))

			var runErr error
			captureStderrDuringFn(t, func() { runErr = c.run(t, session, "") })
			if runErr == nil {
				t.Fatal("error = nil, want a refusal")
			}

			starts, err := db.SessionStarts(dbFile)
			if err != nil {
				t.Fatalf("SessionStarts: %v", err)
			}
			open := false
			for _, s := range starts {
				if s.Name == session && s.Open {
					open = true
				}
			}
			if !open {
				t.Errorf("start records = %+v, want the record of %q open after the refused stop", starts, session)
			}
		})
	}
}

// The --json envelope of a refused cleanup reports the lifecycle fields as
// skipped.
func TestHeadlessCleanupJSON_SandboxProcessStillRuns_3375(t *testing.T) {
	const session = "myrepo@detached-json~investigate-x"
	dbFile, tmuxLog := setupCleanupCaller(t, session)
	withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })
	withSandboxStops(t, dbFile, tmuxLog, stillRuns(session))

	var out bytes.Buffer
	var err error
	captureStderrDuringFn(t, func() { err = headlessCleanupWithJSONTo(session, "branch", "", "", true, &out) })
	if err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	var env map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &env); jsonErr != nil {
		t.Fatalf("envelope %q: %v", out.String(), jsonErr)
	}
	const skipped = "skipped: a process in the sandbox still runs"
	for _, k := range []string{"ended_at_stamped", "harness_port_released", "harness_session_id_cleared"} {
		if env[k] != skipped {
			t.Errorf("%s = %v, want %q", k, env[k], skipped)
		}
	}
	if env["session_killed"] != true {
		t.Errorf("session_killed = %v, want true: the tmux kill runs before the stop", env["session_killed"])
	}
}

// AC: prism does not convert while a process in the sandbox of the root
// session still runs.
func TestConvertPlainClone_RefusesWhileASandboxProcessRuns_3375(t *testing.T) {
	dir, name := newPlainCloneDir(t, "cv3375")
	dbFile := seedHookRows(t, name)
	tmuxLog := withListingSpyTmux(t, name)
	installFakeRunner(t)
	withChildCleanup(t)
	withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })
	calls := withSandboxStops(t, dbFile, tmuxLog, stillRuns(name))
	conversions := withConvertToBare(t, func(string, func(string)) (string, error) { return "", nil })

	var err error
	captureStderrDuringFn(t, func() { _, err = convertPlainClone(dir, func(string) {}) })

	if !slices.Equal(*calls, []string{name}) {
		t.Errorf("sandbox stops = %v, want [%s]", *calls, name)
	}
	if err == nil || !strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), strconv.Quote(name)) {
		t.Errorf("error = %v, want a refusal that names the process 4243 and the session %q", err, name)
	}
	if *conversions != 0 {
		t.Errorf("conversions = %d, want 0", *conversions)
	}
	if readStatus(t, dbFile, name).EndedAt != nil {
		t.Error("row ended, want open")
	}
}

// The hook asks tmux again after the sidecar stop. If `prism switch` made
// the session again in that time, the hook does not stop the sandbox
// processes, because they belong to the new session, and it keeps the row.
func TestEventTmuxSessionEnd_SessionMadeAgainBeforeSandboxStop_3375(t *testing.T) {
	const session = "myrepo@hook-remade"
	dbFile := seedHookRows(t, session)
	withHookStops(t,
		func(string) bool { return true },
		func(*db.DB, string) []string { t.Error("children stopped for a session that runs again"); return nil })
	calls := withSandboxStops(t, dbFile, "", nil)
	checks := 0
	prev := hookTmuxRecheck
	hookTmuxRecheck = func(string) prismSession.TmuxPresence {
		checks++
		if checks == 1 {
			return prismSession.TmuxGone
		}
		return prismSession.TmuxPresent
	}
	t.Cleanup(func() { hookTmuxRecheck = prev })

	runTmuxSessionEnd(t, session)

	if len(*calls) != 0 {
		t.Errorf("sandbox stops = %v, want none: the session runs again", *calls)
	}
	if checks != 2 {
		t.Errorf("tmux checks = %d, want 2: one before each stop", checks)
	}
	if st := readStatus(t, dbFile, session); st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("ended_at=%v port=%v, want the open row of the new session", st.EndedAt, st.HarnessPort)
	}
}

// prism reset ends the processes in the sandbox of each session after the
// tmux kill. A session where a process still runs keeps its row and its
// resume pointer. The other session ends as before.
func TestRunReset_SandboxProcessStillRunsKeepsRow_3375(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const stuck, done = "myrepo@reset-detached", "myrepo@reset-clean"
	dbFile := seedHookRows(t, stuck, done)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, n := range []string{stuck, done} {
		if err := d.UpdateHarnessSessionID(n, "hsid-"+n); err != nil {
			t.Fatalf("UpdateHarnessSessionID: %v", err)
		}
	}
	d.Close()
	writeSidecarRunFiles(t, stuck, done)
	tmuxLog := withListingSpyTmux(t, stuck, done)
	withResetStops(t, dbFile, tmuxLog)
	calls := withSandboxStops(t, dbFile, tmuxLog, nil)
	prev := stopSessionSandboxProcs
	stopSessionSandboxProcs = func(s string) error {
		if err := prev(s); err != nil || s != stuck {
			return err
		}
		return stillRuns(s)
	}

	rootCmd.SetArgs([]string{"reset", "--yes", "--no-launch"})
	var runErr error
	stderr := captureStderrDuringFn(t, func() {
		captureStdoutDuringFn(t, func() { runErr = rootCmd.Execute() })
	})
	if runErr != nil {
		t.Fatalf("prism reset: %v", runErr)
	}

	if got := slices.Sorted(slices.Values(*calls)); !slices.Equal(got, []string{done, stuck}) {
		t.Errorf("sandbox stops = %v, want [%s %s]", got, done, stuck)
	}
	if st := readStatus(t, dbFile, stuck); st.EndedAt != nil || st.HarnessSessionID == nil {
		t.Errorf("session with a running sandbox process: ended_at=%v resume pointer=%v, want an open row with its pointer", st.EndedAt, st.HarnessSessionID)
	}
	if st := readStatus(t, dbFile, done); st.EndedAt == nil || st.HarnessSessionID != nil {
		t.Errorf("other session: ended_at=%v resume pointer=%v, want ended with no pointer", st.EndedAt, st.HarnessSessionID)
	}
	if !strings.Contains(stderr, "4243") || !strings.Contains(stderr, strconv.Quote(stuck)) {
		t.Errorf("stderr does not name the process 4243 and the session %q:\n%s", stuck, stderr)
	}
}

// The tmux-session-end hook ends the processes in the sandbox after the
// sidecar stop. When a process still runs, it keeps the row and the port,
// so that prism confirm-bare and the stop before a conversion still see
// the session.
func TestEventTmuxSessionEnd_StopsSandboxProcesses_3375(t *testing.T) {
	for _, stopErr := range []error{nil, stillRuns("myrepo@hook-detached")} {
		name := "ended"
		if stopErr != nil {
			name = "kept"
		}
		t.Run(name, func(t *testing.T) {
			const session = "myrepo@hook-detached"
			dbFile := seedHookRows(t, session)
			var order []string
			withHookStops(t,
				func(string) bool { order = append(order, "sidecar"); return true },
				func(*db.DB, string) []string { order = append(order, "children"); return nil })
			calls := withSandboxStops(t, dbFile, "", stopErr)
			prev := stopSessionSandboxProcs
			stopSessionSandboxProcs = func(s string) error { order = append(order, "sandbox"); return prev(s) }

			stderr := captureStderrDuringFn(t, func() { runTmuxSessionEnd(t, session) })

			if !slices.Equal(*calls, []string{session}) {
				t.Errorf("sandbox stops = %v, want [%s]", *calls, session)
			}
			st := readStatus(t, dbFile, session)
			if stopErr == nil {
				if !slices.Equal(order, []string{"sidecar", "sandbox", "children"}) {
					t.Errorf("order = %v, want the sidecar stop, the sandbox stop, then the children", order)
				}
				if st.EndedAt == nil {
					t.Error("row open, want ended")
				}
				return
			}
			if st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
			if !strings.Contains(stderr, "4243") {
				t.Errorf("stderr does not name the process 4243:\n%s", stderr)
			}
		})
	}
}
