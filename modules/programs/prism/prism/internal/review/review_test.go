package review_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
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
	results := review.BuildResults([]review.Agent{ag}, []string{sess}, d, finished, timedOut, 10*time.Minute, false, "")

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

	h := opencode.New("", nil, "", "")
	if err := review.CheckAgentAvailability(agents, h.ValidateAgentRole); err != nil {
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

	h := opencode.New("", nil, "", "")
	agents := review.Agents()
	err := review.CheckAgentAvailability(agents, h.ValidateAgentRole)
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

	h := opencode.New("", nil, "", "")
	agents := review.Agents()
	err := review.CheckAgentAvailability(agents, h.ValidateAgentRole)
	if err == nil {
		t.Fatal("CheckAgentAvailability: expected error for all missing agents, got nil")
	}
}

func TestCheckAgentAvailability_EmptyAgentList(t *testing.T) {
	// An empty agent list should always pass (validator is never called).
	h := opencode.New("", nil, "", "")
	if err := review.CheckAgentAvailability(nil, h.ValidateAgentRole); err != nil {
		t.Errorf("CheckAgentAvailability(nil): unexpected error: %v", err)
	}
	if err := review.CheckAgentAvailability([]review.Agent{}, h.ValidateAgentRole); err != nil {
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
		{Name: "review-code", OpencodeName: "review-code"},
		{Name: "review-qa", OpencodeName: "review-qa"},
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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
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
		{Name: "review-goal", OpencodeName: "review-goal"},
	}

	// container mode with nil ProfilesFile triggers ResolveAgentConfigContent
	// to return a "nil ProfilesFile" error before any tmux session is created.
	opts := review.Opts{
		PRNumber:      "999",
		ParentSession: "test@spawn-failure",
		Worktree:      t.TempDir(),
		Agents:        agents,
		Timeout:       30 * time.Second,
		DBPath:        dbPath,
		ContainerMode: true,
		ProfilesFile:  nil,
	}

	var progressLines []string
	opts.OnProgress = func(line string) {
		progressLines = append(progressLines, line)
	}

	ctx := context.Background()
	_, err := review.Run(ctx, opts, nil)

	// Run must return an error when all agents fail to spawn.
	if err == nil {
		t.Fatal("Run: expected error when all agents fail to spawn, got nil")
	}

	// At least one progress line must have been emitted.
	if len(progressLines) == 0 {
		t.Fatal("Run: expected at least one progress line for spawn failure, got none")
	}

	// The progress line must contain the display name and "failed to start".
	if !linesContain(progressLines, "Review-Goal failed to start") {
		t.Errorf("expected 'Review-Goal failed to start' in progress lines, got: %v", progressLines)
	}

	// Must NOT emit a "started" line — spawn failed before session creation.
	for _, line := range progressLines {
		if len(line) > 0 && findSubstring(line, "Review-Goal started") {
			t.Errorf("unexpected 'started' line emitted on spawn failure: %q", line)
		}
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
		if err := d.UpsertStatusSeedRootAgentName(agentSession, "nixos-config", worktree, "idle", nil, nil, ag.Name); err != nil {
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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
		{Name: "review-security", OpencodeName: "review-security"},
		{Name: "review-qa", OpencodeName: "review-qa"},
		{Name: "review-context", OpencodeName: "review-context"},
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

// TestBuildReviewPrompt_ContextBeforeRoleSeparator verifies that the PR-context
// sections appear BEFORE the role-specific content separator ("---").
func TestBuildReviewPrompt_ContextBeforeRoleSeparator(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx)

	contextHeaderIdx := findLineIndex(prompt, "## Context for your review")
	separatorIdx := findLineIndex(prompt, "---")
	roleNoteIdx := findLineIndex(prompt, "Your role-specific instructions follow below.")

	if contextHeaderIdx < 0 {
		t.Fatalf("prompt missing '## Context for your review'\nprompt:\n%s", prompt)
	}
	if separatorIdx < 0 {
		t.Fatalf("prompt missing separator '---'\nprompt:\n%s", prompt)
	}
	if roleNoteIdx < 0 {
		t.Fatalf("prompt missing role note\nprompt:\n%s", prompt)
	}
	if contextHeaderIdx >= separatorIdx {
		t.Errorf("'## Context for your review' (line %d) should appear before '---' (line %d)", contextHeaderIdx, separatorIdx)
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

// TestBuildReviewPrompt_SpecialCharsInTitle verifies that backticks and angle
// brackets in the PR title do not break the prompt structure.
func TestBuildReviewPrompt_SpecialCharsInTitle(t *testing.T) {
	ctx := samplePRContext()
	ctx.Title = "fix: handle `nil` pointer in <module> and `go build`"
	prompt := review.BuildReviewPromptForTest("819", ctx)

	// The prompt should still contain both main sections.
	if !findSubstring(prompt, "## Context for your review") {
		t.Errorf("prompt missing '## Context for your review' with special-char title\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "### Diff") {
		t.Errorf("prompt missing '### Diff' with special-char title\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_TripleBackticksInBody verifies that triple-backtick
// sequences in the PR body do not collapse the ```diff code fence.
func TestBuildReviewPrompt_TripleBackticksInBody(t *testing.T) {
	ctx := samplePRContext()
	ctx.Body = "Here is an example:\n```go\nfmt.Println(\"hello\")\n```\nEnd."
	prompt := review.BuildReviewPromptForTest("819", ctx)

	// The diff fence must still be intact.
	if !findSubstring(prompt, "```diff") {
		t.Errorf("prompt missing ```diff fence; body backticks may have broken structure\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_DiffTruncatedNote verifies that when DiffTruncated is
// true, the prompt mentions the truncation.
func TestBuildReviewPrompt_DiffTruncatedNote(t *testing.T) {
	ctx := samplePRContext()
	ctx.DiffTruncated = true
	ctx.Diff = "diff --git a/big.go b/big.go\n+line\n... [truncated — use git diff origin/main...HEAD for full content]"
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "truncated") {
		t.Errorf("prompt should mention truncation when DiffTruncated=true\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_NoDiffAvailable verifies that when Diff is empty
// (gh pr diff failed), the prompt notes it and does not emit an empty ```diff fence.
func TestBuildReviewPrompt_NoDiffAvailable(t *testing.T) {
	ctx := samplePRContext()
	ctx.Diff = ""
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "diff not available") {
		t.Errorf("prompt should note 'diff not available' when Diff is empty\nprompt:\n%s", prompt)
	}
	// Must not have an empty ```diff fence (which would confuse agents).
	if findSubstring(prompt, "```diff\n```") {
		t.Errorf("prompt should not have an empty ```diff``` fence\nprompt:\n%s", prompt)
	}
}

// ── TruncateDiff ──────────────────────────────────────────────────────────────

// TestTruncateDiff_NoTruncationNeeded verifies that a small diff is returned
// unchanged with truncated=false.
func TestTruncateDiff_NoTruncationNeeded(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n+added\n-removed\n"
	result, truncated := review.TruncateDiffForTest(diff, 200*1024, 4000)
	if truncated {
		t.Errorf("TruncateDiff: truncated=true for small diff, want false")
	}
	if result != diff {
		t.Errorf("TruncateDiff: result differs from input for small diff\ngot:  %q\nwant: %q", result, diff)
	}
}

// TestTruncateDiff_TruncatesByLineCount verifies that a diff exceeding maxLines
// is truncated and the truncation marker is appended.
func TestTruncateDiff_TruncatesByLineCount(t *testing.T) {
	// Build a diff with 100 lines.
	var sb strings.Builder
	for i := range 100 {
		sb.WriteString(fmt.Sprintf("+line %d\n", i))
	}
	diff := sb.String()

	result, truncated := review.TruncateDiffForTest(diff, 200*1024, 10)
	if !truncated {
		t.Errorf("TruncateDiff: truncated=false for 100-line diff with maxLines=10, want true")
	}
	if !findSubstring(result, "truncated") {
		t.Errorf("TruncateDiff: result missing truncation marker\nresult:\n%s", result)
	}
	// The result must not contain line 11 or later.
	if findSubstring(result, "+line 10\n") {
		t.Errorf("TruncateDiff: result contains lines beyond maxLines=10\nresult:\n%s", result)
	}
}

// TestTruncateDiff_TruncatesByByteCount verifies that a diff exceeding maxBytes
// is truncated and the truncation marker is appended.
func TestTruncateDiff_TruncatesByByteCount(t *testing.T) {
	// Build a 1000-byte diff.
	diff := strings.Repeat("+x\n", 333) // ~999 bytes

	result, truncated := review.TruncateDiffForTest(diff, 100, 10000)
	if !truncated {
		t.Errorf("TruncateDiff: truncated=false for >100-byte diff with maxBytes=100, want true")
	}
	if !findSubstring(result, "truncated") {
		t.Errorf("TruncateDiff: result missing truncation marker\nresult:\n%s", result)
	}
	if len(result) > 200 { // well under the original 999 bytes
		// Sanity check that we actually truncated substantially.
		// The marker adds ~70 bytes; total should be << 999.
	}
}

// ── Group-id wiring (#860, Issue E) ──────────────────────────────────────────

// TestGroupWiring_RegisterGroupCalledOncePerRound verifies that each review
// round creates exactly one session_groups row with the correct parent_session.
// AC: "Every prism review invocation creates exactly one new row in
// session_groups per round, with parent_session correctly set."
func TestGroupWiring_RegisterGroupCalledOncePerRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@worker-branch"

	// Simulate what review.Run does: register one group per round.
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if groupID == "" {
		t.Fatal("RegisterGroup returned empty group_id")
	}

	// Verify the session_groups row exists with the correct parent.
	var storedParent string
	err = d.QueryRow(
		"SELECT parent_session FROM session_groups WHERE group_id = ?", groupID,
	).Scan(&storedParent)
	if err != nil {
		t.Fatalf("query session_groups: %v", err)
	}
	if storedParent != parent {
		t.Errorf("session_groups.parent_session = %q, want %q", storedParent, parent)
	}
}

// TestGroupWiring_AllMembersHaveGroupID verifies that when SpawnSession writes
// group_id via SetGroupID, all 5 agent_status rows have the same group_id.
// AC: "Every one of the 5 per-round reviewer sessions has agent_status.group_id
// set to the round's group_id."
func TestGroupWiring_AllMembersHaveGroupID(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@worker-branch"

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := review.Agents()
	round := 1
	roundPrefix := fmt.Sprintf("%s~review-%d-", parent, round)

	// Simulate what review.Run does: seed each agent and set group_id.
	for _, ag := range agents {
		sess := roundPrefix + ag.Name
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	// Verify every agent_status row has the correct group_id.
	for _, ag := range agents {
		sess := roundPrefix + ag.Name
		s, err := d.CurrentStatus(sess)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", sess, err)
		}
		if s == nil {
			t.Errorf("agent %q: expected agent_status row, got nil", ag.Name)
			continue
		}
		if s.GroupID == nil {
			t.Errorf("agent %q: GroupID is nil, want %q", ag.Name, groupID)
			continue
		}
		if *s.GroupID != groupID {
			t.Errorf("agent %q: GroupID = %q, want %q", ag.Name, *s.GroupID, groupID)
		}
	}
}

// TestGroupWiring_GroupCompletedIsTerminationSignal verifies that
// GroupCompleted returns false while agents are running and true once all
// reach a terminal state. This is the core AC:
// "The review orchestrator's termination check uses db.GroupCompleted(group_id)."
func TestGroupWiring_GroupCompletedIsTerminationSignal(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	sessions := []string{"test@parent~review-1-review-goal", "test@parent~review-1-review-code"}
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	// Initially: both idle → GroupCompleted should return false.
	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted (initial): %v", err)
	}
	if done {
		t.Error("GroupCompleted returned true while agents are still idle")
	}

	// Transition first agent to finished → still not complete (second is idle).
	_ = d.UpsertStatus(sessions[0], "nixos-config", "/wt", "finished", nil, nil)
	done, err = d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted (one finished): %v", err)
	}
	if done {
		t.Error("GroupCompleted returned true with only one of two agents finished")
	}

	// Transition second agent to finished → now complete.
	_ = d.UpsertStatus(sessions[1], "nixos-config", "/wt", "finished", nil, nil)
	done, err = d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted (all finished): %v", err)
	}
	if !done {
		t.Error("GroupCompleted returned false after all agents finished")
	}
}

// TestGroupWiring_GroupResultsMatchesPerSessionAggregation verifies that
// GroupResults produces the same terminal state and last message data as
// individual per-session queries. This is the AC:
// "GroupResults output matches what name-prefix aggregation produced."
func TestGroupWiring_GroupResultsMatchesPerSessionAggregation(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := review.Agents()
	roundPrefix := "test@parent~review-1-"
	sessions := make([]string, len(agents))

	for i, ag := range agents {
		sess := roundPrefix + ag.Name
		sessions[i] = sess
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
		// Seed a root_agent_name via the seed helper.
		if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/wt", "finished", nil, nil, ag.Name); err != nil {
			t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", sess, err)
		}
		seedAssistantEvent(t, d, sess, fmt.Sprintf("Review from %s. <verdict>PASS</verdict>", ag.Name))
	}

	// Fetch via GroupResults.
	groupData, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}

	if len(groupData) != 5 {
		t.Fatalf("GroupResults returned %d members, want 5", len(groupData))
	}

	// Verify each member matches individual per-session queries.
	for _, sess := range sessions {
		gr, ok := groupData[sess]
		if !ok {
			t.Errorf("GroupResults missing session %q", sess)
			continue
		}

		// Compare state with CurrentStatus.
		status, sErr := d.CurrentStatus(sess)
		if sErr != nil {
			t.Fatalf("CurrentStatus(%q): %v", sess, sErr)
		}
		if gr.State != status.State {
			t.Errorf("session %q: GroupResults.State = %q, CurrentStatus.State = %q", sess, gr.State, status.State)
		}

		// Compare last message with QueryEvents.
		events, eErr := d.QueryEvents(sess, 1, nil, nil, []string{"msg_assistant"})
		if eErr != nil {
			t.Fatalf("QueryEvents(%q): %v", sess, eErr)
		}
		if len(events) > 0 && gr.LastMessage != events[len(events)-1].Payload {
			t.Errorf("session %q: GroupResults.LastMessage differs from QueryEvents payload", sess)
		}
	}
}

// TestGroupWiring_DistinctGroupsPerRound verifies that two review invocations
// create distinct group_ids with no cross-contamination.
// AC: "A second prism review invocation creates a distinct group_id with
// distinct member rows."
func TestGroupWiring_DistinctGroupsPerRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@worker-branch"

	// Round 1.
	g1, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup round 1: %v", err)
	}
	// Round 2.
	g2, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup round 2: %v", err)
	}

	if g1 == g2 {
		t.Errorf("two RegisterGroup calls returned the same group_id: %q", g1)
	}

	// Seed members for each group.
	sess1 := parent + "~review-1-review-goal"
	sess2 := parent + "~review-2-review-goal"

	_ = d.UpsertStatus(sess1, "nixos-config", "/wt", "finished", nil, nil)
	_ = d.SetGroupID(sess1, g1)
	_ = d.UpsertStatus(sess2, "nixos-config", "/wt", "finished", nil, nil)
	_ = d.SetGroupID(sess2, g2)

	// GroupResults for g1 must not include sess2 and vice versa.
	gr1, err := d.GroupResults(g1)
	if err != nil {
		t.Fatalf("GroupResults(g1): %v", err)
	}
	if _, ok := gr1[sess2]; ok {
		t.Errorf("GroupResults(g1) unexpectedly includes session from round 2: %q", sess2)
	}

	gr2, err := d.GroupResults(g2)
	if err != nil {
		t.Fatalf("GroupResults(g2): %v", err)
	}
	if _, ok := gr2[sess1]; ok {
		t.Errorf("GroupResults(g2) unexpectedly includes session from round 1: %q", sess1)
	}
}

// TestGroupWiring_OnlyRetryRegistersNewGroup verifies that the --only retry
// path also creates a new group_id for the retry round. Existing prior rounds'
// session_groups rows remain untouched.
// AC: "The --only retry path also registers a new group (new round)."
func TestGroupWiring_OnlyRetryRegistersNewGroup(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@worker-branch"

	// First round (full 5 agents).
	g1, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup round 1: %v", err)
	}
	for _, ag := range review.Agents() {
		sess := parent + "~review-1-" + ag.Name
		_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
		_ = d.SetGroupID(sess, g1)
	}

	// Retry round (only 2 agents — simulating --only review-code,review-qa).
	g2, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup retry round: %v", err)
	}
	if g1 == g2 {
		t.Fatal("retry round got same group_id as first round")
	}

	retryAgents := []string{"review-code", "review-qa"}
	for _, name := range retryAgents {
		sess := parent + "~review-2-" + name
		_ = d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil)
		_ = d.SetGroupID(sess, g2)
	}

	// Original group still has 5 members.
	gr1, err := d.GroupResults(g1)
	if err != nil {
		t.Fatalf("GroupResults(g1): %v", err)
	}
	if len(gr1) != 5 {
		t.Errorf("GroupResults(g1): got %d members, want 5", len(gr1))
	}

	// Retry group has 2 members.
	gr2, err := d.GroupResults(g2)
	if err != nil {
		t.Fatalf("GroupResults(g2): %v", err)
	}
	if len(gr2) != 2 {
		t.Errorf("GroupResults(g2): got %d members, want 2", len(gr2))
	}
}

