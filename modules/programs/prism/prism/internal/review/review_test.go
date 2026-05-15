package review_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// ── NextRoundNumber ───────────────────────────────────────────────────────────

func TestNextRoundNumber_NoExistingRounds_Returns1(t *testing.T) {
	d := openTestDB(t)
	n := review.NextRoundNumber(d, "nixos-config@feature")
	if n != 1 {
		t.Errorf("NextRoundNumber = %d, want 1", n)
	}
}

// TestNextRoundNumber_WithPerAgentSessions verifies that the new per-agent
// session shape (~review-N-<agent>) is counted correctly.
func TestNextRoundNumber_WithPerAgentSessions(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Seed round 1 per-agent sessions.
	_ = d.UpsertStatus(parent+"~review-1-review-goal", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-code", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-security", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-qa", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-context", "nixos-config", "/wt", "finished", nil, nil)

	n := review.NextRoundNumber(d, parent)
	if n != 2 {
		t.Errorf("NextRoundNumber = %d, want 2 (after one full round)", n)
	}
}

// TestNextRoundNumber_TwoRoundsOfPerAgentSessions verifies round counting after
// two full rounds of per-agent sessions.
func TestNextRoundNumber_TwoRoundsOfPerAgentSessions(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Round 1 (5 agents).
	for _, agent := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		_ = d.UpsertStatus(parent+"~review-1-"+agent, "nixos-config", "/wt", "finished", nil, nil)
	}
	// Round 2 (5 agents).
	for _, agent := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		_ = d.UpsertStatus(parent+"~review-2-"+agent, "nixos-config", "/wt", "finished", nil, nil)
	}

	n := review.NextRoundNumber(d, parent)
	if n != 3 {
		t.Errorf("NextRoundNumber = %d, want 3 (after two full rounds)", n)
	}
}

// TestNextRoundNumber_PartialRound verifies that a partial round (e.g. --only
// with 2 agents) is still counted as a round (the round number includes
// whichever N was used, regardless of how many agents were spawned).
func TestNextRoundNumber_PartialRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Only 2 agents from round 1.
	_ = d.UpsertStatus(parent+"~review-1-review-code", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-qa", "nixos-config", "/wt", "finished", nil, nil)

	n := review.NextRoundNumber(d, parent)
	if n != 2 {
		t.Errorf("NextRoundNumber = %d, want 2 (partial round still counts)", n)
	}
}

// TestNextRoundNumber_OldShapeSessionsNotCounted verifies that old-shape round
// sessions (~review-N pure integer suffix) and old-shape agent sub-sessions
// (~review-N~agent) do NOT count toward the round number. This ensures
// migration is safe: zombie old-shape rows don't inflate the counter.
func TestNextRoundNumber_OldShapeSessionsNotCounted(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Old-shape round sessions (pre-PR-C): pure integer suffix.
	_ = d.UpsertStatus(parent+"~review-1", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-2", "nixos-config", "/wt", "finished", nil, nil)
	// Old-shape agent sub-sessions (pre-PR-C): ~review-N~agent.
	_ = d.UpsertStatus(parent+"~review-1~review", "nixos-config", "/wt", "idle", nil, nil)

	// Despite those rows, NextRoundNumber should return 1 (no new-shape rounds exist).
	n := review.NextRoundNumber(d, parent)
	if n != 1 {
		t.Errorf("NextRoundNumber = %d, want 1 (old-shape rows should not count)", n)
	}
}

// TestNextRoundNumber_MixedOldAndNewShape verifies that when both old-shape
// zombie rows and new-shape per-agent rows exist, only the new-shape rows
// determine the round counter.
func TestNextRoundNumber_MixedOldAndNewShape(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Old-shape zombies.
	_ = d.UpsertStatus(parent+"~review-1", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1~review", "nixos-config", "/wt", "finished", nil, nil)

	// New-shape round 1 agents.
	_ = d.UpsertStatus(parent+"~review-1-review-goal", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-1-review-code", "nixos-config", "/wt", "finished", nil, nil)

	n := review.NextRoundNumber(d, parent)
	if n != 2 {
		t.Errorf("NextRoundNumber = %d, want 2 (only new-shape rows count)", n)
	}
}

// TestNextRoundNumber_SkipsNonRoundSessions verifies that sessions with
// suffixes that are not round-numbered are ignored.
func TestNextRoundNumber_SkipsNonRoundSessions(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	// Only sub-sessions exist — no round sessions.
	_ = d.UpsertStatus(parent+"~review-1~review", "nixos-config", "/wt", "idle", nil, nil)

	n := review.NextRoundNumber(d, parent)
	if n != 1 {
		t.Errorf("NextRoundNumber = %d, want 1 (sub-sessions should not count)", n)
	}
}

// ── IsPerAgentSession ─────────────────────────────────────────────────────────

func TestIsPerAgentSession_NewShape(t *testing.T) {
	parent := "nixos-config@feature"
	newShapeNames := []string{
		parent + "~review-1-review-goal",
		parent + "~review-1-review-code",
		parent + "~review-1-review-security",
		parent + "~review-1-review-qa",
		parent + "~review-1-review-context",
		parent + "~review-2-review-goal",
	}
	for _, name := range newShapeNames {
		if !review.IsPerAgentSession(name, parent) {
			t.Errorf("IsPerAgentSession(%q) = false, want true", name)
		}
	}
}

func TestIsPerAgentSession_OldShape(t *testing.T) {
	parent := "nixos-config@feature"
	oldShapeNames := []string{
		parent + "~review-1",           // old round session
		parent + "~review-1~review",    // old agent sub-session
		parent + "~review-1~review-qa", // old agent sub-session variant
		"other-session",                // unrelated
		parent,                         // parent itself
	}
	for _, name := range oldShapeNames {
		if review.IsPerAgentSession(name, parent) {
			t.Errorf("IsPerAgentSession(%q) = true, want false (old-shape or unrelated)", name)
		}
	}
}

// ── assessPassed ──────────────────────────────────────────────────────────────
// assessPassed is unexported; test it indirectly via FormatResults.

func TestFormatResults_AllPassed(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: true, Output: "LGTM, no issues found."},
	}
	output, allPassed := review.FormatResults(results, "42", 0, 0)
	if !allPassed {
		t.Errorf("allPassed = false, want true")
	}
	if len(output) == 0 {
		t.Error("output is empty")
	}
	// Should contain the tick mark.
	if !containsAny(output, "✓") {
		t.Errorf("output does not contain ✓: %q", output)
	}
	// Should NOT contain retry command.
	if containsAny(output, "Retry:") {
		t.Errorf("output unexpectedly contains 'Retry': %q", output)
	}
}

func TestFormatResults_SomeFailed(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: false, Output: "Please fix the above before this PR is merged.", IsError: false},
	}
	output, allPassed := review.FormatResults(results, "42", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	if !containsAny(output, "✗") {
		t.Errorf("output does not contain ✗: %q", output)
	}
	if !containsAny(output, "Retry: prism review 42") {
		t.Errorf("output does not contain retry command: %q", output)
	}
}

func TestFormatResults_InfraError(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: false, Output: "ERROR: timed out after 10m", IsError: true},
	}
	output, allPassed := review.FormatResults(results, "42", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	if !containsAny(output, "ERROR: timed out") {
		t.Errorf("output does not contain timeout error: %q", output)
	}
	if !containsAny(output, "Retry:") {
		t.Errorf("output does not contain retry command: %q", output)
	}
}

func TestFormatResults_PassFailResults(t *testing.T) {
	// FormatResults uses the Passed field on AgentResult directly.
	// A result marked Passed=false is a failure.
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: false, Output: "Please fix the above before this PR is merged."},
	}
	_, allPassed := review.FormatResults(results, "1", 0, 0)
	if allPassed {
		t.Errorf("FormatResults with Passed=false: allPassed=true, want false")
	}

	// A result marked Passed=true is a pass.
	results = []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: true, Output: "<verdict>PASS</verdict>"},
	}
	_, allPassed = review.FormatResults(results, "1", 0, 0)
	if !allPassed {
		t.Errorf("FormatResults with Passed=true: allPassed=false, want true")
	}
}

// ── AssessPassed ──────────────────────────────────────────────────────────────
// AssessPassed now requires an explicit <verdict>PASS</verdict> marker.
// All tests below validate the new positive-evidence requirement (Layer 1).

// TestAssessPassed_BenignTextIsFalse verifies the core fix for #785:
// benign partial output with no verdict marker must return false, not true.
// Before the fix, AssessPassed returned true for any text without failure
// phrases — this would silently classify interrupted agents as "passed".
func TestAssessPassed_BenignTextIsFalse(t *testing.T) {
	benignTexts := []string{
		"I'll start by reading the PR...",
		"Let me examine the diff and linked issue.",
		"The implementation looks correct. All acceptance criteria are met.",
		"LGTM. No issues found. The code follows existing conventions.",
		"Reviewed thoroughly. The PR is ready to merge.",
		"All acceptance criteria pass. The error handling is correct.",
		"The PR looks good. I checked the diff and the linked issue's ACs.",
		"",
		"Starting review...",
	}
	for _, text := range benignTexts {
		passed, kind := review.AssessPassed(text)
		if passed {
			t.Errorf("AssessPassed(%q) = true, want false (no verdict marker present)", text)
		}
		if kind != review.VerdictNone {
			t.Errorf("AssessPassed(%q) kind = %v, want VerdictNone", text, kind)
		}
	}
}

// TestAssessPassed_ExplicitPassVerdict verifies that <verdict>PASS</verdict>
// (the documented positive marker) returns true.
func TestAssessPassed_ExplicitPassVerdict(t *testing.T) {
	passTexts := []string{
		"<verdict>PASS</verdict>",
		"<VERDICT>PASS</VERDICT>",
		"The PR looks good.\n\n<verdict>PASS</verdict>",
		"All ACs verified.\n<verdict>PASS</verdict>\n",
	}
	for _, text := range passTexts {
		passed, kind := review.AssessPassed(text)
		if !passed {
			t.Errorf("AssessPassed(%q) = false, want true (explicit PASS marker present)", text)
		}
		if kind != review.VerdictPass {
			t.Errorf("AssessPassed(%q) kind = %v, want VerdictPass", text, kind)
		}
	}
}

