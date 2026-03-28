package cmd

import (
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/tmux"
	"github.com/spf13/cobra"
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

		sessions, err := tmux.Sessions()
		if err != nil {
			// Silently produce no output — status bar should not show errors.
			return nil
		}
		sessions = filterStatusSessions(sessions)

		var nActive, nWaiting, nFinished, nIdle int
		for _, s := range sessions {
			switch s.AgentState {
			case "active":
				nActive++
			case "waiting":
				nWaiting++
			case "finished":
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

// filterStatusSessions excludes infrastructure sessions from status counts.
func filterStatusSessions(all []tmux.Session) []tmux.Session {
	var out []tmux.Session
	for _, s := range all {
		if s.Name == "scratchpad" || s.Name == "prism-dashboard" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func init() {
	statusCmd.Flags().Bool("waiting", false, "Only output the waiting count")
	statusCmd.Flags().Bool("tmux-format", false, "Emit tmux #[fg=...] colour-formatted output")
	rootCmd.AddCommand(statusCmd)
}
