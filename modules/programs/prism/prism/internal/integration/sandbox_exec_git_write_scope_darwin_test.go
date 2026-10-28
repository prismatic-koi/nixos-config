//go:build darwin

package integration_test

// sandbox_exec_git_write_scope_darwin_test.go — the git write scope of the
// sandbox-exec profile (#3293): section 6e, which grants a read of the
// project root and a write of the git paths only, section 21i, which keeps
// the git dir nodes in place, and the host-side dirs that Prepare makes.
//
// The fixture is a real prism bare+worktree layout under HOME with two
// worktrees, main and feature, and a bare remote in the per-session TMPDIR.
// A Nix git runs in the sandbox, in a worktree, as in a session.
//
// The positive tests run git under the production profile. The negative
// tests change one rule and expect the opposite result.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
)

// gitScopeFixture is a prism layout and a prepared profile for the session
// in one worktree of it.
type gitScopeFixture struct {
	m        *container.Manager
	nixGit   string
	bareRoot string
	worktree string
	remote   string
	session  string
	tmpDir   string
	prepared preparedProfile
}

// hostGit runs the Nix git on the host, outside the sandbox, with no user
// config.
func hostGit(t *testing.T, nixGit, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=test-user", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
		"-c", "maintenance.auto=false", "-c", "gc.auto=0",
	}, args...)
	cmd := exec.Command(nixGit, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitScopeFixture makes the layout, the same way prism clones a repo
// (git.CloneWorktree) and makes a worktree (git.CreateWorktree), and
// prepares the profile of the session in the worktree of branch. The
// remote gets one more commit on main after the clone, so a fetch and a
// pull have work to do.
func newGitScopeFixture(t *testing.T, branch, role string, readOnly bool) *gitScopeFixture {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixGit := requireNixGit(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3293-git-scope-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })

	seed := filepath.Join(wrap, "seed")
	bareRoot := filepath.Join(wrap, "repo")
	bare := filepath.Join(bareRoot, ".bare")
	hostGit(t, nixGit, wrap, "init", "-q", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	hostGit(t, nixGit, seed, "add", "README.md")
	hostGit(t, nixGit, seed, "commit", "-q", "-m", "seed")

	hostGit(t, nixGit, wrap, "clone", "-q", "--bare", seed, bare)
	hostGit(t, nixGit, wrap, "--git-dir", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	hostGit(t, nixGit, wrap, "--git-dir", bare, "fetch", "-q", "origin")
	hostGit(t, nixGit, wrap, "--git-dir", bare, "branch", "-q", "--set-upstream-to", "origin/main", "main")
	hostGit(t, nixGit, wrap, "--git-dir", bare, "worktree", "add", "-q", filepath.Join(bareRoot, "main"), "main")
	hostGit(t, nixGit, wrap, "--git-dir", bare, "worktree", "add", "-q", "-b", "feature", filepath.Join(bareRoot, "feature"), "main")

	worktree := filepath.Join(bareRoot, branch)
	gitDir, err := git.ResolveWorktreeGitDir(worktree)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir: %v", err)
	}
	m := container.New(container.Config{
		SessionName:      "repo@" + branch,
		InstanceID:       "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:         worktree,
		BareRoot:         bareRoot,
		WorktreeGitDir:   gitDir,
		AgentRole:        role,
		WorktreeReadOnly: readOnly,
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
	})
	prepared, _ := preparePositiveProfile(t, m)
	session, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	tmpDir := sessionTmpDirOf(t, m)

	// The remote is in the per-session TMPDIR, so that a push from the
	// sandbox can write it.
	remote := filepath.Join(tmpDir, "remote.git")
	hostGit(t, nixGit, wrap, "clone", "-q", "--bare", seed, remote)
	if err := os.WriteFile(filepath.Join(seed, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatalf("write later.txt: %v", err)
	}
	hostGit(t, nixGit, seed, "add", "later.txt")
	hostGit(t, nixGit, seed, "commit", "-q", "-m", "later")
	hostGit(t, nixGit, seed, "push", "-q", remote, "main")
	hostGit(t, nixGit, wrap, "--git-dir", bare, "remote", "set-url", "origin", remote)

	return &gitScopeFixture{
		m: m, nixGit: nixGit, bareRoot: bareRoot, worktree: worktree,
		remote: remote, session: session, tmpDir: tmpDir, prepared: prepared,
	}
}

// run runs script with Nix bash under the profile at profilePath, in the
// worktree, with the git env of a session. BARE is the project root and WT
// is the worktree.
func (f *gitScopeFixture) run(t *testing.T, profilePath, script string) ([]byte, error) {
	t.Helper()
	nixBash := requireNixBash(t)
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", script)
	cmd.Dir = f.worktree
	cmd.Env = append(os.Environ(),
		"BARE="+f.bareRoot,
		"WT="+f.worktree,
		"PATH="+filepath.Dir(f.nixGit)+":"+requireNixCoreutilsDir(t),
		"TMPDIR="+f.tmpDir,
		"XDG_CONFIG_HOME="+f.session,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		// The host signing key is not part of the rule under test, and
		// maintenance runs detached after the script ends.
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=gc.auto", "GIT_CONFIG_VALUE_2=0",
	)
	cmd.Env = append(cmd.Env, container.SessionWorkDirGitEnv(f.session, "")[0])
	return cmd.CombinedOutput()
}

func (f *gitScopeFixture) bare(parts ...string) string {
	return filepath.Join(append([]string{f.bareRoot, ".bare"}, parts...)...)
}

// workerScript does the git work of a worker, then tries each plant. Each
// step has its own exit code.
const workerScript = `set -u
fail() { echo "step $1 failed" >&2; exit "$1"; }
git status --short || fail 10
echo change > change.txt && git add change.txt && git commit -q -m change || fail 11
git fetch -q origin || fail 12
out=$(git push 2>&1) || { echo "$out" >&2; fail 13; }
case "$out" in *"upstream branch configuration"*) echo "$out" >&2; fail 14;; esac
line=$(git status -sb | head -n 1)
case "$line" in *origin/feature*) ;; *) echo "$line" >&2; fail 15;; esac
git rebase -q origin/main || fail 16
git push -q --force-with-lease || fail 17
git worktree list >/dev/null || fail 18
echo plant > "$BARE/main/plant.txt" && fail 20
mkdir "$BARE/main/.pi" && fail 21
mkdir "$BARE/.agents" && fail 22
echo plant >> "$BARE/main/README.md" && fail 23
echo '[core]' >> "$BARE/.bare/config" && fail 24
echo plant > "$BARE/.bare/hooks/post-checkout" && fail 25
git update-ref refs/heads/main HEAD && fail 26
git update-ref refs/stash HEAD && fail 27
echo plant > "$BARE/.bare/worktrees/main/HEAD" && fail 28
mv "$BARE/.bare/objects" "$WT/objects-moved" && fail 29
git config branch.feature.description plant && fail 30
gd=$(git rev-parse --git-dir)
echo plant >> "$gd/commondir" && fail 31
echo plant >> "$gd/gitdir" && fail 32
echo '[core]' > "$gd/config.worktree" && fail 33
echo ../.. > "$gd/commondir.new" && mv "$gd/commondir.new" "$gd/commondir" && fail 34
echo "gitdir: $BARE/.bare/worktrees/main" > "$WT/.git" && fail 35
mv "$WT/.git" "$WT/.git.moved" && fail 36
rm -f "$WT/.git" && fail 37
cp "$WT/.git" "$WT/.git.new" && mv -f "$WT/.git.new" "$WT/.git" && fail 38
chmod 600 "$WT/.git" && fail 39
exit 0`

// TestSandboxExecGitWriteScope_WorkerGitWorksAndPlantsFail: under the
// production profile a worker commits, fetches, pushes, rebases, and lists
// the worktrees. It cannot write in the main worktree, at the project root,
// in .bare/config, in .bare/hooks, in the git dir of main, or in the branch
// ref of main or the stash ref, and it cannot move .bare/objects. In its own
// git dir, it cannot write commondir, gitdir, or config.worktree. It cannot
// write, move, remove, replace, or chmod the .git file of its worktree.
func TestSandboxExecGitWriteScope_WorkerGitWorksAndPlantsFail(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	configBefore, err := os.ReadFile(f.bare("config"))
	if err != nil {
		t.Fatalf("read .bare/config: %v", err)
	}
	gitFile := filepath.Join(f.worktree, ".git")
	gitFileBefore, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatalf("read the .git file: %v", err)
	}
	gitFileInfo, err := os.Lstat(gitFile)
	if err != nil {
		t.Fatalf("Lstat the .git file: %v", err)
	}
	redirectBefore := map[string]string{}
	for _, name := range []string{"commondir", "gitdir"} {
		data, err := os.ReadFile(f.bare("worktrees", "feature", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		redirectBefore[name] = string(data)
	}
	mainBefore := hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "rev-parse", "refs/heads/main")

	profilePath := writeAugmentedPositiveProfile(t, f.prepared)
	out, err := f.run(t, profilePath, workerScript)
	if err != nil {
		t.Fatalf("worker git work failed (exit 10-18) or a plant was not blocked (exit 20-39).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}

	if configAfter, _ := os.ReadFile(f.bare("config")); string(configAfter) != string(configBefore) {
		t.Errorf(".bare/config changed:\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
	for name, before := range redirectBefore {
		if after, _ := os.ReadFile(f.bare("worktrees", "feature", name)); string(after) != before {
			t.Errorf("%s of the git dir changed:\nbefore: %q\nafter: %q", name, before, after)
		}
	}
	if after, _ := os.ReadFile(gitFile); string(after) != string(gitFileBefore) {
		t.Errorf("the .git file changed:\nbefore: %q\nafter: %q", gitFileBefore, after)
	}
	if fi, err := os.Lstat(gitFile); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != gitFileInfo.Mode().Perm() {
		t.Errorf("the .git file is not the same regular file with mode %v after the blocked plants: %v", gitFileInfo.Mode().Perm(), err)
	}
	for _, p := range []string{
		f.bare("worktrees", "feature", "config.worktree"),
		filepath.Join(f.bareRoot, "main", "plant.txt"),
		filepath.Join(f.bareRoot, "main", ".pi"),
		filepath.Join(f.bareRoot, ".agents"),
		f.bare("hooks", "post-checkout"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists after a blocked plant", p)
		}
	}
	if got := hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Errorf("refs/heads/main moved from %s to %s", mainBefore, got)
	}
	local := hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "rev-parse", "refs/heads/feature")
	if pushed := hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.remote, "rev-parse", "refs/heads/feature"); pushed != local {
		t.Errorf("remote feature = %s, local feature = %s: the push did not land", pushed, local)
	}
	if fi, err := os.Lstat(f.bare("objects")); err != nil || !fi.IsDir() {
		t.Errorf(".bare/objects is not a dir after the blocked move: %v", err)
	}
}

// TestSandboxExecGitWriteScope_PlantsWorkWithOldGrant is the paired negative
// test for the plants: with the read of the project root changed back to
// the read-write grant that the profile had before #3293, the plants work.
func TestSandboxExecGitWriteScope_PlantsWorkWithOldGrant(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	readClause := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(f.bareRoot) + "))"
	oldGrant := "(allow file-read* file-write* file-test-existence file-read-metadata\n  (subpath " + sbplQuoteForTest(f.bareRoot) + "))"
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		return strings.Replace(p, readClause, oldGrant, 1)
	})

	out, err := f.run(t, profilePath, `echo plant > "$BARE/main/plant.txt" && mkdir "$BARE/main/.pi" && echo '[core]' >> "$BARE/.bare/config"`)
	if err != nil {
		t.Fatalf("with the old grant the plants still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}

// TestSandboxExecGitWriteScope_CommitFailsWithoutGitClause is the paired
// negative test for the git write grant: with every .bare path of the
// profile pointed at a missing dir, a commit fails.
func TestSandboxExecGitWriteScope_CommitFailsWithoutGitClause(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		return strings.ReplaceAll(p, `"`+f.bare()+`/`, `"`+f.bare()+`-disabled/`)
	})

	out, err := f.run(t, profilePath, `echo c > c.txt && git add c.txt && git commit -q -m c`)
	if err == nil {
		t.Fatalf("a commit worked with no git write grant, so the positive test proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
}

// TestSandboxExecGitWriteScope_FetchFailsWithoutPreparedDirs is the paired
// negative test for the host-side dirs: with .bare/logs removed after
// Prepare, a fetch cannot write the reflog of the remote-tracking ref.
func TestSandboxExecGitWriteScope_FetchFailsWithoutPreparedDirs(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	if err := os.RemoveAll(f.bare("logs")); err != nil {
		t.Fatalf("remove .bare/logs: %v", err)
	}
	profilePath := writeAugmentedPositiveProfile(t, f.prepared)

	out, err := f.run(t, profilePath, `git fetch -q origin`)
	if err == nil {
		t.Fatalf("a fetch worked with no .bare/logs, so the dirs that Prepare makes are not necessary.\nOutput: %s\nProfile: %s", out, profilePath)
	}
}

// TestSandboxExecGitWriteScope_ObjectsReplaceableWithoutNodeDeny is the
// paired negative test for section 21i: with the deny removed, the session
// moves .bare/objects away and puts a symlink at its path.
func TestSandboxExecGitWriteScope_ObjectsReplaceableWithoutNodeDeny(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	gitDir, err := git.ResolveWorktreeGitDir(f.worktree)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir: %v", err)
	}
	var lits []string
	for _, p := range []string{f.bare("objects"), f.bare("refs", "remotes"), f.bare("refs", "tags"), f.bare("logs", "refs", "remotes"), gitDir} {
		lits = append(lits, "(literal "+sbplQuoteForTest(p)+")")
	}
	nodeDeny := "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n  " +
		strings.Join(lits, "\n  ") + ")\n"
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		return strings.Replace(p, nodeDeny, "", 1)
	})

	out, err := f.run(t, profilePath, `mv "$BARE/.bare/objects" "$WT/objects-moved" && ln -s "$WT/objects-moved" "$BARE/.bare/objects"`)
	if err != nil {
		t.Fatalf("without the node deny the move still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if fi, err := os.Lstat(f.bare("objects")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("without the node deny .bare/objects is not a symlink after the swap: %v", err)
	}
}

// TestSandboxExecGitWriteScope_RedirectFilesWritableWithoutDeny is the
// paired negative test for section 21j: with the deny removed, the session
// writes config.worktree and replaces commondir in its git dir.
func TestSandboxExecGitWriteScope_RedirectFilesWritableWithoutDeny(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	gitDir, err := git.ResolveWorktreeGitDir(f.worktree)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir: %v", err)
	}
	var lits []string
	for _, name := range []string{"commondir", "gitdir", "config.worktree"} {
		lits = append(lits, "(literal "+sbplQuoteForTest(filepath.Join(gitDir, name))+")")
	}
	redirectDeny := "(deny file-write*\n  " + strings.Join(lits, "\n  ") + ")\n"
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		return strings.Replace(p, redirectDeny, "", 1)
	})

	out, err := f.run(t, profilePath, `gd=$(git rev-parse --git-dir) &&
echo '[core]' > "$gd/config.worktree" &&
echo ../.. > "$gd/commondir.new" && mv "$gd/commondir.new" "$gd/commondir"`)
	if err != nil {
		t.Fatalf("without the deny the writes still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config.worktree")); err != nil {
		t.Errorf("without the deny config.worktree is missing after the write: %v", err)
	}
}

