package session

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/projecttrust"
)

// RecordPRBranch writes the PR-branch record of the root session name before
// its agent starts (issue #3287). A non-empty pr marks the session. An empty
// pr removes an earlier record of the name, so a new `prism spawn` of the name
// with no --pr gets the same decision as any other branch.
func RecordPRBranch(d *db.DB, name, worktree, pr string) error {
	if pr == "" {
		return d.ClearPRBranchSession(name)
	}
	return d.MarkPRBranchSession(name, worktree, pr)
}

// prBranchSessions reads the PR-branch records for the project-trust
// decision.
func prBranchSessions() ([]projecttrust.PRBranch, error) {
	d, err := openDB()
	if err != nil {
		return nil, err
	}
	defer d.Close()
	rows, err := d.PRBranchSessions()
	if err != nil {
		return nil, err
	}
	out := make([]projecttrust.PRBranch, 0, len(rows))
	for _, r := range rows {
		out = append(out, projecttrust.PRBranch{SessionName: r.SessionName, Worktree: r.Worktree})
	}
	return out, nil
}
