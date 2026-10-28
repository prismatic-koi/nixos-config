package review

// preflight.go — pre-flight rebase gate for `prism review`.
//
// Reviews regularly produce noisy findings of the form "you should also
// update X" when X landed on the base branch after the feature branch was
// cut. A pre-flight ancestor check on the PR's base ref catches this in one
// fetch, before any agent spawns, and either refuses the review or (with
// --rebase) fixes it inline.
//
// The base branch is configurable via PreflightOpts.Branch and is
// resolved at the call site from `gh pr view --json baseRefName` so PRs
// targeting non-main bases are checked against the correct upstream. The
// default remains "main" when no branch is supplied — this preserves
// behaviour for invocations not tied to a discoverable PR.
//
// The gate is a snapshot at review-spawn time. It is not continuous — the base
// can advance during a review run; we do not chase that. This is consistent
// with how CI works.
//
// Strict ancestor check via `git merge-base --is-ancestor <remote>/<base> HEAD`.
// No "files-touched-in-common" / loose variant — that breaks on renames,
// deletes, and cross-cutting helper introductions and gives different verdicts
// in equivalent situations.
//
// Gate failures (refusal, fetch failure, conflict abort) MUST NOT increment the
// review-cycle counter. The counter is computed by NextRoundNumber from
// ~review-<N>-<agent> session rows in the DB; since the gate runs before
// RunAsync spawns any agent sessions, no DB rows are created on a gate
// failure and the counter is structurally unaffected.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sessionname"
)

// PreflightOpts configures the rebase gate.
type PreflightOpts struct {
	// Worktree is the absolute path to the git worktree. Required.
	Worktree string
	// Rebase, when true, performs fetch + rebase + force-push inline before
	// the ancestor check. On rebase conflict the worktree is restored to the
	// original HEAD without `git rebase --abort` (which would apply a planted
	// autostash — #3323), and the gate returns an error — never leaving the
	// worktree mid-rebase.
	Rebase bool
	// Remote is the git remote to fetch from. Defaults to "origin".
	Remote string
	// Branch is the upstream branch to check against. Defaults to "main".
	Branch string
	// OnProgress is an optional callback invoked for each progress event
	// (fetch starting, rebase starting, etc.). When nil, progress is silent.
	OnProgress func(line string)
	// gitRunner is an injectable runner for tests; nil = real git.
	gitRunner gitRunner
}

// gitRunner is the test seam for running git and for inspecting the repo
// that prism resolved on the host. The metadata methods let the pre-flight
// refuse a session that is not in a prism worktree and restore after a
// conflict without trusting state that the session can write.
type gitRunner interface {
	run(args ...string) (stdout string, stderr string, exitCode int, err error)
	// gitDir is the resolved git dir, for the pre-existing-state check and
	// the conflict restore. "" when the runner has no real repo (a test fake
	// that drives git by scripted argv).
	gitDir() string
	// isWorktree reports whether the resolved repo is a pinned prism
	// worktree. Preflight refuses when it is false (#3323).
	isWorktree() bool
	// sessionBranch is the branch of the session record — the path of the
	// worktree below the project root — never the branch that HEAD names,
	// which the session can write in its own git dir. "" when it cannot be
	// derived.
	sessionBranch() string
}

// realGit runs git through a git.Repo that prism resolved on the host with
// git.OpenDir: it pins the git dir and the common dir and does not trust
// <worktree>/.git or a planted commondir. The repo is resolved once, in
// Preflight, so every command and every metadata read sees one resolution.
// See docs/invariants/host-git-commands.md.
type realGit struct{ repo git.Repo }

func (g realGit) run(args ...string) (string, string, int, error) {
	cmd := g.repo.Command(context.Background(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			// Treat non-zero exit as a structured outcome rather than an
			// error — callers inspect exitCode for git's pass/fail signal.
			err = nil
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func (g realGit) gitDir() string   { return g.repo.GitDir() }
func (g realGit) isWorktree() bool { return g.repo.Pinned() && g.repo.WorkTree() != "" }

// sessionBranch derives the branch from the worktree path below the project
// root, as container.resolveGitWriteScope does. It never reads HEAD: a
// session can write HEAD in its own git dir, and #3323 (AC4) requires the
// push to move the branch of the session record, not the branch HEAD names.
func (g realGit) sessionBranch() string {
	wt := g.repo.WorkTree()
	common := g.repo.CommonDir()
	if wt == "" || common == "" {
		return ""
	}
	// common is <root>/.bare, so the project root is its parent.
	root := filepath.Dir(common)
	rel, err := filepath.Rel(root, wt)
	if err != nil {
		return ""
	}
	branch := filepath.ToSlash(rel)
	if sessionname.ValidateBranch(branch) != nil {
		return ""
	}
	// Reject a path part that git or a shell would misread as an option or a
	// non-branch name, so the branch is safe as a git push refspec.
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "-") ||
			strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return ""
		}
	}
	if strings.Contains(branch, "..") {
		return ""
	}
	return branch
}

