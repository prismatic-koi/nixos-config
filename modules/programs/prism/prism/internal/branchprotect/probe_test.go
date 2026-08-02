package branchprotect

import (
	"context"
	"errors"
	"testing"
)

// fakeRun builds a Runner that dispatches on the trailing path argument
// (args[len(args)-1], since callers always invoke as ("api", path)).
func fakeRun(t *testing.T, responses map[string]struct {
	out []byte
	err error
}) Runner {
	t.Helper()
	return func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) < 2 || args[0] != "api" {
			t.Fatalf("unexpected gh invocation: %v", args)
		}
		path := args[1]
		resp, ok := responses[path]
		if !ok {
			t.Fatalf("unexpected gh api call for path %q", path)
		}
		return resp.out, resp.err
	}
}

const (
	classicPath = "repos/{owner}/{repo}/branches/main/protection"
	rulesetPath = "repos/{owner}/{repo}/rules/branches/main"
)

func notFound() ([]byte, error) {
	return []byte("HTTP 404: Branch not protected"), errors.New("exit status 1")
}

// TestProbe_ClassicConfigured is the pre-existing happy path: classic
// protection responds 200, so the ruleset endpoint is never consulted.
func TestProbe_ClassicConfigured(t *testing.T) {
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: []byte(`{"required_status_checks":{"contexts":["legacy-ci"],"checks":[{"context":"pr-gate"}]}}`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Configured {
		t.Fatal("Configured: got false, want true")
	}
	want := map[string]bool{"legacy-ci": true, "pr-gate": true}
	if len(res.RequiredChecks) != len(want) {
		t.Fatalf("RequiredChecks: got %v, want %v", res.RequiredChecks, want)
	}
	for _, n := range res.RequiredChecks {
		if !want[n] {
			t.Errorf("unexpected required check %q", n)
		}
	}
}

// TestProbe_RulesetFallback_Configured is the #2436 false-negative case: the
// classic endpoint 404s (as it does on any ruleset-only-protected repo) but
// the rulesets effective-rules endpoint reports an actively-enforced
// required_status_checks rule. Probe must report Configured=true and extract
// the check context.
func TestProbe_RulesetFallback_Configured(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[
			{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-gate","integration_id":15368}]}},
			{"type":"pull_request"},
			{"type":"non_fast_forward"},
			{"type":"required_linear_history"},
			{"type":"deletion"}
		]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Configured {
		t.Fatal("Configured: got false, want true (ruleset with required_status_checks must count as protection)")
	}
	if len(res.RequiredChecks) != 1 || res.RequiredChecks[0] != "pr-gate" {
		t.Errorf("RequiredChecks: got %v, want [pr-gate]", res.RequiredChecks)
	}
}

// TestProbe_ClassicApprovingReviewCount checks that Probe surfaces the
// classic required_pull_request_reviews.required_approving_review_count so the
// #2576 caller can discriminate a genuine approval requirement from a repo
// that requires zero approvals.
func TestProbe_ClassicApprovingReviewCount(t *testing.T) {
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: []byte(`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]},"required_pull_request_reviews":{"required_approving_review_count":2}}`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.RequiredApprovingReviewCount != 2 {
		t.Errorf("RequiredApprovingReviewCount: got %d, want 2", res.RequiredApprovingReviewCount)
	}
}

// TestProbe_ClassicNoApprovingReviews is the #2576 zero-approval case: a
// classic response with no required_pull_request_reviews block must report a
// count of 0 (the field's zero value), so no false approval requirement is
// inferred.
func TestProbe_ClassicNoApprovingReviews(t *testing.T) {
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: []byte(`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.RequiredApprovingReviewCount != 0 {
		t.Errorf("RequiredApprovingReviewCount: got %d, want 0", res.RequiredApprovingReviewCount)
	}
}

// TestProbe_RulesetApprovingReviewCount checks the count is extracted from the
// pull_request rule's parameters on the ruleset path. This repo's main is
// ruleset-protected with a zero count, which is the exact false-positive the
// #2576 fix removes; a non-zero fixture proves the plumbing.
func TestProbe_RulesetApprovingReviewCount(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-gate"}]}},{"type":"pull_request","parameters":{"required_approving_review_count":1}}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Configured {
		t.Fatal("Configured: got false, want true")
	}
	if res.RequiredApprovingReviewCount != 1 {
		t.Errorf("RequiredApprovingReviewCount: got %d, want 1", res.RequiredApprovingReviewCount)
	}
}

// TestProbe_RulesetZeroApprovingReviews pins the on-repo reality (issue
// #2576): a pull_request rule with required_approving_review_count=0 must
// report a count of 0, so `prism merge` never claims a human must approve.
func TestProbe_RulesetZeroApprovingReviews(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"pull_request","parameters":{"required_approving_review_count":0}}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Configured {
		t.Fatal("Configured: got false, want true (pull_request rule is still protection)")
	}
	if res.RequiredApprovingReviewCount != 0 {
		t.Errorf("RequiredApprovingReviewCount: got %d, want 0", res.RequiredApprovingReviewCount)
	}
}

// TestProbe_RulesetFallback_PullRequestOnly covers a ruleset that only has a
// pull_request rule (no required_status_checks) — still protection, but with
// no required check names.
func TestProbe_RulesetFallback_PullRequestOnly(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"pull_request"},{"type":"deletion"}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Configured {
		t.Fatal("Configured: got false, want true (pull_request rule alone must count as protection)")
	}
	if len(res.RequiredChecks) != 0 {
		t.Errorf("RequiredChecks: got %v, want empty", res.RequiredChecks)
	}
}

// TestProbe_NeitherClassicNorRuleset is the #2420 conservative-default
// regression guard: both endpoints 404 (or the ruleset endpoint returns no
// rules) — Probe must report Configured=false, not an error.
func TestProbe_NeitherClassicNorRuleset(t *testing.T) {
	classicOut, classicErr := notFound()
	rulesetOut, rulesetErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: rulesetOut, err: rulesetErr},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Configured {
		t.Error("Configured: got true, want false (#2420 conservative default)")
	}
	if len(res.RequiredChecks) != 0 {
		t.Errorf("RequiredChecks: got %v, want empty", res.RequiredChecks)
	}
}

// TestProbe_RulesetOnlyIrrelevantRules covers a ruleset response that
// contains rules but none of the protection-relevant types
// (required_status_checks / pull_request) — e.g. only deletion/creation
// restrictions. Must NOT count as protection.
func TestProbe_RulesetOnlyIrrelevantRules(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"deletion"},{"type":"non_fast_forward"}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Configured {
		t.Error("Configured: got true, want false (deletion/non_fast_forward rules alone are not protection)")
	}
}

// TestProbe_RulesetEvaluateEnforcement_NotProtection is the defence-in-depth
// case: even though the rules/branches endpoint is documented to return only
// actively-enforced rules, Probe must not treat an explicit non-active
// enforcement value as protection if one is ever present.
func TestProbe_RulesetEvaluateEnforcement_NotProtection(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"required_status_checks","enforcement":"evaluate","parameters":{"required_status_checks":[{"context":"pr-gate"}]}}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Configured {
		t.Error("Configured: got true, want false (enforcement=evaluate/dry-run rulesets must not count as protection)")
	}
}

// TestProbe_RulesetDisabledEnforcement_NotProtection mirrors the evaluate
// case for an explicitly disabled ruleset rule.
func TestProbe_RulesetDisabledEnforcement_NotProtection(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte(`[{"type":"required_status_checks","enforcement":"disabled","parameters":{"required_status_checks":[{"context":"pr-gate"}]}}]`)},
	})
	res, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Configured {
		t.Error("Configured: got true, want false (disabled ruleset rules must not count as protection)")
	}
}

// TestProbe_ClassicNon404Error surfaces as an error without ever consulting
// the ruleset endpoint — a network/permissions/rate-limit failure must not
// be silently reinterpreted as "unprotected".
func TestProbe_ClassicNon404Error(t *testing.T) {
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: []byte("forbidden"), err: errors.New("exit status 1")},
	})
	_, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err == nil {
		t.Fatal("Probe: got nil error, want non-nil (non-404 classic failure must surface as an error)")
	}
}

// TestProbe_RulesetNon404Error surfaces as an error: the classic endpoint
// 404s (triggering the fallback) but the ruleset endpoint fails for a
// non-404 reason (network, permissions, rate-limit).
func TestProbe_RulesetNon404Error(t *testing.T) {
	classicOut, classicErr := notFound()
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: classicOut, err: classicErr},
		rulesetPath: {out: []byte("rate limit exceeded"), err: errors.New("exit status 1")},
	})
	_, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err == nil {
		t.Fatal("Probe: got nil error, want non-nil (non-404 ruleset failure must surface as an error, not silent 'unprotected')")
	}
}

// TestProbe_ClassicMalformedJSON verifies a parse failure on the classic
// (200) response surfaces as an error rather than falling through to the
// ruleset probe or reporting a false Configured value.
func TestProbe_ClassicMalformedJSON(t *testing.T) {
	run := fakeRun(t, map[string]struct {
		out []byte
		err error
	}{
		classicPath: {out: []byte("not json")},
	})
	_, err := Probe(context.Background(), run, classicPath, rulesetPath)
	if err == nil {
		t.Fatal("Probe: got nil error, want non-nil on malformed classic JSON")
	}
}
