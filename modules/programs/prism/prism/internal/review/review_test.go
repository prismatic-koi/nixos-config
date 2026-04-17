package review_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
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
	output, allPassed := review.FormatResults(results, "42")
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
	output, allPassed := review.FormatResults(results, "42")
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
	output, allPassed := review.FormatResults(results, "42")
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
	_, allPassed := review.FormatResults(results, "1")
	if allPassed {
		t.Errorf("FormatResults with Passed=false: allPassed=true, want false")
	}

	// A result marked Passed=true is a pass.
	results = []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: true, Output: "<verdict>PASS</verdict>"},
	}
	_, allPassed = review.FormatResults(results, "1")
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

// TestBuildResults_InterruptedState verifies Layer 2: an agent whose DB state
// is "interrupted" produces an error result regardless of msg_assistant events.
func TestBuildResults_InterruptedState(t *testing.T) {
	d := openTestDB(t)
	ag := review.Agent{Name: "review-goal"}
	sess := "test@parent~review-1-review-goal"

	// Seed the agent as interrupted with benign assistant output.
	_ = d.UpsertStatus(sess, "nixos-config", "/wt", "interrupted", nil, nil)
	// Even with a benign (no-verdict) assistant message, the result must be error.
	seedAssistantEvent(t, d, sess, "I'll start by reading the PR...")

	finished := []bool{true} // "interrupted" was a terminal state → finished=true
	timedOut := []bool{false}
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false)

	if len(results) != 1 {
		t.Fatalf("BuildResults returned %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed {
		t.Errorf("BuildResults with interrupted state: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("BuildResults with interrupted state: IsError=false, want true")
	}
	if !findSubstring(r.Output, "interrupted") {
		t.Errorf("BuildResults with interrupted state: output does not mention 'interrupted': %q", r.Output)
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
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false)

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
	results := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, true)

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
	results := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, true)

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
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false)

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
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false)

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
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false)

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
		if agents[i].OpencodeName != name {
			t.Errorf("agents[%d].OpencodeName = %q, want %q", i, agents[i].OpencodeName, name)
		}
	}
}

// ── CheckAgentAvailability ────────────────────────────────────────────────────

func TestCheckAgentAvailability_AllPresent(t *testing.T) {
	// Create a temp dir with agent .md files for all agents.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := dir + "/opencode/agents"
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	agents := review.Agents()
	for _, ag := range agents {
		if err := os.WriteFile(agentsDir+"/"+ag.Name+".md", []byte("# "+ag.Name), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	if err := review.CheckAgentAvailability(agents); err != nil {
		t.Errorf("CheckAgentAvailability: unexpected error: %v", err)
	}
}

func TestCheckAgentAvailability_SomeMissing(t *testing.T) {
	// Create a temp dir with only some agents present.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := dir + "/opencode/agents"
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Only create review-goal.md; the other 4 are missing.
	if err := os.WriteFile(agentsDir+"/review-goal.md", []byte("# review-goal"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	agents := review.Agents()
	err := review.CheckAgentAvailability(agents)
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for missing agents, got nil")
	}
	// Error should mention the missing agents.
	for _, ag := range agents[1:] {
		if !findSubstring(err.Error(), ag.Name) {
			t.Errorf("CheckAgentAvailability error does not mention missing agent %q: %v", ag.Name, err)
		}
	}
	// Error should NOT mention review-goal (it is present).
	if findSubstring(err.Error(), "review-goal") {
		t.Errorf("CheckAgentAvailability error unexpectedly mentions present agent review-goal: %v", err)
	}
}

func TestCheckAgentAvailability_AllMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the agents directory at all.

	agents := review.Agents()
	err := review.CheckAgentAvailability(agents)
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for all missing agents, got nil")
	}
}

func TestCheckAgentAvailability_EmptyAgentList(t *testing.T) {
	// An empty agent list should always pass.
	if err := review.CheckAgentAvailability(nil); err != nil {
		t.Errorf("CheckAgentAvailability(nil): unexpected error: %v", err)
	}
	if err := review.CheckAgentAvailability([]review.Agent{}); err != nil {
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
	output, allPassed := review.FormatResults(results, "42")
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
	output, allPassed := review.FormatResults(results, "99")
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
	output, allPassed := review.FormatResults(results, "55")
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
			output, allPassed := review.FormatResults(results, "1")
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

// ── ResolveAgentConfigContent ─────────────────────────────────────────────────

// sampleReviewProfilesFile returns a ProfilesFile with all five per-agent
// review config blobs populated, as generated by the prism NixOS module.
func sampleReviewProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		ContainerReviewGoalConfig:     `{"$schema":"https://opencode.ai/opencode.json","default_agent":"review-goal","agent":{"review-goal":{}}}`,
		ContainerReviewCodeConfig:     `{"$schema":"https://opencode.ai/opencode.json","default_agent":"review-code","agent":{"review-code":{}}}`,
		ContainerReviewSecurityConfig: `{"$schema":"https://opencode.ai/opencode.json","default_agent":"review-security","agent":{"review-security":{}}}`,
		ContainerReviewQaConfig:       `{"$schema":"https://opencode.ai/opencode.json","default_agent":"review-qa","agent":{"review-qa":{}}}`,
		ContainerReviewContextConfig:  `{"$schema":"https://opencode.ai/opencode.json","default_agent":"review-context","agent":{"review-context":{}}}`,
	}
}

// TestResolveAgentConfigContent_HostMode verifies that host mode (containerMode=false)
// always returns ("", nil) regardless of ProfilesFile or agent name.
// This is the regression guard for the host-mode path (#735 domain).
func TestResolveAgentConfigContent_HostMode(t *testing.T) {
	pf := sampleReviewProfilesFile()
	for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context", "review", "worker"} {
		blob, err := review.ResolveAgentConfigContent(false, pf, agentName)
		if err != nil {
			t.Errorf("host mode, agent %q: unexpected error: %v", agentName, err)
		}
		if blob != "" {
			t.Errorf("host mode, agent %q: expected empty blob, got %q", agentName, blob)
		}
	}
	// nil ProfilesFile in host mode must also be safe.
	blob, err := review.ResolveAgentConfigContent(false, nil, "review-goal")
	if err != nil {
		t.Errorf("host mode, nil pf: unexpected error: %v", err)
	}
	if blob != "" {
		t.Errorf("host mode, nil pf: expected empty blob, got %q", blob)
	}
}

