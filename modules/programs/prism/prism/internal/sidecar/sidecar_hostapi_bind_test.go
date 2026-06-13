package sidecar

// Tests for the host-API socket bind path (#1050) and lifetime decoupling from
// the harness-pipe handshake (#1486).
//
// Concerns covered:
//
//   - AC-3 (#1050): worst-case session name must not produce EINVAL.
//   - AC-4 (#1050): bind failure must surface as a fatal Run() error.
//   - AC-1 (#1486): host-API keeps serving after harness-pipe failure.
//   - AC-2 (#1486): harness-pipe failure logs error, does NOT call Shutdown().
//   - AC-3 (#1486): Shutdown removes socket; subsequent connect returns ENOENT.
//   - AC-4 (#1486): stale socket tombstone → ECONNREFUSED → clearer diagnostic.
//   - AC-8 (#1486): no goroutine leak after harness-pipe failure + Shutdown.
//   - AC-9 (#1486): socket permissions unchanged (0o700 on parent dir).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// ── helpers (AC-1 / #1486) ───────────────────────────────────────────────────

// newPI1486Sidecar creates a Sidecar with both a host-API socket and a
// harness-pipe socket, suitable for #1486 lifetime-decoupling tests.
// StartupConnectTimeout is set short so tests that simulate a handshake failure
// do not wait the full 5 m.
func newPI1486Sidecar(t *testing.T, hostAPISockPath, pipeSockPath string) *Sidecar {
	t.Helper()
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@main",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   pipeSockPath,
		HostAPISockPath:       hostAPISockPath,
		StartupConnectTimeout: 200 * time.Millisecond, // short for tests
		PipeReconnectTimeout:  50 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	return New(cfg)
}

