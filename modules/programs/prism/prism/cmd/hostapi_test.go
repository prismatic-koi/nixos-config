package cmd

// Unit tests for proxyToHostAPI and the container-mode proxy paths in
// spawn/cleanup/switch (A-3, issue #509).
//
// Each test spins up a real Unix socket server (net.Listen("unix", ...)) in
// process, sets PRISM_HOST_API to point at it, and verifies that:
//   - the correct HTTP method, path, and JSON body are sent
//   - the session_name is printed for spawn
//   - error responses are surfaced correctly
//   - when PRISM_HOST_API is unset, the proxy check is a no-op

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// mockUnixServer starts a minimal HTTP server on a fresh Unix socket and
// returns the socket path and a channel that receives the first request body
// decoded as a map[string]any. The handler responds with the given handlerFn.
type mockUnixServer struct {
	sockPath string
	listener net.Listener
	server   *http.Server
}

// newMockUnixServer creates a server that listens on a Unix socket in tmpDir.
// handlerFn is called for every request; it writes the response.
func newMockUnixServer(t *testing.T, handlerFn http.HandlerFunc) *mockUnixServer {
	t.Helper()
	// Use os.TempDir() + a short name to avoid hitting the 104-char Unix socket
	// path limit on macOS (t.TempDir() produces very long paths).
	sockPath := filepath.Join(os.TempDir(), "prism-test-"+randCmdHex(6)+".sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: handlerFn}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	return &mockUnixServer{sockPath: sockPath, listener: ln, server: srv}
}

// apiURL returns the unix:///path URL for this server.
func (m *mockUnixServer) apiURL() string {
	return "unix://" + m.sockPath
}

// ── proxyToHostAPI unit tests ─────────────────────────────────────────────────

// TestProxyToHostAPI_SendsCorrectRequestAndParsesResponse verifies AC-4 and
// AC-10: the function dials the Unix socket, POSTs to the correct path, sends
// the expected JSON body, and unmarshals the response into respDst.
func TestProxyToHostAPI_SendsCorrectRequestAndParsesResponse(t *testing.T) {
	type reqBody struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}

	type respBody struct {
		SessionName string `json:"session_name"`
	}

	var capturedMethod string
	var capturedPath string
	var capturedBody reqBody

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"myrepo@my-branch"}`))
	})

	var got respBody
	err := proxyToHostAPI(srv.apiURL(), "/spawn", reqBody{Repo: "myrepo", Branch: "my-branch"}, &got)
	if err != nil {
		t.Fatalf("proxyToHostAPI: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", capturedMethod)
	}
	if capturedPath != "/spawn" {
		t.Errorf("path = %q, want /spawn", capturedPath)
	}
	if capturedBody.Repo != "myrepo" {
		t.Errorf("body.repo = %q, want %q", capturedBody.Repo, "myrepo")
	}
	if capturedBody.Branch != "my-branch" {
		t.Errorf("body.branch = %q, want %q", capturedBody.Branch, "my-branch")
	}
	if got.SessionName != "myrepo@my-branch" {
		t.Errorf("response session_name = %q, want %q", got.SessionName, "myrepo@my-branch")
	}
}

// TestProxyToHostAPI_ReturnsErrorOnHTTP500 verifies AC-5: when the server
// returns HTTP 500 with {"error":"spawn failed"}, proxyToHostAPI returns a
// non-nil error wrapping that message.
func TestProxyToHostAPI_ReturnsErrorOnHTTP500(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"spawn failed: something went wrong"}`))
	})

	err := proxyToHostAPI(srv.apiURL(), "/spawn", map[string]any{"repo": "r", "branch": "b"}, nil)
	if err == nil {
		t.Fatal("expected non-nil error, got nil")
	}
	if !strings.Contains(err.Error(), "spawn failed") {
		t.Errorf("error %q does not contain %q", err.Error(), "spawn failed")
	}
}

// TestProxyToHostAPI_MalformedURL verifies AC-8: a URL that does not start
// with "unix://" returns a clear error rather than panicking.
func TestProxyToHostAPI_MalformedURL(t *testing.T) {
	err := proxyToHostAPI("http://localhost:1234", "/spawn", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected non-nil error for unsupported scheme")
	}
	if !strings.Contains(err.Error(), "unsupported scheme") {
		t.Errorf("error %q does not mention unsupported scheme", err.Error())
	}
}

