package session

// Tests for #3388: prism clone holds the lock of convert_lock.go on its
// target, so a session cannot start in the target during the clone and
// write the .bare that the clone makes.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// cloneLockFixture gives a project location in config.json, an origin with
// one commit on main, and a new prism state dir.
func cloneLockFixture(t *testing.T) (code, origin string) {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Cleanup(git.AllowFileTransportForTest())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	code = filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectLocationConfig(t, code)

	src := filepath.Join(t.TempDir(), "src")
	origin = filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"clone", "-q", "--bare", src, origin},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return code, origin
}

// noTmuxServer makes tmux report that no server runs, and refuse each other
// command. A Create that gets past the start lock then fails at tmux
// new-session, and starts nothing.
func noTmuxServer(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = list-sessions ]; then echo 'no server running on /stub/default' >&2; exit 1; fi\n" +
		"echo \"stub tmux: $1 refused\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := tmux.TmuxBin
	tmux.TmuxBin = filepath.Join(dir, "tmux")
	t.Cleanup(func() { tmux.TmuxBin = prev })
}

// writeProjectSpecificConfig points PRISM_CONFIG_FILE at a config.json with
// the one project_specific root.
func writeProjectSpecificConfig(t *testing.T, root string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"project_locations": []string{}, "project_specific": []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

func setCloneLockWait(t *testing.T, wait time.Duration) {
	t.Helper()
	prev := cloneLockWait
	cloneLockWait = wait
	t.Cleanup(func() { cloneLockWait = prev })
}

// tmuxSessionPaths replaces the tmux list of the clone check. The list is
// empty until set is called.
func tmuxSessionPaths(t *testing.T) (set func(list string)) {
	t.Helper()
	var mu sync.Mutex
	list := ""
	prev := listTmuxSessionPaths
	listTmuxSessionPaths = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if list == "" {
			return "", errors.New("tmux list-sessions: exit status 1: no server running on /stub/default")
		}
		return list, nil
	}
	t.Cleanup(func() { listTmuxSessionPaths = prev })
	return func(l string) {
		mu.Lock()
		defer mu.Unlock()
		list = l
	}
}

func assertNoBare(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, ".bare")); !os.IsNotExist(err) {
		t.Errorf("%s/.bare: %v, want none", dir, err)
	}
}

// AC (#3388): while a clone holds its target, Create refuses a session in
// the target, before tmux, with an error that names the dir. The start is
// refused before the bare clone, and after it, when the .bare exists. A
// symlink to the target names the same lock. After the clone, the lock is
// free, and the .bare refuses the start.
//
// prism switch --path, the picker, prism restore, and the /switch host API
// each start the session through Create.
func TestCloneWorktree_RefusesStartInTargetDuringClone_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	noTmuxServer(t)
	target := filepath.Join(code, "repo")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	var steps []string
	err := git.CloneWorktree(origin, target, func(msg string) {
		if !strings.HasPrefix(msg, "cloning ") && !strings.HasPrefix(msg, "creating worktree") {
			return
		}
		steps = append(steps, msg)
		for _, dir := range []string{target, link} {
			const name = "prism-test-start-during-clone"
			opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: name}
			err := Create(name, dir, opts)
			if !errors.Is(err, ErrConvertInProgress) || !strings.Contains(err.Error(), "start a session in "+dir) {
				t.Errorf("at %q: Create(%s) = %v, want ErrConvertInProgress with the dir", msg, dir, err)
			}
		}
	})
	if err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps with a start = %q, want the bare clone and the worktree", steps)
	}
	if _, err := holdStartLock(target); !errors.Is(err, ErrStartInBareRoot) {
		t.Errorf("holdStartLock after the clone = %v, want ErrStartInBareRoot", err)
	}
	if lock, err := LockForConvert(target, 0); err != nil {
		t.Errorf("the clone kept the lock of %s: %v", target, err)
	} else {
		lock.Release()
	}
}

// AC (#3388): a start that holds the start lock when the clone asks for the
// lock ends first. Its tmux session then runs in the target, and its start
// record can come later. The clone waits for the start, finds the tmux
// session in the target, and refuses. It makes no .bare.
func TestCloneWorktree_RefusesSessionThatStartedBeforeTheLock_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	setPaths := tmuxSessionPaths(t)
	setCloneLockWait(t, 10*time.Second)
	target := filepath.Join(code, "early")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "prism-test-early"

	release, err := holdStartLock(target)
	if err != nil {
		t.Fatalf("holdStartLock: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- git.CloneWorktree(origin, target, func(string) {}) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		release()
		t.Fatalf("the clone ended while a start held the lock of the target: %v", err)
	default:
	}

	// The start makes its tmux session and the sidecar PID file, and ends.
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
	setPaths(name + "\t" + target)
	release()

	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the clone did not end")
	}
	if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "the session "+name) {
		t.Fatalf("CloneWorktree = %v, want a refusal that names the dir and the session", err)
	}
	assertNoBare(t, target)
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Errorf("the refused clone changed %s: %v, %v", target, entries, err)
	}
}

