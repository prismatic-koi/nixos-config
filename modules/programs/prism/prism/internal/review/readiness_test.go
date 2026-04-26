package review_test

// Tests for the per-agent readiness gate in the review fan-out (#1051 Piece A).
// These exercise GateReviewAgentsForTest — the test-only export of
// gateReviewAgents — without spinning up real tmux sessions or sidecars.
//
// The gate's contract:
//
//   - For each agent whose spawnErr[i] is nil, run session.WaitForReady in a
//     goroutine.
//   - On success: emit "<role> started" via OnProgress, set spawnTimes[i] to
//     "now".
//   - On timeout: emit "<role> failed to start: not ready within Xs", populate
//     spawnErr[i] with a *session.ReadinessTimeoutError (wrapped), and clean
//     up the half-alive session via KillSidecar / cleanupAgentSession /
//     tmux.KillSession.
//
// AC-7 (5-agent fan-out, 2 fail): TestGateReviewAgents_PartialFailure_2of5
// AC-8 (port-block reproducer):   TestGateReviewAgents_TimeoutSurfacedWithin30s
// AC-9 (no regressions, healthy): TestGateReviewAgents_AllReady_AllStarted

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

func openGateTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedAgentRow writes a minimal agent_status row for the named session — the
// same shape SpawnSession would write at spawn time. The gate needs the row
// to exist so CurrentStatus / QueryEvents return cleanly.
func seedAgentRow(t *testing.T, d *db.DB, sess string) {
	t.Helper()
	if err := d.UpsertStatusSeedRootAgentName(sess, "test", "/tmp", "idle", nil, nil, "review-x"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", sess, err)
	}
}

// signalReady inserts a state_change event for sess, simulating the sidecar
// receiving the first SSE event from opencode.
func signalReady(t *testing.T, d *db.DB, sess string) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          sess + "-ready",
		SessionName: sess,
		Repo:        "test",
		Worktree:    "/tmp",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
	}); err != nil {
		t.Fatalf("WriteEvent(%q): %v", sess, err)
	}
}

// progressCollector is a thread-safe collector for OnProgress lines.
type progressCollector struct {
	mu    sync.Mutex
	lines []string
}

func (p *progressCollector) callback(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, line)
}

func (p *progressCollector) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.lines))
	copy(out, p.lines)
	return out
}

// containsLine reports whether any line in lines contains substr.
func containsLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// ── AC-7: fan-out spawn where 2 of 5 agents fail their readiness check ───────

