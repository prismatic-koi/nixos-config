// Package git provides helpers for querying git worktree state.
package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

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
// staged changes relative to HEAD. Returns a zero DiffStat on error or if
// the directory is not a git repo.
func Stat(dir string) DiffStat {
	if dir == "" {
		return DiffStat{}
	}

	// --numstat gives machine-readable "added\tdeleted\tfile" lines.
	// Collect both unstaged and staged, deduplicating by filename.
	seen := map[string]bool{}
	var total DiffStat

	for _, args := range [][]string{
		{"-C", dir, "diff", "--numstat", "HEAD"},
		{"-C", dir, "diff", "--numstat", "--cached"},
	} {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
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
	return total
}

// IsBareRepo returns true if dir contains a .bare entry (prism bare layout).
// Accepts both a directory (standard git clone --bare) and a regular file
// (gitdir pointer in alternate configurations) so the detection is consistent
// with cmd.deriveBareRoot.
func IsBareRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".bare"))
	return err == nil
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

// gitDir returns the path to the .bare directory for a bare-layout repo.
func gitDir(projectPath string) string {
	return filepath.Join(projectPath, ".bare")
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

// CreateWorktree creates a new git worktree for a bare-layout repo.
// branchName must already be sanitised. Returns the worktree path on success.
func CreateWorktree(projectPath, branchName string) (string, error) {
	bare := gitDir(projectPath)
	worktreePath := filepath.Join(projectPath, branchName)

	// Check if branch already exists locally.
	localErr := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
		"refs/heads/"+branchName).Run()
	if localErr == nil {
		// Branch exists locally — add worktree at that branch.
		if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
			worktreePath, branchName).CombinedOutput(); err != nil {
			return "", fmt.Errorf("worktree add: %w: %s", err, out)
		}
		return worktreePath, nil
	}

	// Check if branch exists on remote.
	remoteErr := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify",
		"refs/remotes/origin/"+branchName).Run()
	if remoteErr == nil {
		// Track the remote branch.
		if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
			worktreePath, "-b", branchName, "origin/"+branchName).CombinedOutput(); err != nil {
			return "", fmt.Errorf("worktree add (remote): %w: %s", err, out)
		}
		return worktreePath, nil
	}

	// New branch — fork from default.
	base := DefaultBranch(projectPath)
	if base == "" {
		base = "HEAD"
	}
	if out, err := exec.Command("git", "--git-dir", bare, "worktree", "add",
		"-b", branchName, worktreePath, base).CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add (new): %w: %s", err, out)
	}
	return worktreePath, nil
}

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

	rollback := func() {
		if _, e := os.Stat(origGit); e == nil {
			if _, e2 := os.Stat(gitFile); e2 != nil {
				_ = os.Rename(origGit, gitFile)
			}
		}
	}

	// Clone bare.
	progress("  cloning bare repo (this may take a moment)...")
	if out, err := exec.Command("git", "clone", "--bare", remoteURL, barePath).
		CombinedOutput(); err != nil {
		rollback()
		return "", fmt.Errorf("bare clone failed: %w: %s", err, out)
	}
	progress("  bare clone done")

	// Configure fetch refspec.
	progress("  configuring remote tracking refs...")
	if out, err := exec.Command("git", "--git-dir", barePath, "config",
		"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").
		CombinedOutput(); err != nil {
		rollback()
		return "", fmt.Errorf("config fetch refspec: %w: %s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", barePath, "fetch", "origin").
		CombinedOutput(); err != nil {
		rollback()
		return "", fmt.Errorf("fetch origin: %w: %s", err, out)
	}

	// Write .git pointer.
	if err := os.WriteFile(gitFile, []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		rollback()
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
		rollback()
		return "", fmt.Errorf("mkdir worktree: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		rollback()
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
			rollback()
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
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir worktrees dir: %w", err)
	}

	worktreeGitFile := filepath.Join(worktreePath, ".git")
	gitdirFile := filepath.Join(worktreesDir, "gitdir")

	// Write a relative gitdir pointer so the worktree remains functional if the
	// repo root is moved or accessed through a different path.
	relWorktreesDir, err := filepath.Rel(worktreePath, worktreesDir)
	if err != nil {
		return "", fmt.Errorf("compute worktree gitdir rel path: %w", err)
	}
	if err := os.WriteFile(worktreeGitFile,
		[]byte("gitdir: "+relWorktreesDir+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write worktree .git: %w", err)
	}
	// Write a relative back-pointer from worktreesDir to the worktree .git file,
	// keeping the registration relocatable.
	relWorktreeGitFile, err := filepath.Rel(worktreesDir, worktreeGitFile)
	if err != nil {
		return "", fmt.Errorf("compute gitdir rel path: %w", err)
	}
	if err := os.WriteFile(gitdirFile,
		[]byte(relWorktreeGitFile+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write gitdir: %w", err)
	}
	// commondir must be a relative path from worktreesDir back to barePath.
	// Since worktreesDir is always exactly one level under barePath/worktrees/,
	// this is always "../.." regardless of slashes in the branch name.
	commondirRel, err := filepath.Rel(worktreesDir, barePath)
	if err != nil {
		return "", fmt.Errorf("compute commondir rel path: %w", err)
	}
	if err := os.WriteFile(filepath.Join(worktreesDir, "commondir"),
		[]byte(commondirRel+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write commondir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(worktreesDir, "HEAD"),
		[]byte("ref: refs/heads/"+defaultBranch+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write HEAD: %w", err)
	}

	// Remove backed-up .git.orig.
	for _, candidate := range []string{origGit, filepath.Join(worktreePath, ".git.orig")} {
		_ = os.RemoveAll(candidate)
	}

	// Prune stale entries.
	_ = exec.Command("git", "--git-dir", barePath, "worktree", "prune").Run()

	// Populate the index from the branch HEAD so the worktree is not treated
	// as fully untracked. Manual worktree registration skips this step that
	// `git worktree add` normally performs.
	// Must use the worktree-specific gitdir (worktreesDir) so that read-tree
	// writes to .bare/worktrees/<branch>/index, not the bare repo's own index.
	progress("  populating index...")
	if out, err := exec.Command("git",
		"--git-dir", worktreesDir,
		"--work-tree", worktreePath,
		"read-tree", "HEAD",
	).CombinedOutput(); err != nil {
		return "", fmt.Errorf("read-tree: %w: %s", err, out)
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

	// Set upstream tracking.
	progress("setting up tracking for branch '" + defaultBranch + "'...")
	if out, err := exec.Command("git", "--git-dir", bareDir, "branch",
		"--set-upstream-to", "origin/"+defaultBranch, defaultBranch).
		CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("set upstream: %w: %s", err, out)
	}

	// Create default branch worktree.
	worktreeDir := filepath.Join(targetDir, defaultBranch)
	progress("creating worktree for branch '" + defaultBranch + "'...")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", worktreeDir, defaultBranch).CombinedOutput(); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("worktree add: %w: %s", err, out)
	}

	progress("done — " + targetDir + "/" + defaultBranch)
	return nil
}
