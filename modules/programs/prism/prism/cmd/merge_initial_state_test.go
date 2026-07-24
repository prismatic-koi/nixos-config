package cmd

// Tests for the #2420 `prism merge` initial-state probe: probeInitialState,
// probeBranchProtection, pendingRequiredCheckNames, and the state-table
// message discipline enforced by runMerge.
//
// The tests install a discriminating `gh` shim on PATH so `gh pr view` and
// `gh api ...branches/:branch/protection` can return distinct fixtures per
// invocation. Every shim increments a counter file so the tests can assert
// exact call counts.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubGhBinStates installs a `gh` shim on PATH that discriminates on the
// first two argv tokens ("pr view" vs "api ...protection") and returns the
// caller-supplied JSON for each. Every invocation appends a line to a
// counter file so tests can assert exact call counts.
//
// prJSON is returned for `gh pr view`. When protectionJSON is empty the
// branch-protection endpoint returns an HTTP 404 exit status (the shape gh
// produces when the branch is unprotected). When protectionJSON is non-empty
// it is returned verbatim with exit 0.
func stubGhBinStates(t *testing.T, prJSON, protectionJSON string) string {
	t.Helper()
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "gh.calls")
	ghPath := filepath.Join(dir, "gh")
	var protectionCase string
	if protectionJSON == "" {
		protectionCase = `        echo "HTTP 404: Branch not protected (https://api.github.com/...)" >&2
        exit 1
        ;;`
	} else {
		protectionCase = fmt.Sprintf(`        cat <<EOF
%s
EOF
        exit 0
        ;;`, protectionJSON)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo call "$@" >> %q
case "$1 $2" in
    "pr view")
        cat <<EOF
%s
EOF
        exit 0
        ;;
    "api "*)
%s
    *)
        # gh pr merge / gh pr update-branch etc. — return success.
        exit 0
        ;;
esac
`, counterPath, prJSON, protectionCase)
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return counterPath
}

// ── probeInitialState state-table tests ───────────────────────────────────────

func TestProbeInitialState_AlreadyMerged(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"MERGED","number":100,"title":"already done","mergedAt":"2026-07-01T00:00:00Z","mergeStateStatus":"","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[]}`,
		`{"required_status_checks":{"contexts":[],"checks":[]}}`,
	)
	dec, err := probeInitialState(100)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeAlreadyMerged {
		t.Errorf("outcome: got %v, want AlreadyMerged", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "PR #100 already merged") {
		t.Errorf("message %q does not contain 'PR #100 already merged'", dec.Message)
	}
	if !strings.Contains(dec.Message, "Please clean up the branch and worktree") {
		t.Errorf("message %q does not contain 'Please clean up the branch and worktree'", dec.Message)
	}
	if strings.Contains(dec.Message, "prism cleanup") {
		t.Errorf("message %q must not imply prism performed the cleanup", dec.Message)
	}
}

