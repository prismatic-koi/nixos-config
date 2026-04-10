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
	return fmt.Sprintf(`{"messageId":%q,"text":%q,"agent":"opencode","model":"anthropic/claude-sonnet-4-6","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"durationMs":%d,"contextWindowPct":%f}`,
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

// TestRunStats_DaysMutuallyExclusive verifies that --days is mutually exclusive
// with a session name argument. --all is now a no-op so --days + --all is
// allowed.
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

	// --days + --all should NOT error (--all is a no-op for backwards compatibility).
	statsCmd.Flags().Set("all", "true") //nolint:errcheck
	defer statsCmd.Flags().Set("all", "false")

	err = runStats(statsCmd, nil)
	// May return an error from the DB (no events), but must NOT be a "mutually exclusive" error.
	if err != nil && strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("--days + --all should not produce a 'mutually exclusive' error, got: %v", err)
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

// --- model breakdown tests ---

// assistantPayloadWithModel creates an assistant payload with a specific model.
func assistantPayloadWithModel(model string, inputTokens, outputTokens int, durationMs int64) string {
	return fmt.Sprintf(`{"messageId":"msg-%d","text":"reply","agent":"opencode","model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":%d}`,
		inputTokens, model, inputTokens, outputTokens, durationMs)
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
	if len(ghEntry.Sessions) != 1 {
		t.Errorf("github-copilot sessions = %d, want 1", len(ghEntry.Sessions))
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

	for _, want := range []string{"PROVIDER", "MODEL", "TURNS", "LAT p50", "TOK/S p50", "INPUT", "OUTPUT", "COST", "SESSIONS"} {
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

	if !strings.Contains(out, "turn duration") && !strings.Contains(out, "LAT p50") {
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

	// showAll=false should still show both repos.
	out := captureStdout(t, func() {
		if err := runStatsSummary(false); err != nil {
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

// --- fakeEvent helpers for unit tests that don't need a real DB ---

// fakeEvent is a minimal representation for building test events.
type fakeEvent struct {
	session      string
	model        string
	inputTokens  int
	outputTokens int
	durationMs   int64
}

// fakeEventsToDBEvents converts fakeEvents into db.Events for use with
// collectModelMetrics without requiring a real database.
func fakeEventsToDBEvents(fakes []fakeEvent) []db.Event {
	var events []db.Event
	for i, f := range fakes {
		payload := fmt.Sprintf(`{"messageId":"msg-%d","text":"reply","agent":"opencode","model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":%d}`,
			i, f.model, f.inputTokens, f.outputTokens, f.durationMs)
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