// PreflightError is returned by Preflight when the gate refuses the review.
// It carries a Refused flag so callers can distinguish gate refusals from
// infrastructure errors (fetch failure, missing upstream) — both must exit
// non-zero, but only refusal carries the "behind <base>" rebase guidance.
type PreflightError struct {
	// Msg is the user-facing error message, ready to display.
	Msg string
	// Refused is true when the gate refused because the resolved base ref is
	// not an ancestor of HEAD. False for infrastructure errors (fetch
	// failure, missing upstream, conflict restore).
	Refused bool
	// CommitsBehind is the number of commits HEAD is behind the remote
	// branch (HEAD..<remote>/<base>). Populated only when Refused is true.
	// Zero otherwise.
	CommitsBehind int
}

func (e *PreflightError) Error() string { return e.Msg }

// Preflight runs the rebase gate. It returns nil when the resolved base ref
// (<remote>/<branch>, defaulting to origin/main) is an ancestor of HEAD (the
// review may proceed). On any failure (fetch failure, missing upstream,
// behind-base refusal, rebase conflict) it returns a *PreflightError with a
// ready-to-display message; the caller should print it to stderr and exit
// non-zero. Preflight does not spawn any review agents and does not touch the
// prism DB, so a Preflight failure cannot affect the review-cycle counter
// (the counter is derived from per-agent session rows written by RunAsync).
func Preflight(opts PreflightOpts) error {
	if opts.Worktree == "" {
		return &PreflightError{Msg: "prism review: preflight: worktree path is required"}
	}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}
	runner := opts.gitRunner
	if runner == nil {
		repo, openErr := git.OpenDir(opts.Worktree)
		if openErr != nil {
			// A redirect that prism refuses (a planted commondir, a nested
			// .bare, a .git that is not a regular file) or a missing dir.
			// git.OpenDir runs no host git command in these cases.
			return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: %v", openErr)}
		}
		runner = realGit{repo: repo}
	}
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	// Refuse a session that is not in a prism worktree (#3323, AC1). In a
	// plain clone or a bare session dir, git reads a config and hooks that the
	// session wrote, and a host git command there runs them as the user.
	// git.OpenDir runs no git command for a dir outside a prism project, so
	// no host git command runs in the session's dir before this refusal.
	if !runner.isWorktree() {
		return &PreflightError{Msg: fmt.Sprintf(`prism review: preflight: %s is not a prism worktree.

The host git steps of the pre-flight (fetch, rebase, push) run only in a prism
worktree, where prism pins the git dir. In a plain clone or a bare session
dir, git reads a config that the session wrote, so prism refuses to run git
there.`, opts.Worktree)}
	}

	// Refuse a worktree that already holds rebase, merge, cherry-pick, revert,
	// or sequencer state (#3323, AC2). The session can plant a rebase-merge
	// dir with an autostash file; a host `git rebase --abort` would then apply
	// that stash and run a command the session chose. The pre-flight does not
	// run on a worktree that is already mid-operation.
	if gd := runner.gitDir(); gd != "" {
		if state := existingGitOpState(gd); state != "" {
			return &PreflightError{Msg: fmt.Sprintf(`prism review: preflight: the worktree git dir already holds %s state.

Finish or abort the operation in the worktree, then re-run prism review.
prism does not run its rebase pre-flight on a worktree that is mid-operation.`, state)}
		}
	}

	// Step 1: capture original HEAD (only used to restore on conflict).
	origHead, _, _, err := runOrErr(runner, "rev-parse", "HEAD")
	if err != nil {
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: could not resolve HEAD: %v", err)}
	}
	origHead = strings.TrimSpace(origHead)

	// Step 2: fetch the remote branch. One network round-trip; fast.
	progress(fmt.Sprintf("preflight: fetching %s/%s …", remote, branch))
	if _, stderr, code, fetchErr := runner.run("fetch", remote, branch); fetchErr != nil || code != 0 {
		// Fetch failure is fatal — no fallback to running the review against an
		// unverified branch. Surface the underlying error verbatim so the user
		// can see network/auth diagnostics.
		var detail string
		switch {
		case fetchErr != nil:
			detail = fetchErr.Error()
		default:
			detail = strings.TrimSpace(stderr)
			if detail == "" {
				detail = fmt.Sprintf("git fetch exited with code %d", code)
			}
		}
		return &PreflightError{Msg: fmt.Sprintf("prism review: could not verify branch is up to date: git fetch %s %s failed: %s",
			remote, branch, detail)}
	}

	remoteRef := remote + "/" + branch

	// Step 3: verify the remote ref now exists (catches the "no <remote>/<base>
	// configured" case where fetch is silently a no-op because the remote
	// itself does not advertise the requested branch, or the local checkout
	// has no such remote). This must NOT silently retry against "main" — if
	// the resolved base is missing upstream, the operator needs to know.
	if _, stderr, code, _ := runner.run("rev-parse", "--verify", remoteRef); code != 0 {
		return &PreflightError{Msg: fmt.Sprintf(`prism review: no %s ref configured.

After 'git fetch %s %s' the ref %s is still not present. The branch has no
upstream %s configured (this is unusual but possible in local-only setups
or when the base branch has been deleted upstream).

Configure the remote:

    git remote add %s <url>
    git fetch %s %s

Or, if %s already exists, ensure it advertises a '%s' branch.

git stderr: %s`, remoteRef, remote, branch, remoteRef, branch, remote, remote, branch, remote, branch, strings.TrimSpace(stderr))}
	}

	// Step 4 (optional): inline rebase. We do this BEFORE the ancestor check so
	// that a successful rebase causes the ancestor check to pass naturally on
	// the same Preflight call.
	if opts.Rebase {
		progress(fmt.Sprintf("preflight: rebasing onto %s …", remoteRef))
		if _, stderr, code, runErr := runner.run("rebase", remoteRef); runErr != nil || code != 0 {
			// Restore WITHOUT `git rebase --abort` (#3323, AC3/AC5): --abort
			// applies rebase-merge/autostash, and a session can write that
			// file during a real conflict. restoreAfterPreflightRebase removes
			// the rebase state dir and resets to the original HEAD, so it
			// applies no stash and runs no command the session chose.
			restoreAfterPreflightRebase(runner, origHead)
			detail := strings.TrimSpace(stderr)
			if detail == "" && runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = fmt.Sprintf("git rebase exited with code %d", code)
			}
			return &PreflightError{Msg: fmt.Sprintf(`prism review: --rebase produced conflicts; the rebase was undone and the
worktree was restored to its original HEAD.

Resolve conflicts manually:

    git fetch %s %s
    git rebase %s
    # resolve conflicts in the listed files
    git add <files>
    git rebase --continue
    git push --force-with-lease

Then re-run 'prism review <pr>'.

git stderr (truncated):
%s`, remote, branch, remoteRef, truncate(detail, 2000))}
		}
		// Rebase succeeded. Push the branch of the session record (#3323,
		// AC4), never the branch that HEAD names: a session can write HEAD in
		// its own git dir, so a bare `git push` would let it choose which ref
		// the host moves. The explicit refs/heads/<branch> refspec cannot be
		// read as an option. --force-with-lease still guards a concurrent push
		// from another worker on the same branch.
		progress("preflight: pushing rebased HEAD with --force-with-lease …")
		pushArgs := []string{"push", "--force-with-lease"}
		if b := runner.sessionBranch(); b != "" {
			ref := "refs/heads/" + b
			pushArgs = append(pushArgs, remote, ref+":"+ref)
		}
		if _, stderr, code, runErr := runner.run(pushArgs...); runErr != nil || code != 0 {
			detail := strings.TrimSpace(stderr)
			if detail == "" && runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = fmt.Sprintf("git push exited with code %d", code)
			}
			return &PreflightError{Msg: fmt.Sprintf("prism review: --rebase succeeded locally but git push --force-with-lease failed: %s",
				truncate(detail, 2000))}
		}
	}

	// Step 5: strict ancestor check.
	// `git merge-base --is-ancestor <ancestor> <descendant>` returns 0 when
	// the first arg is an ancestor of the second, 1 when it is not, other
	// codes on error (e.g. unknown ref).
	_, stderr, code, runErr := runner.run("merge-base", "--is-ancestor", remoteRef, "HEAD")
	if runErr != nil {
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: merge-base failed: %v", runErr)}
	}
	switch code {
	case 0:
		// Up to date — proceed.
		return nil
	case 1:
		// Not an ancestor — refuse with a clear message. The refusal names the
		// resolved base ref in the "N commits behind …" line and in the
		// suggested `git fetch` / `git rebase` commands so a worker on a PR
		// targeting a non-main base sees the right ref to act on.
		behind := countCommitsBehind(runner, remoteRef)
		return &PreflightError{
			Refused:       true,
			CommitsBehind: behind,
			Msg: fmt.Sprintf(`prism review: branch is %s behind %s

%s has advanced since this branch was cut. Reviewers will see drift
that is not part of your change. Rebase first:

    git fetch %s %s
    git rebase %s
    git push --force-with-lease

Or rerun with --rebase to do this inline (only safe if your local branch
has no uncommitted or local-only state worth preserving).`,
				pluralCommits(behind), remoteRef, branch, remote, branch, remoteRef),
		}
	default:
		// Unknown ref or other git failure.
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: merge-base --is-ancestor exited with code %d: %s",
			code, strings.TrimSpace(stderr))}
	}
}

