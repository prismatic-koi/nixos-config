package cmd

// prism monitor-review — internal daemon that watches a review group and
// delivers results to the worker session when the group completes.
//
// This command is launched as a detached subprocess by `prism review`.
// It is not intended for direct use; its flags and behaviour are internal.
//
// Flags:
//
//	--opts-file <path>   JSON file containing MonitorOpts (required)

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/review"
)

var monitorReviewCmd = &cobra.Command{
	Use:    "monitor-review",
	Short:  "Internal: watch a review group and deliver results to the worker (not for direct use)",
	Hidden: true, // not shown in help
	Args:   cobra.NoArgs,
	RunE:   runMonitorReview,
}

func init() {
	monitorReviewCmd.Flags().String("opts-file", "", "Path to JSON file containing MonitorOpts (required)")
	_ = monitorReviewCmd.MarkFlagRequired("opts-file")
	rootCmd.AddCommand(monitorReviewCmd)
}

func runMonitorReview(cmd *cobra.Command, _ []string) error {
	optsFile, _ := cmd.Flags().GetString("opts-file")
	if optsFile == "" {
		return fmt.Errorf("monitor-review: --opts-file is required")
	}

	opts, err := review.LoadMonitorOptsFromFile(optsFile)
	if err != nil {
		return fmt.Errorf("monitor-review: load opts: %w", err)
	}

	// Clean up the temp opts file immediately — we have the opts in memory.
	_ = os.Remove(optsFile)

	if monErr := review.MonitorFunc(opts); monErr != nil {
		// Log the error — the process is detached so nothing else reads this.
		fmt.Fprintf(os.Stderr, "[prism monitor-review] error: %v\n", monErr)
		os.Exit(1)
	}
	return nil
}