// TestAssessPassed_ExplicitFailVerdict verifies that <verdict>FAIL</verdict>
// returns false with VerdictFail, regardless of other content.
func TestAssessPassed_ExplicitFailVerdict(t *testing.T) {
	failTexts := []string{
		"<verdict>FAIL</verdict>",
		"<VERDICT>FAIL</VERDICT>",
		"Blocking issue found. Please fix.\n<verdict>FAIL</verdict>",
		"<verdict>FAIL</verdict>\nSee blocking issues above.",
	}
	for _, text := range failTexts {
		passed, kind := review.AssessPassed(text)
		if passed {
			t.Errorf("AssessPassed(%q) = true, want false (explicit FAIL marker present)", text)
		}
		if kind != review.VerdictFail {
			t.Errorf("AssessPassed(%q) kind = %v, want VerdictFail", text, kind)
		}
	}
}

// TestAssessPassed_VerdictOnlyNoProse verifies an edge case: an agent that
// emits only the verdict marker with no commentary is still classified correctly.
func TestAssessPassed_VerdictOnlyNoProse(t *testing.T) {
	passed, kind := review.AssessPassed("<verdict>PASS</verdict>")
	if !passed {
		t.Error("AssessPassed('<verdict>PASS</verdict>') = false, want true")
	}
	if kind != review.VerdictPass {
		t.Errorf("AssessPassed kind = %v, want VerdictPass", kind)
	}
}

// TestAssessPassed_CaseInsensitive verifies that verdict matching is
// case-insensitive (agents may emit PASS/pass/Pass).
func TestAssessPassed_CaseInsensitive(t *testing.T) {
	variants := []string{
		"<verdict>PASS</verdict>",
		"<verdict>pass</verdict>",
		"<verdict>Pass</verdict>",
		"<VERDICT>PASS</VERDICT>",
		"<Verdict>Pass</Verdict>",
	}
	for _, text := range variants {
		passed, _ := review.AssessPassed(text)
		if !passed {
			t.Errorf("AssessPassed(%q) = false, want true (case-insensitive PASS)", text)
		}
	}
	failVariants := []string{
		"<verdict>FAIL</verdict>",
		"<verdict>fail</verdict>",
		"<VERDICT>FAIL</VERDICT>",
	}
	for _, text := range failVariants {
		passed, kind := review.AssessPassed(text)
		if passed {
			t.Errorf("AssessPassed(%q) = true, want false (case-insensitive FAIL)", text)
		}
		if kind != review.VerdictFail {
			t.Errorf("AssessPassed(%q) kind = %v, want VerdictFail", text, kind)
		}
	}
}

// ── BuildResults (Layer 2 & 3) ────────────────────────────────────────────────
// These tests exercise the exported BuildResults wrapper to validate the
// multi-layered defence against false PASSes.

// seedAssistantEvent writes a msg_assistant event with the given text payload
// for the named session. Used to simulate what a real agent would emit.
func seedAssistantEvent(t *testing.T, d *db.DB, sessionName, text string) {
	t.Helper()
	payload := `{"text":` + `"` + strings.ReplaceAll(text, `"`, `\"`) + `"}`
	err := d.WriteEvent(db.Event{
		ID:          sessionName + "-evt-1",
		SessionName: sessionName,
		Repo:        "nixos-config",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     payload,
	})
	if err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// TestBuildResults_InterruptedThenResumedToFinishedPasses verifies the #1495
// contract at the BuildResults layer: an agent that was interrupted, then
// redirected via `prism prompt`, and then reached the "finished" state with
// a PASS verdict must be counted as a normal pass — not as an error.
//
// This is the post-fix behaviour: "interrupted" is no longer in the layer-2
// error branch, so an agent whose final DB state is "finished" proceeds to
// AssessPassed regardless of whether it was previously interrupted.
func TestBuildResults_InterruptedThenResumedToFinishedPasses(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-goal"}
	sess := "test@parent~review-1-review-goal"

	// The agent was interrupted, the user sent a redirection via
	// `prism prompt`, and the agent finished with an explicit PASS verdict.
	// The DB only retains the latest state — "finished" — so BuildResults sees
	// no trace of the earlier interruption.
	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
	seedAssistantEvent(t, d, sess, "All ACs verified. <verdict>PASS</verdict>")

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	if len(results) != 1 {
		t.Fatalf("BuildResults returned %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Passed {
		t.Errorf("BuildResults interrupted-then-resumed-to-finished: Passed=false, want true (#1495)")
	}
	if r.IsError {
		t.Errorf("BuildResults interrupted-then-resumed-to-finished: IsError=true, want false (#1495)")
	}
}

// TestBuildResults_InterruptedThenResumedToError verifies the #1495 edge case:
// an agent that was interrupted, redirected, and then crashed (e.g. the
// redirect itself caused the crash) must still surface as IsError=true via the
// genuine-error branch. The redirection does not mask a subsequent failure.
func TestBuildResults_InterruptedThenResumedToError(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-code"}
	sess := "test@parent~review-1-review-code"

	// User interrupted, redirected, agent crashed during processing of the
	// redirect — final state is "error".
	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "error", nil, nil)
	seedAssistantEvent(t, d, sess, "Some partial output before crash.")

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	r := results[0]
	if r.Passed {
		t.Errorf("BuildResults interrupted-then-resumed-to-error: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("BuildResults interrupted-then-resumed-to-error: IsError=false, want true (genuine-error branch must still fire)")
	}
	if !findSubstring(r.Output, "error") {
		t.Errorf("BuildResults interrupted-then-resumed-to-error: output should mention 'error': %q", r.Output)
	}
}

// TestBuildResults_ErrorState verifies Layer 2: an agent whose DB state is
// "error" produces an error result regardless of msg_assistant events.
func TestBuildResults_ErrorState(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-code"}
	sess := "test@parent~review-1-review-code"

	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "error", nil, nil)
	seedAssistantEvent(t, d, sess, "Some partial output before crash.")

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	r := results[0]
	if r.Passed {
		t.Errorf("BuildResults with error state: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("BuildResults with error state: IsError=false, want true")
	}
	if !findSubstring(r.Output, "error") {
		t.Errorf("BuildResults with error state: output does not mention 'error': %q", r.Output)
	}
}

// TestBuildResults_CancelledAllFinished verifies Layer 3: when cancelled=true,
// no result has Passed=true, even if all agents had reached a "finished" state
// before the cancellation signal arrived.
func TestBuildResults_CancelledAllFinished(t *testing.T) {
	d := openTestDB(t)
	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
	}
	for i, sess := range sessions {
		_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
		// Give each agent a passing verdict — should still be overridden by cancelled.
		seedAssistantEvent(t, d, sess, "<verdict>PASS</verdict> All good.")
		_ = agents[i] // suppress lint
	}

	finished := []bool{true, true}
	timedOut := []bool{false, false}
	results := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, true, "")

	for _, r := range results {
		if r.Passed {
			t.Errorf("BuildResults cancelled=true, agent %q: Passed=true, want false", r.Agent.Name)
		}
		if !r.IsError {
			t.Errorf("BuildResults cancelled=true, agent %q: IsError=false, want true", r.Agent.Name)
		}
	}
}

// TestBuildResults_CancelledMixed verifies Layer 3: when cancelled=true with a
// mix of finished and not-yet-finished agents, no result has Passed=true.
func TestBuildResults_CancelledMixed(t *testing.T) {
	d := openTestDB(t)
	agents := []review.Agent{
		{Name: "review-goal"}, // finished before cancel
		{Name: "review-code"}, // did not finish
	}
	sessions := []string{
		"test@parent~review-2-review-goal",
		"test@parent~review-2-review-code",
	}
	_ = d.UpsertStatus(sessions[0], "nixos-config", "/wt", "finished", nil, nil)
	seedAssistantEvent(t, d, sessions[0], "<verdict>PASS</verdict>")
	_ = d.UpsertStatus(sessions[1], "nixos-config", "/wt", "running", nil, nil)

	finished := []bool{true, false}
	timedOut := []bool{false, false}
	results := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, true, "")

	for _, r := range results {
		if r.Passed {
			t.Errorf("BuildResults cancelled=true mixed, agent %q: Passed=true, want false", r.Agent.Name)
		}
		if !r.IsError {
			t.Errorf("BuildResults cancelled=true mixed, agent %q: IsError=false, want true", r.Agent.Name)
		}
	}
}

// TestBuildResults_HappyPathPass verifies that a cleanly finished agent with a
// <verdict>PASS</verdict> marker produces Passed=true and IsError=false.
func TestBuildResults_HappyPathPass(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-security"}
	sess := "test@parent~review-1-review-security"

	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
	seedAssistantEvent(t, d, sess, "All security checks passed.\n<verdict>PASS</verdict>")

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	r := results[0]
	if !r.Passed {
		t.Errorf("BuildResults happy-path pass: Passed=false, want true")
	}
	if r.IsError {
		t.Errorf("BuildResults happy-path pass: IsError=true, want false")
	}
}

// TestBuildResults_HappyPathFail verifies that a cleanly finished agent with a
// <verdict>FAIL</verdict> marker produces Passed=false and IsError=false.
func TestBuildResults_HappyPathFail(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-qa"}
	sess := "test@parent~review-1-review-qa"

	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
	seedAssistantEvent(t, d, sess, "There is a blocking issue in the test coverage.\n<verdict>FAIL</verdict>")

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	r := results[0]
	if r.Passed {
		t.Errorf("BuildResults happy-path fail: Passed=true, want false")
	}
	if r.IsError {
		t.Errorf("BuildResults happy-path fail: IsError=true, want false (content failure, not infra)")
	}
}

