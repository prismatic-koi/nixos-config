package db_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	// Verify schema_version=17 (migrations are applied on Open).
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version: got %d, want 17", version)
	}

	// Verify the partial unique index for coordinator-per-repo was created (v12).
	var indexName string
	if err := d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_active_coordinator_per_repo'",
	).Scan(&indexName); err != nil {
		t.Errorf("idx_active_coordinator_per_repo index not found: %v", err)
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

// TestSetEnded_CascadesToReviewChildren verifies that SetEnded on a parent
// session also ends all child review-agent rows matching <parent>~review-%,
// while leaving unrelated rows untouched. It also asserts the idempotency
// guarantee: children with ended_at already set are not re-touched.
func TestSetEnded_CascadesToReviewChildren(t *testing.T) {
	d := openTestDB(t)

	parent := "nixos-config@feat"

	// Child rows that should be cascaded.
	children := []string{
		parent + "~review-1-review-goal",
		parent + "~review-1-review-code",
		parent + "~review-1-review-security",
		parent + "~review-2-review-qa",
		parent + "~review-2-review-context",
	}

	// A child that already has ended_at set — must not be re-touched.
	alreadyEndedChild := parent + "~review-1-review-qa"

	// An unrelated session that must not be affected.
	unrelated := "other-repo@main"

	// Another session whose name starts similarly but does NOT match the
	// ~review- pattern — must not be affected.
	notReview := "nixos-config@feat-other"

	// Insert the parent row.
	if err := d.UpsertStatus(parent, "nixos-config", "/wt/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	// Insert child rows (active).
	for _, c := range children {
		if err := d.UpsertStatus(c, "nixos-config", "/wt/feat", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus child %q: %v", c, err)
		}
	}

	// Insert the already-ended child row.
	if err := d.UpsertStatus(alreadyEndedChild, "nixos-config", "/wt/feat", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus alreadyEndedChild: %v", err)
	}
	if err := d.SetEnded(alreadyEndedChild); err != nil {
		t.Fatalf("SetEnded alreadyEndedChild (pre-condition): %v", err)
	}
	// Record the ended_at value so we can assert it was not changed.
	preStatus, err := d.CurrentStatus(alreadyEndedChild)
	if err != nil || preStatus == nil || preStatus.EndedAt == nil {
		t.Fatalf("pre-condition: alreadyEndedChild must have ended_at set")
	}
	preEndedAt := *preStatus.EndedAt

	// Insert the unrelated session.
	if err := d.UpsertStatus(unrelated, "other-repo", "/wt/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus unrelated: %v", err)
	}

	// Insert the not-review session (shares the same prefix but no ~review-).
	if err := d.UpsertStatus(notReview, "nixos-config", "/wt/feat-other", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus notReview: %v", err)
	}

	// Call SetEnded on the parent.
	if err := d.SetEnded(parent); err != nil {
		t.Fatalf("SetEnded parent: %v", err)
	}

	// Parent must be ended.
	s, err := d.CurrentStatus(parent)
	if err != nil || s == nil {
		t.Fatalf("CurrentStatus parent: %v", err)
	}
	if s.EndedAt == nil {
		t.Error("parent: EndedAt is nil, want non-nil")
	}

	// All children must be ended.
	for _, c := range children {
		sc, err := d.CurrentStatus(c)
		if err != nil || sc == nil {
			t.Fatalf("CurrentStatus child %q: %v", c, err)
		}
		if sc.EndedAt == nil {
			t.Errorf("child %q: EndedAt is nil, want non-nil", c)
		}
	}

	// The already-ended child must still be ended, with the same timestamp
	// (not re-touched by the cascade).
	postStatus, err := d.CurrentStatus(alreadyEndedChild)
	if err != nil || postStatus == nil {
		t.Fatalf("CurrentStatus alreadyEndedChild post: %v", err)
	}
	if postStatus.EndedAt == nil {
		t.Error("alreadyEndedChild: EndedAt became nil after cascade")
	} else if !(*postStatus.EndedAt).Equal(preEndedAt) {
		t.Errorf("alreadyEndedChild: ended_at changed: got %v, want %v", *postStatus.EndedAt, preEndedAt)
	}

	// Unrelated session must not be ended.
	su, err := d.CurrentStatus(unrelated)
	if err != nil || su == nil {
		t.Fatalf("CurrentStatus unrelated: %v", err)
	}
	if su.EndedAt != nil {
		t.Errorf("unrelated session: EndedAt is non-nil, want nil")
	}

	// Not-review session (shares prefix but no ~review-) must not be ended.
	sn, err := d.CurrentStatus(notReview)
	if err != nil || sn == nil {
		t.Fatalf("CurrentStatus notReview: %v", err)
	}
	if sn.EndedAt != nil {
		t.Errorf("notReview session: EndedAt is non-nil, want nil")
	}
}

// TestSetEnded_NoChildrenIsNoop verifies that SetEnded on a session with no
// review children still works correctly (no regression for the common case).
func TestSetEnded_NoChildrenIsNoop(t *testing.T) {
	d := openTestDB(t)

	session := "myrepo@feat"
	if err := d.UpsertStatus(session, "myrepo", "/wt/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	s, err := d.CurrentStatus(session)
	if err != nil || s == nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.EndedAt == nil {
		t.Error("EndedAt: got nil, want non-nil")
	}
}

// TestSetEnded_LikeWildcardsInSessionName verifies that session names containing
// SQL LIKE wildcard characters (%, _, \) are handled correctly and do not
// cause the cascade to match unintended sibling rows.
func TestSetEnded_LikeWildcardsInSessionName(t *testing.T) {
	d := openTestDB(t)

	// A session name containing an underscore (produced by NameFor when the
	// repo name contains a dot, e.g. "my.repo" → "my_repo").
	parent := "my_repo@feat"
	child := parent + "~review-1-review-goal"

	// A session that would be matched if _ acted as a wildcard:
	// "myXrepo@feat~review-1-review-goal" — the X can be any character.
	decoy := "myXrepo@feat~review-1-review-goal"

	for _, sess := range []string{parent, child, decoy} {
		if err := d.UpsertStatus(sess, "myrepo", "/wt", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", sess, err)
		}
	}

	// End the parent. Only the parent and its exact-prefix child should be ended.
	if err := d.SetEnded(parent); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Parent must be ended.
	sp, err := d.CurrentStatus(parent)
	if err != nil || sp == nil {
		t.Fatalf("CurrentStatus parent: %v", err)
	}
	if sp.EndedAt == nil {
		t.Error("parent: EndedAt is nil, want non-nil")
	}

	// Child must be ended.
	sc, err := d.CurrentStatus(child)
	if err != nil || sc == nil {
		t.Fatalf("CurrentStatus child: %v", err)
	}
	if sc.EndedAt == nil {
		t.Error("child: EndedAt is nil, want non-nil")
	}

	// Decoy (different repo prefix) must NOT be ended.
	sd, err := d.CurrentStatus(decoy)
	if err != nil || sd == nil {
		t.Fatalf("CurrentStatus decoy: %v", err)
	}
	if sd.EndedAt != nil {
		t.Errorf("decoy session %q: EndedAt is non-nil; _ was treated as wildcard", decoy)
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
		if delivered {
			if err := d.WriteBusMessageDelivered(msg); err != nil {
				t.Fatalf("WriteBusMessageDelivered %s: %v", id, err)
			}
		} else {
			if err := d.WriteBusMessage(msg); err != nil {
				t.Fatalf("WriteBusMessage %s: %v", id, err)
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

	// After purge, undelivered messages for target must be gone (to_session path).
	var toTargetCount int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE to_session = ? AND delivered_at IS NULL", target,
	).Scan(&toTargetCount); err != nil {
		t.Fatalf("count to-target undelivered: %v", err)
	}
	if toTargetCount != 0 {
		t.Errorf("undelivered messages for target after purge: got %d, want 0", toTargetCount)
	}

	// Explicitly verify the from_session row was also deleted.
	var fromCount int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE id = ?", "from-target-undelivered",
	).Scan(&fromCount); err != nil {
		t.Fatalf("count from-target-undelivered: %v", err)
	}
	if fromCount != 0 {
		t.Errorf("from-target-undelivered row still present after purge: got %d, want 0", fromCount)
	}

	// Undelivered messages for other sessions must be unaffected.
	var otherCount int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM bus_messages WHERE to_session = ? AND delivered_at IS NULL", "repo@third",
	).Scan(&otherCount); err != nil {
		t.Fatalf("count other undelivered: %v", err)
	}
	if otherCount != 1 {
		t.Errorf("undelivered messages for other after purge: got %d, want 1", otherCount)
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
	if err := d.WriteBusMessageDelivered(msg); err != nil {
		t.Fatalf("WriteBusMessageDelivered: %v", err)
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

// TestPurgeBusMessages_PreservesFailed verifies that messages with failed_at
// set are not removed by PurgeBusMessages (failed-delivery audit records must
// survive session cleanup, just like delivered messages).
func TestPurgeBusMessages_PreservesFailed(t *testing.T) {
	d := openTestDB(t)

	msgID := uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "repo@other",
		ToSession:   "repo@target",
		Repo:        "repo",
		Text:        "failed delivery audit",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}
	if err := d.WriteBusMessageFailed(msg); err != nil {
		t.Fatalf("WriteBusMessageFailed: %v", err)
	}

	if err := d.PurgeBusMessages("repo@target"); err != nil {
		t.Fatalf("PurgeBusMessages: %v", err)
	}

	// The failed row must still be present.
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE id = ?", msgID).Scan(&count); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if count != 1 {
		t.Errorf("failed row count after purge: got %d, want 1 (failed rows should survive purge)", count)
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

	// Open via db.Open — should apply v1→v2 through v8→v9 migrations.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v1 db: %v", err)
	}
	defer d.Close()

	// Verify schema_version=13.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
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
	if s.RootAgentName != nil {
		t.Errorf("RootAgentName: got %v, want nil (newly added column)", s.RootAgentName)
	}
	if s.RootModelID != nil {
		t.Errorf("RootModelID: got %v, want nil (newly added column)", s.RootModelID)
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}
}

// TestMigration_V2ToV3 verifies that Open applies the v2→v3 migration to an
// existing DB that was created at schema_version=2 (no root_agent_name/root_model_id).
func TestMigration_V2ToV3(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v2.db")

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
		  agent_name TEXT, model_id TEXT,
		  last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (2);
		INSERT INTO agent_status (session_name, repo, worktree, state, agent_name, model_id, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'worker', 'github-copilot/claude-sonnet-4.6', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v2 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v2 db: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	if s.AgentName == nil || *s.AgentName != "worker" {
		t.Errorf("AgentName preserved: got %v, want \"worker\"", s.AgentName)
	}
	if s.RootAgentName != nil {
		t.Errorf("RootAgentName: got %v, want nil (migration does not back-fill)", s.RootAgentName)
	}
	if s.RootModelID != nil {
		t.Errorf("RootModelID: got %v, want nil (migration does not back-fill)", s.RootModelID)
	}
}

// TestUpsertStatusWithAgent verifies that agent_name and model_id are written
// and that COALESCE prevents nil values from overwriting existing ones.
func TestUpsertStatusWithAgent(t *testing.T) {
	d := openTestDB(t)

	agentName := strPtr("worker")
	modelID := strPtr("github-copilot/claude-sonnet-4.6")

	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, agentName, modelID); err != nil {
		t.Fatalf("UpsertStatusWithAgent: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.AgentName == nil || *s.AgentName != "worker" {
		t.Errorf("AgentName: got %v, want \"worker\"", s.AgentName)
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
	if s2.AgentName == nil || *s2.AgentName != "worker" {
		t.Errorf("AgentName after nil upsert: got %v, want preserved \"worker\"", s2.AgentName)
	}
	if s2.ModelID == nil || *s2.ModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("ModelID after nil upsert: got %v, want preserved value", s2.ModelID)
	}
}

// TestUpsertStatusWithRootAgent verifies that root_agent_name and root_model_id
// are set on insert and that a subsequent UpsertStatusWithRootAgent call with a
// non-nil value overwrites them (sidecar is authoritative).
func TestUpsertStatusWithRootAgent(t *testing.T) {
	d := openTestDB(t)

	agentName := strPtr("worker")
	modelID := strPtr("github-copilot/claude-sonnet-4.6")

	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, agentName, modelID); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.AgentName == nil || *s.AgentName != "worker" {
		t.Errorf("AgentName: got %v, want \"worker\"", s.AgentName)
	}
	if s.ModelID == nil || *s.ModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("ModelID: got %v, want \"github-copilot/claude-sonnet-4.6\"", s.ModelID)
	}
	if s.RootAgentName == nil || *s.RootAgentName != "worker" {
		t.Errorf("RootAgentName: got %v, want \"worker\"", s.RootAgentName)
	}
	if s.RootModelID == nil || *s.RootModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("RootModelID: got %v, want \"github-copilot/claude-sonnet-4.6\"", s.RootModelID)
	}

	// A subsequent upsert with a different agent (simulating a subagent message)
	// must update agent_name/model_id but preserve root_agent_name/root_model_id.
	reviewAgent := strPtr("review")
	reviewModel := strPtr("github-copilot/gemini-2.5-pro")
	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, reviewAgent, reviewModel); err != nil {
		t.Fatalf("UpsertStatusWithAgent (subagent): %v", err)
	}
	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (subagent): %v", err)
	}
	if s2.AgentName == nil || *s2.AgentName != "review" {
		t.Errorf("AgentName after subagent: got %v, want \"review\"", s2.AgentName)
	}
	if s2.RootAgentName == nil || *s2.RootAgentName != "worker" {
		t.Errorf("RootAgentName after subagent: got %v, want preserved \"worker\"", s2.RootAgentName)
	}
	if s2.RootModelID == nil || *s2.RootModelID != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("RootModelID after subagent: got %v, want preserved original model", s2.RootModelID)
	}

	// UpsertStatusWithRootAgent on existing row must overwrite root fields with
	// the new sidecar-provided values (sidecar is authoritative, corrects stale values).
	newAgent := strPtr("coordinator")
	newModel := strPtr("github-copilot/gpt-4o")
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, newAgent, newModel); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent (second call): %v", err)
	}
	s3, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (second root upsert): %v", err)
	}
	if s3.RootAgentName == nil || *s3.RootAgentName != "coordinator" {
		t.Errorf("RootAgentName after second root upsert: got %v, want \"coordinator\" (sidecar value wins)", s3.RootAgentName)
	}
	if s3.RootModelID == nil || *s3.RootModelID != "github-copilot/gpt-4o" {
		t.Errorf("RootModelID after second root upsert: got %v, want \"github-copilot/gpt-4o\" (sidecar value wins)", s3.RootModelID)
	}
	if s3.AgentName == nil || *s3.AgentName != "coordinator" {
		t.Errorf("AgentName after second root upsert: got %v, want \"coordinator\" (current agent updated)", s3.AgentName)
	}
}

// TestUpsertStatusWithRootAgent_SidecarWins verifies that calling
// UpsertStatusWithRootAgent twice — first with agentName="worker", then with
// agentName="coordinator" — results in root_agent_name="coordinator".
// The sidecar is authoritative and must be able to correct a stale or wrong value.
func TestUpsertStatusWithRootAgent_SidecarWins(t *testing.T) {
	d := openTestDB(t)

	// First call: write with "worker" (simulating a stale/wrong initial value).
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/code/repo/main", "idle", nil, nil, strPtr("worker"), strPtr("model-old")); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent (first call): %v", err)
	}

	s1, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (first call): %v", err)
	}
	if s1.RootAgentName == nil || *s1.RootAgentName != "worker" {
		t.Errorf("RootAgentName after first call: got %v, want \"worker\"", s1.RootAgentName)
	}

	// Second call: sidecar corrects with "coordinator". The new value must win.
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, strPtr("coordinator"), strPtr("model-new")); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent (second call): %v", err)
	}

	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (second call): %v", err)
	}
	if s2.RootAgentName == nil || *s2.RootAgentName != "coordinator" {
		t.Errorf("RootAgentName after second call: got %v, want \"coordinator\" (sidecar value must win)", s2.RootAgentName)
	}
	if s2.RootModelID == nil || *s2.RootModelID != "model-new" {
		t.Errorf("RootModelID after second call: got %v, want \"model-new\" (sidecar value must win)", s2.RootModelID)
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

// TestAllocatePort_NoConflicts verifies that AllocatePort assigns the lowest
// port in the range when no ports are in use.
func TestAllocatePort_NoConflicts(t *testing.T) {
	d := openTestDB(t)

	// Create a session to allocate a port for.
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	port, err := d.AllocatePort("repo@main")
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if port < db.PortRangeStart || port > db.PortRangeEnd {
		t.Errorf("allocated port %d outside range %d–%d", port, db.PortRangeStart, db.PortRangeEnd)
	}

	// Verify the port is written to the DB.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.HarnessPort == nil {
		t.Fatal("OpencodePort: got nil, want non-nil")
	}
	if *s.HarnessPort != port {
		t.Errorf("OpencodePort: got %d, want %d", *s.HarnessPort, port)
	}
}

