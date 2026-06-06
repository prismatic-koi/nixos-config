package cmd

// Tests for prism stats compare — focus on issue #2102:
//
//   Layer 1: spawn_outcome aggregates must be available to `prism stats
//   compare` between terminal-state transition and `prism cleanup`. The
//   read path falls back to db.ComputeSpawnOutcome when no persisted row
//   exists yet.
//
//   Layer 2: the Spawn Inputs block must surface the values written at
//   spawn time (profile_name, isolation, harness, branch, agent_role)
//   rather than the stale C.1 placeholder.
//
// Tests use the openStatsTestDB / writeStatsEvent helpers defined in
// stats_test.go. Sessions are constructed by hand to mimic the
// post-spawn / post-terminal / pre-cleanup state without spinning up
// tmux, sidecar, or a real agent.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// seedCompareSession inserts the minimum rows the compare engine reads:
// agent_status (driver of db.SessionIsTerminal), sessions (driver of Outcome
// fallback aggregation), and optionally spawn_inputs.
//
// finalState is the agent_status.state value the test wants the row to
// carry — pass "active" / "idle" / "finished" / "error" / "interrupted" /
// "deleted" depending on the case under test. The sessions row's
// end_state mirrors the sidecar-shutdown / cleanup contract:
//   - terminal final states populate sessions.end_state with the same value
//     so the sessions-row fallback in db.SessionIsTerminal also returns true,
//   - non-terminal final states leave sessions.end_state NULL.
func seedCompareSession(t *testing.T, d *db.DB, sessionName string, startedAt time.Time, finalState agent.AgentState, inputs *db.SpawnInputs) (instanceID string) {
	t.Helper()
	instanceID = uuid.New().String()

	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, string(finalState), nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}

	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}

	// Mirror the cleanup-path UpdateSessionEnded write only when the final
	// state is terminal. Live sessions intentionally leave sessions.end_state
	// NULL so the negative-test AC exercises the gate correctly.
	if agent.IsTerminal(finalState) {
		if err := d.UpdateSessionEnded(instanceID, string(finalState)); err != nil {
			t.Fatalf("UpdateSessionEnded %q: %v", sessionName, err)
		}
	}

	if inputs != nil {
		inputs.InstanceID = instanceID
		if inputs.CreatedAt == 0 {
			inputs.CreatedAt = startedAt.UnixMilli()
		}
		if err := d.InsertSpawnInputs(*inputs); err != nil {
			t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
		}
	}
	return instanceID
}

// writeAssistantTurn emits a msg_assistant event with the given token /
// cost numbers, linking it to the session's instance_id so the aggregation
// query picks it up.
func writeAssistantTurn(t *testing.T, d *db.DB, sessionName, instanceID string, ts time.Time, inputTokens, outputTokens, cacheRead, cacheWrite int, cost float64) {
	t.Helper()
	payload := assistantPayloadWithTokens("turn-"+uuid.New().String(), "reply", inputTokens, outputTokens, cacheRead, cacheWrite, 5000, 25.0)
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &instanceID,
		Type:        "msg_assistant",
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent (msg_assistant): %v", err)
	}
	if cost > 0 {
		// Replace payload to embed the cost field; the assistantPayloadWithTokens
		// helper does not include it. Cheaper than building a parallel helper.
		// Use a second event purely for cost aggregation — the aggregation
		// SUM picks up the cost field from any msg_assistant row.
		costPayload := assistantPayloadWithEventCost("cost-"+uuid.New().String(), "anthropic/claude-sonnet-4-6", 0, 0, cost)
		ev2 := db.Event{
			ID:          uuid.New().String(),
			SessionName: sessionName,
			Repo:        "repo",
			Worktree:    "/wt/" + sessionName,
			InstanceID:  &instanceID,
			Type:        "msg_assistant",
			Payload:     costPayload,
			CreatedAt:   ts.Add(time.Millisecond),
		}
		if err := d.WriteEvent(ev2); err != nil {
			t.Fatalf("WriteEvent (msg_assistant cost): %v", err)
		}
	}
}

// writeToolCall emits a tool_call event tied to instance_id.
func writeToolCall(t *testing.T, d *db.DB, sessionName, instanceID, tool string, ts time.Time) {
	t.Helper()
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		InstanceID:  &instanceID,
		Type:        "tool_call",
		Payload:     toolCallPayloadWithDuration("tc-"+uuid.New().String(), tool, "args", 100),
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent (tool_call): %v", err)
	}
}

