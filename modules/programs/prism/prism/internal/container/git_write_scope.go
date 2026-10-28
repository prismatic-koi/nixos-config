package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/prismatic-koi/prism/internal/sessionname"
)

// gitWriteScope is the part of the shared git dir of a bare+worktree layout
// that a sandboxed session can write (#3293). The project root and the other
// worktrees stay read-only. See prism/docs/invariants/sandbox-git-write-scope.md.
type gitWriteScope struct {
	// CommonDir is <BareRoot>/.bare, the git dir that all worktrees share.
	CommonDir string
	// GitDir is the private git dir of the worktree,
	// <CommonDir>/worktrees/<id>. It is empty when the check of the
	// worktree git dir fails.
	GitDir string
	// Branch is the branch of the worktree. It is empty when the path of
	// the worktree below BareRoot is not a branch name.
	Branch string
}

// gitSharedWriteDirs are the dirs below CommonDir that git writes for a
// commit, a fetch, and a push in any worktree. fetch writes each
// remote-tracking ref, so the grant cannot name one ref.
var gitSharedWriteDirs = []string{"objects", "refs/remotes", "refs/tags", "logs/refs/remotes"}

// gitSharedWriteFiles are the files below CommonDir that git writes for a
// ref delete (fetch --prune) and for each update of a packed ref.
var gitSharedWriteFiles = []string{"packed-refs", "packed-refs.lock", "packed-refs.new"}

// gitStashRef is the stash ref. It is shared by all worktrees, so only the
// coordinator, the one prism session that can use git stash, can write it.
const gitStashRef = "refs/stash"

// gitGitDirFileLimit is the largest gitdir back-pointer file that
// resolveGitWriteScope reads.
const gitGitDirFileLimit = 4096

// resolveGitWriteScope returns the write scope of the session in worktree.
// bareRoot and worktree come from prism on the host. worktreeGitDir comes
// from the .git file in the worktree, which the session can write, so the
// function accepts it only when all of these are true:
//
//   - It is a direct child of <bareRoot>/.bare/worktrees.
//   - It is a directory, not a symlink.
//   - Its gitdir file is a regular file that names <worktree>/.git.
//
// The other gitdir files are in git dirs that the session cannot write. So
// a session cannot point its .git file at the git dir of another worktree.
//
// The branch is the path of worktree below bareRoot. prism names a worktree
// after its branch when it makes it (git.CreateWorktree). The HEAD file is
// not used, because the session can write it.
//
// A failed check leaves its field empty and adds a reason to the error. The
// bwrap isolator can use the same checks.
func resolveGitWriteScope(bareRoot, worktree, worktreeGitDir string) (gitWriteScope, error) {
	if bareRoot == "" || worktree == "" {
		return gitWriteScope{}, nil
	}
	bareRoot = filepath.Clean(bareRoot)
	worktree = filepath.Clean(worktree)
	scope := gitWriteScope{CommonDir: filepath.Join(bareRoot, ".bare")}
	var errs []error

	branch, err := gitBranchFromWorktreePath(bareRoot, worktree)
	if err != nil {
		errs = append(errs, err)
	} else {
		scope.Branch = branch
	}

	if err := checkWorktreeGitDir(scope.CommonDir, worktree, worktreeGitDir); err != nil {
		errs = append(errs, err)
	} else {
		scope.GitDir = filepath.Clean(worktreeGitDir)
	}
	return scope, errors.Join(errs...)
}

// gitScope returns the git write scope of the session of m.
func (m *Manager) gitScope() (gitWriteScope, error) {
	return resolveGitWriteScope(m.cfg.BareRoot, m.cfg.Worktree, m.cfg.WorktreeGitDir)
}

// gitBranchFromWorktreePath returns the path of worktree below bareRoot, in
// slash form, when it is a branch name that git accepts.
func gitBranchFromWorktreePath(bareRoot, worktree string) (string, error) {
	rel, err := filepath.Rel(bareRoot, worktree)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("git write scope: worktree %s is not below %s, so no branch ref is writable", worktree, bareRoot)
	}
	branch := filepath.ToSlash(rel)
	if err := sessionname.ValidateBranch(branch); err != nil {
		return "", fmt.Errorf("git write scope: no branch ref is writable: %w", err)
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return "", fmt.Errorf("git write scope: %q is not a branch name, so no branch ref is writable", branch)
		}
	}
	if strings.Contains(branch, "..") {
		return "", fmt.Errorf("git write scope: %q is not a branch name, so no branch ref is writable", branch)
	}
	return branch, nil
}

