// Package piexport translates an opencode raw session archive into a pi-mono
// v3 JSONL trace file.
//
// # Input layout (archive raw/ subtree)
//
//	raw/
//	  session.json                          — opencode session metadata
//	  messages/msg_*.json                   — per-message records
//	  parts/msg_<id>/prt_*.json             — per-part records
//	  tool-output/tool_*                    — truncated tool output sidecars
//
// # Output
//
//	<archiveDir>/session.jsonl             — pi-mono v3 JSONL trace
//
// The translator is a pure library function: no running opencode server is
// required. It reads the archived files, maps them to pi-mono v3 entries, and
// writes the JSONL atomically (temp file + rename).
package piexport

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ── public API ────────────────────────────────────────────────────────────────

// Translate reads the raw opencode archive at archiveDir/raw/ and writes
// archiveDir/session.jsonl. On error the partial output is discarded and no
// session.jsonl is left behind. An existing session.jsonl is overwritten
// atomically.
func Translate(archiveDir string) error {
	rawDir := filepath.Join(archiveDir, "raw")

	// Read session.json.
	sessData, err := os.ReadFile(filepath.Join(rawDir, "session.json"))
	if err != nil {
		return fmt.Errorf("piexport: read session.json: %w", err)
	}
	var sess ocSession
	if err := json.Unmarshal(sessData, &sess); err != nil {
		return fmt.Errorf("piexport: parse session.json: %w", err)
	}

	// Read all message files.
	rawMsgs, err := readMessages(filepath.Join(rawDir, "messages"))
	if err != nil {
		return fmt.Errorf("piexport: read messages: %w", err)
	}

	// For each message read its parts.
	var allMsgs []msgWithParts
	for _, msg := range rawMsgs {
		parts, partErr := readParts(filepath.Join(rawDir, "parts", msg.ID))
		if partErr != nil {
			return fmt.Errorf("piexport: read parts for %s: %w", msg.ID, partErr)
		}
		allMsgs = append(allMsgs, msgWithParts{msg: msg, parts: parts})
	}

	// Build the JSONL entries.
	entries, err := buildEntries(sess, allMsgs, rawDir)
	if err != nil {
		return fmt.Errorf("piexport: build entries: %w", err)
	}

	// Atomic write: temp file → rename.
	if err := writeJSONL(archiveDir, entries); err != nil {
		return fmt.Errorf("piexport: write session.jsonl: %w", err)
	}
	return nil
}

// ── opencode on-disk types ────────────────────────────────────────────────────

type ocSession struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Slug      string `json:"slug"`
	Time      struct {
		Created int64 `json:"created"` // unix ms
	} `json:"time"`
}

type ocMessage struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Role       string   `json:"role"`       // "user" | "assistant"
	ParentID   string   `json:"parentID"`   // assistant messages have a parentID
	System     []string `json:"system"`     // per-message system prompts (may be nil)
	ModelID    string   `json:"modelID"`    // assistant only
	ProviderID string   `json:"providerID"` // assistant only
	Cost       float64  `json:"cost"`
	Tokens     struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Finish string `json:"finish"` // assistant: the finish reason from the message level
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type ocPart struct {
	ID        string          `json:"id"`
	MessageID string          `json:"messageID"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`     // type=text, type=reasoning
	Snapshot  string          `json:"snapshot"` // type=step-start, step-finish
	CallID    string          `json:"callID"`   // type=tool
	Tool      string          `json:"tool"`     // type=tool
	Asset     string          `json:"asset"`    // legacy type=tool sidecar ref
	State     json.RawMessage `json:"state"`    // type=tool: ToolState JSON
	Reason    string          `json:"reason"`   // type=step-finish
	Cost      float64         `json:"cost"`     // type=step-finish
	Tokens    struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"` // type=step-finish
	Time struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"` // type=text, reasoning, tool state
	// File parts.
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"` // data: URI or file path
}

type ocToolState struct {
	Status string          `json:"status"` // "pending" | "running" | "completed" | "error"
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"` // completed status: tool output text
	Error  string          `json:"error"`  // error status
	Title  string          `json:"title"`
	Time   struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
}

// msgWithParts bundles an opencode message with its ordered parts.
type msgWithParts struct {
	msg   ocMessage
	parts []ocPart
}

// ── pi-mono v3 entry types ────────────────────────────────────────────────────

type piSessionHeader struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

type piEntryBase struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
}

type piMessageEntry struct {
	piEntryBase
	Message piMessage `json:"message"`
}

type piMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	API        string          `json:"api,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Usage      *piUsage        `json:"usage,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	Timestamp  int64           `json:"timestamp"` // unix ms
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

type piUsage struct {
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	CacheWrite  int64   `json:"cacheWrite"`
	TotalTokens int64   `json:"totalTokens"`
	Cost        piCost  `json:"cost"`
}

type piCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

type piCustomEntry struct {
	piEntryBase
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
}

// Content block types.

type piTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type piThinkingContent struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type piImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`     // base64
	MimeType string `json:"mimeType"` // e.g. "image/png"
}

type piToolCallContent struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ── ID generation ─────────────────────────────────────────────────────────────

// newID returns an 8-character lowercase hex string from 4 random bytes.
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func mustID() string {
	id, err := newID()
	if err != nil {
		panic("piexport: crypto/rand failed: " + err.Error())
	}
	return id
}

// ── file I/O helpers ──────────────────────────────────────────────────────────

func readMessages(dir string) ([]ocMessage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var msgs []ocMessage
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), readErr)
		}
		var m ocMessage
		if parseErr := json.Unmarshal(data, &m); parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), parseErr)
		}
		msgs = append(msgs, m)
	}
	// Sort messages by ID (ULID-sortable — time-ordered).
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].ID < msgs[j].ID
	})
	return msgs, nil
}

func readParts(dir string) ([]ocPart, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var parts []ocPart
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), readErr)
		}
		var p ocPart
		if parseErr := json.Unmarshal(data, &p); parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), parseErr)
		}
		parts = append(parts, p)
	}
	// Sort parts by ID (ULID-sortable). Tiebreak by time.start.
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].ID == parts[j].ID {
			return parts[i].Time.Start < parts[j].Time.Start
		}
		return parts[i].ID < parts[j].ID
	})
	return parts, nil
}

// msToISO converts unix milliseconds to ISO 8601 / RFC3339Nano.
func msToISO(ms int64) string {
	if ms == 0 {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// ── main translation logic ────────────────────────────────────────────────────

func buildEntries(sess ocSession, allMsgs []msgWithParts, rawDir string) ([]any, error) {
	var entries []any

	// Session header.
	ts := msToISO(sess.Time.Created)
	header := piSessionHeader{
		Type:      "session",
		Version:   3,
		ID:        sess.ID,
		Timestamp: ts,
		CWD:       sess.Directory,
	}
	entries = append(entries, header)

	// Linear id/parentId chain state.
	var prevID *string

	// emit appends an entry and returns its ID for chaining.
	emit := func(e any) {
		entries = append(entries, e)
	}

	// Track the last emitted system prompts to detect changes.
	var lastSystem []string

	for _, mp := range allMsgs {
		msg := mp.msg
		parts := mp.parts

		msgTS := msToISO(msg.Time.Created)

		switch msg.Role {
		case "user":
			// Emit system custom entry before this user message when system changes.
			if len(msg.System) > 0 && !stringSliceEqual(msg.System, lastSystem) {
				sysEntry, err := buildSystemEntry(msg.System, prevID, msgTS)
				if err != nil {
					return nil, fmt.Errorf("build system entry for %s: %w", msg.ID, err)
				}
				sysID := sysEntry.ID
				emit(sysEntry)
				prevID = &sysID
				lastSystem = append([]string(nil), msg.System...)
			}

			// Build user content blocks.
			content, err := buildUserContent(parts, rawDir)
			if err != nil {
				return nil, fmt.Errorf("build user content for %s: %w", msg.ID, err)
			}
			contentJSON, err := json.Marshal(content)
			if err != nil {
				return nil, fmt.Errorf("marshal user content for %s: %w", msg.ID, err)
			}

			entryID := mustID()
			entry := piMessageEntry{
				piEntryBase: piEntryBase{
					Type:      "message",
					ID:        entryID,
					ParentID:  prevID,
					Timestamp: msgTS,
				},
				Message: piMessage{
					Role:      "user",
					Content:   contentJSON,
					Timestamp: msg.Time.Created,
				},
			}
			emit(entry)
			prevID = &entryID

		case "assistant":
			// Check for a step-start part with a snapshot — emit before the assistant entry.
			for _, p := range parts {
				if p.Type == "step-start" && p.Snapshot != "" {
					snapEntry, err := buildSnapshotEntry(p.Snapshot, prevID, msgTS)
					if err != nil {
						return nil, fmt.Errorf("build snapshot entry for %s: %w", p.ID, err)
					}
					snapID := snapEntry.ID
					emit(snapEntry)
					prevID = &snapID
					break // one snapshot per message
				}
			}

			// Emit system entry if system changed.
			if len(msg.System) > 0 && !stringSliceEqual(msg.System, lastSystem) {
				sysEntry, err := buildSystemEntry(msg.System, prevID, msgTS)
				if err != nil {
					return nil, fmt.Errorf("build system entry for assistant %s: %w", msg.ID, err)
				}
				sysID := sysEntry.ID
				emit(sysEntry)
				prevID = &sysID
				lastSystem = append([]string(nil), msg.System...)
			}

			// Build assistant content + aggregate usage from step-finish parts.
			content, toolInfos, usage, stopReason, err := buildAssistantContent(parts, rawDir)
			if err != nil {
				return nil, fmt.Errorf("build assistant content for %s: %w", msg.ID, err)
			}
			contentJSON, err := json.Marshal(content)
			if err != nil {
				return nil, fmt.Errorf("marshal assistant content for %s: %w", msg.ID, err)
			}

			entryID := mustID()
			entry := piMessageEntry{
				piEntryBase: piEntryBase{
					Type:      "message",
					ID:        entryID,
					ParentID:  prevID,
					Timestamp: msgTS,
				},
				Message: piMessage{
					Role:       "assistant",
					Content:    contentJSON,
					Provider:   msg.ProviderID,
					Model:      msg.ModelID,
					Usage:      usage,
					StopReason: stopReason,
					Timestamp:  msg.Time.Created,
				},
			}
			emit(entry)
			prevID = &entryID

			// Emit one toolResult entry per tool call.
			for i := range toolInfos {
				toolResultEntry, trErr := buildToolResultEntry(toolInfos[i], prevID, rawDir)
				if trErr != nil {
					return nil, fmt.Errorf("build toolResult for callID %s: %w", toolInfos[i].callID, trErr)
				}
				trID := toolResultEntry.ID
				emit(toolResultEntry)
				prevID = &trID
			}
		}
	}

	return entries, nil
}

// ── user message content ──────────────────────────────────────────────────────

func buildUserContent(parts []ocPart, rawDir string) ([]any, error) {
	var content []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				content = append(content, piTextContent{Type: "text", Text: p.Text})
			}
		case "file":
			if isImageMIME(p.Mime) {
				imgContent, err := filePartToImage(p)
				if err != nil {
					slog.Warn("piexport: could not convert file part to image", "part", p.ID, "err", err)
					continue
				}
				content = append(content, imgContent)
			}
			// Non-image file parts (text/plain, dirs) are skipped for user content.
		}
		// Ignore step-start, step-finish, tool, reasoning, etc. for user messages.
	}
	if len(content) == 0 {
		content = append(content, piTextContent{Type: "text", Text: ""})
	}
	return content, nil
}

// ── assistant message content ─────────────────────────────────────────────────

type toolPartInfo struct {
	callID string
	tool   string
	state  ocToolState
	asset  string // legacy sidecar ref
}

func buildAssistantContent(parts []ocPart, rawDir string) (content []any, toolInfos []toolPartInfo, usage *piUsage, stopReason string, err error) {
	var aggUsage piUsage
	var hasStepFinish bool

	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				content = append(content, piTextContent{Type: "text", Text: p.Text})
			}
		case "reasoning":
			if p.Text != "" {
				content = append(content, piThinkingContent{Type: "thinking", Thinking: p.Text})
			}
		case "tool":
			var state ocToolState
			if len(p.State) > 0 {
				if parseErr := json.Unmarshal(p.State, &state); parseErr != nil {
					return nil, nil, nil, "", fmt.Errorf("parse tool state for part %s: %w", p.ID, parseErr)
				}
			}

			var args map[string]any
			if len(state.Input) > 0 {
				if parseErr := json.Unmarshal(state.Input, &args); parseErr != nil {
					args = map[string]any{"raw": string(state.Input)}
				}
			}

			callID := p.CallID
			if callID == "" {
				callID = p.ID
			}
			content = append(content, piToolCallContent{
				Type:      "toolCall",
				ID:        callID,
				Name:      p.Tool,
				Arguments: args,
			})
			toolInfos = append(toolInfos, toolPartInfo{
				callID: callID,
				tool:   p.Tool,
				state:  state,
				asset:  p.Asset,
			})

		case "step-finish":
			hasStepFinish = true
			aggUsage.Input += p.Tokens.Input
			aggUsage.Output += p.Tokens.Output
			aggUsage.CacheRead += p.Tokens.Cache.Read
			aggUsage.CacheWrite += p.Tokens.Cache.Write
			aggUsage.Cost.Total += p.Cost
			stopReason = mapStopReason(p.Reason)
		}
		// step-start is handled by the caller before this function.
	}

	if !hasStepFinish {
		stopReason = "aborted"
	}
	aggUsage.TotalTokens = aggUsage.Input + aggUsage.Output + aggUsage.CacheRead

	return content, toolInfos, &aggUsage, stopReason, nil
}

func mapStopReason(reason string) string {
	switch reason {
	case "stop", "end_turn":
		return "stop"
	case "length", "max_tokens":
		return "length"
	case "tool-calls", "tool_use":
		return "toolUse"
	case "error":
		return "error"
	default:
		if reason == "" {
			return "stop"
		}
		return reason
	}
}

// ── tool result entries ───────────────────────────────────────────────────────

func buildToolResultEntry(tp toolPartInfo, prevID *string, rawDir string) (piMessageEntry, error) {
	isError := tp.state.Status == "error"

	var outputText string
	switch tp.state.Status {
	case "completed":
		outputText = tp.state.Output
	case "error":
		outputText = tp.state.Error
	case "pending", "running":
		outputText = "[Tool execution was interrupted]"
		isError = true
	}

	// If there's a legacy sidecar asset reference, prefer that content.
	if tp.asset != "" && strings.HasPrefix(tp.asset, "tool_") {
		if tp.asset == filepath.Base(tp.asset) && !strings.ContainsRune(tp.asset, os.PathSeparator) {
			sidecarPath := filepath.Join(rawDir, "tool-output", tp.asset)
			if data, readErr := os.ReadFile(sidecarPath); readErr == nil {
				outputText = string(data)
			}
		}
	}

	resultContent := []any{piTextContent{Type: "text", Text: outputText}}
	contentJSON, err := json.Marshal(resultContent)
	if err != nil {
		return piMessageEntry{}, err
	}

	endTime := tp.state.Time.End
	entryID := mustID()
	return piMessageEntry{
		piEntryBase: piEntryBase{
			Type:      "message",
			ID:        entryID,
			ParentID:  prevID,
			Timestamp: msToISO(endTime),
		},
		Message: piMessage{
			Role:       "toolResult",
			Content:    contentJSON,
			ToolCallID: tp.callID,
			ToolName:   tp.tool,
			IsError:    isError,
			Timestamp:  endTime,
		},
	}, nil
}

// ── system and snapshot custom entries ───────────────────────────────────────

func buildSystemEntry(system []string, prevID *string, ts string) (piCustomEntry, error) {
	type systemData struct {
		Prompts []string `json:"prompts"`
	}
	data, err := json.Marshal(systemData{Prompts: system})
	if err != nil {
		return piCustomEntry{}, err
	}
	entryID := mustID()
	return piCustomEntry{
		piEntryBase: piEntryBase{
			Type:      "custom",
			ID:        entryID,
			ParentID:  prevID,
			Timestamp: ts,
		},
		CustomType: "opencode.system",
		Data:       data,
	}, nil
}

func buildSnapshotEntry(sha string, prevID *string, ts string) (piCustomEntry, error) {
	type snapData struct {
		SHA string `json:"sha"`
	}
	data, err := json.Marshal(snapData{SHA: sha})
	if err != nil {
		return piCustomEntry{}, err
	}
	entryID := mustID()
	return piCustomEntry{
		piEntryBase: piEntryBase{
			Type:      "custom",
			ID:        entryID,
			ParentID:  prevID,
			Timestamp: ts,
		},
		CustomType: "opencode.snapshot.start",
		Data:       data,
	}, nil
}

// ── image helpers ─────────────────────────────────────────────────────────────

func isImageMIME(m string) bool {
	return strings.HasPrefix(m, "image/")
}

func filePartToImage(p ocPart) (piImageContent, error) {
	mimeType := p.Mime
	if mimeType == "" {
		mimeType = "image/png"
	}

	if strings.HasPrefix(p.URL, "data:") {
		comma := strings.Index(p.URL, ",")
		if comma < 0 {
			return piImageContent{}, fmt.Errorf("malformed data URI for part %s", p.ID)
		}
		// Extract mime from the data URI header.
		header := p.URL[5:comma] // strip "data:"
		if semi := strings.Index(header, ";"); semi > 0 {
			mimeType = header[:semi]
		}
		return piImageContent{Type: "image", Data: p.URL[comma+1:], MimeType: mimeType}, nil
	}

	// Treat as a file path.
	data, err := os.ReadFile(p.URL)
	if err != nil {
		return piImageContent{}, fmt.Errorf("read image file %s: %w", p.URL, err)
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		detected := http.DetectContentType(data)
		if parsedMime, _, parseErr := mime.ParseMediaType(detected); parseErr == nil {
			mimeType = parsedMime
		}
	}
	return piImageContent{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}, nil
}

// ── JSONL writer ──────────────────────────────────────────────────────────────

func writeJSONL(archiveDir string, entries []any) error {
	finalPath := filepath.Join(archiveDir, "session.jsonl")
	tmpPath := finalPath + ".tmp"

	// Remove stale temp file if present.
	_ = os.Remove(tmpPath)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	enc := json.NewEncoder(f)
	for _, e := range entries {
		if encErr := enc.Encode(e); encErr != nil {
			f.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("encode entry: %w", encErr)
		}
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to final: %w", err)
	}
	return nil
}

// ── string slice equality ─────────────────────────────────────────────────────

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