// TestAllocatePort_PartiallyUsed verifies that AllocatePort skips ports that
// are already allocated to other active sessions.
func TestAllocatePort_PartiallyUsed(t *testing.T) {
	d := openTestDB(t)

	// Create two sessions, allocate ports for both.
	if err := d.UpsertStatus("repo@s1", "repo", "/code/repo/s1", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus s1: %v", err)
	}
	if err := d.UpsertStatus("repo@s2", "repo", "/code/repo/s2", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus s2: %v", err)
	}

	port1, err := d.AllocatePort("repo@s1")
	if err != nil {
		t.Fatalf("AllocatePort s1: %v", err)
	}

	port2, err := d.AllocatePort("repo@s2")
	if err != nil {
		t.Fatalf("AllocatePort s2: %v", err)
	}

	if port1 == port2 {
		t.Errorf("ports must be different: both got %d", port1)
	}
	// port2 should be the next available (port1 + 1, unless port1+1 is
	// unavailable at the OS level, in which case it should still be > port1).
	if port2 <= port1 {
		t.Errorf("port2 (%d) should be > port1 (%d)", port2, port1)
	}
}

// TestAllocatePort_Exhaustion verifies that AllocatePort returns a clear error
// when all ports in the range are taken. This test uses a small range override
// via direct SQL to simulate exhaustion without allocating 1000 real ports.
func TestAllocatePort_Exhaustion(t *testing.T) {
	d := openTestDB(t)

	// Fill the entire range by creating sessions with every port in the range.
	// To keep the test fast, we manually INSERT rows with ports set rather than
	// calling AllocatePort 1000 times.
	for port := db.PortRangeStart; port <= db.PortRangeEnd; port++ {
		name := fmt.Sprintf("repo@s%d", port)
		if err := d.UpsertStatus(name, "repo", "/code/repo/"+name, "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", name, err)
		}
		// Set the port directly via raw SQL for speed.
		if err := setPort(d, name, port); err != nil {
			t.Fatalf("setPort %s: %v", name, err)
		}
	}

	// Now create one more session and try to allocate — should fail.
	if err := d.UpsertStatus("repo@overflow", "repo", "/code/repo/overflow", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus overflow: %v", err)
	}

	_, err := d.AllocatePort("repo@overflow")
	if err == nil {
		t.Fatal("AllocatePort: expected error for exhausted range, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error message should mention exhaustion: got %q", err.Error())
	}
}

// TestAllocatePort_StaleReclamation verifies that ports from ended sessions
// (ended_at IS NOT NULL) are reclaimed and available for reuse.
func TestAllocatePort_StaleReclamation(t *testing.T) {
	d := openTestDB(t)

	// Create a session, allocate a port, then end it.
	if err := d.UpsertStatus("repo@old", "repo", "/code/repo/old", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus old: %v", err)
	}
	portOld, err := d.AllocatePort("repo@old")
	if err != nil {
		t.Fatalf("AllocatePort old: %v", err)
	}
	if err := d.SetEnded("repo@old"); err != nil {
		t.Fatalf("SetEnded old: %v", err)
	}

	// Create a new session and allocate — it should be able to reuse the port
	// from the ended session (since it was the lowest available).
	if err := d.UpsertStatus("repo@new", "repo", "/code/repo/new", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus new: %v", err)
	}
	portNew, err := d.AllocatePort("repo@new")
	if err != nil {
		t.Fatalf("AllocatePort new: %v", err)
	}

	// The old port should be reclaimed (assuming it's available at the OS level).
	if portNew != portOld {
		t.Errorf("expected reclaimed port %d, got %d", portOld, portNew)
	}
}

// TestReleasePort verifies that ReleasePort clears the opencode_port column.
func TestReleasePort(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	port, err := d.AllocatePort("repo@main")
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if port == 0 {
		t.Fatal("AllocatePort returned 0")
	}

	if err := d.ReleasePort("repo@main"); err != nil {
		t.Fatalf("ReleasePort: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.HarnessPort != nil {
		t.Errorf("OpencodePort after release: got %v, want nil", *s.HarnessPort)
	}

	// After release the port must re-enter the pool so a new session can claim it.
	if err := d.UpsertStatus("repo@other", "repo", "/code/repo/other", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus other: %v", err)
	}
	portReclaimed, err := d.AllocatePort("repo@other")
	if err != nil {
		t.Fatalf("AllocatePort after release: %v", err)
	}
	if portReclaimed != port {
		t.Errorf("expected reclaimed port %d, got %d", port, portReclaimed)
	}
}

// TestReleasePort_NonexistentSession verifies that ReleasePort returns an error
// when the session name does not exist in agent_status.
func TestReleasePort_NonexistentSession(t *testing.T) {
	d := openTestDB(t)

	err := d.ReleasePort("repo@nonexistent")
	if err == nil {
		t.Fatal("ReleasePort: expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error message should mention 'not found': got %q", err.Error())
	}
}

// TestReleasePort_Idempotent verifies that calling ReleasePort on a session
// whose opencode_port is already NULL succeeds without error.
func TestReleasePort_Idempotent(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// Port is NULL from the start — release should be a no-op (not an error).
	if err := d.ReleasePort("repo@main"); err != nil {
		t.Fatalf("ReleasePort on already-NULL port: %v", err)
	}
	// Second call should also succeed.
	if err := d.ReleasePort("repo@main"); err != nil {
		t.Fatalf("ReleasePort second call: %v", err)
	}
}

// TestAllocatePort_NonexistentSession verifies that AllocatePort returns an
// error when the session does not exist in agent_status.
func TestAllocatePort_NonexistentSession(t *testing.T) {
	d := openTestDB(t)

	_, err := d.AllocatePort("repo@nonexistent")
	if err == nil {
		t.Fatal("AllocatePort: expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error message should mention 'not found': got %q", err.Error())
	}
}

// TestMigration_V3ToV4 verifies that Open applies the v3→v4 migration to an
// existing DB that was created at schema_version=3 (no opencode_port column).
func TestMigration_V3ToV4(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v3.db")

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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (3);
		INSERT INTO agent_status (session_name, repo, worktree, state, agent_name, model_id, root_agent_name, root_model_id, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'worker', 'claude-sonnet-4.6', 'worker', 'claude-sonnet-4.6', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v3 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v3 db: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	if s.HarnessPort != nil {
		t.Errorf("OpencodePort: got %v, want nil (newly added column)", s.HarnessPort)
	}
	if s.AgentName == nil || *s.AgentName != "worker" {
		t.Errorf("AgentName preserved: got %v, want \"worker\"", s.AgentName)
	}
}

// TestMigration_V4ToV5 verifies that Open applies the v4→v5 migration to an
// existing DB that was created at schema_version=4 (no host_mode column).
func TestMigration_V4ToV5(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v4.db")

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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER,
		  last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (4);
		INSERT INTO agent_status (session_name, repo, worktree, state, agent_name, model_id, root_agent_name, root_model_id, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'worker', 'claude-sonnet-4.6', 'worker', 'claude-sonnet-4.6', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v4 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v4 db: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	// host_mode should default to false for migrated rows.
	if s.HostMode {
		t.Errorf("HostMode: got true, want false (default for migrated rows)")
	}
	if s.AgentName == nil || *s.AgentName != "worker" {
		t.Errorf("AgentName preserved: got %v, want \"worker\"", s.AgentName)
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}
}

// TestMigration_V5ToV6 verifies that Open applies the v5→v6 migration to an
// existing DB that was created at schema_version=5 (no instance_id / to_instance_id columns).
func TestMigration_V5ToV6(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v5.db")

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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (5);
		INSERT INTO agent_status (session_name, repo, worktree, state, host_mode, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 0, 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v5 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v5 db: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	// instance_id should be NULL for migrated rows.
	if s.InstanceID != nil {
		t.Errorf("InstanceID: got %v, want nil (newly added column)", s.InstanceID)
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}

	// Verify that to_instance_id was added to bus_messages by writing a message
	// and checking that the column exists with a NULL value.
	msg := db.BusMessage{
		ID:          "test-instance-migration",
		FromSession: "repo@feat",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "migration test",
		Urgency:     "normal",
	}
	if err := d.WriteBusMessage(msg); err != nil {
		t.Fatalf("WriteBusMessage after v5→v6 migration: %v", err)
	}
	// Query to_instance_id directly to confirm the column exists and is NULL.
	var toInstanceID *string
	if err := d.QueryRow(
		"SELECT to_instance_id FROM bus_messages WHERE id = ?", "test-instance-migration",
	).Scan(&toInstanceID); err != nil {
		t.Fatalf("query to_instance_id after v5→v6 migration: %v", err)
	}
	// to_instance_id should be NULL (not set).
	if toInstanceID != nil {
		t.Errorf("ToInstanceID: got %v, want nil", toInstanceID)
	}
}

// TestMigration_V6ToV7 verifies that Open applies the v6→v7 migration to an
// existing DB at schema_version=6 (no failed_at column in bus_messages).
func TestMigration_V6ToV7(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v6.db")

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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (6);
		INSERT INTO agent_status (session_name, repo, worktree, state, host_mode, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 0, 0);
		INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at, delivered_at)
		  VALUES ('existing-msg', 'repo@feat', 'repo@main', 'repo', 'existing', 'normal', 1000, NULL);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v6 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v6 db: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// Existing row must be preserved with failed_at = NULL.
	var failedAt *int64
	if err := d.QueryRow(
		"SELECT failed_at FROM bus_messages WHERE id = ?", "existing-msg",
	).Scan(&failedAt); err != nil {
		t.Fatalf("query failed_at after v6→v7 migration: %v", err)
	}
	if failedAt != nil {
		t.Errorf("failed_at: got %v, want nil (additive migration must not affect existing rows)", failedAt)
	}

	// WriteBusMessageFailed must work after migration.
	msg := db.BusMessage{
		ID:          "failed-after-migration",
		FromSession: "repo@feat",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "migration test",
		Urgency:     "normal",
	}
	if err := d.WriteBusMessageFailed(msg); err != nil {
		t.Fatalf("WriteBusMessageFailed after v6→v7 migration: %v", err)
	}
	var failedAt2 *int64
	if err := d.QueryRow(
		"SELECT failed_at FROM bus_messages WHERE id = ?", "failed-after-migration",
	).Scan(&failedAt2); err != nil {
		t.Fatalf("query failed_at after WriteBusMessageFailed: %v", err)
	}
	if failedAt2 == nil {
		t.Error("failed_at: got nil, want non-nil timestamp after WriteBusMessageFailed")
	}
}

// TestMigration_V7ToV11 verifies that Open applies all migrations to an
// existing DB at schema_version=7 (no harness/harness_session_id/harness_port
// columns and with legacy opencode_sid/opencode_port columns). After migration
// the schema is at v11: harness columns are present, legacy columns are gone,
// and pre-existing data is preserved via back-fill.
func TestMigration_V7ToV11(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v7.db")

	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	const sid = "old-session-id"
	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, opencode_sid TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT, opencode_sid TEXT,
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (7);
		INSERT INTO agent_status (session_name, repo, worktree, state, opencode_sid, opencode_port, host_mode, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', '` + sid + `', 14000, 0, 0);
		INSERT INTO agent_status (session_name, repo, worktree, state, host_mode, last_seen)
		  VALUES ('repo@feat', 'repo', '/code/repo/feat', 'finished', 0, 1000);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v7 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v7 db: %v", err)
	}
	defer d.Close()

	// Schema version must be 11 after migration.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// All existing rows must be preserved unmodified (additive migration guarantee).
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus repo@main: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus repo@main: got nil, want existing row")
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}
	// v10→v11 back-fills harness_session_id from opencode_sid.
	if s.HarnessSessionID == nil || *s.HarnessSessionID != sid {
		t.Errorf("HarnessSessionID after migration: got %v, want %q (back-filled from opencode_sid)", s.HarnessSessionID, sid)
	}
	// v10→v11 back-fills harness_port from opencode_port.
	if s.HarnessPort == nil || *s.HarnessPort != 14000 {
		t.Errorf("HarnessPort after migration: got %v, want 14000 (back-filled from opencode_port)", s.HarnessPort)
	}

	// harness column must default to 'opencode' for rows written before migration.
	if s.Harness == nil || *s.Harness != "opencode" {
		t.Errorf("Harness: got %v, want \"opencode\" (default for pre-migration rows)", s.Harness)
	}

	// Second row (repo@feat) must also be preserved.
	sf, err := d.CurrentStatus("repo@feat")
	if err != nil {
		t.Fatalf("CurrentStatus repo@feat: %v", err)
	}
	if sf == nil {
		t.Fatal("CurrentStatus repo@feat: got nil, want existing row")
	}
	if sf.State != "finished" {
		t.Errorf("State preserved for repo@feat: got %q, want \"finished\"", sf.State)
	}
	if sf.Harness == nil || *sf.Harness != "opencode" {
		t.Errorf("Harness for repo@feat: got %v, want \"opencode\"", sf.Harness)
	}

	// UpdateHarnessSessionID must unconditionally overwrite harness_session_id.
	newSID := "new-session-id"
	if err := d.UpdateHarnessSessionID("repo@main", newSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID after migration: %v", err)
	}
	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after UpdateHarnessSessionID: %v", err)
	}
	if s2.HarnessSessionID == nil || *s2.HarnessSessionID != newSID {
		t.Errorf("HarnessSessionID after UpdateHarnessSessionID: got %v, want %q", s2.HarnessSessionID, newSID)
	}

	// AllocatePort must write harness_port.
	// First release the existing port so it's back in the pool, then re-allocate.
	if err := d.ReleasePort("repo@main"); err != nil {
		t.Fatalf("ReleasePort before re-allocate: %v", err)
	}
	port, err := d.AllocatePort("repo@main")
	if err != nil {
		t.Fatalf("AllocatePort after migration: %v", err)
	}
	s3, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after AllocatePort: %v", err)
	}
	if s3.HarnessPort == nil || *s3.HarnessPort != port {
		t.Errorf("HarnessPort after AllocatePort: got %v, want %d", s3.HarnessPort, port)
	}

	// ReleasePort must clear harness_port.
	if err := d.ReleasePort("repo@main"); err != nil {
		t.Fatalf("ReleasePort: %v", err)
	}
	s4, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after ReleasePort: %v", err)
	}
	if s4.HarnessPort != nil {
		t.Errorf("HarnessPort after ReleasePort: got %v, want nil", s4.HarnessPort)
	}
}

// TestWriteBusMessageFailed verifies that WriteBusMessageFailed inserts a row
// with failed_at set and delivered_at NULL.
func TestWriteBusMessageFailed(t *testing.T) {
	d := openTestDB(t)

	msgID := "test-failed-" + uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "repo@feature",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "hello coordinator",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}

	if err := d.WriteBusMessageFailed(msg); err != nil {
		t.Fatalf("WriteBusMessageFailed: %v", err)
	}

	// Verify delivered_at IS NULL and failed_at IS NOT NULL.
	var deliveredAt *int64
	var failedAt *int64
	if err := d.QueryRow(
		"SELECT delivered_at, failed_at FROM bus_messages WHERE id = ?", msgID,
	).Scan(&deliveredAt, &failedAt); err != nil {
		t.Fatalf("scan bus_message row: %v", err)
	}
	if deliveredAt != nil {
		t.Errorf("delivered_at: got %v, want nil (should not be set on failure)", deliveredAt)
	}
	if failedAt == nil {
		t.Error("failed_at: got nil, want non-nil (should be set on failure)")
	}
}

// TestWriteBusMessageDelivered_FailedAtNull verifies that WriteBusMessageDelivered
// writes delivered_at and leaves failed_at NULL.
func TestWriteBusMessageDelivered_FailedAtNull(t *testing.T) {
	d := openTestDB(t)

	msgID := "test-delivered-" + uuid.New().String()
	msg := db.BusMessage{
		ID:          msgID,
		FromSession: "repo@feature",
		ToSession:   "repo@main",
		Repo:        "repo",
		Text:        "hello coordinator",
		Urgency:     "normal",
		SentAt:      time.Now(),
	}

	if err := d.WriteBusMessageDelivered(msg); err != nil {
		t.Fatalf("WriteBusMessageDelivered: %v", err)
	}

	var deliveredAt *int64
	var failedAt *int64
	if err := d.QueryRow(
		"SELECT delivered_at, failed_at FROM bus_messages WHERE id = ?", msgID,
	).Scan(&deliveredAt, &failedAt); err != nil {
		t.Fatalf("scan bus_message row: %v", err)
	}
	if deliveredAt == nil {
		t.Error("delivered_at: got nil, want non-nil after successful delivery")
	}
	if failedAt != nil {
		t.Errorf("failed_at: got %v, want nil (should not be set on success)", failedAt)
	}
}

