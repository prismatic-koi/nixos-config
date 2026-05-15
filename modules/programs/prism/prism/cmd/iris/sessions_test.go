package main

// sessions_test.go — unit tests for the rendering layer of
// `iris sessions list` and `iris sessions status`.
//
// These tests exercise the pure rendering functions (renderSessionsListTable,
// renderSessionsListJSON, renderStatusLine, renderStatusJSON, countStates,
// formatUptime, truncateForColumn) against synthetic SessionSnapshot inputs.
//
// Integration coverage (real daemon socket + real network round-trip) lives in
// sessions_integration_test.go.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// fixedNow returns a deterministic "now" so uptime renders are stable across
// test runs. The fixed value is well after the test fixtures' StartedAt
// values so uptime is always positive.
func fixedNow(t *testing.T) time.Time {
	t.Helper()
	n, err := time.Parse(time.RFC3339, "2026-05-16T10:10:00Z")
	if err != nil {
		t.Fatalf("parse fixed now: %v", err)
	}
	return n
}

// fixtureSessions returns two synthetic SessionSnapshot rows, one coordinator
// in "active" and one worker in "waiting". Both have RFC3339 StartedAt
// stamps that produce stable uptimes against fixedNow().
func fixtureSessions() []iris.SessionSnapshot {
	return []iris.SessionSnapshot{
		{
			Name:             "iris-test@main",
			InstanceID:       "11111111-2222-3333-4444-555555555555",
			State:            "active",
			Role:             "coordinator",
			Worktree:         "/home/user/code/iris-test/.bare/main",
			StartedAt:        "2026-05-16T10:00:00Z", // 10 minutes before fixedNow
			HarnessSessionID: "/home/user/.pi/agent/sessions/aaaaaaaa.jsonl",
		},
		{
			Name:             "iris-test@feature-x",
			InstanceID:       "66666666-7777-8888-9999-aaaaaaaaaaaa",
			State:            "waiting",
			Role:             "worker",
			Worktree:         "/home/user/code/iris-test/feature-x",
			StartedAt:        "2026-05-16T10:09:30Z", // 30 seconds before
			HarnessSessionID: "/home/user/.pi/agent/sessions/bbbbbbbb.jsonl",
		},
	}
}

// TestRenderSessionsListTable_Empty asserts the empty-list AC: header only,
// no body, no "no sessions" message.
func TestRenderSessionsListTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionsListTable(&buf, nil, fixedNow(t)); err != nil {
		t.Fatalf("renderSessionsListTable: %v", err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one line (header), got %d:\n%s", len(lines), out)
	}
	for _, header := range []string{"SESSION", "STATE", "ROLE", "WORKTREE", "UPTIME"} {
		if !strings.Contains(lines[0], header) {
			t.Errorf("header missing %q: %q", header, lines[0])
		}
	}
	if strings.Contains(out, "no sessions") {
		t.Errorf("output must not contain a 'no sessions' message:\n%s", out)
	}
}

// TestRenderSessionsListTable_Populated asserts the populated table shows
// the truncated 12-char UUID, the worktree basename (not the full path),
// and a non-empty uptime.
func TestRenderSessionsListTable_Populated(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionsListTable(&buf, fixtureSessions(), fixedNow(t)); err != nil {
		t.Fatalf("renderSessionsListTable: %v", err)
	}
	out := buf.String()

	// 12-char UUID prefix for each session, no full UUID.
	if !strings.Contains(out, "11111111-222") {
		t.Errorf("expected 12-char UUID prefix for first session:\n%s", out)
	}
	if strings.Contains(out, "11111111-2222-3333-4444-555555555555") {
		t.Errorf("full UUID must not appear in human-readable table:\n%s", out)
	}

	// Worktree basename, not full path.
	if !strings.Contains(out, "feature-x") {
		t.Errorf("expected worktree basename 'feature-x':\n%s", out)
	}
	if strings.Contains(out, "/home/user/code/iris-test/feature-x") {
		t.Errorf("full worktree path must not appear in human-readable table:\n%s", out)
	}

	// State and role columns rendered for both rows.
	if !strings.Contains(out, "active") || !strings.Contains(out, "waiting") {
		t.Errorf("expected both states present:\n%s", out)
	}
	if !strings.Contains(out, "coordinator") || !strings.Contains(out, "worker") {
		t.Errorf("expected both roles present:\n%s", out)
	}

	// Uptime: coordinator started 10m ago → "10m00s"; worker 30s ago → "30s".
	if !strings.Contains(out, "10m00s") {
		t.Errorf("expected coordinator uptime '10m00s':\n%s", out)
	}
	if !strings.Contains(out, "30s") {
		t.Errorf("expected worker uptime '30s':\n%s", out)
	}
}

