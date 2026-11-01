package cmd

// Tests for #3388: a session in the target of a prism clone cannot write the
// .bare that the clone makes, so a later spawn does not run a planted
// filter on the host.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config/configtest"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
)

// emptyRootWithPlanter makes an empty project root, and starts a session in
// it whose pane runs plantLoop. The session has a sidecar PID file and no
// start record: a start that ended just before the clone took its lock, and
// whose start record comes later from the tmux hook.
func (f *plantFixture) emptyRootWithPlanter(t *testing.T, name string) (root, marker, planted string) {
	t.Helper()
	root = filepath.Join(f.scratch, "code", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	configtest.AllowProjectRoots(t, root)
	marker = filepath.Join(f.scratch, name+"-payload")
	planted = filepath.Join(f.scratch, name+"-planted")
	ready := filepath.Join(f.scratch, name+"-ready")
	pidPath, err := session.SidecarPIDPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.server.run("new-session", "-ds", name, "-c", root,
		"/bin/sh "+f.script+" "+root+" "+marker+" "+planted+" "+ready); err != nil {
		t.Fatalf("start the session in the root: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return root, marker, planted
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not start its loop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AC (#3388): a session in the target of a clone cannot plant a smudge
// filter in the .bare of the clone. The control clones with a lock that
// does nothing, as before #3388, and shows that the plant lands and that
// the worktree add of a spawn runs the payload. With the lock of
// internal/session, the clone finds the session in tmux and refuses.
//
// A start during the clone is refused before tmux. prism switch --path, the
// picker, and the /switch host API start the session through
// ensureAndSwitch.
func TestCloneWorktree_SessionInTargetCannotPlantInNewBare_3388(t *testing.T) {
	f := newPlantFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	t.Run("control: a clone with a lock that does nothing", func(t *testing.T) {
		t.Cleanup(git.SetCloneTargetLockForTest(func(string) (func(), error) { return func() {}, nil }))
		root, marker, planted := f.emptyRootWithPlanter(t, "cl3388ctl")
		if err := git.CloneWorktree(f.origin, root, func(string) {}); err != nil {
			t.Fatalf("CloneWorktree: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !hasPlant(root) || !fileExists(planted) {
			if time.Now().After(deadline) {
				t.Fatal("control: the session did not plant the filter; the test does not reproduce the issue")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = f.server.run("kill-session", "-t", "=cl3388ctl")
		if _, err := git.CreateWorktree(root, "spawned"); err != nil {
			t.Fatalf("control: CreateWorktree: %v", err)
		}
		if got, _ := os.ReadFile(marker); !strings.Contains(string(got), "smudge") {
			t.Fatalf("control: the worktree add of a spawn ran %q, want the smudge payload", got)
		}
	})

	t.Run("the clone refuses a session in the target", func(t *testing.T) {
		root, marker, planted := f.emptyRootWithPlanter(t, "cl3388fix")
		err := git.CloneWorktree(f.origin, root, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "clone into "+root) || !strings.Contains(err.Error(), "the session cl3388fix") {
			t.Fatalf("CloneWorktree = %v, want a refusal that names the dir and the session", err)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := os.Lstat(filepath.Join(root, ".bare")); !os.IsNotExist(err) || fileExists(planted) {
			t.Fatalf("the refused clone made %s/.bare (%v), or the session planted", root, err)
		}
		_ = f.server.run("kill-session", "-t", "=cl3388fix")
		if got, _ := os.ReadFile(marker); len(got) != 0 {
			t.Errorf("the payload ran: %q", got)
		}
	})

	t.Run("a start during the clone is refused", func(t *testing.T) {
		root := filepath.Join(f.scratch, "code", "cl3388start")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		configtest.AllowProjectRoots(t, root)
		name := mustNameFor(t, root, "")
		var steps []string
		err := git.CloneWorktree(f.origin, root, func(msg string) {
			if !strings.HasPrefix(msg, "cloning ") && !strings.HasPrefix(msg, "creating worktree") {
				return
			}
			steps = append(steps, msg)
			opts := session.Opts{Headless: true, IsolationMode: "bwrap", HarnessName: "pi"}
			err := ensureAndSwitch(root, "", opts)
			if !errors.Is(err, session.ErrConvertInProgress) || !strings.Contains(err.Error(), "start a session in "+root) {
				t.Errorf("at %q: ensureAndSwitch(%s) = %v, want ErrConvertInProgress with the dir", msg, root, err)
			}
			if f.server.hasSession(name) {
				t.Errorf("at %q: the refused start made the tmux session %s", msg, name)
			}
		})
		if err != nil {
			t.Fatalf("CloneWorktree: %v", err)
		}
		if len(steps) != 2 {
			t.Fatalf("steps with a start = %q, want the bare clone and the worktree", steps)
		}
	})
}
