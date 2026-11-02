package session

// Tests for #3396: the session check of prism clone and prism convert
// finds a tmux session in a locale that is not UTF-8, and refuses on a line
// of the tmux output that does not parse. The start lock acts on the real
// dir of a start, so a symlink cannot take a start past it.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
	"github.com/prismatic-koi/prism/internal/tmux"
	"github.com/prismatic-koi/prism/internal/tmux/tmuxtest"
)

// cLocaleTmux starts a private tmux server and points tmux.TmuxBin at it.
// Then it sets LC_ALL=C, with no TMUX, LANG, or LC_CTYPE, for the rest of
// t. A tmux client with TMUX set, or in a UTF-8 locale, writes UTF-8
// output. It returns a func that runs a tmux command on the server, with
// no -u.
func cLocaleTmux(t *testing.T) (run func(args ...string) (string, error)) {
	t.Helper()
	bin := tmuxtest.RequireServer(t)
	sock := filepath.Join(stateguardtest.ShortTempDir(t, "t3396"), "s")
	run = func(args ...string) (string, error) {
		out, err := exec.Command(bin, append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run("new-session", "-ds", "bootstrap", "-c", t.TempDir(), "sleep 600"); err != nil {
		t.Fatalf("start the tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = run("kill-server") })

	wrapper := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nexec " + bin + " -S " + sock + " -f /dev/null \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := tmux.TmuxBin
	tmux.TmuxBin = wrapper
	t.Cleanup(func() { tmux.TmuxBin = prev })

	for _, k := range []string{"TMUX", "LANG", "LC_CTYPE"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LC_ALL", "C")
	return run
}

// writeSidecarPIDFile makes name a prism session for SessionsInDir.
func writeSidecarPIDFile(t *testing.T, name string) {
	t.Helper()
	pidPath, err := SidecarPIDPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC (#3396): with LC_ALL=C, SessionsInDir finds a tmux session in the dir,
// also in a dir whose path is not ASCII, and the clone refuses the target
// of such a session. The control shows that tmux with no -u writes no tab
// in this locale. Then no line names a dir.
func TestSessionsInDir_FindsSessionInCLocale_3396(t *testing.T) {
	code, origin := cloneLockFixture(t)
	run := cLocaleTmux(t)
	target := filepath.Join(code, "c3396")
	other := filepath.Join(t.TempDir(), "dir-\u00fc")
	sessions := map[string]string{target: "prism-test-c3396", other: "prism-test-c3396-u"}
	for dir, name := range sessions {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeSidecarPIDFile(t, name)
		if out, err := run("new-session", "-ds", name, "-c", dir, "sleep 600"); err != nil {
			t.Fatalf("start the session %s: %v\n%s", name, err, out)
		}
	}

	out, err := run("list-sessions", "-F", "#{session_name}\t#{session_path}")
	if err != nil || strings.Contains(out, "\t") || !strings.Contains(out, sessions[target]+"_"+target) {
		t.Fatalf("control: tmux with no -u and LC_ALL=C wrote %q, %v; want no tab", out, err)
	}

	for dir, name := range sessions {
		got, err := SessionsInDir(nil, dir)
		if err != nil || !slices.Equal(got, []string{name}) {
			t.Errorf("SessionsInDir(%s) = %v, %v; want [%s]", dir, got, err, name)
		}
	}

	err = git.CloneWorktree(origin, target, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "the session "+sessions[target]) {
		t.Fatalf("CloneWorktree = %v, want a refusal that names the dir and the session", err)
	}
	assertNoBare(t, target)
}

// AC (#3396): a line of the tmux output that does not parse makes
// SessionsInDir return an error that quotes the line. The line can be a
// session in the dir. The clone then refuses. Empty output is no session.
func TestSessionsInDir_LineThatDoesNotParseIsAnError_3396(t *testing.T) {
	code, origin := cloneLockFixture(t)
	dir := filepath.Join(code, "p3396")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out string
	prev := listTmuxSessionPaths
	listTmuxSessionPaths = func() (string, error) { return out, nil }
	t.Cleanup(func() { listTmuxSessionPaths = prev })

	cases := []struct{ name, out, bad string }{
		{"the tab of the C locale", "prism-test-a_" + dir, "prism-test-a_" + dir},
		{"no path", "prism-test-a\t", "prism-test-a\t"},
		{"no name", "\t" + dir, "\t" + dir},
		{"an empty line", "prism-test-a\t/elsewhere\n\nprism-test-b\t/x", ""},
		{"a bad line after a good line", "prism-test-a\t/elsewhere\nbroken", "broken"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out = c.out
			names, err := SessionsInDir(nil, dir)
			if err == nil || !strings.Contains(err.Error(), "do not parse") || !strings.Contains(err.Error(), "The first is "+strconv.Quote(c.bad)) {
				t.Errorf("SessionsInDir = %v, %v; want an error that quotes %q", names, err, c.bad)
			}
		})
	}

	out = ""
	if names, err := SessionsInDir(nil, dir); err != nil || len(names) != 0 {
		t.Errorf("SessionsInDir with no tmux session = %v, %v; want none and no error", names, err)
	}

	out = "broken"
	err := git.CloneWorktree(origin, dir, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "clone into "+dir) || !strings.Contains(err.Error(), "do not parse") {
		t.Fatalf("CloneWorktree = %v, want a refusal that names the dir and the cause", err)
	}
	assertNoBare(t, dir)
}

// AC (#3396): a start through a symlink in a worktree to the target of a
// clone takes the start lock of the target, so it is refused while the
// clone runs. The error names the path and the real dir. The control shows
// that the path as written is in a bare+worktree layout, where a start
// takes no lock.
//
// Before the clone makes the target, the symlink does not resolve, and the
// start is refused. After the clone, the target holds a .bare, and the
// start is refused with ErrStartInBareRoot.
func TestHoldStartLock_SymlinkToCloneTarget_3396(t *testing.T) {
	code, origin := cloneLockFixture(t)
	noTmuxServer(t)
	outer := filepath.Join(t.TempDir(), "outer")
	for _, d := range []string{filepath.Join(outer, ".bare"), filepath.Join(outer, "feat")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(code, "linked")
	link := filepath.Join(outer, "feat", "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if root := git.BareRoot(link); root != outer {
		t.Fatalf("control: BareRoot(%s) = %q, want %s: the path as written is in the layout", link, root, outer)
	}

	const name = "prism-test-link-start"
	opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: name}
	if err := Create(name, link, opts); !errors.Is(err, errStartDirMissing) || !strings.Contains(err.Error(), link) {
		t.Errorf("Create(%s) before the clone = %v, want errStartDirMissing with the path", link, err)
	}

	var steps []string
	err := git.CloneWorktree(origin, target, func(msg string) {
		if !strings.HasPrefix(msg, "cloning ") && !strings.HasPrefix(msg, "creating worktree") {
			return
		}
		steps = append(steps, msg)
		realTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		err = Create(name, link, opts)
		if !errors.Is(err, ErrConvertInProgress) || !strings.Contains(err.Error(), "start a session in "+link) || !strings.Contains(err.Error(), realTarget) {
			t.Errorf("at %q: Create(%s) = %v, want ErrConvertInProgress with the path and the real dir", msg, link, err)
		}
	})
	if err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps with a start = %q, want the bare clone and the worktree", steps)
	}
	if _, err := holdStartLock(link); !errors.Is(err, ErrStartInBareRoot) {
		t.Errorf("holdStartLock(%s) after the clone = %v, want ErrStartInBareRoot", link, err)
	}
}
