package container

// git_write_scope_test.go — unit coverage for the git write scope of a
// sandboxed session (#3293). The live sandbox-exec tests are in
// internal/integration/sandbox_exec_git_write_scope_darwin_test.go.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newGitLayout makes a prism bare+worktree layout with no git objects: for
// each branch, the worktree <root>/<branch> with a .git file, and the git
// dir <root>/.bare/worktrees/<id> with a gitdir file that points back.
func newGitLayout(t *testing.T, branches ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	for _, b := range branches {
		addGitWorktree(t, root, b)
	}
	return root
}

func addGitWorktree(t *testing.T, root, branch string) {
	t.Helper()
	worktree := filepath.Join(root, filepath.FromSlash(branch))
	gitDir := gitDirFor(root, branch)
	for _, d := range []string{worktree, gitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	writeTestFile(t, filepath.Join(worktree, ".git"), "gitdir: "+gitDir+"\n")
	writeTestFile(t, filepath.Join(gitDir, "gitdir"), filepath.Join(worktree, ".git")+"\n")
}

func gitDirFor(root, branch string) string {
	return filepath.Join(root, ".bare", "worktrees", filepath.Base(filepath.FromSlash(branch)))
}

func TestResolveGitWriteScope_OwnGitDirAndBranch(t *testing.T) {
	for _, branch := range []string{"feature", "renovate/foo"} {
		t.Run(branch, func(t *testing.T) {
			root := newGitLayout(t, "main", branch)
			worktree := filepath.Join(root, filepath.FromSlash(branch))
			scope, err := resolveGitWriteScope(root, worktree, gitDirFor(root, branch))
			if err != nil {
				t.Fatalf("resolveGitWriteScope: %v", err)
			}
			want := gitWriteScope{CommonDir: filepath.Join(root, ".bare"), GitDir: gitDirFor(root, branch), Branch: branch}
			if scope != want {
				t.Errorf("scope = %+v, want %+v", scope, want)
			}
		})
	}
}

// TestResolveGitWriteScope_RelativeBackPointer: git writes a relative gitdir
// file when worktree.useRelativePaths is set.
func TestResolveGitWriteScope_RelativeBackPointer(t *testing.T) {
	root := newGitLayout(t, "feature")
	writeTestFile(t, filepath.Join(gitDirFor(root, "feature"), "gitdir"), "../../../feature/.git\n")
	scope, err := resolveGitWriteScope(root, filepath.Join(root, "feature"), gitDirFor(root, "feature"))
	if err != nil || scope.GitDir != gitDirFor(root, "feature") {
		t.Fatalf("scope = %+v, err = %v; want the git dir of the worktree", scope, err)
	}
}

// TestResolveGitWriteScope_RefusesForeignGitDir: the session can write the
// .git file of its worktree. If the git dir that it names is not the git
// dir of the worktree, the scope grants no git dir.
func TestResolveGitWriteScope_RefusesForeignGitDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{"git dir of another worktree", func(t *testing.T, root string) string {
			return gitDirFor(root, "main")
		}},
		{"another worktree", func(t *testing.T, root string) string {
			return filepath.Join(root, "main")
		}},
		{"the common dir", func(t *testing.T, root string) string {
			return filepath.Join(root, ".bare")
		}},
		{"a symlink to another worktree", func(t *testing.T, root string) string {
			link := filepath.Join(root, ".bare", "worktrees", "evil")
			if err := os.Symlink(filepath.Join(root, "main"), link); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			writeTestFile(t, filepath.Join(root, "main", "gitdir"), filepath.Join(root, "feature", ".git"))
			return link
		}},
		{"a gitdir file that is a symlink", func(t *testing.T, root string) string {
			own := gitDirFor(root, "feature")
			target := filepath.Join(root, "feature", "pointer")
			writeTestFile(t, target, filepath.Join(root, "feature", ".git"))
			if err := os.Remove(filepath.Join(own, "gitdir")); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(own, "gitdir")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}
			return own
		}},
		{"a gitdir file that is a FIFO", func(t *testing.T, root string) string {
			own := gitDirFor(root, "feature")
			if err := os.Remove(filepath.Join(own, "gitdir")); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if err := syscall.Mkfifo(filepath.Join(own, "gitdir"), 0o644); err != nil {
				t.Fatalf("Mkfifo: %v", err)
			}
			return own
		}},
		{"no git dir", func(t *testing.T, root string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newGitLayout(t, "main", "feature")
			gitDir := tc.setup(t, root)

			done := make(chan struct{})
			var scope gitWriteScope
			var err error
			go func() {
				scope, err = resolveGitWriteScope(root, filepath.Join(root, "feature"), gitDir)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("resolveGitWriteScope blocked on the gitdir file")
			}
			if scope.GitDir != "" {
				t.Errorf("scope grants git dir %s, want none", scope.GitDir)
			}
			if err == nil {
				t.Error("resolveGitWriteScope returned no error for a refused git dir")
			}
			if scope.Branch != "feature" {
				t.Errorf("scope.Branch = %q, want %q: the branch does not depend on the git dir", scope.Branch, "feature")
			}
		})
	}
}

