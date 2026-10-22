package cmd

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// AC (#3170): the cleanup paths run the TMPDIR sweep. It removes the
// TMPDIR that no live session owns and keeps the TMPDIR of a live session.
func TestSweepEndedSessionsAfterCleanup_RemovesStaleSessionTmpDir_3170(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Cleanup(prismSession.SetSweepTmuxPresenceForTest(func(string) prismSession.TmuxPresence {
		return prismSession.TmuxGone
	}))
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	const live = "myrepo@tmp-sweep-live"
	dbFile := seedHookRows(t, live)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.SetInstanceID(live, "live-instance"); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	d.Close()

	mk := func(id string) string {
		dir, err := container.SessionTmpDirPath(id)
		if err != nil {
			t.Fatalf("SessionTmpDirPath: %v", err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		return dir
	}
	liveDir := mk("live-instance")
	staleDir := mk("ended-instance")

	t.Run("no sweep inside a sandbox", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "unix:///nonexistent/hostapi.sock")
		sweepEndedSessionsAfterCleanup()
		if _, err := os.Lstat(staleDir); err != nil {
			t.Fatalf("the sweep ran with PRISM_HOST_API set: %v", err)
		}
	})

	sweepEndedSessionsAfterCleanup()
	if _, err := os.Lstat(liveDir); err != nil {
		t.Errorf("the cleanup sweep removed %s, the TMPDIR of a live session: %v", liveDir, err)
	}
	if _, err := os.Lstat(staleDir); err == nil {
		t.Errorf("the cleanup sweep left %s, which no live session owns", staleDir)
	}
}
