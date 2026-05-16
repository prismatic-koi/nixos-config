package main

// db.go — `iris db` subcommand group.
//
// Implements issue #1698. The `iris db` family is the iris analogue of the
// prism `db` surface (cmd/db.go + cmd/db_query.go + cmd/db_schema.go +
// cmd/db_tables.go). It provides ad-hoc, read-only introspection over the
// iris SQLite database (~/.local/state/iris/iris.db by default), so an
// operator can debug iris session-state problems without dropping into the
// raw `sqlite3` binary.
//
// # Read-only by design
//
// Iris adds a defence-in-depth layer over prism's approach: before opening
// the DB at all, the `iris db query` command runs a keyword-based pre-check
// (see isWriteStatement in db_query.go) that rejects INSERT / UPDATE /
// DELETE / DROP / CREATE / ALTER and friends with a clear error. The DB is
// then opened in SQLite read-only mode (?mode=ro via db.OpenReadOnly),
// which is itself enough to prevent writes — the pre-check exists so the
// failure path is unambiguous and the error message points the user at
// `sqlite3` for legitimate write workflows.
//
// # Daemon coexistence
//
// SQLite's WAL mode (set by the daemon via db.Open) supports multiple
// concurrent readers alongside a single writer, so `iris db query` can
// safely read the DB while the iris daemon has it open for writes. Each
// subcommand opens its own short-lived read-only connection (via
// db.OpenReadOnly) and closes it on completion — no shared handle, no
// long-lived locks.
//
// # No daemon socket
//
// Unlike `iris sessions list` (which dials the daemon's client socket),
// the `iris db` family reads the DB file directly. This mirrors the
// pattern established by `iris checkin` (issue #1676): read-only DB
// inspection is a debug surface and the all-surfaces-go-through-daemon
// rule (#1668 / #1669) only applies to mutations and shared mutable
// state. The user does not need the daemon running to inspect the DB.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// testDBPath overrides resolveDBPath() during tests. Set via SetTestDBPath.
// Mirrors the same hook in prism's cmd/db.go so test infrastructure can
// point the CLI at a tempdir-backed DB without relying on env vars alone.
//
// Tests should pair calls to SetTestDBPath with a t.Cleanup that restores
// it to "" — otherwise subsequent tests in the same package see the stale
// path and exhibit confusing cross-test pollution.
var testDBPath string

// SetTestDBPath overrides the DB path used by iris db subcommands. Only for
// use in tests. Call SetTestDBPath("") (or t.Cleanup(...)) to restore the
// canonical XDG-derived path.
func SetTestDBPath(p string) { testDBPath = p }

// resolveDBPath returns the iris DB path. testDBPath wins; otherwise we
// defer to iris.ResolvePaths() — the same single source of truth used by
// every other iris CLI surface.
func resolveDBPath() string {
	if testDBPath != "" {
		return testDBPath
	}
	return iris.ResolvePaths().DB
}

// dbCmd is the parent cobra command for the read-only iris-db surface.
// Subcommands are registered in their own files' init() funcs so each
// command's flags live next to its RunE.
var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Read-only SQL query and schema introspection over the iris database",
	Long: `Read-only SQL query and schema introspection over the iris database.

The "iris db" surface exposes the iris SQLite database for ad-hoc forensic
queries by operators debugging the daemon or session state. Curated views
(iris sessions list, iris checkin) answer most questions; this surface fills
the gap when raw SQL is needed.

All subcommands are read-only by design:

  - "iris db query" rejects write statements (INSERT / UPDATE / DELETE /
    DROP / CREATE / ALTER and friends) before the database is opened, then
    opens the DB in SQLite read-only mode as a defence-in-depth measure.
    For legitimate writes, use sqlite3 directly.
  - "iris db schema" and "iris db tables" read sqlite_master only.

SQLite's WAL mode (set by the daemon) allows these read-only commands to
run safely while the iris daemon has the DB open for writes. The daemon
does not need to be stopped — concurrent readers do not interfere with
the daemon's session updates.

The DB path is resolved from $XDG_STATE_HOME (default
~/.local/state/iris/iris.db). When the file does not exist (e.g. the iris
daemon has never run), every subcommand exits non-zero with a clear
"iris database not found" error pointing at the expected path.`,
}

func init() {
	rootCmd.AddCommand(dbCmd)
}

// ensureDBExists returns an actionable error when the iris DB file does
// not exist or is otherwise unreadable. Run before db.OpenReadOnly so the
// operator sees "iris database not found" — pointing at the daemon as the
// thing that would normally create it — rather than the raw modernc.org
// "unable to open database file" message.
//
// Each `iris db <verb>` caller wraps the error with its own "iris db
// <verb>:" prefix, so this function returns an unprefixed message.
func ensureDBExists(dbPath string) error {
	if dbPath == "" {
		return fmt.Errorf("iris database path is empty (XDG_STATE_HOME misconfigured?)")
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("iris database not found at %s; has the iris daemon ever run? (start it with `systemctl --user start iris` on Linux, or `launchctl kickstart -k gui/$UID/local.iris.daemon` on Darwin)", dbPath)
		}
		return fmt.Errorf("cannot read iris database at %s: %w", dbPath, err)
	}
	return nil
}
