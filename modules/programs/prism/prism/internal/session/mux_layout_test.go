package session

// Tests for the PRISM_USE_MUX cutover layout helpers (issue #2158).
//
// These tests bind a real mux server on a t.TempDir() socket so the
// full round-trip (Sessions().Create → server-side AddSession + PTY
// spawn cascade → Sessions().Destroy → cascade teardown) is exercised
// end-to-end. The sidecar is NOT started — these tests assert the
// mux-layout side of the wire only, not the SpawnSession composition
// (which is exercised by the existing session_test.go suite).
//
// Homeless-shelter clean: $XDG_STATE_HOME is overridden to t.TempDir()
// so any internal path resolution stays inside the test sandbox.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/client"
	"github.com/prismatic-koi/prism/internal/mux/pane"
	"github.com/prismatic-koi/prism/internal/mux/server"
)

// startTestMuxServer binds a Unix-socket server on a t.TempDir() path
// and returns the path. The server is shut down via t.Cleanup.
func startTestMuxServer(t *testing.T) string {
	t.Helper()

	tree := pane.New()
	srv := server.New(tree)

	sockPath := filepath.Join(t.TempDir(), "mux.sock")
	if len(sockPath) >= 104 {
		t.Fatalf("test socket path too long (%d bytes): %s", len(sockPath), sockPath)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %q: %v", sockPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		srv.Close()
		cancel()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Logf("test mux server did not shut down within 3s")
		}
	})
	return sockPath
}

// TestSpawnMuxLayout_RegistersSessionAndPanes is the happy-path
// end-to-end: SpawnMuxLayout against a live test server, then verify
// the daemon has the session + the canonical 3-pane (or 2-pane) set.
//
// The sidecar startup is skipped (the test binary has no sidecar
// dependency) — we drive a fresh SpawnOpts with a long-lived prompt
// so the SendInput call at the end of SpawnMuxLayout has a path to
// exercise. The agent pane will be a /bin/sh that swallows the
// prompt; that is sufficient for the wire-level assertion.
func TestSpawnMuxLayout_RegistersSessionAndPanes(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh; skipping mux layout test")
	}
	sockPath := startTestMuxServer(t)
	// Direct the in-package client at the test socket.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// SpawnMuxLayout's client lookup goes through client.New() which
	// reads XDG_STATE_HOME via server.DefaultSocketPath. Override via
	// a custom client construction shim — we set the socket path
	// explicitly using the public helper.
	overrideMuxClientFactory(t, sockPath)

	opts := SpawnOpts{
		SessionName: "repo@feat",
		Repo:        "repo",
		Worktree:    t.TempDir(),
		AgentRole:   "worker",
		Layout:      LayoutFull,
		Prompt:      "hello\n",
		BranchFlag:  "feat",
		// Do NOT set anything that triggers sidecar startup; the
		// helper still attempts StartSidecarWithOpts but we want
		// that to fail silently (StartSidecarWithOpts surfaces a
		// warning rather than an error per the issue contract).
	}

	if err := SpawnMuxLayout(opts, 0); err != nil {
		t.Fatalf("SpawnMuxLayout: %v", err)
	}

	// Drive a fresh client to introspect the daemon's state.
	mc, err := client.New(client.WithSocketPath(sockPath))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := mc.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("Sessions().List: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != "repo@feat" {
		t.Errorf("Sessions = %+v, want 1 session with ID repo@feat", list.Sessions)
	}
	sess := list.Sessions[0]
	if sess.Repo != "repo" {
		t.Errorf("session repo = %q, want %q", sess.Repo, "repo")
	}
	if sess.Branch != "feat" {
		t.Errorf("session branch = %q, want %q", sess.Branch, "feat")
	}
	if sess.AgentRole != "worker" {
		t.Errorf("session agent_role = %q, want %q", sess.AgentRole, "worker")
	}
	if len(sess.Panes) != 3 {
		t.Errorf("session has %d panes, want 3 (edit/agent/term)", len(sess.Panes))
	}
	wantPaneNames := []string{"edit", "agent", "term"}
	for i, want := range wantPaneNames {
		if i >= len(sess.Panes) || sess.Panes[i].Name != want {
			t.Errorf("pane %d name = %q, want %q", i, paneNameAt(sess, i), want)
		}
	}
	if sess.ActivePane != "agent" {
		t.Errorf("active pane = %q, want %q", sess.ActivePane, "agent")
	}
}

