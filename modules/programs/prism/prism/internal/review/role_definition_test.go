package review

// role_definition_test.go — white-box unit tests for roleDefinitionMissing
// and roleDefinitionPath (issue #2534).
//
// These are the sole remaining production-side readers of the role
// definition file's presence: they no longer read the file's *content* into
// the review prompt (that duty now belongs entirely to prism.ts's
// composeRoleSystemPrompt), but run.go still checks existence so a missing
// or empty role file is surfaced to the operator via OnProgress instead of
// failing silently.

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRoleAgentsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	agentsDir := filepath.Join(dir, "prism", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return agentsDir
}

func TestRoleDefinitionMissing_FilePresentAndNonEmpty(t *testing.T) {
	agentsDir := setupRoleAgentsDir(t)
	if err := os.WriteFile(filepath.Join(agentsDir, "review-security.md"), []byte("rubric content\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if roleDefinitionMissing("review-security") {
		t.Error("roleDefinitionMissing returned true for a present, non-empty file")
	}
}

func TestRoleDefinitionMissing_FileAbsent(t *testing.T) {
	setupRoleAgentsDir(t)

	if !roleDefinitionMissing("review-goal") {
		t.Error("roleDefinitionMissing returned false for an absent file")
	}
}

func TestRoleDefinitionMissing_FileEmpty(t *testing.T) {
	agentsDir := setupRoleAgentsDir(t)
	if err := os.WriteFile(filepath.Join(agentsDir, "review-qa.md"), []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !roleDefinitionMissing("review-qa") {
		t.Error("roleDefinitionMissing returned false for a whitespace-only file")
	}
}

func TestRoleDefinitionPath_IncludesRoleFileStem(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got := roleDefinitionPath("review-context")
	want := filepath.Join(dir, "prism", "agents", "review-context.md")
	if got != want {
		t.Errorf("roleDefinitionPath(%q) = %q, want %q", "review-context", got, want)
	}
}
