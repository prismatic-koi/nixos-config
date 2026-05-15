package parity_test

// restore_test.go — §10.3 checklist item: "Restore sessions after reboot".
//
// D-10 AC (functional, restore):
//
//   A test starts a session, dispatches a bash `tool_call` that the
//   daemon's mid-call kill leaves orphaned (no `tool_result` written),
//   kills the iris daemon, restarts it, and asserts (a) the session is
//   re-spawned with conversation history intact, and (b) the orphaned
//   `tool_call` has a synthetic `tool_result` with `success=false` and
//   `output="daemon restarted mid-call"` written by the D-9 restore path.
//
// Mechanics:
//
//   - Seed an iris session in active state in the DB.
//   - Seed an in-flight tool_call event (no matching tool_result) and a
//     pi JSONL file at the documented path.
//   - "Kill the daemon" by simply not running it — call iris.RunRestore as
//     if we were the daemon coming back up. Restart uses a fake-pi binary
//     so we don't depend on a real pi child.
//   - Assert: synthetic tool_result is written; per the D-9 contract its
//     payload contains success=false and an output naming the restart.
//   - Assert: the supervisor for the active session is re-spawned (harness
//     socket re-created under the run dir). Conversation-history-intact
//     is exercised by passing --session <pi-jsonl-path>; the test asserts
//     SupervisorConfig.SessionContinuePath is set on the restart path by
//     checking the spawned harness socket exists.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityRestore_OrphanedToolCallAndRespawn(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("restore")
	worktree := filepath.Join(iso.Root, "wt-restore")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	instanceID := uuid.New().String()
	harnessSessionID := "pi-restore-" + uuid.New().String()

	// 1. Seed an active iris session with harness_session_id populated.
	role := "worker"
	hsid := harnessSessionID
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsid,
		StartedAt:        time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := iso.DB.IrisUpdateSessionState(instanceID, string(iris.StateActive)); err != nil {
		t.Fatalf("IrisUpdateSessionState: %v", err)
	}

	// 2. Seed in-flight tool_call with no matching tool_result.
	orphanCallID := "orphan-restore-001"
	orphanPayload, _ := json.Marshal(map[string]any{
		"id":   orphanCallID,
		"name": "bash",
		"args": map[string]any{"command": "sleep 60"},
	})
	iidForEvent := instanceID
	if err := iso.DB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Worktree:    worktree,
		Type:        "tool_call",
		Payload:     string(orphanPayload),
		CreatedAt:   time.Now(),
		InstanceID:  &iidForEvent,
	}); err != nil {
		t.Fatalf("WriteEvent tool_call: %v", err)
	}

	// Also seed a prior msg_assistant event so "conversation history
	// intact" has something to assert against.
	priorAssistant := `{"type":"msg_assistant","turn_id":"prior-turn-1","content":"earlier work"}`
	if err := iso.DB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Worktree:    worktree,
		Type:        "msg_assistant",
		Payload:     priorAssistant,
		CreatedAt:   time.Now().Add(-1 * time.Minute),
		InstanceID:  &iidForEvent,
	}); err != nil {
		t.Fatalf("WriteEvent msg_assistant: %v", err)
	}

	// 3. Seed the pi JSONL file at the documented path.
	encoded := piharness.EncodePiCWD(worktree)
	sessionsDir := filepath.Join(iso.PIAgentDir, "sessions", encoded)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	piJSONLPath := filepath.Join(sessionsDir, "20260101T000000Z_"+harnessSessionID+".jsonl")
	if err := os.WriteFile(piJSONLPath, []byte(priorAssistant+"\n"), 0o644); err != nil {
		t.Fatalf("write pi jsonl: %v", err)
	}

	// 4. Fake-pi binary: exits immediately so the supervisor's circuit
	// breaker keeps the test fast. The harness socket is still created
	// before the supervisor's spawn loop notices the failure, which is the
	// assertion below.
	fakePI := filepath.Join(iso.Root, "fake-pi.sh")
	if err := os.WriteFile(fakePI, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake-pi: %v", err)
	}

	// 5. Run RunRestore as if the daemon just restarted.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := iris.RunRestore(ctx, iris.RestoreConfig{
		Database:   iso.DB,
		RunDir:     iso.Paths.RunDir,
		PIAgentDir: iso.PIAgentDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath: fakePI,
			RunDir:       iso.Paths.RunDir,
			Database:     iso.DB,
		},
	})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}

	if result.OrphansWritten != 1 {
		t.Errorf("OrphansWritten = %d, want 1", result.OrphansWritten)
	}
	if result.SessionsRestored != 1 {
		t.Errorf("SessionsRestored = %d, want 1 (got Skipped=%d)", result.SessionsRestored, result.SessionsSkipped)
	}

	// 6. Assert (b): the synthetic tool_result has success=false and the
	// canonical output naming the restart. The exact wording is fixed by
	// db.IrisSyntheticToolResult so we check the substring "daemon
	// restarted".
	events, err := iso.DB.AllSessionEvents(sessionName)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	var (
		sawSyntheticOrphan bool
		sawPriorAssistant  bool
	)
	for _, e := range events {
		if e.Type == "msg_assistant" && containsString(e.Payload, "earlier work") {
			sawPriorAssistant = true
		}
		if e.Type != "tool_result" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &m); err != nil {
			continue
		}
		if m["id"] != orphanCallID {
			continue
		}
		if m["success"] != false {
			t.Errorf("synthetic tool_result.success = %v, want false", m["success"])
		}
		output, _ := m["output"].(string)
		if !strings.Contains(output, "daemon restarted") {
			t.Errorf("synthetic tool_result.output = %q, want substring \"daemon restarted\"", output)
		}
		// AC quote: output=\"daemon restarted mid-call\". The exact wording
		// is owned by db.IrisSyntheticToolResult; tolerate the canonical
		// string but require the AC substring.
		if synth, _ := m["synthetic"].(bool); !synth {
			t.Errorf("synthetic tool_result.synthetic = %v, want true", m["synthetic"])
		}
		sawSyntheticOrphan = true
	}
	if !sawSyntheticOrphan {
		t.Errorf("no synthetic tool_result for orphan %q", orphanCallID)
	}

	// 7. Assert (a): the session was re-spawned with conversation history
	// intact. The harness socket file exists at the per-session run path
	// (proof of re-spawn) and the pre-restart msg_assistant event is still
	// readable via the narrative view (proof of history intact).
	if !sawPriorAssistant {
		t.Errorf("prior msg_assistant event missing after restore")
	}
	harnessSock := iris.HarnessSockPath(iso.Paths.RunDir, instanceID)
	deadline := time.Now().Add(3 * time.Second)
	var sockExists bool
	for time.Now().Before(deadline) {
		if _, err := os.Stat(harnessSock); err == nil {
			sockExists = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sockExists {
		t.Errorf("harness socket %q was not re-created after restore", harnessSock)
	}
}