// ── Layer 1 — spawn_outcome on-the-fly compute ───────────────────────────────

// TestLoadCompareRuns_TerminalFinishedNoOutcome covers the core AC: a
// session that has transitioned to `finished` but has not yet been cleaned
// up — `prism stats compare` must populate every aggregate axis without
// the spawn_outcome row, by computing on the fly from agent_events.
func TestLoadCompareRuns_TerminalFinishedNoOutcome(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	const sessionName = "repo@finished-no-cleanup"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)

	// Two assistant turns + a tool call. Numbers picked so each aggregate
	// has a distinctive sum, making mis-aggregation easy to catch.
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(40*time.Second), 2000, 900, 400, 200, 0.34)
	writeToolCall(t, d, sessionName, iid, "bash", startedAt.Add(20*time.Second))

	// Sanity: no spawn_outcome row exists yet (cleanup has not run).
	if got, err := d.SpawnOutcomeByInstanceID(iid); err != nil || got != nil {
		t.Fatalf("pre-condition: SpawnOutcomeByInstanceID = (%v, %v), want (nil, nil)", got, err)
	}

	sess, err := d.SessionByInstanceID(iid)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID: sess=%v err=%v", sess, err)
	}

	runs := loadCompareRuns(d, []*db.Session{sess})
	if len(runs) != 1 || runs[0].Outcome == nil {
		t.Fatalf("loadCompareRuns: outcome nil (terminal+no-cleanup case must populate via ComputeSpawnOutcome)")
	}
	out := runs[0].Outcome
	if out.TokensInputTotal != 3500 {
		t.Errorf("TokensInputTotal: got %d, want 3500 (1500+2000)", out.TokensInputTotal)
	}
	if out.TokensOutputTotal != 1600 {
		t.Errorf("TokensOutputTotal: got %d, want 1600 (700+900)", out.TokensOutputTotal)
	}
	if out.MsgAssistantCount != 4 {
		// 2 token-bearing turns + 2 cost-bearing turns = 4 msg_assistant rows.
		t.Errorf("MsgAssistantCount: got %d, want 4", out.MsgAssistantCount)
	}
	if out.ToolCallCount != 1 {
		t.Errorf("ToolCallCount: got %d, want 1", out.ToolCallCount)
	}
	if out.CostUSDTotal < 0.45 || out.CostUSDTotal > 0.47 {
		t.Errorf("CostUSDTotal: got %.4f, want ~0.46 (0.12+0.34)", out.CostUSDTotal)
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState: got %v, want \"finished\"", out.EndState)
	}
	if out.TimeToFirstEventMs == nil || *out.TimeToFirstEventMs <= 0 {
		t.Errorf("TimeToFirstEventMs: got %v, want >0", out.TimeToFirstEventMs)
	}
}

// TestLoadCompareRuns_TerminalErrorNoOutcome verifies the same shape for an
// `error` terminal state (issue #2081 path: zero-output exit transitions to
// error). The AC requires identical behaviour across finished/error/interrupted.
func TestLoadCompareRuns_TerminalErrorNoOutcome(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	const sessionName = "repo@error-no-cleanup"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateError, nil)

	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(5*time.Second), 800, 400, 0, 0, 0.05)
	// An error event between the assistant turn and termination, to exercise
	// ErrorEventCount aggregation.
	ev := db.Event{
		ID: uuid.New().String(), SessionName: sessionName, Repo: "repo",
		Worktree: "/wt/" + sessionName, InstanceID: &iid,
		Type: "error", Payload: `{"message":"transient"}`,
		CreatedAt: startedAt.Add(7 * time.Second),
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent (error): %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})
	if runs[0].Outcome == nil {
		t.Fatalf("Outcome nil on state=error (must be computed on the fly)")
	}
	out := runs[0].Outcome
	if out.EndState == nil || *out.EndState != "error" {
		t.Errorf("EndState: got %v, want \"error\"", out.EndState)
	}
	if out.TokensInputTotal != 800 {
		t.Errorf("TokensInputTotal: got %d, want 800", out.TokensInputTotal)
	}
	if out.ErrorEventCount != 1 {
		t.Errorf("ErrorEventCount: got %d, want 1", out.ErrorEventCount)
	}
}

