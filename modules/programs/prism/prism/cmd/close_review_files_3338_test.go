package cmd

// An interactive `prism close` that keeps the worktree (closeSession)
// removes the review files of the session, as `prism close --yes` does: the
// monitor log and the stored fallback plan of each round, and the diff dir.
// It keeps the review files of another session (#3338, item 1).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// writeReviewRoundFiles registers round 1 of PR 3338 for parent and writes
// its monitor log, its stored plan, and a file in the diff dir of parent.
// It returns the paths of the three files.
func writeReviewRoundFiles(t *testing.T, dbFile, parent string) []string {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	groupID, err := d.RegisterGroupWithPR(parent, "3338", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR(%q): %v", parent, err)
	}
	diffDir, err := review.DiffDirForParent(parent)
	if err != nil {
		t.Fatalf("DiffDirForParent(%q): %v", parent, err)
	}
	paths := []string{
		review.MonitorLogPath(groupID, "3338", 1),
		review.MonitorOptsPath(groupID),
		filepath.Join(diffDir, "pr-3338-round-1.diff"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestCloseSession_RemovesReviewRoundFiles_3338(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withNoopTmux(t)
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")}, scriptedResponse{stdout: []byte("")})

	const parent = "prism-test@close-files"
	const other = "prism-test@close-files-other"
	dbFile := seedHookRows(t, parent, other)
	withChildStops(t, dbFile)
	own := writeReviewRoundFiles(t, dbFile, parent)
	kept := writeReviewRoundFiles(t, dbFile, other)

	if err := closeSession(parent); err != nil {
		t.Fatalf("closeSession: %v", err)
	}

	for _, p := range own {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after the close (err=%v)", p, err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s of another session was removed: %v", p, err)
		}
	}
}
