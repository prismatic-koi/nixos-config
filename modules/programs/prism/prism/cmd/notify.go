package cmd

// prism notify <state> — replaces cli.tmux.setStatus
//
// Called by the opencode tmux-status plugin on every agent state transition.
// Reads TMUX_PANE from the environment to identify which window to update,
// then sets the window-status-format and @agent_state window options so the
// dashboard and status bar reflect the new state.
//
// On "waiting", also sends a display-message to all attached clients.

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// Colour vars are declared in dashboard.go and injected via ldflags.
// ColorBg1 is the slightly lighter background used as the notification bg.
var ColorBg1 = "#343f44"

func init() {
	rootCmd.AddCommand(notifyCmd)
}

var notifyCmd = &cobra.Command{
	Use:   "notify <state>",
	Short: "Update agent window state (called by the tmux-status plugin)",
	Args:  cobra.ExactArgs(1),
	RunE:  runNotify,
}

func runNotify(cmd *cobra.Command, args []string) error {
	// Drain stdin in background — the opencode hook pipes JSON in but we don't
	// need it, and we don't want to block on it before doing work.
	go io.ReadAll(os.Stdin) //nolint:errcheck

	if os.Getenv("TMUX") == "" {
		return nil
	}

	state := args[0]

	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return fmt.Errorf("TMUX_PANE not set")
	}

	windowID, err := tmux.WindowID(pane)
	if err != nil {
		return fmt.Errorf("get window id: %w", err)
	}

	type stateSpec struct {
		state string
		color string
	}

	specs := map[string]stateSpec{
		"set-active":     {"active", ColorPurple},
		"set-waiting":    {"waiting", ColorYellow},
		"set-finished":   {"finished", ColorGreen},
		"set-compacting": {"compacting", ColorBlue},
		"set-error":      {"error", ColorRed},
	}

	if state == "clear" {
		_ = tmux.UnsetWindowOption(windowID, "window-status-format")
		_ = tmux.UnsetWindowOption(windowID, "window-status-current-format")
		_ = tmux.UnsetWindowOption(windowID, "@agent_state")
		return nil
	}

	spec, ok := specs[state]
	if !ok {
		return fmt.Errorf("unknown state: %s", state)
	}

	// Build the status-bar format string — same pattern as the old bash script.
	fmt_ := fmt.Sprintf("#[fg=%s]#I:#W#{?window_flags,#{window_flags}, }", spec.color)

	_ = tmux.SetWindowOption(windowID, "window-status-format", fmt_)
	_ = tmux.SetWindowOption(windowID, "window-status-current-format", fmt_)
	_ = tmux.SetWindowOption(windowID, "@agent_state", spec.state)

	// On waiting: flash a display-message to all attached clients.
	// StartDisplayMessage starts the tmux child process and returns immediately
	// (without waiting for the display duration), so prism notify exits
	// quickly and the plugin's await unblocks before the permission prompt.
	if state == "set-waiting" {
		sessionName, err := tmux.SessionNameOf(windowID)
		if err == nil && sessionName != "" {
			clients, err := tmux.ListClients()
			if err == nil {
				style := fmt.Sprintf("#[fg=%s,bg=%s]", ColorBg0, ColorYellow)
				text := fmt.Sprintf(" %s is waiting", sessionName)
				for _, client := range clients {
					tmux.StartDisplayMessage(client, style, text, 1000)
				}
			}
		}
	}

	return nil
}
