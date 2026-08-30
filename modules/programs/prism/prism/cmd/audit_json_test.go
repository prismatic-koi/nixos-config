package cmd

// Tests for --json flag on prism audit.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestRenderAuditEventsJSON_EmptyEnvelope verifies that an empty result still
// renders the {events: [], truncated: false, hint: null} envelope, never
// null and never absent.
func TestRenderAuditEventsJSON_EmptyEnvelope(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderAuditEventsJSON(nil, "", 0, "", 0); err != nil {
			t.Fatalf("renderAuditEventsJSON: %v", err)
		}
	})

	var resp struct {
		Events    []map[string]interface{} `json:"events"`
		Truncated bool                     `json:"truncated"`
		Hint      *string                  `json:"hint"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if resp.Events == nil {
		t.Errorf("events must be an array, not null")
	}
	if len(resp.Events) != 0 {
		t.Errorf("expected empty events array, got %d entries", len(resp.Events))
	}
	if resp.Truncated {
		t.Errorf("expected truncated=false, got true")
	}
	if resp.Hint != nil {
		t.Errorf("expected hint=null when not truncated, got %q", *resp.Hint)
	}
}

// TestRenderAuditEventsJSON_HappyPath verifies snake_case keys, RFC3339
// timestamps, and that command/payload are extracted correctly.
func TestRenderAuditEventsJSON_HappyPath(t *testing.T) {
	created := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
	iid := "instance-1"
	events := []db.Event{
		{
			ID:          "event-1",
			SessionName: "nixos-config@main",
			Repo:        "nixos-config",
			Worktree:    "/code/nixos-config/main",
			InstanceID:  &iid,
			Type:        "audit",
			Payload:     `{"command":"gh pr merge 1484 --squash","sessionName":"nixos-config@main","cwd":"/tmp"}`,
			CreatedAt:   created,
		},
	}

	out := captureStdout(t, func() {
		if err := renderAuditEventsJSON(events, "", 0, "", 0); err != nil {
			t.Fatalf("renderAuditEventsJSON: %v", err)
		}
	})

	var resp struct {
		Events    []map[string]interface{} `json:"events"`
		Truncated bool                     `json:"truncated"`
		Hint      *string                  `json:"hint"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Events))
	}

	ev := resp.Events[0]
	for _, k := range []string{"id", "session_name", "instance_id", "command", "timestamp", "payload"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("missing required snake_case key %q in %s", k, out)
		}
	}
	if ev["command"] != "gh pr merge 1484 --squash" {
		t.Errorf("command: got %v, want 'gh pr merge 1484 --squash'", ev["command"])
	}
	if ev["timestamp"] != "2026-04-01T12:30:00Z" {
		t.Errorf("timestamp: got %v, want '2026-04-01T12:30:00Z'", ev["timestamp"])
	}
	if ev["instance_id"] != "instance-1" {
		t.Errorf("instance_id: got %v, want 'instance-1'", ev["instance_id"])
	}
}

// TestRenderAuditEventsJSON_TruncatedSetsHint verifies that when the result
// count equals the implicit default cap, truncated=true and hint is set.
func TestRenderAuditEventsJSON_TruncatedSetsHint(t *testing.T) {
	// Build exactly auditDefaultLimit events; with no session filter and
	// limit=0, the default cap is hit so truncated should be true.
	created := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	events := make([]db.Event, auditDefaultLimit)
	for i := 0; i < auditDefaultLimit; i++ {
		events[i] = db.Event{
			ID:          "id",
			SessionName: "s",
			Type:        "audit",
			Payload:     `{"command":"git push"}`,
			CreatedAt:   created,
		}
	}

	out := captureStdout(t, func() {
		if err := renderAuditEventsJSON(events, "", 0, "", 0); err != nil {
			t.Fatalf("renderAuditEventsJSON: %v", err)
		}
	})

	var resp struct {
		Events    []map[string]interface{} `json:"events"`
		Truncated bool                     `json:"truncated"`
		Hint      *string                  `json:"hint"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if !resp.Truncated {
		t.Errorf("expected truncated=true at default cap, got false")
	}
	if resp.Hint == nil || *resp.Hint == "" {
		t.Errorf("expected hint to be set when truncated=true, got nil/empty")
	}
}

// TestRenderAuditEventsJSON_NotTruncatedWithSession verifies that with a
// session filter, the implicit cap does not apply, so truncated stays false
// even when len(events) == 20.
func TestRenderAuditEventsJSON_NotTruncatedWithSession(t *testing.T) {
	created := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	events := make([]db.Event, auditDefaultLimit)
	for i := 0; i < auditDefaultLimit; i++ {
		events[i] = db.Event{ID: "id", SessionName: "s", Type: "audit", Payload: `{"command":"git push"}`, CreatedAt: created}
	}

	out := captureStdout(t, func() {
		if err := renderAuditEventsJSON(events, "nixos-config@main", 0, "", 0); err != nil {
			t.Fatalf("renderAuditEventsJSON: %v", err)
		}
	})

	var resp struct {
		Truncated bool    `json:"truncated"`
		Hint      *string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if resp.Truncated {
		t.Errorf("expected truncated=false with session filter (no implicit cap), got true")
	}
}
