package piexport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── fixture builders ──────────────────────────────────────────────────────────

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal for %s: %v", path, err)
	}
	mustWriteFile(t, path, data)
}

// buildArchiveDir creates a self-contained archive directory under t.TempDir()
// with the given raw/ subtree and returns the archive root path.
func buildArchiveDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "raw", "messages"))
	mustMkdir(t, filepath.Join(dir, "raw", "parts"))
	return dir
}

// ── shared session JSON ───────────────────────────────────────────────────────

func makeSession(id, directory string, createdMS int64) map[string]any {
	return map[string]any{
		"id":        id,
		"directory": directory,
		"slug":      "test-session",
		"time":      map[string]any{"created": createdMS},
	}
}

// ── helpers for parsing output ───────────────────────────────────────────────

type rawEntry map[string]json.RawMessage

func parseJSONL(t *testing.T, path string) []rawEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	var entries []rawEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e rawEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %s", lineNum, err, line)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return entries
}

func entryType(t *testing.T, e rawEntry) string {
	t.Helper()
	var v string
	if err := json.Unmarshal(e["type"], &v); err != nil {
		t.Fatalf("entry.type: %v", err)
	}
	return v
}

func entryID(t *testing.T, e rawEntry) string {
	t.Helper()
	var v string
	if err := json.Unmarshal(e["id"], &v); err != nil {
		t.Fatalf("entry.id: %v", err)
	}
	return v
}

func entryParentID(t *testing.T, e rawEntry) *string {
	t.Helper()
	raw, ok := e["parentId"]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("entry.parentId: %v", err)
	}
	return &v
}

func entryMessage(t *testing.T, e rawEntry) map[string]json.RawMessage {
	t.Helper()
	var v map[string]json.RawMessage
	if err := json.Unmarshal(e["message"], &v); err != nil {
		t.Fatalf("entry.message: %v", err)
	}
	return v
}

func messageRole(t *testing.T, msg map[string]json.RawMessage) string {
	t.Helper()
	var v string
	if err := json.Unmarshal(msg["role"], &v); err != nil {
		t.Fatalf("message.role: %v", err)
	}
	return v
}

// ── fixture 1: simple linear text conversation ────────────────────────────────

// TestFixtureLinearText covers a simple user→assistant text-only exchange.
// It also validates the linear id/parentId chain and round-trip stability.
func TestFixtureLinearText(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	sessionID := "ses_lineartest"
	createdMS := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).UnixMilli()

	// session.json
	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession(sessionID, "/home/user/project", createdMS))

	// User message.
	userMsgID := "msg_aaa0000001user"
	mustWriteJSON(t, filepath.Join(rawDir, "messages", userMsgID+".json"), map[string]any{
		"id":        userMsgID,
		"sessionID": sessionID,
		"role":      "user",
		"time":      map[string]any{"created": createdMS + 1000},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", userMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", userMsgID, "prt_aaa0000001text.json"), map[string]any{
		"id":        "prt_aaa0000001text",
		"messageID": userMsgID,
		"type":      "text",
		"text":      "What is 2 + 2?",
	})

	// Assistant message.
	asstMsgID := "msg_bbb0000001asst"
	asstCreated := createdMS + 2000
	mustWriteJSON(t, filepath.Join(rawDir, "messages", asstMsgID+".json"), map[string]any{
		"id":         asstMsgID,
		"sessionID":  sessionID,
		"role":       "assistant",
		"parentID":   userMsgID,
		"modelID":    "claude-sonnet-4-5",
		"providerID": "anthropic",
		"time":       map[string]any{"created": asstCreated, "completed": asstCreated + 500},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", asstMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_bbb0000001step.json"), map[string]any{
		"id":        "prt_bbb0000001step",
		"messageID": asstMsgID,
		"type":      "step-start",
		"snapshot":  "abc123def456abc123def456abc123def456abc1",
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_bbb0000002text.json"), map[string]any{
		"id":        "prt_bbb0000002text",
		"messageID": asstMsgID,
		"type":      "text",
		"text":      "4",
		"time":      map[string]any{"start": asstCreated + 100, "end": asstCreated + 200},
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_bbb0000003fin.json"), map[string]any{
		"id":        "prt_bbb0000003fin",
		"messageID": asstMsgID,
		"type":      "step-finish",
		"reason":    "stop",
		"cost":      0.001,
		"tokens": map[string]any{
			"input": 10, "output": 5, "reasoning": 0,
			"cache": map[string]any{"read": 0, "write": 0},
		},
	})

	// Run translator.
	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	jsonlPath := filepath.Join(archDir, "session.jsonl")
	entries := parseJSONL(t, jsonlPath)

	// Must have at least: header + snapshot + user-msg + asst-msg = 4 entries.
	if len(entries) < 4 {
		t.Fatalf("expected ≥4 entries, got %d", len(entries))
	}

	// First entry: session header.
	hdr := entries[0]
	if entryType(t, hdr) != "session" {
		t.Errorf("entries[0].type = %q, want %q", entryType(t, hdr), "session")
	}
	var hdrVersion int
	if err := json.Unmarshal(hdr["version"], &hdrVersion); err != nil || hdrVersion != 3 {
		t.Errorf("header.version = %v, want 3", hdrVersion)
	}
	var hdrID string
	if err := json.Unmarshal(hdr["id"], &hdrID); err != nil || hdrID != sessionID {
		t.Errorf("header.id = %q, want %q", hdrID, sessionID)
	}

	// Validate linear chain for non-header entries.
	validateLinearChain(t, entries[1:])

	// Round-trip stability: parse the JSONL into raw maps, re-serialize each
	// line, and verify byte-equality with the original.
	checkRoundTrip(t, jsonlPath)
}

