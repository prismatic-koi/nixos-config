package review_test

// Tests for #3392: on Darwin, a process that a review agent detaches
// outlives the tmux kill, because sandbox-exec has no PID namespace. Each
// path that ends a review agent ends the processes in its sandbox after the
// tmux kill and before the row ends. When a process still runs, the path
// keeps the row and the port, and its message names the process and the
// session. The tests replace the sandbox stop. internal/session and
// internal/integration test the stop itself.

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// sandboxRecorder records the sidecar stops, the tmux kills, and the
// sandbox stops of the review paths, in order. At each sandbox stop it
// checks that the tmux session of the agent was killed first, when
// needTmuxKill is set, and that the row of the agent is still open, when
// needOpenRow is set. The sandbox stop fails with process 4243 for the
// names in fail.
type sandboxRecorder struct {
	t            *testing.T
	d            *db.DB
	fail         map[string]bool
	needTmuxKill bool
	needOpenRow  bool

	mu     sync.Mutex
	events []string
}

func (r *sandboxRecorder) record(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *sandboxRecorder) killTmux(name string) { r.record("tmux:" + name) }

func (r *sandboxRecorder) stopSidecar(name string) bool {
	r.record("sidecar:" + name)
	return true
}

func (r *sandboxRecorder) stopSandbox(name string) error {
	r.mu.Lock()
	killed := slices.Contains(r.events, "tmux:"+name)
	r.events = append(r.events, "sandbox:"+name)
	failing := r.fail[name]
	r.mu.Unlock()
	if r.needTmuxKill && !killed {
		r.t.Errorf("sandbox stop of %q before its tmux kill", name)
	}
	if r.needOpenRow {
		if st, err := r.d.CurrentStatus(name); err != nil || st == nil || st.EndedAt != nil || st.HarnessPort == nil {
			r.t.Errorf("row of %q ended, or its port released, before the sandbox stop (status=%+v, err=%v)", name, st, err)
		}
	}
	if failing {
		return &session.SandboxProcessError{Session: name, PIDs: []int{4243}}
	}
	return nil
}

func (r *sandboxRecorder) sandboxStops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if name, ok := strings.CutPrefix(e, "sandbox:"); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// installSandboxRecorder replaces each process-side effect of the review
// paths with the recorder.
func installSandboxRecorder(t *testing.T, d *db.DB, fail ...string) *sandboxRecorder {
	t.Helper()
	r := &sandboxRecorder{t: t, d: d, fail: map[string]bool{}, needTmuxKill: true, needOpenRow: true}
	for _, f := range fail {
		r.fail[f] = true
	}
	t.Cleanup(review.ReapSideEffectsWithStopForTest(r.killTmux, r.stopSidecar, nil))
	t.Cleanup(review.SetKillTmuxSessionForTest(func(name string) error { r.killTmux(name); return nil }))
	t.Cleanup(review.SandboxStopForTest(r.stopSandbox))
	return r
}

// namesProcess reports whether text names process 4243 and session.
func namesProcess(text, session string) bool {
	return strings.Contains(text, "4243") && strings.Contains(text, strconv.Quote(session))
}

func assertEnded(t *testing.T, d *db.DB, name string) {
	t.Helper()
	if st := statusOf(t, d, name); st.EndedAt == nil || st.HarnessPort != nil {
		t.Errorf("%s: ended_at=%v port=%v, want ended with no port", name, st.EndedAt, st.HarnessPort)
	}
}

func assertKept(t *testing.T, d *db.DB, name string) {
	t.Helper()
	if st := statusOf(t, d, name); st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("%s: ended_at=%v port=%v, want an open row with its port", name, st.EndedAt, st.HarnessPort)
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// AC: the automatic release ends the processes in the sandbox after the
// tmux kill and before the row ends. When a process still runs, the
// release keeps the row and the port, and the next sweep tries again.
func TestReap_StopsSandboxProcessesAfterTmuxKill_3392(t *testing.T) {
	d := openTestDB(t)
	_, sessions := seedRound(t, d, "nixos-config@sandbox-reap", 1, []string{"finished", "finished"}, true)
	stuck := sessions[1]
	rec := installSandboxRecorder(t, d, stuck)

	var res review.ReapResult
	warnings := captureWarnings(t, func() {
		var err error
		if res, err = review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
			t.Fatalf("ReapDeliveredReviewAgents: %v", err)
		}
	})

	if got, want := rec.sandboxStops(), sortedCopy(sessions); !slices.Equal(got, want) {
		t.Errorf("sandbox stops = %v, want %v", got, want)
	}
	if !slices.Equal(res.Reaped, sessions[:1]) || !slices.Equal(res.Failed, []string{stuck}) {
		t.Errorf("result = %+v, want Reaped=%v and Failed=[%s]", res, sessions[:1], stuck)
	}
	assertEnded(t, d, sessions[0])
	assertKept(t, d, stuck)
	if !namesProcess(warnings, stuck) {
		t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", stuck, warnings)
	}

	// The process is gone at the next sweep, which ends the row.
	rec.mu.Lock()
	rec.fail = map[string]bool{}
	rec.mu.Unlock()
	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("second ReapDeliveredReviewAgents: %v", err)
	}
	if !slices.Equal(res.Reaped, []string{stuck}) {
		t.Errorf("second sweep reaped %v, want [%s]", res.Reaped, stuck)
	}
	assertEnded(t, d, stuck)
}