func TestGitBranchFromWorktreePath(t *testing.T) {
	root := "/Users/u/code/repo"
	for _, tc := range []struct {
		worktree string
		want     string
	}{
		{root + "/feature", "feature"},
		{root + "/renovate/foo", "renovate/foo"},
		{root + "/fix-1.2", "fix-1.2"},
		{root, ""},
		{"/Users/u/code/other/feature", ""},
		{root + "/.hidden", ""},
		{root + "/a/x.lock", ""},
		{root + "/a..b", ""},
		{root + "/trailing.", ""},
		{root + "/with space", ""},
	} {
		got, err := gitBranchFromWorktreePath(root, tc.worktree)
		if got != tc.want {
			t.Errorf("gitBranchFromWorktreePath(%q) = %q, %v; want %q", tc.worktree, got, err, tc.want)
		}
		if tc.want == "" && err == nil {
			t.Errorf("gitBranchFromWorktreePath(%q) refused the name with no error", tc.worktree)
		}
	}
}

// newGitScopeManager returns a sandbox-exec manager for the worktree of
// branch in root.
func newGitScopeManager(t *testing.T, root, branch string, cfg Config) *Manager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cfg.SessionName = "repo@" + branch
	cfg.Worktree = filepath.Join(root, filepath.FromSlash(branch))
	cfg.BareRoot = root
	cfg.WorktreeGitDir = gitDirFor(root, branch)
	return newSandboxExecManagerWithInstance(cfg)
}

// assertWriteCoverage fails the test when a path in writable has no write
// grant, or a path in denied has one.
func assertWriteCoverage(t *testing.T, profile string, writable, denied []string) {
	t.Helper()
	granted := writeGrantedSBPLPaths(t, profile)
	covered := func(p string) bool {
		for _, r := range granted {
			if r.covers(p) {
				return true
			}
		}
		return false
	}
	for _, p := range writable {
		if !covered(p) {
			t.Errorf("no write grant covers %s, which git writes", p)
		}
	}
	for _, p := range denied {
		if covered(p) {
			t.Errorf("a write grant covers %s", p)
		}
	}
}

// gitPathsOf returns the paths below the common dir of root that git writes
// for each session (shared), the paths that only a session that can write
// its worktree writes (own), and the paths that a session must not write
// (foreign).
func gitPathsOf(root, branch string) (shared, own, foreign []string) {
	bare := filepath.Join(root, ".bare")
	shared = []string{
		filepath.Join(bare, "objects", "ab", "cdef"),
		filepath.Join(bare, "objects", "pack", "pack-1.pack"),
		filepath.Join(bare, "refs", "remotes", "origin", "main"),
		filepath.Join(bare, "refs", "tags", "v1"),
		filepath.Join(bare, "logs", "refs", "remotes", "origin", "main"),
		filepath.Join(bare, "packed-refs"),
		filepath.Join(bare, "packed-refs.lock"),
		filepath.Join(bare, "packed-refs.new"),
		filepath.Join(gitDirFor(root, branch), "FETCH_HEAD"),
	}
	own = []string{
		filepath.Join(gitDirFor(root, branch), "index"),
		filepath.Join(gitDirFor(root, branch), "HEAD"),
		filepath.Join(gitDirFor(root, branch), "logs", "HEAD"),
		filepath.Join(bare, "refs", "heads", branch),
		filepath.Join(bare, "refs", "heads", branch+".lock"),
		filepath.Join(bare, "logs", "refs", "heads", branch),
	}
	foreign = []string{
		root,
		filepath.Join(root, ".agents", "skills", "x", "SKILL.md"),
		filepath.Join(root, ".git"),
		filepath.Join(root, "main", ".pi", "extensions", "x.ts"),
		filepath.Join(root, "main", ".pi", "settings.json"),
		filepath.Join(root, "main", "README.md"),
		filepath.Join(bare, "config"),
		filepath.Join(bare, "config.lock"),
		filepath.Join(bare, "hooks", "post-checkout"),
		filepath.Join(bare, "info", "attributes"),
		filepath.Join(bare, "HEAD"),
		filepath.Join(bare, "gc.pid"),
		filepath.Join(bare, "refs", "heads", "main"),
		filepath.Join(bare, "logs", "refs", "heads", "main"),
		filepath.Join(bare, "worktrees", "main", "HEAD"),
		filepath.Join(bare, "worktrees", "main", "index"),
		filepath.Join(bare, "worktrees", "other"),
	}
	return shared, own, foreign
}