// TestProxyToHostAPI_SocketNotFound verifies AC-9: when the socket path does
// not exist, the error message mentions the socket path.
func TestProxyToHostAPI_SocketNotFound(t *testing.T) {
	nonExistent := filepath.Join(t.TempDir(), "nonexistent.sock")
	err := proxyToHostAPI("unix://"+nonExistent, "/spawn", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected non-nil error when socket is missing")
	}
	if !strings.Contains(err.Error(), nonExistent) {
		t.Errorf("error %q does not mention socket path %q", err.Error(), nonExistent)
	}
}

// TestParseUnixSocketURL_ValidAndInvalid tests the URL parser directly.
func TestParseUnixSocketURL_ValidAndInvalid(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		want    string
	}{
		{"unix:///var/run/prism-host/test-hostapi.sock", false, "/var/run/prism-host/test-hostapi.sock"},
		{"unix:///tmp/foo.sock", false, "/tmp/foo.sock"},
		{"http://localhost:1234", true, ""},
		{"unix://", true, ""},
		{"", true, ""},
	}

	for _, tc := range tests {
		got, err := parseUnixSocketURL(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseUnixSocketURL(%q): expected error, got nil", tc.input)
			}
		} else {
			if err != nil {
				t.Errorf("parseUnixSocketURL(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseUnixSocketURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		}
	}
}

// ── spawn proxy tests (AC-1, AC-11) ──────────────────────────────────────────

// TestProxySpawn_SendsCorrectPayload verifies AC-11 for spawn: when
// PRISM_HOST_API is set and proxySpawn is called, the mock server receives
// the expected JSON payload. Setting PRISM_BARE_ROOT to a container mount path
// (e.g. "/prism-git" whose filepath.Base differs from the actual repo name)
// confirms that the client-side repo derivation defect (issue #616) is no
// longer exercised: the client does not send a "repo" field at all, and the
// server substitutes its own repo name.
func TestProxySpawn_SendsCorrectPayload(t *testing.T) {
	type spawnReq struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Prompt string `json:"prompt"`
		Agent  string `json:"agent"`
	}

	reqCh := make(chan spawnReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spawn" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req spawnReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"nixos-config@test-branch"}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())
	// Set PRISM_BARE_ROOT to a container mount path whose filepath.Base
	// ("prism-git") does not match the actual repo name ("nixos-config").
	// The client must NOT derive the repo from this value — it should omit
	// the repo field entirely, leaving the server to substitute ownRepo.
	t.Setenv("PRISM_BARE_ROOT", "/prism-git")

	// Build a cobra command with the same flags as spawnCmd.
	cmd := &cobra.Command{Use: "spawn"}
	cmd.Flags().String("branch", "", "")
	cmd.Flags().String("agent", "", "")
	cmd.Flags().Bool("host-mode", false, "")
	addPromptFlags(cmd)
	_ = cmd.Flags().Set("branch", "test-branch")
	_ = cmd.Flags().Set("agent", "worker")
	_ = cmd.Flags().Set("prompt", "hello world")

	if err := proxySpawn(srv.apiURL(), cmd); err != nil {
		t.Fatalf("proxySpawn: %v", err)
	}

	select {
	case req := <-reqCh:
		// The client must NOT send a repo field (empty string in decoded struct).
		if req.Repo != "" {
			t.Errorf("repo = %q, want empty (client must not send repo; server derives it)", req.Repo)
		}
		if req.Branch != "test-branch" {
			t.Errorf("branch = %q, want %q", req.Branch, "test-branch")
		}
		if req.Prompt != "hello world" {
			t.Errorf("prompt = %q, want %q", req.Prompt, "hello world")
		}
		if req.Agent != "worker" {
			t.Errorf("agent = %q, want %q", req.Agent, "worker")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// ── cleanup proxy tests (AC-2, AC-11) ─────────────────────────────────────────

// TestHeadlessCleanup_Proxy verifies AC-11 for cleanup: when PRISM_HOST_API is
// set, headlessCleanup POSTs to /cleanup with the session name and yes:true,
// instead of touching tmux.
func TestHeadlessCleanup_Proxy(t *testing.T) {
	type cleanupReq struct {
		Session string `json:"session"`
		Yes     bool   `json:"yes"`
	}

	reqCh := make(chan cleanupReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cleanup" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req cleanupReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	if err := headlessCleanup("myrepo@my-branch", "my-branch", "/workspace", "/prism-git"); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Session != "myrepo@my-branch" {
			t.Errorf("session = %q, want %q", req.Session, "myrepo@my-branch")
		}
		if !req.Yes {
			t.Errorf("yes = false, want true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestHeadlessCloseSession_Proxy verifies that headlessCloseSession also
// proxies to /cleanup when PRISM_HOST_API is set.
func TestHeadlessCloseSession_Proxy(t *testing.T) {
	type cleanupReq struct {
		Session string `json:"session"`
		Yes     bool   `json:"yes"`
	}

	reqCh := make(chan cleanupReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req cleanupReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	if err := headlessCloseSession("myrepo@main"); err != nil {
		t.Fatalf("headlessCloseSession: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Session != "myrepo@main" {
			t.Errorf("session = %q, want %q", req.Session, "myrepo@main")
		}
		if !req.Yes {
			t.Errorf("yes = false, want true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// ── switch proxy tests (AC-3, AC-11) ──────────────────────────────────────────

// TestSwitchProxy_SendsCorrectPayload verifies AC-11 for switch: when
// PRISM_HOST_API is set, switchCmd.RunE POSTs to /switch with the session
// (path) value from the --path flag.
func TestSwitchProxy_SendsCorrectPayload(t *testing.T) {
	type switchReq struct {
		Session string `json:"session"`
	}

	reqCh := make(chan switchReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/switch" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req switchReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	// Exercise switchCmd.RunE directly (not proxyToHostAPI) to ensure the
	// proxy guard in switch.go is what sends the request.
	cmd := &cobra.Command{}
	cmd.Flags().String("path", "", "")
	_ = cmd.Flags().Set("path", "/workspace/my-project")

	if err := switchCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("switchCmd.RunE: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Session != "/workspace/my-project" {
			t.Errorf("session = %q, want %q", req.Session, "/workspace/my-project")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// ── regression: host mode (PRISM_HOST_API unset) ──────────────────────────────

// TestProxyToHostAPI_NotCalledWhenEnvUnset verifies AC-6: when PRISM_HOST_API
// is not set, proxyToHostAPI is never called (the check is a no-op).
// We verify this indirectly by confirming the mock server receives zero requests.
func TestProxyToHostAPI_NotCalledWhenEnvUnset(t *testing.T) {
	received := make(chan struct{}, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})

	// Ensure PRISM_HOST_API is explicitly unset.
	t.Setenv("PRISM_HOST_API", "")

	// headlessCleanup with no PRISM_HOST_API should NOT contact the server.
	// We use a temp DB to avoid touching production state.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// headlessCleanup with empty worktreePath and bareRoot should just update
	// the DB (or silently fail) but never contact the mock server.
	_ = headlessCleanup("myrepo@host-mode", "host-mode", "", "")

	// The mock server should not have received any request.
	select {
	case <-received:
		t.Errorf("mock server received a request even though PRISM_HOST_API is unset — proxy check is not guarded")
	case <-time.After(200 * time.Millisecond):
		// ka pai — no request received
	}

	_ = srv // keep compiler happy
}

// ── proxyGetFromHostAPI unit tests ────────────────────────────────────────────

// TestProxyGetFromHostAPI_SendsGETWithQueryParams verifies that
// proxyGetFromHostAPI sends a GET request with the correct query parameters.
func TestProxyGetFromHostAPI_SendsGETWithQueryParams(t *testing.T) {
	var capturedMethod string
	var capturedPath string
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})

	var got []any
	err := proxyGetFromHostAPI(srv.apiURL(), "/list-sessions",
		map[string]string{"all": "true"}, &got)
	if err != nil {
		t.Fatalf("proxyGetFromHostAPI: %v", err)
	}

	if capturedMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", capturedMethod)
	}
	if capturedPath != "/list-sessions" {
		t.Errorf("path = %q, want /list-sessions", capturedPath)
	}
	if capturedQuery != "all=true" {
		t.Errorf("query = %q, want all=true", capturedQuery)
	}
}

// TestProxyGetFromHostAPI_Returns403AsError verifies that a 403 response from
// the server is surfaced as an error with the error message.
func TestProxyGetFromHostAPI_Returns403AsError(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"workers cannot list sessions across all repos"}`))
	})

	err := proxyGetFromHostAPI(srv.apiURL(), "/list-sessions",
		map[string]string{"all": "true"}, nil)
	if err == nil {
		t.Fatal("expected non-nil error for 403 response")
	}
	if !strings.Contains(err.Error(), "workers cannot list") {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}

// ── proxyListSessions unit tests ──────────────────────────────────────────────

// TestProxyListSessions_SendsGetRequest verifies that proxyListSessions sends
// a GET /list-sessions request (no all param by default).
func TestProxyListSessions_SendsGetRequest(t *testing.T) {
	var capturedPath string
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"SessionName":"myrepo@main","State":"active"}]`))
	})

	raw, err := proxyListSessions(srv.apiURL(), false)
	if err != nil {
		t.Fatalf("proxyListSessions: %v", err)
	}
	if capturedPath != "/list-sessions" {
		t.Errorf("path = %q, want /list-sessions", capturedPath)
	}
	if capturedQuery != "" {
		t.Errorf("query = %q, want empty (all=false)", capturedQuery)
	}
	if len(raw) == 0 {
		t.Error("expected non-empty raw response")
	}
}

// TestProxyListSessions_SendsAllParam verifies that showAll=true adds all=true.
func TestProxyListSessions_SendsAllParam(t *testing.T) {
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})

	_, err := proxyListSessions(srv.apiURL(), true)
	if err != nil {
		t.Fatalf("proxyListSessions: %v", err)
	}
	if capturedQuery != "all=true" {
		t.Errorf("query = %q, want all=true", capturedQuery)
	}
}

// ── proxyCheckin unit tests ───────────────────────────────────────────────────

// TestProxyCheckin_SendsCorrectQueryParams verifies that proxyCheckin sends the
// correct GET /checkin request with session and last params.
func TestProxyCheckin_SendsCorrectQueryParams(t *testing.T) {
	var capturedQuery string

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session":"myrepo@main","state":"active","events":[]}`))
	})

	raw, err := proxyCheckin(srv.apiURL(), "myrepo@main", 5, nil, nil, nil, false)
	if err != nil {
		t.Fatalf("proxyCheckin: %v", err)
	}
	// url.Values.Encode() percent-encodes the "@" as "%40".
	if !strings.Contains(capturedQuery, "session=myrepo%40main") {
		t.Errorf("query %q does not contain properly encoded session param (want session=myrepo%%40main)", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "last=5") {
		t.Errorf("query %q does not contain last=5", capturedQuery)
	}
	if len(raw) == 0 {
		t.Error("expected non-empty raw response")
	}
}

// ── proxyPrompt unit tests ────────────────────────────────────────────────────

// TestProxyPrompt_SendsCorrectPayload verifies that proxyPrompt POSTs to
// /prompt with the correct JSON body.
func TestProxyPrompt_SendsCorrectPayload(t *testing.T) {
	type promptReq struct {
		Session string `json:"session"`
		Prompt  string `json:"prompt"`
		Urgent  bool   `json:"urgent"`
	}

	reqCh := make(chan promptReq, 1)

	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prompt" {
			http.Error(w, `{"error":"wrong path"}`, http.StatusBadRequest)
			return
		}
		var req promptReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqCh <- req
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	if err := proxyPrompt(srv.apiURL(), "myrepo@main", "do the thing", false); err != nil {
		t.Fatalf("proxyPrompt: %v", err)
	}

	select {
	case req := <-reqCh:
		if req.Session != "myrepo@main" {
			t.Errorf("session = %q, want myrepo@main", req.Session)
		}
		if req.Prompt != "do the thing" {
			t.Errorf("prompt = %q, want 'do the thing'", req.Prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
	}
}

// TestProxyPrompt_Returns403AsError verifies that a 403 from the server
// (e.g. worker trying wrong target) is surfaced as an error.
func TestProxyPrompt_Returns403AsError(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"workers can only prompt their own coordinator"}`))
	})

	err := proxyPrompt(srv.apiURL(), "otherrepo@main", "hello", false)
	if err == nil {
		t.Fatal("expected non-nil error for 403 response")
	}
	if !strings.Contains(err.Error(), "workers can only prompt") {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}
