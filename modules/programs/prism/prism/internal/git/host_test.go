package git

// host_test.go — the host git commands of prism with a planted redirect.
//
// Each redirect test first runs a plain git command with the plant in place
// and checks that the payload runs (the control). It then runs the prism
// command and checks that the command succeeds and that the payload does not
// run. The payload only appends a line to a marker file in t.TempDir().

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostFixture is a scratch prism project: a local origin, a project root with
// .bare and a main worktree, and a worker worktree "feat".
type hostFixture struct {
	origin string
	root   string
	main   string
	worker string
	marker string
}

// hostPayloadHooks are the hooks that the evil git dir installs. Together
// they cover the host git commands of prism: a ref update, a checkout, a
// push, and a rebase.
var hostPayloadHooks = []string{
	"post-checkout", "reference-transaction", "pre-push", "pre-rebase",
	"post-rewrite", "pre-auto-gc", "pre-commit",
}

func newHostFixture(t *testing.T) hostFixture {
	t.Helper()
	isolateHostGitConfig(t)

	src := t.TempDir()
	initRepo(t, src, "main")
	origin := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	root := t.TempDir()
	if err := CloneWorktree(origin, root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if _, err := CreateWorktree(root, "feat"); err != nil {
		t.Fatalf("CreateWorktree feat: %v", err)
	}
	return hostFixture{
		origin: origin,
		root:   root,
		main:   filepath.Join(root, "main"),
		worker: filepath.Join(root, "feat"),
		marker: filepath.Join(t.TempDir(), "marker"),
	}
}

// isolateHostGitConfig keeps the global and system config of the developer
// out of the test, for example commit signing.
func isolateHostGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
}

