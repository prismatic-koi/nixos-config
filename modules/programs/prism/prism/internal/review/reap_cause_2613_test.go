package review_test

// reap_cause_2613_test.go — a closed agent_status row must name one cause.
//
// A report on a closed agent_status row must not end in a disjunction like:
//
//	ERROR: agent produced no verdict — session ended mid-review: the
//	agent_status row was closed at <ts> in state "error", so it is excluded
//	from the group results — the session was force-terminated, or its
//	readiness gate failed
//
// The trailing disjunction cannot name the cause. It is also wrong on one of
// its two halves: a readiness-gate failure cannot reach that message, because
// RunAsync removes the failed agent from the set it hands the monitor, and
// ClassifyRound only classifies the set it is given.

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// conflatedHint is the exact text these tests exist to remove. No report may
// contain it again.
const conflatedHint = "the session was force-terminated, or its readiness gate failed"

// closedRow is the agent_status row a lifecycle path leaves behind: state
// forced to "error", ended_at stamped.
func closedRow(sess string) db.Status {
	ended := time.Date(2026, 6, 12, 9, 14, 3, 0, time.UTC)
	return db.Status{SessionName: sess, State: "error", EndedAt: &ended}
}

// fourPassingSiblings builds the GroupResults map for a five-agent round in
// which every agent except reaped produced a PASS verdict.
func fourPassingSiblings(sessions []string, reaped string) map[string]db.GroupMemberResult {
	out := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == reaped {
			continue
		}
		out[s] = passedMember(s)
	}
	return out
}

// ── One cause per closed row ──────────────────────────────────────────

// TestClassifyRound_ReadinessGateAndForceTerminateAreDistinct is the direct
// pin. Two rows with identical state and identical ended_at, differing
// only in the cause their closing path recorded, must classify differently
// and read differently.
func TestClassifyRound_ReadinessGateAndForceTerminateAreDistinct(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	groupData := fourPassingSiblings(sessions, qa)
	endedRows := map[string]db.Status{qa: closedRow(qa)}
	agents := review.AgentsFromSessionsForTest(sessions)

	cases := []struct {
		name      string
		cause     db.SessionReapCause
		wantClass review.NoVerdictClass
		wantText  string
	}{
		{
			name:      "readiness gate",
			cause:     db.ReapCauseReadinessGate,
			wantClass: review.NoVerdictNotReady,
			wantText:  "readiness gate",
		},
		{
			name:      "monitor safety timeout",
			cause:     db.ReapCauseMonitorTimeout,
			wantClass: review.NoVerdictForceTerminated,
			wantText:  "safety deadline",
		},
		{
			name:      "parent cleanup cascade",
			cause:     db.ReapCauseParentCleanup,
			wantClass: review.NoVerdictForceTerminated,
			wantText:  "cleanup of the parent worker session",
		},
		{
			name:      "operator cleanup command",
			cause:     db.ReapCauseCleanupCommand,
			wantClass: review.NoVerdictForceTerminated,
			wantText:  "prism cleanup",
		},
		{
			name:      "spawn failure",
			cause:     db.ReapCauseSpawnFailure,
			wantClass: review.NoVerdictNoStart,
			wantText:  "spawn of this agent failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			causes := map[string]db.SessionEndCause{qa: {Cause: tc.cause}}
			st := review.ClassifyRoundWithCauses(agents, sessions, groupData, endedRows, causes)

			if len(st.Missing) != 1 {
				t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
			}
			m := st.Missing[0]
			if m.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", m.Class, tc.wantClass)
			}
			if !strings.Contains(m.Reason, tc.wantText) {
				t.Errorf("Reason = %q, want it to contain %q", m.Reason, tc.wantText)
			}
			if strings.Contains(m.Reason, conflatedHint) {
				t.Errorf("Reason still carries the conflated hint: %q", m.Reason)
			}
			if strings.Contains(m.Reason, ", or ") {
				t.Errorf("Reason names more than one possibility: %q", m.Reason)
			}
		})
	}
}

