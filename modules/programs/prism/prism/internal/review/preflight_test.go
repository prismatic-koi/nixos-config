package review

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// ── fake gitRunner: scripted responses keyed by argv ──────────────────────────

// scriptedResponse is one entry in the fakeGit script.
type scriptedResponse struct {
	stdout   string
	stderr   string
	exitCode int
	err      error
}

// fakeGit is a gitRunner that returns scripted responses based on the args
// (joined with spaces) it receives. Calls without a registered script entry
// return a "no script for: ..." error so missing setup is loud.
//
// The metadata fields model a pinned prism worktree by default: isWorktree
// true, sessionBranch "feat", and an empty gitDir so the real-filesystem
// state check and conflict-restore dir removal are no-ops in unit tests. The
// real-filesystem behaviour is covered in preflight_host_git_test.go.
type fakeGit struct {
	script      map[string]scriptedResponse
	calls       []string
	branch      string
	dir         string
	notWorktree bool
}

func newFakeGit() *fakeGit {
	return &fakeGit{script: map[string]scriptedResponse{}, branch: "feat"}
}

func (f *fakeGit) on(args string, resp scriptedResponse) *fakeGit {
	f.script[args] = resp
	return f
}

func (f *fakeGit) run(args ...string) (string, string, int, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if resp, ok := f.script[key]; ok {
		return resp.stdout, resp.stderr, resp.exitCode, resp.err
	}
	// Default: HEAD is on the session branch, so the --rebase guard passes
	// unless a test scripts a different symbolic-ref answer.
	if key == "symbolic-ref -q HEAD" {
		return "refs/heads/" + f.branch + "\n", "", 0, nil
	}
	return "", "", 1, fmt.Errorf("fakeGit: no script for: %s", key)
}

func (f *fakeGit) gitDir() string        { return f.dir }
func (f *fakeGit) isWorktree() bool      { return !f.notWorktree }
func (f *fakeGit) sessionBranch() string { return f.branch }

func (f *fakeGit) called(args string) bool {
	for _, c := range f.calls {
		if c == args {
			return true
		}
	}
	return false
}

// ── unit tests against fakeGit ────────────────────────────────────────────────

// TestPreflight_AncestorPasses verifies that when origin/main is an ancestor
// of HEAD, Preflight returns nil (review may proceed).
func TestPreflight_AncestorPasses(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
	})
	if err != nil {
		t.Fatalf("Preflight: unexpected error: %v", err)
	}
}

// TestPreflight_AncestorFailsRefuses verifies that when origin/main is NOT an
// ancestor of HEAD, Preflight returns a *PreflightError with Refused=true and
// a message naming the commits-behind count and the rebase invocation.
func TestPreflight_AncestorFailsRefuses(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 1}).
		on("rev-list --count HEAD..origin/main", scriptedResponse{stdout: "4\n", exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight: expected error for behind-main branch, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if !pe.Refused {
		t.Errorf("Preflight: expected Refused=true; got %+v", pe)
	}
	if pe.CommitsBehind != 4 {
		t.Errorf("Preflight: expected CommitsBehind=4; got %d", pe.CommitsBehind)
	}
	for _, want := range []string{
		"4 commits",
		"behind origin/main",
		"git rebase origin/main",
		"--rebase",
		"git push --force-with-lease",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight: refusal message missing %q; got:\n%s", want, pe.Msg)
		}
	}
}

// TestPreflight_FetchFailureExitsNonZero verifies that a fetch failure exits
// non-zero with a "could not verify branch is up to date" message and the
// underlying error. No fallback to running the review against an unverified
// branch.
func TestPreflight_FetchFailureExitsNonZero(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{
			stderr:   "fatal: unable to access 'https://example.com/repo.git/': Could not resolve host: example.com",
			exitCode: 128,
		})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight: expected fetch-failure error, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if pe.Refused {
		t.Errorf("Preflight: fetch-failure must not be Refused=true; got %+v", pe)
	}
	for _, want := range []string{
		"could not verify branch is up to date",
		"git fetch origin main failed",
		"Could not resolve host",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight: fetch-failure message missing %q; got:\n%s", want, pe.Msg)
		}
	}
	// Ancestor check must NOT have been called when fetch failed.
	if fg.called("merge-base --is-ancestor origin/main HEAD") {
		t.Error("Preflight: merge-base must not run when fetch fails")
	}
}

