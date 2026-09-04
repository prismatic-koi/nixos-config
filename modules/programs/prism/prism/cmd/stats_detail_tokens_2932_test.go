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

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// writeStateChange emits a state_change event tied to instance_id — the
// signal ComputeSpawnOutcome measures the session's duration against.
func writeStateChange(t *testing.T, d *db.DB, sessionName, instanceID, state string, at time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          "sc-" + state + "-" + at.Format("150405.000"),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &instanceID,
		Type:        "state_change",
		Payload:     `{"state":"` + state + `"}`,
		CreatedAt:   at,
	}); err != nil {
		t.Fatalf("WriteEvent (state_change): %v", err)
	}
}

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

// TestRenderIncarnationDetail_DurationAndStateFromTerminalTransition covers
// the two lines the issue's own evidence came from. `prism stats <session>`
// reported "28m 24s" before cleanup and "44m 56s" after, because the duration
// line measured to sessions.ended_at, which cleanup stamps. Both lines now
// read the outcome, so this surface and `prism stats compare` report one
// duration and one state for one session.
func TestRenderIncarnationDetail_DurationAndStateFromTerminalTransition(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-45 * time.Minute)

	const sessionName = "repo@2932-detail-duration"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Minute), 100, 50, 0, 0, 0.01)
	writeStateChange(t, d, sessionName, iid, "finished", startedAt.Add(28*time.Minute+24*time.Second))

	// seedCompareSession already stamped sessions.ended_at at ~now, standing in
	// for a cleanup that ran 17 minutes after the session actually finished.
	sess, _ := d.SessionByInstanceID(iid)
	if sess.EndedAt == nil {
		t.Fatal("fixture: sessions.ended_at must be stamped to reproduce the inflated reading")
	}

	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })

	if !strings.Contains(out, "duration: 28m 24s") {
		t.Errorf("duration must measure to the terminal transition (28m 24s):\n%s", out)
	}
	if strings.Contains(out, "duration: 45m") || strings.Contains(out, "duration: 44m") {
		t.Errorf("duration still measured to sessions.ended_at:\n%s", out)
	}

}

// TestRenderIncarnationDetail_StateBeforeCleanup covers the sibling line in
// the same block. sessions.end_state is a cleanup write too, so a session
// that has finished but has not been cleaned up carries NULL there and this
// surface reported it as "active" — while `prism stats compare` reported
// "finished" for the same session.
//
// The fixture is hand-built rather than seedCompareSession, which stamps
// sessions.end_state for a terminal state and so cannot express the
// pre-cleanup shape.
func TestRenderIncarnationDetail_StateBeforeCleanup(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-10 * time.Minute)

	const sessionName = "repo@2932-detail-state"
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Minute), 100, 50, 0, 0, 0.01)
	writeStateChange(t, d, sessionName, iid, "finished", startedAt.Add(5*time.Minute))

	sess, _ := d.SessionByInstanceID(iid)
	if sess.EndState != nil || sess.EndedAt != nil {
		t.Fatalf("fixture: sessions row must be un-stamped, got end_state=%v ended_at=%v",
			sess.EndState, sess.EndedAt)
	}

	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })
	if !strings.Contains(out, "state: finished") {
		t.Errorf("state line must report the terminal state before cleanup:\n%s", out)
	}
	if !strings.Contains(out, "duration: 5m") {
		t.Errorf("duration must measure to the terminal transition:\n%s", out)
	}
}

// TestRenderIncarnationDetail_LiveSessionDuration is the negative test for
// the same block: a session that is still running has no terminal transition
// and no persisted duration, so the line must keep measuring to now and no
// ended line may appear.
func TestRenderIncarnationDetail_LiveSessionDuration(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-3 * time.Minute)

	const sessionName = "repo@2932-detail-live"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateActive, nil)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Minute), 100, 50, 0, 0, 0.01)

	sess, _ := d.SessionByInstanceID(iid)
	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })

	if strings.Contains(out, "ended:") {
		t.Errorf("live session must not print an ended line:\n%s", out)
	}
	if !strings.Contains(out, "duration: 3m") {
		t.Errorf("live session duration must measure to now:\n%s", out)
	}
	if !strings.Contains(out, "state: active") {
		t.Errorf("live session state must read active:\n%s", out)
	}
}

// TestRenderStatsIncarnations_StateAndDurationBeforeCleanup covers the
// listing table one level up from the detail view. It read sessions.end_state
// and sessions.ended_at directly, so `prism stats` showed STATE active with a
// still-growing duration for the same session `prism stats <session>` reported
// as finished.
//
// The proxy twin (renderStatsIncarnationsFromSessions) keeps the old
// semantics: it has no DB handle, and carrying a resolved state across the
// host-API boundary changes the /stats?view=summary wire shape. Tracked in
// issue #2935.
func TestRenderStatsIncarnations_StateAndDurationBeforeCleanup(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-40 * time.Minute)

	const sessionName = "repo@2932-table-state"
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Minute), 100, 50, 0, 0, 0.01)
	writeStateChange(t, d, sessionName, iid, "finished", startedAt.Add(12*time.Minute))

	sessions, err := d.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	out := captureStdout(t, func() {
		if rerr := renderIncarnationsWithDB(d, sessions); rerr != nil {
			t.Fatalf("renderIncarnationsWithDB: %v", rerr)
		}
	})

	if !strings.Contains(out, "finished") {
		t.Errorf("STATE column must report the terminal state before cleanup:\n%s", out)
	}
	if !strings.Contains(out, "12m 0s") {
		t.Errorf("DURATION column must measure to the terminal transition:\n%s", out)
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
