// Package lifecycle is the long-lived-daemon wrapper around the layered
// prism-native multiplexer packages (#2157, programme #2147).
//
// Where the lower layers each own one job — internal/mux/pane the data
// model, internal/mux/server the socket API, internal/mux/persist the
// snapshot/restore, internal/mux/render the bubbletea UI — this package
// owns the lifecycle that turns them into a single long-running process:
//
//   - Boot order. Restore the session tree from a snapshot, instantiate
//     a SessionTree, start the socket-API server, start the snapshotter,
//     register SIGTERM/SIGINT handlers, and (when stdout is a TTY) start
//     the bubbletea renderer.
//
//   - PID file management. Atomically write a single
//     $XDG_STATE_HOME/prism/run/mux.pid file on startup, refuse to start
//     if a live process already holds it, treat a PID file whose process
//     is gone as stale and silently clean it up.
//
//   - Foreground vs daemon. `prismd mux start --foreground` runs the
//     main loop in the calling process (the systemd / launchd path);
//     `prismd mux start` re-execs the binary with --foreground and
//     detaches (the human-on-the-CLI path).
//
//   - Graceful shutdown. SIGTERM and SIGINT cancel the root context,
//     which tears down the snapshotter (after one final snapshot — the
//     contract from internal/mux/persist), tears down the server (with
//     the drain grace already implemented there), and returns 0 on the
//     way out.
//
// What this package does NOT own:
//
//   - Per-pane process management (spawning pi/nvim/shell into a pane,
//     reattaching pi via --resume after restart). Those are coupled to
//     the PRISM_USE_MUX cutover in #2158 and possibly later refinement.
//     This package defines the hooks; the per-pane policy lands later.
//
//   - The wire format / methods. internal/mux/server owns the API; this
//     package only starts and stops it.
//
//   - The snapshot format. internal/mux/persist owns Save/Load; this
//     package only schedules them via persist.Snapshotter.
//
// The split between this package and its consumers (cmd/prismd) is
// deliberate: every action this package performs is testable from a
// pure Go test that points all three paths (socket, PID, snapshot) at
// t.TempDir(), without invoking any cobra command. The cmd/prismd
// binary is a thin shell on top of Config + Run / Start / Stop /
// LookupStatus, not the place where logic lives.
package lifecycle
