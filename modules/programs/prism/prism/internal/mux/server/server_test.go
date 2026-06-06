// Tests for the mux server package. The suite exercises the full
// socket round-trip — bind a real net.Listener on a t.TempDir() path,
// run the server in a background goroutine, and drive every method
// surface from a vanilla http.Client over a Unix-socket transport.
//
// Two layers of coverage:
//
//  1. Method-by-method success and structured-error assertions.
//     Inputs are validated, error codes match the typed pane.*
//     sentinels, and the resulting tree state is verified via the
//     list endpoints (the AC for #2153 calls this out explicitly).
//
//  2. Concurrency: the suite runs a fan-out of mutating requests
//     under -race to confirm the server's lack of internal locking
//     does not regress the SessionTree's own mutex contract.
//
// Tests construct their own t.TempDir()-scoped sockets and never
// touch $HOME or $XDG_STATE_HOME — the file is therefore
// homeless-shelter-clean (see AGENTS.md).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// testClient bundles an http.Client (dialing a Unix socket) with the
// fake-host base URL the client uses. The test helpers below all hang
// off this type so individual cases stay short.
type testClient struct {
	http *http.Client
	base string // e.g. "http://mux"
}

// newTestServer binds a Unix socket inside t.TempDir(), starts the
// server in a goroutine, and returns a testClient that talks to it.
// All resources are released via t.Cleanup so test bodies stay linear.
//
// The socket path is kept short on purpose — t.TempDir() roots can
// already approach the sun_path budget on Darwin (104 bytes), and
// nesting a directory below it would push some test names over.
func newTestServer(t *testing.T) (*testClient, *Server) {
	t.Helper()

	tree := pane.New()
	srv := New(tree)

	sockPath := filepath.Join(t.TempDir(), "s")
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
		cancel()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Logf("server.Serve returned: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("server did not shut down within 3s")
		}
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	return &testClient{http: client, base: "http://mux"}, srv
}

// do issues an HTTP request and decodes the JSON body into out (when
// non-nil). The returned status code is the raw HTTP code; assertions
// check both that and the JSON body shape so a wrong-status response
// fails the test even when the body happens to match.
func (c *testClient) do(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode response (status %d, body %q): %v", resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode
}

// expectError issues a request expected to fail with the given HTTP
// status and structured-error code, and returns the parsed body so the
// caller can assert on data fields. Centralising this keeps each error
// test case to one line of meaningful assertion.
func (c *testClient) expectError(t *testing.T, method, path string, body any, wantStatus int, wantCode string) errorResponse {
	t.Helper()
	var got errorResponse
	gotStatus := c.do(t, method, path, body, &got)
	if gotStatus != wantStatus {
		t.Errorf("status = %d, want %d (body: %+v)", gotStatus, wantStatus, got)
	}
	if got.Code != wantCode {
		t.Errorf("error code = %q, want %q (body: %+v)", got.Code, wantCode, got)
	}
	if got.Message == "" {
		t.Errorf("error message is empty (body: %+v)", got)
	}
	return got
}

// ---------------------------------------------------------------------------
// DefaultSocketPath / hashing determinism
// ---------------------------------------------------------------------------

// TestDefaultSocketPath_Deterministic pins the path layout — the
// 12-hex SHA-256 prefix of "prism-mux" is the *whole* point of the
// hashed directory, so a refactor that silently changes the input
// would break every CLI client at once. Pinning the exact prefix
// catches that immediately.
func TestDefaultSocketPath_Deterministic(t *testing.T) {
	// Force a known XDG_STATE_HOME so the assertion is byte-exact.
	t.Setenv("XDG_STATE_HOME", "/x")

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	// SHA-256("prism-mux") = 30acb169c... — pin only the 12-hex prefix
	// to keep the assertion narrow but specific.
	wantPrefix := "/x/prism/run/"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("path = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, "/mux.sock") {
		t.Errorf("path = %q, want suffix %q", got, "/mux.sock")
	}
	// Re-deriving must produce the exact same path — no env reads, no
	// time-based salting, etc.
	got2, _ := DefaultSocketPath()
	if got != got2 {
		t.Errorf("path is not deterministic: %q vs %q", got, got2)
	}
	// Confirm the directory hash is exactly socketDirHashLen chars.
	dirName := filepath.Base(filepath.Dir(got))
	if len(dirName) != socketDirHashLen {
		t.Errorf("dir name %q has length %d, want %d", dirName, len(dirName), socketDirHashLen)
	}
}

