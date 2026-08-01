package mergequeue

// Tests for issue #2525: a BLOCKED PR whose required checks have concluded
// with a failure must terminate the queue row and notify the coordinator,
// instead of polling forever in silence.
//
// The file has two halves.
//
//  1. Table-driven unit tests over failedRequiredChecks, the pure function
//     that discriminates "may still resolve" from "will never resolve".
//  2. Integration tests that drive the real Watcher.tick through an injected
//     gh runner, so the assertions cover the production state machine rather
//     than the fakeWatcher mirror used elsewhere in this package.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// cleanupPhrase is the completion-message discipline phrase that every merge
// and close notification carries. The #2525 failure notification must NOT
// carry it: nothing merged, nothing closed, and the worker still needs the
// branch to push the fix.
const cleanupPhrase = "Please clean up the branch and worktree"

// ── 1. failedRequiredChecks: the state matrix ────────────────────────────────

// TestFailedRequiredChecks_Matrix walks the cross product of rollup states
// against the required-check list. The invariant under test: a non-empty
// result is returned ONLY when every required check is accounted for in the
// rollup AND at least one concluded in a recognised failure state.
func TestFailedRequiredChecks_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		rollup   []checkEntry
		required []string
		want     []string
		why      string
	}{
		// ── The bug this issue fixes ────────────────────────────────────
		{
			name: "completed_failure_single_required",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
			why:      "the PR #2524 production case: required check completed and failed",
		},
		{
			name: "completed_failure_among_passing_required",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "SUCCESS"},
			},
			required: []string{"pr-gate", "validate-flakes"},
			want:     []string{"pr-gate"},
			why:      "only the failing required check is named",
		},
		{
			name: "multiple_required_failures_follow_required_order",
			rollup: []checkEntry{
				{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate", "validate-flakes"},
			want:     []string{"pr-gate", "validate-flakes"},
			why:      "output order follows the required list, not the rollup, so the text is stable across polls",
		},

		// ── Non-SUCCESS failure conclusions ─────────────────────────────
		{
			name:     "timed_out_counts_as_failure",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "TIMED_OUT"}},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
		},
		{
			name:     "cancelled_counts_as_failure",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "CANCELLED"}},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
		},
		{
			name:     "action_required_counts_as_failure",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "ACTION_REQUIRED"}},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
		},
		{
			name:     "conclusion_match_is_case_insensitive",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "completed", Conclusion: "failure"}},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
			why:      "gh has emitted lowercase enums in the past; matching must fold case",
		},

		// ── Still resolvable: must stay silent ──────────────────────────
		{
			name: "required_in_progress",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "a running check can still go green",
		},
		{
			name: "required_queued",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "QUEUED", Conclusion: ""},
			},
			required: []string{"pr-gate"},
			want:     nil,
		},
		{
			name: "required_absent_from_rollup",
			rollup: []checkEntry{
				{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "an absent required check means the set is not accounted for, even though another entry failed",
		},
		{
			name:     "empty_rollup",
			rollup:   nil,
			required: []string{"pr-gate"},
			want:     nil,
			why:      "fresh push: the rollup for the new head commit has not filled yet",
		},
		{
			name: "all_required_success_approval_outstanding",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "BLOCKED here means a human approval is outstanding, which resolves on its own",
		},
		{
			name: "non_required_check_failing",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "optional-lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "branch protection does not gate on optional-lint, so its failure is not a dead end",
		},
		{
			name: "one_required_failed_another_still_running",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "validate-flakes", Status: "IN_PROGRESS", Conclusion: ""},
			},
			required: []string{"pr-gate", "validate-flakes"},
			want:     nil,
			why:      "the required set is not fully accounted for; wait for the running check",
		},
		{
			name:     "no_required_checks_configured",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"}},
			required: nil,
			want:     nil,
			why:      "no configured gate means no evidence of a dead end",
		},
		{
			name:     "completed_with_empty_conclusion",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: ""}},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "an unreadable entry is not evidence that the check finished",
		},
		{
			name:     "neutral_conclusion_is_not_a_failure",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "NEUTRAL"}},
			required: []string{"pr-gate"},
			want:     nil,
		},
		{
			name:     "skipped_conclusion_is_not_a_failure",
			rollup:   []checkEntry{{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SKIPPED"}},
			required: []string{"pr-gate"},
			want:     nil,
		},
		{
			name:     "entry_with_no_readable_fields",
			rollup:   []checkEntry{{Name: "pr-gate"}},
			required: []string{"pr-gate"},
			want:     nil,
		},
		{
			name:     "unnamed_rollup_entries_are_ignored",
			rollup:   []checkEntry{{Status: "COMPLETED", Conclusion: "FAILURE"}},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "an entry with neither name nor context cannot satisfy a required name",
		},

		// ── Duplicate entries for one name: pending must dominate ───────
		{
			name: "rerun_in_flight_masks_older_failure",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "the worker already re-triggered CI; declaring the PR dead here would be the expensive mistake",
		},
		{
			name: "duplicate_entries_all_concluded_one_failed",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate"},
			want:     []string{"pr-gate"},
			why:      "with nothing pending, a failure among the entries for that name is a real failure",
		},
		{
			name: "stale_conclusion_on_a_running_check_is_pending",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate"},
			want:     nil,
			why:      "status wins over a leftover conclusion",
		},
		{
			name: "duplicate_required_names_are_deduplicated",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			required: []string{"pr-gate", "pr-gate"},
			want:     []string{"pr-gate"},
		},

		// ── Legacy commit statuses (context/state, no status/conclusion) ─
		{
			name:     "legacy_state_failure",
			rollup:   []checkEntry{{Context: "ci/legacy", State: "FAILURE"}},
			required: []string{"ci/legacy"},
			want:     []string{"ci/legacy"},
		},
		{
			name:     "legacy_state_error",
			rollup:   []checkEntry{{Context: "ci/legacy", State: "ERROR"}},
			required: []string{"ci/legacy"},
			want:     []string{"ci/legacy"},
		},
		{
			name:     "legacy_state_pending",
			rollup:   []checkEntry{{Context: "ci/legacy", State: "PENDING"}},
			required: []string{"ci/legacy"},
			want:     nil,
		},
		{
			name:     "legacy_state_expected",
			rollup:   []checkEntry{{Context: "ci/legacy", State: "EXPECTED"}},
			required: []string{"ci/legacy"},
			want:     nil,
		},
		{
			name:     "legacy_state_success",
			rollup:   []checkEntry{{Context: "ci/legacy", State: "SUCCESS"}},
			required: []string{"ci/legacy"},
			want:     nil,
		},
		{
			name: "mixed_modern_and_legacy_one_legacy_failure",
			rollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Context: "ci/legacy", State: "FAILURE"},
			},
			required: []string{"pr-gate", "ci/legacy"},
			want:     []string{"ci/legacy"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := failedRequiredChecks(tc.rollup, tc.required)
			if !equalStrings(got, tc.want) {
				msg := fmt.Sprintf("failedRequiredChecks(%+v, %v) = %v, want %v",
					tc.rollup, tc.required, got, tc.want)
				if tc.why != "" {
					msg += "\n  rationale: " + tc.why
				}
				t.Error(msg)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── 2. Notification text ─────────────────────────────────────────────────────

// TestRenderRequiredChecksFailedText pins the notification wording, including
// the AC that forbids the cleanup phrase.
func TestRenderRequiredChecksFailedText(t *testing.T) {
	got := renderRequiredChecksFailedText(2524, "pr-gate, nix-build-prism-checked")
	want := "PR #2524 CI failed: pr-gate, nix-build-prism-checked. " +
		"Worker needs to fix and push. No merge will happen until then."
	if got != want {
		t.Errorf("renderRequiredChecksFailedText:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, cleanupPhrase) {
		t.Errorf("notification text %q must not contain %q — the branch is still needed for the fix (#2525)", got, cleanupPhrase)
	}
}

// TestJoinFailedCheckNames_BoundsLength verifies the rendered name list is
// capped, and that truncation never emits invalid UTF-8 into the DB error
// column or the notification.
func TestJoinFailedCheckNames_BoundsLength(t *testing.T) {
	if got := joinFailedCheckNames([]string{"a", "b"}); got != "a, b" {
		t.Errorf("short list: got %q, want %q", got, "a, b")
	}

	var many []string
	for i := 0; i < 200; i++ {
		many = append(many, fmt.Sprintf("check-%d-\u00e4", i))
	}
	got := joinFailedCheckNames(many)
	if len(got) > maxFailedCheckNamesLen+len("...") {
		t.Errorf("joined length %d exceeds cap %d (+ ellipsis)", len(got), maxFailedCheckNamesLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated list %q does not end with the ellipsis marker", got)
	}
	if !utf8Valid(got) {
		t.Errorf("truncated list %q is not valid UTF-8", got)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

// ── 3. Production tick() integration ─────────────────────────────────────────

// tickGH is a gh-CLI stub for driving the real Watcher.tick. It answers the
// branch-protection probe and `gh pr view`, and fails the test loudly if the
// watcher ever attempts a merge mutation — the #2420 core rule under test.
type tickGH struct {
	t        *testing.T
	required []string
	info     prInfo
	calls    [][]string
}

func (g *tickGH) run(_ context.Context, args ...string) ([]byte, error) {
	g.t.Helper()
	g.calls = append(g.calls, append([]string(nil), args...))

	switch {
	case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
		quoted := make([]string, 0, len(g.required))
		for _, name := range g.required {
			quoted = append(quoted, strconv.Quote(name))
		}
		return []byte(fmt.Sprintf(`{"required_status_checks":{"contexts":[%s]}}`,
			strings.Join(quoted, ","))), nil

	case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
		body, err := json.Marshal(g.info)
		if err != nil {
			g.t.Fatalf("marshal prInfo fixture: %v", err)
		}
		return body, nil

	case len(args) >= 4 && args[2] == "pr" && args[3] == "merge":
		g.t.Errorf("gh pr merge was invoked on a BLOCKED PR — #2525 must add no merge path (argv: %v)", args)
		return nil, fmt.Errorf("merge must not be attempted")

	case len(args) >= 4 && args[2] == "pr" && args[3] == "update-branch":
		g.t.Errorf("gh pr update-branch was invoked on a BLOCKED PR (argv: %v)", args)
		return nil, fmt.Errorf("update-branch must not be attempted")
	}

	g.t.Fatalf("unexpected gh call: %v", args)
	return nil, nil
}

// mergeAttempted reports whether any recorded gh call was a merge mutation.
func (g *tickGH) mergeAttempted() bool {
	for _, argv := range g.calls {
		for _, a := range argv {
			if a == "merge" {
				return true
			}
		}
	}
	return false
}

// blockedTickFixture wires a real Watcher, a real test DB, a seeded
// coordinator, a capturing notification server, and one enqueued PR.
type blockedTickFixture struct {
	db      *db.DB
	watcher *Watcher
	gh      *tickGH
	srv     *capturingServer
	pr      int
	repo    string
	session string
	inst    string
}

func newBlockedTickFixture(t *testing.T, pr int, required []string, info prInfo) *blockedTickFixture {
	t.Helper()

	const (
		session = "myrepo@main"
		repo    = "myrepo"
	)
	instanceID := fmt.Sprintf("inst-2525-%d", pr)

	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, session, instanceID, parsePort(t, srv.URL), fmt.Sprintf("sid-2525-%d", pr))

	if _, err := d.EnqueueMerge(pr, repo, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	gh := &tickGH{t: t, required: required, info: info}
	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "owner/myrepo",
		runGHFunc:   gh.run,
	}

	return &blockedTickFixture{
		db: d, watcher: w, gh: gh, srv: srv,
		pr: pr, repo: repo, session: session, inst: instanceID,
	}
}

// status reads the current pending_merges status for the fixture's PR.
func (f *blockedTickFixture) status(t *testing.T) string {
	t.Helper()
	row, err := f.db.PendingMergeByPR(f.pr, f.repo)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row == nil {
		t.Fatalf("PendingMergeByPR(%d, %q): row disappeared", f.pr, f.repo)
	}
	return row.Status
}

// blockedInfo builds a BLOCKED prInfo fixture.
func blockedInfo(reviewDecision string, rollup []checkEntry) prInfo {
	return prInfo{
		State:             "OPEN",
		MergeStateStatus:  "BLOCKED",
		ReviewDecision:    reviewDecision,
		StatusCheckRollup: rollup,
	}
}

// TestWatcher_BLOCKED_RequiredCheckFailed_TerminatesAndNotifies is the
// headline #2525 regression test, reproducing the PR #2524 production
// scenario: pr-gate COMPLETED/FAILURE, all checks COMPLETED, BLOCKED.
//
// Covers the functional ACs: terminal transition, polling stops, one
// notification naming the PR number and the failed required check names, and
// no cleanup phrase in that notification.
func TestWatcher_BLOCKED_RequiredCheckFailed_TerminatesAndNotifies(t *testing.T) {
	f := newBlockedTickFixture(t, 2524, []string{"pr-gate"}, blockedInfo("APPROVED", []checkEntry{
		{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "nix-build-prism-checked", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "go-tests", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}))

	f.watcher.tick(context.Background())

	if got := f.status(t); got != "failed" {
		t.Errorf("status after BLOCKED+required-failure: got %q, want failed (#2525: the row must terminate, not poll forever)", got)
	}
	if f.srv.called != 1 {
		t.Fatalf("notifications sent: got %d, want 1", f.srv.called)
	}

	text := extractNotifyText(t, f.srv.lastBody)
	if !strings.Contains(text, "PR #2524") {
		t.Errorf("notification %q does not name the PR number", text)
	}
	if !strings.Contains(text, "pr-gate") {
		t.Errorf("notification %q does not name the failed required check pr-gate", text)
	}
	if strings.Contains(text, cleanupPhrase) {
		t.Errorf("notification %q contains %q — forbidden, the branch is still needed for the fix", text, cleanupPhrase)
	}
	// nix-build-prism-checked also failed but is NOT in the required set, so
	// it must not be named: the coordinator acts on required gates only.
	if strings.Contains(text, "nix-build-prism-checked") {
		t.Errorf("notification %q names a non-required check; only required gates belong in the text", text)
	}
	if f.gh.mergeAttempted() {
		t.Error("a merge mutation was attempted on the failure path — #2525 must add no merge path")
	}
}

// TestWatcher_BLOCKED_RequiredCheckFailed_RowLeavesWatchingList covers the AC
// that `prism merges list` no longer shows the row as watching. The command
// reads MergeQueueForInstance, so the assertion is made at that boundary.
func TestWatcher_BLOCKED_RequiredCheckFailed_RowLeavesWatchingList(t *testing.T) {
	f := newBlockedTickFixture(t, 3510, []string{"pr-gate"}, blockedInfo("APPROVED", []checkEntry{
		{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
	}))

	watchingBefore, err := f.db.MergeQueueForInstance(f.inst, f.session, "")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(watching) before tick: %v", err)
	}
	if len(watchingBefore) != 1 {
		t.Fatalf("watching rows before tick: got %d, want 1", len(watchingBefore))
	}

	f.watcher.tick(context.Background())

	watchingAfter, err := f.db.MergeQueueForInstance(f.inst, f.session, "")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(watching) after tick: %v", err)
	}
	if len(watchingAfter) != 0 {
		t.Errorf("watching rows after tick: got %d, want 0 — `prism merges list` would still show the dead PR as watching", len(watchingAfter))
	}

	failedRows, err := f.db.MergeQueueForInstance(f.inst, f.session, "failed")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(failed): %v", err)
	}
	if len(failedRows) != 1 {
		t.Fatalf("failed rows after tick: got %d, want 1", len(failedRows))
	}
	if failedRows[0].PR != f.pr {
		t.Errorf("failed row PR: got %d, want %d", failedRows[0].PR, f.pr)
	}
	if failedRows[0].Error == nil || !strings.Contains(*failedRows[0].Error, "pr-gate") {
		t.Errorf("failed row error column: got %v, want text naming pr-gate", failedRows[0].Error)
	}
}

// TestWatcher_BLOCKED_RequiredCheckFailed_FiresOnce covers the at-most-once
// AC. The second tick must observe an empty queue head (the row is no longer
// watching) and therefore send nothing and call gh not at all.
func TestWatcher_BLOCKED_RequiredCheckFailed_FiresOnce(t *testing.T) {
	f := newBlockedTickFixture(t, 3520, []string{"pr-gate"}, blockedInfo("APPROVED", []checkEntry{
		{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
	}))

	f.watcher.tick(context.Background())
	callsAfterFirst := len(f.gh.calls)
	if f.srv.called != 1 {
		t.Fatalf("notifications after first tick: got %d, want 1", f.srv.called)
	}

	f.watcher.tick(context.Background())

	if f.srv.called != 1 {
		t.Errorf("notifications after second tick: got %d, want 1 — the failure transition must fire at most once per PR", f.srv.called)
	}
	if len(f.gh.calls) != callsAfterFirst {
		t.Errorf("gh calls after second tick: got %d, want %d — a terminated row must not be polled again",
			len(f.gh.calls), callsAfterFirst)
	}
}

// TestWatcher_BLOCKED_StillResolvable_StaysWatching covers the edge-case ACs
// through the production tick: every BLOCKED shape that can still resolve on
// its own must stay watching and stay silent.
func TestWatcher_BLOCKED_StillResolvable_StaysWatching(t *testing.T) {
	cases := []struct {
		name     string
		pr       int
		required []string
		info     prInfo
	}{
		{
			name:     "required_check_in_progress",
			pr:       3530,
			required: []string{"pr-gate"},
			info: blockedInfo("APPROVED", []checkEntry{
				{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
			}),
		},
		{
			name:     "required_check_queued",
			pr:       3531,
			required: []string{"pr-gate"},
			info: blockedInfo("APPROVED", []checkEntry{
				{Name: "pr-gate", Status: "QUEUED", Conclusion: ""},
			}),
		},
		{
			name:     "all_required_success_awaiting_approval",
			pr:       3532,
			required: []string{"pr-gate"},
			info: blockedInfo("REVIEW_REQUIRED", []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
			}),
		},
		{
			name:     "required_check_absent_from_rollup",
			pr:       3533,
			required: []string{"pr-gate"},
			info: blockedInfo("APPROVED", []checkEntry{
				{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "SUCCESS"},
			}),
		},
		{
			name:     "required_check_absent_while_sibling_failed",
			pr:       3534,
			required: []string{"pr-gate"},
			info: blockedInfo("APPROVED", []checkEntry{
				{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "FAILURE"},
			}),
		},
		{
			name:     "non_required_check_failing",
			pr:       3535,
			required: []string{"pr-gate"},
			info: blockedInfo("REVIEW_REQUIRED", []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "optional-lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			}),
		},
		{
			name:     "rerun_in_flight_after_failure",
			pr:       3536,
			required: []string{"pr-gate"},
			info: blockedInfo("APPROVED", []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
			}),
		},
		{
			name:     "empty_rollup_after_fresh_push",
			pr:       3537,
			required: []string{"pr-gate"},
			info:     blockedInfo("", nil),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newBlockedTickFixture(t, tc.pr, tc.required, tc.info)

			f.watcher.tick(context.Background())

			if got := f.status(t); got != "watching" {
				t.Errorf("status: got %q, want watching — this BLOCKED state can still resolve on its own", got)
			}
			if f.srv.called != 0 {
				t.Errorf("notifications sent: got %d, want 0 — a resolvable BLOCKED state must stay silent (#2420)", f.srv.called)
			}
			if f.gh.mergeAttempted() {
				t.Error("a merge mutation was attempted on a BLOCKED PR")
			}
		})
	}
}

// TestWatcher_BLOCKED_Unprotected_StaysWatching pins the interaction between
// #2525 and the #2420 branch-protection gate: with no protection configured
// there is no required-check set, so the failure transition can never fire
// and the watcher keeps waiting for a human.
func TestWatcher_BLOCKED_Unprotected_StaysWatching(t *testing.T) {
	const (
		session = "myrepo@main"
		repo    = "myrepo"
		pr      = 3540
		inst    = "inst-2525-unprotected"
	)

	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, session, inst, parsePort(t, srv.URL), "sid-2525-unprotected")
	if _, err := d.EnqueueMerge(pr, repo, session, inst, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	w := &Watcher{
		db:          d,
		instanceID:  inst,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "owner/myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 2 && args[0] == "api":
				// Both the classic and the ruleset endpoint 404 — genuinely
				// unprotected.
				return []byte("HTTP 404: Branch not protected"), fmt.Errorf("exit status 1")
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				body, _ := json.Marshal(blockedInfo("", []checkEntry{
					{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				}))
				return body, nil
			}
			t.Fatalf("unexpected gh call: %v", args)
			return nil, nil
		},
	}

	w.tick(context.Background())

	row, err := d.PendingMergeByPR(pr, repo)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching — an unprotected repo has no required-check set to fail on (#2420 gate precedes #2525)", row.Status)
	}
	if srv.called != 0 {
		t.Errorf("notifications sent: got %d, want 0", srv.called)
	}
}

// TestWatcher_BLOCKED_ProtectionFetchError_StaysWatching verifies the #2525
// path cannot fire when the branch-protection probe itself fails: without a
// trustworthy required-check list there is no way to know the set is
// accounted for.
func TestWatcher_BLOCKED_ProtectionFetchError_StaysWatching(t *testing.T) {
	const (
		session = "myrepo@main"
		repo    = "myrepo"
		pr      = 3550
		inst    = "inst-2525-proterr"
	)

	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, session, inst, parsePort(t, srv.URL), "sid-2525-proterr")
	if _, err := d.EnqueueMerge(pr, repo, session, inst, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	w := &Watcher{
		db:          d,
		instanceID:  inst,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "owner/myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 2 && args[0] == "api":
				return []byte("HTTP 503: upstream unavailable"), fmt.Errorf("exit status 1")
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				body, _ := json.Marshal(blockedInfo("APPROVED", []checkEntry{
					{Name: "pr-gate", Status: "COMPLETED", Conclusion: "FAILURE"},
				}))
				return body, nil
			}
			t.Fatalf("unexpected gh call: %v", args)
			return nil, nil
		},
	}

	w.tick(context.Background())

	row, err := d.PendingMergeByPR(pr, repo)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching — a failed protection probe must not license a terminal verdict", row.Status)
	}
	if srv.called != 0 {
		t.Errorf("notifications sent: got %d, want 0", srv.called)
	}
}

// TestWatcher_CLEAN_MergeConditionUnchanged is a guard for the AC that #2525
// introduces no new auto-merge path and does not alter the CLEAN condition.
// The BLOCKED discrimination must not leak into the merge decision.
func TestWatcher_CLEAN_MergeConditionUnchanged(t *testing.T) {
	const (
		session = "myrepo@main"
		repo    = "myrepo"
		pr      = 3560
		inst    = "inst-2525-clean"
	)

	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, session, inst, parsePort(t, srv.URL), "sid-2525-clean")
	if _, err := d.EnqueueMerge(pr, repo, session, inst, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	var mergeCalls int
	w := &Watcher{
		db:          d,
		instanceID:  inst,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "owner/myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
				return []byte(`{"required_status_checks":{"contexts":["pr-gate"]}}`), nil
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				body, _ := json.Marshal(prInfo{
					State:            "OPEN",
					MergeStateStatus: "CLEAN",
					ReviewDecision:   "APPROVED",
					StatusCheckRollup: []checkEntry{
						{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
					},
				})
				return body, nil
			case len(args) >= 4 && args[2] == "pr" && args[3] == "merge":
				mergeCalls++
				return nil, nil
			}
			t.Fatalf("unexpected gh call: %v", args)
			return nil, nil
		},
	}

	w.tick(context.Background())

	if mergeCalls != 1 {
		t.Errorf("gh pr merge invocations on CLEAN: got %d, want 1 — #2525 must not change the CLEAN merge condition", mergeCalls)
	}
	row, err := d.PendingMergeByPR(pr, repo)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged", row.Status)
	}
}

// Compile-time guard: the notification path must keep taking a context, so a
// future refactor cannot silently drop cancellation from the delivery call.
var _ func(*Watcher, context.Context, *db.PendingMerge, []string) = (*Watcher).notifyRequiredChecksFailed
