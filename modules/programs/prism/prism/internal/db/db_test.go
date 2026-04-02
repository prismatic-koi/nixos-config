package db_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func strPtr(s string) *string { return &s }

// TestOpen_CreatesSchema verifies that Open creates all required tables.
func TestOpen_CreatesSchema(t *testing.T) {
	d := openTestDB(t)

	// Verify all three main tables plus schema_version exist.
	tables := []string{"agent_events", "agent_status", "bus_messages", "schema_version"}
	for _, table := range tables {
		var name string
		err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}

	// Verify schema_version=2 (migrations are applied on Open).
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 2 {
		t.Errorf("schema_version: got %d, want 2", version)
	}

	// Verify WAL mode.
	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: got %q, want \"wal\"", mode)
	}

	// Opening the same path again must succeed without error.
	path2 := d.Path()
	d2, err := db.Open(path2)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	d2.Close()
}

// TestUpsertStatus_Insert verifies that UpsertStatus creates a new row.
func TestUpsertStatus_Insert(t *testing.T) {
	d := openTestDB(t)

	err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil)
	if err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if s.SessionName != "repo@main" {
		t.Errorf("SessionName: got %q, want %q", s.SessionName, "repo@main")
	}
	if s.Repo != "repo" {
		t.Errorf("Repo: got %q, want %q", s.Repo, "repo")
	}
	if s.Worktree != "/code/repo/main" {
		t.Errorf("Worktree: got %q, want %q", s.Worktree, "/code/repo/main")
	}
	if s.State != "idle" {
		t.Errorf("State: got %q, want %q", s.State, "idle")
	}
	if s.EndedAt != nil {
		t.Errorf("EndedAt: got non-nil, want nil")
	}
}

// TestUpsertStatus_Update verifies that upserting the same session twice updates state
// without creating a duplicate row.
func TestUpsertStatus_Update(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("first UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("second UpsertStatus: %v", err)
	}

	// Must still be only one row.
	all, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("row count: got %d, want 1", len(all))
	}
	if all[0].State != "active" {
		t.Errorf("State: got %q, want \"active\"", all[0].State)
	}
}

// TestWriteEvent verifies that WriteEvent inserts a row retrievable via QueryEvents.
func TestWriteEvent(t *testing.T) {
	d := openTestDB(t)

	id := uuid.New().String()
	e := db.Event{
		ID:          id,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	events, err := d.QueryEvents("repo@main", 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count: got %d, want 1", len(events))
	}
	got := events[0]
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
	if got.SessionName != "repo@main" {
		t.Errorf("SessionName: got %q, want %q", got.SessionName, "repo@main")
	}
	if got.Type != "state_change" {
		t.Errorf("Type: got %q, want %q", got.Type, "state_change")
	}
	if got.Payload != `{"state":"active"}` {
		t.Errorf("Payload: got %q, want %q", got.Payload, `{"state":"active"}`)
	}
}

// TestSetEnded verifies that SetEnded sets ended_at on the status row.
func TestSetEnded(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	if err := d.SetEnded("repo@main"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}
	if s.EndedAt == nil {
		t.Error("EndedAt: got nil, want non-nil")
	}
}

// TestAllActiveStatus_ExcludesEnded verifies that ended sessions are not returned.
func TestAllActiveStatus_ExcludesEnded(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus live: %v", err)
	}
	if err := d.UpsertStatus("repo@feat", "repo", "/code/repo/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus ended: %v", err)
	}
	if err := d.SetEnded("repo@feat"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	all, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("active count: got %d, want 1", len(all))
	}
	if all[0].SessionName != "repo@main" {
		t.Errorf("SessionName: got %q, want \"repo@main\"", all[0].SessionName)
	}
}

// TestWaitingCount verifies that WaitingCount returns the correct count.
func TestWaitingCount(t *testing.T) {
	d := openTestDB(t)

	sessions := []struct {
		name  string
		state string
	}{
		{"repo@s1", "waiting"},
		{"repo@s2", "waiting"},
		{"repo@s3", "active"},
	}
	for _, s := range sessions {
		if err := d.UpsertStatus(s.name, "repo", "/code/repo/"+s.name, s.state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", s.name, err)
		}
	}

	n, err := d.WaitingCount()
	if err != nil {
		t.Fatalf("WaitingCount: %v", err)
	}
	if n != 2 {
		t.Errorf("WaitingCount: got %d, want 2", n)
	}
}

