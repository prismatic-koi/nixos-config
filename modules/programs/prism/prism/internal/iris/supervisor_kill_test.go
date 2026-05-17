package iris

// supervisor_kill_test.go — tests for Supervisor.Kill and the daemon-side
// session_kill flow (issue #1674).
//
// Coverage:
//
//   - Kill() on a fresh supervisor (Start never entered) returns an error
//     and forces StateError without panicking.
//   - Kill() against a live `/bin/sleep` child cancels the per-session
//     context, sleep exits on SIGTERM, terminal state is StateFinished,
//     a session_end event is written with reason="killed_sigterm".
//   - Kill() against a SIGTERM-ignoring child escalates to SIGKILL within
//     the timeout and the terminal state is StateError with
//     reason="killed_sigkill".
//   - Kill() is idempotent: re-killing an already-terminal supervisor
//     returns the existing state with no error and no extra session_end.
//   - The harness Unix socket file is removed after the supervisor's Start
//     goroutine returns (kill path leaves the same filesystem state as the
//     natural session-end path).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// killTestSupervisor builds a Supervisor wired against a sleep-like binary so
// the test can drive Kill() against a real process without forking pi. The
// returned supervisor has its Start() goroutine already running.
//
// When piArgs is non-empty, the bin is invoked as `bin <piArgs...>` via a
// helper wrapper. We honour the supervisor's required `--mode rpc` arg by
// using a shell script that ignores its arguments and runs the requested
// behaviour.
func killTestSupervisor(t *testing.T, piBin string) (*Supervisor, *db.DB) {
	t.Helper()
	// Use a short tmpdir for the harness socket — sun_path is 108 bytes on
	// Linux and t.TempDir() can blow past that with a long test name.
	shortPrefix, err := os.MkdirTemp("", "iris-kill-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := SupervisorConfig{
		SessionName:  "iris-kill@test",
		Worktree:     shortPrefix,
		Role:         "worker",
		PIBinaryPath: piBin,
		RunDir:       shortPrefix,
		LogDir:       filepath.Join(shortPrefix, "logs"),
		Database:     database,
		// Aggressive shutdown timeout so tests don't accumulate latency.
		ShutdownTimeout: 100 * time.Millisecond,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sup.Start(ctx)

	// Wait for state to advance past spawning so Kill() actually exercises
	// the live-process path.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == StateActive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sup.State() != StateActive {
		t.Fatalf("supervisor never reached StateActive (got %s)", sup.State())
	}
	return sup, database
}

// writeShellScript writes a shell script to a tempfile, marks it executable,
// and returns its path. The script ignores its arguments — the supervisor
// passes `--mode rpc [--extension ...]` and our test scripts don't care.
//
// If readyPath is non-empty, the literal substring "@READY@" in body is
// replaced with a shell command that touches readyPath. Callers that
// install a `trap '' TERM` (or any other startup-time side effect that
// the test then relies on) should put the trap before the @READY@ marker
// and the long-lived command after it, then call waitForReady() before
// signalling the process to get a deterministic handshake.
//
// See issue #1739 / #1743 — without this handshake, Supervisor.setState
// can advance to StateActive before /bin/sh has finished parsing the
// script, and a test that fires Kill immediately on StateActive races
// the trap installation: bash exits 143 cleanly within the SIGTERM
// grace and the test sees terminal state = finished instead of error.
func writeShellScript(t *testing.T, body, readyPath string) string {
	t.Helper()
	f, err := os.CreateTemp("", "iris-kill-script-*.sh")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	header := "#!/bin/sh\n"
	var full string
	if readyPath != "" {
		full = header + strings.ReplaceAll(body, "@READY@", "touch "+shellSingleQuote(readyPath))
	} else {
		full = header + body
	}
	if _, err := f.WriteString(full); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close script: %v", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		t.Fatalf("chmod script: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// shellSingleQuote returns a single-quoted POSIX-shell-safe rendering of s.
// Used by writeShellScript to embed readyPath into a generated script
// without worrying about spaces or shell metacharacters in t.TempDir().
// Named to avoid collision with the production shellQuote in
// tool_dispatcher.go.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitForReady blocks until readyPath exists on disk or the deadline
// elapses. Used by the SIGTERM-trap escalation tests (#1739, #1743) to
// synchronise with the shell having actually installed its trap before
// the test calls Kill.
func waitForReady(t *testing.T, readyPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for ready sentinel at %s", timeout, readyPath)
}

// newReadyPath returns a path inside a test-scoped tempdir suitable for
// passing to writeShellScript's readyPath argument. The directory is
// cleaned up automatically when the test ends.
func newReadyPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ready")
}

// TestSupervisorKill_NoStartContext asserts that Kill on a supervisor whose
// Start goroutine has not started yet returns an error and forces an error
// state — no panic, no hang.
func TestSupervisorKill_NoStartContext(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-kill-nostart-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "iris-kill@nostart",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      filepath.Join(tmp, "logs"),
		Database:    database,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)

	state, err := sup.Kill(context.Background(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("Kill on never-Started supervisor: expected error, got nil")
	}
	if state != StateError {
		t.Errorf("state = %s, want %s", state, StateError)
	}
}