// The release of a member whose row another path ended, and whose tmux
// session still runs, ends the processes in its sandbox after the tmux
// kill. The row is already ended, so a process that still runs gets a
// warning that names it.
func TestRelease_EndedRowWithLiveTmuxStopsSandboxProcesses_3392(t *testing.T) {
	for _, stillRuns := range []bool{false, true} {
		name := "no process runs"
		if stillRuns {
			name = "a process still runs"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			d := openTestDB(t)
			groupID, sessions := seedRound(t, d, "nixos-config@sandbox-release-"+strconv.FormatBool(stillRuns), 1, []string{"active"}, false)
			member := sessions[0]
			stubs := stubsFor(t, sessions)
			if err := d.SetEnded(member); err != nil {
				t.Fatalf("SetEnded: %v", err)
			}
			if err := d.SetGroupDeliveredAt(groupID); err != nil {
				t.Fatalf("SetGroupDeliveredAt: %v", err)
			}
			withTmuxPresence(t, func(string) session.TmuxPresence { return session.TmuxPresent })
			var fail []string
			if stillRuns {
				fail = []string{member}
			}
			rec := installSandboxRecorder(t, d, fail...)
			rec.needOpenRow = false

			warnings := captureWarnings(t, func() {
				if _, err := review.SweepEndedSessions(d, afterGrace()); err != nil {
					t.Fatalf("SweepEndedSessions: %v", err)
				}
			})

			if !stubs[0].exited(3 * time.Second) {
				t.Errorf("sidecar of %q still runs after the release", member)
			}
			if got := rec.sandboxStops(); !slices.Equal(got, []string{member}) {
				t.Errorf("sandbox stops = %v, want [%s]", got, member)
			}
			if stillRuns && !namesProcess(warnings, member) {
				t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", member, warnings)
			}
		})
	}
}

// AC: the cleanup of the review children of a parent ends the processes in
// the sandbox of each child after its tmux kill and before its row ends. A
// child where a process still runs keeps its row and its port, also after
// the db.SetEnded cascade of the parent, and the result names it with the
// error of its stop.
func TestParentCleanup_StopsSandboxProcessesAfterTmuxKill_3392(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@sandbox-parent-cleanup"
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(parent): %v", err)
	}
	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "active"}, true)
	stuck := sessions[1]
	rec := installSandboxRecorder(t, d, stuck)

	var kept review.KeptChildren
	warnings := captureWarnings(t, func() { kept = review.CleanupReviewSessionsForParent(d, parent, "") })

	if got, want := rec.sandboxStops(), sortedCopy(sessions); !slices.Equal(got, want) {
		t.Errorf("sandbox stops = %v, want %v", got, want)
	}
	if len(kept.SidecarRuns) != 0 || !slices.Equal(kept.SandboxRuns, []string{stuck}) || !slices.Equal(kept.Names(), []string{stuck}) {
		t.Errorf("kept = %+v, want SandboxRuns=[%s] only", kept, stuck)
	}
	var se *session.SandboxProcessError
	if !errors.As(kept.SandboxErr, &se) || !namesProcess(kept.SandboxErr.Error(), stuck) {
		t.Errorf("SandboxErr = %v, want the *SandboxProcessError that names the process 4243 and %q", kept.SandboxErr, stuck)
	}
	if !namesProcess(warnings, stuck) {
		t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", stuck, warnings)
	}
	causes, err := d.SessionEndCauses(sessions)
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	for _, s := range sessions {
		if got := causes[s].Cause; got != db.ReapCauseParentCleanup {
			t.Errorf("cause of %q = %q, want %q", s, got, db.ReapCauseParentCleanup)
		}
	}
	if st := statusOf(t, d, stuck); st.State != "error" {
		t.Errorf("state of %q = %q, want error", stuck, st.State)
	}

	if err := d.SetEndedExcept(parent, kept.Names()); err != nil {
		t.Fatalf("SetEndedExcept: %v", err)
	}
	assertEnded(t, d, sessions[0])
	assertKept(t, d, stuck)
}