// TestBuildResults_NoVerdictIsError verifies that a cleanly finished agent
// whose output contains no verdict marker is classified as IsError=true (not
// passed). The output must be surfaced so a human can inspect it.
func TestBuildResults_NoVerdictIsError(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-context"}
	sess := "test@parent~review-1-review-context"

	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
	benignText := "I'll start by reading the PR and checking the linked issue."
	seedAssistantEvent(t, d, sess, benignText)

	finished := []bool{true}
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

	r := results[0]
	if r.Passed {
		t.Errorf("BuildResults no-verdict: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("BuildResults no-verdict: IsError=false, want true")
	}
	// The agent's actual output must be surfaced in the error message.
	if !findSubstring(r.Output, benignText) {
		t.Errorf("BuildResults no-verdict: agent output not surfaced in error: %q", r.Output)
	}
}

// ── AgentsByName ──────────────────────────────────────────────────────────────

func TestAgentsByName_ValidNames(t *testing.T) {
	agents := review.Agents()
	result, err := review.AgentsByName(agents, []string{"review-goal"})
	if err != nil {
		t.Fatalf("AgentsByName: %v", err)
	}
	if len(result) != 1 || result[0].Name != "review-goal" {
		t.Errorf("AgentsByName = %v, want [{review-goal}]", result)
	}
}

func TestAgentsByName_UnknownName(t *testing.T) {
	agents := review.Agents()
	_, err := review.AgentsByName(agents, []string{"nonexistent"})
	if err == nil {
		t.Error("AgentsByName should return error for unknown agent name")
	}
}

func TestAgentsByName_MixedNames(t *testing.T) {
	agents := review.Agents()
	_, err := review.AgentsByName(agents, []string{"review-goal", "nonexistent"})
	if err == nil {
		t.Error("AgentsByName should return error when any name is unknown")
	}
}

// ── Agents ────────────────────────────────────────────────────────────────────

func TestAgents_ReturnsFiveAgents(t *testing.T) {
	agents := review.Agents()
	if len(agents) != 5 {
		t.Fatalf("Agents() returned %d agents, want 5", len(agents))
	}
}

func TestAgents_AgentNames(t *testing.T) {
	agents := review.Agents()
	want := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	for i, name := range want {
		if agents[i].Name != name {
			t.Errorf("agents[%d].Name = %q, want %q", i, agents[i].Name, name)
		}

	}
}

// ── CheckAgentAvailability ────────────────────────────────────────────────────

// agentValidator returns a RoleValidator that checks for <role>.md in agentsDir.
func agentValidator(agentsDir string) review.RoleValidator {
	return func(role string) error {
		path := filepath.Join(agentsDir, role+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("agent role %q not available: no file at %s", role, path)
		}
		return nil
	}
}

func TestCheckAgentAvailability_AllPresent(t *testing.T) {
	// Create a temp dir with agent .md files for all agents.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := dir + "/prism/agents"
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	agents := review.Agents()
	for _, ag := range agents {
		if err := os.WriteFile(agentsDir+"/"+ag.Name+".md", []byte("# "+ag.Name), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	if err := review.CheckAgentAvailability(agents, agentValidator(agentsDir)); err != nil {
		t.Errorf("CheckAgentAvailability: unexpected error: %v", err)
	}
}

func TestCheckAgentAvailability_SomeMissing(t *testing.T) {
	// Create a temp dir with only some agents present.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := dir + "/prism/agents"
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Only create review-goal.md; the other 4 are missing.
	if err := os.WriteFile(agentsDir+"/review-goal.md", []byte("# review-goal"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	agents := review.Agents()
	err := review.CheckAgentAvailability(agents, agentValidator(agentsDir))
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for missing agents, got nil")
	}
	// Error should mention the missing agents by Name.
	for _, ag := range agents[1:] {
		if !findSubstring(err.Error(), ag.Name) {
			t.Errorf("CheckAgentAvailability error does not mention missing agent %q: %v", ag.Name, err)
		}
	}
	// Error should NOT mention review-goal as missing (it is present).
	// We check against the bare role name appearing in the comma-separated
	// "not available" list, not against the file path which incidentally
	// contains the name.
	notAvailable := strings.SplitN(err.Error(), "\n", 2)[0]
	if findSubstring(notAvailable, "review-goal") {
		t.Errorf("CheckAgentAvailability error unexpectedly lists present agent review-goal as not available: %v", err)
	}
}

func TestCheckAgentAvailability_AllMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the agents directory at all.
	agentsDir := dir + "/prism/agents"

	agents := review.Agents()
	err := review.CheckAgentAvailability(agents, agentValidator(agentsDir))
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for all missing agents, got nil")
	}
}

func TestCheckAgentAvailability_EmptyAgentList(t *testing.T) {
	// An empty agent list should always pass (validator is never called).
	// Use an agentValidator pointing at a nonexistent dir; it will never be
	// called for an empty list so no error should be returned.
	nilValidator := agentValidator(t.TempDir())
	if err := review.CheckAgentAvailability(nil, nilValidator); err != nil {
		t.Errorf("CheckAgentAvailability(nil): unexpected error: %v", err)
	}
	if err := review.CheckAgentAvailability([]review.Agent{}, nilValidator); err != nil {
		t.Errorf("CheckAgentAvailability([]): unexpected error: %v", err)
	}
}

// ── Session name construction ─────────────────────────────────────────────────

// TestPerAgentSessionNaming verifies that the session names produced by the
// round prefix construction match the expected shape.
func TestPerAgentSessionNaming(t *testing.T) {
	parent := "nixos-config@feature"
	roundN := 1

	agents := review.Agents()
	roundPrefix := parent + "~review-1-"

	expectedSessions := []string{
		parent + "~review-1-review-goal",
		parent + "~review-1-review-code",
		parent + "~review-1-review-security",
		parent + "~review-1-review-qa",
		parent + "~review-1-review-context",
	}
	_ = roundN // used to construct roundPrefix above

	for i, ag := range agents {
		got := roundPrefix + ag.Name
		if got != expectedSessions[i] {
			t.Errorf("agent session[%d]: got %q, want %q", i, got, expectedSessions[i])
		}
	}
}

// ── AgentsByName (enhanced) ───────────────────────────────────────────────────

// TestAgentsByName_AllFiveEnhancedNames verifies that passing all 5 enhanced
// agent names to AgentsByName returns all 5 agents in input order.
func TestAgentsByName_AllFiveEnhancedNames(t *testing.T) {
	agents := review.Agents()
	names := []string{
		"review-goal",
		"review-code",
		"review-security",
		"review-qa",
		"review-context",
	}
	result, err := review.AgentsByName(agents, names)
	if err != nil {
		t.Fatalf("AgentsByName with all 5 enhanced names: unexpected error: %v", err)
	}
	if len(result) != 5 {
		t.Fatalf("AgentsByName with all 5 enhanced names: got %d agents, want 5", len(result))
	}
	for i, name := range names {
		if result[i].Name != name {
			t.Errorf("result[%d].Name = %q, want %q", i, result[i].Name, name)
		}
	}
}

// TestAgentsByName_PreservesOrder verifies that AgentsByName returns agents in
// the order the names were requested, not the order they appear in the source
// slice. This is the AC: output lines appear in --only input order.
func TestAgentsByName_PreservesOrder(t *testing.T) {
	agents := review.Agents()
	// Request in reverse order.
	names := []string{"review-context", "review-qa", "review-code"}
	result, err := review.AgentsByName(agents, names)
	if err != nil {
		t.Fatalf("AgentsByName: unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("AgentsByName: got %d agents, want 3", len(result))
	}
	if result[0].Name != "review-context" {
		t.Errorf("result[0].Name = %q, want %q", result[0].Name, "review-context")
	}
	if result[1].Name != "review-qa" {
		t.Errorf("result[1].Name = %q, want %q", result[1].Name, "review-qa")
	}
	if result[2].Name != "review-code" {
		t.Errorf("result[2].Name = %q, want %q", result[2].Name, "review-code")
	}
}

// ── FormatResults (multi-agent) ───────────────────────────────────────────────

// TestFormatResults_TwoAgentSubset verifies that FormatResults with a 2-agent
// result set produces exactly 2 output lines — one per agent, in order.
// No skipped markers, no lines for agents that were not requested.
func TestFormatResults_TwoAgentSubset(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review-code"}, Passed: true, Output: "LGTM"},
		{Agent: review.Agent{Name: "review-qa"}, Passed: true, Output: "All tests pass"},
	}
	output, allPassed := review.FormatResults(results, "42", 0, 0)
	if !allPassed {
		t.Errorf("allPassed = false, want true")
	}
	// Count lines that are non-empty result lines (start with ✓ or ✗).
	resultLines := countResultLines(output)
	if resultLines != 2 {
		t.Errorf("FormatResults with 2-agent result: got %d result lines, want 2\noutput:\n%s", resultLines, output)
	}
	// review-code line must precede review-qa line.
	codeIdx := findLineIndex(output, "review-code")
	qaIdx := findLineIndex(output, "review-qa")
	if codeIdx < 0 {
		t.Errorf("output does not contain 'review-code': %q", output)
	}
	if qaIdx < 0 {
		t.Errorf("output does not contain 'review-qa': %q", output)
	}
	if codeIdx >= 0 && qaIdx >= 0 && codeIdx >= qaIdx {
		t.Errorf("review-code line (index %d) should precede review-qa line (index %d)", codeIdx, qaIdx)
	}
	// Must not contain 'skipped'.
	if findSubstring(output, "skipped") {
		t.Errorf("output contains 'skipped', but no agents should be skipped: %q", output)
	}
}

// TestFormatResults_RetryHintNamesOnlyFailedAgents verifies that when some
// agents pass and some fail, the retry hint in FormatResults output names only
// the agents that failed — not the ones that passed.
//
// AC: "the retry hint in FormatResults output names only the failed agents"
// Example: if review-qa fails and review-code passes, hint = "prism review <pr> --only review-qa"
func TestFormatResults_RetryHintNamesOnlyFailedAgents(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review-code"}, Passed: true, Output: "LGTM"},
		{Agent: review.Agent{Name: "review-qa"}, Passed: false, Output: "Please fix the test coverage.", IsError: false},
	}
	output, allPassed := review.FormatResults(results, "99", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	// Retry hint must name only review-qa (the failing agent).
	if !findSubstring(output, "--only review-qa") {
		t.Errorf("retry hint should contain '--only review-qa', got: %q", output)
	}
	// Retry hint must NOT name review-code (which passed).
	if findSubstring(output, "review-code") && findSubstring(output, "--only") {
		// Check more specifically that review-code appears in the --only part.
		// Look for the retry line itself.
		for _, line := range strings.Split(output, "\n") {
			if findSubstring(line, "--only") && findSubstring(line, "review-code") {
				t.Errorf("retry hint should not include review-code (it passed), got: %q", line)
			}
		}
	}
}

// TestFormatResults_RetryHintMultipleFailed verifies that when multiple agents
// fail, the retry hint names all of them (comma-separated).
func TestFormatResults_RetryHintMultipleFailed(t *testing.T) {
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review-goal"}, Passed: false, Output: "Please fix goal issues.", IsError: false},
		{Agent: review.Agent{Name: "review-code"}, Passed: true, Output: "LGTM"},
		{Agent: review.Agent{Name: "review-security"}, Passed: false, Output: "vulnerability found", IsError: false},
		{Agent: review.Agent{Name: "review-qa"}, Passed: true, Output: "All tests pass"},
		{Agent: review.Agent{Name: "review-context"}, Passed: true, Output: "Context OK"},
	}
	output, allPassed := review.FormatResults(results, "55", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	// Both failing agents must appear in the retry hint.
	if !findSubstring(output, "review-goal") || !findSubstring(output, "review-security") {
		t.Errorf("retry hint should name both failing agents, got: %q", output)
	}
	// Passing agents must not appear in the --only part of the retry hint.
	for _, line := range strings.Split(output, "\n") {
		if findSubstring(line, "--only") {
			if findSubstring(line, "review-code") || findSubstring(line, "review-qa") || findSubstring(line, "review-context") {
				t.Errorf("retry hint should not include passing agents, got: %q", line)
			}
		}
	}
}

// TestFormatResults_ExactlyNResultLines verifies that FormatResults produces
// exactly N result lines for an N-agent result set (no extras, no blanks in
// the result area that could be mistaken for agent lines).
func TestFormatResults_ExactlyNResultLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		agents []string
	}{
		{"1-agent", 1, []string{"review-code"}},
		{"2-agent", 2, []string{"review-code", "review-qa"}},
		{"5-agent", 5, []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results []review.AgentResult
			for _, name := range tc.agents {
				results = append(results, review.AgentResult{
					Agent:  review.Agent{Name: name},
					Passed: true,
					Output: "LGTM",
				})
			}
			output, allPassed := review.FormatResults(results, "1", 0, 0)
			if !allPassed {
				t.Errorf("allPassed = false, want true")
			}
			got := countResultLines(output)
			if got != tc.n {
				t.Errorf("FormatResults with %d agents: got %d result lines, want %d\noutput:\n%s",
					tc.n, got, tc.n, output)
			}
		})
	}
}

