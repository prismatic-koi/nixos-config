package review_test

// Tests for #3279: the session-closed hook clears agent_status.instance_id
// of a session when its tmux session closes outside prism, or when the
// rename of tmux.KillSession fails. Then a review child can lose its
// instance ID on each of the three paths that kill it, and a parent can lose
// its instance ID before the child cleanup hands over the child audit log.

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/review"
)

const (
	hookRaceParentIID = "32790000-0000-4000-8000-0000000000aa"
	hookRaceChildIID  = "32790000-0000-4000-8000-000000000001"
)

// seedHookRaceRound seeds a parent and one delivered, finished review child.
// Each has an instance ID and a sessions row. It returns the group ID and
// the child.
func seedHookRaceRound(t *testing.T, d *db.DB, parent string) (string, string) {
	t.Helper()
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}
	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)
	child := sessions[0]
	for name, iid := range map[string]string{parent: hookRaceParentIID, child: hookRaceChildIID} {
		if err := d.SetInstanceID(name, iid); err != nil {
			t.Fatalf("SetInstanceID(%q): %v", name, err)
		}
		if err := d.InsertSession(db.Session{
			InstanceID: iid, SessionName: name, Repo: "nixos-config",
			Worktree: "/wt", Harness: "pi", StartedAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatalf("InsertSession(%q): %v", name, err)
		}
	}
	return groupID, child
}

// sessionClosedHook does the DB writes of `prism event tmux-session-end`
// for name: it ends the rows and clears agent_status.instance_id.
func sessionClosedHook(t *testing.T, d *db.DB, name string) {
	t.Helper()
	st := statusOf(t, d, name)
	if err := d.ReleasePort(name); err != nil {
		t.Errorf("hook fake: ReleasePort(%q): %v", name, err)
	}
	if err := d.SetEnded(name); err != nil {
		t.Errorf("hook fake: SetEnded(%q): %v", name, err)
	}
	if st.InstanceID != nil {
		if err := d.UpdateSessionEnded(*st.InstanceID, "finished"); err != nil {
			t.Errorf("hook fake: UpdateSessionEnded(%q): %v", name, err)
		}
	}
	if err := d.ClearInstanceID(name); err != nil {
		t.Errorf("hook fake: ClearInstanceID(%q): %v", name, err)
	}
}

// captureWarnings returns what fn writes to stderr at the warn level. It
// drains the pipe while fn runs, so that a large write cannot block fn.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	defer proglog.SetLevelForTest(proglog.LevelWarn)()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- buf
	}()
	fn()
	_ = w.Close()
	return string(<-done)
}

func spawnOutcomeCount(t *testing.T, d *db.DB, instanceID string) (aggregated, total int) {
	t.Helper()
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM spawn_outcome WHERE instance_id = ? AND aggregated_at IS NOT NULL", instanceID,
	).Scan(&aggregated); err != nil {
		t.Fatalf("count aggregated spawn_outcome rows: %v", err)
	}
	if err := d.QueryRow("SELECT COUNT(*) FROM spawn_outcome").Scan(&total); err != nil {
		t.Fatalf("count spawn_outcome rows: %v", err)
	}
	return aggregated, total
}

// childKillPaths are the three paths that kill the tmux session of a review
// child. Each installs kill as its tmux kill and runs the path.
var childKillPaths = []struct {
	name string
	run  func(t *testing.T, d *db.DB, parent, groupID string, kill func(string))
}{
	{"KillSessionsByNames", func(t *testing.T, d *db.DB, parent, _ string, kill func(string)) {
		t.Cleanup(review.ReapSideEffectsForTest(nil, nil, nil))
		t.Cleanup(review.SetKillTmuxSessionForTest(func(name string) error { kill(name); return nil }))
		if failed := review.CleanupReviewSessionsForParent(d, parent, hookRaceParentIID); len(failed) != 0 {
			t.Fatalf("CleanupReviewSessionsForParent failed = %v", failed)
		}
	}},
	{"StopReviewChildrenForEnd", func(t *testing.T, d *db.DB, parent, _ string, kill func(string)) {
		t.Cleanup(review.ReapSideEffectsForTest(kill, nil, nil))
		if failed := review.StopReviewChildrenForEnd(d, parent); len(failed) != 0 {
			t.Fatalf("StopReviewChildrenForEnd failed = %v", failed)
		}
		if err := d.SetEnded(parent); err != nil {
			t.Fatalf("SetEnded(parent): %v", err)
		}
	}},
	{"auto_release reap", func(t *testing.T, d *db.DB, _, groupID string, kill func(string)) {
		t.Cleanup(review.ReapSideEffectsForTest(kill, nil, nil))
		res, err := review.ReapDeliveredReviewAgents(d, groupID, afterGrace(), 0)
		if err != nil || len(res.Reaped) != 1 {
			t.Fatalf("ReapDeliveredReviewAgents = %+v, %v, want one reaped child", res, err)
		}
	}},
}

