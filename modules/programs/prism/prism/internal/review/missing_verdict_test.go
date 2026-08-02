package review_test

// missing_verdict_test.go — a review agent that produces no verdict must be
// distinguishable from an agent that passed (#2573).
//
// The observed failure: on PR #2568 the review-qa agent failed in all three
// rounds with "agent session not found in group (possibly deleted
// mid-review)". Its four siblings finished normally, the round reported as
// complete, and it consumed a review cycle each time. Four verdicts plus one
// blank read as "four passed".
//
// Root cause: db.GroupResults reads
// `agent_status WHERE group_id = ? AND ended_at IS NULL`, so a member whose
// row was closed mid-round is simply absent from the map. Every consumer read
// that map alone, so the absent member was invisible to the header branch and
// to the cycle counter. The fix walks the EXPECTED member list instead.
//
// The tests below pin, in AC order:
//
//	AC-1  an incomplete round is reported as incomplete
//	AC-2  each agent with no verdict is named, with its recorded reason
//	AC-3  an infrastructure failure consumes no review cycle
//	AC-4  an infrastructure failure is distinguishable from a code FAIL
//	AC-5  the report states the targeted re-run command
//	AC-6  a full round is reported as before and consumes one cycle
//	AC-7  an all-infrastructure round is a no-result round, no cycle
//	AC-8  four PASS plus one infrastructure error never presents as passing

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

const missingVerdictParent = "nixos-config@parent"

// fiveAgentSessions returns the five canonical review-agent session names for
// a round, in spawn order.
func fiveAgentSessions(round int) []string {
	names := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%s~review-%d-%s", missingVerdictParent, round, n))
	}
	return out
}

// passedMember is a member that finished with a parseable PASS verdict.
func passedMember(sess string) db.GroupMemberResult {
	return db.GroupMemberResult{SessionName: sess, State: "finished", LastMessage: `{"text":"<verdict>PASS</verdict>"}`}
}

// failedMember is a member that finished with a parseable FAIL verdict.
func failedMember(sess string) db.GroupMemberResult {
	return db.GroupMemberResult{SessionName: sess, State: "finished", LastMessage: `{"text":"<verdict>FAIL</verdict>"}`}
}

// reapedRow is the agent_status row left behind when a review agent's session
// is closed mid-round: state untouched, ended_at set.
func reapedRow(sess, state string) db.Status {
	ended := time.Date(2026, 6, 12, 9, 14, 3, 0, time.UTC)
	return db.Status{SessionName: sess, State: state, EndedAt: &ended}
}

// ── ClassifyRound ───────────────────────────────────────────────────────────

// TestClassifyRound_ReapedMemberIsMissing is the core #2573 pin: a member
// absent from GroupResults is classified, named, and excluded from the verdict
// count (AC-1, AC-2, AC-3).
func TestClassifyRound_ReapedMemberIsMissing(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]

	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == qa {
			continue // reaped: GroupResults drops rows whose ended_at is set
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{qa: reapedRow(qa, "deleted")}

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, endedRows)

	if st.Expected != 5 {
		t.Errorf("Expected = %d, want 5", st.Expected)
	}
	if st.Verdicts != 4 {
		t.Errorf("Verdicts = %d, want 4", st.Verdicts)
	}
	if st.Complete() {
		t.Error("Complete() = true; want false — one agent produced no verdict")
	}
	if st.CountsAsCycle() {
		t.Error("CountsAsCycle() = true; want false — an infrastructure failure must not consume a cycle (AC-3)")
	}
	if len(st.Missing) != 1 {
		t.Fatalf("len(Missing) = %d, want 1: %+v", len(st.Missing), st.Missing)
	}
	m := st.Missing[0]
	if m.Agent != "review-qa" {
		t.Errorf("Missing[0].Agent = %q, want %q", m.Agent, "review-qa")
	}
	if m.Session != qa {
		t.Errorf("Missing[0].Session = %q, want %q", m.Session, qa)
	}
	if m.Class != review.NoVerdictSessionEnded {
		t.Errorf("Missing[0].Class = %q, want %q", m.Class, review.NoVerdictSessionEnded)
	}
	if !m.Class.Infrastructure() {
		t.Error("a reaped session must classify as an infrastructure failure (AC-4)")
	}
	// AC-2: the reason must carry what the DB actually recorded.
	for _, want := range []string{"2026-06-12T09:14:03Z", "deleted"} {
		if !strings.Contains(m.Reason, want) {
			t.Errorf("Missing[0].Reason = %q, want it to mention %q", m.Reason, want)
		}
	}
}