// validateLinearChain checks that entries (excluding the session header) form
// a valid linear parentId chain and that all IDs are unique 8-char hex.
func validateLinearChain(t *testing.T, entries []rawEntry) {
	t.Helper()
	seen := map[string]bool{}
	var prevID *string
	for i, e := range entries {
		if entryType(t, e) == "session" {
			continue // header has no id/parentId
		}
		id := entryID(t, e)
		if len(id) != 8 {
			t.Errorf("entry[%d].id = %q, want 8-char hex", i, id)
		}
		// Validate hex characters.
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("entry[%d].id = %q contains non-hex character %c", i, id, c)
			}
		}
		if seen[id] {
			t.Errorf("entry[%d].id = %q is duplicate", i, id)
		}
		seen[id] = true

		parentID := entryParentID(t, e)
		if i == 0 {
			if parentID != nil {
				t.Errorf("first non-header entry has parentId = %q, want null", *parentID)
			}
		} else {
			if parentID == nil {
				t.Errorf("entry[%d] has parentId = null, want %q", i, *prevID)
			} else if *parentID != *prevID {
				t.Errorf("entry[%d].parentId = %q, want %q", i, *parentID, *prevID)
			}
		}
		prevID = &id
	}
}

// ── fixture 2: tool calls with sidecar ───────────────────────────────────────