// dialHostAPIHTTP dials hostAPISockPath as a Unix socket and sends a GET
// request to the host-API server, returning the HTTP status code and body.
func dialHostAPIHTTP(t *testing.T, hostAPISockPath, endpoint string) (int, string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", hostAPISockPath)
			},
		},
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get("http://prism-hostapi" + endpoint)
	if err != nil {
		t.Fatalf("dialHostAPIHTTP GET %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// waitForHostAPISock polls until the host-API socket file appears.
func waitForHostAPISock(t *testing.T, sockPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("host-API socket never appeared at %s", sockPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForHostAPIReady polls until a GET /list-sessions to the host-API socket
// returns 200.
func waitForHostAPIReady(t *testing.T, sockPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
			Timeout: 500 * time.Millisecond,
		}
		resp, err := client.Get("http://prism-hostapi/list-sessions")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("host-API socket at %s never became ready", sockPath)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestHostAPIBind_WorstCaseSessionName_DoesNotEINVAL is the AC-3 regression
// test. It exercises the actual net.Listen("unix", ...) call against a path
// derived from the worst-case session name from the issue body (an 80-char
// branch + ~review-99-review-context suffix) and asserts that the bind
// succeeds — i.e. the new short-hash directory scheme produces a bindable
// path even for pathological inputs. The path-length budget itself is
// asserted independently in
// internal/session.TestSidecarHostAPIPath_LengthInvariant_*.
//
// The bind is performed under a freshly-created temp directory rooted at /tmp
// (kept deliberately short so the per-test path itself stays inside the
// kernel limit on every CI runner). This means the test exercises the
// SessionDirName-derived suffix shape, which is the part this fix changes —
// the long input session name no longer pollutes the on-disk path.
func TestHostAPIBind_WorstCaseSessionName_DoesNotEINVAL(t *testing.T) {
	// Mirror the worst-case shape called out by AC-1 / AC-3.
	worstCase := "nixos-config@" + strings.Repeat("x", 80) + "~review-99-review-context"

	// Build the path under a short, controlled XDG_STATE_HOME so the test
	// does not depend on the runner's TMPDIR length. The point of this
	// regression test is the SUFFIX of the path (which used to embed the
	// full session name); the prefix is owned by the deployment.
	stateHome, err := shortStateHome(t)
	if err != nil {
		t.Fatalf("shortStateHome: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)

	sockPath, err := prismsession.SidecarHostAPIPath(worstCase)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	if mkErr := os.MkdirAll(filepath.Dir(sockPath), 0o700); mkErr != nil {
		t.Fatalf("mkdir socket dir: %v", mkErr)
	}

	ln, listenErr := net.Listen("unix", sockPath)
	if listenErr != nil {
		// If we got EINVAL, that is the exact bug #1050 fixed; surface it
		// loudly so the regression is unmistakable.
		if errors.Is(listenErr, syscall.EINVAL) {
			t.Fatalf("net.Listen returned EINVAL — sun_path overflow regression (#1050): %v (path=%q, len=%d)",
				listenErr, sockPath, len(sockPath))
		}
		t.Fatalf("net.Listen unexpected error: %v (path=%q, len=%d)", listenErr, sockPath, len(sockPath))
	}
	defer ln.Close()
}

// shortStateHome creates a short-prefix directory under /tmp (NOT under
// t.TempDir(), which Go derives from TMPDIR and may itself be long enough to
// blow the budget on some runners). The test cleans up via t.Cleanup.
func shortStateHome(t *testing.T) (string, error) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "p1050-")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}

// TestSidecarRun_BindFailureReturnsError is the AC-4 regression test. It
// injects a bind failure by pointing HostAPISockPath at a path that cannot be
// created (a directory whose parent is a regular file) and asserts that Run()
// returns within a bounded time with a non-nil error mentioning "bind failed".
//
// Before the #1050 fix this would log-and-continue with hostAPIListener nil,
// leaving the agent partially functional and effectively undetectable.
func TestSidecarRun_BindFailureReturnsError(t *testing.T) {
	// Construct an HostAPISockPath whose parent directory cannot be created
	// because some component along the path is a regular file — that makes
	// both MkdirAll and the subsequent net.Listen fail without depending on
	// platform-specific permission behaviour.
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	// Path: <tmp>/not-a-dir/sub/hostapi.sock — MkdirAll on the parent will
	// fail (ENOTDIR) and net.Listen will then fail with the missing dir.
	sockPath := filepath.Join(blocker, "sub", "hostapi.sock")

	clk := newTestClock()
	d := openTestDB(t)

	cfg := Config{
		SessionName:     "test-repo@bind-fail",
		Repo:            "test-repo",
		Worktree:        "/tmp/test-worktree",
		HarnessURL:      "http://127.0.0.1:1", // unreachable, but Run() should exit before SSE setup matters
		HostAPISockPath: sockPath,
		DB:              d,
		Clock:           clk,
		Harness:         newSSEHarness(),
	}
	s := New(cfg)

	// Run with a bounded timeout. AC-4 says "within a bounded time".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run returned nil error despite injected bind failure")
		}
		if !strings.Contains(err.Error(), "bind failed") &&
			!strings.Contains(err.Error(), "host-API socket") {
			t.Fatalf("Run returned wrong error (want bind-related): %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Run did not exit within bounded time after bind failure")
	}

	// Defensive: ensure no listener was registered on the sidecar.
	s.mu.Lock()
	ln := s.hostAPIListener
	s.mu.Unlock()
	if ln != nil {
		t.Error("hostAPIListener was set despite bind failure")
	}
}

// ── #1486 tests ──────────────────────────────────────────────────────────────

// TestHostAPI_SurvivesHarnessPipeHandshakeFailure_NeverConnects is the
// primary regression test for #1486 (AC-1).
//
// It creates a PI sidecar with both a host-API socket and a harness-pipe
// socket, then never connects to the harness pipe (triggering a startup
// timeout). After the handshake times out, the host-API socket must still
// accept and serve HTTP requests.
func TestHostAPI_SurvivesHarnessPipeHandshakeFailure_NeverConnects(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the host-API socket to appear.
	waitForHostAPISock(t, hostAPISockPath)

	// Wait for the host-API to be ready (it starts serving before the harness-pipe
	// handshake even begins — this exercises that it remains alive after failure).
	waitForHostAPIReady(t, hostAPISockPath)

	// Never connect to the harness pipe — let the startup timeout expire.
	// StartupConnectTimeout is 200ms for this test.
	// The sidecar should record error state in the DB but NOT tear down the
	// host-API socket.

	// Poll until the DB shows error state (written by runStartupSocketPipe
	// timeout path). The state write happens after the 200ms startup timeout
	// fires; under contended scheduling (race detector + Nix sandbox CI) the
	// scheduling latency between timer fire and DB commit can run into the
	// hundreds of ms, so a 5s ceiling is used instead of the original 2s
	// (#1760). The poll exits as soon as the state lands, so a generous
	// ceiling is essentially free on healthy runs.
	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, "error", 5*time.Second); st != "error" {
		t.Errorf("DB state after harness-pipe timeout = %q, want error", st)
	}

	// The host-API must still be serving after the timeout-induced error state.
	code, body := dialHostAPIHTTP(t, hostAPISockPath, "/list-sessions")
	if code != http.StatusOK {
		t.Errorf("host-API /list-sessions after harness-pipe timeout: status=%d body=%q, want 200", code, body)
	}

	// Trigger external shutdown — only NOW should the host-API go down.
	// Per the production lifecycle: Shutdown() is called first (to write state
	// and clean up listeners), then cancel() to stop Run().
	sc.Shutdown()
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not finish within 3s after Shutdown+cancel")
	}

	// After Shutdown(), the socket file must be removed.
	if _, err := os.Stat(hostAPISockPath); !os.IsNotExist(err) {
		t.Errorf("host-API socket still exists after Shutdown: %v", err)
	}
}

// TestHostAPI_SurvivesHarnessPipeHandshakeFailure_MalformedHello is a
// second variant of AC-1: the extension connects but sends a malformed hello.
func TestHostAPI_SurvivesHarnessPipeHandshakeFailure_MalformedHello(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the host-API socket and the harness-pipe socket.
	waitForHostAPISock(t, hostAPISockPath)
	waitForHostAPIReady(t, hostAPISockPath)

	// Wait for harness-pipe socket to appear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(pipeSockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness-pipe socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Connect to the harness pipe and send a malformed hello (not valid JSON).
	conn, err := net.Dial("unix", pipeSockPath)
	if err != nil {
		t.Fatalf("dial harness pipe: %v", err)
	}
	// Send malformed JSON — the sidecar will reject it and call writeStartupError.
	fmt.Fprintf(conn, "this is not json\n")
	conn.Close()

	// Poll until the DB shows error state — the error write from
	// runStartupSocketPipe can land any time after the malformed hello is
	// rejected, so polling avoids a sleep-then-assert race (issue #1595).
	// 5s ceiling chosen to absorb scheduler contention on race-detector
	// CI runs (#1760).
	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, "error", 5*time.Second); st != "error" {
		t.Errorf("DB state after malformed hello = %q, want error", st)
	}

	// The host-API must still be serving after the malformed hello.
	code, body := dialHostAPIHTTP(t, hostAPISockPath, "/list-sessions")
	if code != http.StatusOK {
		t.Errorf("host-API /list-sessions after malformed hello: status=%d body=%q, want 200", code, body)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not finish within 3s after ctx cancel")
	}
}