// TestPrune verifies that old events are deleted and recent ones preserved.
func TestPrune(t *testing.T) {
	d := openTestDB(t)

	// Insert an old event (2 days ago) and a recent event (now).
	oldID := uuid.New().String()
	newID := uuid.New().String()

	oldEvent := db.Event{
		ID:          oldID,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{}`,
		CreatedAt:   time.Now().Add(-48 * time.Hour),
	}
	newEvent := db.Event{
		ID:          newID,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{}`,
		CreatedAt:   time.Now(),
	}

	if err := d.WriteEvent(oldEvent); err != nil {
		t.Fatalf("WriteEvent old: %v", err)
	}
	if err := d.WriteEvent(newEvent); err != nil {
		t.Fatalf("WriteEvent new: %v", err)
	}

	// Prune with 24h threshold — old event should be deleted, new preserved.
	if err := d.Prune(24 * time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	events, err := d.QueryEvents("repo@main", 100, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count after prune: got %d, want 1", len(events))
	}
	if events[0].ID != newID {
		t.Errorf("remaining event ID: got %q, want %q", events[0].ID, newID)
	}
}

// TestUpsertStatus_CoalesceTitle verifies that a subsequent upsert with nil title
// does not overwrite an existing title (COALESCE behaviour).
func TestUpsertStatus_CoalesceTitle(t *testing.T) {
	d := openTestDB(t)

	title := strPtr("my title")
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", title, nil); err != nil {
		t.Fatalf("first UpsertStatus: %v", err)
	}

	// Second upsert with nil title must not clobber the existing title.
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("second UpsertStatus: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.Title == nil {
		t.Fatal("Title: got nil, want preserved value")
	}
	if *s.Title != "my title" {
		t.Errorf("Title: got %q, want \"my title\"", *s.Title)
	}
}

