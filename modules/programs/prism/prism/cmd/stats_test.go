package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// --- helpers ---

// openStatsTestDB opens a temp DB, registers t.Cleanup, and sets the global
// testDBPath so that openDB() in the stats command uses this DB.
func openStatsTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	return d
}

// writeStatsEvent is a shorthand for writing events in stats tests.
func writeStatsEvent(t *testing.T, d *db.DB, session, typ, payload string, ts time.Time) {
	t.Helper()
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// writeStatsEventWithSID writes an event with an explicit opencode_sid.
func writeStatsEventWithSID(t *testing.T, d *db.DB, session, sid, typ, payload string, ts time.Time) {
	t.Helper()
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		OpencodeSID: &sid,
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

func assistantPayloadWithTokens(msgID, text string, inputTokens, outputTokens, cacheRead, cacheWrite int, durationMs int64, contextPct float64) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q,"agent":"opencode","model":"anthropic/claude-sonnet-4-6","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"durationMs":%d,"contextWindowPct":%f}`,
		msgID, text, inputTokens, outputTokens, cacheRead, cacheWrite, durationMs, contextPct)
}

func toolCallPayloadWithDuration(msgID, tool, args string, durationMs int64) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q,"args":%q,"durationMs":%d}`,
		msgID, tool, args, durationMs)
}

// assistantPayloadWithAgentModel creates an assistant payload with explicit agent and model fields.
func assistantPayloadWithAgentModel(msgID, agent, model string, inputTokens, outputTokens int) string {
	return fmt.Sprintf(`{"messageId":%q,"text":"reply","agent":%q,"model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":5000}`,
		msgID, agent, model, inputTokens, outputTokens)
}

// --- per-session detail tests ---

// TestRunStatsSession_TokenTotals verifies that token counts are summed
// across all assistant turns.
func TestRunStatsSession_TokenTotals(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Two assistant turns with token data.
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply 1", 1000, 500, 200, 100, 5000, 25.5),
		base)
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-2", "reply 2", 2000, 800, 300, 150, 8000, 35.2),
		base.Add(10*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Verify token totals are displayed.
	if !strings.Contains(out, "3.0K") { // 3000 input tokens
		t.Errorf("output missing input token total '3.0K'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "1.3K") { // 1300 output tokens
		t.Errorf("output missing output token total '1.3K'\ngot:\n%s", out)
	}
}

// TestRunStatsSession_CostEstimate verifies cost is calculated from token counts.
func TestRunStatsSession_CostEstimate(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// 100K input tokens at $3/M = $0.30, 10K output at $15/M = $0.15 → ~$0.45
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 100000, 10000, 0, 0, 5000, 50.0),
		base)

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "~$0.45") {
		t.Errorf("output missing cost estimate '~$0.45'\ngot:\n%s", out)
	}
}

// TestRunStatsSession_TurnDurations verifies turn timing is displayed.
func TestRunStatsSession_TurnDurations(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Two turns: 5s and 15s durations.
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply 1", 1000, 500, 0, 0, 5000, 0),
		base)
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-2", "reply 2", 1000, 500, 0, 0, 15000, 0),
		base.Add(20*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Average: 10s, Longest: 15s.
	if !strings.Contains(out, "avg turn:") {
		t.Errorf("output missing 'avg turn:'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "longest:") {
		t.Errorf("output missing 'longest:'\ngot:\n%s", out)
	}
}

// TestRunStatsSession_ToolBreakdown verifies tool call counts and durations.
func TestRunStatsSession_ToolBreakdown(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeStatsEvent(t, d, session, "tool_call",
		toolCallPayloadWithDuration("msg-1", "bash", "echo hello", 2000),
		base)
	writeStatsEvent(t, d, session, "tool_call",
		toolCallPayloadWithDuration("msg-1", "bash", "ls", 1500),
		base.Add(time.Second))
	writeStatsEvent(t, d, session, "tool_call",
		toolCallPayloadWithDuration("msg-1", "read_file", "main.go", 500),
		base.Add(2*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// bash should appear with count 2.
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash' tool\ngot:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("output missing 'read_file' tool\ngot:\n%s", out)
	}
}

// TestRunStatsSession_PeakContext verifies peak context window display.
func TestRunStatsSession_PeakContext(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 1000, 500, 0, 0, 5000, 45.3),
		base)
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-2", "reply", 1000, 500, 0, 0, 5000, 72.8),
		base.Add(10*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "72.8%") {
		t.Errorf("output missing peak context '72.8%%'\ngot:\n%s", out)
	}
}

// TestRunStatsSession_Compactions verifies compaction count display.
func TestRunStatsSession_Compactions(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 1000, 500, 0, 0, 5000, 0),
		base)
	writeStatsEvent(t, d, session, "compaction", `{"note":"compaction started"}`, base.Add(5*time.Second))
	writeStatsEvent(t, d, session, "compaction", `{"note":"compaction complete"}`, base.Add(10*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "compactions:") {
		t.Errorf("output missing 'compactions:'\ngot:\n%s", out)
	}
}

// TestRunStatsSession_NoData verifies graceful handling of empty sessions.
func TestRunStatsSession_NoData(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "no metrics data") {
		t.Errorf("output missing 'no metrics data' for empty session\ngot:\n%s", out)
	}
}

// TestRunStatsSession_NotFound verifies error on unknown session.
func TestRunStatsSession_NotFound(t *testing.T) {
	_ = openStatsTestDB(t)

	err := runStatsSession("nonexistent@main", false)
	if err == nil {
		t.Error("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found'\ngot: %v", err)
	}
}

// TestRunStatsSession_MultipleOpencodeSessions verifies that a tmux session
// with multiple opencode sessions renders a compact table.
func TestRunStatsSession_MultipleOpencodeSessions(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	sid1 := "ses_aaa111"
	sid2 := "ses_bbb222"

	// Session 1: coordinator on sonnet.
	writeStatsEventWithSID(t, d, session, sid1, "msg_assistant",
		assistantPayloadWithAgentModel("msg-1", "coordinator", "anthropic/claude-sonnet-4-6", 1000, 500),
		base)
	writeStatsEventWithSID(t, d, session, sid1, "msg_assistant",
		assistantPayloadWithAgentModel("msg-2", "coordinator", "anthropic/claude-sonnet-4-6", 2000, 800),
		base.Add(10*time.Second))

	// Session 2: coordinator on opus.
	writeStatsEventWithSID(t, d, session, sid2, "msg_assistant",
		assistantPayloadWithAgentModel("msg-3", "coordinator", "anthropic/claude-opus-4-6", 5000, 2000),
		base.Add(1*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Should show compact table with summary row.
	if !strings.Contains(out, "summary:") {
		t.Errorf("output missing 'summary:' line for multi-session\ngot:\n%s", out)
	}
	// Should show both models.
	if !strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("output missing 'claude-sonnet-4-6'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "claude-opus-4-6") {
		t.Errorf("output missing 'claude-opus-4-6'\ngot:\n%s", out)
	}
	// Should show compact table headers.
	if !strings.Contains(out, "STARTED") {
		t.Errorf("output missing 'STARTED' column header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "DURATION") {
		t.Errorf("output missing 'DURATION' column header\ngot:\n%s", out)
	}
}

// TestRunStatsSession_SingleSessionUnchanged verifies that a tmux session with
// exactly one opencode session renders the pre-existing detailed block format.
func TestRunStatsSession_SingleSessionUnchanged(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@feature"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/feature", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	sid := "ses_single123"
	writeStatsEventWithSID(t, d, session, sid, "msg_assistant",
		assistantPayloadWithAgentModel("msg-1", "coordinator", "anthropic/claude-sonnet-4-6", 1000, 500),
		base)

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Should show detailed block, not compact table.
	if strings.Contains(out, "summary:") {
		t.Errorf("single session should NOT show 'summary:' compact table header\ngot:\n%s", out)
	}
	// Should show detailed block headers.
	if !strings.Contains(out, "Token Usage") {
		t.Errorf("output missing 'Token Usage' for single session detailed block\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Turns") {
		t.Errorf("output missing 'Turns' for single session detailed block\ngot:\n%s", out)
	}
}

// TestRunStatsSession_NullSidLegacySentinel verifies that events with NULL
// opencode_sid are grouped under the legacy sentinel and displayed as "(legacy)".
func TestRunStatsSession_NullSidLegacySentinel(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Write events with NULL opencode_sid (no SID — uses writeStatsEvent which doesn't set SID).
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"msg-old","text":"old reply","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":500,"outputTokens":200,"durationMs":3000}`,
		base)
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"msg-old2","text":"old reply 2","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":400,"outputTokens":150,"durationMs":2000}`,
		base.Add(10*time.Second))

	// Also write events with a real SID so we have 2 groups.
	sid := "ses_real456"
	writeStatsEventWithSID(t, d, session, sid, "msg_assistant",
		assistantPayloadWithAgentModel("msg-new", "coordinator", "anthropic/claude-sonnet-4-6", 1000, 500),
		base.Add(1*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Should show compact table (2 groups: legacy + real session).
	if !strings.Contains(out, "summary:") {
		t.Errorf("output missing 'summary:' for multi-group session\ngot:\n%s", out)
	}
	// Legacy row should be labelled.
	if !strings.Contains(out, "legacy") {
		t.Errorf("output missing 'legacy' label for NULL-sid group\ngot:\n%s", out)
	}
}

// TestRunStatsSession_AllNullSidSingleLegacy verifies that a session where ALL
// events have NULL opencode_sid renders as a detailed block labelled "(legacy)".
func TestRunStatsSession_AllNullSidSingleLegacy(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@old"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/old", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// All events have NULL opencode_sid.
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"msg-1","text":"reply","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":500,"outputTokens":200,"durationMs":3000}`,
		base)

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// All-legacy session: should render as detailed block (not compact table).
	if strings.Contains(out, "STARTED") {
		t.Errorf("all-legacy session should use detailed block, not compact table\ngot:\n%s", out)
	}
	// Should show legacy label.
	if !strings.Contains(out, "legacy") {
		t.Errorf("all-legacy session should show 'legacy' label\ngot:\n%s", out)
	}
	// Should show Token Usage block (detailed format).
	if !strings.Contains(out, "Token Usage") {
		t.Errorf("all-legacy session should show 'Token Usage' in detailed block\ngot:\n%s", out)
	}
}