// evilGitDir makes a git dir that the worker controls: a bare clone of the
// origin with a config that sets core.fsmonitor to a command, and the hooks
// in hostPayloadHooks. Each payload appends one line to f.marker.
func (f hostFixture) evilGitDir(t *testing.T) string {
	t.Helper()
	evil := filepath.Join(t.TempDir(), "evil.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", f.origin, evil).CombinedOutput(); err != nil {
		t.Fatalf("clone evil: %v\n%s", err, out)
	}
	cfg, err := os.OpenFile(filepath.Join(evil, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open evil config: %v", err)
	}
	fmt.Fprintf(cfg, "[core]\n\tfsmonitor = \"echo fsmonitor >> '%s'; false\"\n", f.marker)
	if err := cfg.Close(); err != nil {
		t.Fatalf("close evil config: %v", err)
	}
	hooks := filepath.Join(evil, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	for _, name := range hostPayloadHooks {
		script := fmt.Sprintf("#!/bin/sh\necho %s >> '%s'\nexit 0\n", name, f.marker)
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write hook %s: %v", name, err)
		}
	}
	return evil
}

// markerLines returns the payload lines in f.marker and removes the file.
func (f hostFixture) markerLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if err := os.Remove(f.marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	return strings.Fields(string(data))
}

// requireControlFires runs a plain git command (no pinning) and fails the
// test if the payload does not run. It proves that the plant works.
func (f hostFixture) requireControlFires(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	_, _ = cmd.CombinedOutput()
	if lines := f.markerLines(t); len(lines) == 0 {
		t.Fatalf("control: plain `git %s` in %s did not run the payload; the plant does not work", strings.Join(args, " "), dir)
	}
}

// requireNoPayload fails the test if the payload ran.
func (f hostFixture) requireNoPayload(t *testing.T, what string) {
	t.Helper()
	if lines := f.markerLines(t); len(lines) != 0 {
		t.Fatalf("%s ran a payload that the worker wrote: %v", what, lines)
	}
}

func writeCommondir(t *testing.T, gitDir, target string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(target+"\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
}

// TestHostGit_BareCommondirRedirect plants .bare/commondir. prism spawn runs
// `git worktree add` with GIT_DIR=.bare, which reads that file. git reads refs
// through the file even with GIT_COMMON_DIR set, so prism refuses the repo.
func TestHostGit_BareCommondirRedirect(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	writeCommondir(t, filepath.Join(f.root, ".bare"), evil)

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "worktree", "add", "-b", "control", filepath.Join(f.root, "control"), "main")

	if _, err := CreateWorktree(f.root, "spawned"); err == nil || !strings.Contains(err.Error(), "commondir") {
		t.Errorf("CreateWorktree = %v, want a refusal that names commondir", err)
	}
	if err := FetchRemote(f.root); err == nil {
		t.Error("FetchRemote: want a refusal")
	}
	if _, err := SymbolicRef(f.worker); err == nil {
		t.Error("SymbolicRef: want a refusal")
	}
	f.requireNoPayload(t, "the host git commands")

	// Remove the plant: the same commands work again.
	if err := os.Remove(filepath.Join(f.root, ".bare", "commondir")); err != nil {
		t.Fatalf("remove commondir: %v", err)
	}
	created, err := CreateWorktree(f.root, "spawned")
	if err != nil {
		t.Fatalf("CreateWorktree after the plant is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(created.Path, "README")); err != nil {
		t.Errorf("CreateWorktree did not check out the worktree: %v", err)
	}
	f.requireNoPayload(t, "CreateWorktree")
}

// TestHostGit_OwnCommondirRedirect plants .bare/worktrees/<own>/commondir,
// which a session can write in every isolation mode. prism refuses the
// commands of that worktree. The commands of the bare repo and of the other
// worktrees still run.
func TestHostGit_OwnCommondirRedirect(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	ownGitDir, err := ResolveWorktreeGitDir(f.worker)
	if err != nil {
		t.Fatalf("resolve worker git dir: %v", err)
	}
	writeCommondir(t, ownGitDir, evil)
	if err := os.WriteFile(filepath.Join(f.worker, "README"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("change README: %v", err)
	}

	f.requireControlFires(t, f.worker, "status", "--short")

	if _, err := Stat(f.worker); err == nil || !strings.Contains(err.Error(), "commondir") {
		t.Errorf("Stat = %v, want a refusal that names commondir", err)
	}
	if _, err := SymbolicRef(f.worker); err == nil {
		t.Error("SymbolicRef: want a refusal")
	}

	created, err := CreateWorktree(f.root, "spawned")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if ref, err := SymbolicRef(created.Path); err != nil || ref != "refs/heads/spawned" {
		t.Errorf("SymbolicRef(spawned) = %q, %v", ref, err)
	}
	if ref, err := SymbolicRef(f.main); err != nil || ref != "refs/heads/main" {
		t.Errorf("SymbolicRef(main) = %q, %v", ref, err)
	}
	if err := FetchRemote(f.root); err != nil {
		t.Errorf("FetchRemote: %v", err)
	}
	f.requireNoPayload(t, "the host git commands")
}

// TestHostGit_PinAloneIgnoresPlantedConfigAndHooks checks the pin without the
// commondir refusal: with GIT_DIR and GIT_COMMON_DIR set, git reads config
// and hooks from the pinned common dir, not from a planted commondir file.
func TestHostGit_PinAloneIgnoresPlantedConfigAndHooks(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	bareDir := filepath.Join(f.root, ".bare")
	ownGitDir, err := ResolveWorktreeGitDir(f.worker)
	if err != nil {
		t.Fatalf("resolve worker git dir: %v", err)
	}
	writeCommondir(t, ownGitDir, evil)
	writeCommondir(t, bareDir, evil)

	f.requireControlFires(t, f.worker, "status", "--short")

	worker := Repo{gitDir: ownGitDir, commonDir: bareDir, workTree: f.worker, dir: f.worker}
	_, _ = worker.CombinedOutput("status", "--short")
	f.requireNoPayload(t, "a pinned status in the worker worktree")

	bare := Repo{gitDir: bareDir, commonDir: bareDir, dir: bareDir}
	_, _ = bare.CombinedOutput("worktree", "add", "-b", "pinned", filepath.Join(f.root, "pinned"), "main")
	_, _ = bare.CombinedOutput("fetch", "origin")
	f.requireNoPayload(t, "pinned bare-repo commands")
}

// TestHostGit_WorktreeGitFileRedirect points <worktree>/.git at a git dir
// that the worker controls.
func TestHostGit_WorktreeGitFileRedirect(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	if out, err := exec.Command("git", "--git-dir", evil, "config", "core.bare", "false").CombinedOutput(); err != nil {
		t.Fatalf("unbare evil: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(f.worker, ".git"), []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
		t.Fatalf("plant .git: %v", err)
	}

	f.requireControlFires(t, f.worker, "status", "--short")

	repo, err := OpenDir(f.worker)
	if err != nil {
		t.Fatalf("OpenDir with the fix: %v", err)
	}
	if !samePath(repo.CommonDir(), filepath.Join(f.root, ".bare")) {
		t.Errorf("CommonDir = %q, want %s/.bare", repo.CommonDir(), f.root)
	}
	if _, err := Stat(f.worker); err != nil {
		t.Errorf("Stat: %v", err)
	}
	ref, err := SymbolicRef(f.worker)
	if err != nil || ref != "refs/heads/feat" {
		t.Errorf("SymbolicRef = %q, %v; want refs/heads/feat", ref, err)
	}
	f.requireNoPayload(t, "the worktree commands")
}

// TestHostGit_NestedBareInWorktree plants a .bare and a .git in a subdir of
// the worker worktree. A command whose cwd is in that subdir must use the
// real project root.
func TestHostGit_NestedBareInWorktree(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	sub := filepath.Join(f.worker, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.Symlink(evil, filepath.Join(f.worker, ".bare")); err != nil {
		t.Fatalf("plant .bare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.worker, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		t.Fatalf("plant .git: %v", err)
	}

	if got := BareRoot(sub); got != f.worker {
		t.Fatalf("control: BareRoot(sub) = %q, want the planted %q", got, f.worker)
	}
	if got := OuterBareRoot(sub); !samePath(got, f.root) {
		t.Errorf("OuterBareRoot(sub) = %q, want %q", got, f.root)
	}
	repo, err := OpenDir(sub)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	if !samePath(repo.CommonDir(), filepath.Join(f.root, ".bare")) {
		t.Errorf("CommonDir = %q, want %s/.bare", repo.CommonDir(), f.root)
	}
	if _, err := repo.Output("status", "--short"); err != nil {
		t.Errorf("status: %v", err)
	}
	f.requireNoPayload(t, "a command in the subdir")
}

// TestHostGit_HooksOff plants a hook straight in .bare/hooks. The sandbox
// mounts keep a session out of .bare/hooks; this test checks the second
// layer, core.hooksPath=/dev/null.
func TestHostGit_HooksOff(t *testing.T) {
	f := newHostFixture(t)
	script := fmt.Sprintf("#!/bin/sh\necho post-checkout >> '%s'\n", f.marker)
	hook := filepath.Join(f.root, ".bare", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	f.requireControlFires(t, f.root, "--git-dir", ".bare", "worktree", "add", "-b", "control", filepath.Join(f.root, "control"), "main")

	if _, err := CreateWorktree(f.root, "spawned"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	f.requireNoPayload(t, "CreateWorktree")
}

// TestOpenWorktree_ForgedBackPointer makes the gitdir file of the worker
// point at the main worktree. Commands for main must still use the git dir of
// main.
func TestOpenWorktree_ForgedBackPointer(t *testing.T) {
	f := newHostFixture(t)
	ownGitDir, err := ResolveWorktreeGitDir(f.worker)
	if err != nil {
		t.Fatalf("resolve worker git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ownGitDir, "gitdir"), []byte(filepath.Join(f.main, ".git")+"\n"), 0o644); err != nil {
		t.Fatalf("forge gitdir: %v", err)
	}
	repo, err := OpenWorktree(f.root, f.main)
	if err != nil {
		t.Fatalf("OpenWorktree main: %v", err)
	}
	if !samePath(repo.GitDir(), filepath.Join(f.root, ".bare", "worktrees", "main")) {
		t.Errorf("GitDir = %q, want the git dir of main", repo.GitDir())
	}
	ref, err := repo.Output("symbolic-ref", "HEAD")
	if err != nil || ref != "refs/heads/main" {
		t.Errorf("symbolic-ref HEAD = %q, %v; want refs/heads/main", ref, err)
	}
}

func TestOpenWorktree_NoBackPointerRefused(t *testing.T) {
	f := newHostFixture(t)
	ownGitDir, err := ResolveWorktreeGitDir(f.worker)
	if err != nil {
		t.Fatalf("resolve worker git dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ownGitDir, "gitdir"), []byte("/nonexistent/.git\n"), 0o644); err != nil {
		t.Fatalf("break gitdir: %v", err)
	}
	if _, err := OpenWorktree(f.root, f.worker); err == nil {
		t.Fatal("OpenWorktree: want an error when no git dir points back at the worktree")
	}
	if _, err := SymbolicRef(f.worker); err == nil {
		t.Error("SymbolicRef: want an error, not an unpinned command")
	}
}

func TestOpenBare_RefusesSymlinkAndFile(t *testing.T) {
	isolateHostGitConfig(t)
	target := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", target).CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	linkRoot := t.TempDir()
	if err := os.Symlink(target, filepath.Join(linkRoot, ".bare")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := OpenBare(linkRoot); err == nil {
		t.Error("OpenBare: want an error for a .bare symlink")
	}

	fileRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileRoot, ".bare"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatalf("write .bare file: %v", err)
	}
	if _, err := OpenBare(fileRoot); err == nil {
		t.Error("OpenBare: want an error for a .bare file")
	}
}

func TestOpenBare_RefusesWorktreeConfig(t *testing.T) {
	f := newHostFixture(t)
	bareDir := filepath.Join(f.root, ".bare")
	if out, err := exec.Command("git", "--git-dir", bareDir, "config", "extensions.worktreeConfig", "true").CombinedOutput(); err != nil {
		t.Fatalf("set worktreeConfig: %v\n%s", err, out)
	}
	if _, err := OpenBare(f.root); err == nil {
		t.Fatal("OpenBare: want ErrWorktreeConfig")
	}
	if _, err := OpenWorktree(f.root, f.worker); err == nil {
		t.Fatal("OpenWorktree: want ErrWorktreeConfig")
	}
	if _, err := CreateWorktree(f.root, "x"); err == nil {
		t.Error("CreateWorktree: want an error")
	}
}

func TestOpenDir_OutsidePrismProjectIsNotPinned(t *testing.T) {
	isolateHostGitConfig(t)
	dir := t.TempDir()
	initRepo(t, dir, "main")
	repo, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	if repo.Pinned() {
		t.Errorf("OpenDir pinned a plain repo: GitDir = %q", repo.GitDir())
	}
	if ref, err := repo.Output("symbolic-ref", "HEAD"); err != nil || ref != "refs/heads/main" {
		t.Errorf("symbolic-ref = %q, %v", ref, err)
	}
}

func TestOpenDir_ProjectRootIsBare(t *testing.T) {
	f := newHostFixture(t)
	repo, err := OpenDir(f.root)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	if repo.WorkTree() != "" || !samePath(repo.GitDir(), filepath.Join(f.root, ".bare")) {
		t.Errorf("OpenDir(root) = git dir %q, work tree %q; want the bare repo", repo.GitDir(), repo.WorkTree())
	}
}

func TestRepoEnv(t *testing.T) {
	base := []string{
		"PATH=/bin",
		"GIT_DIR=/evil",
		"GIT_COMMON_DIR=/evil",
		"GIT_WORK_TREE=/evil",
		"GIT_INDEX_FILE=/evil/index",
		"GIT_CONFIG_PARAMETERS='user.name=x'",
		"SSH_AUTH_SOCK=/run/agent",
	}
	r := Repo{gitDir: "/p/.bare/worktrees/w", commonDir: "/p/.bare", workTree: "/p/w"}
	got := map[string][]string{}
	for _, kv := range r.Env(base) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = append(got[k], v)
	}
	want := map[string]string{
		"GIT_DIR":        "/p/.bare/worktrees/w",
		"GIT_COMMON_DIR": "/p/.bare",
		"GIT_WORK_TREE":  "/p/w",
		"PATH":           "/bin",
		"SSH_AUTH_SOCK":  "/run/agent",
		"GIT_PAGER":      "cat",
	}
	for k, v := range want {
		if len(got[k]) != 1 || got[k][0] != v {
			t.Errorf("%s = %v, want [%s]", k, got[k], v)
		}
	}
	if _, ok := got["GIT_INDEX_FILE"]; ok {
		t.Error("GIT_INDEX_FILE was not removed")
	}
	params := got["GIT_CONFIG_PARAMETERS"]
	if len(params) != 1 || !strings.HasPrefix(params[0], "'user.name=x' ") {
		t.Fatalf("GIT_CONFIG_PARAMETERS = %v, want the inherited entry first", params)
	}
	for _, kv := range hostGitConfig {
		if !strings.Contains(params[0], "'"+kv[0]+"="+kv[1]+"'") {
			t.Errorf("GIT_CONFIG_PARAMETERS has no %s=%s", kv[0], kv[1])
		}
	}
}

// TestRepoEnv_GitReadsHardening checks that git reads the hardening config
// from the env, with priority over the repo config.
func TestRepoEnv_GitReadsHardening(t *testing.T) {
	f := newHostFixture(t)
	bareDir := filepath.Join(f.root, ".bare")
	if out, err := exec.Command("git", "--git-dir", bareDir, "config", "core.hooksPath", "/evil/hooks").CombinedOutput(); err != nil {
		t.Fatalf("set hooksPath: %v\n%s", err, out)
	}
	bare, err := OpenBare(f.root)
	if err != nil {
		t.Fatalf("OpenBare: %v", err)
	}
	for _, kv := range hostGitConfig {
		got, err := bare.Output("config", "--get", kv[0])
		if err != nil || got != kv[1] {
			t.Errorf("git config --get %s = %q, %v; want %q", kv[0], got, err, kv[1])
		}
	}
}

// TestEnvForDir_ChildGitIsPinned runs git from a child shell with the env of
// EnvForDir, as gh and glab do when they find the repo of the cwd.
func TestEnvForDir_ChildGitIsPinned(t *testing.T) {
	f := newHostFixture(t)
	evil := f.evilGitDir(t)
	if out, err := exec.Command("git", "--git-dir", evil, "config", "core.bare", "false").CombinedOutput(); err != nil {
		t.Fatalf("unbare evil: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(f.worker, ".git"), []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
		t.Fatalf("plant .git: %v", err)
	}

	f.requireControlFires(t, f.worker, "status", "--short")

	env, err := EnvForDir(f.worker, os.Environ())
	if err != nil {
		t.Fatalf("EnvForDir: %v", err)
	}
	child := exec.Command("sh", "-c", "git status --short && git remote get-url origin")
	child.Dir = f.worker
	child.Env = env
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("child git: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), f.origin) {
		t.Errorf("child git output %q does not name the origin %q", out, f.origin)
	}
	f.requireNoPayload(t, "a child git with the EnvForDir env")
}

// TestOpenDir_MissingDirIsAnError: a cleanup can ask for the branch of a
// worktree that is gone. The answer must be an error, not HEAD of the bare
// repo.
func TestOpenDir_MissingDirIsAnError(t *testing.T) {
	f := newHostFixture(t)
	missing := filepath.Join(f.root, "gone")
	if _, err := OpenDir(missing); err == nil {
		t.Error("OpenDir: want an error for a missing dir")
	}
	if ref, err := SymbolicRef(missing); err == nil {
		t.Errorf("SymbolicRef = %q, want an error for a missing dir", ref)
	}
}

// TestOpenDir_WorktreeWithoutGitFile finds the git dir of a worktree whose
// .git the worker removed, from the back-pointer.
func TestOpenDir_WorktreeWithoutGitFile(t *testing.T) {
	f := newHostFixture(t)
	if err := os.Remove(filepath.Join(f.worker, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}
	ref, err := SymbolicRef(f.worker)
	if err != nil || ref != "refs/heads/feat" {
		t.Errorf("SymbolicRef = %q, %v; want refs/heads/feat", ref, err)
	}
}

// TestCloneWorktree_RelativeTarget: `prism clone <url>` passes a relative
// target. The bare-repo commands run in the bare dir, so the paths that
// they get must be absolute.
func TestCloneWorktree_RelativeTarget(t *testing.T) {
	isolateHostGitConfig(t)
	src := t.TempDir()
	initRepo(t, src, "main")
	origin := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := CloneWorktree(origin, "proj", func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "proj", "main", "README")); err != nil {
		t.Errorf("main worktree not at proj/main: %v", err)
	}
	created, err := CreateWorktree("proj", "feat")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "proj", "feat", "README")); err != nil {
		t.Errorf("feat worktree not at proj/feat: %v", err)
	}
	if err := RemoveWorktree("proj", created.Path); err != nil {
		t.Errorf("RemoveWorktree: %v", err)
	}
}

// TestHostGit_NestedRootRefused plants a full .bare in a subdir of the worker
// worktree, with a smudge filter for every file. The hooks-off config does
// not turn off a filter driver, so the only defence is to refuse the root.
// A coordinator can name such a root through `/spawn {"repo": ...}`.
func TestHostGit_NestedRootRefused(t *testing.T) {
	f := newHostFixture(t)
	planted := filepath.Join(f.worker, "x")
	bareDir := filepath.Join(planted, ".bare")
	if out, err := exec.Command("git", "clone", "-q", "--bare", f.origin, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("plant .bare: %v\n%s", err, out)
	}
	cfg, err := os.OpenFile(filepath.Join(bareDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open planted config: %v", err)
	}
	fmt.Fprintf(cfg, "[filter \"evil\"]\n\tsmudge = \"echo smudge >> '%s'; cat\"\n", f.marker)
	if err := cfg.Close(); err != nil {
		t.Fatalf("close planted config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bareDir, "info"), 0o755); err != nil {
		t.Fatalf("mkdir info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bareDir, "info", "attributes"), []byte("* filter=evil\n"), 0o644); err != nil {
		t.Fatalf("write attributes: %v", err)
	}

	// The control uses the env of a pinned Repo for the planted root, so it
	// also shows that the hostGitConfig does not stop a filter driver.
	control := Repo{gitDir: bareDir, commonDir: bareDir, dir: bareDir}
	_, _ = control.CombinedOutput("worktree", "add", filepath.Join(planted, "control"), "main")
	if lines := f.markerLines(t); len(lines) == 0 {
		t.Fatal("control: a pinned worktree add in the planted root did not run the smudge filter; the plant does not work")
	}

	if _, err := OpenBare(planted); err == nil || !strings.Contains(err.Error(), "inside the prism project") {
		t.Errorf("OpenBare = %v, want a refusal for a root inside a project", err)
	}
	if _, err := CreateWorktree(planted, "spawned"); err == nil {
		t.Error("CreateWorktree: want a refusal")
	}
	if err := FetchRemote(planted); err == nil {
		t.Error("FetchRemote: want a refusal")
	}
	f.requireNoPayload(t, "the commands for the planted root")
}
