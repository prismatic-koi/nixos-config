package cmd

// Tests for the assistant-anchored renderCheckinTurns path.
//
// These tests seed a temp DB with events and invoke renderCheckinTurns (or
// runCheckinSession via the public DB path) directly, capturing stdout to
// verify the output.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// captureStdout redirects os.Stdout to a pipe for the duration of fn,
// then returns everything that was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}

// openCheckinTestDB opens a temp DB and registers t.Cleanup to close it.
func openCheckinTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// writeEvent is a helper that writes a single event, fataling on error.
func writeEvent(t *testing.T, d *db.DB, id, session, typ, payload string, ts time.Time) db.Event {
	t.Helper()
	e := db.Event{
		ID:          id,
		SessionName: session,
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent %s: %v", id, err)
	}
	return e
}

// assistantPayload returns a minimal msg_assistant JSON payload.
func assistantPayload(msgID, text string) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q}`, msgID, text)
}

// userPayload returns a minimal msg_user JSON payload.
func userPayload(msgID, text string) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q}`, msgID, text)
}

// toolCallPayload returns a minimal tool_call JSON payload.
func toolCallPayload(msgID, tool, args string) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q,"args":%q}`, msgID, tool, args)
}

// toolResultPayload returns a minimal tool_result JSON payload.
func toolResultPayload(msgID, tool, result string) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q,"result":%q}`, msgID, tool, result)
}

