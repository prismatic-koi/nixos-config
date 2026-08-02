// Package branchprotect implements the shared "is this branch protected"
// probe used by both `prism merge`'s initial-state decision (cmd/merge.go)
// and the merge-queue watcher's polling loop (internal/mergequeue).
//
// GitHub exposes two independent, non-overlapping ways to protect a branch:
//
//   - Classic branch protection: repos/{owner}/{repo}/branches/{branch}/protection
//   - Repository rulesets: repos/{owner}/{repo}/rules/branches/{branch}
//
// A repo protected only via a ruleset (as this repo's `main` is —
// nixos-config-branch-ruleset) returns HTTP 404 from the classic endpoint.
// Prior to #2436, both probe sites treated that 404 as "no protection
// configured at all" and, per the #2420 no-silent-auto-merge rule, waited
// for a human forever even though the PR was fully green.
//
// Probe fixes this by falling back to the rulesets effective-rules endpoint
// when the classic endpoint 404s. The #2420 conservative default is
// preserved: a repo with neither classic protection nor any effective
// ruleset still reports unprotected, and any error other than a 404 from
// either endpoint (network, permissions, rate-limit) is surfaced as an
// error so the caller takes the stay-watching path rather than silently
// concluding "unprotected".
package branchprotect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Runner executes `gh <args...>` (with "api" and the path already included by
// the caller) and returns combined stdout+stderr. It exists so both call
// sites can plug in their own gh-invocation plumbing (cmd/merge.go's
// unadorned exec.CommandContext vs. the watcher's --repo-prefixed runGH)
// while sharing the classic+ruleset fallback logic below.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Result is the outcome of a branch-protection probe.
type Result struct {
	// Configured reports whether the branch has protection configured,
	// either via classic branch protection or an actively-enforced ruleset.
	Configured bool

	// RequiredChecks enumerates the required status-check names. Only
	// meaningful when Configured is true. May be empty even when Configured
	// is true (e.g. a ruleset with only a pull_request rule and no
	// required_status_checks rule).
	RequiredChecks []string

	// RequiredApprovingReviewCount is the number of approving reviews the
	// branch protection requires before a PR may merge. Only meaningful when
	// Configured is true. Zero means no approving review is required — the
	// common case for a repo whose only gate is required status checks
	// (#2576). Callers must not infer "a human must approve" from any signal
	// other than this count being above zero.
	RequiredApprovingReviewCount int
}

// classicProtectionResponse is the subset of the GitHub classic
// branch-protection API response we care about.
type classicProtectionResponse struct {
	RequiredStatusChecks struct {
		Contexts []string `json:"contexts"` // legacy commit-status names
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"` // modern check-run names
	} `json:"required_status_checks"`
	// RequiredPullRequestReviews is absent from the classic response when the
	// branch does not require pull-request reviews, so its zero value (count
	// 0) is the correct "no approvals required" reading.
	RequiredPullRequestReviews struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
}

// rulesetRule is one entry in the array returned by
// GET repos/{owner}/{repo}/rules/branches/{branch} — the "effective rules"
// endpoint. GitHub's documented behaviour is that this endpoint returns only
// rules from rulesets that are actively enforced against the given branch;
// disabled rulesets and rulesets with enforcement="evaluate" (dry-run) are
// not included, nor are rulesets targeting a different branch. We defend
// against that contract loosening (or being wrong) anyway by honouring an
// explicit non-active "enforcement" field if one is ever present.
type rulesetRule struct {
	Type        string `json:"type"`
	Enforcement string `json:"enforcement,omitempty"`
	Parameters  struct {
		RequiredStatusChecks []struct {
			Context string `json:"context"`
		} `json:"required_status_checks"`
		// RequiredApprovingReviewCount lives on the pull_request rule's
		// parameters. Absent (zero) for other rule types.
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"parameters"`
}

// isNotFoundError reports whether a gh api error+output combination
// indicates the requested resource returned HTTP 404. Both the classic
// branch-protection endpoint (branch has no protection at all) and the
// rulesets effective-rules endpoint (branch is not covered by any ruleset)
// use this same "HTTP 404 / Not Found" shape, so a single detector covers
// both call sites. This is a plain absence signal, not a class-of-mistake
// worth distinguishing further.
func isNotFoundError(combinedOutput string) bool {
	low := strings.ToLower(combinedOutput)
	return strings.Contains(low, "http 404") ||
		strings.Contains(low, "branch not protected") ||
		strings.Contains(low, "\"status\":\"404\"") ||
		strings.Contains(low, "not found")
}

// Probe determines whether classicPath (the classic branch-protection API
// path) reports protection. If it 404s, Probe falls back to rulesetPath
// (the rules/branches/{branch} effective-rules path) before concluding the
// branch is unprotected.
//
// Any error other than a 404 from either endpoint is returned as-is —
// callers must treat that as a probe failure (network, permissions,
// rate-limit) and take the conservative "keep watching" path, NOT a silent
// "unprotected" conclusion.
func Probe(ctx context.Context, run Runner, classicPath, rulesetPath string) (Result, error) {
	out, err := run(ctx, "api", classicPath)
	if err == nil {
		res, parseErr := parseClassic(out)
		if parseErr != nil {
			return Result{}, fmt.Errorf("parse branch protection response: %w", parseErr)
		}
		return res, nil
	}

	if !isNotFoundError(string(out) + " " + err.Error()) {
		return Result{}, fmt.Errorf("gh api branch protection: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Classic endpoint 404d — fall back to the rulesets effective-rules
	// endpoint before concluding "unprotected".
	rout, rerr := run(ctx, "api", rulesetPath)
	if rerr != nil {
		if isNotFoundError(string(rout) + " " + rerr.Error()) {
			// Neither classic protection nor any effective ruleset — the
			// #2420 conservative default: genuinely unprotected.
			return Result{Configured: false}, nil
		}
		return Result{}, fmt.Errorf("gh api branch rules: %w: %s", rerr, strings.TrimSpace(string(rout)))
	}

	res, parseErr := parseRuleset(rout)
	if parseErr != nil {
		return Result{}, fmt.Errorf("parse branch rules response: %w", parseErr)
	}
	return res, nil
}

// parseClassic parses a classic branch-protection API response body.
func parseClassic(out []byte) (Result, error) {
	var resp classicProtectionResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return Result{}, err
	}
	seen := make(map[string]bool)
	var names []string
	for _, ctxName := range resp.RequiredStatusChecks.Contexts {
		if ctxName != "" && !seen[ctxName] {
			seen[ctxName] = true
			names = append(names, ctxName)
		}
	}
	for _, c := range resp.RequiredStatusChecks.Checks {
		if c.Context != "" && !seen[c.Context] {
			seen[c.Context] = true
			names = append(names, c.Context)
		}
	}
	return Result{
		Configured:                   true,
		RequiredChecks:               names,
		RequiredApprovingReviewCount: resp.RequiredPullRequestReviews.RequiredApprovingReviewCount,
	}, nil
}

// parseRuleset parses a rules/branches/{branch} effective-rules response
// body (a JSON array of rule objects) into a Result. A required_status_checks
// rule or a pull_request rule is each independently sufficient to mark the
// branch as protected (mirroring the issue's "and/or" framing); required
// check names are extracted only from the required_status_checks rule's
// parameters.
func parseRuleset(out []byte) (Result, error) {
	var rules []rulesetRule
	if err := json.Unmarshal(out, &rules); err != nil {
		return Result{}, err
	}

	configured := false
	seen := make(map[string]bool)
	var names []string
	requiredApprovals := 0
	for _, r := range rules {
		// Defensive: honour an explicit non-active enforcement value if one
		// is ever present, even though the documented API contract already
		// filters these out server-side.
		if r.Enforcement != "" && !strings.EqualFold(r.Enforcement, "active") {
			continue
		}
		switch r.Type {
		case "required_status_checks":
			configured = true
			for _, c := range r.Parameters.RequiredStatusChecks {
				if c.Context != "" && !seen[c.Context] {
					seen[c.Context] = true
					names = append(names, c.Context)
				}
			}
		case "pull_request":
			configured = true
			// A branch can, in principle, be covered by more than one
			// active pull_request rule; take the strictest (highest)
			// approving-review count so we never under-report the
			// requirement.
			if r.Parameters.RequiredApprovingReviewCount > requiredApprovals {
				requiredApprovals = r.Parameters.RequiredApprovingReviewCount
			}
		}
	}
	return Result{
		Configured:                   configured,
		RequiredChecks:               names,
		RequiredApprovingReviewCount: requiredApprovals,
	}, nil
}