// pushAfterHostPush pushes from the sandbox, then does what the pre-flight
// of prism review --rebase does on the host: a fetch, a rebase, and a push
// with the host git config, which sets push.autoSetupRemote. Then it pushes
// and pulls from the sandbox again, and returns the result of that step.
func pushAfterHostPush(t *testing.T, f *gitScopeFixture) ([]byte, error) {
	t.Helper()
	profilePath := writeAugmentedPositiveProfile(t, f.prepared)
	if out, err := f.run(t, profilePath, `echo one > one.txt && git add one.txt && git commit -q -m one && git push -q`); err != nil {
		t.Fatalf("first push from the sandbox failed.\nExit: %v\nOutput: %s", err, out)
	}
	hostGit(t, f.nixGit, f.worktree, "fetch", "-q", "origin")
	hostGit(t, f.nixGit, f.worktree, "rebase", "-q", "origin/main")
	hostGit(t, f.nixGit, f.worktree, "-c", "push.autoSetupRemote=true", "push", "-q", "--force-with-lease")
	return f.run(t, profilePath, `echo two > two.txt && git add two.txt && git commit -q -m two && git push -q && git pull -q --rebase`)
}

// TestSandboxExecGitWriteScope_PushAfterHostPush: Prepare writes the
// upstream into .bare/config, so a push on the host in the worktree adds no
// second upstream, and the next push and pull in the sandbox work.
func TestSandboxExecGitWriteScope_PushAfterHostPush(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	out, err := pushAfterHostPush(t, f)
	if err != nil {
		t.Fatalf("push or pull from the sandbox failed after a push on the host.\nExit: %v\nOutput: %s", err, out)
	}
	if got := hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "config", "--get-all", "branch.feature.merge"); got != "refs/heads/feature" {
		t.Errorf("branch.feature.merge in .bare/config = %q, want one refs/heads/feature", got)
	}
}

