package review

// context.go — FetchPRContextWithOpts implementation and its supporting helpers.
//
// This file contains the GitHub CLI integration layer: the functions that fetch
// PR metadata, git log, diffs, and linked issues before any review agents are
// spawned. All functions here are called at most once per review run, and have
// no dependency on the spawn or poll machinery.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/forge"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/gitlab"
	"github.com/prismatic-koi/prism/internal/proglog"
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

// gitWorktreeTimeout is the per-call timeout for runGitInWorktree.
// Local git operations are filesystem-bound and should complete well within
// 10 seconds; a longer hang likely indicates a stalled index-pack or network
// remote, both of which we want to surface rather than block indefinitely.
const gitWorktreeTimeout = 10 * time.Second

// runGitInWorktree runs a git command in the given worktree directory and returns
// its stdout. Returns an empty string on error (git log failures are non-fatal).
// Uses a 10-second timeout (context.DeadlineExceeded on expiry) to prevent a
// stalled git subprocess from blocking the entire review spawn.
func runGitInWorktree(worktree string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitWorktreeTimeout)
	defer cancel()
	repo, err := git.OpenDir(worktree)
	if err != nil {
		return ""
	}
	cmd := repo.Command(ctx, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ""
		}
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

// ghTimeout is the per-call timeout for runGH. Matches the mergequeue.execGH
// convention (30 s) so both gh-calling subsystems enforce the same policy.
const ghTimeout = 30 * time.Second

// runGH executes a gh command and returns its stdout as a string.
// Returns an error if gh is not found or exits with a non-zero status.
// Uses a 30-second timeout (matches mergequeue.execGH convention) so a hanging
// gh subprocess (network partition, GitHub 5xx, ssh auth prompt) does not
// block the entire review spawn indefinitely.
//
// Env handling: the child process's env is built explicitly rather than
// inherited implicitly. If the inherited GITHUB_TOKEN is a shell
// command-substitution literal (starts with `$(`), it is stripped so that
// gh does not send the literal `$(cat …)` string to GitHub and 401. This is
// defence in depth on top of the sidecar's SanitizeGitHubTokenEnv call at
// startup: that call covers the boot-restore path, and this covers the case
// of `prism review` invoked directly from a systemd-launched shell context
// (for example a coordinator running review on the host without going
// through the sidecar's /review handler).
func runGH(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	// gh finds the repo with git in the cwd. On the /review path the cwd is
	// the worktree of the session, so git.EnvForDir pins the git dir.
	env, err := git.EnvForDir("", sanitisedGHEnv(os.Environ()))
	if err != nil {
		return "", fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("gh %s timed out after %s: %w", strings.Join(args, " "), ghTimeout, context.DeadlineExceeded)
		}
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}

// sanitisedGHEnv returns env with any GITHUB_TOKEN / PRISM_GITHUB_TOKEN_*
// entries whose value is a shell command-substitution literal (`$(…)`)
// removed. This is the runGH-side defence against the failure mode where
// the tmux server was launched from a non-shell context and the token env
// vars propagated verbatim through the process tree. Every other non-token
// entry is passed through untouched.
func sanitisedGHEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if (k == "GITHUB_TOKEN" || strings.HasPrefix(k, "PRISM_GITHUB_TOKEN_")) &&
			container.IsShellExpansionLiteral(v) {
			// Drop the entry entirely rather than passing `$(cat …)` to gh.
			// gh will then fall back to its own keyring / config-file token
			// resolution and emit a clear "unauthenticated" error if none
			// is available, rather than 401'ing on the literal string.
			continue
		}
		out = append(out, kv)
	}
	return out
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

	// GitLab remote: fetch metadata and diff via glab, parsing GitLab JSON
	// only. The GitHub path below is left byte-for-byte unchanged.
	if opts.Forge == forge.GitLab {
		return fetchGitLabPRContext(opts, prCtx, maxBytes, maxLines, inlineMaxBytes, inlineMaxLines)
	}

	// Fetch PR metadata.
	viewOut, err := runGH("pr", "view", opts.PRNumber, "--json",
		"number,title,body,headRefName,headRefOid,baseRefName,baseRefOid,additions,deletions,changedFiles")
	if err != nil {
		proglog.Warnf("[prism review] warning: could not fetch PR metadata via gh: %v — agents will fall back to git-based discovery\n", err)
		prCtx.FetchFailed = true
		return prCtx
	}

	var meta prViewJSON
	if jsonErr := json.Unmarshal([]byte(viewOut), &meta); jsonErr != nil {
		proglog.Warnf("[prism review] warning: could not parse PR metadata JSON: %v — agents will fall back to git-based discovery\n", jsonErr)
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
		proglog.Warnf("[prism review] warning: could not fetch PR diff via gh: %v — agents will use git diff instead\n", diffErr)
		// Leave Diff and DiffFilePath empty; the prompt will note diff unavailability.
	} else {
		prCtx.DiffBytes = len(diffOut)
		prCtx.DiffLines = strings.Count(diffOut, "\n")

		// Truncate to hard limits first.
		truncated, wasTruncated := truncateDiff(diffOut, maxBytes, maxLines)
		prCtx.DiffTruncated = wasTruncated

		// Decide inline vs file based on ORIGINAL (pre-truncation) size.
		placeDiff(&prCtx, opts.StateDir, opts.PRNumber, opts.Round, diffOut, truncated, inlineMaxBytes, inlineMaxLines)
	}

	return prCtx
}

