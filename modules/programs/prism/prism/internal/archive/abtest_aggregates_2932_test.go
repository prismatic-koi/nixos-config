package archive_test

// `prism checkin --compare` is the second pre-merge A/B comparison surface,
// alongside `prism stats compare`. It reads the outcome for each leg through
// archive.LoadAbtestPair.
//
// LoadAbtestPair used to read the persisted spawn_outcome row directly, so it
// carried the whole of issue #2932: for two finished, not-yet-cleaned-up legs
// it printed `state=—  turns=0  in=0  out=0  dur=—`, which is the window this
// command exists to be used in.

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/db"
)

// finishSession marks a session terminal the way the sidecar does — an
// agent_status state and a state_change event — and writes one token-bearing
// assistant turn. It deliberately does NOT stamp sessions.ended_at or
// end_state: those are cleanup's writes, and the point is the window before
// cleanup runs.
func finishSession(t *testing.T, d *db.DB, sessionName, iid string, startedAt time.Time, outputTokens int, cost float64) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	payload := fmt.Sprintf(
		`{"text":"done","inputTokens":100,"outputTokens":%d,"cacheReadTokens":0,"cacheWriteTokens":0,"cost":%v}`,
		outputTokens, cost)
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &iid,
		Type:        "msg_assistant",
		Payload:     payload,
		CreatedAt:   startedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("WriteEvent (msg_assistant): %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &iid,
		Type:        "state_change",
		Payload:     `{"state":"finished"}`,
		CreatedAt:   startedAt.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteEvent (state_change): %v", err)
	}
}

// TestLoadAbtestPair_ReportsAggregatesBeforeCleanup covers both legs of the
// A/B pair in the state the comparison is actually made in: finished,
// reviewed, and not yet cleaned up. The review-complete write leaves a stub
// spawn_outcome row on each, which must not suppress the aggregates.
func TestLoadAbtestPair_ReportsAggregatesBeforeCleanup(t *testing.T) {
	d := openTestDBForArchive(t)
	const pairID = "pair-2932"
	startedAt := time.Now().Add(-30 * time.Minute)

	iidA := uuid.New().String()
	iidB := uuid.New().String()
	insertMinimalSession(t, d, "repo@2932-leg-a", iidA, pairID, "max", startedAt)
	insertMinimalSession(t, d, "repo@2932-leg-b", iidB, pairID, "standard", startedAt.Add(time.Second))
	finishSession(t, d, "repo@2932-leg-a", iidA, startedAt, 17024, 2.125)
	finishSession(t, d, "repo@2932-leg-b", iidB, startedAt.Add(time.Second), 9803, 1.061)

	// Both legs completed a review before anyone ran cleanup.
	if err := d.UpdateSpawnOutcomeReviewResult(iidA, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult(A): %v", err)
	}
	if err := d.UpdateSpawnOutcomeReviewResult(iidB, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult(B): %v", err)
	}

	pair, err := archive.LoadAbtestPair(d, pairID)
	if err != nil {
		t.Fatalf("LoadAbtestPair: %v", err)
	}
	if pair.OutcomeA == nil || pair.OutcomeB == nil {
		t.Fatalf("OutcomeA=%v OutcomeB=%v — both legs are terminal and must carry an outcome",
			pair.OutcomeA, pair.OutcomeB)
	}
	if pair.OutcomeA.TokensOutputTotal != 17024 {
		t.Errorf("OutcomeA.TokensOutputTotal = %d, want 17024", pair.OutcomeA.TokensOutputTotal)
	}
	if pair.OutcomeB.TokensOutputTotal != 9803 {
		t.Errorf("OutcomeB.TokensOutputTotal = %d, want 9803", pair.OutcomeB.TokensOutputTotal)
	}
	// end_state and duration drive the `state=` and `dur=` columns of
	// printAbtestSessionMetrics, both of which rendered "—" before the fix.
	if pair.OutcomeA.EndState == nil || *pair.OutcomeA.EndState != "finished" {
		t.Errorf("OutcomeA.EndState = %v, want \"finished\"", pair.OutcomeA.EndState)
	}
	if pair.OutcomeA.DurationMs == nil {
		t.Error("OutcomeA.DurationMs = nil, want the start→terminal-transition interval")
	}
	if pair.OutcomeA.MsgAssistantCount != 1 {
		t.Errorf("OutcomeA.MsgAssistantCount = %d, want 1", pair.OutcomeA.MsgAssistantCount)
	}
}

// TestLoadAbtestPair_LiveSessionHasNoOutcome is the over-broad-fix negative
// test: a leg that is still running must keep a nil outcome so the renderer
// prints "(no metrics)" rather than a half-finished total presented as final.
func TestLoadAbtestPair_LiveSessionHasNoOutcome(t *testing.T) {
	d := openTestDBForArchive(t)
	const pairID = "pair-2932-live"
	startedAt := time.Now().Add(-5 * time.Minute)

	iid := uuid.New().String()
	insertMinimalSession(t, d, "repo@2932-live", iid, pairID, "max", startedAt)
	if err := d.SetInstanceID("repo@2932-live", iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@2932-live",
		Repo:        "repo",
		Worktree:    "/wt/repo@2932-live",
		InstanceID:  &iid,
		Type:        "msg_assistant",
		Payload:     `{"text":"working","inputTokens":10,"outputTokens":20,"cost":0.01}`,
		CreatedAt:   startedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	pair, err := archive.LoadAbtestPair(d, pairID)
	if err != nil {
		t.Fatalf("LoadAbtestPair: %v", err)
	}
	if pair.OutcomeA != nil {
		t.Errorf("OutcomeA = %+v for a live session, want nil", pair.OutcomeA)
	}
}