func TestProbeInitialState_ClosedNotMerged(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"CLOSED","number":101,"title":"discarded","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)
	dec, err := probeInitialState(101)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeClosedNotMerged {
		t.Errorf("outcome: got %v, want ClosedNotMerged", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "closed without merge") {
		t.Errorf("message %q does not contain 'closed without merge'", dec.Message)
	}
	if !strings.Contains(dec.Message, "No action required from you") {
		t.Errorf("message %q does not contain 'No action required from you'", dec.Message)
	}
	if !strings.Contains(dec.Message, "a human closed this") {
		t.Errorf("message %q does not attribute the close to a human", dec.Message)
	}
	if !strings.Contains(dec.Message, "Please clean up the branch and worktree") {
		t.Errorf("message %q does not contain the cleanup phrase", dec.Message)
	}
}

func TestProbeInitialState_Conflict(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":102,"title":"conflicted","mergedAt":null,"mergeStateStatus":"DIRTY","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)
	dec, err := probeInitialState(102)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeConflict {
		t.Errorf("outcome: got %v, want Conflict", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "conflicts") {
		t.Errorf("message %q does not mention conflicts", dec.Message)
	}
	if !strings.Contains(dec.Message, "Worker needs to rebase") {
		t.Errorf("message %q does not say 'Worker needs to rebase'", dec.Message)
	}
}

func TestProbeInitialState_ProtectedReady(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":103,"title":"ready","mergedAt":null,"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS","status":"COMPLETED"}]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)
	dec, err := probeInitialState(103)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeEnqueueReady {
		t.Errorf("outcome: got %v, want EnqueueReady", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "PR #103 ready. Merging now.") {
		t.Errorf("message %q does not contain 'PR #103 ready. Merging now.'", dec.Message)
	}
}

func TestProbeInitialState_ProtectedPending(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":104,"title":"waiting","mergedAt":null,"mergeStateStatus":"UNSTABLE","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"","status":"IN_PROGRESS"},{"name":"lint","conclusion":"SUCCESS","status":"COMPLETED"}]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"},{"context":"lint"}]}}`,
	)
	dec, err := probeInitialState(104)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeEnqueuePending {
		t.Errorf("outcome: got %v, want EnqueuePending", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "waiting on 1 check(s)") {
		t.Errorf("message %q does not report the pending count", dec.Message)
	}
	if !strings.Contains(dec.Message, "pr-gate") {
		t.Errorf("message %q does not name the pending check pr-gate", dec.Message)
	}
	if !strings.Contains(dec.Message, "Standing by; will merge when green") {
		t.Errorf("message %q does not contain 'Standing by; will merge when green'", dec.Message)
	}
	if !strings.Contains(dec.Message, "No action required from you") {
		t.Errorf("message %q does not contain 'No action required from you'", dec.Message)
	}
}