// fetchGitLabPRContext is the GitLab-forge counterpart to the GitHub read
// path in FetchPRContextWithOpts. It fetches merge-request metadata and diff
// via glab (GitLab JSON only), then applies the identical truncation and
// inline-vs-file placement policy used for GitHub. A glab failure is
// non-fatal in the same way a gh failure is: it sets FetchFailed and the
// agents fall back to git-based discovery.
//
// opts.Repo is the origin remote URL forwarded to glab as -R; when empty glab
// auto-detects the repository from the worktree.
func fetchGitLabPRContext(opts FetchPRContextOpts, prCtx PRContext, maxBytes, maxLines, inlineMaxBytes, inlineMaxLines int) PRContext {
	mr, err := gitlab.ViewMR(opts.Repo, opts.PRNumber)
	if err != nil {
		proglog.Warnf("[prism review] warning: could not fetch MR metadata via glab: %v — agents will fall back to git-based discovery\n", err)
		prCtx.FetchFailed = true
		return prCtx
	}

	prCtx.Title = mr.Title
	prCtx.Body = mr.Description
	prCtx.HeadRefName = mr.SourceBranch
	prCtx.HeadRefOid = mr.SHA
	prCtx.BaseRefName = mr.TargetBranch
	// GitLab's mr view JSON does not carry additions/deletions/changedFiles
	// in the shape prism parses; they stay zero. The diff itself and the
	// git-log commit range below give agents the substance they need.

	// Gather git log — non-fatal; missing log output is noted in the prompt.
	prCtx.RecentCommits = runGitInWorktree(opts.Worktree, "log", "--oneline", "-20")
	if mr.TargetBranch != "" {
		prCtx.BranchCommits = runGitInWorktree(opts.Worktree, "log", "origin/"+mr.TargetBranch+"..HEAD")
	}

	// Fetch linked issues — non-fatal; unfetchable issues get a clear marker.
	issueNumbers := parseLinkedIssues(mr.Description)
	if len(issueNumbers) > 0 {
		prCtx.LinkedIssues = make(map[string]string, len(issueNumbers))
		for _, num := range issueNumbers {
			issueText, issueErr := gitlab.ViewIssue(opts.Repo, num)
			if issueErr != nil {
				prCtx.LinkedIssues[num] = fmt.Sprintf("[issue #%s could not be fetched: %v]", num, issueErr)
			} else {
				prCtx.LinkedIssues[num] = issueText
			}
		}
	}

	// Fetch diff via glab.
	diffOut, diffErr := gitlab.DiffMR(opts.Repo, opts.PRNumber)
	if diffErr != nil {
		proglog.Warnf("[prism review] warning: could not fetch MR diff via glab: %v — agents will use git diff instead\n", diffErr)
		return prCtx
	}
	prCtx.DiffBytes = len(diffOut)
	prCtx.DiffLines = strings.Count(diffOut, "\n")

	truncated, wasTruncated := truncateDiff(diffOut, maxBytes, maxLines)
	prCtx.DiffTruncated = wasTruncated

	placeDiff(&prCtx, opts.StateDir, opts.PRNumber, opts.Round, diffOut, truncated, inlineMaxBytes, inlineMaxLines)
	return prCtx
}