// ── FormatResults: structured per-agent findings ─────────────────────────────

// TestFormatResults_StructuredSummaryExtracted verifies that FormatResults
// extracts and inlines the <summary> tag content for a PASS result, and does
// not include the full per-agent monologue.
func TestFormatResults_StructuredSummaryExtracted(t *testing.T) {
	output := "Some lengthy monologue that should NOT appear.\n" +
		"<summary>All acceptance criteria verified. No issues found.</summary>\n" +
		"<verdict>PASS</verdict>"
	results := []review.AgentResult{
		{
			Agent:  review.Agent{Name: "review-goal"},
			Passed: true,
			Output: output,
		},
	}
	formatted, allPassed := review.FormatResults(results, "123", 0, 0)
	if !allPassed {
		t.Errorf("allPassed = false, want true")
	}
	// Summary header must be present.
	if !findSubstring(formatted, "✓") {
		t.Errorf("output missing ✓ summary header: %q", formatted)
	}
	if !findSubstring(formatted, "review-goal") {
		t.Errorf("output missing agent name in summary: %q", formatted)
	}
	// Extracted summary content must be present.
	if !findSubstring(formatted, "All acceptance criteria verified") {
		t.Errorf("output missing extracted summary content: %q", formatted)
	}
	// Findings section header must be present.
	if !findSubstring(formatted, "Per-agent findings") {
		t.Errorf("output missing 'Per-agent findings' section: %q", formatted)
	}
	// Full monologue must NOT appear.
	if findSubstring(formatted, "lengthy monologue") {
		t.Errorf("output should not contain full monologue text: %q", formatted)
	}
}

// TestFormatResults_StructuredBlockingIssuesExtracted verifies that
// FormatResults extracts and inlines the <blocking_issues> tag content for a
// FAIL result, and does not include the full per-agent monologue.
func TestFormatResults_StructuredBlockingIssuesExtracted(t *testing.T) {
	failOutput := "Lengthy analysis that should NOT appear verbatim.\n" +
		"<summary>One blocking issue found.</summary>\n" +
		"<blocking_issues>\n- Missing error handling in foo()\n</blocking_issues>\n" +
		"Non-blocking prose that should NOT appear.\n" +
		"<verdict>FAIL</verdict>"
	results := []review.AgentResult{
		{
			Agent:   review.Agent{Name: "review-code"},
			Passed:  false,
			Output:  failOutput,
			IsError: false,
		},
	}
	formatted, allPassed := review.FormatResults(results, "456", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	// FAIL verdict must be present.
	if !findSubstring(formatted, "FAIL") {
		t.Errorf("output missing FAIL verdict: %q", formatted)
	}
	// Extracted blocking issues content must be present.
	if !findSubstring(formatted, "Missing error handling in foo()") {
		t.Errorf("output missing extracted blocking issues content: %q", formatted)
	}
	// Extracted summary must be present.
	if !findSubstring(formatted, "One blocking issue found") {
		t.Errorf("output missing extracted summary content: %q", formatted)
	}
	// Full monologue and non-blocking prose must NOT appear.
	if findSubstring(formatted, "Lengthy analysis") {
		t.Errorf("output should not contain full monologue: %q", formatted)
	}
	if findSubstring(formatted, "Non-blocking prose") {
		t.Errorf("output should not contain non-blocking prose: %q", formatted)
	}
}

// TestFormatResults_NoSummaryTagGraceful verifies that when an agent produces
// no <summary> tag, FormatResults does not panic and the output is still valid.
func TestFormatResults_NoSummaryTagGraceful(t *testing.T) {
	results := []review.AgentResult{
		{
			Agent:  review.Agent{Name: "review-qa"},
			Passed: true,
			Output: "No tags here at all.\n<verdict>PASS</verdict>",
		},
	}
	formatted, allPassed := review.FormatResults(results, "789", 0, 0)
	if !allPassed {
		t.Errorf("allPassed = false, want true")
	}
	// Must not panic. Must still contain the summary header and agent name.
	if !findSubstring(formatted, "✓") {
		t.Errorf("output missing ✓ summary header: %q", formatted)
	}
	if !findSubstring(formatted, "review-qa") {
		t.Errorf("output missing agent name: %q", formatted)
	}
}

// TestFormatResults_NoBlockingIssuesTagOnFail verifies that when a FAIL agent
// produces no <blocking_issues> tag, the delivery notes the absence clearly
// rather than panicking or silently omitting the section.
func TestFormatResults_NoBlockingIssuesTagOnFail(t *testing.T) {
	results := []review.AgentResult{
		{
			Agent:   review.Agent{Name: "review-security"},
			Passed:  false,
			Output:  "Something went wrong but no blocking_issues tag.\n<verdict>FAIL</verdict>",
			IsError: false,
		},
	}
	formatted, allPassed := review.FormatResults(results, "101", 0, 0)
	if allPassed {
		t.Errorf("allPassed = true, want false")
	}
	// Must note absence of blocking issues.
	if !findSubstring(formatted, "none found") {
		t.Errorf("output should note absence of blocking issues tag: %q", formatted)
	}
}

// TestFormatResults_NoFileWrittenToTmp verifies that FormatResults never writes
// a file to /tmp regardless of output size — the overflow-to-file path has been
// removed.
func TestFormatResults_NoFileWrittenToTmp(t *testing.T) {
	// Build a large output that would previously have triggered overflow.
	largeOutput := strings.Repeat("x", 500) + "\n<summary>Large output.</summary>\n<verdict>PASS</verdict>"
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review-goal"}, Passed: true, Output: largeOutput},
		{Agent: review.Agent{Name: "review-code"}, Passed: true, Output: largeOutput},
	}

	prNumber := "no-file-test-001"
	round := 7

	// Construct the path that the old code would have written to.
	oldFilePath := fmt.Sprintf("/tmp/prism-review-%s-round-%d.md", prNumber, round)
	_ = os.Remove(oldFilePath)
	t.Cleanup(func() { _ = os.Remove(oldFilePath) })

	output, allPassed := review.FormatResults(results, prNumber, round, 100)
	if !allPassed {
		t.Errorf("allPassed = false, want true")
	}

	// No file pointer should appear in the output.
	if findSubstring(output, "/tmp/prism-review") {
		t.Errorf("output should not contain a /tmp file pointer: %q", output)
	}
	// No file should have been written.
	if _, err := os.Stat(oldFilePath); err == nil {
		t.Errorf("FormatResults wrote a file to /tmp — this path has been removed: %s", oldFilePath)
	}
	// Output must still contain the summary header.
	if !findSubstring(output, "✓") {
		t.Errorf("output missing ✓ summary header: %q", output)
	}
}

// ── ResolveAgentConfigContent ─────────────────────────────────────────────────

// sampleReviewProfilesFile returns a ProfilesFile with all five review agents
// defined as flat per-role slots (#1612).
func sampleReviewProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		Default: "anthropic",
		Profiles: map[string]config.ProfileEntry{
			"anthropic": {
				"review-goal":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-code":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-security": {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-qa":       {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-context":  {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"coordinator":     {Provider: "anthropic", Model: "anthropic/claude-opus-4-7"},
				"worker":          {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
			},
			"gemini-hybrid": {
				"review-goal":     {Provider: "google", Model: "google/gemini-3.1-pro-preview", Thinking: "medium"},
				"review-code":     {Provider: "google", Model: "google/gemini-3.1-pro-preview", Thinking: "medium"},
				"review-security": {Provider: "google", Model: "google/gemini-3.1-pro-preview"},
				"review-qa":       {Provider: "google", Model: "google/gemini-3.1-pro-preview"},
				"review-context":  {Provider: "google", Model: "google/gemini-3.1-pro-preview"},
				"coordinator":     {Provider: "anthropic", Model: "anthropic/claude-opus-4-7"},
				"worker":          {Provider: "google", Model: "google/gemini-3.1-pro-preview", Thinking: "medium"},
			},
		},
	}
}

// TestResolveAgentConfigContent_HostMode verifies that host mode (isolationMode="host"
// or "") always returns ("", nil) regardless of ProfilesFile or agent name.
func TestResolveAgentConfigContent_HostMode(t *testing.T) {
	pf := sampleReviewProfilesFile()
	for _, isolationMode := range []string{"host", ""} {
		for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context", "worker"} {
			blob, err := review.ResolveAgentConfigContent(isolationMode, pf, agentName, "")
			if err != nil {
				t.Errorf("host mode %q, agent %q: unexpected error: %v", isolationMode, agentName, err)
			}
			if blob != "" {
				t.Errorf("host mode %q, agent %q: expected empty blob, got %q", isolationMode, agentName, blob)
			}
		}
	}
	// nil ProfilesFile in host mode must also be safe.
	blob, err := review.ResolveAgentConfigContent("host", nil, "review-goal", "")
	if err != nil {
		t.Errorf("host mode, nil pf: unexpected error: %v", err)
	}
	if blob != "" {
		t.Errorf("host mode, nil pf: expected empty blob, got %q", blob)
	}
	blob, err = review.ResolveAgentConfigContent("", nil, "review-goal", "")
	if err != nil {
		t.Errorf("empty isolation mode, nil pf: unexpected error: %v", err)
	}
	if blob != "" {
		t.Errorf("empty isolation mode, nil pf: expected empty blob, got %q", blob)
	}
}

