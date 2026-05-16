package main

// db_tables.go — `iris db tables` subcommand.
//
// Mirrors prism's cmd/db_tables.go. Prints a sorted list of user table
// names (sqlite_* internal objects are excluded). Used by operators and
// agents to discover the schema before writing a query.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var dbTablesCmd = &cobra.Command{
	Use:   "tables",
	Short: "Print a sorted list of user table names in the iris database (excludes sqlite_*)",
	Long: `Print a sorted list of user table names in the iris database.

Internal sqlite_* objects (sqlite_master, sqlite_sequence, etc.) are
excluded. The list is sorted lexicographically.

This subcommand is read-only: it opens the iris DB in SQLite read-only
mode and runs a single SELECT against sqlite_master. It is safe to run
while the iris daemon is operating on the same DB — SQLite's WAL mode
(set by the daemon) supports concurrent readers without blocking the
daemon's writes.

Use --json to emit a JSON array of strings (e.g. ["agent_events",
"sessions"]) suitable for scripting. The default human-readable form
prints one name per line.`,
	Args:          cobra.NoArgs,
	RunE:          runDBTables,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	dbTablesCmd.Flags().Bool("json", false, "Emit a JSON array of strings instead of one name per line")
	dbCmd.AddCommand(dbTablesCmd)
}

func runDBTables(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")

	dbPath := resolveDBPath()
	if err := ensureDBExists(dbPath); err != nil {
		return fmt.Errorf("iris db tables: %w", err)
	}

	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("iris db tables: %w", err)
	}
	defer conn.Close()

	names, err := db.Tables(context.Background(), conn)
	if err != nil {
		return fmt.Errorf("iris db tables: %w", err)
	}
	return renderDBTables(cmd.OutOrStdout(), names, jsonMode)
}

// renderDBTables writes the table list to w. JSON mode emits a JSON array
// of strings (with a non-nil zero-length array when the DB has no user
// tables, so the output is always a parseable JSON value). Text mode
// prints one name per line and emits nothing for an empty list.
func renderDBTables(w io.Writer, names []string, jsonMode bool) error {
	if jsonMode {
		if names == nil {
			names = []string{}
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(names)
	}
	for _, n := range names {
		fmt.Fprintln(w, n)
	}
	return nil
}
