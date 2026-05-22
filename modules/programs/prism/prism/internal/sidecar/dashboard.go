package sidecar

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DashboardSink is the test-isolation hook for the two filesystem/socket
// side-effects performed on every Sidecar state change:
//
//   - PushEvent: connect to the persistent dashboard Unix socket under
//     $XDG_STATE_HOME/prism/bus/ and write a JSON push frame.
//   - TouchSentinel: ensure the $XDG_STATE_HOME/prism/bus/.dashboard.signal
//     file exists and bump its mtime so the popup dashboard's poller refreshes.
//
// Both default to writing under $XDG_STATE_HOME (falling back to
// os.UserHomeDir()+"/.local/state" when unset). In the nix build sandbox
// (HOME=/homeless-shelter) the fallback path is unwritable, which makes any
// test that constructs a Sidecar without first redirecting $XDG_STATE_HOME a
// homeless-shelter footgun — see issue #1851 and the AGENTS.md note about
// PR #1455.
//
// Production callers leave Config.DashboardSink nil; New() then installs the
// production sink (productionDashboardSink) which preserves the historical
// behaviour exactly: PushEvent dials the dashboard socket in a goroutine,
// TouchSentinel runs inline. Tests that construct a Sidecar via
// sidecartest.NewIsolated get a no-op sink so no filesystem or socket I/O is
// attempted regardless of how $HOME / $XDG_STATE_HOME are configured.
//
// The hook is read once at New() time and never mutated thereafter, so it is
// safe to call without holding s.mu.
type DashboardSink interface {
	// PushEvent is invoked from writeStateChangeWithSID on every state
	// transition. Implementations MUST NOT block the caller — the production
	// implementation dispatches the actual dial+write on a goroutine.
	PushEvent(sessionName, state, title string)
	// TouchSentinel is invoked inline from writeStateChangeWithSID on every
	// state transition. Implementations should be cheap; the production
	// implementation performs an os.MkdirAll + os.Chtimes pair.
	TouchSentinel()
}

// productionDashboardSink is the default DashboardSink installed by New() when
// Config.DashboardSink is nil. It preserves the pre-#1851 behaviour exactly:
// PushEvent is dispatched on a goroutine (fire-and-forget) and TouchSentinel
// runs inline.
type productionDashboardSink struct{}

// PushEvent implements DashboardSink by spawning the historical fire-and-
// forget goroutine that dials the dashboard socket.
func (productionDashboardSink) PushEvent(sessionName, state, title string) {
	go pushDashboardEvent(sessionName, state, title)
}

// TouchSentinel implements DashboardSink by invoking the historical inline
// touch.
func (productionDashboardSink) TouchSentinel() {
	touchDashboardSentinel()
}

// noopDashboardSink is a DashboardSink that does nothing. It is the sink that
// sidecartest.NewIsolated installs so tests cannot accidentally touch
// $XDG_STATE_HOME-derived paths or dial host sockets.
type noopDashboardSink struct{}

// PushEvent implements DashboardSink as a no-op.
func (noopDashboardSink) PushEvent(sessionName, state, title string) {}

// TouchSentinel implements DashboardSink as a no-op.
func (noopDashboardSink) TouchSentinel() {}

// NoopDashboardSink returns a DashboardSink whose methods do nothing. It is
// exported so test helpers outside this package (notably
// sidecartest.NewIsolated) can install it on a Config without re-deriving the
// type.
func NoopDashboardSink() DashboardSink { return noopDashboardSink{} }

// pushDashboardEvent connects to the persistent dashboard Unix socket and sends
// a JSON push event with the session name, new state, and title. It is
// fire-and-forget: any error (socket absent, connection refused, write failure)
// is silently discarded. Must NOT be called with s.mu held (it is called via
// goroutine from productionDashboardSink.PushEvent).
func pushDashboardEvent(sessionName, state, title string) {
	sockPath := dashboardSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		// Socket absent or stale — silently ignore.
		return
	}
	defer conn.Close()

	// Set a short write deadline so we never block the caller.
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))

	data, err := json.Marshal(map[string]string{
		"session": sessionName,
		"state":   state,
		"title":   title,
	})
	if err != nil {
		return
	}
	// Append newline so the dashboard's bufio.Scanner can read the line.
	data = append(data, '\n')
	_, _ = conn.Write(data)
}

// dashboardSocketPath returns the path to the persistent dashboard Unix socket.
// Mirrors dashboard.DashSocketPath() but avoids a package import cycle.
func dashboardSocketPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", "dashboard.sock")
}

// touchDashboardSentinel creates or updates the dashboard sentinel file's
// modification time, causing the dashboard's watcher to refresh.
//
// Test-isolation contract: this function calls os.MkdirAll on a path derived
// from $XDG_STATE_HOME (or os.UserHomeDir()+"/.local/state" when unset). In
// the nix build sandbox HOME=/homeless-shelter, which is unwritable — any
// test that drives a Sidecar state change without first redirecting
// $XDG_STATE_HOME will fail in CI with the homeless-shelter signature
// described in AGENTS.md. Tests MUST go through sidecartest.NewIsolated to
// avoid touching $XDG_STATE_HOME paths. NewIsolated installs a no-op
// DashboardSink (NoopDashboardSink()) which bypasses both this function and
// pushDashboardEvent entirely. See issue #1851 for the footgun analysis and
// the original PR #1455 for the failure shape this contract prevents.
func touchDashboardSentinel() {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	sentinel := filepath.Join(stateHome, "prism", "bus", ".dashboard.signal")
	_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
	now := time.Now()
	if err := os.Chtimes(sentinel, now, now); err != nil {
		f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
		}
	}
}