// TestHostAPI_HarnessPipeFailureRecordsErrorState verifies AC-2 (#1486):
// when the harness-pipe handshake fails, the sidecar records an error state in
// the DB and does NOT call Shutdown() prematurely — shuttingDown remains false
// until an external trigger.
func TestHostAPI_HarnessPipeFailureRecordsErrorState(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())

	go func() { sc.Run(ctx) }() //nolint:errcheck

	// Wait for harness-pipe socket, then poll until the DB shows error state.
	// A fixed sleep-then-assert races the timeout handler's DB write under
	// contended scheduling (e.g. the Nix sandbox runner); a poll-loop with a
	// 5s ceiling is robust without materially slowing healthy runs
	// (issues #1595, #1760).
	waitForHostAPISock(t, hostAPISockPath)
	if s := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, "error", 5*time.Second); s != "error" {
		t.Errorf("DB state after harness-pipe timeout = %q, want error", s)
	}

	// shuttingDown must still be false — Shutdown() has not been called.
	sc.mu.Lock()
	shuttingDown := sc.shuttingDown
	sc.mu.Unlock()
	if shuttingDown {
		t.Error("shuttingDown = true after harness-pipe failure — Shutdown() must not be called on pipe failure")
	}

	// Trigger external shutdown to clean up.
	sc.Shutdown()
	cancel()
}

// TestHostAPI_ShutdownRemovesSocket verifies AC-3 (#1486): when the sidecar
// receives a real Shutdown trigger (SIGTERM / sc.Shutdown()), the host-API
// Unix socket file is removed from disk and a subsequent connect() returns
// ENOENT, not ECONNREFUSED.
//
// Per the production lifecycle, Shutdown() is called before cancel(), so this
// test mirrors that ordering.
func TestHostAPI_ShutdownRemovesSocket(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the host-API to be ready.
	waitForHostAPISock(t, hostAPISockPath)
	waitForHostAPIReady(t, hostAPISockPath)

	// Trigger Shutdown — mirrors the production signal handler: Shutdown() first,
	// then cancel() to stop Run(). The harness-pipe startup timeout is 200ms;
	// we haven't connected so Run() is blocked in <-ctx.Done() inside our
	// pipe-failure path. Shutdown() signals the graceful exit.
	sc.Shutdown()
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not finish within 5s after Shutdown+cancel")
	}

	// The socket file must be gone.
	if _, err := os.Stat(hostAPISockPath); !os.IsNotExist(err) {
		t.Errorf("host-API socket still exists after Shutdown: %v", err)
	}

	// A subsequent connect() must return ENOENT (no such file), not ECONNREFUSED.
	_, dialErr := net.DialTimeout("unix", hostAPISockPath, time.Second)
	if dialErr == nil {
		t.Fatal("expected dial error after Shutdown, got nil")
	}
	// ECONNREFUSED would mean the socket file is still there (tombstone).
	if errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("connect after Shutdown returned ECONNREFUSED (stale socket), want ENOENT: %v", dialErr)
	}
}

