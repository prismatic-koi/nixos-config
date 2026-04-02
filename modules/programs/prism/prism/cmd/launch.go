package cmd

// prism launch — replaces cli.prism.launch
//
// Launches Prism: ensures the scratchpad session and the prism-dashboard
// session both exist, then attaches to the dashboard as the default landing
// point. The C-f context-switcher popup is no longer opened on startup; the
// dashboard is the primary navigation surface.
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
	Short: "Launch Prism dashboard (default landing point)",
	Args:  cobra.NoArgs,
	RunE:  runLaunch,
}

func runLaunch(_ *cobra.Command, _ []string) error {
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

	switch {
	case inTmux:
		// Already inside tmux: ensure both the scratchpad (shell fallback) and
		// the dashboard session exist, then switch to the dashboard.
		if err := ensureScratchpad(); err != nil {
			return err
		}
		if err := ensureDashSession(); err != nil {
			return err
		}
		client, _ := tmux.CurrentClient()
		return tmux.SwitchClient(client, dashSession)

	case launchInTerminal:
		// In a terminal but not in tmux: attach to the dashboard in-place.
		if err := ensureScratchpad(); err != nil {
			return err
		}
		if err := ensureDashSession(); err != nil {
			return err
		}
		// Replace this process with tmux attach so no parent process remains.
		return syscallExecTmux(dashSession)

	default:
		// Outside tmux entirely: spawn a new kitty window attached to the
		// dashboard session. The scratchpad is created in the background so it
		// is ready for use as a shell without being the initial focus.
		kittyBin, err := exec.LookPath(LaunchKittyBin)
		if err != nil {
			kittyBin = LaunchKittyBin
		}
		// Build the startup command sequence:
		//   1. Create (or reuse) the scratchpad session in the background.
		//   2. Create (or reuse) the prism-dashboard session.
		//   3. Attach the new kitty window to the dashboard.
		self, err := os.Executable()
		if err != nil {
			self = "prism"
		}
		loopCmd := "while " + self + " dashboard --popup; do true; done"
		cmd := exec.Command(kittyBin,
			"--title", "Prism",
			tmux.TmuxBin, "new-session", "-As", "scratchpad", "-c", os.Getenv("HOME"),
			";", "rename-window", "-t", "scratchpad:0", "term",
			";", "new-session", "-ds", dashSession, "-n", "dashboard", loopCmd,
			";", "switch-client", "-t", dashSession,
		)
		// Restore() is not called here: prism restart handles restore before
		// re-execing into launch, and prism-restore.service covers the login/reboot
		// scenario. runLaunch itself never restores.
		return cmd.Start()
	}
}