// TestUpdateHarnessSessionID verifies that UpdateHarnessSessionID unconditionally
// overwrites the stored harness_session_id (unlike COALESCE-based upserts).
func TestUpdateHarnessSessionID(t *testing.T) {
	d := openTestDB(t)

	oldSID := "old-session-id"
	newSID := "new-session-id"

	// Create a row with an initial SID.
	if err := d.UpsertStatus("repo@main", "repo", "/wt", "active", nil, &oldSID); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	s, _ := d.CurrentStatus("repo@main")
	if s.HarnessSessionID == nil || *s.HarnessSessionID != oldSID {
		t.Fatalf("pre-condition: harness_session_id = %v, want %q", s.HarnessSessionID, oldSID)
	}

	// UpdateHarnessSessionID must overwrite unconditionally.
	if err := d.UpdateHarnessSessionID("repo@main", newSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	s2, _ := d.CurrentStatus("repo@main")
	if s2.HarnessSessionID == nil || *s2.HarnessSessionID != newSID {
		t.Errorf("harness_session_id after UpdateHarnessSessionID: got %v, want %q", s2.HarnessSessionID, newSID)
	}
}

// TestUpdateHarnessSessionID_NoopWhenNoRow verifies that UpdateHarnessSessionID is a
// no-op when the session does not exist in agent_status.
func TestUpdateHarnessSessionID_NoopWhenNoRow(t *testing.T) {
	d := openTestDB(t)

	// Must not error when no row exists.
	if err := d.UpdateHarnessSessionID("nonexistent@branch", "some-sid"); err != nil {
		t.Errorf("UpdateHarnessSessionID on non-existent session: %v (want nil)", err)
	}
}

// TestSetHostMode verifies that SetHostMode sets and clears the host_mode flag.
func TestSetHostMode(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Initially host_mode should be false.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.HostMode {
		t.Error("HostMode: got true initially, want false")
	}

	// Set host_mode to true.
	if err := d.SetHostMode("repo@main", true); err != nil {
		t.Fatalf("SetHostMode(true): %v", err)
	}
	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after SetHostMode(true): %v", err)
	}
	if !s2.HostMode {
		t.Error("HostMode: got false after SetHostMode(true), want true")
	}

	// Set host_mode back to false.
	if err := d.SetHostMode("repo@main", false); err != nil {
		t.Fatalf("SetHostMode(false): %v", err)
	}
	s3, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after SetHostMode(false): %v", err)
	}
	if s3.HostMode {
		t.Error("HostMode: got true after SetHostMode(false), want false")
	}

	// SetHostMode on a non-existent session should be a no-op (not an error).
	if err := d.SetHostMode("repo@nonexistent", true); err != nil {
		t.Fatalf("SetHostMode on nonexistent session: %v", err)
	}
}

// TestClearEnded_RestoresVisibility verifies the tmux-session-start scenario:
// a session that was ended (via SetEnded) becomes visible again in
// AllActiveStatus and AllActiveStatusForRepo after UpsertStatus + ClearEnded,
// matching the fix in cmd/event.go's tmux-session-start handler.
func TestClearEnded_RestoresVisibility(t *testing.T) {
	d := openTestDB(t)

	const session = "repo@main"
	const repo = "repo"
	const worktree = "/code/repo/main"

	// Insert initial status row via the tmux-session-start path.
	if err := d.UpsertStatus(session, repo, worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (initial idle): %v", err)
	}

	// Advance through the real lifecycle: idle → active → finished.
	// tmux-session-end fires after the session has progressed to a terminal
	// state; skipping these steps would cause spurious state-machine warnings.
	if err := d.UpsertStatus(session, repo, worktree, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (active): %v", err)
	}
	if err := d.UpsertStatus(session, repo, worktree, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (finished): %v", err)
	}

	// Simulate tmux-session-end: mark the session as ended.
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Confirm the session is now invisible to active-session queries.
	all, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus (after SetEnded): %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("AllActiveStatus after SetEnded: got %d rows, want 0", len(all))
	}

	// Simulate tmux-session-start: UpsertStatus then ClearEnded (the fix).
	if err := d.UpsertStatus(session, repo, worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (restart): %v", err)
	}
	if err := d.ClearEnded(session); err != nil {
		t.Fatalf("ClearEnded: %v", err)
	}

	// The session must now appear in AllActiveStatus.
	all2, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus (after ClearEnded): %v", err)
	}
	if len(all2) != 1 {
		t.Fatalf("AllActiveStatus after ClearEnded: got %d rows, want 1", len(all2))
	}
	if all2[0].SessionName != session {
		t.Errorf("AllActiveStatus: got %q, want %q", all2[0].SessionName, session)
	}
	if all2[0].State != "idle" {
		t.Errorf("State after restart: got %q, want \"idle\"", all2[0].State)
	}
	if all2[0].EndedAt != nil {
		t.Errorf("EndedAt after ClearEnded: got non-nil, want nil")
	}

	// The session must also appear in AllActiveStatusForRepo.
	forRepo, err := d.AllActiveStatusForRepo(repo)
	if err != nil {
		t.Fatalf("AllActiveStatusForRepo (after ClearEnded): %v", err)
	}
	if len(forRepo) != 1 {
		t.Fatalf("AllActiveStatusForRepo after ClearEnded: got %d rows, want 1", len(forRepo))
	}
	if forRepo[0].SessionName != session {
		t.Errorf("AllActiveStatusForRepo: got %q, want %q", forRepo[0].SessionName, session)
	}
}

// TestCheckTransition_SameState verifies that upserting the same state that is
// already stored produces no "invalid transition" log output and completes
// without error, for every defined agent state.
//
// The test captures os.Stderr by redirecting it to a pipe, then checks that
// the captured output is empty after a same-state upsert.  The session is
// first inserted with the initial state (which goes through the fresh-insert
// path — no transition logged), then a second upsert with the identical state
// is performed — this is the same-state path being exercised.
func TestCheckTransition_SameState(t *testing.T) {
	states := []string{"idle", "active", "error", "finished", "waiting", "compacting", "interrupted", "deleted"}

	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			d := openTestDB(t)

			// Insert initial row (fresh insert — no transition to validate).
			if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", state, nil, nil); err != nil {
				t.Fatalf("initial UpsertStatus: %v", err)
			}

			// Redirect stderr to capture any log output from checkTransition.
			origStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			os.Stderr = w

			// Same-state upsert — must not log anything.
			upsertErr := d.UpsertStatus("repo@main", "repo", "/code/repo/main", state, nil, nil)

			// Restore stderr and read any output that was written.
			w.Close()
			os.Stderr = origStderr
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, r); err != nil {
				t.Fatalf("read captured stderr: %v", err)
			}
			r.Close()

			if upsertErr != nil {
				t.Fatalf("same-state UpsertStatus: %v", upsertErr)
			}
			if buf.Len() != 0 {
				t.Errorf("same-state upsert produced unexpected log output for state %q:\n%s", state, buf.String())
			}
		})
	}
}

// TestCheckTransition_InvalidTransition verifies that a genuinely invalid
// transition (e.g. idle → error, idle → finished) still logs a warning to
// stderr after the same-state early-return fix is in place.
func TestCheckTransition_InvalidTransition(t *testing.T) {
	// "error → active" is valid per ValidTransitions; use only pairs that are
	// genuinely invalid (not present in ValidTransitions).
	invalidPairs := []struct {
		from string
		to   string
	}{
		{"idle", "error"},
		{"idle", "finished"},
		{"deleted", "active"},
		{"idle", "waiting"},
	}

	for _, pair := range invalidPairs {
		t.Run(pair.from+"→"+pair.to, func(t *testing.T) {
			d := openTestDB(t)

			// Insert initial row with fromState.
			if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", pair.from, nil, nil); err != nil {
				t.Fatalf("initial UpsertStatus: %v", err)
			}

			// Redirect stderr to capture log output.
			origStderr := os.Stderr
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			os.Stderr = w

			// Invalid-transition upsert — must log a warning.
			upsertErr := d.UpsertStatus("repo@main", "repo", "/code/repo/main", pair.to, nil, nil)

			w.Close()
			os.Stderr = origStderr
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, r); err != nil {
				t.Fatalf("read captured stderr: %v", err)
			}
			r.Close()

			if upsertErr != nil {
				t.Fatalf("UpsertStatus with invalid transition: %v", upsertErr)
			}
			if buf.Len() == 0 {
				t.Errorf("expected warning log for invalid transition %q → %q but got no output",
					pair.from, pair.to)
			}
		})
	}
}

// TestCheckTransition_ValidTransitionNotSuppressed verifies that a valid
// (state-changing) transition — e.g. idle → active — is NOT suppressed by
// the same-state short-circuit and continues to pass through without logging.
func TestCheckTransition_ValidTransitionNotSuppressed(t *testing.T) {
	d := openTestDB(t)

	// Insert initial idle row.
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("initial UpsertStatus: %v", err)
	}

	// Capture stderr.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	// idle → active is a valid transition per ValidTransitions — must not log.
	upsertErr := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil)

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	r.Close()

	if upsertErr != nil {
		t.Fatalf("UpsertStatus (idle→active): %v", upsertErr)
	}
	if buf.Len() != 0 {
		t.Errorf("valid transition idle→active produced unexpected log output:\n%s", buf.String())
	}

	// Confirm the state was actually updated.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.State != "active" {
		t.Errorf("State after idle→active: got %q, want \"active\"", s.State)
	}
}

// TestCheckTransition_EmptyStates verifies that checkTransition does not panic
// when called with an empty string for either fromState (no row in DB) or
// toState (empty target state). This is exercised indirectly via UpsertStatus.
func TestCheckTransition_EmptyStates(t *testing.T) {
	d := openTestDB(t)

	// No prior row: fresh insert with empty-string state must not panic.
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "", nil, nil); err != nil {
		// A DB error is acceptable (empty state constraint); the key thing is no panic.
		t.Logf("UpsertStatus with empty state returned error (acceptable): %v", err)
	}

	// Session with a valid state, then upsert with empty toState — must not panic.
	d2 := openTestDB(t)
	if err := d2.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("initial UpsertStatus: %v", err)
	}
	// This exercises the fromState=idle, toState="" path through checkTransition.
	if err := d2.UpsertStatus("repo@main", "repo", "/code/repo/main", "", nil, nil); err != nil {
		t.Logf("UpsertStatus with empty toState returned error (acceptable): %v", err)
	}
	// If we reach here, no panic occurred.
}

// ── TestOpen_CreatesSchema addendum: session_groups and group_id column ────────

// TestOpen_CreatesSessionGroupsTable verifies that the session_groups table and
// the group_id column on agent_status exist after Open (fresh DB).
func TestOpen_CreatesSessionGroupsTable(t *testing.T) {
	d := openTestDB(t)

	// session_groups table must exist.
	var name string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='session_groups'").Scan(&name); err != nil {
		t.Fatalf("session_groups table not found: %v", err)
	}
	if name != "session_groups" {
		t.Errorf("session_groups: got %q, want \"session_groups\"", name)
	}

	// group_id column must exist on agent_status.
	// We probe it by inserting a NULL group_id row.
	if err := d.UpsertStatus("repo@main", "repo", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	var groupID *string
	if err := d.QueryRow("SELECT group_id FROM agent_status WHERE session_name = 'repo@main'").Scan(&groupID); err != nil {
		t.Fatalf("query group_id column: %v", err)
	}
	if groupID != nil {
		t.Errorf("group_id for new row: got %v, want nil", groupID)
	}
}

// ── Migration v8→v9 ────────────────────────────────────────────────────────────

// TestMigration_V8ToV9 verifies that Open applies the v8→v9 migration to an
// existing DB that was created at schema_version=8 (no session_groups table,
// no group_id column).
func TestMigration_V8ToV9(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v8.db")

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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT, harness_port INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (8);
		INSERT INTO agent_status (session_name, repo, worktree, state, harness, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'opencode', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v8 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v8 db: %v", err)
	}
	defer d.Close()

	// Schema version must be 10 after migration.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// session_groups table must exist after migration.
	var tname string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='session_groups'").Scan(&tname); err != nil {
		t.Fatalf("session_groups table not found after v8→v9 migration: %v", err)
	}

	// Existing row must still be present and unmodified.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}

	// group_id column must exist and be NULL for pre-migration rows.
	var groupID *string
	if err := d.QueryRow("SELECT group_id FROM agent_status WHERE session_name = 'repo@main'").Scan(&groupID); err != nil {
		t.Fatalf("query group_id column after migration: %v", err)
	}
	if groupID != nil {
		t.Errorf("group_id for migrated row: got %v, want nil", groupID)
	}
}

// TestMigration_V9ToV10 verifies that Open applies the v9→v10 migration to an
// existing DB that was created at schema_version=9 (no isolation_mode column).
func TestMigration_V9ToV10(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v9.db")

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
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT, opencode_sid TEXT,
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT, harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (9);
		INSERT INTO agent_status (session_name, repo, worktree, state, harness, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'opencode', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v9 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v9 db: %v", err)
	}
	defer d.Close()

	// Schema version must be 10 after migration.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// isolation_mode column must exist (NULL for pre-migration rows).
	var isoMode *string
	if err := d.QueryRow("SELECT isolation_mode FROM agent_status WHERE session_name = 'repo@main'").Scan(&isoMode); err != nil {
		t.Fatalf("query isolation_mode column after migration: %v", err)
	}
	if isoMode != nil {
		t.Errorf("isolation_mode for migrated row: got %q, want nil", *isoMode)
	}

	// Existing row must still be present and unmodified.
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus after migration: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want existing row")
	}
	if s.State != "active" {
		t.Errorf("State preserved: got %q, want \"active\"", s.State)
	}
	// IsolationMode for pre-migration rows must be "".
	if s.IsolationMode != "" {
		t.Errorf("IsolationMode: got %q, want \"\" (NULL → empty)", s.IsolationMode)
	}
	// EffectiveIsolationMode with HostMode=false falls back to "podman".
	if s.EffectiveIsolationMode() != "podman" {
		t.Errorf("EffectiveIsolationMode: got %q, want %q", s.EffectiveIsolationMode(), "podman")
	}
}

// ── RegisterGroup, GroupCompleted, GroupResults ───────────────────────────────

// TestRegisterGroup verifies that RegisterGroup inserts a row into
// session_groups and returns a non-empty group_id.
func TestRegisterGroup(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if groupID == "" {
		t.Fatal("RegisterGroup returned empty group_id")
	}

	// The row must be present in session_groups.
	var parent string
	if err := d.QueryRow(
		"SELECT parent_session FROM session_groups WHERE group_id = ?", groupID,
	).Scan(&parent); err != nil {
		t.Fatalf("query session_groups: %v", err)
	}
	if parent != "nixos-config@feature" {
		t.Errorf("parent_session: got %q, want \"nixos-config@feature\"", parent)
	}

	// Each call must return a distinct group_id.
	groupID2, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("second RegisterGroup: %v", err)
	}
	if groupID2 == groupID {
		t.Error("RegisterGroup: second call returned same group_id as first")
	}
}

// TestGroupCompleted_AllTerminal verifies that GroupCompleted returns true when
// all members have reached a terminal state.
func TestGroupCompleted_AllTerminal(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Create three member sessions and assign them to the group.
	members := []struct {
		name  string
		state string
	}{
		{"nixos-config@feature~review-1-goal", "finished"},
		{"nixos-config@feature~review-1-code", "finished"},
		{"nixos-config@feature~review-1-security", "interrupted"},
	}
	for _, m := range members {
		if err := d.UpsertStatus(m.name, "nixos-config", "/wt", m.state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", m.name, err)
		}
		if err := d.QueryRow(
			"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
			groupID, m.name,
		).Scan(new(int)); err != nil {
			t.Fatalf("set group_id for %s: %v", m.name, err)
		}
	}

	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted: %v", err)
	}
	if !done {
		t.Error("GroupCompleted: got false, want true (all members terminal)")
	}
}

// TestGroupCompleted_ActiveMember verifies that GroupCompleted returns false
// when at least one member is still active.
func TestGroupCompleted_ActiveMember(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	members := []struct {
		name  string
		state string
	}{
		{"nixos-config@feature~review-1-goal", "finished"},
		{"nixos-config@feature~review-1-code", "active"}, // still running
	}
	for _, m := range members {
		if err := d.UpsertStatus(m.name, "nixos-config", "/wt", m.state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", m.name, err)
		}
		if err := d.QueryRow(
			"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
			groupID, m.name,
		).Scan(new(int)); err != nil {
			t.Fatalf("set group_id for %s: %v", m.name, err)
		}
	}

	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted: %v", err)
	}
	if done {
		t.Error("GroupCompleted: got true, want false (active member present)")
	}
}

// TestGroupCompleted_NoMembers verifies that GroupCompleted returns true for a
// group that exists but has no members assigned yet.
func TestGroupCompleted_NoMembers(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted: %v", err)
	}
	// No members = all zero members are terminal = true.
	if !done {
		t.Error("GroupCompleted with no members: got false, want true")
	}
}

