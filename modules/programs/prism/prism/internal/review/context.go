package review

// context.go — FetchPRContextWithOpts implementation and its supporting helpers.
//
// This file contains the GitHub CLI integration layer: the functions that fetch
// PR metadata, git log, diffs, and linked issues before any review agents are
// spawned. All functions here are called at most once per review run, and have
// no dependency on the spawn or poll machinery.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// prViewJSON is the JSON shape returned by `gh pr view --json ...`.
type prViewJSON struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	HeadRefName  string `json:"headRefName"`
	HeadRefOid   string `json:"headRefOid"`
	BaseRefName  string `json:"baseRefName"`
	BaseRefOid   string `json:"baseRefOid"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
}

// diffFilePath returns the path for the diff temp file for a given PR and round.
// Format: /tmp/prism-review-<pr>-round-<N>.diff
func diffFilePath(prNumber string, round int) string {
	if round <= 0 {
		round = 1
	}
	return fmt.Sprintf("/tmp/prism-review-%s-round-%d.diff", prNumber, round)
}

// parseLinkedIssues extracts issue numbers referenced by "Closes #N", "Refs #N",
// "Fixes #N", or "References #N" in the PR body (case-insensitive).
// Returns a deduplicated, ordered list of issue number strings (e.g. ["123", "456"]).
func parseLinkedIssues(body string) []string {
	// Match patterns like: Closes #123, Refs #456, Fixes #789, References #012
	// Allow for optional comma or whitespace after the number.
	re := linkedIssueRe
	matches := re.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		num := m[1]
		if !seen[num] {
			seen[num] = true
			result = append(result, num)
		}
	}
	return result
}

// runGitInWorktree runs a git command in the given worktree directory and returns
// its stdout. Returns an empty string on error (git log failures are non-fatal).
func runGitInWorktree(worktree string, args ...string) string {
	cmd := exec.Command("git", args...)
	if worktree != "" {
		cmd.Dir = worktree
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return stdout.String()
}

// truncateDiff truncates a diff to at most maxBytes bytes and maxLines lines.
// Returns the (possibly truncated) diff and a bool indicating truncation.
func truncateDiff(diff string, maxBytes, maxLines int) (string, bool) {
	// Check byte limit first.
	if len(diff) > maxBytes {
		// Truncate to maxBytes, then find the last newline to avoid a mid-line cut.
		truncated := diff[:maxBytes]
		if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
			truncated = truncated[:idx]
		}
		return truncated + "\n... [truncated — use git diff origin/<base>...HEAD for full content]", true
	}

	// Check line limit.
	lines := strings.Split(diff, "\n")
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + "\n... [truncated — use git diff origin/<base>...HEAD for full content]", true
	}

	return diff, false
}

// runGH executes a gh command and returns its stdout as a string.
// Returns an error if gh is not found or exits with a non-zero status.
func runGH(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}

// FetchPRContextWithOpts fetches PR metadata, git log, diff, and linked issues.
// It is the full implementation; FetchPRContext is a thin wrapper for callers
// that only need the legacy (maxBytes, maxLines) API.
func FetchPRContextWithOpts(opts FetchPRContextOpts) PRContext {
	maxBytes := opts.MaxBytes
	maxLines := opts.MaxLines
	inlineMaxBytes := opts.InlineMaxBytes
	inlineMaxLines := opts.InlineMaxLines

	if maxBytes <= 0 {
		maxBytes = DiffMaxBytes
	}
	if maxLines <= 0 {
		maxLines = DiffMaxLines
	}
	if inlineMaxBytes <= 0 {
		inlineMaxBytes = DiffInlineMaxBytes
	}
	if inlineMaxLines <= 0 {
		inlineMaxLines = DiffInlineMaxLines
	}

	prCtx := PRContext{PRNumber: opts.PRNumber, Round: opts.Round}

	// Fetch PR metadata.
	viewOut, err := runGH("pr", "view", opts.PRNumber, "--json",
		"number,title,body,headRefName,headRefOid,baseRefName,baseRefOid,additions,deletions,changedFiles")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR metadata via gh: %v — agents will fall back to git-based discovery\n", err)
		prCtx.FetchFailed = true
		return prCtx
	}

	var meta prViewJSON
	if jsonErr := json.Unmarshal([]byte(viewOut), &meta); jsonErr != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not parse PR metadata JSON: %v — agents will fall back to git-based discovery\n", jsonErr)
		prCtx.FetchFailed = true
		return prCtx
	}

	prCtx.Title = meta.Title
	prCtx.Body = meta.Body
	prCtx.HeadRefName = meta.HeadRefName
	prCtx.HeadRefOid = meta.HeadRefOid
	prCtx.BaseRefName = meta.BaseRefName
	prCtx.BaseRefOid = meta.BaseRefOid
	prCtx.Additions = meta.Additions
	prCtx.Deletions = meta.Deletions
	prCtx.ChangedFiles = meta.ChangedFiles

	// Gather git log — non-fatal; missing log output is noted in the prompt.
	prCtx.RecentCommits = runGitInWorktree(opts.Worktree, "log", "--oneline", "-20")
	if meta.BaseRefName != "" {
		prCtx.BranchCommits = runGitInWorktree(opts.Worktree, "log", "origin/"+meta.BaseRefName+"..HEAD")
	}

	// Fetch linked issues — non-fatal; unfetchable issues get a clear marker.
	issueNumbers := parseLinkedIssues(meta.Body)
	if len(issueNumbers) > 0 {
		prCtx.LinkedIssues = make(map[string]string, len(issueNumbers))
		for _, num := range issueNumbers {
			issueText, issueErr := runGH("issue", "view", num)
			if issueErr != nil {
				prCtx.LinkedIssues[num] = fmt.Sprintf("[issue #%s could not be fetched: %v]", num, issueErr)
			} else {
				prCtx.LinkedIssues[num] = issueText
			}
		}
	}

	// Fetch diff.
	diffOut, diffErr := runGH("pr", "diff", opts.PRNumber)
	if diffErr != nil {
		// Diff failure is non-fatal: we have metadata, just no diff content.
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR diff via gh: %v — agents will use git diff instead\n", diffErr)
		// Leave Diff and DiffFilePath empty; the prompt will note diff unavailability.
	} else {
		prCtx.DiffBytes = len(diffOut)
		prCtx.DiffLines = strings.Count(diffOut, "\n")

		// Truncate to hard limits first.
		truncated, wasTruncated := truncateDiff(diffOut, maxBytes, maxLines)
		prCtx.DiffTruncated = wasTruncated

		// Decide inline vs file based on ORIGINAL (pre-truncation) size.
		if len(diffOut) <= inlineMaxBytes && strings.Count(diffOut, "\n") <= inlineMaxLines {
			// Small enough to inline.
			prCtx.Diff = truncated
		} else {
			// Large diff — write to a temp file and point agents at the path.
			diffPath := diffFilePath(opts.PRNumber, opts.Round)
			if writeErr := os.WriteFile(diffPath, []byte(diffOut), 0o644); writeErr != nil {
				fmt.Fprintf(os.Stderr, "[prism review] warning: could not write diff to %s: %v — inlining diff instead\n", diffPath, writeErr)
				prCtx.Diff = truncated
			} else {
				prCtx.DiffFilePath = diffPath
			}
		}
	}

	return prCtx
}
