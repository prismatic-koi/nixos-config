package session

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
)

// AC (#3381): this package gives git.CloneWorktree its check of the running
// sessions, and internal/db gives it the session start records. A clone
// refuses while tmux lists a session that started in a dir above the
// target, also when the start record of that session has an end time.
func TestCloneWorktree_AsksTmuxForSessionsAbove_3381(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Cleanup(git.AllowFileTransportForTest())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectLocationConfig(t, code)

	src := filepath.Join(t.TempDir(), "src")
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"clone", "-q", "--bare", src, origin},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	d, err := db.Open(filepath.Join(state, "prism", "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := d.InsertSession(db.Session{InstanceID: "i1", SessionName: "code", Repo: "code", Worktree: code}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	d.Close()

	tmuxLists := "code\n"
	prev := listTmuxSessions
	listTmuxSessions = func() (string, error) { return tmuxLists, nil }
	t.Cleanup(func() { listTmuxSessions = prev })

	target := filepath.Join(code, "repo")
	err = git.CloneWorktree(origin, target, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "the session code runs") {
		t.Fatalf("CloneWorktree while tmux lists the session above = %v, want a refusal that names it", err)
	}

	tmuxLists = "other\n"
	if err := git.CloneWorktree(origin, target, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree after the session stopped: %v", err)
	}
}

// writeProjectLocationConfig points PRISM_CONFIG_FILE at a config.json with
// the one project location.
func writeProjectLocationConfig(t *testing.T, location string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"project_locations": []string{location}, "project_specific": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

// AC (#3381): Create refuses a sandboxed session in a project location and
// in a dir above it, before it takes the start lock and before tmux. The
// error names the dir. A host session is not refused for this reason: it
// reaches the start lock, which a conversion holds in this test.
func TestCreate_RefusesSandboxedSessionAboveProjectRoots_3381(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(base, "code")
	for _, d := range []string{filepath.Join(code, "repo", ".bare"), filepath.Join(code, "repo", "main")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectLocationConfig(t, code)

	for _, dir := range []string{code, base} {
		lock, err := LockForConvert(dir, time.Second)
		if err != nil {
			t.Fatalf("LockForConvert(%s): %v", dir, err)
		}
		for _, mode := range []string{"bwrap", "sandbox-exec"} {
			name := "prism-test-above-root-" + mode
			opts := Opts{Layout: LayoutFull, IsolationMode: mode, HarnessName: "pi", SessionName: name}
			err := Create(name, dir, opts)
			if !errors.Is(err, container.ErrSessionDirHoldsProjectRoots) {
				t.Errorf("Create(%s, %s) = %v, want ErrSessionDirHoldsProjectRoots", mode, dir, err)
				continue
			}
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("Create(%s, %s) error %q does not name the dir", mode, dir, err)
			}
			if TmuxSessionPresence(name) == TmuxPresent {
				t.Errorf("Create(%s, %s) started a tmux session", mode, dir)
			}
		}
		hostOpts := Opts{Layout: LayoutFull, IsolationMode: "host", HarnessName: "pi", SessionName: "prism-test-above-root-host", PIExtensionDir: t.TempDir()}
		if err := Create("prism-test-above-root-host", dir, hostOpts); !errors.Is(err, ErrConvertInProgress) {
			t.Errorf("Create(host, %s) = %v, want ErrConvertInProgress from the start lock", dir, err)
		}
		lock.Release()
	}

	// A session in a project root reaches the start lock.
	root := filepath.Join(code, "scratch")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := LockForConvert(root, time.Second)
	if err != nil {
		t.Fatalf("LockForConvert(%s): %v", root, err)
	}
	defer lock.Release()
	opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: "prism-test-project-root"}
	if err := Create("prism-test-project-root", root, opts); !errors.Is(err, ErrConvertInProgress) {
		t.Errorf("Create(bwrap, %s) = %v, want ErrConvertInProgress from the start lock", root, err)
	}
}
