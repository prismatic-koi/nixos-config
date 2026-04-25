package cmd

// Tests for the PRISM_HOST_API proxy paths in cmd/merge.go and cmd/merges.go
// added in #1043 to fix the bwrap shadow-DB issue.
//
// These tests stand up a real httptest server bound to a Unix socket on disk
// and point PRISM_HOST_API at it, then exercise runMerge / runMergesList /
// runMergesCancel and assert that the request reached the host (mirror of
// the sidecar /merge handler) rather than touching the local DB.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHostAPIServer is a minimal mirror of the sidecar's /merge, /merges, and
// /merges/cancel routes. It records the requests it receives so tests can
// assert that the proxy hit the socket instead of the local DB.
type fakeHostAPIServer struct {
	mu       sync.Mutex
	requests []recordedRequest

	// merges holds pending_merges-shaped rows for /merges responses.
	merges []map[string]any
	// cancelOK is the value returned for the /merges/cancel response.
	cancelOK bool
	// cancelRow is returned alongside cancelOK.
	cancelRow map[string]any
	// failStatus, when non-zero, makes /merge return that status with
	// {"error": failError}.
	failStatus int
	failError  string
}

type recordedRequest struct {
	Path string
	Body string
	URL  string
}

func (s *fakeHostAPIServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/merge", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{Path: "/merge", Body: string(body), URL: r.URL.String()})
		fail := s.failStatus
		failMsg := s.failError
		s.mu.Unlock()
		if fail != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fail)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": failMsg})
			return
		}
		// Echo back a minimal PendingMerge-shaped response.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"PR":            42,
			"QueuePosition": 1,
			"Status":        "watching",
		})
	})
	mux.HandleFunc("/merges", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{Path: "/merges", URL: r.URL.String()})
		merges := s.merges
		s.mu.Unlock()
		if merges == nil {
			merges = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(merges)
	})
	mux.HandleFunc("/merges/cancel", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{Path: "/merges/cancel", Body: string(body)})
		ok := s.cancelOK
		row := s.cancelRow
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cancelled": ok,
			"row":       row,
		})
	})
	return mux
}