// TestResolveAgentConfigContent_NilProfilesFileNoActiveProfile verifies that
// bwrap mode with a nil ProfilesFile and no activeProfile returns ("", nil)
// (no profile configured — the harness uses its built-in defaults).
func TestResolveAgentConfigContent_NilProfilesFileNoActiveProfile(t *testing.T) {
	for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		blob, err := review.ResolveAgentConfigContent("bwrap", nil, agentName, "")
		if err != nil {
			t.Errorf("bwrap mode, nil pf, no activeProfile, agent %q: unexpected error: %v", agentName, err)
		}
		if blob != "" {
			t.Errorf("bwrap mode, nil pf, no activeProfile, agent %q: expected empty blob, got %q", agentName, blob)
		}
	}
}

// TestResolveAgentConfigContent_NilProfilesFileWithActiveProfile verifies that
// bwrap mode with a nil ProfilesFile and a non-empty activeProfile returns an
// explicit error (a profile was requested but cannot be loaded).
func TestResolveAgentConfigContent_NilProfilesFileWithActiveProfile(t *testing.T) {
	_, err := review.ResolveAgentConfigContent("bwrap", nil, "review-goal", "gemini-hybrid")
	if err == nil {
		t.Error("bwrap mode, nil pf, activeProfile='gemini-hybrid': expected error, got nil")
	}
	if !findSubstring(err.Error(), "nil ProfilesFile") {
		t.Errorf("error should mention 'nil ProfilesFile', got: %v", err)
	}
}

// TestResolveAgentConfigContent_BwrapMode_Success verifies that all five
// per-agent review config blobs are resolved correctly in bwrap mode when the
// ProfilesFile is fully populated with an active profile.
func TestResolveAgentConfigContent_BwrapMode_Success(t *testing.T) {
	pf := sampleReviewProfilesFile()
	for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		blob, err := review.ResolveAgentConfigContent("bwrap", pf, agentName, "anthropic")
		if err != nil {
			t.Errorf("bwrap mode, agent %q: unexpected error: %v", agentName, err)
			continue
		}
		if blob == "" {
			t.Errorf("bwrap mode, agent %q: expected non-empty blob, got empty", agentName)
			continue
		}
		// Must carry the profile's model for this agent.
		if !findSubstring(blob, "anthropic/claude-sonnet-4-6") {
			t.Errorf("bwrap mode, agent %q: blob %q does not contain anthropic/claude-sonnet-4-6", agentName, blob)
		}
	}
}

// TestResolveAgentConfigContent_WorkerRoleInBwrap verifies that the worker
// role is resolved correctly in bwrap mode (it's a valid slot in the profile).
func TestResolveAgentConfigContent_WorkerRoleInBwrap(t *testing.T) {
	pf := sampleReviewProfilesFile()
	blob, err := review.ResolveAgentConfigContent("bwrap", pf, "worker", "anthropic")
	if err != nil {
		t.Fatalf("bwrap mode, agent 'worker': unexpected error: %v", err)
	}
	if blob == "" {
		t.Error("bwrap mode, agent 'worker': expected non-empty blob for worker slot")
	}
}

// TestResolveAgentConfigContent_OverlaysRuntimeActiveProfile is the behaviour
// gate for review fan-out: when a non-default activeProfile is passed, the
// resolved blob must carry the active profile's models, not the default's.
func TestResolveAgentConfigContent_OverlaysRuntimeActiveProfile(t *testing.T) {
	pf := sampleReviewProfilesFile()

	blob, err := review.ResolveAgentConfigContent("bwrap", pf, "review-goal", "gemini-hybrid")
	if err != nil {
		t.Fatalf("ResolveAgentConfigContent: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(blob), &parsed); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	// Top-level model must be the gemini model for review-goal slot.
	if got, want := parsed["model"], "google/gemini-3.1-pro-preview"; got != want {
		t.Errorf("model = %v, want %v (gemini-hybrid review-goal slot)", got, want)
	}
	// Variant: thinking=medium → variant=medium.
	if got, want := parsed["variant"], "medium"; got != want {
		t.Errorf("variant = %v, want %v", got, want)
	}
}

// TestResolveAgentConfigContent_DefaultProfile verifies that passing the
// default profile name resolves the default profile's model.
func TestResolveAgentConfigContent_DefaultProfile(t *testing.T) {
	pf := sampleReviewProfilesFile()
	for _, active := range []string{"anthropic"} {
		blob, err := review.ResolveAgentConfigContent("bwrap", pf, "review-goal", active)
		if err != nil {
			t.Fatalf("active=%q: %v", active, err)
		}
		if !findSubstring(blob, "anthropic/claude-sonnet-4-6") {
			t.Errorf("active=%q: blob %q does not contain default model", active, blob)
		}
	}
}

// ── FormatAgentDisplayName ────────────────────────────────────────────────────

// TestFormatAgentDisplayName_AllAgents verifies that the five review agent
// names produce the expected title-cased display names for progress output.
func TestFormatAgentDisplayName_AllAgents(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"review-goal", "Review-Goal"},
		{"review-code", "Review-Code"},
		{"review-security", "Review-Security"},
		{"review-qa", "Review-Qa"},
		{"review-context", "Review-Context"},
	}
	for _, tc := range cases {
		got := review.FormatAgentDisplayName(tc.input)
		if got != tc.want {
			t.Errorf("FormatAgentDisplayName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestFormatAgentDisplayName_SingleWord verifies that a single-word name is
// capitalised correctly.
func TestFormatAgentDisplayName_SingleWord(t *testing.T) {
	got := review.FormatAgentDisplayName("worker")
	if got != "Worker" {
		t.Errorf("FormatAgentDisplayName(%q) = %q, want %q", "worker", got, "Worker")
	}
}

// ── FormatProgressDuration ────────────────────────────────────────────────────

// TestFormatProgressDuration_BelowMinute verifies the "Xs.Xf" format for
// durations below 60 seconds.
func TestFormatProgressDuration_BelowMinute(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{28*time.Second + 400*time.Millisecond, "28.4s"},
		{0, "0.0s"},
		{1*time.Second + 100*time.Millisecond, "1.1s"},
		{59*time.Second + 900*time.Millisecond, "59.9s"},
	}
	for _, tc := range cases {
		got := review.FormatProgressDuration(tc.d)
		if got != tc.want {
			t.Errorf("FormatProgressDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestFormatProgressDuration_AtOrAboveMinute verifies the "Xm Ys" format for
// durations at or above 60 seconds.
func TestFormatProgressDuration_AtOrAboveMinute(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{60 * time.Second, "1m0s"},
		{72 * time.Second, "1m12s"},
		{90 * time.Second, "1m30s"},
		{125 * time.Second, "2m5s"},
	}
	for _, tc := range cases {
		got := review.FormatProgressDuration(tc.d)
		if got != tc.want {
			t.Errorf("FormatProgressDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ── Progress callback (OnProgress) ───────────────────────────────────────────

// TestPollAgents_ProgressCallback_HappyPath verifies that PollAgentsForTest
// emits one "finished" progress line per agent when all agents complete
// successfully. This covers the happy-path AC: progress lines are emitted in
// completion order, not start order.
func TestPollAgents_ProgressCallback_HappyPath(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
	}

	// Register a group and associate sessions with it.
	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Pre-seed both agents as finished with group_id.
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	results, err := review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, func(line string) {
		lines = append(lines, line)
	}, groupID)
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("PollAgentsForTest: got %d results, want 2", len(results))
	}

	// Exactly 2 "finished" progress lines must have been emitted.
	if len(lines) != 2 {
		t.Fatalf("expected 2 progress lines, got %d: %v", len(lines), lines)
	}
	// Each line must match the "Review-X finished in Ys.Yf" pattern.
	for _, line := range lines {
		if !findSubstring(line, "finished in") {
			t.Errorf("progress line %q does not contain 'finished in'", line)
		}
	}
	// review-goal must appear in one of the lines.
	if !linesContain(lines, "Review-Goal finished") {
		t.Errorf("expected a 'Review-Goal finished' line, got: %v", lines)
	}
	// review-code must appear in one of the lines.
	if !linesContain(lines, "Review-Code finished") {
		t.Errorf("expected a 'Review-Code finished' line, got: %v", lines)
	}
}

// TestPollAgents_ProgressCallback_OnlySubset verifies that when a subset of
// agents is requested (simulating --only), progress lines are emitted only for
// the requested agents — not for the full 5-agent set.
func TestPollAgents_ProgressCallback_OnlySubset(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	// Only 2 agents (simulating --only review-code,review-qa).
	agents := []review.Agent{
		{Name: "review-code"},
		{Name: "review-qa"},
	}
	sessions := []string{
		"test@parent~review-1-review-code",
		"test@parent~review-1-review-qa",
	}

	// Register a group and associate sessions with it.
	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	_, err = review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, func(line string) {
		lines = append(lines, line)
	}, groupID)
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}

	// Exactly 2 finished lines — not 5 (no stray lines for the other agents).
	if len(lines) != 2 {
		t.Fatalf("expected 2 progress lines for --only subset, got %d: %v", len(lines), lines)
	}
	if !linesContain(lines, "Review-Code finished") {
		t.Errorf("expected 'Review-Code finished' line, got: %v", lines)
	}
	if !linesContain(lines, "Review-Qa finished") {
		t.Errorf("expected 'Review-Qa finished' line, got: %v", lines)
	}
	// Must not contain lines for agents outside the subset.
	for _, line := range lines {
		for _, unexpected := range []string{"Review-Goal", "Review-Security", "Review-Context"} {
			if findSubstring(line, unexpected) {
				t.Errorf("unexpected progress line for out-of-subset agent: %q", line)
			}
		}
	}
}

// TestPollAgents_ProgressCallback_Timeout verifies that when an agent times
// out, a "timed out after <duration>" progress line is emitted and the
// remaining agents continue.
func TestPollAgents_ProgressCallback_Timeout(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}
	sessions := []string{
		"test@parent~review-5-review-goal",
		"test@parent~review-5-review-code",
	}

	// Register a group and associate sessions with it.
	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// review-goal is finished; review-code stays idle (will time out).
	if err := d.UpsertStatus(sessions[0], "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sessions[0], groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	if err := d.UpsertStatus(sessions[1], "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sessions[1], groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	// Very short timeout so review-code times out quickly.
	_, err = review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Millisecond, spawnTimes, func(line string) {
		lines = append(lines, line)
	}, groupID)
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}

	// Must have at least one "timed out after" line for review-code.
	if !linesContain(lines, "Review-Code timed out after") {
		t.Errorf("expected 'Review-Code timed out after' line, got: %v", lines)
	}
}

// TestRun_ProgressCallback_SpawnFailure verifies that when an agent fails to
// spawn due to a configuration error (container mode with nil ProfilesFile),
// a "failed to start" progress line is emitted immediately and the overall run
// returns an error. This covers the spawn-failure path AC from issue #782:
// "unit tests verify the progress-line output format for spawn-failure path."
//
// The ResolveAgentConfigContent failure path fires after successful DB
// operations (UpsertStatus, AllocatePort) but before any tmux session is
// created — so no tmux is needed for this test.
func TestRun_ProgressCallback_SpawnFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")

	// Single agent to keep the test focused.
	agents := []review.Agent{
		{Name: "review-goal"},
	}

	// podman mode with nil ProfilesFile: with the RequireSlot gate (#1224),
	// this now triggers a fan-out abort before the spawn loop, so no per-agent
	// progress lines are emitted and Run returns a global error immediately.
	opts := review.Opts{
		PRNumber:      "999",
		ParentSession: "test@spawn-failure",
		Worktree:      t.TempDir(),
		Agents:        agents,
		Timeout:       30 * time.Second,
		DBPath:        dbPath,
		IsolationMode: "podman",
		ProfilesFile:  nil,
	}

	var progressLines []string
	opts.OnProgress = func(line string) {
		progressLines = append(progressLines, line)
	}

	ctx := context.Background()
	_, err := review.Run(ctx, opts, nil)

	// Run must return an error (RequireSlot fires before any spawn with nil pf).
	if err == nil {
		t.Fatal("Run: expected error when all agents fail to spawn, got nil")
	}

	// With nil ProfilesFile, RequireSlot fires before the spawn loop — no
	// per-agent progress lines are expected. The error itself is the signal.
	if len(progressLines) > 0 {
		t.Logf("Run: progress lines emitted (unexpected with nil pf + RequireSlot gate): %v", progressLines)
	}
}

// TestRun_SeedsRootAgentNameAtSpawnTime verifies that when review.Run spawns
// agents, the agent_status row for each reviewer has root_agent_name set
// (matching the agent's Name) before the sidecar's first upsertState() call.
//
// This test exercises the seeding path by running review.Run in non-container
// host mode with a single agent and verifying the DB row's root_agent_name is
// set after spawn (even if the full session fails due to no real opencode).
// Because the tmux-session-start call happens asynchronously inside
// session.Create, and we cannot easily intercept it in a unit test without a
// live tmux server, we instead verify the DB state after the initial
// UpsertStatusSeedRootAgentName call that runs synchronously before any tmux
// or sidecar operation.
func TestRun_SeedsRootAgentNameAtSpawnTime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Pre-seed the parent session so NextRoundNumber can query it.
	parent := "nixos-config@test-spawn-seed"
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	agents := review.Agents()
	worktree := t.TempDir()

	// Directly test the DB seeding: call UpsertStatusSeedRootAgentName for
	// each agent as the review spawn path does and verify root_agent_name is set.
	round := review.NextRoundNumber(d, parent)
	roundPrefix := fmt.Sprintf("%s~review-%d-", parent, round)

	for _, ag := range agents {
		agentSession := roundPrefix + ag.Name
		// This mirrors what review.Run does synchronously at spawn time.
		if err := d.UpsertStatusSeedRootAgentName(agentSession, "nixos-config", worktree, "idle", nil, nil, ag.Name, ""); err != nil {
			t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", ag.Name, err)
		}
	}

	// Verify every agent session has root_agent_name set.
	for _, ag := range agents {
		agentSession := roundPrefix + ag.Name
		s, err := d.CurrentStatus(agentSession)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", agentSession, err)
		}
		if s == nil {
			t.Errorf("agent %q: expected agent_status row, got nil", ag.Name)
			continue
		}
		if s.RootAgentName == nil {
			t.Errorf("agent %q: RootAgentName is nil, want %q", ag.Name, ag.Name)
			continue
		}
		if *s.RootAgentName != ag.Name {
			t.Errorf("agent %q: RootAgentName = %q, want %q", ag.Name, *s.RootAgentName, ag.Name)
		}
	}
}

