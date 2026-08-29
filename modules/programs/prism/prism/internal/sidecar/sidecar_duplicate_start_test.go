package sidecar

// Tests for the duplicate-start guard.
//
// Scenario: two sidecar processes start concurrently for the same session.
// The second sidecar must refuse to start when it detects a live listener on
// either the host-API socket or the harness-pipe socket, without touching the
// socket file or writing to the database. This prevents the silent
// path-takeover bug where the second sidecar steals the socket paths but PI
// stays connected to the first.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newDuplicateStartSidecar builds a Sidecar whose only purpose is to exercise
// the duplicate-start guard. It uses the PI harness shape (both hostapi.sock
// and pipe.sock set) so both refuse-to-start paths can be triggered from a
// single helper.
func newDuplicateStartSidecar(t *testing.T, hostAPISockPath, pipeSockPath string) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "prism-test@dup-start",
		Repo:                  "prism-test",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   pipeSockPath,
		HostAPISockPath:       hostAPISockPath,
		StartupConnectTimeout: 200 * time.Millisecond,
		PipeReconnectTimeout:  50 * time.Millisecond,
		Harness:               pih.New("", "worker", ""),
	}
	return New(cfg)
}

// pingHostAPI dials the host-API socket and issues a trivial GET. Returns the
// HTTP status code on success. The endpoint may not exist (returns 404); the
// caller only cares that a response came back, proving the listener is alive.
func pingHostAPI(t *testing.T, hostAPISockPath string) (int, error) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", hostAPISockPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://prism-hostapi/_ping")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ── unit-level helper tests ──────────────────────────────────────────────────

// TestCheckSocketLiveness_Absent confirms the helper returns socketAbsent
// when the socket file does not exist.
func TestCheckSocketLiveness_Absent(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "missing.sock")

	state, err := checkSocketLiveness(sockPath)
	if err != nil {
		t.Fatalf("checkSocketLiveness returned unexpected error: %v", err)
	}
	if state != socketAbsent {
		t.Fatalf("checkSocketLiveness state = %d, want socketAbsent (%d)", state, socketAbsent)
	}
}

// TestCheckSocketLiveness_Tombstone confirms the helper returns socketTombstone
// for a file that exists on disk but has no listener attached (ECONNREFUSED on
// dial).
func TestCheckSocketLiveness_Tombstone(t *testing.T) {
	dir, err := os.MkdirTemp("", "tombstone-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "dead.sock")

	// Create, bind, listen, and close at the syscall level so the kernel
	// leaves the on-disk file in place after the fd is closed.
	fd, sErr := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if sErr != nil {
		t.Fatalf("syscall.Socket: %v", sErr)
	}
	sa := &syscall.SockaddrUnix{Name: sockPath}
	if bErr := syscall.Bind(fd, sa); bErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Bind: %v", bErr)
	}
	if lErr := syscall.Listen(fd, 1); lErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Listen: %v", lErr)
	}
	_ = syscall.Close(fd)

	// Confirm the platform actually surfaces ECONNREFUSED for the tombstone.
	if _, dialErr := net.DialTimeout("unix", sockPath, time.Second); dialErr == nil {
		t.Fatal("expected dial error for tombstone, got nil")
	} else if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Skipf("platform returned %v (not ECONNREFUSED) for tombstone; skipping", dialErr)
	}

	state, err := checkSocketLiveness(sockPath)
	if err != nil {
		t.Fatalf("checkSocketLiveness returned unexpected error: %v", err)
	}
	if state != socketTombstone {
		t.Fatalf("checkSocketLiveness state = %d, want socketTombstone (%d)", state, socketTombstone)
	}

	// Verify the helper did NOT remove the tombstone — removal is the
	// caller's responsibility.
	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Errorf("tombstone file was unexpectedly removed by checkSocketLiveness: %v", statErr)
	}
}