// TestLoadCompareRuns_TerminalInterruptedNoOutcome covers the interrupted
// terminal path.
func TestLoadCompareRuns_TerminalInterruptedNoOutcome(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	const sessionName = "repo@interrupted-no-cleanup"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateInterrupted, nil)

	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(5*time.Second), 600, 300, 0, 0, 0.02)
	// Interrupted: a state_change event with state=interrupted.
	ev := db.Event{
		ID: uuid.New().String(), SessionName: sessionName, Repo: "repo",
		Worktree: "/wt/" + sessionName, InstanceID: &iid,
		Type: "state_change", Payload: `{"state":"interrupted"}`,
		CreatedAt: startedAt.Add(6 * time.Second),
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent (state_change interrupted): %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})
	if runs[0].Outcome == nil {
		t.Fatalf("Outcome nil on state=interrupted (must be computed on the fly)")
	}
	out := runs[0].Outcome
	if out.EndState == nil || *out.EndState != "interrupted" {
		t.Errorf("EndState: got %v, want \"interrupted\"", out.EndState)
	}
	if out.InterruptedCount != 1 {
		t.Errorf("InterruptedCount: got %d, want 1", out.InterruptedCount)
	}
}

// TestLoadCompareRuns_TerminalThenCleanup verifies the idempotence AC: the
// values surfaced before cleanup must match the values surfaced after
// cleanup. Compute → WriteSpawnOutcome → read → must agree, byte for byte
// on every aggregate column.
func TestLoadCompareRuns_TerminalThenCleanup(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-3 * time.Minute)

	const sessionName = "repo@finished-then-cleanup"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)

	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(10*time.Second), 1200, 600, 100, 50, 0.10)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(30*time.Second), 800, 400, 50, 25, 0.07)
	writeToolCall(t, d, sessionName, iid, "bash", startedAt.Add(15*time.Second))
	writeToolCall(t, d, sessionName, iid, "read", startedAt.Add(25*time.Second))

	sess, _ := d.SessionByInstanceID(iid)

	// Before cleanup: outcome is computed on the fly.
	before := loadCompareRuns(d, []*db.Session{sess})[0].Outcome
	if before == nil {
		t.Fatalf("pre-cleanup outcome nil")
	}

	// Run the cleanup-equivalent write.
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	// After cleanup: same values, now from the persisted row.
	after := loadCompareRuns(d, []*db.Session{sess})[0].Outcome
	if after == nil {
		t.Fatalf("post-cleanup outcome nil")
	}

	if before.TokensInputTotal != after.TokensInputTotal ||
		before.TokensOutputTotal != after.TokensOutputTotal ||
		before.TokensCacheReadTotal != after.TokensCacheReadTotal ||
		before.TokensCacheWriteTotal != after.TokensCacheWriteTotal ||
		before.ToolCallCount != after.ToolCallCount ||
		before.MsgAssistantCount != after.MsgAssistantCount {
		t.Errorf("idempotence drift\n  before: %+v\n  after:  %+v", before, after)
	}
	// Cost is a float; allow a tiny epsilon though INSERT OR REPLACE should
	// produce an exact round-trip on the same SUM.
	if absDiff(before.CostUSDTotal, after.CostUSDTotal) > 1e-9 {
		t.Errorf("CostUSDTotal drift: before=%.6f, after=%.6f", before.CostUSDTotal, after.CostUSDTotal)
	}
}

// TestLoadCompareRuns_ActiveSessionReturnsDash is the over-broad-fix negative
// test: a session still in state=active must surface — for aggregate axes,
// not stale numbers. Live sessions are excluded from the on-the-fly compute
// gate even if events have been written for them.
func TestLoadCompareRuns_ActiveSessionReturnsDash(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-30 * time.Second)

	const sessionName = "repo@still-active"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateActive, nil)

	// Events exist but the session has NOT transitioned to terminal.
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(5*time.Second), 1000, 500, 0, 0, 0.05)
	writeToolCall(t, d, sessionName, iid, "bash", startedAt.Add(10*time.Second))

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})
	if runs[0].Outcome != nil {
		t.Errorf("active session must surface nil Outcome (renderer shows \"—\"); got non-nil: %+v", runs[0].Outcome)
	}

	// The axisValue path must also collapse to "—" for aggregate axes on a
	// live session — this is what the renderer actually emits.
	for _, axis := range []string{"tokens_input", "tokens_output", "cost_usd", "tool_call", "duration_ms"} {
		if got := axisValue(axis, runs[0]); got != "—" {
			t.Errorf("axisValue(%q) on active session = %q, want \"—\"", axis, got)
		}
	}
}

// ── Layer 2 — spawn_inputs renderer ──────────────────────────────────────────

