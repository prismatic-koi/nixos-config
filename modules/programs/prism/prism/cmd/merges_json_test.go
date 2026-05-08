package cmd

// Tests for --json flag on prism merges (#1499).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestRenderMergesListJSON_EmptyArray verifies that an empty merge queue
// renders as a bare JSON array "[]" (never null, never absent — AC #1499).
func TestRenderMergesListJSON_EmptyArray(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderMergesListJSON(nil); err != nil {
			t.Fatalf("renderMergesListJSON: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	if trimmed != "[]" {
		t.Errorf("expected empty JSON array '[]' for empty merges list, got %q", trimmed)
	}
}

// TestRenderMergesListJSON_SnakeCaseKeysAndRFC3339 verifies the JSON shape:
// snake_case keys, RFC3339 timestamps, optional fields rendered as null
// (not absent) per the issue conventions.
func TestRenderMergesListJSON_SnakeCaseKeysAndRFC3339(t *testing.T) {
	queuedAt := time.Date(2026, 5, 8, 7, 18, 33, 0, time.UTC)
	title := "feat: foo"
	merges := []db.PendingMerge{
		{
			PR:            1484,
			SessionName:   "nixos-config@main",
			InstanceID:    "abc-123",
			QueuePosition: 1778224434415,
			Status:        "watching",
			Title:         &title,
			QueuedAt:      queuedAt,
		},
	}

	out := captureStdout(t, func() {
		if err := renderMergesListJSON(merges); err != nil {
			t.Fatalf("renderMergesListJSON: %v", err)
		}
	})

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\noutput: %s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]

	// Required snake_case keys.
	for _, k := range []string{"queue_position", "pr", "title", "status", "error", "enqueued_at", "last_checked_at", "merged_at", "ended_at", "coordinator_session", "instance_id"} {
		if _, ok := row[k]; !ok {
			t.Errorf("missing required snake_case key %q in %s", k, out)
		}
	}

	// RFC3339 timestamp.
	if got := row["enqueued_at"]; got != "2026-05-08T07:18:33Z" {
		t.Errorf("enqueued_at: want 2026-05-08T07:18:33Z, got %v", got)
	}

	// Coordinator session and instance_id propagated.
	if got := row["coordinator_session"]; got != "nixos-config@main" {
		t.Errorf("coordinator_session: want nixos-config@main, got %v", got)
	}
	if got := row["instance_id"]; got != "abc-123" {
		t.Errorf("instance_id: want abc-123, got %v", got)
	}

	// Optional fields not set on the input row are null, not absent.
	for _, k := range []string{"error", "last_checked_at", "merged_at", "ended_at"} {
		v, ok := row[k]
		if !ok {
			t.Errorf("optional key %q must be present (as null), not absent", k)
		}
		if v != nil {
			t.Errorf("optional key %q must be null when unset, got %v", k, v)
		}
	}

	// pr is a number, not a string.
	if pr, ok := row["pr"].(float64); !ok || int(pr) != 1484 {
		t.Errorf("pr must be a JSON number with value 1484, got %v", row["pr"])
	}
}