// TestPreflight_NoOriginMainConfigured verifies that when the remote ref does
// not exist after fetch (e.g. local-only setup with no origin/main), the gate
// exits non-zero with a clear message instructing the user to configure origin.
func TestPreflight_NoOriginMainConfigured(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}). // fetch is silently a no-op
		on("rev-parse --verify origin/main", scriptedResponse{
			stderr:   "fatal: Needed a single revision",
			exitCode: 128,
		})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight: expected missing-upstream error, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if pe.Refused {
		t.Errorf("Preflight: missing-upstream must not be Refused=true; got %+v", pe)
	}
	for _, want := range []string{
		"no origin/main ref configured",
		"git remote add origin",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight: missing-upstream message missing %q; got:\n%s", want, pe.Msg)
		}
	}
}

// TestPreflight_RebaseHappyPath verifies that --rebase runs fetch + rebase +
// push --force-with-lease and that the ancestor check runs against the
// (now-rebased) HEAD.
func TestPreflight_RebaseHappyPath(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/main", scriptedResponse{exitCode: 0}).
		on("push --force-with-lease origin refs/heads/feat:refs/heads/feat", scriptedResponse{exitCode: 0}).
		on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Rebase:    true,
		gitRunner: fg,
	})
	if err != nil {
		t.Fatalf("Preflight (--rebase happy path): unexpected error: %v", err)
	}
	for _, want := range []string{
		"fetch origin main",
		"rebase origin/main",
		"push --force-with-lease origin refs/heads/feat:refs/heads/feat",
		"merge-base --is-ancestor origin/main HEAD",
	} {
		if !fg.called(want) {
			t.Errorf("Preflight (--rebase): expected %q to be called; calls=%v", want, fg.calls)
		}
	}
	// The push must name the session branch explicitly, never a bare
	// current-branch push that follows HEAD (#3323, AC4).
	if fg.called("push --force-with-lease") {
		t.Errorf("Preflight (--rebase): bare `push --force-with-lease` must not run; calls=%v", fg.calls)
	}
}

// TestPreflight_RebaseConflictRestoresWithoutStash verifies that an
// inline-rebase conflict restores the worktree WITHOUT `git rebase --abort`
// (#3323): the gate re-attaches HEAD to the session branch and resets to the
// original HEAD, and returns a non-zero error telling the worker to resolve
// manually. `git rebase --abort` must never run, because it would apply a
// rebase-merge/autostash file the session can plant. The worktree must never
// be left mid-rebase.
func TestPreflight_RebaseConflictRestoresWithoutStash(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/main", scriptedResponse{
			stderr:   "CONFLICT (content): Merge conflict in foo.txt\nfatal: ...",
			exitCode: 1,
		}).
		on("symbolic-ref HEAD refs/heads/feat", scriptedResponse{exitCode: 0}).
		on("reset --hard deadbeef", scriptedResponse{exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Rebase:    true,
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight (--rebase conflict): expected error, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight (--rebase conflict): error is not *PreflightError: %T %v", err, err)
	}
	if pe.Refused {
		t.Errorf("Preflight (--rebase conflict): must not be Refused=true; got %+v", pe)
	}
	for _, want := range []string{
		"--rebase produced conflicts",
		"restored to its original HEAD",
		"Resolve conflicts manually",
		"CONFLICT (content)",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight (--rebase conflict): message missing %q; got:\n%s", want, pe.Msg)
		}
	}
	// The stash-free restore must have run: re-attach HEAD to the session
	// branch, then reset to the original HEAD.
	if !fg.called("symbolic-ref HEAD refs/heads/feat") {
		t.Errorf("Preflight (--rebase conflict): expected `symbolic-ref HEAD refs/heads/feat`; calls=%v", fg.calls)
	}
	if !fg.called("reset --hard deadbeef") {
		t.Errorf("Preflight (--rebase conflict): expected `reset --hard deadbeef`; calls=%v", fg.calls)
	}
	// `git rebase --abort` must NEVER run: it applies rebase-merge/autostash.
	if fg.called("rebase --abort") {
		t.Errorf("Preflight (--rebase conflict): `rebase --abort` must not run (#3323); calls=%v", fg.calls)
	}
	// Ancestor check must NOT have been called after a conflict — the gate
	// exits before reaching that step.
	if fg.called("merge-base --is-ancestor origin/main HEAD") {
		t.Errorf("Preflight (--rebase conflict): merge-base must not run after conflict; calls=%v", fg.calls)
	}
}

