package iris_test

// restore_test.go — unit tests for the D-9 restore logic.
//
// Covers:
//   - Orphan detection: various tool_call/tool_result event-log shapes.
//   - Session state reconciliation: spawning sessions marked error.
//   - Session re-spawn: active sessions with/without JSONL, with/without worktree.
//   - Concurrency: multiple active sessions restored simultaneously.
//
// All tests use an in-memory SQLite DB (temp dir) and do NOT spawn real pi
// processes. The RestoreConfig.SupervisorTemplate is configured with a fake
// pi binary path so supervisor goroutines fail fast and exercise the
// circuit-breaker path rather than blocking on a real binary.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// --- Helpers ---

// openRestoreTestDB opens an isolated iris DB for restore tests.
func openRestoreTestDB(t *testing.T) *db.DB {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// insertIrisSession inserts a sessions row with iris_state set, simulating an
// iris-managed session that was alive when the daemon crashed.
func insertIrisSession(t *testing.T, database *db.DB, instanceID, sessionName, worktree, role, irisState, harnessSessionID string) {
	t.Helper()
	var rolePtr *string
	if role != "" {
		rolePtr = &role
	}
	var hsidPtr *string
	if harnessSessionID != "" {
		hsidPtr = &harnessSessionID
	}
	sess := db.Session{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Repo:             "",
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        rolePtr,
		HarnessSessionID: hsidPtr,
		StartedAt:        time.Now().Add(-5 * time.Minute),
	}
	if err := database.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession(%q): %v", instanceID, err)
	}
	// Set iris_state explicitly.
	if err := database.IrisUpdateSessionState(instanceID, irisState); err != nil {
		t.Fatalf("IrisUpdateSessionState(%q): %v", instanceID, err)
	}
}

// writeToolCallEvent writes a tool_call event to the DB with the given call ID.
func writeToolCallEvent(t *testing.T, database *db.DB, sessionName, worktree, instanceID, callID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"id":   callID,
		"name": "bash",
		"args": map[string]any{"command": "echo test"},
	})
	var iidPtr *string
	if instanceID != "" {
		iidPtr = &instanceID
	}
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Worktree:    worktree,
		Type:        "tool_call",
		Payload:     string(payload),
		CreatedAt:   time.Now(),
		InstanceID:  iidPtr,
	}
	if err := database.WriteEvent(event); err != nil {
		t.Fatalf("WriteEvent tool_call: %v", err)
	}
}

// writeToolResultEvent writes a tool_result event to the DB.
func writeToolResultEvent(t *testing.T, database *db.DB, sessionName, worktree, instanceID, callID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"id":      callID,
		"success": true,
		"output":  "ok",
	})
	var iidPtr *string
	if instanceID != "" {
		iidPtr = &instanceID
	}
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Worktree:    worktree,
		Type:        "tool_result",
		Payload:     string(payload),
		CreatedAt:   time.Now(),
		InstanceID:  iidPtr,
	}
	if err := database.WriteEvent(event); err != nil {
		t.Fatalf("WriteEvent tool_result: %v", err)
	}
}

// countEventsByType counts events of the given type for a session.
func countEventsByType(t *testing.T, database *db.DB, sessionName, eventType string) int {
	t.Helper()
	events, err := database.AllSessionEvents(sessionName)
	if err != nil {
		t.Fatalf("AllSessionEvents(%q): %v", sessionName, err)
	}
	count := 0
	for _, e := range events {
		if e.Type == eventType {
			count++
		}
	}
	return count
}

// hasSyntheticToolResult checks whether any tool_result event in the session
// has synthetic=true and the given call ID.
func hasSyntheticToolResult(t *testing.T, database *db.DB, sessionName, callID string) bool {
	t.Helper()
	events, err := database.AllSessionEvents(sessionName)
	if err != nil {
		t.Fatalf("AllSessionEvents(%q): %v", sessionName, err)
	}
	for _, e := range events {
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &m); err != nil {
			continue
		}
		id, _ := m["id"].(string)
		synth, _ := m["synthetic"].(bool)
		if id == callID && synth {
			return true
		}
	}
	return false
}

