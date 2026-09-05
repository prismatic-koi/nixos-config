package db_test

// Regression tests for issue #2932 — `prism stats` and `prism stats compare`
// reported no token, cost, or duration data for finished sessions whose
// agent_events carried all three.
//
// Two defects, both on the read path between ComputeSpawnOutcome and its
// callers:
//
//  1. CompareRunOutcome returned the persisted spawn_outcome row whenever one
//     existed. Three partial writers create that row before `prism cleanup`
//     computes anything, so a review verdict or a captured PR number was
//     enough to shadow the live aggregation with a row of zeros.
//
//  2. duration_ms and time_to_finished_ms were measured against
//     sessions.ended_at, which is stamped by `prism cleanup`, not by the
//     terminal-state transition. Every persisted duration carried the idle
//     gap before cleanup, and two sessions cleaned up in one loop reported
//     near-identical durations whatever their real times.

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedTerminalSession inserts the agent_status + sessions rows for a session
// that has reached a terminal state, WITHOUT stamping sessions.ended_at /
// end_state. That is the real pre-cleanup shape: the sidecar writes the
// terminal state to agent_status and emits a state_change event, and nothing
// touches the sessions row until `prism cleanup` runs.
func seedTerminalSession(t *testing.T, d *db.DB, sessionName string, startedAt time.Time) string {
	t.Helper()
	instanceID := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	return instanceID
}

// writeEventAt writes one agent_events row for instanceID at the given time.
func writeEventAt(t *testing.T, d *db.DB, sessionName, instanceID, eventType, payload string, at time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &instanceID,
		Type:        eventType,
		Payload:     payload,
		CreatedAt:   at,
	}); err != nil {
		t.Fatalf("WriteEvent(%s): %v", eventType, err)
	}
}

// assistantPayload builds a msg_assistant payload with the five fields both
// ComputeSpawnOutcome and the Grafana exporter read.
func assistantPayload(input, output, cacheRead, cacheWrite int, cost float64) string {
	return fmt.Sprintf(
		`{"text":"hi","model":"anthropic/test","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"cost":%v}`,
		input, output, cacheRead, cacheWrite, cost)
}

// TestCompareRunOutcome_StubRowDoesNotMaskAggregates is the primary
// regression test for issue #2932. A review-complete write creates a
// spawn_outcome row carrying only the verdict columns; the token, cost, and
// tool aggregates must still be reported from agent_events.
func TestCompareRunOutcome_StubRowDoesNotMaskAggregates(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-stub"
	started := time.Now().Add(-30 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(1200, 9000, 4000, 100, 1.25), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(800, 8024, 1000, 50, 0.875), started.Add(2*time.Minute))
	writeEventAt(t, d, sess, iid, "tool_call", `{"tool":"bash"}`, started.Add(3*time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(4*time.Minute))

	// The review-complete handler fires long before cleanup and creates the
	// row that used to shadow everything above.
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	sess1, err := d.SessionByInstanceID(iid)
	if err != nil || sess1 == nil {
		t.Fatalf("SessionByInstanceID = (%v, %v)", sess1, err)
	}
	out := d.CompareRunOutcome(sess1)
	if out == nil {
		t.Fatal("CompareRunOutcome = nil for a terminal session")
	}

	if out.TokensOutputTotal != 17024 {
		t.Errorf("TokensOutputTotal = %d, want 17024", out.TokensOutputTotal)
	}
	if out.TokensInputTotal != 2000 {
		t.Errorf("TokensInputTotal = %d, want 2000", out.TokensInputTotal)
	}
	if out.TokensCacheReadTotal != 5000 {
		t.Errorf("TokensCacheReadTotal = %d, want 5000", out.TokensCacheReadTotal)
	}
	if out.TokensCacheWriteTotal != 150 {
		t.Errorf("TokensCacheWriteTotal = %d, want 150", out.TokensCacheWriteTotal)
	}
	if out.CostUSDTotal != 2.125 {
		t.Errorf("CostUSDTotal = %v, want 2.125", out.CostUSDTotal)
	}
	if out.ToolCallCount != 1 {
		t.Errorf("ToolCallCount = %d, want 1", out.ToolCallCount)
	}
	if out.MsgAssistantCount != 2 {
		t.Errorf("MsgAssistantCount = %d, want 2", out.MsgAssistantCount)
	}

	// The stub's own columns survive: ComputeSpawnOutcome folds the persisted
	// agent-level values back in.
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict = %v, want \"pass\"", out.ReviewVerdict)
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 5 {
		t.Errorf("ReviewPassCount = %v, want 5", out.ReviewPassCount)
	}
}

