package review

// diff_dir.go — the dir of the diff files of `prism review` (issue #3377).
//
// A diff above the inline limit goes to a file, and the review prompt names
// the file. The file is in the state dir, not in the worktree: the sandbox of
// the worker can write its worktree, so a symlink there could send a host
// write to any file of the user. No sandbox can write the state dir.
//
// Each parent session has its own dir, <DiffStateDir>/<12 hex of
// sha256(parent)>. Two parents that review the same PR number in the same
// round write to different files. A review agent gets a read of the dir of
// its parent only (container.Config.ReviewDiffDir).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/session"
)

// DiffStateDir returns $XDG_STATE_HOME/prism/review-diff, or
// $HOME/.local/state/prism/review-diff when XDG_STATE_HOME is empty.
//
// It returns an error when the result is not an absolute path. A relative
// path resolves against the cwd of `prism review`, which is the worktree.
func DiffStateDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("review diff dir: resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", fmt.Errorf("review diff dir: the state dir %q is not an absolute path", stateHome)
	}
	return filepath.Join(stateHome, "prism", "review-diff"), nil
}

// DiffDirForParent returns the diff dir of the review rounds of
// parentSession.
func DiffDirForParent(parentSession string) (string, error) {
	if parentSession == "" {
		return "", errors.New("review diff dir: the parent session name is empty")
	}
	base, err := DiffStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, session.SessionDirName(parentSession)), nil
}

// RemoveDiffDirForParent removes the diff dir of parentSession. Errors are
// logged, not returned: a dir that stays on disk does not stop the cleanup.
// os.RemoveAll removes a symlink at the path, not its target.
func RemoveDiffDirForParent(parentSession string) {
	dir, err := DiffDirForParent(parentSession)
	if err != nil {
		proglog.Warnf("[prism] warning: remove the review diff dir of %q: %v\n", parentSession, err)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		proglog.Warnf("[prism] warning: remove the review diff dir %s: %v\n", dir, err)
	}
}

// diffFilePath returns <stateDir>/pr-<prNumber>-round-<round>.diff. The PR
// number comes from the caller of `prism review`, so it is sanitised: the
// path stays in stateDir.
func diffFilePath(stateDir, prNumber string, round int) string {
	if round <= 0 {
		round = 1
	}
	return filepath.Join(stateDir, fmt.Sprintf("pr-%s-round-%d.diff", sanitisePRNumber(prNumber), round))
}

// writeDiffFile writes diff to its file in stateDir and returns the path.
// rundir.WriteFile refuses a stateDir that is a symlink, and it replaces a
// symlink at the file path: it does not follow it.
func writeDiffFile(stateDir, prNumber string, round int, diff string) (string, error) {
	if stateDir == "" {
		return "", errors.New("no diff dir is set")
	}
	if !filepath.IsAbs(stateDir) {
		return "", fmt.Errorf("the diff dir %q is not an absolute path", stateDir)
	}
	path := diffFilePath(stateDir, prNumber, round)
	if err := rundir.WriteFile(path, []byte(diff)); err != nil {
		return "", err
	}
	return path, nil
}

// placeDiff puts diff into prCtx. A diff within the inline limits goes into
// prCtx.Diff, and no file is written. A larger diff goes to a file in
// stateDir, and prCtx.DiffFilePath names it. When the file cannot be
// written, the truncated diff goes into prCtx.Diff, and a warning names the
// path. truncated is diff after the hard size limits.
func placeDiff(prCtx *PRContext, stateDir, prNumber string, round int, diff, truncated string, inlineMaxBytes, inlineMaxLines int) {
	if len(diff) <= inlineMaxBytes && strings.Count(diff, "\n") <= inlineMaxLines {
		prCtx.Diff = truncated
		return
	}
	path, err := writeDiffFile(stateDir, prNumber, round, diff)
	if err != nil {
		where := stateDir
		if where == "" {
			where = "(none)"
		}
		proglog.Warnf("[prism review] warning: could not write the diff file in %s: %v — inlining the truncated diff instead\n", where, err)
		prCtx.Diff = truncated
		return
	}
	prCtx.DiffFilePath = path
}
