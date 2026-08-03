package db_test

// Write-time credential redaction (issue #2589).
//
// These tests assert the SECOND control: a payload that reaches the database
// layer with a credential in it does not reach a row with a credential in it.
// The first control lives in the pi extension and is tested there.
//
// SECURITY: every credential value here is synthetic. Nothing is read from
// the environment of the test process, and no value resembles a live token.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

// Synthetic values, shaped so no shape rule claims them. A shape hit would
// mask a value-layer bug.
const (
	fakeGitHubToken    = "SYNTHETIC-GITHUB-VALUE-000000000000"
	fakeAnthropicKey   = "SYNTHETIC-ANTHROPIC-VALUE-111111111"
	fakeSessionName    = "prism-test@redaction"
	syntheticEventRepo = "prism-test-repo"
)

// redactingTestDB opens a test database whose write-time redactor knows the
// synthetic values. It never touches the process environment.
func redactingTestDB(t *testing.T) *db.DB {
	t.Helper()
	d := openTestDB(t)
	d.SetRedactor(payload.NewRedactor(map[string]string{
		"GITHUB_TOKEN":      fakeGitHubToken,
		"ANTHROPIC_API_KEY": fakeAnthropicKey,
	}))
	return d
}

func storedPayload(t *testing.T, d *db.DB, sessionName string) string {
	t.Helper()
	events, err := d.AllSessionEvents(sessionName)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}
	return events[0].Payload
}

func writeEventWithPayload(t *testing.T, d *db.DB, eventType, raw string) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Repo:        syntheticEventRepo,
		Type:        eventType,
		Payload:     raw,
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

func TestWriteEvent_ToolResultPayloadCarriesNoCredentialValue(t *testing.T) {
	d := redactingTestDB(t)

	body, err := json.Marshal(payload.ToolResult{
		ID:      "call_1",
		Success: true,
		Output:  "GITHUB_TOKEN=" + fakeGitHubToken + "\n",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeEventWithPayload(t, d, "tool_result", string(body))

	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, fakeGitHubToken) {
		t.Fatal("stored tool_result payload still carries the credential value")
	}
	if !strings.Contains(stored, "[redacted:GITHUB_TOKEN]") {
		t.Errorf("stored payload does not name the redacted variable: %s", stored)
	}

	// The row must still parse as the typed payload, so the renderers keep
	// working.
	var out payload.ToolResult
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		t.Fatalf("stored payload is no longer valid JSON: %v", err)
	}
	if out.ID != "call_1" || !out.Success {
		t.Errorf("redaction damaged neighbouring fields: %+v", out)
	}
	if want := "GITHUB_TOKEN=[redacted:GITHUB_TOKEN]\n"; out.Output != want {
		t.Errorf("Output = %q, want %q", out.Output, want)
	}
}

func TestWriteEvent_MsgAssistantPayloadCarriesNoCredentialValue(t *testing.T) {
	d := redactingTestDB(t)

	body, err := json.Marshal(payload.MsgAssistant{
		MessageID: "msg_1",
		Text:      "The key is " + fakeAnthropicKey + " — do not share it.",
		Agent:     "worker",
		Model:     "anthropic/claude",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeEventWithPayload(t, d, "msg_assistant", string(body))

	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, fakeAnthropicKey) {
		t.Fatal("stored msg_assistant payload still carries the credential value")
	}
	if !strings.Contains(stored, "[redacted:ANTHROPIC_API_KEY]") {
		t.Errorf("stored payload does not name the redacted variable: %s", stored)
	}
}

// TestWriteEvent_EveryFreeTextEventTypeIsCovered pins the "any other captured
// frame carrying free text" half of the AC: redaction is applied to the raw
// payload column, so it does not depend on the event type at all.
func TestWriteEvent_EveryFreeTextEventTypeIsCovered(t *testing.T) {
	types := []struct {
		eventType string
		body      any
	}{
		{"msg_user", payload.MsgUser{MessageID: "m1", Text: "token " + fakeGitHubToken}},
		{"thinking", payload.Thinking{MessageID: "m1", Text: "token " + fakeGitHubToken}},
		{"error", payload.ErrorEvent{Note: "failed with " + fakeGitHubToken}},
		{"compaction", payload.Compaction{Note: "carried " + fakeGitHubToken}},
		{"audit", payload.Audit{Tool: "bash", Command: "curl -H 'Authorization: Bearer " + fakeGitHubToken + "'", SessionName: fakeSessionName}},
		{"provider_error", payload.ProviderError{Provider: "anthropic", Message: "401 for " + fakeGitHubToken}},
		{"tool_call", map[string]any{"name": "bash", "id": "c1", "args": map[string]string{"command": "echo " + fakeGitHubToken}}},
	}

	for _, tc := range types {
		t.Run(tc.eventType, func(t *testing.T) {
			d := redactingTestDB(t)
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			writeEventWithPayload(t, d, tc.eventType, string(body))

			stored := storedPayload(t, d, fakeSessionName)
			if strings.Contains(stored, fakeGitHubToken) {
				t.Fatalf("%s payload still carries the credential value: %s", tc.eventType, stored)
			}
			if !strings.Contains(stored, "[redacted:GITHUB_TOKEN]") {
				t.Errorf("%s payload does not name the redacted variable: %s", tc.eventType, stored)
			}
		})
	}
}

func TestWriteEventReturningRowID_RedactsToo(t *testing.T) {
	d := redactingTestDB(t)

	rowID, err := d.WriteEventReturningRowID(db.Event{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Repo:        syntheticEventRepo,
		Type:        "tool_result",
		Payload:     `{"output":"` + fakeGitHubToken + `"}`,
	})
	if err != nil {
		t.Fatalf("WriteEventReturningRowID: %v", err)
	}
	if rowID <= 0 {
		t.Fatalf("rowID = %d; want a positive rowid", rowID)
	}

	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, fakeGitHubToken) {
		t.Fatal("stored payload still carries the credential value")
	}
}

