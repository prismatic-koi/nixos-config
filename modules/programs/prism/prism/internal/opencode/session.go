package opencode

// session.go provides helpers for querying the opencode SQLite database.
//
// opencode stores session data at:
//
//	$XDG_DATA_HOME/opencode/opencode-stable.db   (preferred)
//	~/.local/share/opencode/opencode-stable.db   (fallback)
//
// The `session` table has a `directory` column (the worktree path) and
// `time_updated`.  We query the most-recently-updated session for a given
// directory so prism can resume it with `opencode -s <id>`.

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO required
)

// dbPath returns the resolved path to the opencode SQLite database.
// Resolution order:
//  1. $XDG_DATA_HOME/opencode/opencode-stable.db
//  2. ~/.local/share/opencode/opencode-stable.db
func dbPath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "opencode", "opencode-stable.db")
}

// LatestSessionForDir queries the opencode SQLite database and returns the most
// recently updated session ID for the given directory, or "" if none found.
// Errors are swallowed — a missing/unreadable DB is not fatal, it just means
// opencode will start a fresh session.
func LatestSessionForDir(dir string) string {
	path := dbPath()

	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()

	var id string
	err = db.QueryRow(
		`SELECT id FROM session WHERE directory = ? ORDER BY time_updated DESC LIMIT 1`,
		dir,
	).Scan(&id)
	if err != nil {
		// Includes sql.ErrNoRows — not an error we need to surface.
		return ""
	}
	return id
}