// TestFixtureToolCalls covers an assistant message with tool calls, including
// the tool-output sidecar file.
func TestFixtureToolCalls(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	sessionID := "ses_tooltest001"
	createdMS := time.Date(2026, 2, 10, 8, 0, 0, 0, time.UTC).UnixMilli()

	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession(sessionID, "/home/user/repo", createdMS))

	// User message asking to run a bash command.
	userMsgID := "msg_tooluser0001"
	mustWriteJSON(t, filepath.Join(rawDir, "messages", userMsgID+".json"), map[string]any{
		"id":        userMsgID,
		"sessionID": sessionID,
		"role":      "user",
		"time":      map[string]any{"created": createdMS + 500},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", userMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", userMsgID, "prt_tooluser0001text.json"), map[string]any{
		"id":        "prt_tooluser0001text",
		"messageID": userMsgID,
		"type":      "text",
		"text":      "Run ls in the current directory",
	})

	// Assistant message with tool call.
	asstMsgID := "msg_toolasst0001"
	asstCreated := createdMS + 2000
	mustWriteJSON(t, filepath.Join(rawDir, "messages", asstMsgID+".json"), map[string]any{
		"id":         asstMsgID,
		"sessionID":  sessionID,
		"role":       "assistant",
		"parentID":   userMsgID,
		"modelID":    "claude-opus-4",
		"providerID": "anthropic",
		"time":       map[string]any{"created": asstCreated, "completed": asstCreated + 3000},
	})

	toolCallID := "toolu_01ExampleBashCall"
	toolOutputContent := strings.Repeat("file\n", 3000) // large enough to simulate sidecar
	sidecarID := "tool_01SidecarULIDxyz"
	mustMkdir(t, filepath.Join(rawDir, "tool-output"))
	mustWriteFile(t, filepath.Join(rawDir, "tool-output", sidecarID), []byte(toolOutputContent))

	mustMkdir(t, filepath.Join(rawDir, "parts", asstMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_toolasst0001step.json"), map[string]any{
		"id":        "prt_toolasst0001step",
		"messageID": asstMsgID,
		"type":      "step-start",
		"snapshot":  "",
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_toolasst0001text.json"), map[string]any{
		"id":        "prt_toolasst0001text",
		"messageID": asstMsgID,
		"type":      "text",
		"text":      "I'll run ls for you.",
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_toolasst0001tool.json"), map[string]any{
		"id":        "prt_toolasst0001tool",
		"messageID": asstMsgID,
		"type":      "tool",
		"callID":    toolCallID,
		"tool":      "bash",
		"asset":     sidecarID, // legacy sidecar reference
		"state": map[string]any{
			"status": "completed",
			"input":  map[string]any{"command": "ls"},
			"output": "truncated output...", // will be replaced by sidecar
			"title":  "ls",
			"time":   map[string]any{"start": asstCreated + 500, "end": asstCreated + 1500},
		},
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_toolasst0001fin.json"), map[string]any{
		"id":        "prt_toolasst0001fin",
		"messageID": asstMsgID,
		"type":      "step-finish",
		"reason":    "tool-calls",
		"cost":      0.005,
		"tokens": map[string]any{
			"input": 50, "output": 30, "reasoning": 0,
			"cache": map[string]any{"read": 10, "write": 5},
		},
	})

	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	jsonlPath := filepath.Join(archDir, "session.jsonl")
	entries := parseJSONL(t, jsonlPath)

	// Expected entries: header + user-msg + asst-msg + toolResult-msg
	// The step-start has no snapshot so no custom entry.
	if len(entries) < 4 {
		t.Fatalf("expected ≥4 entries, got %d", len(entries))
	}

	validateLinearChain(t, entries[1:])

	// Find the toolResult entry.
	var toolResultEntry rawEntry
	for _, e := range entries {
		if entryType(t, e) == "message" {
			msg := entryMessage(t, e)
			if messageRole(t, msg) == "toolResult" {
				toolResultEntry = e
				break
			}
		}
	}
	if toolResultEntry == nil {
		t.Fatal("no toolResult entry found")
	}

	// Verify the toolResult references the correct toolCallId.
	msg := entryMessage(t, toolResultEntry)
	var toolCallIDGot string
	if err := json.Unmarshal(msg["toolCallId"], &toolCallIDGot); err != nil {
		t.Fatalf("toolResult.toolCallId: %v", err)
	}
	if toolCallIDGot != toolCallID {
		t.Errorf("toolResult.toolCallId = %q, want %q", toolCallIDGot, toolCallID)
	}

	// Verify sidecar content appears in the toolResult.
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(msg["content"], &content); err != nil {
		t.Fatalf("toolResult.content: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("toolResult.content is empty")
	}
	var textVal string
	if err := json.Unmarshal(content[0]["text"], &textVal); err != nil {
		t.Fatalf("toolResult.content[0].text: %v", err)
	}
	if textVal != toolOutputContent {
		t.Errorf("toolResult sidecar content mismatch: got %q, want sidecar content", textVal[:min(50, len(textVal))])
	}

	// Round-trip stability.
	checkRoundTrip(t, jsonlPath)
}

