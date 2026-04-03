package cmd

// prism launch — replaces cli.prism.launch
//
// Launches Prism: ensures the scratchpad session and the prism-dashboard
// session both exist, then attaches to the dashboard as the default landing
// point. The C-f context-switcher popup is no longer opened on plain startup;
// the dashboard is the primary navigation surface.
//
// When --path is supplied, the behaviour is unchanged from before: instead of
// landing on the dashboard, the context switcher is opened pre-seeded with that
// path. This preserves existing keybindings (ALT+o, ALT+n, zsh ^o) that jump
// directly to a specific project.
//
// Flags:
//
//	--in-terminal  attach in the current terminal instead of spawning a new kitty window
//	--path <dir>   open the context switcher targeted at this directory (bypasses dashboard)
//	--fresh        start a new session without --continue (used with --path)

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/tmux"
)

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

func init() {
	rootCmd.AddCommand(launchCmd)
	launchCmd.Flags().BoolVar(&launchInTerminal, "in-terminal", false, "attach in the current terminal")
	launchCmd.Flags().StringVar(&launchPath, "path", "", "open context switcher at this directory (bypasses dashboard)")
	launchCmd.Flags().BoolVar(&launchFresh, "fresh", false, "start a new session without --continue")
}

func runLaunch(_ *cobra.Command, _ []string) error {
	// --path: bypass the dashboard and open the context switcher at the given
	// directory. This preserves the existing UX for project-specific keybindings
	// (e.g. ALT+o → Obsidian, ALT+n → nixos-config, zsh ^o).
	if launchPath != "" {
		return runLaunchWithPath()
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
		kittyBin := config.Load().KittyBin
		if resolved, err := exec.LookPath(kittyBin); err == nil {
			kittyBin = resolved
		}
		// Build the startup command sequence:
		//   1. Create (or reuse) the scratchpad session in the background.
		//   2. Create (or reuse) the prism-dashboard session.
		//   3. Attach the new kitty window to the dashboard.
		self, err := os.Executable()
		if err != nil {
			self = "prism"
		}
		loopCmd := "while '" + strings.ReplaceAll(self, "'", "'\\''") + "' dashboard --popup; do true; done"
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

// runLaunchWithPath is the legacy --path fast-path: open the context switcher
// targeted at launchPath, bypassing the dashboard. Preserves the previous
// behaviour for keybindings like ALT+o (Obsidian) and ALT+n (nixos-config).
func runLaunchWithPath() error {
	switcherCmd := "prism switch --path " + launchPath
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
		if err := ensureScratchpad(); err != nil {
			return err
		}
		if _, err := tmux.SwitchClientCurrent("scratchpad"); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		return openSwitcher()

	case launchInTerminal:
		if err := ensureScratchpad(); err != nil {
			return err
		}
		_, _ = tmux.Run("set-hook", "-t", "scratchpad", "client-attached",
			"run-shell 'sleep 0.1' ; display-popup -w 80% -h 80% -E '"+switcherCmd+"' ; set-hook -u client-attached",
		)
		tmuxBin, err := exec.LookPath(tmux.TmuxBin)
		if err != nil {
			tmuxBin = tmux.TmuxBin
		}
		return syscall.Exec(tmuxBin, []string{tmux.TmuxBin, "new-session", "-As", "scratchpad"}, os.Environ())

	default:
		kittyBin := config.Load().KittyBin
		if resolved, err := exec.LookPath(kittyBin); err == nil {
			kittyBin = resolved
		}
		cmd := exec.Command(kittyBin,
			"--title", "Prism",
			tmux.TmuxBin, "new-session", "-As", "scratchpad", "-c", os.Getenv("HOME"),
			";", "rename-window", "-t", "scratchpad:0", "term",
			";", "run-shell", "sleep 0.2",
			";", "display-popup", "-w", "80%", "-h", "80%", "-E", switcherCmd,
		)
		return cmd.Start()
	}
}
