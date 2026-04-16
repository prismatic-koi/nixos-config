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
		{"nixos-config@feature~review-1", true},
		{"nixos-config@feature~review-2", true},
		{"nixos-config@feature~review-1~review", true}, // agent sub-session
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
		{"nixos-config@feature~review-1", "@feature"},
		{"nixos-config@feature~review-2", "@feature"},
		{"nixos-config@feature~review-1~review", "@feature"},
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
		{"nixos-config@feature~review-1", "~review-1"},
		{"nixos-config@feature~review-2", "~review-2"},
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
