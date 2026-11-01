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
// table of the DB at path: the name and the start dir. It opens the DB
// read-only and does not make it. If there is no DB file, it returns no
// records.
//
// Open is true when the agent_status row of the name has no end time and
// names the instance of a sessions row of the pair. That sessions row can
// have an end time. A sidecar stop ends the sessions row first
// (EndIncarnation), and a stop that fails after it keeps agent_status, for
// example while a process in the sandbox of the session still runs
// (#3375). A session that sends session_shutdown also ends its sessions
// row and keeps its sandbox.
//
// prism can leave a sessions row open after its session stops (#3027), and
// then agent_status names another instance or has an end time. If the
// agent_status row has no instance, a start is in progress, so a sessions
// row with no end time is open. If there is no agent_status row, Open is
// false: every start writes the agent_status row before the sessions row,
// and Prune removes only an agent_status row with an end time.
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
SELECT s.session_name, s.worktree,
       MAX(a.session_name IS NOT NULL AND a.ended_at IS NULL
           AND (a.instance_id = s.instance_id
                OR (a.instance_id IS NULL AND s.ended_at IS NULL)))
  FROM sessions s
  LEFT JOIN agent_status a ON a.session_name = s.session_name
 GROUP BY s.session_name, s.worktree`)
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
