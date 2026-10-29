package git

// host.go — the one place where prism runs git on the host.
//
// A sandboxed session can write its own worktree and its own git dir
// (.bare/worktrees/<own>). Through these it can redirect the git dir that a
// host git command uses: it can point <worktree>/.git or the commondir file
// of its git dir at a dir that holds its own config and hooks. A host git
// command that lets git find its git dir then reads that config and can run a
// command that the session wrote.
//
// Repo closes the redirect. prism computes the git dir and the common dir on
// the host from the project root and sets GIT_DIR and GIT_COMMON_DIR, so git
// reads config and hooks from those dirs only. Env also turns off the config
// keys that run a command, or pins them to the git default, because a worker
// can still write the config of the common dir after a bwrap pin detaches
// (#3337).
//
// The rule is in docs/invariants/host-git-commands.md.
// TestHostGit_NoDirectGitExec fails if a non-test file outside this file
// starts git itself.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

// hostGitConfig is the config that every host git command gets as
// command-line config. Command-line config has priority over every config
// file. Each value was tested on git 2.56. docs/invariants/host-git-commands.md
// gives the reason for each key.
//
// An empty value does not always turn a key off. An empty core.sshCommand or
// gpg.ssh.program makes git run an empty command, so those keys get the git
// default.
//
// The submodule keys are necessary in addition to the pinned git dir: a
// submodule has its own git dir in the worktree, and its config is not
// prism's.
var hostGitConfig = append([][2]string{
	{"core.hooksPath", "/dev/null"},
	{"core.fsmonitor", "false"},
	{"submodule.recurse", "false"},
	{"fetch.recurseSubmodules", "false"},
	{"push.recurseSubmodules", "no"},
	{"diff.ignoreSubmodules", "all"},
	{"status.submoduleSummary", "false"},
	{"protocol.ext.allow", "never"},
	{"protocol.file.allow", "never"},
	{"core.sshCommand", "ssh"},
	{"credential.helper", ""},
	{"core.askPass", ""},
	{"core.editor", ":"},
	{"sequence.editor", ":"},
	{"core.alternateRefsCommand", ""},
	{"core.attributesFile", "/dev/null"},
	{"gpg.program", "gpg"},
	{"gpg.openpgp.program", "gpg"},
	{"gpg.x509.program", "gpgsm"},
	{"gpg.ssh.program", "ssh-keygen"},
	{"gpg.ssh.defaultKeyCommand", ""},
	{"interactive.diffFilter", "cat"},
	{"maintenance.auto", "false"},
}, hookEventsOff()...)

// hookEvents is each hook event of githooks(5) on git 2.56. git 2.56 runs a
// hook that config defines (hook.<name>.command and hook.<name>.event), and
// core.hooksPath does not stop it.
var hookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit",
	"pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push", "pre-receive",
	"update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc",
	"post-rewrite", "sendemail-validate", "fsmonitor-watchman",
	"p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// hookEventsOff returns hook.<event>.enabled = false for each hook event.
// A config that also sets hook.<event>.command defeats this (#3349).
func hookEventsOff() [][2]string {
	out := make([][2]string, 0, len(hookEvents))
	for _, event := range hookEvents {
		out = append(out, [2]string{"hook." + event + ".enabled", "false"})
	}
	return out
}

// emptyTree is the SHA-1 object name of the empty tree. git knows this object
// without a read of the object store.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// allowFileTransport makes Env allow the file transport. Only tests set it,
// through AllowFileTransportForTest.
var allowFileTransport atomic.Bool

// AllowFileTransportForTest makes host git allow a remote that is a local
// path, which test fixtures use as origin. Production host git refuses such a
// remote (protocol.file.allow = never). Call restore when the test ends.
func AllowFileTransportForTest() (restore func()) {
	prev := allowFileTransport.Swap(true)
	return func() { allowFileTransport.Store(prev) }
}

