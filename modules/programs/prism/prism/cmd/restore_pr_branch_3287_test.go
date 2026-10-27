package cmd

// restore_pr_branch_3287_test.go — `prism restore` starts a PR-branch
// session again with --no-approve (issue #3287).

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestRestoreSession_PRBranchSessionKeepsNoApprove restores a session of the
// trusted repo home-ops in each isolation mode, with and without a
// PR-branch record, and reads the agent command from the tmux pane. The
// seeded row has no instance ID, so restore starts a new incarnation: the
// record must not depend on the instance ID.
func TestRestoreSession_PRBranchSessionKeepsNoApprove(t *testing.T) {
	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		for _, marked := range []bool{true, false} {
			name := mode + "/no record"
			if marked {
				name = mode + "/record"
			}
			t.Run(name, func(t *testing.T) {
				// Uses withCmdServer — must not run in parallel.
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				stubNvimOnPath(t)
				s := newCmdTestServer(t)
				withCmdServer(t, s)
				keepDeadPanes(s)

				l := projecttrusttest.New(t, "home-ops")
				wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "renovate-x")
				dbFile := filepath.Join(t.TempDir(), "prism.db")
				d, err := db.Open(dbFile)
				if err != nil {
					t.Fatalf("db.Open: %v", err)
				}
				t.Cleanup(func() { d.Close() })
				session.SetTestDBPath(dbFile)
				t.Cleanup(func() { session.SetTestDBPath("") })

				const sessionName = "home-ops@renovate-x"
				if err := d.UpsertStatus(sessionName, "home-ops", wt, "idle", nil, nil); err != nil {
					t.Fatalf("UpsertStatus: %v", err)
				}
				if err := d.SetIsolationMode(sessionName, mode); err != nil {
					t.Fatalf("SetIsolationMode: %v", err)
				}
				if marked {
					if err := d.MarkPRBranchSession(sessionName, wt, "12"); err != nil {
						t.Fatalf("MarkPRBranchSession: %v", err)
					}
				}
				status, err := d.CurrentStatus(sessionName)
				if err != nil || status == nil {
					t.Fatalf("CurrentStatus: (%v, %v)", status, err)
				}

				if err := callRestoreSession(d, *status); err != nil {
					t.Fatalf("restoreSession: %v", err)
				}
				t.Cleanup(func() { session.KillSidecar(sessionName) })
				if !s.hasSession(sessionName) {
					t.Fatalf("session %q was not restored", sessionName)
				}

				pane := agentPaneStartCmd(t, s, sessionName)
				if approves := commandApproves(t, pane, mode, wt); approves == marked {
					t.Errorf("restored agent command gives --approve = %v, want %v:\n%s", approves, !marked, pane)
				}
			})
		}
	}
}
