package cmd

// Tests for #3361: prism convert and the convert entry of prism switch stop
// the root session of the plain clone, and refuse its restart, before
// ConvertToBare. The session can write the clone, so it can write the .bare
// that the conversion makes, and host git runs in that .bare.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config/configtest"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
)

// withConvertToBare replaces the conversion with fn and returns the number
// of calls.
func withConvertToBare(t *testing.T, fn func(dir string, progress func(string)) (string, error)) *int {
	t.Helper()
	calls := 0
	prev := convertToBare
	convertToBare = func(dir string, progress func(string)) (string, error) {
		calls++
		return fn(dir, progress)
	}
	t.Cleanup(func() { convertToBare = prev })
	return &calls
}

// newPlainCloneDir makes a dir with a .git dir and returns it and the name
// of its root session.
func newPlainCloneDir(t *testing.T, name string) (dir, sessionName string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, mustNameFor(t, dir, "")
}

// AC (#3361): the root session stops, its row ends, and a start in the
// clone is refused, before the conversion runs. The lock goes after the
// conversion.
func TestConvertPlainClone_StopsSessionAndRefusesStartBeforeConversion_3361(t *testing.T) {
	dir, name := newPlainCloneDir(t, "cv3361order")
	dbFile := seedHookRows(t, name)
	tmuxLog := withListingSpyTmux(t, name)
	installFakeRunner(t)
	withChildCleanup(t)
	stops := withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })

	calls := withConvertToBare(t, func(d string, _ func(string)) (string, error) {
		if !slices.Equal(*stops, []string{name}) {
			t.Errorf("sidecar stops before the conversion = %v, want [%s]", *stops, name)
		}
		if !tmuxKilled(t, tmuxLog, name) {
			t.Error("tmux session not killed before the conversion")
		}
		if readStatus(t, dbFile, name).EndedAt == nil {
			t.Error("row open during the conversion, want ended: prism restore starts an open row in the clone")
		}
		opts := session.Opts{Layout: session.LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: name}
		if err := session.Create(name, d, opts); !errors.Is(err, session.ErrConvertInProgress) {
			t.Errorf("session.Create during the conversion = %v, want ErrConvertInProgress", err)
		}
		return filepath.Join(d, "main"), nil
	})

	res, err := convertPlainClone(dir, func(string) {})
	if err != nil {
		t.Fatalf("convertPlainClone: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("conversions = %d, want 1", *calls)
	}
	if !res.ran || res.sessionName != name || res.worktreePath != filepath.Join(dir, "main") {
		t.Errorf("result = %+v, want the session %q as ran, and the main worktree", res, name)
	}
	lock, err := session.LockForConvert(dir, 0)
	if err != nil {
		t.Fatalf("the conversion lock is still held after the conversion: %v", err)
	}
	lock.Release()
}

// AC (#3361): when prism cannot prove that the session stopped, it does not
// convert.
func TestConvertPlainClone_RefusesWithoutAProvenStop_3361(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir, session string)
		noStop bool // no sidecar stop runs
		want   func(err error) bool
	}{
		{
			name: "the sidecar does not stop",
			want: func(err error) bool { return errors.Is(err, errSidecarNotStopped) },
		},
		{
			name: "a pane process still runs after the tmux kill",
			setup: func(t *testing.T, _, _ string) {
				prevPIDs, prevWait := convertPanePIDs, convertPaneWait
				convertPanePIDs = func(string) ([]int, error) { return []int{os.Getpid()}, nil }
				convertPaneWait = 100 * time.Millisecond
				t.Cleanup(func() { convertPanePIDs, convertPaneWait = prevPIDs, prevWait })
			},
			want: func(err error) bool { return err != nil && strings.Contains(err.Error(), "still run") },
		},
		{
			name: "the command runs in a pane of the session",
			setup: func(t *testing.T, _, session string) {
				prev := convertCallerSession
				convertCallerSession = func() string { return session }
				t.Cleanup(func() { convertCallerSession = prev })
			},
			noStop: true,
			want:   func(err error) bool { return err != nil && strings.Contains(err.Error(), "runs in a pane") },
		},
		{
			name:   "the command runs in a sandbox",
			setup:  func(t *testing.T, _, _ string) { t.Setenv("PRISM_HOST_API", "http://127.0.0.1:1") },
			noStop: true,
			want:   func(err error) bool { return errors.Is(err, git.ErrBareInSandbox) },
		},
		{
			name: "the dir is not a plain clone",
			setup: func(t *testing.T, dir, _ string) {
				if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
			},
			noStop: true,
			want:   func(err error) bool { return err != nil && strings.Contains(err.Error(), "not a plain git clone") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, name := newPlainCloneDir(t, "cv3361refuse")
			dbFile := seedHookRows(t, name)
			tmuxLog := withListingSpyTmux(t, name)
			installFakeRunner(t)
			withChildCleanup(t)
			stopOK := c.name != "the sidecar does not stop"
			stops := withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return stopOK })
			calls := withConvertToBare(t, func(string, func(string)) (string, error) { return "", nil })
			if c.setup != nil {
				c.setup(t, dir, name)
			}

			var err error
			captureStderrDuringFn(t, func() { _, err = convertPlainClone(dir, func(string) {}) })

			if !c.want(err) {
				t.Errorf("convertPlainClone error = %v", err)
			}
			if *calls != 0 {
				t.Errorf("conversions = %d, want 0", *calls)
			}
			if c.noStop {
				if len(*stops) != 0 || tmuxKilled(t, tmuxLog, name) {
					t.Errorf("stops = %v, tmux killed = %v, want no stop", *stops, tmuxKilled(t, tmuxLog, name))
				}
			}
			if !stopOK && tmuxKilled(t, tmuxLog, name) {
				t.Error("tmux session killed after a failed sidecar stop, want kept")
			}
		})
	}
}