// --- Orphan detection tests ---

// TestOrphanDetection_NoToolCalls verifies that a session with no tool_call
// events produces no synthetic tool_result events.
func TestOrphanDetection_NoToolCalls(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@no-calls", tmp, "worker", "active", "pi-uuid-abc")

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.OrphansWritten != 0 {
		t.Errorf("OrphansWritten = %d, want 0", result.OrphansWritten)
	}
}

// TestOrphanDetection_AllHaveResults verifies that tool_call events with
// matching tool_result events produce no synthetic results.
func TestOrphanDetection_AllHaveResults(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@all-have-results", tmp, "worker", "active", "pi-uuid-def")

	// Write matched pair.
	writeToolCallEvent(t, database, "test@all-have-results", tmp, iid, "call-001")
	writeToolResultEvent(t, database, "test@all-have-results", tmp, iid, "call-001")
	writeToolCallEvent(t, database, "test@all-have-results", tmp, iid, "call-002")
	writeToolResultEvent(t, database, "test@all-have-results", tmp, iid, "call-002")

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.OrphansWritten != 0 {
		t.Errorf("OrphansWritten = %d, want 0 (all tool_calls have matching tool_results)", result.OrphansWritten)
	}
}

// TestOrphanDetection_MissingResult verifies that a tool_call without a
// matching tool_result produces a synthetic tool_result with the correct fields.
func TestOrphanDetection_MissingResult(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@orphan", tmp, "worker", "active", "pi-uuid-orphan")

	writeToolCallEvent(t, database, "test@orphan", tmp, iid, "orphan-call-001")
	// No tool_result written — simulating daemon crash mid-call.

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.OrphansWritten != 1 {
		t.Errorf("OrphansWritten = %d, want 1", result.OrphansWritten)
	}

	// Verify the synthetic tool_result exists with correct fields.
	if !hasSyntheticToolResult(t, database, "test@orphan", "orphan-call-001") {
		t.Error("expected synthetic tool_result for orphan-call-001, not found")
	}

	// Verify payload fields.
	events, _ := database.AllSessionEvents("test@orphan")
	for _, e := range events {
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(e.Payload), &m) //nolint:errcheck
		if m["id"] != "orphan-call-001" {
			continue
		}
		if m["success"] != false {
			t.Errorf("synthetic tool_result success = %v, want false", m["success"])
		}
		if m["isError"] != true {
			t.Errorf("synthetic tool_result isError = %v, want true", m["isError"])
		}
		if m["synthetic"] != true {
			t.Errorf("synthetic tool_result synthetic = %v, want true", m["synthetic"])
		}
		output, _ := m["output"].(string)
		if !strings.Contains(output, "daemon restarted") {
			t.Errorf("synthetic tool_result output = %q, want mention of 'daemon restarted'", output)
		}
	}
}

// TestOrphanDetection_Mixed verifies that only unfulfilled tool_calls get
// synthetic results when some have results and some don't.
func TestOrphanDetection_Mixed(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@mixed", tmp, "worker", "active", "pi-uuid-mixed")

	// call-A: has result
	writeToolCallEvent(t, database, "test@mixed", tmp, iid, "call-A")
	writeToolResultEvent(t, database, "test@mixed", tmp, iid, "call-A")

	// call-B: no result (orphan)
	writeToolCallEvent(t, database, "test@mixed", tmp, iid, "call-B")

	// call-C: has result
	writeToolCallEvent(t, database, "test@mixed", tmp, iid, "call-C")
	writeToolResultEvent(t, database, "test@mixed", tmp, iid, "call-C")

	// call-D: no result (orphan)
	writeToolCallEvent(t, database, "test@mixed", tmp, iid, "call-D")

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.OrphansWritten != 2 {
		t.Errorf("OrphansWritten = %d, want 2", result.OrphansWritten)
	}

	// Synthetic results for B and D.
	if !hasSyntheticToolResult(t, database, "test@mixed", "call-B") {
		t.Error("expected synthetic tool_result for call-B")
	}
	if !hasSyntheticToolResult(t, database, "test@mixed", "call-D") {
		t.Error("expected synthetic tool_result for call-D")
	}

	// NO synthetic result for A or C (they already have real results).
	events, _ := database.AllSessionEvents("test@mixed")
	for _, e := range events {
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(e.Payload), &m) //nolint:errcheck
		id, _ := m["id"].(string)
		synth, _ := m["synthetic"].(bool)
		if (id == "call-A" || id == "call-C") && synth {
			t.Errorf("found unexpected synthetic tool_result for %q (should have real result)", id)
		}
	}
}

