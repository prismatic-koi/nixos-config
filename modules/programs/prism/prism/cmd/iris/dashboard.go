package main

// dashboard.go — `iris dashboard` subcommand (issue #1703).
//
// The iris analogue of `prism dashboard`. Two surfaces:
//
//   iris dashboard               persistent mode (long-lived bubbletea
//                                program). When invoked outside tmux, the
//                                process execs `tmux attach-session -t
//                                iris-dashboard`, creating the session if
//                                it does not yet exist. When invoked from
//                                inside tmux, the same recipe applies: the
//                                tmux binding handles entry, this command
//                                runs the bubbletea loop directly.
//
//   iris dashboard --popup       ephemeral popup mode (one-shot, q/esc
//                                quits). Spawned by `tmux display-popup
//                                -E ...` from the C-q binding. The popup
//                                runs inside the caller's own client.
//
//   iris dashboard --caller-session <name>
//                                marks the calling session as "you are
//                                here" — a ◆ prefix on the matching row.
//                                The tmux binding passes
//                                "$(tmux display-message -p '#S')" so the
//                                indicator works without any additional
//                                plumbing.
//
// Read /home/ben/code/nixos-config/.../prism/cmd/dashboard.go for the
// canonical shape this command follows.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/dashboard"
	"github.com/prismatic-koi/prism/internal/iris/tui"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var (
	dashboardPopup         bool
	dashboardCallerSession string
	dashboardSocketPath    string
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live multi-session dashboard for all daemon-known iris sessions",
	Long: `iris dashboard opens a full-screen bubbletea dashboard showing every
daemon-known iris session at a glance: state, role, worktree basename,
uptime, and a recent-activity indicator. It updates live as sessions are
spawned, transition states, or are cleaned up.

Two surfaces:

  iris dashboard            Persistent mode. When run outside tmux, this
                            process execs into 'tmux attach-session -t
                            iris-dashboard', creating the session on first
                            use. The tmux binding (prefix+I) toggles
                            between the dashboard session and the previous
                            client location.

  iris dashboard --popup    Ephemeral popup mode (one-shot, q/esc quits),
                            spawned by 'tmux display-popup -E ...' via the
                            C-q binding. The popup runs inside the
                            caller's own client.

  iris dashboard --caller-session <name>
                            Marks the named tmux session as "you are here"
                            with a ◆ prefix on the matching row. The tmux
                            binding passes the result of 'tmux
                            display-message -p "#S"' so this works
                            without any additional plumbing.

The iris daemon must be running ('iris daemon' or 'systemctl --user start
iris') for the dashboard to populate. If the daemon is not reachable at
startup the command exits non-zero with a clear 'systemctl --user start
iris' hint (matching 'iris sessions list' / 'iris prompt'). If the
daemon disappears mid-session (e.g. the user restarts iris while the
dashboard is open) the bubbletea program renders a "daemon not
connected" overlay and reconnects automatically when the socket
returns.`,
	RunE:          runDashboard,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	dashboardCmd.Flags().BoolVar(&dashboardPopup, "popup", false, "Run as ephemeral popup (spawned by C-q keybinding); q/esc closes")
	dashboardCmd.Flags().StringVar(&dashboardCallerSession, "caller-session", "", "Tmux session name of the invoking client (for 'you are here' indicator)")
	dashboardCmd.Flags().StringVar(&dashboardSocketPath, "socket", "", "Path to the iris daemon socket (default: ~/.local/state/iris/iris.sock)")
	rootCmd.AddCommand(dashboardCmd)
}