// TestPreflight_NoIncrementsCounter is the explicit no-counter-increment test.
// It exercises the contract that a
// worker hitting the gate three times in a row (refusal, fetch failure,
// conflict abort) and then running three real reviews must still have all
// three real cycles available before the LOOP-LIMIT fires.
//
// The gate is implemented in Preflight, which runs BEFORE RunAsync writes
// any per-agent session rows for a new round. The cycle counter
// (NextRoundNumber) is derived purely from `<parent>~review-<N>-<agent>`
// rows in agent_status — so the structural property is: as long as
// Preflight does not write any such rows, the counter cannot move. This
// test asserts that property by:
//
//  1. running Preflight three times against scripted gate failures, and
//  2. verifying NextRoundNumber for the parent session still returns 1.
//
// This guards against a future refactor that, e.g., starts seeding round
// rows from inside the gate or shares numbering with NextRoundNumber.
func TestPreflight_NoIncrementsCounter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	parent := "test-repo@feature"

	// Sanity: counter starts at 1 (no prior rounds).
	if got := NextRoundNumber(d, parent); got != 1 {
		t.Fatalf("NextRoundNumber baseline: got %d, want 1", got)
	}

	cases := []struct {
		name string
		fg   *fakeGit
		opts PreflightOpts
	}{
		{
			name: "behind-main refusal",
			fg: newFakeGit().
				on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
				on("fetch origin main", scriptedResponse{exitCode: 0}).
				on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc\n", exitCode: 0}).
				on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 1}).
				on("rev-list --count HEAD..origin/main", scriptedResponse{stdout: "2\n", exitCode: 0}),
		},
		{
			name: "fetch failure",
			fg: newFakeGit().
				on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
				on("fetch origin main", scriptedResponse{stderr: "fatal: net", exitCode: 128}),
		},
		{
			name: "rebase conflict abort",
			fg: newFakeGit().
				on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
				on("fetch origin main", scriptedResponse{exitCode: 0}).
				on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc\n", exitCode: 0}).
				on("rebase origin/main", scriptedResponse{stderr: "CONFLICT", exitCode: 1}).
				on("symbolic-ref HEAD refs/heads/feat", scriptedResponse{exitCode: 0}).
				on("reset --hard deadbeef", scriptedResponse{exitCode: 0}),
		},
	}

	for _, tc := range cases {
		opts := tc.opts
		opts.Worktree = "/fake/worktree"
		opts.gitRunner = tc.fg
		// Conflict-abort case requires Rebase=true to take that branch.
		if tc.name == "rebase conflict abort" {
			opts.Rebase = true
		}
		if err := Preflight(opts); err == nil {
			t.Errorf("%s: expected gate error, got nil", tc.name)
		}
		// After every gate failure, the counter must still be 1 — no
		// per-agent session rows have been written.
		if got := NextRoundNumber(d, parent); got != 1 {
			t.Errorf("%s: NextRoundNumber after gate failure: got %d, want 1 (gate must not advance counter)", tc.name, got)
		}
	}

	// Final assertion: three gate failures back to back must leave the
	// counter exactly where it started.
	if got := NextRoundNumber(d, parent); got != 1 {
		t.Fatalf("after 3 gate failures: NextRoundNumber=%d, want 1 (gate failures must not consume cycles)", got)
	}
}

// TestPreflight_RebasePushesSessionBranchNotHead verifies that the --rebase
// push targets the branch of the session record through an explicit
// refs/heads/<branch> refspec, never a bare `git push` that would follow the
// session-controlled HEAD (#3323, AC4). sessionBranch is derived from the
// worktree path, so the fake returns it directly.
func TestPreflight_RebasePushesSessionBranchNotHead(t *testing.T) {
	fg := newFakeGit()
	fg.branch = "work-branch"
	fg.on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/main", scriptedResponse{exitCode: 0}).
		on("push --force-with-lease origin refs/heads/work-branch:refs/heads/work-branch", scriptedResponse{exitCode: 0}).
		on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 0})

	if err := Preflight(PreflightOpts{Worktree: "/fake/worktree", Rebase: true, gitRunner: fg}); err != nil {
		t.Fatalf("Preflight: unexpected error: %v", err)
	}
	if !fg.called("push --force-with-lease origin refs/heads/work-branch:refs/heads/work-branch") {
		t.Errorf("expected a push of the session-branch refspec; calls=%v", fg.calls)
	}
	if fg.called("push --force-with-lease") {
		t.Errorf("a bare `push --force-with-lease` (follows HEAD) must not run; calls=%v", fg.calls)
	}
}

