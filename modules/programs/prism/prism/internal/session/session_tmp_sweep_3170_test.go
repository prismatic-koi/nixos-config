package session

import (
	"os"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// makeTmpDirFor creates the per-session TMPDIR for key under the test root.
func makeTmpDirFor(t *testing.T, key string) string {
	t.Helper()
	dir, err := container.SessionTmpDirPath(key)
	if err != nil {
		t.Fatalf("SessionTmpDirPath(%q): %v", key, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

func dirExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestSweepStaleSessionTmpDirs_KeepsLiveRowsAndRemovesTheRest(t *testing.T) {
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	d := openSweepTestDB(t)

	const live = "prism-test@tmp-live"
	seedRow(t, d, live)
	if err := d.SetInstanceID(live, "live-instance"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	liveDir := makeTmpDirFor(t, "live-instance")

	// A live row with no instance ID: the dispatcher keys its TMPDIR on the
	// container name.
	const noID = "prism-test@tmp-no-instance-id"
	seedRow(t, d, noID)
	noIDDir := makeTmpDirFor(t, container.NameForSession(noID))

	const ended = "prism-test@tmp-ended"
	seedRow(t, d, ended)
	if err := d.SetInstanceID(ended, "ended-instance"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	endedDir := makeTmpDirFor(t, "ended-instance")
	endRow(t, d, ended)

	orphanDir := makeTmpDirFor(t, "instance-with-no-row")

	if _, err := SweepStaleSessionTmpDirs(d); err != nil {
		t.Fatalf("SweepStaleSessionTmpDirs: %v", err)
	}
	for _, keep := range []string{liveDir, noIDDir} {
		if !dirExists(keep) {
			t.Errorf("the sweep removed %s, the TMPDIR of a live session", keep)
		}
	}
	for _, gone := range []string{endedDir, orphanDir} {
		if dirExists(gone) {
			t.Errorf("the sweep left %s, which no live session owns", gone)
		}
	}
}

// A respawned agent-only session (an investigator) runs with the ended_at of
// its old row: SessionSpawn sets a new instance ID, and no tmux-session-start
// seed clears ended_at. Its new TMPDIR is newer than that ended_at, so it
// stays. The TMPDIR of the old incarnation goes.
func TestSweepStaleSessionTmpDirs_KeepsTheDirOfARespawnedSession(t *testing.T) {
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	d := openSweepTestDB(t)

	const name = "prism-test@tmp-respawn~investigate-x"
	seedRow(t, d, name)
	if err := d.SetInstanceID(name, "old-instance"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	oldDir := makeTmpDirFor(t, "old-instance")
	endRow(t, d, name)
	time.Sleep(20 * time.Millisecond)

	if err := d.SetInstanceID(name, "respawned-instance"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	newDir := makeTmpDirFor(t, "respawned-instance")

	if _, err := SweepStaleSessionTmpDirs(d); err != nil {
		t.Fatalf("SweepStaleSessionTmpDirs: %v", err)
	}
	if !dirExists(newDir) {
		t.Errorf("the sweep removed %s, the TMPDIR of a running respawned session", newDir)
	}
	if dirExists(oldDir) {
		t.Errorf("the sweep left %s, the TMPDIR of the ended incarnation", oldDir)
	}
}

func TestSweepStaleSessionTmpDirs_FailsClosedWhenLiveSetUnreadable(t *testing.T) {
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	d := openSweepTestDB(t)
	stale := makeTmpDirFor(t, "ended-instance")

	d.Close()
	if _, err := SweepStaleSessionTmpDirs(d); err == nil {
		t.Error("SweepStaleSessionTmpDirs = nil error, want the live-set read error")
	}
	if _, err := SweepStaleSessionTmpDirs(nil); err == nil {
		t.Error("SweepStaleSessionTmpDirs(nil) = nil error, want an error")
	}
	if !dirExists(stale) {
		t.Errorf("the sweep removed %s without a live set", stale)
	}
}
