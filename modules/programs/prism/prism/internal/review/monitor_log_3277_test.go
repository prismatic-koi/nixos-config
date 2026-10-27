package review_test

// monitor_log_3277_test.go — issue #3277. The monitor log of a round has a
// path of its own group, ReadMonitorLogs finds it from the session_groups
// rows of the parent, and parent cleanup removes the logs of the parent and
// no other log.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

func writeMonitorLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedRound registers PR 2649 for every parent, so two parents with the same
// round number have the same PR number and the same round number.
const seedRoundPR = "2649"

func TestMonitorLogPath_KeyedByGroup_3277(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := review.MonitorLogPath("group-a", "42", 1)
	b := review.MonitorLogPath("group-b", "42", 1)
	if a == b {
		t.Fatalf("two groups of PR 42 round 1 share the log path %s", a)
	}
	if filepath.Dir(a) != review.MonitorStateDir() {
		t.Errorf("log path %s is not in %s", a, review.MonitorStateDir())
	}
	// Characters that are not a letter, a digit, or "-" are removed, so a
	// value from a row cannot move the path out of the directory.
	for _, p := range []string{
		review.MonitorLogPath("../../etc", "../1", 1),
		review.MonitorLogPath("a/b", "1/2", 1),
	} {
		if filepath.Dir(p) != review.MonitorStateDir() || strings.Contains(filepath.Base(p), "..") {
			t.Errorf("path %s escapes %s", p, review.MonitorStateDir())
		}
	}
}

func TestReadMonitorLogs_OwnRoundOnly_3277(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	mine := "nixos-config@log-mine"
	other := "nixos-config@log-other"
	g1, _ := seedRound(t, d, mine, 1, []string{"finished"}, true)
	g2, _ := seedRound(t, d, mine, 2, []string{"finished"}, true)
	og, _ := seedRound(t, d, other, 1, []string{"finished"}, true)
	writeMonitorLog(t, review.MonitorLogPath(g1, seedRoundPR, 1), "mine round 1\n")
	writeMonitorLog(t, review.MonitorLogPath(g2, seedRoundPR, 2), "mine round 2\n")
	writeMonitorLog(t, review.MonitorLogPath(og, seedRoundPR, 1), "other round 1\n")

	for round, want := range map[int]string{1: "mine round 1\n", 2: "mine round 2\n"} {
		logs, err := review.ReadMonitorLogs(d, mine, round)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(logs) != 1 || logs[0].Content != want {
			t.Errorf("round %d: logs = %+v, want one log %q", round, logs, want)
		}
	}
	logs, err := review.ReadMonitorLogs(d, other, 1)
	if err != nil || len(logs) != 1 || logs[0].Content != "other round 1\n" || logs[0].GroupID != og {
		t.Errorf("other parent round 1: logs = %+v, err = %v", logs, err)
	}
}

func TestReadMonitorLogs_NoLog_3277(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	parent := "nixos-config@log-none"
	seedRound(t, d, parent, 1, []string{"finished"}, true)

	_, err := review.ReadMonitorLogs(d, parent, 1)
	if !errors.Is(err, review.ErrNoMonitorLog) || !strings.Contains(err.Error(), "has no monitor log") {
		t.Errorf("round with no file: err = %v, want ErrNoMonitorLog naming the missing log", err)
	}
	_, err = review.ReadMonitorLogs(d, parent, 7)
	if !errors.Is(err, review.ErrNoMonitorLog) || !strings.Contains(err.Error(), "has no review round 7") {
		t.Errorf("round that does not exist: err = %v, want ErrNoMonitorLog naming the round", err)
	}
	for _, round := range []int{0, -1} {
		if _, err := review.ReadMonitorLogs(d, parent, round); err == nil {
			t.Errorf("round %d: no error, want a refusal", round)
		}
	}
}

// A legacy log has no group ID in its name, so the read cannot prove that
// it belongs to the parent. ReadMonitorLogs does not serve it.
func TestReadMonitorLogs_IgnoresLegacyPath_3277(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	parent := "nixos-config@log-legacy-read"
	seedRound(t, d, parent, 1, []string{"finished"}, true)
	writeMonitorLog(t, filepath.Join(review.MonitorStateDir(), "review-"+seedRoundPR+"-round-1.log"), "legacy\n")

	if _, err := review.ReadMonitorLogs(d, parent, 1); !errors.Is(err, review.ErrNoMonitorLog) {
		t.Errorf("legacy log: err = %v, want ErrNoMonitorLog", err)
	}
}

func TestParentCleanup_RemovesMonitorLogs_3277(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	t.Cleanup(review.ReapSideEffectsWithStopForTest(nil, func(string) bool { return true }, nil))
	parent := "nixos-config@log-cleanup"
	other := "nixos-config@log-cleanup-other"
	g1, _ := seedRound(t, d, parent, 1, []string{"finished"}, true)
	g2, _ := seedRound(t, d, parent, 2, []string{"finished"}, false)
	og, _ := seedRound(t, d, other, 1, []string{"finished"}, false)
	own := []string{review.MonitorLogPath(g1, seedRoundPR, 1), review.MonitorLogPath(g2, seedRoundPR, 2)}
	kept := review.MonitorLogPath(og, seedRoundPR, 1)
	for _, p := range append(own, kept) {
		writeMonitorLog(t, p, "log\n")
	}
	// Round 2 of the parent is the only round 2 of PR 2649, so its legacy
	// log can be removed. Round 1 is shared with the other parent, so its
	// legacy log stays.
	legacyOwn := filepath.Join(review.MonitorStateDir(), "review-"+seedRoundPR+"-round-2.log")
	legacyShared := filepath.Join(review.MonitorStateDir(), "review-"+seedRoundPR+"-round-1.log")
	writeMonitorLog(t, legacyOwn, "legacy 2\n")
	writeMonitorLog(t, legacyShared, "legacy 1\n")

	review.CleanupReviewSessionsForParent(d, parent, "")

	for _, p := range append(own, legacyOwn) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("log %s still exists after parent cleanup (err=%v)", p, err)
		}
	}
	for _, p := range []string{kept, legacyShared} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("log %s of another parent was removed: %v", p, err)
		}
	}
}
