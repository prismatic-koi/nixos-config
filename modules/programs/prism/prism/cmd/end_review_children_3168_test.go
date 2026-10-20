package cmd

// Tests for #3168: closeSession, the restore skip, and switch_project stop
// the sidecar of each open review child before the db.SetEnded cascade
// ends the child row. A child whose sidecar does not stop keeps its row
// and its port.

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// withChildStops replaces stopReviewChildrenForEnd. The fake checks that
// each open child row is still open when its sidecar stops, and returns
// failed as the children whose sidecar did not stop.
func withChildStops(t *testing.T, dbFile string, failed ...string) *[]string {
	t.Helper()
	var parents []string
	prev := stopReviewChildrenForEnd
	stopReviewChildrenForEnd = func(_ *db.DB, parent string) []string {
		parents = append(parents, parent)
		d, err := db.Open(dbFile)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer d.Close()
		rows, err := d.AllStatusesWithPrefix(parent + "~review-")
		if err != nil {
			t.Fatalf("AllStatusesWithPrefix: %v", err)
		}
		for _, r := range rows {
			if r.EndedAt != nil {
				t.Errorf("child %q ended before its sidecar stopped", r.SessionName)
			}
		}
		return failed
	}
	t.Cleanup(func() { stopReviewChildrenForEnd = prev })
	return &parents
}

func assertChildEnds(t *testing.T, dbFile, parent, stopped, stuck string, parents []string) {
	t.Helper()
	if !slices.Equal(parents, []string{parent}) {
		t.Errorf("review children stopped for %v, want [%s]", parents, parent)
	}
	if readStatus(t, dbFile, parent).EndedAt == nil {
		t.Error("parent row open, want ended")
	}
	if readStatus(t, dbFile, stopped).EndedAt == nil {
		t.Error("child with a stopped sidecar open, want ended by the cascade")
	}
	st := readStatus(t, dbFile, stuck)
	if st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("child whose sidecar did not stop: ended_at=%v port=%v, want open with its port", st.EndedAt, st.HarnessPort)
	}
}

func TestSetEndedStoppingReviewChildren_3168(t *testing.T) {
	const parent = "myrepo@end-children"
	stuck := parent + "~review-1-review-goal"
	stopped := parent + "~review-1-review-code"
	dbFile := seedHookRows(t, parent, stuck, stopped)
	parents := withChildStops(t, dbFile, stuck)

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := setEndedStoppingReviewChildren(d, parent); err != nil {
		t.Fatalf("setEndedStoppingReviewChildren: %v", err)
	}
	d.Close()

	assertChildEnds(t, dbFile, parent, stopped, stuck, *parents)
}

func TestCloseSession_StopsReviewChildren_3168(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")}, scriptedResponse{stdout: []byte("")})

	const parent = "prism-test@close-children"
	stuck := parent + "~review-2-review-qa"
	stopped := parent + "~review-2-review-security"
	dbFile := seedHookRows(t, parent, stuck, stopped)
	parents := withChildStops(t, dbFile, stuck)

	// A child whose sidecar did not stop makes the close exit non-zero and
	// name that child (#3192).
	err := closeSession(parent)
	if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), stuck) {
		t.Fatalf("closeSession = %v, want an errSidecarNotStopped error that names %q", err, stuck)
	}

	assertChildEnds(t, dbFile, parent, stopped, stuck, *parents)
}

func TestRestoreSkip_StopsReviewChildren_3168(t *testing.T) {
	for _, tc := range []struct{ desc, worktree string }{
		{"no worktree", ""},
		{"missing worktree", filepath.Join(t.TempDir(), "gone")},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			const parent = "myrepo@restore-children"
			stuck := parent + "~review-3-review-context"
			stopped := parent + "~review-3-review-goal"
			dbFile := seedHookRows(t, parent, stuck, stopped)
			parents := withChildStops(t, dbFile, stuck)

			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			pending := false
			outcome, err := restoreProjectSession(d, db.Status{SessionName: parent, Worktree: tc.worktree}, &pending, 0)
			d.Close()
			if err != nil || outcome != restoreOutcomeSkipped {
				t.Fatalf("restoreProjectSession = %v, %v, want skipped, nil", outcome, err)
			}

			assertChildEnds(t, dbFile, parent, stopped, stuck, *parents)
		})
	}
}
