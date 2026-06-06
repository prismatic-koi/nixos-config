package client

// Tests for the PR #2158 additions to the client surface:
//
//   - Panes().Create now takes a PaneCreateOptions struct with Argv,
//     Cwd, Env, Cols, Rows.
//   - Panes().ReadOutput is a new GET-shape method that returns a
//     PaneFrame snapshot of the rendered cell grid.
//
// The tests use a stub http.RoundTripper so they do not depend on a
// live server, keeping them homeless-shelter clean (no XDG_STATE_HOME,
// no PTY syscalls, no children).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// fakeRoundTripper records the most recent request and serves a
// scripted response. Distinct from any helper in client_test.go so
// these tests stand on their own.
type fakeRoundTripper struct {
	lastReq    *http.Request
	lastBody   []byte
	respStatus int
	respBody   []byte
}

func (f *fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	f.lastReq = r
	if r.Body != nil {
		buf, _ := io.ReadAll(r.Body)
		f.lastBody = buf
	}
	status := f.respStatus
	if status == 0 {
		status = http.StatusOK
	}
	body := f.respBody
	if body == nil {
		body = []byte("{}")
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// newFakeClient constructs a Client using the supplied round-tripper
// in place of the default Unix-dialing transport. The socket path is
// fixed because the round-tripper short-circuits the dial.
func newFakeClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := New(WithSocketPath("/tmp/notused"), WithTransport(rt))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestPanesCreate_NoArgv_OmitsRuntimeFields asserts that a zero-value
// PaneCreateOptions produces the legacy wire shape — no argv, cwd, or
// env. The server interprets this as "model-only pane, no PTY".
func TestPanesCreate_NoArgv_OmitsRuntimeFields(t *testing.T) {
	rt := &fakeRoundTripper{respStatus: http.StatusOK, respBody: []byte("{}")}
	c := newFakeClient(t, rt)

	if err := c.Panes().Create(context.Background(), "r@b", "agent", PaneCreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rt.lastReq.URL.Path != "/pane/create" {
		t.Errorf("path = %q, want /pane/create", rt.lastReq.URL.Path)
	}
	if rt.lastReq.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", rt.lastReq.Method)
	}
	var body map[string]any
	if err := json.Unmarshal(rt.lastBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"argv", "cwd", "env", "cols", "rows"} {
		if _, ok := body[key]; ok {
			t.Errorf("body[%q] is present for zero-value opts: %v", key, body[key])
		}
	}
}

// TestPanesCreate_WithRuntimeFields_PopulatesWire confirms that the
// runtime fields (argv, cwd, env, cols, rows) round-trip into the
// JSON body the way the server expects.
func TestPanesCreate_WithRuntimeFields_PopulatesWire(t *testing.T) {
	rt := &fakeRoundTripper{respStatus: http.StatusOK, respBody: []byte("{}")}
	c := newFakeClient(t, rt)

	opts := PaneCreateOptions{
		Argv: []string{"/bin/sh", "-c", "echo hi"},
		Cwd:  "/var/tmp",
		Env:  map[string]string{"FOO": "bar"},
		Cols: 120,
		Rows: 40,
	}
	if err := c.Panes().Create(context.Background(), "r@b", "agent", opts); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(rt.lastBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	argv, ok := body["argv"].([]any)
	if !ok || len(argv) != 3 || argv[0] != "/bin/sh" {
		t.Errorf("argv = %v, want [/bin/sh -c echo hi]", body["argv"])
	}
	if body["cwd"] != "/var/tmp" {
		t.Errorf("cwd = %v, want /var/tmp", body["cwd"])
	}
	if env, ok := body["env"].(map[string]any); !ok || env["FOO"] != "bar" {
		t.Errorf("env = %v, want {FOO:bar}", body["env"])
	}
	if body["cols"] != float64(120) {
		t.Errorf("cols = %v, want 120", body["cols"])
	}
	if body["rows"] != float64(40) {
		t.Errorf("rows = %v, want 40", body["rows"])
	}
}

// TestPanesReadOutput_HappyPath decodes a representative server
// response into a PaneFrame. The wire shape is pinned here so a
// rename / retype on the server side fails this test.
func TestPanesReadOutput_HappyPath(t *testing.T) {
	respBody, _ := json.Marshal(map[string]any{
		"cols":       80,
		"rows":       24,
		"cursor_x":   5,
		"cursor_y":   3,
		"alt_screen": true,
		"lines":      []string{"hello", "world"},
	})
	rt := &fakeRoundTripper{respStatus: http.StatusOK, respBody: respBody}
	c := newFakeClient(t, rt)

	frame, err := c.Panes().ReadOutput(context.Background(), "r@b", "agent")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if frame.Cols != 80 || frame.Rows != 24 {
		t.Errorf("frame dims = (%d,%d), want (80,24)", frame.Cols, frame.Rows)
	}
	if frame.CursorX != 5 || frame.CursorY != 3 {
		t.Errorf("cursor = (%d,%d), want (5,3)", frame.CursorX, frame.CursorY)
	}
	if !frame.AltScreen {
		t.Errorf("alt screen = false, want true")
	}
	if len(frame.Lines) != 2 || frame.Lines[0] != "hello" || frame.Lines[1] != "world" {
		t.Errorf("lines = %v, want [hello world]", frame.Lines)
	}

	// Verify the request shape — GET with the right query string.
	if rt.lastReq.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", rt.lastReq.Method)
	}
	if rt.lastReq.URL.Path != "/pane/read_output" {
		t.Errorf("path = %q, want /pane/read_output", rt.lastReq.URL.Path)
	}
	q, err := url.ParseQuery(rt.lastReq.URL.RawQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if q.Get("session_id") != "r@b" || q.Get("name") != "agent" {
		t.Errorf("query = %v, want session_id=r@b name=agent", q)
	}
}

// TestPanesReadOutput_NilLines_PromotedToEmptySlice mirrors the
// SessionAPI.List defensive normalisation — a server that ever
// returns `"lines": null` (older build, future bug) should not force
// the caller to nil-check.
func TestPanesReadOutput_NilLines_PromotedToEmptySlice(t *testing.T) {
	respBody := []byte(`{"cols": 0, "rows": 0, "lines": null}`)
	rt := &fakeRoundTripper{respStatus: http.StatusOK, respBody: respBody}
	c := newFakeClient(t, rt)

	frame, err := c.Panes().ReadOutput(context.Background(), "r@b", "agent")
	if err != nil {
		t.Fatalf("ReadOutput: %v", err)
	}
	if frame.Lines == nil {
		t.Errorf("Lines is nil; want empty slice")
	}
	if len(frame.Lines) != 0 {
		t.Errorf("Lines has %d entries; want 0", len(frame.Lines))
	}
}

// TestPanesReadOutput_ServerError surfaces a structured *ClientError
// when the server returns 404/pane_not_found. The caller can branch
// via errors.Is(err, ErrPaneNotFound).
func TestPanesReadOutput_ServerError(t *testing.T) {
	respBody, _ := json.Marshal(map[string]any{
		"code":    CodePaneNotFound,
		"message": "session \"r@b\" has no pane \"missing\"",
	})
	rt := &fakeRoundTripper{respStatus: http.StatusNotFound, respBody: respBody}
	c := newFakeClient(t, rt)

	_, err := c.Panes().ReadOutput(context.Background(), "r@b", "missing")
	if err == nil {
		t.Fatalf("ReadOutput: want error, got nil")
	}
	if !strings.Contains(err.Error(), CodePaneNotFound) {
		t.Errorf("error = %q, want it to mention %q", err.Error(), CodePaneNotFound)
	}
}