// TestGroupResults verifies that GroupResults returns state and last message for
// all group members, keyed by session_name.
func TestGroupResults(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Create two members.
	for _, name := range []string{"nixos-config@feature~review-1-goal", "nixos-config@feature~review-1-code"} {
		if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", name, err)
		}
		if err := d.QueryRow(
			"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
			groupID, name,
		).Scan(new(int)); err != nil {
			t.Fatalf("set group_id for %s: %v", name, err)
		}
	}

	// Write a msg_assistant event for the first member only.
	goalSession := "nixos-config@feature~review-1-goal"
	if err := d.WriteEvent(db.Event{
		ID:          "evt-assistant-1",
		SessionName: goalSession,
		Repo:        "nixos-config",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     `{"content":"<verdict>PASS</verdict>"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	results, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("GroupResults: got %d results, want 2", len(results))
	}

	goalResult, ok := results[goalSession]
	if !ok {
		t.Fatalf("GroupResults: missing entry for %q", goalSession)
	}
	if goalResult.State != "finished" {
		t.Errorf("goal State: got %q, want \"finished\"", goalResult.State)
	}
	if goalResult.LastMessage != `{"content":"<verdict>PASS</verdict>"}` {
		t.Errorf("goal LastMessage: got %q, want msg_assistant payload", goalResult.LastMessage)
	}

	codeSession := "nixos-config@feature~review-1-code"
	codeResult, ok := results[codeSession]
	if !ok {
		t.Fatalf("GroupResults: missing entry for %q", codeSession)
	}
	if codeResult.State != "finished" {
		t.Errorf("code State: got %q, want \"finished\"", codeResult.State)
	}
	if codeResult.LastMessage != "" {
		t.Errorf("code LastMessage: got %q, want \"\" (no msg_assistant)", codeResult.LastMessage)
	}
}

// ── Foreign key enforcement tests ─────────────────────────────────────────────

// TestGroupFK_Violation verifies that attempting to set agent_status.group_id to
// a value not present in session_groups raises a FK constraint error. This proves
// PRAGMA foreign_keys = ON is active for every connection opened by db.Open.
func TestGroupFK_Violation(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Attempt to set group_id to a non-existent group — must fail with FK error.
	err := d.QueryRow(
		"UPDATE agent_status SET group_id = 'does-not-exist' WHERE session_name = 'repo@main' RETURNING 1",
	).Scan(new(int))
	if err == nil {
		t.Fatal("expected FK constraint error when setting group_id to non-existent group, got nil")
	}
	// The error message from SQLite for FK violations contains "FOREIGN KEY".
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Errorf("error should mention FOREIGN KEY constraint: got %q", err.Error())
	}
}

// TestGroupFK_OnDeleteSetNull verifies that deleting a session_groups row clears
// agent_status.group_id (SET NULL cascade) for all members, while leaving all
// other columns untouched.
func TestGroupFK_OnDeleteSetNull(t *testing.T) {
	d := openTestDB(t)

	// Register a group.
	groupID, err := d.RegisterGroup("coordinator@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Create two member sessions and assign them to the group.
	for _, name := range []string{"coordinator@main~review-1-goal", "coordinator@main~review-1-code"} {
		if err := d.UpsertStatus(name, "repo", "/wt", "active", strPtr("My Title"), nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", name, err)
		}
		if err := d.QueryRow(
			"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
			groupID, name,
		).Scan(new(int)); err != nil {
			t.Fatalf("set group_id for %s: %v", name, err)
		}
	}

	// Confirm group_id is set for both members.
	var gid *string
	if err := d.QueryRow(
		"SELECT group_id FROM agent_status WHERE session_name = 'coordinator@main~review-1-goal'",
	).Scan(&gid); err != nil || gid == nil || *gid != groupID {
		t.Fatalf("pre-condition: group_id not set correctly: gid=%v, err=%v", gid, err)
	}

	// Delete the session_groups row — should cascade SET NULL to members.
	if err := d.QueryRow(
		"DELETE FROM session_groups WHERE group_id = ? RETURNING group_id", groupID,
	).Scan(new(string)); err != nil {
		t.Fatalf("delete session_groups row: %v", err)
	}

	// Both members should now have group_id = NULL.
	for _, name := range []string{"coordinator@main~review-1-goal", "coordinator@main~review-1-code"} {
		var groupIDAfter *string
		if err := d.QueryRow(
			"SELECT group_id FROM agent_status WHERE session_name = ?", name,
		).Scan(&groupIDAfter); err != nil {
			t.Fatalf("query group_id for %s: %v", name, err)
		}
		if groupIDAfter != nil {
			t.Errorf("group_id for %s after group deletion: got %v, want nil (SET NULL cascade)", name, *groupIDAfter)
		}

		// State and title must be preserved (other columns untouched).
		s, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus %s: %v", name, err)
		}
		if s == nil {
			t.Fatalf("CurrentStatus %s: got nil", name)
		}
		if s.State != "active" {
			t.Errorf("State for %s after group deletion: got %q, want \"active\"", name, s.State)
		}
		if s.Title == nil || *s.Title != "My Title" {
			t.Errorf("Title for %s after group deletion: got %v, want \"My Title\"", name, s.Title)
		}
	}
}

// seedV8DB creates a raw SQLite database at dbPath seeded with the v8 schema
// and an existing agent_status row, simulating a real pre-migration database.
func seedV8DB(t *testing.T, dbPath string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v8 db: %v", err)
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
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  opencode_port INTEGER, host_mode INTEGER NOT NULL DEFAULT 0,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT, harness_port INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (8);
		INSERT INTO agent_status (session_name, repo, worktree, state, harness, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'opencode', 0);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v8 db: %v", err)
	}
}

// TestGroupFK_Violation_MigratedDB verifies that FK enforcement works on a
// database that was migrated from v8 to v9 (not freshly created). This is
// the critical production path: most deployed prism instances will arrive
// at v9 via migration, not via a fresh Open. The rename-and-recreate pattern
// used in the migration ensures the REFERENCES clause is present in the
// schema metadata, so PRAGMA foreign_keys = ON can enforce it.
func TestGroupFK_Violation_MigratedDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v8_fk_violation.db")
	seedV8DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v8 db: %v", err)
	}
	defer d.Close()

	// Attempt to set group_id to a non-existent group — must fail with FK error.
	err = d.QueryRow(
		"UPDATE agent_status SET group_id = 'does-not-exist' WHERE session_name = 'repo@main' RETURNING 1",
	).Scan(new(int))
	if err == nil {
		t.Fatal("expected FK constraint error on migrated DB when setting group_id to non-existent group, got nil")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Errorf("error should mention FOREIGN KEY constraint on migrated DB: got %q", err.Error())
	}
}

// TestGroupFK_OnDeleteSetNull_MigratedDB verifies that the ON DELETE SET NULL
// cascade works on a database that was migrated from v8 to v9. The
// rename-and-recreate migration pattern is required for this to work: a plain
// ALTER TABLE ADD COLUMN cannot carry a REFERENCES clause, so without the
// recreate the cascade would silently do nothing.
func TestGroupFK_OnDeleteSetNull_MigratedDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v8_fk_cascade.db")
	seedV8DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v8 db: %v", err)
	}
	defer d.Close()

	// Register a group, assign the existing row to it.
	groupID, err := d.RegisterGroup("coordinator@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = 'repo@main' RETURNING 1",
		groupID,
	).Scan(new(int)); err != nil {
		t.Fatalf("set group_id: %v", err)
	}

	// Confirm group_id is set.
	var gid *string
	if err := d.QueryRow(
		"SELECT group_id FROM agent_status WHERE session_name = 'repo@main'",
	).Scan(&gid); err != nil || gid == nil || *gid != groupID {
		t.Fatalf("pre-condition: group_id not set correctly: gid=%v, err=%v", gid, err)
	}

	// Delete the session_groups row — ON DELETE SET NULL must clear group_id.
	if err := d.QueryRow(
		"DELETE FROM session_groups WHERE group_id = ? RETURNING group_id", groupID,
	).Scan(new(string)); err != nil {
		t.Fatalf("delete session_groups: %v", err)
	}

	// group_id must now be NULL.
	var groupIDAfter *string
	if err := d.QueryRow(
		"SELECT group_id FROM agent_status WHERE session_name = 'repo@main'",
	).Scan(&groupIDAfter); err != nil {
		t.Fatalf("query group_id after cascade: %v", err)
	}
	if groupIDAfter != nil {
		t.Errorf("group_id after ON DELETE SET NULL on migrated DB: got %v, want nil", *groupIDAfter)
	}

	// State must be preserved (other columns untouched).
	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}
	if s.State != "active" {
		t.Errorf("State after cascade on migrated DB: got %q, want \"active\"", s.State)
	}
}

// setPort is a test helper that writes harness_port directly via QueryRow.
func setPort(d *db.DB, sessionName string, port int) error {
	// Use QueryRow with a dummy scan to execute the UPDATE.
	var dummy int
	err := d.QueryRow(
		"UPDATE agent_status SET harness_port = ? WHERE session_name = ? RETURNING 1",
		port, sessionName,
	).Scan(&dummy)
	return err
}

// ── QueryAuditEvents tests ────────────────────────────────────────────────────

// writeAuditEvent is a test helper that writes an audit event to the DB.
func writeAuditEvent(t *testing.T, d *db.DB, sessionName, repo, command string, createdAt time.Time) {
	t.Helper()
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/code/" + repo + "/main",
		Type:        "audit",
		Payload:     fmt.Sprintf(`{"tool":"bash","command":%q,"sessionName":%q}`, command, sessionName),
		CreatedAt:   createdAt,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent audit: %v", err)
	}
}

func TestQueryAuditEvents_NoFilter(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 1", now.Add(-3*time.Hour))
	writeAuditEvent(t, d, "repo@feat", "repo", "git push origin feat", now.Add(-2*time.Hour))
	writeAuditEvent(t, d, "repo@feat", "repo", "gh pr create --title foo", now.Add(-1*time.Hour))

	// Also write a non-audit event — should not appear.
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "tool_call",
		Payload:     `{"tool":"bash","args":"ls"}`,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("WriteEvent tool_call: %v", err)
	}

	events, err := d.QueryAuditEvents("", 0, "", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	// Default limit is 20; all 3 audit events should be present.
	if len(events) != 3 {
		t.Errorf("got %d events, want 3", len(events))
	}
	// Verify all returned events have type=audit.
	for _, e := range events {
		if e.Type != "audit" {
			t.Errorf("unexpected event type %q", e.Type)
		}
	}
}

func TestQueryAuditEvents_SessionFilter(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 1", now.Add(-2*time.Hour))
	writeAuditEvent(t, d, "repo@feat", "repo", "git push origin feat", now.Add(-1*time.Hour))

	events, err := d.QueryAuditEvents("repo@main", 0, "", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].SessionName != "repo@main" {
		t.Errorf("sessionName = %q, want repo@main", events[0].SessionName)
	}
}

func TestQueryAuditEvents_SinceFilter(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 1", old)
	writeAuditEvent(t, d, "repo@main", "repo", "git push origin main", recent)

	// Filter to last 24h.
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()
	events, err := d.QueryAuditEvents("", sinceMs, "", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (only recent)", len(events))
	}
}

func TestQueryAuditEvents_PatternFilter(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 1 --squash", now.Add(-3*time.Hour))
	writeAuditEvent(t, d, "repo@main", "repo", "git push origin main", now.Add(-2*time.Hour))
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr create --title foo", now.Add(-1*time.Hour))

	// Pattern matching "merge" should match only the first.
	events, err := d.QueryAuditEvents("", 0, "merge", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if !strings.Contains(events[0].Payload, "merge") {
		t.Errorf("event payload does not contain 'merge': %s", events[0].Payload)
	}
}

func TestQueryAuditEvents_LimitFilter(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	for i := 0; i < 5; i++ {
		writeAuditEvent(t, d, "repo@main", "repo", fmt.Sprintf("gh pr merge %d", i), now.Add(time.Duration(i)*time.Minute))
	}

	events, err := d.QueryAuditEvents("", 0, "", 3)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("got %d events, want 3", len(events))
	}
}

func TestQueryAuditEvents_OrderedDescending(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 1", now.Add(-2*time.Hour))
	writeAuditEvent(t, d, "repo@main", "repo", "gh pr merge 2", now.Add(-1*time.Hour))

	events, err := d.QueryAuditEvents("", 0, "", 0)
	if err != nil {
		t.Fatalf("QueryAuditEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	// Results should be DESC (newest first).
	if !events[0].CreatedAt.After(events[1].CreatedAt) {
		t.Errorf("events not in DESC order: [0]=%v [1]=%v", events[0].CreatedAt, events[1].CreatedAt)
	}
}

// ── AllStatusesWithPrefix ──────────────────────────────────────────────────────

func TestAllStatusesWithPrefix_ReturnsMatchingRows(t *testing.T) {
	d := openTestDB(t)

	// Insert rows with and without the target prefix.
	parent := "nixos-config@feature"
	if err := d.UpsertStatus(parent+"~review-1", "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.UpsertStatus(parent+"~review-2", "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// Agent sub-session: also starts with the prefix.
	if err := d.UpsertStatus(parent+"~review-1~review", "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// Unrelated row.
	if err := d.UpsertStatus("nixos-config@other~review-1", "nixos-config", "/wt", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	rows, err := d.AllStatusesWithPrefix(parent + "~review-")
	if err != nil {
		t.Fatalf("AllStatusesWithPrefix: %v", err)
	}

	// Should return 3 rows (2 rounds + 1 sub-session), not the unrelated row.
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3; rows: %v", len(rows), sessionNames(rows))
	}
	for _, r := range rows {
		if r.SessionName == "nixos-config@other~review-1" {
			t.Errorf("unexpected row %q returned", r.SessionName)
		}
	}
}

func TestAllStatusesWithPrefix_UnderscoreInPrefix_ExactMatch(t *testing.T) {
	d := openTestDB(t)

	// Session name with underscore — the prefix should match exactly (not use _ as wildcard).
	_ = d.UpsertStatus("repo@feat_ure~review-1", "repo", "/wt", "idle", nil, nil)
	_ = d.UpsertStatus("repo@featXure~review-1", "repo", "/wt", "idle", nil, nil) // should NOT match

	rows, err := d.AllStatusesWithPrefix("repo@feat_ure~review-")
	if err != nil {
		t.Fatalf("AllStatusesWithPrefix: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 (exact underscore match); names: %v", len(rows), sessionNames(rows))
	}
	if len(rows) > 0 && rows[0].SessionName != "repo@feat_ure~review-1" {
		t.Errorf("got %q, want %q", rows[0].SessionName, "repo@feat_ure~review-1")
	}
}

func TestAllStatusesWithPrefix_IncludesEndedRows(t *testing.T) {
	d := openTestDB(t)

	_ = d.UpsertStatus("nixos@feat~review-1", "nixos", "/wt", "finished", nil, nil)
	_ = d.SetEnded("nixos@feat~review-1")

	rows, err := d.AllStatusesWithPrefix("nixos@feat~review-")
	if err != nil {
		t.Fatalf("AllStatusesWithPrefix: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 (ended row should be included)", len(rows))
	}
}

func sessionNames(rows []db.Status) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.SessionName
	}
	return names
}

// ─── ConsecutiveSidecarFailures tests ────────────────────────────────────────

// writeStateChange is a test helper that inserts a state_change event for
// the given session with the given state value.
func writeStateChange(t *testing.T, d *db.DB, sessionName, state string) {
	t.Helper()
	id := uuid.New().String()
	if err := d.WriteEvent(db.Event{
		ID:          id,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "state_change",
		Payload:     `{"state":"` + state + `"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent state_change(%s): %v", state, err)
	}
}

// TestConsecutiveSidecarFailures_NoHistory verifies that a session with no
// recorded terminal state_change events returns 0 consecutive failures.
// Brand-new and pre-existing sessions should always be restored normally.
func TestConsecutiveSidecarFailures_NoHistory(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@main", "repo", "/wt", "idle", nil, nil)

	n, err := d.ConsecutiveSidecarFailures("repo@main", 10)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d, want 0 (no history)", n)
	}
}

// TestConsecutiveSidecarFailures_NMinusOneFailures verifies that N-1
// consecutive non-successful terminal states do NOT reach the threshold — the
// session should still be restored.
func TestConsecutiveSidecarFailures_NMinusOneFailures(t *testing.T) {
	const threshold = 3
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@feat", "repo", "/wt", "idle", nil, nil)

	// Write threshold-1 == 2 interrupted state_change events.
	for i := 0; i < threshold-1; i++ {
		writeStateChange(t, d, "repo@feat", "interrupted")
	}

	n, err := d.ConsecutiveSidecarFailures("repo@feat", threshold)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != threshold-1 {
		t.Errorf("got %d, want %d (N-1 failures)", n, threshold-1)
	}
	if n >= threshold {
		t.Errorf("N-1 failures should not reach threshold %d", threshold)
	}
}

// TestConsecutiveSidecarFailures_ExactlyNFailures verifies that exactly N
// consecutive non-successful terminal states reaches (and equals) the threshold,
// causing the circuit breaker to trip.
func TestConsecutiveSidecarFailures_ExactlyNFailures(t *testing.T) {
	const threshold = 3
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@broken", "repo", "/wt", "idle", nil, nil)

	for i := 0; i < threshold; i++ {
		writeStateChange(t, d, "repo@broken", "interrupted")
	}

	n, err := d.ConsecutiveSidecarFailures("repo@broken", threshold)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != threshold {
		t.Errorf("got %d, want %d (exactly N failures)", n, threshold)
	}
	if n < threshold {
		t.Errorf("exactly N failures should reach threshold %d", threshold)
	}
}

