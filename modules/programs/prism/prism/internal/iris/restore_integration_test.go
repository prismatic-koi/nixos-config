package iris_test

// restore_integration_test.go — end-to-end integration test for D-9 restore.
//
// Test: spawn a session via NewSupervisor (simulating a pre-crash active
// session in the DB), write a tool_call event to simulate an in-flight call,
// then call RunRestore (simulating daemon restart) and verify:
//   - The synthetic tool_result was written with the correct fields.
//   - A new supervisor/goroutine was started for the active session.
//   - Sessions in spawning state were marked error.
//
// This test does NOT SIGKILL a real daemon process — instead it exercises the
// DB-level and supervisor-level behaviour that RunRestore produces. The
// "daemon died" scenario is simulated by:
//   1. Calling insertIrisSession with iris_state="active" to represent the
//      session as it would appear in the DB after a daemon crash.
//   2. Creating a pi JSONL file (empty) to simulate history.
//   3. Calling RunRestore with a fake pi binary that exits immediately.
//
// The test is Linux-only (uses /bin/sh) but otherwise self-contained.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/iris"
)

// TestRestoreIntegration_EndToEnd exercises the full restore path:
//
//  1. A session is represented in the DB as active (iris_state="active") with
//     an in-flight tool_call event (no matching tool_result).
//  2. A pi JSONL file exists at the expected path.
//  3. RunRestore is called.
//  4. Verify: synthetic tool_result written, session was re-spawned.
func TestRestoreIntegration_EndToEnd(t *testing.T) {
	if os.Getenv("CI") == "" {
		// Only skip on non-CI if there's a specific reason; this test is fast
		// enough to run locally.
		_ = struct{}{} // keep tests running locally too
	}

	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	// Use a short run dir to stay under the Unix socket path limit (108 bytes).
	runDir, err := os.MkdirTemp("", "iris-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp runDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })

	// --- Setup: active session with in-flight tool call ---

	worktree := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}

	piUUID := uuid.New().String()
	iid := uuid.New().String()

	// Simulate the session as it would appear after a daemon crash.
	insertIrisSession(t, database, iid, "integration@branch", worktree, "worker", "active", piUUID)

	// Write a tool_call event with no matching tool_result (in-flight at crash).
	orphanCallID := "orphan-integration-call-001"
	writeToolCallEvent(t, database, "integration@branch", worktree, iid, orphanCallID)

	// Write a second tool_call that has a result (should not get synthetic result).
	completedCallID := "completed-call-002"
	writeToolCallEvent(t, database, "integration@branch", worktree, iid, completedCallID)
	writeToolResultEvent(t, database, "integration@branch", worktree, iid, completedCallID)

	// --- Setup: pi JSONL file ---

	piAgentDir := filepath.Join(tmp, "pi-agent")
	encodedCWD := encodePiCWDForTest(worktree)
	sessionsDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll sessionsDir: %v", err)
	}
	// Write a minimal pi JSONL file.
	ts := time.Now().UTC().Format("20060102T150405Z")
	jsonlFile := filepath.Join(sessionsDir, ts+"_"+piUUID+".jsonl")
	if err := os.WriteFile(jsonlFile, []byte(`{"type":"session_init"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile jsonl: %v", err)
	}

	// --- Setup: also add a spawning session ---
	spawningIID := uuid.New().String()
	insertIrisSession(t, database, spawningIID, "integration@spawning", worktree, "worker", "spawning", "")

	// --- Run restore ---

	fakePiBin := filepath.Join(tmp, "fake-pi")
	if err := os.WriteFile(fakePiBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake-pi: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := iris.RestoreConfig{
		Database:   database,
		RunDir:     runDir,
		PIAgentDir: piAgentDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: fakePiBin,
			RunDir:       runDir,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(ctx, cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	// --- Assertions ---

	// Spawning session was marked error.
	if result.SpawningMarkError != 1 {
		t.Errorf("SpawningMarkError = %d, want 1", result.SpawningMarkError)
	}
	spawningRow, err := database.SessionByInstanceID(spawningIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID(spawning): %v", err)
	}
	if spawningRow.EndState == nil || *spawningRow.EndState != "error" {
		t.Errorf("spawning session end_state = %v, want 'error'", spawningRow.EndState)
	}

	// Orphan synthetic tool_result was written.
	if result.OrphansWritten != 1 {
		t.Errorf("OrphansWritten = %d, want 1 (only orphan-integration-call-001 should get synthetic result)", result.OrphansWritten)
	}
	if !hasSyntheticToolResult(t, database, "integration@branch", orphanCallID) {
		t.Error("synthetic tool_result for orphaned call not found in DB")
	}

	// The completed call should NOT have a synthetic result.
	events, _ := database.AllSessionEvents("integration@branch")
	for _, e := range events {
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(e.Payload), &m) //nolint:errcheck
		id, _ := m["id"].(string)
		synth, _ := m["synthetic"].(bool)
		if id == completedCallID && synth {
			t.Errorf("found unexpected synthetic tool_result for completed call %q", completedCallID)
		}
	}

	// Active session was re-spawned (supervisor started).
	if result.SessionsRestored != 1 {
		t.Errorf("SessionsRestored = %d, want 1", result.SessionsRestored)
	}
	if result.SessionsSkipped != 0 {
		t.Errorf("SessionsSkipped = %d, want 0", result.SessionsSkipped)
	}

	// Verify --session flag was passed by checking the harness socket was created
	// under the runDir (not tmp).
	harnessSock := filepath.Join(runDir, iid, "harness.sock")
	// Give the goroutine a moment to start.
	var sockExists bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(harnessSock); err == nil {
			sockExists = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sockExists {
		t.Errorf("harness socket %q was not created (expected supervisor to start and listen)", harnessSock)
	}

}

// TestRestoreIntegration_MultipleActiveSessions verifies that multiple
// active sessions are all processed (restored or skipped) concurrently and
// that the total accounts for all sessions.
func TestRestoreIntegration_MultipleActiveSessions(t *testing.T) {
	database := openRestoreTestDB(t)

	// Use a short run dir to stay under the Unix socket path limit (108 bytes).
	// Socket path: <runDir>/<uuid>/harness.sock (36-char UUID + '/harness.sock' = 49)
	// So runDir must be <= 108 - 49 - 1 = 58 chars.
	runDir, err := os.MkdirTemp("", "iris-rt-")
	if err != nil {
		t.Fatalf("MkdirTemp runDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })

	tmp := t.TempDir()
	const n = 4 // number of active sessions
	piAgentDir := filepath.Join(tmp, "pia")

	for i := range n {
		worktree := filepath.Join(tmp, "wt", uuid.New().String())
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("MkdirAll worktree %d: %v", i, err)
		}

		piUUID := uuid.New().String()
		iid := uuid.New().String()
		insertIrisSession(t, database, iid, uuid.New().String()+"@branch", worktree, "worker", "active", piUUID)

		// Write a JSONL for each session.
		encodedCWD := encodePiCWDForTest(worktree)
		sessionsDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
		if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll sessionsDir %d: %v", i, err)
		}
		ts := time.Now().UTC().Format("20060102T150405Z")
		jsonlFile := filepath.Join(sessionsDir, ts+"_"+piUUID+".jsonl")
		if err := os.WriteFile(jsonlFile, []byte(`{"type":"session_init"}`+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile jsonl %d: %v", i, err)
		}
	}

	fakePiBin := filepath.Join(tmp, "fake-pi")
	if err := os.WriteFile(fakePiBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake-pi: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := iris.RestoreConfig{
		Database:   database,
		RunDir:     runDir,
		PIAgentDir: piAgentDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: fakePiBin,
			RunDir:       runDir,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(ctx, cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	total := result.SessionsRestored + result.SessionsSkipped
	if total != n {
		t.Errorf("SessionsRestored + SessionsSkipped = %d, want %d", total, n)
	}
	if result.SessionsRestored != n {
		t.Errorf("SessionsRestored = %d, want %d (all sessions have JSONL)", result.SessionsRestored, n)
	}
}
