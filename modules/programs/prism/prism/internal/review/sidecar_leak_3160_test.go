package review_test

// Tests for #3160: review-agent sidecars that no path stopped.
//
// The leak: a path that ends a review-agent row without stopping its sidecar
// (parent cleanup, the tmux session-closed hook, the db.SetEnded cascade, the
// monitor's force-terminate) leaves the sidecar running, and the 15-minute
// release then skips the row because its ended_at is set. These tests run
// real sidecar stub processes, so "no sidecar process runs" is checked on
// the process, not on a fake.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// sidecarStubLongEnv makes TestMain act as a long-running sidecar stub.
const sidecarStubLongEnv = "PRISM_REVIEW_TEST_STUB_LONG"

// sidecarStub is one stub process with the argv of a sidecar.
type sidecarStub struct {
	pid  int
	done chan struct{}
}

// exited reports whether the stub exited within timeout.
func (s *sidecarStub) exited(timeout time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// startSidecarStubFor starts this test binary as `sidecar --session
// <sessionName>` and writes its PID to the sidecar PID file of the session.
func startSidecarStubFor(t *testing.T, sessionName string) *sidecarStub {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "sidecar", "--session", sessionName)
	cmd.Env = append(os.Environ(), sidecarStubLongEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	pidPath, err := session.SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	s := &sidecarStub{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return s
}

func assertNoPIDFile(t *testing.T, sessionName string) {
	t.Helper()
	pidPath, _ := session.SidecarPIDPath(sessionName)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file of %q still present (stat err %v)", sessionName, err)
	}
}

// withTmuxPresence makes the sweep see tmux sessions through presence.
func withTmuxPresence(t *testing.T, presence func(string) session.TmuxPresence) {
	t.Helper()
	t.Cleanup(session.SetSweepTmuxPresenceForTest(presence))
}

func allGone(string) session.TmuxPresence { return session.TmuxGone }

// realStop is the production sidecar stop with a short timeout.
func realStop(name string) bool { return session.EnsureSidecarStopped(name, 2*time.Second) }

// stubsFor starts one sidecar stub per session and pauses, so that a row
// ended after it has an ended_at later than the PID file mtime.
func stubsFor(t *testing.T, sessions []string) []*sidecarStub {
	t.Helper()
	stubs := make([]*sidecarStub, len(sessions))
	for i, s := range sessions {
		stubs[i] = startSidecarStubFor(t, s)
	}
	time.Sleep(150 * time.Millisecond)
	return stubs
}

// AC: when the parent worker is cleaned up before the release, no sidecar
// process of its review agents runs after the cleanup. This is the leak path
// on m4mac: CleanupReviewSessionsForParent ended the rows and killed the tmux
// sessions but never stopped a sidecar, and the /proc safety net in
// cmd/cleanup_orphan_sidecars.go does nothing on Darwin.
func TestParentCleanup_StopsReviewSidecars_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	parent := "nixos-config@leak-parent-cleanup"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished", "finished", "finished", "finished"}, true)
	stubs := stubsFor(t, sessions)

	failed := review.CleanupReviewSessionsForParent(d, parent)

	if len(failed) != 0 {
		t.Errorf("failed = %v, want none", failed)
	}
	for i, s := range sessions {
		if !stubs[i].exited(3 * time.Second) {
			t.Errorf("sidecar of %q still runs after the parent cleanup", s)
		}
		assertNoPIDFile(t, s)
		if statusOf(t, d, s).EndedAt == nil {
			t.Errorf("row of %q still open after the parent cleanup", s)
		}
	}
}

// AC: 15 minutes after the review-complete prompt, plus the release margin,
// no sidecar process and no PID file of the round remains, also when another
// path ended the rows first. Here the db.SetEnded cascade from the parent
// ends every row while the sidecars still run, which is what the
// session-closed hook of the parent did. The candidate query then returns
// nothing, and only the ended-session sweep reaches the sidecars.
func TestRelease_StopsSidecarsOfRowsAnotherPathEnded_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	probe := &reapProbe{}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(probe.killTmux, realStop, nil))

	d := openTestDB(t)
	parent := "nixos-config@leak-cascade"
	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished", "finished", "finished", "finished"}, true)
	stubs := stubsFor(t, sessions)
	if err := d.SetEnded(parent); err != nil {
		t.Fatalf("SetEnded(parent): %v", err)
	}

	clock := time.Now()
	t.Cleanup(review.ReapClockForTest(
		func(d time.Duration) { clock = clock.Add(d) },
		func() time.Time { return clock },
	))
	review.ReapGroupAfterGrace(d, groupID, 0)

	for i, s := range sessions {
		if !stubs[i].exited(3 * time.Second) {
			t.Errorf("sidecar of %q still runs after the release", s)
		}
		assertNoPIDFile(t, s)
	}
}

