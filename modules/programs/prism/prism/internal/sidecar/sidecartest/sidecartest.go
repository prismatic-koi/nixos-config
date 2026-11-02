// Package sidecartest provides test isolation helpers for sidecar unit tests.
//
// # Isolation contract
//
// Tests in internal/sidecar/ must use NewIsolated (or at minimum t.Setenv
// "XDG_STATE_HOME" to a t.TempDir()) before constructing a sidecar.Sidecar.
// This ensures:
//
//   - No file is created under the real $XDG_STATE_HOME/prism/ directory.
//   - No notification is delivered to any live coordinator on the host.
//   - The socket-pipe delivery path (deliverViaSidecarSocket) cannot dial any
//     host socket — a guard env var (PRISM_TEST_MODE_RESTRICT_HOSTAPI) is set
//     automatically, causing DeliverToSession to refuse socket paths outside
//     the test's tempdir.
//   - The dashboard side-effects on state change (sidecar.touchDashboardSentinel
//     and sidecar.pushDashboardEvent, both of which derive paths from
//     $XDG_STATE_HOME / $HOME) are suppressed: sidecar.New() consults the same
//     PRISM_TEST_MODE_RESTRICT_HOSTAPI guard set by NewIsolated and installs a
//     no-op DashboardSink when it is set. See dashboard.go for the
//     homeless-shelter footgun this closes.
//
// Session names used in test fixtures must NOT use real coordinator slugs
// (for example, "nixos-config@main"). Use "prism-test@invoker-<testname>" instead so
// that if isolation is accidentally broken, it cannot collide with a live session.
package sidecartest

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	_ "github.com/prismatic-koi/prism/internal/harness/pi" // DeliverToSession looks up the shape of the pi harness
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// EnvRestrictHostAPI is the environment variable name that, when set to any
// non-empty value:
//
//   - causes promptdelivery.deliverViaSidecarSocket to refuse to dial any
//     socket path that does not reside under the process's $XDG_STATE_HOME
//     directory.
//   - causes sidecar.New() to install a no-op DashboardSink when
//     Config.DashboardSink is nil, so writeStateChangeWithSID does not touch
//     $XDG_STATE_HOME-derived paths.
//
// It is set automatically by NewIsolated.
//
// This is a test-mode guard that prevents future regressions where new
// transport paths or state-change side-effects accidentally escape the test's
// tempdir isolation.
const EnvRestrictHostAPI = "PRISM_TEST_MODE_RESTRICT_HOSTAPI"

// Bus is the in-process test bus for a single isolated sidecar test. It
// provides a Unix-socket listener for the socket-pipe delivery path. The
// listener resides under a t.TempDir() so it can never interfere with
// host-side prism state.
type Bus struct {
	// SockPath is the Unix socket path for the socket-pipe delivery path.
	// It lives at $XDG_STATE_HOME/prism/run/<hash>/hostapi.sock (where
	// XDG_STATE_HOME is the test tempdir set by NewIsolated).
	SockPath string

	// DB is an isolated test database opened under a private MkdirTemp path.
	DB *db.DB

	// XDGStateHome is the tempdir that was set as $XDG_STATE_HOME.
	XDGStateHome string

	// ReceivedBodies records the raw JSON bodies received by the socket
	// listener.
	// Access is mutex-protected; use CopyBodies() from tests.
	mu             sync.Mutex
	receivedBodies []string
}

// CopyBodies returns a snapshot of all bodies received by the socket
// listener.
func (b *Bus) CopyBodies() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.receivedBodies))
	copy(out, b.receivedBodies)
	return out
}

