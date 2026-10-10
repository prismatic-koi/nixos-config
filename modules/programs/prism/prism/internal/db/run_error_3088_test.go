package db_test

// run_error_3088_test.go — issue #3088. Both read paths of the review report
// must carry the run_error reason: GroupResults for a live member, and
// SessionEndCauses for a member whose row was closed.

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

const runErrorReason = `the model ended its turn with no text and no tool call (stop reason \"length\")`

func writeRunErrorEvent(t *testing.T, d *db.DB, session string) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          "evt-run-err-" + session,
		SessionName: session,
		Repo:        "nixos-config",
		Worktree:    "/wt",
		Type:        "run_error",
		Payload:     `{"type":"run_error","stop_reason":"length","reason":"` + runErrorReason + `"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent run_error: %v", err)
	}
}

const wantRunErrorReason = `the model ended its turn with no text and no tool call (stop reason "length")`

func TestGroupResults_RunError(t *testing.T) {
	d := openTestDB(t)
	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	session := "nixos-config@feature~review-1-review-security"
	if err := d.UpsertStatus(session, "nixos-config", "/wt", "error", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
		groupID, session,
	).Scan(new(int)); err != nil {
		t.Fatalf("set group_id: %v", err)
	}
	writeRunErrorEvent(t, d, session)

	results, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	r, ok := results[session]
	if !ok {
		t.Fatalf("GroupResults: missing entry for %q", session)
	}
	if r.RunError != wantRunErrorReason {
		t.Errorf("RunError = %q, want %q", r.RunError, wantRunErrorReason)
	}
	if r.StallError != "" || r.StartupError != "" {
		t.Errorf("StallError = %q, StartupError = %q, want both empty", r.StallError, r.StartupError)
	}
}

func TestSessionEndCauses_RunError(t *testing.T) {
	d := openTestDB(t)
	session := "nixos-config@feature~review-1-review-security"
	writeRunErrorEvent(t, d, session)

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	c, ok := causes[session]
	if !ok {
		t.Fatalf("SessionEndCauses: no entry for %q — a run_error alone must count as a recorded cause", session)
	}
	if c.RunError != wantRunErrorReason {
		t.Errorf("RunError = %q, want %q", c.RunError, wantRunErrorReason)
	}
}
