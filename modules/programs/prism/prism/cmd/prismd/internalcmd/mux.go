package internalcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/mux/lifecycle"
)

// NewMuxCommand returns the `prismd mux` subtree, with `start`,
// `stop`, and `status` children. The split between this function and
// NewRoot keeps the daemon's subcommand surface in one file —
// reviewers do not have to read across files to see what `prismd mux`
// exposes.
func NewMuxCommand() *cobra.Command {
	muxCmd := &cobra.Command{
		Use:   "mux",
		Short: "Manage the prism-native multiplexer daemon",
		Long: `The prism mux daemon is a long-running per-user process that owns the
multiplexer's session tree, the Unix-socket API, and the periodic snapshot
loop. It is normally started by the systemd user unit (Linux) or launchd
agent (Darwin) packaged with prism; the subcommands here are for ad-hoc
control and inspection.

The daemon listens on a Unix socket under $XDG_STATE_HOME/prism/run/ and
keeps a snapshot of its session tree under $XDG_STATE_HOME/prism/mux/. Both
locations follow the per-user state-dir conventions already in use by the
prism sidecar.`,
	}
	muxCmd.AddCommand(newMuxStartCommand())
	muxCmd.AddCommand(newMuxStopCommand())
	muxCmd.AddCommand(newMuxStatusCommand())
	return muxCmd
}

// ---------------------------------------------------------------------------
// mux start
// ---------------------------------------------------------------------------

func newMuxStartCommand() *cobra.Command {
	var (
		foreground       bool
		pidPath          string
		socketPath       string
		snapshotPath     string
		snapshotInterval time.Duration
		logPath          string
		waitTimeout      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the mux daemon",
		Long: `Start the mux daemon.

Without --foreground (the default), the daemon is started in the background:
prismd re-execs itself with --foreground and detaches via setsid, mirroring
the per-session sidecar's detach shape. The CLI returns as soon as the
daemon has written its PID file and is listening on the socket.

With --foreground, the daemon runs in the calling process. This is the
mode the systemd user unit and launchd agent use: each supervisor expects
to own the process directly, so it can manage restart-on-failure and log
routing.

If a live mux daemon already holds the PID file, start refuses with a
non-zero exit and prints the existing PID. A stale PID file (pointing at
a process that has gone) is silently cleared.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := lifecycle.Config{
				PIDPath:          pidPath,
				SocketPath:       socketPath,
				SnapshotPath:     snapshotPath,
				SnapshotInterval: snapshotInterval,
				Logger:           newCmdLogger(cmd.OutOrStderr()),
			}
			if foreground {
				return runForeground(cmd.Context(), cfg)
			}
			return runDetached(cmd, cfg, logPath, waitTimeout)
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false,
		"Run the daemon in the calling process (used by systemd / launchd). Without this flag, start fork-detaches.")
	cmd.Flags().StringVar(&pidPath, "pid-file", "",
		"Path to the PID file (default $XDG_STATE_HOME/prism/run/mux.pid).")
	cmd.Flags().StringVar(&socketPath, "socket", "",
		"Path to the Unix socket the API server listens on (default $XDG_STATE_HOME/prism/run/<hash>/mux.sock).")
	cmd.Flags().StringVar(&snapshotPath, "snapshot", "",
		"Path to the session-tree snapshot file (default $XDG_STATE_HOME/prism/mux/session.json).")
	cmd.Flags().DurationVar(&snapshotInterval, "snapshot-interval", 0,
		"Periodic snapshot interval (default 30s).")
	cmd.Flags().StringVar(&logPath, "log-file", "",
		"When fork-detaching, redirect the daemon's stdout/stderr to this file (default /dev/null).")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Second,
		"How long to wait for the detached daemon to write its PID file before considering startup failed.")
	return cmd
}

// runForeground is the systemd / launchd path: install lifecycle.Run
// in the calling process and block until ctx is cancelled or a signal
// arrives (lifecycle.Run installs its own SIGTERM/SIGINT handler).
//
// The returned error is wrapped with a non-zero exit code so the
// supervisor (systemd, launchd) sees a real failure when the daemon
// cannot start.
func runForeground(ctx context.Context, cfg lifecycle.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := lifecycle.Run(ctx, cfg); err != nil {
		if errors.Is(err, lifecycle.ErrAlreadyRunning) {
			// Conflict — exit code 2 distinguishes "already running"
			// from other startup failures. systemd's restart logic
			// will not flap on this.
			return exitErrorf(2, "%v", err)
		}
		return exitErrorf(1, "%v", err)
	}
	return nil
}

// runDetached is the human-on-the-CLI path: fork-detach the daemon,
// poll the PID file until it identifies a live process (or the wait
// budget is exhausted), and print "started, pid N" to stdout.
//
// The pre-fork ErrAlreadyRunning check lives here rather than inside
// lifecycle.Start so the user-facing error message stays next to the
// code that formats it.
func runDetached(cmd *cobra.Command, cfg lifecycle.Config, logPath string, waitTimeout time.Duration) error {
	// Pre-flight: if a live daemon already holds the PID file, refuse
	// before forking. A stale PID file is handled inside Run itself.
	st, err := lifecycle.LookupStatus(cfg)
	if err != nil {
		return exitErrorf(1, "look up status: %v", err)
	}
	if st.State == lifecycle.StateRunning {
		return exitErrorf(2, "mux daemon already running (pid %d, socket %s)", st.PID, st.SocketPath)
	}

	self, err := os.Executable()
	if err != nil {
		return exitErrorf(1, "resolve self path: %v", err)
	}

	args := buildDetachArgs(cfg)
	pid, err := lifecycle.Start(lifecycle.StartOptions{
		Self:           self,
		ForegroundArgs: args,
		LogPath:        logPath,
	})
	if err != nil {
		return exitErrorf(1, "start daemon: %v", err)
	}
	// Resolve the PID-file path (LookupStatus already did, but the
	// resolved Config is private to lifecycle — re-resolve via the
	// same Default helpers).
	resolvedPID := cfg.PIDPath
	if resolvedPID == "" {
		resolvedPID, err = lifecycle.DefaultPIDPath()
		if err != nil {
			return exitErrorf(1, "resolve pid path: %v", err)
		}
	}
	readyPID, err := lifecycle.WaitForReady(resolvedPID, waitTimeout)
	if err != nil {
		return exitErrorf(1, "wait for daemon (pid %d): %v", pid, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "started (pid %d)\n", readyPID)
	return nil
}

// buildDetachArgs constructs the argv the detached child re-execs with.
// Every path override the user passed to `start` is forwarded verbatim
// so the foreground child looks at the same files. --foreground is
// always added.
func buildDetachArgs(cfg lifecycle.Config) []string {
	args := []string{"mux", "start", "--foreground"}
	if cfg.PIDPath != "" {
		args = append(args, "--pid-file", cfg.PIDPath)
	}
	if cfg.SocketPath != "" {
		args = append(args, "--socket", cfg.SocketPath)
	}
	if cfg.SnapshotPath != "" {
		args = append(args, "--snapshot", cfg.SnapshotPath)
	}
	if cfg.SnapshotInterval > 0 {
		args = append(args, "--snapshot-interval", cfg.SnapshotInterval.String())
	}
	return args
}

// ---------------------------------------------------------------------------
// mux stop
// ---------------------------------------------------------------------------

func newMuxStopCommand() *cobra.Command {
	var (
		pidPath string
		grace   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the mux daemon",
		Long: `Send SIGTERM to the mux daemon and wait for it to exit.

A daemon that does not exit within the grace period (default 10s) is
escalated to SIGKILL. The PID file is removed unconditionally on the way
out; the next periodic snapshot taken by the dying daemon represents the
last durable state of the session tree.

If no daemon is running (no PID file, or the PID file points at a process
that has gone), stop is a no-op and exits 0.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := lifecycle.Stop(lifecycle.StopOptions{
				PIDPath: pidPath,
				Grace:   grace,
			}); err != nil {
				return exitErrorf(1, "%v", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "stopped")
			return nil
		},
	}
	cmd.Flags().StringVar(&pidPath, "pid-file", "",
		"Path to the PID file (default $XDG_STATE_HOME/prism/run/mux.pid).")
	cmd.Flags().DurationVar(&grace, "grace", lifecycle.DefaultStopGrace,
		"Maximum time to wait between SIGTERM and SIGKILL escalation.")
	return cmd
}

