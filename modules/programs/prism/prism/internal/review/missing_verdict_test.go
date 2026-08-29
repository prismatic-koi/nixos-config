package review_test

// missing_verdict_test.go — a review agent that produces no verdict must be
// distinguishable from an agent that passed.
//
// The failure this guards against: a review agent that fails in every round
// with "agent session not found in group (possibly deleted mid-review)".
// Its four siblings finish normally, the round reports as complete, and it
// consumes a review cycle each time. Four verdicts plus one blank read as
// "four passed".
//
// Root cause: db.GroupResults reads
// `agent_status WHERE group_id = ? AND ended_at IS NULL`, so a member whose
// row was closed mid-round is simply absent from the map. Every consumer read
// that map alone, so the absent member was invisible to the header branch and
// to the cycle counter. ClassifyRound walks the EXPECTED member list instead.

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

// TestClassifyRound_ReapedMemberIsMissing pins that a member
// absent from GroupResults is classified, named, and excluded from the verdict
// count.
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
	// The reason must carry what the DB actually recorded.
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

// TestClassifyRound_CompleteRoundCountsAsCycle is the regression guard:
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

// TestClassifyRound_AllInfraIsNoResult pins the mix-of-infrastructure-classes
// case: no agent produced a verdict, so the round is a no-result round and
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

// TestRoundStatus_TargetedRerunCommand pins the command at the source: it
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

// TestRoundStatus_CompleteRoundHasNoRerunCommand guards the no-op case.
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
// for the cycle-counter defect. It documents the shape of
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

	// The keys-only view sees four verdicts and calls the round complete. This
	// assertion is the record of why the expected-member list has to be
	// threaded through.
	if !review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("keys-only view: expected the pre-#2573 shape to report the round as verdict-producing")
	}
}

// TestCycleProducedVerdicts_FullRoundStillCounts is the guard for the
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
// otherwise identical full round must.
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

// TestBuildDeliveryMessage_FourPassOneReaped is the headline pin: the
// four-pass-one-reaped shape must not read as "four passed".
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
		"Round incomplete: 4 of 5",
		"Agents with no verdict (1 of 5)",
		"review-qa",
		string(review.NoVerdictSessionEnded),
		"2026-06-12T09:14:03Z",
		"infrastructure failure",
		"prism review 2568 --only review-qa",
		"does NOT count toward the 3-cycle limit",
		"never as a pass",
	} {
		if !findSubstring(msg, want) {
			t.Errorf("four-pass-one-reaped: message missing %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{
		"All 5 review agents passed",
		"Fix the blocking issues", // not an ordinary code-FAIL round
		"failed to start",         // no agent no-started
		"stalled mid-run",         // no agent stalled
	} {
		if findSubstring(msg, unwanted) {
			t.Errorf("four-pass-one-reaped: message must NOT contain %q:\n%s", unwanted, msg)
		}
	}
}

// TestBuildDeliveryMessage_AllPassedFlagWithMissingAgent is the belt-and-
// braces arm: even when the caller hands in allPassed=true, a round
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

// TestBuildDeliveryMessage_AllReapedIsNoResult pins the report case: a
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

// TestBuildDeliveryMessage_CompleteFailRoundUnchanged is the guard for
// the report: a full round with a FAIL verdict keeps the code-FAIL wording and
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
// dimension that was never examined.
//
// It also pins the targeted-rerun gate. A FAIL verdict means the worker must
// change code before re-running, which makes every verdict in this round stale
// — so the report must advertise the FULL re-run, never `--only`. Advertising
// `--only` here would record four PASS verdicts against the pre-fix commit.
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
		"Fix the blocking issues from the agents that ran",
		"session ended mid-review",
		"targeted `--only` re-run is NOT valid here",
		"prism review 2568",
	} {
		if !findSubstring(msg, want) {
			t.Errorf("mixed infra + code FAIL: message missing %q:\n%s", want, msg)
		}
	}
	if findSubstring(msg, "--only review-qa") {
		t.Errorf("mixed infra + code FAIL: must NOT advertise a targeted re-run "+
			"— the fix invalidates the verdicts this round produced (#2530 / #2557):\n%s", msg)
	}
}

// TestBuildDeliveryMessage_NoFailAdvertisesTargetedRerun is the other arm of
// the gate: with no FAIL verdict there is nothing to fix, so the targeted
// command is valid — but only if the worker pushes nothing else. The report
// must carry that caveat and the full-set fallback.
func TestBuildDeliveryMessage_NoFailAdvertisesTargetedRerun(t *testing.T) {
	sessions := fiveAgentSessions(1)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		if s == sessions[3] {
			continue
		}
		groupData[s] = passedMember(s)
	}
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "deleted")}

	msg := review.BuildDeliveryMessageWithEndedForTest("2568", 1, "results text", false, groupData, sessions, endedRows)

	for _, want := range []string{
		"prism review 2568 --only review-qa",
		"provided you push nothing else first",
		"other than formatter output, comments, or documentation",
		"re-run the full set instead",
	} {
		if !findSubstring(msg, want) {
			t.Errorf("no-FAIL round: message missing %q:\n%s", want, msg)
		}
	}
	if findSubstring(msg, "NOT valid here") {
		t.Errorf("no-FAIL round: the targeted command is valid, so the report must not refuse it:\n%s", msg)
	}
}

