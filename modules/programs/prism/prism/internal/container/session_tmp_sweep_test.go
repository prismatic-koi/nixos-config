package container

// session_tmp_sweep_test.go — SweepSessionTmpDirs (#3170). TestMain points
// sessionTmpRoot at a test-owned dir, so no test here lists /var/tmp.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// useSweepRoot points sessionTmpRoot at a new test-owned dir for the rest of
// t and returns it.
func useSweepRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(SetSessionTmpRootForTest(root))
	return root
}

// makeSessionTmpDir creates the per-session TMPDIR of instanceID with mode
// 0700 and a file in it, and returns its path.
func makeSessionTmpDir(t *testing.T, instanceID string) string {
	t.Helper()
	dir, err := SessionTmpDirPath(instanceID)
	if err != nil {
		t.Fatalf("SessionTmpDirPath(%q): %v", instanceID, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write in %s: %v", dir, err)
	}
	return dir
}

// liveSet returns an owners callback with one live owner per ID.
func liveSet(ids ...string) func() ([]SessionTmpDirOwner, error) {
	return func() ([]SessionTmpDirOwner, error) {
		owners := make([]SessionTmpDirOwner, 0, len(ids))
		for _, id := range ids {
			owners = append(owners, SessionTmpDirOwner{Key: id})
		}
		return owners, nil
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestSweepSessionTmpDirs_RemovesEveryDirWithoutALiveID(t *testing.T) {
	useSweepRoot(t)
	live := makeSessionTmpDir(t, "live-instance")
	staleA := makeSessionTmpDir(t, "ended-instance")
	staleB := makeSessionTmpDir(t, "instance-of-a-deleted-row")

	removed, err := SweepSessionTmpDirs(liveSet("live-instance", "no-dir-instance"))
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if !exists(live) {
		t.Errorf("the sweep removed %s, the TMPDIR of a live session", live)
	}
	for _, d := range []string{staleA, staleB} {
		if exists(d) {
			t.Errorf("the sweep left %s, which no live session owns", d)
		}
	}
	slices.Sort(removed)
	want := []string{staleA, staleB}
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
}

// An ended row keeps a dir that is newer than its ended_at: a respawned
// agent-only session runs with the ended_at of its old row. The dir of an
// ended row that is older than its ended_at goes.
func TestSweepSessionTmpDirs_EndedOwnerKeepsOnlyADirNewerThanEndedAt(t *testing.T) {
	useSweepRoot(t)
	stale := makeSessionTmpDir(t, "old-incarnation")
	time.Sleep(20 * time.Millisecond)
	endedAt := time.Now()
	time.Sleep(20 * time.Millisecond)
	respawned := makeSessionTmpDir(t, "respawned-incarnation")

	removed, err := SweepSessionTmpDirs(func() ([]SessionTmpDirOwner, error) {
		return []SessionTmpDirOwner{
			{Key: "old-incarnation", EndedAt: &endedAt},
			{Key: "respawned-incarnation", EndedAt: &endedAt},
		}, nil
	})
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if !exists(respawned) {
		t.Errorf("the sweep removed %s, which is newer than the ended_at of its row", respawned)
	}
	if exists(stale) || !slices.Equal(removed, []string{stale}) {
		t.Errorf("removed = %v, want only %s, which is older than the ended_at of its row", removed, stale)
	}
}

func TestSweepSessionTmpDirs_FailsClosedWhenLiveSetUnreadable(t *testing.T) {
	useSweepRoot(t)
	stale := makeSessionTmpDir(t, "ended-instance")

	readErr := errors.New("database is locked")
	removed, err := SweepSessionTmpDirs(func() ([]SessionTmpDirOwner, error) { return nil, readErr })
	if !errors.Is(err, readErr) {
		t.Errorf("SweepSessionTmpDirs error = %v, want the live-set read error", err)
	}
	if len(removed) != 0 || !exists(stale) {
		t.Errorf("the sweep removed %v without a live set", removed)
	}
}

// A dir that a session creates after the listing is not in the live set of
// this sweep, but the sweep must not remove it.
func TestSweepSessionTmpDirs_ListsDirsBeforeItReadsTheLiveSet(t *testing.T) {
	useSweepRoot(t)
	makeSessionTmpDir(t, "ended-instance")

	var spawned string
	removed, err := SweepSessionTmpDirs(func() ([]SessionTmpDirOwner, error) {
		spawned = makeSessionTmpDir(t, "spawned-during-the-sweep")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if !exists(spawned) {
		t.Errorf("the sweep removed %s, which did not exist when it listed the dirs", spawned)
	}
	if len(removed) != 1 {
		t.Errorf("removed = %v, want only the TMPDIR of ended-instance", removed)
	}
}

func TestSweepSessionTmpDirs_LeavesNamesOutsideThePattern(t *testing.T) {
	root := useSweepRoot(t)
	var dirs []string
	for _, name := range []string{
		"prism-ABCDEF012345",   // upper case
		"prism-0123456789a",    // 11 hex chars
		"prism-0123456789abc",  // 13 hex chars
		"prism-0123456789ag",   // not hex
		"prism-0123456789ab.x", // suffix
		"pst-0123456789ab",
		"other",
	} {
		d := filepath.Join(root, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		dirs = append(dirs, d)
	}
	file := filepath.Join(root, "prism-fedcba987654")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}

	removed, err := SweepSessionTmpDirs(liveSet())
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	for _, p := range append(dirs, file) {
		if !exists(p) {
			t.Errorf("the sweep removed %s", p)
		}
	}
}

// /var/tmp is writable by every local user. A symlink at a TMPDIR path can
// point anywhere, so the sweep must not remove it or its target.
func TestSweepSessionTmpDirs_LeavesASymlinkAndItsTarget(t *testing.T) {
	root := useSweepRoot(t)
	target := t.TempDir()
	victim := filepath.Join(target, "keep")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", victim, err)
	}
	link := filepath.Join(root, "prism-0123456789ab")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	removed, err := SweepSessionTmpDirs(liveSet())
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if len(removed) != 0 || !exists(link) || !exists(victim) {
		t.Errorf("removed = %v; link present = %v; target file present = %v, want no removal", removed, exists(link), exists(victim))
	}
}

func TestSweepSessionTmpDirs_LeavesADirOfAnotherUser(t *testing.T) {
	useSweepRoot(t)
	stale := makeSessionTmpDir(t, "instance-of-another-user")
	prev := geteuid
	geteuid = func() int { return prev() + 1 }
	t.Cleanup(func() { geteuid = prev })

	removed, err := SweepSessionTmpDirs(liveSet())
	if err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if len(removed) != 0 || !exists(stale) {
		t.Errorf("the sweep removed %v, a dir that the current user does not own", removed)
	}
}

func TestSweepSessionTmpDirs_DoesNotFollowASymlinkInsideTheDir(t *testing.T) {
	useSweepRoot(t)
	stale := makeSessionTmpDir(t, "ended-instance")
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", victim, err)
	}
	if err := os.Symlink(outside, filepath.Join(stale, "link-to-dir")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(stale, "link-to-file")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := SweepSessionTmpDirs(liveSet()); err != nil {
		t.Fatalf("SweepSessionTmpDirs: %v", err)
	}
	if exists(stale) {
		t.Errorf("the sweep left %s", stale)
	}
	if !exists(victim) {
		t.Errorf("the sweep followed a symlink in %s and removed %s", stale, victim)
	}
}

// A dir that the sweep cannot remove does not stop the removal of the other
// dirs, and the error names it.
func TestSweepSessionTmpDirs_ContinuesAfterAFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file in a read-only dir")
	}
	useSweepRoot(t)
	stuck := makeSessionTmpDir(t, "stuck-instance")
	locked := filepath.Join(stuck, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	stale := makeSessionTmpDir(t, "ended-instance")

	removed, err := SweepSessionTmpDirs(liveSet())
	if err == nil {
		t.Error("SweepSessionTmpDirs = nil error, want the removal error")
	}
	if exists(stale) || !slices.Contains(removed, stale) {
		t.Errorf("removed = %v: the failed removal stopped the sweep of %s", removed, stale)
	}
}

// The production root holds the TMPDIRs of the running sessions of the user.
// A test binary has a test database, so it must never sweep that root.
func TestSessionTmpSweepAllowed_ProductionRootRefusedInATestBinary(t *testing.T) {
	if sessionTmpSweepAllowed(productionSessionTmpRoot) {
		t.Fatalf("sessionTmpSweepAllowed(%q) = true in a test binary", productionSessionTmpRoot)
	}
	if !sessionTmpSweepAllowed(t.TempDir()) {
		t.Errorf("sessionTmpSweepAllowed(<test root>) = false, want true")
	}

	t.Cleanup(SetSessionTmpRootForTest(productionSessionTmpRoot))
	removed, err := SweepSessionTmpDirs(func() ([]SessionTmpDirOwner, error) {
		t.Error("the sweep read the live set for the production root")
		return nil, errors.New("refuse")
	})
	if err != nil || len(removed) != 0 {
		t.Errorf("SweepSessionTmpDirs on the production root = %v, %v, want no action", removed, err)
	}
}
