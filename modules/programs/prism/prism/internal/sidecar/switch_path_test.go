package sidecar

// switch_path_test.go — /switch takes the dir from the prism project roots in
// config.json and from host git, not from agent_status (issue #3330).

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/git"
)

const (
	swRepo  = "prism-test-sw"
	swPlain = "prism-test-swplain"
)

// switchLayout is a project location with a bare+worktree project (worktrees
// main and feat/x), a project_specific dir with no .bare, and a dir outside
// every project root with a copy of the project (a stand-in for ~/.npm).
type switchLayout struct {
	location string
	root     string
	main     string
	feat     string
	plain    string
	writable string // a copy of the project root outside the project roots
}

func newSwitchLayout(t *testing.T) switchLayout {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")

	base := t.TempDir()
	l := switchLayout{
		location: filepath.Join(base, "code"),
		plain:    filepath.Join(base, "documents", swPlain),
	}
	l.root = filepath.Join(l.location, swRepo)
	l.main = filepath.Join(l.root, "main")
	l.feat = filepath.Join(l.root, "feat", "x")
	for _, d := range []string{l.location, l.plain} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSwitchConfig(t, []string{l.location}, []string{l.plain})

	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := git.CloneWorktree(src, l.root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if _, err := git.CreateWorktree(l.root, "feat/x"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	// The same project in a dir that a sandbox can write.
	l.writable = filepath.Join(base, "npm", swRepo)
	if err := git.CloneWorktree(src, l.writable, func(string) {}); err == nil {
		t.Fatal("CloneWorktree outside the project roots succeeded; the config does not work")
	}
	if out, err := exec.Command("cp", "-R", l.root, l.writable).CombinedOutput(); err != nil {
		t.Fatalf("copy the project: %v\n%s", err, out)
	}
	return l
}

func writeSwitchConfig(t *testing.T, locations, specific []string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"project_locations": locations, "project_specific": specific})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

// TestSwitchPathForSession is the decision table of switchPathForSession.
func TestSwitchPathForSession(t *testing.T) {
	l := newSwitchLayout(t)

	ok := []struct {
		session string
		want    string
	}{
		{swRepo + "@main", l.main},
		{swRepo + "@feat--x", l.feat},
		{swRepo + "@feat--x~review-1-review-goal", l.feat},
		{swRepo + "@main~investigate-y~review-1-review-qa", l.main},
		{swPlain, l.plain},
		{swPlain + "~investigate-x", l.plain},
	}
	for _, tc := range ok {
		got, err := switchPathForSession(tc.session)
		if err != nil || got != tc.want {
			t.Errorf("switchPathForSession(%q) = %q, %v; want %q", tc.session, got, err, tc.want)
		}
	}

	notFound := []string{
		swRepo + "@gone",
		swRepo + "@x",     // the basename of the feat/x worktree, not its branch
		swRepo,            // a bare name for a bare+worktree root
		swPlain + "@main", // a branch for a root with no .bare
		"prism-test-swother@main",
		"npm",
		"scratchpad",
	}
	for _, name := range notFound {
		if got, err := switchPathForSession(name); !errors.Is(err, errNoSwitchTarget) {
			t.Errorf("switchPathForSession(%q) = %q, %v; want errNoSwitchTarget", name, got, err)
		}
	}
}

// TestSwitchPathForSession_SymlinkedLocation: a project location in
// config.json that goes through a symlink gives the path under the
// configured location, also when git lists the resolved path.
func TestSwitchPathForSession_SymlinkedLocation(t *testing.T) {
	l := newSwitchLayout(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(l.location, alias); err != nil {
		t.Fatal(err)
	}
	writeSwitchConfig(t, []string{alias}, nil)
	want := filepath.Join(alias, swRepo, "feat", "x")
	if got, err := switchPathForSession(swRepo + "@feat--x"); err != nil || got != want {
		t.Errorf("switchPathForSession = %q, %v; want %q", got, err, want)
	}
}

// TestSwitchPathForSession_RedirectedOwnEntry: a session can write the
// gitdir file of its own entry in .bare/worktrees, so `git worktree list`
// lists the path that it chose. A path outside the project root, and a
// subdir of another worktree, are not a switch target.
func TestSwitchPathForSession_RedirectedOwnEntry(t *testing.T) {
	l := newSwitchLayout(t)
	gitdir := filepath.Join(l.root, ".bare", "worktrees", "x", "gitdir")
	if _, err := os.Stat(gitdir); err != nil {
		t.Fatalf("the feat/x entry: %v", err)
	}
	sub := filepath.Join(l.main, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(l.writable, "feat", "x")
	for _, target := range []string{outside, sub} {
		if err := os.WriteFile(gitdir, []byte(filepath.Join(target, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !listsPath(git.Worktrees(l.root), target) {
			t.Fatalf("control: git worktree list = %q, want it to list %s", git.Worktrees(l.root), target)
		}
		for _, name := range []string{swRepo + "@feat--x", swRepo + "@main"} {
			got, err := switchPathForSession(name)
			if got == target {
				t.Errorf("gitdir -> %s: switchPathForSession(%q) = %q, the redirected path", target, name, got)
			}
			if name == swRepo+"@main" && (err != nil || got != l.main) {
				t.Errorf("gitdir -> %s: switchPathForSession(%q) = %q, %v; want %q", target, name, got, err, l.main)
			}
		}
	}
}

// TestHostAPI_Switch_IgnoresTheAgentStatusWorktree is AC 2 of #3330. Each
// target session has an agent_status row whose worktree is a dir outside the
// prism project roots: a copy of the project under a writable dir. That is
// the row of step 1 of the issue, if /event writes it. A coordinator
// /switch must run `prism switch --path` with the dir from the project roots,
// not with the dir from the row. If the handler takes the dir from the row,
// this test fails.
func TestHostAPI_Switch_IgnoresTheAgentStatusWorktree(t *testing.T) {
	l := newSwitchLayout(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stub := "#!/bin/sh\necho \"$*\" >> '" + argvLog + "'\nexit 0\n"
	sc, d := nwrSidecar(t, swRepo+"@main", "coordinator", l.main, "coordinator", stub, nil)

	cases := []struct {
		session string
		row     string
		want    string
	}{
		{swRepo + "@feat--x", filepath.Join(l.writable, "feat", "x"), l.feat},
		{swRepo + "@main", filepath.Join(l.writable, "main"), l.main},
		{swPlain, l.writable, l.plain},
	}
	for _, tc := range cases {
		if err := d.UpsertStatus(tc.session, sessionRepoForTest(tc.session), tc.row, "idle", nil, nil); err != nil {
			t.Fatalf("seed %q: %v", tc.session, err)
		}
		_ = os.Remove(argvLog)
		rr := doHostAPI(t, sc, http.MethodPost, "/switch", `{"session":"`+tc.session+`"}`)
		logged, _ := os.ReadFile(argvLog)
		argv := strings.TrimSpace(string(logged))
		if rr.Code != http.StatusOK || argv != "switch --path "+tc.want {
			t.Errorf("/switch %s: status = %d, body = %s, argv = %q; want 200 and %q",
				tc.session, rr.Code, rr.Body.String(), argv, "switch --path "+tc.want)
		}
	}

	// A row for a name that is in no project root: 404, and no exec.
	const planted = "prism-test-swnpm"
	if err := d.UpsertStatus(planted, "", l.writable, "idle", nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(argvLog)
	rr := doHostAPI(t, sc, http.MethodPost, "/switch", `{"session":"`+planted+`"}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("/switch %s: status = %d, body = %s, want 404", planted, rr.Code, rr.Body.String())
	}
	if logged, _ := os.ReadFile(argvLog); len(logged) != 0 {
		t.Errorf("/switch %s ran `prism %s`", planted, strings.TrimSpace(string(logged)))
	}
}

// listsPath reports whether paths holds p, after symlinks are resolved. git
// can list a resolved path (for example /private/var on macOS).
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

func sessionRepoForTest(name string) string {
	if i := strings.Index(name, "@"); i >= 0 {
		return name[:i]
	}
	return ""
}
