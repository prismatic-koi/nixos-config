package db

// Tests for the active-profile write path and its mtime-cached resolver
// Internal-package tests so they can reach
// newProfileResolverForTest and the unexported ProfileResolver.reads counter.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeStateFile writes the active-profile state file with content and sets
// its mtime so a switch is deterministically newer than the previous read.
func writeStateFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes state file: %v", err)
	}
}

// eventProfileName reads the profile_name column for one agent_events row.
// The returned bool is false when the column is SQL NULL.
func eventProfileName(t *testing.T, d *DB, id string) (string, bool) {
	t.Helper()
	var name sql.NullString
	err := d.QueryRow(`SELECT profile_name FROM agent_events WHERE id = ?`, id).Scan(&name)
	if err != nil {
		t.Fatalf("select profile_name for %s: %v", id, err)
	}
	return name.String, name.Valid
}

// AC [functional]: an event written while the state file holds "heavy" records
// profile_name='heavy'.
func TestWriteEvent_RecordsActiveProfile_StateFile(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "heavy\n", time.Now())
	d.SetProfileResolver(newProfileResolverForTest(statePath, "standard"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "heavy" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"heavy\", true)", got, ok)
	}
}

// AC [functional]: with no state file, an event records the nix default — the
// coordinator case that would otherwise fold to "default".
func TestWriteEvent_RecordsNixDefault_NoStateFile(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile") // never created
	d.SetProfileResolver(newProfileResolverForTest(statePath, "standard"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "standard" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"standard\", true)", got, ok)
	}
}

// AC [edge]: with neither a state file nor a nix default, an event records the
// explicit "unknown" placeholder, never SQL NULL or an empty string.
func TestWriteEvent_UnknownWhenNoStateAndNoDefault(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile") // never created
	d.SetProfileResolver(newProfileResolverForTest(statePath, ""))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != unknownProfile {
		t.Fatalf("profile_name = (%q, valid=%v), want (%q, true)", got, ok, unknownProfile)
	}
}

// AC [functional]: a whitespace-only state file falls through to the nix
// default, mirroring config.ActiveProfile's empty-means-default contract.
func TestWriteEvent_WhitespaceStateFile_FallsToDefault(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "   \n", time.Now())
	d.SetProfileResolver(newProfileResolverForTest(statePath, "light"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "light" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"light\", true)", got, ok)
	}
}

// The resolver reads the state-file CONTENT at most twice across N events that
// span one profile switch: once for the initial value, once for the value
// after the switch. This mirrors the account resolver's mtime-cache guarantee.
func TestProfileResolver_ReadsAtMostTwicePerSwitch(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "standard\n", time.Now().Add(-time.Hour))
	r := newProfileResolverForTest(statePath, "light")

	for i := 0; i < 10; i++ {
		if got := r.Name(); got != "standard" {
			t.Fatalf("before switch: Name() = %q, want standard", got)
		}
	}
	// Switch to "heavy" with a strictly newer mtime.
	writeStateFile(t, statePath, "heavy\n", time.Now())
	for i := 0; i < 10; i++ {
		if got := r.Name(); got != "heavy" {
			t.Fatalf("after switch: Name() = %q, want heavy", got)
		}
	}

	if r.reads > 2 {
		t.Errorf("resolver read the state file %d times across 20 resolutions and one switch, want at most 2", r.reads)
	}
}

// TestMigrateV41ToV42_PopulatedDBNoDataLoss migrates a populated v41 prism.db
// without data loss, is idempotent across two runs, and leaves pre-migration
// rows reading back as SQL NULL profile_name (the no-backfill policy).
func TestMigrateV41ToV42_PopulatedDBNoDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	seedV41PopulatedDB(t, path)

	// First Open migrates v41 → v42 and adds the nullable column.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}

	// Pre-migration rows survive and read back as SQL NULL profile_name.
	if got, ok := eventProfileName(t, d, "old-event-1"); ok {
		t.Errorf("pre-migration agent_events row profile_name = %q, want SQL NULL (no backfill)", got)
	}
	var eventCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 2 {
		t.Errorf("agent_events count after migration = %d, want 2 (no data loss)", eventCount)
	}
	d.Close()

	// Second Open is idempotent: version stays at currentSchemaVersion.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (idempotent): %v", err)
	}
	defer d2.Close()
	var version int
	if err := d2.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", version, currentSchemaVersion)
	}
	if err := d2.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events after 2nd open: %v", err)
	}
	if eventCount != 2 {
		t.Errorf("agent_events count after 2nd open = %d, want 2", eventCount)
	}
}

// seedV41PopulatedDB writes a raw SQLite database in its v41 shape
// (agent_events has account_name but no profile_name), populated with a couple
// of rows, and stamps schema_version = 41.
func seedV41PopulatedDB(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()

	stmts := []string{
		`CREATE TABLE agent_events (
			id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
			worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
			payload TEXT NOT NULL, created_at INTEGER NOT NULL, instance_id TEXT,
			account_name TEXT
		)`,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (41)`,
		`INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		   VALUES ('old-event-1', 'repo@main', 'repo', '/wt', 'msg_assistant', '{}', 1),
		          ('old-event-2', 'repo@main', 'repo', '/wt', 'msg_assistant', '{}', 2)`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed exec: %v\nstmt: %s", err, s)
		}
	}
}
