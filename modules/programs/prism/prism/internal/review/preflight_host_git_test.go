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