// ── fixture 3: thinking blocks ────────────────────────────────────────────────

// TestFixtureThinkingBlocks covers an assistant message with reasoning parts
// (mapped to pi ThinkingContent blocks).
func TestFixtureThinkingBlocks(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	sessionID := "ses_thinktest001"
	createdMS := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).UnixMilli()

	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession(sessionID, "/home/user/think-project", createdMS))

	// User message.
	userMsgID := "msg_thinkuser0001"
	mustWriteJSON(t, filepath.Join(rawDir, "messages", userMsgID+".json"), map[string]any{
		"id":        userMsgID,
		"sessionID": sessionID,
		"role":      "user",
		"time":      map[string]any{"created": createdMS + 1000},
		"system":    []string{"You are a helpful assistant.", "Think carefully before responding."},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", userMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", userMsgID, "prt_thinkuser0001t.json"), map[string]any{
		"id":        "prt_thinkuser0001t",
		"messageID": userMsgID,
		"type":      "text",
		"text":      "What is the meaning of life?",
	})

	// Assistant message with thinking block.
	asstMsgID := "msg_thinkasst0001"
	asstCreated := createdMS + 5000
	mustWriteJSON(t, filepath.Join(rawDir, "messages", asstMsgID+".json"), map[string]any{
		"id":         asstMsgID,
		"sessionID":  sessionID,
		"role":       "assistant",
		"parentID":   userMsgID,
		"modelID":    "claude-opus-4-5",
		"providerID": "anthropic",
		"time":       map[string]any{"created": asstCreated, "completed": asstCreated + 4000},
		"system":     []string{"You are a helpful assistant.", "Think carefully before responding."},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", asstMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_thinkasst0001s.json"), map[string]any{
		"id":        "prt_thinkasst0001s",
		"messageID": asstMsgID,
		"type":      "step-start",
		"snapshot":  "deadbeefdeadbeefdeadbeefdeadbeef01234567",
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_thinkasst0001r.json"), map[string]any{
		"id":        "prt_thinkasst0001r",
		"messageID": asstMsgID,
		"type":      "reasoning",
		"text":      "The question asks about the meaning of life. This is a philosophical question...",
		"time":      map[string]any{"start": asstCreated + 100, "end": asstCreated + 2000},
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_thinkasst0001t.json"), map[string]any{
		"id":        "prt_thinkasst0001t",
		"messageID": asstMsgID,
		"type":      "text",
		"text":      "The meaning of life is 42.",
		"time":      map[string]any{"start": asstCreated + 2000, "end": asstCreated + 3000},
	})
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_thinkasst0001f.json"), map[string]any{
		"id":        "prt_thinkasst0001f",
		"messageID": asstMsgID,
		"type":      "step-finish",
		"reason":    "end_turn",
		"cost":      0.01,
		"tokens": map[string]any{
			"input": 100, "output": 50, "reasoning": 200,
			"cache": map[string]any{"read": 20, "write": 10},
		},
	})

	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	jsonlPath := filepath.Join(archDir, "session.jsonl")
	entries := parseJSONL(t, jsonlPath)

	// Expected: header + system-custom + snapshot-custom + user-msg + asst-msg
	// (system is on user msg, snapshot is on asst msg)
	if len(entries) < 5 {
		t.Fatalf("expected ≥5 entries, got %d", len(entries))
	}

	validateLinearChain(t, entries[1:])

	// Find the assistant message entry.
	var asstEntry rawEntry
	for _, e := range entries {
		if entryType(t, e) == "message" {
			msg := entryMessage(t, e)
			if messageRole(t, msg) == "assistant" {
				asstEntry = e
				break
			}
		}
	}
	if asstEntry == nil {
		t.Fatal("no assistant message entry found")
	}

	// Verify thinking block appears in content.
	msg := entryMessage(t, asstEntry)
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(msg["content"], &content); err != nil {
		t.Fatalf("assistant.content: %v", err)
	}

	hasThinking := false
	for _, block := range content {
		var blockType string
		if err := json.Unmarshal(block["type"], &blockType); err == nil && blockType == "thinking" {
			hasThinking = true
			break
		}
	}
	if !hasThinking {
		t.Error("assistant message content has no thinking block")
	}

	// Verify a snapshot custom entry exists.
	hasSnapshot := false
	for _, e := range entries {
		if entryType(t, e) == "custom" {
			var ct string
			if err := json.Unmarshal(e["customType"], &ct); err == nil && ct == "opencode.snapshot.start" {
				hasSnapshot = true
				break
			}
		}
	}
	if !hasSnapshot {
		t.Error("no opencode.snapshot.start custom entry found")
	}

	// Verify system custom entry.
	hasSystem := false
	for _, e := range entries {
		if entryType(t, e) == "custom" {
			var ct string
			if err := json.Unmarshal(e["customType"], &ct); err == nil && ct == "opencode.system" {
				hasSystem = true
				break
			}
		}
	}
	if !hasSystem {
		t.Error("no opencode.system custom entry found")
	}

	// Verify stopReason is "stop" (mapped from "end_turn").
	var stopReason string
	if err := json.Unmarshal(msg["stopReason"], &stopReason); err != nil {
		t.Fatalf("assistant.stopReason: %v", err)
	}
	if stopReason != "stop" {
		t.Errorf("assistant.stopReason = %q, want %q", stopReason, "stop")
	}

	// Verify usage.
	var usage piUsage
	if err := json.Unmarshal(msg["usage"], &usage); err != nil {
		t.Fatalf("assistant.usage: %v", err)
	}
	if usage.Input != 100 || usage.Output != 50 || usage.CacheRead != 20 || usage.CacheWrite != 10 {
		t.Errorf("assistant.usage = %+v, want input=100 output=50 cacheRead=20 cacheWrite=10", usage)
	}

	// Round-trip stability.
	checkRoundTrip(t, jsonlPath)
}

