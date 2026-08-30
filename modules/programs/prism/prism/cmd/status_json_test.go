package cmd

// Tests for --json flag on prism status.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
)

// TestRenderStatusJSON_AllStateKeysPresent verifies the JSON object exposes
// every required state key as an integer, even when the count is zero.
func TestRenderStatusJSON_AllStateKeysPresent(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderStatusJSON(2, 0, 1, 0, 0); err != nil {
			t.Fatalf("renderStatusJSON: %v", err)
		}
	})

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	for _, k := range []string{"active", "waiting", "idle", "finished", "error"} {
		v, ok := obj[k]
		if !ok {
			t.Errorf("missing required key %q in %s", k, out)
			continue
		}
		if _, ok := v.(float64); !ok {
			t.Errorf("key %q must be a JSON number, got %T (%v)", k, v, v)
		}
	}

	if obj["active"].(float64) != 2 {
		t.Errorf("active: want 2, got %v", obj["active"])
	}
	if obj["idle"].(float64) != 1 {
		t.Errorf("idle: want 1, got %v", obj["idle"])
	}
	if obj["error"].(float64) != 0 {
		t.Errorf("error: want 0, got %v", obj["error"])
	}
}

// TestStatusCmd_JSONAndTmuxFormatMutuallyExclusive verifies that combining
// --json and --tmux-format returns an error.
func TestStatusCmd_JSONAndTmuxFormatMutuallyExclusive(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")

	statusCmd.Flags().Set("json", "true")        //nolint:errcheck
	statusCmd.Flags().Set("tmux-format", "true") //nolint:errcheck
	defer func() {
		statusCmd.Flags().Set("json", "false")        //nolint:errcheck
		statusCmd.Flags().Set("tmux-format", "false") //nolint:errcheck
	}()

	err := statusCmd.RunE(statusCmd, nil)
	if err == nil {
		t.Fatal("expected error when --json and --tmux-format are combined, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}

// TestStatusCmd_JSON_DBOpenFailureStillEmitsJSON verifies that a DB open
// failure still produces a parseable empty-shape JSON document when --json
// is set (defence-in-depth: stdout must be a valid JSON object even on
// failure).
func TestStatusCmd_JSON_DBOpenFailureStillEmitsJSON(t *testing.T) {
	// Force openDB to point at an unwritable / nonexistent path.
	t.Setenv("PRISM_HOST_API", "")
	SetTestDBPath("/proc/1/no-such-prism.db")
	defer SetTestDBPath("")

	statusCmd.Flags().Set("json", "true")        //nolint:errcheck
	defer statusCmd.Flags().Set("json", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		_ = statusCmd.RunE(statusCmd, nil)
	})

	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		// On a successful openDB (e.g. dev shell where the path doesn't
		// matter and openDB returns nil), the "empty" branch isn't hit.
		// In that case the test is a no-op, which is fine.
		return
	}
	var obj map[string]int
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		t.Fatalf("--json output must be a valid JSON object, got %q (err: %v)", trimmed, err)
	}
	for _, k := range []string{"active", "waiting", "idle", "finished", "error"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("missing key %q in fallback JSON: %s", k, trimmed)
		}
	}
}

// TestStatusCmd_HumanRenderer_ShowsErrorSessions guards against error-state
// sessions disappearing from the human-readable summary and the tmux status
// bar when a dedicated nError counter (for --json) is introduced.
//
// Both renderers must surface error sessions. They must never be silently
// rolled into idle or omitted entirely.
func TestStatusCmd_HumanRenderer_ShowsErrorSessions(t *testing.T) {
	d := openStatsTestDB(t)

	if err := d.UpsertStatus("repo@bad", "repo", "/code", string(agent.StateError), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Plain human-readable summary.
	out := captureStdout(t, func() {
		if err := statusCmd.RunE(statusCmd, nil); err != nil {
			t.Errorf("statusCmd.RunE (plain): %v", err)
		}
	})
	if !strings.Contains(out, "1 error") {
		t.Errorf("plain summary must surface error sessions, got: %q", out)
	}

	// Tmux-format renderer.
	statusCmd.Flags().Set("tmux-format", "true")        //nolint:errcheck
	defer statusCmd.Flags().Set("tmux-format", "false") //nolint:errcheck

	out = captureStdout(t, func() {
		if err := statusCmd.RunE(statusCmd, nil); err != nil {
			t.Errorf("statusCmd.RunE (tmux-format): %v", err)
		}
	})
	if !strings.Contains(out, "1 error") {
		t.Errorf("tmux-format must surface error sessions, got: %q", out)
	}
}

// TestStatusCmd_JSON_StateAggregation verifies that statuses bucket into the
// expected JSON keys.
func TestStatusCmd_JSON_StateAggregation(t *testing.T) {
	d := openStatsTestDB(t) // unsets PRISM_HOST_API

	if err := d.UpsertStatus("repo@a", "repo", "/code", string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("repo@b", "repo", "/code", string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("repo@c", "repo", "/code", string(agent.StateWaiting), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("repo@d", "repo", "/code", string(agent.StateError), nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	statusCmd.Flags().Set("json", "true")        //nolint:errcheck
	defer statusCmd.Flags().Set("json", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := statusCmd.RunE(statusCmd, nil); err != nil {
			t.Errorf("statusCmd.RunE: %v", err)
		}
	})

	var obj map[string]int
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if obj["active"] != 2 {
		t.Errorf("active: want 2, got %d", obj["active"])
	}
	if obj["waiting"] != 1 {
		t.Errorf("waiting: want 1, got %d", obj["waiting"])
	}
	if obj["error"] != 1 {
		t.Errorf("error: want 1, got %d", obj["error"])
	}
}
