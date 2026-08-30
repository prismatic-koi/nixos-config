package cmd

// Tests for `prism review --wait`. We test the wait-and-aggregate
// path directly (waitForReviewTerminal / emitReviewWaitTerminal) — runReview
// itself is exercised by review_test.go and its full wiring is out of scope
// here. The wait loop's contract is "given a registered group and seeded
// agent_status rows, observe completion via db.GroupCompleted and aggregate
// verdicts via db.GroupResults".

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

func openReviewWaitTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "review_wait.db")
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

// linkAgentToGroup is a small helper that mirrors the SQL used in internal/db
// tests. Avoids exporting a test-only helper from the db package.
func linkAgentToGroup(t *testing.T, d *db.DB, sessionName, groupID string) {
	t.Helper()
	var one int
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
		groupID, sessionName,
	).Scan(&one); err != nil {
		t.Fatalf("link group_id: %v", err)
	}
}

// writeAssistantEvent inserts a msg_assistant event whose payload's text
// field carries the supplied verdict marker. Tests use this to seed PASS /
// FAIL verdicts for the aggregator.
func writeAssistantEvent(t *testing.T, d *db.DB, session, text string) {
	t.Helper()
	payload := `{"text":` + jsonString(text) + `}`
	now := time.Now()
	if err := d.WriteEvent(db.Event{
		ID:          "evt-" + session + "-" + now.Format("150405.000000"),
		SessionName: session,
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     payload,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestEmitReviewWaitTerminal_AllPass verifies the all-PASS aggregation:
// every member finished with a <verdict>PASS</verdict> marker → exit 0.
func TestEmitReviewWaitTerminal_AllPass(t *testing.T) {
	d := openReviewWaitTestDB(t)
	groupID, err := d.RegisterGroup("repo@pr-1500")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	const s1 = "repo@pr-1500~review-1-review-goal"
	const s2 = "repo@pr-1500~review-1-review-code"
	for _, s := range []string{s1, s2} {
		if err := d.UpsertStatusSeedRootAgentName(s, "repo", "/wt", "finished", nil, nil, "review", "", ""); err != nil {
			t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
		}
		linkAgentToGroup(t, d, s, groupID)
		writeAssistantEvent(t, d, s, "<verdict>PASS</verdict> looks good")
	}

	out := captureStdout(t, func() {
		if err := emitReviewWaitTerminal("1500", groupID, d, false); err != nil {
			t.Errorf("emitReviewWaitTerminal: expected nil for all-PASS, got %v", err)
		}
	})
	if !strings.Contains(out, "verdict: PASS") {
		t.Errorf("expected PASS summary, got %q", out)
	}
}

// TestEmitReviewWaitTerminal_AnyFailReturnsTerminalFail — one FAIL among
// PASSes flips the overall verdict to FAIL with exit code 2.
func TestEmitReviewWaitTerminal_AnyFailReturnsTerminalFail(t *testing.T) {
	d := openReviewWaitTestDB(t)
	groupID, _ := d.RegisterGroup("repo@pr-1500")
	const s1 = "repo@pr-1500~review-1-review-goal"
	const s2 = "repo@pr-1500~review-1-review-code"
	if err := d.UpsertStatusSeedRootAgentName(s1, "repo", "/wt", "finished", nil, nil, "review", "", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	linkAgentToGroup(t, d, s1, groupID)
	writeAssistantEvent(t, d, s1, "<verdict>PASS</verdict>")

	if err := d.UpsertStatusSeedRootAgentName(s2, "repo", "/wt", "finished", nil, nil, "review", "", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	linkAgentToGroup(t, d, s2, groupID)
	writeAssistantEvent(t, d, s2, "<verdict>FAIL</verdict> needs work")

	_ = captureStdout(t, func() {
		err := emitReviewWaitTerminal("1500", groupID, d, false)
		if err == nil {
			t.Fatal("expected non-nil error for any-FAIL")
		}
		var ec *exitCodeError
		if !errors.As(err, &ec) {
			t.Fatalf("expected *exitCodeError, got %T: %v", err, err)
		}
		if ec.code != waitExitTerminalFail {
			t.Errorf("expected exit code %d, got %d", waitExitTerminalFail, ec.code)
		}
	})
}

// TestEmitReviewWaitTerminal_JSONShape verifies the --json contract.
func TestEmitReviewWaitTerminal_JSONShape(t *testing.T) {
	d := openReviewWaitTestDB(t)
	groupID, _ := d.RegisterGroup("repo@pr-1500")
	const s = "repo@pr-1500~review-1-review-goal"
	if err := d.UpsertStatusSeedRootAgentName(s, "repo", "/wt", "finished", nil, nil, "review", "", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	linkAgentToGroup(t, d, s, groupID)
	writeAssistantEvent(t, d, s, "<verdict>PASS</verdict>")

	out := captureStdout(t, func() {
		if err := emitReviewWaitTerminal("1500", groupID, d, true /* json */); err != nil {
			t.Errorf("emitReviewWaitTerminal: %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		t.Fatalf("not JSON: %v\nout: %s", err, out)
	}
	for _, k := range []string{"pr", "group_id", "verdict", "agents", "status"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing required key %q in JSON: %v", k, payload)
		}
	}
	if payload["verdict"] != "PASS" {
		t.Errorf("verdict: want PASS, got %v", payload["verdict"])
	}
	agents, ok := payload["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("agents: expected 1-element array, got %v", payload["agents"])
	}
	a := agents[0].(map[string]any)
	if a["session"] != s {
		t.Errorf("agents[0].session: want %q, got %v", s, a["session"])
	}
	if a["verdict"] != "PASS" {
		t.Errorf("agents[0].verdict: want PASS, got %v", a["verdict"])
	}
}

// TestWaitForReviewTerminal_PollsUntilGroupCompleted exercises the full
// poll loop: start with one active member, flip to finished+PASS in a
// goroutine, observe terminal.
func TestWaitForReviewTerminal_PollsUntilGroupCompleted(t *testing.T) {
	d := openReviewWaitTestDB(t)
	groupID, _ := d.RegisterGroup("repo@pr-1500")
	const s = "repo@pr-1500~review-1-review-goal"
	if err := d.UpsertStatusSeedRootAgentName(s, "repo", "/wt", "active", nil, nil, "review", "", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	linkAgentToGroup(t, d, s, groupID)

	go func() {
		time.Sleep(150 * time.Millisecond)
		d2, _ := openDB()
		defer d2.Close()
		_ = d2.UpsertStatus(s, "repo", "/wt", "finished", nil, nil)
		writeAssistantEvent(t, d2, s, "<verdict>PASS</verdict>")
	}()

	out := captureStdout(t, func() {
		if err := waitForReviewTerminal("1500", groupID, false, 5*time.Second); err != nil {
			t.Errorf("waitForReviewTerminal: expected nil for PASS, got %v", err)
		}
	})
	if !strings.Contains(out, "PASS") {
		t.Errorf("expected PASS in output, got %q", out)
	}
}

// TestAgentNameFromSession recovers the agent role from a review session name.
func TestAgentNameFromSession(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"repo@pr-1500~review-1-review-goal", "review-goal"},
		{"repo@pr-1500~review-3-review-security", "review-security"},
		{"repo@main", "repo@main"},
		{"weird-name", "weird-name"},
	}
	for _, tc := range cases {
		if got := agentNameFromSession(tc.in); got != tc.want {
			t.Errorf("agentNameFromSession(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}