// TestRenderSessionsListJSON_Empty asserts the empty list emits `[]` not `null`.
func TestRenderSessionsListJSON_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionsListJSON(&buf, nil, fixedNow(t)); err != nil {
		t.Fatalf("renderSessionsListJSON: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Errorf("expected `[]` for empty list, got %q", got)
	}
}

// TestRenderSessionsListJSON_Schema asserts the AC-fixed field set is present
// with the expected types, and full identifiers (UUID, full worktree path)
// are preserved.
func TestRenderSessionsListJSON_Schema(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionsListJSON(&buf, fixtureSessions(), fixedNow(t)); err != nil {
		t.Fatalf("renderSessionsListJSON: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	// AC field set: id, state, role, worktree, harness_session_id,
	// created_at, uptime_seconds. Name is informational but stable.
	requiredKeys := []string{
		"id", "state", "role", "worktree",
		"harness_session_id", "created_at", "uptime_seconds",
	}
	for i, row := range rows {
		for _, k := range requiredKeys {
			if _, ok := row[k]; !ok {
				t.Errorf("row %d missing required key %q: %v", i, k, row)
			}
		}
		// uptime_seconds must decode as a number.
		if _, ok := row["uptime_seconds"].(float64); !ok {
			t.Errorf("row %d uptime_seconds is not a number: %v", i, row["uptime_seconds"])
		}
	}

	// First row: full UUID, full worktree path preserved verbatim.
	if rows[0]["id"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("row 0 id: got %v, want full UUID", rows[0]["id"])
	}
	if rows[0]["worktree"] != "/home/user/code/iris-test/.bare/main" {
		t.Errorf("row 0 worktree: got %v, want full path", rows[0]["worktree"])
	}
	// Uptime in seconds: 10 minutes = 600.
	if v, _ := rows[0]["uptime_seconds"].(float64); v != 600 {
		t.Errorf("row 0 uptime_seconds: got %v, want 600", v)
	}
	if v, _ := rows[1]["uptime_seconds"].(float64); v != 30 {
		t.Errorf("row 1 uptime_seconds: got %v, want 30", v)
	}
	// created_at preserved as RFC3339 input.
	if rows[0]["created_at"] != "2026-05-16T10:00:00Z" {
		t.Errorf("row 0 created_at: got %v, want RFC3339 stamp", rows[0]["created_at"])
	}
}

// TestCountStates exercises the state-bucketing logic, including the unknown
// state (folded into idle) and spawning (its own bucket in JSON, folded into
// active in the human line).
func TestCountStates(t *testing.T) {
	in := []iris.SessionSnapshot{
		{State: "active"},
		{State: "active"},
		{State: "waiting"},
		{State: "finished"},
		{State: "error"},
		{State: "spawning"},
		{State: "idle"},
		{State: "totally-made-up"}, // unknown → idle bucket
	}
	c := countStates(in)
	if c.Active != 2 {
		t.Errorf("Active = %d, want 2", c.Active)
	}
	if c.Waiting != 1 {
		t.Errorf("Waiting = %d, want 1", c.Waiting)
	}
	if c.Finished != 1 {
		t.Errorf("Finished = %d, want 1", c.Finished)
	}
	if c.Error != 1 {
		t.Errorf("Error = %d, want 1", c.Error)
	}
	if c.Spawning != 1 {
		t.Errorf("Spawning = %d, want 1", c.Spawning)
	}
	if c.Idle != 2 {
		// one explicit "idle" + one unknown bucketed into idle
		t.Errorf("Idle = %d, want 2 (explicit + unknown bucket)", c.Idle)
	}
}

// TestRenderStatusLine asserts the canonical-set always-present padded form.
func TestRenderStatusLine(t *testing.T) {
	var buf bytes.Buffer
	if err := renderStatusLine(&buf, sessionStateCounts{Active: 2, Finished: 1}); err != nil {
		t.Fatalf("renderStatusLine: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	want := "active: 2  waiting: 0  idle: 0  finished: 1  error: 0"
	if out != want {
		t.Errorf("status line mismatch:\n got: %q\nwant: %q", out, want)
	}
}

// TestRenderStatusLine_FoldsSpawningIntoActive asserts the human line groups
// spawning sessions with active (operator perspective: "in flight").
func TestRenderStatusLine_FoldsSpawningIntoActive(t *testing.T) {
	var buf bytes.Buffer
	if err := renderStatusLine(&buf, sessionStateCounts{Active: 1, Spawning: 2}); err != nil {
		t.Fatalf("renderStatusLine: %v", err)
	}
	if !strings.Contains(buf.String(), "active: 3") {
		t.Errorf("expected 'active: 3' (1 active + 2 spawning folded in), got %q", buf.String())
	}
}

// TestRenderStatusJSON asserts the JSON keys and types match the AC.
func TestRenderStatusJSON(t *testing.T) {
	var buf bytes.Buffer
	c := sessionStateCounts{Active: 2, Waiting: 1, Finished: 3, Error: 1}
	if err := renderStatusJSON(&buf, c); err != nil {
		t.Fatalf("renderStatusJSON: %v", err)
	}

	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, buf.String())
	}

	// Canonical keys are always present.
	for _, k := range []string{"active", "waiting", "idle", "finished", "error"} {
		v, ok := obj[k]
		if !ok {
			t.Errorf("missing canonical key %q in JSON: %s", k, buf.String())
			continue
		}
		if _, ok := v.(float64); !ok {
			t.Errorf("value for %q is not a number: %T %v", k, v, v)
		}
	}
	if v, _ := obj["active"].(float64); v != 2 {
		t.Errorf("active = %v, want 2", v)
	}
	if v, _ := obj["finished"].(float64); v != 3 {
		t.Errorf("finished = %v, want 3", v)
	}
}

// TestFormatUptime spot-checks the duration formatter at the boundaries.
func TestFormatUptime(t *testing.T) {
	now := fixedNow(t)
	cases := []struct {
		started string
		want    string
	}{
		{"", "—"},
		{"not-a-date", "—"},
		{"2030-01-01T00:00:00Z", "—"}, // future
		{"2026-05-16T10:09:59.5Z", "<1s"},
		{"2026-05-16T10:09:30Z", "30s"},
		{"2026-05-16T10:00:00Z", "10m00s"},
		{"2026-05-16T08:30:00Z", "1h40m"},
	}
	for _, tc := range cases {
		got := formatUptime(tc.started, now)
		if got != tc.want {
			t.Errorf("formatUptime(%q): got %q, want %q", tc.started, got, tc.want)
		}
	}
}

// TestTruncateForColumn covers the three branches: short string, exact, and
// over-long string with the ellipsis tail.
func TestTruncateForColumn(t *testing.T) {
	cases := []struct {
		in     string
		max    int
		want   string
	}{
		{"abc", 5, "abc"},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcd…"},
		{"abcdef", 1, "a"},
		{"abc", 0, ""},
	}
	for _, tc := range cases {
		got := truncateForColumn(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("truncateForColumn(%q, %d): got %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}