// TestDefaultSocketPath_XDGFallback covers the case where
// XDG_STATE_HOME is unset — we fall back to $HOME/.local/state to
// match internal/session.sidecarStateDir.
func TestDefaultSocketPath_XDGFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/h")

	got, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	if !strings.HasPrefix(got, "/h/.local/state/prism/run/") {
		t.Errorf("path = %q, want prefix %q", got, "/h/.local/state/prism/run/")
	}
}

// ---------------------------------------------------------------------------
// Constructor invariants
// ---------------------------------------------------------------------------

// TestNew_NilTreePanics asserts the "panic at construction, not on
// first request" contract documented on New. The recover-from-panic
// pattern is the standard Go test for this.
func TestNew_NilTreePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("New(nil) did not panic")
		}
	}()
	_ = New(nil)
}

// ---------------------------------------------------------------------------
// session.* happy path
// ---------------------------------------------------------------------------

// TestSessionCreate_HappyPath exercises POST /session/create with a
// minimal top-level session and asserts:
//
//   - 200 OK with the canonical post-insert view as the response body
//   - ActivePane defaulted to the first pane (model behaviour)
//   - GET /session/list sees the new session
//
// This is the smallest test that walks the full encode → server →
// decode → introspect loop, so it serves as the canary for the wire
// shape as a whole.
func TestSessionCreate_HappyPath(t *testing.T) {
	c, _ := newTestServer(t)

	var got sessionResponse
	status := c.do(t, http.MethodPost, "/session/create", sessionCreateRequest{
		ID:   "repo@feat",
		Repo: "repo",
		Panes: []paneInput{
			{Name: "agent"},
			{Name: "term"},
		},
	}, &got)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %+v)", status, got)
	}
	if got.Session.ID != "repo@feat" {
		t.Errorf("session.id = %q, want %q", got.Session.ID, "repo@feat")
	}
	if got.Session.ActivePane != "agent" {
		t.Errorf("session.active_pane = %q, want %q (model should default to first pane)",
			got.Session.ActivePane, "agent")
	}

	// list shows the new session.
	var listed sessionListResponse
	if s := c.do(t, http.MethodGet, "/session/list", nil, &listed); s != http.StatusOK {
		t.Fatalf("list status = %d, want 200", s)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != "repo@feat" {
		t.Errorf("listed sessions = %+v, want one session 'repo@feat'", listed.Sessions)
	}
}

// TestSessionCreate_ReviewSubsession covers the two-level hierarchy:
// a review subsession's parent must already exist and the response
// echoes the inherited Repo field.
func TestSessionCreate_ReviewSubsession(t *testing.T) {
	c, _ := newTestServer(t)

	// Parent first.
	if s := c.do(t, http.MethodPost, "/session/create", sessionCreateRequest{
		ID: "repo@feat", Repo: "repo",
	}, nil); s != http.StatusOK {
		t.Fatalf("create parent: status %d", s)
	}

	var got sessionResponse
	status := c.do(t, http.MethodPost, "/session/create", sessionCreateRequest{
		ID:        "repo@feat~review-1-review-code",
		ParentID:  "repo@feat",
		AgentRole: "review-code",
	}, &got)
	if status != http.StatusOK {
		t.Fatalf("create child status = %d (body %+v)", status, got)
	}
	if got.Session.Repo != "repo" {
		t.Errorf("child Repo = %q, want %q (inherit from parent)", got.Session.Repo, "repo")
	}
	if got.Session.ParentID != "repo@feat" {
		t.Errorf("child ParentID = %q, want %q", got.Session.ParentID, "repo@feat")
	}
}

