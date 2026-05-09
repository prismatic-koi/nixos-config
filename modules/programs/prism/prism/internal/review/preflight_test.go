package review

import (
	"errors"
	"fmt"
	"os"
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
type fakeGit struct {
	script map[string]scriptedResponse
	calls  []string
}

func newFakeGit() *fakeGit {
	return &fakeGit{script: map[string]scriptedResponse{}}
}

func (f *fakeGit) on(args string, resp scriptedResponse) *fakeGit {
	f.script[args] = resp
	return f
}

func (f *fakeGit) run(_ string, args ...string) (string, string, int, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	resp, ok := f.script[key]
	if !ok {
		return "", "", 1, fmt.Errorf("fakeGit: no script for: %s", key)
	}
	return resp.stdout, resp.stderr, resp.exitCode, resp.err
}

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
		on("push --force-with-lease", scriptedResponse{exitCode: 0}).
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
		"push --force-with-lease",
		"merge-base --is-ancestor origin/main HEAD",
	} {
		if !fg.called(want) {
			t.Errorf("Preflight (--rebase): expected %q to be called; calls=%v", want, fg.calls)
		}
	}
}

// TestPreflight_RebaseConflictAbortsAndRestores verifies that an inline-rebase
// conflict triggers `git rebase --abort` and a `reset --hard <origHead>` to
// restore the worktree, and returns a non-zero error instructing the worker to
// resolve manually. The worktree must NEVER be left mid-rebase.
func TestPreflight_RebaseConflictAbortsAndRestores(t *testing.T) {
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("rebase origin/main", scriptedResponse{
			stderr:   "CONFLICT (content): Merge conflict in foo.txt\nfatal: ...",
			exitCode: 1,
		}).
		on("rebase --abort", scriptedResponse{exitCode: 0}).
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
		"rebase aborted",
		"Resolve conflicts manually",
		"CONFLICT (content)",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("Preflight (--rebase conflict): message missing %q; got:\n%s", want, pe.Msg)
		}
	}
	// The abort + reset must have been issued so the worktree is not
	// left mid-rebase.
	if !fg.called("rebase --abort") {
		t.Errorf("Preflight (--rebase conflict): expected `rebase --abort`; calls=%v", fg.calls)
	}
	if !fg.called("reset --hard deadbeef") {
		t.Errorf("Preflight (--rebase conflict): expected `reset --hard deadbeef` to restore HEAD; calls=%v", fg.calls)
	}
	// Ancestor check must NOT have been called after a conflict abort —
	// the gate exits before reaching that step.
	if fg.called("merge-base --is-ancestor origin/main HEAD") {
		t.Errorf("Preflight (--rebase conflict): merge-base must not run after conflict abort; calls=%v", fg.calls)
	}
}

// TestPreflight_NoIncrementsCounter is the explicit no-counter-increment test
// required by the AC and by issue #1518. It exercises the contract that a
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
				on("rebase --abort", scriptedResponse{exitCode: 0}).
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

// ── integration test against a real local git repo ────────────────────────────

// TestPreflight_RealGit_AncestorPasses creates a real local git repo with two
// branches (main and feature), where feature is up to date with main, and
// verifies Preflight passes against the real git binary. This exercises the
// `realGit` runner end-to-end (the unit tests above use the fake runner).
func TestPreflight_RealGit_AncestorPasses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	worktree, _ := setupRealRepo(t)

	if err := Preflight(PreflightOpts{Worktree: worktree}); err != nil {
		t.Fatalf("Preflight: unexpected error on up-to-date branch: %v", err)
	}
}

// TestPreflight_RealGit_BehindMainRefuses creates a real repo where main has
// advanced past the feature branch (feature does not contain main's tip) and
// verifies Preflight returns a Refused error.
func TestPreflight_RealGit_BehindMainRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	worktree, advanceMain := setupRealRepo(t)
	advanceMain(t, "extra commit on main")

	err := Preflight(PreflightOpts{Worktree: worktree})
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

// setupRealRepo creates a bare "remote" plus a local clone, makes an initial
// commit on main, branches a feature, and checks out feature. It returns the
// feature worktree path and a helper that advances main on the bare remote so
// that origin/main moves past feature after `git fetch origin main`.
//
// The helper also takes care of `t.TempDir()` cleanup; callers should not
// inspect the returned remote.
func setupRealRepo(t *testing.T) (worktree string, advanceMain func(t *testing.T, msg string)) {
	t.Helper()

	// Don't let any user-level git config (e.g. signing) interfere.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	mustGit(t, "", "init", "--bare", "-b", "main", bare)

	// Local clone: this is our "feature" worktree.
	worktree = filepath.Join(root, "local")
	mustGit(t, "", "clone", bare, worktree)
	mustGit(t, worktree, "config", "user.email", "test@example.com")
	mustGit(t, worktree, "config", "user.name", "Test")
	mustGit(t, worktree, "config", "commit.gpgsign", "false")
	mustGit(t, worktree, "config", "tag.gpgsign", "false")

	// Initial commit on main.
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, worktree, "add", ".")
	mustGit(t, worktree, "commit", "-m", "init")
	mustGit(t, worktree, "push", "origin", "main")

	// Branch off as `feature`. The clone's HEAD is now `feature` but
	// origin/main is at the same commit (so the gate passes initially).
	mustGit(t, worktree, "checkout", "-b", "feature")

	// advanceMain: append a commit to main on the bare remote so that
	// `git fetch origin main` from the worktree pulls a new origin/main
	// that is NOT an ancestor of the feature HEAD.
	advanceMain = func(t *testing.T, msg string) {
		t.Helper()

		// Use a separate scratch worktree on the bare remote to push from.
		scratch := filepath.Join(root, "scratch-"+msg)
		mustGit(t, "", "clone", bare, scratch)
		mustGit(t, scratch, "config", "user.email", "test@example.com")
		mustGit(t, scratch, "config", "user.name", "Test")
		mustGit(t, scratch, "config", "commit.gpgsign", "false")
		mustGit(t, scratch, "checkout", "main")
		if err := os.WriteFile(filepath.Join(scratch, "advance.txt"), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatalf("write advance: %v", err)
		}
		mustGit(t, scratch, "add", ".")
		mustGit(t, scratch, "commit", "-m", msg)
		mustGit(t, scratch, "push", "origin", "main")
	}
	return worktree, advanceMain
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
