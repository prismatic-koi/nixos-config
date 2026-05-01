package cmd

// Tests for the PRISM_HOST_API proxy paths in cmd/event.go added in #1254 to
// fix the container shadow-DB issue.
//
// These tests stand up a real HTTP server bound to a Unix socket on disk
// and point PRISM_HOST_API at it, then exercise each event subcommand and
// assert that the request reached the host (mirror of the sidecar /event
// handler) rather than touching the local DB.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEventAPIServer is a minimal mirror of the sidecar's /event route.
// It records requests and can be configured to return errors.
type fakeEventAPIServer struct {
	requests []recordedRequest

	// failStatus, when non-zero, causes /event to return that HTTP status with
	// {"error": failError}.
	failStatus int
	failError  string
}

func (s *fakeEventAPIServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.requests = append(s.requests, recordedRequest{Path: "/event", Body: string(body)})
		if s.failStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.failStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": s.failError})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{})
	})
	return mux
}

// startFakeEventAPIServer starts the fake /event server bound to a Unix socket
// and returns the server and the apiURL string for PRISM_HOST_API.
//
// Uses the same short-path trick as startFakeHostAPIServer to stay within
// sockaddr_un.sun_path limits on Darwin (104 bytes).
func startFakeEventAPIServer(t *testing.T) (*fakeEventAPIServer, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "pe")
	if err != nil {
		t.Fatalf("mkdir short sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "host.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	s := &fakeEventAPIServer{}
	srv := &http.Server{Handler: s.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return s, "unix://" + sockPath
}

// setEventFlags sets the given flag values on cmd and returns a cleanup func
// that resets them to their zero values.
func setDoomLoopFlags(t *testing.T, session, tool, pattern, count string) {
	t.Helper()
	_ = eventDoomLoopDetectedCmd.Flags().Set("session", session)
	_ = eventDoomLoopDetectedCmd.Flags().Set("tool", tool)
	_ = eventDoomLoopDetectedCmd.Flags().Set("pattern", pattern)
	_ = eventDoomLoopDetectedCmd.Flags().Set("count", count)
	t.Cleanup(func() {
		_ = eventDoomLoopDetectedCmd.Flags().Set("session", "")
		_ = eventDoomLoopDetectedCmd.Flags().Set("tool", "")
		_ = eventDoomLoopDetectedCmd.Flags().Set("pattern", "")
		_ = eventDoomLoopDetectedCmd.Flags().Set("count", "5")
	})
}

// ── doom-loop-detected proxy tests ───────────────────────────────────────────

// TestEventDoomLoopDetected_ProxiesToHostAPIWhenSet is the headline test:
// when PRISM_HOST_API is set, the doom-loop-detected subcommand sends its
// payload to the sidecar socket rather than touching the local DB.
func TestEventDoomLoopDetected_ProxiesToHostAPIWhenSet(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	setDoomLoopFlags(t, "myrepo@main", "bash", "gh issue view", "5")

	if err := eventDoomLoopDetectedCmd.RunE(eventDoomLoopDetectedCmd, nil); err != nil {
		t.Fatalf("doom-loop-detected RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	got := server.requests[0]
	if got.Path != "/event" {
		t.Errorf("first request path = %q, want /event", got.Path)
	}
	if !strings.Contains(got.Body, `"kind":"doom-loop-detected"`) {
		t.Errorf("request body %q does not include kind=doom-loop-detected", got.Body)
	}
	if !strings.Contains(got.Body, `"session":"myrepo@main"`) {
		t.Errorf("request body %q does not include session=myrepo@main", got.Body)
	}
	if !strings.Contains(got.Body, `"tool":"bash"`) {
		t.Errorf("request body %q does not include tool=bash", got.Body)
	}
}

// TestEventDoomLoopDetected_ProxyDoesNotTouchLocalDB confirms that when
// PRISM_HOST_API is set, no event row is written to the local (sandbox/test) DB.
func TestEventDoomLoopDetected_ProxyDoesNotTouchLocalDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	_, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	setDoomLoopFlags(t, "myrepo@main", "bash", "gh issue view", "5")

	if err := eventDoomLoopDetectedCmd.RunE(eventDoomLoopDetectedCmd, nil); err != nil {
		t.Fatalf("doom-loop-detected RunE: %v", err)
	}

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d.Close()
	events, _ := d.QueryEvents("myrepo@main", 100, nil, nil, nil)
	if len(events) > 0 {
		t.Errorf("local DB has %d event(s) after proxy call — proxy must NOT touch the sandbox DB", len(events))
	}
}

// TestEventDoomLoopDetected_ProxyUnreachableSocketReturnsClearError covers the
// AC edge-case: when PRISM_HOST_API is set but the socket is unreachable, the
// command must return a clear error and exit non-zero without falling back to
// the local DB.
func TestEventDoomLoopDetected_ProxyUnreachableSocketReturnsClearError(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Point at a non-existent socket path.
	bogus := filepath.Join(t.TempDir(), "no-such.sock")
	t.Setenv("PRISM_HOST_API", "unix://"+bogus)

	setDoomLoopFlags(t, "myrepo@main", "bash", "gh issue view", "5")

	err := eventDoomLoopDetectedCmd.RunE(eventDoomLoopDetectedCmd, nil)
	if err == nil {
		t.Fatal("want error for unreachable socket, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "host-API") && !strings.Contains(msg, "socket") {
		t.Errorf("error %q does not mention 'host-API' or 'socket'", msg)
	}

	// DB must be untouched (no shadow write attempted).
	d, openErr := openDB()
	if openErr != nil {
		t.Fatalf("openDB for verify: %v", openErr)
	}
	defer d.Close()
	events, _ := d.QueryEvents("myrepo@main", 100, nil, nil, nil)
	if len(events) > 0 {
		t.Errorf("local DB has %d event(s) after unreachable-socket call — must not fall back to direct DB path", len(events))
	}
}

// ── per-kind smoke tests ──────────────────────────────────────────────────────

// TestEventProxy_CompactionProxies verifies the compaction subcommand proxies
// when PRISM_HOST_API is set.
func TestEventProxy_CompactionProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	_ = eventCompactionCmd.Flags().Set("session", "myrepo@main")
	t.Cleanup(func() { _ = eventCompactionCmd.Flags().Set("session", "") })

	if err := eventCompactionCmd.RunE(eventCompactionCmd, nil); err != nil {
		t.Fatalf("compaction RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].Body, `"kind":"compaction"`) {
		t.Errorf("body %q does not contain kind=compaction", server.requests[0].Body)
	}
}

// TestEventProxy_ErrorSubcmdProxies verifies the error subcommand proxies.
func TestEventProxy_ErrorSubcmdProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	_ = eventErrorCmd.Flags().Set("session", "myrepo@main")
	_ = eventErrorCmd.Flags().Set("message", "something went wrong")
	t.Cleanup(func() {
		_ = eventErrorCmd.Flags().Set("session", "")
		_ = eventErrorCmd.Flags().Set("message", "")
	})

	if err := eventErrorCmd.RunE(eventErrorCmd, nil); err != nil {
		t.Fatalf("error RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	body := server.requests[0].Body
	if !strings.Contains(body, `"kind":"error"`) {
		t.Errorf("body %q does not contain kind=error", body)
	}
	if !strings.Contains(body, "something went wrong") {
		t.Errorf("body %q does not contain the error message", body)
	}
}

// TestEventProxy_TmuxSessionEndProxies verifies tmux-session-end proxies after
// the liveness check passes (session not in tmux).
func TestEventProxy_TmuxSessionEndProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	// Use a name that definitely does not exist in any local tmux server so
	// HasSession returns false and the liveness guard passes.
	const sess = "nonexistent-proxy-test@no-branch"
	_ = eventTmuxSessionEndCmd.Flags().Set("session", sess)
	t.Cleanup(func() { _ = eventTmuxSessionEndCmd.Flags().Set("session", "") })

	if err := eventTmuxSessionEndCmd.RunE(eventTmuxSessionEndCmd, nil); err != nil {
		t.Fatalf("tmux-session-end RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].Body, `"kind":"tmux-session-end"`) {
		t.Errorf("body %q does not contain kind=tmux-session-end", server.requests[0].Body)
	}
}

// TestEventProxy_ServerReturns400ForUnknownKindSurfacesError verifies that
// when the server returns a 400 (e.g. for an unknown kind), the CLI returns a
// non-nil error containing the server's message. This covers the AC edge-case
// where an unknown kind is rejected with a clear error message.
func TestEventProxy_ServerReturns400ForUnknownKindSurfacesError(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	server.failStatus = http.StatusBadRequest
	server.failError = `unknown event kind "badkind" — must be one of: compaction, doom-loop-detected, error, pane-died, state-change, tmux-session-end, tmux-session-start`
	t.Setenv("PRISM_HOST_API", apiURL)

	setDoomLoopFlags(t, "myrepo@main", "bash", "pat", "5")

	err := eventDoomLoopDetectedCmd.RunE(eventDoomLoopDetectedCmd, nil)
	if err == nil {
		t.Fatal("want error when server returns 400, got nil")
	}
	if !strings.Contains(err.Error(), "unknown event kind") {
		t.Errorf("error %q does not mention 'unknown event kind'", err.Error())
	}
}

// TestEventProxy_StateChangeProxies verifies the state-change subcommand
// proxies to the host API when PRISM_HOST_API is set.
func TestEventProxy_StateChangeProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	_ = eventStateChangeCmd.Flags().Set("session", "myrepo@main")
	_ = eventStateChangeCmd.Flags().Set("state", "active")
	_ = eventStateChangeCmd.Flags().Set("worktree", t.TempDir())
	t.Cleanup(func() {
		_ = eventStateChangeCmd.Flags().Set("session", "")
		_ = eventStateChangeCmd.Flags().Set("state", "")
		_ = eventStateChangeCmd.Flags().Set("worktree", "")
	})

	if err := eventStateChangeCmd.RunE(eventStateChangeCmd, nil); err != nil {
		t.Fatalf("state-change RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].Body, `"kind":"state-change"`) {
		t.Errorf("body %q does not contain kind=state-change", server.requests[0].Body)
	}
}

// TestEventProxy_PaneDiedProxies verifies the pane-died subcommand proxies to
// the host API when PRISM_HOST_API is set.
func TestEventProxy_PaneDiedProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	_ = eventPaneDiedCmd.Flags().Set("session", "myrepo@main")
	_ = eventPaneDiedCmd.Flags().Set("window", "agent")
	t.Cleanup(func() {
		_ = eventPaneDiedCmd.Flags().Set("session", "")
		_ = eventPaneDiedCmd.Flags().Set("window", "")
	})

	if err := eventPaneDiedCmd.RunE(eventPaneDiedCmd, nil); err != nil {
		t.Fatalf("pane-died RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].Body, `"kind":"pane-died"`) {
		t.Errorf("body %q does not contain kind=pane-died", server.requests[0].Body)
	}
}

// TestEventProxy_TmuxSessionStartProxies verifies the tmux-session-start
// subcommand proxies to the host API when PRISM_HOST_API is set.
func TestEventProxy_TmuxSessionStartProxies(t *testing.T) {
	server, apiURL := startFakeEventAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	_ = eventTmuxSessionStartCmd.Flags().Set("session", "myrepo@main")
	_ = eventTmuxSessionStartCmd.Flags().Set("worktree", t.TempDir())
	t.Cleanup(func() {
		_ = eventTmuxSessionStartCmd.Flags().Set("session", "")
		_ = eventTmuxSessionStartCmd.Flags().Set("worktree", "")
	})

	if err := eventTmuxSessionStartCmd.RunE(eventTmuxSessionStartCmd, nil); err != nil {
		t.Fatalf("tmux-session-start RunE: %v", err)
	}

	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].Body, `"kind":"tmux-session-start"`) {
		t.Errorf("body %q does not contain kind=tmux-session-start", server.requests[0].Body)
	}
}

// ── host path unchanged ───────────────────────────────────────────────────────

// TestEventProxy_NoProxyWhenHostAPIUnset verifies that when PRISM_HOST_API is
// unset, the subcommand uses the direct DB path (host behaviour unchanged).
func TestEventProxy_NoProxyWhenHostAPIUnset(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	// Seed the DB with a session row so the direct DB path succeeds.
	dbFile := setupEventTestDB(t, "myrepo@main")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	_ = eventCompactionCmd.Flags().Set("session", "myrepo@main")
	t.Cleanup(func() { _ = eventCompactionCmd.Flags().Set("session", "") })

	if err := eventCompactionCmd.RunE(eventCompactionCmd, nil); err != nil {
		t.Fatalf("compaction RunE (direct DB path): %v", err)
	}

	// The event should be in the local DB (direct path fired).
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d.Close()
	events, _ := d.QueryEvents("myrepo@main", 10, nil, nil, []string{"compaction"})
	if len(events) == 0 {
		t.Error("no compaction event in local DB — direct DB path did not write the event")
	}
}