// TestPreflight_NonWorktreeRefused verifies that Preflight refuses a runner
// that reports it is not a prism worktree, before any git command runs
// (#3323, AC1).
func TestPreflight_NonWorktreeRefused(t *testing.T) {
	fg := newFakeGit()
	fg.notWorktree = true
	err := Preflight(PreflightOpts{Worktree: "/fake/worktree", Rebase: true, gitRunner: fg})
	if err == nil || !strings.Contains(err.Error(), "not a prism worktree") {
		t.Fatalf("Preflight = %v, want a refusal naming 'not a prism worktree'", err)
	}
	if len(fg.calls) != 0 {
		t.Errorf("no git command must run for a non-worktree session; calls=%v", fg.calls)
	}
}

// TestPreflight_RebaseRefusedWhenHeadOffSessionBranch verifies that --rebase
// is refused when HEAD is not on the session branch, before the rebase runs
// (review round 1 on PR #3332). Otherwise the rebase acts on one branch while
// the push and restore act on another.
func TestPreflight_RebaseRefusedWhenHeadOffSessionBranch(t *testing.T) {
	fg := newFakeGit() // sessionBranch "feat"
	fg.on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("symbolic-ref -q HEAD", scriptedResponse{stdout: "refs/heads/feat-v2\n", exitCode: 0})

	err := Preflight(PreflightOpts{Worktree: "/fake/worktree", Rebase: true, gitRunner: fg})
	if err == nil || !strings.Contains(err.Error(), "not on the session branch") {
		t.Fatalf("Preflight = %v, want a refusal naming the session branch", err)
	}
	if fg.called("rebase origin/main") || fg.called("push --force-with-lease origin refs/heads/feat:refs/heads/feat") {
		t.Errorf("no rebase or push must run when HEAD is off the session branch; calls=%v", fg.calls)
	}
}

// TestPreflight_RebaseRefusedWhenNoSessionBranch verifies that --rebase fails
// closed when the session branch cannot be derived — no bare push that would
// follow HEAD (review round 1 on PR #3332).
func TestPreflight_RebaseRefusedWhenNoSessionBranch(t *testing.T) {
	fg := newFakeGit()
	fg.branch = ""
	fg.on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0})

	err := Preflight(PreflightOpts{Worktree: "/fake/worktree", Rebase: true, gitRunner: fg})
	if err == nil || !strings.Contains(err.Error(), "cannot determine the branch") {
		t.Fatalf("Preflight = %v, want a refusal that the branch cannot be determined", err)
	}
	if fg.called("push --force-with-lease") {
		t.Errorf("a bare `push --force-with-lease` must not run; calls=%v", fg.calls)
	}
}

// TestPreflight_WorktreeRequired verifies that Preflight refuses to run with
// an empty worktree path (defence in depth — callers should always supply one).
func TestPreflight_WorktreeRequired(t *testing.T) {
	err := Preflight(PreflightOpts{})
	if err == nil {
		t.Fatal("Preflight: expected error for empty worktree, got nil")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Errorf("Preflight: empty-worktree error missing 'worktree': %v", err)
	}
}

// ── base-ref resolver tests ───────────────────────────────────────────

// fakeBaseRefRunner is a scripted baseRefRunner for unit-testing
// resolvePRBaseRefWithRunner. It captures the PR numbers it was called with
// so tests can assert the resolver did (or didn't) actually invoke gh.
type fakeBaseRefRunner struct {
	stdout string
	err    error
	calls  []string
}

func (f *fakeBaseRefRunner) run(prNumber string) (string, error) {
	f.calls = append(f.calls, prNumber)
	return f.stdout, f.err
}

