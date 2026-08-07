package review

// pr_existence.go — PR-existence/state fast-fail check for `prism review`.
//
// Issue #2040: `prism review <N>` used to spawn 5 review agents without first
// verifying that PR <N> exists and is OPEN. When <N> did not resolve to an
// open PR (never created, closed, merged, or guessed by a worker), the command
// still registered a review group and launched 5 agents against a non-existent
// target. Each agent independently discovered the PR did not exist, wasted a
// full cycle, and left the group lingering in `in-progress` — manufacturing
// the stale-group symptom that then blocks subsequent legitimate reviews.
//
// This check runs as the FIRST pre-flight step in `prism review`, BEFORE the
// rebase gate (preflight.go). The ordering matters: it is cheaper and more
// fundamental — no point running `git fetch` against a PR that does not exist.
//
// Like the rebase gate, a fast-fail here MUST NOT register a review group and
// MUST NOT increment the review-cycle counter. Both invariants follow
// structurally from the fact that this function runs before RunAsync (which
// is the sole writer of per-agent session rows used by NextRoundNumber).
//
// Three terminal cases (all exit non-zero, no side effects):
//
//   - PRStateMissing  — gh reports "Could not resolve to a PullRequest"
//   - PRStateClosed   — gh returns {"state":"CLOSED"} with mergedAt null
//   - PRStateMerged   — gh returns {"state":"MERGED"} or mergedAt non-null
//
// One transient case (exit non-zero, no side effects, distinct message):
//
//   - any other gh error (network, rate-limit, auth) is surfaced as
//     "could not determine PR state: <err>" and DOES NOT silently proceed to
//     spawn agents against an unverified target. This is the [edge-case] AC.
//
// One pass-through case:
//
//   - PR exists and state == "OPEN" → returns nil; caller proceeds to the
//     rebase gate.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/forge"
	"github.com/prismatic-koi/prism/internal/gitlab"
)

// PRStateError is returned by CheckPRState when the PR-existence/state gate
// refuses the review. It carries the kind of refusal so callers can
// distinguish the four terminal cases (missing / closed / merged / transient)
// from a passing OPEN PR. All four kinds map to a non-zero exit with a
// ready-to-display Msg.
type PRStateError struct {
	// Msg is the user-facing error message, ready to display on stderr.
	Msg string
	// Kind identifies the refusal category. Set for missing/closed/merged/
	// transient; "" only for the (unused) zero value.
	Kind PRStateErrorKind
}

// PRStateErrorKind enumerates the categories of PR-state refusal.
type PRStateErrorKind string

const (
	// PRStateMissing — gh reported "Could not resolve to a PullRequest with
	// the number of <N>", i.e. the PR does not exist on the remote at all.
	PRStateMissing PRStateErrorKind = "missing"
	// PRStateClosed — PR exists but is in state "CLOSED" with no merge.
	PRStateClosed PRStateErrorKind = "closed"
	// PRStateMerged — PR exists and is in state "MERGED" (or mergedAt non-null).
	PRStateMerged PRStateErrorKind = "merged"
	// PRStateTransient — gh failed for any other reason (network, rate-limit,
	// auth) and we could NOT determine the PR's state. Distinct from
	// PRStateMissing because the right operator action is different
	// (retry / check connectivity, not "the PR doesn't exist").
	PRStateTransient PRStateErrorKind = "transient"
)

func (e *PRStateError) Error() string { return e.Msg }

// PRStateRunner is the test seam for invoking gh. Production code uses
// realGHForPRState (which calls runGH from context.go). Tests inject a
// scripted runner that returns canned stdout/stderr/error responses.
type PRStateRunner interface {
	// run executes `gh pr view <prNumber> --json state,mergedAt` and returns
	// its stdout, the trimmed stderr, and any underlying error. When the gh
	// process exits non-zero, err is non-nil and stderr should contain gh's
	// diagnostic message. When gh exits zero, err is nil and stdout contains
	// the JSON body.
	run(prNumber string) (stdout string, stderr string, err error)
}

// realGHForPRState shells out via the existing runGH helper in context.go.
// It uses the same 30 s timeout and process plumbing as the rest of the
// review package's gh calls (no second gh client introduced).
type realGHForPRState struct{}

