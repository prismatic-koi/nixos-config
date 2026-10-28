package cmd

// Tests for #3279: the session-closed hook clears agent_status.instance_id
// of a session that closes outside prism, or whose tmux.KillSession rename
// fails. The tests act as the hook for the parent and for each review
// child. Both clears can occur before the child cleanup and the
// spawn_outcome cascade of a cleanup path run.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
	"github.com/prismatic-koi/prism/internal/review"
)

const hookRaceChildIID = "32790000-0000-4000-8000-000000000001"

// hookRaceChild holds the review child of a cleanup fixture and the paths
// of its instance dirs.
type hookRaceChild struct {
	session, workDir, tmpDir, proxyAuditDir, auditLine string
}

// seedHookRaceChild adds a review child with an instance ID, a sessions
// row, a work dir, a TMPDIR, and a podman-proxy audit dir to f. With
// withAudit, the child also has a prism-container audit log.
func seedHookRaceChild(t *testing.T, f archiveOrderFixture, withAudit bool) hookRaceChild {
	t.Helper()
	c := hookRaceChild{session: f.session + "~review-1-review-qa"}
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(c.session, archiveOrderRepo, f.worktree, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child: %v", err)
	}
	if err := d.SetInstanceID(c.session, hookRaceChildIID); err != nil {
		t.Fatalf("SetInstanceID child: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: hookRaceChildIID, SessionName: c.session, Repo: archiveOrderRepo,
		Worktree: f.worktree, Harness: "pi", StartedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession child: %v", err)
	}
	d.Close()

	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	for _, p := range []struct {
		dst  *string
		path func(string) (string, error)
	}{
		{&c.workDir, container.SessionWorkDirPath},
		{&c.tmpDir, container.SessionTmpDirPath},
		{&c.proxyAuditDir, container.PodmanProxyAuditDirPath},
	} {
		dir, err := p.path(hookRaceChildIID)
		if err != nil {
			t.Fatalf("child dir path: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write in %s: %v", dir, err)
		}
		*p.dst = dir
	}
	if withAudit {
		c.auditLine = writeFakePrismContainerAuditLog(t, hookRaceChildIID, "child")
	}
	fake := &prismcontainertest.Fake{}
	installPrismContainerFake(t, fake)
	t.Cleanup(review.SetChildPrismContainerRunnerForTest(fake, fake))
	return c
}

// childSessionClosedHook does the DB writes of the hook of child: it ends
// the rows of child and clears its instance ID.
func childSessionClosedHook(t *testing.T, d *db.DB, child string) {
	t.Helper()
	st, err := d.CurrentStatus(child)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", child, st, err)
	}
	_ = d.ReleasePort(child)
	if err := d.SetEnded(child); err != nil {
		t.Errorf("SetEnded(%q): %v", child, err)
	}
	if st.InstanceID != nil {
		if err := d.UpdateSessionEnded(*st.InstanceID, "finished"); err != nil {
			t.Errorf("UpdateSessionEnded(%q): %v", child, err)
		}
	}
	if err := d.ClearInstanceID(child); err != nil {
		t.Errorf("ClearInstanceID(%q): %v", child, err)
	}
}