// AC (#3279): when the hook clears the instance ID of a review child before
// the cascade runs, the cascade writes the spawn_outcome row of that child.
// When the hook has not run, each child incarnation gets one row only.
func TestChildKillPaths_SpawnOutcome_3279(t *testing.T) {
	for _, path := range childKillPaths {
		for _, hookRuns := range []bool{true, false} {
			name := path.name + "/hook cleared the child"
			if !hookRuns {
				name = path.name + "/hook has not run"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				d := openTestDB(t)
				parent := "nixos-config@hook-race-" + strings.ReplaceAll(strings.ToLower(path.name), " ", "-")
				groupID, child := seedHookRaceRound(t, d, parent)
				var killed []string
				path.run(t, d, parent, groupID, func(name string) {
					killed = append(killed, name)
					if hookRuns {
						sessionClosedHook(t, d, name)
					}
				})

				if len(killed) != 1 || killed[0] != child {
					t.Fatalf("tmux kills = %v, want [%s]", killed, child)
				}
				if cleared := statusOf(t, d, child).InstanceID == nil; cleared != hookRuns {
					t.Fatalf("child instance_id cleared = %v, want %v", cleared, hookRuns)
				}

				if err := d.WriteSpawnOutcomeCascade(parent, hookRaceParentIID); err != nil {
					t.Fatalf("WriteSpawnOutcomeCascade: %v", err)
				}
				if aggregated, total := spawnOutcomeCount(t, d, hookRaceChildIID); aggregated != 1 || total != 2 {
					t.Errorf("spawn_outcome rows: %d aggregated for the child, %d in total, want 1 and 2", aggregated, total)
				}
			})
		}
	}
}

// AC (#3279): when the hook has cleared the instance ID of the parent and of
// the child before cleanupAgentSession runs, the child audit lines go to the
// log of the parent incarnation that the caller gives, and the child dirs
// are removed. A child with no audit log is cleaned up with no warning.
func TestCleanupReviewSessionsForParent_HookClearedIDs_3279(t *testing.T) {
	for _, withAudit := range []bool{true, false} {
		name := "child audit log"
		if !withAudit {
			name = "no child audit log"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d := openTestDB(t)
			parent := "nixos-config@hook-race-audit"
			_, child := seedHookRaceRound(t, d, parent)
			workDir, proxyAuditDir := mkInstanceDirs(t, hookRaceChildIID)
			tmpDir := mkSessionTmpDir(t, hookRaceChildIID)
			var childLine string
			if withAudit {
				childLine = writeChildAuditLog(t, hookRaceChildIID)
			}
			fake := &prismcontainertest.Fake{}
			t.Cleanup(review.SetChildPrismContainerRunnerForTest(fake, fake))
			t.Cleanup(review.ReapSideEffectsForTest(nil, nil, nil))
			sessionClosedHook(t, d, parent)
			sessionClosedHook(t, d, child)

			var failed []string
			stderr := captureWarnings(t, func() {
				failed = review.CleanupReviewSessionsForParent(d, parent, hookRaceParentIID)
			})

			if len(failed) != 0 {
				t.Fatalf("failed = %v, want none", failed)
			}
			if strings.Contains(stderr, "keep the prism-container audit log") {
				t.Errorf("stderr warns about the child audit log:\n%s", stderr)
			}
			mustNotExist(t, workDir, "child work dir")
			mustNotExist(t, proxyAuditDir, "child podman-proxy audit dir")
			mustNotExist(t, tmpDir, "child TMPDIR")
			if container.PrismContainerAuditDirExists(hookRaceChildIID) {
				t.Errorf("child prism-container audit dir stays")
			}
			parentLog, err := container.PrismContainerAuditLogPath(hookRaceParentIID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(parentLog)
			if withAudit && (err != nil || string(got) != childLine) {
				t.Errorf("parent audit log = %q (%v), want the child line %q", got, err, childLine)
			}
			if !withAudit && !os.IsNotExist(err) {
				t.Errorf("parent audit log = %q (%v), want none for a child with no audit log", got, err)
			}
		})
	}
}
