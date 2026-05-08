package cmd

// prism db schema — print CREATE TABLE / CREATE INDEX statements.
//
// With no argument, prints every user table and index. With one argument,
// prints only that table's DDL. Output is deterministic across invocations
// (tables before indexes, alpha within each group) per the AC.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var dbSchemaCmd = &cobra.Command{
	Use:          "schema [table]",
	Short:        "Print CREATE TABLE / CREATE INDEX statements",
	SilenceUsage: true,
	Long: `Print CREATE TABLE / CREATE INDEX statements from the prism database.

With no argument, prints DDL for every user table and index. With a single
argument, prints only that table's CREATE TABLE statement.

Output is deterministic: tables before indexes, sorted alphabetically within
each group. Internal sqlite_* objects are excluded.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDBSchema,
}

func init() {
	dbSchemaCmd.Flags().Bool("json", false, "Emit a JSON object keyed by table/index name instead of plain text")
	dbCmd.AddCommand(dbSchemaCmd)
}

func runDBSchema(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	tableFilter := ""
	if len(args) == 1 {
		tableFilter = args[0]
	}

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runDBSchemaProxy(cmd, apiURL, tableFilter, jsonMode)
	}
	return runDBSchemaLocal(cmd, tableFilter, jsonMode)
}

func runDBSchemaLocal(cmd *cobra.Command, tableFilter string, jsonMode bool) error {
	conn, err := db.OpenReadOnly(dbPath())
	if err != nil {
		return fmt.Errorf("db schema: %w", err)
	}
	defer conn.Close()

	entries, err := db.Schema(context.Background(), conn, tableFilter)
	if err != nil {
		return fmt.Errorf("db schema: %w", err)
	}
	if tableFilter != "" && len(entries) == 0 {
		return fmt.Errorf("db schema: table %q not found", tableFilter)
	}
	return renderDBSchema(cmd.OutOrStdout(), entries, jsonMode)
}

func runDBSchemaProxy(cmd *cobra.Command, apiURL, tableFilter string, jsonMode bool) error {
	params := map[string]string{}
	if tableFilter != "" {
		params["table"] = tableFilter
	}
	raw, err := proxyReadToHostAPI(apiURL, "/db/schema", params)
	if err != nil {
		return fmt.Errorf("db schema: %w", err)
	}
	var resp struct {
		Entries []db.SchemaEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("db schema: unmarshal proxy response: %w", err)
	}
	return renderDBSchema(cmd.OutOrStdout(), resp.Entries, jsonMode)
}

// renderDBSchema writes entries to w. JSON mode emits an object keyed by the
// entry name (matches the issue's "for schema, an object keyed by table
// name"). Text mode prints each CREATE statement followed by a blank line.
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
		// Some sqlite_master rows omit the trailing semicolon; add one so
		// the output is paste-able into sqlite3.
		text := strings.TrimSpace(e.SQL)
		if !strings.HasSuffix(text, ";") {
			text += ";"
		}
		fmt.Fprintln(w, text)
		fmt.Fprintln(w)
	}
	return nil
}