// TestGenerateProfile_GitWriteScope: a worker writes its worktree and the
// git paths of a commit, a fetch, and a push. It writes no other path in
// the project root (#3293).
func TestGenerateProfile_GitWriteScope(t *testing.T) {
	root := newGitLayout(t, "main", "feature")
	m := newGitScopeManager(t, root, "feature", Config{AgentRole: "worker"})
	profile := generateProfile(m)

	shared, own, foreign := gitPathsOf(root, "feature")
	writable := append(append([]string{filepath.Join(root, "feature", ".pi", "settings.json")}, shared...), own...)
	stash := filepath.Join(root, ".bare", "refs", "stash")
	assertWriteCoverage(t, profile, writable, append(foreign, stash))

	if want := "(allow file-read* file-test-existence file-read-metadata\n  (subpath " + quoteSBPL(root) + "))\n"; !strings.Contains(profile, want) {
		t.Errorf("profile has no read of the project root %q.\nprofile:\n%s", want, profile)
	}
}

// TestGenerateProfile_GitWriteScope_Coordinator: the coordinator also
// writes the stash ref, and only the branch of its own worktree.
func TestGenerateProfile_GitWriteScope_Coordinator(t *testing.T) {
	root := newGitLayout(t, "main", "feature")
	m := newGitScopeManager(t, root, "main", Config{AgentRole: "coordinator"})
	profile := generateProfile(m)

	bare := filepath.Join(root, ".bare")
	writable := []string{
		filepath.Join(bare, "refs", "heads", "main"),
		filepath.Join(bare, "refs", "stash"),
		filepath.Join(bare, "refs", "stash.lock"),
		filepath.Join(bare, "logs", "refs", "stash"),
		filepath.Join(bare, "logs", "refs", "stash.lock"),
		filepath.Join(gitDirFor(root, "main"), "index"),
	}
	denied := []string{
		filepath.Join(bare, "refs", "heads", "feature"),
		filepath.Join(bare, "worktrees", "feature", "HEAD"),
		filepath.Join(root, "feature", "README.md"),
		filepath.Join(bare, "config"),
	}
	assertWriteCoverage(t, profile, writable, denied)
}

// TestGenerateProfile_GitWriteScope_ReadOnly: a review agent or an
// investigator can fetch, but cannot move the branch or the stash, and
// cannot write HEAD or the index of the worktree.
func TestGenerateProfile_GitWriteScope_ReadOnly(t *testing.T) {
	root := newGitLayout(t, "main", "feature")
	for _, role := range []string{"worker", "coordinator"} {
		t.Run(role, func(t *testing.T) {
			m := newGitScopeManager(t, root, "feature", Config{AgentRole: role, WorktreeReadOnly: true})
			profile := generateProfile(m)

			shared, own, _ := gitPathsOf(root, "feature")
			assertWriteCoverage(t, profile, shared, append(own, filepath.Join(root, ".bare", "refs", "stash")))
		})
	}
}

// TestGenerateProfile_GitWriteScope_TamperedGitFile: a worker points the
// .git file of its worktree at the git dir of main before a restart. The
// profile then grants neither git dir.
func TestGenerateProfile_GitWriteScope_TamperedGitFile(t *testing.T) {
	root := newGitLayout(t, "main", "feature")
	m := newGitScopeManager(t, root, "feature", Config{AgentRole: "worker"})
	m.cfg.WorktreeGitDir = gitDirFor(root, "main")
	profile := generateProfile(m)

	assertWriteCoverage(t, profile,
		[]string{filepath.Join(root, ".bare", "refs", "heads", "feature")},
		[]string{
			filepath.Join(gitDirFor(root, "main"), "HEAD"),
			filepath.Join(gitDirFor(root, "main"), "index"),
			filepath.Join(gitDirFor(root, "feature"), "index"),
		})
}

// TestGenerateProfile_GitNodeDenyFollowsEveryAllow: the section-21i deny of
// the git dir nodes follows every allow. SBPL takes the later rule, so an
// allow after the deny gives the node back.
func TestGenerateProfile_GitNodeDenyFollowsEveryAllow(t *testing.T) {
	root := newGitLayout(t, "feature")
	m := newGitScopeManager(t, root, "feature", Config{AgentRole: "worker", Harness: "pi"})
	profile := generateProfile(m)

	scope, err := m.gitScope()
	if err != nil {
		t.Fatalf("gitScope: %v", err)
	}
	deny := scope.sbplNodeDeny()
	denyAt := strings.Index(profile, deny)
	if denyAt < 0 {
		t.Fatalf("profile has no git node deny %q.\nprofile:\n%s", deny, profile)
	}
	if allowAt := strings.LastIndex(profile, "(allow "); allowAt > denyAt {
		t.Errorf("an allow clause (offset %d) follows the git node deny (offset %d).\nprofile:\n%s", allowAt, denyAt, profile)
	}
	for _, node := range []string{
		filepath.Join(root, ".bare", "objects"),
		filepath.Join(root, ".bare", "refs", "remotes"),
		filepath.Join(root, ".bare", "refs", "tags"),
		filepath.Join(root, ".bare", "logs", "refs", "remotes"),
		gitDirFor(root, "feature"),
	} {
		if !strings.Contains(deny, "(literal "+quoteSBPL(node)+")") {
			t.Errorf("git node deny does not list %s:\n%s", node, deny)
		}
	}
}

