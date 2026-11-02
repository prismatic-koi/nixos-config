package session

// Tests for #3392: the cleanup of a failed spawn ends the processes in the
// sandbox of the session after the tmux kill and before the row ends. When
// a process still runs, the row and the port stay, and the warning names
// the process and the session. The tests replace the sandbox stop.
// sandbox_procs_3375_test.go and internal/integration test the stop itself.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// withLoggingTmux makes tmux a script that appends its arguments to a log
// and returns the log path.
func withLoggingTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	bin := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(log) + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	prev := tmux.TmuxBin
	tmux.TmuxBin = bin
	t.Cleanup(func() { tmux.TmuxBin = prev })
	return log
}

// tmuxRenamed reports whether the tmux log holds the rename of
// tmux.KillSession for name.
func tmuxRenamed(t *testing.T, log, name string) bool {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read tmux log: %v", err)
	}
	return strings.Contains(string(data), "rename-session -t ="+name+" ")
}

func TestCleanupFailedSpawn_StopsSandboxProcessesAfterTmuxKill_3392(t *testing.T) {
	const name = "prism-test@3392~review-1-review-qa"
	for _, tc := range []struct {
		label   string
		stopErr error
	}{
		{"no process runs", nil},
		{"a process still runs", &SandboxProcessError{Session: name, PIDs: []int{4243}}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			tmuxLog := withLoggingTmux(t)
			d := openReapTestDB(t)
			if err := d.UpsertStatus(name, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
				t.Fatalf("UpsertStatus: %v", err)
			}
			if _, err := d.AllocatePort(name); err != nil {
				t.Fatalf("AllocatePort: %v", err)
			}
			prevSidecar, prevSandbox := stopFailedSpawnSidecar, stopFailedSpawnSandboxProcs
			t.Cleanup(func() { stopFailedSpawnSidecar, stopFailedSpawnSandboxProcs = prevSidecar, prevSandbox })
			stopFailedSpawnSidecar = func(string) bool { return true }
			var stops []string
			stopFailedSpawnSandboxProcs = func(s string) error {
				stops = append(stops, s)
				if !tmuxRenamed(t, tmuxLog, s) {
					t.Errorf("sandbox stop of %q before its tmux kill", s)
				}
				if st, err := d.CurrentStatus(s); err != nil || st == nil || st.EndedAt != nil || st.HarnessPort == nil {
					t.Errorf("row of %q ended, or its port released, before the sandbox stop (status=%+v, err=%v)", s, st, err)
				}
				return tc.stopErr
			}

			stderr := captureStderr(t, func() { cleanupFailedSpawn(d, SpawnOpts{SessionName: name}, db.ReapCauseSpawnFailure) })

			if len(stops) != 1 || stops[0] != name {
				t.Fatalf("sandbox stops = %v, want [%s]", stops, name)
			}
			st, err := d.CurrentStatus(name)
			if err != nil || st == nil {
				t.Fatalf("CurrentStatus = %v, %v", st, err)
			}
			if st.State != "error" {
				t.Errorf("state = %q, want error", st.State)
			}
			causes, err := d.SessionEndCauses([]string{name})
			if err != nil {
				t.Fatalf("SessionEndCauses: %v", err)
			}
			if got := causes[name].Cause; got != db.ReapCauseSpawnFailure {
				t.Errorf("cause = %q, want %q", got, db.ReapCauseSpawnFailure)
			}
			if tc.stopErr == nil {
				if st.EndedAt == nil || st.HarnessPort != nil {
					t.Errorf("ended_at=%v port=%v, want ended with no port", st.EndedAt, st.HarnessPort)
				}
				return
			}
			if st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
			if !strings.Contains(stderr, "4243") || !strings.Contains(stderr, strconv.Quote(name)) {
				t.Errorf("stderr does not name the process 4243 and the session %q:\n%s", name, stderr)
			}
		})
	}
}

// A sidecar that does not stop keeps the tmux session, so the sandbox stop
// does not run: it would also end the agent of a session that prism keeps.
func TestCleanupFailedSpawn_SidecarStopFailsSkipsSandboxStop_3392(t *testing.T) {
	const name = "prism-test@3392-kept~review-1-review-qa"
	tmuxLog := withLoggingTmux(t)
	d := openReapTestDB(t)
	if err := d.UpsertStatus(name, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	prevSidecar, prevSandbox := stopFailedSpawnSidecar, stopFailedSpawnSandboxProcs
	t.Cleanup(func() { stopFailedSpawnSidecar, stopFailedSpawnSandboxProcs = prevSidecar, prevSandbox })
	stopFailedSpawnSidecar = func(string) bool { return false }
	stopFailedSpawnSandboxProcs = func(s string) error {
		t.Errorf("sandbox stop of %q with a sidecar that did not stop", s)
		return nil
	}

	_ = captureStderr(t, func() { cleanupFailedSpawn(d, SpawnOpts{SessionName: name}, db.ReapCauseSpawnFailure) })

	if tmuxRenamed(t, tmuxLog, name) {
		t.Error("tmux session killed with a sidecar that did not stop")
	}
	if st, err := d.CurrentStatus(name); err != nil || st == nil || st.EndedAt != nil {
		t.Errorf("row = %+v, %v, want open", st, err)
	}
}
