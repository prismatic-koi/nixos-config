package review_test

// retro_cycles_test.go — tests for AssembleReviewCycles, the per-cycle,
// per-agent review detail behind `prism retro <train-session>` (issue #2584).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

const cyclesParent = "nixos-config@cycles-feature"

// cyclesAgentSession returns one review-agent session name for round/agent.
func cyclesAgentSession(round int, agent string) string {
	return cyclesParent + "~review-" + itoaCycles(round) + "-" + agent
}

func itoaCycles(n int) string {
	// Avoid importing strconv twice across files; small local helper.
	digits := "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

// seedCycleAgent registers one review agent's agent_status row for groupID
// and, when msg carries a verdict, writes a msg_assistant event carrying it
// plus turns/cost via the standard payload shape ComputeSpawnOutcome and
// SessionEventAggregates both read.
func seedCycleAgent(t *testing.T, d *db.DB, sess, groupID, state string) {
	t.Helper()
	if err := d.UpsertStatus(sess, "nixos-config", "/wt", state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", sess, err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID(%q): %v", sess, err)
	}
}

func writeCycleMsgAssistant(t *testing.T, d *db.DB, sess, text string, outputTokens int, cost float64) {
	t.Helper()
	payload := `{"text":"` + text + `","outputTokens":` + itoaCycles(outputTokens) +
		`,"cacheReadTokens":0,"cacheWriteTokens":0,"cost":` + ftoaCycles(cost) + `}`
	if err := d.WriteEvent(db.Event{
		ID:          sess + "-msg-" + text,
		SessionName: sess,
		Repo:        "nixos-config",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     payload,
	}); err != nil {
		t.Fatalf("WriteEvent msg_assistant for %q: %v", sess, err)
	}
}

func ftoaCycles(f float64) string {
	if f == 0 {
		return "0.0"
	}
	// Minimal, test-only float formatting (no scientific notation risk at
	// these magnitudes).
	whole := int64(f)
	frac := int64((f - float64(whole)) * 100)
	if frac < 0 {
		frac = -frac
	}
	return itoaCycles(int(whole)) + "." + itoaCycles(int(frac))
}

// TestAssembleReviewCycles_NoGroups verifies the edge-case AC: a train with no
// session_groups rows returns an empty, non-nil slice — the caller renders
// "no review cycles ran", not an empty table.
func TestAssembleReviewCycles_NoGroups(t *testing.T) {
	d := openTestDB(t)
	cycles, err := review.AssembleReviewCycles(d, "nixos-config@lonely-worker")
	if err != nil {
		t.Fatalf("AssembleReviewCycles: %v", err)
	}
	if cycles == nil {
		t.Fatal("AssembleReviewCycles returned a nil slice; want non-nil empty slice")
	}
	if len(cycles) != 0 {
		t.Fatalf("len(cycles) = %d, want 0", len(cycles))
	}
}