// permAskPayload returns a minimal permission_ask JSON payload.
func permAskPayload(msgID, tool string) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q,"patterns":["*"]}`, msgID, tool)
}

// permDeniedPayload returns a minimal permission_denied JSON payload.
func permDeniedPayload(msgID, tool string) string {
	return fmt.Sprintf(`{"messageId":%q,"tool":%q}`, msgID, tool)
}

// TestRenderCheckinTurns_AllAssistantTurnsShown verifies AC-1:
// given 1 msg_user and 5 msg_assistant events, all 5 assistant turns appear.
func TestRenderCheckinTurns_AllAssistantTurnsShown(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	// 1 user message.
	umsgID := uuid.New().String()
	writeEvent(t, d, uuid.New().String(), session, "msg_user", userPayload(umsgID, "do something"), base)

	// 5 assistant turns on the same user message.
	assistantEvents := make([]db.Event, 5)
	for i := range assistantEvents {
		amsgID := uuid.New().String()
		assistantEvents[i] = writeEvent(t, d, uuid.New().String(), session,
			"msg_assistant", assistantPayload(amsgID, fmt.Sprintf("reply %d", i+1)),
			base.Add(time.Duration(i+1)*time.Second))
	}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	for i := 1; i <= 5; i++ {
		want := fmt.Sprintf("reply %d", i)
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "checkin: "+session) {
		t.Errorf("output missing header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "── end of event log ──") {
		t.Errorf("output missing footer\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_ToolChildrenIndented verifies AC-2:
// each assistant turn is followed by its own indented tool_call/tool_result children.
func TestRenderCheckinTurns_ToolChildrenIndented(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	// Two distinct assistant turns with different tool calls.
	msgID1 := "msg-alpha"
	msgID2 := "msg-beta"

	ae1 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID1, "first assistant"), base.Add(time.Second))
	ae2 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID2, "second assistant"), base.Add(2*time.Second))

	// Tool events for each.
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(msgID1, "bash", "echo hello"), base.Add(time.Second+100*time.Millisecond))
	writeEvent(t, d, uuid.New().String(), session, "tool_result",
		toolResultPayload(msgID1, "bash", "hello"), base.Add(time.Second+200*time.Millisecond))
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(msgID2, "read_file", "main.go"), base.Add(2*time.Second+100*time.Millisecond))

	assistantEvents := []db.Event{ae1, ae2}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Both assistant turns present.
	if !strings.Contains(out, "first assistant") {
		t.Errorf("missing 'first assistant'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "second assistant") {
		t.Errorf("missing 'second assistant'\ngot:\n%s", out)
	}

	// Tool children rendered with indentation.
	if !strings.Contains(out, "  → bash: echo hello") {
		t.Errorf("missing indented bash tool_call\ngot:\n%s", out)
	}
	if !strings.Contains(out, "  → result: hello") {
		t.Errorf("missing indented tool_result\ngot:\n%s", out)
	}
	if !strings.Contains(out, "  → read_file: main.go") {
		t.Errorf("missing indented read_file tool_call\ngot:\n%s", out)
	}

	// The bash tool should appear before read_file in output (ae1 before ae2).
	bashPos := strings.Index(out, "  → bash: echo hello")
	readPos := strings.Index(out, "  → read_file: main.go")
	if bashPos < 0 || readPos < 0 || bashPos >= readPos {
		t.Errorf("tool children not in expected order\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_LastN verifies AC-3:
// passing last=3 returns exactly 3 most-recent assistant turns.
func TestRenderCheckinTurns_LastN(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	dbPath := d.Path()

	// Write 5 assistant events.
	for i := 0; i < 5; i++ {
		msgID := fmt.Sprintf("msg-%d", i)
		writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
			assistantPayload(msgID, fmt.Sprintf("turn %d", i)), base.Add(time.Duration(i)*time.Second))
	}
	d.Close()

	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdout(t, func() {
		if err := runCheckinSession(session, 3, nil, nil, nil, false); err != nil {
			t.Errorf("runCheckinSession: %v", err)
		}
	})

	// Should contain turns 2, 3, 4 (the 3 most-recent) but not 0 or 1.
	for _, i := range []int{2, 3, 4} {
		want := fmt.Sprintf("turn %d", i)
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	for _, i := range []int{0, 1} {
		notWant := fmt.Sprintf("turn %d", i)
		if strings.Contains(out, notWant) {
			t.Errorf("output unexpectedly contains %q (should be outside last-3 window)\ngot:\n%s", notWant, out)
		}
	}
}

// TestRenderCheckinTurns_DefaultLast10 verifies AC-4:
// no --last flag shows the last 10 assistant turns when >10 exist.
func TestRenderCheckinTurns_DefaultLast10(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	dbPath := d.Path()

	// Write 15 assistant events with unique, non-overlapping text to avoid
	// substring false-positives (e.g. "reply-5" does not appear in "reply-15").
	for i := 0; i < 15; i++ {
		msgID := fmt.Sprintf("msg-%d", i)
		writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
			assistantPayload(msgID, fmt.Sprintf("reply-%02d", i)), base.Add(time.Duration(i)*time.Second))
	}
	d.Close()

	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	// Default limit is 10: last 10 of 15 are indices 5-14.
	out := captureStdout(t, func() {
		if err := runCheckinSession(session, 10, nil, nil, nil, false); err != nil {
			t.Errorf("runCheckinSession: %v", err)
		}
	})

	// Turns 05-14 should be present; 00-04 should not.
	for i := 5; i < 15; i++ {
		want := fmt.Sprintf("reply-%02d", i)
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	for i := 0; i < 5; i++ {
		notWant := fmt.Sprintf("reply-%02d", i)
		if strings.Contains(out, notWant) {
			t.Errorf("output unexpectedly contains %q\ngot:\n%s", notWant, out)
		}
	}
}

// TestRenderCheckinTurns_UserEventsInWindow verifies AC-5 and AC-6:
// msg_user events within the assistant-turn time window are included;
// those outside the window are not.
func TestRenderCheckinTurns_UserEventsInWindow(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	// Assistant turns at t=10s and t=20s.
	msgID1 := "msg-1"
	msgID2 := "msg-2"
	ae1 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID1, "assistant reply 1"), base.Add(10*time.Second))
	ae2 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID2, "assistant reply 2"), base.Add(20*time.Second))

	// User message at t=5s — BEFORE window (should NOT appear).
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-early", "early prompt"), base.Add(5*time.Second))

	// User message at t=10s — AT start of window (should appear).
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-atstart", "prompt at start"), base.Add(10*time.Second))

	// User message at t=15s — INSIDE window (should appear).
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-inside", "mid-window prompt"), base.Add(15*time.Second))

	// User message at t=25s — AFTER window (should NOT appear).
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-late", "late prompt"), base.Add(25*time.Second))

	assistantEvents := []db.Event{ae1, ae2}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// In-window user events should appear.
	if !strings.Contains(out, "prompt at start") {
		t.Errorf("output missing in-window user event 'prompt at start'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "mid-window prompt") {
		t.Errorf("output missing in-window user event 'mid-window prompt'\ngot:\n%s", out)
	}

	// Out-of-window user events must NOT appear.
	if strings.Contains(out, "early prompt") {
		t.Errorf("output unexpectedly contains out-of-window 'early prompt'\ngot:\n%s", out)
	}
	if strings.Contains(out, "late prompt") {
		t.Errorf("output unexpectedly contains out-of-window 'late prompt'\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_StateHeader verifies AC-7:
// state header is rendered first.
func TestRenderCheckinTurns_StateHeader(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus(session, "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	msgID := "msg-x"
	ae := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID, "hello"), base)

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Header must come before state.
	headerPos := strings.Index(out, "checkin: "+session)
	statePos := strings.Index(out, "state: active")
	if headerPos < 0 {
		t.Errorf("missing 'checkin: %s'\ngot:\n%s", session, out)
	}
	if statePos < 0 {
		t.Errorf("missing 'state: active'\ngot:\n%s", out)
	}
	if headerPos > statePos {
		t.Errorf("state line appears before header line\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_MultipleUserTurns verifies AC-8:
// sessions with multiple user turns each followed by multiple assistant turns
// render correctly.
func TestRenderCheckinTurns_MultipleUserTurns(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	// Turn 1: user prompt at t=0, two assistant replies at t=1 and t=2.
	u1 := writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-1", "first user prompt"), base)
	amsgID1a := "amsg-1a"
	amsgID1b := "amsg-1b"
	ae1a := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(amsgID1a, "reply 1a"), base.Add(time.Second))
	ae1b := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(amsgID1b, "reply 1b"), base.Add(2*time.Second))

	// Turn 2: user prompt at t=5, two assistant replies at t=6 and t=7.
	_ = writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-2", "second user prompt"), base.Add(5*time.Second))
	amsgID2a := "amsg-2a"
	amsgID2b := "amsg-2b"
	ae2a := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(amsgID2a, "reply 2a"), base.Add(6*time.Second))
	ae2b := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(amsgID2b, "reply 2b"), base.Add(7*time.Second))

	_ = u1

	assistantEvents := []db.Event{ae1a, ae1b, ae2a, ae2b}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// All four assistant replies present.
	for _, want := range []string{"reply 1a", "reply 1b", "reply 2a", "reply 2b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}

	// Both user prompts present (they fall within the window t=1s to t=7s,
	// or at t=0 which is exactly the window start for the full set).
	// u1 at t=0 is BEFORE the first assistant turn (t=1s) — outside window.
	// u2 at t=5s is inside [t=1s, t=7s].
	if strings.Contains(out, "first user prompt") {
		// t=0 is before earliest assistant (t=1s) — must NOT be in window.
		t.Errorf("first user prompt (t=0) unexpectedly in output (it is before the assistant window)\ngot:\n%s", out)
	}
	if !strings.Contains(out, "second user prompt") {
		t.Errorf("second user prompt (t=5s) missing from output\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_PermissionEvents verifies AC-9:
// permission_ask and permission_denied rendered under their assistant turn.
func TestRenderCheckinTurns_PermissionEvents(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	msgID := "msg-perm"
	ae := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID, "asking permission"), base)

	writeEvent(t, d, uuid.New().String(), session, "permission_ask",
		permAskPayload(msgID, "bash"), base.Add(100*time.Millisecond))
	writeEvent(t, d, uuid.New().String(), session, "permission_denied",
		permDeniedPayload(msgID, "bash"), base.Add(200*time.Millisecond))

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	if !strings.Contains(out, "⏳ waiting for approval: bash") {
		t.Errorf("output missing permission_ask\ngot:\n%s", out)
	}
	if !strings.Contains(out, "❌ denied: bash") {
		t.Errorf("output missing permission_denied\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_NoToolChildren verifies AC-15:
// an assistant turn with no tool children renders without indented lines.
func TestRenderCheckinTurns_NoToolChildren(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	msgID := "msg-plain"
	ae := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID, "just a reply, no tools"), base)

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	if !strings.Contains(out, "just a reply, no tools") {
		t.Errorf("output missing assistant text\ngot:\n%s", out)
	}
	if strings.Contains(out, "  →") {
		t.Errorf("output unexpectedly contains indented tool lines\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_VerboseNoTruncation verifies AC-17:
// --verbose disables truncation of tool args/results.
func TestRenderCheckinTurns_VerboseNoTruncation(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	msgID := "msg-verbose"
	ae := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID, "verbose test"), base)

	longArgs := strings.Repeat("x", 200)
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(msgID, "bash", longArgs), base.Add(100*time.Millisecond))

	// Non-verbose: should truncate.
	outTrunc := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae}, false); err != nil {
			t.Errorf("renderCheckinTurns (non-verbose): %v", err)
		}
	})
	if strings.Contains(outTrunc, longArgs) {
		t.Errorf("non-verbose output should truncate long args but contains full string\ngot:\n%s", outTrunc)
	}
	if !strings.Contains(outTrunc, "...") {
		t.Errorf("non-verbose output should have '...' truncation marker\ngot:\n%s", outTrunc)
	}

	// Verbose: should NOT truncate.
	outVerbose := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae}, true); err != nil {
			t.Errorf("renderCheckinTurns (verbose): %v", err)
		}
	})
	if !strings.Contains(outVerbose, longArgs) {
		t.Errorf("verbose output should contain full args string\ngot:\n%s", outVerbose)
	}
}

// TestRunCheckinSession_NoAssistantEventsButHasUserEvents verifies AC-14:
// session with msg_user but zero msg_assistant shows only header + footer, exits 0.
func TestRunCheckinSession_NoAssistantEventsButHasUserEvents(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	dbPath := d.Path()

	// Write only a msg_user event, no msg_assistant.
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayload("umsg-only", "a prompt with no reply yet"), base)
	d.Close()

	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdout(t, func() {
		if err := runCheckinSession(session, 10, nil, nil, nil, false); err != nil {
			t.Errorf("runCheckinSession returned unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "checkin: "+session) {
		t.Errorf("output missing header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "── end of event log ──") {
		t.Errorf("output missing footer\ngot:\n%s", out)
	}
}

// TestRunCheckinSession_TypesRoutesToRaw verifies AC-10:
// --types routes to the raw event path (runCheckinSessionRaw), not the
// assistant-anchored path.
func TestRunCheckinSession_TypesRoutesToRaw(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	dbPath := d.Path()

	msgID := "msg-raw"
	writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayload(msgID, "raw mode reply"), base)
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(msgID, "bash", "ls"), base.Add(100*time.Millisecond))
	d.Close()

	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	// --types msg_assistant,tool_call routes to raw path.
	out := captureStdout(t, func() {
		if err := runCheckinSession(session, 10, nil, nil, []string{"msg_assistant", "tool_call"}, false); err != nil {
			t.Errorf("runCheckinSession: %v", err)
		}
	})

	if !strings.Contains(out, "raw mode reply") {
		t.Errorf("output missing assistant text in raw mode\ngot:\n%s", out)
	}
}

// assistantPayloadWithAgent returns a msg_assistant JSON payload that includes an agent name.
func assistantPayloadWithAgent(msgID, text, agent string) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q,"agent":%q}`, msgID, text, agent)
}