// ── edge case tests ───────────────────────────────────────────────────────────

// TestZeroMessages verifies a session with no messages produces only the header.
func TestZeroMessages(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession("ses_empty001", "/home/user/empty", 0))

	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	entries := parseJSONL(t, filepath.Join(archDir, "session.jsonl"))
	if len(entries) != 1 {
		t.Errorf("expected 1 entry (header only), got %d", len(entries))
	}
	if entryType(t, entries[0]) != "session" {
		t.Errorf("entries[0].type = %q, want %q", entryType(t, entries[0]), "session")
	}
}

// TestAbortedAssistant verifies that an interrupted assistant message (no
// step-finish) produces a valid entry with stopReason "aborted" and zero usage.
func TestAbortedAssistant(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	sessionID := "ses_aborted001"
	createdMS := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC).UnixMilli()

	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession(sessionID, "/home/user/proj", createdMS))

	userMsgID := "msg_abortuser0001"
	mustWriteJSON(t, filepath.Join(rawDir, "messages", userMsgID+".json"), map[string]any{
		"id":        userMsgID,
		"sessionID": sessionID,
		"role":      "user",
		"time":      map[string]any{"created": createdMS + 100},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", userMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", userMsgID, "prt_abortuser0001t.json"), map[string]any{
		"id":        "prt_abortuser0001t",
		"messageID": userMsgID,
		"type":      "text",
		"text":      "Do something slow",
	})

	// Assistant message with NO step-finish part (interrupted mid-stream).
	asstMsgID := "msg_abortasst0001"
	mustWriteJSON(t, filepath.Join(rawDir, "messages", asstMsgID+".json"), map[string]any{
		"id":         asstMsgID,
		"sessionID":  sessionID,
		"role":       "assistant",
		"parentID":   userMsgID,
		"modelID":    "claude-sonnet-4-5",
		"providerID": "anthropic",
		"time":       map[string]any{"created": createdMS + 500},
	})
	mustMkdir(t, filepath.Join(rawDir, "parts", asstMsgID))
	mustWriteJSON(t, filepath.Join(rawDir, "parts", asstMsgID, "prt_abortasst0001t.json"), map[string]any{
		"id":        "prt_abortasst0001t",
		"messageID": asstMsgID,
		"type":      "text",
		"text":      "I was going to say something but I got interrupted",
	})
	// No step-finish part — simulates abort mid-stream.

	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	entries := parseJSONL(t, filepath.Join(archDir, "session.jsonl"))
	validateLinearChain(t, entries[1:])

	// Find the assistant entry.
	var asstEntry rawEntry
	for _, e := range entries {
		if entryType(t, e) == "message" {
			msg := entryMessage(t, e)
			if messageRole(t, msg) == "assistant" {
				asstEntry = e
				break
			}
		}
	}
	if asstEntry == nil {
		t.Fatal("no assistant entry found")
	}

	msg := entryMessage(t, asstEntry)

	// stopReason must be "aborted".
	var stopReason string
	if err := json.Unmarshal(msg["stopReason"], &stopReason); err != nil {
		t.Fatalf("assistant.stopReason: %v", err)
	}
	if stopReason != "aborted" {
		t.Errorf("stopReason = %q, want %q", stopReason, "aborted")
	}

	// usage must be present with zero values.
	var usage piUsage
	if err := json.Unmarshal(msg["usage"], &usage); err != nil {
		t.Fatalf("assistant.usage: %v", err)
	}
	if usage.Input != 0 || usage.Output != 0 {
		t.Errorf("expected zero usage for aborted message, got %+v", usage)
	}
}

