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

// gitDirRedirectFiles are the files in a worktree git dir that tell git
// where the common dir is, where the worktree is, and which extra config to
// read. Each git command in the worktree reads them, host-side commands
// included. A commondir that names a dir of the session gives git the
// config and the hooks of that dir. git writes none of them for a commit, a
// fetch, a push, a rebase, a pull, git status, or git gc.
var gitDirRedirectFiles = []string{"commondir", "gitdir", "config.worktree"}

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
//   - <worktree>/.git is a regular file, not a symlink.
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
	gitFile := filepath.Join(worktree, ".git")
	if fi, err := os.Lstat(gitFile); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("git write scope: %s is not a regular file, so no git dir is writable", gitFile)
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
	pointer = filepath.Clean(pointer)
	if pointer == gitFile || withRealDir(pointer) == withRealDir(gitFile) {
		return nil
	}
	return fmt.Errorf("git write scope: git dir %s belongs to %s, not to %s, so no git dir is writable",
		gitDir, pointer, gitFile)
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

// withRealDir returns path with the symlinks in its dir resolved, and its
// last part as it is. git writes the gitdir file with the resolved path of
// the worktree. The last part is the .git file of the worktree, which the
// session can replace with a symlink to the .git file of another worktree.
// So it must not be resolved. The session cannot replace the worktree dir
// or its parents (section 21b), so their resolution is safe.
func withRealDir(path string) string {
	dir := filepath.Dir(path)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Join(dir, filepath.Base(path))
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
//   - The worktree git dir: a session that can write its worktree. Section
//     21j takes back the redirect files in it (gitDirRedirectFiles).
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

// sbplRedirectFileDeny returns the section-21j clause: no write of the
// redirect files in the worktree git dir, which section 6e grants as a
// subpath. It returns "" when the scope has no git dir.
func (s gitWriteScope) sbplRedirectFileDeny() string {
	if s.GitDir == "" {
		return ""
	}
	var rules []string
	for _, f := range gitDirRedirectFiles {
		rules = append(rules, "(literal "+quoteSBPL(filepath.Join(s.GitDir, f))+")")
	}
	return "(deny file-write*\n  " + strings.Join(rules, "\n  ") + ")\n"
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

// gitUpstreamRemote is the remote that prism names in the upstream of a
// branch. Each prism layout clones its repo as origin.
const gitUpstreamRemote = "origin"

// ensureGitUpstream writes the upstream of the worktree branch into
// .bare/config on the host, when the repo config names none. The generated
// gitconfig sets push.autoSetupRemote, so the first git push of a branch
// with no upstream writes it into .bare/config. The session cannot write
// that file. With the upstream in place, a push in the session writes no
// config, and git pull and git status know the upstream.
//
// The upstream must be in .bare/config, and in no other file. Host-side git
// in the worktree, for example the push of prism review --rebase, reads
// .bare/config and not the per-session gitconfig. If it finds no upstream
// there, it writes one, and git push then refuses a branch that has an
// upstream in two files.
//
// It does nothing for a session that cannot write its worktree, and when
// there is no branch. git config reads and writes the config files only. It
// runs no hook and no fsmonitor command. The values come from the worktree
// path, so the session controls none of them.
func ensureGitUpstream(s gitWriteScope, readOnly bool) error {
	if readOnly || s.Branch == "" || s.CommonDir == "" {
		return nil
	}
	has, err := repoConfigHasUpstream(s.CommonDir, s.Branch)
	if err != nil {
		return fmt.Errorf("git write scope: read the upstream of %s: %w", s.Branch, err)
	}
	if has {
		return nil
	}
	for _, kv := range [][2]string{
		{"branch." + s.Branch + ".remote", gitUpstreamRemote},
		{"branch." + s.Branch + ".merge", "refs/heads/" + s.Branch},
	} {
		if err := runGitConfig(s.CommonDir, "--local", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git write scope: write the upstream of %s: %w", s.Branch, err)
		}
	}
	return nil
}

// repoConfigHasUpstream reports whether the repo config in commonDir names
// a merge ref for branch.
func repoConfigHasUpstream(commonDir, branch string) (bool, error) {
	err := runGitConfig(commonDir, "--local", "--get-all", "branch."+branch+".merge")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// runGitConfig runs git config with args on the repo in commonDir.
func runGitConfig(commonDir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitBareRootTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", append([]string{"--git-dir", commonDir, "config"}, args...)...).Run()
}
