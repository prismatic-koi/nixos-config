package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// testDBPath overrides dbPath() during tests. Set via SetTestDBPath.
var testDBPath string

// SetTestDBPath overrides the DB path used by openDB. Only for use in tests.
// Call t.Cleanup(func() { SetTestDBPath("") }) to restore after each test.
func SetTestDBPath(p string) { testDBPath = p }

// dbPath returns the path to prism.db, honouring $XDG_STATE_HOME.
// During tests, testDBPath (set via SetTestDBPath) takes precedence.
func dbPath() string {
	if testDBPath != "" {
		return testDBPath
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "prism.db")
}

// openDB opens prism.db, returning it or an error.
func openDB() (*db.DB, error) {
	return db.Open(dbPath())
}

// dbCmd is the parent cobra command for the read-only db surface (#1467).
// Subcommands are registered in their own files (db_query.go, db_schema.go,
// db_tables.go).
var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Read-only SQL query and schema introspection over the prism database",
	Long: `Read-only SQL query and schema introspection over the prism database.

The "prism db" surface exposes the prism SQLite database for ad-hoc forensic
queries by humans and agents debugging prism itself. The curated views
(prism stats, prism checkin) answer most questions; this surface fills the
gap when raw SQL is needed.

All subcommands open the database in read-only mode (?mode=ro). Any write
statement is rejected by SQLite with a clear "read-only" error.

When PRISM_HOST_API is set (i.e. running inside a sandbox), the subcommands
proxy through the host-API socket so they work identically inside and
outside a sandbox.`,
}

func init() {
	rootCmd.AddCommand(dbCmd)
}

// activeSessionNamesForError returns a capped list of active session names
// suitable for inclusion in enum-shaped error messages (e.g.
// "session must be one of: A, B (got: \"C\")").
// cap controls the maximum number of names returned. When the full list is
// longer, an "...and N more" sentinel is appended.
// d may be nil or produce an error — in both cases an empty slice is returned
// so callers can safely handle the no-DB case.
func activeSessionNamesForError(d *db.DB, cap int) ([]string, error) {
	if d == nil {
		return nil, nil
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(statuses))
	for _, s := range statuses {
		names = append(names, s.SessionName)
	}
	if len(names) <= cap {
		return names, nil
	}
	truncated := make([]string, cap+1)
	copy(truncated, names[:cap])
	truncated[cap] = fmt.Sprintf("...and %d more", len(names)-cap)
	return truncated, nil
}