// TestAtomicWrite verifies that translator failure leaves no partial session.jsonl.
func TestAtomicWrite(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	// Write a malformed session.json to trigger an error.
	mustWriteFile(t, filepath.Join(rawDir, "session.json"), []byte("not valid json"))

	err := Translate(archDir)
	if err == nil {
		t.Fatal("expected error from malformed session.json, got nil")
	}

	// No partial session.jsonl should exist.
	if _, statErr := os.Stat(filepath.Join(archDir, "session.jsonl")); !os.IsNotExist(statErr) {
		t.Error("session.jsonl exists after failed Translate() — should have been cleaned up")
	}
}

// TestOverwriteAtomic verifies that a second Translate() call overwrites an
// existing session.jsonl atomically.
func TestOverwriteAtomic(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession("ses_overwrite001", "/home/user/proj", 1000000))

	// First run.
	if err := Translate(archDir); err != nil {
		t.Fatalf("first Translate(): %v", err)
	}
	// Second run — must succeed and overwrite.
	if err := Translate(archDir); err != nil {
		t.Fatalf("second Translate(): %v", err)
	}
	entries := parseJSONL(t, filepath.Join(archDir, "session.jsonl"))
	if len(entries) == 0 {
		t.Error("session.jsonl is empty after overwrite")
	}
}

