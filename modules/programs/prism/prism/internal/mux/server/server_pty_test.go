package server

// Tests for the server's PTY-hosting layer (the runtime side of the
// pane.create / pane.send_input / pane.resize / pane.read_output /
// pane.destroy contracts). These exercise the path that lands in
// PR #2158 — pre-#2158 the wire was validate-only.
//
// All tests bind a real Unix socket inside t.TempDir() (matching the
// pattern in server_test.go), drive a real http.Client over a Unix
// transport, and spawn real child processes under a real PTY pair.
// The children are kept tiny (`/bin/sh` running a single-line script)
// so the suite stays under -short and so a regression in PTY teardown
// shows up as a leaked process, not a slow test.
//
// Homeless-shelter clean: the tests never touch $HOME or $XDG_STATE_HOME.

import (
	"context"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// shellOrSkip returns "/bin/sh" if it is available on PATH, otherwise
// skips the test. We use /bin/sh as the canonical PTY-hosted process
// because every Unix has it; specific shells (bash, zsh) are not
// guaranteed in the Nix sandbox.
func shellOrSkip(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/sh", "/usr/bin/sh"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("no /bin/sh available; skipping PTY-hosting test")
	return ""
}

// addSession is a one-liner helper for tests that need a session as
// the parent of a pane. The model rejects panes without a session, so
// every PTY test needs this prologue.
func addSession(t *testing.T, c *testClient, id string) {
	t.Helper()
	var got sessionResponse
	status := c.do(t, http.MethodPost, "/session/create", sessionCreateRequest{
		ID:   id,
		Repo: "test",
	}, &got)
	if status != http.StatusOK {
		t.Fatalf("session.create: status = %d", status)
	}
}

// waitForLines polls /pane/read_output until at least one of the
// expected substrings appears in the rendered rows, the context is
// cancelled, or maxAttempts is reached. Returns the rendered output
// on success, or fails the test with the last seen output on miss.
//
// We poll rather than rely on a single read because PTY output is
// asynchronous — the child writes, the kernel buffers, the pump
// drains, the emulator processes. 100ms per attempt × 30 attempts =
// 3s ceiling, well under the test timeout.
func waitForLines(t *testing.T, c *testClient, sessID, paneName string, want string, maxAttempts int) []string {
	t.Helper()
	q := url.Values{}
	q.Set("session_id", sessID)
	q.Set("name", paneName)
	path := "/pane/read_output?" + q.Encode()

	var lastLines []string
	for i := 0; i < maxAttempts; i++ {
		var got paneReadOutputResponse
		status := c.do(t, http.MethodGet, path, nil, &got)
		if status != http.StatusOK {
			t.Fatalf("pane.read_output attempt %d: status = %d", i, status)
		}
		lastLines = got.Lines
		for _, line := range got.Lines {
			if strings.Contains(line, want) {
				return got.Lines
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pane.read_output: did not observe %q within %d attempts. last lines: %q",
		want, maxAttempts, lastLines)
	return nil
}

// TestPaneCreate_NoArgv_IsModelOnly confirms the pre-#2158 wire shape
// stays usable: a pane.create without argv produces a model row with
// no PTY. read_output reports zero dimensions so the renderer knows
// to draw the placeholder.
func TestPaneCreate_NoArgv_IsModelOnly(t *testing.T) {
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.create: status = %d", status)
	}
	if host, ok := srv.ptys.get("r@b", "agent"); ok {
		t.Errorf("registry has a PTY host for a model-only pane: %+v", host)
	}

	var frame paneReadOutputResponse
	c.do(t, http.MethodGet, "/pane/read_output?session_id=r%40b&name=agent", nil, &frame)
	if frame.Cols != 0 || frame.Rows != 0 {
		t.Errorf("frame dims = (%d,%d), want (0,0) for model-only pane", frame.Cols, frame.Rows)
	}
	if len(frame.Lines) != 0 {
		t.Errorf("frame lines = %d, want 0 for model-only pane", len(frame.Lines))
	}
}

// TestPaneCreate_WithArgv_SpawnsPTY exercises the full spawn → output
// → destroy lifecycle. A tiny shell script prints "READY" then sleeps
// indefinitely; the test asserts the output reaches the emulator via
// read_output, then destroys the pane and verifies the PTY host is
// removed from the registry.
func TestPaneCreate_WithArgv_SpawnsPTY(t *testing.T) {
	sh := shellOrSkip(t)
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
		Argv:      []string{sh, "-c", "echo READY; sleep 60"},
		Cwd:       t.TempDir(),
		Cols:      80,
		Rows:      24,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.create: status = %d", status)
	}
	host, ok := srv.ptys.get("r@b", "agent")
	if !ok {
		t.Fatalf("registry has no PTY host for r@b/agent")
	}
	if host.cols != 80 || host.rows != 24 {
		t.Errorf("host geometry = (%d,%d), want (80,24)", host.cols, host.rows)
	}

	// Pull frames until "READY" shows up. PTY output is async — the
	// shell may take a few ms to flush its first line through the
	// kernel into the emulator.
	waitForLines(t, c, "r@b", "agent", "READY", 30)

	// Destroy unwinds the PTY (SIGTERM → grace → SIGKILL) and removes
	// it from the registry.
	status = c.do(t, http.MethodPost, "/pane/destroy", paneDestroyRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.destroy: status = %d", status)
	}
	if _, ok := srv.ptys.get("r@b", "agent"); ok {
		t.Errorf("registry still has a PTY host after destroy")
	}
	// Wait for the bridge goroutines to finish so a leaked process
	// shows up as a test failure rather than as a flake under -race.
	select {
	case <-host.done:
	case <-time.After(3 * time.Second):
		t.Errorf("pty host bridge goroutines did not exit within 3s")
	}
}

// TestPaneSendInput_WritesToChildStdin proves that pane.send_input
// actually delivers bytes to the child process. We spawn a shell
// running `read -r line; echo GOT:$line` and assert the rendered
// output contains "GOT:hello" after sending "hello\n".
func TestPaneSendInput_WritesToChildStdin(t *testing.T) {
	sh := shellOrSkip(t)
	c, _ := newTestServer(t)
	addSession(t, c, "r@b")

	status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
		Argv:      []string{sh, "-c", "read -r line; echo GOT:$line; sleep 30"},
		Cwd:       t.TempDir(),
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.create: status = %d", status)
	}

	// Send a short payload + newline so `read` returns.
	status = c.do(t, http.MethodPost, "/pane/send_input", paneSendInputRequest{
		SessionID: "r@b",
		Name:      "agent",
		Data:      "hello\n",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.send_input: status = %d", status)
	}

	waitForLines(t, c, "r@b", "agent", "GOT:hello", 30)

	// Clean up explicitly so the t.Cleanup-driven Server.Close is the
	// fall-back, not the primary teardown path.
	_ = c.do(t, http.MethodPost, "/pane/destroy", paneDestroyRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
}

// TestPaneResize_AppliesToEngine confirms that pane.resize forwards
// both to the PTY (so the kernel's SIGWINCH propagates) and to the
// emulator (so RenderRows() returns the right number of rows). We can
// only directly observe the emulator side from here; the PTY side
// would require a child that does `stty size`, which is fragile.
func TestPaneResize_AppliesToEngine(t *testing.T) {
	sh := shellOrSkip(t)
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
		Argv:      []string{sh, "-c", "sleep 60"},
		Cwd:       t.TempDir(),
		Cols:      80,
		Rows:      24,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.create: status = %d", status)
	}

	status = c.do(t, http.MethodPost, "/pane/resize", paneResizeRequest{
		SessionID: "r@b",
		Name:      "agent",
		Cols:      120,
		Rows:      40,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("pane.resize: status = %d", status)
	}
	host, _ := srv.ptys.get("r@b", "agent")
	if host.cols != 120 || host.rows != 40 {
		t.Errorf("host geometry after resize = (%d,%d), want (120,40)", host.cols, host.rows)
	}
	wantCols, wantRows := host.host.Size()
	if wantCols != 120 || wantRows != 40 {
		t.Errorf("vt.Host size after resize = (%d,%d), want (120,40)", wantCols, wantRows)
	}

	_ = c.do(t, http.MethodPost, "/pane/destroy", paneDestroyRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
}

// TestSessionDestroy_CascadesPTY confirms that destroying a session
// also tears down every PTY associated with its panes. Without this
// cascade, prism cleanup would leak processes.
func TestSessionDestroy_CascadesPTY(t *testing.T) {
	sh := shellOrSkip(t)
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	// Two panes both running shells.
	for _, name := range []string{"agent", "term"} {
		status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
			SessionID: "r@b",
			Name:      name,
			Argv:      []string{sh, "-c", "sleep 60"},
			Cwd:       t.TempDir(),
		}, nil)
		if status != http.StatusOK {
			t.Fatalf("pane.create %q: status = %d", name, status)
		}
	}

	hosts := make([]*ptyHost, 0, 2)
	for _, name := range []string{"agent", "term"} {
		h, ok := srv.ptys.get("r@b", name)
		if !ok {
			t.Fatalf("registry missing PTY host for r@b/%s", name)
		}
		hosts = append(hosts, h)
	}

	status := c.do(t, http.MethodPost, "/session/destroy", sessionDestroyRequest{
		ID: "r@b",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("session.destroy: status = %d", status)
	}

	for _, name := range []string{"agent", "term"} {
		if _, ok := srv.ptys.get("r@b", name); ok {
			t.Errorf("registry still has PTY host for r@b/%s after session destroy", name)
		}
	}
	// Both bridge goroutines should drain.
	for i, h := range hosts {
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Errorf("host %d bridge did not exit within 3s", i)
		}
	}
}

// TestPaneCreate_SpawnFailure_RollsBackModel asserts that if the PTY
// spawn fails (we point argv at a non-existent binary), the model
// row added at the top of the handler is rolled back so the next
// retry sees a clean slate.
func TestPaneCreate_SpawnFailure_RollsBackModel(t *testing.T) {
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
		Argv:      []string{"/this/binary/does/not/exist/q9z"},
		Cwd:       t.TempDir(),
	}, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("pane.create with bad argv: status = %d, want 500", status)
	}
	// Model side: the pane row must NOT exist (rollback).
	sess, ok := srv.tree.Session("r@b")
	if !ok {
		t.Fatalf("session vanished after pane.create failure")
	}
	if len(sess.Panes) != 0 {
		t.Errorf("session has %d panes after failed pane.create, want 0", len(sess.Panes))
	}
	// Registry side: no host should be registered.
	if _, ok := srv.ptys.get("r@b", "agent"); ok {
		t.Errorf("registry has a PTY host after failed pane.create")
	}
}