// NewIsolated creates a fully isolated test bus for a sidecar test. It:
//
//   - Sets XDG_STATE_HOME to a new t.TempDir() path via t.Setenv so that all
//     path resolution (SidecarHostAPIPath, SidecarHarnessPipePath, and more) is
//     redirected to the tempdir.
//   - Sets PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 so deliverViaSidecarSocket
//     refuses to dial sockets outside the tempdir.
//   - Opens an isolated SQLite DB at $XDG_STATE_HOME/prism/prism.db (the
//     production-resolved path under the redirected tempdir), so tests can
//     assert DB isolation by construction via Bus.DB.Path().
//   - Starts a Unix-socket listener at the test-session hostapi.sock path
//     that records the received prompt bodies.
//   - Registers t.Cleanup handlers for graceful shutdown.
//
// invokerSession is the session name to seed into the DB as an active pi
// invoker row, whose host-API socket is the listener. Pass an empty string to
// skip seeding.
//
// All session names used in tests must use the "prism-test@" prefix, for example:
//
//	invoker := "prism-test@invoker-" + t.Name()
func NewIsolated(t *testing.T, invokerSession string) *Bus {
	t.Helper()

	// 1. Point XDG_STATE_HOME at a tempdir so all host-path resolution is
	//    sandboxed. t.Setenv restores the original value on t.Cleanup. The
	//    dir is short, so the hostapi.sock path under it fits in sun_path.
	xdgTmp := stateguardtest.ShortTempDir(t, "sidecartest-xdg-")
	t.Setenv("XDG_STATE_HOME", xdgTmp)

	// 2. Activate the test-mode guard so deliverViaSidecarSocket refuses to
	//    dial sockets outside the tempdir.
	t.Setenv(EnvRestrictHostAPI, "1")

	// 3. Open an isolated DB at the exact path production resolution would
	//    compute under the redirected $XDG_STATE_HOME
	//    ($XDG_STATE_HOME/prism/prism.db). Keeping the DB inside the
	//    test-scoped XDG tempdir makes the isolation assertable by
	//    construction: a test can verify Bus.DB.Path() resides under
	//    Bus.XDGStateHome instead of probing live host state.
	//    OpenDB stamps a pre-migrated template instead of re-running the
	//    schema and every migration, so the open costs no fsync.
	prismDir := filepath.Join(xdgTmp, "prism")
	if err := os.MkdirAll(prismDir, 0o700); err != nil {
		t.Fatalf("sidecartest: create prism state dir: %v", err)
	}
	d := OpenDB(t, filepath.Join(prismDir, "prism.db"))
	t.Cleanup(func() { d.Close() })

	bus := &Bus{
		DB:           d,
		XDGStateHome: xdgTmp,
	}

	// 4. Seed the invoker row in the DB.
	if invokerSession != "" {
		seedInvoker(t, d, invokerSession)
	}

	// 5. Start a Unix-socket listener for the socket-pipe delivery path.
	//    The socket lives at $XDG_STATE_HOME/prism/run/<hash>/hostapi.sock —
	//    the exact path that SidecarHostAPIPath would resolve for the invoker.
	//    This allows tests that exercise the pi-harness path to also be caught
	//    by the isolation guard without dialling a real host socket.
	if invokerSession != "" {
		sockPath, err := session.SidecarHostAPIPath(invokerSession)
		if err != nil {
			t.Fatalf("sidecartest: resolve socket path: %v", err)
		}
		sockDir := filepath.Dir(sockPath)
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			t.Fatalf("sidecartest: create socket dir: %v", err)
		}
		bus.SockPath = sockPath

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("sidecartest: listen on socket %s: %v", sockPath, err)
		}
		sockSrv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body := make([]byte, 65536)
					n, _ := r.Body.Read(body)
					bus.mu.Lock()
					bus.receivedBodies = append(bus.receivedBodies, string(body[:n]))
					bus.mu.Unlock()
				}
				w.WriteHeader(http.StatusOK)
			}),
		}
		go func() { _ = sockSrv.Serve(ln) }()
		t.Cleanup(func() { _ = sockSrv.Close() })
	}

	return bus
}

// seedInvoker writes an active invoker row into d with the pi harness, so
// DeliverToSession dials the host-API socket of the invoker.
func seedInvoker(t *testing.T, d *db.DB, invokerSession string) {
	t.Helper()

	// Derive a repo name from the invoker session name ("prism-test" from
	// "prism-test@...").
	repo := invokerSession
	if idx := len(invokerSession); idx > 0 {
		for i, c := range invokerSession {
			if c == '@' {
				repo = invokerSession[:i]
				break
			}
		}
	}

	sid := "test-sid-" + invokerSession
	agentName := "coordinator"
	modelID := "anthropic/claude-sonnet-4-5"
	if err := d.UpsertStatusWithAgent(invokerSession, repo, "/tmp/test-worktree", "active", nil, &sid, &agentName, &modelID); err != nil {
		t.Fatalf("sidecartest: seed invoker %q: %v", invokerSession, err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET harness_session_id = ?, harness = 'pi' WHERE session_name = ? RETURNING session_name",
		sid, invokerSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("sidecartest: set sid and harness for invoker %q: %v", invokerSession, err)
	}
}

// BodyContains reports whether any received body contains substring.
func (b *Bus) BodyContains(substring string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, body := range b.receivedBodies {
		if contains(body, substring) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		jsonContains(s, substr))
}

// jsonContains is a plain substring check used for assertion helpers.
func jsonContains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ParseBodies returns all received bodies unmarshalled as JSON maps. Bodies
// that fail to unmarshal are returned as raw string maps with key "raw".
func (b *Bus) ParseBodies() []map[string]json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]json.RawMessage, 0, len(b.receivedBodies))
	for _, body := range b.receivedBodies {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}
