package main

// logs_test.go — unit tests for `iris logs <session>` (issue #1675).
//
// Covers:
//   - one-shot read: full log, --tail N
//   - missing log file → exit 0 with empty output
//   - --follow: streams new bytes, exits after terminal-state grace
//   - --follow: idle-timeout fallback when the daemon is unreachable
//   - subscribeTerminalState: detects "session not found" vs. "daemon down"
//
// Integration with the supervisor's per-session log writer is covered by
// internal/iris/supervisor_logfile_test.go.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// TestRunLogsOneShot_FullFile exercises the bare `iris logs <session>` path:
// the entire log file is copied to stdout, byte-for-byte.
func TestRunLogsOneShot_FullFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "test.log")
	content := "line one\nline two\nline three\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Reset flag globals between tests — Cobra leaves them set across runs.
	logsTail = 0
	logsFollow = false

	var buf bytes.Buffer
	if err := runLogsOneShot(&buf, logPath); err != nil {
		t.Fatalf("runLogsOneShot: %v", err)
	}
	if buf.String() != content {
		t.Fatalf("output mismatch:\n got:  %q\n want: %q", buf.String(), content)
	}
}

// TestRunLogsOneShot_TailN exercises --tail N: only the last N lines are
// printed, in order.
func TestRunLogsOneShot_TailN(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "test.log")
	content := "a\nb\nc\nd\ne\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logsTail = 3
	logsFollow = false
	t.Cleanup(func() { logsTail = 0 })

	var buf bytes.Buffer
	if err := runLogsOneShot(&buf, logPath); err != nil {
		t.Fatalf("runLogsOneShot: %v", err)
	}
	want := "c\nd\ne\n"
	if buf.String() != want {
		t.Fatalf("tail output mismatch:\n got:  %q\n want: %q", buf.String(), want)
	}
}

// TestRunLogsOneShot_TailLargerThanFile asserts --tail N > file-lines returns
// the entire file rather than padding or erroring.
func TestRunLogsOneShot_TailLargerThanFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "test.log")
	content := "x\ny\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logsTail = 100
	logsFollow = false
	t.Cleanup(func() { logsTail = 0 })

	var buf bytes.Buffer
	if err := runLogsOneShot(&buf, logPath); err != nil {
		t.Fatalf("runLogsOneShot: %v", err)
	}
	if buf.String() != content {
		t.Fatalf("output mismatch:\n got:  %q\n want: %q", buf.String(), content)
	}
}

// TestRunLogsOneShot_MissingFile asserts that a missing log file yields
// exit 0 with no output — the AC: "no log file yet → exit 0 with empty
// output (not error)".
func TestRunLogsOneShot_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "does-not-exist.log")

	logsTail = 0
	logsFollow = false

	var buf bytes.Buffer
	if err := runLogsOneShot(&buf, logPath); err != nil {
		t.Fatalf("runLogsOneShot: expected nil error for missing file, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for missing file, got %q", buf.String())
	}
}

// TestStreamLog_TailFollow_TerminalGrace asserts that streamLog reads new
// bytes appended after start, then exits within the grace window after
// terminalCh closes.
func TestStreamLog_TailFollow_TerminalGrace(t *testing.T) {
	// Short-circuit constants for the test so the suite stays fast.
	oldGrace := logsFollowGraceForTest
	logsFollowGraceForTest = 150 * time.Millisecond
	t.Cleanup(func() { logsFollowGraceForTest = oldGrace })

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "tail.log")
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	terminalCh := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		// Start at offset 0 so the test can see "initial\n" plus any appended lines.
		done <- streamLog(ctx, &buf, logPath, 0, terminalCh)
	}()

	// Append a second line after a brief delay so the poller picks it up.
	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("appended\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	// Allow the poller to read the appended line.
	time.Sleep(2 * logsPollInterval)

	// Signal terminal state. streamLog should exit within grace.
	close(terminalCh)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamLog returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamLog did not exit within timeout after terminalCh closed")
	}

	got := buf.String()
	if !strings.Contains(got, "initial") || !strings.Contains(got, "appended") {
		t.Fatalf("expected to see both 'initial' and 'appended' in output, got %q", got)
	}
}