// TestResolvePRBaseRef_Success verifies that the resolver returns the
// baseRefName from a successful gh response. This is the happy path that
// drives a PR-aware base-branch in the rebase gate.
func TestResolvePRBaseRef_Success(t *testing.T) {
	fr := &fakeBaseRefRunner{
		stdout: `{"baseRefName":"eks-pipeline"}` + "\n",
	}
	got := resolvePRBaseRefWithRunner("1234", fr)
	if got != "eks-pipeline" {
		t.Errorf("ResolvePRBaseRef: got %q, want %q", got, "eks-pipeline")
	}
	if len(fr.calls) != 1 || fr.calls[0] != "1234" {
		t.Errorf("ResolvePRBaseRef: expected one gh call with PR %q; got %v", "1234", fr.calls)
	}
}

// TestResolvePRBaseRef_SuccessMainBase verifies the main-targeting case
// returns "main". Ensures the resolver does not collapse "main" to "" — the
// preflight default mechanism handles the "" → "main" fallback separately.
func TestResolvePRBaseRef_SuccessMainBase(t *testing.T) {
	fr := &fakeBaseRefRunner{stdout: `{"baseRefName":"main"}`}
	got := resolvePRBaseRefWithRunner("42", fr)
	if got != "main" {
		t.Errorf("ResolvePRBaseRef: got %q, want %q", got, "main")
	}
}

// TestResolvePRBaseRef_GHFailureFallsBack verifies the silent-fallback
// contract: a gh failure (network error, unauthenticated, PR not found, gh
// missing) collapses to "" so the caller defaults to "main". Preflight falls
// back silently to origin/main without a scary warning.
func TestResolvePRBaseRef_GHFailureFallsBack(t *testing.T) {
	fr := &fakeBaseRefRunner{err: errors.New("gh: Could not resolve to a PullRequest with the number of 9999")}
	got := resolvePRBaseRefWithRunner("9999", fr)
	if got != "" {
		t.Errorf("ResolvePRBaseRef: expected \"\" on gh failure; got %q", got)
	}
}

// TestResolvePRBaseRef_EmptyBaseFallsBack verifies that gh succeeding with
// an empty baseRefName is treated the same as a lookup failure — an empty
// baseRefName falls back to origin/main.
func TestResolvePRBaseRef_EmptyBaseFallsBack(t *testing.T) {
	fr := &fakeBaseRefRunner{stdout: `{"baseRefName":""}`}
	got := resolvePRBaseRefWithRunner("7", fr)
	if got != "" {
		t.Errorf("ResolvePRBaseRef: expected \"\" on empty baseRefName; got %q", got)
	}
}

// TestResolvePRBaseRef_MalformedJSONFallsBack verifies that an unparseable gh
// response is treated as a lookup failure. Defends against a future gh output
// schema change silently producing the wrong base ref.
func TestResolvePRBaseRef_MalformedJSONFallsBack(t *testing.T) {
	fr := &fakeBaseRefRunner{stdout: `not json`}
	got := resolvePRBaseRefWithRunner("7", fr)
	if got != "" {
		t.Errorf("ResolvePRBaseRef: expected \"\" on malformed JSON; got %q", got)
	}
}

// TestResolvePRBaseRef_EmptyPRNumberSkipsGH verifies that an empty PR number
// does not invoke gh at all. Defence in depth — guards against `prism review`
// invocations that somehow forwarded an empty positional arg.
func TestResolvePRBaseRef_EmptyPRNumberSkipsGH(t *testing.T) {
	fr := &fakeBaseRefRunner{stdout: `{"baseRefName":"main"}`}
	got := resolvePRBaseRefWithRunner("", fr)
	if got != "" {
		t.Errorf("ResolvePRBaseRef: expected \"\" for empty PR; got %q", got)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ResolvePRBaseRef: expected no gh call for empty PR; got %v", fr.calls)
	}
}

// ── Preflight tests against a non-main base branch ────────────────────