// AC (#3361): prism convert starts the session again when it ran before the
// stop: in the new worktree after a conversion, and in the clone after a
// failed conversion. It does not start a session beside a .bare that a
// failed rollback left.
func TestRestartAfterConvert_3361(t *testing.T) {
	type start struct{ dir, projectRoot string }
	var starts []start
	prev := convertStartSession
	convertStartSession = func(dir, projectRoot string) error {
		starts = append(starts, start{dir, projectRoot})
		return nil
	}
	t.Cleanup(func() { convertStartSession = prev })

	clone := t.TempDir()
	worktree := filepath.Join(clone, "main")
	ran := convertResult{worktreePath: worktree, sessionName: "x", ran: true}

	if err := restartAfterConvert(convertResult{worktreePath: worktree}, clone, true); err != nil || len(starts) != 0 {
		t.Fatalf("no session ran: starts = %v, err = %v, want no start", starts, err)
	}
	if err := restartAfterConvert(ran, clone, true); err != nil || !slices.Equal(starts, []start{{worktree, clone}}) {
		t.Fatalf("after a conversion: starts = %v, err = %v, want the worktree", starts, err)
	}
	starts = nil
	if err := restartAfterConvert(ran, clone, false); err != nil || !slices.Equal(starts, []start{{clone, ""}}) {
		t.Fatalf("after a failed conversion: starts = %v, err = %v, want the clone", starts, err)
	}
	starts = nil
	if err := os.Mkdir(filepath.Join(clone, ".bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restartAfterConvert(ran, clone, false); err == nil || len(starts) != 0 {
		t.Fatalf("with a .bare left: starts = %v, err = %v, want a refusal", starts, err)
	}
}

// plantLoop is the work of a root session that tries to plant a smudge
// filter in the .bare of a conversion. It writes the filter to the config
// and selects it in info/attributes each time that git rewrites the config.
const plantLoop = `#!/bin/sh
root=$1; marker=$2; planted=$3; ready=$4
: > "$ready"
while :; do
  if [ -f "$root/.bare/config" ] && ! grep -q 'filter "evil"' "$root/.bare/config"; then
    printf '[filter "evil"]\n\tsmudge = "echo smudge >> %s; cat"\n' "$marker" >> "$root/.bare/config"
    mkdir -p "$root/.bare/info" && printf '* filter=evil\n' > "$root/.bare/info/attributes"
    : > "$planted"
  fi
  sleep 0.01
done
`

type plantFixture struct {
	server  *cmdTestServer
	origin  string
	script  string
	scratch string
}

func newPlantFixture(t *testing.T) *plantFixture {
	t.Helper()
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_COUNT", "0")

	f := &plantFixture{server: s, scratch: t.TempDir()}
	src := filepath.Join(f.scratch, "src")
	f.origin = filepath.Join(f.scratch, "origin.git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", src, "add", "file.txt"},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "file"},
		{"clone", "-q", "--bare", src, f.origin},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f.script = filepath.Join(f.scratch, "plant.sh")
	if err := os.WriteFile(f.script, []byte(plantLoop), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// cloneWithPlanter makes a plain clone in a project root and starts its root
// session, whose pane runs plantLoop. It returns the clone and the paths of
// the payload marker and of the planted marker.
func (f *plantFixture) cloneWithPlanter(t *testing.T, name string) (root, marker, planted string) {
	t.Helper()
	root = filepath.Join(f.scratch, "code", name)
	if out, err := exec.Command("git", "clone", "-q", f.origin, root).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	configtest.AllowProjectRoots(t, root)
	marker = filepath.Join(f.scratch, name+"-payload")
	planted = filepath.Join(f.scratch, name+"-planted")
	ready := filepath.Join(f.scratch, name+"-ready")
	if err := f.server.run("new-session", "-ds", mustNameFor(t, root, ""), "-c", root,
		"/bin/sh "+f.script+" "+root+" "+marker+" "+planted+" "+ready); err != nil {
		t.Fatalf("start the root session: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return root, marker, planted
		}
		if time.Now().After(deadline) {
			t.Fatal("the root session did not start its loop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AC (#3361): the root session of a plain clone cannot plant a smudge filter
// in the .bare of a conversion, so a later spawn does not run it on the
// host. The control converts with ConvertToBare alone while the session
// runs, and shows that the plant lands and that the worktree add of a spawn
// runs the payload.
func TestConvertPlainClone_RootSessionCannotPlantInNewBare_3361(t *testing.T) {
	f := newPlantFixture(t)

	t.Run("control: ConvertToBare while the session runs", func(t *testing.T) {
		root, marker, planted := f.cloneWithPlanter(t, "cv3361ctl")
		if _, err := git.ConvertToBare(root, func(string) {}); err != nil {
			t.Fatalf("ConvertToBare: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !hasPlant(root) || !fileExists(planted) {
			if time.Now().After(deadline) {
				t.Fatal("control: the session did not plant the filter; the test does not reproduce the issue")
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = f.server.run("kill-session", "-t", "="+mustNameFor(t, root, ""))
		if !hasPlant(root) {
			t.Fatal("control: the plant is gone after the session stopped")
		}
		if _, err := git.CreateWorktree(root, "spawned"); err != nil {
			t.Fatalf("control: CreateWorktree: %v", err)
		}
		if got, _ := os.ReadFile(marker); !strings.Contains(string(got), "smudge") {
			t.Fatalf("control: the worktree add of a spawn ran %q, want the smudge payload", got)
		}
	})

	t.Run("convertPlainClone", func(t *testing.T) {
		root, marker, planted := f.cloneWithPlanter(t, "cv3361fix")
		name := mustNameFor(t, root, "")
		dbFile := seedHookRows(t, name)
		installFakeRunner(t)
		d, err := db.Open(dbFile)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer d.Close()
		if err := d.RefreshWorktree(name, "", root); err != nil {
			t.Fatalf("RefreshWorktree: %v", err)
		}
		if err := d.SetIsolationMode(name, "bwrap"); err != nil {
			t.Fatalf("SetIsolationMode: %v", err)
		}
		// A `prism restore` that read its row list before the conversion.
		staleRow := *readStatus(t, dbFile, name)

		res, err := convertPlainClone(root, func(string) {})
		if err != nil {
			t.Fatalf("convertPlainClone: %v", err)
		}
		if !res.ran {
			t.Error("the root session did not count as running before the stop")
		}
		if f.server.hasSession(name) {
			t.Error("the root session still runs after the conversion")
		}
		if readStatus(t, dbFile, name).EndedAt == nil {
			t.Error("the row of the root session is open after the conversion")
		}

		// After the conversion, a restore with the stale row and an "open
		// directly" switch do not start the root session in the root.
		if _, err := restoreProjectSession(d, staleRow, nil, 0); !errors.Is(err, session.ErrStartInBareRoot) {
			t.Errorf("restore with a row read before the conversion = %v, want ErrStartInBareRoot", err)
		}
		openDirect := session.Opts{Headless: true, IsolationMode: "bwrap", HarnessName: "pi"}
		if err := ensureAndSwitch(root, "", openDirect); !errors.Is(err, session.ErrStartInBareRoot) {
			t.Errorf("open directly after the conversion = %v, want ErrStartInBareRoot", err)
		}
		if f.server.hasSession(name) {
			t.Fatal("the root session runs in the root after the conversion")
		}
		time.Sleep(300 * time.Millisecond)
		if hasPlant(root) || fileExists(planted) {
			t.Fatal("the root session planted the filter in the .bare of the conversion")
		}
		if _, err := git.CreateWorktree(root, "spawned"); err != nil {
			t.Fatalf("CreateWorktree: %v", err)
		}
		if got, _ := os.ReadFile(marker); len(got) != 0 {
			t.Errorf("the worktree add of a spawn ran %q, want no payload", got)
		}
		if out, err := exec.Command("git", "-C", res.worktreePath, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
			t.Errorf("git status in the new worktree = %q, %v; want a clean worktree", out, err)
		}
	})
}

func hasPlant(root string) bool {
	cfg, _ := os.ReadFile(filepath.Join(root, ".bare", "config"))
	return strings.Contains(string(cfg), `filter "evil"`) && fileExists(filepath.Join(root, ".bare", "info", "attributes"))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
