package main

// pr.go — `iris pr <number>` subcommand.
//
// `iris pr <n>` is the iris analogue of `prism pr <n>` (cmd/pr.go). It fetches
// the PR's head branch via `gh`, sets up a worktree at <bareRoot>/<branch> in
// the prism bare+worktree layout, and asks the running iris daemon to spawn a
// session on it. If `--prompt`, `--prompt-file`, or `--prompt -` is given,
// the prompt is delivered to the freshly-spawned session after the spawn ack.
//
// This is a pure CLI wrapper on top of the daemon-routed spawn introduced in
// #1680 (iris spawn) and the prompt-delivery wire frame introduced in
// #1677 (iris prompt). No new daemon-side surface is required:
//
//   1. CLI resolves the bare repo root from --repo or the working directory.
//   2. CLI checks `gh` is on PATH (clearer error than the generic exec one).
//   3. CLI runs `gh pr view <n>` (via internal/git.PRBranch) to resolve the
//      PR's head branch.
//   4. CLI either (a) reuses an existing clean worktree for that branch, or
//      (b) calls git.CreateWorktree(bareRoot, branch) to make one.
//      Dirty existing worktrees cause a hard refusal.
//   5. CLI dials the daemon and sends a session_spawn frame for the worktree
//      — same code path as `iris spawn`.
//   6. If a prompt was supplied, CLI sends a follow-up prompt_deliver frame.
//
// # Edge-case handling (acceptance criteria for issue #1702)
//
//   - No such PR     → `gh pr view` exits non-zero; we surface its stderr.
//   - Daemon down    → canonical "iris daemon not running … systemctl --user
//                      start iris" error from dialDaemon (shared with spawn).
//   - Dirty worktree → "worktree dirty" refusal before any spawn frame.
//   - gh missing     → "gh CLI not found on PATH" before any network call.
//
// # Out of scope
//
//   - `--review` shortcut. Compose with `--prompt 'review this PR'`.
//   - Auto-cleanup on PR merge. Handled by the merge queue elsewhere.
//   - Smart workspace setup (nix develop etc.). Out of scope per the issue.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
)

// prRole is the agent role used for sessions spawned by `iris pr`. The role is
// resolved daemon-side by iris.ResolveAgent — for a PR worktree, the basename
// is the PR branch (never "main"), so it always resolves to "worker".
const prRole = "worker"

// prCmd is the `iris pr <number>` cobra subcommand.
var prCmd = &cobra.Command{
	Use:   "pr <number>",
	Short: "Fetch a PR branch and spawn an iris session on it",
	Long: `iris pr fetches the head branch of the named PR (via 'gh pr view') and
asks the running iris daemon to spawn a session on a worktree for it.

Behaviour:

  iris pr <n>                       fetch + worktree + spawn (no prompt)
  iris pr <n> --prompt <text>       deliver <text> after spawn acks
  iris pr <n> --prompt-file <path>  read prompt from file
  iris pr <n> --prompt -            read prompt from stdin
  iris pr <n> --repo <repo>         operate on another repo (name under ~/code
                                    or an absolute path to a prism bare repo)

A single trailing newline is stripped from --prompt-file and --prompt -
input (matches prism's convention). --prompt and --prompt-file are mutually
exclusive.

# Worktree reuse

If a worktree for the PR's head branch already exists under <bareRoot>/<branch>
it is reused — no duplicate is created. If the current working directory is
that worktree, the spawn happens in place.

If the existing worktree has uncommitted changes (staged or unstaged),
the command refuses with a "worktree dirty" error rather than silently
spawning on dirty state.

# Requirements

The 'gh' CLI must be installed and authenticated. The iris daemon must be
running (start it with 'systemctl --user start iris'). The target repo must
use the prism bare+worktree layout (a '.bare' directory at the repo root).`,
	Args:          cobra.ExactArgs(1),
	RunE:          runPRCmd,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	prCmd.Flags().String(
		"repo", "",
		"Target repo: a folder name under ~/code, or an absolute path to a prism bare repo. Defaults to the current working directory's bare root.",
	)
	prCmd.Flags().String(
		"prompt", "",
		"Text to deliver to the spawned session. Use '-' to read from stdin. Mutually exclusive with --prompt-file.",
	)
	prCmd.Flags().String(
		"prompt-file", "",
		"Path to a file containing the prompt text. Mutually exclusive with --prompt.",
	)
	prCmd.Flags().String(
		"socket", "",
		"Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)",
	)
	prCmd.MarkFlagsMutuallyExclusive("prompt", "prompt-file")
	rootCmd.AddCommand(prCmd)
}

