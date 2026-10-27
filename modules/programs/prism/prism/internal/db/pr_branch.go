package db

import (
	"fmt"
	"time"
)

// PRBranchSession is a row of pr_branch_sessions: a root session that prism
// started on the branch of an existing PR (issue #3287).
type PRBranchSession struct {
	SessionName string
	Worktree    string
	// PR is the --pr value: a PR number, a URL, or a branch.
	PR        string
	CreatedAt int64
}

// MarkPRBranchSession records sessionName as a session on the branch of PR
// pr, in worktree. It replaces an earlier record of sessionName.
func (d *DB) MarkPRBranchSession(sessionName, worktree, pr string) error {
	if sessionName == "" || worktree == "" || pr == "" {
		return fmt.Errorf("db: mark PR-branch session: session name, worktree, and PR are required")
	}
	if _, err := d.conn.Exec(`
INSERT INTO pr_branch_sessions (session_name, worktree, pr, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(session_name) DO UPDATE SET
  worktree   = excluded.worktree,
  pr         = excluded.pr,
  created_at = excluded.created_at`,
		sessionName, worktree, pr, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("db: mark PR-branch session %q: %w", sessionName, err)
	}
	return nil
}

// ClearPRBranchSession removes the record of sessionName. It is not an error
// when sessionName has no record.
func (d *DB) ClearPRBranchSession(sessionName string) error {
	if _, err := d.conn.Exec(`DELETE FROM pr_branch_sessions WHERE session_name = ?`, sessionName); err != nil {
		return fmt.Errorf("db: clear PR-branch session %q: %w", sessionName, err)
	}
	return nil
}

// PRBranchSessions returns every record of pr_branch_sessions.
func (d *DB) PRBranchSessions() ([]PRBranchSession, error) {
	rows, err := d.conn.Query(`SELECT session_name, worktree, pr, created_at FROM pr_branch_sessions ORDER BY session_name`)
	if err != nil {
		return nil, fmt.Errorf("db: read PR-branch sessions: %w", err)
	}
	defer rows.Close()
	var out []PRBranchSession
	for rows.Next() {
		var s PRBranchSession
		if err := rows.Scan(&s.SessionName, &s.Worktree, &s.PR, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: read PR-branch sessions: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read PR-branch sessions: %w", err)
	}
	return out, nil
}