func TestProbeInitialState_ProtectedReviewRequired(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":105,"title":"awaiting review","mergedAt":null,"mergeStateStatus":"BLOCKED","reviewDecision":"REVIEW_REQUIRED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS","status":"COMPLETED"}]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)
	dec, err := probeInitialState(105)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeEnqueueReview {
		t.Errorf("outcome: got %v, want EnqueueReview", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "requires human approval") {
		t.Errorf("message %q does not contain 'requires human approval'", dec.Message)
	}
	if !strings.Contains(dec.Message, "No action required from you") {
		t.Errorf("message %q does not contain 'No action required from you'", dec.Message)
	}
	if !strings.Contains(dec.Message, "do not request reviewers, do not add approvers, just wait") {
		t.Errorf("message %q does not contain the #2420 anti-reviewer-shopping guidance", dec.Message)
	}
	if !strings.Contains(dec.Message, "notify if merged out-of-band") {
		t.Errorf("message %q does not contain the out-of-band notification promise", dec.Message)
	}
}

// TestProbeInitialState_NoBranchProtection is the AC1-critical case: a repo
// whose branch-protection endpoint returns HTTP 404 takes the "no protection,
// wait for a human" path, NOT the "merge now" path. This is the bootstrap-
// repo accidental-auto-merge class the #2420 spec explicitly closes.
func TestProbeInitialState_NoBranchProtection(t *testing.T) {
	// Note: mergeStateStatus is CLEAN — on an unprotected repo GitHub often
	// reports the PR as trivially clean, which is exactly the trap the
	// #2420 fix closes. The 404 on branch protection is the deciding signal.
	stubGhBinStates(t,
		`{"state":"OPEN","number":106,"title":"bootstrap PR","mergedAt":null,"mergeStateStatus":"CLEAN","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)
	dec, err := probeInitialState(106)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeEnqueueUnprotected {
		t.Errorf("outcome: got %v, want EnqueueUnprotected (#2420 AC: 404 on branch protection must take the 'wait for human' path, NOT the 'merge now' path)", dec.Outcome)
	}
	if !strings.Contains(dec.Message, "no branch protection configured") {
		t.Errorf("message %q does not contain 'no branch protection configured'", dec.Message)
	}
	if !strings.Contains(dec.Message, "Not auto-merging") {
		t.Errorf("message %q does not contain 'Not auto-merging'", dec.Message)
	}
	if !strings.Contains(dec.Message, "Waiting for a human") {
		t.Errorf("message %q does not contain 'Waiting for a human'", dec.Message)
	}
	if !strings.Contains(dec.Message, "No action required from you") {
		t.Errorf("message %q does not contain 'No action required from you'", dec.Message)
	}
}

// ── probeBranchProtection tests ───────────────────────────────────────────────

func TestProbeBranchProtection_404NotAnError(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":1,"title":"","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"", // 404
	)
	protected, required, err := probeBranchProtection("main")
	if err != nil {
		t.Fatalf("probeBranchProtection on 404: got err %v, want nil (404 is a state, not an error)", err)
	}
	if protected {
		t.Errorf("protected: got true, want false on 404 (#2420)")
	}
	if len(required) != 0 {
		t.Errorf("required: got %v, want empty on 404", required)
	}
}

func TestProbeBranchProtection_ParsesRequiredChecks(t *testing.T) {
	stubGhBinStates(t,
		`{"state":"OPEN","number":1,"title":"","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		`{"required_status_checks":{"contexts":["legacy-ci"],"checks":[{"context":"pr-gate"},{"context":"lint"}]}}`,
	)
	protected, required, err := probeBranchProtection("main")
	if err != nil {
		t.Fatalf("probeBranchProtection: %v", err)
	}
	if !protected {
		t.Error("protected: got false, want true")
	}
	if len(required) != 3 {
		t.Fatalf("required: got %v, want 3 names", required)
	}
	seen := map[string]bool{}
	for _, n := range required {
		seen[n] = true
	}
	for _, want := range []string{"legacy-ci", "pr-gate", "lint"} {
		if !seen[want] {
			t.Errorf("required %v missing %q", required, want)
		}
	}
}

// ── #2436 ruleset-fallback tests ──────────────────────────────────────────────

// stubGhBinRuleset installs a `gh` shim that discriminates between the
// classic branches/.../protection path and the rules/branches/... path, so
// tests can exercise the #2436 fallback independently of each other. Both
// classicJSON and rulesetJSON empty means "404" for that endpoint.
func stubGhBinRuleset(t *testing.T, prJSON, classicJSON, rulesetJSON string) string {
	t.Helper()
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "gh.calls")
	ghPath := filepath.Join(dir, "gh")

	renderCase := func(json string) string {
		if json == "" {
			return `        echo "HTTP 404: Not Found (https://api.github.com/...)" >&2
        exit 1
        ;;`
		}
		return fmt.Sprintf(`        cat <<'GHEOF'
%s
GHEOF
        exit 0
        ;;`, json)
	}

	script := fmt.Sprintf(`#!/bin/sh
echo call "$@" >> %q
case "$1 $2" in
    "pr view")
        cat <<'GHEOF'
%s
GHEOF
        exit 0
        ;;
    "api "*)
        case "$2" in
            */rules/branches/*)
%s
            *)
%s
        esac
        ;;
    *)
        exit 0
        ;;
esac
`, counterPath, prJSON, renderCase(rulesetJSON), renderCase(classicJSON))
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return counterPath
}

// TestProbeBranchProtection_RulesetFallback_Configured is the PR #2435
// false-negative regression guard: classic protection 404s (as it does on
// this repo's ruleset-only-protected main) but the rules/branches endpoint
// reports an actively-enforced required_status_checks rule. The probe must
// report protected=true with the check name extracted, not "unprotected".
func TestProbeBranchProtection_RulesetFallback_Configured(t *testing.T) {
	stubGhBinRuleset(t,
		`{"state":"OPEN","number":1,"title":"","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"", // classic 404s
		`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-gate","integration_id":15368}]}},{"type":"pull_request"},{"type":"non_fast_forward"},{"type":"required_linear_history"},{"type":"deletion"}]`,
	)
	protected, required, err := probeBranchProtection("main")
	if err != nil {
		t.Fatalf("probeBranchProtection: %v", err)
	}
	if !protected {
		t.Fatal("protected: got false, want true (ruleset-only protection must be detected, #2436)")
	}
	if len(required) != 1 || required[0] != "pr-gate" {
		t.Errorf("required: got %v, want [pr-gate]", required)
	}
}

// TestProbeBranchProtection_NeitherClassicNorRuleset is the #2420
// conservative-default regression guard: both endpoints 404, so the probe
// must still report unprotected.
func TestProbeBranchProtection_NeitherClassicNorRuleset(t *testing.T) {
	stubGhBinRuleset(t,
		`{"state":"OPEN","number":1,"title":"","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"", // classic 404s
		"", // ruleset 404s too
	)
	protected, required, err := probeBranchProtection("main")
	if err != nil {
		t.Fatalf("probeBranchProtection: %v", err)
	}
	if protected {
		t.Error("protected: got true, want false (no classic protection and no effective ruleset, #2420)")
	}
	if len(required) != 0 {
		t.Errorf("required: got %v, want empty", required)
	}
}

// TestProbeInitialState_RulesetProtected_Ready is the PR #2435 end-to-end
// scenario: a ruleset-protected repo with a CLEAN, all-checks-green PR must
// reach the enqueue-ready outcome rather than being stuck reporting
// "no branch protection configured".
func TestProbeInitialState_RulesetProtected_Ready(t *testing.T) {
	stubGhBinRuleset(t,
		`{"state":"OPEN","number":2435,"title":"ruleset protected","mergedAt":null,"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS","status":"COMPLETED"}]}`,
		"", // classic 404s
		`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-gate"}]}},{"type":"pull_request"}]`,
	)
	dec, err := probeInitialState(2435)
	if err != nil {
		t.Fatalf("probeInitialState: %v", err)
	}
	if dec.Outcome != initialOutcomeEnqueueReady {
		t.Errorf("outcome: got %v, want EnqueueReady (#2436 must un-block the PR #2435 scenario)", dec.Outcome)
	}
}

