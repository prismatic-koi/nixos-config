package db_test

// Tests for msg_assistant payload decoding in groupResults.
//
// groupResults must store the DECODED text of the msg_assistant JSON payload
// as LastMessage, not the raw payload. encoding/json escapes '<' and '>' as
// \u003c / \u003e, so a raw stored verdict block reads as
// \u003cverdict\u003ePASS\u003c/verdict\u003e and the substring rule the
// dashboard and the roll-up apply could never match it.
//
// The fixture below is built by marshalling a real struct through
// encoding/json, so the escaping is exercised. A hand-written literal-'<'
// payload would pass even against the old raw-store code and prove nothing
// (the test-fidelity trap called out in the issue).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/verdict"
)

// escapedAssistantPayload marshals text into the {"text":…} envelope exactly
// as the sidecar's capture path does, so '<' becomes \u003c in the stored
// payload.
func escapedAssistantPayload(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(b); verdict.Parse(got) != verdict.None {
		// Guard the guard: if this envelope already matched, the fixture is
		// not exercising the escaping and the test proves nothing.
		t.Fatalf("fixture payload %q is not JSON-escaped; the marker is still visible in the raw envelope", got)
	}
	return string(b)
}

func TestGroupResults_DecodesMsgAssistantPayload(t *testing.T) {
	d := openTestDB(t)
	parent := "prism-test@decode-msg"

	groupID, err := d.RegisterGroupWithPR(parent, "2862", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	sess := parent + "~review-1-review-goal"
	if err := d.UpsertStatus(sess, "prism-test-repo", "/tmp/test-wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sess,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		Type:        "msg_assistant",
		Payload:     escapedAssistantPayload(t, "Reviewed.\n<verdict>PASS</verdict>"),
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	members, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	m, ok := members[sess]
	if !ok {
		t.Fatalf("member %q missing from GroupResults", sess)
	}
	want := "Reviewed.\n<verdict>PASS</verdict>"
	if m.LastMessage != want {
		t.Fatalf("LastMessage = %q, want the decoded text %q", m.LastMessage, want)
	}
	if verdict.Parse(m.LastMessage) != verdict.Pass {
		t.Errorf("verdict.Parse(LastMessage) = %v, want Pass", verdict.Parse(m.LastMessage))
	}
}

// TestGroupResults_DecodeFallsBackToRawOnNonEnvelope covers a payload that is
// not the {"text":…} envelope: the store must keep the raw payload rather than
// blank it, mirroring the startup_error / stall_error fallback.
func TestGroupResults_DecodeFallsBackToRawOnNonEnvelope(t *testing.T) {
	d := openTestDB(t)
	parent := "prism-test@decode-fallback"

	groupID, err := d.RegisterGroupWithPR(parent, "2862", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	sess := parent + "~review-1-review-code"
	if err := d.UpsertStatus(sess, "prism-test-repo", "/tmp/test-wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	// A bare string, not the {"text":…} envelope.
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sess,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		Type:        "msg_assistant",
		Payload:     "plain text with no envelope",
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	members, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	if got := members[sess].LastMessage; got != "plain text with no envelope" {
		t.Errorf("LastMessage = %q, want the raw payload preserved", got)
	}
}
