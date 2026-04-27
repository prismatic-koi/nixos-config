package sidecar

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"
)

// pushDashboardEvent connects to the persistent dashboard Unix socket and sends
// a JSON push event with the session name, new state, and title. It is
// fire-and-forget: any error (socket absent, connection refused, write failure)
// is silently discarded. Must NOT be called with s.mu held (it is called via
// goroutine from writeStateChangeWithSID).
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
