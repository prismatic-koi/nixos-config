// Package git provides helpers for querying git worktree state.
package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotAWorktree is returned by ResolveWorktreeGitDir when worktreePath's
// .git entry is a directory rather than a "gitdir: " pointer file — i.e. the
// path is a normal git clone, not a prism bare+worktree checkout. Callers
// must treat this as a distinguishable non-error condition (there is no
// private git-state dir to resolve), not the same as a malformed or missing
// pointer file, which remains a hard error.
var ErrNotAWorktree = errors.New("not a git worktree: .git is a directory")

// DiffStat holds a summary of uncommitted changes in a worktree.
type DiffStat struct {
	Files      int
	Insertions int
	Deletions  int
}

// String returns a compact representation, e.g. "3 files +42 -7".
// Returns an empty string if there are no changes.
func (d DiffStat) String() string {
	if d.Files == 0 {
		return ""
	}
	fileWord := "files"
	if d.Files == 1 {
		fileWord = "file"
	}
	return fmt.Sprintf("%d %s +%d -%d", d.Files, fileWord, d.Insertions, d.Deletions)
}

// Stat returns a DiffStat for the given directory, combining unstaged and
// staged changes relative to HEAD. Returns an error if dir is empty, does not
// exist, or is not a git repository, or if any underlying git command fails.
// A nil error with a zero DiffStat means the worktree is clean.
func Stat(dir string) (DiffStat, error) {
	if dir == "" {
		return DiffStat{}, fmt.Errorf("empty directory path")
	}

	// --numstat gives machine-readable "added\tdeleted\tfile" lines.
	// Collect both unstaged and staged, deduplicating by filename.
	seen := map[string]bool{}
	var total DiffStat
	var firstErr error

	for _, args := range [][]string{
		{"-C", dir, "diff", "--numstat", "HEAD"},
		{"-C", dir, "diff", "--numstat", "--cached"},
	} {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) < 3 {
				continue
			}
			filename := parts[2]
			ins, _ := strconv.Atoi(parts[0])
			del, _ := strconv.Atoi(parts[1])
			if !seen[filename] {
				seen[filename] = true
				total.Files++
				total.Insertions += ins
				total.Deletions += del
			}
		}
	}
	// Both commands must succeed for a valid result. If either failed, the stat
	// is unreliable — return the error so callers can distinguish "clean" from
	// "unknown".
	if firstErr != nil {
		return DiffStat{}, firstErr
	}
	return total, nil
}

// IsBareRepo returns true if dir contains a .bare entry (prism bare layout).
// Accepts both a directory (standard git clone --bare) and a regular file
// (gitdir pointer in alternate configurations) so the detection is consistent
// with cmd.deriveBareRoot.
func IsBareRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".bare"))
	return err == nil
}

// IsRawBareGitDir returns true if dir is itself a raw git bare repository
// (i.e. contains HEAD and objects/ directly, without a .bare wrapper).
// This is used to detect the container layout where the bare repo is mounted
// directly at /prism-git rather than being a project root containing .bare/.
func IsRawBareGitDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		return false
	}
	return true
}

// IsRegularRepo returns true if dir is a regular (non-bare) git repo.
func IsRegularRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && !IsBareRepo(dir)
}

