package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "prism",
	Short: "Prism — tmux-based AI development environment",
	// SilenceErrors + SilenceUsage make main.go the sole error printer.
	// Without these, cobra prints
	// `Error: <msg>` and the full usage block on every RunE failure, and
	// main.go then prints the message a second time.
	//
	// Cobra has two error paths, and this pair covers both:
	//
	//   1. Command/flag discovery (`Find`): unknown top-level command or
	//      global flag parse error. Cobra's Find path unconditionally
	//      prints `Run '<cmd> --help' for usage.` regardless of
	//      SilenceUsage, so top-level unknown-flag errors still surface
	//      one-line usage guidance. SilenceErrors suppresses the
	//      duplicate `Error: <msg>` prefix so main.go remains the sole
	//      printer of the message.
	//
	//   2. Command execution (`execute`): RunE failures and subcommand
	//      flag-parse errors flow here. SilenceUsage + SilenceErrors on
	//      root suppress the usage-block dump and duplicate error print
	//      for RunE failures. For genuine flag-parse errors we still want
	//      usage — the SetFlagErrorFunc below prints the offending
	//      subcommand's usage explicitly so unknown-flag misuse still gets
	//      help text while RunE failures stay quiet.
	SilenceErrors: true,
	SilenceUsage:  true,
}

func init() {
	// Ensure genuine flag-parse errors on any subcommand still surface
	// usage guidance. Cobra checks BOTH the
	// offending command's and the root's SilenceUsage — both must be
	// false for the usage block to print — so we print the usage
	// ourselves from FlagErrorFunc and rely on main.go to print the
	// error message exactly once. This keeps RunE failures quiet
	// (no usage) while unknown-flag errors still get help text.
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		c.Println(c.UsageString())
		return err
	})
}

func Execute() error {
	return rootCmd.Execute()
}