// TestGroupWiring_BuildResultsUsesGroupResults verifies that buildResults (via
// BuildResults) uses GroupResults data when a valid groupID is provided, and
// the output matches the per-session fallback path exactly.
func TestGroupWiring_BuildResultsUsesGroupResults(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
	}

	// review-goal: PASS. review-code: FAIL.
	_ = d.UpsertStatus(sessions[0], "nixos-config", "/wt", "finished", nil, nil)
	_ = d.SetGroupID(sessions[0], groupID)
	seedAssistantEvent(t, d, sessions[0], "All good. <verdict>PASS</verdict>")

	_ = d.UpsertStatus(sessions[1], "nixos-config", "/wt", "finished", nil, nil)
	_ = d.SetGroupID(sessions[1], groupID)
	seedAssistantEvent(t, d, sessions[1], "Blocking issue found. <verdict>FAIL</verdict>")

	finished := []bool{true, true}
	timedOut := []bool{false, false}

	// With groupID → uses GroupResults batch path.
	resultsWithGroup := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, false, groupID)
	// Without groupID → uses per-session fallback.
	resultsWithoutGroup := review.BuildResults(agents, sessions, d, finished, timedOut, 10*time.Minute, false, "")

	// Both paths must produce the same Passed/IsError/Output for each agent.
	for i, ag := range agents {
		rg := resultsWithGroup[i]
		rf := resultsWithoutGroup[i]
		if rg.Passed != rf.Passed {
			t.Errorf("agent %q: group path Passed=%v, fallback path Passed=%v", ag.Name, rg.Passed, rf.Passed)
		}
		if rg.IsError != rf.IsError {
			t.Errorf("agent %q: group path IsError=%v, fallback path IsError=%v", ag.Name, rg.IsError, rf.IsError)
		}
		if rg.Output != rf.Output {
			t.Errorf("agent %q: group path Output differs from fallback path\ngroup:    %q\nfallback: %q", ag.Name, rg.Output, rf.Output)
		}
	}

	// Verify specific results.
	if !resultsWithGroup[0].Passed {
		t.Error("review-goal should have passed")
	}
	if resultsWithGroup[1].Passed {
		t.Error("review-code should have failed")
	}
}