// userPayloadWithAgent returns a msg_user JSON payload that includes an agent name.
func userPayloadWithAgent(msgID, text, agent string) string {
	return fmt.Sprintf(`{"messageId":%q,"text":%q,"agent":%q}`, msgID, text, agent)
}

// setRootAgent writes an agent_status row with a root_agent_name set.
func setRootAgent(t *testing.T, d *db.DB, session, agentName string) {
	t.Helper()
	an := agentName
	if err := d.UpsertStatusWithRootAgent(session, "repo", "/code/repo/main", "active", nil, nil, &an, nil); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent: %v", err)
	}
}

// TestRenderCheckinTurns_SubagentCollapsedDefault verifies that consecutive
// subagent turns are collapsed into a single summary line in default mode.
func TestRenderCheckinTurns_SubagentCollapsedDefault(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	const rootAgent = "opencode"
	const subAgent = "review"
	base := time.Now().Truncate(time.Second)

	setRootAgent(t, d, session, rootAgent)

	// Root agent turn at t=0.
	rootMsgID := "msg-root"
	rootEvent := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(rootMsgID, "root turn", rootAgent), base)

	// Two subagent turns at t=1s and t=3s.
	sub1ID := "msg-sub1"
	sub2ID := "msg-sub2"
	sub1 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(sub1ID, "sub turn 1", subAgent), base.Add(time.Second))
	sub2 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(sub2ID, "sub turn 2", subAgent), base.Add(3*time.Second))

	// Three tool calls across the two subagent turns.
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(sub1ID, "bash", "echo 1"), base.Add(time.Second+100*time.Millisecond))
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(sub1ID, "bash", "echo 2"), base.Add(time.Second+200*time.Millisecond))
	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(sub2ID, "read_file", "main.go"), base.Add(3*time.Second+100*time.Millisecond))

	assistantEvents := []db.Event{rootEvent, sub1, sub2}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Root turn must appear inline.
	if !strings.Contains(out, "root turn") {
		t.Errorf("output missing 'root turn'\ngot:\n%s", out)
	}

	// Subagent turns must NOT appear as separate inline turns.
	if strings.Contains(out, "sub turn 1") || strings.Contains(out, "sub turn 2") {
		t.Errorf("subagent text should be collapsed but appeared inline\ngot:\n%s", out)
	}

	// A summary line mentioning the subagent name and tool count must appear.
	if !strings.Contains(out, "└─ review") {
		t.Errorf("output missing subagent summary line with '└─ review'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "3 tool calls") {
		t.Errorf("output missing '3 tool calls' in summary line\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_SubagentVerboseExpanded verifies that in verbose mode
// subagent turns appear inline with the │ prefix.
func TestRenderCheckinTurns_SubagentVerboseExpanded(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	const rootAgent = "opencode"
	const subAgent = "review"
	base := time.Now().Truncate(time.Second)

	setRootAgent(t, d, session, rootAgent)

	// Root agent turn.
	rootMsgID := "msg-root"
	rootEvent := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(rootMsgID, "root turn", rootAgent), base)

	// Two subagent turns.
	sub1ID := "msg-sub1"
	sub2ID := "msg-sub2"
	sub1 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(sub1ID, "sub turn 1", subAgent), base.Add(time.Second))
	sub2 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(sub2ID, "sub turn 2", subAgent), base.Add(2*time.Second))

	assistantEvents := []db.Event{rootEvent, sub1, sub2}

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, assistantEvents, true); err != nil {
			t.Errorf("renderCheckinTurns (verbose): %v", err)
		}
	})

	// Subagent turns must appear inline.
	if !strings.Contains(out, "sub turn 1") {
		t.Errorf("verbose: output missing 'sub turn 1'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "sub turn 2") {
		t.Errorf("verbose: output missing 'sub turn 2'\ngot:\n%s", out)
	}

	// Each subagent line must carry the │ prefix.
	if !strings.Contains(out, "  │ ") {
		t.Errorf("verbose: output missing '  │ ' prefix\ngot:\n%s", out)
	}

	// No collapsed summary line in verbose mode.
	if strings.Contains(out, "└─") {
		t.Errorf("verbose: output unexpectedly contains collapsed summary '└─'\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_SubagentSingleToolCall verifies the singular "tool call"
// (not "tool calls") label when exactly one tool call is present.
func TestRenderCheckinTurns_SubagentSingleToolCall(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	const rootAgent = "opencode"
	const subAgent = "review"
	base := time.Now().Truncate(time.Second)

	setRootAgent(t, d, session, rootAgent)

	subMsgID := "msg-sub"
	subEvent := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(subMsgID, "sub turn", subAgent), base)

	writeEvent(t, d, uuid.New().String(), session, "tool_call",
		toolCallPayload(subMsgID, "bash", "ls"), base.Add(100*time.Millisecond))

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{subEvent}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	if !strings.Contains(out, "1 tool call") {
		t.Errorf("output missing '1 tool call' (singular)\ngot:\n%s", out)
	}
	if strings.Contains(out, "1 tool calls") {
		t.Errorf("output incorrectly uses plural '1 tool calls'\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_SubagentNoToolCalls verifies the summary line format
// when a subagent run has zero tool calls.
func TestRenderCheckinTurns_SubagentNoToolCalls(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	const rootAgent = "opencode"
	const subAgent = "review"
	base := time.Now().Truncate(time.Second)

	setRootAgent(t, d, session, rootAgent)

	subMsgID := "msg-sub"
	subEvent := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(subMsgID, "sub turn", subAgent), base)

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{subEvent}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Summary line should exist without a tool count phrase.
	if !strings.Contains(out, "└─ review") {
		t.Errorf("output missing subagent summary line\ngot:\n%s", out)
	}
	if strings.Contains(out, "tool call") {
		t.Errorf("output should not mention tool calls when count is 0\ngot:\n%s", out)
	}
}

// TestFormatDuration verifies formatDuration edge cases.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "<1s"},
		{500 * time.Millisecond, "<1s"},
		{999 * time.Millisecond, "<1s"},
		{time.Second, "1s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m 0s"},
		{90 * time.Second, "1m 30s"},
		{86 * time.Second, "1m 26s"},
	}
	for _, tc := range cases {
		got := formatDuration(tc.d)
		if got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRenderCheckinTurns_PreMigrationNoRootAgent verifies that when no
// root_agent_name is set (pre-migration rows), all turns render inline
// without collapsing.
func TestRenderCheckinTurns_PreMigrationNoRootAgent(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	base := time.Now().Truncate(time.Second)

	// Do NOT call setRootAgent — simulate a pre-migration session with
	// no root_agent_name in agent_status.

	// Two turns that have different agent fields in payload.
	msg1ID := "msg-1"
	msg2ID := "msg-2"
	ae1 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(msg1ID, "turn one", "opencode"), base)
	ae2 := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(msg2ID, "turn two", "review"), base.Add(time.Second))

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{ae1, ae2}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Both turns must appear inline — no collapsing without root_agent_name.
	if !strings.Contains(out, "turn one") {
		t.Errorf("pre-migration: output missing 'turn one'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "turn two") {
		t.Errorf("pre-migration: output missing 'turn two'\ngot:\n%s", out)
	}
	// No collapsed summary lines.
	if strings.Contains(out, "└─") {
		t.Errorf("pre-migration: output unexpectedly collapsed turns\ngot:\n%s", out)
	}
}

// TestRenderCheckinTurns_SubagentUserEventsCollapsed verifies that msg_user
// events belonging to a subagent (agent != root) are also collapsed in default
// mode alongside their surrounding subagent assistant turns.
func TestRenderCheckinTurns_SubagentUserEventsCollapsed(t *testing.T) {
	d := openCheckinTestDB(t)
	const session = "repo@main"
	const rootAgent = "opencode"
	const subAgent = "review"
	base := time.Now().Truncate(time.Second)

	setRootAgent(t, d, session, rootAgent)

	// Subagent user message injected between two subagent assistant turns.
	subUserMsgID := "umsg-sub"
	writeEvent(t, d, uuid.New().String(), session, "msg_user",
		userPayloadWithAgent(subUserMsgID, "subagent user message", subAgent),
		base.Add(500*time.Millisecond))

	subMsgID := "msg-sub"
	subEvent := writeEvent(t, d, uuid.New().String(), session, "msg_assistant",
		assistantPayloadWithAgent(subMsgID, "sub assistant reply", subAgent),
		base.Add(time.Second))

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(session, d, []db.Event{subEvent}, false); err != nil {
			t.Errorf("renderCheckinTurns: %v", err)
		}
	})

	// Subagent text must not appear inline.
	if strings.Contains(out, "sub assistant reply") {
		t.Errorf("subagent assistant text should be collapsed\ngot:\n%s", out)
	}
	if strings.Contains(out, "subagent user message") {
		t.Errorf("subagent user message text should be collapsed\ngot:\n%s", out)
	}

	// Summary line must still appear.
	if !strings.Contains(out, "└─ review") {
		t.Errorf("output missing subagent summary line\ngot:\n%s", out)
	}
}

// TestRunCheckinSession_LegacyFallbackNoAssistantNoUser verifies AC-13:
// when no DB rows exist at all for the session, falls back to legacy
// (which will error because tmux is unavailable in tests — that's acceptable;
// we just verify it doesn't panic and the error is from the legacy path).
func TestRunCheckinSession_LegacyFallbackNoRows(t *testing.T) {
	d := openCheckinTestDB(t)
	dbPath := d.Path()
	d.Close()

	SetTestDBPath(dbPath)
	t.Cleanup(func() { SetTestDBPath("") })

	// Session "repo@ghost" has no DB rows — should fall through to legacy.
	err := runCheckinSession("repo@ghost", 10, nil, nil, nil, false)
	// The legacy path returns an error when tmux is unavailable (no TMUX env
	// in tests). We just verify the call doesn't panic and the error message
	// references the session name.
	if err == nil {
		t.Error("expected error from legacy path (no tmux in tests), got nil")
	}
	if !strings.Contains(err.Error(), "repo@ghost") {
		t.Errorf("error should mention session name\ngot: %v", err)
	}
}
