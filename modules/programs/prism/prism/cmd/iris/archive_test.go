package main

// archive_test.go — unit + integration tests for `iris archive` (#1697).
//
// These tests exercise:
//
//   - The cobra command's flag parsing (--instance-id, --all-json, mutually
//     exclusive arg vs --instance-id).
//   - The human and JSON output shapes via emitArchiveHuman / emitArchiveJSON.
//   - End-to-end invocation against an isolated DB through cobra's
//     SetArgs/Execute API so the wiring matches what a real CLI invocation
//     does.
//
// All tests use iristest.NewIsolated, which redirects HOME and XDG_STATE_HOME
// to a per-test tempdir so the homeless-shelter sandbox in CI stays happy.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// seedArchivableSession inserts a sessions row, writes the matching pi
// JSONL to disk under iso.PIAgentDir, and returns the names the caller
// needs for assertions.
func seedArchivableSession(t *testing.T, iso *iristest.Isolated, suffix string) (sessionName, instanceID string) {
	t.Helper()
	sessionName = iristest.SessionName(suffix)
	instanceID = "iris-test-cmd-archive-" + suffix
	hsid := "pi-" + suffix + "-ULID"
	worktree := filepath.Join(iso.Root, "wt-"+suffix)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	role := "worker"
	hsidPtr := hsid
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
	encoded := piharness.EncodePiCWD(worktree)
	sessionsDir := filepath.Join(iso.PIAgentDir, "sessions", encoded)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	body := []byte(`{"type":"session_init","session_id":"` + hsid + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(sessionsDir, "20260101T000000Z_"+hsid+".jsonl"), body, 0o644); err != nil {
		t.Fatalf("write pi jsonl: %v", err)
	}
	return sessionName, instanceID
}

// runCmd executes a fresh cobra Command tree with the given args and
// captures stdout. We rebuild the tree per test instead of mutating the
// package-level archiveCmd so the package-level flag globals are reset
// cleanly between invocations.
func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Reset package-level flag globals before each run. Cobra writes flag
	// values into these on Execute and they would otherwise leak between
	// tests sharing the package-level archiveCmd.
	archiveInstanceID = ""
	archivePIAgentDir = ""
	archiveAllJSON = false

	root := &cobra.Command{Use: "iris-test-root"}
	// Build a fresh archiveCmd that mirrors the production registration.
	c := &cobra.Command{
		Use:           archiveCmd.Use,
		RunE:          runArchive,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.Flags().StringVar(&archiveInstanceID, "instance-id", "", "")
	c.Flags().StringVar(&archivePIAgentDir, "pi-agent-dir", "", "")
	c.Flags().BoolVar(&archiveAllJSON, "all-json", false, "")
	root.AddCommand(c)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"archive"}, args...))
	err := root.ExecuteContext(context.Background())
	return buf.String(), err
}

// TestArchiveCmd_Human asserts the human-readable summary form prints the
// session name and the documented archive path.
func TestArchiveCmd_Human(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID := seedArchivableSession(t, iso, "human")

	out, err := runCmd(t, sessionName, "--pi-agent-dir", iso.PIAgentDir)
	if err != nil {
		t.Fatalf("runCmd: %v\n%s", err, out)
	}
	if !strings.Contains(out, sessionName) {
		t.Errorf("output missing session name %q:\n%s", sessionName, out)
	}
	wantPath := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if !strings.Contains(out, wantPath) {
		t.Errorf("output missing archive path %q:\n%s", wantPath, out)
	}
}

// TestArchiveCmd_AllJSON asserts the JSON form parses, has the expected
// fields, and points at the documented archive path.
func TestArchiveCmd_AllJSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID := seedArchivableSession(t, iso, "json")

	out, err := runCmd(t, sessionName, "--pi-agent-dir", iso.PIAgentDir, "--all-json")
	if err != nil {
		t.Fatalf("runCmd: %v\n%s", err, out)
	}
	var got archiveJSONOutput
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("parse JSON: %v\n%s", jerr, out)
	}
	if got.SessionName != sessionName {
		t.Errorf("SessionName = %q, want %q", got.SessionName, sessionName)
	}
	if got.InstanceID != instanceID {
		t.Errorf("InstanceID = %q, want %q", got.InstanceID, instanceID)
	}
	if got.Skipped {
		t.Errorf("Skipped = true on happy path: %q", got.SkipReason)
	}
	wantPath := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if got.ArchivePath == nil || *got.ArchivePath != wantPath {
		t.Errorf("ArchivePath = %v, want %q", got.ArchivePath, wantPath)
	}
}