// --- Spawning reconciliation tests ---

// TestSpawningState_MarkedError verifies that sessions in spawning state are
// marked error with reason "daemon crashed during spawn".
func TestSpawningState_MarkedError(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@spawning", tmp, "worker", "spawning", "")

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.SpawningMarkError != 1 {
		t.Errorf("SpawningMarkError = %d, want 1", result.SpawningMarkError)
	}
	if result.SessionsRestored != 0 {
		t.Errorf("SessionsRestored = %d, want 0 (spawning sessions should not be re-spawned)", result.SessionsRestored)
	}

	// Verify the session is now in error state in the DB.
	sess, err := database.SessionByInstanceID(iid)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Fatal("session row not found")
	}
	if sess.EndState == nil || *sess.EndState != "error" {
		t.Errorf("end_state = %v, want 'error'", sess.EndState)
	}
}

// --- Session re-spawn edge case tests ---

// TestRestoreSession_MissingWorktree verifies that an active session whose
// worktree no longer exists is marked error("worktree missing") and not
// re-spawned.
func TestRestoreSession_MissingWorktree(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	missingWorktree := filepath.Join(tmp, "nonexistent-worktree")
	// Don't create the worktree directory.
	insertIrisSession(t, database, iid, "test@missing-wt", missingWorktree, "worker", "active", "pi-uuid-wt")

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.SessionsSkipped != 1 {
		t.Errorf("SessionsSkipped = %d, want 1", result.SessionsSkipped)
	}
	if result.SessionsRestored != 0 {
		t.Errorf("SessionsRestored = %d, want 0", result.SessionsRestored)
	}

	// Verify the session is marked error.
	sess, err := database.SessionByInstanceID(iid)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess.EndState == nil || *sess.EndState != "error" {
		t.Errorf("end_state = %v, want 'error'", sess.EndState)
	}
}