// AC (#3388): when prism cannot ask tmux, the clone refuses: a session in
// the target can run.
func TestCloneWorktree_RefusesWhenTmuxGivesNoAnswer_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	prev := listTmuxSessionPaths
	listTmuxSessionPaths = func() (string, error) {
		return "", errors.New("tmux list-sessions: exit status 1: error connecting to /stub/default (Permission denied)")
	}
	t.Cleanup(func() { listTmuxSessionPaths = prev })
	target := filepath.Join(code, "noanswer")

	err := git.CloneWorktree(origin, target, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "cannot tell") {
		t.Fatalf("CloneWorktree = %v, want a refusal that names the dir", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("the refused clone left %s, a dir that it made: %v", target, err)
	}
}

// AC (#3388): a clone into a target whose lock a conversion or another
// clone holds is refused with an error that names the dir. It changes
// nothing in the target, and the other clone ends as before.
func TestCloneWorktree_RefusesTargetLockedByConvertOrClone_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	noTmuxServer(t)
	setCloneLockWait(t, 200*time.Millisecond)

	t.Run("a conversion holds the lock", func(t *testing.T) {
		target := filepath.Join(code, "converting")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		lock, err := LockForConvert(target, 0)
		if err != nil {
			t.Fatalf("LockForConvert: %v", err)
		}
		defer lock.Release()
		var steps []string
		err = git.CloneWorktree(origin, target, func(msg string) { steps = append(steps, msg) })
		if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "did not end") {
			t.Fatalf("CloneWorktree = %v, want a refusal that names the dir", err)
		}
		if len(steps) != 0 {
			t.Errorf("the refused clone started: %q", steps)
		}
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Errorf("the refused clone changed %s: %v, %v", target, entries, err)
		}
	})

	t.Run("another clone holds the lock", func(t *testing.T) {
		target := filepath.Join(code, "twice")
		var second error
		err := git.CloneWorktree(origin, target, func(msg string) {
			if !strings.HasPrefix(msg, "cloning ") {
				return
			}
			second = git.CloneWorktree(origin, target, func(string) {})
			if _, err := os.Stat(target); err != nil {
				t.Errorf("the second clone removed %s, the target of the first clone: %v", target, err)
			}
			assertNoBare(t, target)
		})
		if err != nil {
			t.Fatalf("first CloneWorktree: %v", err)
		}
		if second == nil || !strings.Contains(second.Error(), "clone into "+target) || !strings.Contains(second.Error(), "did not end") {
			t.Fatalf("second CloneWorktree = %v, want a refusal that names the dir", second)
		}
		if _, err := os.Stat(filepath.Join(target, ".bare", "HEAD")); err != nil {
			t.Errorf("the first clone made no .bare: %v", err)
		}
	})
}

// AC (#3388): after a failed clone, the lock is free. A start and a clone
// in the target then work.
func TestCloneWorktree_ReleasesTheLockAfterAFailure_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	noTmuxServer(t)
	target := filepath.Join(code, "retry")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "missing.git")
	if err := git.CloneWorktree(missing, target, func(string) {}); err == nil {
		t.Fatal("CloneWorktree of a missing origin succeeded")
	}
	assertNoBare(t, target)
	release, err := holdStartLock(target)
	if err != nil {
		t.Fatalf("holdStartLock after the failed clone: %v", err)
	}
	release()
	if err := git.CloneWorktree(origin, target, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree after the failed clone: %v", err)
	}
}

// cloneHoldEnv makes the test binary a helper process that runs a clone and
// stops at the bare clone (runCloneHold). The value is the target.
const (
	cloneHoldEnv       = "PRISM_TEST_CLONE_HOLD_TARGET"
	cloneHoldOriginEnv = "PRISM_TEST_CLONE_HOLD_ORIGIN"
	cloneHoldReadyEnv  = "PRISM_TEST_CLONE_HOLD_READY"
	cloneHoldTmuxEnv   = "PRISM_TEST_CLONE_HOLD_TMUX"
)

// The helper process starts here, before TestMain, so it runs no test.
func init() {
	if target := os.Getenv(cloneHoldEnv); target != "" {
		os.Exit(runCloneHold(target))
	}
}