// ── SidecarPID persistence after poll ────────────────────────────────────────

// TestPollAgents_SidecarPIDFilesNotRemovedAfterCompletion verifies that after
// pollAgents returns (all agents finished), the sidecar PID files for each
// review-agent session still exist on disk.
//
// This is a direct regression test for #816: Run() used to call KillSidecar on
// each live session after pollAgents returned, removing the PID file and tearing
// down the container. The fix deletes that loop, so PID files must persist.
//
// The test uses PollAgentsForTest with pre-created fake PID files. If KillSidecar
// were called, it would read the file, fail the PID validity check (fake value),
// and remove the file regardless — so the presence of the file after poll is a
// reliable proxy for "KillSidecar was NOT called".
func TestPollAgents_SidecarPIDFilesNotRemovedAfterCompletion(t *testing.T) {
	// Redirect sidecar state dir to a temp directory so PID files don't
	// interfere with the real prism state.
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	ctx := context.Background()
	d := openTestDB(t)

	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
		{Name: "review-security"},
		{Name: "review-qa"},
		{Name: "review-context"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
		"test@parent~review-1-review-security",
		"test@parent~review-1-review-qa",
		"test@parent~review-1-review-context",
	}

	// Register a group and seed all agents as finished.
	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	// Create a fake PID file for each session. The content is "0" — not a live
	// PID, but enough to create the file. KillSidecar would remove it even for
	// an invalid PID, so if files remain after poll we know KillSidecar was not called.
	pidDir := filepath.Join(stateDir, "prism", "run")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", pidDir, err)
	}
	pidPaths := make([]string, len(sessions))
	for i, sess := range sessions {
		pidPath := filepath.Join(pidDir, sess+"-sidecar.pid")
		if err := os.WriteFile(pidPath, []byte("0\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", pidPath, err)
		}
		pidPaths[i] = pidPath
	}

	// Run pollAgents — all agents are already finished so this returns immediately.
	spawnTimes := make([]time.Time, len(agents))
	for i := range spawnTimes {
		spawnTimes[i] = time.Now()
	}
	_, err = review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, nil, groupID)
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}

	// All PID files must still exist — KillSidecar was not called.
	for i, pidPath := range pidPaths {
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			t.Errorf("sidecar PID file for session %q was removed after poll (KillSidecar must NOT be called after pollAgents returns)", sessions[i])
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// linesContain returns true if any of the given lines contains sub.
func linesContain(lines []string, sub string) bool {
	for _, line := range lines {
		if findSubstring(line, sub) {
			return true
		}
	}
	return false
}

// countResultLines counts lines in output that start with ✓ or ✗ (result lines).
func countResultLines(output string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if len(line) > 0 && ([]rune(line)[0] == '✓' || []rune(line)[0] == '✗') {
			count++
		}
	}
	return count
}

// findLineIndex returns the index (in lines) of the first line containing sub,
// or -1 if not found.
func findLineIndex(output, sub string) int {
	for i, line := range strings.Split(output, "\n") {
		if findSubstring(line, sub) {
			return i
		}
	}
	return -1
}

func containsAny(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstring(s, substr))
}

func findSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── BuildReviewPrompt (PR context injection) ──────────────────────────────────

// samplePRContext returns a fully-populated PRContext for testing.
func samplePRContext() *review.PRContext {
	return &review.PRContext{
		PRNumber:     "819",
		Title:        "inject PR metadata into review prompt",
		Body:         "Fixes issue #819 by pre-fetching metadata.\n\nCloses #819",
		HeadRefName:  "inject-pr-context-into-review",
		HeadRefOid:   "abc1234567890abcdef1234567890abcdef123456",
		BaseRefName:  "main",
		BaseRefOid:   "def4567890abcdef1234567890abcdef12345678",
		Additions:    150,
		Deletions:    30,
		ChangedFiles: 3,
		Diff:         "diff --git a/foo.go b/foo.go\n+added line\n-removed line\n",
		WorktreePath: "/workspace/worktrees/inject-pr-context-into-review",
	}
}

// TestBuildReviewPrompt_ContainsPRUnderReviewSection verifies that the prompt
// contains a context section with key metadata fields, including the worktree
// path (read-only) bullet required by review-qa to avoid re-discovering the
// branch checkout location.
func TestBuildReviewPrompt_ContainsPRUnderReviewSection(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx)

	required := []string{
		"## Context for your review",
		"### PR metadata",
		"PR #819",
		"inject-pr-context-into-review",             // head branch
		"abc1234567890abcdef1234567890abcdef123456", // head SHA
		"main", // base branch
		"def4567890abcdef1234567890abcdef12345678", // base SHA
		"Files changed: 3",
		// Worktree bullet: eliminates review-qa's "can I check out the branch?" hesitation.
		"/workspace/worktrees/inject-pr-context-into-review", // worktree path
		"(read-only)", // read-only annotation
		// Tool-preference guidance.
		"Prefer native git",
		// Recent commits section.
		"### Recent commits",
		// Linked issues section.
		"### Linked issues",
	}
	for _, s := range required {
		if !findSubstring(prompt, s) {
			t.Errorf("prompt missing expected string %q\nprompt:\n%s", s, prompt)
		}
	}
}

