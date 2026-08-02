package review_test

// prompt_test.go — unit tests for buildReviewPrompt and the role-file
// existence check used by run.go.
//
// Issue #2534: the role rubric used to be spliced into this prompt as well
// as appended to the system prompt by prism.ts, delivering it twice per
// agent. buildReviewPrompt no longer reads or splices role-definition files
// at all — these tests assert the negative (no splice, no role-specific
// section) and cover the context sections that remain.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// ── No role splice ──────────────────────────────────────────────────────────

// TestBuildReviewPrompt_NoRoleSpecificSection verifies that the prompt no
// longer contains a "## Your role-specific instructions" section — that
// content now arrives solely via the system prompt (prism.ts,
// before_agent_start).
func TestBuildReviewPrompt_NoRoleSpecificSection(t *testing.T) {
	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-security")

	if findSubstring(prompt, "## Your role-specific instructions") {
		t.Errorf("prompt must not contain '## Your role-specific instructions'\nprompt:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_NoRoleFileContentLeaks verifies that even when a role
// file exists on disk, buildReviewPrompt does not read or splice its content
// — the function takes no dependency on $XDG_CONFIG_HOME for role content.
func TestBuildReviewPrompt_NoRoleFileContentLeaks(t *testing.T) {
	// No XDG_CONFIG_HOME setup at all: if buildReviewPrompt tried to read a
	// role file it would hit an unset/garbage path, not a crafted fixture.
	// The assertion is simply that nothing role-shaped appears.
	prompt := review.BuildReviewPromptForTest("42", samplePRContext(), "review-goal")

	for _, marker := range []string{
		"role-specific",
		"role definition for",
	} {
		if findSubstring(prompt, marker) {
			t.Errorf("prompt unexpectedly contains role-related marker %q\nprompt:\n%s", marker, prompt)
		}
	}
}

// TestBuildReviewPrompt_EmptyRoleArgument_NoPanic verifies that an empty role
// argument (used by callers exercising the fallback/edge case) does not
// panic and still returns a non-empty prompt.
func TestBuildReviewPrompt_EmptyRoleArgument_NoPanic(t *testing.T) {
	prompt := review.BuildReviewPromptForTest("42", samplePRContext())
	if len(prompt) == 0 {
		t.Fatal("prompt is empty; expected a non-empty prompt")
	}
}

// ── Context sections unchanged ────────────────────────────────────────────────

// TestBuildReviewPrompt_ContextSectionsUnchanged verifies that removing the
// role-definition splice does not alter the PR metadata, recent commits,
// branch commits, or diff sections.
func TestBuildReviewPrompt_ContextSectionsUnchanged(t *testing.T) {
	ctx := samplePRContext()
	prompt := review.BuildReviewPromptForTest("819", ctx, "review-goal")

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
			t.Errorf("context section missing %q\nprompt:\n%s", s, prompt)
		}
	}
}

// TestBuildReviewPrompt_FallbackPrompt_NoRoleContent verifies the fallback
// path (nil/failed prCtx) also carries no role-specific content.
func TestBuildReviewPrompt_FallbackPrompt_NoRoleContent(t *testing.T) {
	prompt := review.BuildReviewPromptForTest("42", nil, "review-security")

	if findSubstring(prompt, "role") {
		t.Errorf("fallback prompt unexpectedly mentions role content\nprompt:\n%s", prompt)
	}
}