// startFakeHostAPIServer starts the fake server bound to a unix socket inside
// t.TempDir() and returns the server, the apiURL string for PRISM_HOST_API,
// and a teardown function.
func startFakeHostAPIServer(t *testing.T) (*fakeHostAPIServer, string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "host.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	s := &fakeHostAPIServer{}
	srv := &http.Server{Handler: s.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return s, "unix://" + sockPath
}

// stubGhBin replaces `gh` on PATH with a fake script that returns a fixed JSON
// PR view, so preflight() succeeds in the test. The script writes to stderr
// the same "enqueueing PR ..." line that real gh would have triggered, but
// that comes from preflight, not gh.
func stubGhBin(t *testing.T, pr int, state, title string) {
	t.Helper()
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
# Stub gh — returns a fake PR view.
exec cat <<EOF
{"state":"%s","number":%d,"title":"%s"}
EOF
`, state, pr, title)
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	// Prepend the dir to PATH for the test so preflight() resolves our stub.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// ── runMerge proxy tests ──────────────────────────────────────────────────────

// TestRunMerge_ProxiesToHostAPIWhenSet is the headline test: when
// PRISM_HOST_API is set, runMerge sends the enqueue request to the sidecar
// socket rather than touching the local DB. This is the fix for #1043.
func TestRunMerge_ProxiesToHostAPIWhenSet(t *testing.T) {
	openMergeTestDB(t) // sets PRISM_HOST_API="" — we override below.
	stubGhBin(t, 42, "OPEN", "test PR")

	server, apiURL := startFakeHostAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	if err := runMerge(mergeCmd, []string{"42"}); err != nil {
		t.Fatalf("runMerge: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	got := server.requests[0]
	if got.Path != "/merge" {
		t.Errorf("first request path = %q, want /merge", got.Path)
	}
	// Body must include the PR number; title may be the stubbed value.
	if !strings.Contains(got.Body, `"pr":42`) {
		t.Errorf("request body %q does not include pr=42", got.Body)
	}
}

// TestRunMerge_ProxyDoesNotTouchLocalDB confirms that when PRISM_HOST_API is
// set, no row is written to the local (sandbox/test) DB. This is the
// fundamental bug being fixed: shadow-DB writes silently land on the wrong
// side of the bwrap namespace boundary.
func TestRunMerge_ProxyDoesNotTouchLocalDB(t *testing.T) {
	openMergeTestDB(t)
	stubGhBin(t, 42, "OPEN", "test PR")

	_, apiURL := startFakeHostAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	if err := runMerge(mergeCmd, []string{"42"}); err != nil {
		t.Fatalf("runMerge: %v", err)
	}

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d.Close()
	row, _ := d.PendingMergeByPR(42)
	if row != nil {
		t.Errorf("local DB row exists after proxy call: %+v — proxy must NOT touch the sandbox DB", row)
	}
}

// TestRunMerge_ProxyUnreachableSocketReturnsClearError covers AC-6: a bwrap
// session whose host-API socket is unreachable must NOT silently fall back to
// the direct DB path. It must return a clear error and exit non-zero.
func TestRunMerge_ProxyUnreachableSocketReturnsClearError(t *testing.T) {
	openMergeTestDB(t)
	stubGhBin(t, 42, "OPEN", "test PR")

	// Point at a non-existent socket path.
	bogus := filepath.Join(t.TempDir(), "no-such.sock")
	t.Setenv("PRISM_HOST_API", "unix://"+bogus)

	err := runMerge(mergeCmd, []string{"42"})
	if err == nil {
		t.Fatal("runMerge: want error for unreachable socket, got nil")
	}
	// The error should mention the socket so the user can diagnose it. Avoid
	// asserting exact wording — just confirm it's not silently swallowed and
	// references the host-API or socket layer.
	msg := err.Error()
	if !strings.Contains(msg, "host-API") && !strings.Contains(msg, "socket") {
		t.Errorf("error %q does not mention 'host-API' or 'socket' — fallback to direct DB?", msg)
	}

	// And the DB must be untouched (no shadow write attempted).
	d, openErr := openDB()
	if openErr != nil {
		t.Fatalf("openDB for verify: %v", openErr)
	}
	defer d.Close()
	row, _ := d.PendingMergeByPR(42)
	if row != nil {
		t.Errorf("local DB row exists after unreachable-socket call: %+v — must not fall back to direct DB path", row)
	}
}

// ── runMergesList proxy tests ─────────────────────────────────────────────────

func TestRunMergesList_ProxiesToHostAPIWhenSet(t *testing.T) {
	openMergeTestDB(t)
	server, apiURL := startFakeHostAPIServer(t)
	server.mu.Lock()
	server.merges = []map[string]any{}
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)

	if err := runMergesList(mergesListCmd, nil); err != nil {
		t.Fatalf("runMergesList: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if server.requests[0].Path != "/merges" {
		t.Errorf("first request path = %q, want /merges", server.requests[0].Path)
	}
}

func TestRunMergesList_ProxyPassesFilterFlag(t *testing.T) {
	openMergeTestDB(t)
	server, apiURL := startFakeHostAPIServer(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	// Set the --failed flag on the command for this invocation.
	if err := mergesListCmd.Flags().Set("failed", "true"); err != nil {
		t.Fatalf("set --failed: %v", err)
	}
	t.Cleanup(func() { _ = mergesListCmd.Flags().Set("failed", "false") })

	if err := runMergesList(mergesListCmd, nil); err != nil {
		t.Fatalf("runMergesList: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	if !strings.Contains(server.requests[0].URL, "filter=failed") {
		t.Errorf("request URL %q does not include filter=failed", server.requests[0].URL)
	}
}

// ── runMergesCancel proxy tests ───────────────────────────────────────────────

func TestRunMergesCancel_ProxiesToHostAPIWhenSet(t *testing.T) {
	openMergeTestDB(t)
	server, apiURL := startFakeHostAPIServer(t)
	server.mu.Lock()
	server.cancelOK = true
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)

	if err := runMergesCancel(mergesCancelCmd, []string{"55"}); err != nil {
		t.Fatalf("runMergesCancel: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) == 0 {
		t.Fatal("server received no requests — proxy did not fire")
	}
	got := server.requests[0]
	if got.Path != "/merges/cancel" {
		t.Errorf("path = %q, want /merges/cancel", got.Path)
	}
	if !strings.Contains(got.Body, `"pr":55`) {
		t.Errorf("body %q does not include pr=55", got.Body)
	}
}
