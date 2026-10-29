package review

// preflight_host_git_test.go — the pre-flight gate of the /review handler on a
// prism bare+worktree layout, with a redirect that the worker planted.
//
// Each test runs a plain git command with the plant first (the control) and
// checks that the payload runs. It then runs Preflight and checks that the
// payload does not run. The payload only appends to a marker in t.TempDir().

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config/configtest"
	"github.com/prismatic-koi/prism/internal/forge"
	"github.com/prismatic-koi/prism/internal/git"
)

type preflightLayout struct {
	origin  string
	root    string
	worker  string
	marker  string
	advance func(t *testing.T)
}

// newPreflightLayout makes a prism project with a worker worktree "feat"
// that has one commit and an upstream on origin. advance adds a commit to
// origin/main, so the worker is behind.
func newPreflightLayout(t *testing.T) preflightLayout {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	// A host shell carries the user git overrides (modules/programs/git.nix)
	// in GIT_CONFIG_COUNT and the GIT_CONFIG_KEY_<n>/VALUE_<n> pairs. Count 0
	// makes git read none of them, so the control checks see a plain git.
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	mustGit(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(tmp, "seed")
	mustGit(t, "", "clone", "-q", origin, seed)
	writeFile(t, filepath.Join(seed, "README.md"), "init\n")
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-q", "-m", "init")
	mustGit(t, seed, "push", "-q", "origin", "main")

	root := filepath.Join(tmp, "proj")
	configtest.AllowProjectRoots(t, root)
	if err := git.CloneWorktree(origin, root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	created, err := git.CreateWorktree(root, "feat")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	worker := created.Path
	writeFile(t, filepath.Join(worker, "feat.txt"), "feat\n")
	mustGit(t, worker, "add", ".")
	mustGit(t, worker, "commit", "-q", "-m", "feat")
	mustGit(t, worker, "push", "-q", "-u", "origin", "feat")

	return preflightLayout{
		origin: origin,
		root:   root,
		worker: worker,
		marker: filepath.Join(t.TempDir(), "marker"),
		advance: func(t *testing.T) {
			t.Helper()
			writeFile(t, filepath.Join(seed, "main.txt"), "main\n")
			mustGit(t, seed, "add", ".")
			mustGit(t, seed, "commit", "-q", "-m", "main advances")
			mustGit(t, seed, "push", "-q", "origin", "main")
		},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// evilGitDir makes a non-bare git dir that the worker controls, with
// core.fsmonitor set to a command and the hooks of fetch, rebase, and push.
func (l preflightLayout) evilGitDir(t *testing.T) string {
	t.Helper()
	evil := filepath.Join(t.TempDir(), "evil.git")
	mustGit(t, "", "clone", "-q", "--bare", l.origin, evil)
	mustGit(t, "", "--git-dir", evil, "config", "core.bare", "false")
	cfg, err := os.OpenFile(filepath.Join(evil, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open evil config: %v", err)
	}
	fmt.Fprintf(cfg, "[core]\n\tfsmonitor = \"echo fsmonitor >> '%s'; false\"\n", l.marker)
	if err := cfg.Close(); err != nil {
		t.Fatalf("close evil config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(evil, "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	for _, name := range []string{"reference-transaction", "pre-rebase", "post-rewrite", "post-checkout", "pre-push"} {
		writeFile(t, filepath.Join(evil, "hooks", name), fmt.Sprintf("#!/bin/sh\necho %s >> '%s'\nexit 0\n", name, l.marker))
		if err := os.Chmod(filepath.Join(evil, "hooks", name), 0o755); err != nil {
			t.Fatalf("chmod hook: %v", err)
		}
	}
	return evil
}

func (l preflightLayout) takeMarker(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(l.marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if err := os.Remove(l.marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	return strings.Fields(string(data))
}

// TestPreflight_HostGit_WorktreeGitFileRedirect points <worktree>/.git at a
// git dir that the worker controls, then runs the --rebase pre-flight: fetch,
// rebase, push, and the ancestor check.
func TestPreflight_HostGit_WorktreeGitFileRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	l.advance(t)
	evil := l.evilGitDir(t)
	writeFile(t, filepath.Join(l.worker, ".git"), "gitdir: "+evil+"\n")

	control := exec.Command("git", "status", "--short")
	control.Dir = l.worker
	_, _ = control.CombinedOutput()
	if len(l.takeMarker(t)) == 0 {
		t.Fatal("control: plain git status did not run the payload; the plant does not work")
	}

	if err := Preflight(PreflightOpts{Worktree: l.worker, Rebase: true}); err != nil {
		t.Fatalf("Preflight --rebase with the fix: %v", err)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("Preflight ran a payload that the worker wrote: %v", lines)
	}
	// The rebased branch is on origin: origin/main is an ancestor of origin/feat.
	if err := exec.Command("git", "--git-dir", l.origin, "merge-base", "--is-ancestor", "main", "feat").Run(); err != nil {
		t.Errorf("origin/feat is not rebased onto origin/main: %v", err)
	}
}

// TestPreflight_HostGit_OwnCommondirRedirect points the commondir file of the
// worker git dir at a git dir that the worker controls. Preflight refuses.
func TestPreflight_HostGit_OwnCommondirRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	l.advance(t)
	evil := l.evilGitDir(t)
	ownGitDir, err := git.ResolveWorktreeGitDir(l.worker)
	if err != nil {
		t.Fatalf("resolve git dir: %v", err)
	}
	writeFile(t, filepath.Join(ownGitDir, "commondir"), evil+"\n")
	writeFile(t, filepath.Join(l.worker, "feat.txt"), "dirty\n")

	control := exec.Command("git", "status", "--short")
	control.Dir = l.worker
	_, _ = control.CombinedOutput()
	if len(l.takeMarker(t)) == 0 {
		t.Fatal("control: plain git status did not run the payload; the plant does not work")
	}

	err = Preflight(PreflightOpts{Worktree: l.worker, Rebase: true})
	if err == nil || !strings.Contains(err.Error(), "commondir") {
		t.Fatalf("Preflight = %v, want a refusal that names commondir", err)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("Preflight ran a payload that the worker wrote: %v", lines)
	}
}

// TestPreflight_HostGit_RebaseOnCleanLayout is the functional check: with no
// plant, the --rebase pre-flight on a bare+worktree layout fetches, rebases,
// and pushes as before.
func TestPreflight_HostGit_RebaseOnCleanLayout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	l.advance(t)

	if err := Preflight(PreflightOpts{Worktree: l.worker}); err == nil || !strings.Contains(err.Error(), "behind") {
		t.Fatalf("Preflight without --rebase = %v, want a refusal", err)
	}

	if err := Preflight(PreflightOpts{Worktree: l.worker, Rebase: true}); err != nil {
		t.Fatalf("Preflight --rebase: %v", err)
	}
	if err := exec.Command("git", "--git-dir", l.origin, "merge-base", "--is-ancestor", "main", "feat").Run(); err != nil {
		t.Errorf("origin/feat is not rebased onto origin/main: %v", err)
	}
	if err := Preflight(PreflightOpts{Worktree: l.worker}); err != nil {
		t.Errorf("Preflight after the rebase: %v", err)
	}
}

// TestRunGH_HostGit_WorktreeGitFileRedirect runs runGH from the worker
// worktree, as the /review handler does. A stub gh runs git in the cwd to find
// the repo, as the real gh does.
func TestRunGH_HostGit_WorktreeGitFileRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	evil := l.evilGitDir(t)
	writeFile(t, filepath.Join(l.worker, ".git"), "gitdir: "+evil+"\n")

	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "gh"), "#!/bin/sh\ngit status --short >/dev/null && git remote get-url origin\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatalf("chmod gh: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(l.worker)

	control := exec.Command("gh")
	_, _ = control.CombinedOutput()
	if len(l.takeMarker(t)) == 0 {
		t.Fatal("control: the stub gh without the env did not run the payload; the plant does not work")
	}

	out, err := runGH("pr", "view", "1")
	if err != nil {
		t.Fatalf("runGH with the fix: %v", err)
	}
	if strings.TrimSpace(out) != l.origin {
		t.Errorf("runGH output %q, want the origin %q", out, l.origin)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("runGH ran a payload that the worker wrote: %v", lines)
	}
}

// ── #3323 security regression tests ────────────────────────────────────────

// gitOut runs git in cwd and returns its trimmed stdout, failing the test on
// a non-zero exit.
func gitOut(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// gitOutEnv is gitOut with an explicit environment (for GIT_INDEX_FILE).
func gitOutEnv(t *testing.T, cwd string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// mustGitEnv runs git in cwd with an explicit environment, failing on error.
func mustGitEnv(t *testing.T, cwd string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitHashBlob writes content as a blob and returns its object id.
func gitHashBlob(t *testing.T, cwd, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// touchFuture bumps the mtime of path well into the future so git treats the
// index entry as stat-dirty and re-runs its filter on the next status.
func touchFuture(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// plantAutostashChain builds the host-code-execution chain from the #3323
// issue comment in the worker worktree: a committed submodule "sub" with a
// ".gitmodules" `ignore = none` entry and an evil clean filter in its own git
// dir, plus a rebase-merge dir whose autostash file names a stash-shaped
// commit. On the host, `git rebase --abort` applies that autostash, runs a
// submodule status, and the submodule filter runs as the user — even with the
// pinned `diff.ignoreSubmodules=all`, which the `ignore = none` entry
// overrides. It returns the worker git dir and the pre-plant HEAD.
func plantAutostashChain(t *testing.T, l preflightLayout) (gitDir, origHead string) {
	t.Helper()
	sub := filepath.Join(l.worker, "sub")
	mustGit(t, "", "init", "-q", sub)
	writeFile(t, filepath.Join(sub, "f.txt"), "hello\n")
	mustGit(t, sub, "add", "f.txt")
	mustGit(t, sub, "commit", "-q", "-m", "subinit")
	mustGit(t, sub, "config", "filter.evil.clean", fmt.Sprintf("echo pwned >> '%s'; cat", l.marker))
	if err := os.MkdirAll(filepath.Join(sub, ".git", "info"), 0o755); err != nil {
		t.Fatalf("mkdir sub/.git/info: %v", err)
	}
	writeFile(t, filepath.Join(sub, ".git", "info", "attributes"), "* filter=evil\n")
	subSHA := gitOut(t, sub, "rev-parse", "HEAD")

	mustGit(t, l.worker, "update-index", "--add", "--cacheinfo", "160000,"+subSHA+",sub")
	writeFile(t, filepath.Join(l.worker, ".gitmodules"),
		"[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n\tignore = none\n")
	mustGit(t, l.worker, "add", ".gitmodules")
	mustGit(t, l.worker, "commit", "-q", "-m", "add submodule sub")

	resolved, err := git.ResolveWorktreeGitDir(l.worker)
	if err != nil {
		t.Fatalf("resolve git dir: %v", err)
	}
	gitDir = resolved
	origHead = gitOut(t, l.worker, "rev-parse", "HEAD")
	writeRebaseMerge(t, l.worker, gitDir, origHead)
	return gitDir, origHead
}

// writeRebaseMerge writes the rebase-merge state dir for the worker worktree
// with an autostash file naming a freshly built stash-shaped commit, and
// makes the submodule file stat-dirty so the clean filter re-runs on status.
func writeRebaseMerge(t *testing.T, worker, gitDir, origHead string) {
	t.Helper()
	stash := stashShapedCommit(t, worker, origHead)
	dir := filepath.Join(gitDir, "rebase-merge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir rebase-merge: %v", err)
	}
	writeFile(t, filepath.Join(dir, "head-name"), "refs/heads/feat\n")
	writeFile(t, filepath.Join(dir, "orig-head"), origHead+"\n")
	writeFile(t, filepath.Join(dir, "onto"), origHead+"\n")
	writeFile(t, filepath.Join(dir, "autostash"), stash+"\n")
	touchFuture(t, filepath.Join(worker, "sub", "f.txt"))
}

// stashShapedCommit builds a stash-shaped commit with git commit-tree (no
// `git stash`, which is blocked for worker sessions). The work-tree parent is
// the index commit, so `git stash apply` accepts it. The scratch index keeps
// the worker's own index untouched.
func stashShapedCommit(t *testing.T, worker, origHead string) string {
	t.Helper()
	baseTree := gitOut(t, worker, "rev-parse", origHead+"^{tree}")
	iCommit := gitOut(t, worker, "commit-tree", baseTree, "-p", origHead, "-m", "index on feat")
	blob := gitHashBlob(t, worker, "stashed content\n")
	scratch := filepath.Join(t.TempDir(), "scratch-index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+scratch)
	mustGitEnv(t, worker, env, "read-tree", baseTree)
	mustGitEnv(t, worker, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+",stashed.txt")
	wtree := gitOutEnv(t, worker, env, "write-tree")
	return gitOut(t, worker, "commit-tree", wtree, "-p", origHead, "-p", iCommit, "-m", "WIP on feat")
}

// TestPreflight_HostGit_RefusesNonWorktree plants an evil core.fsmonitor in a
// plain clone (no .bare above it) and shows that Preflight refuses it and runs
// no host git command in its dir (#3323, AC1).
func TestPreflight_HostGit_RefusesNonWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	plain := filepath.Join(t.TempDir(), "plain")
	mustGit(t, "", "clone", "-q", l.origin, plain)
	mustGit(t, plain, "config", "core.fsmonitor", fmt.Sprintf("echo pwned >> '%s'; false", l.marker))

	err := Preflight(PreflightOpts{Worktree: plain, Rebase: true})
	if err == nil || !strings.Contains(err.Error(), "not a prism worktree") {
		t.Fatalf("Preflight = %v, want a refusal naming 'not a prism worktree'", err)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("Preflight ran a host git command in the non-worktree dir: %v", lines)
	}
}

// TestPreflight_HostGit_RefusesPreexistingRebaseState plants each in-progress
// operation marker in the worker git dir and shows that Preflight refuses it
// by name (#3323, AC2).
func TestPreflight_HostGit_RefusesPreexistingRebaseState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	cases := []struct {
		name  string
		isDir bool
	}{
		{"rebase-merge", true},
		{"rebase-apply", true},
		{"sequencer", true},
		{"MERGE_HEAD", false},
		{"CHERRY_PICK_HEAD", false},
		{"REVERT_HEAD", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newPreflightLayout(t)
			gitDir, err := git.ResolveWorktreeGitDir(l.worker)
			if err != nil {
				t.Fatalf("resolve git dir: %v", err)
			}
			if tc.isDir {
				if err := os.MkdirAll(filepath.Join(gitDir, tc.name), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", tc.name, err)
				}
			} else {
				writeFile(t, filepath.Join(gitDir, tc.name), gitOut(t, l.worker, "rev-parse", "HEAD")+"\n")
			}
			err = Preflight(PreflightOpts{Worktree: l.worker, Rebase: true})
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("Preflight = %v, want a refusal naming %q", err, tc.name)
			}
		})
	}
}

// TestPreflight_HostGit_RebaseAutostashChainRefused is the #3323 regression
// test. The control shows the chain runs on the host through the old restore
// (`git rebase --abort`) even with the pinned config. The fix shows Preflight
// refuses the pre-existing rebase state before any git command runs, so the
// payload never runs (AC2, AC3).
func TestPreflight_HostGit_RebaseAutostashChainRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	gitDir, origHead := plantAutostashChain(t, l)

	// Control: the host's old restore (`git rebase --abort`) through a pinned
	// Repo applies the autostash and runs the submodule filter on the host.
	// The pin does not stop this (the reason #3323 exists).
	repo, err := git.OpenDir(l.worker)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	_, _ = repo.CombinedOutput("rebase", "--abort")
	if len(l.takeMarker(t)) == 0 {
		t.Fatal("control: git rebase --abort did not run the planted payload; the plant does not work")
	}

	// Re-plant the rebase-merge state that the control consumed.
	writeRebaseMerge(t, l.worker, gitDir, origHead)

	// Fix: Preflight refuses the pre-existing rebase state before running any
	// git command, so the payload never runs.
	err = Preflight(PreflightOpts{Worktree: l.worker, Rebase: true})
	if err == nil || !strings.Contains(err.Error(), "rebase-merge") {
		t.Fatalf("Preflight = %v, want a refusal naming rebase-merge state", err)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("Preflight ran the planted payload: %v", lines)
	}
}

// TestPreflight_HostGit_RestoreAppliesNoStash shows that the conflict restore
// applies no stash: it plants the autostash chain, confirms with the control
// that `git rebase --abort` would run the payload, then runs
// restoreAfterPreflightRebase directly and shows the payload does not run and
// the worktree is restored (#3323, AC5).
func TestPreflight_HostGit_RestoreAppliesNoStash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	gitDir, origHead := plantAutostashChain(t, l)

	repo, err := git.OpenDir(l.worker)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	_, _ = repo.CombinedOutput("rebase", "--abort")
	if len(l.takeMarker(t)) == 0 {
		t.Fatal("control: the plant does not run the payload")
	}

	// Re-plant and simulate a mid-rebase state: rebase-merge present and HEAD
	// detached. Write HEAD directly so no git command runs.
	writeRebaseMerge(t, l.worker, gitDir, origHead)
	writeFile(t, filepath.Join(gitDir, "HEAD"), origHead+"\n")

	restoreAfterPreflightRebase(realGit{repo: repo}, origHead)

	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("restore applied a stash and ran the payload: %v", lines)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); !os.IsNotExist(err) {
		t.Errorf("rebase-merge still present after restore: %v", err)
	}
	if ref := gitOut(t, l.worker, "symbolic-ref", "HEAD"); ref != "refs/heads/feat" {
		t.Errorf("HEAD = %q, want refs/heads/feat", ref)
	}
	if head := gitOut(t, l.worker, "rev-parse", "HEAD"); head != origHead {
		t.Errorf("HEAD commit = %q, want origHead %q", head, origHead)
	}
}

// TestPreflight_HostGit_RebaseConflictRestores drives a real rebase conflict
// and shows the worktree returns to its pre-flight state with no stash applied
// (#3323, AC5): HEAD on the session branch at the original commit, no
// rebase-merge dir, clean tree.
func TestPreflight_HostGit_RebaseConflictRestores(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	writeFile(t, filepath.Join(l.worker, "README.md"), "feat change\n")
	mustGit(t, l.worker, "add", "README.md")
	mustGit(t, l.worker, "commit", "-q", "-m", "feat edits README")
	origHead := gitOut(t, l.worker, "rev-parse", "HEAD")

	// Advance origin/main with a conflicting README via a scratch clone.
	scratch := filepath.Join(t.TempDir(), "scratch")
	mustGit(t, "", "clone", "-q", l.origin, scratch)
	writeFile(t, filepath.Join(scratch, "README.md"), "main change\n")
	mustGit(t, scratch, "add", "README.md")
	mustGit(t, scratch, "commit", "-q", "-m", "main edits README")
	mustGit(t, scratch, "push", "-q", "origin", "main")

	err := Preflight(PreflightOpts{Worktree: l.worker, Rebase: true})
	if err == nil || !strings.Contains(err.Error(), "produced conflicts") {
		t.Fatalf("Preflight = %v, want a conflict error", err)
	}

	if ref := gitOut(t, l.worker, "symbolic-ref", "HEAD"); ref != "refs/heads/feat" {
		t.Errorf("HEAD = %q, want refs/heads/feat", ref)
	}
	if head := gitOut(t, l.worker, "rev-parse", "HEAD"); head != origHead {
		t.Errorf("HEAD commit = %q, want origHead %q", head, origHead)
	}
	gitDir, err := git.ResolveWorktreeGitDir(l.worker)
	if err != nil {
		t.Fatalf("resolve git dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); !os.IsNotExist(err) {
		t.Errorf("rebase-merge still present after restore: %v", err)
	}
	if out := gitOut(t, l.worker, "status", "--porcelain"); out != "" {
		t.Errorf("worktree not clean after restore: %q", out)
	}
}

// TestPreflight_HostGit_SuccessPathBranchMovePlantClosed is the #3335
// regression test for the success-path branch-move and autostash races. A
// worker can plant rebase-merge/head-name, orig-head, or autostash while a
// host git rebase runs; git re-reads head-name/orig-head when the rebase
// finishes and moves an arbitrary local branch (for example main), and it
// applies autostash on both the success and the abort path. The fix runs the
// --rebase steps in the session sandbox, so the host runs no rebase for a
// sandboxed session (see TestHostAPI_Review_RebaseRefusedOnHost and
// TestRunReview_SandboxedRebaseRunsInSandboxNotHost for that routing).
//
// Control: a real host git rebase that reads a planted head-name pointing at
// main moves the local main branch — the escalation this issue removes.
// Fix: the non-rebase host pre-flight (what a sandboxed session's host
// subprocess runs) performs no rebase, so the same planted head-name,
// orig-head, and autostash move no local branch and run no payload.
func TestPreflight_HostGit_SuccessPathBranchMovePlantClosed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	l.advance(t)
	bare := filepath.Join(l.root, ".bare")

	// The local main branch that a planted head-name would hijack.
	mainBefore := gitOut(t, "", "--git-dir", bare, "rev-parse", "refs/heads/main")

	// Control: a host git rebase whose step rewrites head-name->main and
	// orig-head mid-rebase makes git move refs/heads/main when the rebase
	// finishes. Run it in a throwaway clone so it does not disturb the worker
	// worktree the fix leg uses.
	ctl := filepath.Join(t.TempDir(), "ctl")
	mustGit(t, "", "clone", "-q", "-b", "feat", l.origin, ctl)
	mustGit(t, ctl, "branch", "-f", "main", "origin/main")
	ctlMainBefore := gitOut(t, ctl, "rev-parse", "refs/heads/main")
	plant := `gd=$(git rev-parse --git-dir); echo refs/heads/main > "$gd/rebase-merge/head-name"; git rev-parse refs/heads/main > "$gd/rebase-merge/orig-head"`
	mustGit(t, ctl, "rebase", "-x", plant, "origin/main")
	if ctlMainAfter := gitOut(t, ctl, "rev-parse", "refs/heads/main"); ctlMainAfter == ctlMainBefore {
		t.Fatal("control: the head-name plant did not move main; the control does not reproduce the #3335 escalation")
	}

	// Fix: plant the full chain (autostash + head-name->main + orig-head) in
	// the worker git dir, then run the pre-flight WITHOUT --rebase — what a
	// sandboxed session's host subprocess runs. The host runs no rebase, so
	// main does not move and no payload runs.
	gitDir, _ := plantAutostashChain(t, l)
	writeFile(t, filepath.Join(gitDir, "rebase-merge", "head-name"), "refs/heads/main\n")
	writeFile(t, filepath.Join(gitDir, "rebase-merge", "orig-head"), mainBefore+"\n")

	err := Preflight(PreflightOpts{Worktree: l.worker, Rebase: false})
	if err == nil {
		t.Fatal("Preflight(Rebase:false) with a planted rebase-merge = nil, want a refusal that runs no rebase")
	}
	if mainAfter := gitOut(t, "", "--git-dir", bare, "rev-parse", "refs/heads/main"); mainAfter != mainBefore {
		t.Errorf("host moved refs/heads/main from %q to %q with Rebase:false", mainBefore, mainAfter)
	}
	if lines := l.takeMarker(t); len(lines) != 0 {
		t.Fatalf("host ran a planted payload with Rebase:false: %v", lines)
	}
}

// TestEnsurePrismWorktree_HostGit checks that EnsurePrismWorktree accepts a
// prism worktree and refuses a plain clone. The control shows that a forge
// read runs git in the plain clone's dir — the host git run that AC1 forbids
// for a non-worktree session, and that the cmd-level gate now prevents (#3323,
// AC1).
func TestEnsurePrismWorktree_HostGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)

	if err := EnsurePrismWorktree(l.worker); err != nil {
		t.Errorf("EnsurePrismWorktree(worktree) = %v, want nil", err)
	}

	plain := filepath.Join(t.TempDir(), "plain")
	mustGit(t, "", "clone", "-q", l.origin, plain)
	// Control: forge.OriginRemoteURL runs git in the dir and reads its config
	// (it returns the origin), the host git run AC1 forbids for a non-worktree
	// session. EnsurePrismWorktree lets cmd/review.go gate it.
	if url := forge.OriginRemoteURL(plain); url == "" {
		t.Fatal("control: forge.OriginRemoteURL ran no git in the plain clone; the control does not work")
	}
	if err := EnsurePrismWorktree(plain); err == nil || !strings.Contains(err.Error(), "not a prism worktree") {
		t.Errorf("EnsurePrismWorktree(plain clone) = %v, want a 'not a prism worktree' refusal", err)
	}
}

