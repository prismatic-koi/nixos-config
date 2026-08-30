package cmd

// prism launch — replaces cli.prism.launch
//
// Launches Prism: ensures the scratchpad session and the prism-dashboard
// session both exist, then attaches to the dashboard as the default landing
// point. On plain startup the dashboard is the primary navigation surface.
// The C-f context-switcher popup is not opened.
//
// When --path is supplied, the context switcher is opened pre-seeded with
// that path instead of landing on the dashboard. This preserves the
// keybindings (ALT+o, ALT+n, zsh ^o) that jump directly to a specific
// project.
//
// Flags:
//
//	--in-terminal  attach in the current terminal instead of spawning a new kitty window
//	--path <dir>   open the context switcher targeted at this directory (bypasses dashboard)
//	--fresh        start a new session without --continue (used with --path)

import (
	"fmt"
	"os"
	"os/exec"
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

// execStart starts the given *exec.Cmd without waiting for it (mirrors
// (*exec.Cmd).Start). It is a package-level indirection so tests can redirect
// the kitty spawn through a real pty (for example, via `script`) against an
// isolated test tmux server, to exercise the actual tmux command-list
// execution semantics, instead of only inspecting argv.
var execStart = func(cmd *exec.Cmd) error {
	return cmd.Start()
}

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
		client, err := tmux.CurrentClient()
		if err != nil {
			return fmt.Errorf("resolving current tmux client: %w", err)
		}
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
		// dashboard session. The scratchpad and dashboard sessions are ensured
		// here, on the Go side, exactly as the other two branches do — this
		// avoids chaining their creation into the same tmux command list as the
		// final attach. If creation is chained: when prism-dashboard already
		// exists, the chained "new-session -ds prism-dashboard" command fails,
		// which aborts the rest of the tmux command list (including the trailing
		// "switch-client"/attach), leaving the new kitty window's client attached
		// to scratchpad instead of the dashboard.
		if err := ensureScratchpad(); err != nil {
			return err
		}
		if err := ensureDashSession(); err != nil {
			return err
		}
		kittyBin := config.Load().KittyBin
		if resolved, err := exec.LookPath(kittyBin); err == nil {
			kittyBin = resolved
		}
		// Attach the new kitty window directly to the (now guaranteed to exist)
		// dashboard session. Using a single "attach-session" command means there
		// is nothing else in the command list that can fail and abort the attach.
		cmd := exec.Command(kittyBin,
			"--title", "Prism",
			tmux.TmuxBin, "attach-session", "-t", dashSession,
		)
		// Restore() is not called here: prism restart handles restore before
		// re-execing into launch, and prism-restore.service covers the login/reboot
		// scenario. runLaunch itself never restores.
		return execStart(cmd)
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