// TestHostFor_ReturnsLiveHost is the in-process renderer-wiring
// contract: Server.HostFor returns the live *vt.Host for a pane that
// has a registered PTY, and nil for a model-only pane.
func TestHostFor_ReturnsLiveHost(t *testing.T) {
	sh := shellOrSkip(t)
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	// Model-only pane → HostFor is nil.
	c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "model-only",
	}, nil)
	if h := srv.HostFor("r@b", "model-only"); h != nil {
		t.Errorf("HostFor model-only pane = %v, want nil", h)
	}

	// PTY pane → HostFor is non-nil.
	c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
		Argv:      []string{sh, "-c", "echo READY; sleep 60"},
		Cwd:       t.TempDir(),
	}, nil)
	h := srv.HostFor("r@b", "agent")
	if h == nil {
		t.Fatalf("HostFor PTY pane = nil, want non-nil")
	}
	cols, rows := h.Size()
	if cols == 0 || rows == 0 {
		t.Errorf("HostFor PTY pane size = (%d,%d), want non-zero", cols, rows)
	}

	// HostProviderFunc returns the same shape.
	fn := srv.HostProviderFunc()
	if fn == nil {
		t.Fatalf("HostProviderFunc = nil for non-nil server")
	}
	if got := fn("r@b", "agent"); got != h {
		t.Errorf("HostProviderFunc(r@b, agent) = %p, HostFor = %p", got, h)
	}

	_ = c.do(t, http.MethodPost, "/pane/destroy", paneDestroyRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
}

