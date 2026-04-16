package cmd

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live agent status dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		popup, _ := cmd.Flags().GetBool("popup")
		callerSession, _ := cmd.Flags().GetString("caller-session")

		if popup {
			// Popup mode (C-w): run the TUI directly inside a display-popup frame.
			// callerSession is passed via --caller-session flag so the "you are here"
			// indicator and initial cursor snap work correctly. The popup runs inside
			// the caller's own client (m.client), so no --caller-client flag is needed.
			client, _ := tmux.CurrentClient()
			m := dashboard.NewPopupModel(client, callerSession)
			p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
			ctx, cancel := context.WithCancel(context.Background())
			dashboard.WatchDashboardSentinel(ctx, p)
			_, err := p.Run()
			cancel()
			return err
		}

		inTmux := os.Getenv("TMUX") != ""

		if inTmux {
			// Check if we are already in the prism-dashboard session — if so,
			// just run the persistent TUI directly (avoids re-entering).
			currentSess, _ := tmux.CurrentSession()
			if currentSess == dashboard.DashSession {
				// Already in the dashboard session — run the persistent TUI.
				// callerSession is passed for the "you are here" indicator on
				// first load; it is optional (empty is fine).
				client, _ := tmux.CurrentClient()
				m := dashboard.NewPersistentModel(client, callerSession)
				p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
				ctx, cancel := context.WithCancel(context.Background())
				// Use socket listener for real-time push events (persistent dashboard only).
				if _, err := dashboard.StartSocketListener(ctx, p); err != nil {
					log.Printf("dashboard: socket listener: %v (falling back to sentinel polling)", err)
					dashboard.WatchDashboardSentinel(ctx, p)
				}
				_, err := p.Run()
				cancel()
				return err
			}

			// Inside tmux as a CLI call: ensure session exists, switch to it.
			if err := ensureDashSession(); err != nil {
				return err
			}
			client, _ := tmux.CurrentClient()
			return tmux.SwitchClient(client, dashboard.DashSession)
		}

		// Outside tmux: ensure session exists, then exec tmux attach so this
		// process is replaced by a full tmux client attached to the dashboard.
		if err := ensureDashSession(); err != nil {
			return err
		}
		return syscallExecTmux(dashboard.DashSession)
	},
}

// ensureDashSession creates the prism-dashboard session if it doesn't exist.
// The session runs `prism dashboard` (persistent mode) directly — no restart
// loop. The persistent dashboard keeps itself alive without exiting on quit.
//
// The session command uses the absolute path of the running prism binary
// (os.Executable) rather than the bare name "prism", so the command works
// even when the tmux pane shell does not have the Nix store path in PATH.
// Similarly, tmux.TmuxBin is used instead of the bare "tmux" string so the
// call respects the ldflags-injected binary path on NixOS.
func ensureDashSession() error {
	if tmux.HasSession(dashboard.DashSession) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		// Fall back to "prism" if we cannot resolve our own path.
		self = "prism"
	}
	dashCmd := "'" + strings.ReplaceAll(self, "'", "'\\''") + "' dashboard"
	c := exec.Command(tmux.TmuxBin, "new-session", "-ds", dashboard.DashSession, "-n", "dashboard", dashCmd)
	return c.Run()
}

// syscallExecTmux replaces the current process with tmux attached to session
// using syscall.Exec so no parent process remains.
// Uses tmux.TmuxBin (ldflags-injected absolute path on NixOS) so the exec
// succeeds even when "tmux" is not on the invoking shell's PATH.
func syscallExecTmux(sess string) error {
	tmuxBin, err := exec.LookPath(tmux.TmuxBin)
	if err != nil {
		// LookPath fails when TmuxBin is already an absolute path that
		// doesn't exist in the PATH search; try using it directly.
		tmuxBin = tmux.TmuxBin
	}
	return syscall.Exec(tmuxBin, []string{"tmux", "attach-session", "-t", sess}, os.Environ())
}

func init() {
	dashboardCmd.Flags().Bool("popup", false, "Run as ephemeral popup (spawned by C-w keybinding)")
	dashboardCmd.Flags().String("caller-session", "", "Tmux session name of the invoking client (for 'you are here' indicator)")
	rootCmd.AddCommand(dashboardCmd)
}

// ── package-level shims ───────────────────────────────────────────────────────
// These thin wrappers allow other cmd/ files (checkin.go, list_sessions.go,
// event.go, switch.go, launch.go) to continue using their original call sites
// without modification.

// dashSession is the name of the persistent dashboard tmux session.
const dashSession = dashboard.DashSession

// dashSentinelPath returns the path to the dashboard sentinel file.
// Used by event.go to touch the sentinel when session state changes.
func dashSentinelPath() string { return dashboard.DashSentinelPath() }

// stateStyle returns a lipgloss.Style with the state-appropriate foreground
// colour. Used by checkin.go and list_sessions.go.
func stateStyle(state string) lipgloss.Style {
	return dashboard.StateStyle(state)
}
