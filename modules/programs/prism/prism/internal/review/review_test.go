package review_test

import (
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

func TestNextRoundNumber_WithExistingRounds(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@feature"

	_ = d.UpsertStatus(parent+"~review-1", "nixos-config", "/wt", "finished", nil, nil)
	_ = d.UpsertStatus(parent+"~review-2", "nixos-config", "/wt", "idle", nil, nil)
	// Agent sub-session — should NOT count as a round.
	_ = d.UpsertStatus(parent+"~review-1~review", "nixos-config", "/wt", "idle", nil, nil)

	n := review.NextRoundNumber(d, parent)
	if n != 3 {
		t.Errorf("NextRoundNumber = %d, want 3", n)
	}
}

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
