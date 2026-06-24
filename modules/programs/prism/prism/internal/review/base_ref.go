package review

// base_ref.go — resolve a PR's base branch name via `gh pr view`.
//
// Issue #2304: the pre-flight rebase gate (preflight.go) refuses the review
// when HEAD is not a descendant of `origin/main`, hardcoding "main". This is
// correct for PRs targeting main but wrong for PRs targeting any other base
// branch — long-lived integration branches, release branches, environment
// branches, etc. — and `--rebase` against the wrong base is a silent footgun.
//
// The fix is to discover the PR's actual base ref before invoking Preflight,
// and pass it through PreflightOpts.Branch. This file provides the discovery
// helper. The discovery is best-effort: any failure (gh missing, network
// error, unauthenticated, PR not found, empty baseRefName) returns "" and the
// caller falls back silently to the existing "main" default. The fallback
// preserves today's behaviour for invocations not tied to a discoverable PR
// and never surfaces a warning that would scare the operator.

import (
	"encoding/json"
	"strings"
)

// baseRefRunner is the test seam for invoking `gh pr view --json baseRefName`.
// Production code uses realGHForBaseRef (which shells out via the runGH helper
// in context.go); tests inject a scripted runner that returns canned values.
type baseRefRunner interface {
	// run executes `gh pr view <prNumber> --json baseRefName` and returns
	// its stdout body and any underlying error. On non-zero gh exit, err is
	// non-nil and the stdout body should be ignored.
	run(prNumber string) (stdout string, err error)
}

// realGHForBaseRef shells out via the existing runGH helper in context.go.
// It uses the same 30-second timeout and process plumbing as the rest of the
// review package's gh calls (no second gh client introduced).
type realGHForBaseRef struct{}

func (realGHForBaseRef) run(prNumber string) (string, error) {
	return runGH("pr", "view", prNumber, "--json", "baseRefName")
}

// baseRefJSON is the minimal JSON shape we need from
// `gh pr view <pr> --json baseRefName`. Defined locally rather than reusing
// prViewJSON from context.go because we only need one field and want a tight
// contract for this helper.
type baseRefJSON struct {
	BaseRefName string `json:"baseRefName"`
}

// ResolvePRBaseRef returns the PR's base branch name (e.g. "main",
// "eks-pipeline"), or "" on any failure. The caller treats "" as
// "fall back to the default base" — typically "main". This is the public
// entry point used by cmd/review.go.
//
// Resolution is best-effort and silent: a missing gh binary, network failure,
// unauthenticated session, non-existent PR, or an empty baseRefName all
// collapse to "" without surfacing a warning. Surfacing a scary warning on
// every invocation that isn't tied to a discoverable PR would be noise; the
// rebase-gate guarantees its own clear messaging when the resolved base is
// genuinely wrong.
func ResolvePRBaseRef(prNumber string) string {
	return resolvePRBaseRefWithRunner(prNumber, realGHForBaseRef{})
}

// resolvePRBaseRefWithRunner is the test entry point. ResolvePRBaseRef wires
// in the real gh runner; tests inject a scripted baseRefRunner.
func resolvePRBaseRefWithRunner(prNumber string, runner baseRefRunner) string {
	if strings.TrimSpace(prNumber) == "" {
		return ""
	}
	stdout, err := runner.run(prNumber)
	if err != nil {
		return ""
	}
	var info baseRefJSON
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &info); jsonErr != nil {
		return ""
	}
	return strings.TrimSpace(info.BaseRefName)
}
