package cmd

// `prism stats <session>` must report token counts and cost for a finished
// session before `prism cleanup` runs (issue #2932).
//
// The detail renderer reads db.CompareRunOutcome, the same helper
// `prism stats compare` reads. When a partial spawn_outcome row existed —
// written at PR-create or review-complete time, long before cleanup — that
// helper returned the stub, and the block printed "no token data" for a
// session whose events carried both tokens and cost.

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

func TestRenderIncarnationDetail_TokensBeforeCleanup(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	const sessionName = "repo@2932-detail"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)

	// The review-complete write creates the row that used to shadow the
	// aggregation.
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	sess, err := d.SessionByInstanceID(iid)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID = (%v, %v)", sess, err)
	}

	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })

	if strings.Contains(out, "no token data") {
		t.Errorf("detail view reports \"no token data\" for a session with token-bearing events:\n%s", out)
	}
	for _, want := range []string{"input:", "output:", "est. cost:"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail view missing %q:\n%s", want, out)
		}
	}
}

// TestRenderIncarnationDetail_NoTokenFields is the edge-case AC: a finished
// session whose events carry no token fields prints the explicit no-data
// marker rather than a fabricated zero row, and does not error.
func TestRenderIncarnationDetail_NoTokenFields(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@2932-detail-empty"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)
	if err := d.WriteEvent(db.Event{
		ID:          "evt-2932-detail-empty",
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &iid,
		Type:        "msg_assistant",
		Payload:     `{"text":"no usage object here"}`,
		CreatedAt:   startedAt.Add(10 * time.Second),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })

	if !strings.Contains(out, "no token data") {
		t.Errorf("token-less session must print the no-data marker:\n%s", out)
	}
}
