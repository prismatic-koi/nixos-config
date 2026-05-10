// Package cmd provides the CLI command tree for the atlassian binary.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "atlassian",
	Short: "atlassian — read-only CLI for Jira Cloud and Confluence Cloud",
	Long: `atlassian provides read-only access to Jira Cloud and Confluence Cloud.

Required environment variables:
  ATLASSIAN_SITE         e.g. mycompany.atlassian.net
  ATLASSIAN_EMAIL        your Atlassian account email
  ATLASSIAN_API_TOKEN    your API token (from id.atlassian.com)

All data is written to stdout as JSON.
All errors and diagnostics go to stderr.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(whoamiCmd)
	rootCmd.AddCommand(resourcesCmd)
	rootCmd.AddCommand(jiraCmd)
	rootCmd.AddCommand(confluenceCmd)
}

// printJSON serialises v to stdout as indented JSON.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// dieErr prints a one-line error to stderr and exits non-zero.
func dieErr(err error) {
	fmt.Fprintln(os.Stderr, err.Error())
	os.Exit(1)
}
