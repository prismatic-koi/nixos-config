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
//
// Session names used in test fixtures must NOT use real coordinator slugs
// (e.g. "nixos-config@main"). Use "prism-test@invoker-<testname>" instead so
// that if isolation is accidentally broken, it cannot collide with a live session.
package sidecartest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// EnvRestrictHostAPI is the environment variable name that, when set to any
// non-empty value, causes promptdelivery.deliverViaSidecarSocket to refuse to
// dial any socket path that does not reside under the process's
// $XDG_STATE_HOME directory. It is set automatically by NewIsolated.
//
// This is a test-mode guard that prevents future regressions where new
// transport paths accidentally escape the test's tempdir isolation.
const EnvRestrictHostAPI = "PRISM_TEST_MODE_RESTRICT_HOSTAPI"

// Bus is the in-process test bus for a single isolated sidecar test. It
// provides an httptest.Server for the HTTP delivery path and a Unix-socket
// listener for the socket-pipe delivery path. Both listeners reside under a
// t.TempDir() so they can never interfere with host-side prism state.
type Bus struct {
	// HTTPServer is the httptest.Server that captures HTTP deliveries
	// (the harness HTTP prompt_async path).
	HTTPServer *httptest.Server

	// SockPath is the Unix socket path for the socket-pipe delivery path.
	// It lives at $XDG_STATE_HOME/prism/run/<hash>/hostapi.sock (where
	// XDG_STATE_HOME is the test tempdir set by NewIsolated).
	SockPath string

	// DB is an isolated test database opened under a private MkdirTemp path.
	DB *db.DB

	// XDGStateHome is the tempdir that was set as $XDG_STATE_HOME.
	XDGStateHome string

	// ReceivedBodies records the raw JSON bodies received by the HTTP server.
	// Access is mutex-protected; use CopyBodies() from tests.
	mu             sync.Mutex
	receivedBodies []string
}

// CopyBodies returns a snapshot of all bodies received by the HTTP server.
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
//     path resolution (SidecarHostAPIPath, SidecarHarnessPipePath, etc.) is
//     redirected to the tempdir.
//   - Sets PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 so deliverViaSidecarSocket
//     refuses to dial sockets outside the tempdir.
//   - Opens an isolated SQLite DB under a private os.MkdirTemp() directory.
//   - Starts an httptest.Server that records received prompt bodies.
//   - Starts a Unix-socket listener at the test-session hostapi.sock path so
//     that the socket-pipe delivery path is also exercised in-process.
//   - Registers t.Cleanup handlers for graceful shutdown.
//
// invokerSession is the session name to seed into the DB as an active invoker
// row pointing to the httptest.Server. Pass an empty string to skip seeding.
//
// All session names used in tests should use the "prism-test@" prefix, e.g.:
//
//	invoker := "prism-test@invoker-" + t.Name()
func NewIsolated(t *testing.T, invokerSession string) *Bus {
	t.Helper()

	// 1. Point XDG_STATE_HOME at a tempdir so all host-path resolution is
	//    sandboxed. t.Setenv restores the original value on t.Cleanup.
	xdgTmp, err := os.MkdirTemp("", "sidecartest-xdg-")
	if err != nil {
		t.Fatalf("sidecartest: create XDG temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(xdgTmp) })
	t.Setenv("XDG_STATE_HOME", xdgTmp)

	// 2. Activate the test-mode guard so deliverViaSidecarSocket refuses to
	//    dial sockets outside the tempdir.
	t.Setenv(EnvRestrictHostAPI, "1")

	// 3. Open an isolated DB.
	dbDir, err := os.MkdirTemp("", "sidecartest-db-")
	if err != nil {
		t.Fatalf("sidecartest: create DB temp dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		_ = os.RemoveAll(dbDir)
		t.Fatalf("sidecartest: open test DB: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		_ = os.RemoveAll(dbDir)
	})

	bus := &Bus{
		DB:           d,
		XDGStateHome: xdgTmp,
	}

	// 4. Start the HTTP server (captures prompt_async deliveries).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/session" {
			// Return a minimal session list for GET /session requests from
			// the promptdelivery HTTP path. The session ID here is a
			// recognisable placeholder — tests that need to assert on the
			// session ID should seed the DB explicitly.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"test-sid"}]`))
			return
		}
		if r.Method == http.MethodPost {
			body := make([]byte, 65536)
			n, _ := r.Body.Read(body)
			bus.mu.Lock()
			bus.receivedBodies = append(bus.receivedBodies, string(body[:n]))
			bus.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	bus.HTTPServer = srv

	// 5. Seed the invoker row in the DB, pointing to the httptest.Server.
	if invokerSession != "" {
		port := testServerPort(t, srv.URL)
		seedInvoker(t, d, invokerSession, port)
	}

	// 6. Start a Unix-socket listener for the socket-pipe delivery path.
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

// seedInvoker writes an active invoker row into d pointing to the HTTP test
// server at port, using an empty harness so DeliverToSession routes via the
// HTTP fallback path.
func seedInvoker(t *testing.T, d *db.DB, invokerSession string, port int) {
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
	// Use empty harness so DeliverToSession uses the HTTP fallback path —
	// tests use httptest.Server which has a port but no socket.
	if err := d.QueryRow(
		"UPDATE agent_status SET harness_port = ?, harness_session_id = ?, harness = '' WHERE session_name = ? RETURNING session_name",
		port, sid, invokerSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("sidecartest: set port/sid for invoker %q: %v", invokerSession, err)
	}
}

// testServerPort extracts the port number from an httptest.Server URL of the
// form "http://127.0.0.1:<port>".
func testServerPort(t *testing.T, srvURL string) int {
	t.Helper()
	var port int
	_, err := fmt.Sscanf(srvURL, "http://127.0.0.1:%d", &port)
	if err != nil {
		_, err = fmt.Sscanf(srvURL, "http://localhost:%d", &port)
	}
	if err != nil {
		t.Fatalf("sidecartest: parse test server port from %q: %v", srvURL, err)
	}
	return port
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
