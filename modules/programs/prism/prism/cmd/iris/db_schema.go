package main

// db_schema.go — `iris db schema` subcommand.
//
// Mirrors prism's cmd/db_schema.go with one surface tweak called out by
// issue #1698: iris requires either a <table> argument or the --all flag;
// it does not default to "print everything" on a bare invocation. This
// matches the issue's AC wording verbatim:
//
//   iris db schema [table]      # show CREATE TABLE for the named table
//   iris db schema --all        # show schema for every table
//
// Either way, output is deterministic: tables before indexes, alpha
// within each group. Internal sqlite_* objects are excluded by the
// underlying db.Schema primitive.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var dbSchemaCmd = &cobra.Command{
	Use:   "schema [table]",
	Short: "Print CREATE TABLE / CREATE INDEX statements from the iris database",
	Long: `Print CREATE TABLE / CREATE INDEX statements from the iris database.

Either a <table> argument or --all is required:

  iris db schema sessions     # print the CREATE TABLE for the sessions table
  iris db schema --all        # print DDL for every user table and index

Output is deterministic: tables before indexes, sorted alphabetically
within each group. Internal sqlite_* objects (sqlite_master,
sqlite_sequence, …) are excluded. Auto-generated indexes for PRIMARY
KEY / UNIQUE constraints — which have no printable DDL in sqlite_master
— are also excluded.

This subcommand is read-only: it opens the iris DB in SQLite read-only
mode and only queries sqlite_master. It runs safely alongside the iris
daemon (SQLite WAL mode supports concurrent readers).

Use --json to emit a JSON object keyed by table/index name with the
CREATE statement as the value. The default human-readable form prints
each CREATE statement on its own (with a trailing semicolon, so the
output is paste-able into sqlite3).`,
	Args:          cobra.MaximumNArgs(1),
	RunE:          runDBSchema,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	dbSchemaCmd.Flags().Bool("json", false, "Emit a JSON object keyed by table/index name instead of plain text")
	dbSchemaCmd.Flags().Bool("all", false, "Print the schema for every user table and index (mutually exclusive with a <table> argument)")
	dbCmd.AddCommand(dbSchemaCmd)
}

func runDBSchema(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	allMode, _ := cmd.Flags().GetBool("all")

	tableFilter := ""
	if len(args) == 1 {
		tableFilter = args[0]
	}

	// Require either an argument or --all. Both is a usage error so the
	// operator doesn't write `iris db schema sessions --all` and wonder
	// which one wins.
	switch {
	case tableFilter == "" && !allMode:
		return fmt.Errorf("iris db schema: a <table> argument or --all is required")
	case tableFilter != "" && allMode:
		return fmt.Errorf("iris db schema: --all is mutually exclusive with a <table> argument")
	}

	dbPath := resolveDBPath()
	if err := ensureDBExists(dbPath); err != nil {
		return fmt.Errorf("iris db schema: %w", err)
	}

	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("iris db schema: %w", err)
	}
	defer conn.Close()

	entries, err := db.Schema(context.Background(), conn, tableFilter)
	if err != nil {
		return fmt.Errorf("iris db schema: %w", err)
	}
	if tableFilter != "" && len(entries) == 0 {
		return fmt.Errorf("iris db schema: table %q not found", tableFilter)
	}
	return renderDBSchema(cmd.OutOrStdout(), entries, jsonMode)
}

// renderDBSchema writes entries to w. JSON mode emits an object keyed by
// the entry name (matches the issue's "for schema, an object keyed by
// table name"). Text mode prints each CREATE statement followed by a
// blank line — and adds a trailing semicolon when sqlite_master omits one,
// so the output pastes cleanly into sqlite3.
func renderDBSchema(w io.Writer, entries []db.SchemaEntry, jsonMode bool) error {
	if jsonMode {
		out := make(map[string]string, len(entries))
		for _, e := range entries {
			out[e.Name] = e.SQL
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	for _, e := range entries {
		text := strings.TrimSpace(e.SQL)
		if !strings.HasSuffix(text, ";") {
			text += ";"
		}
		fmt.Fprintln(w, text)
		fmt.Fprintln(w)
	}
	return nil
}