// hostGitConfigValues returns hostGitConfig with the values that Env uses.
func hostGitConfigValues() [][2]string {
	out := make([][2]string, len(hostGitConfig))
	copy(out, hostGitConfig)
	if allowFileTransport.Load() {
		for i := range out {
			if out[i][0] == "protocol.file.allow" {
				out[i][1] = "always"
			}
		}
	}
	return out
}

// hostGitStrippedEnv names the inherited variables that Env removes. Some
// change where git finds its repository. Env sets its own value for the
// others.
var hostGitStrippedEnv = []string{
	"GIT_DIR",
	"GIT_COMMON_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_IMPLICIT_WORK_TREE",
	"GIT_PREFIX",
	"GIT_NAMESPACE",
	"GIT_SHALLOW_FILE",
	"GIT_GRAFT_FILE",
	"GIT_REPLACE_REF_BASE",
	"GIT_NO_REPLACE_OBJECTS",
	"GIT_CONFIG",
	"GIT_PAGER",
	"GIT_ATTR_SOURCE",
	"GIT_PROXY_COMMAND",
	"GIT_NO_LAZY_FETCH",
}

// ErrWorktreeConfig is returned when the common config of a project turns
// on extensions.worktreeConfig. With it, git also reads config.worktree from
// the git dir, and a sandboxed session can write its own git dir.
var ErrWorktreeConfig = errors.New("extensions.worktreeConfig is on in the common config. With it, git reads config.worktree from a git dir that a sandboxed session can write, so prism does not run host git commands in this repo")

// ErrNotProjectRoot is returned when a project root is not a prism project
// root in config.json.
var ErrNotProjectRoot = errors.New("not a prism project root. A prism project root is a dir in a project_locations entry of the prism config.json, or a project_specific entry. A sandboxed session can write a dir outside these, so prism does not run git in it")

// worktreeConfigCheckTimeout bounds the config read of refuseWorktreeConfig.
// The read is local, so a longer run means that git hangs.
const worktreeConfigCheckTimeout = 5 * time.Second

// Repo is a repository location that prism computed on the host. Get one
// from OpenBare, OpenWorktree, or OpenDir. Run git through its methods only.
//
// A Repo with an empty gitDir is not pinned: git finds the repository from
// dir. OpenDir returns such a Repo only for a dir that is not in a prism
// project.
type Repo struct {
	gitDir    string
	commonDir string
	workTree  string
	dir       string
}

// GitDir returns the git dir that the commands of r use, or "" when r is not
// pinned.
func (r Repo) GitDir() string { return r.gitDir }

// CommonDir returns the common dir that the commands of r use, or "" when r
// is not pinned.
func (r Repo) CommonDir() string { return r.commonDir }

// WorkTree returns the work tree of r, or "" for a bare repo.
func (r Repo) WorkTree() string { return r.workTree }

// Pinned reports whether r sets GIT_DIR and GIT_COMMON_DIR.
func (r Repo) Pinned() bool { return r.gitDir != "" }

// OpenBare returns the Repo of the bare repo of the project at root. root is
// the dir that holds .bare. The caller must get root from a source that a
// sandboxed session cannot write: the session record, a project name under a
// configured project location, or OuterBareRoot.
//
// OpenBare refuses a root that is not a prism project root in config.json
// (config.Config.IsProjectRoot). A sandboxed session can write dirs outside
// every project, for example under ~/.npm, and so can make the .bare of such
// a root.
//
// OpenBare refuses a root inside another prism project. Such a root is in a
// worktree, so a sandboxed session can have made its .bare.
//
// If root has no .bare and root itself is a bare git dir, OpenBare uses root.
func OpenBare(root string) (Repo, error) {
	if root == "" {
		return Repo{}, fmt.Errorf("host git: project root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Repo{}, fmt.Errorf("host git: %w", err)
	}
	if outer := OuterBareRoot(abs); outer != "" && outer != abs {
		return Repo{}, fmt.Errorf("host git: %s is inside the prism project %s, so prism does not run git in it", abs, outer)
	}
	if err := requireProjectRoot(abs); err != nil {
		return Repo{}, err
	}
	gitDir := filepath.Join(abs, ".bare")
	if _, statErr := os.Lstat(gitDir); errors.Is(statErr, os.ErrNotExist) && IsRawBareGitDir(abs) {
		gitDir = abs
	}
	if err := requireRealDir(gitDir); err != nil {
		return Repo{}, fmt.Errorf("host git: bare repo: %w", err)
	}
	// git reads refs through a commondir file even when GIT_COMMON_DIR is
	// set. A bare repo has no such file, so one is a plant.
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); !errors.Is(err, os.ErrNotExist) {
		return Repo{}, fmt.Errorf("host git: %s/commondir exists. A bare repo has no commondir file, so prism does not run git in this repo. Remove the file", gitDir)
	}
	r := Repo{gitDir: gitDir, commonDir: gitDir, dir: gitDir}
	if err := r.refuseWorktreeConfig(); err != nil {
		return Repo{}, err
	}
	return r, nil
}

