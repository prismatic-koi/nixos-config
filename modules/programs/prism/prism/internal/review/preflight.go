package review

// preflight.go — pre-flight rebase gate for `prism review`.
//
// Issue #1518: Reviews regularly produce noisy findings of the form "you should
// also update X" when X landed on main after the branch was cut. A pre-flight
// ancestor check on origin/main catches this in one fetch, before any agent
// spawns, and either refuses the review or (with --rebase) fixes it inline.
//
// The gate is a snapshot at review-spawn time. It is not continuous — main can
// advance during a review run; we do not chase that. This is consistent with
// how CI works.
//
// Strict ancestor check via `git merge-base --is-ancestor origin/main HEAD`.
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
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PreflightOpts configures the rebase gate.
type PreflightOpts struct {
	// Worktree is the absolute path to the git worktree. Required.
	Worktree string
	// Rebase, when true, performs fetch + rebase + force-push inline before
	// the ancestor check. On rebase conflict the rebase is aborted, the
	// worktree is restored to the original HEAD, and the gate returns an
	// error — never leaving the worktree mid-rebase.
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

// gitRunner is the test seam for executing git in a specific worktree.
type gitRunner interface {
	run(worktree string, args ...string) (stdout string, stderr string, exitCode int, err error)
}

// realGit shells out to the system git binary.
type realGit struct{}

func (realGit) run(worktree string, args ...string) (string, string, int, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = worktree
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

// PreflightError is returned by Preflight when the gate refuses the review.
// It carries a Refused flag so callers can distinguish gate refusals from
// infrastructure errors (fetch failure, missing upstream) — both must exit
// non-zero, but only refusal carries the "behind main" rebase guidance.
type PreflightError struct {
	// Msg is the user-facing error message, ready to display.
	Msg string
	// Refused is true when the gate refused because origin/main is not an
	// ancestor of HEAD. False for infrastructure errors (fetch failure,
	// missing upstream, conflict abort).
	Refused bool
	// CommitsBehind is the number of commits HEAD is behind the remote
	// branch (origin/main..HEAD vs HEAD..origin/main). Populated only when
	// Refused is true. Zero otherwise.
	CommitsBehind int
}

func (e *PreflightError) Error() string { return e.Msg }

// Preflight runs the rebase gate. It returns nil when origin/main is an
// ancestor of HEAD (the review may proceed). On any failure (fetch failure,
// missing upstream, behind-main refusal, rebase conflict) it returns a
// *PreflightError with a ready-to-display message; the caller should print it
// to stderr and exit non-zero. Preflight does not spawn any review agents and
// does not touch the prism DB, so a Preflight failure cannot affect the
// review-cycle counter (the counter is derived from per-agent session rows
// written by RunAsync).
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
		runner = realGit{}
	}
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	// Step 1: capture original HEAD (only used to restore on conflict abort).
	origHead, _, _, err := runOrErr(runner, opts.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: could not resolve HEAD: %v", err)}
	}
	origHead = strings.TrimSpace(origHead)

	// Step 2: fetch the remote branch. One network round-trip; fast.
	progress(fmt.Sprintf("preflight: fetching %s/%s …", remote, branch))
	if _, stderr, code, fetchErr := runner.run(opts.Worktree, "fetch", remote, branch); fetchErr != nil || code != 0 {
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

	// Step 3: verify the remote ref now exists (catches the "no origin/main
	// configured" case where fetch is silently a no-op because the remote
	// itself does not advertise main, or the local checkout has no origin).
	if _, stderr, code, _ := runner.run(opts.Worktree, "rev-parse", "--verify", remoteRef); code != 0 {
		return &PreflightError{Msg: fmt.Sprintf(`prism review: no %s ref configured.

After 'git fetch %s %s' the ref %s is still not present. The branch has no
upstream main configured (this is unusual but possible in local-only setups).

Configure the remote:

    git remote add %s <url>
    git fetch %s %s

Or, if origin already exists, ensure it advertises a 'main' branch.

git stderr: %s`, remoteRef, remote, branch, remoteRef, remote, remote, branch, strings.TrimSpace(stderr))}
	}

	// Step 4 (optional): inline rebase. We do this BEFORE the ancestor check so
	// that a successful rebase causes the ancestor check to pass naturally on
	// the same Preflight call.
	if opts.Rebase {
		progress(fmt.Sprintf("preflight: rebasing onto %s …", remoteRef))
		if _, stderr, code, runErr := runner.run(opts.Worktree, "rebase", remoteRef); runErr != nil || code != 0 {
			// Rebase failed — abort it and restore HEAD to the original state.
			// `git rebase --abort` is a no-op (exit 128 with "no rebase in
			// progress") if the rebase did not actually start, so we run it
			// unconditionally and ignore its exit code.
			_, _, _, _ = runner.run(opts.Worktree, "rebase", "--abort")
			// Defence in depth: if --abort somehow left HEAD shifted, force it
			// back. `reset --hard <origHead>` is idempotent and safe.
			if origHead != "" {
				_, _, _, _ = runner.run(opts.Worktree, "reset", "--hard", origHead)
			}
			detail := strings.TrimSpace(stderr)
			if detail == "" && runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = fmt.Sprintf("git rebase exited with code %d", code)
			}
			return &PreflightError{Msg: fmt.Sprintf(`prism review: --rebase produced conflicts; rebase aborted, worktree restored to original HEAD.

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
		// Rebase succeeded. Push with --force-with-lease so a concurrent push
		// from another worker on the same branch is not silently overwritten.
		progress("preflight: pushing rebased HEAD with --force-with-lease …")
		if _, stderr, code, runErr := runner.run(opts.Worktree, "push", "--force-with-lease"); runErr != nil || code != 0 {
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
	_, stderr, code, runErr := runner.run(opts.Worktree, "merge-base", "--is-ancestor", remoteRef, "HEAD")
	if runErr != nil {
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: merge-base failed: %v", runErr)}
	}
	switch code {
	case 0:
		// Up to date — proceed.
		return nil
	case 1:
		// Not an ancestor — refuse with a clear message.
		behind := countCommitsBehind(runner, opts.Worktree, remoteRef)
		return &PreflightError{
			Refused:       true,
			CommitsBehind: behind,
			Msg: fmt.Sprintf(`prism review: branch is %s behind %s

main has advanced since this branch was cut. Reviewers will see drift
that is not part of your change. Rebase first:

    git fetch %s %s
    git rebase %s
    git push --force-with-lease

Or rerun with --rebase to do this inline (only safe if your local branch
has no uncommitted or local-only state worth preserving).`,
				pluralCommits(behind), remoteRef, remote, branch, remoteRef),
		}
	default:
		// Unknown ref or other git failure.
		return &PreflightError{Msg: fmt.Sprintf("prism review: preflight: merge-base --is-ancestor exited with code %d: %s",
			code, strings.TrimSpace(stderr))}
	}
}

// countCommitsBehind returns the number of commits in remoteRef that are not in
// HEAD (i.e. commits HEAD is missing). Best effort — returns 0 on any error.
func countCommitsBehind(runner gitRunner, worktree, remoteRef string) int {
	out, _, code, _ := runner.run(worktree, "rev-list", "--count", "HEAD.."+remoteRef)
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

// runOrErr is a thin wrapper that promotes a non-zero git exit to an error.
// Used for "the command must succeed" steps where the caller does not want
// to inspect the exit code separately.
func runOrErr(r gitRunner, worktree string, args ...string) (string, string, int, error) {
	stdout, stderr, code, err := r.run(worktree, args...)
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