// runCloneHold runs a clone into target, writes the ready file when the
// clone holds the lock, and then waits until the test kills it.
func runCloneHold(target string) int {
	git.AllowFileTransportForTest()
	git.SetCloneTargetLockSource(lockCloneTarget)
	// The stub tmux of the test: tmux.TmuxBin of the test process does not
	// pass to this process.
	tmux.TmuxBin = os.Getenv(cloneHoldTmuxEnv)
	err := git.CloneWorktree(os.Getenv(cloneHoldOriginEnv), target, func(msg string) {
		if strings.HasPrefix(msg, "cloning ") {
			_ = os.WriteFile(os.Getenv(cloneHoldReadyEnv), nil, 0o644)
			select {}
		}
	})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
	}
	return 1
}

// AC (#3388): when the process of a clone ends before the clone ends, the
// kernel releases the lock. A start and a clone in the target then work.
func TestCloneWorktree_ReleasesTheLockWhenTheCloneIsKilled_3388(t *testing.T) {
	code, origin := cloneLockFixture(t)
	noTmuxServer(t)
	target := filepath.Join(code, "killed")
	ready := filepath.Join(t.TempDir(), "ready")

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		cloneHoldEnv+"="+target,
		cloneHoldOriginEnv+"="+origin,
		cloneHoldReadyEnv+"="+ready,
		cloneHoldTmuxEnv+"="+tmux.TmuxBin,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the clone process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the clone process did not take the lock: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := holdStartLock(target); !errors.Is(err, ErrConvertInProgress) {
		t.Fatalf("holdStartLock while the clone process runs = %v, want ErrConvertInProgress", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the clone process: %v", err)
	}
	_ = cmd.Wait()
	release, err := holdStartLock(target)
	if err != nil {
		t.Fatalf("holdStartLock after the clone process ended: %v", err)
	}
	release()
	if err := git.CloneWorktree(origin, target, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree after the clone process ended: %v", err)
	}
}

// The lock names the dir by its inode. When another dir takes the place of
// the dir while a clone or a conversion waits for the lock, a start in the
// new dir takes another lock, so the wait ends with an error.
func TestLockExclusive_RefusesDirThatTookThePlaceOfTheDir_3388(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := holdStartLock(dir)
	if err != nil {
		t.Fatalf("holdStartLock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		lock, err := lockExclusive(dir, 10*time.Second)
		lock.Release()
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	release()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "took the place") {
			t.Fatalf("lockExclusive = %v, want an error for the new dir", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("lockExclusive did not end")
	}
}

// AC (#3388): a start in a dir that does not exist is refused, so each start
// in a dir that a clone can make holds the start lock. A worktree path in a
// bare+worktree layout takes no lock, as before.
func TestHoldStartLock_RefusesDirThatDoesNotExist_3388(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	noTmuxServer(t)
	missing := filepath.Join(t.TempDir(), "later")

	if _, err := holdStartLock(missing); !errors.Is(err, errStartDirMissing) || !strings.Contains(err.Error(), missing) {
		t.Errorf("holdStartLock(%s) = %v, want errStartDirMissing with the dir", missing, err)
	}
	const name = "prism-test-missing-dir"
	opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: name}
	if err := Create(name, missing, opts); !errors.Is(err, errStartDirMissing) {
		t.Errorf("Create(%s) = %v, want errStartDirMissing", missing, err)
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := holdStartLock(filepath.Join(root, "feat"))
	if err != nil {
		t.Fatalf("holdStartLock(a worktree path) = %v, want no lock", err)
	}
	release()
}

// AC (#3388): a clone refuses a target in a bare+worktree layout, because a
// start in such a dir takes no start lock. It removes the dir that it made.
func TestCloneWorktree_RefusesTargetInABareLayout_3388(t *testing.T) {
	_, origin := cloneLockFixture(t)
	noTmuxServer(t)
	outer := filepath.Join(t.TempDir(), "outer")
	if err := os.MkdirAll(filepath.Join(outer, ".bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outer, "main", "nested")
	writeProjectSpecificConfig(t, target)

	err := git.CloneWorktree(origin, target, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "bare+worktree layout of "+outer) {
		t.Fatalf("CloneWorktree = %v, want a refusal that names the dir and the layout", err)
	}
	if _, err := os.Lstat(filepath.Join(outer, "main")); !os.IsNotExist(err) {
		t.Errorf("the refused clone left %s/main, a dir that it made: %v", outer, err)
	}

	// The same target through a symlink outside the layout.
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err = git.CloneWorktree(origin, link, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "clone into "+link) || !strings.Contains(err.Error(), "bare+worktree layout of ") {
		t.Fatalf("CloneWorktree(%s) = %v, want a refusal that names the dir and the layout", link, err)
	}
	assertNoBare(t, target)
}
