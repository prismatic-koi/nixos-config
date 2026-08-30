package dashboard_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/dashboard"
)

// refreshRecorder is a minimal tea.Model that closes seen and quits the first
// time it receives a RefreshMsg. RefreshMsg is what WatchDashboardSentinel
// sends on a sentinel change and what the persistent model turns into a full
// FetchSessionsFromDB.
type refreshRecorder struct{ seen chan struct{} }

func (m refreshRecorder) Init() tea.Cmd { return nil }

func (m refreshRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(dashboard.RefreshMsg); ok {
		select {
		case <-m.seen:
		default:
			close(m.seen)
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m refreshRecorder) View() string { return "" }

// TestStartPersistentWatchers_SentinelTriggersRefetch asserts that the
// persistent dashboard triggers a full session re-fetch when the dashboard
// sentinel changes. StartPersistentWatchers must wire the sentinel watcher,
// which sends RefreshMsg; the persistent model handles RefreshMsg by returning
// FetchSessionsFromDB (a full re-fetch). Wiring only the socket listener
// (without the sentinel watcher) means no RefreshMsg arrives and this test
// times out.
func TestStartPersistentWatchers_SentinelTriggersRefetch(t *testing.T) {
	// Isolate all dashboard bus paths (sentinel + socket) under a temp
	// XDG_STATE_HOME so the test never touches the real ~/.local/state and is
	// homeless-shelter safe.
	//
	// Use a SHORT temp dir (os.MkdirTemp with a 1-char prefix), NOT t.TempDir():
	// t.TempDir() embeds the full test-function name in the path, and with the
	// "prism/bus/dashboard.sock" suffix the Unix socket path exceeds the 108-byte
	// sun_path limit inside the nix build sandbox (TMPDIR=/dev/shm/prism-go-test.*),
	// so net.Listen fails with "bind: invalid argument". A short dir keeps the
	// socket path under the cap.
	stateHome, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatalf("mkdir temp state home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	seen := make(chan struct{})
	// A blocking pipe reader keeps the program's input open so Run does not
	// exit before it can process a RefreshMsg. It is unblocked on cleanup.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	p := tea.NewProgram(refreshRecorder{seen: seen},
		tea.WithInput(pr),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The socket listener is incidental to this test; a socket-creation error must
	// not fail a sentinel-wiring assertion. StartPersistentWatchers starts the
	// sentinel watcher unconditionally, regardless of socket state, so a socket
	// error is logged and the sentinel path is still exercised below.
	if _, err := dashboard.StartPersistentWatchers(ctx, p); err != nil {
		t.Logf("StartPersistentWatchers socket listener error (non-fatal for this test): %v", err)
	}

	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()

	// Touch the sentinel to signal a session-list state change.
	sentinel := dashboard.DashSentinelPath()
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatalf("mkdir sentinel dir: %v", err)
	}
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	select {
	case <-seen:
		// RefreshMsg received: the sentinel watcher is wired into
		// StartPersistentWatchers and drives a full re-fetch.
	case <-time.After(5 * time.Second):
		t.Fatal("no RefreshMsg within 5s of a sentinel change; the sentinel watcher is not wired into StartPersistentWatchers")
	}

	cancel()
	_ = pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// The program did not exit promptly; not fatal for the assertion above.
	}
}