// TestConsecutiveSidecarFailures_NFailuresThenSuccessThenFailure verifies that
// a single successful run ("finished") resets the consecutive-failure count.
// Pattern: fail, fail, fail, succeed, fail → count == 1 (not 4).
func TestConsecutiveSidecarFailures_NFailuresThenSuccessThenFailure(t *testing.T) {
	const threshold = 3
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@recovered", "repo", "/wt", "idle", nil, nil)

	// Write N failures, then a success, then one more failure (oldest → newest).
	for i := 0; i < threshold; i++ {
		writeStateChange(t, d, "repo@recovered", "interrupted")
	}
	writeStateChange(t, d, "repo@recovered", "finished")
	writeStateChange(t, d, "repo@recovered", "interrupted")

	n, err := d.ConsecutiveSidecarFailures("repo@recovered", threshold+1)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	// Most recent is "interrupted" (1 failure). The "finished" before it
	// resets the count, so we should see exactly 1 consecutive failure.
	if n != 1 {
		t.Errorf("got %d, want 1 (success between failures resets count)", n)
	}
	if n >= threshold {
		t.Errorf("count %d should not reach threshold %d after a success", n, threshold)
	}
}

// TestConsecutiveSidecarFailures_SuccessOnly verifies that a session whose last
// terminal state is "finished" returns 0 consecutive failures.
func TestConsecutiveSidecarFailures_SuccessOnly(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@clean", "repo", "/wt", "idle", nil, nil)

	writeStateChange(t, d, "repo@clean", "interrupted")
	writeStateChange(t, d, "repo@clean", "interrupted")
	writeStateChange(t, d, "repo@clean", "finished") // most recent is a success

	n, err := d.ConsecutiveSidecarFailures("repo@clean", 5)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d, want 0 (most recent is finished)", n)
	}
}

// TestConsecutiveSidecarFailures_ErrorStateCountsAsFailure verifies that
// "error" terminal states are counted as failures (not successes).
func TestConsecutiveSidecarFailures_ErrorStateCountsAsFailure(t *testing.T) {
	const threshold = 3
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@erroring", "repo", "/wt", "idle", nil, nil)

	writeStateChange(t, d, "repo@erroring", "error")
	writeStateChange(t, d, "repo@erroring", "interrupted")
	writeStateChange(t, d, "repo@erroring", "error")

	n, err := d.ConsecutiveSidecarFailures("repo@erroring", threshold)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != threshold {
		t.Errorf("got %d, want %d (error states count as failures)", n, threshold)
	}
}

// TestConsecutiveSidecarFailures_IgnoresNonTerminalStateChanges verifies that
// state_change events with non-terminal states (idle, active, waiting, etc.)
// are not included in the failure count. Only terminal states matter.
func TestConsecutiveSidecarFailures_IgnoresNonTerminalStateChanges(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@active", "repo", "/wt", "idle", nil, nil)

	// A bunch of non-terminal state changes followed by one failure.
	writeStateChange(t, d, "repo@active", "idle")
	writeStateChange(t, d, "repo@active", "active")
	writeStateChange(t, d, "repo@active", "waiting")
	writeStateChange(t, d, "repo@active", "interrupted") // first terminal state

	n, err := d.ConsecutiveSidecarFailures("repo@active", 5)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d, want 1 (only terminal states counted)", n)
	}
}

// TestConsecutiveSidecarFailures_HistoryQueryError verifies that a broken DB
// connection (simulated via a closed DB) returns an error and a count of 0.
// The restore code must fall back to restoring the session normally in this case.
func TestConsecutiveSidecarFailures_HistoryQueryError(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@main", "repo", "/wt", "idle", nil, nil)

	// Close the DB before the query to force an error.
	if err := d.Close(); err != nil {
		t.Fatalf("close DB: %v", err)
	}
	// Prevent t.Cleanup from double-closing (openTestDB registers a Cleanup).
	// The Close() above should succeed; the deferred Close() from openTestDB
	// will return an error on the closed connection, which is fine.

	n, err := d.ConsecutiveSidecarFailures("repo@main", 5)
	if err == nil {
		t.Error("expected error from closed DB, got nil")
	}
	if n != 0 {
		t.Errorf("got count %d, want 0 on error", n)
	}
}

// TestConsecutiveSidecarFailures_TieBreaker exercises the rowid DESC
// tie-breaker directly. Two state_change events are inserted with an
// IDENTICAL created_at timestamp (explicitly set, not relying on time.Now()).
// The first insertion is "interrupted"; the second (later rowid) is "finished".
// ConsecutiveSidecarFailures must return 0 because the later-inserted
// "finished" is considered the most-recent event.
func TestConsecutiveSidecarFailures_TieBreaker(t *testing.T) {
	d := openTestDB(t)
	_ = d.UpsertStatus("repo@tie", "repo", "/wt", "idle", nil, nil)

	// Use a fixed timestamp so both rows have exactly the same created_at.
	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Insert "interrupted" first (lower rowid).
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@tie",
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "state_change",
		Payload:     `{"state":"interrupted"}`,
		CreatedAt:   fixedTime,
	}); err != nil {
		t.Fatalf("WriteEvent interrupted: %v", err)
	}

	// Insert "finished" second (higher rowid) — same created_at.
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@tie",
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "state_change",
		Payload:     `{"state":"finished"}`,
		CreatedAt:   fixedTime,
	}); err != nil {
		t.Fatalf("WriteEvent finished: %v", err)
	}

	n, err := d.ConsecutiveSidecarFailures("repo@tie", 5)
	if err != nil {
		t.Fatalf("ConsecutiveSidecarFailures: %v", err)
	}
	// "finished" was inserted last (higher rowid) so rowid DESC makes it first
	// in the result set. The loop should stop immediately → 0 consecutive failures.
	if n != 0 {
		t.Errorf("got %d, want 0 (tie-break: later-inserted finished is most recent)", n)
	}
}

// TestUpsertStatusSeedRootAgentName_Insert verifies that a fresh insert via
// UpsertStatusSeedRootAgentName sets root_agent_name from the first moment.
func TestUpsertStatusSeedRootAgentName_Insert(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "idle", nil, nil, "worker"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if s.RootAgentName == nil {
		t.Fatal("RootAgentName: got nil, want \"worker\"")
	}
	if *s.RootAgentName != "worker" {
		t.Errorf("RootAgentName: got %q, want \"worker\"", *s.RootAgentName)
	}
	// agent_name should NOT be set by this method (spawn-time seeding only
	// writes root_agent_name, not the transient agent_name).
	if s.AgentName != nil {
		t.Errorf("AgentName: got %q, want nil (UpsertStatusSeedRootAgentName must not write agent_name)", *s.AgentName)
	}
}

// TestUpsertStatusSeedRootAgentName_EmptyRole verifies that when rootAgentName
// is empty, the method behaves like UpsertStatus and leaves root_agent_name NULL.
func TestUpsertStatusSeedRootAgentName_EmptyRole(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "idle", nil, nil, ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName (empty role): %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	// root_agent_name must remain NULL when rootAgentName is empty.
	if s.RootAgentName != nil {
		t.Errorf("RootAgentName: got %q, want nil (empty role must not write root_agent_name)", *s.RootAgentName)
	}
}

// TestUpsertStatusSeedRootAgentName_Idempotent verifies that calling
// UpsertStatusSeedRootAgentName with the same role twice leaves the row with
// the original root_agent_name intact (COALESCE preserves the existing value
// — the sidecar's subsequent write of the same value is a no-op).
func TestUpsertStatusSeedRootAgentName_Idempotent(t *testing.T) {
	d := openTestDB(t)

	// First call: seed with "review-code".
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "idle", nil, nil, "review-code"); err != nil {
		t.Fatalf("first UpsertStatusSeedRootAgentName: %v", err)
	}

	// Second call: write the same role — must be a no-op for root_agent_name.
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "active", nil, nil, "review-code"); err != nil {
		t.Fatalf("second UpsertStatusSeedRootAgentName: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}
	if s.RootAgentName == nil || *s.RootAgentName != "review-code" {
		t.Errorf("RootAgentName: got %v, want \"review-code\" (idempotent write)", s.RootAgentName)
	}
	// State should be updated to "active" (non-root fields still update).
	if s.State != "active" {
		t.Errorf("State: got %q, want \"active\"", s.State)
	}
}

// TestUpsertStatusSeedRootAgentName_PreservesExisting verifies that when a row
// already has root_agent_name set (e.g. by a prior seed call), a subsequent
// call with an empty rootAgentName preserves the existing value (COALESCE).
func TestUpsertStatusSeedRootAgentName_PreservesExisting(t *testing.T) {
	d := openTestDB(t)

	// Seed with a known role.
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Update with empty role — existing root_agent_name must survive.
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "active", nil, nil, ""); err != nil {
		t.Fatalf("update with empty role: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}
	if s.RootAgentName == nil || *s.RootAgentName != "coordinator" {
		t.Errorf("RootAgentName: got %v, want preserved \"coordinator\"", s.RootAgentName)
	}
}

// TestUpsertStatusSeedRootAgentName_SidecarWriteIdempotent verifies that when
// a row was seeded at spawn time (root_agent_name="review-code"), the sidecar's
// subsequent UpsertStatusWithRootAgent call with the same role is a no-op for
// root_agent_name (COALESCE in UpsertStatusWithRootAgent preserves the value).
func TestUpsertStatusSeedRootAgentName_SidecarWriteIdempotent(t *testing.T) {
	d := openTestDB(t)

	// Spawn-time seed: root_agent_name = "review-code", agent_name = nil.
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/repo/main", "idle", nil, nil, "review-code"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	s1, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (before sidecar): %v", err)
	}
	if s1 == nil || s1.RootAgentName == nil || *s1.RootAgentName != "review-code" {
		t.Fatalf("pre-condition: RootAgentName should be \"review-code\", got %v", s1.RootAgentName)
	}

	// Sidecar write: UpsertStatusWithRootAgent with the same role — must be
	// idempotent (root_agent_name stays "review-code", no error).
	agentName := strPtr("review-code")
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/code/repo/main", "active", nil, nil, agentName, nil); err != nil {
		t.Fatalf("UpsertStatusWithRootAgent (sidecar write): %v", err)
	}

	s2, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (after sidecar): %v", err)
	}
	if s2 == nil {
		t.Fatal("CurrentStatus: got nil after sidecar write")
	}
	if s2.RootAgentName == nil || *s2.RootAgentName != "review-code" {
		t.Errorf("RootAgentName after sidecar write: got %v, want \"review-code\" (idempotent)", s2.RootAgentName)
	}
	// State must be updated to "active" by the sidecar write.
	if s2.State != "active" {
		t.Errorf("State after sidecar write: got %q, want \"active\"", s2.State)
	}
}

// TestCoordinatorForRepo verifies the DB-backed coordinator lookup by repo.
func TestCoordinatorForRepo(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	// Seed a coordinator row with root_agent_name = "coordinator".
	if err := d.UpsertStatusSeedRootAgentName("myrepo@main", "myrepo", "/code/myrepo/main", "active", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	// Seed a worker row.
	if err := d.UpsertStatusSeedRootAgentName("myrepo@feature", "myrepo", "/code/myrepo/feature", "active", nil, nil, "worker"); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	// Happy path: find the coordinator for the repo.
	coord, err := d.CoordinatorForRepo("myrepo")
	if err != nil {
		t.Fatalf("CoordinatorForRepo: %v", err)
	}
	if coord == nil {
		t.Fatal("CoordinatorForRepo: got nil, want coordinator row")
	}
	if coord.SessionName != "myrepo@main" {
		t.Errorf("CoordinatorForRepo: SessionName = %q, want %q", coord.SessionName, "myrepo@main")
	}

	// No coordinator for an unknown repo returns nil.
	none, err := d.CoordinatorForRepo("other-repo")
	if err != nil {
		t.Fatalf("CoordinatorForRepo(other-repo): %v", err)
	}
	if none != nil {
		t.Errorf("CoordinatorForRepo(other-repo): got %v, want nil", none)
	}

	// Pre-migration row: a session named "oldrepo@main" with NULL root_agent_name
	// is NOT returned by CoordinatorForRepo (it requires the DB field).
	if err := d.UpsertStatus("oldrepo@main", "oldrepo", "/code/oldrepo/main", "active", nil, nil); err != nil {
		t.Fatalf("seed pre-migration coordinator: %v", err)
	}
	noPreMig, err := d.CoordinatorForRepo("oldrepo")
	if err != nil {
		t.Fatalf("CoordinatorForRepo(oldrepo): %v", err)
	}
	if noPreMig != nil {
		t.Errorf("CoordinatorForRepo(oldrepo): expected nil for pre-migration row (NULL root_agent_name), got %v", noPreMig)
	}

	// Ended coordinator should not be returned.
	if err := d.SetEnded("myrepo@main"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	ended, err := d.CoordinatorForRepo("myrepo")
	if err != nil {
		t.Fatalf("CoordinatorForRepo after SetEnded: %v", err)
	}
	if ended != nil {
		t.Errorf("CoordinatorForRepo after SetEnded: got %v, want nil", ended)
	}
}

// TestRootAgentName verifies the RootAgentName DB helper.
func TestRootAgentName(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	// Non-existent session returns ("", false, nil).
	name, rowExists, err := d.RootAgentName("nonexistent@session")
	if err != nil {
		t.Fatalf("RootAgentName(nonexistent): %v", err)
	}
	if name != "" {
		t.Errorf("RootAgentName(nonexistent): got %q, want empty", name)
	}
	if rowExists {
		t.Error("RootAgentName(nonexistent): got rowExists=true, want false")
	}

	// Pre-migration row: root_agent_name is NULL — returns ("", true, nil).
	if err := d.UpsertStatus("repo@old", "repo", "/code", "active", nil, nil); err != nil {
		t.Fatalf("seed pre-migration: %v", err)
	}
	nameNull, rowExistsNull, err := d.RootAgentName("repo@old")
	if err != nil {
		t.Fatalf("RootAgentName(pre-migration): %v", err)
	}
	if nameNull != "" {
		t.Errorf("RootAgentName(pre-migration): got %q, want empty for NULL", nameNull)
	}
	if !rowExistsNull {
		t.Error("RootAgentName(pre-migration): got rowExists=false, want true")
	}

	// Post-migration row: root_agent_name is populated — returns (name, true, nil).
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/code/main", "active", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed coordinator: %v", err)
	}
	nameCoord, rowExistsCoord, err := d.RootAgentName("repo@main")
	if err != nil {
		t.Fatalf("RootAgentName(coordinator): %v", err)
	}
	if nameCoord != "coordinator" {
		t.Errorf("RootAgentName(coordinator): got %q, want \"coordinator\"", nameCoord)
	}
	if !rowExistsCoord {
		t.Error("RootAgentName(coordinator): got rowExists=false, want true")
	}
}