// TestClassifyRound_AbsentMemberWithoutRowIsUnknown covers the other way to be
// absent: no agent_status row can be read at all.
func TestClassifyRound_AbsentMemberWithoutRowIsUnknown(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions[:4] {
		groupData[s] = passedMember(s)
	}

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, nil)

	if len(st.Missing) != 1 {
		t.Fatalf("len(Missing) = %d, want 1", len(st.Missing))
	}
	if st.Missing[0].Class != review.NoVerdictSessionUnknown {
		t.Errorf("Class = %q, want %q", st.Missing[0].Class, review.NoVerdictSessionUnknown)
	}
	if !strings.Contains(st.Missing[0].Reason, "no agent_status row") {
		t.Errorf("Reason = %q, want it to say no row could be read", st.Missing[0].Reason)
	}
}

// TestClassifyRound_CompleteRoundCountsAsCycle is the AC-6 regression guard:
// a round in which every agent produced a verdict is complete and consumes a
// cycle, whether the verdicts are PASS or FAIL.
func TestClassifyRound_CompleteRoundCountsAsCycle(t *testing.T) {
	sessions := fiveAgentSessions(2)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		groupData[s] = passedMember(s)
	}
	groupData[sessions[2]] = failedMember(sessions[2])

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, nil)

	if !st.Complete() {
		t.Errorf("Complete() = false; want true. Missing: %+v", st.Missing)
	}
	if !st.CountsAsCycle() {
		t.Error("CountsAsCycle() = false; want true — a full round must consume one cycle (AC-6)")
	}
	if st.Verdicts != 5 {
		t.Errorf("Verdicts = %d, want 5", st.Verdicts)
	}
}

// TestClassifyRound_AllInfraIsNoResult pins AC-7 for a mix of infrastructure
// classes: no agent produced a verdict, so the round is a no-result round and
// consumes no cycle.
func TestClassifyRound_AllInfraIsNoResult(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{
		sessions[0]: {SessionName: sessions[0], State: "error", StartupError: "container never bound its port"},
		sessions[1]: {SessionName: sessions[1], State: "error", StallError: "stalled mid-run after 1m20s (4 frame(s) received, last at 2026-06-11T13:51:04Z)"},
		sessions[2]: {SessionName: sessions[2], State: "error"},
		sessions[4]: {SessionName: sessions[4], State: "active"},
	}
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "interrupted")}

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, endedRows)

	if st.Verdicts != 0 {
		t.Errorf("Verdicts = %d, want 0", st.Verdicts)
	}
	if st.CountsAsCycle() {
		t.Error("CountsAsCycle() = true; want false — an all-infrastructure round consumes no cycle (AC-7)")
	}
	wantClasses := []review.NoVerdictClass{
		review.NoVerdictNoStart,
		review.NoVerdictStalled,
		review.NoVerdictCrashed,
		review.NoVerdictSessionEnded,
		review.NoVerdictUnexpectedState,
	}
	for _, c := range wantClasses {
		if len(st.MissingOfClass(c)) != 1 {
			t.Errorf("MissingOfClass(%q) = %d entries, want 1. Missing: %+v", c, len(st.MissingOfClass(c)), st.Missing)
		}
	}
}

// TestRoundStatus_TargetedRerunCommand pins AC-5 at the source: the command
// names exactly the agents with no verdict, in spawn order.
func TestRoundStatus_TargetedRerunCommand(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{
		sessions[0]: passedMember(sessions[0]),
		sessions[1]: {SessionName: sessions[1], State: "error", StartupError: "no port"},
		sessions[2]: passedMember(sessions[2]),
		sessions[4]: passedMember(sessions[4]),
	}
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "deleted")}

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, endedRows)

	got := st.TargetedRerunCommand("2568")
	want := "prism review 2568 --only review-code,review-qa"
	if got != want {
		t.Errorf("TargetedRerunCommand = %q, want %q", got, want)
	}
	if cmd := st.TargetedRerunCommand(""); !strings.Contains(cmd, "<pr>") {
		t.Errorf("TargetedRerunCommand with no PR number = %q, want a <pr> placeholder", cmd)
	}
}