// TestArchiveCmd_InstanceID asserts the --instance-id lookup variant works
// when no positional argument is given.
func TestArchiveCmd_InstanceID(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID := seedArchivableSession(t, iso, "id")

	out, err := runCmd(t, "--instance-id", instanceID, "--pi-agent-dir", iso.PIAgentDir, "--all-json")
	if err != nil {
		t.Fatalf("runCmd: %v\n%s", err, out)
	}
	var got archiveJSONOutput
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("parse JSON: %v\n%s", jerr, out)
	}
	if got.SessionName != sessionName {
		t.Errorf("SessionName = %q, want %q", got.SessionName, sessionName)
	}
	if got.InstanceID != instanceID {
		t.Errorf("InstanceID = %q, want %q", got.InstanceID, instanceID)
	}
}

// TestArchiveCmd_NoArgsNoFlag asserts that calling `iris archive` with no
// session and no --instance-id is a usage error.
func TestArchiveCmd_NoArgsNoFlag(t *testing.T) {
	iristest.NewIsolated(t)
	_, err := runCmd(t)
	if err == nil {
		t.Fatalf("expected error when called with no args and no --instance-id, got nil")
	}
	if !strings.Contains(err.Error(), "requires") {
		t.Errorf("error %q does not mention \"requires\"", err)
	}
}

// TestArchiveCmd_BothArgAndInstanceID asserts that supplying both a
// positional session and --instance-id is a usage error (intent is
// ambiguous).
func TestArchiveCmd_BothArgAndInstanceID(t *testing.T) {
	iristest.NewIsolated(t)
	_, err := runCmd(t, "some-name", "--instance-id", "some-uuid")
	if err == nil {
		t.Fatalf("expected error when called with both positional and --instance-id, got nil")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error %q does not mention \"not both\"", err)
	}
}

// TestArchiveCmd_EmptyJSONLExitsZero asserts that a session with no pi
// JSONL on disk exits 0 with an informative "(skipped — ...)" message and
// does NOT create an empty archive directory tree. This is the spec's
// "Empty JSONL → exit 0 with informative message, no empty archive" AC.
func TestArchiveCmd_EmptyJSONLExitsZero(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("cmd-empty")
	worktree := filepath.Join(iso.Root, "wt-cmd-empty")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	hsid := "pi-cmd-empty-ULID"
	role := "worker"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       "iris-test-cmd-empty",
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsid,
		StartedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	out, err := runCmd(t, sessionName, "--pi-agent-dir", iso.PIAgentDir)
	if err != nil {
		t.Fatalf("runCmd (empty jsonl) returned err: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("output should mention \"skipped\":\n%s", out)
	}
	wantNoDir := filepath.Join(iso.Paths.ArchiveRoot, sessionName)
	if _, statErr := os.Stat(wantNoDir); !os.IsNotExist(statErr) {
		t.Errorf("archive dir %q should not exist on empty-JSONL skip (stat err=%v)", wantNoDir, statErr)
	}
}

// TestArchiveCmd_DaemonDownStillWorks documents the "daemon-down but
// session in DB → still works" AC. The cobra command never dials the
// daemon socket — it reads the DB and copies a file. We assert that by
// running the command without any daemon process and without ever passing
// a socket path.
func TestArchiveCmd_DaemonDownStillWorks(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, _ := seedArchivableSession(t, iso, "no-daemon")

	// Sanity: the iris client socket path under iso must NOT exist; this
	// test would be vacuous if a stray daemon were running there.
	if _, err := os.Stat(iso.Paths.Sock); !os.IsNotExist(err) {
		t.Fatalf("precondition: iris.sock %q must not exist (err=%v)", iso.Paths.Sock, err)
	}

	out, err := runCmd(t, sessionName, "--pi-agent-dir", iso.PIAgentDir)
	if err != nil {
		t.Fatalf("runCmd (daemon down): %v\n%s", err, out)
	}
	if !strings.Contains(out, "session.jsonl") {
		t.Errorf("output missing session.jsonl path:\n%s", out)
	}
}

// TestArchiveCmd_SessionNotFound asserts a missing session name surfaces a
// non-zero exit (returned error) rather than silently exiting 0.
func TestArchiveCmd_SessionNotFound(t *testing.T) {
	iristest.NewIsolated(t)
	_, err := runCmd(t, iristest.SessionName("nope-not-real"))
	if err == nil {
		t.Fatalf("expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention \"not found\"", err)
	}
}

// TestArchiveCmd_SessionStaysRunning asserts the cobra path does not mark
// the session ended — same contract as TestArchiveSession_SessionRowUntouched
// but exercised through the user-facing entry point. Belt-and-braces guard
// against a future refactor that wires cleanup-style mutation into the
// archive command.
func TestArchiveCmd_SessionStaysRunning(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID := seedArchivableSession(t, iso, "running")

	if _, err := runCmd(t, sessionName, "--pi-agent-dir", iso.PIAgentDir); err != nil {
		t.Fatalf("runCmd: %v", err)
	}
	got, err := iso.DB.SessionByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatalf("session row vanished after archive")
	}
	if got.EndedAt != nil || got.EndState != nil {
		t.Errorf("session row mutated by archive: ended_at=%v end_state=%v", got.EndedAt, got.EndState)
	}
}
