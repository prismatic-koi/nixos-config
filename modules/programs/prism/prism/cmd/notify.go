package cmd

// prism notify <state> — replaces cli.tmux.setStatus
//
// Called by the opencode tmux-status plugin on every agent state transition.
// Reads TMUX_PANE from the environment to identify which window to update,
// then sets the window-status-format and @agent_state window options so the
// dashboard and status bar reflect the new state.
//
// On "waiting", increments the global @prism_waiting counter so the
// status-right can show "N waiting". On any other state, decrements it
// if this window was previously waiting.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

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
		"set-active":      {"active", ColorPurple},
		"set-waiting":     {"waiting", ColorYellow},
		"set-finished":    {"finished", ColorGreen},
		"set-interrupted": {"interrupted", ColorRed},
		"set-compacting":  {"compacting", ColorBlue},
		"set-error":       {"error", ColorRed},
	}

	if state == "clear" {
		// If this window was waiting, decrement the global counter.
		prevState, _ := tmux.GetWindowOption(windowID, "@agent_state")
		if strings.TrimSpace(prevState) == "waiting" {
			adjustWaitingCount(-1)
		}
		_ = tmux.UnsetWindowOption(windowID, "window-status-format")
		_ = tmux.UnsetWindowOption(windowID, "window-status-current-format")
		_ = tmux.UnsetWindowOption(windowID, "@agent_state")
		return nil
	}

	spec, ok := specs[state]
	if !ok {
		return fmt.Errorf("unknown state: %s", state)
	}

	// Update the global waiting counter based on the transition.
	prevState, _ := tmux.GetWindowOption(windowID, "@agent_state")
	wasWaiting := strings.TrimSpace(prevState) == "waiting"
	isWaiting := spec.state == "waiting"
	if isWaiting && !wasWaiting {
		adjustWaitingCount(+1)
	} else if !isWaiting && wasWaiting {
		adjustWaitingCount(-1)
	}

	// Build the status-bar format string — same pattern as the old bash script.
	fmt_ := fmt.Sprintf("#[fg=%s]#I:#W#{?window_flags,#{window_flags}, }", spec.color)

	_ = tmux.SetWindowOption(windowID, "window-status-format", fmt_)
	_ = tmux.SetWindowOption(windowID, "window-status-current-format", fmt_)
	_ = tmux.SetWindowOption(windowID, "@agent_state", spec.state)

	return nil
}

// adjustWaitingCount increments or decrements the global @prism_waiting counter.
// When the count reaches zero the option is unset so the status-right
// conditional hides the indicator cleanly.
func adjustWaitingCount(delta int) {
	current := 0
	if val, err := tmux.GetGlobalOption("@prism_waiting"); err == nil {
		current, _ = strconv.Atoi(strings.TrimSpace(val))
	}
	next := current + delta
	if next <= 0 {
		_ = tmux.UnsetGlobalOption("@prism_waiting")
	} else {
		_ = tmux.SetGlobalOption("@prism_waiting", strconv.Itoa(next))
	}
}