// TestRoundStatus_CompleteRoundHasNoRerunCommand guards the AC-6 no-op case.
func TestRoundStatus_CompleteRoundHasNoRerunCommand(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		groupData[s] = passedMember(s)
	}
	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, nil)
	if cmd := st.TargetedRerunCommand("2568"); cmd != "" {
		t.Errorf("TargetedRerunCommand on a complete round = %q, want \"\"", cmd)
	}
}

// ── cycle counting ──────────────────────────────────────────────────────────

// TestCycleProducedVerdicts_MissingMemberDoesNotCount is the direct regression
// for the #2573 cycle-counter defect (AC-3). It also documents the shape of
// the bug: reading only the keys that came back cannot see the member that
// did not.
func TestCycleProducedVerdicts_MissingMemberDoesNotCount(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == qa {
			continue
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{qa: reapedRow(qa, "deleted")}

	if review.CycleProducedVerdictsForTest(sessions, groupData, endedRows) {
		t.Error("a round with a reaped member must NOT count as verdict-producing (AC-3)")
	}

	// The keys-only view — what every call site used before #2573 — sees four
	// verdicts and calls the round complete. This assertion is the record of
	// why the expected-member list has to be threaded through.
	if !review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("keys-only view: expected the pre-#2573 shape to report the round as verdict-producing")
	}
}

// TestCycleProducedVerdicts_FullRoundStillCounts is the AC-6 guard for the
// predicate.
func TestCycleProducedVerdicts_FullRoundStillCounts(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		groupData[s] = passedMember(s)
	}
	if !review.CycleProducedVerdictsForTest(sessions, groupData, nil) {
		t.Error("a full round of parseable verdicts must count as verdict-producing (AC-6)")
	}
}

// TestCompletedReviewCyclesForParent_ReapedMemberRoundDoesNotCount exercises
// the historical counter end-to-end against a real DB: a round whose member
// row was closed mid-review must not count toward the 3-cycle limit, while an
// otherwise identical full round must (AC-3, AC-6).
func TestCompletedReviewCyclesForParent_ReapedMemberRoundDoesNotCount(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	const parent = "nixos-config@parent"

	// seedRound registers a group of five agents. When reapedIdx >= 0 that
	// member's row is closed with SetEnded (the shape `prism cleanup`, the
	// tmux-session-end hook, and the harness session.deleted handler all
	// leave behind) instead of finishing with a verdict.
	seedRound := func(round, reapedIdx int) string {
		gid, gErr := d.RegisterGroup(parent)
		if gErr != nil {
			t.Fatalf("RegisterGroup: %v", gErr)
		}
		sessions := fiveAgentSessions(round)
		for i, s := range sessions {
			if uErr := d.UpsertStatus(s, "nixos-config", "/wt", "active", nil, nil); uErr != nil {
				t.Fatalf("UpsertStatus(%q, active): %v", s, uErr)
			}
			if gsErr := d.SetGroupID(s, gid); gsErr != nil {
				t.Fatalf("SetGroupID(%q): %v", s, gsErr)
			}
			if i == reapedIdx {
				// Reaped mid-round: ended_at set, state left as-is.
				if eErr := d.SetEnded(s); eErr != nil {
					t.Fatalf("SetEnded(%q): %v", s, eErr)
				}
				continue
			}
			if uErr := d.UpsertStatus(s, "nixos-config", "/wt", "finished", nil, nil); uErr != nil {
				t.Fatalf("UpsertStatus(%q, finished): %v", s, uErr)
			}
			if wErr := d.WriteEvent(db.Event{
				ID:          s + "-verdict",
				SessionName: s,
				Repo:        "nixos-config",
				Worktree:    "/wt",
				Type:        "msg_assistant",
				Payload:     `{"text":"<verdict>PASS</verdict>"}`,
			}); wErr != nil {
				t.Fatalf("WriteEvent(%q): %v", s, wErr)
			}
		}
		return gid
	}

	// Round 1: review-qa (index 3) reaped mid-review.
	seedRound(1, 3)
	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 0 {
		t.Errorf("cycles after a reaped-member round = %d, want 0 (AC-3)", n)
	}

	// Round 2: every agent produced a verdict.
	seedRound(2, -1)
	n, err = review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 1 {
		t.Errorf("cycles after adding one full round = %d, want 1 (AC-6)", n)
	}
}