func TestWriteHarnessFrame_RedactsTheRawWireArchive(t *testing.T) {
	d := redactingTestDB(t)

	raw := `{"type":"tool_result","id":"c1","output":"` + fakeGitHubToken + `"}`
	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Direction:   db.HarnessFrameDirectionIn,
		Type:        "tool_result",
		Payload:     raw,
	}); err != nil {
		t.Fatalf("WriteHarnessFrame: %v", err)
	}

	frames, err := d.QueryHarnessFrames(fakeSessionName, "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if strings.Contains(frames[0].Payload, fakeGitHubToken) {
		t.Fatal("stored harness frame still carries the credential value")
	}
	if !strings.Contains(frames[0].Payload, "[redacted:GITHUB_TOKEN]") {
		t.Errorf("stored frame does not name the redacted variable: %s", frames[0].Payload)
	}
}

func TestWriteEvent_PayloadWithoutCredentialIsStoredByteForByte(t *testing.T) {
	d := redactingTestDB(t)

	raw := `{"type":"tool_result","id":"call_1","success":true,"output":"PASS\nok\tgithub.com/prismatic-koi/prism/internal/db\t0.412s\n","truncated":false}`
	writeEventWithPayload(t, d, "tool_result", raw)

	if stored := storedPayload(t, d, fakeSessionName); stored != raw {
		t.Errorf("payload was modified:\n got %q\nwant %q", stored, raw)
	}
}

// TestWriteEvent_ShapeLayerCoversAHarnessWithNoValueKnowledge is the
// defence-in-depth half: the handle's redactor knows no values at all, which
// is what a frame from a future harness looks like to this layer.
func TestWriteEvent_ShapeLayerCoversAHarnessWithNoValueKnowledge(t *testing.T) {
	d := openTestDB(t)
	d.SetRedactor(payload.NewShapeOnlyRedactor())

	shaped := "ghp_" + strings.Repeat("A", 36)
	writeEventWithPayload(t, d, "tool_result", `{"output":"`+shaped+`"}`)

	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, shaped) {
		t.Fatal("shape-matched credential survived the write-time control")
	}
	if !strings.Contains(stored, "[redacted:github-token]") {
		t.Errorf("stored payload does not name the redacted shape: %s", stored)
	}
}

