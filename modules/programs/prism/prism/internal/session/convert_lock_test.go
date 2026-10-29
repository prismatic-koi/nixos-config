package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AC (#3361): while a conversion holds the lock of a dir, Create refuses a
// session in that dir before it starts tmux. A symlink to the dir names the
// same lock. After the release, a start takes the lock again.
func TestConvertLock_CreateRefusesStartWhileHeld_3361(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	lock, err := LockForConvert(dir, time.Second)
	if err != nil {
		t.Fatalf("LockForConvert: %v", err)
	}
	for _, d := range []string{dir, link} {
		if _, err := holdStartLock(d); !errors.Is(err, ErrConvertInProgress) {
			t.Errorf("holdStartLock(%s) = %v, want ErrConvertInProgress", d, err)
		}
	}
	opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: "prism-test-convert-lock"}
	if err := Create("prism-test-convert-lock", dir, opts); !errors.Is(err, ErrConvertInProgress) {
		t.Errorf("Create = %v, want ErrConvertInProgress", err)
	}

	lock.Release()
	lock.Release()
	release, err := holdStartLock(dir)
	if err != nil {
		t.Fatalf("holdStartLock after the release: %v", err)
	}
	release()
}

// A conversion waits for a start that holds the shared lock, and stops
// waiting after its timeout.
func TestConvertLock_WaitsForRunningStart_3361(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	release, err := holdStartLock(dir)
	if err != nil {
		t.Fatalf("holdStartLock: %v", err)
	}

	if lock, err := LockForConvert(dir, 100*time.Millisecond); err == nil {
		lock.Release()
		t.Fatal("LockForConvert took the lock while a start held it")
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	lock, err := LockForConvert(dir, 5*time.Second)
	if err != nil {
		t.Fatalf("LockForConvert after the start ended: %v", err)
	}
	lock.Release()
}

// AC (#3361): a start in a root that holds a .bare is refused after the
// conversion releases the lock. A start that read its row before the
// conversion, for example a `prism restore`, reaches Create at that time.
func TestConvertLock_RefusesStartInBareRootAfterConversion_3361(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	lock, err := LockForConvert(root, time.Second)
	if err != nil {
		t.Fatalf("LockForConvert: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock.Release()

	if _, err := holdStartLock(root); !errors.Is(err, ErrStartInBareRoot) {
		t.Errorf("holdStartLock = %v, want ErrStartInBareRoot", err)
	}
	opts := Opts{Layout: LayoutFull, IsolationMode: "bwrap", HarnessName: "pi", SessionName: "prism-test-bare-root"}
	if err := Create("prism-test-bare-root", root, opts); !errors.Is(err, ErrStartInBareRoot) {
		t.Errorf("Create = %v, want ErrStartInBareRoot", err)
	}
	if lock, err := LockForConvert(root, 0); err != nil {
		t.Errorf("the refused start kept the lock: %v", err)
	} else {
		lock.Release()
	}
}

// A worktree in a bare+worktree layout gets no start lock, and the .bare
// that a conversion makes in a root does not take the root out of the lock.
func TestConvertLock_ScopeOfTheStartLock_3361(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	worktree := filepath.Join(root, "main")
	for _, d := range []string{filepath.Join(root, ".bare"), worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rootLock, err := LockForConvert(root, time.Second)
	if err != nil {
		t.Fatalf("LockForConvert(root): %v", err)
	}
	defer rootLock.Release()
	if _, err := holdStartLock(root); !errors.Is(err, ErrConvertInProgress) {
		t.Errorf("holdStartLock(root with a .bare) = %v, want ErrConvertInProgress", err)
	}

	wtLock, err := LockForConvert(worktree, time.Second)
	if err != nil {
		t.Fatalf("LockForConvert(worktree): %v", err)
	}
	defer wtLock.Release()
	release, err := holdStartLock(worktree)
	if err != nil {
		t.Errorf("holdStartLock(worktree) = %v, want no lock for a worktree", err)
	} else {
		release()
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "prism", convertLockDirName))
	if err != nil {
		t.Fatalf("read the lock dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("lock files = %d, want 2: one for each LockForConvert", len(entries))
	}
}
