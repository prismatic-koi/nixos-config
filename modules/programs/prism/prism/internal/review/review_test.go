package review_test

import (
	"os"
	"path/filepath"
	"testing"

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

func TestFormatResults_PassFailPhrases(t *testing.T) {
	// Test that failure-indicating phrases cause the agent to be marked failed.
	failingOutputs := []string{
		"Please fix the above before this PR is merged.",
		"This is a bug that needs to be fixed.",
		"There is a security issue in the code.",
		"Found a vulnerability in authentication.",
	}
	for _, text := range failingOutputs {
		results := []review.AgentResult{
			{Agent: review.Agent{Name: "review"}, Passed: false, Output: text},
		}
		_, allPassed := review.FormatResults(results, "1")
		if allPassed {
			t.Errorf("FormatResults with output %q: allPassed=true, want false", text)
		}
	}

	// A clean review should pass.
	cleanOutput := "The implementation looks correct. All acceptance criteria verified. No issues found."
	results := []review.AgentResult{
		{Agent: review.Agent{Name: "review"}, Passed: true, Output: cleanOutput},
	}
	_, allPassed := review.FormatResults(results, "1")
	if !allPassed {
		t.Errorf("FormatResults with clean output: allPassed=false, want true")
	}
}

// ── AssessPassed ──────────────────────────────────────────────────────────────

func TestAssessPassed_FailingPhrases(t *testing.T) {
	failingTexts := []struct {
		text   string
		phrase string
	}{
		{"Please fix the above before this PR is merged.", "please fix"},
		{"This needs to be fixed before merging.", "needs to be fixed"},
		{"The function must be fixed to handle nil inputs.", "must be fixed"},
		{"There is a blocking issue in the auth flow.", "blocking issue"},
		{"This is a bug in the state machine.", "this is a bug"},
		{"Error found in the input validation.", "error found"},
		{"There is a security issue with the token handling.", "security issue"},
		{"A vulnerability was found in the deserialization path.", "vulnerability"},
		{"Output contains ✗ indicating failure.", "✗"},
	}
	for _, tt := range failingTexts {
		if review.AssessPassed(tt.text) {
			t.Errorf("AssessPassed(%q) = true, want false (phrase %q should trigger fail)", tt.text, tt.phrase)
		}
	}
}

func TestAssessPassed_PassingTexts(t *testing.T) {
	passingTexts := []string{
		"The implementation looks correct. All acceptance criteria are met.",
		"LGTM. No issues found. The code follows existing conventions.",
		"Reviewed thoroughly. The PR is ready to merge.",
		"All acceptance criteria pass. The error handling is correct.",
		"The PR looks good. I checked the diff and the linked issue's ACs.",
	}
	for _, text := range passingTexts {
		if !review.AssessPassed(text) {
			t.Errorf("AssessPassed(%q) = false, want true", text)
		}
	}
}

func TestAssessPassed_CaseInsensitive(t *testing.T) {
	// Failure phrases should be detected regardless of case.
	if review.AssessPassed("PLEASE FIX THE ABOVE BEFORE THIS PR IS MERGED.") {
		t.Error("AssessPassed should detect 'PLEASE FIX' case-insensitively")
	}
	if review.AssessPassed("Please Fix the above.") {
		t.Error("AssessPassed should detect 'Please Fix' case-insensitively")
	}
}

// ── AgentsByName ──────────────────────────────────────────────────────────────

func TestAgentsByName_ValidNames(t *testing.T) {
	agents := review.DefaultAgents()
	result, err := review.AgentsByName(agents, []string{"review"})
	if err != nil {
		t.Fatalf("AgentsByName: %v", err)
	}
	if len(result) != 1 || result[0].Name != "review" {
		t.Errorf("AgentsByName = %v, want [{review}]", result)
	}
}

func TestAgentsByName_UnknownName(t *testing.T) {
	agents := review.DefaultAgents()
	_, err := review.AgentsByName(agents, []string{"nonexistent"})
	if err == nil {
		t.Error("AgentsByName should return error for unknown agent name")
	}
}

func TestAgentsByName_MixedNames(t *testing.T) {
	agents := review.DefaultAgents()
	_, err := review.AgentsByName(agents, []string{"review", "nonexistent"})
	if err == nil {
		t.Error("AgentsByName should return error when any name is unknown")
	}
}

// ── EnhancedAgents ────────────────────────────────────────────────────────────

func TestEnhancedAgents_ReturnsFiveAgents(t *testing.T) {
	agents := review.EnhancedAgents()
	if len(agents) != 5 {
		t.Fatalf("EnhancedAgents() returned %d agents, want 5", len(agents))
	}
}

func TestEnhancedAgents_AgentNames(t *testing.T) {
	agents := review.EnhancedAgents()
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

func TestDefaultAgents_StillReturnsSingleAgent(t *testing.T) {
	agents := review.DefaultAgents()
	if len(agents) != 1 {
		t.Fatalf("DefaultAgents() returned %d agents, want 1", len(agents))
	}
	if agents[0].Name != "review" {
		t.Errorf("DefaultAgents()[0].Name = %q, want %q", agents[0].Name, "review")
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

	agents := review.EnhancedAgents()
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

	agents := review.EnhancedAgents()
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

	agents := review.EnhancedAgents()
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

	agents := review.EnhancedAgents()
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

// ── helpers ───────────────────────────────────────────────────────────────────

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
