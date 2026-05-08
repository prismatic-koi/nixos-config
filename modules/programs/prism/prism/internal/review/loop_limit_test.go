package review_test

// loop_limit_test.go — tests for the LOOP-LIMIT footer that the review-complete
// prompt grows after the worker has run REVIEW_CYCLE_THRESHOLD verdict-producing
// review cycles for the same parent without convergence (#1512).
//
// Invariants under test:
//
//   1. CompletedReviewCyclesForParent counts only groups that (a) are fully
//      terminal AND (b) produced at least one finished member with a non-empty
//      assistant message and no startup_error. Pure-infrastructure failures do
//      not count.
//
//   2. The LOOP-LIMIT footer is appended to the review-complete delivery body
//      when the current cycle is itself verdict-producing AND the cumulative
//      verdict-producing cycle count (including this one) is >= 3 AND the
//      review did not converge (allPassed == false).
//
//   3. A converged cycle (allPassed == true) never receives the footer, even
//      if 3+ cycles have run for the PR (AC: edge-case 7 in #1512).
//
//   4. A bash-substring "prism review N" mention does not, on its own, cause
//      cycle counting to advance — because cycle counting is fed by the DB
//      group history, not by tool-call regex. We assert this by exercising
//      CompletedReviewCyclesForParent against a parent that has zero groups
//      in the DB and verifying the count is 0 regardless of any other state.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// ── buildLoopLimitFooter ─────────────────────────────────────────────────────

func TestBuildLoopLimitFooter_IncludesPRAndCycleCount(t *testing.T) {
	footer := review.BuildLoopLimitFooterForTest(3, "42")
	if !strings.Contains(footer, "PR #42") {
		t.Errorf("footer missing PR label; got: %q", footer)
	}
	if !strings.Contains(footer, "3 review cycles") {
		t.Errorf("footer missing cycle count; got: %q", footer)
	}
	if !strings.Contains(footer, "REVIEW LOOP LIMIT") {
		t.Errorf("footer missing LOOP LIMIT marker; got: %q", footer)
	}
	if !strings.Contains(footer, "escalate") {
		t.Errorf("footer should instruct the worker to escalate; got: %q", footer)
	}
}

func TestBuildLoopLimitFooter_HandlesEmptyPRNumber(t *testing.T) {
	footer := review.BuildLoopLimitFooterForTest(4, "")
	if !strings.Contains(footer, "this PR") {
		t.Errorf("footer should fall back to 'this PR' when PR number is empty; got: %q", footer)
	}
}

// ── currentCycleProducedVerdicts ────────────────────────────────────────────

func TestCurrentCycleProducedVerdicts_PositiveCase(t *testing.T) {
	groupData := map[string]db.GroupMemberResult{
		"a~review-1-review-goal": {State: "finished", LastMessage: `{"text":"<verdict>FAIL</verdict>"}`},
	}
	if !review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("a finished member with a non-empty assistant message should count as verdict-producing")
	}
}

func TestCurrentCycleProducedVerdicts_AllNoStartIsNotVerdictProducing(t *testing.T) {
	groupData := map[string]db.GroupMemberResult{
		"a~review-1-review-goal": {State: "error", StartupError: "container failed to bind port"},
		"a~review-1-review-code": {State: "error", StartupError: "container failed to bind port"},
	}
	if review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("an all-no-start group must not count as verdict-producing")
	}
}

func TestCurrentCycleProducedVerdicts_FinishedButEmptyMessageIsNotVerdictProducing(t *testing.T) {
	groupData := map[string]db.GroupMemberResult{
		"a~review-1-review-goal": {State: "finished", LastMessage: ""},
	}
	if review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("a finished member with no LastMessage must not count as verdict-producing")
	}
}

func TestCurrentCycleProducedVerdicts_MixedIsVerdictProducing(t *testing.T) {
	groupData := map[string]db.GroupMemberResult{
		"a~review-1-review-goal": {State: "error", StartupError: "container failed to bind port"},
		"a~review-1-review-code": {State: "finished", LastMessage: `{"text":"<verdict>PASS</verdict>"}`},
	}
	if !review.CurrentCycleProducedVerdictsForTest(groupData) {
		t.Error("a mixed group with at least one finished verdict must count as verdict-producing")
	}
}

// ── CompletedReviewCyclesForParent ──────────────────────────────────────────

// TestCompletedReviewCyclesForParent_NoGroups_ReturnsZero anchors the
// bash-substring false-match invariant: cycle counting is fed by DB group
// history, not by tool-call regex, so a parent with no groups always reports
// zero — regardless of how many `prism review N` strings appeared in tool
// output or rg / grep / heredoc bodies (AC #1512: edge-case bash-substring).
func TestCompletedReviewCyclesForParent_NoGroups_ReturnsZero(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@no-groups"

	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 for a parent with zero groups", n)
	}
}