// A member that the monitor's force-terminate ended keeps its tmux session.
// The release stops its sidecar and then closes its tmux session.
func TestRelease_StopsForceTerminatedMemberWithLiveTmux_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	parent := "nixos-config@leak-force-terminate"
	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished", "active"}, false)
	stubs := stubsFor(t, sessions)
	stuck := sessions[1]
	if spared := review.ForceTerminateStuckMembersForTest(d, []string{stuck}, time.Minute); spared != 0 {
		t.Fatalf("force-terminate spared %d member(s), want 0", spared)
	}
	if err := d.SetGroupDeliveredAt(groupID); err != nil {
		t.Fatalf("SetGroupDeliveredAt: %v", err)
	}
	withTmuxPresence(t, func(string) session.TmuxPresence { return session.TmuxPresent })

	var mu sync.Mutex
	var order []string
	record := func(event string) {
		mu.Lock()
		order = append(order, event)
		mu.Unlock()
	}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(
		func(name string) { record("tmux:" + name) },
		func(name string) bool { record("stop:" + name); return realStop(name) },
		nil,
	))

	if _, err := review.ReapDeliveredReviewAgents(d, groupID, afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if _, err := review.SweepEndedSidecars(d, afterGrace()); err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}

	for i, s := range sessions {
		if !stubs[i].exited(3 * time.Second) {
			t.Errorf("sidecar of %q still runs after the release", s)
		}
		assertNoPIDFile(t, s)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(order, "tmux:"+stuck) {
		t.Errorf("tmux session of the force-terminated member not closed; events %v", order)
	}
}

// AC: when the monitor process stops before the release, the next
// `prism review`, `prism cleanup`, or sidecar start stops every sidecar of
// an ended session. All three call review.SweepEndedSidecars.
func TestSweep_AfterMonitorStopped_StopsSidecarsOfEndedSessions_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	d := openTestDB(t)
	parent := "nixos-config@leak-monitor-gone"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished", "error"}, true)
	stubs := stubsFor(t, sessions)
	// The parent's tmux session closed: the hook ended the parent row and,
	// through the cascade, every review row. No monitor runs a release.
	if err := d.SetEnded(parent); err != nil {
		t.Fatalf("SetEnded(parent): %v", err)
	}

	res, err := review.SweepEndedSidecars(d, time.Now())
	if err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	for i, s := range sessions {
		if !stubs[i].exited(3 * time.Second) {
			t.Errorf("sidecar of %q still runs after the sweep", s)
		}
		assertNoPIDFile(t, s)
	}
	if len(res.Stopped) != len(sessions) {
		t.Errorf("Stopped = %v, want all %d members", res.Stopped, len(sessions))
	}
}

// AC: a sweep does not stop the sidecar of a live session, including the
// review agents of a round that still runs, and of a delivered round that
// is still inside its grace period.
func TestSweep_SparesRunningAndUnreleasedRounds_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withTmuxPresence(t, allGone)
	d := openTestDB(t)
	_, running := seedRound(t, d, "nixos-config@leak-running", 1, []string{"active", "finished", "active"}, false)
	_, graced := seedRound(t, d, "nixos-config@leak-graced", 1, []string{"finished", "finished"}, true)
	all := append(append([]string{}, running...), graced...)
	stubs := stubsFor(t, all)

	if _, err := review.SweepEndedSidecars(d, time.Now()); err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	if _, err := review.ReapDeliveredReviewAgents(d, "", time.Now(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	for i, s := range all {
		if stubs[i].exited(200 * time.Millisecond) {
			t.Errorf("sidecar of live session %q was stopped", s)
		}
		pidPath, _ := session.SidecarPIDPath(s)
		if _, err := os.Stat(pidPath); err != nil {
			t.Errorf("PID file of live session %q removed: %v", s, err)
		}
	}
}

// AC: when a sidecar stop fails, the session row and its port stay
// reserved. The release also keeps the tmux session, so that the next
// sweep retries the whole teardown.
func TestReap_FailedStopKeepsRowPortAndTmux_3160(t *testing.T) {
	d := openTestDB(t)
	probe := &reapProbe{}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(probe.killTmux, func(string) bool { return false }, nil))
	parent := "nixos-config@leak-stop-fails"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if !slices.Equal(res.Failed, sessions) || len(res.Reaped) != 0 {
		t.Errorf("result = %+v, want Failed=%v and nothing reaped", res, sessions)
	}
	st := statusOf(t, d, sessions[0])
	if st.EndedAt != nil {
		t.Error("ended_at set after a failed sidecar stop")
	}
	if st.HarnessPort == nil {
		t.Error("harness port released after a failed sidecar stop")
	}
	if killed := probe.killed(); len(killed) != 0 {
		t.Errorf("tmux sessions killed after a failed sidecar stop: %v", killed)
	}
}