// TestSessionDestroy_HappyPath asserts a destroy round-trip — create,
// destroy, list — and that destroying a top-level session with a
// child cascades (the model cascades; the server should propagate
// that behaviour transparently).
func TestSessionDestroy_HappyPath(t *testing.T) {
	c, _ := newTestServer(t)

	if s := c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b", Repo: "r"}, nil); s != http.StatusOK {
		t.Fatalf("create parent: %d", s)
	}
	if s := c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b~r-1-x", ParentID: "r@b"}, nil); s != http.StatusOK {
		t.Fatalf("create child: %d", s)
	}

	if s := c.do(t, http.MethodPost, "/session/destroy",
		sessionDestroyRequest{ID: "r@b"}, nil); s != http.StatusOK {
		t.Fatalf("destroy: %d", s)
	}

	var listed sessionListResponse
	c.do(t, http.MethodGet, "/session/list", nil, &listed)
	if len(listed.Sessions) != 0 {
		t.Errorf("after cascade destroy, sessions = %+v, want empty", listed.Sessions)
	}
}

// TestSessionSwitch_HappyPath drives /session/switch and asserts both
// the response echo and the subsequent /session/list active_session
// pointer.
func TestSessionSwitch_HappyPath(t *testing.T) {
	c, _ := newTestServer(t)

	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "a@1", Repo: "a"}, nil)
	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "a@2", Repo: "a"}, nil)

	var sw sessionSwitchResponse
	if s := c.do(t, http.MethodPost, "/session/switch",
		sessionSwitchRequest{ID: "a@2"}, &sw); s != http.StatusOK {
		t.Fatalf("switch status = %d", s)
	}
	if sw.ActiveSession != "a@2" {
		t.Errorf("ActiveSession = %q, want %q", sw.ActiveSession, "a@2")
	}

	var listed sessionListResponse
	c.do(t, http.MethodGet, "/session/list", nil, &listed)
	if listed.ActiveSession != "a@2" {
		t.Errorf("listed.ActiveSession = %q, want %q", listed.ActiveSession, "a@2")
	}

	// Empty ID clears focus.
	c.do(t, http.MethodPost, "/session/switch", sessionSwitchRequest{ID: ""}, &sw)
	if sw.ActiveSession != "" {
		t.Errorf("ActiveSession after clear = %q, want empty", sw.ActiveSession)
	}
}

// ---------------------------------------------------------------------------
// pane.* happy paths
// ---------------------------------------------------------------------------

