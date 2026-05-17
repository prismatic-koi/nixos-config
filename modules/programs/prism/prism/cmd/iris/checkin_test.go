package main

// checkin_test.go — unit + integration tests for `iris checkin`.
//
// The pure-rendering tests (TestRenderNarrative_*, TestRenderJSON_*) live
// alongside the flag-parsing checks (TestCheckinFlags_*) so the cobra
// command's RunE wiring is exercised end-to-end against an isolated DB.
//
// All tests use iristest.NewIsolated to redirect HOME / XDG_STATE_HOME to a
// per-test tempdir. This makes the suite safe to run in CI's nix sandbox
// ($HOME=/homeless-shelter) and stops a stray write from touching the
// developer's real ~/.local/state/iris.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
	"github.com/prismatic-koi/prism/internal/payload"
)

// seedSession writes a sessions + agent_status row and returns the chosen
// session_name + instance_id. Used by every test that needs a session
// resolvable via either name or UUID prefix.
//
// Sessions are seeded with role "worker" and worktree fixed under iso.Root
// so they don't escape isolation.
func seedSession(t *testing.T, iso *iristest.Isolated, name string) (sessionName, instanceID string) {
	t.Helper()
	instanceID = uuid.NewString()
	role := "worker"
	worktree := iso.Root + "/worktree"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: name,
		AgentRole:   &role,
		Repo:        "iris-test",
		Worktree:    worktree,
		Harness:     "pi",
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedSession: insert sessions row: %v", err)
	}
	if err := iso.DB.UpsertStatus(name, "iris-test", worktree, "active", nil, nil); err != nil {
		t.Fatalf("seedSession: upsert status: %v", err)
	}
	return name, instanceID
}

// seedAssistantTurn writes msg_assistant + tool_call + tool_result events
// for a single conversational turn. createdAt anchors the assistant event;
// the children get successive +1ms timestamps so the chronological order is
// deterministic.
func seedAssistantTurn(t *testing.T, iso *iristest.Isolated, sessionName, text, tool, args, result string, createdAt time.Time) {
	t.Helper()
	msgID := "msg-" + uuid.NewString()

	ap, _ := json.Marshal(payload.MsgAssistant{
		Text:      text,
		MessageID: msgID,
		Agent:     "worker",
		Model:     "sonnet",
	})
	if err := iso.DB.WriteEvent(db.Event{
		ID:          uuid.NewString(),
		SessionName: sessionName,
		Repo:        "iris-test",
		Worktree:    iso.Root + "/worktree",
		Type:        "msg_assistant",
		Payload:     string(ap),
		CreatedAt:   createdAt,
	}); err != nil {
		t.Fatalf("seedAssistantTurn: write msg_assistant: %v", err)
	}

	// Post-#1783: payload.ToolCall.Args is a json.RawMessage holding
	// the raw JSON value the pi extension emits (typically an
	// object). Tests pass JSON-object literals as strings; we just
	// re-tag them as RawMessage. An empty fixture maps to the
	// canonical "no args" wire value (null) so the renderer's
	// "(no args)" placeholder is exercised.
	var tcArgs json.RawMessage
	if args == "" {
		tcArgs = nil
	} else {
		tcArgs = json.RawMessage(args)
	}
	// Inject a synthetic `messageId` alongside the post-#1783 wire
	// shape so the cmd/iris/checkin secondary-query path
	// (QueryEventsByMessageIDs) can still locate this child event
	// during tests. The pi extension does NOT emit messageId on
	// tool_call — see coordinator escalation 2026-05-17 and the
	// commentary on toolCallPayload in cmd/checkin_test.go.
	tcMarshalled, _ := json.Marshal(payload.ToolCall{
		Name: tool,
		Args: tcArgs,
		ID:   msgID,
	})
	tcp := injectSyntheticMessageID(tcMarshalled, msgID)
	if err := iso.DB.WriteEvent(db.Event{
		ID:          uuid.NewString(),
		SessionName: sessionName,
		Repo:        "iris-test",
		Worktree:    iso.Root + "/worktree",
		Type:        "tool_call",
		Payload:     string(tcp),
		CreatedAt:   createdAt.Add(1 * time.Millisecond),
	}); err != nil {
		t.Fatalf("seedAssistantTurn: write tool_call: %v", err)
	}

	// Same synthetic-messageId rationale for tool_result, per
	// `cmd/checkin_test.go::toolResultPayload`'s commentary.
	trMarshalled, _ := json.Marshal(payload.ToolResult{
		ID:      msgID,
		Success: true,
		Output:  result,
	})
	trp := injectSyntheticMessageID(trMarshalled, msgID)
	if err := iso.DB.WriteEvent(db.Event{
		ID:          uuid.NewString(),
		SessionName: sessionName,
		Repo:        "iris-test",
		Worktree:    iso.Root + "/worktree",
		Type:        "tool_result",
		Payload:     string(trp),
		CreatedAt:   createdAt.Add(2 * time.Millisecond),
	}); err != nil {
		t.Fatalf("seedAssistantTurn: write tool_result: %v", err)
	}
}