// TestBuildDeliveryMessage_NeverCarriesTheConflatedHint checks the rendered
// report, not just the classification. The worker reads this text.
func TestBuildDeliveryMessage_NeverCarriesTheConflatedHint(t *testing.T) {
	sessions := fiveAgentSessions(4)
	qa := sessions[3]
	groupData := fourPassingSiblings(sessions, qa)
	endedRows := map[string]db.Status{qa: closedRow(qa)}

	for _, cause := range []db.SessionReapCause{
		db.ReapCauseReadinessGate,
		db.ReapCauseMonitorTimeout,
		db.ReapCauseParentCleanup,
		db.ReapCauseCleanupCommand,
		db.ReapCauseSpawnFailure,
		"", // nothing recorded — the degraded shape
	} {
		causes := map[string]db.SessionEndCause{}
		if cause != "" {
			causes[qa] = db.SessionEndCause{Cause: cause}
		}
		msg := review.BuildDeliveryMessageWithCausesForTest(
			"2610", 4, "results body", false, groupData, sessions, endedRows, causes)

		if strings.Contains(msg, conflatedHint) {
			t.Errorf("cause %q: delivery message still carries the conflated hint:\n%s", cause, msg)
		}
	}
}

// TestClassifyRound_ClosedRowWithNoRecordedCause_SaysSo pins the degraded
// shape. When no path recorded a cause the report must say that, rather than
// guess between paths — a guess reads as a finding and is not one.
func TestClassifyRound_ClosedRowWithNoRecordedCause_SaysSo(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	st := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions),
		sessions,
		fourPassingSiblings(sessions, qa),
		map[string]db.Status{qa: closedRow(qa)},
		nil,
	)

	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
	}
	m := st.Missing[0]
	if m.Class != review.NoVerdictSessionEnded {
		t.Errorf("Class = %q, want %q", m.Class, review.NoVerdictSessionEnded)
	}
	if !strings.Contains(m.Reason, "no close cause was recorded") {
		t.Errorf("Reason = %q, want it to state that no cause was recorded", m.Reason)
	}
	if strings.Contains(m.Reason, conflatedHint) {
		t.Errorf("Reason still carries the conflated hint: %q", m.Reason)
	}
}

// TestClassifyRound_StalledThenClosed_ReportsTheStall pins the shape that
// the report can lose. The sidecar's inactivity watchdog sets state="error"
// and writes stall_error but leaves ended_at NULL; the tmux session-closed hook
// then stamps ended_at without rewriting state. The row drops out of
// GroupResults and the recorded stall must survive that.
func TestClassifyRound_StalledThenClosed_ReportsTheStall(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	const stall = "stalled mid-run after 6m0s (23 frame(s) received, last at 2026-06-12T09:08:01Z)"

	st := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions),
		sessions,
		fourPassingSiblings(sessions, qa),
		map[string]db.Status{qa: closedRow(qa)},
		map[string]db.SessionEndCause{qa: {StallError: stall, TmuxSessionEnded: true}},
	)

	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
	}
	m := st.Missing[0]
	if m.Class != review.NoVerdictStalled {
		t.Errorf("Class = %q, want %q — a recorded stall outranks the later close", m.Class, review.NoVerdictStalled)
	}
	if !strings.Contains(m.Reason, stall) {
		t.Errorf("Reason = %q, want it to carry the recorded stall text", m.Reason)
	}
}

// TestClassifyRound_NoStartThenClosed_ReportsTheNoStart is the sibling of the
// stall case: a startup_error recorded before the close outranks it.
func TestClassifyRound_NoStartThenClosed_ReportsTheNoStart(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	const reason = "inactivity timeout: no inbound frame for 5m0s (no frames received)"

	st := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions),
		sessions,
		fourPassingSiblings(sessions, qa),
		map[string]db.Status{qa: closedRow(qa)},
		map[string]db.SessionEndCause{qa: {StartupError: reason, Cause: db.ReapCauseParentCleanup}},
	)

	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
	}
	if st.Missing[0].Class != review.NoVerdictNoStart {
		t.Errorf("Class = %q, want %q — the agent's own failure outranks the later close",
			st.Missing[0].Class, review.NoVerdictNoStart)
	}
}

