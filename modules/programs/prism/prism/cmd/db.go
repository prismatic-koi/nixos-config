package cmd

import (
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/db"
)

// dbPath returns the path to prism.db, honouring $XDG_STATE_HOME.
func dbPath() string {
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
