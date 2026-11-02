package cmd

// Tests for #3396: the prism switch picker and `prism switch --path` start
// a session only in a worktree of the bare+worktree layout, not through a
// symlink in a worktree to a dir outside the layout.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
)

// symlinkWorktreeFixture is a bare+worktree project root with the
// worktrees main and feat. The feat worktree holds link, a symlink to
// target, a dir outside the layout. The gitdir file of the feat entry names
// link/.git, so `git worktree list` lists link. A session in feat can make
// this change.
type symlinkWorktreeFixture struct {
	root    string
	main    string
	link    string
	real    string // the real path of the target of link
	tmuxLog string
}

func newSymlinkWorktreeFixture(t *testing.T) symlinkWorktreeFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	seedHookRows(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tmuxLog := withConvertCheckTmux(t, noTmuxServerListing)

	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"project_locations":      []string{code},
		"project_specific":       []string{},
		"default_isolation_mode": "host",
		"pi_extension_dir":       "/test/prism-pi-extension",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	src := filepath.Join(t.TempDir(), "src")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f := symlinkWorktreeFixture{root: filepath.Join(code, "sw3396"), tmuxLog: tmuxLog}
	f.main = filepath.Join(f.root, "main")
	if err := git.CloneWorktree(src, f.root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if _, err := git.CreateWorktree(f.root, "feat"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.real, err = filepath.EvalSymlinks(target); err != nil {
		t.Fatal(err)
	}
	f.link = filepath.Join(f.root, "feat", "link")
	if err := os.Symlink(target, f.link); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(f.root, ".bare", "worktrees", "feat")
	if err := os.WriteFile(filepath.Join(target, ".git"), []byte("gitdir: "+entry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "gitdir"), []byte(filepath.Join(f.link, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !listsPath(git.Worktrees(f.root), f.link) {
		t.Fatalf("control: git worktree list = %q, want it to list %s", git.Worktrees(f.root), f.link)
	}
	return f
}

// newSessionDirs returns the -c dir of each tmux new-session call.
func (f symlinkWorktreeFixture) newSessionDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	for _, c := range tmuxCalls(t, f.tmuxLog) {
		if c[0] != "new-session" {
			continue
		}
		if i := slices.Index(c, "-c"); i >= 0 && i+1 < len(c) {
			dirs = append(dirs, c[i+1])
		}
	}
	return dirs
}

// assertRefused fails t unless err refuses the start at the symlink, names
// the path and its real target, and no tmux session started.
func (f symlinkWorktreeFixture) assertRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "does not start a session in "+f.link) || !strings.Contains(err.Error(), "resolves to "+f.real) {
		t.Errorf("start at %s = %v, want a refusal that names the path and %s", f.link, err, f.real)
	}
	if dirs := f.newSessionDirs(t); len(dirs) != 0 {
		t.Errorf("tmux new-session in %v, want none", dirs)
	}
}

// assertStartedInMain fails t unless the start reached tmux new-session
// in the main worktree. The spy tmux refuses new-session, so err is that
// refusal.
func (f symlinkWorktreeFixture) assertStartedInMain(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "new-session") {
		t.Errorf("start = %v, want the start to reach tmux new-session", err)
	}
	if dirs := f.newSessionDirs(t); !slices.Equal(dirs, []string{f.main}) {
		t.Errorf("tmux new-session in %v, want [%s]", dirs, f.main)
	}
}

// listsPath reports whether paths holds p, after symlinks are resolved.
func listsPath(paths []string, p string) bool {
	want, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	for _, q := range paths {
		if got, err := filepath.EvalSymlinks(q); err == nil && got == want {
			return true
		}
	}
	return false
}

// AC (#3396): the worktree picker of prism switch refuses an entry that is
// a symlink to a dir outside the layout, with an error that names the path
// and its real target. It starts the main worktree as before.
func TestSwitchPicker_RefusesSymlinkedWorktreeEntry_3396(t *testing.T) {
	f := newSymlinkWorktreeFixture(t)
	var chosen string
	prev := pickWorktree
	pickWorktree = func(_ string, items []entry) *entry {
		for i := range items {
			if items[i].path != "" && listsPath([]string{items[i].path}, chosen) {
				return &items[i]
			}
		}
		t.Errorf("the picker has no entry for %s", chosen)
		return nil
	}
	t.Cleanup(func() { pickWorktree = prev })
	start := func(path string) error {
		chosen = path
		return handleBareRepo(f.root, nil, switchTestOpts(), container.CapabilitiesFor(config.IsolationHost), config.LoadFresh())
	}

	f.assertRefused(t, start(f.link))
	f.assertStartedInMain(t, start(f.main))
}

// AC (#3396): `prism switch --path` refuses a symlink in a worktree to a dir
// outside the layout, in the same way as the picker. It starts the main
// worktree as before, by the worktree path and by the project root.
func TestSwitchPath_RefusesSymlinkedWorktree_3396(t *testing.T) {
	f := newSymlinkWorktreeFixture(t)
	setup := switchSetup{cfg: config.LoadFresh(), isoCaps: container.CapabilitiesFor(config.IsolationHost), opts: switchTestOpts()}
	start := func(path string) error {
		var err error
		captureStderrDuringFn(t, func() { err = switchToPath(path, setup) })
		return err
	}

	f.assertRefused(t, start(f.link))
	f.assertStartedInMain(t, start(f.main))
	if err := os.Remove(f.tmuxLog); err != nil {
		t.Fatal(err)
	}
	f.assertStartedInMain(t, start(f.root))
}