// ── pendingRequiredCheckNames unit tests ──────────────────────────────────────

func TestPendingRequiredCheckNames(t *testing.T) {
	cases := []struct {
		name     string
		rollup   []checkEntry
		required []string
		want     []string
	}{
		{
			name: "all_pass",
			rollup: []checkEntry{
				{Name: "pr-gate", Conclusion: "SUCCESS"},
			},
			required: []string{"pr-gate"},
			want:     nil,
		},
		{
			name: "one_pending",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "IN_PROGRESS"},
				{Name: "lint", Conclusion: "SUCCESS"},
			},
			required: []string{"pr-gate", "lint"},
			want:     []string{"pr-gate"},
		},
		{
			name: "missing_from_rollup",
			rollup: []checkEntry{
				{Name: "lint", Conclusion: "SUCCESS"},
			},
			required: []string{"pr-gate", "lint"},
			want:     []string{"pr-gate"},
		},
		{
			name:     "empty_required",
			rollup:   []checkEntry{{Name: "pr-gate", Conclusion: "SUCCESS"}},
			required: nil,
			want:     nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := pendingRequiredCheckNames(tc.rollup, tc.required)
			if len(got) != len(tc.want) {
				t.Fatalf("pendingRequiredCheckNames = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("pendingRequiredCheckNames[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── runMerge terminal-outcome short-circuit tests ─────────────────────────────

// TestRunMerge_AlreadyMerged_ShortCircuitsAndPrintsMessage verifies that
// invoking `prism merge <pr>` on an already-merged PR emits the terminal
// message and does NOT write a row to pending_merges.
func TestRunMerge_AlreadyMerged_ShortCircuitsAndPrintsMessage(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-already-merged")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"MERGED","number":7001,"title":"done","mergedAt":"2026-07-01T00:00:00Z","mergeStateStatus":"","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"7001"}); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
	})

	if !strings.Contains(out, "PR #7001 already merged") {
		t.Errorf("stdout %q does not contain 'PR #7001 already merged'", out)
	}
	if !strings.Contains(out, "Please clean up the branch and worktree") {
		t.Errorf("stdout %q does not contain the #2420 cleanup phrase", out)
	}
	if strings.Contains(out, "enqueued") {
		t.Errorf("stdout %q falsely reports 'enqueued' on an already-merged PR", out)
	}

	// No row must be written to pending_merges.
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	row, _ := d.PendingMergeByPR(7001, "nixos-config")
	if row != nil {
		t.Errorf("pending_merges row exists for already-merged PR: %+v (must not enqueue)", row)
	}
}