func (realGHForPRState) run(prNumber string) (string, string, error) {
	// runGH wraps stderr into the returned error on non-zero exit, so we
	// re-derive (stdout, stderr, err) by stringifying the error. This keeps
	// the runGH contract intact (no second exec.Command path) while giving
	// CheckPRState the structured signal it needs to distinguish "PR not
	// found" from transient gh errors.
	stdout, err := runGH("pr", "view", prNumber, "--json", "state,mergedAt")
	if err == nil {
		return stdout, "", nil
	}
	// runGH's error format is "exit status N: <stderr>" or "<wrapper>: <stderr>".
	// We treat the entire error string as the diagnostic blob; the
	// "Could not resolve to a PullRequest" substring is what we match on.
	return stdout, err.Error(), err
}

// prStateJSON is the minimal JSON shape we need from `gh pr view --json state,mergedAt`.
// Defined locally rather than reusing prViewJSON from context.go because we
// only need two fields and want a tight contract for this gate.
type prStateJSON struct {
	State    string  `json:"state"`
	MergedAt *string `json:"mergedAt"`
}

// CheckPRState resolves PR <prNumber> via `gh pr view` and refuses the review
// when the PR does not exist, is CLOSED, or is MERGED. It returns nil when the
// PR is OPEN — the caller then proceeds to the rebase gate (Preflight).
//
// On any non-OPEN outcome it returns a *PRStateError whose Msg is
// ready-to-display and whose Kind identifies the terminal case. The caller
// should print Msg to stderr and exit non-zero. CheckPRState does NOT spawn
// any review agents and does NOT touch the prism DB, so a CheckPRState
// failure cannot register a group or move the review-cycle counter (the
// counter is derived from per-agent session rows written by RunAsync).
//
// Transient gh failures (network, rate-limit, auth, etc.) are surfaced as
// PRStateTransient with a distinct "could not determine PR state" message —
// the function NEVER silently passes a transient failure through to agent
// spawn. If we cannot determine the PR's state, we refuse.
// On a GitHub remote (fg == forge.GitHub) the state is resolved via
// `gh pr view --json state,mergedAt`. On a gitlab.com remote
// (fg == forge.GitLab) it is resolved from the merge request's state via glab;
// repo carries the origin remote URL (or "" to let glab auto-detect from the
// worktree). The GitLab mapping mirrors the GitHub one: an open MR passes, a
// merged or closed/locked MR refuses with a state-specific message, a missing
// MR is PRStateMissing, and any other glab failure is PRStateTransient.
func CheckPRState(prNumber string, fg forge.Forge, repo string) error {
	if fg == forge.GitLab {
		iid := strings.TrimSpace(prNumber)
		if iid == "" {
			return &PRStateError{
				Kind: PRStateTransient,
				Msg:  "prism review: MR IID is required",
			}
		}
		mr, err := gitlab.ViewMR(repo, iid)
		return gitLabMRStateToError(iid, mr, err)
	}
	return checkPRStateWithRunner(prNumber, realGHForPRState{})
}

// gitLabMRStateToError maps a gitlab.ViewMR result to the same *PRStateError
// contract CheckPRState uses for GitHub. It is a pure function so the
// state-to-refusal mapping is unit tested without a live glab. The IID is
// echoed into the user-facing messages so they read the same as the GitHub
// ones ("MR !<iid>" rather than "PR #<n>" — GitLab addresses MRs with a bang).
func gitLabMRStateToError(iid string, mr *gitlab.MR, err error) error {
	if err != nil {
		if errors.Is(err, gitlab.ErrMRNotFound) {
			return &PRStateError{
				Kind: PRStateMissing,
				Msg:  fmt.Sprintf("prism review: MR !%s does not exist", iid),
			}
		}
		return &PRStateError{
			Kind: PRStateTransient,
			Msg: fmt.Sprintf("prism review: could not determine MR state: %s",
				truncateDiag(err.Error(), 2000)),
		}
	}
	if mr == nil {
		return &PRStateError{
			Kind: PRStateTransient,
			Msg:  "prism review: could not determine MR state: empty glab response",
		}
	}
	// merged_at set, or state "merged", both mean merged. Check first so a
	// merged MR is never reported as merely closed.
	merged := mr.MergedAt != nil && *mr.MergedAt != ""
	if mr.State == "merged" || merged {
		return &PRStateError{
			Kind: PRStateMerged,
			Msg:  fmt.Sprintf("prism review: MR !%s is merged — nothing to review", iid),
		}
	}
	if mr.State == "closed" || mr.State == "locked" {
		return &PRStateError{
			Kind: PRStateClosed,
			Msg:  fmt.Sprintf("prism review: MR !%s is closed (merged: false) — nothing to review", iid),
		}
	}
	if mr.State == "opened" {
		return nil
	}
	return &PRStateError{
		Kind: PRStateTransient,
		Msg: fmt.Sprintf("prism review: MR !%s has unrecognised state %q — refusing to spawn agents",
			iid, mr.State),
	}
}