// TestUpsertStatusIfNotTerminal verifies the conditional state-update semantics.
func TestUpsertStatusIfNotTerminal(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Should update a non-terminal state (active → interrupted).
	updated, err := d.UpsertStatusIfNotTerminal("repo@main", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (active): %v", err)
	}
	if !updated {
		t.Error("UpsertStatusIfNotTerminal (active): got updated=false, want true")
	}
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != "interrupted" {
		t.Errorf("State after update: got %q, want \"interrupted\"", s.State)
	}

	// Should be a no-op when already in "interrupted" (terminal state).
	updated2, err := d.UpsertStatusIfNotTerminal("repo@main", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (interrupted): %v", err)
	}
	if updated2 {
		t.Error("UpsertStatusIfNotTerminal (interrupted): got updated=true, want false (no-op)")
	}

	// Should be a no-op when already in "finished" (terminal state).
	if err := d.UpsertStatus("repo@feat", "repo", "/code/repo/feat", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (finished): %v", err)
	}
	updated3, err := d.UpsertStatusIfNotTerminal("repo@feat", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (finished): %v", err)
	}
	if updated3 {
		t.Error("UpsertStatusIfNotTerminal (finished): got updated=true, want false (no-op)")
	}
	sf, _ := d.CurrentStatus("repo@feat")
	if sf.State != "finished" {
		t.Errorf("State after no-op: got %q, want \"finished\"", sf.State)
	}

	// Should be a no-op for a non-existent session.
	updated4, err := d.UpsertStatusIfNotTerminal("repo@nonexistent", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (nonexistent): %v", err)
	}
	if updated4 {
		t.Error("UpsertStatusIfNotTerminal (nonexistent): got updated=true, want false (no-op)")
	}

	// Should be a no-op when already in "deleted" (terminal state).
	if err := d.UpsertStatus("repo@old", "repo", "/code/repo/old", "deleted", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (deleted): %v", err)
	}
	updated5, err := d.UpsertStatusIfNotTerminal("repo@old", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (deleted): %v", err)
	}
	if updated5 {
		t.Error("UpsertStatusIfNotTerminal (deleted): got updated=true, want false (no-op)")
	}

	// Should be a no-op when ended_at is set — covers the cleanup race where
	// KillSession fires pane-exited before SetEnded, but SetEnded runs first.
	if err := d.UpsertStatus("repo@ended", "repo", "/code/repo/ended", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (ended): %v", err)
	}
	if err := d.SetEnded("repo@ended"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	updated6, err := d.UpsertStatusIfNotTerminal("repo@ended", "interrupted")
	if err != nil {
		t.Fatalf("UpsertStatusIfNotTerminal (ended): %v", err)
	}
	if updated6 {
		t.Error("UpsertStatusIfNotTerminal (ended): got updated=true, want false (no-op for ended session)")
	}
	sEnded, _ := d.CurrentStatus("repo@ended")
	if sEnded.State != "active" {
		t.Errorf("State after ended no-op: got %q, want \"active\" (unchanged)", sEnded.State)
	}
}

// TestUpsertStatusInterruptedOverrideFinished verifies that the method
// overrides "finished" with "interrupted" (unlike UpsertStatusIfNotTerminal
// which treats "finished" as a terminal no-op).
func TestUpsertStatusInterruptedOverrideFinished(t *testing.T) {
	d := openTestDB(t)

	// Should update a non-terminal state (active → interrupted).
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	updated, err := d.UpsertStatusInterruptedOverrideFinished("repo@main")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (active): %v", err)
	}
	if !updated {
		t.Error("UpsertStatusInterruptedOverrideFinished (active): got updated=false, want true")
	}
	s, _ := d.CurrentStatus("repo@main")
	if s.State != "interrupted" {
		t.Errorf("State after active update: got %q, want \"interrupted\"", s.State)
	}

	// Key difference from UpsertStatusIfNotTerminal: "finished" should be
	// overridden (this is the fix for the pane-died / idle-debounce race).
	if err := d.UpsertStatus("repo@feat", "repo", "/code/repo/feat", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (finished): %v", err)
	}
	updated2, err := d.UpsertStatusInterruptedOverrideFinished("repo@feat")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (finished): %v", err)
	}
	if !updated2 {
		t.Error("UpsertStatusInterruptedOverrideFinished (finished): got updated=false, want true (override)")
	}
	sf, _ := d.CurrentStatus("repo@feat")
	if sf.State != "interrupted" {
		t.Errorf("State after finished override: got %q, want \"interrupted\"", sf.State)
	}

	// "interrupted" should be a no-op (already in target state).
	updated3, err := d.UpsertStatusInterruptedOverrideFinished("repo@main")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (interrupted): %v", err)
	}
	if updated3 {
		t.Error("UpsertStatusInterruptedOverrideFinished (interrupted): got updated=true, want false (no-op)")
	}

	// "deleted" should be a no-op — do not resurrect deleted sessions.
	if err := d.UpsertStatus("repo@old", "repo", "/code/repo/old", "deleted", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (deleted): %v", err)
	}
	updated4, err := d.UpsertStatusInterruptedOverrideFinished("repo@old")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (deleted): %v", err)
	}
	if updated4 {
		t.Error("UpsertStatusInterruptedOverrideFinished (deleted): got updated=true, want false (no-op)")
	}

	// Sessions with ended_at set should be a no-op.
	if err := d.UpsertStatus("repo@ended", "repo", "/code/repo/ended", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (ended): %v", err)
	}
	if err := d.SetEnded("repo@ended"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	updated5, err := d.UpsertStatusInterruptedOverrideFinished("repo@ended")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (ended): %v", err)
	}
	if updated5 {
		t.Error("UpsertStatusInterruptedOverrideFinished (ended): got updated=true, want false (no-op)")
	}

	// Non-existent session should be a no-op.
	updated6, err := d.UpsertStatusInterruptedOverrideFinished("repo@nonexistent")
	if err != nil {
		t.Fatalf("UpsertStatusInterruptedOverrideFinished (nonexistent): %v", err)
	}
	if updated6 {
		t.Error("UpsertStatusInterruptedOverrideFinished (nonexistent): got updated=true, want false (no-op)")
	}
}

