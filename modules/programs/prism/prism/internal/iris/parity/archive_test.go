package parity_test

// archive_test.go — §10.3 checklist item: "Archive sessions".
//
// D-10 AC (functional, archive):
//
//   A test runs a session, calls iris cleanup, and asserts the archive
//   directory contains the session JSONL file at the documented path.
//   Other artefacts (DB events, run-directory contents) are out of scope
//   for this AC.
//
// Documented path:
//   <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl
//
// The pi-side JSONL is sourced from <PIAgentDir>/sessions/<encoded-cwd>/<ts>_<UUID>.jsonl.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityArchive_SessionJSONLAtDocumentedPath(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("archive")
	worktree := filepath.Join(iso.Root, "worktree-archive")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	instanceID := "iris-test-archive-instance-001"
	harnessSessionID := "pi-archive-test-ULID-XYZ"

	// Seed sessions row with harness_session_id set so the archive step
	// can locate the pi JSONL file via the EncodePiCWD formula.
	role := "worker"
	hsidPtr := harnessSessionID
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsidPtr,
		StartedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Seed the pi JSONL file at the path the adapter expects.
	encoded := piharness.EncodePiCWD(worktree)
	sessionsDir := filepath.Join(iso.PIAgentDir, "sessions", encoded)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	jsonlPayload := []string{
		`{"type":"session_init","session_id":"` + harnessSessionID + `"}`,
		`{"type":"msg_assistant","turn_id":"t1","content":"hello from parity archive"}`,
	}
	piJSONL := filepath.Join(sessionsDir, "20260101T000000Z_"+harnessSessionID+".jsonl")
	var body []byte
	for _, line := range jsonlPayload {
		body = append(body, []byte(line+"\n")...)
	}
	if err := os.WriteFile(piJSONL, body, 0o644); err != nil {
		t.Fatalf("write pi jsonl: %v", err)
	}

	// Run iris CleanupSession (archive step is part of cleanup).
	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}

	wantArchive := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if res.ArchivePath != wantArchive {
		t.Errorf("ArchivePath = %q, want %q (errors=%v)", res.ArchivePath, wantArchive, res.Errors)
	}

	// File exists at the documented path.
	if _, err := os.Stat(wantArchive); err != nil {
		t.Fatalf("archive file missing at %q: %v", wantArchive, err)
	}

	// Content was copied verbatim from the pi JSONL.
	got, err := os.ReadFile(wantArchive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("archive content mismatch:\n got: %q\nwant: %q", got, body)
	}

	// Bonus: verify each line parses as JSON so a downstream consumer can
	// load the archive as a JSONL stream.
	for i, line := range jsonlPayload {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("archive line %d does not parse as JSON: %v", i, err)
		}
	}
}

// TestParityArchive_NoOpWhenNoJSONL verifies that an archive call on a
// session whose pi JSONL never existed (pi crashed before any frame was
// written) is a no-op — no error, no archive file. This is parity with
// prism's pi archive adapter which has the same contract.
func TestParityArchive_NoOpWhenNoJSONL(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName := iristest.SessionName("noop-archive")
	worktree := filepath.Join(iso.Root, "wt-noop")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	instanceID := "iris-test-noop-001"
	role := "worker"
	hsid := "pi-noop-ULID"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsid,
		StartedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if res.ArchivePath != "" {
		t.Errorf("ArchivePath = %q, want \"\" for the no-jsonl case", res.ArchivePath)
	}
}