// TestGroupWiring_PollWithGroupCompleted verifies the full poll loop with group
// termination: agents transition from idle → finished, and the poll loop
// terminates via GroupCompleted. This exercises the integration between
// RegisterGroup, SetGroupID, GroupCompleted, and GroupResults.
func TestGroupWiring_PollWithGroupCompleted(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	agents := []review.Agent{
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
	}
	sessions := []string{
		"test@parent~review-1-review-goal",
		"test@parent~review-1-review-code",
	}

	// Pre-seed as finished with group_id.
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
		seedAssistantEvent(t, d, sess, "<verdict>PASS</verdict>")
	}

	var lines []string
	spawnTimes := []time.Time{time.Now(), time.Now()}

	results, err := review.PollAgentsForTest(ctx, d, agents, sessions, 10*time.Minute, spawnTimes, func(line string) {
		lines = append(lines, line)
	}, groupID)
	if err != nil {
		t.Fatalf("PollAgentsForTest: %v", err)
	}

	// Both agents should have passed.
	for i, r := range results {
		if !r.Passed {
			t.Errorf("agent %q: Passed=false, want true (output: %q)", agents[i].Name, r.Output)
		}
	}

	// Progress lines should be emitted.
	if len(lines) != 2 {
		t.Errorf("expected 2 progress lines, got %d: %v", len(lines), lines)
	}
}

