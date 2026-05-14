package review_test

// prompt_test.go — unit tests for buildReviewPrompt and resolveRoleDefinition.
//
// These tests exercise the role-definition splice behaviour introduced in
// issue #1439: the full role rubric is now embedded inline in the prompt so
// that every harness (opencode, PI, etc.) receives it without relying on an
// out-of-band system-prompt injection.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// setupAgentsDir creates a temporary $XDG_CONFIG_HOME, registers the cleanup,
// and returns the path to the agents directory. The caller may write role files
// into it before calling BuildReviewPromptForTest.
func setupAgentsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	agentsDir := filepath.Join(dir, "prism", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return agentsDir
}

// ── Role file present ─────────────────────────────────────────────────────────

// TestBuildReviewPrompt_RoleFilePresentAndSpliced verifies that when the role
// definition file exists and is non-empty, its full content appears in the
// prompt under the "## Your role-specific instructions" heading.
//
// The file is named using the ValidationName form ("review-security-subagent.md")
// to match the actual on-disk layout (#1231).
func TestBuildReviewPrompt_RoleFilePresentAndSpliced(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	const roleContent = "# review-security\n\nYou are a security auditor. Check for vulnerabilities.\n"
	// Files on disk use the "-subagent" suffix (Agent.ValidationName form).
	if err := os.WriteFile(filepath.Join(agentsDir, "review-security-subagent.md"), []byte(roleContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Pass the ValidationName ("review-security-subagent") as the roleFile argument.
	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-security-subagent")

	// Role-section header must be present.
	if !findSubstring(prompt, "## Your role-specific instructions") {
		t.Errorf("prompt missing '## Your role-specific instructions'\nprompt:\n%s", prompt)
	}
	// Full role content must be spliced in.
	if !findSubstring(prompt, "You are a security auditor") {
		t.Errorf("prompt missing role content\nprompt:\n%s", prompt)
	}
	// The old dangling trailer must NOT appear.
	if findSubstring(prompt, "Your role-specific instructions follow below.") {
		t.Errorf("prompt must not contain old dangling trailer\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_AllFiveRolesGetTheirOwnRubric verifies that each of
// the five review agents receives the content of its own definition file,
// not a shared or empty block.
//
// Files are written using the ValidationName ("-subagent") suffix to match
// the actual on-disk layout rendered by the NixOS module (#1231).
func TestBuildReviewPrompt_AllFiveRolesGetTheirOwnRubric(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	// validationNames mirrors Agent.ValidationName for each review agent.
	validationNames := []string{
		"review-goal-subagent",
		"review-code-subagent",
		"review-security-subagent",
		"review-qa-subagent",
		"review-context-subagent",
	}
	for _, vn := range validationNames {
		content := "# " + vn + " unique rubric content\n"
		if err := os.WriteFile(filepath.Join(agentsDir, vn+".md"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile for %s: %v", vn, err)
		}
	}

	ctx := samplePRContext()
	for _, vn := range validationNames {
		prompt := review.BuildReviewPromptForTest("99", ctx, vn)
		expectedContent := vn + " unique rubric content"
		if !findSubstring(prompt, expectedContent) {
			t.Errorf("role %q: prompt missing expected rubric content %q\nprompt:\n%s", vn, expectedContent, prompt)
		}
		// Other roles' content must NOT appear (each prompt is distinct).
		for _, otherVN := range validationNames {
			if otherVN == vn {
				continue
			}
			otherContent := otherVN + " unique rubric content"
			if findSubstring(prompt, otherContent) {
				t.Errorf("role %q: prompt unexpectedly contains content for other role %q\nprompt:\n%s", vn, otherVN, prompt)
			}
		}
	}
}

// ── Role file missing ─────────────────────────────────────────────────────────

// TestBuildReviewPrompt_MissingRoleFile_NoErrorNoPanic verifies that when the
// role definition file is absent, buildReviewPrompt returns a valid prompt
// without panicking or returning an error.
func TestBuildReviewPrompt_MissingRoleFile_NoErrorNoPanic(t *testing.T) {
	// Point XDG_CONFIG_HOME at an empty temp dir — no agent files exist.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Must not panic.
	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-goal")

	// Must still be a non-empty string.
	if len(prompt) == 0 {
		t.Fatal("prompt is empty; expected a non-empty fallback prompt")
	}
}

// TestBuildReviewPrompt_MissingRoleFile_ContainsNotice verifies that when the
// role definition file is absent, the prompt includes a clearly-marked notice
// so the receiving agent and human readers can see what happened.
//
// The roleFile argument uses the ValidationName form ("review-goal-subagent")
// since that is what the call sites pass.
func TestBuildReviewPrompt_MissingRoleFile_ContainsNotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-goal-subagent")

	// Must contain a notice mentioning the roleFile stem and "not found".
	if !findSubstring(prompt, "role definition for review-goal-subagent not found") {
		t.Errorf("prompt should contain 'role definition for review-goal-subagent not found'\nprompt:\n%s", prompt)
	}
	// The notice must name the path so readers know where to look.
	if !findSubstring(prompt, "prism/agents/review-goal-subagent.md") {
		t.Errorf("prompt should include the expected path in the notice\nprompt:\n%s", prompt)
	}
}

// ── Role file empty ───────────────────────────────────────────────────────────

// TestBuildReviewPrompt_EmptyRoleFile_ContainsSameNoticeAsMissing verifies that
// when the role definition file exists but is empty (or whitespace-only), the
// prompt includes the same clearly-marked notice as the missing-file case,
// rather than appending blank lines.
func TestBuildReviewPrompt_EmptyRoleFile_ContainsSameNoticeAsMissing(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	// Write a whitespace-only file using the ValidationName ("-subagent") suffix.
	if err := os.WriteFile(filepath.Join(agentsDir, "review-code-subagent.md"), []byte("   \n\t\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-code-subagent")

	// Must contain the same "not found" notice as the missing-file case.
	if !findSubstring(prompt, "role definition for review-code-subagent not found") {
		t.Errorf("prompt should contain 'role definition for review-code-subagent not found' for empty file\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_EmptyRoleFile_NoBlanksOnly verifies that the empty-file
// case does not result in a prompt that simply appends blank lines where the
// role content should be.
func TestBuildReviewPrompt_EmptyRoleFile_NoBlanksOnly(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	// Use the ValidationName ("-subagent") suffix to match production layout.
	if err := os.WriteFile(filepath.Join(agentsDir, "review-qa-subagent.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-qa-subagent")

	// The role section must contain a notice, not just trailing whitespace.
	if !findSubstring(prompt, "not found") {
		t.Errorf("prompt for empty role file should contain 'not found' notice\nprompt:\n%s", prompt)
	}
}

// ── Trailer absent in all cases ───────────────────────────────────────────────

// TestBuildReviewPrompt_TrailerAbsentWhenFilePresent verifies that the old
// "Your role-specific instructions follow below." trailer is absent even when
// the role file exists and is spliced in successfully.
func TestBuildReviewPrompt_TrailerAbsentWhenFilePresent(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	// Use the ValidationName ("-subagent") suffix to match production layout.
	if err := os.WriteFile(filepath.Join(agentsDir, "review-context-subagent.md"), []byte("# context rubric\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-context-subagent")

	if findSubstring(prompt, "Your role-specific instructions follow below.") {
		t.Errorf("prompt must not contain the old dangling trailer\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_TrailerAbsentWhenFileMissing verifies that the old
// trailer is also absent when the role file is missing.
func TestBuildReviewPrompt_TrailerAbsentWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-security-subagent")

	if findSubstring(prompt, "Your role-specific instructions follow below.") {
		t.Errorf("prompt must not contain the old dangling trailer\nprompt:\n%s", prompt)
	}
}

// ── Context sections unchanged ────────────────────────────────────────────────

// TestBuildReviewPrompt_ContextSectionsUnchanged verifies that the presence of
// the role-definition splice does not alter the PR metadata, recent commits,
// branch commits, or diff sections.
func TestBuildReviewPrompt_ContextSectionsUnchanged(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	// Use the ValidationName ("-subagent") suffix to match production layout.
	if err := os.WriteFile(filepath.Join(agentsDir, "review-goal-subagent.md"), []byte("role rubric\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx, "review-goal-subagent")

	// All core context sections must still be present.
	required := []string{
		"## Context for your review",
		"### PR metadata",
		"### Recent commits",
		"### Linked issues",
		"### Diff",
		"PR #819",
		"inject-pr-context-into-review",
		"diff --git a/foo.go b/foo.go",
	}
	for _, s := range required {
		if !findSubstring(prompt, s) {
			t.Errorf("context section missing %q after role splice\nprompt:\n%s", s, prompt)
		}
	}
}