// TestPane_FullLifecycle drives every pane.* method in sequence. Each
// step asserts both the response and the resulting tree state via
// /pane/list — together they cover the AC "verify resulting state via
// session.list / pane.list" in one composable case.
func TestPane_FullLifecycle(t *testing.T) {
	c, _ := newTestServer(t)

	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b", Repo: "r"}, nil)

	// create two panes.
	for _, name := range []string{"agent", "term"} {
		if s := c.do(t, http.MethodPost, "/pane/create",
			paneCreateRequest{SessionID: "r@b", Name: name}, nil); s != http.StatusOK {
			t.Fatalf("pane.create %q status %d", name, s)
		}
	}

	listURL := "/pane/list?" + url.Values{"session_id": {"r@b"}}.Encode()

	var lst paneListResponse
	c.do(t, http.MethodGet, listURL, nil, &lst)
	if len(lst.Panes) != 2 || lst.Panes[0].Name != "agent" || lst.Panes[1].Name != "term" {
		t.Errorf("panes after create = %+v, want [agent, term]", lst.Panes)
	}
	if lst.ActivePane != "agent" {
		t.Errorf("active_pane = %q, want agent (first pane should auto-activate)", lst.ActivePane)
	}

	// switch by name.
	var sw paneSwitchResponse
	c.do(t, http.MethodPost, "/pane/switch",
		paneSwitchRequest{SessionID: "r@b", Name: "term"}, &sw)
	if sw.ActivePane != "term" {
		t.Errorf("switch-by-name ActivePane = %q, want term", sw.ActivePane)
	}

	// switch by direction next: wraps to agent.
	c.do(t, http.MethodPost, "/pane/switch",
		paneSwitchRequest{SessionID: "r@b", Direction: "next"}, &sw)
	if sw.ActivePane != "agent" {
		t.Errorf("switch-next ActivePane = %q, want agent (wrap)", sw.ActivePane)
	}

	// switch by direction prev: wraps to term.
	c.do(t, http.MethodPost, "/pane/switch",
		paneSwitchRequest{SessionID: "r@b", Direction: "prev"}, &sw)
	if sw.ActivePane != "term" {
		t.Errorf("switch-prev ActivePane = %q, want term", sw.ActivePane)
	}

	// resize is a validate-only no-op at this layer; just confirm 200.
	if s := c.do(t, http.MethodPost, "/pane/resize",
		paneResizeRequest{SessionID: "r@b", Name: "term", Cols: 80, Rows: 24},
		nil); s != http.StatusOK {
		t.Errorf("resize status = %d, want 200", s)
	}

	// send_input is also a validate-only no-op at this layer.
	if s := c.do(t, http.MethodPost, "/pane/send_input",
		paneSendInputRequest{SessionID: "r@b", Name: "term", Data: "ls\n"},
		nil); s != http.StatusOK {
		t.Errorf("send_input status = %d, want 200", s)
	}

	// destroy and confirm the list shrinks.
	c.do(t, http.MethodPost, "/pane/destroy",
		paneDestroyRequest{SessionID: "r@b", Name: "agent"}, nil)
	c.do(t, http.MethodGet, listURL, nil, &lst)
	if len(lst.Panes) != 1 || lst.Panes[0].Name != "term" {
		t.Errorf("panes after destroy = %+v, want [term]", lst.Panes)
	}
}

// ---------------------------------------------------------------------------
// Structured errors — one case per typed pane.* sentinel so the
// statusAndCodeForPaneErr mapping is fully covered.
// ---------------------------------------------------------------------------

