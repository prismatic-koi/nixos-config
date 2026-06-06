package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/persist"
	"github.com/prismatic-koi/prism/internal/mux/server"
)

// DefaultStopGrace is the default grace period passed to Stop before it
// escalates SIGTERM → SIGKILL. 10 s is the figure named in the issue's
// AC text and is comfortable headroom over the snapshotter's worst-
// case final-snapshot duration (one file write to local disk).
const DefaultStopGrace = 10 * time.Second

// ErrAlreadyRunning is returned by Run when the PID file already
// identifies a live process. Callers (cmd/prismd) translate this into
// a user-facing "already running (pid N)" error.
var ErrAlreadyRunning = errors.New("lifecycle: mux daemon is already running")

// Config configures a daemon Run, Start, or Stop call. Every path is
// overrideable so the test suite can point all four (PID, socket,
// snapshot, snapshot interval) at t.TempDir() and never touch the real
// $XDG_STATE_HOME.
//
// The zero Config is valid: Run will resolve each empty path via the
// corresponding Default*() helper, defaulting Logger to log.Default().
type Config struct {
	// PIDPath is the path to the daemon's PID file. Empty means
	// DefaultPIDPath(); errors there propagate from Run.
	PIDPath string

	// SocketPath is the Unix-socket path the server listens on. Empty
	// means server.DefaultSocketPath().
	SocketPath string

	// SnapshotPath is the path to the persisted session-tree file.
	// Empty means persist.DefaultPath().
	SnapshotPath string

	// SnapshotInterval is the periodic-snapshot interval. Zero means
	// persist.DefaultInterval (30 s).
	SnapshotInterval time.Duration

	// Logger receives lifecycle log lines (startup, shutdown, restored
	// vs empty tree, signal received). Nil means log.Default().
	Logger *log.Logger
}

// resolve fills in zero-valued paths from the matching Default*()
// helpers and returns a copy with logger normalised. The returned
// Config is safe to mutate without affecting the caller's.
func (c Config) resolve() (Config, error) {
	out := c
	if out.PIDPath == "" {
		p, err := DefaultPIDPath()
		if err != nil {
			return Config{}, fmt.Errorf("lifecycle: resolve pid path: %w", err)
		}
		out.PIDPath = p
	}
	if out.SocketPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return Config{}, fmt.Errorf("lifecycle: resolve socket path: %w", err)
		}
		out.SocketPath = p
	}
	if out.SnapshotPath == "" {
		p, err := persist.DefaultPath()
		if err != nil {
			return Config{}, fmt.Errorf("lifecycle: resolve snapshot path: %w", err)
		}
		out.SnapshotPath = p
	}
	if out.Logger == nil {
		out.Logger = log.Default()
	}
	return out, nil
}

// State enumerates the externally-visible lifecycle states of the mux
// daemon. The names match the status output verbatim so the CLI does
// not have to translate.
type State int

const (
	// StateStopped means no PID file is present.
	StateStopped State = iota

	// StateRunning means the PID file exists and the process is alive.
	StateRunning

	// StateStale means the PID file exists but the process is gone.
	// Status reports this so the user knows why a `mux start` succeeded
	// even though the file was there; subsequent starts will clean it
	// up transparently.
	StateStale
)

// String returns the human-readable form ("running", "stopped",
// "stale") used by the CLI's status output. Trailing context (the PID,
// for stale entries) is appended by the CLI layer.
func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateStale:
		return "stale"
	default:
		return "stopped"
	}
}

// Status is the result of LookupStatus — the daemon's externally
// visible runtime state. The PID field is meaningful only when State
// is StateRunning or StateStale; otherwise it is zero.
type Status struct {
	State      State
	PID        int
	PIDPath    string
	SocketPath string
}