// TestIDUniqueness verifies that all entry IDs in a multi-message session are unique.
func TestIDUniqueness(t *testing.T) {
	archDir := buildArchiveDir(t)
	rawDir := filepath.Join(archDir, "raw")

	sessionID := "ses_iduniq0001"
	createdMS := time.Now().UnixMilli()
	mustWriteJSON(t, filepath.Join(rawDir, "session.json"),
		makeSession(sessionID, "/home/user/proj", createdMS))

	// Create 10 user+assistant message pairs.
	prevUserID := ""
	for i := 0; i < 10; i++ {
		userID := "msg_iduniq" + strings.Repeat("0", 6-len(strings.Repeat("", i))) + strings.Repeat("u", i+1)
		asstID := "msg_iduniq" + strings.Repeat("0", 6-len(strings.Repeat("", i))) + strings.Repeat("a", i+1)
		_ = prevUserID

		mustWriteJSON(t, filepath.Join(rawDir, "messages", userID+".json"), map[string]any{
			"id":        userID,
			"sessionID": sessionID,
			"role":      "user",
			"time":      map[string]any{"created": createdMS + int64(i*1000)},
		})
		mustMkdir(t, filepath.Join(rawDir, "parts", userID))
		mustWriteJSON(t, filepath.Join(rawDir, "parts", userID, "prt_"+userID[:min(len(userID), 8)]+"t.json"), map[string]any{
			"id":        "prt_" + userID[:min(len(userID), 8)] + "t",
			"messageID": userID,
			"type":      "text",
			"text":      "Question " + userID,
		})

		mustWriteJSON(t, filepath.Join(rawDir, "messages", asstID+".json"), map[string]any{
			"id":         asstID,
			"sessionID":  sessionID,
			"role":       "assistant",
			"parentID":   userID,
			"modelID":    "claude-sonnet-4-5",
			"providerID": "anthropic",
			"time":       map[string]any{"created": createdMS + int64(i*1000) + 100},
		})
		mustMkdir(t, filepath.Join(rawDir, "parts", asstID))
		mustWriteJSON(t, filepath.Join(rawDir, "parts", asstID, "prt_"+asstID[:min(len(asstID), 8)]+"t.json"), map[string]any{
			"id":        "prt_" + asstID[:min(len(asstID), 8)] + "t",
			"messageID": asstID,
			"type":      "text",
			"text":      "Answer " + asstID,
		})
		mustWriteJSON(t, filepath.Join(rawDir, "parts", asstID, "prt_"+asstID[:min(len(asstID), 8)]+"f.json"), map[string]any{
			"id":        "prt_" + asstID[:min(len(asstID), 8)] + "f",
			"messageID": asstID,
			"type":      "step-finish",
			"reason":    "stop",
			"cost":      0.0,
			"tokens": map[string]any{
				"input": 5, "output": 3, "reasoning": 0,
				"cache": map[string]any{"read": 0, "write": 0},
			},
		})
		prevUserID = userID
	}

	if err := Translate(archDir); err != nil {
		t.Fatalf("Translate(): %v", err)
	}

	entries := parseJSONL(t, filepath.Join(archDir, "session.jsonl"))
	seen := map[string]bool{}
	for _, e := range entries {
		if entryType(t, e) == "session" {
			continue
		}
		id := entryID(t, e)
		if seen[id] {
			t.Errorf("duplicate entry ID: %s", id)
		}
		seen[id] = true
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// checkRoundTrip verifies that each line of the JSONL at path can be
// round-tripped: parsed into a generic map and re-serialized to a canonical
// (alphabetically sorted) form, and that the canonical form equals the
// canonical form of the original line. This ensures emitted JSON is valid and
// structurally stable — both the original and re-encoded use canonical key order.
func checkRoundTrip(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("checkRoundTrip ReadFile: %v", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Increase scanner buffer for large tool outputs.
	buf := make([]byte, 0, 512*1024)
	scanner.Buffer(buf, 4*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Parse into generic value (maps get alphabetically sorted on re-encode).
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line %d: json.Unmarshal: %v", lineNum, err)
		}
		// Re-serialize to canonical form.
		canonical, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("line %d: json.Marshal: %v", lineNum, err)
		}
		// Re-parse the canonical form and re-serialize again — must be identical
		// (i.e. the canonical form is a fixed point of json.Marshal).
		var v2 any
		if err := json.Unmarshal(canonical, &v2); err != nil {
			t.Fatalf("line %d: second json.Unmarshal: %v", lineNum, err)
		}
		canonical2, err := json.Marshal(v2)
		if err != nil {
			t.Fatalf("line %d: second json.Marshal: %v", lineNum, err)
		}
		if !bytes.Equal(canonical, canonical2) {
			t.Errorf("line %d: canonical form is not a fixed point of json.Marshal", lineNum)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("checkRoundTrip scanner error: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