// prOptions is the fully-resolved input to runPRAt: positional and flag values
// parsed and validated, ready to drive the wire path. Bundling them keeps the
// function signature small and makes the integration test's call-site readable.
type prOptions struct {
	PRNumber  string
	BareRoot  string
	Prompt    string
	HasPrompt bool
	SockPath  string
	HomeDir   string // used to expand "~/" in --repo for tests
	CWD       string // used for in-place detection

	// GHLookPath stubs exec.LookPath("gh") for tests. Defaults to exec.LookPath.
	GHLookPath func(string) (string, error)

	// ResolveBranch stubs the PR-number → branch resolution for tests. Defaults
	// to a wrapper around git.PRBranch (which shells out to `gh pr view`). The
	// integration test substitutes an in-memory stub so it does not need a real
	// gh CLI or network access.
	ResolveBranch func(bareRoot, prNumber string) (string, error)
}

// runPRCmd is the cobra entry point. It marshals flags into prOptions and
// delegates to runPRAt for the wire path so the integration test can drive
// the same code against an in-process ClientSocket.
func runPRCmd(cmd *cobra.Command, args []string) error {
	prNumber := args[0]
	if prNumber == "" {
		return errors.New("iris pr: <number> is required")
	}

	repoFlag, _ := cmd.Flags().GetString("repo")
	bareRoot, err := resolvePRBareRoot(repoFlag, "", "")
	if err != nil {
		return err
	}

	promptText, hasPrompt, err := resolvePRPromptInput(cmd)
	if err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	opts := prOptions{
		PRNumber:      prNumber,
		BareRoot:      bareRoot,
		Prompt:        promptText,
		HasPrompt:     hasPrompt,
		SockPath:      resolveSocketPath(cmd),
		CWD:           cwd,
		GHLookPath:    exec.LookPath,
		ResolveBranch: resolvePRBranchVerbose,
	}
	return runPRAt(cmd.Context(), opts, os.Stdout)
}

// resolvePRPromptInput reads --prompt, --prompt-file, or --prompt - (stdin).
// It is the same surface as resolveIrisPromptInput in prompt.go but also
// returns a `hasPrompt` discriminator so the caller can distinguish "no
// prompt requested" from "explicit empty prompt" — the post-spawn delivery
// is only attempted when hasPrompt is true.
//
// A single trailing newline is stripped from file/stdin input (matches
// prism's prompt_input.go convention).
//
// Mutual exclusion of --prompt vs --prompt-file is enforced by cobra before
// RunE is called.
func resolvePRPromptInput(cmd *cobra.Command) (text string, hasPrompt bool, err error) {
	promptFile, _ := cmd.Flags().GetString("prompt-file")
	promptText, _ := cmd.Flags().GetString("prompt")

	if promptFile != "" {
		data, readErr := os.ReadFile(promptFile)
		if readErr != nil {
			return "", false, fmt.Errorf("read prompt file %q: %w", promptFile, readErr)
		}
		return strings.TrimSuffix(string(data), "\n"), true, nil
	}

	if cmd.Flags().Changed("prompt") {
		if promptText == "-" {
			data, readErr := io.ReadAll(os.Stdin)
			if readErr != nil {
				return "", false, fmt.Errorf("read prompt from stdin: %w", readErr)
			}
			return strings.TrimSuffix(string(data), "\n"), true, nil
		}
		return promptText, true, nil
	}

	return "", false, nil
}

// resolvePRBareRoot resolves the bare repo root to operate on:
//
//   - If repoFlag is non-empty, treat it as a shorthand under ~/code (or
//     other prism project locations are not honoured here — iris does not
//     load prism's config.json) or as an absolute path.
//   - Otherwise, walk up from CWD looking for a .bare entry.
//
// homeDirOverride and cwdOverride are test hooks. Both default to the real
// $HOME / os.Getwd() when empty.
func resolvePRBareRoot(repoFlag, homeDirOverride, cwdOverride string) (string, error) {
	home := homeDirOverride
	if home == "" {
		home, _ = os.UserHomeDir()
	}

	if repoFlag != "" {
		// Expand "~/" if the user typed it.
		candidate := repoFlag
		if strings.HasPrefix(candidate, "~/") && home != "" {
			candidate = filepath.Join(home, candidate[2:])
		}
		if filepath.IsAbs(candidate) {
			if git.IsBareRepo(candidate) {
				return candidate, nil
			}
			return "", fmt.Errorf("iris pr: %s is not a prism bare repo (missing .bare/)", candidate)
		}
		// Shorthand: only look under ~/code (iris does not load prism's
		// ProjectLocations config — keep the lookup table small and
		// predictable). If the user wants a non-~/code repo, they pass an
		// absolute path.
		if home != "" {
			p := filepath.Join(home, "code", candidate)
			if git.IsBareRepo(p) {
				return p, nil
			}
		}
		return "", fmt.Errorf("iris pr: repo %q not found under ~/code (pass an absolute path for repos outside ~/code)", repoFlag)
	}

	cwd := cwdOverride
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("iris pr: get working directory: %w", err)
		}
	}
	bareRoot := git.BareRoot(cwd)
	if bareRoot == "" {
		// Maybe cwd is itself the bare root.
		if git.IsBareRepo(cwd) {
			return cwd, nil
		}
		return "", fmt.Errorf("iris pr: current directory %s is not inside a prism bare repo; pass --repo <name|path>", cwd)
	}
	return bareRoot, nil
}

