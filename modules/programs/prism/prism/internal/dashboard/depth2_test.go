package dashboard_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

func TestIsDepth2Session(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"nixos-config@main", false},
		{"nixos-config@feature", false},
		{"scratchpad", false},
		// Old-shape round sessions (pre-PR-C) — still detected as depth-2.
		{"nixos-config@feature~review-1", true},
		{"nixos-config@feature~review-2", true},
		// Old-shape agent sub-sessions (pre-PR-C).
		{"nixos-config@feature~review-1~review", true},
		// New per-agent session shape (PR-C).
		{"nixos-config@feature~review-1-review-goal", true},
		{"nixos-config@feature~review-1-review-code", true},
		{"nixos-config@feature~review-2-review-qa", true},
		{"repo@branch~something", true},
	}
	for _, tt := range tests {
		got := dashboard.IsDepth2Session(tt.name)
		if got != tt.want {
			t.Errorf("IsDepth2Session(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDepth2ParentBranch(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Old-shape (pre-PR-C).
		{"nixos-config@feature~review-1", "@feature"},
		{"nixos-config@feature~review-2", "@feature"},
		{"nixos-config@feature~review-1~review", "@feature"},
		// New per-agent shape (PR-C).
		{"nixos-config@feature~review-1-review-goal", "@feature"},
		{"nixos-config@feature~review-2-review-security", "@feature"},
		// Non-review depth-2.
		{"nixos-config@main", ""},    // top-level
		{"nixos-config@feature", ""}, // depth-1, no ~
		{"scratchpad", ""},
	}
	for _, tt := range tests {
		got := dashboard.Depth2ParentBranch(tt.name)
		if got != tt.want {
			t.Errorf("Depth2ParentBranch(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDepth2Label(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Old-shape (pre-PR-C).
		{"nixos-config@feature~review-1", "~review-1"},
		{"nixos-config@feature~review-2", "~review-2"},
		// New per-agent shape (PR-C): label includes agent name.
		{"nixos-config@feature~review-1-review-goal", "~review-1-review-goal"},
		{"nixos-config@feature~review-1-review-code", "~review-1-review-code"},
		{"nixos-config@feature~review-2-review-qa", "~review-2-review-qa"},
		// Non-review.
		{"nixos-config@main", ""},
		{"nixos-config@feature", ""},
		{"scratchpad", ""},
	}
	for _, tt := range tests {
		got := dashboard.Depth2Label(tt.name)
		if got != tt.want {
			t.Errorf("Depth2Label(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSortDisplayed_Depth2AfterParent(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature~review-2"},
		{Name: "nixos-config@main"},
		{Name: "nixos-config@feature~review-1"},
		{Name: "nixos-config@feature"},
	}
	dashboard.SortDisplayed(sessions)

	// Expected order: @main, @feature, @feature~review-1, @feature~review-2
	want := []string{
		"nixos-config@main",
		"nixos-config@feature",
		"nixos-config@feature~review-1",
		"nixos-config@feature~review-2",
	}
	for i, w := range want {
		if sessions[i].Name != w {
			t.Errorf("position %d: got %q, want %q", i, sessions[i].Name, w)
		}
	}
}

// TestSortDisplayed_PerAgentSessionsAfterParent verifies that the new per-agent
// session shape (PR-C) sorts correctly: all 5 agents grouped under the parent
// branch, sorted alphabetically by ~review-N-<agent> label.
func TestSortDisplayed_PerAgentSessionsAfterParent(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature~review-1-review-security"},
		{Name: "nixos-config@main"},
		{Name: "nixos-config@feature~review-1-review-goal"},
		{Name: "nixos-config@feature~review-1-review-qa"},
		{Name: "nixos-config@feature"},
		{Name: "nixos-config@feature~review-1-review-code"},
		{Name: "nixos-config@feature~review-1-review-context"},
	}
	dashboard.SortDisplayed(sessions)

	// Expected: @main first, then @feature, then all 5 review agents sorted
	// alphabetically by label under @feature.
	want := []string{
		"nixos-config@main",
		"nixos-config@feature",
		"nixos-config@feature~review-1-review-code",
		"nixos-config@feature~review-1-review-context",
		"nixos-config@feature~review-1-review-goal",
		"nixos-config@feature~review-1-review-qa",
		"nixos-config@feature~review-1-review-security",
	}
	for i, w := range want {
		if i >= len(sessions) {
			t.Fatalf("position %d: want %q but only %d sessions", i, w, len(sessions))
		}
		if sessions[i].Name != w {
			t.Errorf("position %d: got %q, want %q", i, sessions[i].Name, w)
		}
	}
}

// TestSortDisplayed_MultipleRoundsPerAgentSessions verifies that sessions from
// round 1 all appear before sessions from round 2 (sorted by label).
func TestSortDisplayed_MultipleRoundsPerAgentSessions(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature~review-2-review-goal"},
		{Name: "nixos-config@feature~review-1-review-goal"},
		{Name: "nixos-config@feature~review-2-review-code"},
		{Name: "nixos-config@feature~review-1-review-code"},
		{Name: "nixos-config@feature"},
	}
	dashboard.SortDisplayed(sessions)

	// Labels: ~review-1-review-code < ~review-1-review-goal < ~review-2-review-code < ~review-2-review-goal
	want := []string{
		"nixos-config@feature",
		"nixos-config@feature~review-1-review-code",
		"nixos-config@feature~review-1-review-goal",
		"nixos-config@feature~review-2-review-code",
		"nixos-config@feature~review-2-review-goal",
	}
	for i, w := range want {
		if i >= len(sessions) {
			t.Fatalf("position %d: want %q but only %d sessions", i, w, len(sessions))
		}
		if sessions[i].Name != w {
			t.Errorf("position %d: got %q, want %q", i, sessions[i].Name, w)
		}
	}
}
