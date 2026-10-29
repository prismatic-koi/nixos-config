package db

import (
	"errors"
	"fmt"
	"os"

	"github.com/prismatic-koi/prism/internal/git"
)

// The bare repo record of internal/git starts from the session start records
// (#3364). internal/git cannot import this package, so this package gives it
// the reader.
func init() {
	git.SetSessionStartSource(SessionStarts)
}

// SessionStarts returns the start record of each session in the sessions
// table of the DB at path: the name, the start dir, and whether a record of
// that pair has no end time. It opens the DB read-only and does not make it.
// If there is no DB file, it returns no records.
func SessionStarts(path string) ([]git.SessionStart, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	conn, err := OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(`
SELECT session_name, worktree, MAX(ended_at IS NULL)
  FROM sessions
 GROUP BY session_name, worktree`)
	if err != nil {
		return nil, fmt.Errorf("db: read session starts: %w", err)
	}
	defer rows.Close()
	var starts []git.SessionStart
	for rows.Next() {
		var s git.SessionStart
		if err := rows.Scan(&s.Name, &s.Worktree, &s.Open); err != nil {
			return nil, fmt.Errorf("db: read session starts: %w", err)
		}
		starts = append(starts, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read session starts: %w", err)
	}
	return starts, nil
}