// TestGateReviewAgents_PartialFailure_2of5 simulates the headline #1051
// scenario: a 5-agent fan-out where 3 agents become ready (state_change
// event written) and 2 stay silent (timeout). Verifies that:
//
//   - The 3 successful agents are reported as "started".
//   - The 2 failures are reported with "failed to start: not ready within Xs".
//   - spawnErr[i] is populated for the 2 failures with a
//     *session.ReadinessTimeoutError (visible through errors.As).
//   - The 3 successful agents have spawnErr[i] == nil after the gate.
//
// AC-7 satisfied: subsequent code can build the partial-success summary
// from the spawnErr slice.
func TestGateReviewAgents_PartialFailure_2of5(t *testing.T) {
	d := openGateTestDB(t)

	agents := review.Agents() // 5 standard review agents
	const parent = "test@partial-failure"
	sessions := make([]string, len(agents))
	for i, ag := range agents {
		sessions[i] = parent + "~review-1-" + ag.Name
		seedAgentRow(t, d, sessions[i])
	}

	// 3 of 5 become ready: review-goal, review-code, review-context.
	// review-security and review-qa stay silent → timeout.
	signalReady(t, d, sessions[0]) // review-goal
	signalReady(t, d, sessions[1]) // review-code
	signalReady(t, d, sessions[4]) // review-context

	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))
	collector := &progressCollector{}

	// Use a short timeout so the test runs quickly. The gate runs the
	// state_change check as soon as it starts, so the 3 ready agents
	// resolve in the first poll and the test completes in ~1s.
	const timeout = 800 * time.Millisecond
	start := time.Now()
	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes, timeout, collector.callback)
	elapsed := time.Since(start)

	// AC-7: 3 successful agents → spawnErr[i] == nil.
	for _, i := range []int{0, 1, 4} {
		if spawnErr[i] != nil {
			t.Errorf("spawnErr[%d] (%s) = %v, want nil — agent should have become ready", i, agents[i].Name, spawnErr[i])
		}
		if spawnTimes[i].IsZero() {
			t.Errorf("spawnTimes[%d] (%s) is zero — gate should have set it on success", i, agents[i].Name)
		}
	}

	// AC-7: 2 failures → spawnErr[i] is *session.ReadinessTimeoutError.
	for _, i := range []int{2, 3} {
		if spawnErr[i] == nil {
			t.Errorf("spawnErr[%d] (%s) = nil, want *ReadinessTimeoutError", i, agents[i].Name)
			continue
		}
		if !session.IsReadinessTimeout(spawnErr[i]) {
			t.Errorf("spawnErr[%d] (%s) = %v, want *ReadinessTimeoutError", i, agents[i].Name, spawnErr[i])
		}
	}

	// AC-7: progress lines.
	lines := collector.snapshot()
	if !containsLine(lines, "Review-Goal started") {
		t.Errorf("missing 'Review-Goal started' in progress lines: %v", lines)
	}
	if !containsLine(lines, "Review-Code started") {
		t.Errorf("missing 'Review-Code started' in progress lines: %v", lines)
	}
	if !containsLine(lines, "Review-Context started") {
		t.Errorf("missing 'Review-Context started' in progress lines: %v", lines)
	}
	if !containsLine(lines, "Review-Security failed to start: not ready within") {
		t.Errorf("missing 'Review-Security failed to start' in progress lines: %v", lines)
	}
	if !containsLine(lines, "Review-Qa failed to start: not ready within") {
		t.Errorf("missing 'Review-Qa failed to start' in progress lines: %v", lines)
	}

	// AC-8 spirit: total wall time should be bounded by the timeout, not
	// 5x timeout. The gate runs in parallel.
	if elapsed > timeout+1*time.Second {
		t.Errorf("gate took %v, want close to %v (per-agent gates must run concurrently)", elapsed, timeout)
	}
}

// ── AC-8: surface the failure within the timeout window ──────────────────────

// TestGateReviewAgents_TimeoutSurfacedWithinWindow is the AC-8 reproducer:
// when 2 of 5 agents are blocked from emitting a readiness signal (here
// simulated by simply not writing one), the spawn loop must surface the
// failure within the configured window — NOT after the 20-minute monitor
// timeout. This guards against regressions where the gate accidentally
// runs sequentially or doesn't fire at all.
//
// We use a 1-second timeout here; on the production path the timeout is 30s
// per AC-1 and the same parallelism guarantee holds.
func TestGateReviewAgents_TimeoutSurfacedWithinWindow(t *testing.T) {
	d := openGateTestDB(t)

	agents := review.Agents() // 5 standard review agents
	const parent = "test@ac-8-window"
	sessions := make([]string, len(agents))
	for i, ag := range agents {
		sessions[i] = parent + "~review-1-" + ag.Name
		seedAgentRow(t, d, sessions[i])
	}

	// Simulate the headline scenario: 3 succeed, 2 fail (review-goal and
	// review-qa — same indices as the original #1051 incident).
	signalReady(t, d, sessions[1]) // review-code
	signalReady(t, d, sessions[2]) // review-security
	signalReady(t, d, sessions[4]) // review-context

	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))
	collector := &progressCollector{}

	const timeout = 1 * time.Second
	start := time.Now()
	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes, timeout, collector.callback)
	elapsed := time.Since(start)

	// AC-8: the gate must have completed within roughly the timeout window
	// — categorically not in 20 minutes (which would mean the gate ran
	// sequentially or wasn't called at all).
	if elapsed > timeout+1*time.Second {
		t.Errorf("gate took %v with %v timeout — failure must be surfaced within the window, not after the monitor timeout", elapsed, timeout)
	}

	// AC-8: the 2 failed agents must have a timeout error.
	if !session.IsReadinessTimeout(spawnErr[0]) {
		t.Errorf("review-goal: spawnErr = %v, want *ReadinessTimeoutError", spawnErr[0])
	}
	if !session.IsReadinessTimeout(spawnErr[3]) {
		t.Errorf("review-qa: spawnErr = %v, want *ReadinessTimeoutError", spawnErr[3])
	}
}