// TestRunMerge_ClosedNotMerged_ShortCircuits verifies the same for the
// closed-without-merge terminal outcome.
func TestRunMerge_ClosedNotMerged_ShortCircuits(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-closed-notmerged")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"CLOSED","number":7002,"title":"nope","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"7002"}); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
	})

	if !strings.Contains(out, "closed without merge") {
		t.Errorf("stdout %q does not contain 'closed without merge'", out)
	}
	if !strings.Contains(out, "No action required from you") {
		t.Errorf("stdout %q does not contain 'No action required from you'", out)
	}
	if !strings.Contains(out, "Please clean up the branch and worktree") {
		t.Errorf("stdout %q does not contain the cleanup phrase", out)
	}
	if strings.Contains(out, "enqueued") {
		t.Errorf("stdout %q falsely reports 'enqueued' on a closed-not-merged PR", out)
	}
}

// TestRunMerge_MergeConflict_ExitsFailure verifies the merge-conflict path:
// runMerge emits the "conflicts, worker needs to rebase" message and returns
// a non-zero exit (so scripting flows see the failure) without enqueueing.
func TestRunMerge_MergeConflict_ExitsFailure(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-conflict")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"OPEN","number":7003,"title":"conflicted","mergedAt":null,"mergeStateStatus":"DIRTY","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)

	var runErr error
	out := captureStdout(t, func() {
		runErr = runMerge(mergeCmd, []string{"7003"})
	})

	if runErr == nil {
		t.Fatal("runMerge on DIRTY PR: got nil error, want non-nil (must exit non-zero)")
	}
	if !strings.Contains(out, "conflicts") {
		t.Errorf("stdout %q does not contain 'conflicts'", out)
	}
	if !strings.Contains(out, "Worker needs to rebase") {
		t.Errorf("stdout %q does not contain 'Worker needs to rebase'", out)
	}

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	row, _ := d.PendingMergeByPR(7003, "nixos-config")
	if row != nil {
		t.Errorf("pending_merges row exists for DIRTY PR: %+v (must not enqueue)", row)
	}
}

// TestRunMerge_Unprotected_EnqueuesButAnnounces verifies the AC1-critical
// user-facing path: `prism merge` on an unprotected repo emits the
// "no branch protection" message and enqueues the PR for silent polling.
// The watcher will never auto-merge; the initial message tells the
// coordinator to wait for a human.
func TestRunMerge_Unprotected_EnqueuesButAnnounces(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-unprotected")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"OPEN","number":7004,"title":"bootstrap","mergedAt":null,"mergeStateStatus":"CLEAN","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"", // 404
	)

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"7004"}); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
	})

	if !strings.Contains(out, "no branch protection configured") {
		t.Errorf("stdout %q does not contain 'no branch protection configured'", out)
	}
	if !strings.Contains(out, "Not auto-merging") {
		t.Errorf("stdout %q does not contain 'Not auto-merging'", out)
	}
	if !strings.Contains(out, "No action required from you") {
		t.Errorf("stdout %q does not contain 'No action required from you'", out)
	}
	if !strings.Contains(out, "PR #7004 enqueued") {
		t.Errorf("stdout %q does not report enqueue (unprotected repos still enqueue for silent polling)", out)
	}
}

// TestRunMerge_ProtectedReviewRequired_HasAntiReviewerShoppingGuidance is
// the exact-phrase AC: reviewer/approval waiting messages must contain the
// literal "do not request reviewers, do not add approvers, just wait"
// guidance to prevent the coordinator's helpful-but-wrong reviewer-shopping
// reflex.
func TestRunMerge_ProtectedReviewRequired_HasAntiReviewerShoppingGuidance(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-review-required")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"OPEN","number":7005,"title":"awaiting","mergedAt":null,"mergeStateStatus":"BLOCKED","reviewDecision":"REVIEW_REQUIRED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS"}]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"7005"}); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
	})

	if !strings.Contains(out, "do not request reviewers, do not add approvers, just wait") {
		t.Errorf("stdout %q does not contain the #2420 anti-reviewer-shopping guidance", out)
	}
	if !strings.Contains(out, "No action required from you") {
		t.Errorf("stdout %q does not contain 'No action required from you'", out)
	}
	// Enqueue still fires so the watcher can drive the merge on approval.
	if !strings.Contains(out, "PR #7005 enqueued") {
		t.Errorf("stdout %q does not report enqueue", out)
	}
}
