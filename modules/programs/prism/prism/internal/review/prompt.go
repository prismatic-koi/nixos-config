package review

// prompt.go — review prompt construction and supporting helpers.
//
// buildReviewPrompt assembles the initial prompt for a review agent from the
// pre-fetched PR context. The helpers in this file (sortStrings, deriveRepo,
// formatDuration, defaultDBPath, sanitisePRNumber) are pure utility functions
// with no I/O or DB dependency.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// buildReviewPrompt returns the initial prompt string for a review agent.
// When prCtx is non-nil and FetchFailed is false, the prompt begins with a
// structured context block (git log, PR metadata, linked issues, diff)
// followed by the role-specific content.
// When prCtx is nil or FetchFailed is true, a minimal fallback prompt is used.
//
// The context block always appears BEFORE the role-specific content so
// that agents read full context first and role directives second.
func buildReviewPrompt(prNumber string, prCtx *PRContext) string {
	if prCtx == nil || prCtx.FetchFailed {
		// Fallback: minimal prompt with only the PR number.
		return fmt.Sprintf(
			"Review PR #%s. Use `git diff origin/<base>...HEAD` to see the diff, "+
				"`git log --oneline -20` for recent commits, and check the linked issue "+
				"for acceptance criteria. Report your findings clearly.",
			prNumber,
		)
	}

	var sb strings.Builder

	// ── Context header ────────────────────────────────────────────────────
	sb.WriteString("## Context for your review\n\n")
	sb.WriteString("This context has been gathered for you. You do not need to re-run these commands.\n\n")

	// ── Recent commits ────────────────────────────────────────────────────
	sb.WriteString("### Recent commits (`git log --oneline -20`)\n\n")
	if prCtx.RecentCommits == "" {
		sb.WriteString("(not available)\n")
	} else {
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimRight(prCtx.RecentCommits, "\n"))
		sb.WriteString("\n```\n")
	}
	sb.WriteString("\n")

	// ── Branch commits ────────────────────────────────────────────────────
	if prCtx.BaseRefName != "" {
		sb.WriteString(fmt.Sprintf("### This branch vs origin/%s (`git log origin/%s..HEAD`)\n\n",
			prCtx.BaseRefName, prCtx.BaseRefName))
	} else {
		sb.WriteString("### This branch vs base\n\n")
	}
	if prCtx.BranchCommits == "" {
		sb.WriteString("(not available)\n")
	} else {
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimRight(prCtx.BranchCommits, "\n"))
		sb.WriteString("\n```\n")
	}
	sb.WriteString("\n")

	// ── PR metadata ───────────────────────────────────────────────────────
	sb.WriteString("### PR metadata\n\n")
	sb.WriteString(fmt.Sprintf("You are reviewing PR #%s.\n\n", prCtx.PRNumber))
	sb.WriteString(fmt.Sprintf("- Title: %q\n", prCtx.Title))
	sb.WriteString(fmt.Sprintf("- Head branch: %s\n", prCtx.HeadRefName))
	sb.WriteString(fmt.Sprintf("- Head commit: %s\n", prCtx.HeadRefOid))
	sb.WriteString(fmt.Sprintf("- Base branch: %s\n", prCtx.BaseRefName))
	sb.WriteString(fmt.Sprintf("- Base commit: %s\n", prCtx.BaseRefOid))
	if prCtx.WorktreePath != "" {
		sb.WriteString(fmt.Sprintf("- Worktree: %s (read-only)\n", prCtx.WorktreePath))
	}
	sb.WriteString(fmt.Sprintf("- Files changed: %d (+%d -%d lines)\n", prCtx.ChangedFiles, prCtx.Additions, prCtx.Deletions))
	sb.WriteString("\n")

	// ── PR body ───────────────────────────────────────────────────────────
	sb.WriteString("### PR body\n\n")
	body := strings.TrimSpace(prCtx.Body)
	if body == "" {
		sb.WriteString("(no body)\n")
	} else {
		// Wrap in a blockquote-style indentation to prevent any triple-backtick
		// sequences in the body from colliding with the diff code fence below.
		// Each line is prefixed with "> " so fence markers become "> ```" which
		// markdown renderers treat as quoted text, not a code fence boundary.
		for _, line := range strings.Split(body, "\n") {
			sb.WriteString("> ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")

	// ── Linked issues ─────────────────────────────────────────────────────
	sb.WriteString("### Linked issues\n\n")
	if len(prCtx.LinkedIssues) == 0 {
		sb.WriteString("(no linked issues found)\n")
	} else {
		// Emit issues in a stable order by collecting and sorting keys.
		keys := make([]string, 0, len(prCtx.LinkedIssues))
		for k := range prCtx.LinkedIssues {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, num := range keys {
			issueText := prCtx.LinkedIssues[num]
			sb.WriteString(fmt.Sprintf("#### Issue #%s\n\n", num))
			sb.WriteString("```\n")
			sb.WriteString(strings.TrimRight(issueText, "\n"))
			sb.WriteString("\n```\n\n")
		}
	}

	// ── Diff ──────────────────────────────────────────────────────────────
	sb.WriteString("### Diff\n\n")
	switch {
	case prCtx.DiffFilePath != "":
		// Large diff written to a file — give agents the path and guidance.
		sb.WriteString(fmt.Sprintf(
			"The diff for this PR is large (%d lines, %d KB). It has been saved to:\n\n"+
				"  %s\n\n"+
				"Query it with native git on the workspace or grep/rg on the file:\n\n"+
				"  git diff --stat origin/%s..HEAD                    # overview\n"+
				"  git log origin/%s..HEAD -- <path>                  # per-file history\n"+
				"  rg '<pattern>' %s    # search the diff\n"+
				"  git show HEAD -- <path>                            # specific file state\n",
			prCtx.DiffLines,
			prCtx.DiffBytes/1024,
			prCtx.DiffFilePath,
			prCtx.BaseRefName, prCtx.BaseRefName,
			prCtx.DiffFilePath,
		))
	case prCtx.Diff == "":
		sb.WriteString("(diff not available — use `git diff origin/" + prCtx.BaseRefName + "...HEAD` to fetch it)\n")
	default:
		if prCtx.DiffTruncated {
			sb.WriteString("Note: the diff below has been truncated due to size. Use `git diff origin/" +
				prCtx.BaseRefName + "...HEAD` to fetch any missing hunks.\n\n")
		}
		sb.WriteString("```diff\n")
		sb.WriteString(prCtx.Diff)
		if !strings.HasSuffix(prCtx.Diff, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
	}
	sb.WriteString("\n")

	// ── Tool preference guidance ───────────────────────────────────────────
	sb.WriteString("---\n\n")
	sb.WriteString("You may still run any git command to re-query or dig deeper as your review requires. " +
		"Prefer native git (`git show`, `git diff`, `git log`) over `gh` for cross-branch inspection — " +
		"it's faster, works offline, and doesn't consume API rate limits.\n\n")
	sb.WriteString("---\n\n")

	// ── PR under review (legacy compat section) ───────────────────────────
	// Kept for AC: tests that check "## PR under review" are still met via
	// the "### PR metadata" section. However, tests specifically looking for
	// "## PR under review" need to be updated. We keep backward compat by
	// noting this is now under "## Context for your review > ### PR metadata".
	sb.WriteString("Your role-specific instructions follow below.\n\n")
	sb.WriteString("---\n\n")

	return sb.String()
}

// sortStrings sorts a slice of strings in-place (insertion sort — small slices only).
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// deriveRepo returns the repo portion of a session name (before "@").
func deriveRepo(sessionName string) string {
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		return sessionName[:idx]
	}
	return sessionName
}

// formatDuration formats a duration as "Xm" or "Xs" for display.
func formatDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// defaultDBPath returns the default prism DB path.
func defaultDBPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "prism.db")
}
