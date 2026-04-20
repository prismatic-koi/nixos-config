package cmd

// prism quick <action> — lightweight AI-driven utility commands.
//
// Currently supported actions:
//
//	pr   Generate a PR description from staged changes, commit, push,
//	     create a GitHub PR, switch back to main, and open the PR in
//	     the system browser.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/quick"
)

var quickCmd = &cobra.Command{
	Use:   "quick <action>",
	Short: "Lightweight AI-driven utility commands",
	Long: `Run a fast AI-assisted action without spinning up a full agent session.

Available actions:
  pr   Generate a PR description from staged changes, commit, push,
       create a GitHub PR, and open it in the browser.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		action := args[0]
		switch action {
		case "pr":
			return quick.Run()
		default:
			return fmt.Errorf("unknown quick action %q — available: pr", action)
		}
	},
}

func init() {
	rootCmd.AddCommand(quickCmd)
}