// TestSpawnMuxLayout_LayoutAgentOnly exercises the 2-pane branch
// (shell + agent). Mirrors the happy-path test but asserts the
// review-style layout's pane list.
func TestSpawnMuxLayout_LayoutAgentOnly(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh; skipping mux layout test")
	}
	sockPath := startTestMuxServer(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	overrideMuxClientFactory(t, sockPath)

	opts := SpawnOpts{
		SessionName: "repo@review-1",
		Repo:        "repo",
		Worktree:    t.TempDir(),
		AgentRole:   "review-code",
		Layout:      LayoutAgentOnly,
		Prompt:      "",
		BranchFlag:  "review",
	}

	if err := SpawnMuxLayout(opts, 0); err != nil {
		t.Fatalf("SpawnMuxLayout: %v", err)
	}

	mc, _ := client.New(client.WithSocketPath(sockPath))
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := mc.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("Sessions().List: %v", err)
	}
	if len(list.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(list.Sessions))
	}
	sess := list.Sessions[0]
	wantPaneNames := []string{"shell", "agent"}
	if len(sess.Panes) != 2 {
		t.Fatalf("session has %d panes, want 2 (shell/agent)", len(sess.Panes))
	}
	for i, want := range wantPaneNames {
		if sess.Panes[i].Name != want {
			t.Errorf("pane %d name = %q, want %q", i, sess.Panes[i].Name, want)
		}
	}
}

// TestTeardownMuxSession_DestroysAndCascadesPTY checks that a
// SpawnMuxLayout-created session can be torn down via the helper. The
// helper also returns nil on a missing session, so a re-run is safe.
func TestTeardownMuxSession_DestroysAndCascadesPTY(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh; skipping mux layout test")
	}
	sockPath := startTestMuxServer(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	overrideMuxClientFactory(t, sockPath)

	opts := SpawnOpts{
		SessionName: "repo@feat",
		Repo:        "repo",
		Worktree:    t.TempDir(),
		AgentRole:   "worker",
		Layout:      LayoutFull,
	}
	if err := SpawnMuxLayout(opts, 0); err != nil {
		t.Fatalf("SpawnMuxLayout: %v", err)
	}

	if err := TeardownMuxSession("repo@feat"); err != nil {
		t.Errorf("TeardownMuxSession: %v", err)
	}

	// Idempotent: missing session is not an error.
	if err := TeardownMuxSession("repo@feat"); err != nil {
		t.Errorf("TeardownMuxSession (second call): %v", err)
	}

	// Confirm the daemon has no sessions left.
	mc, _ := client.New(client.WithSocketPath(sockPath))
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := mc.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("Sessions().List: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Errorf("daemon still has %d sessions after teardown: %+v",
			len(list.Sessions), list.Sessions)
	}
}

// TestTeardownMuxSession_DaemonNotRunning returns the client-side
// ErrServerUnavailable so the CLI gate can surface the canonical
// "daemon not running" diagnostic without scraping a string.
func TestTeardownMuxSession_DaemonNotRunning(t *testing.T) {
	// Point at an unbound socket path inside a t.TempDir() so the
	// client gets a clean ECONNREFUSED on dial.
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	// Build a socket path that does NOT exist (we do not bind a
	// listener on it).
	overrideMuxClientFactory(t, filepath.Join(dir, "no-such.sock"))

	err := TeardownMuxSession("repo@feat")
	if err == nil {
		t.Fatal("TeardownMuxSession on absent daemon: got nil err, want ErrServerUnavailable")
	}
	if !isServerUnavailable(err) {
		t.Errorf("TeardownMuxSession error = %v, want wrapping ErrServerUnavailable", err)
	}
}

// paneNameAt is a tiny accessor for the pane-index assertion above —
// keeps the assertion line short.
func paneNameAt(s pane.Session, i int) string {
	if i < 0 || i >= len(s.Panes) {
		return "<oob>"
	}
	return s.Panes[i].Name
}

// isServerUnavailable returns true when err is or wraps the client's
// canonical "daemon not reachable" sentinel.
func isServerUnavailable(err error) bool {
	return err != nil && (unwrapErrContains(err, "server unavailable") ||
		unwrapErrContains(err, "ErrServerUnavailable"))
}

// unwrapErrContains is a string-search fallback for the assertion
// above. Using errors.Is(err, client.ErrServerUnavailable) would be
// cleaner, but that adds a dependency edge inside this test file we
// already cover in cmd/mux_cutover_test.go.
func unwrapErrContains(err error, needle string) bool {
	if err == nil {
		return false
	}
	return strContains(err.Error(), needle)
}

// strContains is a tiny inline strings.Contains so this file does
// not import strings for a single call site.
func strContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// overrideMuxClientFactory swaps the package-level client construction
// path so SpawnMuxLayout's client.New() call lands at sockPath instead
// of the canonical $XDG_STATE_HOME path. Restores at t.Cleanup.
//
// Implementation: the helper writes a tiny override into a
// package-private variable, then SpawnMuxLayout consults it before
// calling client.New(). See mux_layout.go's clientFactory.
func overrideMuxClientFactory(t *testing.T, sockPath string) {
	t.Helper()
	prev := muxClientFactory
	muxClientFactory = func() (client.MuxClient, error) {
		return client.New(client.WithSocketPath(sockPath))
	}
	t.Cleanup(func() { muxClientFactory = prev })
}
