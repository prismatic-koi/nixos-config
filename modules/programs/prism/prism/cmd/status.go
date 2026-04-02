package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print agent session counts",
	Long: `Print counts of agent sessions by state.

Without flags, prints a human-readable summary of all states.
Use --waiting to restrict output to the waiting count, and
--tmux-format to emit a tmux-style colour-formatted string
suitable for embedding in status-right.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		waitingOnly, _ := cmd.Flags().GetBool("waiting")
		tmuxFormat, _ := cmd.Flags().GetBool("tmux-format")

		d, err := openDB()
		if err != nil {
			// Silently produce no output — status bar should not show errors.
			return nil
		}
		defer d.Close()

		statuses, err := d.AllActiveStatus()
		if err != nil {
			// Silently produce no output — status bar should not show errors.
			return nil
		}

		var nActive, nWaiting, nFinished, nIdle int
		for _, s := range statuses {
			switch agent.AgentState(s.State) {
			case agent.StateActive:
				nActive++
			case agent.StateWaiting:
				nWaiting++
			case agent.StateFinished:
				nFinished++
			default:
				nIdle++
			}
		}

		if waitingOnly {
			if tmuxFormat {
				// Emit a tmux-formatted colour string, or nothing if count is zero.
				if nWaiting > 0 {
					fmt.Printf("#[fg=%s]%d waiting #[fg=%s]| ", ColorYellow, nWaiting, ColorPrimary)
				}
			} else {
				fmt.Println(nWaiting)
			}
			return nil
		}

		// Default: human-readable summary of all states.
		if tmuxFormat {
			var parts []string
			if nWaiting > 0 {
				parts = append(parts, fmt.Sprintf("#[fg=%s]%d waiting", ColorYellow, nWaiting))
			}
			if nActive > 0 {
				parts = append(parts, fmt.Sprintf("#[fg=%s]%d active", ColorPurple, nActive))
			}
			if nFinished > 0 {
				parts = append(parts, fmt.Sprintf("#[fg=%s]%d done", ColorGreen, nFinished))
			}
			if len(parts) > 0 {
				fmt.Printf("%s #[fg=%s]| ", strings.Join(parts, " "), ColorPrimary)
			}
		} else {
			var parts []string
			if nActive > 0 {
				parts = append(parts, fmt.Sprintf("%d active", nActive))
			}
			if nWaiting > 0 {
				parts = append(parts, fmt.Sprintf("%d waiting", nWaiting))
			}
			if nFinished > 0 {
				parts = append(parts, fmt.Sprintf("%d done", nFinished))
			}
			if nIdle > 0 || len(parts) == 0 {
				parts = append(parts, fmt.Sprintf("%d idle", nIdle))
			}
			fmt.Println(strings.Join(parts, "  "))
		}
		return nil
	},
}

func init() {
	statusCmd.Flags().Bool("waiting", false, "Only output the waiting count")
	statusCmd.Flags().Bool("tmux-format", false, "Emit tmux #[fg=...] colour-formatted output")
	rootCmd.AddCommand(statusCmd)
}
