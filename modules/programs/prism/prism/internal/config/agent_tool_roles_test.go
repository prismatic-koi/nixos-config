package config_test

// agent_tool_roles_test.go — coverage for the role-aware pi builtin tool
// exclusion (issue #2531).

import (
	"reflect"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func TestExcludedToolsForRole_ReviewRolesExcludeWriteAndEdit(t *testing.T) {
	for _, role := range reviewRoles {
		t.Run(role, func(t *testing.T) {
			got := config.ExcludedToolsForRole(role)
			want := []string{"write", "edit"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ExcludedToolsForRole(%q) = %v, want %v", role, got, want)
			}
		})
	}
}

func TestExcludedToolsForRole_NonReviewRolesGetFullSet(t *testing.T) {
	roles := []string{
		"coordinator",
		"worker",
		"investigate",
		"ac",
		"retro",
		"",
		"review",
		"review-performance",
		"totally-made-up",
	}
	for _, role := range roles {
		t.Run("role="+role, func(t *testing.T) {
			if got := config.ExcludedToolsForRole(role); got != nil {
				t.Errorf("ExcludedToolsForRole(%q) = %v, want nil (full builtin set)", role, got)
			}
		})
	}
}

// TestExcludedToolsForRole_ReturnsACopy verifies a caller mutating the
// returned slice cannot corrupt the shared exclusion list for later callers.
func TestExcludedToolsForRole_ReturnsACopy(t *testing.T) {
	got := config.ExcludedToolsForRole("review-code")
	got[0] = "mutated"
	again := config.ExcludedToolsForRole("review-code")
	if again[0] != "write" {
		t.Errorf("ExcludedToolsForRole shares backing storage across calls: got %v", again)
	}
}