// TestResolveAgentConfigContent_ContainerMode_NilProfilesFile verifies that
// container mode with a nil ProfilesFile returns an explicit error.
// This is the primary regression test for issue #784 (silent build-agent spawn).
func TestResolveAgentConfigContent_ContainerMode_NilProfilesFile(t *testing.T) {
	for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		_, err := review.ResolveAgentConfigContent(true, nil, agentName)
		if err == nil {
			t.Errorf("container mode, nil pf, agent %q: expected error, got nil", agentName)
		}
		if !findSubstring(err.Error(), "nil ProfilesFile") {
			t.Errorf("container mode, nil pf, agent %q: error should mention 'nil ProfilesFile', got: %v", agentName, err)
		}
	}
}

// TestResolveAgentConfigContent_ContainerMode_EmptyBlob verifies that
// container mode with a ProfilesFile that has empty review config blobs returns
// an explicit error instead of silently falling back to the build agent.
// This is the root cause of issue #784.
func TestResolveAgentConfigContent_ContainerMode_EmptyBlob(t *testing.T) {
	// A ProfilesFile with empty review config blobs — simulates a stale profiles.json
	// built before the containerReview*Config Nix options were added.
	stale := &config.ProfilesFile{
		ContainerWorkerConfig:      `{"model":"worker"}`,
		ContainerCoordinatorConfig: `{"model":"coordinator"}`,
		// All review configs are intentionally empty — the pre-PR-B state.
		ContainerReviewGoalConfig:     "",
		ContainerReviewCodeConfig:     "",
		ContainerReviewSecurityConfig: "",
		ContainerReviewQaConfig:       "",
		ContainerReviewContextConfig:  "",
	}
	for _, agentName := range []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"} {
		_, err := review.ResolveAgentConfigContent(true, stale, agentName)
		if err == nil {
			t.Errorf("container mode, empty blob, agent %q: expected error, got nil (would have silently spawned as build agent)", agentName)
		}
		if !findSubstring(err.Error(), "stale") {
			t.Errorf("container mode, empty blob, agent %q: error should mention 'stale', got: %v", agentName, err)
		}
	}
}

// TestResolveAgentConfigContent_ContainerMode_Success verifies that all five
// per-agent review config blobs are resolved correctly when the ProfilesFile
// is fully populated.
func TestResolveAgentConfigContent_ContainerMode_Success(t *testing.T) {
	pf := sampleReviewProfilesFile()
	want := map[string]string{
		"review-goal":     pf.ContainerReviewGoalConfig,
		"review-code":     pf.ContainerReviewCodeConfig,
		"review-security": pf.ContainerReviewSecurityConfig,
		"review-qa":       pf.ContainerReviewQaConfig,
		"review-context":  pf.ContainerReviewContextConfig,
	}
	for agentName, wantBlob := range want {
		blob, err := review.ResolveAgentConfigContent(true, pf, agentName)
		if err != nil {
			t.Errorf("container mode, agent %q: unexpected error: %v", agentName, err)
			continue
		}
		if blob != wantBlob {
			t.Errorf("container mode, agent %q: got blob %q, want %q", agentName, blob, wantBlob)
		}
	}
}

// TestResolveAgentConfigContent_ContainerMode_WorkerAndCoordinator verifies
// that worker and coordinator roles in container mode return empty blob (they
// are not review agents and their blobs are not expected by this function —
// they are handled by the spawn path, not the review path).
// The function returns an empty-blob error for them, which prevents accidental
// use in the review path.
func TestResolveAgentConfigContent_ContainerMode_WorkerRole(t *testing.T) {
	pf := sampleReviewProfilesFile()
	// Worker and coordinator are not review agents — ContainerConfigForRole
	// returns their blobs, but they are not set in sampleReviewProfilesFile.
	// Empty blob should produce an error.
	_, err := review.ResolveAgentConfigContent(true, pf, "worker")
	if err == nil {
		t.Error("container mode, agent 'worker': expected error for empty blob, got nil")
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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
	}

	// Pre-seed both agents as finished.
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	results, err := review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, func(line string) {
		lines = append(lines, line)
	})
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
		{Name: "review-code", OpencodeName: "review-code"},
		{Name: "review-qa", OpencodeName: "review-qa"},
	}
	sessions := []string{
		"test@parent~review-1-review-code",
		"test@parent~review-1-review-qa",
	}
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	_, err := review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, func(line string) {
		lines = append(lines, line)
	})
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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
	}
	sessions := []string{
		"test@parent~review-5-review-goal",
		"test@parent~review-5-review-code",
	}

	// review-goal is finished; review-code stays idle (will time out).
	if err := d.UpsertStatus(sessions[0], "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus(sessions[1], "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	// Very short timeout so review-code times out quickly.
	_, err := review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Millisecond, spawnTimes, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}

	// Must have at least one "timed out after" line for review-code.
	if !linesContain(lines, "Review-Code timed out after") {
		t.Errorf("expected 'Review-Code timed out after' line, got: %v", lines)
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