// OpenWorktree returns the Repo of worktree, a worktree of the project at
// root.
//
// It does not trust <worktree>/.git, which the session can write. The git
// dir is the entry in <root>/.bare/worktrees whose gitdir file points back at
// <worktree>/.git. A session can write the gitdir file of its own entry only.
// If two entries point at the worktree, <worktree>/.git chooses between them.
func OpenWorktree(root, worktree string) (Repo, error) {
	bare, err := OpenBare(root)
	if err != nil {
		return Repo{}, err
	}
	wt, err := filepath.Abs(worktree)
	if err != nil {
		return Repo{}, fmt.Errorf("host git: %w", err)
	}
	r, found, err := bare.worktree(wt)
	if err != nil {
		return Repo{}, err
	}
	if !found {
		return Repo{}, fmt.Errorf("host git: worktree %s: no git dir in %s/worktrees points back at it", wt, bare.gitDir)
	}
	return r, nil
}

// worktree returns the worktree Repo of wt in the bare repo bare. found is
// false when no git dir points back at wt. An error means that a git dir
// points back at wt, but prism does not run git with it.
func (bare Repo) worktree(wt string) (r Repo, found bool, err error) {
	worktreesDir := filepath.Join(bare.gitDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if errors.Is(err, os.ErrNotExist) {
		return Repo{}, false, nil
	}
	if err != nil {
		return Repo{}, false, fmt.Errorf("host git: worktree %s: %w", wt, err)
	}
	var matches []string
	for _, e := range entries {
		gitDir := filepath.Join(worktreesDir, e.Name())
		if requireRealDir(gitDir) != nil {
			continue
		}
		if pointsBackAt(gitDir, wt) {
			matches = append(matches, gitDir)
		}
	}

	var gitDir string
	switch len(matches) {
	case 0:
		return Repo{}, false, nil
	case 1:
		gitDir = matches[0]
	default:
		pointer, err := ResolveWorktreeGitDir(wt)
		if err != nil {
			return Repo{}, true, fmt.Errorf("host git: worktree %s: %d git dirs point back at it, and .git does not choose one: %w", wt, len(matches), err)
		}
		for _, m := range matches {
			if samePath(pointer, m) {
				gitDir = m
			}
		}
		if gitDir == "" {
			return Repo{}, true, fmt.Errorf("host git: worktree %s: %d git dirs point back at it, and .git names none of them", wt, len(matches))
		}
	}
	// git reads refs through the commondir file even when GIT_COMMON_DIR is
	// set, so the file must name the bare repo.
	if !commondirIs(gitDir, bare.gitDir) {
		return Repo{}, true, fmt.Errorf("host git: worktree %s: %s/commondir does not point at %s, so prism does not run git in this worktree", wt, gitDir, bare.gitDir)
	}
	return Repo{gitDir: gitDir, commonDir: bare.gitDir, workTree: wt, dir: wt}, true, nil
}

// commondirIs reports whether the commondir file of gitDir names commonDir.
// A relative path in it is relative to gitDir.
func commondirIs(gitDir, commonDir string) bool {
	file := filepath.Join(gitDir, "commondir")
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return samePath(target, commonDir)
}

// pointsBackAt reports whether the gitdir file of gitDir names
// <worktree>/.git. git writes the file at `git worktree add`. A relative path
// in it is relative to gitDir.
func pointsBackAt(gitDir, worktree string) bool {
	backFile := filepath.Join(gitDir, "gitdir")
	info, err := os.Lstat(backFile)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(backFile)
	if err != nil {
		return false
	}
	back := strings.TrimSpace(string(data))
	if back == "" {
		return false
	}
	if !filepath.IsAbs(back) {
		back = filepath.Join(gitDir, back)
	}
	return filepath.Base(back) == ".git" && samePath(filepath.Dir(back), worktree)
}

// OpenDir returns the Repo for a command that prism runs in dir, for example
// the cwd of the process. dir must exist.
//
// In a prism project, OpenDir finds the project root with OuterBareRoot, so a
// .bare that a session puts in its own worktree has no effect. It returns
// the worktree Repo when dir is in a worktree, and the bare Repo when it is
// not. If a dir between the root and dir is a worktree that prism refuses,
// OpenDir returns the error: it does not fall back to the bare repo or to a
// command that is not pinned.
//
// Outside a prism project, OpenDir returns a Repo that is not pinned. No
// worker runs in such a dir.
func OpenDir(dir string) (Repo, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Repo{}, fmt.Errorf("host git: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Repo{}, fmt.Errorf("host git: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return Repo{}, fmt.Errorf("host git: %w", err)
	}
	root := OuterBareRoot(abs)
	if root == "" {
		return Repo{dir: abs}, nil
	}
	bare, err := OpenBare(root)
	if err != nil {
		return Repo{}, err
	}
	if abs == root {
		return bare, nil
	}
	// Try each dir from just below root down to abs. A session cannot write
	// a dir between root and its worktree, so the outermost match is the
	// worktree.
	var candidates []string
	for p := abs; p != root; p = filepath.Dir(p) {
		candidates = append([]string{p}, candidates...)
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, c := range candidates {
		r, found, err := bare.worktree(c)
		if err != nil {
			return Repo{}, err
		}
		if found {
			return r, nil
		}
		if _, statErr := os.Lstat(filepath.Join(c, ".git")); statErr == nil {
			return Repo{}, fmt.Errorf("host git: %s has a .git, but no git dir in %s/worktrees points back at it", c, bare.gitDir)
		}
	}
	return bare, nil
}

// OuterBareRoot returns the outermost dir at or above path that has a .bare
// entry, or "" if there is none.
//
// Use it, not BareRoot, when path can be in a worktree: a session can make a
// .bare in its own worktree, but not above the project root.
func OuterBareRoot(path string) string {
	found := ""
	p := filepath.Clean(path)
	for {
		if IsBareRepo(p) {
			found = p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return found
		}
		p = parent
	}
}

// EnvForDir returns base with the host git env of OpenDir(dir). Use it for a
// gh or glab command that finds its repo with git in dir ("" means the cwd).
func EnvForDir(dir string, base []string) ([]string, error) {
	r, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	return r.Env(base), nil
}

// Env returns base without the inherited repo-location variables, with
// GIT_DIR, GIT_COMMON_DIR, and GIT_WORK_TREE of r, and with hostGitConfig
// added to GIT_CONFIG_PARAMETERS. A git child of a gh or glab command gets
// the same values.
//
// For a pinned Repo, Env also sets GIT_ATTR_SOURCE to the empty tree, so git
// reads no .gitattributes file and a planted filter, diff, or merge driver
// does not run. git stops with an error if GIT_ATTR_SOURCE is set and there
// is no repository, so a Repo that is not pinned does not get it.
func (r Repo) Env(base []string) []string {
	return hostEnv(base, r.gitDir, r.commonDir, r.workTree)
}

// Command returns a git command for args that runs in r.
func (r Repo) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.Env(os.Environ())
	return cmd
}

// Output runs git args in r and returns stdout without surrounding space.
func (r Repo) Output(args ...string) (string, error) {
	out, err := r.Command(context.Background(), args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CombinedOutput runs git args in r and returns stdout and stderr.
func (r Repo) CombinedOutput(args ...string) ([]byte, error) {
	return r.Command(context.Background(), args...).CombinedOutput()
}

// Run runs git args in r.
func (r Repo) Run(args ...string) error {
	return r.Command(context.Background(), args...).Run()
}

// CloneCommand returns a `git clone` command that runs in dir ("" means the
// cwd). A clone makes a new repository, so there is no git dir to pin. The
// command gets the hostGitConfig.
func CloneCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"clone"}, args...)...)
	cmd.Dir = dir
	cmd.Env = hostEnv(os.Environ(), "", "", "")
	return cmd
}