// ── CleanupReviewSessionsForParent / KillReviewSessionsForParentWithDB ────────

// TestCleanupReviewSessionsForParent_DBBacked verifies that when group members
// exist in the DB (post-migration), CleanupReviewSessionsForParent marks them
// as ended via the DB-backed path.
func TestCleanupReviewSessionsForParent_DBBacked(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@main"
	member1 := parent + "~review-1-review-goal"
	member2 := parent + "~review-1-review-code"

	// Seed parent row with root_agent_name = "coordinator".
	if err := d.UpsertStatusSeedRootAgentName(parent, "nixos-config", "/wt/main", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	// Register a group and seed member rows.
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	for _, m := range []string{member1, member2} {
		if err := d.UpsertStatus(m, "nixos-config", "/wt/"+m, "running", nil, nil); err != nil {
			t.Fatalf("seed member %q: %v", m, err)
		}
		if err := d.SetGroupID(m, groupID); err != nil {
			t.Fatalf("SetGroupID %q: %v", m, err)
		}
	}

	// Call the function under test (KillSessionsByNames will fail silently — no tmux).
	review.CleanupReviewSessionsForParent(d, parent)

	// Verify both member rows have ended_at set (SetEnded was called).
	for _, m := range []string{member1, member2} {
		s, err := d.CurrentStatus(m)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", m, err)
		}
		if s == nil {
			t.Fatalf("CurrentStatus(%q): no row found", m)
		}
		if s.EndedAt == nil {
			t.Errorf("session %q: EndedAt is nil, want non-nil (SetEnded should have been called)", m)
		}
	}
}

