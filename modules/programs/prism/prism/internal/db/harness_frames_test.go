package db_test

// Tests for the harness_frames table (P5.LOGS / #1218).
//
// Coverage:
//   - WriteHarnessFrame round-trips through QueryHarnessFrames.
//   - Filtering by direction and types behaves as documented.
//   - The follow cursor (afterID) returns only later rows.
//   - PruneHarnessFrames drops rows older than the threshold but does NOT
//     touch agent_events written for the same session — this is the AC
//     guarantee that retention does not lose structured derivative data.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

func writeFrame(t *testing.T, d *db.DB, sessionName, direction, typ, payload string, ts time.Time) string {
	t.Helper()
	id := uuid.New().String()
	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          id,
		SessionName: sessionName,
		Direction:   direction,
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}); err != nil {
		t.Fatalf("WriteHarnessFrame: %v", err)
	}
	return id
}

func TestHarnessFrame_RoundTrip(t *testing.T) {
	d := openTestDB(t)

	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrame(t, d, "repo@main", "in", "tool_call", `{"type":"tool_call","name":"bash"}`, t0)
	writeFrame(t, d, "repo@main", "out", "prompt", `{"type":"prompt","text":"hi"}`, t0.Add(1*time.Millisecond))
	writeFrame(t, d, "other@main", "in", "tool_call", `{"type":"tool_call","name":"read"}`, t0.Add(2*time.Millisecond))

	frames, err := d.QueryHarnessFrames("repo@main", "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	// Check chronological order.
	if !frames[0].CreatedAt.Before(frames[1].CreatedAt) {
		t.Errorf("frames not in chronological order: %v before %v?", frames[0].CreatedAt, frames[1].CreatedAt)
	}
	if frames[0].Type != "tool_call" || frames[1].Type != "prompt" {
		t.Errorf("frame types = %q,%q; want tool_call,prompt", frames[0].Type, frames[1].Type)
	}
	if !strings.Contains(frames[0].Payload, `"name":"bash"`) {
		t.Errorf("payload[0] = %q; want bash", frames[0].Payload)
	}
}

func TestHarnessFrame_DirectionFilter(t *testing.T) {
	d := openTestDB(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrame(t, d, "s@main", "in", "tool_call", `{"type":"tool_call"}`, t0)
	writeFrame(t, d, "s@main", "out", "prompt", `{"type":"prompt"}`, t0.Add(1*time.Millisecond))
	writeFrame(t, d, "s@main", "in", "tool_result", `{"type":"tool_result"}`, t0.Add(2*time.Millisecond))

	in, err := d.QueryHarnessFrames("s@main", "in", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames in: %v", err)
	}
	if len(in) != 2 {
		t.Errorf("inbound count = %d, want 2", len(in))
	}
	for _, f := range in {
		if f.Direction != "in" {
			t.Errorf("got direction %q, want in", f.Direction)
		}
	}

	out, err := d.QueryHarnessFrames("s@main", "out", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames out: %v", err)
	}
	if len(out) != 1 || out[0].Direction != "out" {
		t.Errorf("outbound = %v, want one out frame", out)
	}
}

func TestHarnessFrame_TypeFilter(t *testing.T) {
	d := openTestDB(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrame(t, d, "s@main", "in", "tool_call", `{"type":"tool_call"}`, t0)
	writeFrame(t, d, "s@main", "in", "tool_result", `{"type":"tool_result"}`, t0.Add(1*time.Millisecond))
	writeFrame(t, d, "s@main", "in", "msg_assistant", `{"type":"msg_assistant"}`, t0.Add(2*time.Millisecond))

	got, err := d.QueryHarnessFrames("s@main", "", []string{"tool_call", "tool_result"}, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("count = %d, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.Type] = true
	}
	if !seen["tool_call"] || !seen["tool_result"] {
		t.Errorf("expected tool_call and tool_result; saw %v", seen)
	}
	if seen["msg_assistant"] {
		t.Errorf("msg_assistant should have been filtered out")
	}
}

func TestHarnessFrame_FollowCursor(t *testing.T) {
	d := openTestDB(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	id1 := writeFrame(t, d, "s@main", "in", "tool_call", `{"i":1}`, t0)
	writeFrame(t, d, "s@main", "in", "tool_call", `{"i":2}`, t0.Add(1*time.Millisecond))
	writeFrame(t, d, "s@main", "in", "tool_call", `{"i":3}`, t0.Add(2*time.Millisecond))

	got, err := d.QueryHarnessFrames("s@main", "", nil, id1)
	if err != nil {
		t.Fatalf("QueryHarnessFrames after cursor: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("after-cursor count = %d, want 2", len(got))
	}
	// The cursor frame itself must be excluded.
	for _, f := range got {
		if f.ID == id1 {
			t.Errorf("cursor frame %q should not appear in result set", id1)
		}
	}
}

func TestHarnessFrame_FollowCursor_UnknownID(t *testing.T) {
	d := openTestDB(t)
	_, err := d.QueryHarnessFrames("s@main", "", nil, "no-such-frame")
	if err == nil {
		t.Fatal("expected error for unknown cursor; got nil")
	}
	if !strings.Contains(err.Error(), "cursor frame") {
		t.Errorf("error = %q; want substring 'cursor frame'", err.Error())
	}
}

func TestHarnessFrame_CountIsolatedPerSession(t *testing.T) {
	d := openTestDB(t)
	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrame(t, d, "a@main", "in", "tool_call", `{}`, t0)
	writeFrame(t, d, "a@main", "in", "tool_call", `{}`, t0.Add(1*time.Millisecond))
	writeFrame(t, d, "b@main", "in", "tool_call", `{}`, t0.Add(2*time.Millisecond))

	if n, err := d.CountHarnessFrames("a@main"); err != nil || n != 2 {
		t.Errorf("count(a) = (%d, %v); want (2, nil)", n, err)
	}
	if n, err := d.CountHarnessFrames("b@main"); err != nil || n != 1 {
		t.Errorf("count(b) = (%d, %v); want (1, nil)", n, err)
	}
	if n, err := d.CountHarnessFrames("nobody@main"); err != nil || n != 0 {
		t.Errorf("count(nobody) = (%d, %v); want (0, nil)", n, err)
	}
}

// TestPruneHarnessFrames_DoesNotTouchAgentEvents is the AC for "Retention
// cutoff drops frames older than the configured window without dropping the
// corresponding agent_events rows".
func TestPruneHarnessFrames_DoesNotTouchAgentEvents(t *testing.T) {
	d := openTestDB(t)

	// Mint an old frame and an old agent_event for the same session at the
	// same timestamp.
	old := time.Now().Add(-30 * 24 * time.Hour) // 30 days ago
	writeFrame(t, d, "s@main", "in", "tool_call", `{"type":"tool_call"}`, old)
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "s@main",
		Repo:        "repo",
		Worktree:    "/tmp/wt",
		Type:        "tool_call",
		Payload:     `{"type":"tool_call"}`,
		CreatedAt:   old,
	}); err != nil {
		t.Fatalf("WriteEvent (old): %v", err)
	}

	// Add a recent frame and event so we can verify they're preserved.
	now := time.Now()
	writeFrame(t, d, "s@main", "in", "tool_call", `{"type":"tool_call","fresh":true}`, now)
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "s@main",
		Repo:        "repo",
		Worktree:    "/tmp/wt",
		Type:        "tool_call",
		Payload:     `{"type":"tool_call","fresh":true}`,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("WriteEvent (fresh): %v", err)
	}

	// Prune frames older than 7d.
	if err := d.PruneHarnessFrames(7 * 24 * time.Hour); err != nil {
		t.Fatalf("PruneHarnessFrames: %v", err)
	}

	// The old frame should be gone, the fresh one preserved.
	frames, err := d.QueryHarnessFrames("s@main", "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frame count after prune = %d, want 1 (fresh only)", len(frames))
	}
	if !strings.Contains(frames[0].Payload, `"fresh":true`) {
		t.Errorf("surviving frame payload = %q; expected fresh frame", frames[0].Payload)
	}

	// agent_events for the same session must be untouched — both rows still
	// present.
	events, err := d.QueryEvents("s@main", 0, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("agent_events count after PruneHarnessFrames = %d; want 2 (prune must not touch agent_events)", len(events))
	}
}

func TestHarnessFrame_MigrationCreatesTable(t *testing.T) {
	d := openTestDB(t)

	// On a fresh DB the harness_frames table should exist (created via the
	// declarative schema block) and accept rows.
	id := uuid.New().String()
	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          id,
		SessionName: "smoke@main",
		Direction:   "in",
		Type:        "hello",
		Payload:     `{"type":"hello"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteHarnessFrame on fresh DB: %v", err)
	}
	frames, err := d.QueryHarnessFrames("smoke@main", "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(frames) != 1 || frames[0].ID != id {
		t.Errorf("round trip failed: got %+v", frames)
	}

	// schema_version must be at v27 on a fresh DB after the v26→v27
	// migration has run (the harness_frames migration's bump step).
	var ver int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&ver); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if ver != 28 {
		t.Errorf("schema_version after fresh open = %d, want 28", ver)
	}

	// The expected indexes must exist (look them up by name).
	for _, idx := range []string{
		"idx_harness_frames_session",
		"idx_harness_frames_session_dir",
	} {
		var got string
		if err := d.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx,
		).Scan(&got); err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

// TestHarnessFrame_MigrationIdempotent verifies that opening a DB at v27
// twice does not fail or duplicate the table.
func TestHarnessFrame_MigrationIdempotent(t *testing.T) {
	d := openTestDB(t)
	// First open already ran migrations via openTestDB; a second open of the
	// same path must be a no-op.
	d.Close()
	d2, err := db.Open(d.Path())
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	var ver int
	if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&ver); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if ver != 28 {
		t.Errorf("schema_version after second open = %d, want 28", ver)
	}

	// The table must still accept rows.
	if err := d2.WriteHarnessFrame(db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: "idem@main",
		Direction:   "in",
		Type:        "hello",
		Payload:     `{"type":"hello"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteHarnessFrame after idempotent open: %v", err)
	}
}