// TestSandboxExecGitWriteScope_PushFailsWithUpstreamInBothFiles is the
// paired negative test: with the upstream in the session gitconfig and not
// in .bare/config, the push on the host writes a second upstream into
// .bare/config, and the next push from the sandbox fails.
func TestSandboxExecGitWriteScope_PushFailsWithUpstreamInBothFiles(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "config", "--unset", "branch.feature.remote")
	hostGit(t, f.nixGit, f.bareRoot, "--git-dir", f.bare(), "config", "--unset", "branch.feature.merge")
	gitconfig, err := os.OpenFile(container.SessionWorkDirGitconfigPath(f.session), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open session gitconfig: %v", err)
	}
	_, err = gitconfig.WriteString("\n[branch \"feature\"]\n    remote = origin\n    merge = refs/heads/feature\n")
	if closeErr := gitconfig.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("write session gitconfig: %v", err)
	}

	out, err := pushAfterHostPush(t, f)
	if err == nil {
		t.Fatalf("the sandbox pushed with an upstream in two files, so the positive test proves nothing.\nOutput: %s", out)
	}
}

// TestSandboxExecGitWriteScope_GitFileReplaceableWithoutDeny is the paired
// negative test for section 21k: with the deny removed, the session
// replaces the .git file of its worktree and changes its mode.
func TestSandboxExecGitWriteScope_GitFileReplaceableWithoutDeny(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	deny := "(deny file-write*\n  (literal " + sbplQuoteForTest(filepath.Join(f.worktree, ".git")) + ")"
	if real, err := filepath.EvalSymlinks(f.worktree); err == nil && real != f.worktree {
		deny += "\n  (literal " + sbplQuoteForTest(filepath.Join(real, ".git")) + ")"
	}
	deny += ")\n"
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		return strings.Replace(p, deny, "", 1)
	})

	out, err := f.run(t, profilePath, `cp "$WT/.git" "$WT/.git.new" && mv -f "$WT/.git.new" "$WT/.git" && chmod 600 "$WT/.git"`)
	if err != nil {
		t.Fatalf("without the deny the replace still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
	if fi, err := os.Lstat(filepath.Join(f.worktree, ".git")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("without the deny the .git file does not have mode 0600 after the chmod: %v", err)
	}
}

// coordinatorScript does the git work of a coordinator in main, then
// tries the writes that only a worker of feature can do.
const coordinatorScript = `set -u
fail() { echo "step $1 failed" >&2; exit "$1"; }
git pull -q --ff-only || fail 40
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" || fail 41
git update-ref --create-reflog refs/stash HEAD || fail 42
git update-ref -d refs/stash || fail 43
echo plant >> "$BARE/feature/README.md" && fail 44
git update-ref refs/heads/feature HEAD && fail 45
exit 0`

// TestSandboxExecGitWriteScope_CoordinatorPullsAndStashes: the coordinator
// pulls in main, with the upstream in .bare/config, and writes the stash
// ref. It cannot write the worktree or the branch of a worker.
func TestSandboxExecGitWriteScope_CoordinatorPullsAndStashes(t *testing.T) {
	f := newGitScopeFixture(t, "main", "coordinator", false)
	profilePath := writeAugmentedPositiveProfile(t, f.prepared)

	out, err := f.run(t, profilePath, coordinatorScript)
	if err != nil {
		t.Fatalf("coordinator git work failed (exit 40-43) or a write in the worker area was not blocked (exit 44-45).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}

// TestSandboxExecGitWriteScope_CoordinatorStashFailsWithoutStashGrant is the
// paired negative test: with the stash files pointed at another name, the
// coordinator cannot write the stash ref.
func TestSandboxExecGitWriteScope_CoordinatorStashFailsWithoutStashGrant(t *testing.T) {
	f := newGitScopeFixture(t, "main", "coordinator", false)
	profilePath := withMutatedProfile(t, f.m, func(p string) string {
		p = strings.ReplaceAll(p, sbplQuoteForTest(f.bare("refs", "stash")), sbplQuoteForTest(f.bare("refs", "stash-disabled")))
		return strings.ReplaceAll(p, sbplQuoteForTest(f.bare("refs", "stash.lock")), sbplQuoteForTest(f.bare("refs", "stash-disabled.lock")))
	})

	out, err := f.run(t, profilePath, `git update-ref --create-reflog refs/stash HEAD`)
	if err == nil {
		t.Fatalf("the stash write worked with no stash grant, so the positive test proves nothing.\nOutput: %s\nProfile: %s", out, profilePath)
	}
}

// readOnlyScript does the git work of a review agent, then tries to move
// the branch and to write HEAD and the index of the worktree.
const readOnlyScript = `set -u
fail() { echo "step $1 failed" >&2; exit "$1"; }
git status --short || fail 50
git fetch -q origin || fail 51
git diff --stat HEAD origin/main >/dev/null || fail 52
git log --oneline -1 >/dev/null || fail 53
git update-ref refs/heads/feature origin/main && fail 54
echo 'ref: refs/heads/feature' > "$(git rev-parse --git-dir)/HEAD" && fail 55
git update-index --refresh && fail 56
exit 0`

// readOnlyWrites are the writes that readOnlyScript expects to fail. The
// worktree of the fixture is clean, so git update-index --refresh exits 0
// when it can write the index.
const readOnlyWrites = `git fetch -q origin &&
git update-ref refs/heads/feature origin/main &&
echo 'ref: refs/heads/feature' > "$(git rev-parse --git-dir)/HEAD" &&
git update-index --refresh`

// TestSandboxExecGitWriteScope_ReadOnlyCannotMoveBranch: a review agent
// runs git status, git fetch, git diff, and git log, but cannot move the
// branch, and cannot write HEAD or the index of the worktree.
func TestSandboxExecGitWriteScope_ReadOnlyCannotMoveBranch(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", true)
	profilePath := writeAugmentedPositiveProfile(t, f.prepared)

	out, err := f.run(t, profilePath, readOnlyScript)
	if err != nil {
		t.Fatalf("review-agent git work failed (exit 50-53) or a write of the branch, HEAD, or the index was not blocked (exit 54-56).\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}

// TestSandboxExecGitWriteScope_WorkerMovesBranch is the paired negative
// test: under the profile of a worker, the same writes work.
func TestSandboxExecGitWriteScope_WorkerMovesBranch(t *testing.T) {
	f := newGitScopeFixture(t, "feature", "worker", false)
	profilePath := writeAugmentedPositiveProfile(t, f.prepared)

	out, err := f.run(t, profilePath, readOnlyWrites)
	if err != nil {
		t.Fatalf("a worker cannot write its branch, HEAD, or index, so the read-only test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}
