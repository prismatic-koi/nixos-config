package cmd

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestAgentsForHarness_EnvUnset verifies that when ENHANCED_REVIEW is not set,
// agentsForHarness returns the single default agent set.
func TestAgentsForHarness_EnvUnset(t *testing.T) {
	os.Unsetenv("ENHANCED_REVIEW")

	agents := agentsForHarness("opencode")
	want := review.DefaultAgents()
	if len(agents) != len(want) {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW unset: got %d agents, want %d", len(agents), len(want))
	}
	for i, a := range agents {
		if a.Name != want[i].Name || a.OpencodeName != want[i].OpencodeName {
			t.Errorf("agent[%d]: got {%q, %q}, want {%q, %q}", i, a.Name, a.OpencodeName, want[i].Name, want[i].OpencodeName)
		}
	}
}

// TestAgentsForHarness_EnvTrue verifies that when ENHANCED_REVIEW=true,
// agentsForHarness returns the five-agent enhanced set.
func TestAgentsForHarness_EnvTrue(t *testing.T) {
	t.Setenv("ENHANCED_REVIEW", "true")

	agents := agentsForHarness("opencode")
	want := review.EnhancedAgents()
	if len(agents) != 5 {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=true: got %d agents, want 5", len(agents))
	}
	if len(agents) != len(want) {
		t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=true: got %d agents, want %d", len(agents), len(want))
	}
	for i, a := range agents {
		if a.Name != want[i].Name || a.OpencodeName != want[i].OpencodeName {
			t.Errorf("agent[%d]: got {%q, %q}, want {%q, %q}", i, a.Name, a.OpencodeName, want[i].Name, want[i].OpencodeName)
		}
	}
}

// TestAgentsForHarness_EnvFalse verifies that ENHANCED_REVIEW=false (or any
// non-"true" value) returns the default single-agent set.
func TestAgentsForHarness_EnvFalse(t *testing.T) {
	for _, val := range []string{"false", "0", "yes", "TRUE", "1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("ENHANCED_REVIEW", val)

			agents := agentsForHarness("opencode")
			want := review.DefaultAgents()
			if len(agents) != len(want) {
				t.Fatalf("agentsForHarness(opencode) with ENHANCED_REVIEW=%q: got %d agents, want %d", val, len(agents), len(want))
			}
			if agents[0].Name != "review" {
				t.Errorf("agentsForHarness(opencode) with ENHANCED_REVIEW=%q: got agent name %q, want %q", val, agents[0].Name, "review")
			}
		})
	}
}

// TestAgentsForHarness_EnhancedAgentNames verifies that the enhanced agent names
// match the expected opencode agent identifiers.
func TestAgentsForHarness_EnhancedAgentNames(t *testing.T) {
	t.Setenv("ENHANCED_REVIEW", "true")

	agents := agentsForHarness("opencode")
	expectedNames := []string{
		"review-goal",
		"review-code",
		"review-security",
		"review-qa",
		"review-context",
	}
	for i, name := range expectedNames {
		if i >= len(agents) {
			t.Fatalf("not enough agents: got %d, want at least %d", len(agents), i+1)
		}
		if agents[i].Name != name {
			t.Errorf("agents[%d].Name = %q, want %q", i, agents[i].Name, name)
		}
		if agents[i].OpencodeName != name {
			t.Errorf("agents[%d].OpencodeName = %q, want %q", i, agents[i].OpencodeName, name)
		}
	}
}