// ── A reaped agent leaves the round incomplete ────────────────────────

// TestClassifyRound_ReapedAgent_KeepsSiblingVerdictsAndConsumesNoCycle pins
// the safety property, across every new cause. Whatever
// closed the row, the round must report the four verdicts that did arrive and
// must not consume a review cycle.
func TestClassifyRound_ReapedAgent_KeepsSiblingVerdictsAndConsumesNoCycle(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	agents := review.AgentsFromSessionsForTest(sessions)
	groupData := fourPassingSiblings(sessions, qa)
	endedRows := map[string]db.Status{qa: closedRow(qa)}

	for _, cause := range []db.SessionReapCause{
		db.ReapCauseReadinessGate,
		db.ReapCauseSpawnFailure,
		db.ReapCauseMonitorTimeout,
		db.ReapCauseParentCleanup,
		db.ReapCauseCleanupCommand,
	} {
		t.Run(string(cause), func(t *testing.T) {
			st := review.ClassifyRoundWithCauses(agents, sessions, groupData, endedRows,
				map[string]db.SessionEndCause{qa: {Cause: cause}})

			if st.Expected != 5 {
				t.Errorf("Expected = %d, want 5", st.Expected)
			}
			if st.Verdicts != 4 {
				t.Errorf("Verdicts = %d, want 4 — the siblings' verdicts must still be reported", st.Verdicts)
			}
			if st.Complete() {
				t.Error("Complete() = true, want false")
			}
			if st.CountsAsCycle() {
				t.Error("CountsAsCycle() = true, want false — a reaped agent must not consume a review cycle")
			}
			if !st.HasInfrastructureFailure() {
				t.Error("HasInfrastructureFailure() = false, want true")
			}
			if got := st.MissingAgentNames(); len(got) != 1 || got[0] != "review-qa" {
				t.Errorf("MissingAgentNames() = %v, want [review-qa]", got)
			}
		})
	}
}

// TestBuildDeliveryMessage_ReapedAgent_NeverReadsAsAllPassed is the
// safety-property pin at the report layer: four PASS plus one closed row must
// never render as a passing round.
func TestBuildDeliveryMessage_ReapedAgent_NeverReadsAsAllPassed(t *testing.T) {
	sessions := fiveAgentSessions(4)
	qa := sessions[3]

	msg := review.BuildDeliveryMessageWithCausesForTest(
		"2610", 4, "results body", true, // allPassed=true for the four that ran
		fourPassingSiblings(sessions, qa), sessions,
		map[string]db.Status{qa: closedRow(qa)},
		map[string]db.SessionEndCause{qa: {Cause: db.ReapCauseReadinessGate}},
	)

	if strings.Contains(msg, "All 5 review agents passed") {
		t.Errorf("a round with a closed row rendered as a full pass:\n%s", msg)
	}
	if !strings.Contains(msg, "Round incomplete: 4 of 5") {
		t.Errorf("message does not report the shortfall:\n%s", msg)
	}
	if !strings.Contains(msg, "does NOT count toward the 3-cycle limit") {
		t.Errorf("message does not state that the round consumes no cycle:\n%s", msg)
	}
	if !strings.Contains(msg, string(review.NoVerdictNotReady)) {
		t.Errorf("message does not name the readiness-gate class:\n%s", msg)
	}
}

// TestClassSummary_NamesTheNewClasses pins the count-summary labels. The
// operator picks a response from this line, so the two split causes must be
// visible in it.
func TestClassSummary_NamesTheNewClasses(t *testing.T) {
	sessions := fiveAgentSessions(3)
	agents := review.AgentsFromSessionsForTest(sessions)
	endedRows := map[string]db.Status{sessions[2]: closedRow(sessions[2]), sessions[3]: closedRow(sessions[3])}
	groupData := map[string]db.GroupMemberResult{
		sessions[0]: passedMember(sessions[0]),
		sessions[1]: passedMember(sessions[1]),
		sessions[4]: passedMember(sessions[4]),
	}
	causes := map[string]db.SessionEndCause{
		sessions[2]: {Cause: db.ReapCauseReadinessGate},
		sessions[3]: {Cause: db.ReapCauseMonitorTimeout},
	}

	summary := review.ClassifyRoundWithCauses(agents, sessions, groupData, endedRows, causes).ClassSummary()
	for _, want := range []string{"failed its readiness gate", "force-terminated"} {
		if !strings.Contains(summary, want) {
			t.Errorf("ClassSummary() = %q, want it to contain %q", summary, want)
		}
	}
}