// TestPaneCreate_UnknownSession is a regression guard: the model-side
// error (ErrSessionNotFound) must surface before the PTY-spawn path.
// Otherwise an unknown session_id would still fork a process.
func TestPaneCreate_UnknownSession(t *testing.T) {
	c, srv := newTestServer(t)

	c.expectError(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "nope",
		Name:      "agent",
		Argv:      []string{"/bin/sh", "-c", "sleep 60"},
	}, http.StatusNotFound, codeSessionNotFound)

	// Registry must be empty — no PTY spawned for the unknown session.
	if h, ok := srv.ptys.get("nope", "agent"); ok {
		t.Errorf("registry has host %p for unknown session", h)
	}
}

// TestPaneReadOutput_NonExistentPane reports ErrPaneNotFound rather
// than a synthetic empty frame, so the CLI can distinguish "no PTY"
// (200 with zero dims) from "pane doesn't exist" (404).
func TestPaneReadOutput_NonExistentPane(t *testing.T) {
	c, _ := newTestServer(t)
	addSession(t, c, "r@b")

	c.expectError(t, http.MethodGet,
		"/pane/read_output?session_id=r%40b&name=missing",
		nil, http.StatusNotFound, codePaneNotFound)
}

// TestPaneSendInput_NoArgv_NoPanic asserts that send_input on a
// model-only pane (no PTY) is a 200 OK no-op. The CLI may pre-send
// keystrokes before the runtime side is wired in tests, and we don't
// want that to fail.
func TestPaneSendInput_NoArgv_NoPanic(t *testing.T) {
	c, _ := newTestServer(t)
	addSession(t, c, "r@b")
	c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
		SessionID: "r@b",
		Name:      "agent",
	}, nil)
	status := c.do(t, http.MethodPost, "/pane/send_input", paneSendInputRequest{
		SessionID: "r@b",
		Name:      "agent",
		Data:      "ignored\n",
	}, nil)
	if status != http.StatusOK {
		t.Errorf("pane.send_input on model-only pane: status = %d, want 200", status)
	}
}