// TestHostAPI_StaleTombstoneSocket verifies AC-4 (edge case, #1486): when
// the sidecar exits abnormally and a stale socket file remains on disk, a dial
// to that socket returns ECONNREFUSED rather than ENOENT. The
// isStaleTombstoneSocket helper (used in cmd/hostapi.go and
// internal/promptdelivery) must detect this condition.
//
// This test creates a dangling socket file — a socket that exists on disk but
// has no listener — using raw syscalls so the file is NOT automatically removed
// when the file descriptor is closed (Go's net.Listen does remove it on Close).
//
// It verifies:
//  1. The raw dial error is ECONNREFUSED (not ENOENT).
//  2. The isStaleTombstoneSocketForTest function correctly identifies a tombstone.
//  3. For a missing socket, isStaleTombstoneSocketForTest returns false.
func TestHostAPI_StaleTombstoneSocket(t *testing.T) {
	// Use raw syscalls to create the socket file without Go's auto-cleanup.
	// Go's net.Listen("unix", ...).Close() automatically removes the socket file,
	// which doesn't simulate the abnormal-exit tombstone case.
	dir, err := os.MkdirTemp("", "tombstone-test")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "dead.sock")

	// Create, bind, listen, and close at the syscall level.
	fd, sErr := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if sErr != nil {
		t.Fatalf("syscall.Socket: %v", sErr)
	}
	sa := &syscall.SockaddrUnix{Name: sockPath}
	if bErr := syscall.Bind(fd, sa); bErr != nil {
		syscall.Close(fd) //nolint:errcheck
		t.Fatalf("syscall.Bind: %v", bErr)
	}
	if lErr := syscall.Listen(fd, 1); lErr != nil {
		syscall.Close(fd) //nolint:errcheck
		t.Fatalf("syscall.Listen: %v", lErr)
	}
	// Close the fd without removing the file — leaves a tombstone on disk.
	syscall.Close(fd) //nolint:errcheck

	// Verify the socket file exists.
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file missing after raw close: %v", err)
	}

	// Dial the tombstone socket.
	_, dialErr := net.DialTimeout("unix", sockPath, time.Second)
	if dialErr == nil {
		t.Fatal("expected dial error for tombstone socket, got nil")
	}

	// On Linux and Darwin, a closed-but-existing Unix socket returns ECONNREFUSED.
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Skipf("platform returned %v (not ECONNREFUSED) for tombstone socket; skipping tombstone classification test", dialErr)
	}

	// isStaleTombstoneSocketForTest must return true for the tombstone case.
	if !isStaleTombstoneSocketForTest(sockPath, dialErr) {
		t.Errorf("isStaleTombstoneSocketForTest returned false for tombstone socket; want true")
	}

	// For a missing socket with ECONNREFUSED, isStaleTombstoneSocketForTest
	// must return false (the file does not exist, so it is not a tombstone).
	missingPath := filepath.Join(dir, "missing.sock")
	fakeConnRefused := syscall.ECONNREFUSED
	if isStaleTombstoneSocketForTest(missingPath, fakeConnRefused) {
		t.Errorf("isStaleTombstoneSocketForTest returned true for missing socket; want false")
	}
}

// TestHostAPI_SocketDirPermissions verifies AC-9 (#1486 security invariant):
// the host-API socket directory continues to be created with mode 0o700.
// This is the regression guard that the lifetime change (PR #1486) does not
// widen the on-disk permissions.
func TestHostAPI_SocketDirPermissions(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the host-API socket to appear.
	waitForHostAPISock(t, hostAPISockPath)

	// Check the directory permissions.
	sockDir := filepath.Dir(hostAPISockPath)
	info, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat socket dir %s: %v", sockDir, err)
	}
	perm := info.Mode().Perm()
	if perm != 0o700 {
		t.Errorf("socket dir permissions = %04o, want 0700", perm)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not finish within 5s after ctx cancel")
	}
}