// findExistingWorktreeForBranch scans the bare repo's worktree list and
// returns the path of the worktree currently checked out at branch, or "" if
// none. We match by SymbolicRef rather than by directory name because a user
// may have renamed the worktree directory after `git worktree add`.
func findExistingWorktreeForBranch(bareRoot, branch string) string {
	for _, wt := range git.Worktrees(bareRoot) {
		ref, err := git.SymbolicRef(wt)
		if err != nil {
			continue
		}
		if ref == "refs/heads/"+branch {
			return wt
		}
	}
	return ""
}

// runPRAt is the testable core of `iris pr`. It is invoked by both the cobra
// RunE wrapper and the integration test. opts is fully resolved; out is the
// stdout writer that captures human-readable progress output.
//
// Phases:
//
//  1. Verify the gh CLI is available (defensive: gives a clearer error than
//     the generic "executable file not found in $PATH" from gh-pr-view).
//  2. Resolve the PR's head branch via gh.
//  3. Find or create the worktree:
//     - If a worktree for the branch already exists, reject if dirty.
//     - Otherwise call git.CreateWorktree.
//  4. Dial the daemon and send session_spawn (reuse the spawn.go helpers).
//  5. On ack, if a prompt was supplied, send prompt_deliver on a fresh conn.
func runPRAt(ctx context.Context, opts prOptions, out io.Writer) error {
	if opts.PRNumber == "" {
		return errors.New("iris pr: <number> is required")
	}
	if opts.BareRoot == "" {
		return errors.New("iris pr: bare root not resolved")
	}

	lookPath := opts.GHLookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("gh"); err != nil {
		return fmt.Errorf(
			"iris pr: 'gh' CLI not found on PATH — install GitHub CLI and run 'gh auth login' first (%w)",
			err,
		)
	}

	// Resolve PR → branch. The default implementation calls git.PRBranch which
	// shells out to `gh pr view --json headRefName`. Tests inject a stub via
	// opts.ResolveBranch to avoid needing a real gh CLI / network call.
	resolveBranch := opts.ResolveBranch
	if resolveBranch == nil {
		resolveBranch = resolvePRBranchVerbose
	}
	fmt.Fprintf(out, "[iris pr] resolving PR #%s in %s...\n", opts.PRNumber, filepath.Base(opts.BareRoot))
	branch, err := resolveBranch(opts.BareRoot, opts.PRNumber)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[iris pr] PR #%s → branch %q\n", opts.PRNumber, branch)

	// Find or create the worktree for that branch.
	worktree, reused, err := acquireWorktreeForBranch(opts.BareRoot, branch, out)
	if err != nil {
		return err
	}
	if reused {
		// AC: when the PR branch matches the CWD's worktree, spawn in-place
		// (no re-checkout). The "no re-checkout" semantics are automatic
		// because acquireWorktreeForBranch did not create a new worktree.
		// We surface the in-place detail only as informational output.
		if opts.CWD != "" {
			if cwdMatch, _ := samePath(opts.CWD, worktree); cwdMatch {
				fmt.Fprintf(out, "[iris pr] current directory matches PR worktree; spawning in place\n")
			}
		}
	}

	// Dial the daemon and send session_spawn. We re-use the existing helpers
	// from spawn.go (dialDaemon, sendSpawnFrame, readSpawnAck) so the wire
	// behaviour stays identical to `iris spawn`.
	conn, err := dialDaemon(ctx, opts.SockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendSpawnFrame(conn, worktree, prRole); err != nil {
		return fmt.Errorf("iris pr: send spawn frame: %w", err)
	}
	sessionName, instanceID, err := readSpawnAck(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[iris pr] session %s spawned (instance_id=%s, worktree=%s)\n",
		sessionName, instanceID, worktree)

	// Optional follow-up: deliver the prompt to the freshly spawned session.
	if opts.HasPrompt {
		if err := deliverPRPrompt(ctx, opts.SockPath, sessionName, opts.Prompt); err != nil {
			// The spawn already succeeded — surface the delivery failure but
			// don't pretend the session didn't get created.
			return fmt.Errorf("iris pr: session %s spawned but prompt delivery failed: %w", sessionName, err)
		}
		fmt.Fprintf(out, "[iris pr] prompt delivered to %s\n", sessionName)
	}

	return nil
}

// resolvePRBranchVerbose wraps git.PRBranch with a more useful error message
// when gh exits non-zero (the common case being "no such PR"). git.PRBranch
// only returns a generic "gh pr view: exit status 1" today; we re-run gh
// directly to surface its stderr to the user.
func resolvePRBranchVerbose(bareRoot, prNumber string) (string, error) {
	branch, err := git.PRBranch(bareRoot, prNumber)
	if err == nil {
		if branch == "" {
			return "", fmt.Errorf("iris pr: gh pr view returned an empty branch name for PR #%s", prNumber)
		}
		return branch, nil
	}
	// Re-run gh to capture stderr for a clearer error.
	remoteURL, urlErr := runGitCmd(bareRoot, "config", "--get", "remote.origin.url")
	if urlErr != nil || remoteURL == "" {
		return "", fmt.Errorf("iris pr: could not resolve PR #%s (%w)", prNumber, err)
	}
	cmd := exec.Command("gh", "pr", "view", prNumber,
		"--repo", strings.TrimSpace(remoteURL),
		"--json", "headRefName", "--jq", ".headRefName")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, runErr := cmd.Output(); runErr != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return "", fmt.Errorf("iris pr: could not resolve PR #%s: %s", prNumber, stderrText)
		}
		return "", fmt.Errorf("iris pr: could not resolve PR #%s (%w)", prNumber, runErr)
	}
	// gh succeeded on the retry — odd, but return the original error if
	// branch is still empty.
	return "", fmt.Errorf("iris pr: could not resolve PR #%s (%w)", prNumber, err)
}

