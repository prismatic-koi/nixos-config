package db_test

// Tests for the read-time spawn_outcome compute path (issue #2102):
// SpawnOutcomeForCompare returns the persisted row post-cleanup and otherwise
// computes the aggregates on the fly for a terminal-state session that has not
// been cleaned up yet — while leaving in-progress sessions reporting "—".

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// seedComputeSession inserts a sessions row (no ended_at / end_state — i.e.
// pre-cleanup), a handful of token / tool / error-bearing agent_events, and an
// agent_status row in the given state linked by instance_id. It mirrors a real
// session that has reached a terminal agent state but has not yet been cleaned
// up (so the spawn_outcome row is still absent).
func seedComputeSession(t *testing.T, d *db.DB, state string) string {
	t.Helper()
	iid := uuid.New().String()
	sessName := "prism-test@compute-" + iid[:8]
	startedAt := time.Now().Add(-10 * time.Minute)

	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessName,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/" + iid[:8],
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Two assistant messages carrying tokens + cost.
	writeComputeEvent(t, d, sessName, iid, "msg_assistant",
		`{"inputTokens":100,"outputTokens":50,"cacheReadTokens":10,"cacheWriteTokens":5,"cost":0.0010}`,
		startedAt.Add(1*time.Minute))
	writeComputeEvent(t, d, sessName, iid, "msg_assistant",
		`{"inputTokens":200,"outputTokens":80,"cacheReadTokens":20,"cacheWriteTokens":7,"cost":0.0020}`,
		startedAt.Add(2*time.Minute))
	// Tool activity: two calls, one error.
	writeComputeEvent(t, d, sessName, iid, "tool_call", `{"name":"bash"}`, startedAt.Add(90*time.Second))
	writeComputeEvent(t, d, sessName, iid, "tool_call", `{"name":"read"}`, startedAt.Add(100*time.Second))
	writeComputeEvent(t, d, sessName, iid, "tool_error", `{"name":"bash"}`, startedAt.Add(110*time.Second))

	// agent_status in the requested state, linked to the instance.
	if err := d.UpsertStatus(sessName, "testrepo", "/code/testrepo/"+iid[:8], state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", state, err)
	}
	if err := d.SetInstanceID(sessName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	return iid
}

func writeComputeEvent(t *testing.T, d *db.DB, sess, iid, typ, payload string, ts time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sess,
		InstanceID:  &iid,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/wt",
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}); err != nil {
		t.Fatalf("WriteEvent(%s): %v", typ, err)
	}
}

// assertSeededAggregates checks the aggregate axes against the fixed values
// written by seedComputeSession.
func assertSeededAggregates(t *testing.T, out *db.SpawnOutcome) {
	t.Helper()
	if out == nil {
		t.Fatal("SpawnOutcomeForCompare: got nil, want populated outcome")
	}
	if out.TokensInputTotal != 300 {
		t.Errorf("TokensInputTotal: got %d, want 300", out.TokensInputTotal)
	}
	if out.TokensOutputTotal != 130 {
		t.Errorf("TokensOutputTotal: got %d, want 130", out.TokensOutputTotal)
	}
	if out.TokensCacheReadTotal != 30 {
		t.Errorf("TokensCacheReadTotal: got %d, want 30", out.TokensCacheReadTotal)
	}
	if out.TokensCacheWriteTotal != 12 {
		t.Errorf("TokensCacheWriteTotal: got %d, want 12", out.TokensCacheWriteTotal)
	}
	if out.CostUSDTotal < 0.0029 || out.CostUSDTotal > 0.0031 {
		t.Errorf("CostUSDTotal: got %v, want ~0.003", out.CostUSDTotal)
	}
	if out.ToolCallCount != 2 {
		t.Errorf("ToolCallCount: got %d, want 2", out.ToolCallCount)
	}
	if out.ToolErrorCount != 1 {
		t.Errorf("ToolErrorCount: got %d, want 1", out.ToolErrorCount)
	}
	if out.MsgAssistantCount != 2 {
		t.Errorf("MsgAssistantCount: got %d, want 2", out.MsgAssistantCount)
	}
	if out.TimeToFirstEventMs == nil {
		t.Error("TimeToFirstEventMs: got nil, want populated")
	}
	if out.DurationMs == nil {
		t.Error("DurationMs: got nil, want populated (fallback to last event ts pre-cleanup)")
	}
}

// TestSpawnOutcomeForCompare_TerminalStatesPopulate verifies the headline
// Layer-1 AC: after a session reaches a terminal state (finished / error /
// interrupted) but BEFORE cleanup, prism stats compare's data source returns
// the full aggregate axes computed on the fly from agent_events.
func TestSpawnOutcomeForCompare_TerminalStatesPopulate(t *testing.T) {
	for _, state := range []string{"finished", "error", "interrupted"} {
		state := state
		t.Run(state, func(t *testing.T) {
			d := openComputeTestDB(t)
			iid := seedComputeSession(t, d, state)

			// No spawn_outcome row was written (no cleanup ran).
			if persisted, err := d.SpawnOutcomeByInstanceID(iid); err != nil {
				t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
			} else if persisted != nil {
				t.Fatal("precondition: spawn_outcome row should be absent pre-cleanup")
			}

			out, err := d.SpawnOutcomeForCompare(iid)
			if err != nil {
				t.Fatalf("SpawnOutcomeForCompare: %v", err)
			}
			assertSeededAggregates(t, out)

			// end_state reflects the real terminal agent state pre-cleanup.
			if out.EndState == nil || *out.EndState != state {
				t.Errorf("EndState: got %v, want %q", out.EndState, state)
			}
		})
	}
}