// TestPreflight_HostGit_RefusesRebaseWhenHeadOffSessionBranch moves HEAD onto
// a branch other than the one the worktree path names, then shows that
// --rebase is refused before the rebase, and that neither branch moves (review
// round 1 on PR #3332). The rebase acts on HEAD, so it must act on the branch
// that the push and the restore act on.
func TestPreflight_HostGit_RefusesRebaseWhenHeadOffSessionBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	l := newPreflightLayout(t)
	l.advance(t) // origin/main advances, so --rebase would do real work
	mustGit(t, l.worker, "switch", "-q", "-c", "feat-v2")
	featTip := gitOut(t, l.worker, "rev-parse", "refs/heads/feat")
	v2Tip := gitOut(t, l.worker, "rev-parse", "refs/heads/feat-v2")

	err := Preflight(PreflightOpts{Worktree: l.worker, Rebase: true})
	if err == nil || !strings.Contains(err.Error(), "not on the session branch") {
		t.Fatalf("Preflight = %v, want a refusal that HEAD is not on the session branch", err)
	}
	if got := gitOut(t, l.worker, "rev-parse", "refs/heads/feat"); got != featTip {
		t.Errorf("feat moved to %q, want %q (the gate must refuse before the rebase)", got, featTip)
	}
	if got := gitOut(t, l.worker, "rev-parse", "refs/heads/feat-v2"); got != v2Tip {
		t.Errorf("feat-v2 moved to %q, want %q", got, v2Tip)
	}
}
