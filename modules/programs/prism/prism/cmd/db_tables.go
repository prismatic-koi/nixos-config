package cmd

// prism db tables — print a sorted list of user table names.
//
// Convenience wrapper over `SELECT name FROM sqlite_master WHERE type='table'
// AND name NOT LIKE 'sqlite_%' ORDER BY name`. Used by agents and humans to
// discover the schema before writing a query.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var dbTablesCmd = &cobra.Command{
	Use:   "tables",
	Short: "Print a sorted list of user table names (excludes sqlite_*)",
	Args:  cobra.NoArgs,
	RunE:  runDBTables,
}

func init() {
	dbTablesCmd.Flags().Bool("json", false, "Emit a JSON array of strings instead of one name per line")
	dbCmd.AddCommand(dbTablesCmd)
}

func runDBTables(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runDBTablesProxy(cmd, apiURL, jsonMode)
	}
	return runDBTablesLocal(cmd, jsonMode)
}

func runDBTablesLocal(cmd *cobra.Command, jsonMode bool) error {
	conn, err := db.OpenReadOnly(dbPath())
	if err != nil {
		return fmt.Errorf("db tables: %w", err)
	}
	defer conn.Close()

	names, err := db.Tables(context.Background(), conn)
	if err != nil {
		return fmt.Errorf("db tables: %w", err)
	}
	return renderDBTables(cmd.OutOrStdout(), names, jsonMode)
}

func runDBTablesProxy(cmd *cobra.Command, apiURL string, jsonMode bool) error {
	raw, err := proxyReadToHostAPI(apiURL, "/db/tables", nil)
	if err != nil {
		return fmt.Errorf("db tables: %w", err)
	}
	var resp struct {
		Tables []string `json:"tables"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("db tables: unmarshal proxy response: %w", err)
	}
	return renderDBTables(cmd.OutOrStdout(), resp.Tables, jsonMode)
}

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