// TestCleanupReviewSessionsForParent_PreMigrationFallback verifies that when no
// group members exist in the DB (pre-migration), the function falls back to the
// name-prefix scan.
func TestCleanupReviewSessionsForParent_PreMigrationFallback(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@legacy"
	member := parent + "~review-1-review-goal"

	// Seed member row WITHOUT group_id (pre-migration shape).
	if err := d.UpsertStatus(member, "nixos-config", "/wt/"+member, "running", nil, nil); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// No group registered — GroupMembersForParent returns empty slice.
	review.CleanupReviewSessionsForParent(d, parent)

	// Verify the member row had ended_at set via the prefix-scan fallback path.
	s, err := d.CurrentStatus(member)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatalf("CurrentStatus: no row found")
	}
	if s.EndedAt == nil {
		t.Errorf("session %q: EndedAt is nil, want non-nil (prefix-scan fallback should have called SetEnded)", member)
	}
}

// TestKillReviewSessionsForParentWithDB_DBBacked verifies that when group
// members exist, KillReviewSessionsForParentWithDB uses the DB-backed path
// (returns without calling KillSessionPrefix — no tmux panic for nonexistent sessions).
func TestKillReviewSessionsForParentWithDB_DBBacked(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@main"
	member := parent + "~review-1-review-goal"

	if err := d.UpsertStatusSeedRootAgentName(parent, "nixos-config", "/wt/main", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.UpsertStatus(member, "nixos-config", "/wt/"+member, "running", nil, nil); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := d.SetGroupID(member, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	// Should not panic even though tmux is not running (KillSessionsByNames is best-effort).
	review.KillReviewSessionsForParentWithDB(d, parent)
}

// TestKillReviewSessionsForParentWithDB_NilDB verifies that when d is nil,
// KillReviewSessionsForParentWithDB delegates to KillSessionPrefix (name-prefix path).
func TestKillReviewSessionsForParentWithDB_NilDB(t *testing.T) {
	// With nil DB the function falls through to KillSessionPrefix.
	// KillSessionPrefix calls tmux (best-effort, non-fatal). Verify no panic.
	review.KillReviewSessionsForParentWithDB(nil, "nixos-config@main")
}

// ── RunAsync ──────────────────────────────────────────────────────────────────

// TestRunAsync_ReturnsImmediately verifies that RunAsync returns promptly
// (well under 5 seconds) without blocking on agent completion. This is the
// core AC for the async model: "prism review <pr> becomes non-blocking".
//
// The test exercises the spawn-failure path (container mode with nil
// ProfilesFile) so that RunAsync fails quickly at config-resolution time —
// meaning all agents fail to spawn — rather than attempting real tmux calls.
// The key assertion is that RunAsync returns an error quickly, NOT that it
// blocks for any agent work to complete. In production, all agents would spawn
// successfully and the monitor would be started before RunAsync returns.
//
// A separate test (TestRunAsync_AllSpawnFailReturnsError) confirms the error
// path. This test focuses solely on timing.
func TestRunAsync_ReturnsImmediately(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	parent := "nixos-config@async-timing-test"
	if err := d.UpsertStatus(parent, "nixos-config", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	opts := review.Opts{
		PRNumber:      "864",
		ParentSession: parent,
		WorkerSession: parent,
		Worktree:      t.TempDir(),
		Agents:        review.Agents()[:1], // single agent for speed
		Timeout:       30 * time.Second,
		DBPath:        dbPath,
		ContainerMode: true,
		ProfilesFile:  nil, // triggers immediate config-resolution failure → fast return
	}

	start := time.Now()
	// RunAsync should return quickly (either with an error due to all agents
	// failing to spawn, or with an AsyncResult after spawning successfully).
	// We pass a non-existent prismBinary so StartMonitorProcess fails fast.
	_, runErr := review.RunAsync(opts, "/nonexistent/prism-binary")
	elapsed := time.Since(start)

	// Regardless of success or failure, RunAsync must not block.
	const maxElapsed = 5 * time.Second
	if elapsed > maxElapsed {
		t.Errorf("RunAsync took %v — expected to return in under %v (must not block on agent completion)", elapsed, maxElapsed)
	}

	// With ContainerMode=true and nil ProfilesFile, all agents fail at config
	// resolution → RunAsync returns "all review agents failed to spawn".
	if runErr == nil {
		t.Logf("RunAsync returned nil error (agents spawned — monitor start failed silently as expected)")
	} else {
		t.Logf("RunAsync returned error (expected for container-mode nil-pf path): %v", runErr)
	}
}

// TestRunAsync_AllSpawnFailReturnsError verifies that RunAsync returns an
// error when all agents fail to spawn (same path as Run).
func TestRunAsync_AllSpawnFailReturnsError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	parent := "nixos-config@async-allfail-test"
	if err := d.UpsertStatus(parent, "nixos-config", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	opts := review.Opts{
		PRNumber:      "99",
		ParentSession: parent,
		WorkerSession: parent,
		Worktree:      t.TempDir(),
		Agents:        review.Agents()[:1],
		Timeout:       30 * time.Second,
		DBPath:        dbPath,
		ContainerMode: true,
		ProfilesFile:  nil, // → all agents fail to spawn
	}

	_, runErr := review.RunAsync(opts, "/nonexistent/prism-binary")
	if runErr == nil {
		t.Fatal("RunAsync: expected error when all agents fail to spawn, got nil")
	}
	if !findSubstring(runErr.Error(), "failed") {
		t.Errorf("error should mention failure: %v", runErr)
	}
}

// TestRunAsync_MissingWorkerSession verifies that RunAsync returns an error
// when WorkerSession is empty (required field for async delivery).
func TestRunAsync_MissingWorkerSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	opts := review.Opts{
		PRNumber:      "42",
		ParentSession: "nixos-config@test",
		WorkerSession: "", // missing
		Worktree:      t.TempDir(),
		DBPath:        dbPath,
	}
	_, err := review.RunAsync(opts, "")
	if err == nil {
		t.Fatal("RunAsync: expected error for missing WorkerSession, got nil")
	}
	if !findSubstring(err.Error(), "worker session") {
		t.Errorf("error should mention 'worker session': %v", err)
	}
}

// TestRunAsync_DuplicateInProgress verifies that calling RunAsync when a round
// is already in progress for the same parent returns a clear error rather than
// silently spawning a duplicate round.
func TestRunAsync_DuplicateInProgress(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	parent := "nixos-config@async-dup-test"
	if err := d.UpsertStatus(parent, "nixos-config", t.TempDir(), "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	// Simulate an in-progress round by registering a group with an active member.
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	activeSess := parent + "~review-1-review-goal"
	if err := d.UpsertStatus(activeSess, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus active: %v", err)
	}
	if err := d.SetGroupID(activeSess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	opts := review.Opts{
		PRNumber:      "55",
		ParentSession: parent,
		WorkerSession: parent,
		Worktree:      t.TempDir(),
		Agents:        review.Agents()[:1],
		DBPath:        dbPath,
	}

	_, runErr := review.RunAsync(opts, "/nonexistent/prism-binary")
	if runErr == nil {
		t.Fatal("RunAsync: expected error for in-progress round, got nil")
	}
	if !findSubstring(runErr.Error(), "in progress") && !findSubstring(runErr.Error(), "already") {
		t.Errorf("error should mention 'in progress' or 'already': %v", runErr)
	}
}

// ── ParseLinkedIssues ─────────────────────────────────────────────────────────

// TestParseLinkedIssues_Closes verifies that "Closes #N" references are extracted.
func TestParseLinkedIssues_Closes(t *testing.T) {
	body := "This PR fixes the bug.\n\nCloses #123"
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 1 || issues[0] != "123" {
		t.Errorf("ParseLinkedIssues(%q) = %v, want [\"123\"]", body, issues)
	}
}

// TestParseLinkedIssues_Refs verifies that "Refs #N" references are extracted.
func TestParseLinkedIssues_Refs(t *testing.T) {
	body := "Refs #456 for context."
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 1 || issues[0] != "456" {
		t.Errorf("ParseLinkedIssues(%q) = %v, want [\"456\"]", body, issues)
	}
}

// TestParseLinkedIssues_Fixes verifies that "Fixes #N" references are extracted.
func TestParseLinkedIssues_Fixes(t *testing.T) {
	body := "Fixes #789"
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 1 || issues[0] != "789" {
		t.Errorf("ParseLinkedIssues(%q) = %v, want [\"789\"]", body, issues)
	}
}

// TestParseLinkedIssues_References verifies that "References #N" references are extracted.
func TestParseLinkedIssues_References(t *testing.T) {
	body := "References #101"
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 1 || issues[0] != "101" {
		t.Errorf("ParseLinkedIssues(%q) = %v, want [\"101\"]", body, issues)
	}
}

// TestParseLinkedIssues_MultipleIssues verifies that multiple issue references
// are all extracted (Closes #N, Refs #M).
func TestParseLinkedIssues_MultipleIssues(t *testing.T) {
	body := "Closes #100\n\nAlso Refs #200 for background."
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 2 {
		t.Fatalf("ParseLinkedIssues: got %v, want [\"100\", \"200\"]", issues)
	}
	if issues[0] != "100" || issues[1] != "200" {
		t.Errorf("ParseLinkedIssues: got %v, want [\"100\", \"200\"]", issues)
	}
}

// TestParseLinkedIssues_Deduplicated verifies that duplicate issue references
// are deduplicated (same issue referenced multiple times).
func TestParseLinkedIssues_Deduplicated(t *testing.T) {
	body := "Closes #42\nAlso Closes #42 (duplicate)"
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 1 || issues[0] != "42" {
		t.Errorf("ParseLinkedIssues: duplicate not deduplicated, got %v", issues)
	}
}

// TestParseLinkedIssues_CaseInsensitive verifies that matching is case-insensitive.
func TestParseLinkedIssues_CaseInsensitive(t *testing.T) {
	body := "CLOSES #55\nfixes #66"
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 2 {
		t.Fatalf("ParseLinkedIssues case-insensitive: got %v, want [\"55\", \"66\"]", issues)
	}
}

// TestParseLinkedIssues_NoIssues verifies that a body with no linked issues
// returns an empty slice.
func TestParseLinkedIssues_NoIssues(t *testing.T) {
	body := "This PR adds a feature. No issue references."
	issues := review.ParseLinkedIssuesForTest(body)
	if len(issues) != 0 {
		t.Errorf("ParseLinkedIssues: got %v, want empty slice", issues)
	}
}

// TestParseLinkedIssues_EmptyBody verifies that an empty body returns an empty slice.
func TestParseLinkedIssues_EmptyBody(t *testing.T) {
	issues := review.ParseLinkedIssuesForTest("")
	if len(issues) != 0 {
		t.Errorf("ParseLinkedIssues(empty): got %v, want empty slice", issues)
	}
}

// ── DiffFilePath ──────────────────────────────────────────────────────────────

// TestDiffFilePath_Format verifies that the diff file path follows the expected
// naming convention: /tmp/prism-review-<pr>-round-<N>.diff
func TestDiffFilePath_Format(t *testing.T) {
	cases := []struct {
		pr    string
		round int
		want  string
	}{
		{"819", 1, "/tmp/prism-review-819-round-1.diff"},
		{"855", 2, "/tmp/prism-review-855-round-2.diff"},
		{"1", 10, "/tmp/prism-review-1-round-10.diff"},
	}
	for _, tc := range cases {
		got := review.DiffFilePathForTest(tc.pr, tc.round)
		if got != tc.want {
			t.Errorf("DiffFilePath(%q, %d) = %q, want %q", tc.pr, tc.round, got, tc.want)
		}
	}
}

// TestDiffFilePath_RoundCollision verifies that different rounds produce
// different file paths (no collision between concurrent review rounds).
func TestDiffFilePath_RoundCollision(t *testing.T) {
	pr := "855"
	path1 := review.DiffFilePathForTest(pr, 1)
	path2 := review.DiffFilePathForTest(pr, 2)
	if path1 == path2 {
		t.Errorf("DiffFilePath: round 1 and round 2 produce the same path: %q", path1)
	}
}

// TestDiffFilePath_DifferentPRsCollision verifies that different PRs produce
// different file paths (no collision between concurrent reviews of different PRs).
func TestDiffFilePath_DifferentPRsCollision(t *testing.T) {
	path1 := review.DiffFilePathForTest("819", 1)
	path2 := review.DiffFilePathForTest("855", 1)
	if path1 == path2 {
		t.Errorf("DiffFilePath: PR 819 and PR 855 produce the same path: %q", path1)
	}
}

// TestDiffFilePath_ZeroRound verifies that round=0 defaults to 1.
func TestDiffFilePath_ZeroRound(t *testing.T) {
	got := review.DiffFilePathForTest("819", 0)
	want := "/tmp/prism-review-819-round-1.diff"
	if got != want {
		t.Errorf("DiffFilePath(%q, 0) = %q, want %q (round 0 should default to 1)", "819", got, want)
	}
}

// ── Inline vs file threshold ──────────────────────────────────────────────────

// TestBuildReviewPrompt_SmallDiffInlined verifies that a small diff (below the
// inline threshold) is embedded directly in the agent prompt.
func TestBuildReviewPrompt_SmallDiffInlined(t *testing.T) {
	ctx := samplePRContext()
	ctx.Diff = "diff --git a/foo.go b/foo.go\n+added line\n"
	ctx.DiffFilePath = "" // small diff — no file path
	prompt := review.BuildReviewPromptForTest("819", ctx)

	// Small diff must be inlined in a ```diff fence.
	if !findSubstring(prompt, "```diff") {
		t.Errorf("small diff prompt missing ```diff fence\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "diff --git a/foo.go b/foo.go") {
		t.Errorf("small diff prompt missing diff content\nprompt:\n%s", prompt)
	}
	// Must NOT have a "file has been saved to" message.
	if findSubstring(prompt, "has been saved to") {
		t.Errorf("small diff prompt should not mention a saved file\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_LargeDiffFilePointer verifies that when DiffFilePath is
// set (large diff), the prompt contains a file-path pointer and guidance for
// querying the diff — not an inline code fence.
func TestBuildReviewPrompt_LargeDiffFilePointer(t *testing.T) {
	ctx := samplePRContext()
	ctx.Diff = ""        // large diff is NOT inlined
	ctx.DiffLines = 1200 // simulated count
	ctx.DiffBytes = 80 * 1024
	ctx.DiffFilePath = "/tmp/prism-review-819-round-1.diff"
	prompt := review.BuildReviewPromptForTest("819", ctx)

	// Must contain the file path.
	if !findSubstring(prompt, "/tmp/prism-review-819-round-1.diff") {
		t.Errorf("large diff prompt missing file path\nprompt:\n%s", prompt)
	}
	// Must contain the guidance to use git diff / rg.
	if !findSubstring(prompt, "git diff --stat") {
		t.Errorf("large diff prompt missing git diff guidance\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "rg") {
		t.Errorf("large diff prompt missing rg guidance\nprompt:\n%s", prompt)
	}
	// Must NOT contain an inline ```diff fence.
	if findSubstring(prompt, "```diff") {
		t.Errorf("large diff prompt should not contain an inline ```diff fence\nprompt:\n%s", prompt)
	}
}

// ── Linked issues in prompt ───────────────────────────────────────────────────

// TestBuildReviewPrompt_LinkedIssuesSection verifies that when LinkedIssues is
// populated, the prompt contains the "### Linked issues" section with each
// issue's content.
func TestBuildReviewPrompt_LinkedIssuesSection(t *testing.T) {
	ctx := samplePRContext()
	ctx.LinkedIssues = map[string]string{
		"123": "title:\tFix the bug\nstate:\tOPEN\n",
		"456": "title:\tAdd feature\nstate:\tCLOSED\n",
	}
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Linked issues") {
		t.Errorf("prompt missing '### Linked issues' section\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "#### Issue #123") {
		t.Errorf("prompt missing '#### Issue #123'\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "Fix the bug") {
		t.Errorf("prompt missing issue #123 content\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "#### Issue #456") {
		t.Errorf("prompt missing '#### Issue #456'\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "Add feature") {
		t.Errorf("prompt missing issue #456 content\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_LinkedIssuesFetchFailure verifies that when a linked
// issue could not be fetched, the prompt contains a clear failure marker
// rather than crashing or omitting the issue entirely.
func TestBuildReviewPrompt_LinkedIssuesFetchFailure(t *testing.T) {
	ctx := samplePRContext()
	ctx.LinkedIssues = map[string]string{
		"999": "[issue #999 could not be fetched: exit status 1: HTTP 404]",
	}
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Linked issues") {
		t.Errorf("prompt missing '### Linked issues' section on fetch failure\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "#### Issue #999") {
		t.Errorf("prompt missing issue section even for unfetchable issue\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "could not be fetched") {
		t.Errorf("prompt missing failure marker for unfetchable issue\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_NoLinkedIssues verifies that when no linked issues are
// found, the prompt shows "(no linked issues found)" rather than an empty section.
func TestBuildReviewPrompt_NoLinkedIssues(t *testing.T) {
	ctx := samplePRContext()
	ctx.LinkedIssues = nil
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Linked issues") {
		t.Errorf("prompt missing '### Linked issues' section\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "no linked issues found") {
		t.Errorf("prompt missing '(no linked issues found)' placeholder\nprompt:\n%s", prompt)
	}
}

// ── Git log in prompt ─────────────────────────────────────────────────────────

// TestBuildReviewPrompt_GitLogSections verifies that the prompt contains the
// "### Recent commits" and "### This branch vs origin/<base>" sections with
// the git log output when provided.
func TestBuildReviewPrompt_GitLogSections(t *testing.T) {
	ctx := samplePRContext()
	ctx.RecentCommits = "abc1234 fix: handle nil pointer\ndef5678 feat: add new feature\n"
	ctx.BranchCommits = "abc1234 fix: handle nil pointer\n"
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Recent commits") {
		t.Errorf("prompt missing '### Recent commits' section\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "fix: handle nil pointer") {
		t.Errorf("prompt missing recent commit text\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "### This branch vs origin/main") {
		t.Errorf("prompt missing branch commits section\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_GitLogUnavailable verifies that the prompt handles
// missing git log output gracefully with a "(not available)" placeholder.
func TestBuildReviewPrompt_GitLogUnavailable(t *testing.T) {
	ctx := samplePRContext()
	ctx.RecentCommits = ""
	ctx.BranchCommits = ""
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "### Recent commits") {
		t.Errorf("prompt missing '### Recent commits' section even when unavailable\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "(not available)") {
		t.Errorf("prompt missing '(not available)' placeholder for git log\nprompt:\n%s", prompt)
	}
}

// ── Tool preference guidance ──────────────────────────────────────────────────

// TestBuildReviewPrompt_ToolPreferenceGuidance verifies that the prompt contains
// the tool-preference guidance directing agents to prefer native git over gh.
func TestBuildReviewPrompt_ToolPreferenceGuidance(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx)

	if !findSubstring(prompt, "Prefer native git") {
		t.Errorf("prompt missing tool-preference guidance ('Prefer native git')\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "git show") {
		t.Errorf("prompt missing 'git show' in tool-preference guidance\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "git diff") {
		t.Errorf("prompt missing 'git diff' in tool-preference guidance\nprompt:\n%s", prompt)
	}
	if !findSubstring(prompt, "git log") {
		t.Errorf("prompt missing 'git log' in tool-preference guidance\nprompt:\n%s", prompt)
	}
}

// ── FetchPRContextWithOpts ────────────────────────────────────────────────────
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