// refuseWorktreeConfig returns ErrWorktreeConfig when the common config of r
// turns on extensions.worktreeConfig.
func (r Repo) refuseWorktreeConfig() error {
	ctx, cancel := context.WithTimeout(context.Background(), worktreeConfigCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(r.commonDir, "config"),
		"--no-includes", "--type=bool", "--get", "extensions.worktreeConfig")
	cmd.Dir = r.commonDir
	cmd.Env = hostEnv(os.Environ(), r.commonDir, r.commonDir, "")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// The key is not set.
			return nil
		}
		return fmt.Errorf("host git: read extensions.worktreeConfig in %s: %v: %s", r.commonDir, err, strings.TrimSpace(stderr.String()))
	}
	if strings.TrimSpace(string(out)) == "true" {
		return fmt.Errorf("host git: %s: %w", r.commonDir, ErrWorktreeConfig)
	}
	return nil
}

func hostEnv(base []string, gitDir, commonDir, workTree string) []string {
	out := make([]string, 0, len(base)+8)
	params := ""
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_PARAMETERS" {
			params = v
			continue
		}
		if isStrippedGitEnv(k) {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range hostGitConfigValues() {
		if params != "" {
			params += " "
		}
		params += "'" + kv[0] + "=" + kv[1] + "'"
	}
	// GIT_PROXY_COMMAND: git uses the first core.gitProxy that matches, so a
	// command-line value cannot replace a planted one. A set
	// GIT_PROXY_COMMAND makes git skip core.gitProxy, and an empty one means
	// no proxy.
	//
	// GIT_NO_LAZY_FETCH: a planted partial-clone config makes git fetch a
	// missing object from a remote that the config names.
	out = append(out,
		"GIT_CONFIG_PARAMETERS="+params,
		"GIT_PAGER=cat",
		"GIT_PROXY_COMMAND=",
		"GIT_NO_LAZY_FETCH=1",
	)
	if gitDir != "" {
		out = append(out, "GIT_DIR="+gitDir, "GIT_COMMON_DIR="+commonDir, "GIT_ATTR_SOURCE="+emptyTree)
	}
	if workTree != "" {
		out = append(out, "GIT_WORK_TREE="+workTree)
	}
	return out
}

func isStrippedGitEnv(k string) bool {
	for _, name := range hostGitStrippedEnv {
		if k == name {
			return true
		}
	}
	return false
}

// requireProjectRoot returns ErrNotProjectRoot unless the absolute path root
// is a prism project root in config.json. It reads config.json each time, so
// a long-running process sees a config that a switch rewrote.
func requireProjectRoot(root string) error {
	if !config.LoadFresh().IsProjectRoot(root) {
		return fmt.Errorf("host git: %s: %w", root, ErrNotProjectRoot)
	}
	return nil
}

// requireRealDir returns an error unless p is a directory and not a symlink.
// GIT_DIR that names a file makes git read the file as a .git file.
func requireRealDir(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	return nil
}

// samePath reports whether a and b name the same path after symlinks are
// resolved. git can record a resolved path (for example /private/tmp on
// macOS) while prism holds the unresolved one.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