// TestStreamLog_IdleTimeout asserts that with terminalCh == nil (daemon
// unreachable), streamLog exits after logsIdleGraceForTest of no new bytes.
func TestStreamLog_IdleTimeout(t *testing.T) {
	oldIdle := logsIdleGraceForTest
	logsIdleGraceForTest = 200 * time.Millisecond
	t.Cleanup(func() { logsIdleGraceForTest = oldIdle })

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "idle.log")
	if err := os.WriteFile(logPath, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var buf bytes.Buffer
	start := time.Now()
	if err := streamLog(ctx, &buf, logPath, 0, nil); err != nil {
		t.Fatalf("streamLog: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("streamLog returned too quickly: %v (expected >= idle grace)", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("streamLog took too long: %v", elapsed)
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("expected initial content, got %q", buf.String())
	}
}

// TestSubscribeTerminalState_SessionNotFound starts an in-process daemon
// client socket with no sessions, then asserts that subscribeTerminalState
// returns a "not found" error (not a daemon-unreachable error).
func TestSubscribeTerminalState_SessionNotFound(t *testing.T) {
	sockPath := startEmptyDaemonSocket(t)
	terminalCh := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := subscribeTerminalState(ctx, sockPath, "nonexistent@branch", terminalCh)
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if !isSessionNotFound(err) {
		t.Fatalf("expected session-not-found error, got: %v", err)
	}
}

// TestSubscribeTerminalState_DaemonDown asserts that a non-existent socket
// path returns a non-not-found error (the streamer's fallback path).
func TestSubscribeTerminalState_DaemonDown(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "missing.sock")

	terminalCh := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := subscribeTerminalState(ctx, sockPath, "any@branch", terminalCh)
	if err == nil {
		t.Fatal("expected error for missing daemon socket, got nil")
	}
	if isSessionNotFound(err) {
		t.Fatalf("expected daemon-down error, got session-not-found: %v", err)
	}
}

// TestSubscribeTerminalState_TerminalCloses starts an in-process daemon,
// subscribes, then publishes a "finished" state. terminalCh must close.
func TestSubscribeTerminalState_TerminalCloses(t *testing.T) {
	sessions := []iris.SessionSnapshot{{Name: "x@branch", InstanceID: "00000000-0000-0000-0000-000000000001", State: "active"}}
	cs, sockPath := startDaemonSocketWithSessions(t, sessions)

	terminalCh := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := subscribeTerminalState(ctx, sockPath, "x@branch", terminalCh); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Give the daemon's runSubscription goroutine a moment to register
	// the live channel before we publish.
	time.Sleep(50 * time.Millisecond)
	cs.PublishState("x@branch", "finished")

	select {
	case <-terminalCh:
	case <-time.After(2 * time.Second):
		t.Fatal("terminalCh did not close after publishing 'finished' state")
	}
}

// --- Helpers ---

// startEmptyDaemonSocket starts an iris.ClientSocket bound to a tempdir
// socket path, with no active sessions. The socket is torn down on cleanup.
func startEmptyDaemonSocket(t *testing.T) string {
	t.Helper()
	_, sockPath := startDaemonSocketWithSessions(t, nil)
	return sockPath
}

// startDaemonSocketWithSessions starts an iris.ClientSocket bound to a
// tempdir socket path, returning the socket and its path. The provided
// sessions list is served via the GetActiveSessions callback.
func startDaemonSocketWithSessions(t *testing.T, sessions []iris.SessionSnapshot) (*iris.ClientSocket, string) {
	t.Helper()

	// Short prefix to stay under the 108-byte sun_path limit.
	shortPrefix, err := os.MkdirTemp("", "iris-logs-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })
	sockPath := filepath.Join(shortPrefix, "iris.sock")

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return sessions
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	serveCtx, serveCancel := context.WithCancel(context.Background())
	t.Cleanup(serveCancel)
	go cs.Serve(serveCtx)

	// Spin until the socket file exists.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return cs, sockPath
}

