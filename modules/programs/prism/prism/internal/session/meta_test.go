package session

import "testing"

func TestIsMetaSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		// Known meta-sessions — must return true.
		{"scratchpad", true},
		{"prism-dashboard", true},

		// Non-meta sessions — must return false.
		{"nixos-config@main", false},
		{"nixos-config@feature-branch", false},
		{"worker", false},
		{"coordinator", false},
		{"obsidian", false},
		{"", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsMetaSession(tt.name)
			if got != tt.want {
				t.Errorf("IsMetaSession(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