// TestIsGroupMember verifies the IsGroupMember DB helper.
func TestIsGroupMember(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	// Seed a session and a group.
	if err := d.UpsertStatusSeedRootAgentName("repo@worker", "repo", "/code/worker", "active", nil, nil, "worker"); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	groupID, err := d.RegisterGroup("repo@worker")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Seed a review agent session and assign it to the group.
	if err := d.UpsertStatusSeedRootAgentName("repo@worker~review-1-review-goal", "repo", "/code/worker", "active", nil, nil, "review-goal"); err != nil {
		t.Fatalf("seed review agent: %v", err)
	}
	if err := d.SetGroupID("repo@worker~review-1-review-goal", groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	// The review agent IS a group member.
	isMember, err := d.IsGroupMember("repo@worker~review-1-review-goal")
	if err != nil {
		t.Fatalf("IsGroupMember(review agent): %v", err)
	}
	if !isMember {
		t.Error("IsGroupMember(review agent): got false, want true")
	}

	// The parent worker is NOT a group member (it is the parent, not a member).
	isParentMember, err := d.IsGroupMember("repo@worker")
	if err != nil {
		t.Fatalf("IsGroupMember(parent): %v", err)
	}
	if isParentMember {
		t.Error("IsGroupMember(parent worker): got true, want false (parent is not in a group)")
	}

	// Pre-migration row (NULL group_id) is not a group member.
	if err := d.UpsertStatus("repo@old-worker", "repo", "/code", "active", nil, nil); err != nil {
		t.Fatalf("seed pre-migration: %v", err)
	}
	isOldMember, err := d.IsGroupMember("repo@old-worker")
	if err != nil {
		t.Fatalf("IsGroupMember(pre-migration): %v", err)
	}
	if isOldMember {
		t.Error("IsGroupMember(pre-migration NULL group_id): got true, want false")
	}

	// Non-existent session returns false.
	isNone, err := d.IsGroupMember("nonexistent@session")
	if err != nil {
		t.Fatalf("IsGroupMember(nonexistent): %v", err)
	}
	if isNone {
		t.Error("IsGroupMember(nonexistent): got true, want false")
	}
}

// TestGroupMembersForParent verifies the GroupMembersForParent DB helper.
func TestGroupMembersForParent(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	// Empty result when no groups exist.
	rows, err := d.GroupMembersForParent("repo@worker")
	if err != nil {
		t.Fatalf("GroupMembersForParent (no groups): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("GroupMembersForParent (no groups): got %d rows, want 0", len(rows))
	}

	// Seed a worker session and register a group.
	if err := d.UpsertStatus("repo@worker", "repo", "/code/worker", "active", nil, nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	groupID, err := d.RegisterGroup("repo@worker")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Seed two reviewer sessions and assign them to the group.
	for _, name := range []string{"repo@worker~review-1-code", "repo@worker~review-2-goal"} {
		if err := d.UpsertStatus(name, "repo", "/code/"+name, "active", nil, nil); err != nil {
			t.Fatalf("seed reviewer %q: %v", name, err)
		}
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID %q: %v", name, err)
		}
	}

	// GroupMembersForParent should return both reviewer rows.
	members, err := d.GroupMembersForParent("repo@worker")
	if err != nil {
		t.Fatalf("GroupMembersForParent (with members): %v", err)
	}
	if len(members) != 2 {
		t.Errorf("GroupMembersForParent (with members): got %d rows, want 2", len(members))
	}

	// Different parent session returns empty.
	other, err := d.GroupMembersForParent("repo@other")
	if err != nil {
		t.Fatalf("GroupMembersForParent (other parent): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("GroupMembersForParent (other parent): got %d rows, want 0", len(other))
	}
}

// TestHasReviewGroup verifies the HasReviewGroup DB helper.
func TestHasReviewGroup(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()

	// Seed a worker session.
	if err := d.UpsertStatus("repo@worker", "repo", "/code/worker", "active", nil, nil); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	// Before registering any group, HasReviewGroup returns false.
	has, err := d.HasReviewGroup("repo@worker")
	if err != nil {
		t.Fatalf("HasReviewGroup (before group): %v", err)
	}
	if has {
		t.Error("HasReviewGroup (before group): got true, want false")
	}

	// Register a group for the worker.
	if _, err := d.RegisterGroup("repo@worker"); err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Now HasReviewGroup returns true.
	hasAfter, err := d.HasReviewGroup("repo@worker")
	if err != nil {
		t.Fatalf("HasReviewGroup (after group): %v", err)
	}
	if !hasAfter {
		t.Error("HasReviewGroup (after group): got false, want true")
	}

	// Different session returns false.
	hasOther, err := d.HasReviewGroup("repo@other")
	if err != nil {
		t.Fatalf("HasReviewGroup (other session): %v", err)
	}
	if hasOther {
		t.Error("HasReviewGroup (other session): got true, want false")
	}
}

// TestUpsertStatusWithAgent_WorktreeUpdatedOnConflict verifies that a second
// call to UpsertStatusWithAgent with the same session name but a different
// worktree overwrites the stored worktree rather than silently keeping the old
// value.
func TestUpsertStatusWithAgent_WorktreeUpdatedOnConflict(t *testing.T) {
	d := openTestDB(t)

	agentName := "opencode"
	modelID := "gpt-4o"

	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/old/worktree", "idle", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("first UpsertStatusWithAgent: %v", err)
	}
	if err := d.UpsertStatusWithAgent("repo@main", "repo", "/new/worktree", "active", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("second UpsertStatusWithAgent: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if s.Worktree != "/new/worktree" {
		t.Errorf("Worktree: got %q, want %q", s.Worktree, "/new/worktree")
	}
}

// TestUpsertStatusSeedRootAgentName_WorktreeUpdatedOnConflict verifies that a
// second call to UpsertStatusSeedRootAgentName with the same session name but a
// different worktree overwrites the stored worktree.
func TestUpsertStatusSeedRootAgentName_WorktreeUpdatedOnConflict(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/old/worktree", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("first UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName("repo@main", "repo", "/new/worktree", "active", nil, nil, "coordinator"); err != nil {
		t.Fatalf("second UpsertStatusSeedRootAgentName: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if s.Worktree != "/new/worktree" {
		t.Errorf("Worktree: got %q, want %q", s.Worktree, "/new/worktree")
	}
}

// TestUpsertStatusWithRootAgent_WorktreeUpdatedOnConflict verifies that a
// second call to UpsertStatusWithRootAgent with the same session name but a
// different worktree overwrites the stored worktree.
func TestUpsertStatusWithRootAgent_WorktreeUpdatedOnConflict(t *testing.T) {
	d := openTestDB(t)

	agentName := "opencode"
	modelID := "gpt-4o"

	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/old/worktree", "idle", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("first UpsertStatusWithRootAgent: %v", err)
	}
	if err := d.UpsertStatusWithRootAgent("repo@main", "repo", "/new/worktree", "active", nil, nil, &agentName, &modelID); err != nil {
		t.Fatalf("second UpsertStatusWithRootAgent: %v", err)
	}

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if s.Worktree != "/new/worktree" {
		t.Errorf("Worktree: got %q, want %q", s.Worktree, "/new/worktree")
	}
}

// ── Migration v12→v13: malformed session name cleanup (#826) ──────────────────

// seedV12DB creates a raw SQLite database at dbPath at schema_version=12 with
// the full current schema (including isolation_mode and group_id columns).
// It inserts the given agent_status rows directly so the v12→v13 migration can
// be exercised without going through db.Open.
func seedV12DB(t *testing.T, dbPath string, rows []struct {
	sessionName string
	lastSeen    int64 // 0 means store as 0 (simulates unpopulated)
	endedAt     *int64
}) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v12 db: %v", err)
	}
	defer rawConn.Close()

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, opencode_sid TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (12);
	`)
	if err != nil {
		t.Fatalf("seed v12 schema: %v", err)
	}

	for _, row := range rows {
		_, err = rawConn.Exec(
			`INSERT INTO agent_status (session_name, repo, worktree, state, last_seen, ended_at)
			 VALUES (?, 'repo', '/wt', 'interrupted', ?, ?)`,
			row.sessionName, row.lastSeen, row.endedAt,
		)
		if err != nil {
			t.Fatalf("insert row %q: %v", row.sessionName, err)
		}
	}
}

// TestMigration_V12ToV13_LegacyRowsEnded verifies that the v12→v13 migration
// sets ended_at on rows whose session_name matches the legacy malformed double-
// ~review patterns AND whose last_seen is NULL (0), zero, or old.
//
// Also verifies that the current valid shape <parent>~review-<N>-review-<role>
// is NOT matched (AC edge-case check from issue #826).
func TestMigration_V12ToV13_LegacyRowsEnded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12_malformed.db")

	// Build the set of test rows. last_seen=0 simulates the unpopulated state
	// that matches `last_seen IS NULL OR last_seen = 0` in the migration guard.
	var noEnded *int64 // ended_at = NULL (these are the rows we expect to be cleaned up)
	rows := []struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
		wantEnded   bool // whether we expect the migration to set ended_at
	}{
		// Legacy doubled-review with number: matched by %~review-%~review%
		{"nixos-config@fix-tmux~review-1-review~review-1-review", 0, noEnded, true},
		// Legacy back-to-back ~review~review: matched by %~review~review%
		{"nixos-config@fix-tmux~review-1~review", 0, noEnded, true},
		// Another back-to-back variant
		{"nixos-config@fix-tmux~review-3~review", 0, noEnded, true},
		// Variant with number in both positions
		{"nixos-config@fix-tmux~review-4~review~review-1~review", 0, noEnded, true},
		// Bare review suffix with no role (listed first in issue #826 example output):
		// matched by %~review-%-review
		{"nixos-config@fix-tmux-podman-keybinds~review-1-review", 0, noEnded, true},

		// *** MUST NOT be matched: current valid shape <parent>~review-<N>-review-<role> ***
		// These end in "-<role>" (non-empty after "-review-"), so the third LIKE
		// pattern (%~review-%-review) does NOT match them.
		{"nixos-config@fix-tmux~review-2-review-code", 0, noEnded, false},
		{"nixos-config@fix-tmux~review-1-review-goal", 0, noEnded, false},
		{"nixos-config@fix-tmux~review-3-review-security", 0, noEnded, false},

		// Normal sessions: must not be touched
		{"nixos-config@fix-tmux", 0, noEnded, false},
		{"nixos-config@main", 0, noEnded, false},
	}

	seedRows := make([]struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
	}, len(rows))
	for i, r := range rows {
		seedRows[i] = struct {
			sessionName string
			lastSeen    int64
			endedAt     *int64
		}{r.sessionName, r.lastSeen, r.endedAt}
	}
	seedV12DB(t, dbPath, seedRows)

	// Run the migration via db.Open.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Verify schema_version=13.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// Check each row.
	for _, r := range rows {
		s, err := d.CurrentStatus(r.sessionName)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", r.sessionName, err)
		}
		if s == nil {
			t.Fatalf("CurrentStatus(%q): got nil, want row", r.sessionName)
		}
		if r.wantEnded {
			if s.EndedAt == nil {
				t.Errorf("session %q: expected ended_at to be set by migration, got nil", r.sessionName)
			}
		} else {
			if s.EndedAt != nil {
				t.Errorf("session %q: expected ended_at to be nil (not matched by migration), got %v", r.sessionName, s.EndedAt)
			}
		}
	}
}

// TestMigration_V12ToV13_AlreadyEndedUntouched verifies that rows which already
// have ended_at set are not re-touched by the migration (idempotency of the
// ended_at guard).
func TestMigration_V12ToV13_AlreadyEndedUntouched(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12_already_ended.db")

	// A sentinel value we can use to verify ended_at was not overwritten.
	existingEndedAt := int64(1000000)

	rows := []struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
	}{
		// Legacy malformed name, BUT already ended — must NOT be updated.
		{"repo@feat~review-1~review", 0, &existingEndedAt},
		// Another legacy shape, already ended.
		{"repo@feat~review-1-review~review-1-review", 0, &existingEndedAt},
	}
	seedV12DB(t, dbPath, rows)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	for _, r := range rows {
		s, err := d.CurrentStatus(r.sessionName)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", r.sessionName, err)
		}
		if s == nil {
			t.Fatalf("CurrentStatus(%q): got nil", r.sessionName)
		}
		if s.EndedAt == nil {
			t.Errorf("session %q: ended_at was cleared (should have been preserved)", r.sessionName)
			continue
		}
		// ended_at must retain the original value, not be overwritten by the migration.
		gotMs := s.EndedAt.UnixMilli()
		if gotMs != existingEndedAt {
			t.Errorf("session %q: ended_at = %d, want original %d (migration must not overwrite)", r.sessionName, gotMs, existingEndedAt)
		}
	}
}

// TestMigration_V12ToV13_RecentLastSeenUntouched verifies that rows with a
// recent last_seen (within the 7-day window) are NOT touched by the migration,
// even if their session_name matches a legacy pattern.
func TestMigration_V12ToV13_RecentLastSeenUntouched(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12_recent.db")

	// last_seen in Unix milliseconds (the unit used throughout the codebase).
	// A recent value is well above the 7-day threshold
	// ((unixepoch('now') - 604800) * 1000), so the age guard must NOT fire.
	recentLastSeen := time.Now().UnixMilli()

	rows := []struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
	}{
		// Legacy malformed name, but last_seen is recent — must NOT be ended.
		{"repo@feat~review-1~review", recentLastSeen, nil},
	}
	seedV12DB(t, dbPath, rows)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	s, err := d.CurrentStatus("repo@feat~review-1~review")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}
	if s.EndedAt != nil {
		t.Errorf("session with recent last_seen was incorrectly ended by migration (ended_at = %v)", s.EndedAt)
	}
}

// TestMigration_V12ToV13_Idempotent verifies that running the migration a
// second time (by opening the DB again after it has already been migrated to
// v13) is a no-op — rows already with ended_at set keep their value, and
// rows that were not matched on the first pass are still not touched.
func TestMigration_V12ToV13_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12_idempotent.db")

	var noEnded *int64
	rows := []struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
	}{
		// Legacy malformed name — will be ended on first open.
		{"repo@feat~review-1~review", 0, noEnded},
		// Valid current shape — must never be touched.
		{"repo@feat~review-2-review-code", 0, noEnded},
	}
	seedV12DB(t, dbPath, rows)

	// First open: applies the migration.
	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}

	s1Legacy, err := d1.CurrentStatus("repo@feat~review-1~review")
	if err != nil {
		t.Fatalf("first pass CurrentStatus (legacy): %v", err)
	}
	if s1Legacy == nil || s1Legacy.EndedAt == nil {
		t.Fatal("first pass: legacy row should have ended_at set after migration")
	}
	firstEndedAt := s1Legacy.EndedAt.UnixMilli()
	d1.Close()

	// Second open: migration is at v13 already; the UPDATE WHERE ended_at IS NULL
	// guard means rows already ended are untouched.
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	s2Legacy, err := d2.CurrentStatus("repo@feat~review-1~review")
	if err != nil {
		t.Fatalf("second pass CurrentStatus (legacy): %v", err)
	}
	if s2Legacy == nil || s2Legacy.EndedAt == nil {
		t.Fatal("second pass: legacy row ended_at should still be set")
	}
	secondEndedAt := s2Legacy.EndedAt.UnixMilli()
	if secondEndedAt != firstEndedAt {
		t.Errorf("second pass: ended_at changed (first=%d, second=%d); migration is not idempotent", firstEndedAt, secondEndedAt)
	}

	// Valid shape must still have no ended_at after both passes.
	s2Valid, err := d2.CurrentStatus("repo@feat~review-2-review-code")
	if err != nil {
		t.Fatalf("second pass CurrentStatus (valid shape): %v", err)
	}
	if s2Valid == nil {
		t.Fatal("second pass: valid-shape row not found")
	}
	if s2Valid.EndedAt != nil {
		t.Errorf("second pass: valid-shape row incorrectly ended (ended_at = %v)", s2Valid.EndedAt)
	}
}

// ── WriteEvent last_seen tests (issue #824) ───────────────────────────────────

// TestWriteEvent_BumpsLastSeen verifies that WriteEvent updates
// agent_status.last_seen for the owning session to the event's created_at
// value (AC from #824).
func TestWriteEvent_BumpsLastSeen(t *testing.T) {
	d := openTestDB(t)

	// Create a status row — last_seen is set to time.Now() by UpsertStatus.
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Capture the initial last_seen.
	s0, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (initial): %v", err)
	}
	initialLastSeen := s0.LastSeen

	// Write an event with a created_at that is strictly in the future relative
	// to the initial last_seen. Use a fixed offset so the assertion is stable.
	eventTime := initialLastSeen.Add(5 * time.Second)
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   eventTime,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	s1, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (after WriteEvent): %v", err)
	}

	// last_seen must have been bumped to the event's created_at (within
	// millisecond rounding — both are stored as UnixMilli).
	wantMs := eventTime.UnixMilli()
	gotMs := s1.LastSeen.UnixMilli()
	if gotMs != wantMs {
		t.Errorf("LastSeen after WriteEvent: got %d ms, want %d ms (event created_at)",
			gotMs, wantMs)
	}
}

// TestWriteEvent_LastSeen_MaxGuard verifies that writing an event with a
// created_at OLDER than the current last_seen does NOT move last_seen backward
// (MAX semantics from issue #824).
func TestWriteEvent_LastSeen_MaxGuard(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Read the current last_seen so we can build a definitely-older timestamp.
	s0, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (initial): %v", err)
	}
	currentLastSeen := s0.LastSeen

	// Write an event with created_at 10 seconds in the past.
	oldTime := currentLastSeen.Add(-10 * time.Second)
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   oldTime,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent (old event): %v", err)
	}

	s1, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus (after WriteEvent old): %v", err)
	}

	// last_seen must not have gone backward.
	if s1.LastSeen.UnixMilli() < currentLastSeen.UnixMilli() {
		t.Errorf("LastSeen moved backward: was %d ms, now %d ms (old event must not decrease last_seen)",
			currentLastSeen.UnixMilli(), s1.LastSeen.UnixMilli())
	}
}

// TestWriteEvent_UnknownSession_NoError verifies the edge-case AC from #824:
// writing an event for a session_name that has no agent_status row does not
// produce an error — the event is still recorded.
func TestWriteEvent_UnknownSession_NoError(t *testing.T) {
	d := openTestDB(t)

	// No UpsertStatus call — the session does not exist in agent_status.
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@nonexistent",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent for unknown session: %v (want nil)", err)
	}

	// The event must be retrievable.
	events, err := d.QueryEvents("repo@nonexistent", 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count: got %d, want 1", len(events))
	}
	if events[0].ID != e.ID {
		t.Errorf("event ID: got %q, want %q", events[0].ID, e.ID)
	}
}

// TestUpsertStatus_SetsLastSeenOnInsert verifies that a newly created
// agent_status row has a non-zero last_seen set to approximately now (#824 AC).
func TestUpsertStatus_SetsLastSeenOnInsert(t *testing.T) {
	d := openTestDB(t)

	before := time.Now().Add(-time.Second) // give 1s slack for slow systems
	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	after := time.Now().Add(time.Second)

	s, err := d.CurrentStatus("repo@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s == nil {
		t.Fatal("CurrentStatus: got nil")
	}

	if s.LastSeen.IsZero() {
		t.Error("LastSeen: got zero, want non-zero timestamp")
	}
	if s.LastSeen.Before(before) {
		t.Errorf("LastSeen (%v) is before expected window start (%v)", s.LastSeen, before)
	}
	if s.LastSeen.After(after) {
		t.Errorf("LastSeen (%v) is after expected window end (%v)", s.LastSeen, after)
	}
}

// TestMigration_V13ToV14_BackfillsLastSeen verifies the one-shot backfill
// migration (v13→v14): agent_status rows with last_seen=0 are populated from
// MAX(agent_events.created_at) for the owning session, while rows that already
// have a real last_seen are left untouched (#824 AC).
func TestMigration_V13ToV14_BackfillsLastSeen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v13_backfill.db")

	// Seed a v13 database with:
	//   - "repo@stale"  — last_seen=0, has agent_events → should be backfilled
	//   - "repo@noevts" — last_seen=0, no agent_events  → should stay 0
	//   - "repo@live"   — last_seen=already_set         → must not be overwritten
	const alreadySet = int64(9_000_000_000_000) // ms, some past date
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
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;
		INSERT INTO schema_version (version) VALUES (13);

		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('repo@stale',  'repo', '/wt', 'active', 0);
		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('repo@noevts', 'repo', '/wt', 'active', 0);
		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('repo@live',   'repo', '/wt', 'active', 9000000000000);

		-- Two events for repo@stale, different timestamps.
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		  VALUES ('evt-stale-1', 'repo@stale', 'repo', '/wt', 'state_change', '{}', 1000);
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		  VALUES ('evt-stale-2', 'repo@stale', 'repo', '/wt', 'state_change', '{}', 5000);
		-- No events for repo@noevts.
		-- No events for repo@live (its last_seen is already set).
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v13 db: %v", err)
	}

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v13 db: %v", err)
	}
	defer d.Close()

	// Schema version must advance to 16.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// repo@stale: last_seen must be MAX(created_at) = 5000.
	stale, err := d.CurrentStatus("repo@stale")
	if err != nil {
		t.Fatalf("CurrentStatus(repo@stale): %v", err)
	}
	if stale == nil {
		t.Fatal("CurrentStatus(repo@stale): got nil")
	}
	if stale.LastSeen.UnixMilli() != 5000 {
		t.Errorf("repo@stale last_seen: got %d ms, want 5000 (MAX of events)", stale.LastSeen.UnixMilli())
	}

	// repo@noevts: last_seen stays 0 (COALESCE returns 0 for NULL subquery result).
	noevts, err := d.CurrentStatus("repo@noevts")
	if err != nil {
		t.Fatalf("CurrentStatus(repo@noevts): %v", err)
	}
	if noevts == nil {
		t.Fatal("CurrentStatus(repo@noevts): got nil")
	}
	if noevts.LastSeen.UnixMilli() != 0 {
		t.Errorf("repo@noevts last_seen: got %d ms, want 0 (no events)", noevts.LastSeen.UnixMilli())
	}

	// repo@live: last_seen must not be overwritten (already had a non-zero value).
	live, err := d.CurrentStatus("repo@live")
	if err != nil {
		t.Fatalf("CurrentStatus(repo@live): %v", err)
	}
	if live == nil {
		t.Fatal("CurrentStatus(repo@live): got nil")
	}
	if live.LastSeen.UnixMilli() != alreadySet {
		t.Errorf("repo@live last_seen: got %d ms, want %d ms (must not overwrite existing value)", live.LastSeen.UnixMilli(), alreadySet)
	}
}

// TestMigration_V13ToV14_Idempotent verifies that running the v13→v14 backfill
// a second time (by opening an already-migrated DB) does not overwrite
// last_seen values that were set by the first migration pass (#824 AC:
// idempotent backfill).
func TestMigration_V13ToV14_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v13_backfill_idempotent.db")

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
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;
		INSERT INTO schema_version (version) VALUES (13);
		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('repo@stale', 'repo', '/wt', 'active', 0);
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		  VALUES ('evt-1', 'repo@stale', 'repo', '/wt', 'state_change', '{}', 7777);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v13 db: %v", err)
	}

	// First open: applies the backfill.
	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}
	s1, err := d1.CurrentStatus("repo@stale")
	if err != nil {
		t.Fatalf("first CurrentStatus: %v", err)
	}
	if s1 == nil || s1.LastSeen.UnixMilli() != 7777 {
		t.Fatalf("first pass: expected last_seen=7777, got %v", s1)
	}
	d1.Close()

	// Second open: migration is at v16; the backfill WHERE guard means rows with
	// last_seen != 0 are not touched.
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	s2, err := d2.CurrentStatus("repo@stale")
	if err != nil {
		t.Fatalf("second CurrentStatus: %v", err)
	}
	if s2 == nil || s2.LastSeen.UnixMilli() != 7777 {
		t.Errorf("second pass: last_seen changed (want 7777, got %v); backfill is not idempotent", s2.LastSeen.UnixMilli())
	}
}

// TestLastSeen_ActivityQuery verifies the functional AC from #824: a query
//
//	SELECT session_name FROM agent_status WHERE last_seen >= unixepoch('now', '-1 day') * 1000
//
// returns only sessions that have had recent event activity.
func TestLastSeen_ActivityQuery(t *testing.T) {
	d := openTestDB(t)

	// Create two sessions.
	if err := d.UpsertStatus("repo@recent", "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus recent: %v", err)
	}
	if err := d.UpsertStatus("repo@old", "repo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus old: %v", err)
	}

	// Force repo@old's last_seen to a timestamp more than 24h ago via SQL directly,
	// so that the query below can discriminate. Use a timestamp 2 days in the past.
	twoDaysAgo := time.Now().Add(-48 * time.Hour).UnixMilli()
	if err := d.QueryRow(
		"UPDATE agent_status SET last_seen = ? WHERE session_name = 'repo@old' RETURNING 1",
		twoDaysAgo,
	).Scan(new(int)); err != nil {
		t.Fatalf("force-set old last_seen: %v", err)
	}

	// Write a recent event for repo@recent to bump its last_seen.
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@recent",
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent recent: %v", err)
	}

	// Check that only 'repo@recent' qualifies for the last-24h query from #824.
	// last_seen is stored in milliseconds; unixepoch() returns seconds.
	var recentCount, oldCount int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE last_seen >= (unixepoch('now') - 86400) * 1000 AND session_name = 'repo@recent'",
	).Scan(&recentCount); err != nil {
		t.Fatalf("count recent: %v", err)
	}
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE last_seen >= (unixepoch('now') - 86400) * 1000 AND session_name = 'repo@old'",
	).Scan(&oldCount); err != nil {
		t.Fatalf("count old: %v", err)
	}

	if recentCount != 1 {
		t.Errorf("repo@recent should appear in last-24h query: got count %d, want 1", recentCount)
	}
	if oldCount != 0 {
		t.Errorf("repo@old should NOT appear in last-24h query: got count %d, want 0", oldCount)
	}
}

// TestMigration_V14ToV15_RenamesColumn verifies the v14→v15 migration:
// agent_events.opencode_sid is renamed to harness_session_id, existing
// non-NULL values are preserved under the new name, and the migration is
// idempotent (running it twice does not error or corrupt data).
func TestMigration_V14ToV15_RenamesColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v14_rename.db")

	// Seed a v14 database: agent_events still has opencode_sid, not harness_session_id.
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	sid := "ses_abc123"
	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, opencode_sid TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;
		INSERT INTO schema_version (version) VALUES (14);

		-- One event with a non-NULL opencode_sid, one with NULL.
		INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
		  VALUES ('evt-1', 'repo@main', 'repo', '/wt', 'ses_abc123', 'state_change', '{}', 1000);
		INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
		  VALUES ('evt-2', 'repo@main', 'repo', '/wt', NULL, 'state_change', '{}', 2000);
	`)
	rawConn.Close()
	if err != nil {
		t.Fatalf("seed v14 db: %v", err)
	}

	// First open: applies v14→v15 migration.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v14 db: %v", err)
	}
	defer d.Close()

	// Schema version must advance to 16.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// harness_session_id column must now exist in agent_events.
	var hsiExists int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'harness_session_id'`,
	).Scan(&hsiExists); err != nil {
		t.Fatalf("pragma_table_info harness_session_id: %v", err)
	}
	if hsiExists == 0 {
		t.Error("harness_session_id column does not exist in agent_events after migration")
	}

	// opencode_sid column must no longer exist.
	var oldColExists int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'opencode_sid'`,
	).Scan(&oldColExists); err != nil {
		t.Fatalf("pragma_table_info opencode_sid: %v", err)
	}
	if oldColExists != 0 {
		t.Error("opencode_sid column still exists in agent_events after migration")
	}

	// Non-NULL value is preserved: evt-1 must have harness_session_id = sid.
	var got *string
	if err := d.QueryRow(
		`SELECT harness_session_id FROM agent_events WHERE id = 'evt-1'`,
	).Scan(&got); err != nil {
		t.Fatalf("read evt-1 harness_session_id: %v", err)
	}
	if got == nil || *got != sid {
		t.Errorf("evt-1 harness_session_id: got %v, want %q", got, sid)
	}

	// NULL value is preserved: evt-2 must have harness_session_id = NULL.
	var gotNull *string
	if err := d.QueryRow(
		`SELECT harness_session_id FROM agent_events WHERE id = 'evt-2'`,
	).Scan(&gotNull); err != nil {
		t.Fatalf("read evt-2 harness_session_id: %v", err)
	}
	if gotNull != nil {
		t.Errorf("evt-2 harness_session_id: got %v, want nil", gotNull)
	}

	// Idempotency: opening the same (now v16) DB a second time must not error.
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open on already-migrated db: %v", err)
	}
	defer d2.Close()

	// harness_session_id value must still be intact after second open.
	var got2 *string
	if err := d2.QueryRow(
		`SELECT harness_session_id FROM agent_events WHERE id = 'evt-1'`,
	).Scan(&got2); err != nil {
		t.Fatalf("read evt-1 after second open: %v", err)
	}
	if got2 == nil || *got2 != sid {
		t.Errorf("evt-1 after second open: got %v, want %q", got2, sid)
	}
}