// TestRenderNarrative_BasicTurn asserts the default narrative output
// includes the header, state line, assistant text, and a tool one-liner
// with the paired result summary.
func TestRenderNarrative_BasicTurn(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("basic-turn"))

	seedAssistantTurn(t, iso, sess,
		"I'll run the build.",
		"bash",
		`{"command":"go build ./..."}`,
		"", // empty result → ✓
		time.Now().UTC().Add(-1*time.Minute),
	)

	var buf bytes.Buffer
	if err := runCheckinNarrative(&buf, iso.DB, sess, 10, nil, nil, false); err != nil {
		t.Fatalf("runCheckinNarrative: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"checkin: " + sess,
		"state: active",
		"I'll run the build.",
		"→ bash:",
		"go build ./...",
		"✓",
		"── end of event log ──",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderNarrative_LastZero asserts that --last 0 emits only the header
// + state + footer (no body), per the AC.
func TestRenderNarrative_LastZero(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("last-zero"))
	seedAssistantTurn(t, iso, sess, "Hello.", "bash", `{"command":"echo hi"}`, "hi\n", time.Now().UTC().Add(-time.Minute))

	var buf bytes.Buffer
	if err := runCheckinNarrative(&buf, iso.DB, sess, 0, nil, nil, false); err != nil {
		t.Fatalf("runCheckinNarrative(last=0): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "state: active") {
		t.Errorf("state line missing:\n%s", out)
	}
	if strings.Contains(out, "Hello.") {
		t.Errorf("body should be suppressed at --last 0:\n%s", out)
	}
	if !strings.Contains(out, "── end of event log ──") {
		t.Errorf("footer missing:\n%s", out)
	}
}

// TestRenderNarrative_EmptySession asserts that a session with no events
// still prints the state line (exit 0).
func TestRenderNarrative_EmptySession(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("empty"))

	var buf bytes.Buffer
	if err := runCheckinNarrative(&buf, iso.DB, sess, 10, nil, nil, false); err != nil {
		t.Fatalf("runCheckinNarrative(empty): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "state: active") {
		t.Errorf("state line missing for empty session:\n%s", out)
	}
	if !strings.Contains(out, "── end of event log ──") {
		t.Errorf("footer missing for empty session:\n%s", out)
	}
}

// TestRenderNarrative_Verbose asserts --verbose emits full args and full
// result, not the truncated one-liner.
func TestRenderNarrative_Verbose(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("verbose"))

	longCmd := strings.Repeat("x", 200) // longer than the one-liner truncation
	args := fmt.Sprintf(`{"command":%q}`, longCmd)
	seedAssistantTurn(t, iso, sess, "Long bash.", "bash", args, "ok\n", time.Now().UTC().Add(-time.Minute))

	var buf bytes.Buffer
	if err := runCheckinNarrative(&buf, iso.DB, sess, 10, nil, nil, true); err != nil {
		t.Fatalf("runCheckinNarrative(verbose): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, longCmd) {
		t.Errorf("verbose output must contain full %d-char command (no truncation), got:\n%s", len(longCmd), out)
	}
	if !strings.Contains(out, "result:") {
		t.Errorf("verbose output must contain raw result line:\n%s", out)
	}
}

// TestRenderJSON_EmptySession asserts an empty session emits "[]".
func TestRenderJSON_EmptySession(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("json-empty"))

	var buf bytes.Buffer
	if err := runCheckinJSON(&buf, iso.DB, sess, 10, nil, nil); err != nil {
		t.Fatalf("runCheckinJSON: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Errorf("empty session JSON: got %q, want %q", got, "[]")
	}
}

// TestRenderJSON_FieldShape asserts the JSON event shape matches the
// agent_events schema field names.
func TestRenderJSON_FieldShape(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("json-shape"))
	seedAssistantTurn(t, iso, sess, "Hi.", "bash", `{"command":"true"}`, "", time.Now().UTC().Add(-time.Minute))

	var buf bytes.Buffer
	if err := runCheckinJSON(&buf, iso.DB, sess, 10, nil, nil); err != nil {
		t.Fatalf("runCheckinJSON: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, buf.String())
	}
	if len(rows) == 0 {
		t.Fatalf("expected ≥1 events, got 0:\n%s", buf.String())
	}
	required := []string{"id", "session_name", "repo", "worktree", "type", "payload", "created_at"}
	for _, key := range required {
		if _, ok := rows[0][key]; !ok {
			t.Errorf("JSON row missing required field %q: %v", key, rows[0])
		}
	}
}

// TestResolveSession_FullName asserts a full session_name resolves to itself.
func TestResolveSession_FullName(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("by-name"))

	got, err := resolveSession(iso.DB, sess)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got != sess {
		t.Errorf("resolveSession by name: got %q, want %q", got, sess)
	}
}

// TestResolveSession_UUIDPrefix asserts a 12-char UUID prefix resolves to
// the matching session_name.
func TestResolveSession_UUIDPrefix(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, id := seedSession(t, iso, iristest.SessionName("by-prefix"))

	prefix := id[:12]
	got, err := resolveSession(iso.DB, prefix)
	if err != nil {
		t.Fatalf("resolveSession by prefix: %v", err)
	}
	if got != sess {
		t.Errorf("resolveSession by prefix: got %q, want %q", got, sess)
	}
}

// TestResolveSession_PrefixTooShort asserts a <12-char input that is not
// an exact session_name returns "no such session".
func TestResolveSession_PrefixTooShort(t *testing.T) {
	iso := iristest.NewIsolated(t)
	_, id := seedSession(t, iso, iristest.SessionName("short-prefix"))

	short := id[:8]
	_, err := resolveSession(iso.DB, short)
	if err == nil {
		t.Fatalf("expected error for short prefix %q, got nil", short)
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("expected 'no such session' error, got: %v", err)
	}
}

// TestResolveSession_NotFound asserts a long-enough input that matches
// nothing returns "no such session".
func TestResolveSession_NotFound(t *testing.T) {
	iso := iristest.NewIsolated(t)
	_, _ = seedSession(t, iso, iristest.SessionName("present"))

	_, err := resolveSession(iso.DB, "deadbeefdead")
	if err == nil {
		t.Fatalf("expected error for unknown prefix, got nil")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("expected 'no such session' error, got: %v", err)
	}
}

// TestResolveSession_AmbiguousPrefix asserts a prefix matching multiple
// sessions returns a clear "ambiguous" error listing the candidates.
//
// To force ambiguity we generate two UUIDs that share an 8-char prefix and
// then query with that shared prefix. uuid.NewString() entropy makes
// collisions on 12+ chars vanishingly rare in tests, so we craft the
// prefix by inserting two rows whose instance_ids we control directly via
// InsertSession.
func TestResolveSession_AmbiguousPrefix(t *testing.T) {
	iso := iristest.NewIsolated(t)
	role := "worker"
	worktree := iso.Root + "/worktree"

	// Two synthetic UUIDs that share the same 16-char prefix.
	idA := "aaaaaaaaaaaa1111-1111-1111-1111-aaaaaaaaaaaa"
	idB := "aaaaaaaaaaaa2222-2222-2222-2222-bbbbbbbbbbbb"
	// The real db schema treats instance_id as TEXT; non-canonical UUID
	// formatting is fine for the LIKE-prefix path.
	nameA := iristest.SessionName("ambig-a")
	nameB := iristest.SessionName("ambig-b")
	for _, row := range []db.Session{
		{InstanceID: idA, SessionName: nameA, AgentRole: &role, Repo: "iris-test", Worktree: worktree, Harness: "pi", StartedAt: time.Now().UTC()},
		{InstanceID: idB, SessionName: nameB, AgentRole: &role, Repo: "iris-test", Worktree: worktree, Harness: "pi", StartedAt: time.Now().UTC()},
	} {
		if err := iso.DB.InsertSession(row); err != nil {
			t.Fatalf("InsertSession: %v", err)
		}
	}

	_, err := resolveSession(iso.DB, "aaaaaaaaaaaa") // 12 chars, matches both
	if err == nil {
		t.Fatalf("expected ambiguous-prefix error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
	if !strings.Contains(msg, nameA) || !strings.Contains(msg, nameB) {
		t.Errorf("ambiguous-prefix error must list both candidates %q and %q, got:\n%s", nameA, nameB, msg)
	}
}

// runCheckinForTest dispatches the checkin subcommand through rootCmd so the
// cobra flag-parsing path is exercised end-to-end. Each test gets a fresh
// out/err buffer and the rootCmd args are reset on return so global state
// can't leak between tests.
func runCheckinForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	rootCmd.SetArgs(append([]string{"checkin"}, args...))
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	err := rootCmd.Execute()
	// Reset flags so test order doesn't leak default-mutations between tests.
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		_ = checkinCmd.Flags().Set("from", "")
		_ = checkinCmd.Flags().Set("before", "")
		_ = checkinCmd.Flags().Set("last", "10")
		_ = checkinCmd.Flags().Set("verbose", "false")
		_ = checkinCmd.Flags().Set("json", "false")
	})
	return buf.String(), err
}

