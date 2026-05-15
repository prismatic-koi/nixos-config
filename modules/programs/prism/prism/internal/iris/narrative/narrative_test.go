package narrative_test

// narrative_test.go — unit tests for the shared narrative renderer.
//
// These tests cover the helpers that `iris checkin` relies on (ToolKeyArg,
// ToolResultSummary, TurnLabel, ExtractMessageID). The per-event RenderEvent
// path is integration-tested via the TUI's own test suite, which sits in
// internal/iris/tui — the helpers package owns the unit-level coverage so a
// regression in either surface is caught here first.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris/narrative"
	"github.com/prismatic-koi/prism/internal/payload"
)

func TestTurnLabel(t *testing.T) {
	cases := []struct {
		agent, model, want string
	}{
		{"", "", ""},
		{"worker", "", "worker"},
		{"", "sonnet", "sonnet"},
		{"worker", "sonnet", "worker · sonnet"},
	}
	for _, c := range cases {
		if got := narrative.TurnLabel(c.agent, c.model); got != c.want {
			t.Errorf("TurnLabel(%q,%q) = %q, want %q", c.agent, c.model, got, c.want)
		}
	}
}

func TestToolKeyArg_Bash(t *testing.T) {
	// Object-form args.
	if got := narrative.ToolKeyArg("bash", `{"command":"go test ./..."}`); got != "go test ./..." {
		t.Errorf("bash command extract: got %q", got)
	}
	// Plain string args.
	if got := narrative.ToolKeyArg("bash", `"echo hi"`); got != "echo hi" {
		t.Errorf("bash plain-string extract: got %q", got)
	}
	// Long command is truncated with "...".
	long := strings.Repeat("x", 200)
	got := narrative.ToolKeyArg("bash", `{"command":"`+long+`"}`)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long bash arg should be truncated with ..., got %q", got[len(got)-10:])
	}
	if len([]rune(got)) > 83 { // 80 + "..."
		t.Errorf("truncation overran: len=%d", len([]rune(got)))
	}
}

func TestToolKeyArg_FilePath(t *testing.T) {
	args := `{"filePath":"/tmp/x.go","content":"…"}`
	for _, tool := range []string{"read", "Read", "edit", "Edit", "write", "Write"} {
		if got := narrative.ToolKeyArg(tool, args); got != "/tmp/x.go" {
			t.Errorf("%s key-arg: got %q, want /tmp/x.go", tool, got)
		}
	}
}

func TestToolKeyArg_GlobGrep(t *testing.T) {
	if got := narrative.ToolKeyArg("glob", `{"pattern":"**/*.go"}`); got != "**/*.go" {
		t.Errorf("glob: got %q", got)
	}
	if got := narrative.ToolKeyArg("grep", `{"pattern":"TODO"}`); got != "TODO" {
		t.Errorf("grep: got %q", got)
	}
}

func TestToolResultSummary_Bash(t *testing.T) {
	cases := []struct {
		result   string
		wantSubs []string
	}{
		{"", []string{"✓"}},
		{"hi\n", []string{"hi"}},
		{"error: command not found: foo", []string{"✗"}},
	}
	for _, c := range cases {
		got := narrative.ToolResultSummary("bash", c.result)
		for _, sub := range c.wantSubs {
			if !strings.Contains(got, sub) {
				t.Errorf("bash result %q → %q, want substring %q", c.result, got, sub)
			}
		}
	}
}

func TestToolResultSummary_Read(t *testing.T) {
	if got := narrative.ToolResultSummary("read", ""); got != "✓ (0 lines)" {
		t.Errorf("empty read: got %q", got)
	}
	if got := narrative.ToolResultSummary("read", "a\nb\nc\n"); got != "✓ (3 lines)" {
		t.Errorf("3-line read: got %q", got)
	}
}

func TestToolResultSummary_GlobMatchCounts(t *testing.T) {
	if got := narrative.ToolResultSummary("glob", ""); got != "no matches" {
		t.Errorf("empty glob: got %q", got)
	}
	if got := narrative.ToolResultSummary("glob", "a.go\n"); got != "1 match" {
		t.Errorf("1-match glob: got %q", got)
	}
	if got := narrative.ToolResultSummary("glob", "a.go\nb.go\nc.go\n"); got != "3 matches" {
		t.Errorf("3-match glob: got %q", got)
	}
}

func TestExtractMessageID(t *testing.T) {
	p, _ := json.Marshal(payload.MsgAssistant{MessageID: "mid-42", Text: "hi"})
	if got := narrative.ExtractMessageID(string(p)); got != "mid-42" {
		t.Errorf("ExtractMessageID: got %q, want mid-42", got)
	}
	if got := narrative.ExtractMessageID("not-json"); got != "" {
		t.Errorf("bad JSON ExtractMessageID: got %q, want \"\"", got)
	}
}

func TestRenderEvent_StateChange(t *testing.T) {
	p, _ := json.Marshal(payload.StateChange{State: "active"})
	lines := narrative.RenderEvent(1, "state_change", string(p))
	if len(lines) == 0 {
		t.Fatal("no lines for state_change")
	}
	if !strings.Contains(lines[0].Text, "●") || !strings.Contains(lines[0].Text, "active") {
		t.Errorf("state_change render: %q", lines[0].Text)
	}
}

func TestRenderEvent_TurnCollapsed(t *testing.T) {
	if got := narrative.RenderEvent(2, "turn_start", `{}`); len(got) != 0 {
		t.Errorf("turn_start should collapse, got %d lines", len(got))
	}
	if got := narrative.RenderEvent(3, "turn_end", `{}`); len(got) != 0 {
		t.Errorf("turn_end should collapse, got %d lines", len(got))
	}
}

func TestRenderEvent_Unknown(t *testing.T) {
	lines := narrative.RenderEvent(4, "future_type", `{}`)
	if len(lines) == 0 {
		t.Fatal("unknown event must produce ≥1 line")
	}
	if !strings.Contains(lines[0].Text, "future_type") {
		t.Errorf("unknown render: %q", lines[0].Text)
	}
}
