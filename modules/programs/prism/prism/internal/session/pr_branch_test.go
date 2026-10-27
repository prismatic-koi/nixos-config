package session

// pr_branch_test.go — a session that prism started on the branch of an
// existing PR, and each of its descendants, get --no-approve in a trusted
// repo (issue #3287).

import (
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

var trustModes = []string{"host", "bwrap", "sandbox-exec"}

// prDescendants are the names of the sessions that start in the worktree of
// the PR session root: its review agents, a fallback reviewer, and its
// investigators.
func prDescendants(root string) []string {
	return []string{
		root + "~review-1-review-goal",
		root + "~review-2-review-security-fallback-1",
		root + "~investigate-flake",
		root + "~investigate-flake~review-1-review-qa",
	}
}

// TestBuildAgentCmd_PRBranchSession: with a PR-branch record, BuildAgentCmd
// gives the PR session and each of its descendants --no-approve in every
// isolation mode. With no record, the same calls give --approve, so the
// record alone changes the result.
func TestBuildAgentCmd_PRBranchSession(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	l := projecttrusttest.New(t, "home-ops")
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "renovate-x")
	const root = "home-ops@renovate-x"
	names := append([]string{root}, prDescendants(root)...)

	check := func(t *testing.T, want bool) {
		t.Helper()
		for _, name := range names {
			for _, mode := range trustModes {
				cmd, err := BuildAgentCmd(Opts{
					SessionName:   name,
					Worktree:      wt,
					IsolationMode: mode,
					HarnessName:   "pi",
				})
				if err != nil {
					t.Fatalf("BuildAgentCmd(%s, %s): %v", name, mode, err)
				}
				t.Run(name+"/"+mode, func(t *testing.T) { assertTrust(t, mode, cmd, wt, want) })
			}
		}
	}

	t.Run("no record", func(t *testing.T) { check(t, true) })
	if err := d.MarkPRBranchSession(root, wt, "12"); err != nil {
		t.Fatalf("MarkPRBranchSession: %v", err)
	}
	t.Run("record", func(t *testing.T) { check(t, false) })
}

// TestBuildAgentCmd_PRBranchDBUnreadable: if prism.db cannot be opened, a
// session of a trusted repo gets --no-approve.
func TestBuildAgentCmd_PRBranchDBUnreadable(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat")
	notADir := l.Root + "/file"
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	SetTestDBPath(notADir + "/prism.db")
	t.Cleanup(func() { SetTestDBPath("") })

	for _, mode := range trustModes {
		cmd, err := BuildAgentCmd(Opts{SessionName: "home-ops@feat", Worktree: wt, IsolationMode: mode, HarnessName: "pi"})
		if err != nil {
			t.Fatalf("BuildAgentCmd(%s): %v", mode, err)
		}
		assertTrust(t, mode, cmd, wt, false)
	}
}

// spawnTrustFlag runs SpawnSession with a spy tmux and reports whether the
// agent command that reached tmux gives pi --approve.
func spawnTrustFlag(t *testing.T, d *db.DB, opts SpawnOpts) bool {
	t.Helper()
	argsFile := spyTmuxBin(t)
	opts.Repo = "home-ops"
	opts.Prompt = "go"
	opts.HarnessName = "pi"
	opts.PIExtensionDir = testPIExtensionDir
	if opts.Layout == 0 {
		opts.Layout = LayoutAgentOnly
	}
	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession(%s): %v", opts.SessionName, err)
	}
	joined := strings.Join(readSpyArgs(argsFile), " ")
	if !strings.Contains(joined, "new-window") {
		t.Fatalf("SpawnSession(%s) created no agent window; tmux args: %s", opts.SessionName, joined)
	}
	if opts.IsolationMode == "host" {
		flags := trustFlags(joined)
		if len(flags) != 1 {
			t.Fatalf("host agent command has project-trust flags %q, want exactly one: %s", flags, joined)
		}
		return flags[0] == "--approve"
	}
	return strings.Contains(joined, "--approve-project-dir '"+opts.Worktree+"'")
}

// TestSpawnSession_RecordsPRBranchBeforeTheAgentStarts: SpawnSession writes
// the record of a PR-branch root session before it builds the agent
// command, so the first start gets --no-approve. Its review agents and
// investigators get --no-approve. A later spawn of the same name with no
// PRRef removes the record and gets --approve.
func TestSpawnSession_RecordsPRBranchBeforeTheAgentStarts(t *testing.T) {
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	for _, mode := range trustModes {
		t.Run(mode, func(t *testing.T) {
			d, _ := openSpawnTestDB(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			l := projecttrusttest.New(t, "home-ops")
			wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "renovate-x")
			const root = "home-ops@renovate-x"

			if spawnTrustFlag(t, d, SpawnOpts{SessionName: root, Worktree: wt, AgentRole: "worker", IsolationMode: mode, PRRef: "12", Layout: LayoutFull}) {
				t.Errorf("PR session: agent command gives --approve, want --no-approve")
			}
			records, err := d.PRBranchSessions()
			if err != nil || len(records) != 1 || records[0].SessionName != root || records[0].Worktree != wt || records[0].PR != "12" {
				t.Fatalf("PRBranchSessions = (%+v, %v), want the record of %s", records, err, root)
			}

			for _, name := range prDescendants(root) {
				if spawnTrustFlag(t, d, SpawnOpts{SessionName: name, Worktree: wt, AgentRole: "review-goal", IsolationMode: mode}) {
					t.Errorf("%s: agent command gives --approve, want --no-approve", name)
				}
			}
			if records, err := d.PRBranchSessions(); err != nil || len(records) != 1 {
				t.Errorf("a descendant spawn changed the records: (%+v, %v)", records, err)
			}

			if err := d.SetEnded(root); err != nil {
				t.Fatalf("SetEnded: %v", err)
			}
			if !spawnTrustFlag(t, d, SpawnOpts{SessionName: root, Worktree: wt, AgentRole: "worker", IsolationMode: mode, Layout: LayoutFull}) {
				t.Errorf("a prism spawn with no --pr: agent command gives --no-approve, want --approve")
			}
			if records, err := d.PRBranchSessions(); err != nil || len(records) != 0 {
				t.Errorf("after a spawn with no --pr: PRBranchSessions = (%+v, %v), want none", records, err)
			}
		})
	}
}