// TestCheckinFlags_FromBeforeMutex asserts that passing both --from and
// --before is a usage error.
func TestCheckinFlags_FromBeforeMutex(t *testing.T) {
	iso := iristest.NewIsolated(t)
	_, _ = seedSession(t, iso, iristest.SessionName("mutex"))

	_, err := runCheckinForTest(t, iristest.SessionName("mutex"), "--from", "abc", "--before", "def")
	if err == nil {
		t.Fatalf("expected error from --from + --before, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' error, got: %v", err)
	}
}

// TestCheckinFlags_NegativeLast asserts --last < 0 is rejected.
func TestCheckinFlags_NegativeLast(t *testing.T) {
	iso := iristest.NewIsolated(t)
	_, _ = seedSession(t, iso, iristest.SessionName("neg-last"))

	_, err := runCheckinForTest(t, iristest.SessionName("neg-last"), "--last", "-1")
	if err == nil {
		t.Fatalf("expected error from --last -1, got nil")
	}
}

// TestOpenIrisDBForRead_Missing asserts a missing DB path returns a clear
// "iris database not found" error.
func TestOpenIrisDBForRead_Missing(t *testing.T) {
	// Use an isolated env so we don't accidentally find the host DB.
	iso := iristest.NewIsolated(t)
	missing := iso.Root + "/nope/iris.db"

	_, err := openIrisDBForRead(missing)
	if err == nil {
		t.Fatalf("expected error for missing DB, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

// TestCheckinEndToEnd_ViaCobra exercises the full cobra RunE path against
// an isolated DB. This satisfies the AC "integration test … invokes
// iris checkin <session> and asserts the output contains the expected
// assistant message and tool one-liners."
//
// It does NOT spawn a real pi process — that would require pi/Claude
// being installed, network access, and is the responsibility of the parity
// suite. Instead it seeds the same agent_events rows a real session would
// have written and asserts the CLI surface renders them correctly.
func TestCheckinEndToEnd_ViaCobra(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sess, _ := seedSession(t, iso, iristest.SessionName("end-to-end"))

	seedAssistantTurn(t, iso, sess,
		"Looking at the build.",
		"bash",
		`{"command":"go build ./..."}`,
		"",
		time.Now().UTC().Add(-2*time.Minute),
	)
	seedAssistantTurn(t, iso, sess,
		"Tests pass; pushing.",
		"bash",
		`{"command":"git push"}`,
		"To origin...\n",
		time.Now().UTC().Add(-1*time.Minute),
	)

	// The cobra command opens iris.OpenDB(ResolvePaths().DB). iristest
	// already redirects ResolvePaths() under the tempdir, so this hits
	// the same DB we just seeded.
	if got := iris.ResolvePaths().DB; got != iso.Paths.DB {
		t.Fatalf("ResolvePaths().DB drift: got %q, want %q", got, iso.Paths.DB)
	}

	got, err := runCheckinForTest(t, sess)
	if err != nil {
		t.Fatalf("rootCmd checkin execute: %v\n%s", err, got)
	}
	for _, want := range []string{
		"checkin: " + sess,
		"state: active",
		"Looking at the build.",
		"Tests pass; pushing.",
		"→ bash:",
		"go build ./...",
		"git push",
		"── end of event log ──",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("e2e output missing %q:\n%s", want, got)
		}
	}
}

// injectSyntheticMessageID rewrites a marshalled payload.ToolCall or
// payload.ToolResult to add a `messageId` field alongside the new
// `id` field. The pi prism extension does NOT emit messageId on
// tool_call/tool_result; the field is synthetic-for-tests so the
// secondary-query SQL pushdown in db.QueryEventsByMessageIDs (which
// matches on $.messageId) can still locate these child events. See
// coordinator escalation 2026-05-17 and the matching helper in
// cmd/checkin_test.go for the full rationale.
//
// The rewrite is a simple JSON-object surgery: drops the leading "{"
// and inserts the synthetic field at the start of the object body.
// Returns the original input unchanged if the input doesn't parse as
// a JSON object (defensive — never panics).
func injectSyntheticMessageID(in []byte, msgID string) []byte {
	if len(in) < 2 || in[0] != '{' {
		return in
	}
	// Special case: an empty object literal "{}". Replace with
	// `{"messageId":"..."}`.
	if len(in) == 2 && in[1] == '}' {
		return []byte(fmt.Sprintf(`{"messageId":%q}`, msgID))
	}
	// Insert `"messageId":"...",` after the opening "{".
	prefix := fmt.Sprintf(`{"messageId":%q,`, msgID)
	out := make([]byte, 0, len(prefix)+len(in)-1)
	out = append(out, prefix...)
	out = append(out, in[1:]...)
	return out
}