// TestAllActiveStatusForRepo verifies that AllActiveStatusForRepo filters by repo.
func TestAllActiveStatusForRepo(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repoA@main", "repoA", "/code/repoA/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoA: %v", err)
	}
	if err := d.UpsertStatus("repoB@main", "repoB", "/code/repoB/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoB: %v", err)
	}

	results, err := d.AllActiveStatusForRepo("repoA")
	if err != nil {
		t.Fatalf("AllActiveStatusForRepo: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count: got %d, want 1", len(results))
	}
	if results[0].SessionName != "repoA@main" {
		t.Errorf("SessionName: got %q, want \"repoA@main\"", results[0].SessionName)
	}
}

// TestQueryEvents_LastN verifies that QueryEvents with a limit returns the most-recent
// N events in chronological (ASC) order, not the oldest N.
func TestQueryEvents_LastN(t *testing.T) {
	d := openTestDB(t)

	// Write 5 events with distinct, spaced timestamps so ordering is unambiguous.
	base := time.Now().Truncate(time.Second)
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = uuid.New().String()
		e := db.Event{
			ID:          ids[i],
			SessionName: "repo@main",
			Repo:        "repo",
			Worktree:    "/code/repo/main",
			Type:        "state_change",
			Payload:     `{}`,
			CreatedAt:   base.Add(time.Duration(i) * time.Second),
		}
		if err := d.WriteEvent(e); err != nil {
			t.Fatalf("WriteEvent[%d]: %v", i, err)
		}
	}

	// Query with limit=3 — should return the 3 newest events (ids[2], ids[3], ids[4]).
	events, err := d.QueryEvents("repo@main", 3, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("result count: got %d, want 3", len(events))
	}

	// Must be the 3 newest, in chronological (ASC) order.
	wantIDs := []string{ids[2], ids[3], ids[4]}
	for i, e := range events {
		if e.ID != wantIDs[i] {
			t.Errorf("events[%d].ID: got %q, want %q", i, e.ID, wantIDs[i])
		}
	}

	// Verify chronological order: each event must be no earlier than the previous.
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt.Before(events[i-1].CreatedAt) {
			t.Errorf("events not in chronological order: events[%d] (%v) < events[%d] (%v)",
				i, events[i].CreatedAt, i-1, events[i-1].CreatedAt)
		}
	}
}

// TestPurgeBusMessages verifies the PurgeBusMessages helper.
func TestPurgeBusMessages(t *testing.T) {
	d := openTestDB(t)

	writeMsg := func(id, from, to string, delivered bool) {
		t.Helper()
		msg := db.BusMessage{
			ID:          id,
			FromSession: from,
			ToSession:   to,
			Repo:        "repo",
			Text:        "test",
			Urgency:     "normal",
			SentAt:      time.Now(),
		}
		if err := d.WriteBusMessage(msg); err != nil {
			t.Fatalf("WriteBusMessage %s: %v", id, err)
		}
		if delivered {
			if err := d.MarkDelivered(id); err != nil {
				t.Fatalf("MarkDelivered %s: %v", id, err)
			}
		}
	}

	// Session under test.
	const target = "repo@feature"
	const other = "repo@main"

	// Undelivered messages involving target (should be deleted).
	writeMsg("to-target-undelivered", other, target, false)
	writeMsg("from-target-undelivered", target, other, false)

	// Delivered messages involving target (must NOT be deleted).
	writeMsg("to-target-delivered", other, target, true)
	writeMsg("from-target-delivered", target, other, true)

	// Undelivered message between two other sessions (must NOT be deleted).
	writeMsg("other-undelivered", other, "repo@third", false)

	if err := d.PurgeBusMessages(target); err != nil {
		t.Fatalf("PurgeBusMessages: %v", err)
	}

	// After purge, pending messages for target must be empty (to_session path).
	pending, err := d.PendingMessages(target, "normal")
	if err != nil {
		t.Fatalf("PendingMessages(target): %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending messages for target after purge: got %d, want 0", len(pending))
	}

	// Explicitly verify the from_session row was also deleted (PendingMessages
	// only queries to_session, so we must check via a direct row count).
	var fromCount int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE id = ?", "from-target-undelivered",
	).Scan(&fromCount); err != nil {
		t.Fatalf("count from-target-undelivered: %v", err)
	}
	if fromCount != 0 {
		t.Errorf("from-target-undelivered row still present after purge: got %d, want 0", fromCount)
	}

	// Pending messages for other sessions must be unaffected.
	pendingOther, err := d.PendingMessages("repo@third", "normal")
	if err != nil {
		t.Fatalf("PendingMessages(other): %v", err)
	}
	if len(pendingOther) != 1 {
		t.Errorf("pending messages for other after purge: got %d, want 1", len(pendingOther))
	}
}