// AC: StopReviewChildrenForEnd ends the processes in the sandbox of each
// open child after its tmux kill. A child where a process still runs is
// kept, so that the db.SetEnded cascade of the parent leaves its row open.
func TestStopReviewChildrenForEnd_StopsSandboxProcesses_3392(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@sandbox-children-end"
	if err := d.UpsertStatus(parent, "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(parent): %v", err)
	}
	_, sessions := seedRound(t, d, parent, 1, []string{"active", "finished"}, false)
	stuck := sessions[0]
	rec := installSandboxRecorder(t, d, stuck)

	var kept review.KeptChildren
	warnings := captureWarnings(t, func() { kept = review.StopReviewChildrenForEnd(d, parent) })

	if got, want := rec.sandboxStops(), sortedCopy(sessions); !slices.Equal(got, want) {
		t.Errorf("sandbox stops = %v, want %v", got, want)
	}
	if len(kept.SidecarRuns) != 0 || !slices.Equal(kept.SandboxRuns, []string{stuck}) {
		t.Errorf("kept = %+v, want SandboxRuns=[%s] only", kept, stuck)
	}
	if kept.SandboxErr == nil || !namesProcess(kept.SandboxErr.Error(), stuck) {
		t.Errorf("SandboxErr = %v, want an error that names the process 4243 and %q", kept.SandboxErr, stuck)
	}
	if !namesProcess(warnings, stuck) {
		t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", stuck, warnings)
	}

	if err := d.SetEndedExcept(parent, kept.Names()); err != nil {
		t.Fatalf("SetEndedExcept: %v", err)
	}
	if statusOf(t, d, sessions[1]).EndedAt == nil {
		t.Errorf("row of %q open after the cascade, want ended", sessions[1])
	}
	assertKept(t, d, stuck)
}

// The spawn-failure and readiness-gate cleanup of a review agent ends the
// processes in its sandbox after the tmux kill and before the row ends.
// When a process still runs, the row stays open in state "error", which is
// terminal, so the round can complete.
func TestCleanupFailedAgentSession_StopsSandboxProcessesAfterTmuxKill_3392(t *testing.T) {
	d := openGateTestDB(t)
	const ended = "test@sandbox-spawn-fail~review-1-review-code"
	const stuck = "test@sandbox-spawn-fail~review-1-review-goal"
	seedPortedAgentRow(t, d, ended)
	seedPortedAgentRow(t, d, stuck)
	rec := installSandboxRecorder(t, d, stuck)

	warnings := captureWarnings(t, func() {
		review.CleanupFailedAgentSessionForTest(d, ended, db.ReapCauseSpawnFailure, "boom")
		review.CleanupFailedAgentSessionForTest(d, stuck, db.ReapCauseSpawnFailure, "boom")
	})

	if got := rec.sandboxStops(); !slices.Equal(got, []string{ended, stuck}) {
		t.Errorf("sandbox stops = %v, want [%s %s]", got, ended, stuck)
	}
	assertFailedAgentRow(t, d, ended, true)
	assertFailedAgentRow(t, d, stuck, false)
	if !namesProcess(warnings, stuck) {
		t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", stuck, warnings)
	}
	causes, err := d.SessionEndCauses([]string{ended, stuck})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	for _, s := range []string{ended, stuck} {
		if got := causes[s].Cause; got != db.ReapCauseSpawnFailure {
			t.Errorf("cause of %q = %q, want %q", s, got, db.ReapCauseSpawnFailure)
		}
	}
}

// The force-terminate of a stuck member ends the processes in its sandbox
// before the row ends. It kills no tmux session: the release does that.
// When a process still runs, the row stays open in state "error", so the
// round can complete and the release tries again.
func TestForceTerminateStuckMembers_StopsSandboxProcessesBeforeRowEnds_3392(t *testing.T) {
	d := openTestDB(t)
	_, sessions := seedRound(t, d, "prism-test@sandbox-force-terminate", 1, []string{"active", "active"}, false)
	stuck := sessions[1]
	rec := installSandboxRecorder(t, d, stuck)
	rec.needTmuxKill = false

	var spared int
	warnings := captureWarnings(t, func() {
		spared = review.ForceTerminateStuckMembersForTest(d, sessions, 10*time.Minute)
	})

	if spared != 0 {
		t.Errorf("spared = %d, want 0", spared)
	}
	if got, want := rec.sandboxStops(), sortedCopy(sessions); !slices.Equal(got, want) {
		t.Errorf("sandbox stops = %v, want %v", got, want)
	}
	rec.mu.Lock()
	for _, e := range rec.events {
		if strings.HasPrefix(e, "tmux:") || strings.HasPrefix(e, "sidecar:") {
			t.Errorf("force-terminate ran %s, want only the sandbox stop", e)
		}
	}
	rec.mu.Unlock()
	for _, s := range sessions {
		if st := statusOf(t, d, s); st.State != "error" {
			t.Errorf("state of %q = %q, want error", s, st.State)
		}
	}
	if st := statusOf(t, d, sessions[0]); st.EndedAt == nil {
		t.Errorf("row of %q open, want ended", sessions[0])
	}
	assertKept(t, d, stuck)
	if !strings.Contains(warnings, "4243") || !strings.Contains(warnings, stuck) {
		t.Errorf("warnings do not name the process 4243 and the session %q:\n%s", stuck, warnings)
	}
}