// TestHostAPI_NoGoroutineLeak verifies AC-8 (#1486): when the session is shut
// down after a harness-pipe failure, Shutdown() cleanly stops every goroutine
// it started — the host-API serve goroutine, the harness-pipe writer goroutine,
// and the Run() goroutine itself.
//
// The test measures goroutines before starting the sidecar and asserts that
// after Shutdown()+cancel() the count returns to within a small delta of the
// pre-start baseline.
func TestHostAPI_NoGoroutineLeak(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	// Allow any in-flight goroutines from prior tests to drain before measuring.
	time.Sleep(50 * time.Millisecond)
	baselineGoroutines := goroutineCount()

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the host-API socket (proves Run() has started listeners).
	waitForHostAPISock(t, hostAPISockPath)

	// Poll until the DB shows error state — this confirms the startup timeout
	// has fired and its handler has committed the state write. Only then do we
	// check that the host-API is still serving. Using a poll-loop instead of a
	// fixed sleep avoids a sleep-then-assert race under contended scheduling
	// (e.g. the Nix sandbox runner) (issues #1595, #1760).
	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, "error", 5*time.Second); st != "error" {
		t.Errorf("DB state after harness-pipe timeout = %q, want error", st)
	}

	// Host-API must still be serving at this point (primary regression check).
	code, _ := dialHostAPIHTTP(t, hostAPISockPath, "/list-sessions")
	if code != http.StatusOK {
		t.Errorf("host-API status after pipe failure = %d, want 200", code)
	}

	// Trigger external shutdown (mirrors the signal handler: Shutdown first,
	// then cancel).
	sc.Shutdown()
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not finish within 5s after Shutdown+cancel")
	}

	// Allow a brief window for goroutines to exit.
	// We allow a slack of 3 goroutines above the pre-start baseline to account
	// for the merge-queue watcher goroutine (which may still be draining) and
	// any test-framework goroutines.
	deadline := time.Now().Add(3 * time.Second)
	var finalGoroutines int
	for time.Now().Before(deadline) {
		finalGoroutines = goroutineCount()
		if finalGoroutines <= baselineGoroutines+3 {
			return // goroutines cleaned up within acceptable slack
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("goroutine count after Shutdown = %d, baseline = %d (possible goroutine leak: delta=%d > slack=3)",
		finalGoroutines, baselineGoroutines, finalGoroutines-baselineGoroutines)
}

// TestHostAPI_FullHandshake_BothSurfacesWork verifies AC-6 (#1486): on a fresh
// PI session where the harness-pipe handshake succeeds, both surfaces work:
// the harness pipe completes its handshake AND a host-API GET /list-sessions
// succeeds after the handshake.
func TestHostAPI_FullHandshake_BothSurfacesWork(t *testing.T) {
	pipeSockPath := shortSockPath(t)
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long: %s", hostAPISockPath)
	}

	sc := newPI1486Sidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for both sockets.
	waitForHostAPISock(t, hostAPISockPath)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(pipeSockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness-pipe socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Complete the harness-pipe handshake (stub PI peer).
	conn, _ := dialAndHandshake(t, pipeSockPath)
	defer conn.Close()

	// Both surfaces should work: the harness-pipe handshake has completed AND
	// the host-API is serving.
	code, body := dialHostAPIHTTP(t, hostAPISockPath, "/list-sessions")
	if code != http.StatusOK {
		t.Errorf("host-API /list-sessions after successful handshake: status=%d body=%q, want 200", code, body)
	}

	// Clean shutdown via session_shutdown from the fake peer.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not finish within 3s after session_shutdown")
	}
}

// ── helper functions for #1486 tests ────────────────────────────────────────

// goroutineCount returns the current number of goroutines.
func goroutineCount() int {
	return runtime.NumGoroutine()
}

// isStaleTombstoneSocketForTest is a test-visible wrapper that lets tests
// exercise the tombstone detection logic. The real implementations live in
// cmd/hostapi.go and internal/promptdelivery/promptdelivery.go; here we
// replicate the same logic for package-internal testing.
//
// This avoids importing cmd from internal/sidecar (which would be a cycle).
func isStaleTombstoneSocketForTest(sockPath string, dialErr error) bool {
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return false
	}
	_, statErr := os.Stat(sockPath)
	return statErr == nil
}
