package cmd

// Tests for #3213: each cleanup path reads the instance ID and the isolation
// mode before the tmux kill. The kill fires the global session-closed hook,
// and `prism event tmux-session-end` can end the rows and clear instance_id
// before the DB block of the cleanup runs.

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// withHookAfterTmuxKill replaces the tmux kill of the cleanup paths. Right
// after the kill, the fake calls hook, as the session-closed hook can write
// before the DB block of the cleanup runs.
func withHookAfterTmuxKill(t *testing.T, hook func(session string)) {
	t.Helper()
	prev := cleanupKillTmuxSession
	cleanupKillTmuxSession = func(session string) error {
		err := prev(session)
		hook(session)
		return err
	}
	t.Cleanup(func() { cleanupKillTmuxSession = prev })
}

func readSessionsRow(t *testing.T, dbFile, instanceID string) *db.Session {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	s, err := d.SessionByInstanceID(instanceID)
	if err != nil || s == nil {
		t.Fatalf("SessionByInstanceID(%q) = %v, %v", instanceID, s, err)
	}
	return s
}

// snapshotCleanupPaths seed the archive-order fixture for each of the four
// cleanup paths. Each seed returns the fixture and a function that runs the
// path.
var snapshotCleanupPaths = []struct {
	name string
	seed func(t *testing.T) (archiveOrderFixture, func() error)
}{
	{"doCleanup", func(t *testing.T) (archiveOrderFixture, func() error) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not found in PATH")
		}
		bareRoot, worktreePath, branchName := setupMinimalBareRepo(t)
		f := setupArchiveOrderFixture(t, "snapshot-tui", worktreePath)
		m := cleanupModel{session: f.session, worktreeName: branchName, worktreePath: worktreePath, bareRoot: bareRoot}
		return f, func() error {
			done, ok := m.doCleanup()().(cleanupDoneMsg)
			if !ok {
				t.Fatal("doCleanup did not return a cleanupDoneMsg")
			}
			return done.err
		}
	}},
	{"headlessCleanupWithJSONTo", func(t *testing.T) (archiveOrderFixture, func() error) {
		f := setupArchiveOrderFixture(t, "snapshot-headless", "")
		return f, func() error {
			return headlessCleanupWithJSONTo(f.session, "snapshot-headless", "", "", false, io.Discard)
		}
	}},
	{"closeSession", func(t *testing.T) (archiveOrderFixture, func() error) {
		f := setupArchiveOrderFixture(t, "snapshot-close", "")
		return f, func() error { return closeSession(f.session) }
	}},
	{"headlessCloseSessionWithJSONTo", func(t *testing.T) (archiveOrderFixture, func() error) {
		f := setupArchiveOrderFixture(t, "snapshot-headless-close", "")
		return f, func() error { return headlessCloseSessionWithJSONTo(f.session, false, io.Discard) }
	}},
}

// AC (#3213): when the hook clears instance_id before the DB block, the
// cleanup still archives the transcript and removes the instance dirs. When
// the hook has already ended the sessions row, the cleanup does not fail
// and does not end the row a second time.
func TestCleanupPaths_SnapshotBeforeTmuxKill_3213(t *testing.T) {
	hookWrites := []struct {
		name  string
		write func(t *testing.T, dbFile, session string)
	}{
		{"hook ran to the end", func(t *testing.T, _, session string) {
			runTmuxSessionEnd(t, session)
		}},
		{"hook cleared instance_id only", func(t *testing.T, dbFile, session string) {
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			defer d.Close()
			if err := d.ClearInstanceID(session); err != nil {
				t.Fatalf("ClearInstanceID: %v", err)
			}
		}},
	}
	for _, path := range snapshotCleanupPaths {
		for _, hw := range hookWrites {
			t.Run(path.name+"/"+hw.name, func(t *testing.T) {
				f, run := path.seed(t)
				workDir, err := container.SessionWorkDirPath(archiveOrderIID)
				if err != nil {
					t.Fatalf("SessionWorkDirPath: %v", err)
				}
				if err := os.MkdirAll(workDir, 0o700); err != nil {
					t.Fatalf("mkdir work dir: %v", err)
				}
				withHookStops(t, func(string) bool { return true }, func(*db.DB, string) []string { return nil })
				hookRuns := 0
				var hookEndedAt *time.Time
				withHookAfterTmuxKill(t, func(session string) {
					hookRuns++
					hw.write(t, f.dbFile, session)
					if st := readStatus(t, f.dbFile, session); st.InstanceID != nil {
						t.Fatalf("instance_id = %q after the hook, want cleared", *st.InstanceID)
					}
					hookEndedAt = readSessionsRow(t, f.dbFile, archiveOrderIID).EndedAt
					// ended_at has a resolution of 1 ms. The pause makes a
					// second stamp by the cleanup visible.
					time.Sleep(5 * time.Millisecond)
				})

				if err := run(); err != nil {
					t.Fatalf("cleanup: %v", err)
				}

				if hookRuns != 1 {
					t.Fatalf("hook ran %d times, want 1", hookRuns)
				}
				assertTranscriptArchived(t, f)
				if _, err := os.Stat(workDir); !os.IsNotExist(err) {
					t.Errorf("work dir: %v, want removed", err)
				}
				row := readSessionsRow(t, f.dbFile, archiveOrderIID)
				if row.EndedAt == nil || row.EndState == nil || *row.EndState != "finished" {
					t.Fatalf("sessions row: ended_at=%v end_state=%v, want ended as finished", row.EndedAt, row.EndState)
				}
				if hookEndedAt != nil && !row.EndedAt.Equal(*hookEndedAt) {
					t.Errorf("sessions row ended_at = %v, want %v from the hook: the cleanup ended the row a second time", row.EndedAt, *hookEndedAt)
				}
				if readStatus(t, f.dbFile, f.session).EndedAt == nil {
					t.Error("agent_status row open, want ended")
				}
			})
		}
	}
}

// AC (#3213): a session that has no instance ID before the tmux kill is
// cleaned up as before, with no error.
func TestCleanupPaths_NoInstanceIDBeforeTmuxKill_3213(t *testing.T) {
	for _, c := range cleanupCallers {
		if c.name == "teardownConvertedSession" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const session = "myrepo@no-instance-id~investigate-x"
			dbFile, _ := setupCleanupCaller(t, session)
			withHookStops(t, func(string) bool { return true }, func(*db.DB, string) []string { return nil })
			hookRuns := 0
			withHookAfterTmuxKill(t, func(s string) {
				hookRuns++
				runTmuxSessionEnd(t, s)
			})

			var err error
			stderr := captureStderrDuringFn(t, func() { err = c.run(t, session, "") })

			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if hookRuns != 1 {
				t.Errorf("hook ran %d times, want 1", hookRuns)
			}
			if st := readStatus(t, dbFile, session); st.EndedAt == nil || st.HarnessPort != nil {
				t.Errorf("ended_at=%v port=%v, want an ended row with no port", st.EndedAt, st.HarnessPort)
			}
			if strings.Contains(stderr, "[prism] archive:") {
				t.Errorf("stderr reports an archive step for a session with no instance ID:\n%s", stderr)
			}
		})
	}
}