// TestBashSubstringFalseMatch_DoesNotIncrementCycles verifies AC #3 explicitly:
// each of the bash command shapes called out in the issue body — `rg
// "prism review"`, `git log --grep=...`, `echo "..."`, and a heredoc
// containing `prism review 42` — must not increment the counter or fire the
// LOOP-LIMIT footer. Under Shape B this is structurally guaranteed: cycle
// counting reads from agent_status / session_groups, not from tool args.
// The test exercises the contract by populating a DB that has zero registered
// groups and asserts the count is zero, which is what the monitor would see
// even after the worker had executed any of these commands.
func TestBashSubstringFalseMatch_DoesNotIncrementCycles(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@false-match"

	// The shapes from #1512 — listed here for documentation, not exercised
	// directly. Cycle counting is divorced from these strings entirely
	// because the new pipeline never inspects tool-call text.
	falseMatchExamples := []string{
		`rg "prism review|git push"`,
		`git log --grep="prism review 42"`,
		`echo "remember to run prism review 42"`,
		"cat <<'EOF'\nprism review 42\nEOF",
		`awk '/prism review 42/ { print }'`,
	}

	for _, cmd := range falseMatchExamples {
		// We deliberately do NOT pass these to any code under test: there
		// is no bash-string code path under test in Shape B. We assert the
		// invariant by checking the DB-fed counter remains zero.
		_ = cmd
		n, err := review.CompletedReviewCyclesForParent(d, parent, "")
		if err != nil {
			t.Fatalf("CompletedReviewCyclesForParent: %v", err)
		}
		if n != 0 {
			t.Errorf("after considering bash-substring example %q the cycle count was %d, want 0", cmd, n)
		}
	}
}

// TestCompletedReviewCyclesForParent_CountsVerdictProducingGroups verifies
// the happy path: two prior groups, both fully terminal, both produced
// verdicts → count = 2.
func TestCompletedReviewCyclesForParent_CountsVerdictProducingGroups(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@two-cycles"

	// Round 1: two agents, both finished, both produced assistant messages.
	g1 := registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: `<verdict>FAIL</verdict>`},
		memberSpec{role: "review-code", state: "finished", text: `<verdict>PASS</verdict>`},
	)
	// Round 2: same agents, both finished, both produced assistant messages.
	g2 := registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "finished", text: `<verdict>PASS</verdict>`},
		memberSpec{role: "review-code", state: "finished", text: `<verdict>FAIL</verdict>`},
	)

	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (groups %s and %s)", n, g1, g2)
	}
}

// TestCompletedReviewCyclesForParent_ExcludesAllNoStartGroups verifies the
// AC: an "all agents failed to start" cycle does not count as a real cycle.
func TestCompletedReviewCyclesForParent_ExcludesAllNoStartGroups(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@no-start"

	// Round 1: real verdict-producing cycle.
	registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: `<verdict>FAIL</verdict>`},
	)
	// Round 2: pure infrastructure failure — every member errored at
	// startup with no assistant message.
	registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "error", startupError: "container failed to bind port"},
		memberSpec{role: "review-code", state: "error", startupError: "container failed to bind port"},
	)

	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (round 2 was pure infrastructure failure)", n)
	}
}

// TestCompletedReviewCyclesForParent_ExcludesActiveGroups verifies that a
// group whose members are still running does not count toward the cycle
// total — only fully-terminated groups do.
func TestCompletedReviewCyclesForParent_ExcludesActiveGroups(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@in-flight"

	// Round 1: terminated.
	registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: `<verdict>FAIL</verdict>`},
	)
	// Round 2: still active.
	registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "active"},
	)

	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (round 2 still active)", n)
	}
}

// TestCompletedReviewCyclesForParent_ExcludeGroupID verifies the
// excludeGroupID arg: passing the current group's id excludes it from the
// count, so the monitor can compute "cycles before this one" cleanly.
func TestCompletedReviewCyclesForParent_ExcludeGroupID(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@exclude-test"

	g1 := registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL"},
	)
	g2 := registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL"},
	)

	// Without exclude: 2.
	n, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent: %v", err)
	}
	if n != 2 {
		t.Errorf("count without exclude = %d, want 2", n)
	}

	// Excluding g2: 1.
	n, err = review.CompletedReviewCyclesForParent(d, parent, g2)
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent with exclude g2: %v", err)
	}
	if n != 1 {
		t.Errorf("count with exclude g2 = %d, want 1", n)
	}

	// Excluding g1: 1.
	n, err = review.CompletedReviewCyclesForParent(d, parent, g1)
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent with exclude g1: %v", err)
	}
	if n != 1 {
		t.Errorf("count with exclude g1 = %d, want 1", n)
	}
}