// TestInputsValue_PullsFromSpawnInputs verifies the per-axis lookup against
// a populated spawn_inputs row.
func TestInputsValue_PullsFromSpawnInputs(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-full"
	inputs := &db.SpawnInputs{
		ProfileName:   strPtr("anthropic"),
		HarnessFlag:   strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
		AgentFlag:     strPtr("worker"),
		BranchFlag:    strPtr("feature/x"),
		AbtestPairID:  strPtr("pair-uuid-0001"),
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	want := map[string]string{
		"profile_name":   "anthropic",
		"harness":        "pi",
		"isolation_mode": "bwrap",
		"agent_role":     "worker",
		"branch":         "feature/x",
		"abtest_pair_id": "pair-uuid-0001",
	}
	for axis, wantVal := range want {
		if got := inputsValue(axis, runs[0]); got != wantVal {
			t.Errorf("inputsValue(%q) = %q, want %q", axis, got, wantVal)
		}
	}
}

// TestInputsValue_IsolationModePreferredOverFlag is the issue #2105
// renderer AC. When a row has BOTH isolation_mode and isolation_flag set
// (the new post-fix shape), the renderer must surface isolation_mode —
// the resolved effective mode is what the operator wants to see for
// A/B leg comparisons. isolation_flag is the raw audit trail.
func TestInputsValue_IsolationModePreferredOverFlag(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-iso-mode-preferred"
	inputs := &db.SpawnInputs{
		ProfileName:   strPtr("anthropic"),
		HarnessFlag:   strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
		IsolationMode: strPtr("sandbox-exec"),
		AgentFlag:     strPtr("worker"),
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("isolation_mode", runs[0]); got != "sandbox-exec" {
		t.Errorf("inputsValue(isolation_mode) = %q, want %q (resolved mode wins over raw flag)",
			got, "sandbox-exec")
	}
}

// TestInputsValue_IsolationModeFallsBackToFlagForLegacyRow is the
// issue #2105 over-broad-fix guard. Pre-#2105 rows have isolation_mode
// NULL (the column did not yet exist or the writer did not yet populate
// it) but isolation_flag set to the resolved mode (the old shim). The
// renderer must surface the legacy isolation_flag value gracefully
// rather than "—" — historical sessions should still display sensibly
// in stats compare output.
func TestInputsValue_IsolationModeFallsBackToFlagForLegacyRow(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-iso-mode-legacy"
	inputs := &db.SpawnInputs{
		ProfileName:   strPtr("anthropic"),
		HarnessFlag:   strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
		// IsolationMode deliberately nil — simulating a pre-#2105 row.
		AgentFlag: strPtr("worker"),
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("isolation_mode", runs[0]); got != "bwrap" {
		t.Errorf("inputsValue(isolation_mode) on legacy row = %q, want %q (fallback to isolation_flag)",
			got, "bwrap")
	}
}

// TestInputsValue_IsolationModeAbsentRendersDash guards the empty-row
// branch: when neither isolation_mode nor isolation_flag is set (e.g. a
// pre-spawn_inputs row from a test fixture), the renderer must return
// "—" rather than crash on a nil pointer dereference.
func TestInputsValue_IsolationModeAbsentRendersDash(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-iso-mode-absent"
	inputs := &db.SpawnInputs{
		ProfileName: strPtr("anthropic"),
		// Both IsolationFlag and IsolationMode deliberately nil.
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("isolation_mode", runs[0]); got != "—" {
		t.Errorf("inputsValue(isolation_mode) on empty row = %q, want %q", got, "—")
	}
}

// TestInputsValue_PartialRowSurfacesWhatExists guards the Layer 2 AC: a row
// with only profile_name set (the #2092/#2093 case) must surface that field
// rather than treating the whole row as absent.
func TestInputsValue_PartialRowSurfacesWhatExists(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-partial"
	inputs := &db.SpawnInputs{
		ProfileName: strPtr("anthropic-opus-max"),
		// All other flags intentionally nil — partial row case.
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("profile_name", runs[0]); got != "anthropic-opus-max" {
		t.Errorf("profile_name on partial row = %q, want \"anthropic-opus-max\"", got)
	}
	// Harness falls back to sessions.harness (the InsertSession default in
	// seedCompareSession is "pi"), which is the intended graceful-degrade.
	if got := inputsValue("harness", runs[0]); got != "pi" {
		t.Errorf("harness on partial row = %q, want fallback to sessions.harness (\"pi\")", got)
	}
	// Branch is absent — must surface as "—" rather than blank.
	if got := inputsValue("branch", runs[0]); got != "—" {
		t.Errorf("branch on partial row = %q, want \"—\"", got)
	}
}

// TestRenderCompareTable_NoCInOnePlaceholder is the smoke test for the
// stale-placeholder removal AC. The old text "(spawn_inputs table not yet
// populated — run prism spawn with C.1 to capture inputs)" must not appear
// in the rendered table for a session with a populated spawn_inputs row.
func TestRenderCompareTable_NoCInOnePlaceholder(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionA = "repo@compare-a"
	const sessionB = "repo@compare-b"
	inputsA := &db.SpawnInputs{
		ProfileName: strPtr("anthropic"), HarnessFlag: strPtr("pi"),
		IsolationFlag: strPtr("bwrap"), AgentFlag: strPtr("worker"),
	}
	inputsB := &db.SpawnInputs{
		ProfileName: strPtr("google-gemini"), HarnessFlag: strPtr("pi"),
		IsolationFlag: strPtr("bwrap"), AgentFlag: strPtr("worker"),
	}
	iidA := seedCompareSession(t, d, sessionA, startedAt, agent.StateFinished, inputsA)
	iidB := seedCompareSession(t, d, sessionB, startedAt, agent.StateFinished, inputsB)
	writeAssistantTurn(t, d, sessionA, iidA, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, sessionB, iidB, startedAt.Add(time.Second), 200, 75, 0, 0, 0.02)

	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	runs := loadCompareRuns(d, []*db.Session{sessA, sessB})

	out := captureStdout(t, func() {
		if err := renderCompareTable(runs, defaultAxes(), true /* includeInputs */, false, false); err != nil {
			t.Fatalf("renderCompareTable: %v", err)
		}
	})

	if strings.Contains(out, "C.1") {
		t.Errorf("rendered output still mentions C.1:\n%s", out)
	}
	if strings.Contains(out, "spawn_inputs table not yet populated") {
		t.Errorf("rendered output still carries the stale placeholder:\n%s", out)
	}
	// Both profile names must appear — they're the per-leg discriminator
	// for an A/B-test comparison, and the whole point of Layer 2.
	if !strings.Contains(out, "anthropic") {
		t.Errorf("rendered output missing run-A profile_name:\n%s", out)
	}
	if !strings.Contains(out, "google-gemini") {
		t.Errorf("rendered output missing run-B profile_name:\n%s", out)
	}
	if !strings.Contains(out, "profile_name:") {
		t.Errorf("rendered output missing profile_name row label:\n%s", out)
	}
	// Aggregate axes for the terminal-no-cleanup case must NOT all be "—".
	// Token totals from the two assistant turns must appear in the table.
	if !strings.Contains(out, "tokens_input:") {
		t.Errorf("rendered output missing tokens_input row:\n%s", out)
	}
}

// TestRenderCompareTable_LiveSessionStillShowsDash verifies the negative
// AC at the table-renderer level: an active session in the pair must
// render aggregate axes as "—" even when spawn_inputs are populated.
func TestRenderCompareTable_LiveSessionStillShowsDash(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-30 * time.Second)

	const sessionName = "repo@live-active"
	inputs := &db.SpawnInputs{
		ProfileName: strPtr("anthropic"), HarnessFlag: strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateActive, inputs)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess, sess})

	out := captureStdout(t, func() {
		if err := renderCompareTable(runs, defaultAxes(), true, false, false); err != nil {
			t.Fatalf("renderCompareTable: %v", err)
		}
	})

	// Inputs block must still appear (those values ARE known at spawn time).
	if !strings.Contains(out, "anthropic") {
		t.Errorf("rendered output missing profile_name on live session:\n%s", out)
	}
	// But aggregate axes must read "—" — the session is still in progress.
	// "100" / "150" are the seeded tokens from writeAssistantTurn — they
	// must not surface for a still-active session. Use word-boundary regex
	// rather than substring containment because the instance_id row in the
	// rendered table is a UUID and ~1/256 UUIDs happen to end in "100" or
	// "150", triggering a false-positive leak detection (issue #2169 §
	// Cluster 4). Word boundaries ensure we only match the token values
	// (which are whitespace-surrounded in the table), not UUID substrings.
	leakRe := regexp.MustCompile(`\b(100|150)\b`)
	if leakRe.MatchString(out) {
		t.Errorf("rendered output leaks aggregate data for an active session:\n%s", out)
	}
}

// ── Helper ───────────────────────────────────────────────────────────────────

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