// The release stops the sidecar before it kills the tmux session. The kill
// fires the session-closed hook, which ends the row, so a stop after the
// kill could not keep the row reserved.
func TestReap_StopsSidecarBeforeTmuxKill_3160(t *testing.T) {
	d := openTestDB(t)
	var mu sync.Mutex
	var order []string
	t.Cleanup(review.ReapSideEffectsWithStopForTest(
		func(name string) { mu.Lock(); order = append(order, "tmux"); mu.Unlock() },
		func(name string) bool { mu.Lock(); order = append(order, "stop"); mu.Unlock(); return true },
		nil,
	))
	seedRound(t, d, "nixos-config@leak-order", 1, []string{"finished"}, true)

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if !slices.Equal(order, []string{"stop", "tmux"}) {
		t.Errorf("order = %v, want [stop tmux]", order)
	}
}

// Parent cleanup with a failed stop: the child is returned, its row and its
// port stay, and db.SetEndedExcept keeps the cascade off its row.
func TestParentCleanup_FailedStopKeepsChildReserved_3160(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@leak-cleanup-stop-fails"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)
	stuck := sessions[1]
	t.Cleanup(review.ReapSideEffectsWithStopForTest(nil, func(name string) bool { return name != stuck }, nil))
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(parent): %v", err)
	}

	failed := review.CleanupReviewSessionsForParent(d, parent)
	if !slices.Equal(failed, []string{stuck}) {
		t.Fatalf("failed = %v, want [%s]", failed, stuck)
	}
	if err := d.SetEndedExcept(parent, failed); err != nil {
		t.Fatalf("SetEndedExcept: %v", err)
	}

	if statusOf(t, d, sessions[0]).EndedAt == nil {
		t.Errorf("row of %q open, want ended", sessions[0])
	}
	st := statusOf(t, d, stuck)
	if st.EndedAt != nil {
		t.Errorf("row of %q ended after a failed sidecar stop", stuck)
	}
	if st.HarnessPort == nil {
		t.Errorf("port of %q released after a failed sidecar stop", stuck)
	}
	if statusOf(t, d, parent).EndedAt == nil {
		t.Error("parent row open, want ended")
	}
}

// The tmux session-closed hook of a parent ends the open review children
// through the cascade. StopReviewChildrenForEnd stops their sidecars first
// and closes their tmux sessions; an already-ended child is left alone.
func TestStopReviewChildrenForEnd_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	probe := &reapProbe{}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(probe.killTmux, realStop, nil))
	d := openTestDB(t)
	parent := "nixos-config@leak-hook"
	_, sessions := seedRound(t, d, parent, 1, []string{"active", "finished", "finished"}, false)
	if err := d.SetEnded(sessions[2]); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	stubs := stubsFor(t, sessions[:2])

	if failed := review.StopReviewChildrenForEnd(d, parent); len(failed) != 0 {
		t.Errorf("failed = %v, want none", failed)
	}
	for i, s := range sessions[:2] {
		if !stubs[i].exited(3 * time.Second) {
			t.Errorf("sidecar of %q still runs", s)
		}
	}
	killed := probe.killed()
	slices.Sort(killed)
	want := append([]string{}, sessions[:2]...)
	slices.Sort(want)
	if !slices.Equal(killed, want) {
		t.Errorf("tmux kills = %v, want %v", killed, want)
	}
}

// db.SetEndedExcept must not use the keep list to leave the row of the
// session itself open.
func TestSetEndedExcept_SelfAlwaysEnds_3160(t *testing.T) {
	d := openTestDB(t)
	const parent = "nixos-config@leak-self"
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetEndedExcept(parent, []string{parent}); err != nil {
		t.Fatalf("SetEndedExcept: %v", err)
	}
	if statusOf(t, d, parent).EndedAt == nil {
		t.Error("row of the session itself open, want ended")
	}
}