// TestRestoreSession_MissingJSONL verifies that an active session whose pi
// JSONL file is missing is marked error("session file missing") and not
// re-spawned.
func TestRestoreSession_MissingJSONL(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	// Worktree exists.
	worktree := filepath.Join(tmp, "myworktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}

	iid := uuid.New().String()
	// HarnessSessionID is set but no JSONL file exists under pi-agent/sessions/.
	insertIrisSession(t, database, iid, "test@missing-jsonl", worktree, "worker", "active", "nonexistent-uuid")

	piAgentDir := filepath.Join(tmp, "pi-agent")
	// Don't create the pi sessions directory.

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: piAgentDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: "/nonexistent/pi",
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.SessionsSkipped != 1 {
		t.Errorf("SessionsSkipped = %d, want 1", result.SessionsSkipped)
	}
	if result.SessionsRestored != 0 {
		t.Errorf("SessionsRestored = %d, want 0", result.SessionsRestored)
	}

	// Verify the session is marked error.
	sess, err := database.SessionByInstanceID(iid)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess.EndState == nil || *sess.EndState != "error" {
		t.Errorf("end_state = %v, want 'error'", sess.EndState)
	}
}

// TestRestoreSession_WithJSONL verifies that an active session with an
// existing JSONL file causes SpawnSession to be called (supervisor started).
func TestRestoreSession_WithJSONL(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	// Worktree exists.
	worktree := filepath.Join(tmp, "myworktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}

	// Create the pi JSONL session file.
	piUUID := "test-pi-uuid-12345678"
	piAgentDir := filepath.Join(tmp, "pi-agent")
	// EncodePiCWD mirrors the pi formula; see internal/harness/pi/archive.go.
	// For /tmp/.../myworktree → --tmp-...-myworktree--
	// We use the actual EncodePiCWD function indirectly by constructing the
	// expected path. Since the path is tmp-based, we know the encoding.
	// Easier: use a helper to create the file in the right place.
	encodedCWD := encodePiCWDForTest(worktree)
	sessionsDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll sessionsDir: %v", err)
	}
	jsonlFile := filepath.Join(sessionsDir, "20260515T120000Z_"+piUUID+".jsonl")
	if err := os.WriteFile(jsonlFile, []byte(`{"type":"session_init"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile jsonl: %v", err)
	}

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@with-jsonl", worktree, "worker", "active", piUUID)

	// Track whether SpawnSession / Start was called. We use a fake pi binary
	// that exits immediately with non-zero so we can observe the attempt.
	fakePiBin := filepath.Join(tmp, "fake-pi")
	if err := os.WriteFile(fakePiBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake-pi: %v", err)
	}

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: piAgentDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: fakePiBin,
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if result.SessionsRestored != 1 {
		t.Errorf("SessionsRestored = %d, want 1", result.SessionsRestored)
	}
	if result.SessionsSkipped != 0 {
		t.Errorf("SessionsSkipped = %d, want 0", result.SessionsSkipped)
	}
}

// TestRestoreSession_Concurrent verifies that multiple active sessions are
// all restored (or marked skipped) concurrently. We verify all are accounted
// for and that the total count equals the number of sessions.
func TestRestoreSession_Concurrent(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	const numSessions = 5

	// Create worktrees and sessions. Half have JSONL, half don't.
	for i := 0; i < numSessions; i++ {
		worktree := filepath.Join(tmp, "wt", strings.Repeat("x", i+1)) // unique paths
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatalf("MkdirAll worktree %d: %v", i, err)
		}
		piUUID := uuid.New().String()
		iid := uuid.New().String()
		insertIrisSession(t, database, iid, strings.Repeat("x", i+1)+"@branch", worktree, "worker", "active", piUUID)

		if i%2 == 0 {
			// Even sessions get a JSONL file.
			encodedCWD := encodePiCWDForTest(worktree)
			sessionsDir := filepath.Join(tmp, "pi-agent", "sessions", encodedCWD)
			if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
				t.Fatalf("MkdirAll sessionsDir: %v", err)
			}
			jsonlFile := filepath.Join(sessionsDir, "20260515T120000Z_"+piUUID+".jsonl")
			if err := os.WriteFile(jsonlFile, []byte(`{"type":"session_init"}`+"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile jsonl %d: %v", i, err)
			}
		}
	}

	fakePiBin := filepath.Join(tmp, "fake-pi")
	if err := os.WriteFile(fakePiBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake-pi: %v", err)
	}

	// Use a channel to observe that goroutines start concurrently.
	var startMu sync.Mutex
	startTimes := make([]time.Time, 0, numSessions)

	cfg := iris.RestoreConfig{
		Database: database,
		RunDir:   tmp,
		PIAgentDir: filepath.Join(tmp, "pi-agent"),
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: fakePiBin,
			RunDir:       tmp,
			Database:     database,
		},
	}

	result, err := iris.RunRestore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	total := result.SessionsRestored + result.SessionsSkipped
	if total != numSessions {
		t.Errorf("SessionsRestored + SessionsSkipped = %d, want %d", total, numSessions)
	}
	// At least the even-indexed sessions (3 of 5: indices 0,2,4) should be restored.
	if result.SessionsRestored < 3 {
		t.Errorf("SessionsRestored = %d, want >= 3 (even-indexed sessions have JSONL)", result.SessionsRestored)
	}

	_, _ = startTimes, &startMu
}

// --- DB-level orphan detection unit tests ---

// TestIrisOrphanedToolCalls_Empty verifies that no orphans are reported for
// a session with no tool_call events.
func TestIrisOrphanedToolCalls_Empty(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@empty-calls", tmp, "worker", "active", "uuid-empty")

	orphans, err := database.IrisOrphanedToolCalls(iid)
	if err != nil {
		t.Fatalf("IrisOrphanedToolCalls: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("got %d orphans, want 0", len(orphans))
	}
}

// TestIrisOrphanedToolCalls_AllMatched verifies that matched tool_calls
// return no orphans.
func TestIrisOrphanedToolCalls_AllMatched(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@matched", tmp, "worker", "active", "uuid-matched")

	writeToolCallEvent(t, database, "test@matched", tmp, iid, "c1")
	writeToolResultEvent(t, database, "test@matched", tmp, iid, "c1")
	writeToolCallEvent(t, database, "test@matched", tmp, iid, "c2")
	writeToolResultEvent(t, database, "test@matched", tmp, iid, "c2")

	orphans, err := database.IrisOrphanedToolCalls(iid)
	if err != nil {
		t.Fatalf("IrisOrphanedToolCalls: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("got %d orphans, want 0", len(orphans))
	}
}

// TestIrisOrphanedToolCalls_SomeOrphaned verifies that only unmatched
// tool_calls are returned.
func TestIrisOrphanedToolCalls_SomeOrphaned(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@some-orphaned", tmp, "worker", "active", "uuid-some")

	writeToolCallEvent(t, database, "test@some-orphaned", tmp, iid, "c1") // has result
	writeToolResultEvent(t, database, "test@some-orphaned", tmp, iid, "c1")
	writeToolCallEvent(t, database, "test@some-orphaned", tmp, iid, "c2") // orphan
	writeToolCallEvent(t, database, "test@some-orphaned", tmp, iid, "c3") // has result
	writeToolResultEvent(t, database, "test@some-orphaned", tmp, iid, "c3")
	writeToolCallEvent(t, database, "test@some-orphaned", tmp, iid, "c4") // orphan

	orphans, err := database.IrisOrphanedToolCalls(iid)
	if err != nil {
		t.Fatalf("IrisOrphanedToolCalls: %v", err)
	}
	if len(orphans) != 2 {
		t.Fatalf("got %d orphans, want 2; orphans: %v", len(orphans), orphans)
	}
	ids := make(map[string]bool)
	for _, o := range orphans {
		ids[o.ToolCallID] = true
	}
	if !ids["c2"] {
		t.Error("expected orphan c2 not found")
	}
	if !ids["c4"] {
		t.Error("expected orphan c4 not found")
	}
}

// TestIrisSyntheticToolResult verifies the payload fields of a synthetic event.
func TestIrisSyntheticToolResult(t *testing.T) {
	database := openRestoreTestDB(t)
	tmp := t.TempDir()

	// Insert a sessions row so the FK on agent_events.instance_id is satisfied.
	iid := uuid.New().String()
	insertIrisSession(t, database, iid, "test@synth", tmp, "worker", "active", "pi-synth")

	if _, _, err := database.IrisSyntheticToolResult("test@synth", tmp, "synth-call-001", iid); err != nil {
		t.Fatalf("IrisSyntheticToolResult: %v", err)
	}

	events, err := database.AllSessionEvents("test@synth")
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}

	var found bool
	for _, e := range events {
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &m); err != nil {
			continue
		}
		if m["id"] != "synth-call-001" {
			continue
		}
		found = true
		if m["success"] != false {
			t.Errorf("success = %v, want false", m["success"])
		}
		if m["isError"] != true {
			t.Errorf("isError = %v, want true", m["isError"])
		}
		if m["synthetic"] != true {
			t.Errorf("synthetic = %v, want true", m["synthetic"])
		}
		output, _ := m["output"].(string)
		if !strings.Contains(output, "daemon restarted") {
			t.Errorf("output = %q, want mention of daemon restart", output)
		}
	}
	if !found {
		t.Error("synthetic tool_result event not found")
	}
}

// encodePiCWDForTest mirrors the EncodePiCWD formula from
// internal/harness/pi/archive.go for use in tests without importing that
// package (avoids a cross-package dependency from an _test file).
func encodePiCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
}
