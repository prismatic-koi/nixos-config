package git

// Tests for #3388: CloneWorktree holds the lock of its target from before
// its checks to its end. internal/session gives the real lock. These tests
// pin the contract of CloneWorktree with a stand-in lock.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setCloneTargetLock makes lock the CloneTargetLock for the rest of t. A nil
// lock removes it.
func setCloneTargetLock(t *testing.T, lock CloneTargetLock) {
	t.Helper()
	prev := cloneTargetLockSource.Load()
	if lock == nil {
		cloneTargetLockSource.Store(nil)
	} else {
		cloneTargetLockSource.Store(&lock)
	}
	t.Cleanup(func() { cloneTargetLockSource.Store(prev) })
}

// AC (#3388): the clone takes the lock of the target before the bare clone,
// and releases it after the last step.
func TestCloneWorktree_HoldsTheTargetLockToTheEnd_3388(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	useBareRecord(t)
	target := filepath.Join(code, "held")
	var events []string
	setCloneTargetLock(t, func(dir string) (func(), error) {
		events = append(events, "lock "+dir)
		return func() { events = append(events, "release") }, nil
	})

	if err := CloneWorktree(origin, target, func(msg string) { events = append(events, msg) }); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if len(events) < 4 || events[0] != "lock "+target || !strings.HasPrefix(events[1], "cloning ") ||
		!strings.HasPrefix(events[len(events)-2], "done") || events[len(events)-1] != "release" {
		t.Fatalf("events = %q, want the lock first, the release last", events)
	}
}

// AC (#3388): a refused clone removes the dirs that it made only while it
// holds the lock. Without the lock, another clone can use the target. With
// no lock set, the clone refuses.
func TestCloneWorktree_RefusalOfTheTargetLock_3388(t *testing.T) {
	code, origin := cloneTargetFixture(t)
	useBareRecord(t)
	refused := errors.New("clone into the target: refused by the lock")

	t.Run("refused with the lock held", func(t *testing.T) {
		target := filepath.Join(code, "held-refused")
		goneAtRelease := false
		setCloneTargetLock(t, func(string) (func(), error) {
			return func() {
				_, err := os.Lstat(target)
				goneAtRelease = os.IsNotExist(err)
			}, refused
		})
		var steps []string
		if err := CloneWorktree(origin, target, func(msg string) { steps = append(steps, msg) }); !errors.Is(err, refused) {
			t.Fatalf("CloneWorktree = %v, want the refusal of the lock", err)
		}
		if !goneAtRelease {
			t.Errorf("%s, a dir that the clone made, was there at the release of the lock", target)
		}
		if len(steps) != 0 {
			t.Errorf("the refused clone started: %q", steps)
		}
	})

	t.Run("refused with no lock held", func(t *testing.T) {
		target := filepath.Join(code, "busy")
		setCloneTargetLock(t, func(string) (func(), error) { return nil, refused })
		if err := CloneWorktree(origin, target, func(string) {}); !errors.Is(err, refused) {
			t.Fatalf("CloneWorktree = %v, want the refusal of the lock", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("the clone removed %s with no lock held: %v", target, err)
		}
	})

	t.Run("no lock set", func(t *testing.T) {
		target := filepath.Join(code, "nolock")
		setCloneTargetLock(t, nil)
		err := CloneWorktree(origin, target, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "clone into "+target) || !strings.Contains(err.Error(), "no lock") {
			t.Fatalf("CloneWorktree = %v, want a refusal that names the dir", err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("the refused clone left %s: %v", target, err)
		}
	})
}
