package main

// tui.go — `iris tui` subcommand.
//
// Opens a bubbletea TUI that connects to ~/.local/state/iris/iris.sock and
// provides a real-time session list + event stream + prompt input.
//
// The TUI reads NO state from the DB directly; every piece of state comes via
// the iris daemon socket (§4 of daemon-mode-design.md).
//
// Usage:
//
//	iris tui                         — connect to the default socket path
//	iris tui --socket /path/to.sock  — connect to a custom socket path
//	iris tui --help                  — show usage

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

var (
	tuiSocketPath string
	tuiSession    string
)

// tuiCmd is the `iris tui` subcommand.
var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the iris TUI (session list + live event stream + prompt delivery)",
	Long: `iris tui opens a full-screen bubbletea TUI that connects to the iris daemon
at ~/.local/state/iris/iris.sock (or --socket <path> to override).

The TUI shows:
  - Left pane:   live session list (name, state, role).
  - Right pane:  streaming event log for the selected session in the same
                 narrative format as 'prism checkin'.
  - Bottom bar:  prompt input; press Enter to deliver to the selected session.

Navigation:
  ↑/↓ or k/j    move between sessions
  PgUp/PgDn      scroll the event pane
  Enter          send the typed prompt to the selected session
  q or Ctrl+C    quit

The daemon must be running ('iris daemon') before opening the TUI.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := tuiSocketPath
		if sockPath == "" {
			p := iris.ResolvePaths()
			sockPath = p.Sock
		}

		fmt.Fprintf(os.Stderr, "iris tui: connecting to %s\n", sockPath)

		if err := tui.RunFocused(sockPath, tuiSession); err != nil {
			return fmt.Errorf("iris tui: %w", err)
		}
		return nil
	},
}

func init() {
	tuiCmd.Flags().StringVar(&tuiSocketPath, "socket", "", "Path to the iris daemon socket (default: ~/.local/state/iris/iris.sock)")
	tuiCmd.Flags().StringVar(&tuiSession, "session", "", "Pre-select this session by name on the first snapshot (used by the iris-switch picker to focus the TUI on a specific session)")
}
