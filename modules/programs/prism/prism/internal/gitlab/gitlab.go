// Package gitlab wraps the `glab` CLI for the read-only metadata prism needs
// from a gitlab.com merge request: the target (base) branch, the source
// branch, the MR state, and the diff.
//
// This is deliberately a thin, single-forge helper. GitHub
// stays prism's primary forge and keeps its own `gh`-based read path; this
// package exists so GitLab JSON is parsed by GitLab code only, never mixed
// with the GitHub `gh pr view` shapes in internal/review. It is read-only:
// no function here writes to an MR (no comment, no approval, no merge).
package gitlab

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
)

// glabTimeout is the per-call timeout for a glab invocation. Matches the
// 30-second convention used by internal/review.runGH and
// internal/mergequeue.execGH so every forge-CLI subsystem enforces the same
// policy — a hanging glab subprocess (network partition, gitlab.com 5xx, ssh
// auth prompt) does not block the caller indefinitely.
const glabTimeout = 30 * time.Second

// ErrMRNotFound is returned by ViewMR when glab reports the merge request does
// not exist (HTTP 404). Callers distinguish this from a transient failure to
// mirror the GitHub PRStateMissing vs PRStateTransient split in
// internal/review.
var ErrMRNotFound = errors.New("gitlab: merge request not found")

// MR is the subset of the GitLab merge-request JSON that prism consumes. The
// field set is intentionally minimal — only what the review read path and the
// `prism pr` fetch need. It is the GitLab-shaped counterpart to the
// GitHub-shaped prViewJSON in internal/review/context.go.
type MR struct {
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	// State is one of GitLab's MR states: "opened", "closed", "merged",
	// "locked".
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	// SHA is the MR head commit SHA (diff_refs.head_sha equivalent that glab
	// surfaces as the top-level "sha").
	SHA string `json:"sha"`
}

// Runner is the test seam for invoking glab. Production code uses the default
// exec runner; tests inject a scripted runner that returns canned
// stdout/stderr/err triples so no live glab or gitlab.com access is needed.
type Runner interface {
	// Run executes `glab <args...>` and returns its stdout, its stderr, and
	// any underlying error (non-nil on non-zero exit).
	Run(args ...string) (stdout string, stderr string, err error)
}

// execRunner is the production Runner: it shells out to glab with a bounded
// timeout and the inherited environment (GITLAB_TOKEN, PATH, ssh config).
type execRunner struct{}

func (execRunner) Run(args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), glabTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "glab", args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.String(), stderr.String(),
			fmt.Errorf("glab %s timed out after %s: %w", strings.Join(args, " "), glabTimeout, context.DeadlineExceeded)
	}
	return stdout.String(), stderr.String(), err
}

// DefaultRunner is the production Runner used by the package-level helpers.
var DefaultRunner Runner = execRunner{}

// repoArgs prepends `-R <repo>` when repo is non-empty. glab accepts an
// OWNER/REPO slug, a full https URL, or a git URL for -R. When repo is empty
// glab auto-detects the repository from the current directory's git remote —
// which is correct when the caller runs inside the target worktree.
func repoArgs(repo string, rest ...string) []string {
	if strings.TrimSpace(repo) == "" {
		return rest
	}
	return append([]string{"-R", repo}, rest...)
}

// ViewMR fetches the merge request <iid> in repo (an OWNER/REPO slug, full
// URL, git URL, or "" to auto-detect) via `glab mr view <iid> -F json`.
//
// Returns ErrMRNotFound when glab reports a 404, a wrapped error for any other
// glab failure (network, auth, timeout), and the parsed MR on success.
func ViewMR(repo, iid string) (*MR, error) {
	return ViewMRWith(DefaultRunner, repo, iid)
}

// ViewMRWith is the test entry point for ViewMR: callers inject a scripted
// Runner. ViewMR wires in DefaultRunner.
func ViewMRWith(r Runner, repo, iid string) (*MR, error) {
	stdout, stderr, err := r.Run(repoArgs(repo, "mr", "view", iid, "-F", "json")...)
	if err != nil {
		// glab prints a JSON error body to stdout and a human message to
		// stderr on a 404; both carry "404". Match on the stable substring.
		blob := stdout + "\n" + stderr
		if is404(blob) {
			return nil, ErrMRNotFound
		}
		return nil, fmt.Errorf("gitlab: glab mr view %s: %w: %s", iid, err, strings.TrimSpace(stderr))
	}
	var mr MR
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &mr); jsonErr != nil {
		return nil, fmt.Errorf("gitlab: parse glab mr view output: %w", jsonErr)
	}
	return &mr, nil
}

// DiffMR returns the unified diff for merge request <iid> via
// `glab mr diff <iid>`. Returns the raw diff text on success.
func DiffMR(repo, iid string) (string, error) {
	return DiffMRWith(DefaultRunner, repo, iid)
}

// DiffMRWith is the test entry point for DiffMR.
func DiffMRWith(r Runner, repo, iid string) (string, error) {
	stdout, stderr, err := r.Run(repoArgs(repo, "mr", "diff", iid, "--color", "never")...)
	if err != nil {
		return "", fmt.Errorf("gitlab: glab mr diff %s: %w: %s", iid, err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// ViewIssue returns the text rendering of GitLab issue <n> via
// `glab issue view <n>`. Used for the linked-issue context in a review; a
// failure is surfaced to the caller, which records a "could not be fetched"
// marker rather than failing the whole review.
func ViewIssue(repo, n string) (string, error) {
	return ViewIssueWith(DefaultRunner, repo, n)
}

// ViewIssueWith is the test entry point for ViewIssue.
func ViewIssueWith(r Runner, repo, n string) (string, error) {
	stdout, stderr, err := r.Run(repoArgs(repo, "issue", "view", n)...)
	if err != nil {
		return "", fmt.Errorf("gitlab: glab issue view %s: %w: %s", n, err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// is404 reports whether a glab error blob indicates a 404 Not Found. glab
// emits "404 Not Found" in both its JSON error body and its human stderr
// message, so a substring match on the stable status is the right balance
// (matching on the exact IID would break if glab changes its message format).
func is404(blob string) bool {
	return strings.Contains(blob, "404")
}
