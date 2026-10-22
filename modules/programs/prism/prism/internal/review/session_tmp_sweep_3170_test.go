package review_test

// Tests for #3170: the ended-session sweeps also remove each per-session
// TMPDIR that no live session owns. Every caller of the #3160 sidecar sweep
// calls review.SweepEndedSessions, or ReapGroupAfterGrace for the monitor
// release, so these two entry points cover each call site.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// tmpDirFixture is one live TMPDIR and one stale TMPDIR under a test root.
type tmpDirFixture struct {
	live, stale string
}

// seedTmpDirs gives the live row name the instance ID live-<name>, and
// creates its TMPDIR and the TMPDIR of an instance ID that no row has.
func seedTmpDirs(t *testing.T, d *db.DB, name string) tmpDirFixture {
	t.Helper()
	t.Cleanup(container.SetSessionTmpRootForTest(t.TempDir()))
	if err := d.UpsertStatus(name, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(name, "live-"+name); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	var fx tmpDirFixture
	for _, p := range []struct {
		id  string
		dst *string
	}{{"live-" + name, &fx.live}, {"ended-" + name, &fx.stale}} {
		dir, err := container.SessionTmpDirPath(p.id)
		if err != nil {
			t.Fatalf("SessionTmpDirPath: %v", err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		*p.dst = dir
	}
	return fx
}

func (fx tmpDirFixture) check(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(fx.live); err != nil {
		t.Errorf("the sweep removed %s, the TMPDIR of a live session: %v", fx.live, err)
	}
	if _, err := os.Lstat(fx.stale); err == nil {
		t.Errorf("the sweep left %s, which no live session owns", fx.stale)
	}
}

// The pre-spawn sweep of `prism review`, `prism cleanup` and `prism close`,
// and the start of every sidecar call review.SweepEndedSessions.
func TestSweepEndedSessions_RemovesStaleSessionTmpDir_3170(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	d := openTestDB(t)
	fx := seedTmpDirs(t, d, "nixos-config@tmp-sweep")

	if _, err := review.SweepEndedSessions(d, time.Now()); err != nil {
		t.Fatalf("SweepEndedSessions: %v", err)
	}
	fx.check(t)
}

// The monitor release runs the sweep through ReapGroupAfterGrace.
func TestRelease_RemovesStaleSessionTmpDir_3170(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	probe := &reapProbe{}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(probe.killTmux, nil, nil))
	d := openTestDB(t)
	groupID, _ := seedRound(t, d, "nixos-config@tmp-release", 1, []string{"finished"}, true)
	fx := seedTmpDirs(t, d, "nixos-config@tmp-release")

	clock := time.Now()
	t.Cleanup(review.ReapClockForTest(
		func(d time.Duration) { clock = clock.Add(d) },
		func() time.Time { return clock },
	))
	review.ReapGroupAfterGrace(d, groupID, 0)
	fx.check(t)
}

// The TMPDIR sweep runs wherever the #3160 sidecar sweep runs only while
// review.sweepEndedSessions is the one production caller of
// session.SweepEndedSidecars. A new call site of the sidecar sweep must go
// through review.SweepEndedSessions, or this test fails.
func TestSidecarSweep_OnlyCallerIsSweepEndedSessions_3170(t *testing.T) {
	moduleRoot := filepath.Join("..", "..")
	const call = "SweepEndedSidecars("
	var callers []string
	err := filepath.WalkDir(moduleRoot, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			if strings.Contains(line, call) {
				callers = append(callers, filepath.ToSlash(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}
	want := filepath.ToSlash(filepath.Join(moduleRoot, "internal", "review", "reap.go"))
	if len(callers) != 1 || callers[0] != want {
		t.Errorf("production calls of %s: %v, want one call in %s. Call review.SweepEndedSessions, so that the TMPDIR sweep runs too (#3170).", call, callers, want)
	}
}

func TestSweepEndedSessions_KeepsTmpDirsWhenLiveSetUnreadable_3170(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	d := openTestDB(t)
	fx := seedTmpDirs(t, d, "nixos-config@tmp-closed-db")

	d.Close()
	if _, err := review.SweepEndedSessions(d, time.Now()); err == nil {
		t.Error("SweepEndedSessions = nil error, want the live-set read error")
	}
	for _, dir := range []string{fx.live, fx.stale} {
		if _, err := os.Lstat(dir); err != nil {
			t.Errorf("the sweep removed %s without a live set: %v", dir, err)
		}
	}
}