// TestPurgeBusMessages_NoRows verifies that PurgeBusMessages is a no-op when
// there are no matching rows (no error, no panic).
func TestPurgeBusMessages_NoRows(t *testing.T) {
	d := openTestDB(t)

	// No rows at all — must not error.
	if err := d.PurgeBusMessages("repo@nonexistent"); err != nil {
		t.Fatalf("PurgeBusMessages (no rows): %v", err)
	}
}

// TestPurgeBusMessages_PreservesDelivered verifies that messages with
// delivered_at set are not removed by PurgeBusMessages.
func TestPurgeBusMessages_PreservesDelivered(t *testing.T) {
	d := openTestDB(t)

	msgID := uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "repo@other",
		ToSession:   "repo@target",
		Repo:        "repo",
		Text:        "already delivered",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}
	if err := d.WriteBusMessage(msg); err != nil {
		t.Fatalf("WriteBusMessage: %v", err)
	}
	if err := d.MarkDelivered(msgID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	if err := d.PurgeBusMessages("repo@target"); err != nil {
		t.Fatalf("PurgeBusMessages: %v", err)
	}

	// The delivered row should still be present.
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE id = ?", msgID).Scan(&count); err != nil {
		t.Fatalf("count delivered: %v", err)
	}
	if count != 1 {
		t.Errorf("delivered row count after purge: got %d, want 1", count)
	}
}

// TestWriteBusMessage_PendingMessages_MarkDelivered tests the full bus message lifecycle.
func TestWriteBusMessage_PendingMessages_MarkDelivered(t *testing.T) {
	d := openTestDB(t)

	msgID := uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "repo@feat",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "hello coordinator",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}

	if err := d.WriteBusMessage(msg); err != nil {
		t.Fatalf("WriteBusMessage: %v", err)
	}

	// Must appear in pending messages.
	pending, err := d.PendingMessages("repo@main", "normal")
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending count: got %d, want 1", len(pending))
	}
	if pending[0].ID != msgID {
		t.Errorf("message ID: got %q, want %q", pending[0].ID, msgID)
	}
	if pending[0].DeliveredAt != nil {
		t.Error("DeliveredAt: got non-nil, want nil")
	}

	// Mark delivered.
	if err := d.MarkDelivered(msgID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// Must no longer appear in pending messages.
	pending2, err := d.PendingMessages("repo@main", "normal")
	if err != nil {
		t.Fatalf("PendingMessages after deliver: %v", err)
	}
	if len(pending2) != 0 {
		t.Errorf("pending count after deliver: got %d, want 0", len(pending2))
	}
}

// TestMigration_V1ToV2 verifies that Open applies the v1→v2 migration to an
// existing DB that was created at schema_version=1 (no agent_name/model_id).
func TestMigration_V1ToV2(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v1.db")

	// Seed a v1 DB directly via raw sql.Open (no agent_name/model_id columns).
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, opencode_sid TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT, opencode_sid TEXT,
		  last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (1);
		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v1 db: %v", err)
	}

	// Open via db.Open — should apply the v1→v2 migration.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v1 db: %v", err)
	}
	defer d.Close()

	// Verify schema_version=2.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 2 {
		t.Errorf("schema_version after migration: got %d, want 2", version)
	}

	// Verify the new columns exist and the existing row is preserved.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	if s.AgentName != nil {
		t.Errorf("AgentName: got %v, want nil (newly added column)", s.AgentName)
	}
	if s.ModelID != nil {
		t.Errorf("ModelID: got %v, want nil (newly added column)", s.ModelID)
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}
}