// TestRoundStatus_FailVerdictGatesTargetedRerun pins the gate at the source.
func TestRoundStatus_FailVerdictGatesTargetedRerun(t *testing.T) {
	sessions := fiveAgentSessions(1)
	base := map[string]db.GroupMemberResult{
		sessions[0]: passedMember(sessions[0]),
		sessions[1]: passedMember(sessions[1]),
		sessions[2]: passedMember(sessions[2]),
		sessions[4]: passedMember(sessions[4]),
	}
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "deleted")}
	agents := review.AgentsFromSessionsForTest(sessions)

	st := review.ClassifyRound(agents, sessions, base, endedRows)
	if st.Fails != 0 {
		t.Errorf("Fails = %d, want 0", st.Fails)
	}
	if !st.TargetedRerunAllowed() {
		t.Error("TargetedRerunAllowed() = false with no FAIL verdict; want true")
	}

	withFail := map[string]db.GroupMemberResult{}
	for k, v := range base {
		withFail[k] = v
	}
	withFail[sessions[1]] = failedMember(sessions[1])

	st = review.ClassifyRound(agents, sessions, withFail, endedRows)
	if st.Fails != 1 {
		t.Errorf("Fails = %d, want 1", st.Fails)
	}
	if st.TargetedRerunAllowed() {
		t.Error("TargetedRerunAllowed() = true with a FAIL verdict; want false (#2530 / #2557)")
	}
	if got, want := st.FullRerunCommand("2568"), "prism review 2568"; got != want {
		t.Errorf("FullRerunCommand = %q, want %q", got, want)
	}
}

// ── per-agent result ────────────────────────────────────────────────────────

// TestBuildMonitorResults_ReapedSessionNamesReason verifies the per-agent
// entry carries the recorded cause rather than a "possibly deleted
// mid-review" guess.
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

// TestFormatResultsForRound_RetryHintMatchesRerunAdvice pins the summary
// footer against the re-run advice below it. The two must not
// contradict each other: the footer must not offer `--only` in a round
// where a FAIL makes a targeted retry invalid.
func TestFormatResultsForRound_RetryHintMatchesRerunAdvice(t *testing.T) {
	sessions := fiveAgentSessions(1)
	agents := review.AgentsFromSessionsForTest(sessions)
	endedRows := map[string]db.Status{sessions[3]: reapedRow(sessions[3], "deleted")}

	build := func(withFail bool) (string, review.RoundStatus) {
		groupData := map[string]db.GroupMemberResult{}
		for _, s := range sessions {
			if s == sessions[3] {
				continue
			}
			groupData[s] = passedMember(s)
		}
		if withFail {
			groupData[sessions[1]] = failedMember(sessions[1])
		}
		st := review.ClassifyRound(agents, sessions, groupData, endedRows)
		results := review.BuildMonitorResultsWithEndedForTest(agents, sessions, groupData, endedRows)
		out, _ := review.FormatResultsForRound(results, "2568", 1, 0, st)
		return out, st
	}

	// Incomplete, no FAIL: the targeted retry is valid and still offered.
	out, _ := build(false)
	if !findSubstring(out, "Retry: prism review 2568 --only review-qa") {
		t.Errorf("incomplete, no FAIL: want the targeted retry hint:\n%s", out)
	}

	// Incomplete with a FAIL: the targeted retry is invalid, so the footer
	// must offer the full set instead.
	out, _ = build(true)
	if findSubstring(out, "--only") {
		t.Errorf("incomplete + FAIL: footer must not offer a targeted retry (#2530 / #2557):\n%s", out)
	}
	if !findSubstring(out, "Retry: prism review 2568") {
		t.Errorf("incomplete + FAIL: want the full-set retry hint:\n%s", out)
	}
}

// TestFormatResultsForRound_CompleteRoundFooterUnchanged is the guard for
// the footer: a complete FAIL round must render byte-for-byte as FormatResults
// renders it.
func TestFormatResultsForRound_CompleteRoundFooterUnchanged(t *testing.T) {
	sessions := fiveAgentSessions(1)
	agents := review.AgentsFromSessionsForTest(sessions)
	groupData := map[string]db.GroupMemberResult{}
	for _, s := range sessions {
		groupData[s] = passedMember(s)
	}
	groupData[sessions[1]] = failedMember(sessions[1])

	st := review.ClassifyRound(agents, sessions, groupData, nil)
	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)

	withStatus, passedA := review.FormatResultsForRound(results, "2568", 1, 0, st)
	legacy, passedB := review.FormatResults(results, "2568", 1, 0)
	if withStatus != legacy || passedA != passedB {
		t.Errorf("complete round: classification-aware output diverged from the legacy output\n--- with status ---\n%s\n--- legacy ---\n%s", withStatus, legacy)
	}
	if !findSubstring(withStatus, "Retry: prism review 2568 --only review-code") {
		t.Errorf("complete FAIL round: want the unchanged targeted retry hint:\n%s", withStatus)
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