// TestPreflight_NonMainBaseRefusalNamesResolvedRef verifies that when the
// caller supplies a non-main Branch, the refusal message names the resolved
// ref in the "N commits behind …" line, the "<base> has advanced" line, and
// in the suggested `git fetch` / `git rebase` commands. The hardcoded
// origin/main must NOT appear.
func TestPreflight_NonMainBaseRefusalNamesResolvedRef(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin eks-pipeline", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/eks-pipeline", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("merge-base --is-ancestor origin/eks-pipeline HEAD", scriptedResponse{exitCode: 1}).
		on("rev-list --count HEAD..origin/eks-pipeline", scriptedResponse{stdout: "3\n", exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Branch:    "eks-pipeline",
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight: expected refusal for behind-base branch, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if !pe.Refused {
		t.Errorf("Preflight: expected Refused=true; got %+v", pe)
	}
	if pe.CommitsBehind != 3 {
		t.Errorf("Preflight: expected CommitsBehind=3; got %d", pe.CommitsBehind)
	}
	for _, want := range []string{
		"3 commits",
		"behind origin/eks-pipeline",
		"eks-pipeline has advanced",
		"git fetch origin eks-pipeline",
		"git rebase origin/eks-pipeline",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight: non-main refusal message missing %q; got:\n%s", want, pe.Msg)
		}
	}
	// Defence in depth: the message must NOT reference origin/main when the
	// resolved base is something else. A leaked "origin/main" or stray "main"
	// reference is the footgun.
	for _, mustNot := range []string{
		"origin/main",
		"behind main",
		"git rebase origin/main",
	} {
		if strings.Contains(pe.Msg, mustNot) {
			t.Errorf("Preflight: non-main refusal message must not contain %q; got:\n%s", mustNot, pe.Msg)
		}
	}
	// Sanity: the ancestor check must have run against the resolved ref, not
	// against the hardcoded origin/main.
	if !fg.called("merge-base --is-ancestor origin/eks-pipeline HEAD") {
		t.Errorf("Preflight: expected merge-base against origin/eks-pipeline; calls=%v", fg.calls)
	}
	if fg.called("merge-base --is-ancestor origin/main HEAD") {
		t.Errorf("Preflight: merge-base against origin/main must NOT run when Branch=eks-pipeline; calls=%v", fg.calls)
	}
}

// TestPreflight_NonMainBaseRebaseTargetsResolvedRef verifies that --rebase
// against a PR with a non-main base rebases onto the resolved ref (not
// origin/main) and force-pushes the result. This is the footgun: a
// rebase onto the wrong base silently pulls in unrelated commits and inflates
// the PR's apparent diff.
func TestPreflight_NonMainBaseRebaseTargetsResolvedRef(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin eks-pipeline", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/eks-pipeline", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/eks-pipeline", scriptedResponse{exitCode: 0}).
		on("push --force-with-lease origin refs/heads/feat:refs/heads/feat", scriptedResponse{exitCode: 0}).
		on("merge-base --is-ancestor origin/eks-pipeline HEAD", scriptedResponse{exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Branch:    "eks-pipeline",
		Rebase:    true,
		gitRunner: fg,
	})
	if err != nil {
		t.Fatalf("Preflight (--rebase, non-main base): unexpected error: %v", err)
	}
	for _, want := range []string{
		"fetch origin eks-pipeline",
		"rebase origin/eks-pipeline",
		"push --force-with-lease origin refs/heads/feat:refs/heads/feat",
		"merge-base --is-ancestor origin/eks-pipeline HEAD",
	} {
		if !fg.called(want) {
			t.Errorf("Preflight (--rebase, non-main): expected %q to be called; calls=%v", want, fg.calls)
		}
	}
	// Defence in depth: a rebase against origin/main must NEVER fire when
	// the PR's base is something else — this is the silent-footgun class
	// the issue calls out.
	for _, mustNot := range []string{
		"fetch origin main",
		"rebase origin/main",
		"merge-base --is-ancestor origin/main HEAD",
	} {
		if fg.called(mustNot) {
			t.Errorf("Preflight (--rebase, non-main): origin/main op %q must NOT run when Branch=eks-pipeline; calls=%v", mustNot, fg.calls)
		}
	}
}

// TestPreflight_NonMainBaseMissingUpstreamSurfacesResolvedRef verifies that
// when the resolved base ref does not exist after fetch (e.g. base branch
// deleted upstream), the missing-upstream error names the resolved ref —
// preflight does NOT silently retry against main.
func TestPreflight_NonMainBaseMissingUpstreamSurfacesResolvedRef(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin eks-pipeline", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/eks-pipeline", scriptedResponse{
			stderr:   "fatal: Needed a single revision",
			exitCode: 128,
		})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Branch:    "eks-pipeline",
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight: expected missing-upstream error for non-main base, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if pe.Refused {
		t.Errorf("Preflight: missing-upstream (non-main) must not be Refused=true; got %+v", pe)
	}
	for _, want := range []string{
		"no origin/eks-pipeline ref configured",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight: missing-upstream (non-main) message missing %q; got:\n%s", want, pe.Msg)
		}
	}
	// Defence in depth: must not silently retry against main.
	if fg.called("fetch origin main") || fg.called("rev-parse --verify origin/main") {
		t.Errorf("Preflight: missing-upstream (non-main) must NOT retry against main; calls=%v", fg.calls)
	}
}