// ── report wording ──────────────────────────────────────────────────────────

// TestBuildDeliveryMessage_FourPassOneReaped is the headline AC pin: the
// #2568 shape must not read as "four passed" (AC-1, AC-2, AC-4, AC-5, AC-8).
func TestBuildDeliveryMessage_FourPassOneReaped(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == qa {
			continue
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{qa: reapedRow(qa, "deleted")}

	msg := review.BuildDeliveryMessageWithEndedForTest("2568", 3, "results text", false, groupData, sessions, endedRows)

	for _, want := range []string{
		"Round incomplete: 4 of 5",                // AC-1
		"Agents with no verdict (1 of 5)",         // AC-1 / AC-2
		"review-qa",                               // AC-2
		string(review.NoVerdictSessionEnded),      // AC-2 / AC-4
		"2026-06-12T09:14:03Z",                    // AC-2
		"infrastructure failure",                  // AC-4
		"prism review 2568 --only review-qa",      // AC-5
		"does NOT count toward the 3-cycle limit", // AC-3
		"never as a pass",                         // AC-8
	} {
		if !findSubstring(msg, want) {
			t.Errorf("four-pass-one-reaped: message missing %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{
		"All 5 review agents passed", // AC-8
		"Fix the blocking issues",    // AC-4: not an ordinary code-FAIL round
		"failed to start",            // no agent no-started
		"stalled mid-run",            // no agent stalled
	} {
		if findSubstring(msg, unwanted) {
			t.Errorf("four-pass-one-reaped: message must NOT contain %q:\n%s", unwanted, msg)
		}
	}
}

// TestBuildDeliveryMessage_AllPassedFlagWithMissingAgent is the belt-and-
// braces arm of AC-8: even when the caller hands in allPassed=true, a round
// with a missing verdict must not present as passing.
func TestBuildDeliveryMessage_AllPassedFlagWithMissingAgent(t *testing.T) {
	sessions := fiveAgentSessions(1)
	qa := sessions[3]
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == qa {
			continue
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{qa: reapedRow(qa, "finished")}

	msg := review.BuildDeliveryMessageWithEndedForTest("2568", 1, "results text", true, groupData, sessions, endedRows)

	if findSubstring(msg, "All 5 review agents passed") {
		t.Errorf("allPassed flag with a missing agent must not present as passing:\n%s", msg)
	}
	if !findSubstring(msg, "Round incomplete: 4 of 5") {
		t.Errorf("allPassed flag with a missing agent: want the incomplete header:\n%s", msg)
	}
}

// TestBuildDeliveryMessage_AllReapedIsNoResult pins AC-7 for the report: a
// round in which every agent was reaped is a no-result round.
func TestBuildDeliveryMessage_AllReapedIsNoResult(t *testing.T) {
	sessions := fiveAgentSessions(2)
	endedRows := map[string]db.Status{}
	for _, s := range sessions {
		endedRows[s] = reapedRow(s, "deleted")
	}

	msg := review.BuildDeliveryMessageWithEndedForTest("2568", 2, "results text", false,
		map[string]db.GroupMemberResult{}, sessions, endedRows)

	for _, want := range []string{
		"Round incomplete: 0 of 5",
		"no agent produced a verdict",
		"infrastructure failure",
		"do not treat this as FAIL",
		"does NOT count toward the 3-cycle limit",
		"prism review 2568 --only review-goal,review-code,review-security,review-qa,review-context",
	} {
		if !findSubstring(msg, want) {
			t.Errorf("all-reaped: message missing %q:\n%s", want, msg)
		}
	}
	if findSubstring(msg, "Fix the blocking issues") || findSubstring(msg, "Fix any blocking issues") {
		t.Errorf("all-reaped: no agent ran, so the message must not ask for blocking-issue fixes:\n%s", msg)
	}
}

// TestBuildDeliveryMessage_CompleteFailRoundUnchanged is the AC-6 guard for
// the report: a full round with a FAIL verdict keeps the pre-#2573 wording and
// grows no no-verdict section.
func TestBuildDeliveryMessage_CompleteFailRoundUnchanged(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		groupData[s] = passedMember(s)
	}
	groupData[sessions[3]] = failedMember(sessions[3])

	msg := review.BuildDeliveryMessageForTest("2568", 1, "results text", false, groupData, sessions)

	if !findSubstring(msg, "**One or more review agents failed.** Fix the blocking issues and re-run `prism review`.") {
		t.Errorf("complete FAIL round: want the unchanged code-FAIL header:\n%s", msg)
	}
	for _, unwanted := range []string{"Round incomplete", "Agents with no verdict", "infrastructure failure", "does NOT count toward the 3-cycle limit"} {
		if findSubstring(msg, unwanted) {
			t.Errorf("complete FAIL round: message must NOT contain %q:\n%s", unwanted, msg)
		}
	}
}

// TestBuildDeliveryMessage_MixedInfraAndCodeFailNamesBoth verifies the mixed
// shape keeps both signals: fix what the agents that ran found, and re-run the
// dimension that was never examined (AC-1, AC-4, AC-5).
func TestBuildDeliveryMessage_MixedInfraAndCodeFailNamesBoth(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{
		sessions[0]: passedMember(sessions[0]),
		sessions[1]: failedMember(sessions[1]),
		sessions[2]: passedMember(sessions[2]),
		sessions[4]: passedMember(sessions[4]),
	}
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "interrupted")}

	msg := review.BuildDeliveryMessageWithEndedForTest("2568", 1, "results text", false, groupData, sessions, endedRows)

	for _, want := range []string{
		"Round incomplete: 4 of 5",
		"Fix any blocking issues from the agents that ran",
		"prism review 2568 --only review-qa",
		"session ended mid-review",
	} {
		if !findSubstring(msg, want) {
			t.Errorf("mixed infra + code FAIL: message missing %q:\n%s", want, msg)
		}
	}
}

// ── per-agent result ────────────────────────────────────────────────────────

// TestBuildMonitorResults_ReapedSessionNamesReason verifies the per-agent
// entry carries the recorded cause rather than the old "possibly deleted
// mid-review" guess (AC-2, AC-4).
func TestBuildMonitorResults_ReapedSessionNamesReason(t *testing.T) {
	sessions := fiveAgentSessions(3)
	qa := sessions[3]
	agents := review.AgentsFromSessionsForTest(sessions)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == qa {
			continue
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{qa: reapedRow(qa, "deleted")}

	results := review.BuildMonitorResultsWithEndedForTest(agents, sessions, groupData, endedRows)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}
	r := results[3]
	if !r.IsError {
		t.Error("reaped agent: IsError = false, want true")
	}
	if r.Passed {
		t.Error("reaped agent: Passed = true, want false")
	}
	for _, want := range []string{"produced no verdict", "session ended mid-review", "2026-06-12T09:14:03Z", "session.deleted"} {
		if !findSubstring(r.Output, want) {
			t.Errorf("reaped agent: output missing %q: %q", want, r.Output)
		}
	}
	// Siblings are unaffected.
	for i, r := range results {
		if i == 3 {
			continue
		}
		if !r.Passed || r.IsError {
			t.Errorf("sibling %d (%s): Passed=%v IsError=%v, want true/false", i, r.Agent.Name, r.Passed, r.IsError)
		}
	}
}

// TestEndedRowsFrom_KeepsOnlyClosedRows pins the filter that feeds the
// classifier: live rows must not be reported as reaped.
func TestEndedRowsFrom_KeepsOnlyClosedRows(t *testing.T) {
	ended := time.Date(2026, 6, 12, 9, 14, 3, 0, time.UTC)
	members := []db.Status{
		{SessionName: "live", State: "finished"},
		{SessionName: "closed", State: "deleted", EndedAt: &ended},
	}
	got := review.EndedRowsFromForTest(members)
	if _, ok := got["live"]; ok {
		t.Error("endedRowsFrom kept a row whose ended_at is NULL")
	}
	if _, ok := got["closed"]; !ok {
		t.Error("endedRowsFrom dropped a row whose ended_at is set")
	}
}