// checkWorktreeGitDir returns nil when gitDir is the git dir of worktree in
// commonDir. See resolveGitWriteScope for the conditions.
func checkWorktreeGitDir(commonDir, worktree, gitDir string) error {
	if gitDir == "" {
		return errors.New("git write scope: the worktree has no git dir, so no git dir is writable")
	}
	gitDir = filepath.Clean(gitDir)
	if filepath.Dir(gitDir) != filepath.Join(commonDir, "worktrees") {
		return fmt.Errorf("git write scope: git dir %s is not in %s, so no git dir is writable",
			gitDir, filepath.Join(commonDir, "worktrees"))
	}
	if fi, err := os.Lstat(gitDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("git write scope: git dir %s is not a directory, so no git dir is writable", gitDir)
	}
	pointer, err := readGitDirBackPointer(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return fmt.Errorf("git write scope: %w, so no git dir is writable", err)
	}
	if !filepath.IsAbs(pointer) {
		pointer = filepath.Join(gitDir, pointer)
	}
	want := filepath.Join(worktree, ".git")
	if samePath(filepath.Clean(pointer), want) {
		return nil
	}
	return fmt.Errorf("git write scope: git dir %s belongs to %s, not to %s, so no git dir is writable",
		gitDir, pointer, want)
}

// readGitDirBackPointer reads the gitdir file of a worktree git dir. The
// session can write that file, so the read does not follow a symlink, does
// not wait on a FIFO, and reads a regular file only.
func readGitDirBackPointer(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, gitGitDirFileLimit+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > gitGitDirFileLimit {
		return "", fmt.Errorf("%s is larger than %d bytes", path, gitGitDirFileLimit)
	}
	return strings.TrimSpace(string(data)), nil
}

// samePath reports whether a and b name the same path, as strings or after
// symlink resolution. git writes the gitdir file with the resolved path.
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// branchRefFiles returns the files below CommonDir that git writes to move
// the branch: the loose ref and its reflog, each with its lock file.
func (s gitWriteScope) branchRefFiles() []string {
	if s.Branch == "" {
		return nil
	}
	ref := "refs/heads/" + s.Branch
	return refWriteFiles(ref)
}

func refWriteFiles(ref string) []string {
	return []string{ref, ref + ".lock", "logs/" + ref, "logs/" + ref + ".lock"}
}

// sbplAllow returns the section-6e clauses of the sandbox-exec profile: a
// read of the whole project root, and a write of the git paths below.
//
//   - The shared dirs and files: every session.
//   - The branch ref of the worktree: a session that can write its
//     worktree. A review agent or an investigator cannot move the branch.
//   - The stash ref: the coordinator only. A worker that writes the stash
//     can change the files that the next git stash pop of the coordinator
//     writes in the main worktree.
//   - The worktree git dir: a session that can write its worktree.
//   - FETCH_HEAD in the worktree git dir: a session that cannot write its
//     worktree. A review agent runs in the worktree of its worker, and an
//     investigator runs in the worktree of the session that started it,
//     often main. HEAD and the index there decide what the next commit of
//     that worktree holds. git status, git diff, and git log work when git
//     cannot write the index.
//
// No other path in the project root is writable: not the other worktrees,
// not .bare/config, not .bare/hooks, and not the git dirs and branch refs of
// the other worktrees. A write of .bare/config or .bare/hooks runs code in
// each git command of each other worktree.
func (s gitWriteScope) sbplAllow(bareRoot string, readOnly, coordinator bool) string {
	var b strings.Builder
	b.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
	b.WriteString("  (subpath " + quoteSBPL(bareRoot) + "))\n")
	if s.CommonDir == "" {
		return b.String()
	}

	var rules []string
	for _, d := range gitSharedWriteDirs {
		rules = append(rules, "(subpath "+quoteSBPL(filepath.Join(s.CommonDir, d))+")")
	}
	files := append([]string{}, gitSharedWriteFiles...)
	if !readOnly {
		files = append(files, s.branchRefFiles()...)
		if coordinator {
			files = append(files, refWriteFiles(gitStashRef)...)
		}
	}
	for _, f := range files {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(s.CommonDir, f))+")")
	}
	switch {
	case s.GitDir == "":
	case readOnly:
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(s.GitDir, "FETCH_HEAD"))+")")
	default:
		rules = append(rules, "(subpath "+quoteSBPL(s.GitDir)+")")
	}
	b.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n  ")
	b.WriteString(strings.Join(rules, "\n  ") + ")\n")
	return b.String()
}