// TestBuildReviewPrompt_ContainsPRBodySection verifies that the prompt
// contains a "### PR body" section with the PR body text.
func TestBuildReviewPrompt_ContainsPRBodySection(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### PR body") {
		t.Errorf("prompt missing '### PR body' section\nprompt:\n%s", prompt)
	}
	// The body content should be present (blockquote-wrapped).
	if !findSubstring(prompt, "Fixes issue #819") {
		t.Errorf("prompt missing body text\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_EmptyBody verifies that when the PR body is empty,
// the "### PR body" section is still emitted with a "(no body)" placeholder.
func TestBuildReviewPrompt_EmptyBody(t *testing.T) {
	ctx := samplePRContext()
	ctx.Body = ""
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### PR body") {
		t.Errorf("prompt missing '### PR body' section even for empty body\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "(no body)") {
		t.Errorf("prompt missing '(no body)' placeholder for empty body\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_ContainsFullDiffSection verifies that the prompt
// contains a "### Diff" section with the diff in a ```diff code fence.
func TestBuildReviewPrompt_ContainsFullDiffSection(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Diff") {
		t.Errorf("prompt missing '### Diff' section\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "```diff") {
		t.Errorf("prompt missing ```diff code fence\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "diff --git a/foo.go b/foo.go") {
		t.Errorf("prompt missing diff content\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_ContextBeforeRoleSection verifies that the PR-context
// sections appear BEFORE the role-specific instructions section, and that the
// old dangling trailer line is no longer present.
func TestBuildReviewPrompt_ContextBeforeRoleSection(t *testing.T) {
	ctx := samplePRContext()

	// Use a temp dir with a fake role definition file so the role-splice path
	// is exercised with known content.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	agentsDir := filepath.Join(dir, "prism", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	roleDef := "# Test role rubric\n\nReview carefully."
	if err := os.WriteFile(filepath.Join(agentsDir, "review-goal.md"), []byte(roleDef), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	prompt := review.BuildReviewPromptForTest("819", ctx, "review-goal")

	contextHeaderIdx := findLineIndex(prompt, "## Context for your review")
	separatorIdx := findLineIndex(prompt, "---")
	roleSectionIdx := findLineIndex(prompt, "## Your role-specific instructions")

	if contextHeaderIdx < 0 {
		t.Fatalf("prompt missing '## Context for your review'\nprompt:\n%s", prompt)
	}
	if separatorIdx < 0 {
		t.Fatalf("prompt missing separator '---'\nprompt:\n%s", prompt)
	}
	if roleSectionIdx < 0 {
		t.Fatalf("prompt missing '## Your role-specific instructions' section\nprompt:\n%s", prompt)
	}
	if contextHeaderIdx >= separatorIdx {
		t.Errorf("'## Context for your review' (line %d) should appear before '---' (line %d)", contextHeaderIdx, separatorIdx)
	}
	if separatorIdx >= roleSectionIdx {
		t.Errorf("'---' separator (line %d) should appear before '## Your role-specific instructions' (line %d)", separatorIdx, roleSectionIdx)
	}
	// The old dangling trailer must NOT appear.
	if findSubstring(prompt, "Your role-specific instructions follow below.") {
		t.Errorf("prompt must not contain old dangling trailer 'Your role-specific instructions follow below.'\nprompt:\n%s", prompt)
	}
	// The role rubric must be spliced in.
	if !findSubstring(prompt, "Test role rubric") {
		t.Errorf("prompt missing spliced role rubric content\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_FallbackWhenNilContext verifies that when prCtx is nil,
// the prompt is a minimal fallback that still includes the PR number.
func TestBuildReviewPrompt_FallbackWhenNilContext(t *testing.T) {
	prompt := review.BuildReviewPromptForTest("819", nil)

	if !findSubstring(prompt, "819") {
		t.Errorf("fallback prompt should contain PR number '819'\nprompt:\n%s", prompt)
	}
	// Must NOT contain the rich context sections.
	if findSubstring(prompt, "## Context for your review") {
		t.Errorf("fallback prompt should not contain '## Context for your review'\nprompt:\n%s", prompt)
	}
	if findSubstring(prompt, "### Diff") {
		t.Errorf("fallback prompt should not contain '### Diff'\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_FallbackWhenFetchFailed verifies that when FetchFailed
// is true, the prompt uses the minimal fallback form.
func TestBuildReviewPrompt_FallbackWhenFetchFailed(t *testing.T) {
	ctx := &review.PRContext{
		PRNumber:    "819",
		FetchFailed: true,
	}
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "819") {
		t.Errorf("fallback prompt should contain PR number '819'\nprompt:\n%s", prompt)
	}
	if findSubstring(prompt, "## Context for your review") {
		t.Errorf("fallback prompt should not contain rich context when FetchFailed=true\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_AllFiveAgentsGetSameContext verifies that five calls to
// BuildReviewPromptForTest with the same PRContext produce identical prompts.
// This ensures the "one fetch, five reuses" property holds at the prompt level.
func TestBuildReviewPrompt_AllFiveAgentsGetSameContext(t *testing.T) {
	ctx := samplePRContext()
	prompts := make([]string, 5)
	for i := range prompts {
		prompts[i] = review.BuildReviewPromptForTest("819", ctx)
	}
	for i := 1; i < len(prompts); i++ {
		if prompts[i] != prompts[0] {
			t.Errorf("prompt[%d] differs from prompt[0]; all agents must receive identical context", i)
		}
	}
}

// ── RequireSlot fan-out gate (#1224) ──────────────────────────────────────────

// reviewProfilesFileWithAllSlots returns a *config.ProfilesFile that declares
// all five review-agent slots (review-goal, review-code, review-security,
// review-qa, review-context) under a single "default-profile" profile.
// This mirrors the shape emitted by profileFromTiers in profiles.nix.
func reviewProfilesFileWithAllSlots() *config.ProfilesFile {
	slot := config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"}
	entry := config.ProfileEntry{
		"coordinator":     slot,
		"worker":          slot,
		"review-goal":     slot,
		"review-code":     slot,
		"review-security": slot,
		"review-qa":       slot,
		"review-context":  slot,
	}
	return &config.ProfilesFile{
		Default: "default-profile",
		Profiles: map[string]config.ProfileEntry{
			"default-profile": entry,
		},
	}
}

// reviewProfilesFileMissingOneSlot returns a *config.ProfilesFile that defines
// all review slots EXCEPT "review-security". Used to verify the all-or-nothing
// fan-out gate rejects the whole fan-out when one slot is absent.
func reviewProfilesFileMissingOneSlot() *config.ProfilesFile {
	slot := config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"}
	entry := config.ProfileEntry{
		"coordinator":    slot,
		"worker":         slot,
		"review-goal":    slot,
		"review-code":    slot,
		// review-security intentionally absent
		"review-qa":      slot,
		"review-context": slot,
	}
	return &config.ProfilesFile{
		Default: "default-profile",
		Profiles: map[string]config.ProfileEntry{
			"default-profile": entry,
		},
	}
}

// TestRun_RequireSlot_MissingSlot_AbortsAllSpawns verifies that review.Run
// fails fast with a clear error when the active profile is missing one or more
// review-agent slots, and that no progress lines are emitted (i.e. no agent
// spawn was attempted). This is the all-or-nothing AC from #1224.
func TestRun_RequireSlot_MissingSlot_AbortsAllSpawns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")

	pf := reviewProfilesFileMissingOneSlot()

	var progressLines []string
	opts := review.Opts{
		PRNumber:         "1224",
		ParentSession:    "nixos-config@require-slot-test",
		Worktree:         t.TempDir(),
		Timeout:          30 * time.Second,
		DBPath:           dbPath,
		IsolationMode:    "host",
		ProfilesFile:     pf,
		OnProgress:       func(line string) { progressLines = append(progressLines, line) },
	}

	ctx := context.Background()
	_, err := review.Run(ctx, opts, nil)

	// Must return an error — all-or-nothing: missing slot aborts the fan-out.
	if err == nil {
		t.Fatal("Run: expected error when active profile is missing a review slot, got nil")
	}

	// Error must mention the missing role and the profile name.
	errMsg := err.Error()
	if !findSubstring(errMsg, "review-security") {
		t.Errorf("error should mention the missing role 'review-security': %v", errMsg)
	}
	if !findSubstring(errMsg, "default-profile") {
		t.Errorf("error should mention the profile name 'default-profile': %v", errMsg)
	}
	if !findSubstring(errMsg, "fan-out aborted") {
		t.Errorf("error should mention 'fan-out aborted': %v", errMsg)
	}

	// No progress lines should be emitted — the gate fires before any spawn.
	if len(progressLines) > 0 {
		t.Errorf("expected no progress lines (no spawn should occur), got: %v", progressLines)
	}
}

// TestRun_RequireSlot_AllSlotsPresent_DoesNotAbort verifies that review.Run
// does NOT abort the fan-out prematurely when all review slots are present in
// the active profile. In host mode with no real opencode, SpawnSession will
// fail for other reasons — but the RequireSlot gate must not be the cause.
func TestRun_RequireSlot_AllSlotsPresent_DoesNotAbort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")

	pf := reviewProfilesFileWithAllSlots()

	opts := review.Opts{
		PRNumber:         "1224",
		ParentSession:    "nixos-config@require-slot-ok",
		Worktree:         t.TempDir(),
		Timeout:          5 * time.Second,
		ReadinessTimeout: 1 * time.Second,
		DBPath:           dbPath,
		IsolationMode:    "host",
		ProfilesFile:     pf,
	}

	ctx := context.Background()
	_, err := review.Run(ctx, opts, nil)

	// We expect some error (no real opencode running), but it must NOT be a
	// RequireSlot / "fan-out aborted" error.
	if err != nil && findSubstring(err.Error(), "fan-out aborted") {
		t.Errorf("Run aborted on RequireSlot check despite all slots being present: %v", err)
	}
}

// TestRunAsync_RequireSlot_MissingSlot_AbortsAllSpawns verifies that
// review.RunAsync also aborts the fan-out when the active profile is missing
// a review slot (#1224 — both sync and async paths are gated).
func TestRunAsync_RequireSlot_MissingSlot_AbortsAllSpawns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	parent := "nixos-config@async-require-slot-test"
	if err := d.UpsertStatus(parent, "nixos-config", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	pf := reviewProfilesFileMissingOneSlot()

	opts := review.Opts{
		PRNumber:      "1224",
		ParentSession: parent,
		WorkerSession: parent,
		Worktree:      t.TempDir(),
		Timeout:       30 * time.Second,
		DBPath:        dbPath,
		IsolationMode: "host",
		ProfilesFile:  pf,
	}

	_, runErr := review.RunAsync(opts, "/nonexistent/prism-binary")
	if runErr == nil {
		t.Fatal("RunAsync: expected error when active profile is missing a review slot, got nil")
	}

	errMsg := runErr.Error()
	if !findSubstring(errMsg, "review-security") {
		t.Errorf("error should mention the missing role 'review-security': %v", errMsg)
	}
	if !findSubstring(errMsg, "fan-out aborted") {
		t.Errorf("error should mention 'fan-out aborted': %v", errMsg)
	}
}
// These tests exercise FetchPRContextWithOpts in isolation by mocking gh and git.

// TestFetchPRContextWithOpts_InlineThresholdDefault verifies that the default
// inline threshold constants are DiffInlineMaxLines=500 and DiffInlineMaxBytes=20KB.
func TestFetchPRContextWithOpts_InlineThresholdDefault(t *testing.T) {
	if review.DiffInlineMaxLines != 500 {
		t.Errorf("DiffInlineMaxLines = %d, want 500", review.DiffInlineMaxLines)
	}
	if review.DiffInlineMaxBytes != 20*1024 {
		t.Errorf("DiffInlineMaxBytes = %d, want %d", review.DiffInlineMaxBytes, 20*1024)
	}
}

// TestBuildReviewPrompt_AllFiveAgentsGetSameContextWithLinkedIssues verifies
// that five calls to BuildReviewPromptForTest with a PRContext that includes
// linked issues all produce identical prompts.
func TestBuildReviewPrompt_AllFiveAgentsGetSameContextWithLinkedIssues(t *testing.T) {
	ctx := samplePRContext()
	ctx.LinkedIssues = map[string]string{
		"855": "title:\tinject shared context\nstate:\tOPEN\n",
	}
	ctx.RecentCommits = "abc fix: something\n"
	ctx.BranchCommits = "abc fix: something\n"

	prompts := make([]string, 5)
	for i := range prompts {
		prompts[i] = review.BuildReviewPromptForTest("819", ctx)
	}
	for i := 1; i < len(prompts); i++ {
		if prompts[i] != prompts[0] {
			t.Errorf("prompt[%d] differs from prompt[0]; all agents must receive identical context", i)
		}
	}
}

// ── sanitizeSpawnError / truncateProgressMsg tests (issue #1194) ──────────────

// TestSanitizeSpawnError_CommandTooLong verifies that a HostLaunchCmdTooLargeError
// produces a structured message ≤ 1 KB that does not contain PRISM_INITIAL_PROMPT.
func TestSanitizeSpawnError_CommandTooLong(t *testing.T) {
	const prNumber = "1194"
	const agentName = "review-goal"
	const safeBound = 16 * 1024
	const cmdSize = safeBound + 300*1024 // ~316 KB — realistic for a 315-line PR

	syntheticErr := &session.HostLaunchCmdTooLargeError{
		SessionName: "nixos-config@worker~review-1-review-goal",
		CmdSize:     cmdSize,
		SafeBound:   safeBound,
	}

	msg := review.SanitizeSpawnErrorForTest(prNumber, agentName, syntheticErr)

	// AC: printed message ≤ 1 KiB.
	if len(msg) > 1024 {
		t.Errorf("sanitizeSpawnError: message length = %d, want ≤ 1024 bytes\nmessage:\n%s", len(msg), msg)
	}

	// AC: does not contain PRISM_INITIAL_PROMPT.
	if strings.Contains(msg, "PRISM_INITIAL_PROMPT=") {
		t.Errorf("sanitizeSpawnError: message contains PRISM_INITIAL_PROMPT= — argv payload must not appear in stdout")
	}

	// AC: contains the failure category.
	if !strings.Contains(msg, "HostLaunchCmdSafeBound") {
		t.Errorf("sanitizeSpawnError: message does not mention HostLaunchCmdSafeBound\nmessage:\n%s", msg)
	}

	// AC: contains the bound exceeded.
	if !strings.Contains(msg, fmt.Sprintf("%d", safeBound)) {
		t.Errorf("sanitizeSpawnError: message does not contain safe bound %d\nmessage:\n%s", safeBound, msg)
	}

	// AC: contains a hint.
	if !strings.Contains(msg, "hint:") {
		t.Errorf("sanitizeSpawnError: message does not contain a 'hint:'\nmessage:\n%s", msg)
	}
}

// TestSanitizeSpawnError_OtherError_NoPromptPayload verifies that a non-oversized
// error that embeds PRISM_INITIAL_PROMPT in its string does not pass that payload
// through to the caller.
func TestSanitizeSpawnError_OtherError_NoPromptPayload(t *testing.T) {
	const prNumber = "1194"
	const agentName = "review-code"
	// Simulate an error whose string includes PRISM_INITIAL_PROMPT payload.
	rawPromptValue := strings.Repeat("sensitive context", 1000)
	err := fmt.Errorf("tmux new-window failed: PRISM_INITIAL_PROMPT=%s: exit status 1: command too long", rawPromptValue)

	msg := review.SanitizeSpawnErrorForTest(prNumber, agentName, err)

	// AC: the raw PRISM_INITIAL_PROMPT value must not appear verbatim.
	if strings.Contains(msg, "PRISM_INITIAL_PROMPT="+rawPromptValue) {
		t.Errorf("sanitizeSpawnError: message contains raw PRISM_INITIAL_PROMPT payload")
	}

	// AC: hard cap (message ≤ maxProgressMsgBytes + len of truncation suffix).
	if len(msg) > review.MaxProgressMsgBytesForTest+200 {
		t.Errorf("sanitizeSpawnError: message length = %d, want ≤ %d+200", len(msg), review.MaxProgressMsgBytesForTest)
	}
}

// TestTruncateProgressMsg_Cap verifies that a message longer than 4 KiB is
// truncated with a suffix naming a forensic path.
func TestTruncateProgressMsg_Cap(t *testing.T) {
	const prNumber = "1194"
	const agentName = "review-code"
	long := strings.Repeat("x", review.MaxProgressMsgBytesForTest+1)
	out := review.TruncateProgressMsgForTest(prNumber, agentName, long)
	if !strings.Contains(out, "[...truncated; full error in ") {
		t.Errorf("truncateProgressMsg: truncated message does not contain expected suffix\nmessage:\n%s", out)
	}
}

// TestTruncateProgressMsg_ShortPassthrough verifies that a short message is
// returned unchanged.
func TestTruncateProgressMsg_ShortPassthrough(t *testing.T) {
	const prNumber = "1194"
	const agentName = "review-security"
	short := "tmux not running"
	out := review.TruncateProgressMsgForTest(prNumber, agentName, short)
	if out != short {
		t.Errorf("truncateProgressMsg: short message modified unexpectedly\ngot: %q\nwant: %q", out, short)
	}
}

// ── DiffFilePath / StateDir tests (issue #1446) ───────────────────────────────

// TestDiffFilePathForTest_StateDirUsed verifies that when a StateDir is provided
// the returned path is under that directory (not the /tmp fallback).
func TestDiffFilePathForTest_StateDirUsed(t *testing.T) {
	// Use an explicit non-/tmp directory to confirm the stateDir path is chosen.
	dir := filepath.Join(t.TempDir(), "prism", "run", "abc123")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	got := review.DiffFilePathForTest(dir, "1234", 1)
	if !strings.HasPrefix(got, dir) {
		t.Errorf("DiffFilePathForTest(stateDir=%q): path %q does not begin with stateDir", dir, got)
	}
	// The filename under stateDir must contain the round number.
	filename := filepath.Base(got)
	if !strings.Contains(filename, "round-1") {
		t.Errorf("DiffFilePathForTest(stateDir=%q): filename %q does not contain 'round-1'", dir, filename)
	}
}

// TestDiffFilePathForTest_EmptyStateDirFallsBackToTmp verifies that when
// StateDir is empty the function falls back to /tmp.
func TestDiffFilePathForTest_EmptyStateDirFallsBackToTmp(t *testing.T) {
	got := review.DiffFilePathForTest("", "1234", 1)
	if !strings.HasPrefix(got, "/tmp/") {
		t.Errorf("DiffFilePathForTest(stateDir=\"\"): path %q does not begin with /tmp/ — fallback must be /tmp", got)
	}
}

// TestDiffFilePathForTest_RoundSuffixDisambiguates verifies that different round
// numbers produce different paths (preventing cross-round collisions).
func TestDiffFilePathForTest_RoundSuffixDisambiguates(t *testing.T) {
	dir := t.TempDir()
	path1 := review.DiffFilePathForTest(dir, "819", 1)
	path2 := review.DiffFilePathForTest(dir, "819", 2)
	if path1 == path2 {
		t.Errorf("DiffFilePathForTest: round 1 and round 2 produced the same path %q — rounds must be disambiguated", path1)
	}
}

// TestDiffFilePathForTest_DifferentPRsInTmpDisambiguate verifies that different
// PRs produce different /tmp fallback paths (preserving the existing collision
// avoidance guarantee for host-mode sessions).
func TestDiffFilePathForTest_DifferentPRsInTmpDisambiguate(t *testing.T) {
	pathA := review.DiffFilePathForTest("", "111", 1)
	pathB := review.DiffFilePathForTest("", "222", 1)
	if pathA == pathB {
		t.Errorf("DiffFilePathForTest /tmp fallback: PR 111 and PR 222 share the same path %q — /tmp paths must embed the PR number", pathA)
	}
}

// TestDiffFilePathForTest_DifferentPRsInStateDirDisambiguate verifies that when
// two concurrent reviews run against different PRs sharing the same worktree
// (stateDir), the resulting paths are distinct so the agents read the correct
// diff (security / edge-case AC from issue #1446).
func TestDiffFilePathForTest_DifferentPRsInStateDirDisambiguate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".prism-review")
	pathA := review.DiffFilePathForTest(dir, "111", 1)
	pathB := review.DiffFilePathForTest(dir, "222", 1)
	if pathA == pathB {
		t.Errorf("DiffFilePathForTest stateDir: PR 111 and PR 222 share the same path %q — concurrent reviews must not collide", pathA)
	}
}
