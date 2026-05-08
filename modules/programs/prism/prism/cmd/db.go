package cmd

import (
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