// ---------------------------------------------------------------------------
// mux status
// ---------------------------------------------------------------------------

func newMuxStatusCommand() *cobra.Command {
	var (
		pidPath    string
		socketPath string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the mux daemon's running state",
		Long: `Print whether the mux daemon is running, stopped, or has left a stale
PID file behind. The exit code mirrors the state for scripted use:

  0 — running
  1 — stopped (no PID file)
  2 — stale (PID file present, process gone)

The status command never writes to the PID file. Use mux start to clear a
stale file as part of the next startup.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := lifecycle.LookupStatus(lifecycle.Config{
				PIDPath:    pidPath,
				SocketPath: socketPath,
			})
			if err != nil {
				return exitErrorf(1, "%v", err)
			}
			out := cmd.OutOrStdout()
			switch st.State {
			case lifecycle.StateRunning:
				fmt.Fprintf(out, "running (pid %d)\nsocket: %s\npid file: %s\n",
					st.PID, st.SocketPath, st.PIDPath)
				return nil
			case lifecycle.StateStale:
				if st.PID == 0 {
					fmt.Fprintf(out, "stale (pid file %s is corrupt)\n", st.PIDPath)
				} else {
					fmt.Fprintf(out, "stale (pid %d in %s — process gone)\n", st.PID, st.PIDPath)
				}
				return exitErrorf(2, "")
			default:
				fmt.Fprintf(out, "stopped (no pid file at %s)\n", st.PIDPath)
				return exitErrorf(1, "")
			}
		},
	}
	cmd.Flags().StringVar(&pidPath, "pid-file", "",
		"Path to the PID file (default $XDG_STATE_HOME/prism/run/mux.pid).")
	cmd.Flags().StringVar(&socketPath, "socket", "",
		"Path to the Unix socket (default $XDG_STATE_HOME/prism/run/<hash>/mux.sock).")
	return cmd
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// exitError carries a non-zero exit code so main.go can translate it
// into os.Exit. The wrapped error message is printed by main.go before
// the exit; an empty message suppresses the print (used by status
// where the structured output has already been emitted).
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) ExitCode() int { return e.code }
func (e *exitError) Unwrap() error { return nil }
func exitErrorf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

// newCmdLogger returns a logger that writes to w with the same prefix
// convention as log.Default(). Used so the foreground daemon's output
// matches the CLI's standard handling instead of going to a separate
// global logger.
func newCmdLogger(w io.Writer) *log.Logger {
	return log.New(w, "", log.LstdFlags|log.Lmsgprefix)
}