// checkPRStateWithRunner is the test entry point. CheckPRState wires in the
// real gh runner; tests inject a scripted PRStateRunner.
func checkPRStateWithRunner(prNumber string, runner PRStateRunner) error {
	if strings.TrimSpace(prNumber) == "" {
		return &PRStateError{
			Kind: PRStateTransient,
			Msg:  "prism review: PR number is required",
		}
	}

	stdout, stderr, err := runner.run(prNumber)
	if err != nil {
		// Distinguish "Could not resolve to a PullRequest" (PR does not
		// exist) from any other gh error (transient).
		blob := stderr
		if blob == "" {
			blob = err.Error()
		}
		if isPRNotFoundDiagnostic(blob, prNumber) {
			return &PRStateError{
				Kind: PRStateMissing,
				Msg:  fmt.Sprintf("prism review: PR #%s does not exist", prNumber),
			}
		}
		// Transient — DO NOT silently proceed. Surface distinctly.
		return &PRStateError{
			Kind: PRStateTransient,
			Msg: fmt.Sprintf("prism review: could not determine PR state: %s",
				truncateDiag(blob, 2000)),
		}
	}

	info, parseErr := parsePRStateJSON(stdout)
	if parseErr != nil {
		// Treat unparseable output as transient — same contract as a gh
		// error: we did not get a clean state signal, so we refuse rather
		// than silently passing through.
		return &PRStateError{
			Kind: PRStateTransient,
			Msg: fmt.Sprintf("prism review: could not determine PR state: parse gh pr view output: %v",
				parseErr),
		}
	}

	// MERGED takes precedence over CLOSED because a merged PR's state can be
	// reported as either "MERGED" (modern gh) or "CLOSED" with a non-null
	// mergedAt (defence in depth — mirrors the prInfo.isMerged() convention
	// in internal/mergequeue/watcher.go).
	merged := info.MergedAt != nil && *info.MergedAt != ""
	if info.State == "MERGED" || merged {
		return &PRStateError{
			Kind: PRStateMerged,
			Msg:  fmt.Sprintf("prism review: PR #%s is merged — nothing to review", prNumber),
		}
	}
	if info.State == "CLOSED" {
		return &PRStateError{
			Kind: PRStateClosed,
			Msg:  fmt.Sprintf("prism review: PR #%s is closed (merged: false) — nothing to review", prNumber),
		}
	}
	if info.State == "OPEN" {
		return nil
	}
	// Unknown state — refuse rather than guess. This protects against future
	// gh schema additions that introduce a new state value.
	return &PRStateError{
		Kind: PRStateTransient,
		Msg: fmt.Sprintf("prism review: PR #%s has unrecognised state %q — refusing to spawn agents",
			prNumber, info.State),
	}
}

// isPRNotFoundDiagnostic returns true when the gh stderr/error blob
// unambiguously indicates the PR does not exist. We match on the substring
// "Could not resolve to a PullRequest" which is the GraphQL error message gh
// emits for a non-existent PR. Including the PR number in the match would be
// stricter but would break if gh ever changes the diagnostic format —
// substring on the stable prefix is the right balance.
func isPRNotFoundDiagnostic(blob, _ string) bool {
	return strings.Contains(blob, "Could not resolve to a PullRequest")
}

// parsePRStateJSON parses the JSON body returned by
// `gh pr view <pr> --json state,mergedAt`. Returns an error on malformed JSON.
func parsePRStateJSON(stdout string) (*prStateJSON, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, errors.New("empty gh pr view output")
	}
	var info prStateJSON
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// truncateDiag clips a diagnostic blob to maxLen runes, appending an ellipsis
// marker when clipped. Used so that a very long gh error (e.g. a multi-page
// auth-failure dump) does not flood the worker's stderr.
func truncateDiag(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n… (truncated)"
}