// LookupStatus inspects the PID file at cfg.PIDPath (or the default
// path when empty) and reports the current state. It never writes —
// callers that want to clean up a stale PID file do so via Run, which
// removes the stale file as part of starting.
//
// Empty fields in cfg are resolved via the same Default*() helpers as
// Run, so a zero Config yields the canonical defaults.
func LookupStatus(cfg Config) (Status, error) {
	resolved, err := cfg.resolve()
	if err != nil {
		return Status{}, err
	}
	st := Status{
		PIDPath:    resolved.PIDPath,
		SocketPath: resolved.SocketPath,
		State:      StateStopped,
	}
	pid, err := readPIDFile(resolved.PIDPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, nil
	case errors.Is(err, errInvalidPIDFile):
		// Garbage PID file is functionally identical to stale: the
		// process clearly is not us. Reporting "stale" without a PID
		// is the right thing here.
		st.State = StateStale
		return st, nil
	case err != nil:
		return st, fmt.Errorf("lifecycle: read pid file: %w", err)
	}
	st.PID = pid
	if processAlive(pid) {
		st.State = StateRunning
	} else {
		st.State = StateStale
	}
	return st, nil
}

// Run is the daemon's foreground entrypoint. It blocks until ctx is
// cancelled or a fatal startup error occurs, at which point it tears
// down the server, writes a final snapshot, removes the PID file, and
// returns.
//
// Boot order (mirrors the issue text):
//
//  1. Refuse to start if a live mux daemon already holds the PID file.
//     Stale PID files (process gone) are silently cleaned up.
//  2. Restore the session tree via persist.LoadOrEmpty — corrupt or
//     unknown-schema snapshots fall back to an empty tree, never
//     crash.
//  3. Atomically write the PID file.
//  4. Start the socket-API server (internal/mux/server) listening on
//     SocketPath.
//  5. Start the snapshotter (internal/mux/persist.Snapshotter) writing
//     to SnapshotPath every SnapshotInterval.
//  6. Install SIGTERM / SIGINT handlers that cancel the root context.
//  7. Block until ctx is cancelled or any sub-goroutine errors fatally.
//
// Shutdown order (the reverse, with the snapshotter's final-Save
// contract intact):
//
//   - Cancel the root context. This drives the server's Shutdown
//     (drains in-flight requests within its internal grace) and the
//     snapshotter's final-Save (one Save call before Run returns).
//   - Wait for both goroutines to exit. Use a hard upper bound
//     (DefaultStopGrace) so a wedged sub-goroutine cannot keep the
//     process alive past the user's tolerance.
//   - Remove the PID file. Best-effort: if the unlink fails (e.g. the
//     run/ directory is gone), log and proceed — a stale PID file is
//     not a reason to refuse to exit.
//
// Returned errors:
//
//   - ErrAlreadyRunning — another live mux daemon holds the PID file.
//   - A wrapped error from persist.Save, server.ListenAndServe, or
//     PID-file write failures.
//   - nil — clean shutdown after ctx cancellation.
func Run(ctx context.Context, cfg Config) error {
	resolved, err := cfg.resolve()
	if err != nil {
		return err
	}

	// Step 1: PID-file guard. ErrAlreadyRunning is the only "refuse
	// to start" failure mode; stale files are silently cleared so the
	// next start does not require user intervention.
	if pid, err := readPIDFile(resolved.PIDPath); err == nil {
		if processAlive(pid) {
			return fmt.Errorf("%w: pid %d holds %s", ErrAlreadyRunning, pid, resolved.PIDPath)
		}
		// Stale PID file — log and overwrite.
		resolved.Logger.Printf("lifecycle: clearing stale pid file %s (pid %d is gone)", resolved.PIDPath, pid)
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errInvalidPIDFile) {
		return fmt.Errorf("lifecycle: read pid file: %w", err)
	}

	// Step 2: restore. LoadOrEmpty guarantees a non-nil tree even on
	// corrupt / unknown-schema snapshots; the failure path is logged
	// inside that function.
	tree := persist.LoadOrEmpty(resolved.SnapshotPath, resolved.Logger)

	// Step 3: write our PID file. Done after Restore so a panic
	// during restore does not leave a PID file behind for status to
	// trip on.
	if err := writePIDFile(resolved.PIDPath, os.Getpid()); err != nil {
		return fmt.Errorf("lifecycle: write pid file: %w", err)
	}
	// Track whether the unlink has run so we never double-unlink (the
	// signal handler and the deferred cleanup both want to claim this).
	var pidFileGoneOnce sync.Once
	removePIDFile := func() {
		pidFileGoneOnce.Do(func() {
			if err := os.Remove(resolved.PIDPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resolved.Logger.Printf("lifecycle: remove pid file %s: %v", resolved.PIDPath, err)
			}
		})
	}
	defer removePIDFile()

	// Root context: cancelled by the caller (test / cobra) OR by our
	// own signal handler. Using a derived context keeps the original
	// ctx's deadlines (if any) intact.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Step 4: signal handlers. Installed before the server / snapshotter
	// start so an interrupt during startup is honoured.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Step 5: server. Started in a goroutine; its error (if any) is
	// reported via serverErrCh and triggers shutdown.
	srv := server.New(tree)
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.ListenAndServe(runCtx, resolved.SocketPath)
	}()

	// Step 6: snapshotter. Same shape — goroutine + error channel +
	// cancellation triggers shutdown. The Snapshotter's contract is
	// that ctx-cancel triggers one final Save before Run returns.
	snap := &persist.Snapshotter{
		Path:     resolved.SnapshotPath,
		Tree:     tree,
		Interval: resolved.SnapshotInterval,
		Logger:   resolved.Logger,
	}
	snapErrCh := make(chan error, 1)
	go func() {
		snapErrCh <- snap.Run(runCtx)
	}()

	resolved.Logger.Printf("lifecycle: mux daemon ready (pid %d, socket %s, snapshot %s)",
		os.Getpid(), resolved.SocketPath, resolved.SnapshotPath)

	// Step 7: block on a shutdown trigger.
	var shutdownCause string
	select {
	case sig := <-sigCh:
		shutdownCause = fmt.Sprintf("signal %s", sig)
	case <-runCtx.Done():
		shutdownCause = "context cancelled"
	case err := <-serverErrCh:
		// Server returned before shutdown was requested — fatal.
		if err != nil {
			// Drain the snapshotter before returning so the in-flight
			// final-save still has a chance to land.
			runCancel()
			<-snapErrCh
			return fmt.Errorf("lifecycle: server: %w", err)
		}
		shutdownCause = "server exited cleanly (unexpected)"
	case err := <-snapErrCh:
		// Snapshotter returned before shutdown was requested — fatal.
		if err != nil {
			runCancel()
			<-serverErrCh
			return fmt.Errorf("lifecycle: snapshotter: %w", err)
		}
		shutdownCause = "snapshotter exited cleanly (unexpected)"
	}
	resolved.Logger.Printf("lifecycle: shutting down (%s)", shutdownCause)

	// Trigger graceful shutdown on both goroutines.
	runCancel()

	// Wait for both to drain, bounded by DefaultStopGrace so a wedged
	// goroutine cannot keep the daemon alive past the user's
	// patience. The snapshotter's final-save is run as part of its
	// own teardown — we only need to observe its exit.
	shutdownDeadline := time.NewTimer(DefaultStopGrace)
	defer shutdownDeadline.Stop()

	var serverShutdownErr, snapShutdownErr error
	for waiting := 2; waiting > 0; {
		select {
		case err := <-serverErrCh:
			serverShutdownErr = err
			waiting--
			serverErrCh = nil
		case err := <-snapErrCh:
			snapShutdownErr = err
			waiting--
			snapErrCh = nil
		case <-shutdownDeadline.C:
			// Hard timeout — log which goroutine is wedged and
			// return. The deferred removePIDFile still runs.
			resolved.Logger.Printf("lifecycle: shutdown deadline %s exceeded; %d goroutine(s) still draining", DefaultStopGrace, waiting)
			return fmt.Errorf("lifecycle: shutdown deadline exceeded")
		}
	}

	// Snapshotter's final-save error is the only one worth surfacing
	// — the server's Shutdown error is almost always net.ErrClosed
	// and is filtered inside server.Serve. Surface the snapshotter
	// error so a full disk on shutdown is visible in the daemon's
	// logs and exit code.
	if snapShutdownErr != nil {
		return fmt.Errorf("lifecycle: final snapshot: %w", snapShutdownErr)
	}
	if serverShutdownErr != nil {
		return fmt.Errorf("lifecycle: server shutdown: %w", serverShutdownErr)
	}
	return nil
}