// AC (#3279): the tmux kill of the parent runs the real hook of the parent.
// That hook stops the review children, and the hook of the child clears
// the instance ID of the child. The hook of the parent then clears the
// instance ID of the parent. The cleanup still removes the child dirs,
// puts the child audit lines in the archive of the parent, and writes the
// spawn_outcome row of the child. A child with no audit log is cleaned up
// with no error and no warning.
//
// closeSession does not clean up the child dirs, so only the spawn_outcome
// row applies to it.
func TestCleanupPaths_HookClearsParentAndChild_3279(t *testing.T) {
	for _, path := range snapshotCleanupPaths {
		for _, withAudit := range []bool{true, false} {
			name := path.name + "/child audit log"
			if !withAudit {
				name = path.name + "/no child audit log"
			}
			t.Run(name, func(t *testing.T) {
				f, run := path.seed(t)
				c := seedHookRaceChild(t, f, withAudit)
				childHookRuns := 0
				withHookStops(t, func(string) bool { return true }, func(d *db.DB, parent string) []string {
					if parent != f.session {
						t.Errorf("hook stopped the children of %q, want %q", parent, f.session)
					}
					childHookRuns++
					childSessionClosedHook(t, d, c.session)
					return nil
				})
				withHookAfterTmuxKill(t, func(session string) { runTmuxSessionEnd(t, session) })

				var err error
				stderr := captureStderrDuringFn(t, func() { err = run() })

				if err != nil {
					t.Fatalf("cleanup: %v", err)
				}
				if childHookRuns != 1 {
					t.Fatalf("child hook ran %d times, want 1", childHookRuns)
				}
				for name, s := range map[string]string{"parent": f.session, "child": c.session} {
					if st := readStatus(t, f.dbFile, s); st.InstanceID != nil {
						t.Fatalf("%s instance_id = %q after the cleanup, want cleared by the hook", name, *st.InstanceID)
					}
				}
				if strings.Contains(stderr, "keep the prism-container audit log") {
					t.Errorf("stderr warns about the child audit log:\n%s", stderr)
				}
				if aggregated, _ := spawnOutcomeRows(t, f.dbFile, hookRaceChildIID); aggregated != 1 {
					t.Errorf("child spawn_outcome rows with aggregated_at = %d, want 1", aggregated)
				}
				if aggregated, total := spawnOutcomeRows(t, f.dbFile, archiveOrderIID); aggregated != 1 || total != 2 {
					t.Errorf("spawn_outcome rows: %d aggregated for the parent, %d in total, want 1 and 2", aggregated, total)
				}
				if path.name == "closeSession" {
					return
				}

				for label, dir := range map[string]string{"work dir": c.workDir, "TMPDIR": c.tmpDir, "podman-proxy audit dir": c.proxyAuditDir} {
					if _, err := os.Stat(dir); !os.IsNotExist(err) {
						t.Errorf("child %s %s: %v, want removed", label, dir, err)
					}
				}
				if container.PrismContainerAuditDirExists(hookRaceChildIID) {
					t.Errorf("child prism-container audit dir stays")
				}
				archived, err := os.ReadFile(filepath.Join(assertTranscriptArchived(t, f), "prism-container-audit.log"))
				switch {
				case withAudit && (err != nil || !strings.Contains(string(archived), c.auditLine)):
					t.Errorf("archived prism-container-audit.log = %q (%v), want the child line %q", archived, err, c.auditLine)
				case !withAudit && !os.IsNotExist(err):
					t.Errorf("archived prism-container-audit.log = %q (%v), want none for a child with no audit log", archived, err)
				}
			})
		}
	}
}

// AC (#3279): when the hook has not run, the cleanup writes one
// spawn_outcome row for the child incarnation and one for the parent.
func TestCleanupPaths_ChildSpawnOutcomeNoHook_3279(t *testing.T) {
	for _, path := range snapshotCleanupPaths {
		t.Run(path.name, func(t *testing.T) {
			f, run := path.seed(t)
			c := seedHookRaceChild(t, f, false)
			withHookAfterTmuxKill(t, func(string) {})

			if err := run(); err != nil {
				t.Fatalf("cleanup: %v", err)
			}

			if st := readStatus(t, f.dbFile, c.session); st.InstanceID == nil || *st.InstanceID != hookRaceChildIID {
				t.Fatalf("child instance_id = %v, want %s: no hook ran", st.InstanceID, hookRaceChildIID)
			}
			if aggregated, _ := spawnOutcomeRows(t, f.dbFile, hookRaceChildIID); aggregated != 1 {
				t.Errorf("child spawn_outcome rows with aggregated_at = %d, want 1", aggregated)
			}
			if aggregated, total := spawnOutcomeRows(t, f.dbFile, archiveOrderIID); aggregated != 1 || total != 2 {
				t.Errorf("spawn_outcome rows: %d aggregated for the parent, %d in total, want 1 and 2", aggregated, total)
			}
		})
	}
}