// runGitCmd shells out to `git -C <bareRoot> <args...>` and returns trimmed
// stdout. This is used only by resolvePRBranchVerbose to fetch the remote URL
// when the primary lookup has already failed; the normal happy path goes
// through internal/git helpers.
func runGitCmd(bareRoot string, args ...string) (string, error) {
	// The bare repo lives at <bareRoot>/.bare in the prism layout. Use
	// --git-dir directly so we don't depend on `git -C` finding it.
	bareDir := filepath.Join(bareRoot, ".bare")
	full := append([]string{"--git-dir", bareDir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// acquireWorktreeForBranch returns a worktree path for the given branch in
// the bare repo, reusing an existing one if present and clean. Returns
// (path, reused, err). On a dirty existing worktree it refuses.
func acquireWorktreeForBranch(bareRoot, branch string, out io.Writer) (string, bool, error) {
	if existing := findExistingWorktreeForBranch(bareRoot, branch); existing != "" {
		// Reject if the existing worktree has uncommitted changes — silently
		// spawning on dirty state would surprise the user.
		stat, err := git.Stat(existing)
		if err != nil {
			return "", false, fmt.Errorf("iris pr: stat existing worktree %s: %w", existing, err)
		}
		if stat.Files > 0 {
			return "", false, fmt.Errorf(
				"iris pr: existing worktree for branch %q at %s is dirty (%s) — commit, stash, or `git restore` first",
				branch, existing, stat.String(),
			)
		}
		fmt.Fprintf(out, "[iris pr] reusing existing worktree at %s\n", existing)
		return existing, true, nil
	}

	fmt.Fprintf(out, "[iris pr] creating worktree for branch %q...\n", branch)
	worktree, err := git.CreateWorktree(bareRoot, branch)
	if err != nil {
		return "", false, fmt.Errorf("iris pr: create worktree: %w", err)
	}
	fmt.Fprintf(out, "[iris pr] worktree at %s\n", worktree)
	return worktree, false, nil
}

// samePath compares two paths for filesystem equality after Abs + symlink-aware
// normalisation. Returns (true, nil) when the paths refer to the same location.
// Errors fall through as (false, err); callers may treat any error as "not
// the same path" since the worst case is a redundant message line.
func samePath(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	if absA == absB {
		return true, nil
	}
	realA, _ := filepath.EvalSymlinks(absA)
	realB, _ := filepath.EvalSymlinks(absB)
	return realA != "" && realA == realB, nil
}

// deliverPRPrompt sends a prompt_deliver frame for sessionName/promptText on a
// fresh connection. It mirrors the shape of `iris prompt`'s send path but uses
// a slightly tighter ack window because the spawn ack has just told us the
// session exists and is live — the only reason for an error frame here would
// be an internal daemon failure.
func deliverPRPrompt(ctx context.Context, sockPath, sessionName, promptText string) error {
	conn, err := dialDaemonForPrompt(ctx, sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendPromptDeliverFrame(conn, sessionName, promptText); err != nil {
		return err
	}

	// Brief read window matches `iris prompt`: no news (timeout) means
	// success; an error frame is surfaced; an early EOF is a lost connection.
	ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return readPromptAck(ackCtx, conn)
}