// runDashboard is the cobra RunE. It branches on --popup vs persistent;
// persistent mode further branches on whether we're already inside tmux:
// if not, exec into `tmux attach-session -t iris-dashboard` so the user
// ends up attached to the persistent session rather than running a
// short-lived foreground bubbletea program in their shell.
func runDashboard(cmd *cobra.Command, _ []string) error {
	sockPath := dashboardSocketPath
	if sockPath == "" {
		sockPath = iris.ResolvePaths().Sock
	}

	if err := dashboardPreflightProbe(cmd.Context(), sockPath); err != nil {
		return err
	}

	if dashboardPopup {
		return runDashboardProgram(dashboard.ModePopup, sockPath, dashboardCallerSession)
	}

	// Persistent mode.
	inTmux := os.Getenv("TMUX") != ""
	if inTmux {
		// If we're already inside the iris-dashboard session, run the
		// bubbletea program directly. Otherwise the binding (prefix+I)
		// has done its job and asked us to switch — but a CLI invocation
		// from any other tmux session reaches here too; ensure the
		// dashboard session exists, then switch the current client to it.
		currentSess, _ := tmux.CurrentSession()
		if currentSess == dashboard.DashSession {
			return runDashboardProgram(dashboard.ModePersistent, sockPath, dashboardCallerSession)
		}
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
	return syscallExecTmuxAttach(dashboard.DashSession)
}

// dashboardPreflightProbe synchronously verifies the iris daemon is
// reachable before the cobra RunE hands control to bubbletea. AC #8 of
// issue #1703 requires that `iris dashboard` exit non-zero with the
// canonical `systemctl --user start iris` hint when the daemon is not
// running. The bubbletea program by itself would only surface a
// DisconnectedMsg into the model (rendering an overlay) and the
// user-driven quit path returns exit 0 — contradicting the literal AC
// and diverging from `iris sessions list` / `iris prompt`.
//
// We reuse fetchSessionsSnapshot (the same helper backing `iris sessions
// list`) so the hint shape is locked to a single source of truth across
// the iris CLI. Probing applies to popup mode too: a daemon-down popup
// would otherwise render an opaque overlay inside `tmux display-popup
// -E` with no readable error code for shell callers; a synchronous probe
// lets the popup close immediately with the error visible on the
// invoking pane's stderr.
//
// The DisconnectedMsg overlay still applies for mid-session reconnects
// (daemon restarts while the dashboard is open) — the DaemonClient's
// retry loop keeps running and the overlay narrates the outage. Only the
// daemon-down-at-startup case maps to exit non-zero.
//
// Factored into its own function (rather than inlined into runDashboard)
// so dashboard_test.go can assert the exit-non-zero + hint-shape
// contract without driving the cobra command.
func dashboardPreflightProbe(ctx context.Context, sockPath string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := fetchSessionsSnapshot(probeCtx, sockPath)
	return err
}

// runDashboardProgram runs the bubbletea program for the given mode against
// the iris daemon socket at sockPath. callerSession is forwarded to the
// model for the "you are here" indicator (empty is fine — no indicator is
// rendered). Blocks until the user quits.
func runDashboardProgram(mode dashboard.Mode, sockPath, callerSession string) error {
	client := tui.NewDaemonClient(sockPath)
	m := dashboard.NewModel(client, mode, callerSession)

	p := tea.NewProgram(m, tea.WithAltScreen())
	client.SetProgram(p)

	// Connect asynchronously so the dashboard can render the disconnected
	// overlay immediately while the dial is in progress. Mirrors the iris
	// TUI's startup sequence. The DaemonClient owns its own retry loop, so
	// we do not pass a context here — the goroutine exits when the process
	// exits.
	go client.Connect()

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("iris dashboard: %w", err)
	}
	return nil
}

// ensureDashSession creates the iris-dashboard tmux session if it doesn't
// exist. The session runs `iris dashboard` (persistent mode) directly — no
// restart loop. The persistent dashboard exits cleanly on q; tmux will
// keep the session around with a "[exited]" indicator until the binding
// re-enters.
//
// We use os.Executable to resolve the absolute path of the running iris
// binary, so the spawned tmux session command works even when the pane
// shell does not have the Nix store path on PATH (the same trick prism's
// dashboard.go uses for the same reason).
func ensureDashSession() error {
	if tmux.HasSession(dashboard.DashSession) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		// Fall back to the bare name; if PATH doesn't have iris, tmux
		// will surface the error inside the new session, which is more
		// debuggable than failing here silently.
		self = "iris"
	}
	dashCmd := "'" + strings.ReplaceAll(self, "'", "'\\''") + "' dashboard"
	c := exec.Command(tmux.TmuxBin, "new-session", "-ds", dashboard.DashSession, "-n", "dashboard", dashCmd)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("iris dashboard: create tmux session: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// syscallExecTmuxAttach replaces the current process with `tmux
// attach-session -t sess` using syscall.Exec so no parent process remains.
// Mirrors the prism dashboard's syscallExecTmux helper.
func syscallExecTmuxAttach(sess string) error {
	tmuxBin, err := exec.LookPath(tmux.TmuxBin)
	if err != nil {
		// LookPath fails when TmuxBin is already an absolute path that
		// doesn't appear on PATH; try using it directly (the ldflags
		// injection path on NixOS).
		tmuxBin = tmux.TmuxBin
	}
	return syscall.Exec(tmuxBin, []string{"tmux", "attach-session", "-t", sess}, os.Environ())
}