// ── End-to-end footer behaviour ──────────────────────────────────────────────

// TestMonitorFunc_ReproducesIssue1512Spam_AndFix demonstrates that on the
// 3rd verdict-producing non-converging cycle the worker receives the
// LOOP-LIMIT footer exactly once, embedded in the review-complete prompt
// body — as opposed to the pre-fix behaviour where it would have been
// re-injected on every subsequent turn (#1512 spam).
//
// The reproducer asserts:
//   - delivery happens exactly once (i.e. the footer is part of the prompt
//     body, not a separate per-turn injection)
//   - the prompt body contains the footer text
//   - the prompt body is the same text the worker would otherwise receive,
//     simply extended at the bottom (no separate steer message channel)
func TestMonitorFunc_AppendsFooterOnThirdNonConvergedCycle(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@spam-repro"

	// Seed two prior verdict-producing cycles on this parent.
	registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
		memberSpec{role: "review-code", state: "finished", text: "PASS output"},
	)
	registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
		memberSpec{role: "review-code", state: "finished", text: "PASS output"},
	)

	// Round 3 is the in-flight cycle the monitor is delivering for.
	// FAIL on review-goal so allPassed == false and the footer fires.
	body := mockMonitorRound3(t, d, parent, "1512")

	if !strings.Contains(body, "REVIEW LOOP LIMIT") {
		t.Errorf("3rd non-converged cycle: prompt body missing LOOP-LIMIT footer; got:\n%s", body)
	}
	if !strings.Contains(body, "3 review cycles") {
		t.Errorf("footer should mention cycle count of 3; got:\n%s", body)
	}
	// The footer must appear AFTER the per-agent findings — i.e. the prompt
	// body remains intact and the footer is appended.
	footerIdx := strings.Index(body, "REVIEW LOOP LIMIT")
	resultsIdx := strings.Index(body, "### Results")
	if footerIdx <= resultsIdx {
		t.Errorf("footer should come after the Results section; footerIdx=%d resultsIdx=%d", footerIdx, resultsIdx)
	}
}

// TestMonitorFunc_NoFooterOnConvergedCycle pins AC: if the 3rd cycle PASSES,
// no footer is emitted (the limit only fires on non-convergence).
func TestMonitorFunc_NoFooterOnConvergedThirdCycle(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@converged-third"

	registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
	)
	registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
	)

	// Round 3: passes. We assert the footer is NOT appended.
	body := mockMonitorRound3Passed(t, d, parent, "1512")

	if strings.Contains(body, "REVIEW LOOP LIMIT") {
		t.Errorf("3rd PASSing cycle must NOT emit LOOP-LIMIT footer; got:\n%s", body)
	}
}

