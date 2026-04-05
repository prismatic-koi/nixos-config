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

func assistantPayloadWithTokens(msgID, text string, inputTokens, outputTokens, cacheRead, cacheWrite int, durationMs int64, contextPct float64) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q,"agent":"opencode","model":"anthropic/claude-sonnet-4-20250514","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"durationMs":%d,"contextWindowPct":%f}`,
		msgID, text, inputTokens, outputTokens, cacheRead, cacheWrite, durationMs, contextPct)
}

func toolCallPayloadWithDuration(msgID, tool, args string, durationMs int64) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q,"args":%q,"durationMs":%d}`,
		msgID, tool, args, durationMs)
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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
		if err := runStatsSession(session); err != nil {
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

	err := runStatsSession("nonexistent@main")
	if err == nil {
		t.Error("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found'\ngot: %v", err)
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
		if err := runStatsSummary(true); err != nil {
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
		if err := runStatsSummary(true); err != nil {
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
		if err := runStatsSession(session); err != nil {
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

	m := collectMetrics(events)

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