// IsInsideRegularRepo returns true if dir or any of its parents is a regular
// (non-bare) git repo. Mirrors the walk-up logic used by BareRoot.
func IsInsideRegularRepo(dir string) bool {
	p := dir
	for {
		if IsRegularRepo(p) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return false
}

// gitDir returns the path to the git bare directory for a prism repo.
// Normally this is <projectPath>/.bare. When projectPath itself is a raw bare
// git dir (e.g. /prism-git in container mode), it is returned directly so
// that all git operations work without requiring a .bare wrapper.
func gitDir(projectPath string) string {
	candidate := filepath.Join(projectPath, ".bare")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// If projectPath is itself a raw bare git dir, use it directly.
	if IsRawBareGitDir(projectPath) {
		return projectPath
	}
	// Fall through to the standard path (will fail if .bare doesn't exist,
	// which is the correct behaviour for non-bare project dirs).
	return candidate
}

// runGit runs git with the given args and returns trimmed stdout.
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultBranch returns the default branch name for a bare-layout repo.
// It tries refs/remotes/origin/HEAD first, then falls back to main/master.
func DefaultBranch(projectPath string) string {
	bare := gitDir(projectPath)
	ref, err := runGit("--git-dir", bare, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil && ref != "" {
		parts := strings.Split(ref, "/")
		return parts[len(parts)-1]
	}
	for _, branch := range []string{"main", "master"} {
		if err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
			"refs/heads/"+branch).Run(); err == nil {
			return branch
		}
	}
	return ""
}

// Worktrees returns the list of worktree paths for a bare-layout repo,
// with the default branch worktree first.
func Worktrees(projectPath string) []string {
	bare := gitDir(projectPath)
	out, err := runGit("--git-dir", bare, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}

	var worktrees []string
	var current string
	isBare := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			if current != "" && !isBare {
				worktrees = append(worktrees, current)
			}
			current = strings.TrimPrefix(line, "worktree ")
			isBare = false
		} else if strings.TrimSpace(line) == "bare" {
			isBare = true
		}
	}
	if current != "" && !isBare {
		worktrees = append(worktrees, current)
	}

	// Put default branch first.
	defaultBranch := DefaultBranch(projectPath)
	if defaultBranch != "" {
		defaultPath := filepath.Join(projectPath, defaultBranch)
		var others []string
		hasDefault := false
		for _, w := range worktrees {
			if w == defaultPath {
				hasDefault = true
			} else {
				others = append(others, w)
			}
		}
		if hasDefault {
			worktrees = append([]string{defaultPath}, others...)
		}
	}
	return worktrees
}

// CreatedWorktree describes the result of a successful CreateWorktree call,
// carrying what RollbackCreatedWorktree needs to undo the creation when a
// later spawn step fails (#2363).
type CreatedWorktree struct {
	// Path is the filesystem path of the created worktree.
	Path string
	// Branch is the branch the worktree was created at.
	Branch string
	// BranchForked is true only when CreateWorktree freshly forked the
	// branch from the repo's default branch (the `worktree add -b <branch>
	// <base>` path, taken when the branch existed neither locally nor on
	// the remote). It is false when the branch already existed locally and
	// false when the branch was checked out from a pre-existing remote
	// branch — in both cases the branch ref must survive a rollback.
	BranchForked bool
	// ForkPoint is the commit the branch pointed at immediately after
	// creation. Populated only when BranchForked is true;
	// RollbackCreatedWorktree deletes the branch only while its tip still
	// equals ForkPoint (i.e. no commits were made beyond the fork point).
	ForkPoint string
}

// CreateWorktree creates a new git worktree for a bare-layout repo.
// branchName must already be sanitised. Returns a CreatedWorktree describing
// what was created so callers can register a rollback (#2363).
func CreateWorktree(projectPath, branchName string) (CreatedWorktree, error) {
	bare := gitDir(projectPath)
	worktreePath := filepath.Join(projectPath, branchName)

	// Check if branch already exists locally.
	localErr := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
		"refs/heads/"+branchName).Run()
	if localErr == nil {
		// Branch exists locally — add worktree at that branch. The branch
		// pre-exists, so a rollback must never delete it.
		if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
			worktreePath, branchName).CombinedOutput(); err != nil {
			return CreatedWorktree{}, fmt.Errorf("worktree add: %w: %s", err, out)
		}
		return CreatedWorktree{Path: worktreePath, Branch: branchName}, nil
	}

	// Check if branch exists on remote.
	remoteErr := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
		"refs/remotes/origin/"+branchName).Run()
	if remoteErr == nil {
		// Track the remote branch. The local ref is created here (via -b),
		// but it checks out a pre-existing remote branch — a rollback must
		// not delete it (#2363: a failed `prism pr` keeps the PR branch and
		// loses only the worktree).
		if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
			worktreePath, "-b", branchName, "origin/"+branchName).CombinedOutput(); err != nil {
			return CreatedWorktree{}, fmt.Errorf("worktree add (remote): %w: %s", err, out)
		}
		return CreatedWorktree{Path: worktreePath, Branch: branchName}, nil
	}

	// New branch — fork from default.
	base := DefaultBranch(projectPath)
	if base == "" {
		base = "HEAD"
	}
	if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
		"-b", branchName, worktreePath, base).CombinedOutput(); err != nil {
		return CreatedWorktree{}, fmt.Errorf("worktree add (new): %w: %s", err, out)
	}
	// Record the fork point so a rollback can prove the branch still has no
	// commits of its own before deleting it. If the lookup fails, stay
	// conservative: report the branch as not-forked so a rollback keeps it.
	forkPoint, fpErr := runGit("--git-dir", bare, "rev-parse", "--verify",
		"refs/heads/"+branchName)
	if fpErr != nil || forkPoint == "" {
		return CreatedWorktree{Path: worktreePath, Branch: branchName}, nil
	}
	return CreatedWorktree{
		Path:         worktreePath,
		Branch:       branchName,
		BranchForked: true,
		ForkPoint:    forkPoint,
	}, nil
}