// TestUpsertStatusWithAgent verifies that agent_name and model_id are written
// and that COALESCE prevents nil values from overwriting existing ones.
func TestUpsertStatusWithAgent(t *testing.T) {
	d := openTestDB(t)

	agentName := strPtr("build")
	modelID := strPtr("github-copilot/claude-sonnet-4.6")

	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, agentName, modelID); err != nil {
		t.Fatalf("UpsertStatusWithAgent: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.AgentName == nil || *s.AgentName != "build" {
		t.Errorf("AgentName: got %v, want \"build\"", s.AgentName)
	}
	if s.ModelID == nil || *s.ModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("ModelID: got %v, want \"github-copilot/claude-sonnet-4.6\"", s.ModelID)
	}

	// Second upsert with nil agent/model must not clobber the existing values.
	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/code/repo/main", "finished", nil, nil, nil, nil); err != nil {
		t.Fatalf("second UpsertStatusWithAgent: %v", err)
	}
	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (2): %v", err)
	}
	if s2.AgentName == nil || *s2.AgentName != "build" {
		t.Errorf("AgentName after nil upsert: got %v, want preserved \"build\"", s2.AgentName)
	}
	if s2.ModelID == nil || *s2.ModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("ModelID after nil upsert: got %v, want preserved value", s2.ModelID)
	}
}

// TestQueryEventsByMessageIDs verifies the secondary query fetches events
// keyed by messageId JSON field, filtered by type.
func TestQueryEventsByMessageIDs(t *testing.T) {
	d := openTestDB(t)

	base := time.Now().Truncate(time.Second)

	writeE := func(id, typ, msgID string, offset time.Duration) {
		t.Helper()
		payload := `{"messageId":"` + msgID + `","tool":"bash","args":"go test","result":"ok"}`
		e := db.Event{
			ID:          id,
			SessionName: "repo@main",
			Repo:        "repo",
			Worktree:    "/code/repo/main",
			Type:        typ,
			Payload:     payload,
			CreatedAt:   base.Add(offset),
		}
		if err := d.WriteEvent(e); err != nil {
			t.Fatalf("WriteEvent %s: %v", id, err)
		}
	}

	// Two message IDs, each with a tool_call and a tool_result.
	writeE("e1", "tool_call", "msg-A", 0)
	writeE("e2", "tool_result", "msg-A", time.Second)
	writeE("e3", "tool_call", "msg-B", 2*time.Second)
	writeE("e4", "tool_result", "msg-B", 3*time.Second)
	// An event for a different messageId (should NOT be returned).
	writeE("e5", "tool_call", "msg-C", 4*time.Second)

	results, err := d.QueryEventsByMessageIDs("repo@main", []string{"msg-A", "msg-B"}, []string{"tool_call", "tool_result"})
	if err != nil {
		t.Fatalf("QueryEventsByMessageIDs: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("result count: got %d, want 4", len(results))
	}

	// Verify order is chronological ASC.
	wantIDs := []string{"e1", "e2", "e3", "e4"}
	for i, e := range results {
		if e.ID != wantIDs[i] {
			t.Errorf("results[%d].ID: got %q, want %q", i, e.ID, wantIDs[i])
		}
	}

	// Query with only one messageId.
	results2, err := d.QueryEventsByMessageIDs("repo@main", []string{"msg-B"}, nil)
	if err != nil {
		t.Fatalf("QueryEventsByMessageIDs (single): %v", err)
	}
	if len(results2) != 2 {
		t.Fatalf("single result count: got %d, want 2", len(results2))
	}

	// Empty messageIds should return nil, not error.
	results3, err := d.QueryEventsByMessageIDs("repo@main", nil, nil)
	if err != nil {
		t.Fatalf("QueryEventsByMessageIDs (empty): %v", err)
	}
	if results3 != nil {
		t.Errorf("empty result: got %v, want nil", results3)
	}
}
