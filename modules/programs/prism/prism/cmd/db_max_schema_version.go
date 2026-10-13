package cmd

// prism db max-schema-version — print the highest database schema version
// that this prism binary supports.
//
// db.Open runs this verb on the prism on PATH when the database is newer than
// the running binary. The answer tells it whether a restart corrects the
// error. The verb does not open the database and does not proxy to the host
// API, so it works when the database refuses this binary.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var dbMaxSchemaVersionCmd = &cobra.Command{
	Use:    "max-schema-version",
	Short:  "Print the highest database schema version that this prism binary supports",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), db.MaxSchemaVersion())
		return err
	},
}

func init() {
	dbCmd.AddCommand(dbMaxSchemaVersionCmd)
}