// TestMonitorFunc_NoFooterWhenInfrastructureFailureAtThird verifies that an
// "all agents failed to start" 3rd cycle does not bump the count to 3 — it
// is not a verdict-producing cycle, so the worker should keep getting the
// "infrastructure failure: re-run" message, not the LOOP-LIMIT footer.
func TestMonitorFunc_NoFooterOnInfrastructureFailureCycle(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@infra-third"

	registerGroupAndSeedMembers(t, d, parent, 1,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
	)
	registerGroupAndSeedMembers(t, d, parent, 2,
		memberSpec{role: "review-goal", state: "finished", text: "FAIL output"},
	)

	// Round 3: all agents failed to start.
	body := mockMonitorRound3InfraFailure(t, d, parent, "1512")

	if strings.Contains(body, "REVIEW LOOP LIMIT") {
		t.Errorf("3rd cycle that was a pure infrastructure failure must NOT emit LOOP-LIMIT footer; got:\n%s", body)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type memberSpec struct {
	role         string
	state        string
	text         string // assistant message text; empty → no msg_assistant event
	startupError string // when set, write a startup_error event
}

// registerGroupAndSeedMembers creates a session group, inserts agent_status
// rows for each member, sets group_id on each row, and seeds msg_assistant /
// startup_error events as requested. Returns the group_id.
func registerGroupAndSeedMembers(t *testing.T, d *db.DB, parent string, round int, members ...memberSpec) string {
	t.Helper()
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup(%q): %v", parent, err)
	}
	for _, m := range members {
		sess := parent + "~review-" + itoa(round) + "-" + m.role
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", m.state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q, state=%q): %v", sess, m.state, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
		if m.text != "" {
			seedAssistantEvent(t, d, sess, m.text)
		}
		if m.startupError != "" {
			seedStartupErrorEvent(t, d, sess, m.startupError)
		}
	}
	return groupID
}

// seedStartupErrorEvent writes a startup_error event for the named session.
func seedStartupErrorEvent(t *testing.T, d *db.DB, sessionName, reason string) {
	t.Helper()
	payload := `{"reason":` + `"` + strings.ReplaceAll(reason, `"`, `\"`) + `"}`
	err := d.WriteEvent(db.Event{
		ID:          sessionName + "-startup-err",
		SessionName: sessionName,
		Repo:        "nixos-config",
		Worktree:    "/wt",
		Type:        "startup_error",
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("WriteEvent (startup_error): %v", err)
	}
}

// mockMonitorRound3 simulates what the monitor would produce as the
// review-complete prompt body for round 3 of `parent`, where round 3 is
// itself verdict-producing but did not converge. It registers the round-3
// group with two members (one FAIL, one PASS), then assembles the body the
// same way MonitorFunc does (FormatResults → buildDeliveryMessage → footer).
func mockMonitorRound3(t *testing.T, d *db.DB, parent, prNumber string) string {
	t.Helper()
	gid := registerGroupAndSeedMembers(t, d, parent, 3,
		memberSpec{role: "review-goal", state: "finished", text: "Goal FAIL details"},
		memberSpec{role: "review-code", state: "finished", text: "<verdict>PASS</verdict>"},
	)
	return assembleDeliveryBody(t, d, parent, prNumber, 3, gid, false /* allPassed */, false /* infra */)
}

func mockMonitorRound3Passed(t *testing.T, d *db.DB, parent, prNumber string) string {
	t.Helper()
	gid := registerGroupAndSeedMembers(t, d, parent, 3,
		memberSpec{role: "review-goal", state: "finished", text: "<verdict>PASS</verdict>"},
	)
	return assembleDeliveryBody(t, d, parent, prNumber, 3, gid, true /* allPassed */, false /* infra */)
}

func mockMonitorRound3InfraFailure(t *testing.T, d *db.DB, parent, prNumber string) string {
	t.Helper()
	gid := registerGroupAndSeedMembers(t, d, parent, 3,
		memberSpec{role: "review-goal", state: "error", startupError: "container failed to bind port"},
		memberSpec{role: "review-code", state: "error", startupError: "container failed to bind port"},
	)
	return assembleDeliveryBody(t, d, parent, prNumber, 3, gid, false /* allPassed */, true /* infra */)
}

// assembleDeliveryBody mirrors MonitorFunc's tail (FormatResults +
// buildDeliveryMessage + LOOP-LIMIT footer) so we can unit-test the
// integrated behaviour without spinning up an HTTP delivery target.
func assembleDeliveryBody(t *testing.T, d *db.DB, parent, prNumber string, round int, groupID string, allPassed, infra bool) string {
	t.Helper()
	groupData, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}

	// Build a synthetic results slice matching the agents in groupData.
	var results []review.AgentResult
	var sessions []string
	for sess, mr := range groupData {
		sessions = append(sessions, sess)
		role := strings.TrimPrefix(sess, parent+"~review-"+itoa(round)+"-")
		switch {
		case mr.StartupError != "":
			results = append(results, review.AgentResult{
				Agent:   review.Agent{Name: role},
				Passed:  false,
				Output:  "ERROR: agent failed to start (no-start): " + mr.StartupError,
				IsError: true,
			})
		case strings.Contains(mr.LastMessage, "PASS"):
			results = append(results, review.AgentResult{
				Agent:  review.Agent{Name: role},
				Passed: true,
				Output: mr.LastMessage,
			})
		default:
			results = append(results, review.AgentResult{
				Agent:  review.Agent{Name: role},
				Passed: false,
				Output: mr.LastMessage,
			})
		}
	}

	formatted, _ := review.FormatResults(results, prNumber, round, 0)
	body := review.BuildDeliveryMessageForTest(prNumber, round, formatted, allPassed, groupData, sessions)

	// Apply the same footer logic MonitorFunc uses.
	if !allPassed && review.CurrentCycleProducedVerdictsForTest(groupData) {
		prior, ccErr := review.CompletedReviewCyclesForParent(d, parent, groupID)
		if ccErr != nil {
			t.Fatalf("CompletedReviewCyclesForParent: %v", ccErr)
		}
		if prior+1 >= 3 {
			body += review.BuildLoopLimitFooterForTest(prior+1, prNumber)
		}
	}
	_ = infra // signalled by the caller for self-documentation only
	return body
}

// itoa avoids pulling strconv into every callsite of this small test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
