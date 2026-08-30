package dashboard

// review_verdict_closed_row_internal_test.go
//
// The dashboard attaches each review agent's last message via
// attachReviewLastMessages so the collapsed group row and the expanded child
// rows can render a verdict. That read must not use the narrow db.GroupResults,
// which drops rows whose ended_at is set: the 15-minute release closes every
// member of a delivered round, so a still-visible round would render every
// agent as pending once it aged out.
//
// This white-box test seeds a review member, closes it the way the release
// does, and asserts the verdict still renders. The msg_assistant payload is
// built by marshalling through encoding/json so the '<' escaping is exercised
// (the decode half) — a literal-'<' fixture would pass against raw-store code
// and prove nothing.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

func openDBForClosedRowTest(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func escapedEnvelope(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func TestAttachReviewLastMessages_ClosedRowStillRendersVerdict(t *testing.T) {
	d := openDBForClosedRowTest(t)
	parent := "prism-test@closed-row"

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
		Payload:     escapedEnvelope(t, "Reviewed.\n<verdict>PASS</verdict>"),
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	// Close the row exactly as the automatic release does.
	if err := d.SetEnded(sess); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	gid := groupID
	sessions := []AgentSession{{
		Name:       sess,
		AgentState: "finished",
		GroupID:    &gid,
	}}
	attachReviewLastMessages(d, sessions)

	if sessions[0].LastMessage == "" {
		t.Fatal("LastMessage empty after the release closed the row — the narrow read dropped it (#2862)")
	}
	if got := classifyVerdict(sessions[0].AgentState, sessions[0].LastMessage); got != VerdictPass {
		t.Errorf("classifyVerdict = %q, want %q for a closed row with a PASS verdict", got, VerdictPass)
	}
	// The expanded child row must show the verdict, not the prompt heading.
	if got := reviewChildVerdictLabel(classifyVerdict(sessions[0].AgentState, sessions[0].LastMessage)); got != "PASS" {
		t.Errorf("reviewChildVerdictLabel = %q, want %q", got, "PASS")
	}
}
