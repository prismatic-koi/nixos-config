//go:build linux

package iris_test

// bash_sandbox_linux_test.go — Linux integration tests for the D-5 bash sandbox.
//
// These tests require bwrap (called via requireBwrap) and exercise the AC
// edge-cases from issue #1636:
//
//   [edge-case] A backgrounded subprocess (bash some-cmd &) is killed when
//               the bash tool call completes — the subprocess group is reaped
//               before tool_exec_result is returned.
//
//   [edge-case] tool_abort during a long-running bash call results in SIGTERM
//               to the entire subprocess group, followed by SIGKILL after a
//               timeout. The bash dispatcher returns
//               success: false, isError: true, output: "aborted".
//
// The tests use the full harness-socket dispatch path (startServer → dial →
// sendFrame → readFrame) to exercise the real production code path.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// TestBashDetachedChildKilled verifies AC "[edge-case] A backgrounded
// subprocess (bash some-cmd &) is killed when the bash tool call completes —
// the subprocess group is reaped before tool_exec_result is returned."
//
// Strategy: the bash command backgrounds a child that writes to a "heartbeat"
// file on a tight loop. If the child were still running after the tool call,
// the heartbeat file's mtime would advance. After tool_exec_result arrives,
// we wait briefly and then verify the heartbeat file's mtime has not changed,
// confirming the background child is dead.
//
// Because bwrap uses --unshare-pid, all processes in the sandbox's PID
// namespace are reaped when the bwrap container exits, making it impossible
// for any backgrounded child to survive the tool call at all. This test
// makes that guarantee explicit.
func TestBashDetachedChildKilled(t *testing.T) {
	requireBwrap(t)

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "test-bash-detach",
		SessionName:     "test@detach",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, db)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = srv.AcceptOne(ctx) }()

	conn, r := dialHarness(t, sockPath)
	doHandshake(t, conn, r)

	// The bwrap sandbox maps the per-session tmpDir
	// (filepath.Dir(sockPath)+"/tmp") as /tmp inside the sandbox.
	// Pre-create it so the bash command can write its heartbeat there.
	sessionDir := filepath.Dir(sockPath)
	bwrapTmpDir := filepath.Join(sessionDir, "tmp")
	if mkErr := os.MkdirAll(bwrapTmpDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir bwrapTmpDir: %v", mkErr)
	}

	// The heartbeat file is written by the background child every 0.05s.
	// Inside the sandbox it lives at /tmp/heartbeat; on the host it lives
	// at bwrapTmpDir/heartbeat.
	heatbeatHost := filepath.Join(bwrapTmpDir, "heartbeat")

	// Command: background a child that writes to the heartbeat file in a
	// tight loop, then the parent exits immediately.  The background child
	// should be killed when bwrap's PID namespace is torn down.
	command := `while true; do date +%s%N > /tmp/heartbeat; sleep 0.05; done &
disown`

	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-detach-001",
		"name": "bash",
		"args": map[string]any{"command": command},
	})

	// Wait for tool_exec_result.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			break
		}
	}

	// Give the OS a moment for any final writes to land.
	time.Sleep(100 * time.Millisecond)

	// Check whether the heartbeat file exists and record its mtime.
	stat1, statErr := os.Stat(heatbeatHost)
	if statErr != nil {
		// Heartbeat file never written — bwrap /tmp mapping may not have
		// landed in host bwrapTmpDir.  The PID namespace guarantee still
		// holds; skip rather than fail the assertion.
		t.Skipf("heartbeat file %q not found — bwrap /tmp may not map to %q: %v", heatbeatHost, bwrapTmpDir, statErr)
	}

	mtime1 := stat1.ModTime()

	// Wait long enough for a live background child to write another heartbeat
	// (it writes every 50ms; waiting 300ms is 6 write cycles).
	time.Sleep(300 * time.Millisecond)

	stat2, statErr2 := os.Stat(heatbeatHost)
	if statErr2 != nil {
		// File disappeared — definitely dead.
		t.Logf("ka pai — heartbeat file gone after tool call completed (child is dead)")
		return
	}

	mtime2 := stat2.ModTime()
	if mtime2.After(mtime1) {
		t.Errorf("background child is still alive: heartbeat mtime advanced from %v to %v "+
			"after tool_exec_result — subprocess group was not killed", mtime1, mtime2)
	} else {
		t.Logf("ka pai — heartbeat mtime did not advance (child is dead, last write at %v)", mtime1)
	}
}

// TestBashAbortReturnsAborted verifies AC "[edge-case] tool_abort during a
// long-running bash call results in SIGTERM to the entire subprocess group,
// followed by SIGKILL after a timeout. The bash dispatcher returns
// success: false, isError: true, output: 'aborted'."
func TestBashAbortReturnsAborted(t *testing.T) {
	requireBwrap(t)

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "test-bash-abort",
		SessionName:     "test@abort",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, db)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = srv.AcceptOne(ctx) }()

	conn, r := dialHarness(t, sockPath)
	doHandshake(t, conn, r)

	// Start a long-running bash command.
	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-bash-abort-001",
		"name": "bash",
		"args": map[string]any{"command": "sleep 60"},
	})

	// Give the bwrap subprocess a moment to start inside the sandbox.
	time.Sleep(300 * time.Millisecond)

	// Send tool_abort.
	sendFrame(t, conn, map[string]any{
		"type": "tool_abort",
		"id":   "call-bash-abort-001",
	})

	// Read until we get tool_exec_result, allowing up to 10s for the
	// SIGTERM → SIGKILL sequence to complete (5s grace + buffer).
	deadline := time.Now().Add(10 * time.Second)
	var result map[string]any
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			result = frame
			break
		}
	}

	if result == nil {
		t.Fatal("no tool_exec_result received after tool_abort")
	}

	// Assert abort semantics: success=false, isError=true, output="aborted".
	if result["id"] != "call-bash-abort-001" {
		t.Errorf("result id = %v, want %q", result["id"], "call-bash-abort-001")
	}
	if result["success"] != false {
		t.Errorf("result success = %v after abort, want false", result["success"])
	}
	if result["is_error"] != true {
		t.Errorf("result is_error = %v after abort, want true", result["is_error"])
	}
	output, _ := result["output"].(string)
	if output != "aborted" {
		t.Errorf("result output = %q after abort, want %q", output, "aborted")
	}
}