// sbplNodeDeny returns the section-21i clause: no unlink, create, mode,
// flags, or acl change of each dir node that section 6e grants as a
// subpath. A subpath covers its node. Without this deny, a session can move
// .bare/objects away and put a symlink to a dir of its choice at the path.
// Then each worktree reads its objects from that dir. It returns "" when the
// scope has no common dir.
func (s gitWriteScope) sbplNodeDeny() string {
	if s.CommonDir == "" {
		return ""
	}
	var rules []string
	for _, d := range gitSharedWriteDirs {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(s.CommonDir, d))+")")
	}
	if s.GitDir != "" {
		rules = append(rules, "(literal "+quoteSBPL(s.GitDir)+")")
	}
	return "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  " +
		strings.Join(rules, "\n  ") + ")\n"
}

// prepareGitWriteDirs makes, on the host, the dirs that git needs before
// the sandbox starts. If the parent dir of a ref or of a reflog is missing,
// git cannot write the ref: a fetch exits 255 and a commit exits 128. The
// sandbox cannot make the parent, because section 6e grants no write of
// .bare, .bare/logs, or .bare/refs/heads. A failure is logged, and the
// session still starts.
func prepareGitWriteDirs(s gitWriteScope, readOnly bool) {
	if s.CommonDir == "" {
		return
	}
	if fi, err := os.Stat(s.CommonDir); err != nil || !fi.IsDir() {
		return
	}
	dirs := append([]string{}, gitSharedWriteDirs...)
	if !readOnly && s.Branch != "" {
		parent := filepath.Dir(filepath.FromSlash(s.Branch))
		dirs = append(dirs,
			filepath.Join("refs", "heads", parent),
			filepath.Join("logs", "refs", "heads", parent))
	}
	for _, d := range dirs {
		path := filepath.Join(s.CommonDir, d)
		if err := os.MkdirAll(path, 0o755); err != nil {
			log.Printf("container: git write scope: create %s: %v", path, err)
		}
	}
}

// gitUpstreamRemote is the remote that prism names in the per-session
// upstream. Each prism layout clones its repo as origin.
const gitUpstreamRemote = "origin"

// gitUpstreamSection returns the gitconfig section that names the upstream
// of the worktree branch. The session cannot write .bare/config, so the
// upstream that push.autoSetupRemote writes there on the first push goes in
// the per-session GIT_CONFIG_GLOBAL instead. Then git push writes no config,
// and git pull and git status know the upstream.
//
// It returns "" when the repo config already names an upstream for the
// branch: git push refuses a branch with two upstreams. It also returns ""
// for a session that cannot write its worktree, and when there is no branch.
func gitUpstreamSection(s gitWriteScope, readOnly bool) string {
	if readOnly || s.Branch == "" || s.CommonDir == "" {
		return ""
	}
	has, err := repoConfigHasUpstream(s.CommonDir, s.Branch)
	if err != nil {
		log.Printf("container: git write scope: read the upstream of %s: %v", s.Branch, err)
		return ""
	}
	if has {
		return ""
	}
	return fmt.Sprintf("\n[branch %q]\n    remote = %s\n    merge = refs/heads/%s\n", s.Branch, gitUpstreamRemote, s.Branch)
}

// repoConfigHasUpstream reports whether the repo config in commonDir names
// a merge ref for branch. git config reads the config files only. It runs
// no hook and no fsmonitor command.
func repoConfigHasUpstream(commonDir, branch string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitBareRootTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir", commonDir, "config", "--local", "--get-all", "branch."+branch+".merge")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