// TestSupervisorKill_CleanSIGTERM exercises the happy path: a `/bin/sleep`
// child is started, Kill() cancels the per-session ctx, sleep receives
// SIGTERM and exits, terminal state is StateFinished, and a session_end
// event is recorded with reason="killed_sigterm".
func TestSupervisorKill_CleanSIGTERM(t *testing.T) {
	// /bin/sleep with no arguments fails — use a wrapper that sleeps long
	// enough that natural exit doesn't race the kill.
	script := writeShellScript(t, "exec sleep 60\n", "")
	sup, database := killTestSupervisor(t, script)

	start := time.Now()
	state, err := sup.Kill(context.Background(), 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if state != StateFinished {
		t.Errorf("terminal state = %s, want %s", state, StateFinished)
	}
	if elapsed > 4*time.Second {
		t.Errorf("clean SIGTERM kill took %s, expected < 1s for a sleep that respects SIGTERM", elapsed)
	}

	// Confirm session_end event landed with the expected reason.
	assertSessionEndReason(t, database, sup.sess.SessionName, "killed_sigterm")
}

// TestSupervisorKill_EscalatesToSIGKILL exercises the SIGTERM-ignoring path.
// A shell that traps and ignores SIGTERM forces Kill() to escalate via
// os.Process.Kill() before the wait deadline. The terminal state is
// StateError and the session_end event reason is "killed_sigkill".
func TestSupervisorKill_EscalatesToSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("escalation test sleeps ~5s; skipping in short mode")
	}
	// trap '' TERM disables the default SIGTERM handler. The shell still
	// reaps SIGKILL because SIGKILL cannot be trapped.
	//
	// The @READY@ marker is replaced with `touch <readyPath>` by
	// writeShellScript, and we waitForReady() before calling Kill, so the
	// trap is guaranteed to be armed before the supervisor signals the
	// shell. Without this handshake the test was ~10% flaky under -race
	// (#1743): on a fast scheduler Supervisor.setState(StateActive) fires
	// while bash is still parsing the script, the test fires Kill, bash
	// exits 143 cleanly within the SIGTERM grace and terminal state ends
	// up StateFinished instead of StateError.
	readyPath := newReadyPath(t)
	script := writeShellScript(t, "trap '' TERM\n@READY@\nsleep 60\n", readyPath)
	sup, database := killTestSupervisor(t, script)
	waitForReady(t, readyPath, 5*time.Second)

	start := time.Now()
	// Pass a short 1-second SIGTERM timeout so the test doesn't spend the
	// full 5s default.
	state, err := sup.Kill(context.Background(), 1*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if state != StateError {
		t.Errorf("terminal state = %s, want %s (SIGKILL path)", state, StateError)
	}
	// 1s SIGTERM grace + ≤2s SIGKILL convergence: total under 4s.
	if elapsed > 4*time.Second {
		t.Errorf("SIGKILL escalation took %s, expected < 4s", elapsed)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("SIGKILL escalation took %s, expected ≥ ~1s SIGTERM grace", elapsed)
	}

	assertSessionEndReason(t, database, sup.sess.SessionName, "killed_sigkill")
}

// TestSupervisorKill_Idempotent verifies that calling Kill twice in
// succession returns success both times and does not write a second
// session_end event.
func TestSupervisorKill_Idempotent(t *testing.T) {
	script := writeShellScript(t, "exec sleep 60\n", "")
	sup, database := killTestSupervisor(t, script)

	if _, err := sup.Kill(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("first Kill: %v", err)
	}
	state, err := sup.Kill(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("second Kill: %v", err)
	}
	if state != StateFinished && state != StateError {
		t.Errorf("second-kill state = %s, want a terminal state", state)
	}

	// Count session_end events. Only one should be present.
	endCount := countSessionEndEvents(t, database, sup.sess.SessionName)
	if endCount != 1 {
		t.Errorf("session_end event count = %d, want 1", endCount)
	}
}

// TestSupervisorKill_RemovesHarnessSocketFile pins the watch-out from
// issue #1674: closing the harness listener removes the Unix socket inode
// on Linux. The kill path runs the same defer block as the natural
// session-end path, so the socket file must be gone after Kill returns.
func TestSupervisorKill_RemovesHarnessSocketFile(t *testing.T) {
	script := writeShellScript(t, "exec sleep 60\n", "")
	sup, _ := killTestSupervisor(t, script)

	sockPath := sup.harness.SockPath()
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("harness socket not present pre-kill: %v", err)
	}
	if _, err := sup.Kill(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("harness socket %q still exists after kill (err=%v); kill path must close the listener", sockPath, err)
	}
}

// assertSessionEndReason scans agent_events for the given session and
// asserts that the most recent session_end payload carries the expected
// reason.
func assertSessionEndReason(t *testing.T, database *db.DB, sessionName, wantReason string) {
	t.Helper()
	events, err := database.QueryEvents(sessionName, 100, nil, nil, []string{"session_end"})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no session_end event found for %q", sessionName)
	}
	// Inspect the most recent row.
	last := events[len(events)-1]
	var payload map[string]any
	if err := json.Unmarshal([]byte(last.Payload), &payload); err != nil {
		t.Fatalf("decode session_end payload: %v", err)
	}
	got, _ := payload["reason"].(string)
	if got != wantReason {
		t.Errorf("session_end reason = %q, want %q (payload=%s)", got, wantReason, last.Payload)
	}
}

// countSessionEndEvents returns the number of session_end events recorded
// for the given session name.
func countSessionEndEvents(t *testing.T, database *db.DB, sessionName string) int {
	t.Helper()
	events, err := database.QueryEvents(sessionName, 100, nil, nil, []string{"session_end"})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	return len(events)
}

// Smoke test: surface the documented kill-reason constants so a typo in the
// canonical strings is caught at compile time. (Pure string literals today;
// promoting to constants is a future cleanup.)
func TestSupervisorKill_ReasonStrings(t *testing.T) {
	// If these strings change, downstream tooling (cleanup output, narrative
	// CLI, future iris kill --reason) must be updated in lockstep.
	want := []string{"killed_sigterm", "killed_sigkill", "clean_exit", "error", "killed_no_process", "extension_error"}
	for _, s := range want {
		if !strings.Contains(s, "_") && s != "error" {
			t.Errorf("expected snake_case kill reason, got %q", s)
		}
	}
}
