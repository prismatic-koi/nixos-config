package review_test

// Tests for #3168: the readiness-gate cleanup and the spawn-failure cleanup
// of a review agent stop its sidecar with the stop that reports a result,
// before the row ends. A failed stop keeps the row and the port.

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// stopRecorder is a sidecar stop that records each call, checks that the
// row is still open at the stop, and fails for the names in fail.
type stopRecorder struct {
	t    *testing.T
	d    *db.DB
	fail map[string]bool
	mu   sync.Mutex
	got  []string
}

func (s *stopRecorder) stop(name string) bool {
	s.mu.Lock()
	s.got = append(s.got, name)
	s.mu.Unlock()
	if st, err := s.d.CurrentStatus(name); err != nil || st == nil || st.EndedAt != nil {
		s.t.Errorf("row of %q was ended or absent at the sidecar stop (status=%v, err=%v)", name, st, err)
	}
	return !s.fail[name]
}

func (s *stopRecorder) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.got)
	slices.Sort(out)
	return out
}

func seedPortedAgentRow(t *testing.T, d *db.DB, name string) {
	t.Helper()
	seedAgentRow(t, d, name)
	if _, err := d.AllocatePort(name); err != nil {
		t.Fatalf("AllocatePort(%q): %v", name, err)
	}
}

func assertFailedAgentRow(t *testing.T, d *db.DB, name string, wantEnded bool) {
	t.Helper()
	st, err := d.CurrentStatus(name)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", name, st, err)
	}
	if st.State != "error" {
		t.Errorf("%s: state = %q, want error", name, st.State)
	}
	if wantEnded {
		if st.EndedAt == nil || st.HarnessPort != nil {
			t.Errorf("%s: ended_at=%v port=%v, want ended with no port", name, st.EndedAt, st.HarnessPort)
		}
		return
	}
	if st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("%s: ended_at=%v port=%v, want open with its port after a failed sidecar stop", name, st.EndedAt, st.HarnessPort)
	}
}

func TestCleanupFailedAgentSession_StopsSidecarFirst_3168(t *testing.T) {
	d := openGateTestDB(t)
	const stopped = "test@spawn-fail~review-1-review-code"
	const stuck = "test@spawn-fail~review-1-review-goal"
	seedPortedAgentRow(t, d, stopped)
	seedPortedAgentRow(t, d, stuck)
	rec := &stopRecorder{t: t, d: d, fail: map[string]bool{stuck: true}}
	defer review.ReapSideEffectsWithStopForTest(nil, rec.stop, nil)()

	review.CleanupFailedAgentSessionForTest(d, stopped, db.ReapCauseSpawnFailure, "boom")
	review.CleanupFailedAgentSessionForTest(d, stuck, db.ReapCauseSpawnFailure, "boom")

	if got, want := rec.calls(), []string{stopped, stuck}; !slices.Equal(got, want) {
		t.Errorf("sidecar stops = %v, want %v", got, want)
	}
	assertFailedAgentRow(t, d, stopped, true)
	assertFailedAgentRow(t, d, stuck, false)
}

func TestGateReviewAgents_TimeoutStopsSidecarFirst_3168(t *testing.T) {
	d := openGateTestDB(t)
	agents := review.Agents()[:2]
	const parent = "test@gate-stop"
	sessions := []string{parent + "~review-1-" + agents[0].Name, parent + "~review-1-" + agents[1].Name}
	for _, s := range sessions {
		seedPortedAgentRow(t, d, s)
	}
	rec := &stopRecorder{t: t, d: d, fail: map[string]bool{sessions[1]: true}}
	defer review.ReapSideEffectsWithStopForTest(nil, rec.stop, nil)()

	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))
	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes, 300*time.Millisecond, nil)

	got, want := rec.calls(), slices.Clone(sessions)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("sidecar stops = %v, want %v", got, want)
	}
	assertFailedAgentRow(t, d, sessions[0], true)
	assertFailedAgentRow(t, d, sessions[1], false)
}