// TestRunStatsSession_DetailFlag verifies that --detail forces detailed block
// format even for multi-session tmux sessions.
func TestRunStatsSession_DetailFlag(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	sid1 := "ses_aaa111"
	sid2 := "ses_bbb222"

	writeStatsEventWithSID(t, d, session, sid1, "msg_assistant",
		assistantPayloadWithAgentModel("msg-1", "coordinator", "anthropic/claude-sonnet-4-6", 1000, 500),
		base)
	writeStatsEventWithSID(t, d, session, sid2, "msg_assistant",
		assistantPayloadWithAgentModel("msg-2", "coordinator", "anthropic/claude-opus-4-6", 5000, 2000),
		base.Add(1*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, true); err != nil { // detail=true
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// With --detail, should show detailed block format for each session.
	if !strings.Contains(out, "Token Usage") {
		t.Errorf("--detail flag should show 'Token Usage' in block format\ngot:\n%s", out)
	}
	// Should still show the summary line at top.
	if !strings.Contains(out, "summary:") {
		t.Errorf("--detail flag should still show 'summary:' header\ngot:\n%s", out)
	}
}

// TestGroupEventsByOpencodeSID verifies event grouping by opencode_sid.
func TestGroupEventsByOpencodeSID(t *testing.T) {
	sid1 := "ses_aaa"
	sid2 := "ses_bbb"

	events := []db.Event{
		{ID: "1", SessionName: "s", Type: "msg_assistant", OpencodeSID: &sid1, Payload: `{}`, CreatedAt: time.Now()},
		{ID: "2", SessionName: "s", Type: "msg_assistant", OpencodeSID: &sid2, Payload: `{}`, CreatedAt: time.Now()},
		{ID: "3", SessionName: "s", Type: "msg_assistant", OpencodeSID: nil, Payload: `{}`, CreatedAt: time.Now()},   // NULL → sentinel
		{ID: "4", SessionName: "s", Type: "msg_assistant", OpencodeSID: &sid1, Payload: `{}`, CreatedAt: time.Now()}, // same as id=1
	}

	grouped, order := groupEventsByOpencodeSID(events)

	if len(order) != 3 {
		t.Errorf("expected 3 groups (sid1, sid2, sentinel), got %d: %v", len(order), order)
	}
	if len(grouped[sid1]) != 2 {
		t.Errorf("sid1 group should have 2 events, got %d", len(grouped[sid1]))
	}
	if len(grouped[sid2]) != 1 {
		t.Errorf("sid2 group should have 1 event, got %d", len(grouped[sid2]))
	}
	if len(grouped[legacySentinel]) != 1 {
		t.Errorf("legacy sentinel group should have 1 event, got %d", len(grouped[legacySentinel]))
	}
	// Order should be: sid1 first (first seen), then sid2, then sentinel.
	if order[0] != sid1 {
		t.Errorf("first group should be %q, got %q", sid1, order[0])
	}
	if order[1] != sid2 {
		t.Errorf("second group should be %q, got %q", sid2, order[1])
	}
	if order[2] != legacySentinel {
		t.Errorf("third group should be legacySentinel %q, got %q", legacySentinel, order[2])
	}
}

// TestCollectMetrics_CoordinatorModelPreferred verifies that the coordinator
// agent's model is preferred over the most-frequent model.
func TestCollectMetrics_CoordinatorModelPreferred(t *testing.T) {
	// 3 turns on opus (build/review), 1 turn on sonnet (coordinator).
	// Coordinator model should win despite being less frequent.
	events := []db.Event{
		{ID: "1", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"build","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
		{ID: "2", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"review","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
		{ID: "3", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"build","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
		{ID: "4", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
	}

	m := collectMetrics(events, "ses_abc")

	if m.ModelID != "anthropic/claude-sonnet-4-6" {
		t.Errorf("expected coordinator model 'anthropic/claude-sonnet-4-6', got %q", m.ModelID)
	}
	if m.AssistantTurns != 4 {
		t.Errorf("expected 4 assistant turns, got %d", m.AssistantTurns)
	}
}

// TestCollectMetrics_FallbackToMostFrequent verifies that when no coordinator
// turn exists, the most-frequent model is used.
func TestCollectMetrics_FallbackToMostFrequent(t *testing.T) {
	events := []db.Event{
		{ID: "1", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"build","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
		{ID: "2", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"review","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
		{ID: "3", SessionName: "s", Type: "msg_assistant", OpencodeSID: strPtr("ses_abc"),
			Payload: `{"agent":"explore","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
	}

	m := collectMetrics(events, "ses_abc")

	// opus appears 2 times, sonnet 1 time — opus should win.
	if m.ModelID != "anthropic/claude-opus-4-6" {
		t.Errorf("expected most-frequent model 'anthropic/claude-opus-4-6', got %q", m.ModelID)
	}
}

// TestCollectMetrics_NullSidLegacy verifies that the legacy sentinel is set
// correctly for NULL-sid events.
func TestCollectMetrics_NullSidLegacy(t *testing.T) {
	events := []db.Event{
		{ID: "1", SessionName: "s", Type: "msg_assistant", OpencodeSID: nil,
			Payload: `{"agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50}`, CreatedAt: time.Now()},
	}

	m := collectMetrics(events, legacySentinel)

	if !m.isLegacy() {
		t.Errorf("expected m.isLegacy() == true for legacySentinel key")
	}
	if m.OpencodeSID != legacySentinel {
		t.Errorf("OpencodeSID should be legacySentinel %q, got %q", legacySentinel, m.OpencodeSID)
	}
}

// TestRunStatsSession_ZeroEvents verifies that a zero-event tmux session renders
// without panic.
func TestRunStatsSession_ZeroEvents(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@empty"

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/empty", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "no metrics data") {
		t.Errorf("zero-event session should show 'no metrics data'\ngot:\n%s", out)
	}
}

// --- summary table tests ---

// TestRunStatsSummary_ActiveSessions verifies the summary table shows active sessions.
func TestRunStatsSummary_ActiveSessions(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two active sessions.
	if err := d.UpsertStatus("testrepo@main", "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("testrepo@feature", "testrepo", "/code/testrepo/feature", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 5000, 2000, 0, 0, 5000, 0),
		base)

	out := captureStdout(t, func() {
		if err := runStatsSummary(); err != nil {
			t.Errorf("runStatsSummary: %v", err)
		}
	})

	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("output missing 'testrepo@main'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "testrepo@feature") {
		t.Errorf("output missing 'testrepo@feature'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "SESSION") {
		t.Errorf("output missing table header 'SESSION'\ngot:\n%s", out)
	}
}

// TestRunStatsSummary_NoSessions verifies graceful output when no sessions exist.
func TestRunStatsSummary_NoSessions(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsSummary(); err != nil {
			t.Errorf("runStatsSummary: %v", err)
		}
	})

	if !strings.Contains(out, "no active sessions") {
		t.Errorf("output missing 'no active sessions'\ngot:\n%s", out)
	}
}

// --- historical aggregate tests ---

// TestRunStatsHistorical_Aggregate verifies the --days aggregate output.
func TestRunStatsHistorical_Aggregate(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus("testrepo@main", "testrepo", "/code/testrepo/main", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("testrepo@feature", "testrepo", "/code/testrepo/feature", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Session 1: 100K input @ $3/M = $0.30
	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 100000, 0, 0, 0, 5000, 0),
		base.Add(-1*time.Hour))
	writeStatsEvent(t, d, "testrepo@main", "tool_call",
		toolCallPayloadWithDuration("msg-1", "bash", "echo", 1000),
		base.Add(-50*time.Minute))

	// Session 2.
	writeStatsEvent(t, d, "testrepo@feature", "msg_assistant",
		assistantPayloadWithTokens("msg-2", "reply", 50000, 5000, 0, 0, 3000, 0),
		base.Add(-30*time.Minute))
	writeStatsEvent(t, d, "testrepo@feature", "tool_call",
		toolCallPayloadWithDuration("msg-2", "read_file", "main.go", 500),
		base.Add(-25*time.Minute))

	out := captureStdout(t, func() {
		if err := runStatsHistorical(7); err != nil {
			t.Errorf("runStatsHistorical: %v", err)
		}
	})

	if !strings.Contains(out, "sessions:") {
		t.Errorf("output missing 'sessions:'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "2") { // 2 sessions
		t.Errorf("output missing session count '2'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "total cost:") {
		t.Errorf("output missing 'total cost:'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash' in tool breakdown\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Cost by Session") {
		t.Errorf("output missing 'Cost by Session' section\ngot:\n%s", out)
	}
}

// TestRunStatsHistorical_NoEvents verifies graceful output when no events exist.
func TestRunStatsHistorical_NoEvents(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsHistorical(7); err != nil {
			t.Errorf("runStatsHistorical: %v", err)
		}
	})

	if !strings.Contains(out, "no events") {
		t.Errorf("output missing 'no events'\ngot:\n%s", out)
	}
}

// --- formatting tests ---

func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{9999, "10.0K"},
		{10000, "10K"},
		{45000, "45K"},
		{999999, "999K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
	}
	for _, tc := range cases {
		got := formatTokenCount(tc.n)
		if got != tc.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	cases := []struct {
		cost float64
		want string
	}{
		{0, "<$0.01"},
		{0.005, "<$0.01"},
		{0.01, "~$0.01"},
		{0.42, "~$0.42"},
		{1.0, "~$1.00"},
		{12.345, "~$12.35"},
	}
	for _, tc := range cases {
		got := formatCost(tc.cost)
		if got != tc.want {
			t.Errorf("formatCost(%f) = %q, want %q", tc.cost, got, tc.want)
		}
	}
}

func TestFormatDurationLong(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "—"},
		{500 * time.Millisecond, "<1s"},
		{time.Second, "1s"},
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m 30s"},
		{3600 * time.Second, "1h 0m"},
		{3661 * time.Second, "1h 1m"},
	}
	for _, tc := range cases {
		got := formatDurationLong(tc.d)
		if got != tc.want {
			t.Errorf("formatDurationLong(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRunStatsSession_SubagentInvocations verifies subagent invocation display.
func TestRunStatsSession_SubagentInvocations(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Add a msg_assistant to make the session non-empty.
	writeStatsEvent(t, d, session, "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 1000, 500, 0, 0, 5000, 0),
		base)

	// Two subagent invocations.
	writeStatsEvent(t, d, session, "subagent_start",
		`{"agent":"review","description":"Review PR","messageId":"msg-sub1"}`,
		base.Add(5*time.Second))
	writeStatsEvent(t, d, session, "subagent_end",
		`{"agent":"review","durationMs":30000,"messageId":"msg-sub1"}`,
		base.Add(35*time.Second))

	writeStatsEvent(t, d, session, "subagent_start",
		`{"agent":"explore","description":"Search code","messageId":"msg-sub2"}`,
		base.Add(40*time.Second))
	writeStatsEvent(t, d, session, "subagent_end",
		`{"agent":"explore","durationMs":10000,"messageId":"msg-sub2"}`,
		base.Add(50*time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	if !strings.Contains(out, "Subagent Invocations") {
		t.Errorf("output missing 'Subagent Invocations' section\ngot:\n%s", out)
	}
	if !strings.Contains(out, "review") {
		t.Errorf("output missing 'review' subagent\ngot:\n%s", out)
	}
	if !strings.Contains(out, "explore") {
		t.Errorf("output missing 'explore' subagent\ngot:\n%s", out)
	}
}

// TestRunStats_DaysMutuallyExclusive verifies that --days is mutually exclusive
// with a session name argument.
func TestRunStats_DaysMutuallyExclusive(t *testing.T) {
	_ = openStatsTestDB(t)            // needed so openDB() works in runStats
	statsCmd.Flags().Set("days", "7") //nolint:errcheck
	defer statsCmd.Flags().Set("days", "0")

	// --days + session arg should error.
	err := runStats(statsCmd, []string{"testrepo@main"})
	if err == nil {
		t.Fatal("expected error when --days and session arg are both provided, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error for --days+session, got: %v", err)
	}
}

// TestRunStats_AllFlagRemoved verifies that --all is no longer a registered flag
// on statsCmd (passing it should result in an error/unknown flag).
func TestRunStats_AllFlagRemoved(t *testing.T) {
	f := statsCmd.Flags().Lookup("all")
	if f != nil {
		t.Error("statsCmd should NOT have an --all flag (it was removed), but it is still registered")
	}
}

// TestRunStatsModel_AllFlagRemoved verifies that --all is no longer a registered
// flag on modelCmd.
func TestRunStatsModel_AllFlagRemoved(t *testing.T) {
	f := modelCmd.Flags().Lookup("all")
	if f != nil {
		t.Error("modelCmd should NOT have an --all flag (it was removed), but it is still registered")
	}
}

// TestCollectMetrics_PreEnrichmentEvents verifies that events without token
// data (pre-enrichment) result in zero values rather than errors.
func TestCollectMetrics_PreEnrichmentEvents(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	// Old-format assistant event — no token fields.
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"msg-old","text":"old reply","agent":"opencode","model":"anthropic/claude-sonnet-4-20250514"}`,
		base)
	writeStatsEvent(t, d, session, "tool_call",
		`{"messageId":"msg-old","tool":"bash","args":"echo hi"}`,
		base.Add(time.Second))

	events, err := d.AllSessionEvents(session)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}

	m := collectMetrics(events, legacySentinel)

	if m.AssistantTurns != 1 {
		t.Errorf("AssistantTurns = %d, want 1", m.AssistantTurns)
	}
	if m.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 (pre-enrichment)", m.InputTokens)
	}
	if m.totalToolCalls() != 1 {
		t.Errorf("totalToolCalls = %d, want 1", m.totalToolCalls())
	}
	if len(m.TurnDurations) != 0 {
		t.Errorf("TurnDurations should be empty for pre-enrichment events, got %d", len(m.TurnDurations))
	}
}

// --- model breakdown tests ---

// assistantPayloadWithModel creates an assistant payload with a specific model.
func assistantPayloadWithModel(model string, inputTokens, outputTokens int, durationMs int64) string {
	// Use a UUID-like ID based on the model and token values to ensure uniqueness.
	msgID := fmt.Sprintf("%s-%d-%d-%d", model, inputTokens, outputTokens, durationMs)
	return fmt.Sprintf(`{"messageId":%q,"text":"reply","agent":"opencode","model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":%d}`,
		msgID, model, inputTokens, outputTokens, durationMs)
}

// TestRunStatsModel_BasicOutput verifies that the model breakdown produces a
// table with expected columns and rows.
func TestRunStatsModel_BasicOutput(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two turns for github-copilot/claude-sonnet-4.6 in session A.
	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithModel("github-copilot/claude-sonnet-4.6", 10000, 3000, 22000),
		base.Add(-1*time.Hour))
	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithModel("github-copilot/claude-sonnet-4.6", 5000, 1500, 18000),
		base.Add(-50*time.Minute))

	// One turn for anthropic/claude-sonnet-4-6 in session B.
	writeStatsEvent(t, d, "testrepo@feature", "msg_assistant",
		assistantPayloadWithModel("anthropic/claude-sonnet-4-6", 8000, 2000, 18000),
		base.Add(-30*time.Minute))

	events, err := d.EventsSince(base.Add(-2 * time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}

	metrics := collectModelMetrics(events)

	// Should have two distinct providers.
	if len(metrics) != 2 {
		t.Errorf("expected 2 model entries, got %d", len(metrics))
	}

	ghEntry, ok := metrics["github-copilot/claude-sonnet-4.6"]
	if !ok {
		t.Fatal("missing github-copilot/claude-sonnet-4.6 entry")
	}
	if ghEntry.Turns != 2 {
		t.Errorf("github-copilot turns = %d, want 2", ghEntry.Turns)
	}
	if ghEntry.InputTokens != 15000 {
		t.Errorf("github-copilot input tokens = %d, want 15000", ghEntry.InputTokens)
	}
	if ghEntry.OutputTokens != 4500 {
		t.Errorf("github-copilot output tokens = %d, want 4500", ghEntry.OutputTokens)
	}
	if ghEntry.Provider != "github-copilot" {
		t.Errorf("github-copilot provider = %q, want %q", ghEntry.Provider, "github-copilot")
	}
	if ghEntry.Model != "claude-sonnet-4.6" {
		t.Errorf("github-copilot model = %q, want %q", ghEntry.Model, "claude-sonnet-4.6")
	}

	// Verify render doesn't panic and contains expected headers.
	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	for _, want := range []string{"PROVIDER", "MODEL", "TURNS", "TTFT p50", "DUR p50", "TOK/S p50", "INPUT", "OUTPUT", "COST", "SESSIONS", "AGENTS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing column header %q\ngot:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "github-copilot") {
		t.Errorf("output missing 'github-copilot'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "anthropic") {
		t.Errorf("output missing 'anthropic'\ngot:\n%s", out)
	}
}

// TestRunStatsModel_SessionCountByOpencodeSID verifies that sessions are counted
// by opencode_sid (not session_name) so that multi-session tmux sessions are
// counted correctly.
func TestRunStatsModel_SessionCountByOpencodeSID(t *testing.T) {
	sid1 := "ses_opencode_aaa"
	sid2 := "ses_opencode_bbb"
	sid3 := "ses_opencode_ccc"

	// Three distinct opencode sessions all within the same tmux session "repo@main",
	// all using the same model.
	events := []db.Event{
		{ID: "1", SessionName: "repo@main", Type: "msg_assistant", OpencodeSID: &sid1,
			Payload:   `{"messageId":"m1","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
		{ID: "2", SessionName: "repo@main", Type: "msg_assistant", OpencodeSID: &sid2,
			Payload:   `{"messageId":"m2","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
		{ID: "3", SessionName: "repo@main", Type: "msg_assistant", OpencodeSID: &sid3,
			Payload:   `{"messageId":"m3","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
	}

	metrics := collectModelMetrics(events)
	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing entry for anthropic/claude-sonnet-4-6")
	}

	// Should count 3 sessions (one per opencode_sid), NOT 1 (session_name).
	if len(entry.Sessions) != 3 {
		t.Errorf("expected 3 distinct opencode sessions, got %d", len(entry.Sessions))
	}
}

// TestRunStatsModel_NullSidFallbackSessionName verifies that NULL opencode_sid
// events fall back to counting by session_name as a legacy bucket.
func TestRunStatsModel_NullSidFallbackSessionName(t *testing.T) {
	// Two different sessions, both with NULL opencode_sid, same model.
	events := []db.Event{
		{ID: "1", SessionName: "repo@main", Type: "msg_assistant", OpencodeSID: nil,
			Payload:   `{"messageId":"m1","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50}`,
			CreatedAt: time.Now()},
		{ID: "2", SessionName: "repo@feature", Type: "msg_assistant", OpencodeSID: nil,
			Payload:   `{"messageId":"m2","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50}`,
			CreatedAt: time.Now()},
		// Same session_name as first but different event — should NOT add another session count.
		{ID: "3", SessionName: "repo@main", Type: "msg_assistant", OpencodeSID: nil,
			Payload:   `{"messageId":"m3","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50}`,
			CreatedAt: time.Now()},
	}

	metrics := collectModelMetrics(events)
	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing entry for anthropic/claude-sonnet-4-6")
	}

	// Should count 2 distinct sessions (repo@main and repo@feature), not 3.
	if len(entry.Sessions) != 2 {
		t.Errorf("expected 2 distinct legacy sessions, got %d: %v", len(entry.Sessions), entry.Sessions)
	}
}

// TestRunStatsModel_NoData verifies graceful output when no session data exists.
func TestRunStatsModel_NoData(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsModel(modelCmd, nil); err != nil {
			t.Errorf("runStatsModel: %v", err)
		}
	})

	if !strings.Contains(out, "no model data") {
		t.Errorf("output missing 'no model data' for empty DB\ngot:\n%s", out)
	}
}

// TestRunStatsModel_DaysZeroError verifies that --days 0 returns an error.
func TestRunStatsModel_DaysZeroError(t *testing.T) {
	modelCmd.Flags().Set("days", "0") //nolint:errcheck
	defer modelCmd.Flags().Set("days", "7")

	err := runStatsModel(modelCmd, nil)
	if err == nil {
		t.Fatal("expected error for --days 0, got nil")
	}
	if !strings.Contains(err.Error(), "--days must be greater than 0") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestRunStatsModel_ZeroDurationExcluded verifies that turns with durationMs==0
// are excluded from latency/throughput P50 but still count towards token totals.
func TestRunStatsModel_ZeroDurationExcluded(t *testing.T) {
	events := []fakeEvent{
		// Turn 1: has duration.
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 10000},
		// Turn 2: no duration (zero).
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 2000, outputTokens: 800, durationMs: 0},
	}

	dbEvents := fakeEventsToDBEvents(events)
	metrics := collectModelMetrics(dbEvents)

	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing anthropic/claude-sonnet-4-6 entry")
	}

	// Both turns count towards token totals.
	if entry.Turns != 2 {
		t.Errorf("Turns = %d, want 2", entry.Turns)
	}
	if entry.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", entry.InputTokens)
	}
	if entry.OutputTokens != 1300 {
		t.Errorf("OutputTokens = %d, want 1300", entry.OutputTokens)
	}

	// Only the non-zero duration turn contributes to latency/throughput.
	if len(entry.DurationsMs) != 1 {
		t.Errorf("DurationsMs has %d entries, want 1 (zero excluded)", len(entry.DurationsMs))
	}
	if len(entry.TokPerSec) != 1 {
		t.Errorf("TokPerSec has %d entries, want 1 (zero excluded)", len(entry.TokPerSec))
	}
}

// TestRunStatsModel_ZeroDurationOnlyModel verifies that a model with only
// zero-duration turns shows "-" for latency and throughput, not 0 or an error.
func TestRunStatsModel_ZeroDurationOnlyModel(t *testing.T) {
	events := []fakeEvent{
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 0},
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 2000, outputTokens: 800, durationMs: 0},
	}

	dbEvents := fakeEventsToDBEvents(events)
	metrics := collectModelMetrics(dbEvents)

	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	// The LAT p50 and TOK/S p50 columns should show "-" not "0s" or "0 t/s".
	if strings.Contains(out, "0s") {
		t.Errorf("output should not contain '0s' for zero-duration-only model\ngot:\n%s", out)
	}
	if strings.Contains(out, "0 t/s") {
		t.Errorf("output should not contain '0 t/s' for zero-duration-only model\ngot:\n%s", out)
	}
}

// TestPercentileFloat64 verifies the P50 percentile helper.
func TestPercentileFloat64(t *testing.T) {
	cases := []struct {
		vals []float64
		p    int
		want float64
	}{
		{nil, 50, 0},
		{[]float64{10}, 50, 10},
		{[]float64{1, 2, 3, 4, 5}, 50, 3},
		{[]float64{5, 3, 1, 4, 2}, 50, 3}, // unsorted input
		{[]float64{1, 2}, 50, 1},
		{[]float64{1, 2, 3, 4}, 50, 2},
		{[]float64{1, 2, 3, 4, 5, 6}, 50, 3},
	}
	for _, tc := range cases {
		got := percentileFloat64(tc.vals, tc.p)
		if got != tc.want {
			t.Errorf("percentileFloat64(%v, %d) = %v, want %v", tc.vals, tc.p, got, tc.want)
		}
	}
}

// TestFormatLatency verifies the latency formatting helper.
func TestFormatLatency(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{0, "0s"},
		{1000, "1s"},
		{22000, "22s"},
		{59999, "59s"},
		{60000, "1m 0s"},
		{90000, "1m 30s"},
		{124000, "2m 4s"},
	}
	for _, tc := range cases {
		got := formatLatency(tc.ms)
		if got != tc.want {
			t.Errorf("formatLatency(%v) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// TestSplitModel verifies provider/model splitting.
func TestSplitModel(t *testing.T) {
	cases := []struct {
		input    string
		provider string
		model    string
	}{
		{"github-copilot/claude-sonnet-4.6", "github-copilot", "claude-sonnet-4.6"},
		{"anthropic/claude-sonnet-4-6", "anthropic", "claude-sonnet-4-6"},
		{"no-slash", "", "no-slash"},
		{"google/gemini-3-flash-preview", "google", "gemini-3-flash-preview"},
	}
	for _, tc := range cases {
		gotProvider, gotModel := splitModel(tc.input)
		if gotProvider != tc.provider || gotModel != tc.model {
			t.Errorf("splitModel(%q) = (%q, %q), want (%q, %q)",
				tc.input, gotProvider, gotModel, tc.provider, tc.model)
		}
	}
}

// TestRunStatsModel_SortedByCostDescending verifies rows are sorted by cost descending.
func TestRunStatsModel_SortedByCostDescending(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// anthropic/claude-sonnet-4-6: 100K input @ $3/M = $0.30 + 5K output @ $15/M = $0.075 → ~$0.375
	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithModel("anthropic/claude-sonnet-4-6", 100000, 5000, 5000),
		base.Add(-1*time.Hour))

	// google/gemini-3-flash-preview: 1K input @ $0.15/M → very cheap
	writeStatsEvent(t, d, "testrepo@feature", "msg_assistant",
		assistantPayloadWithModel("google/gemini-3-flash-preview", 1000, 100, 5000),
		base.Add(-30*time.Minute))

	events, err := d.EventsSince(base.Add(-2 * time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}

	metrics := collectModelMetrics(events)
	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	// anthropic should appear before google (higher cost).
	anthropicPos := strings.Index(out, "anthropic")
	googlePos := strings.Index(out, "google")
	if anthropicPos == -1 || googlePos == -1 {
		t.Fatalf("output missing expected model rows\ngot:\n%s", out)
	}
	if anthropicPos > googlePos {
		t.Errorf("expected 'anthropic' to appear before 'google' (sorted by cost desc)\ngot:\n%s", out)
	}
}

// TestRunStatsModel_LatencyNote verifies the footer note about latency measurement.
func TestRunStatsModel_LatencyNote(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeStatsEvent(t, d, "testrepo@main", "msg_assistant",
		assistantPayloadWithModel("anthropic/claude-sonnet-4-6", 1000, 500, 5000),
		base.Add(-1*time.Hour))

	events, err := d.EventsSince(base.Add(-2 * time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}

	metrics := collectModelMetrics(events)
	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	if !strings.Contains(out, "turn duration") && !strings.Contains(out, "DUR p50") {
		t.Errorf("output should contain latency note\ngot:\n%s", out)
	}
}

// TestRunStatsSummary_ShowsAllRepos verifies that the summary table shows all
// repos by default (no longer scoped to current repo).
func TestRunStatsSummary_ShowsAllRepos(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two active sessions in different repos.
	if err := d.UpsertStatus("repo-a@main", "repo-a", "/code/repo-a/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("repo-b@main", "repo-b", "/code/repo-b/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeStatsEvent(t, d, "repo-a@main", "msg_assistant",
		assistantPayloadWithTokens("msg-1", "reply", 1000, 500, 0, 0, 5000, 0),
		base)
	writeStatsEvent(t, d, "repo-b@main", "msg_assistant",
		assistantPayloadWithTokens("msg-2", "reply", 2000, 800, 0, 0, 5000, 0),
		base.Add(time.Second))

	out := captureStdout(t, func() {
		if err := runStatsSummary(); err != nil {
			t.Errorf("runStatsSummary: %v", err)
		}
	})

	if !strings.Contains(out, "repo-a@main") {
		t.Errorf("output missing 'repo-a@main' — should show all repos\ngot:\n%s", out)
	}
	if !strings.Contains(out, "repo-b@main") {
		t.Errorf("output missing 'repo-b@main' — should show all repos\ngot:\n%s", out)
	}
}

// TestRunStatsSummary_MultiModelCost verifies that the summary table sums cost
// per opencode session (each with its own model pricing) rather than applying
// a single live model's rate to all accumulated tokens. This is critical for
// long-lived tmux sessions that span multiple model changes.
func TestRunStatsSummary_MultiModelCost(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)
	const session = "testrepo@main"

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	sid1 := "ses_cheap111"
	sid2 := "ses_expensive222"

	// Session 1: haiku — 100K input @ $0.80/M = $0.08
	writeStatsEventWithSID(t, d, session, sid1, "msg_assistant",
		`{"messageId":"m1","text":"r","agent":"coordinator","model":"anthropic/claude-haiku-4-5","inputTokens":100000,"outputTokens":0,"durationMs":5000}`,
		base)

	// Session 2: opus — 100K input @ $15.00/M = $1.50
	writeStatsEventWithSID(t, d, session, sid2, "msg_assistant",
		`{"messageId":"m2","text":"r","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":100000,"outputTokens":0,"durationMs":5000}`,
		base.Add(1*time.Hour))

	// Expected: $0.08 (haiku) + $1.50 (opus) = ~$1.58
	// Wrong (single-model bug): applying opus rate to all 200K tokens = $3.00
	out := captureStdout(t, func() {
		if err := runStatsSummary(); err != nil {
			t.Errorf("runStatsSummary: %v", err)
		}
	})

	// The cost shown should be ~$1.58, not ~$3.00.
	if !strings.Contains(out, "~$1.58") {
		t.Errorf("summary cost should be ~$1.58 (per-session pricing), got:\n%s", out)
	}
	if strings.Contains(out, "~$3.00") {
		t.Errorf("summary cost must NOT be ~$3.00 (single-model-all-tokens bug)\ngot:\n%s", out)
	}
}

// TestRenderSessionCompactTable_LegacyLabelFits verifies that the legacy row
// label in the compact table fits within the STARTED column without truncation,
// and never produces the malformed "(legacy, pre-sidecar" artefact.
func TestRenderSessionCompactTable_LegacyLabelFits(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Legacy events (NULL sid).
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"old1","text":"r","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50,"durationMs":3000}`,
		base)

	// Real session alongside legacy — triggers compact table with legacy row.
	sid := "ses_real999"
	writeStatsEventWithSID(t, d, session, sid, "msg_assistant",
		assistantPayloadWithAgentModel("new1", "coordinator", "anthropic/claude-sonnet-4-6", 500, 200),
		base.Add(1*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// The compact table must show "(legacy)" — the full word with closing paren.
	if !strings.Contains(out, "(legacy)") {
		t.Errorf("compact table should contain '(legacy)' label\ngot:\n%s", out)
	}
	// Must NOT contain the truncated artefact "(legacy, pre-sidecar" (missing close paren).
	if strings.Contains(out, "(legacy, pre-sidecar") {
		t.Errorf("compact table must NOT contain truncated '(legacy, pre-sidecar' artefact\ngot:\n%s", out)
	}
}

// TestRenderSessionCompactTable_SummaryExcludesLegacy verifies that the summary
// line counts only real opencode sessions, not the legacy sentinel group, and
// appends "(+ legacy events)" when legacy data is present.
func TestRenderSessionCompactTable_SummaryExcludesLegacy(t *testing.T) {
	d := openStatsTestDB(t)
	const session = "testrepo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "testrepo", "/code/testrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Legacy events (NULL sid) — should NOT count as an opencode session.
	writeStatsEvent(t, d, session, "msg_assistant",
		`{"messageId":"old1","text":"r","agent":"coordinator","model":"anthropic/claude-opus-4-6","inputTokens":100,"outputTokens":50,"durationMs":3000}`,
		base)

	// Two real opencode sessions.
	sid1 := "ses_aaa"
	sid2 := "ses_bbb"
	writeStatsEventWithSID(t, d, session, sid1, "msg_assistant",
		assistantPayloadWithAgentModel("new1", "coordinator", "anthropic/claude-sonnet-4-6", 500, 200),
		base.Add(1*time.Hour))
	writeStatsEventWithSID(t, d, session, sid2, "msg_assistant",
		assistantPayloadWithAgentModel("new2", "coordinator", "anthropic/claude-sonnet-4-6", 500, 200),
		base.Add(2*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsSession(session, false); err != nil {
			t.Errorf("runStatsSession: %v", err)
		}
	})

	// Summary should say "2 opencode sessions" (not "3").
	if !strings.Contains(out, "2 opencode sessions") {
		t.Errorf("summary should say '2 opencode sessions' (excluding legacy), got:\n%s", out)
	}
	// Should also mention legacy events.
	if !strings.Contains(out, "legacy events") {
		t.Errorf("summary should mention '+ legacy events' when legacy data present\ngot:\n%s", out)
	}
	// Must NOT say "3 opencode sessions".
	if strings.Contains(out, "3 opencode sessions") {
		t.Errorf("summary must NOT count legacy sentinel as an opencode session\ngot:\n%s", out)
	}
}

// TestFormatAgentSummary verifies the agent summary formatting helper.
func TestFormatAgentSummary(t *testing.T) {
	cases := []struct {
		agentCounts map[string]int
		want        string
	}{
		{nil, "—"},
		{map[string]int{}, "—"},
		{map[string]int{"coordinator": 10}, "coordinator"},
		{map[string]int{"coordinator": 10, "build": 5}, "coordinator (×2)"},
		{map[string]int{"coordinator": 5, "build": 10, "review": 3}, "build (×3)"},
	}
	for _, tc := range cases {
		got := formatAgentSummary(tc.agentCounts)
		if got != tc.want {
			t.Errorf("formatAgentSummary(%v) = %q, want %q", tc.agentCounts, got, tc.want)
		}
	}
}

// TestRenderModelBreakdown_AgentsColumn verifies that the AGENTS column appears
// in the model breakdown table with correct values.
func TestRenderModelBreakdown_AgentsColumn(t *testing.T) {
	// Two models: sonnet used by coordinator+review, haiku used only by explore.
	events := []db.Event{
		{ID: "1", SessionName: "s1", Type: "msg_assistant",
			Payload:   `{"messageId":"m1","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
		{ID: "2", SessionName: "s1", Type: "msg_assistant",
			Payload:   `{"messageId":"m2","text":"r","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
		{ID: "3", SessionName: "s1", Type: "msg_assistant",
			Payload:   `{"messageId":"m3","text":"r","agent":"review","model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
		{ID: "4", SessionName: "s1", Type: "msg_assistant",
			Payload:   `{"messageId":"m4","text":"r","agent":"explore","model":"anthropic/claude-haiku-4-5","inputTokens":100,"outputTokens":50,"durationMs":5000}`,
			CreatedAt: time.Now()},
	}

	metrics := collectModelMetrics(events)
	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	// AGENTS column header must appear.
	if !strings.Contains(out, "AGENTS") {
		t.Errorf("output missing 'AGENTS' column header\ngot:\n%s", out)
	}
	// Sonnet: coordinator dominant, 2 agent types → "coordinator (×2)"
	if !strings.Contains(out, "coordinator (×2)") {
		t.Errorf("output missing 'coordinator (×2)' for sonnet\ngot:\n%s", out)
	}
	// Haiku: only explore → "explore"
	if !strings.Contains(out, "explore") {
		t.Errorf("output missing 'explore' agent for haiku\ngot:\n%s", out)
	}
}

// --- fakeEvent helpers for unit tests that don't need a real DB ---

// fakeEvent is a minimal representation for building test events.
type fakeEvent struct {
	session      string
	model        string
	inputTokens  int
	outputTokens int
	durationMs   int64
	ttftMs       int64
}

// fakeEventsToDBEvents converts fakeEvents into db.Events for use with
// collectModelMetrics without requiring a real database.
func fakeEventsToDBEvents(fakes []fakeEvent) []db.Event {
	var events []db.Event
	for i, f := range fakes {
		payload := fmt.Sprintf(`{"messageId":"msg-%d","text":"reply","agent":"opencode","model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":%d,"ttftMs":%d}`,
			i, f.model, f.inputTokens, f.outputTokens, f.durationMs, f.ttftMs)
		events = append(events, db.Event{
			ID:          fmt.Sprintf("id-%d", i),
			SessionName: f.session,
			Type:        "msg_assistant",
			Payload:     payload,
			CreatedAt:   time.Now(),
		})
	}
	return events
}

// TestRunStatsModel_TtftP50 verifies that TTFT p50 is calculated only from
// turns that have a non-zero ttftMs, and that the DUR p50 column still shows
// full turn duration.
func TestRunStatsModel_TtftP50(t *testing.T) {
	events := []fakeEvent{
		// Turn 1: ttft=2s, duration=10s
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 10000, ttftMs: 2000},
		// Turn 2: ttft=4s, duration=20s
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 20000, ttftMs: 4000},
		// Turn 3: no ttft (zero) — should be excluded from TTFT p50
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 15000, ttftMs: 0},
	}

	dbEvents := fakeEventsToDBEvents(events)
	metrics := collectModelMetrics(dbEvents)

	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing anthropic/claude-sonnet-4-6 entry")
	}

	// All three turns count towards totals.
	if entry.Turns != 3 {
		t.Errorf("Turns = %d, want 3", entry.Turns)
	}

	// Only the two non-zero ttft turns contribute to TtftMs.
	if len(entry.TtftMs) != 2 {
		t.Errorf("TtftMs has %d entries, want 2 (zero excluded)", len(entry.TtftMs))
	}

	// All three non-zero duration turns contribute to DurationsMs.
	if len(entry.DurationsMs) != 3 {
		t.Errorf("DurationsMs has %d entries, want 3", len(entry.DurationsMs))
	}

	// TTFT p50: [2000, 4000] → p50 = 2000 (nearest rank, idx=0 for 2 values at p50)
	ttftP50 := percentileFloat64(append([]float64{}, entry.TtftMs...), 50)
	if ttftP50 != 2000 {
		t.Errorf("TTFT p50 = %v, want 2000", ttftP50)
	}

	// Render and verify columns appear.
	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	if !strings.Contains(out, "TTFT p50") {
		t.Errorf("output missing 'TTFT p50' column header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "DUR p50") {
		t.Errorf("output missing 'DUR p50' column header\ngot:\n%s", out)
	}
	// "2s" should appear as TTFT p50 value.
	if !strings.Contains(out, "2s") {
		t.Errorf("output missing '2s' for TTFT p50\ngot:\n%s", out)
	}
}

// TestRunStatsModel_ZeroTtftShowsDash verifies that when all turns have
// ttftMs == 0 (or the field is absent), the TTFT p50 column shows "-".
func TestRunStatsModel_ZeroTtftShowsDash(t *testing.T) {
	events := []fakeEvent{
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 10000, ttftMs: 0},
		{session: "s1", model: "anthropic/claude-sonnet-4-6", inputTokens: 1000, outputTokens: 500, durationMs: 20000, ttftMs: 0},
	}

	dbEvents := fakeEventsToDBEvents(events)
	metrics := collectModelMetrics(dbEvents)

	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing anthropic/claude-sonnet-4-6 entry")
	}

	if len(entry.TtftMs) != 0 {
		t.Errorf("TtftMs should be empty when all ttftMs == 0, got %d entries", len(entry.TtftMs))
	}

	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	// The TTFT p50 column value should be "-" (a dash), not a duration.
	// The header "TTFT p50" must still be present.
	if !strings.Contains(out, "TTFT p50") {
		t.Errorf("output missing 'TTFT p50' column header\ngot:\n%s", out)
	}
	// The rendered data row should show " - " for the TTFT p50 value.
	if !strings.Contains(out, " - ") {
		t.Errorf("expected '-' for TTFT p50 in render output\ngot:\n%s", out)
	}
}

// TestRunStatsModel_TtftAbsentInOldRows verifies that old DB rows without a
// ttftMs field degrade gracefully (TTFT p50 shows "-", no error).
func TestRunStatsModel_TtftAbsentInOldRows(t *testing.T) {
	// Old-format payloads with no ttftMs field.
	dbEvents := []db.Event{
		{
			ID:          "id-0",
			SessionName: "s1",
			Type:        "msg_assistant",
			Payload:     `{"messageId":"msg-0","text":"reply","agent":"opencode","model":"anthropic/claude-sonnet-4-6","inputTokens":1000,"outputTokens":500,"durationMs":10000}`,
			CreatedAt:   time.Now(),
		},
	}

	metrics := collectModelMetrics(dbEvents)

	entry, ok := metrics["anthropic/claude-sonnet-4-6"]
	if !ok {
		t.Fatal("missing anthropic/claude-sonnet-4-6 entry")
	}

	if len(entry.TtftMs) != 0 {
		t.Errorf("TtftMs should be empty for old rows without ttftMs field, got %d entries", len(entry.TtftMs))
	}
	if entry.Turns != 1 {
		t.Errorf("Turns = %d, want 1", entry.Turns)
	}

	out := captureStdout(t, func() {
		renderModelBreakdown(metrics, 7)
	})

	if !strings.Contains(out, "TTFT p50") {
		t.Errorf("output missing 'TTFT p50' column header\ngot:\n%s", out)
	}
}