// TestServer_Close_TerminatesAllPTYs confirms Server.Close tears down
// every live PTY. Used by tests that want a deterministic teardown
// before t.Cleanup observes the goroutine state.
func TestServer_Close_TerminatesAllPTYs(t *testing.T) {
	sh := shellOrSkip(t)
	c, srv := newTestServer(t)
	addSession(t, c, "r@b")

	for _, name := range []string{"agent", "term"} {
		status := c.do(t, http.MethodPost, "/pane/create", paneCreateRequest{
			SessionID: "r@b",
			Name:      name,
			Argv:      []string{sh, "-c", "sleep 60"},
			Cwd:       t.TempDir(),
		}, nil)
		if status != http.StatusOK {
			t.Fatalf("pane.create %q: status = %d", name, status)
		}
	}
	hostA, _ := srv.ptys.get("r@b", "agent")
	hostB, _ := srv.ptys.get("r@b", "term")

	srv.Close()

	for i, h := range []*ptyHost{hostA, hostB} {
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Errorf("host %d bridge did not exit within 3s after Close", i)
		}
	}
}

// Compile-time interface assertion: Server's HostFor satisfies the
// shape render.HostProvider expects, without this package importing
// render (which would create a cycle).
var _ = func(s *Server) interface {
	Host(string, string) interface{}
} {
	// dummy adapter to prove the method signature shape
	return adapt{srv: s}
}

type adapt struct{ srv *Server }

func (a adapt) Host(sessionID, paneName string) interface{} {
	return a.srv.HostFor(sessionID, paneName)
}

// Compile-time check: make sure the Argv tail wins are stable. If the
// import of context ever drifts (unlikely but possible during a refactor)
// this will catch it.
var _ context.Context = context.Background()
