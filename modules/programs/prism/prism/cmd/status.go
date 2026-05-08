package cmd

import (
	"encoding/json"
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
Use --waiting to restrict output to the waiting count,
--tmux-format to emit a tmux-style colour-formatted string
suitable for embedding in status-right, or --json to emit a
JSON object keyed by state with integer counts.

--json and --tmux-format are mutually exclusive.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		waitingOnly, _ := cmd.Flags().GetBool("waiting")
		tmuxFormat, _ := cmd.Flags().GetBool("tmux-format")
		jsonMode, _ := cmd.Flags().GetBool("json")

		if jsonMode && tmuxFormat {
			return fmt.Errorf("prism status: --json and --tmux-format are mutually exclusive")
		}

		d, err := openDB()
		if err != nil {
			// Silently produce no output — status bar should not show errors.
			// Exception: --json must always emit a parseable document.
			if jsonMode {
				return renderStatusJSON(0, 0, 0, 0, 0)
			}
			return nil
		}
		defer d.Close()

		statuses, err := d.AllActiveStatus()
		if err != nil {
			// Silently produce no output — status bar should not show errors.
			if jsonMode {
				return renderStatusJSON(0, 0, 0, 0, 0)
			}
			return nil
		}

		var nActive, nWaiting, nFinished, nIdle, nError int
		for _, s := range statuses {
			switch agent.AgentState(s.State) {
			case agent.StateActive:
				nActive++
			case agent.StateWaiting:
				nWaiting++
			case agent.StateFinished:
				nFinished++
			case agent.StateError:
				nError++
			default:
				nIdle++
			}
		}

		if jsonMode {
			return renderStatusJSON(nActive, nWaiting, nIdle, nFinished, nError)
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
		//
		// Errored sessions must remain visible in both renderers. Pre-#1499
		// they were folded into the `idle` bucket via the `default:` arm of
		// the state switch, so they showed up as "N idle" — the new dedicated
		// `nError` counter (added for --json) must be rendered explicitly
		// here, otherwise error sessions silently disappear from the status
		// bar. Render in red so they're visually distinct from idle.
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
			if nError > 0 {
				parts = append(parts, fmt.Sprintf("#[fg=%s]%d error", ColorRed, nError))
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
			if nError > 0 {
				parts = append(parts, fmt.Sprintf("%d error", nError))
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
	statusCmd.Flags().Bool("json", false, "Emit a JSON object keyed by state with integer counts (mutually exclusive with --tmux-format)")
	rootCmd.AddCommand(statusCmd)
}

// renderStatusJSON emits the snake_case JSON object for `prism status --json`.
// Keys: active, waiting, idle, finished, error.
func renderStatusJSON(active, waiting, idle, finished, errored int) error {
	obj := map[string]int{
		"active":   active,
		"waiting":  waiting,
		"idle":     idle,
		"finished": finished,
		"error":    errored,
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("prism status --json: marshal: %w", err)
	}
	return printJSON(data)
}