// TestSpawnOutcomeForCompare_ActiveReturnsNil is the over-broad-fix guard
// (negative test): a session still in a non-terminal state must report "—"
// (nil outcome), not stale aggregates.
func TestSpawnOutcomeForCompare_ActiveReturnsNil(t *testing.T) {
	for _, state := range []string{"active", "idle", "waiting", "reviewing"} {
		state := state
		t.Run(state, func(t *testing.T) {
			d := openComputeTestDB(t)
			iid := seedComputeSession(t, d, state)

			out, err := d.SpawnOutcomeForCompare(iid)
			if err != nil {
				t.Fatalf("SpawnOutcomeForCompare: %v", err)
			}
			if out != nil {
				t.Errorf("SpawnOutcomeForCompare(%s): got non-nil outcome, want nil (in progress)", state)
			}
		})
	}
}

// TestSpawnOutcomeForCompare_NoStatusRowReturnsNil verifies that a session with
// no agent_status row at all (and no persisted outcome) is treated as
// non-terminal — nil, not a stale/partial compute.
func TestSpawnOutcomeForCompare_NoStatusRowReturnsNil(t *testing.T) {
	d := openComputeTestDB(t)
	iid := uuid.New().String()
	startedAt := time.Now().Add(-5 * time.Minute)
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: "prism-test@nostatus",
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/nostatus",
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	out, err := d.SpawnOutcomeForCompare(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeForCompare: %v", err)
	}
	if out != nil {
		t.Errorf("SpawnOutcomeForCompare: got non-nil, want nil (no agent_status row)")
	}
}

// TestSpawnOutcomeForCompare_IdempotentAcrossCleanup verifies the idempotence
// AC: the aggregate axes returned pre-cleanup (on-the-fly compute) are
// byte-identical to those returned post-cleanup (the persisted row written by
// WriteSpawnOutcome). The shared computeSpawnOutcome aggregation is the
// canonical source of truth, so there is no double-counting and no missed
// delta when cleanup overwrites the row.
func TestSpawnOutcomeForCompare_IdempotentAcrossCleanup(t *testing.T) {
	d := openComputeTestDB(t)
	iid := seedComputeSession(t, d, "finished")

	// Pre-cleanup: computed on the fly.
	pre, err := d.SpawnOutcomeForCompare(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeForCompare (pre-cleanup): %v", err)
	}
	assertSeededAggregates(t, pre)

	// Simulate cleanup: UpdateSessionEnded then WriteSpawnOutcome (the exact
	// cmd/cleanup.go sequence). This persists the spawn_outcome row.
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	// Post-cleanup: served from the persisted row.
	post, err := d.SpawnOutcomeForCompare(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeForCompare (post-cleanup): %v", err)
	}
	if post == nil {
		t.Fatal("SpawnOutcomeForCompare (post-cleanup): got nil, want persisted row")
	}

	// The event-derived aggregate axes must agree byte-for-byte.
	if pre.TokensInputTotal != post.TokensInputTotal {
		t.Errorf("TokensInputTotal drift: pre=%d post=%d", pre.TokensInputTotal, post.TokensInputTotal)
	}
	if pre.TokensOutputTotal != post.TokensOutputTotal {
		t.Errorf("TokensOutputTotal drift: pre=%d post=%d", pre.TokensOutputTotal, post.TokensOutputTotal)
	}
	if pre.TokensCacheReadTotal != post.TokensCacheReadTotal {
		t.Errorf("TokensCacheReadTotal drift: pre=%d post=%d", pre.TokensCacheReadTotal, post.TokensCacheReadTotal)
	}
	if pre.TokensCacheWriteTotal != post.TokensCacheWriteTotal {
		t.Errorf("TokensCacheWriteTotal drift: pre=%d post=%d", pre.TokensCacheWriteTotal, post.TokensCacheWriteTotal)
	}
	if pre.CostUSDTotal != post.CostUSDTotal {
		t.Errorf("CostUSDTotal drift: pre=%v post=%v", pre.CostUSDTotal, post.CostUSDTotal)
	}
	if pre.ToolCallCount != post.ToolCallCount {
		t.Errorf("ToolCallCount drift: pre=%d post=%d", pre.ToolCallCount, post.ToolCallCount)
	}
	if pre.ToolErrorCount != post.ToolErrorCount {
		t.Errorf("ToolErrorCount drift: pre=%d post=%d", pre.ToolErrorCount, post.ToolErrorCount)
	}
	if pre.MsgAssistantCount != post.MsgAssistantCount {
		t.Errorf("MsgAssistantCount drift: pre=%d post=%d", pre.MsgAssistantCount, post.MsgAssistantCount)
	}
	if (pre.TimeToFirstEventMs == nil) != (post.TimeToFirstEventMs == nil) {
		t.Errorf("TimeToFirstEventMs nil-ness drift: pre=%v post=%v", pre.TimeToFirstEventMs, post.TimeToFirstEventMs)
	} else if pre.TimeToFirstEventMs != nil && *pre.TimeToFirstEventMs != *post.TimeToFirstEventMs {
		t.Errorf("TimeToFirstEventMs drift: pre=%d post=%d", *pre.TimeToFirstEventMs, *post.TimeToFirstEventMs)
	}

	// Calling WriteSpawnOutcome a second time must remain idempotent.
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome (second call): %v", err)
	}
	post2, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID (after second write): %v", err)
	}
	if post2 == nil || post2.TokensInputTotal != post.TokensInputTotal {
		t.Errorf("idempotent second write changed aggregates: %+v", post2)
	}
}

// openComputeTestDB is a thin wrapper to keep these tests self-documenting.
func openComputeTestDB(t *testing.T) *db.DB {
	t.Helper()
	return openTestDB(t)
}