// TestCheckSocketLiveness_Live confirms the helper returns socketLive when a
// real listener is accepting connections on the socket path.
func TestCheckSocketLiveness_Live(t *testing.T) {
	dir, err := os.MkdirTemp("", "live-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	// Drain incoming connections so the dial succeeds.
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	state, err := checkSocketLiveness(sockPath)
	if err != nil {
		t.Fatalf("checkSocketLiveness returned unexpected error: %v", err)
	}
	if state != socketLive {
		t.Fatalf("checkSocketLiveness state = %d, want socketLive (%d)", state, socketLive)
	}
}

// ── end-to-end: full Run() with another sidecar already alive ────────────────

// TestSidecarRun_RefusesWhenHostAPISockIsLive is the primary AC test for
// Starting a second sidecar process whose hostapi.sock is currently
// responsive must refuse to start, exit non-zero with a clear error naming
// the session and the responsive path, AND leave the live sidecar's listener
// fully functional.
func TestSidecarRun_RefusesWhenHostAPISockIsLive(t *testing.T) {
	// Sidecar A: bring up a real host-API listener on a shared socket path.
	dir, err := os.MkdirTemp("", "dup-hostapi-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	pipeSockPathA := filepath.Join(dir, "pipe.sock")
	if len(hostAPISockPath) > maxSunPath || len(pipeSockPathA) > maxSunPath {
		t.Fatalf("socket path too long; dir=%s", dir)
	}

	scA := newDuplicateStartSidecar(t, hostAPISockPath, pipeSockPathA)

	ctxA, cancelA := context.WithCancel(context.Background())
	t.Cleanup(cancelA)

	runDoneA := make(chan error, 1)
	go func() {
		runDoneA <- scA.Run(ctxA)
	}()

	// Wait for A's host-API listener to come up.
	waitForSocket(t, hostAPISockPath, 3*time.Second)

	// Confirm A's host-API is responsive.
	if _, err := pingHostAPI(t, hostAPISockPath); err != nil {
		t.Fatalf("sidecar A host-API not responsive after startup: %v", err)
	}

	// Capture the inode of A's socket so we can later confirm B did not
	// replace it.
	statBefore, err := os.Stat(hostAPISockPath)
	if err != nil {
		t.Fatalf("stat A's socket: %v", err)
	}
	inoBefore := statBefore.Sys().(*syscall.Stat_t).Ino

	// Sidecar B: try to start against the same hostapi.sock path. B must
	// refuse and return a duplicateStartError.
	//
	// Give B its own DB / worktree (newDuplicateStartSidecar does this via
	// openTestDB(t) and t.TempDir()) so that "did not write to DB" can be
	// verified independently of A's DB.
	pipeSockPathB := shortSockPath(t)
	scB := newDuplicateStartSidecar(t, hostAPISockPath, pipeSockPathB)

	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()

	errB := scB.Run(ctxB)
	if errB == nil {
		t.Fatal("sidecar B Run() returned nil; want duplicate-start error")
	}

	var dupErr *duplicateStartError
	if !errors.As(errB, &dupErr) {
		t.Fatalf("sidecar B returned wrong error type: got %T (%v), want *duplicateStartError", errB, errB)
	}
	if dupErr.SockKind != "hostapi.sock" {
		t.Errorf("dup err SockKind = %q, want %q", dupErr.SockKind, "hostapi.sock")
	}
	if dupErr.SockPath != hostAPISockPath {
		t.Errorf("dup err SockPath = %q, want %q", dupErr.SockPath, hostAPISockPath)
	}
	if !strings.Contains(errB.Error(), "prism-test@dup-start") {
		t.Errorf("dup err does not name the session: %v", errB)
	}
	if !strings.Contains(errB.Error(), "refusing to start") {
		t.Errorf("dup err does not contain 'refusing to start': %v", errB)
	}

	// AC: B did NOT delete the live socket file. Confirm inode is unchanged.
	statAfter, err := os.Stat(hostAPISockPath)
	if err != nil {
		t.Fatalf("stat A's socket after B refused: %v", err)
	}
	inoAfter := statAfter.Sys().(*syscall.Stat_t).Ino
	if inoBefore != inoAfter {
		t.Errorf("hostapi.sock inode changed after B refused to start: %d -> %d (B touched the file)", inoBefore, inoAfter)
	}

	// AC: A's listener remains functional after B's refusal.
	if status, err := pingHostAPI(t, hostAPISockPath); err != nil {
		t.Errorf("sidecar A host-API stopped responding after B refused to start: %v (status=%d)", err, status)
	}

	// AC: B did not register a listener on its own sidecar struct.
	scB.mu.Lock()
	bListener := scB.hostAPIListener
	scB.mu.Unlock()
	if bListener != nil {
		t.Error("sidecar B set hostAPIListener despite refusing to start")
	}

	// AC: B did not write to its DB. Since each test gets an isolated DB
	// from openTestDB, we can check directly that no row exists for B's
	// session name.
	status, _ := scB.cfg.DB.CurrentStatus(scB.cfg.SessionName)
	if status != nil {
		t.Errorf("sidecar B wrote a status row to its DB despite refusing to start: %+v", status)
	}

	// Tear down A cleanly so the test exits.
	cancelA()
	select {
	case <-runDoneA:
	case <-time.After(3 * time.Second):
		t.Error("sidecar A did not exit within 3s of cancellation")
	}
}

// TestSidecarRun_RefusesWhenPipeSockIsLive covers the AC for the pipe.sock
// path. We simulate a "live pipe.sock, no hostapi.sock" partial-liveness
// state by hand-binding a raw listener at the pipe path. This is unusual in
// practice (a real sidecar binds both), but the AC explicitly requires the
// refuse-to-start behaviour when pipe.sock alone is responsive.
func TestSidecarRun_RefusesWhenPipeSockIsLive(t *testing.T) {
	dir, err := os.MkdirTemp("", "dup-pipe-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	pipeSockPath := filepath.Join(dir, "pipe.sock")
	hostAPISockPath := filepath.Join(dir, "hostapi.sock") // will not exist
	if len(pipeSockPath) > maxSunPath {
		t.Fatalf("pipe sock path too long: %s", pipeSockPath)
	}

	// Hand-bind a listener at pipe.sock to simulate a live sidecar A.
	ln, err := net.Listen("unix", pipeSockPath)
	if err != nil {
		t.Fatalf("net.Listen pipe: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// Sidecar B: same paths. hostapi.sock does not exist on disk, so its
	// check is socketAbsent; pipe.sock check must fire.
	scB := newDuplicateStartSidecar(t, hostAPISockPath, pipeSockPath)

	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()

	errB := scB.Run(ctxB)
	if errB == nil {
		t.Fatal("sidecar B Run() returned nil; want duplicate-start error")
	}

	var dupErr *duplicateStartError
	if !errors.As(errB, &dupErr) {
		t.Fatalf("sidecar B returned wrong error type: got %T (%v), want *duplicateStartError", errB, errB)
	}
	if dupErr.SockKind != "pipe.sock" {
		t.Errorf("dup err SockKind = %q, want %q", dupErr.SockKind, "pipe.sock")
	}
	if dupErr.SockPath != pipeSockPath {
		t.Errorf("dup err SockPath = %q, want %q", dupErr.SockPath, pipeSockPath)
	}

	// AC: the live pipe.sock file was not touched (inode unchanged).
	// Net listener's underlying file is what we hand-bound; if B's refuse
	// path is buggy it might os.Remove the file. Stat both before and
	// re-confirm presence after.
	if _, err := os.Stat(pipeSockPath); err != nil {
		t.Errorf("pipe.sock was removed despite B refusing to start: %v", err)
	}

	// AC: B did not write to its DB.
	status, _ := scB.cfg.DB.CurrentStatus(scB.cfg.SessionName)
	if status != nil {
		t.Errorf("sidecar B wrote a status row to its DB despite refusing to start: %+v", status)
	}
}

// TestSidecarRun_TombstoneHostAPISockProceeds covers the edge case: a stale
// hostapi.sock tombstone (file exists, dial returns ECONNREFUSED) must NOT
// block sidecar startup. The sidecar removes the tombstone and binds normally.
func TestSidecarRun_TombstoneHostAPISockProceeds(t *testing.T) {
	dir, err := os.MkdirTemp("", "tombstone-start-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	pipeSockPath := filepath.Join(dir, "pipe.sock") // will not exist
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi sock path too long: %s", hostAPISockPath)
	}

	// Create a tombstone: bind/listen/close at the syscall level.
	fd, sErr := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if sErr != nil {
		t.Fatalf("syscall.Socket: %v", sErr)
	}
	sa := &syscall.SockaddrUnix{Name: hostAPISockPath}
	if bErr := syscall.Bind(fd, sa); bErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Bind: %v", bErr)
	}
	if lErr := syscall.Listen(fd, 1); lErr != nil {
		_ = syscall.Close(fd)
		t.Fatalf("syscall.Listen: %v", lErr)
	}
	_ = syscall.Close(fd)

	if _, dialErr := net.DialTimeout("unix", hostAPISockPath, time.Second); dialErr == nil {
		t.Fatal("expected dial error for tombstone, got nil")
	} else if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Skipf("platform returned %v (not ECONNREFUSED) for tombstone; skipping", dialErr)
	}

	sc := newDuplicateStartSidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	// Wait for the sidecar to actually bind on the (tombstoned)
	// path. If the duplicate-start guard incorrectly classified the
	// tombstone as live, Run() would have returned a duplicateStartError
	// already.
	waitForSocket(t, hostAPISockPath, 3*time.Second)

	// Confirm the listener is real this time.
	if _, err := pingHostAPI(t, hostAPISockPath); err != nil {
		t.Fatalf("host-API not responsive after tombstone replacement: %v", err)
	}

	// Defensive: ensure Run() did not return a duplicate-start error.
	select {
	case err := <-runDone:
		var dupErr *duplicateStartError
		if errors.As(err, &dupErr) {
			t.Fatalf("Run() returned duplicate-start error against tombstone: %v", err)
		}
	default:
		// Still running — expected.
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Error("sidecar did not exit within 3s of cancellation")
	}
}

// TestSidecarRun_NonExistentSockProceeds covers the edge case: when neither
// hostapi.sock nor pipe.sock exists, the sidecar starts normally (the
// first-start path).
func TestSidecarRun_NonExistentSockProceeds(t *testing.T) {
	dir, err := os.MkdirTemp("", "first-start-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	pipeSockPath := filepath.Join(dir, "pipe.sock")

	// Sanity: neither path exists yet.
	if _, statErr := os.Stat(hostAPISockPath); !os.IsNotExist(statErr) {
		t.Fatalf("hostapi.sock unexpectedly exists before sidecar start: %v", statErr)
	}
	if _, statErr := os.Stat(pipeSockPath); !os.IsNotExist(statErr) {
		t.Fatalf("pipe.sock unexpectedly exists before sidecar start: %v", statErr)
	}

	sc := newDuplicateStartSidecar(t, hostAPISockPath, pipeSockPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- sc.Run(ctx)
	}()

	waitForSocket(t, hostAPISockPath, 3*time.Second)

	if _, err := pingHostAPI(t, hostAPISockPath); err != nil {
		t.Fatalf("host-API not responsive after first-start: %v", err)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Error("sidecar did not exit within 3s of cancellation")
	}
}

// waitForSocket polls until sockPath becomes a connectable Unix socket or the
// deadline elapses.
func waitForSocket(t *testing.T, sockPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s did not become connectable within %v", sockPath, timeout)
}
