package iris

import (
	"github.com/prismatic-koi/prism/internal/db"
)

// OpenDB opens the iris SQLite database at the given path, creating the file
// and parent directories if they do not exist. The database carries the same
// schema as the prism database (§10.4: "the schema is shared by design") by
// calling into the shared db.Open function with the iris-specific path.
//
// If the database already exists, it is opened without re-running migrations
// (the migration logic inside db.Open is idempotent — migrations check the
// current schema_version and skip steps that have already been applied).
//
// The caller is responsible for closing the returned *db.DB.
func OpenDB(path string) (*db.DB, error) {
	return db.Open(path)
}
