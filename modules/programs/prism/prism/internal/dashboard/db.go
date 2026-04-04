package dashboard

import (
	"os"
	"path/filepath"

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

// defaultOpenDB opens prism.db, returning it or an error.
func defaultOpenDB() (*db.DB, error) {
	return db.Open(dbPath())
}