// TestMigration_V12ToV13_PostMigrationQueryReturnsZero verifies the AC
// assertion from issue #826: after the migration, a query for rows that are
// active AND match the legacy malformed pattern returns zero results.
//
//	SELECT COUNT(*) FROM agent_status
//	WHERE ended_at IS NULL AND session_name GLOB '*~review-*~review*'
func TestMigration_V12ToV13_PostMigrationQueryReturnsZero(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12_ac_query.db")

	var noEnded *int64
	rows := []struct {
		sessionName string
		lastSeen    int64
		endedAt     *int64
	}{
		{"nixos-config@fix-tmux~review-1-review~review-1-review", 0, noEnded},
		{"nixos-config@fix-tmux~review-1~review", 0, noEnded},
		{"nixos-config@fix-tmux~review-3~review", 0, noEnded},
		// Bare review suffix (no role) — first example in issue #826.
		{"nixos-config@fix-tmux-podman-keybinds~review-1-review", 0, noEnded},
		// A current valid shape — should NOT be counted by this query.
		{"nixos-config@fix-tmux~review-2-review-code", 0, noEnded},
	}
	seedV12DB(t, dbPath, rows)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var count int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM agent_status WHERE ended_at IS NULL AND session_name GLOB '*~review-*~review*'`,
	).Scan(&count); err != nil {
		t.Fatalf("AC query: %v", err)
	}
	if count != 0 {
		t.Errorf("AC query: got %d active malformed rows, want 0 after migration", count)
	}
}

// ── Migration v15→v16 ─────────────────────────────────────────────────────────

// seedV15DB creates a raw SQLite database at dbPath seeded at schema_version=15
// (the full v15 schema: agent_status, agent_events with harness_session_id,
// session_groups, bus_messages). It inserts one live and one ended agent_status
// row, along with several agent_events rows, for use by v15→v16 migration tests.
func seedV15DB(t *testing.T, dbPath string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v15 db: %v", err)
	}
	defer rawConn.Close()

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);

		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);

		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;

		INSERT INTO schema_version (version) VALUES (15);

		-- Live session with instance_id (should be backfilled into sessions).
		INSERT INTO agent_status (session_name, repo, worktree, state, instance_id, harness, last_seen)
		  VALUES ('repo@main', 'repo', '/code/repo/main', 'active', 'iid-live-1', 'opencode', 1000);

		-- Ended session with instance_id (not backfilled: ended_at IS NOT NULL).
		INSERT INTO agent_status (session_name, repo, worktree, state, instance_id, harness, last_seen, ended_at)
		  VALUES ('repo@feat', 'repo', '/code/repo/feat', 'finished', 'iid-ended-2', 'opencode', 2000, 3000);

		-- Live session WITHOUT instance_id (should be skipped by backfill).
		INSERT INTO agent_status (session_name, repo, worktree, state, instance_id, harness, last_seen)
		  VALUES ('repo@noid', 'repo', '/code/repo/noid', 'active', NULL, 'opencode', 4000);

		-- Events for the live session.
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		  VALUES ('evt-1', 'repo@main', 'repo', '/code/repo/main', 'state_change', '{"state":"active"}', 1000);
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		  VALUES ('evt-2', 'repo@feat', 'repo', '/code/repo/feat', 'state_change', '{"state":"finished"}', 2000);

		-- Bus message and session_group rows.
		INSERT INTO session_groups (group_id, parent_session)
		  VALUES ('grp-1', 'repo@main');
		INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at)
		  VALUES ('msg-1', 'repo@main', 'repo@feat', 'repo', 'hello', 'normal', 1000);
	`)
	if err != nil {
		t.Fatalf("seed v15 db: %v", err)
	}
}

// TestMigration_V15ToV16_CreatesSessionsTable verifies that the v15→v16
// migration creates the sessions table with all required columns and indexes,
// adds instance_id to agent_events, and backfills sessions from live
// agent_status rows.
func TestMigration_V15ToV16_CreatesSessionsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v15_sessions.db")
	seedV15DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v15 db: %v", err)
	}
	defer d.Close()

	// Schema version must advance to 17 (v15→v16 + v16→v17 both applied).
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// sessions table must exist.
	var tname string
	if err := d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'",
	).Scan(&tname); err != nil {
		t.Fatalf("sessions table not found after v15→v16 migration: %v", err)
	}

	// idx_sessions_repo_started must exist.
	var idxName string
	if err := d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sessions_repo_started'",
	).Scan(&idxName); err != nil {
		t.Fatalf("idx_sessions_repo_started not found: %v", err)
	}

	// idx_sessions_name must exist.
	if err := d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sessions_name'",
	).Scan(&idxName); err != nil {
		t.Fatalf("idx_sessions_name not found: %v", err)
	}

	// instance_id column must now exist in agent_events.
	var colExists int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'instance_id'`,
	).Scan(&colExists); err != nil {
		t.Fatalf("pragma_table_info agent_events.instance_id: %v", err)
	}
	if colExists == 0 {
		t.Error("instance_id column not found in agent_events after v15→v16 migration")
	}
}

// TestMigration_V15ToV16_BackfillsLiveSessions verifies that the backfill step
// creates exactly one sessions row per live agent_status row with a non-empty
// instance_id, and skips both ended rows and rows with NULL instance_id.
func TestMigration_V15ToV16_BackfillsLiveSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v15_backfill.db")
	seedV15DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v15 db: %v", err)
	}
	defer d.Close()

	// Only the live 'repo@main' row has ended_at IS NULL AND instance_id != ''.
	// 'repo@feat' is ended; 'repo@noid' has NULL instance_id.
	var sessionCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("sessions count after backfill: got %d, want 1 (only live sessions with instance_id)", sessionCount)
	}

	// The backfilled row must have instance_id = 'iid-live-1'.
	var iid string
	if err := d.QueryRow("SELECT instance_id FROM sessions WHERE session_name = 'repo@main'").Scan(&iid); err != nil {
		t.Fatalf("query sessions for repo@main: %v", err)
	}
	if iid != "iid-live-1" {
		t.Errorf("backfilled instance_id: got %q, want %q", iid, "iid-live-1")
	}
}

// TestMigration_V15ToV16_PreservesExistingRows verifies that existing rows in
// agent_events, agent_status, bus_messages, and session_groups are unmodified
// after the v15→v16 migration (counts and content preserved).
func TestMigration_V15ToV16_PreservesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v15_preserve.db")
	seedV15DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v15 db: %v", err)
	}
	defer d.Close()

	// agent_events: 2 rows, instance_id now NULL (pre-migration events are not backfilled).
	var evtCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM agent_events").Scan(&evtCount); err != nil {
		t.Fatalf("count agent_events: %v", err)
	}
	if evtCount != 2 {
		t.Errorf("agent_events count: got %d, want 2", evtCount)
	}
	// Pre-migration agent_events rows must have instance_id = NULL.
	var nullCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM agent_events WHERE instance_id IS NULL").Scan(&nullCount); err != nil {
		t.Fatalf("count agent_events with NULL instance_id: %v", err)
	}
	if nullCount != 2 {
		t.Errorf("pre-migration events with NULL instance_id: got %d, want 2", nullCount)
	}

	// agent_status: 3 rows (live+ended+noid).
	var statusCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM agent_status").Scan(&statusCount); err != nil {
		t.Fatalf("count agent_status: %v", err)
	}
	if statusCount != 3 {
		t.Errorf("agent_status count: got %d, want 3", statusCount)
	}

	// bus_messages: 1 row.
	var msgCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages").Scan(&msgCount); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if msgCount != 1 {
		t.Errorf("bus_messages count: got %d, want 1", msgCount)
	}

	// session_groups: 1 row.
	var grpCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM session_groups").Scan(&grpCount); err != nil {
		t.Fatalf("count session_groups: %v", err)
	}
	if grpCount != 1 {
		t.Errorf("session_groups count: got %d, want 1", grpCount)
	}
}

// TestMigration_V15ToV16_Idempotent verifies that running the v15→v16
// migration twice (opening an already-migrated DB) is a no-op.
func TestMigration_V15ToV16_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v15_idempotent.db")
	seedV15DB(t, dbPath)

	// First open: applies the migration.
	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}
	d1.Close()

	// Second open: must succeed without error (idempotent).
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open on already-migrated db: %v", err)
	}
	defer d2.Close()

	var version int
	if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after second open: got %d, want 17", version)
	}
}

// ── sessions table AC tests ───────────────────────────────────────────────────

// TestOpen_CreatesSessionsTable verifies that the sessions table exists and has
// all required columns on a fresh DB (no migration needed).
func TestOpen_CreatesSessionsTable(t *testing.T) {
	d := openTestDB(t)

	// sessions table must exist.
	var name string
	if err := d.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'",
	).Scan(&name); err != nil {
		t.Fatalf("sessions table not found: %v", err)
	}
	if name != "sessions" {
		t.Errorf("sessions table name: got %q, want \"sessions\"", name)
	}

	// Verify columns exist by probing the declarative schema.
	requiredCols := []string{
		"instance_id", "session_name", "agent_role", "root_agent_name",
		"repo", "worktree", "harness", "harness_session_id", "group_id",
		"started_at", "ended_at", "end_state", "archive_path", "prism_version",
	}
	for _, col := range requiredCols {
		var n int
		if err := d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info('sessions') for %q: %v", col, err)
		}
		if n == 0 {
			t.Errorf("column %q not found in sessions table", col)
		}
	}

	// agent_events.instance_id must exist on a fresh DB too.
	var aeCol int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'instance_id'`,
	).Scan(&aeCol); err != nil {
		t.Fatalf("pragma_table_info agent_events.instance_id: %v", err)
	}
	if aeCol == 0 {
		t.Error("instance_id column not found in agent_events on fresh DB")
	}
}

