package projecttrust

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

func noPRBranches() ([]PRBranch, error) { return nil, nil }

func prBranches(records ...PRBranch) func() ([]PRBranch, error) {
	return func() ([]PRBranch, error) { return records, nil }
}

// TestApprove_PRBranchSession is the decision table of issue #3287. Each row
// is a session that gets --approve with no PR-branch record. A record of the
// root session, or of the worktree, gives --no-approve to the session and to
// each of its descendants.
func TestApprove_PRBranchSession(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops", "obsidian")
	prWT := projecttrusttest.Worktree(t, l.Code, "home-ops", "renovate/x")
	workerWT := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat")
	record := PRBranch{SessionName: "home-ops@renovate-x", Worktree: prWT}

	cases := []struct {
		name    string
		session string
		dir     string
		want    bool
	}{
		{"PR session", "home-ops@renovate-x", prWT, false},
		{"review agent of the PR session", "home-ops@renovate-x~review-1-review-goal", prWT, false},
		{"fallback reviewer of the PR session", "home-ops@renovate-x~review-2-review-security-fallback-1", prWT, false},
		{"investigator of the PR session", "home-ops@renovate-x~investigate-flake", prWT, false},
		{"review agent of an investigator of the PR session", "home-ops@renovate-x~investigate-flake~review-1-review-qa", prWT, false},
		{"another name in the PR worktree", "home-ops@other", prWT, false},
		{"PR worktree path with a trailing slash", "home-ops@other", prWT + "/", false},
		{"PR session name in another worktree", "home-ops@renovate-x", workerWT, false},

		{"worker of the same repo", "home-ops@feat", workerWT, true},
		{"review agent of the worker", "home-ops@feat~review-1-review-goal", workerWT, true},
		{"name with the PR session as a prefix", "home-ops@renovate-xy", workerWT, true},
		{"non-worktree project", "obsidian", l.Obsidian, true},
	}
	p := Load(prBranches(record))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Approve(tc.session, tc.dir); got != tc.want {
				t.Errorf("Approve(%q, %q) = %v, want %v", tc.session, tc.dir, got, tc.want)
			}
		})
	}

	// The same sessions with no record: every row gets --approve, so the
	// false rows above come from the record alone.
	none := Load(noPRBranches)
	for _, tc := range cases {
		if tc.name == "PR session name in another worktree" {
			continue
		}
		if !none.Approve(tc.session, tc.dir) {
			t.Errorf("with no record: Approve(%q, %q) = false, want true", tc.session, tc.dir)
		}
	}
}

// TestApprove_PRBranchWorktreeMatchesByIdentity: a record matches the
// worktree by file identity, so a different path to the PR worktree also gets
// --no-approve.
func TestApprove_PRBranchWorktreeMatchesByIdentity(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	prWT := projecttrusttest.Worktree(t, l.Code, "home-ops", "pr")
	alias := filepath.Join(l.Root, "alias")
	if err := os.Symlink(l.Root, alias); err != nil {
		t.Fatal(err)
	}
	p := Load(prBranches(PRBranch{
		SessionName: "home-ops@pr",
		Worktree:    filepath.Join(alias, "code", "home-ops", "pr"),
	}))
	if p.Approve("home-ops@other", prWT) {
		t.Error("Approve = true for the PR worktree under another path, want false")
	}
}

// TestApprove_PRBranchReaderFailsClosed: if the records cannot be read, no
// session gets --approve.
func TestApprove_PRBranchReaderFailsClosed(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "feat")

	failing := Load(func() ([]PRBranch, error) { return nil, errors.New("database is locked") })
	if failing.Approve("home-ops@feat", wt) {
		t.Error("Approve = true when the PR-branch records cannot be read, want false")
	}
	if Load(nil).Approve("home-ops@feat", wt) {
		t.Error("Approve = true with no PR-branch reader, want false")
	}
	if !Load(noPRBranches).Approve("home-ops@feat", wt) {
		t.Error("Approve = false with an empty record set, want true")
	}
}

// TestApprove_UntrustedRepoReadsNoRecords: Approve reads the records last, so
// a session of an untrusted repo opens no database.
func TestApprove_UntrustedRepoReadsNoRecords(t *testing.T) {
	l := projecttrusttest.New(t, "home-ops")
	nixos := projecttrusttest.Worktree(t, l.Code, "nixos-config", "main")
	calls := 0
	p := Load(func() ([]PRBranch, error) { calls++; return nil, nil })
	if p.Approve("nixos-config@main", nixos) {
		t.Fatal("Approve = true for an untrusted repo")
	}
	if calls != 0 {
		t.Errorf("PRBranches called %d times for an untrusted repo, want 0", calls)
	}
}