// TestAssembleReviewCycles_PassFailAndRound verifies the core AC: cost, turn
// count, and verdict are reported per agent, grouped by the native `round`
// column, and read from agent_events (not spawn_outcome or live GroupResults).
func TestAssembleReviewCycles_PassFailAndRound(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroupWithPR(cyclesParent, "42", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	pass := cyclesAgentSession(1, "review-goal")
	fail := cyclesAgentSession(1, "review-code")
	seedCycleAgent(t, d, pass, groupID, "finished")
	seedCycleAgent(t, d, fail, groupID, "finished")
	// Every review agent's session row is closed by the time an operator
	// looks at history — mirrors the live DB (#2594).
	if err := d.SetEnded(pass); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.SetEnded(fail); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	writeCycleMsgAssistant(t, d, pass, "<verdict>PASS</verdict>", 100, 1.5)
	writeCycleMsgAssistant(t, d, fail, "<verdict>FAIL</verdict>", 200, 2.0)

	cycles, err := review.AssembleReviewCycles(d, cyclesParent)
	if err != nil {
		t.Fatalf("AssembleReviewCycles: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("len(cycles) = %d, want 1: %+v", len(cycles), cycles)
	}
	c := cycles[0]
	if c.Round != 1 {
		t.Errorf("Round = %d, want 1", c.Round)
	}
	if c.PRNumber != "42" {
		t.Errorf("PRNumber = %q, want %q", c.PRNumber, "42")
	}
	if !c.CountsAsCycle {
		t.Error("CountsAsCycle = false, want true — every expected agent produced a verdict")
	}
	if c.NonCountingLabel != "" {
		t.Errorf("NonCountingLabel = %q, want empty for a complete round", c.NonCountingLabel)
	}
	if len(c.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2: %+v", len(c.Agents), c.Agents)
	}

	byAgent := map[string]review.ReviewCycleAgent{}
	for _, a := range c.Agents {
		byAgent[a.Session] = a
	}

	got := byAgent[pass]
	if got.Verdict != "PASS" {
		t.Errorf("pass agent Verdict = %q, want PASS", got.Verdict)
	}
	if !got.DataRecorded || got.Turns != 1 || got.CostUSD != 1.5 {
		t.Errorf("pass agent aggregate = %+v, want DataRecorded=true, Turns=1, CostUSD=1.5", got)
	}

	got = byAgent[fail]
	if got.Verdict != "FAIL" {
		t.Errorf("fail agent Verdict = %q, want FAIL", got.Verdict)
	}
	if !got.DataRecorded || got.Turns != 1 || got.CostUSD != 2.0 {
		t.Errorf("fail agent aggregate = %+v, want DataRecorded=true, Turns=1, CostUSD=2.0", got)
	}
}

// TestAssembleReviewCycles_MissingVerdictIsDistinctFromPass verifies an agent
// with no verdict is reported distinctly from PASS/FAIL, using #2573's
// classification, not the live db.GroupResults (which would drop the reaped
// row entirely because ended_at is set).
func TestAssembleReviewCycles_MissingVerdictIsDistinctFromPass(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroupWithPR(cyclesParent, "43", 2)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	pass := cyclesAgentSession(2, "review-goal")
	reaped := cyclesAgentSession(2, "review-qa")
	seedCycleAgent(t, d, pass, groupID, "finished")
	seedCycleAgent(t, d, reaped, groupID, "deleted")
	if err := d.SetEnded(pass); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.SetEnded(reaped); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	writeCycleMsgAssistant(t, d, pass, "<verdict>PASS</verdict>", 50, 1.0)
	// No msg_assistant event for the reaped agent — it never produced output.

	cycles, err := review.AssembleReviewCycles(d, cyclesParent)
	if err != nil {
		t.Fatalf("AssembleReviewCycles: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("len(cycles) = %d, want 1", len(cycles))
	}
	c := cycles[0]
	if c.CountsAsCycle {
		t.Error("CountsAsCycle = true, want false — one agent produced no verdict (#2573)")
	}
	if c.NonCountingLabel == "" {
		t.Error("NonCountingLabel is empty, want a #2573 label for the non-counting round")
	}

	byAgent := map[string]review.ReviewCycleAgent{}
	for _, a := range c.Agents {
		byAgent[a.Session] = a
	}
	got := byAgent[reaped]
	if got.Verdict != "" {
		t.Errorf("reaped agent Verdict = %q, want empty (no verdict)", got.Verdict)
	}
	// Historical reads use GroupResultsAll (no ended_at filter), so a member
	// whose row is present but never reached a parseable verdict classifies
	// via classifyMember, not the live-only "session ended mid-review" class
	// (which applies only when a row is ABSENT from the live GroupResults).
	if got.NoVerdictClass != string(review.NoVerdictUnexpectedState) {
		t.Errorf("reaped agent NoVerdictClass = %q, want %q", got.NoVerdictClass, review.NoVerdictUnexpectedState)
	}
	if got.DataRecorded {
		t.Error("reaped agent DataRecorded = true, want false — no msg_assistant event was ever recorded")
	}

	passGot := byAgent[pass]
	if passGot.Verdict != "PASS" {
		t.Errorf("pass agent Verdict = %q, want PASS", passGot.Verdict)
	}
}

// TestAssembleReviewCycles_NoDataDistinctFromZeroCost verifies correction 2:
// a round with no agent_status rows at all ("no review data recorded") must
// render distinctly from a round whose agents ran and recorded a genuine zero
// cost.
func TestAssembleReviewCycles_NoDataDistinctFromZeroCost(t *testing.T) {
	d := openTestDB(t)

	// Round 1: registered but no members were ever written (e.g. the group
	// was registered and then the process died before any agent spawned).
	if _, err := d.RegisterGroupWithPR(cyclesParent, "44", 1); err != nil {
		t.Fatalf("RegisterGroupWithPR round1: %v", err)
	}

	// Round 2: one agent ran and genuinely recorded a $0 cost (subscription
	// profile).
	groupID2, err := d.RegisterGroupWithPR(cyclesParent, "44", 2)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR round2: %v", err)
	}
	zeroCost := cyclesAgentSession(2, "review-goal")
	seedCycleAgent(t, d, zeroCost, groupID2, "finished")
	if err := d.SetEnded(zeroCost); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	writeCycleMsgAssistant(t, d, zeroCost, "<verdict>PASS</verdict>", 10, 0.0)

	cycles, err := review.AssembleReviewCycles(d, cyclesParent)
	if err != nil {
		t.Fatalf("AssembleReviewCycles: %v", err)
	}
	if len(cycles) != 2 {
		t.Fatalf("len(cycles) = %d, want 2: %+v", len(cycles), cycles)
	}

	round1 := cycles[0]
	if len(round1.Agents) != 0 {
		t.Errorf("round1 Agents = %+v, want empty — no agent_status rows were ever written", round1.Agents)
	}

	round2 := cycles[1]
	if len(round2.Agents) != 1 {
		t.Fatalf("round2 Agents = %+v, want 1", round2.Agents)
	}
	a := round2.Agents[0]
	if !a.DataRecorded {
		t.Error("round2 agent DataRecorded = false, want true — the agent ran and recorded a real (zero) cost")
	}
	if a.CostUSD != 0 {
		t.Errorf("round2 agent CostUSD = %v, want 0", a.CostUSD)
	}
}

// TestAssembleReviewCycles_GroupedByRoundColumn verifies rounds are grouped
// by the native session_groups.round column and returned in round order, not
// by parsing a round number out of a session name.
func TestAssembleReviewCycles_GroupedByRoundColumn(t *testing.T) {
	d := openTestDB(t)

	// Register round 2 before round 1 to prove ordering follows the `round`
	// column, not creation order.
	g2, err := d.RegisterGroupWithPR(cyclesParent, "45", 2)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR round2: %v", err)
	}
	g1, err := d.RegisterGroupWithPR(cyclesParent, "45", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR round1: %v", err)
	}

	s1 := cyclesAgentSession(1, "review-goal")
	s2 := cyclesAgentSession(2, "review-goal")
	seedCycleAgent(t, d, s1, g1, "finished")
	seedCycleAgent(t, d, s2, g2, "finished")
	if err := d.SetEnded(s1); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.SetEnded(s2); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	writeCycleMsgAssistant(t, d, s1, "<verdict>PASS</verdict>", 1, 0)
	writeCycleMsgAssistant(t, d, s2, "<verdict>PASS</verdict>", 1, 0)

	cycles, err := review.AssembleReviewCycles(d, cyclesParent)
	if err != nil {
		t.Fatalf("AssembleReviewCycles: %v", err)
	}
	if len(cycles) != 2 {
		t.Fatalf("len(cycles) = %d, want 2", len(cycles))
	}
	if cycles[0].Round != 1 || cycles[1].Round != 2 {
		t.Errorf("round order = [%d,%d], want [1,2]", cycles[0].Round, cycles[1].Round)
	}
}

// TestRetroReportWithCycles_JSONShape pins the --json wire contract: the
// wrapper embeds db.RetroReport's fields and adds "train" and
// "review_cycles", so section 3's --json output shares the base command's
// snake_case, RFC 3339 contract.
func TestRetroReportWithCycles_JSONShape(t *testing.T) {
	report := &db.RetroReport{
		Repo:   "nixos-config",
		Since:  "2026-08-01T00:00:00Z",
		Until:  "2026-08-02T00:00:00Z",
		Trains: []db.RetroTrain{},
	}
	wrapped := review.RetroReportWithCycles{
		RetroReport:  report,
		Train:        cyclesParent,
		ReviewCycles: []review.ReviewCycle{},
	}
	b, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data := string(b)
	for _, want := range []string{`"repo":"nixos-config"`, `"train":"nixos-config@cycles-feature"`, `"review_cycles":[]`} {
		if !strings.Contains(data, want) {
			t.Errorf("json output missing %q; got %s", want, data)
		}
	}
}