// TestInsertSession_Basic verifies that InsertSession inserts a row with the
// expected values and that the row is retrievable.
func TestInsertSession_Basic(t *testing.T) {
	d := openTestDB(t)

	iid := uuid.New().String()
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Harness:     "opencode",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	var gotIID, gotSession, gotRepo, gotWorktree, gotHarness string
	var startedAt int64
	if err := d.QueryRow(
		`SELECT instance_id, session_name, repo, worktree, harness, started_at
		   FROM sessions WHERE instance_id = ?`, iid,
	).Scan(&gotIID, &gotSession, &gotRepo, &gotWorktree, &gotHarness, &startedAt); err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	if gotIID != iid {
		t.Errorf("instance_id: got %q, want %q", gotIID, iid)
	}
	if gotSession != "repo@main" {
		t.Errorf("session_name: got %q, want %q", gotSession, "repo@main")
	}
	if gotRepo != "repo" {
		t.Errorf("repo: got %q, want %q", gotRepo, "repo")
	}
	if gotWorktree != "/code/repo/main" {
		t.Errorf("worktree: got %q, want %q", gotWorktree, "/code/repo/main")
	}
	if gotHarness != "opencode" {
		t.Errorf("harness: got %q, want \"opencode\"", gotHarness)
	}
	if startedAt == 0 {
		t.Error("started_at: got 0, want non-zero")
	}
}

// TestInsertSession_Idempotent verifies that inserting the same instance_id
// twice is a no-op (INSERT OR IGNORE).
func TestInsertSession_Idempotent(t *testing.T) {
	d := openTestDB(t)

	iid := uuid.New().String()
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("first InsertSession: %v", err)
	}
	// Second insert with same instance_id must not fail.
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("second InsertSession (idempotent): %v", err)
	}

	var rowCount int
	if err := d.QueryRow("SELECT COUNT(*) FROM sessions WHERE instance_id = ?", iid).Scan(&rowCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("sessions count after duplicate insert: got %d, want 1", rowCount)
	}
}

// TestUpdateSessionEnded verifies that UpdateSessionEnded sets ended_at and
// end_state on the sessions row.
func TestUpdateSessionEnded(t *testing.T) {
	d := openTestDB(t)

	iid := uuid.New().String()
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Verify ended_at is NULL before update.
	var endedAtBefore *int64
	if err := d.QueryRow("SELECT ended_at FROM sessions WHERE instance_id = ?", iid).Scan(&endedAtBefore); err != nil {
		t.Fatalf("query ended_at before: %v", err)
	}
	if endedAtBefore != nil {
		t.Errorf("ended_at before update: got %v, want nil", endedAtBefore)
	}

	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}

	var endedAt *int64
	var endState *string
	if err := d.QueryRow(
		"SELECT ended_at, end_state FROM sessions WHERE instance_id = ?", iid,
	).Scan(&endedAt, &endState); err != nil {
		t.Fatalf("query sessions after UpdateSessionEnded: %v", err)
	}
	if endedAt == nil {
		t.Error("ended_at: got nil, want non-nil after UpdateSessionEnded")
	}
	if endState == nil || *endState != "finished" {
		t.Errorf("end_state: got %v, want \"finished\"", endState)
	}
}

// TestUpdateSessionEnded_NoopWhenNoRow verifies that UpdateSessionEnded on a
// non-existent instance_id does not error (it is a no-op).
func TestUpdateSessionEnded_NoopWhenNoRow(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpdateSessionEnded("does-not-exist", "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded on non-existent row: %v (want nil)", err)
	}
}

// TestWriteEvent_PropagatesInstanceID verifies that WriteEvent stores
// instance_id on the event row and that QueryEvents returns it.
func TestWriteEvent_PropagatesInstanceID(t *testing.T) {
	d := openTestDB(t)

	iid := uuid.New().String()
	// Insert a sessions row so the FK is satisfied.
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	evtID := uuid.New().String()
	e := db.Event{
		ID:          evtID,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		InstanceID:  &iid,
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
	if events[0].InstanceID == nil {
		t.Fatal("InstanceID: got nil, want non-nil")
	}
	if *events[0].InstanceID != iid {
		t.Errorf("InstanceID: got %q, want %q", *events[0].InstanceID, iid)
	}
}

// TestWriteEvent_NullInstanceID verifies that WriteEvent with InstanceID=nil
// succeeds and stores NULL in agent_events.instance_id (legacy-compatible path).
func TestWriteEvent_NullInstanceID(t *testing.T) {
	d := openTestDB(t)

	evtID := uuid.New().String()
	e := db.Event{
		ID:          evtID,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		InstanceID:  nil, // no instance_id
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent with nil InstanceID: %v", err)
	}

	events, err := d.QueryEvents("repo@main", 10, nil, nil, nil)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count: got %d, want 1", len(events))
	}
	if events[0].InstanceID != nil {
		t.Errorf("InstanceID: got %v, want nil (legacy-compatible NULL)", events[0].InstanceID)
	}
}

// TestWriteEvent_ForeignKeyViolation verifies that writing an agent_events row
// with a non-NULL instance_id that does not exist in sessions fails with a
// foreign-key error (AC from issue #996).
func TestWriteEvent_ForeignKeyViolation(t *testing.T) {
	d := openTestDB(t)

	nonExistentIID := uuid.New().String()
	evtID := uuid.New().String()
	e := db.Event{
		ID:          evtID,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		InstanceID:  &nonExistentIID,
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	}
	err := d.WriteEvent(e)
	if err == nil {
		t.Fatal("WriteEvent with non-existent instance_id: expected FK error, got nil")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Errorf("error should mention FOREIGN KEY constraint: got %q", err.Error())
	}
}

// TestSessionsFK_OnDeleteSetNull verifies that deleting a session_groups row
// sets sessions.group_id to NULL (ON DELETE SET NULL cascade), preserving the
// sessions row itself.
func TestSessionsFK_OnDeleteSetNull(t *testing.T) {
	d := openTestDB(t)

	// Register a group.
	groupID, err := d.RegisterGroup("repo@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Insert a sessions row referencing the group.
	iid := uuid.New().String()
	g := groupID
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
		GroupID:     &g,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Confirm group_id is set.
	var gid *string
	if err := d.QueryRow("SELECT group_id FROM sessions WHERE instance_id = ?", iid).Scan(&gid); err != nil {
		t.Fatalf("query group_id before delete: %v", err)
	}
	if gid == nil || *gid != groupID {
		t.Fatalf("pre-condition: group_id = %v, want %q", gid, groupID)
	}

	// Delete the session_groups row — should cascade SET NULL to sessions.group_id.
	if err := d.QueryRow(
		"DELETE FROM session_groups WHERE group_id = ? RETURNING group_id", groupID,
	).Scan(new(string)); err != nil {
		t.Fatalf("delete session_groups: %v", err)
	}

	// sessions.group_id must now be NULL.
	var gidAfter *string
	if err := d.QueryRow("SELECT group_id FROM sessions WHERE instance_id = ?", iid).Scan(&gidAfter); err != nil {
		t.Fatalf("query group_id after delete: %v", err)
	}
	if gidAfter != nil {
		t.Errorf("group_id after ON DELETE SET NULL: got %v, want nil", *gidAfter)
	}

	// sessions row must still exist.
	var sessName string
	if err := d.QueryRow("SELECT session_name FROM sessions WHERE instance_id = ?", iid).Scan(&sessName); err != nil {
		t.Fatalf("sessions row should still exist after group deletion: %v", err)
	}
	if sessName != "repo@main" {
		t.Errorf("session_name after group deletion: got %q, want %q", sessName, "repo@main")
	}
}

// ── InsertSession zero-value guard (issue #1010) ──────────────────────────────

// TestInsertSession_ZeroStartedAt verifies that InsertSession with a zero
// time.Time{} StartedAt writes a current unix-ms timestamp (not -62135596800000
// which is what time.Time{}.UnixMilli() returns).
func TestInsertSession_ZeroStartedAt(t *testing.T) {
	d := openTestDB(t)

	before := time.Now().UnixMilli()

	iid := uuid.New().String()
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
		// StartedAt intentionally left as zero time.Time{}
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	after := time.Now().UnixMilli()

	var startedAt int64
	if err := d.QueryRow("SELECT started_at FROM sessions WHERE instance_id = ?", iid).Scan(&startedAt); err != nil {
		t.Fatalf("query started_at: %v", err)
	}

	// Must not be the zero-time sentinel.
	const zeroTimeMs = -62135596800000
	if startedAt == zeroTimeMs {
		t.Errorf("started_at: got zero-time sentinel %d, want current time", zeroTimeMs)
	}
	// Must be positive (a real unix-ms timestamp).
	if startedAt <= 0 {
		t.Errorf("started_at: got %d, want positive unix-ms timestamp", startedAt)
	}
	// Must be within the window around the test execution.
	if startedAt < before || startedAt > after {
		t.Errorf("started_at: got %d, want in [%d, %d]", startedAt, before, after)
	}
}

// TestInsertSession_ExplicitStartedAt verifies that InsertSession with a
// non-zero StartedAt preserves that exact value.
func TestInsertSession_ExplicitStartedAt(t *testing.T) {
	d := openTestDB(t)

	want := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	iid := uuid.New().String()
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "repo@main",
		Repo:        "repo",
		Worktree:    "/wt",
		Harness:     "opencode",
		StartedAt:   want,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	var startedAt int64
	if err := d.QueryRow("SELECT started_at FROM sessions WHERE instance_id = ?", iid).Scan(&startedAt); err != nil {
		t.Fatalf("query started_at: %v", err)
	}

	if startedAt != want.UnixMilli() {
		t.Errorf("started_at: got %d, want %d (%s)", startedAt, want.UnixMilli(), want)
	}
}

// ── Migration v16→v17 (issue #1010) ──────────────────────────────────────────

// seedV16DB creates a raw SQLite database at dbPath seeded at schema_version=16.
// It inserts two sessions rows with broken started_at (-62135596800000):
//   - iid-has-events: has agent_events rows → migration should fix its started_at
//   - iid-no-events:  has no agent_events  → migration should leave it unchanged
//
// It also inserts one session with a valid started_at to confirm it is untouched.
func seedV16DB(t *testing.T, dbPath string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v16 db: %v", err)
	}
	defer rawConn.Close()

	const zeroTimeMs = -62135596800000
	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL,
		  instance_id TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo TEXT NOT NULL,
		  worktree TEXT NOT NULL,
		  state TEXT NOT NULL,
		  title TEXT,
		  agent_name TEXT,
		  model_id TEXT,
		  root_agent_name TEXT,
		  root_model_id TEXT,
		  host_mode INTEGER NOT NULL DEFAULT 0,
		  isolation_mode TEXT,
		  instance_id TEXT,
		  last_seen INTEGER NOT NULL,
		  ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'opencode',
		  harness_session_id TEXT,
		  harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
		);

		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);

		CREATE TABLE IF NOT EXISTS sessions (
		  instance_id        TEXT PRIMARY KEY,
		  session_name       TEXT NOT NULL,
		  agent_role         TEXT,
		  root_agent_name    TEXT,
		  repo               TEXT NOT NULL,
		  worktree           TEXT NOT NULL,
		  harness            TEXT NOT NULL,
		  harness_session_id TEXT,
		  group_id           TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  started_at         INTEGER NOT NULL,
		  ended_at           INTEGER,
		  end_state          TEXT,
		  archive_path       TEXT,
		  prism_version      TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_repo_started ON sessions(repo, started_at DESC);
		CREATE INDEX IF NOT EXISTS idx_sessions_name         ON sessions(session_name, started_at DESC);

		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;

		INSERT INTO schema_version (version) VALUES (16);

		-- Broken row: started_at is the zero-time sentinel; has events.
		INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at)
		  VALUES ('iid-has-events', 'repo@main', 'repo', '/code/repo/main', 'opencode', -62135596800000);

		-- Broken row: started_at is the zero-time sentinel; no matching events.
		INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at)
		  VALUES ('iid-no-events', 'repo@other', 'repo', '/code/repo/other', 'opencode', -62135596800000);

		-- Valid row: started_at is already correct; must not be touched.
		INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at)
		  VALUES ('iid-good', 'repo@good', 'repo', '/code/repo/good', 'opencode', 1700000000000);

		-- Events for iid-has-events; min created_at = 1600000000000.
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at, instance_id)
		  VALUES ('evt-1', 'repo@main', 'repo', '/code/repo/main', 'state_change', '{}', 1600000000000, 'iid-has-events');
		INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at, instance_id)
		  VALUES ('evt-2', 'repo@main', 'repo', '/code/repo/main', 'state_change', '{}', 1600000099999, 'iid-has-events');
	`)
	if err != nil {
		t.Fatalf("seed v16 db: %v", err)
	}
}

// TestMigration_V16ToV17_BackfillsStartedAt verifies that the v16→v17
// migration updates sessions rows with negative started_at to the minimum
// agent_events.created_at for the matching instance_id.
func TestMigration_V16ToV17_BackfillsStartedAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v16_backfill.db")
	seedV16DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on v16 db: %v", err)
	}
	defer d.Close()

	// Schema version must advance to 17.
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after migration: got %d, want 17", version)
	}

	// iid-has-events: started_at must be updated to 1600000000000 (min event ts).
	var startedAtHasEvents int64
	if err := d.QueryRow("SELECT started_at FROM sessions WHERE instance_id = 'iid-has-events'").Scan(&startedAtHasEvents); err != nil {
		t.Fatalf("query iid-has-events: %v", err)
	}
	if startedAtHasEvents != 1600000000000 {
		t.Errorf("iid-has-events started_at: got %d, want 1600000000000", startedAtHasEvents)
	}

	// iid-no-events: started_at must remain unchanged (no events → left alone).
	var startedAtNoEvents int64
	if err := d.QueryRow("SELECT started_at FROM sessions WHERE instance_id = 'iid-no-events'").Scan(&startedAtNoEvents); err != nil {
		t.Fatalf("query iid-no-events: %v", err)
	}
	const zeroTimeMs = int64(-62135596800000)
	if startedAtNoEvents != zeroTimeMs {
		t.Errorf("iid-no-events started_at: got %d, want %d (unchanged)", startedAtNoEvents, zeroTimeMs)
	}

	// iid-good: started_at must not be touched.
	var startedAtGood int64
	if err := d.QueryRow("SELECT started_at FROM sessions WHERE instance_id = 'iid-good'").Scan(&startedAtGood); err != nil {
		t.Fatalf("query iid-good: %v", err)
	}
	if startedAtGood != 1700000000000 {
		t.Errorf("iid-good started_at: got %d, want 1700000000000 (untouched)", startedAtGood)
	}
}

// TestMigration_V16ToV17_Idempotent verifies that running the v16→v17
// migration twice (by opening the same DB after it is already at v17) is a
// no-op: no panic, no error, and started_at values are unchanged on re-open.
func TestMigration_V16ToV17_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v16_idem.db")
	seedV16DB(t, dbPath)

	// First open: applies the migration.
	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}
	d1.Close()

	// Second open: must be a no-op.
	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	var version int
	if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version on second open: %v", err)
	}
	if version != 17 {
		t.Errorf("schema_version after second open: got %d, want 17", version)
	}

	// iid-has-events should still have the corrected timestamp.
	var startedAt int64
	if err := d2.QueryRow("SELECT started_at FROM sessions WHERE instance_id = 'iid-has-events'").Scan(&startedAt); err != nil {
		t.Fatalf("query started_at on second open: %v", err)
	}
	if startedAt != 1600000000000 {
		t.Errorf("started_at after idempotent run: got %d, want 1600000000000", startedAt)
	}
}