// RollbackCreatedWorktree undoes a CreateWorktree call after a later step of
// a spawn fails (#2363). It removes the created worktree and then deletes
// the branch — but only when the branch was freshly forked by that
// CreateWorktree call AND its tip is still at the fork point (no commits
// were made on it). Branches that pre-existed locally, were checked out
// from a pre-existing remote branch, or have accumulated commits are never
// deleted.
//
// The rollback is idempotent: a worktree or branch that is already gone is
// treated as success. All failures are collected into a single returned
// error so the caller can log them; callers must never let this error mask
// the original spawn failure.
func RollbackCreatedWorktree(projectPath string, c CreatedWorktree) error {
	bare := gitDir(projectPath)
	var errs []string

	if err := RemoveWorktree(projectPath, c.Path); err != nil {
		// `git worktree remove` fails when the worktree is already
		// unregistered or its directory was deleted out from under git.
		// Both are already-unwound states: prune stale bookkeeping, then
		// report a failure only if the worktree is still registered.
		_ = exec.Command("git", "--git-dir", bare, "worktree", "prune").Run()
		if isWorktreeRegistered(projectPath, c.Path) {
			errs = append(errs, fmt.Sprintf("remove worktree %q: %v", c.Path, err))
		}
	}

	if c.BranchForked && c.Branch != "" {
		tip, tipErr := runGit("--git-dir", bare, "rev-parse", "--verify",
			"refs/heads/"+c.Branch)
		switch {
		case tipErr != nil:
			// Branch already gone — idempotent success.
		case tip != c.ForkPoint:
			// Commits were made beyond the fork point — keep the branch.
		default:
			if delErr := ForceDeleteBranch(projectPath, c.Branch); delErr != nil {
				errs = append(errs, fmt.Sprintf("delete branch %q: %v", c.Branch, delErr))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("rollback created worktree: %s", strings.Join(errs, "; "))
	}
	return nil
}

// isWorktreeRegistered reports whether path is still registered as a
// worktree of the bare-layout repo at projectPath. Both sides are
// symlink-resolved where possible so paths under symlinked prefixes (e.g.
// /tmp → /private/tmp on Darwin) compare equal; a path whose leaf no longer
// exists falls back to resolving its parent.
func isWorktreeRegistered(projectPath, path string) bool {
	want := resolvePathBestEffort(path)
	for _, w := range Worktrees(projectPath) {
		if resolvePathBestEffort(w) == want {
			return true
		}
	}
	return false
}

// resolvePathBestEffort resolves symlinks in p, falling back to resolving
// the parent directory when p itself no longer exists, and to a lexical
// Clean when neither resolves.
func resolvePathBestEffort(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return filepath.Clean(p)
}

// convertToBareStepHook is called at each named step inside ConvertToBare.
// If it returns a non-nil error that error is returned as if the step itself
// failed, triggering rollback. The hook is nil in production and is set only
// by tests via the package-internal convertToBareTestHook variable.
var convertToBareStepHook func(step string) error

// ConvertToBare converts a regular git repo at dir to the prism bare+worktree
// layout in-place. progress receives human-readable step messages.
// Returns the path to the default-branch worktree on success.
func ConvertToBare(dir string, progress func(string)) (string, error) {
	barePath := filepath.Join(dir, ".bare")
	gitFile := filepath.Join(dir, ".git")
	origGit := filepath.Join(dir, ".git.orig")

	progress("converting " + filepath.Base(dir) + " to bare+worktree layout...")

	// Detect current branch.
	headOut, err := runGit("-C", dir, "symbolic-ref", "--short", "HEAD")
	defaultBranch := "main"
	if err == nil && headOut != "" {
		defaultBranch = headOut
	}
	progress("  default branch: " + defaultBranch)

	// Get remote URL.
	remoteURL, err := runGit("-C", dir, "remote", "get-url", "origin")
	if err != nil || remoteURL == "" {
		return "", fmt.Errorf("could not get remote URL")
	}
	progress("  remote: " + remoteURL)

	// Back up .git.
	if err := os.Rename(gitFile, origGit); err != nil {
		return "", fmt.Errorf("rename .git: %w", err)
	}

	// rollback restores the repo to its pre-conversion state. It is safe to
	// call at any point after .git has been renamed to .git.orig. After the
	// file-move loop completes, worktreePath and barePath may also be
	// populated; rollback removes them and moves files back to dir.
	//
	// rollback is idempotent: calling it when the state is already clean is a
	// no-op. If any individual step fails, the remaining steps are still
	// attempted, and the caller receives a combined error describing what
	// could not be cleaned up.
	var rollbackErr error
	rollback := func(worktreePath string) {
		var errs []string

		// If .git exists as a file/dir (the .bare pointer written mid-way),
		// remove it so we can restore .git.orig unconditionally.
		if info, e := os.Stat(gitFile); e == nil && !info.IsDir() {
			// It's the gitfile we wrote, not the original directory — remove it.
			if re := os.Remove(gitFile); re != nil {
				errs = append(errs, fmt.Sprintf("remove .git file: %v", re))
			}
		}

		// If worktreePath was populated, move entries back to dir.
		if worktreePath != "" {
			if entries, re := os.ReadDir(worktreePath); re == nil {
				for _, e := range entries {
					if e.Name() == ".git" {
						// The worktree .git file we may have written — just remove it.
						_ = os.Remove(filepath.Join(worktreePath, ".git"))
						continue
					}
					src := filepath.Join(worktreePath, e.Name())
					dst := filepath.Join(dir, e.Name())
					if re2 := os.Rename(src, dst); re2 != nil {
						errs = append(errs, fmt.Sprintf("move %s back: %v", e.Name(), re2))
					}
				}
			} else if !os.IsNotExist(re) {
				errs = append(errs, fmt.Sprintf("readdir worktree for rollback: %v", re))
			}
			// Remove the now-empty worktree directory (and any parent dirs for
			// slash-branch names like "feat/foo" whose "feat" dir was created).
			_ = os.Remove(worktreePath)
			// If branch contains a slash, also remove the top-level segment dir.
			_ = os.Remove(filepath.Join(dir, strings.SplitN(defaultBranch, "/", 2)[0]))
		}

		// Remove .bare.
		if _, e := os.Stat(barePath); e == nil {
			if re := os.RemoveAll(barePath); re != nil {
				errs = append(errs, fmt.Sprintf("remove .bare: %v", re))
			}
		}

		// Restore .git.orig → .git.
		if _, e := os.Stat(origGit); e == nil {
			if _, e2 := os.Stat(gitFile); e2 != nil {
				if re := os.Rename(origGit, gitFile); re != nil {
					errs = append(errs, fmt.Sprintf("rename .git.orig → .git: %v", re))
				}
			}
		}

		if len(errs) > 0 {
			rollbackErr = fmt.Errorf("rollback incomplete — manual recovery needed: %s",
				strings.Join(errs, "; "))
		}
	}

	// Clone bare.
	progress("  cloning bare repo (this may take a moment)...")
	if out, err := exec.Command("git", "clone", "--bare", remoteURL, barePath).
		CombinedOutput(); err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("bare clone failed: %w: %s", err, out)
	}
	progress("  bare clone done")

	// Configure fetch refspec.
	progress("  configuring remote tracking refs...")
	if out, err := exec.Command("git", "--git-dir", barePath, "config",
		"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").
		CombinedOutput(); err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("config fetch refspec: %w: %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", barePath, "fetch", "origin").
		CombinedOutput(); err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("fetch origin: %w: %s", err, out)
	}

	// Write .git pointer.
	if err := os.WriteFile(gitFile, []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("write .git: %w", err)
	}

	// Set upstream.
	_ = exec.Command("git", "--git-dir", barePath, "branch",
		"--set-upstream-to", "origin/"+defaultBranch, defaultBranch).Run()

	// Move working tree contents into <defaultBranch>/.
	// MkdirAll is used because the branch name may contain slashes (e.g. "feat/foo").
	progress("  moving working tree into " + defaultBranch + "/...")
	worktreePath := filepath.Join(dir, defaultBranch)
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("mkdir worktree: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		rollback("")
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("readdir: %w", err)
	}
	// topLevelBranchComponent is the first path segment of the branch name.
	// When the branch is "feat/foo", os.ReadDir returns "feat" as the entry
	// name (not "feat/foo"), so we skip based on that top-level segment.
	topLevelBranchComponent := strings.SplitN(defaultBranch, "/", 2)[0]
	skip := map[string]bool{".bare": true, ".git": true, ".git.orig": true, topLevelBranchComponent: true}
	for _, e := range entries {
		if skip[e.Name()] {
			continue
		}
		src := filepath.Join(dir, e.Name())
		dst := filepath.Join(worktreePath, e.Name())
		if err := os.Rename(src, dst); err != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", fmt.Errorf("move %s: %w", e.Name(), err)
		}
	}

	// Manually register the worktree instead of using `git worktree add --force`.
	// `git worktree add --force` refuses to adopt a directory that already contains
	// files, so we write the four worktree bookkeeping files by hand:
	//
	//   worktree/.git     – a gitfile whose content is "gitdir: <worktreesDir>",
	//                       telling git where this worktree's private state lives.
	//   worktrees/<b>/gitdir     – a relative path back to the worktree's .git
	//                              file, used by git to locate the working tree
	//                              from the bare repo side.
	//   worktrees/<b>/commondir  – contains "..", pointing at the bare repo so
	//                              the worktree shares objects, refs, and config.
	//   worktrees/<b>/HEAD       – the symbolic ref for the branch checked out in
	//                              this worktree (e.g. "ref: refs/heads/main").
	//
	// Git's internal worktrees directory uses the last path component of the branch
	// name as the directory name (i.e. "feat/foo" → worktrees/foo), matching the
	// behaviour of `git worktree add`. This is required: git worktree prune treats
	// any entry with a slash in its name as stale and removes it.
	//
	// This format has been validated against git 2.39, 2.43, and 2.47.
	progress("  registering worktree...")
	// Use the last path component as the worktrees entry name, matching git's own
	// naming convention for `git worktree add` with slash-containing branch names.
	worktreeEntryName := filepath.Base(defaultBranch)
	worktreesDir := filepath.Join(barePath, "worktrees", worktreeEntryName)
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("mkdir-worktrees-dir"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("mkdir worktrees dir: %w", err)
	}

	worktreeGitFile := filepath.Join(worktreePath, ".git")
	gitdirFile := filepath.Join(worktreesDir, "gitdir")

	// Write a relative gitdir pointer so the worktree remains functional if the
	// repo root is moved or accessed through a different path.
	relWorktreesDir, err := filepath.Rel(worktreePath, worktreesDir)
	if err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("compute worktree gitdir rel path: %w", err)
	}
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("write-worktree-git"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if err := os.WriteFile(worktreeGitFile,
		[]byte("gitdir: "+relWorktreesDir+"\n"), 0o644); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("write worktree .git: %w", err)
	}
	// Write a relative back-pointer from worktreesDir to the worktree .git file,
	// keeping the registration relocatable.
	relWorktreeGitFile, err := filepath.Rel(worktreesDir, worktreeGitFile)
	if err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("compute gitdir rel path: %w", err)
	}
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("write-gitdir"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if err := os.WriteFile(gitdirFile,
		[]byte(relWorktreeGitFile+"\n"), 0o644); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("write gitdir: %w", err)
	}
	// commondir must be a relative path from worktreesDir back to barePath.
	// Since worktreesDir is always exactly one level under barePath/worktrees/,
	// this is always "../.." regardless of slashes in the branch name.
	commondirRel, err := filepath.Rel(worktreesDir, barePath)
	if err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("compute commondir rel path: %w", err)
	}
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("write-commondir"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if err := os.WriteFile(filepath.Join(worktreesDir, "commondir"),
		[]byte(commondirRel+"\n"), 0o644); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("write commondir: %w", err)
	}
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("write-head"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if err := os.WriteFile(filepath.Join(worktreesDir, "HEAD"),
		[]byte("ref: refs/heads/"+defaultBranch+"\n"), 0o644); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("write HEAD: %w", err)
	}

	// Prune stale entries.
	_ = exec.Command("git", "--git-dir", barePath, "worktree", "prune").Run()

	// Populate the index from the branch HEAD so the worktree is not treated
	// as fully untracked. Manual worktree registration skips this step that
	// `git worktree add` normally performs.
	// Must use the worktree-specific gitdir (worktreesDir) so that read-tree
	// writes to .bare/worktrees/<branch>/index, not the bare repo's own index.
	progress("  populating index...")
	if convertToBareStepHook != nil {
		if hookErr := convertToBareStepHook("read-tree"); hookErr != nil {
			rollback(worktreePath)
			if rollbackErr != nil {
				progress("  WARNING: " + rollbackErr.Error())
			}
			return "", hookErr
		}
	}
	if out, err := exec.Command("git",
		"--git-dir", worktreesDir,
		"--work-tree", worktreePath,
		"read-tree", "HEAD",
	).CombinedOutput(); err != nil {
		rollback(worktreePath)
		if rollbackErr != nil {
			progress("  WARNING: " + rollbackErr.Error())
		}
		return "", fmt.Errorf("read-tree: %w: %s", err, out)
	}

	// Remove backed-up .git.orig only after all steps have succeeded, so that
	// rollback can restore it if read-tree (or a prior step) fails.
	for _, candidate := range []string{origGit, filepath.Join(worktreePath, ".git.orig")} {
		_ = os.RemoveAll(candidate)
	}

	progress("  done — worktree at " + worktreePath)
	return worktreePath, nil
}

