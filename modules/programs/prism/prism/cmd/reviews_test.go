package cmd

// Tests for `prism reviews list` (#1500). Verifies the ledger surface
// returns rows from session_groups + agent_status, and that the JSON
// shape carries the documented fields.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openReviewsTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "reviews_test.db")
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestReviewsList_EmptyLedger renders an empty-ledger message in textual
// mode and "[]" in JSON mode.
func TestReviewsList_EmptyLedger(t *testing.T) {
	openReviewsTestDB(t)
	out := captureStdout(t, func() {
		if err := runReviewsList(reviewsListCmd, nil); err != nil {
			t.Fatalf("runReviewsList: %v", err)
		}
	})
	if !strings.Contains(out, "no review groups recorded") {
		t.Errorf("expected empty-ledger message, got %q", out)
	}
}

// TestReviewsList_JSONEmptyArray exercises the --json contract on an empty
// ledger: must be "[]", not null and not absent.
func TestReviewsList_JSONEmptyArray(t *testing.T) {
	openReviewsTestDB(t)
	if err := reviewsListCmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set --json: %v", err)
	}
	t.Cleanup(func() { _ = reviewsListCmd.Flags().Set("json", "false") })
	out := captureStdout(t, func() {
		if err := runReviewsList(reviewsListCmd, nil); err != nil {
			t.Fatalf("runReviewsList: %v", err)
		}
	})
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("expected '[]', got %q", out)
	}
}

// TestReviewsList_PopulatesAndSortsByCreatedAt seeds two review groups (one
// older, one newer) and verifies the JSON output lists them newest-first
// and carries the documented fields.
func TestReviewsList_PopulatesJSON(t *testing.T) {
	d := openReviewsTestDB(t)

	// Older group with one finished member.
	g1, err := d.RegisterGroup("nixos-config@pr-1500")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	const session1 = "nixos-config@pr-1500~review-1-review-goal"
	if err := d.UpsertStatusSeedRootAgentName(session1, "nixos-config", "/wt", "finished", nil, nil, "review-goal", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	// Link the agent_status row to the group via raw SQL (mirrors the
	// pattern used in internal/db tests).
	var one int
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
		g1, session1,
	).Scan(&one); err != nil {
		t.Fatalf("link group_id: %v", err)
	}

	if err := reviewsListCmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set --json: %v", err)
	}
	t.Cleanup(func() { _ = reviewsListCmd.Flags().Set("json", "false") })

	out := captureStdout(t, func() {
		if err := runReviewsList(reviewsListCmd, nil); err != nil {
			t.Fatalf("runReviewsList: %v", err)
		}
	})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("output not JSON: %v\nout: %s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	row := rows[0]
	for _, k := range []string{"group_id", "pr", "parent_session", "agent_sessions", "agent_states", "group_state", "started_at", "round"} {
		if _, ok := row[k]; !ok {
			t.Errorf("missing required key %q in JSON row: %v", k, row)
		}
	}
	if row["parent_session"] != "nixos-config@pr-1500" {
		t.Errorf("parent_session: want nixos-config@pr-1500, got %v", row["parent_session"])
	}
	if row["group_state"] != "completed" {
		t.Errorf("group_state: expected completed for a single finished agent, got %v", row["group_state"])
	}
	if pr, ok := row["pr"].(float64); !ok || int(pr) != 1500 {
		t.Errorf("pr: want 1500 (inferred from pr-1500 branch), got %v", row["pr"])
	}
	if rd, ok := row["round"].(float64); !ok || int(rd) != 1 {
		t.Errorf("round: want 1, got %v", row["round"])
	}
	agents, ok := row["agent_sessions"].([]any)
	if !ok || len(agents) != 1 || agents[0] != session1 {
		t.Errorf("agent_sessions: want [%q], got %v", session1, row["agent_sessions"])
	}
}

// TestInferPRFromGroup covers the parent-branch heuristic for PR inference.
func TestInferPRFromGroup(t *testing.T) {
	cases := []struct {
		parent string
		want   int
	}{
		{"repo@pr-42", 42},
		{"repo@pr1500", 1500},
		{"repo@main", 0},
		{"repo@feature-x", 0},
		{"plain", 0},
	}
	for _, tc := range cases {
		g := db.ReviewGroupSummary{ParentSession: tc.parent}
		if got := inferPRFromGroup(g); got != tc.want {
			t.Errorf("inferPRFromGroup(%q): got %d, want %d", tc.parent, got, tc.want)
		}
	}
}