// TestPreflight_NonMainBaseRebaseConflictAbortsAndRestores verifies the
// rebase-conflict abort/restore guarantee holds for non-main bases — the
// worktree is never left mid-rebase, mirroring the same-restoration
// guarantee the gate provides against main.
func TestPreflight_NonMainBaseRebaseConflictAbortsAndRestores(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin eks-pipeline", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/eks-pipeline", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/eks-pipeline", scriptedResponse{
			stderr:   "CONFLICT (content): Merge conflict in foo.txt",
			exitCode: 1,
		}).
		on("symbolic-ref HEAD refs/heads/feat", scriptedResponse{exitCode: 0}).
		on("reset --hard deadbeef", scriptedResponse{exitCode: 0})

	err := Preflight(PreflightOpts{
		Worktree:  "/fake/worktree",
		Branch:    "eks-pipeline",
		Rebase:    true,
		gitRunner: fg,
	})
	if err == nil {
		t.Fatal("Preflight (--rebase conflict, non-main): expected error, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if pe.Refused {
		t.Errorf("Preflight (--rebase conflict, non-main): must not be Refused=true; got %+v", pe)
	}
	if !fg.called("symbolic-ref HEAD refs/heads/feat") {
		t.Errorf("Preflight (--rebase conflict, non-main): expected `symbolic-ref HEAD refs/heads/feat`; calls=%v", fg.calls)
	}
	if !fg.called("reset --hard deadbeef") {
		t.Errorf("Preflight (--rebase conflict, non-main): expected `reset --hard deadbeef`; calls=%v", fg.calls)
	}
	if fg.called("rebase --abort") {
		t.Errorf("Preflight (--rebase conflict, non-main): `rebase --abort` must not run (#3323); calls=%v", fg.calls)
	}
	// Recovery commands must name the resolved base, not origin/main.
	for _, want := range []string{
		"git fetch origin eks-pipeline",
		"git rebase origin/eks-pipeline",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight (--rebase conflict, non-main): recovery message missing %q; got:\n%s", want, pe.Msg)
		}
	}
}

// ── integration test against a real local git repo ────────────────────────────

// TestPreflight_RealGit_AncestorPasses verifies Preflight passes against the
// real git binary when the worker worktree already contains origin/main. It
// drives the realGit runner end-to-end on a prism bare+worktree layout
// (newPreflightLayout, in preflight_host_git_test.go). A plain clone is no
// longer a valid target: Preflight refuses a session that is not in a prism
// worktree (#3323, AC1).
func TestPreflight_RealGit_AncestorPasses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	l := newPreflightLayout(t)

	if err := Preflight(PreflightOpts{Worktree: l.worker}); err != nil {
		t.Fatalf("Preflight: unexpected error on up-to-date branch: %v", err)
	}
}

// TestPreflight_RealGit_BehindMainRefuses advances origin/main past the worker
// branch and verifies Preflight returns a Refused error.
func TestPreflight_RealGit_BehindMainRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	l := newPreflightLayout(t)
	l.advance(t)

	err := Preflight(PreflightOpts{Worktree: l.worker})
	if err == nil {
		t.Fatal("Preflight: expected refusal when main has advanced, got nil")
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Preflight: error is not *PreflightError: %T %v", err, err)
	}
	if !pe.Refused {
		t.Errorf("Preflight: expected Refused=true; got %+v", pe)
	}
	if pe.CommitsBehind < 1 {
		t.Errorf("Preflight: expected CommitsBehind >= 1; got %d", pe.CommitsBehind)
	}
}

// mustGit runs git with the given args in cwd (or the default cwd if empty),
// failing the test if it returns non-zero.
func mustGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