// TestCompareRunOutcome_TerminalSessionReportsEndStateAndDuration covers the
// AC that `prism stats compare` reports end_state and duration_ms for a
// finished session with no persisted spawn_outcome row. Before the fix both
// were nil, because both were read from the sessions row that only
// `prism cleanup` writes.
func TestCompareRunOutcome_TerminalSessionReportsEndStateAndDuration(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-no-row"
	started := time.Now().Add(-20 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(10, 20, 0, 0, 0.5), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(15*time.Minute))

	sess1, _ := d.SessionByInstanceID(iid)
	if sess1.EndState != nil || sess1.EndedAt != nil {
		t.Fatalf("fixture: sessions row must be un-stamped before cleanup, got end_state=%v ended_at=%v",
			sess1.EndState, sess1.EndedAt)
	}

	out := d.CompareRunOutcome(sess1)
	if out == nil {
		t.Fatal("CompareRunOutcome = nil for a terminal session with no persisted row")
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState = %v, want \"finished\"", out.EndState)
	}
	if out.DurationMs == nil {
		t.Fatal("DurationMs = nil, want the start→terminal-transition interval")
	}
	if want := (15 * time.Minute).Milliseconds(); *out.DurationMs != want {
		t.Errorf("DurationMs = %d, want %d", *out.DurationMs, want)
	}
	if out.TimeToFinishedMs == nil || *out.TimeToFinishedMs != *out.DurationMs {
		t.Errorf("TimeToFinishedMs = %v, want %d", out.TimeToFinishedMs, *out.DurationMs)
	}
}

// TestComputeSpawnOutcome_DurationIndependentOfCleanupTime is the regression
// test for the second defect. The persisted duration must measure
// start → terminal transition, so it does not move when `prism cleanup` runs
// sixteen minutes later, and two sessions cleaned up in one loop keep their
// real, different durations.
func TestComputeSpawnOutcome_DurationIndependentOfCleanupTime(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-duration"
	started := time.Now().Add(-45 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(10, 20, 0, 0, 0.5), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(28*time.Minute+24*time.Second))

	before, err := d.ComputeSpawnOutcome(iid)
	if err != nil || before == nil {
		t.Fatalf("ComputeSpawnOutcome (pre-cleanup) = (%v, %v)", before, err)
	}

	// `prism cleanup` stamps sessions.ended_at with the time IT ran — here,
	// ~17 minutes after the session actually finished — then writes the row.
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	after, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || after == nil {
		t.Fatalf("SpawnOutcomeByInstanceID = (%v, %v)", after, err)
	}

	want := (28*time.Minute + 24*time.Second).Milliseconds()
	if before.DurationMs == nil || *before.DurationMs != want {
		t.Errorf("pre-cleanup DurationMs = %v, want %d", before.DurationMs, want)
	}
	if after.DurationMs == nil || *after.DurationMs != want {
		t.Errorf("persisted DurationMs = %v, want %d — cleanup time must not enter the measurement",
			after.DurationMs, want)
	}
	if after.TimeToFinishedMs == nil || *after.TimeToFinishedMs != want {
		t.Errorf("persisted TimeToFinishedMs = %v, want %d", after.TimeToFinishedMs, want)
	}
}