// ── AC-9: no regressions when all agents come up healthily ───────────────────

// TestGateReviewAgents_AllReady_AllStarted verifies the happy path: when all
// 5 agents signal readiness within the gate window, all 5 emit the "started"
// progress line and none have a populated spawnErr. The wall time should be
// ~one poll interval (250ms), well under the AC-9 ceiling of "a few seconds".
func TestGateReviewAgents_AllReady_AllStarted(t *testing.T) {
	d := openGateTestDB(t)

	agents := review.Agents()
	const parent = "test@all-healthy"
	sessions := make([]string, len(agents))
	for i, ag := range agents {
		sessions[i] = parent + "~review-1-" + ag.Name
		seedAgentRow(t, d, sessions[i])
		signalReady(t, d, sessions[i])
	}

	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))
	collector := &progressCollector{}

	const timeout = 5 * time.Second
	start := time.Now()
	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes, timeout, collector.callback)
	elapsed := time.Since(start)

	// AC-9: all 5 must report "started" and have no spawnErr.
	lines := collector.snapshot()
	for i, ag := range agents {
		if spawnErr[i] != nil {
			t.Errorf("spawnErr[%d] (%s) = %v, want nil", i, ag.Name, spawnErr[i])
		}
		if spawnTimes[i].IsZero() {
			t.Errorf("spawnTimes[%d] (%s) is zero, want set", i, ag.Name)
		}
		if !containsLine(lines, review.FormatAgentDisplayName(ag.Name)+" started") {
			t.Errorf("missing %q in progress lines: %v", review.FormatAgentDisplayName(ag.Name)+" started", lines)
		}
	}

	// AC-9: "the extra readiness check should add at most ~2 s on a healthy
	// machine". We're well below that — typically <500ms in the test.
	if elapsed > 2*time.Second {
		t.Errorf("gate took %v on healthy path — AC-9 ceiling is ~2s", elapsed)
	}
}

// TestGateReviewAgents_SkipsAlreadyFailedAgents verifies that pre-existing
// spawn failures (config errors, SpawnSession errors) are not re-processed
// by the gate. The gate must skip agents whose spawnErr[i] is already
// non-nil, so the spawn loop's earlier "failed to start: <reason>" line is
// not duplicated by a second readiness-timeout line.
func TestGateReviewAgents_SkipsAlreadyFailedAgents(t *testing.T) {
	d := openGateTestDB(t)

	agents := review.Agents()
	const parent = "test@skip-failed"
	sessions := make([]string, len(agents))
	for i, ag := range agents {
		sessions[i] = parent + "~review-1-" + ag.Name
		seedAgentRow(t, d, sessions[i])
	}

	// Mark agent 0 as already failed (e.g. config error from spawn loop).
	preExistingErr := &session.ReadinessTimeoutError{SessionName: "x", Timeout: time.Second}
	spawnErr := make([]error, len(agents))
	spawnErr[0] = preExistingErr // sentinel; actual cause irrelevant here.
	// All other agents become ready so the gate completes quickly.
	for i := 1; i < len(agents); i++ {
		signalReady(t, d, sessions[i])
	}

	spawnTimes := make([]time.Time, len(agents))
	collector := &progressCollector{}

	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes, 1*time.Second, collector.callback)

	// Agent 0's spawnErr must be left untouched.
	if spawnErr[0] != preExistingErr {
		t.Errorf("spawnErr[0] = %v, want preExistingErr (gate must not overwrite pre-existing failures)", spawnErr[0])
	}
	// No "started" line for agent 0 (it was pre-failed).
	lines := collector.snapshot()
	if containsLine(lines, review.FormatAgentDisplayName(agents[0].Name)+" started") {
		t.Errorf("unexpected 'started' line for pre-failed agent: %v", lines)
	}
}