// countCommitsBehind returns the number of commits in remoteRef that are not in
// HEAD (i.e. commits HEAD is missing). Best effort — returns 0 on any error.
func countCommitsBehind(runner gitRunner, remoteRef string) int {
	out, _, code, _ := runner.run("rev-list", "--count", "HEAD.."+remoteRef)
	if code != 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

// pluralCommits formats a commit count for display.
func pluralCommits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

// truncate clips s to maxLen runes, appending an ellipsis marker when clipped.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n… (truncated)"
}

// gitOpStateMarkers are the files and dirs in a worktree git dir that mean an
// operation is in progress: a rebase, a merge, a cherry-pick, a revert, or a
// sequencer run. Preflight refuses when any is present (#3323, AC2).
var gitOpStateMarkers = []string{
	"rebase-merge",
	"rebase-apply",
	"sequencer",
	"MERGE_HEAD",
	"CHERRY_PICK_HEAD",
	"REVERT_HEAD",
}

// existingGitOpState returns the name of the first in-progress-operation
// marker present in gitDir, or "" when the worktree holds none.
func existingGitOpState(gitDir string) string {
	for _, name := range gitOpStateMarkers {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return name
		}
	}
	return ""
}

// restoreAfterPreflightRebase undoes a conflicted pre-flight rebase without
// applying any stash (#3323, AC3/AC5). `git rebase --abort` would apply
// rebase-merge/autostash, and a session can write that file during a real
// conflict, so the host must not run it. Instead this removes the rebase
// state dir directly, re-attaches HEAD to the branch of the session record
// (the rebase detached it), and resets the worktree to the pre-rebase commit.
func restoreAfterPreflightRebase(runner gitRunner, origHead string) {
	if gitDir := runner.gitDir(); gitDir != "" {
		for _, d := range []string{"rebase-merge", "rebase-apply"} {
			_ = os.RemoveAll(filepath.Join(gitDir, d))
		}
	}
	if b := runner.sessionBranch(); b != "" {
		_, _, _, _ = runner.run("symbolic-ref", "HEAD", "refs/heads/"+b)
	}
	if origHead != "" {
		_, _, _, _ = runner.run("reset", "--hard", origHead)
	}
}

// runOrErr is a thin wrapper that promotes a non-zero git exit to an error.
// Used for "the command must succeed" steps where the caller does not want
// to inspect the exit code separately.
func runOrErr(r gitRunner, args ...string) (string, string, int, error) {
	stdout, stderr, code, err := r.run(args...)
	if err != nil {
		return stdout, stderr, code, err
	}
	if code != 0 {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = fmt.Sprintf("git %s exited with code %d", strings.Join(args, " "), code)
		}
		return stdout, stderr, code, errors.New(detail)
	}
	return stdout, stderr, code, nil
}