// ── The cause is readable from the DB ─────────────────────────────────

// TestEndedMemberCauses_ReadsWhatTheCleanupPathRecorded closes the loop from
// the write side to the read side against a real database: record a reap the
// way a cleanup path does, then read it the way the monitor does.
func TestEndedMemberCauses_ReadsWhatTheCleanupPathRecorded(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613~review-1-review-qa"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	d.RecordReapBestEffort(session, db.ReapCauseMonitorTimeout, "still in state \"active\" when the deadline fired")
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	row, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	causes := review.EndedMemberCausesForTest(d, map[string]db.Status{session: *row})
	if causes[session].Cause != db.ReapCauseMonitorTimeout {
		t.Fatalf("Cause = %q, want %q", causes[session].Cause, db.ReapCauseMonitorTimeout)
	}

	results := review.BuildMonitorResultsWithCausesForTest(
		review.AgentsFromSessionsForTest([]string{session}),
		[]string{session},
		map[string]db.GroupMemberResult{}, // GroupResults drops the closed row
		map[string]db.Status{session: *row},
		causes,
	)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if !strings.Contains(results[0].Output, "safety deadline") {
		t.Errorf("per-agent output does not name the recorded cause: %q", results[0].Output)
	}
	if strings.Contains(results[0].Output, conflatedHint) {
		t.Errorf("per-agent output still carries the conflated hint: %q", results[0].Output)
	}
}

// TestEndedMemberCauses_NilInputsDegradeQuietly pins the degraded shapes the
// monitor can hit: no DB handle, or no closed rows.
func TestEndedMemberCauses_NilInputsDegradeQuietly(t *testing.T) {
	if got := review.EndedMemberCausesForTest(nil, map[string]db.Status{"x": {}}); got != nil {
		t.Errorf("with a nil DB: got %v, want nil", got)
	}
	d := openTestDB(t)
	if got := review.EndedMemberCausesForTest(d, nil); got != nil {
		t.Errorf("with no closed rows: got %v, want nil", got)
	}
}

// ── the expected set is never shrunk to fit its own failures ────────────────

// TestExpectedRoundSet_NotFilteredBySpawnErrors is the pin for the defect that
// would make a readiness-gate failure unreportable. If RunAsync handed the
// monitor only the agents that came up, ClassifyRound would derive Expected
// from that shorter list, so a four-agent round would read as complete: four
// PASS verdicts rendered as "All 5 review agents passed" and consumed one of
// the worker's three cycles, while the fifth dimension was never examined.
func TestExpectedRoundSet_NotFilteredBySpawnErrors(t *testing.T) {
	sessions := fiveAgentSessions(3)
	agents := review.AgentsFromSessionsForTest(sessions)
	spawnErr := make([]error, len(agents))
	spawnErr[3] = errReadinessTimeout

	gotAgents, gotSessions := review.ExpectedRoundSetForTest(agents, sessions, spawnErr)

	if len(gotAgents) != len(agents) {
		t.Errorf("agents = %d, want %d — the expected set must not be filtered by spawn failures",
			len(gotAgents), len(agents))
	}
	if len(gotSessions) != len(sessions) {
		t.Errorf("sessions = %d, want %d", len(gotSessions), len(sessions))
	}
	for i, s := range sessions {
		if gotSessions[i] != s {
			t.Errorf("sessions[%d] = %q, want %q", i, gotSessions[i], s)
		}
	}
}

// errReadinessTimeout stands in for the readiness-gate error the review spawn
// loop records in spawnErr.
var errReadinessTimeout = errStub("readiness gate for review-qa: not ready within 30s")

type errStub string

func (e errStub) Error() string { return string(e) }