// TestComputeSpawnOutcome_FallsBackToSessionsEndedAt covers the sessions that
// have no terminal state_change event at all: pre-migration rows, and
// sessions killed while active. Those keep the previous behaviour — duration
// measured against sessions.ended_at.
func TestComputeSpawnOutcome_FallsBackToSessionsEndedAt(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-no-state-change"
	started := time.Now().Add(-10 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	// The only state_change is non-terminal: the session was still working
	// when it was killed, so there is no terminal transition to measure to.
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"active"}`, started.Add(time.Minute))
	if err := d.UpdateSessionEnded(iid, "interrupted"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}

	out, err := d.ComputeSpawnOutcome(iid)
	if err != nil || out == nil {
		t.Fatalf("ComputeSpawnOutcome = (%v, %v)", out, err)
	}
	if out.EndState == nil || *out.EndState != "interrupted" {
		t.Errorf("EndState = %v, want \"interrupted\"", out.EndState)
	}
	if out.DurationMs == nil {
		t.Fatal("DurationMs = nil, want the sessions.ended_at fallback")
	}
	// ~10 minutes, measured to the ended_at stamp written just above.
	if *out.DurationMs < (9 * time.Minute).Milliseconds() {
		t.Errorf("DurationMs = %d, want the full ~10m span from sessions.ended_at", *out.DurationMs)
	}
	if out.TimeToFinishedMs != nil {
		t.Errorf("TimeToFinishedMs = %v, want nil for a non-finished end state", out.TimeToFinishedMs)
	}
}

// TestCompareRunOutcome_PersistedAggregatesWin pins the other half of the
// ordering rule. agent_events is pruned at 90 days and spawn_outcome is not,
// so once the aggregates are persisted they are authoritative — a later
// recomputation must never replace them.
func TestCompareRunOutcome_PersistedAggregatesWin(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-persisted"
	started := time.Now().Add(-30 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(100, 200, 0, 0, 0.5), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(2*time.Minute))
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	// A later event that the persisted row does not know about stands in for
	// the inverse case (rows pruned out from under a persisted row).
	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(999, 999, 0, 0, 9.5), started.Add(3*time.Minute))

	sess1, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess1)
	if out == nil {
		t.Fatal("CompareRunOutcome = nil")
	}
	if out.TokensOutputTotal != 200 {
		t.Errorf("TokensOutputTotal = %d, want 200 (the persisted row must win)", out.TokensOutputTotal)
	}
}

// TestCompareRunOutcome_NoTokenFields covers the edge-case AC: a session
// whose events carry no token fields reports zeros and does not error.
func TestCompareRunOutcome_NoTokenFields(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2932-no-tokens"
	started := time.Now().Add(-5 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)

	writeEventAt(t, d, sess, iid, "msg_assistant", `{"text":"no usage object here"}`, started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(2*time.Minute))

	sess1, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess1)
	if out == nil {
		t.Fatal("CompareRunOutcome = nil for a terminal session with token-less events")
	}
	if out.TokensInputTotal != 0 || out.TokensOutputTotal != 0 || out.CostUSDTotal != 0 {
		t.Errorf("token-less session reported input=%d output=%d cost=%v, want all zero",
			out.TokensInputTotal, out.TokensOutputTotal, out.CostUSDTotal)
	}
	if out.MsgAssistantCount != 1 {
		t.Errorf("MsgAssistantCount = %d, want 1", out.MsgAssistantCount)
	}
}

// TestHasComputedAggregates checks the predicate that decides whether a
// persisted row is a cleanup-written aggregate or a partial stub.
func TestHasComputedAggregates(t *testing.T) {
	dur := int64(1234)
	cases := []struct {
		name string
		out  *db.SpawnOutcome
		want bool
	}{
		{name: "nil", out: nil, want: false},
		{name: "empty", out: &db.SpawnOutcome{}, want: false},
		{name: "pr stub", out: &db.SpawnOutcome{PRNumber: new(int)}, want: false},
		{name: "review stub", out: &db.SpawnOutcome{ReviewVerdict: new(string), ReviewPassCount: new(int)}, want: false},
		{name: "tokens", out: &db.SpawnOutcome{TokensOutputTotal: 1}, want: true},
		{name: "cost", out: &db.SpawnOutcome{CostUSDTotal: 0.01}, want: true},
		{name: "tool calls", out: &db.SpawnOutcome{ToolCallCount: 1}, want: true},
		{name: "duration only", out: &db.SpawnOutcome{DurationMs: &dur}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.HasComputedAggregates(); got != tc.want {
				t.Errorf("HasComputedAggregates() = %v, want %v", got, tc.want)
			}
		})
	}
}