// TestGenerateProfile_NoGitScopeWithoutBareRoot: a project with no
// worktrees has no BareRoot, and the profile names no .bare path.
func TestGenerateProfile_NoGitScopeWithoutBareRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManagerWithInstance(Config{SessionName: "obsidian", Worktree: t.TempDir()})
	if profile := generateProfile(m); strings.Contains(profile, ".bare") {
		t.Errorf("profile names a .bare path with no BareRoot:\n%s", profile)
	}
}

// TestPrepareGitWriteDirs: the parent dirs of the refs and reflogs exist
// before the sandbox starts. A read-only session gets no branch dirs.
func TestPrepareGitWriteDirs(t *testing.T) {
	root := newGitLayout(t, "renovate/foo")
	scope, err := resolveGitWriteScope(root, filepath.Join(root, "renovate", "foo"), gitDirFor(root, "renovate/foo"))
	if err != nil {
		t.Fatalf("resolveGitWriteScope: %v", err)
	}
	bare := scope.CommonDir
	branchDirs := []string{
		filepath.Join(bare, "refs", "heads", "renovate"),
		filepath.Join(bare, "logs", "refs", "heads", "renovate"),
	}
	sharedDirs := []string{
		filepath.Join(bare, "objects"),
		filepath.Join(bare, "refs", "remotes"),
		filepath.Join(bare, "refs", "tags"),
		filepath.Join(bare, "logs", "refs", "remotes"),
	}

	prepareGitWriteDirs(scope, true)
	for _, d := range sharedDirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("read-only: %s is not a dir: %v", d, err)
		}
	}
	for _, d := range branchDirs {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("read-only: %s exists, want no branch dir", d)
		}
	}

	prepareGitWriteDirs(scope, false)
	for _, d := range branchDirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("worker: %s is not a dir: %v", d, err)
		}
	}
}

// initBareCommonDir runs git init --bare in <root>/.bare.
func initBareCommonDir(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", filepath.Join(root, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
}

// TestGitUpstreamSection: the per-session gitconfig names the upstream only
// when the repo config does not. git push refuses a branch with an
// upstream in both files.
func TestGitUpstreamSection(t *testing.T) {
	root := newGitLayout(t, "feature")
	initBareCommonDir(t, root)
	scope, err := resolveGitWriteScope(root, filepath.Join(root, "feature"), gitDirFor(root, "feature"))
	if err != nil {
		t.Fatalf("resolveGitWriteScope: %v", err)
	}

	want := "\n[branch \"feature\"]\n    remote = origin\n    merge = refs/heads/feature\n"
	if got := gitUpstreamSection(scope, false); got != want {
		t.Errorf("no repo upstream: section = %q, want %q", got, want)
	}
	if got := gitUpstreamSection(scope, true); got != "" {
		t.Errorf("read-only session: section = %q, want none", got)
	}

	if out, err := exec.Command("git", "--git-dir", scope.CommonDir, "config", "branch.feature.merge", "refs/heads/feature").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	if got := gitUpstreamSection(scope, false); got != "" {
		t.Errorf("repo upstream present: section = %q, want none", got)
	}
}

// TestWriteGitconfigToDir_Upstream: the session gitconfig ends with the
// upstream section of the worktree branch.
func TestWriteGitconfigToDir_Upstream(t *testing.T) {
	root := newGitLayout(t, "feature")
	initBareCommonDir(t, root)
	m := newGitScopeManager(t, root, "feature", Config{AgentRole: "worker"})
	sessionDir := t.TempDir()
	if err := m.writeGitconfigToDir(sessionDir); err != nil {
		t.Fatalf("writeGitconfigToDir: %v", err)
	}
	data, err := os.ReadFile(SessionWorkDirGitconfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	if !strings.Contains(string(data), "[user]") || !strings.HasSuffix(string(data), "[branch \"feature\"]\n    remote = origin\n    merge = refs/heads/feature\n") {
		t.Errorf("gitconfig does not hold [user] and end with the upstream section:\n%s", data)
	}
}
