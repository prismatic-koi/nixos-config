package cmd

// prism launch — replaces cli.prism.launch
//
// Launches Prism: ensures the scratchpad tmux session exists, then opens the
// context switcher (prism switch) in a display-popup.
//
// Flags:
//
//	--in-terminal  attach in the current terminal instead of spawning a new kitty window
//	--path <dir>   skip the interactive picker and switch directly to this directory
//
// Injected at build time via ldflags:
//
//	LaunchKittyBin  path to the kitty binary

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

var LaunchKittyBin = "kitty"

func init() {
	rootCmd.AddCommand(launchCmd)
	launchCmd.Flags().BoolVar(&launchInTerminal, "in-terminal", false, "attach in the current terminal")
	launchCmd.Flags().StringVar(&launchPath, "path", "", "open a specific directory directly")
	launchCmd.Flags().BoolVar(&launchFresh, "fresh", false, "start a new session without --continue")
}

var (
	launchInTerminal bool
	launchPath       string
	launchFresh      bool
)

var launchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch Prism scratchpad + context switcher",
	Args:  cobra.NoArgs,
	RunE:  runLaunch,
}

func runLaunch(_ *cobra.Command, _ []string) error {
	switcherCmd := "prism switch"
	if launchPath != "" {
		switcherCmd += " --path " + launchPath
	}
	if launchFresh {
		switcherCmd += " --fresh"
	}

	inTmux := os.Getenv("TMUX") != ""

	ensureScratchpad := func() error {
		if tmux.HasSession("scratchpad") {
			return nil
		}
		if err := tmux.NewSessionDetached("scratchpad", ""); err != nil {
			return err
		}
		return tmux.RenameWindow("scratchpad:0", "term")
	}

	openSwitcher := func() error {
		_, err := tmux.Run("display-popup", "-w", "80%", "-h", "80%", "-E", switcherCmd)
		return err
	}

	switch {
	case inTmux:
		// Already inside tmux: switch to scratchpad then open the popup.
		// Restore() is not called here: prism restart handles restore before
		// re-execing into launch, and prism-restore.service covers the login/reboot
		// scenario. runLaunch itself never restores.
		if err := ensureScratchpad(); err != nil {
			return err
		}
		if _, err := tmux.SwitchClientCurrent("scratchpad"); err != nil {
			return err
		}
		// Small delay to let the session settle before the popup.
		time.Sleep(100 * time.Millisecond)
		return openSwitcher()

	case launchInTerminal:
		// In a terminal but not in tmux: attach in-place, fire switcher once attached.
		// Restore() is not called here for the same reason as the inTmux branch above.
		if err := ensureScratchpad(); err != nil {
			return err
		}
		// Set a one-shot hook that opens the switcher as soon as the client attaches.
		_, _ = tmux.Run("set-hook", "-t", "scratchpad", "client-attached",
			"run-shell 'sleep 0.1' ; display-popup -w 80% -h 80% -E '"+switcherCmd+"' ; set-hook -u client-attached",
		)
		// Replace this process with tmux attach (new-session -As reuses existing).
		tmuxBin, err := exec.LookPath(tmux.TmuxBin)
		if err != nil {
			tmuxBin = tmux.TmuxBin
		}
		return syscall.Exec(tmuxBin, []string{tmux.TmuxBin, "new-session", "-As", "scratchpad"}, os.Environ())

	default:
		// Outside tmux entirely: spawn a new kitty window.
		kittyBin, err := exec.LookPath(LaunchKittyBin)
		if err != nil {
			kittyBin = LaunchKittyBin
		}
		cmd := exec.Command(kittyBin,
			"--title", "Prism",
			tmux.TmuxBin, "new-session", "-As", "scratchpad", "-c", os.Getenv("HOME"),
			";", "rename-window", "-t", "scratchpad:0", "term",
			";", "run-shell", "sleep 0.2",
			";", "display-popup", "-w", "80%", "-h", "80%", "-E", switcherCmd,
		)
		// Restore() is not called here: (1) the tmux server does not exist yet —
		// kitty will start it — so there is no server to talk to. (2) The
		// login/reboot scenario is covered by prism-restore.service instead.
		return cmd.Start()
	}
}