// TestWriteEvent_SubprocessStdoutCredentialIsAbsentFromTheRow is the
// end-to-end shape of the reported defect: a command an agent runs prints a
// credential, the harness captures stdout into a tool_result, and the row is
// written. The credential must not be in the row.
func TestWriteEvent_SubprocessStdoutCredentialIsAbsentFromTheRow(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}

	d := redactingTestDB(t)

	// The subprocess reads the synthetic value from its own environment and
	// prints it, exactly as `env` or a config-dumping tool would.
	cmd := exec.Command(sh, "-c", `printf 'GITHUB_TOKEN=%s\n' "$GITHUB_TOKEN"`)
	cmd.Env = append(os.Environ(), "GITHUB_TOKEN="+fakeGitHubToken)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("subprocess: %v", err)
	}
	if !strings.Contains(string(out), fakeGitHubToken) {
		t.Fatal("test setup is wrong: the subprocess did not print the synthetic value")
	}

	body, err := json.Marshal(payload.ToolResult{ID: "call_1", Success: true, Output: string(out)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeEventWithPayload(t, d, "tool_result", string(body))

	stored := storedPayload(t, d, fakeSessionName)
	if strings.Contains(stored, fakeGitHubToken) {
		t.Fatal("the value printed by the subprocess reached the database row")
	}
}

func TestSetRedactor_NilRestoresTheProcessDefault(t *testing.T) {
	d := redactingTestDB(t)
	d.SetRedactor(nil)

	// The process default knows no synthetic value, so the value layer
	// cannot match — but the shape layer still runs. Assert on the shape
	// layer so the test does not depend on the developer's environment.
	shaped := "ghp_" + strings.Repeat("Z", 36)
	writeEventWithPayload(t, d, "tool_result", `{"output":"`+shaped+`"}`)

	if stored := storedPayload(t, d, fakeSessionName); strings.Contains(stored, shaped) {
		t.Error("process-default redactor did not apply the shape layer")
	}
}

// ---------------------------------------------------------------------------
// JSON-structure safety at write time.
//
// Regression tests for the round-1 review finding on PR #2606: the write-time
// control applied the shape regexp to the serialised payload with no
// structural awareness, so the private-key-block shape could span a JSON
// delimiter and either store invalid JSON or silently delete fields. The
// intact original is never stored, so the damage was permanent.
// ---------------------------------------------------------------------------

// spanningPayloads are the reproducers from the review. None of them holds a
// complete PEM block inside any single field, so a structurally-aware
// redactor must leave every one of them exactly as written.
func spanningPayloads() []string {
	return []string{
		`{"a":"-----BEGIN A PRIVATE KEY-----","b":[1,"-----END A PRIVATE KEY-----"]}`,
		`{"tool":"edit","args":{"a_old":"x -----BEGIN RSA PRIVATE KEY-----","z_new":"-----END RSA PRIVATE KEY-----"},"n":[1,2]}`,
		`{"content":[{"text":"a -----BEGIN RSA PRIVATE KEY-----"},{"text":"-----END RSA PRIVATE KEY-----"}],"truncated":false}`,
	}
}

func TestWriteEvent_ShapeCannotSpanAJSONDelimiter(t *testing.T) {
	for i, raw := range spanningPayloads() {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			d := openTestDB(t)
			d.SetRedactor(payload.NewShapeOnlyRedactor())
			writeEventWithPayload(t, d, "tool_result", raw)

			stored := storedPayload(t, d, fakeSessionName)
			if !json.Valid([]byte(stored)) {
				t.Fatalf("stored payload is not valid JSON: %s", stored)
			}
			if stored != raw {
				t.Errorf("payload was rewritten although no field holds a complete block:\n  got:  %s\n  want: %s", stored, raw)
			}
		})
	}
}

func TestWriteEventReturningRowID_ShapeCannotSpanAJSONDelimiter(t *testing.T) {
	raw := spanningPayloads()[0]
	d := openTestDB(t)
	d.SetRedactor(payload.NewShapeOnlyRedactor())

	if _, err := d.WriteEventReturningRowID(db.Event{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Repo:        syntheticEventRepo,
		Type:        "tool_result",
		Payload:     raw,
	}); err != nil {
		t.Fatalf("WriteEventReturningRowID: %v", err)
	}

	stored := storedPayload(t, d, fakeSessionName)
	if !json.Valid([]byte(stored)) {
		t.Fatalf("stored payload is not valid JSON: %s", stored)
	}
	if stored != raw {
		t.Errorf("payload was rewritten:\n  got:  %s\n  want: %s", stored, raw)
	}
}

func TestWriteHarnessFrame_ShapeCannotSpanAJSONDelimiter(t *testing.T) {
	raw := spanningPayloads()[0]
	d := openTestDB(t)
	d.SetRedactor(payload.NewShapeOnlyRedactor())

	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: fakeSessionName,
		Direction:   db.HarnessFrameDirectionIn,
		Type:        "tool_result",
		Payload:     raw,
	}); err != nil {
		t.Fatalf("WriteHarnessFrame: %v", err)
	}

	frames, err := d.QueryHarnessFrames(fakeSessionName, "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if !json.Valid([]byte(frames[0].Payload)) {
		t.Fatalf("stored frame is not valid JSON: %s", frames[0].Payload)
	}
	if frames[0].Payload != raw {
		t.Errorf("frame was rewritten:\n  got:  %s\n  want: %s", frames[0].Payload, raw)
	}
}

// TestWriteEvent_CompleteBlockInOneFieldIsStillRedacted pins the other half:
// refusing to span must not switch the shape layer off.
func TestWriteEvent_CompleteBlockInOneFieldIsStillRedacted(t *testing.T) {
	d := openTestDB(t)
	d.SetRedactor(payload.NewShapeOnlyRedactor())

	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGlj\n-----END OPENSSH PRIVATE KEY-----"
	body, err := json.Marshal(payload.ToolResult{ID: "call_1", Success: true, Output: pem})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeEventWithPayload(t, d, "tool_result", string(body))

	stored := storedPayload(t, d, fakeSessionName)
	if !json.Valid([]byte(stored)) {
		t.Fatalf("stored payload is not valid JSON: %s", stored)
	}
	if strings.Contains(stored, "PRIVATE KEY") {
		t.Errorf("a complete block inside one field was not redacted: %s", stored)
	}
	if !strings.Contains(stored, "[redacted:private-key-block]") {
		t.Errorf("stored payload does not name the redacted shape: %s", stored)
	}
}