var (
	reSep     = regexp.MustCompile(`[\s/_]+`)
	reInvalid = regexp.MustCompile(`[^a-z0-9\-.]`)
	reMulti   = regexp.MustCompile(`-{2,}`)
)

// SanitiseBranch converts free-form text to a valid git branch name,
// e.g. "Fix login Bug" → "fix-login-bug".
func SanitiseBranch(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = reSep.ReplaceAllString(s, "-")
	s = reInvalid.ReplaceAllString(s, "")
	s = reMulti.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	return s
}

// BranchExists returns true if branch exists in the bare-layout repo at projectPath.
func BranchExists(projectPath, branch string) bool {
	bare := gitDir(projectPath)
	return exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
		"refs/heads/"+branch).Run() == nil
}

// BranchMerged returns true if branch is fully merged into defaultBranch.
func BranchMerged(projectPath, branch, defaultBranch string) bool {
	bare := gitDir(projectPath)
	out, err := runGit("--git-dir", bare, "branch", "--merged", defaultBranch)
	if err != nil {
		return false
	}
	for _, b := range strings.Split(out, "\n") {
		if strings.TrimLeft(strings.TrimSpace(b), "* ") == branch {
			return true
		}
	}
	return false
}

// DeleteBranch deletes the branch in the bare-layout repo (safe delete).
// Returns an error if the branch is not merged; caller should use ForceDeleteBranch if needed.
func DeleteBranch(projectPath, branch string) error {
	bare := gitDir(projectPath)
	out, err := exec.Command("git", "--git-dir", bare, "branch", "-d", branch).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ForceDeleteBranch force-deletes the branch.
func ForceDeleteBranch(projectPath, branch string) error {
	bare := gitDir(projectPath)
	out, err := exec.Command("git", "--git-dir", bare, "branch", "-D", branch).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveWorktree removes a worktree from the bare-layout repo (force).
func RemoveWorktree(projectPath, worktreePath string) error {
	bare := gitDir(projectPath)
	out, err := exec.Command("git", "--git-dir", bare, "worktree", "remove",
		"--force", worktreePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// FetchRemote runs git fetch origin for a bare-layout repo.
func FetchRemote(projectPath string) error {
	bare := gitDir(projectPath)
	out, err := exec.Command("git", "--git-dir", bare, "fetch", "origin").CombinedOutput()
	if err != nil {
		return fmt.Errorf("fetch origin: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PRBranch uses the gh CLI to return the head branch name for a given PR number
// in the repo at projectPath.
func PRBranch(projectPath, prNumber string) (string, error) {
	bare := gitDir(projectPath)
	// Resolve the remote URL so gh knows which repo to query.
	remoteURL, err := runGit("--git-dir", bare, "remote", "get-url", "origin")
	if err != nil || remoteURL == "" {
		return "", fmt.Errorf("could not determine remote URL")
	}
	out, err := exec.Command("gh", "pr", "view", prNumber,
		"--repo", remoteURL, "--json", "headRefName", "--jq", ".headRefName").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("could not determine branch for PR %s", prNumber)
	}
	return branch, nil
}

// ResolveWorktreeGitDir returns the absolute path to worktreePath's private
// git-state directory (the entry under <bareRoot>/.bare/worktrees/<name>),
// by reading the authoritative pointer recorded in worktreePath's own .git
// file rather than deriving the name from filepath.Base(worktreePath).
//
// Git does not always name a worktree's registry entry after the worktree's
// basename — when two worktrees have colliding basenames (e.g. "feat/login"
// and "bugfix/login" both basename to "login"), git deduplicates with a
// numeric suffix ("login", "login1", ...). Deriving the name by basename
// therefore silently resolves to the WRONG worktree's git-state directory
// for every colliding worktree after the first (issue #2518).
//
// The worktree's .git file is always the single authoritative source: it
// contains a line "gitdir: <path>", where <path> may be absolute or relative
// to worktreePath. This function reads that line and resolves it to an
// absolute path.
//
// Returns an error if worktreePath's .git file is missing, unreadable, or
// malformed (no "gitdir: " line) — callers must treat that as a real error,
// not silently skip the resolution (see bwrap.go's os.Stat guard, which used
// to mask exactly this class of bug when combined with a derived-and-wrong
// path).
//
// If worktreePath is not a git worktree at all — i.e. its .git entry is a
// directory, as in a normal (non-bare+worktree) clone — this returns
// ErrNotAWorktree, a distinguishable sentinel that callers can check with
// errors.Is. That case is not an error condition for the caller: it means
// "there is no private git-state dir to resolve", not "resolution failed".
// A .git pointer FILE that is missing, unreadable, or malformed (no
// "gitdir: " line) is still a hard error — see the doc comment above.
func ResolveWorktreeGitDir(worktreePath string) (string, error) {
	gitFile := filepath.Join(worktreePath, ".git")

	if info, statErr := os.Lstat(gitFile); statErr == nil && info.IsDir() {
		return "", ErrNotAWorktree
	}

	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", gitFile, err)
	}

	const prefix = "gitdir: "
	var pointer string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, prefix) {
			pointer = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			break
		}
	}
	if pointer == "" {
		return "", fmt.Errorf("%s: no %q line found", gitFile, prefix)
	}

	if filepath.IsAbs(pointer) {
		return filepath.Clean(pointer), nil
	}
	return filepath.Clean(filepath.Join(worktreePath, pointer)), nil
}

// BareRoot walks up from worktreePath to find the nearest ancestor with a
// .bare subdirectory (the prism bare repo root). Returns empty string if not
// found. Starts at the parent of worktreePath because the path itself is a
// worktree directory, not the bare root.
func BareRoot(worktreePath string) string {
	p := filepath.Dir(worktreePath)
	for {
		if IsBareRepo(p) {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return ""
}

// DefaultBranchFromBareRoot is like DefaultBranch but the arg is the bare root
// (dir containing .bare), not the project path passed to DefaultBranch.
// Actually DefaultBranch already expects the project dir (parent of .bare),
// so this is an alias kept for clarity at call sites.
func DefaultBranchFromBareRoot(bareRoot string) string {
	return DefaultBranch(bareRoot)
}

// SymbolicRef returns the symbolic ref HEAD points to in worktree (e.g.
// "refs/heads/feat/my-thing"). Returns an error if HEAD is detached or the
// directory is not a git repo.
func SymbolicRef(worktree string) (string, error) {
	return runGit("-C", worktree, "symbolic-ref", "HEAD")
}

// ShortHash returns the abbreviated commit hash for HEAD in worktree.
// Returns an error if the directory is not a git repo.
func ShortHash(worktree string) (string, error) {
	return runGit("-C", worktree, "rev-parse", "--short", "HEAD")
}

// CloneWorktree clones repoURL into targetDir using the prism bare+worktree layout.
// progress receives human-readable step messages.
func CloneWorktree(repoURL, targetDir string, progress func(string)) error {
	bareDir := filepath.Join(targetDir, ".bare")
	gitFile := filepath.Join(targetDir, ".git")

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", targetDir, err)
	}

	// Clone bare.
	progress("cloning " + repoURL + "...")
	if out, err := exec.Command("git", "clone", "--bare", repoURL, bareDir).
		CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("git clone --bare: %w: %s", err, out)
	}

	// Configure fetch refspec.
	if out, err := exec.Command("git", "--git-dir", bareDir, "config",
		"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").
		CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("config fetch refspec: %w: %s", err, out)
	}

	if out, err := exec.Command("git", "--git-dir", bareDir, "fetch", "origin").
		CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("fetch origin: %w: %s", err, out)
	}

	// Write .git pointer.
	if err := os.WriteFile(gitFile, []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("write .git: %w", err)
	}

	// Determine default branch from HEAD file.
	headContent, err := os.ReadFile(filepath.Join(bareDir, "HEAD"))
	if err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("read HEAD: %w", err)
	}
	headStr := strings.TrimSpace(string(headContent))
	const prefix = "ref: refs/heads/"
	if !strings.HasPrefix(headStr, prefix) {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("unexpected HEAD format: %s", headStr)
	}
	defaultBranch := strings.TrimPrefix(headStr, prefix)

	worktreeDir := filepath.Join(targetDir, defaultBranch)

	// Check whether the remote repo is truly empty (no commits on any branch).
	// We inspect all refs under refs/heads/ rather than just the default branch:
	// if the remote has commits on some branch (e.g. master) but HEAD was
	// renamed to main without yet pushing a commit, for-each-ref returns a
	// non-empty result and we correctly keep the non-empty path, which will
	// fail loudly if the specific default branch ref is missing.
	refsOut, err := exec.Command("git", "--git-dir", bareDir, "for-each-ref",
		"--count=1", "refs/heads").Output()
	if err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("for-each-ref: %w", err)
	}
	isEmpty := strings.TrimSpace(string(refsOut)) == ""

	if isEmpty {
		// Empty repo — skip upstream tracking (no branch exists to track) and
		// create an orphan worktree so the user can make the first commit.
		progress("remote repository is empty — creating orphan worktree for '" + defaultBranch + "'...")
		if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
			"add", "--orphan", "-b", defaultBranch, worktreeDir).CombinedOutput(); err != nil {
			_ = os.RemoveAll(targetDir)
			return fmt.Errorf("worktree add --orphan: %w: %s", err, out)
		}
		progress("done — orphan worktree at " + worktreeDir)
		progress("  hint: make a first commit and run: git push -u origin " + defaultBranch)
		return nil
	}

	// Set upstream tracking.
	progress("setting up tracking for branch '" + defaultBranch + "'...")
	if out, err := exec.Command("git", "--git-dir", bareDir, "branch",
		"--set-upstream-to", "origin/"+defaultBranch, defaultBranch).
		CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("set upstream: %w: %s", err, out)
	}

	// Create default branch worktree.
	progress("creating worktree for branch '" + defaultBranch + "'...")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", worktreeDir, defaultBranch).CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("worktree add: %w: %s", err, out)
	}

	progress("done — " + targetDir + "/" + defaultBranch)
	return nil
}