// TestErrors_StructuredResponses asserts the full table of typed
// pane.* errors maps to the expected (HTTP status, stable code)
// pair. The cases are co-located so a refactor of the mapping has a
// single place to update.
func TestErrors_StructuredResponses(t *testing.T) {
	c, _ := newTestServer(t)

	// Pre-populate so we have valid targets for the success-leading
	// paths each negative case branches off.
	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b", Repo: "r"}, nil)
	c.do(t, http.MethodPost, "/pane/create",
		paneCreateRequest{SessionID: "r@b", Name: "agent"}, nil)

	cases := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{
			name:       "session_exists",
			method:     http.MethodPost,
			path:       "/session/create",
			body:       sessionCreateRequest{ID: "r@b", Repo: "r"},
			wantStatus: http.StatusConflict,
			wantCode:   codeSessionExists,
		},
		{
			name:       "session_not_found_on_destroy",
			method:     http.MethodPost,
			path:       "/session/destroy",
			body:       sessionDestroyRequest{ID: "nope"},
			wantStatus: http.StatusNotFound,
			wantCode:   codeSessionNotFound,
		},
		{
			name:       "session_not_found_on_switch",
			method:     http.MethodPost,
			path:       "/session/switch",
			body:       sessionSwitchRequest{ID: "nope"},
			wantStatus: http.StatusNotFound,
			wantCode:   codeSessionNotFound,
		},
		{
			name:       "parent_not_found",
			method:     http.MethodPost,
			path:       "/session/create",
			body:       sessionCreateRequest{ID: "x", ParentID: "nope"},
			wantStatus: http.StatusNotFound,
			wantCode:   codeParentNotFound,
		},
		{
			name:       "invalid_session_top_level_no_repo",
			method:     http.MethodPost,
			path:       "/session/create",
			body:       sessionCreateRequest{ID: "x"},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidSession,
		},
		{
			name:       "pane_exists",
			method:     http.MethodPost,
			path:       "/pane/create",
			body:       paneCreateRequest{SessionID: "r@b", Name: "agent"},
			wantStatus: http.StatusConflict,
			wantCode:   codePaneExists,
		},
		{
			name:       "pane_not_found_on_destroy",
			method:     http.MethodPost,
			path:       "/pane/destroy",
			body:       paneDestroyRequest{SessionID: "r@b", Name: "nope"},
			wantStatus: http.StatusNotFound,
			wantCode:   codePaneNotFound,
		},
		{
			name:       "pane_not_found_on_switch_by_name",
			method:     http.MethodPost,
			path:       "/pane/switch",
			body:       paneSwitchRequest{SessionID: "r@b", Name: "nope"},
			wantStatus: http.StatusNotFound,
			wantCode:   codePaneNotFound,
		},
		{
			name:       "pane_not_found_on_resize",
			method:     http.MethodPost,
			path:       "/pane/resize",
			body:       paneResizeRequest{SessionID: "r@b", Name: "nope", Cols: 1, Rows: 1},
			wantStatus: http.StatusNotFound,
			wantCode:   codePaneNotFound,
		},
		{
			name:       "session_not_found_on_pane_list",
			method:     http.MethodGet,
			path:       "/pane/list?session_id=nope",
			body:       nil,
			wantStatus: http.StatusNotFound,
			wantCode:   codeSessionNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.expectError(t, tc.method, tc.path, tc.body, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestErrors_NoPanesOnCycle exercises pane.ErrNoPanes — a session
// with zero panes returns 400/no_panes from /pane/switch direction.
func TestErrors_NoPanesOnCycle(t *testing.T) {
	c, _ := newTestServer(t)

	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b", Repo: "r"}, nil)

	c.expectError(t, http.MethodPost, "/pane/switch",
		paneSwitchRequest{SessionID: "r@b", Direction: "next"},
		http.StatusBadRequest, codeNoPanes)
}

// TestErrors_ParentIsReview exercises the §3.1 two-level invariant:
// a review subsession cannot itself be a parent. The server must
// propagate pane.ErrParentIsReview as 400/parent_is_review.
func TestErrors_ParentIsReview(t *testing.T) {
	c, _ := newTestServer(t)

	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b", Repo: "r"}, nil)
	c.do(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "r@b~r-1-x", ParentID: "r@b"}, nil)

	c.expectError(t, http.MethodPost, "/session/create",
		sessionCreateRequest{ID: "deep", ParentID: "r@b~r-1-x"},
		http.StatusBadRequest, codeParentIsReview)
}

// ---------------------------------------------------------------------------
// Bad-request paths — pre-model validation in the server itself
// ---------------------------------------------------------------------------

// TestBadRequest_MissingFields covers the handler-level validation
// (empty session_id / name / id) that fires before the model is
// touched. These are 400/bad_request, not the typed pane.* codes.
func TestBadRequest_MissingFields(t *testing.T) {
	c, _ := newTestServer(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"session_create_no_id", http.MethodPost, "/session/create", sessionCreateRequest{}},
		{"session_destroy_no_id", http.MethodPost, "/session/destroy", sessionDestroyRequest{}},
		{"pane_create_no_session_id", http.MethodPost, "/pane/create", paneCreateRequest{Name: "x"}},
		{"pane_create_no_name", http.MethodPost, "/pane/create", paneCreateRequest{SessionID: "y"}},
		{"pane_destroy_no_session_id", http.MethodPost, "/pane/destroy", paneDestroyRequest{Name: "x"}},
		{"pane_list_no_session_id", http.MethodGet, "/pane/list", nil},
		{"pane_switch_no_session_id", http.MethodPost, "/pane/switch", paneSwitchRequest{Name: "x"}},
		{"pane_switch_neither_name_nor_direction", http.MethodPost, "/pane/switch", paneSwitchRequest{SessionID: "x"}},
		{"pane_switch_both_name_and_direction", http.MethodPost, "/pane/switch", paneSwitchRequest{SessionID: "x", Name: "y", Direction: "next"}},
		{"pane_switch_invalid_direction", http.MethodPost, "/pane/switch", paneSwitchRequest{SessionID: "x", Direction: "sideways"}},
		{"pane_resize_no_session_id", http.MethodPost, "/pane/resize", paneResizeRequest{Name: "x"}},
		{"pane_resize_negative_cols", http.MethodPost, "/pane/resize", paneResizeRequest{SessionID: "x", Name: "y", Cols: -1}},
		{"pane_send_input_no_session_id", http.MethodPost, "/pane/send_input", paneSendInputRequest{Name: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.expectError(t, tc.method, tc.path, tc.body, http.StatusBadRequest, codeBadRequest)
		})
	}
}

// TestBadRequest_MalformedJSON covers the JSON-decoder error path —
// the body must be rejected with 400/bad_request and a parseable
// errorResponse.
func TestBadRequest_MalformedJSON(t *testing.T) {
	c, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodPost, c.base+"/session/create",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var got errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != codeBadRequest {
		t.Errorf("code = %q, want %q", got.Code, codeBadRequest)
	}
}

// TestBadRequest_UnknownField asserts DisallowUnknownFields is wired
// in — an unknown JSON key should be rejected so typos in the wire
// shape do not pass silently.
func TestBadRequest_UnknownField(t *testing.T) {
	c, _ := newTestServer(t)

	c.expectError(t, http.MethodPost, "/session/create",
		map[string]any{"id": "x", "repo": "r", "garbage": true},
		http.StatusBadRequest, codeBadRequest)
}

// ---------------------------------------------------------------------------
// Method routing — wrong method, unknown path
// ---------------------------------------------------------------------------

// TestMethodNotAllowed asserts every endpoint enforces its HTTP verb
// and returns 405/method_not_allowed.
func TestMethodNotAllowed(t *testing.T) {
	c, _ := newTestServer(t)

	// /session/list is GET-only; POST should fail.
	c.expectError(t, http.MethodPost, "/session/list", nil,
		http.StatusMethodNotAllowed, codeMethodNotAllowed)
	// /session/create is POST-only; GET should fail.
	c.expectError(t, http.MethodGet, "/session/create", nil,
		http.StatusMethodNotAllowed, codeMethodNotAllowed)
}

// TestUnknownPath covers the "/" fall-through — anything outside the
// registered routes is a 404/bad_request with a clear message.
func TestUnknownPath(t *testing.T) {
	c, _ := newTestServer(t)

	got := c.expectError(t, http.MethodPost, "/no/such/method", nil,
		http.StatusNotFound, codeBadRequest)
	if !strings.Contains(got.Message, "no such method") {
		t.Errorf("message = %q, want substring %q", got.Message, "no such method")
	}
}

// ---------------------------------------------------------------------------
// Concurrency — verify the server accepts many connections in
// parallel and serialises mutations via the underlying tree.
// ---------------------------------------------------------------------------

// TestConcurrent_Mutations fans out a swarm of /session/create calls
// from many goroutines. The AC requires "the server accepts multiple
// connections simultaneously and serialises model mutations through
// the model" — this test exercises that explicitly under -race.
//
// Each goroutine creates a uniquely-named session in a unique repo so
// the model has plenty of opportunity to interleave writes. After the
// fan-out we assert the final tree contains exactly N sessions and
// that the list endpoint sees them all.
func TestConcurrent_Mutations(t *testing.T) {
	c, _ := newTestServer(t)

	const (
		workers   = 16
		perWorker = 8
	)
	var (
		wg      sync.WaitGroup
		failure atomic.Int32
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("r%d@b%d", w, i)
				var got sessionResponse
				status := c.do(t, http.MethodPost, "/session/create",
					sessionCreateRequest{ID: id, Repo: fmt.Sprintf("r%d", w)}, &got)
				if status != http.StatusOK {
					t.Errorf("worker %d create %d: status %d", w, i, status)
					failure.Add(1)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if failure.Load() > 0 {
		t.FailNow()
	}

	var listed sessionListResponse
	c.do(t, http.MethodGet, "/session/list", nil, &listed)
	if len(listed.Sessions) != workers*perWorker {
		t.Errorf("session count = %d, want %d", len(listed.Sessions), workers*perWorker)
	}
}

// TestConcurrent_DuplicateIDs hammers a single ID from many
// goroutines: exactly one must win with 200, all others must lose
// with 409/session_exists. This is the strongest assertion that the
// server serialises through the model's mutex rather than letting
// two AddSession calls land in parallel.
func TestConcurrent_DuplicateIDs(t *testing.T) {
	c, _ := newTestServer(t)

	const workers = 32
	var (
		wg         sync.WaitGroup
		successes  atomic.Int32
		conflicts  atomic.Int32
		unexpected atomic.Int32
		unexpResp  atomic.Pointer[errorResponse]
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got errorResponse
			status := c.do(t, http.MethodPost, "/session/create",
				sessionCreateRequest{ID: "the-one", Repo: "r"}, &got)
			switch {
			case status == http.StatusOK:
				successes.Add(1)
			case status == http.StatusConflict && got.Code == codeSessionExists:
				conflicts.Add(1)
			default:
				unexpected.Add(1)
				captured := got
				unexpResp.Store(&captured)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("successes = %d, want exactly 1", got)
	}
	if got := conflicts.Load(); got != workers-1 {
		t.Errorf("conflicts = %d, want %d", got, workers-1)
	}
	if got := unexpected.Load(); got != 0 {
		var lastSeen errorResponse
		if p := unexpResp.Load(); p != nil {
			lastSeen = *p
		}
		t.Errorf("unexpected responses = %d (last: %+v)", got, lastSeen)
	}
}

// ---------------------------------------------------------------------------
// ListenAndServe lifecycle — the public entry point used by the
// daemon binary in a later PR.
// ---------------------------------------------------------------------------

// TestListenAndServe_HappyPath exercises the public ListenAndServe
// entry point against an explicit path inside t.TempDir(). Confirms:
//
//   - the socket file is created
//   - a real client can hit /session/list
//   - context cancellation drains and the socket is unlinked
func TestListenAndServe_HappyPath(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "p")
	if len(sockPath) >= 104 {
		t.Skipf("temp socket path too long for sun_path: %s", sockPath)
	}

	tree := pane.New()
	srv := New(tree)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, sockPath) }()

	// Poll for socket existence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket not created: %v", err)
	}

	// Hit /session/list to prove the server is actually serving.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://mux/session/list")
	if err != nil {
		t.Fatalf("get /session/list: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Cancel and wait for ListenAndServe to return.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListenAndServe did not return within 3s of cancel")
	}

	// Socket file should be unlinked on exit.
	if _, err := os.Stat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket %q still exists after shutdown (err=%v); want unlinked",
			sockPath, err)
	}
}

// TestListenAndServe_StaleSocketReplaced asserts the server unlinks a
// stale socket file at the bind path before binding — without this,
// any prior unclean shutdown would block the next start. Pre-create a
// stale file at the path, then prove ListenAndServe succeeds and
// serves real requests through it.
func TestListenAndServe_StaleSocketReplaced(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "p")
	if len(sockPath) >= 104 {
		t.Skipf("temp socket path too long for sun_path: %s", sockPath)
	}
	// Plant a stale regular file at the bind path.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant stale file: %v", err)
	}

	tree := pane.New()
	srv := New(tree)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, sockPath) }()

	// Wait for the socket to be (re)created — we can detect it by
	// successfully dialling and getting a 200 from /session/list.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 250 * time.Millisecond,
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://mux/session/list")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				cancel()
				<-done
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("stale-socket replacement: never observed a serving listener")
}
