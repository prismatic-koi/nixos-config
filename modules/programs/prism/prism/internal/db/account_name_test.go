package db

// Tests for the account-name write path and its mtime-cached resolver
// (issue #2714). Internal-package tests so they can reach
// newAccountResolverForPaths and the unexported AccountResolver.reads counter.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/usage"
)

// accountFixture builds an accounts directory under a fresh temp dir and
// returns the resolver paths for it. The directory is created; the `current`
// pointer is not written until writeCurrent is called.
func accountFixture(t *testing.T) account.Paths {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "accounts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	return account.Paths{
		Dir:      dir,
		Current:  filepath.Join(dir, "current"),
		AuthJSON: filepath.Join(dir, "auth.json"), // never read by the resolver
	}
}

// writeCurrent writes the pointer file with content and sets its mtime to the
// given time so a switch is deterministically newer than the previous read.
func writeCurrent(t *testing.T, p account.Paths, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(p.Current, []byte(content), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}
	if err := os.Chtimes(p.Current, mtime, mtime); err != nil {
		t.Fatalf("chtimes current: %v", err)
	}
}

// eventAccountName reads the account_name column for one agent_events row.
// The returned bool is false when the column is SQL NULL.
func eventAccountName(t *testing.T, d *DB, id string) (string, bool) {
	t.Helper()
	var name sql.NullString
	err := d.QueryRow(`SELECT account_name FROM agent_events WHERE id = ?`, id).Scan(&name)
	if err != nil {
		t.Fatalf("select account_name for %s: %v", id, err)
	}
	return name.String, name.Valid
}

func openAccountTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func writeTestEvent(t *testing.T, d *DB, session string) string {
	t.Helper()
	id := uuid.New().String()
	err := d.WriteEvent(Event{
		ID:          id,
		SessionName: session,
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		Type:        "state_change",
		Payload:     `{"state":"active"}`,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	return id
}

// AC [functional]: an event written while `personal` is active records
// account_name='personal'.
func TestWriteEvent_RecordsActiveAccount(t *testing.T) {
	d := openAccountTestDB(t)
	p := accountFixture(t)
	writeCurrent(t, p, "personal\n", time.Now())
	d.SetAccountResolver(newAccountResolverForPaths(p))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventAccountName(t, d, id)
	if !ok || got != "personal" {
		t.Fatalf("account_name = (%q, valid=%v), want (\"personal\", true)", got, ok)
	}
}

// AC [functional]: after `prism account use work`, subsequent rows record
// 'work' while earlier rows retain 'personal'.
func TestWriteEvent_MidSwitchAttribution(t *testing.T) {
	d := openAccountTestDB(t)
	p := accountFixture(t)
	t0 := time.Now()
	writeCurrent(t, p, "personal\n", t0)
	d.SetAccountResolver(newAccountResolverForPaths(p))

	earlier := writeTestEvent(t, d, "repo@main")

	// Switch accounts with a strictly newer mtime so the resolver invalidates.
	writeCurrent(t, p, "work\n", t0.Add(2*time.Second))
	later := writeTestEvent(t, d, "repo@main")

	if got, ok := eventAccountName(t, d, earlier); !ok || got != "personal" {
		t.Errorf("earlier row account_name = (%q, %v), want personal", got, ok)
	}
	if got, ok := eventAccountName(t, d, later); !ok || got != "work" {
		t.Errorf("later row account_name = (%q, %v), want work", got, ok)
	}
}

// AC [functional]: a spawn_inputs row records the account active at spawn time,
// and it round-trips through SpawnInputsByInstanceID.
func TestInsertSpawnInputs_RecordsActiveAccount(t *testing.T) {
	d := openAccountTestDB(t)
	p := accountFixture(t)
	writeCurrent(t, p, "personal\n", time.Now())
	d.SetAccountResolver(newAccountResolverForPaths(p))

	// spawn_inputs.instance_id has an FK to sessions; insert the parent first.
	inst := uuid.New().String()
	if _, err := d.conn.Exec(
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		inst, "repo@main", "repo", "/code/repo/main", "pi", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := d.InsertSpawnInputs(SpawnInputs{InstanceID: inst, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	si, err := d.SpawnInputsByInstanceID(inst)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil || si.AccountName == nil || *si.AccountName != "personal" {
		t.Fatalf("spawn_inputs.account_name = %v, want personal", si.AccountName)
	}
}

// AC [edge-case]: with no accounts directory present, event writes succeed and
// record account_name='unknown'.
func TestWriteEvent_NoAccountsDir_Unknown(t *testing.T) {
	d := openAccountTestDB(t)
	// Point at a path whose parent does not exist.
	p := account.Paths{Current: filepath.Join(t.TempDir(), "nope", "current")}
	d.SetAccountResolver(newAccountResolverForPaths(p))

	id := writeTestEvent(t, d, "repo@main")
	if got, ok := eventAccountName(t, d, id); !ok || got != usage.UnknownAccount {
		t.Fatalf("account_name = (%q, %v), want unknown", got, ok)
	}
}

// AC [edge-case]: with an empty or whitespace-only `current` pointer, event
// writes succeed and record account_name='unknown'.
func TestWriteEvent_WhitespacePointer_Unknown(t *testing.T) {
	for _, content := range []string{"", "   \n\t "} {
		d := openAccountTestDB(t)
		p := accountFixture(t)
		writeCurrent(t, p, content, time.Now())
		d.SetAccountResolver(newAccountResolverForPaths(p))

		id := writeTestEvent(t, d, "repo@main")
		if got, ok := eventAccountName(t, d, id); !ok || got != usage.UnknownAccount {
			t.Fatalf("content %q: account_name = (%q, %v), want unknown", content, got, ok)
		}
	}
}

// AC [performance]: the resolver reads the pointer at most once per mtime
// change. N events after a single switch trigger at most two content reads.
func TestAccountResolver_ReadsAtMostTwicePerSwitch(t *testing.T) {
	d := openAccountTestDB(t)
	p := accountFixture(t)
	t0 := time.Now()
	writeCurrent(t, p, "personal\n", t0)
	r := newAccountResolverForPaths(p)
	d.SetAccountResolver(r)

	const n = 25
	// Warm the cache and write several events on the same pointer mtime.
	for i := 0; i < n; i++ {
		writeTestEvent(t, d, "repo@main")
	}
	// One switch, then N more events.
	writeCurrent(t, p, "work\n", t0.Add(3*time.Second))
	for i := 0; i < n; i++ {
		writeTestEvent(t, d, "repo@main")
	}

	r.mu.Lock()
	reads := r.reads
	r.mu.Unlock()
	if reads > 2 {
		t.Fatalf("pointer content reads = %d, want at most 2 across %d events and one switch", reads, 2*n)
	}
}

// AC [security]: no accounts/*.json content is read or stored. The resolver
// stats and reads only the `current` pointer; a token planted in an account
// blob never reaches the database.
func TestWriteEvent_NeverStoresTokenContent(t *testing.T) {
	d := openAccountTestDB(t)
	p := accountFixture(t)
	writeCurrent(t, p, "personal\n", time.Now())
	// Plant a token in personal.json — the resolver must never touch it.
	const token = "SECRET-oauth-access-DO-NOT-STORE-abcdef123456"
	if err := os.WriteFile(p.AccountPath("personal"),
		[]byte(`{"type":"oauth","access":"`+token+`"}`), 0o600); err != nil {
		t.Fatalf("write personal.json: %v", err)
	}
	d.SetAccountResolver(newAccountResolverForPaths(p))

	id := writeTestEvent(t, d, "repo@main")
	if got, ok := eventAccountName(t, d, id); !ok || got != "personal" {
		t.Fatalf("account_name = (%q, %v), want personal", got, ok)
	}

	// The token string must not appear anywhere in agent_events.
	var hits int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM agent_events WHERE account_name LIKE '%' || ? || '%'
		  OR payload LIKE '%' || ? || '%'`, token, token,
	).Scan(&hits); err != nil {
		t.Fatalf("scan for token: %v", err)
	}
	if hits != 0 {
		t.Fatalf("token content found in agent_events (%d rows); resolver must record the name only", hits)
	}
}

// AC [functional] + [edge-case]: the migration applies to an existing populated
// prism.db without data loss, is idempotent across two runs, and pre-migration
// rows read back as SQL NULL without any query path panicking.
func TestMigrateV40ToV41_PopulatedDBNoDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	seedV40PopulatedDB(t, path)

	// First Open migrates v40 → v41 and adds the nullable columns.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}

	// Pre-migration rows survive and read back as SQL NULL account_name.
	if got, ok := eventAccountName(t, d, "old-event-1"); ok {
		t.Errorf("pre-migration agent_events row account_name = %q, want SQL NULL", got)
	}
	var eventCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 2 {
		t.Errorf("agent_events count after migration = %d, want 2 (no data loss)", eventCount)
	}

	si, err := d.SpawnInputsByInstanceID("old-inst-1")
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("pre-migration spawn_inputs row lost")
	}
	if si.AccountName != nil {
		t.Errorf("pre-migration spawn_inputs account_name = %q, want nil (SQL NULL)", *si.AccountName)
	}
	if si.ProfileName == nil || *si.ProfileName != "max" {
		t.Errorf("pre-migration spawn_inputs profile_name lost: %v", si.ProfileName)
	}
	d.Close()

	// Second Open is idempotent: version stays 41, rows intact.
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

// seedV40PopulatedDB writes a raw SQLite database in its v40 shape (no
// account_name columns), populated with a couple of rows, and stamps
// schema_version = 40. Foreign keys are off on this raw connection, so a
// spawn_inputs row needs no matching sessions row.
func seedV40PopulatedDB(t *testing.T, path string) {
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
			payload TEXT NOT NULL, created_at INTEGER NOT NULL, instance_id TEXT
		)`,
		`CREATE TABLE spawn_inputs (
			instance_id TEXT PRIMARY KEY, profile_name TEXT, model_flag TEXT,
			variant_flag TEXT, agent_flag TEXT, harness_flag TEXT,
			isolation_flag TEXT, host_mode_flag INTEGER NOT NULL DEFAULT 0,
			pr_number INTEGER, branch_flag TEXT,
			ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,
			containers_flag INTEGER NOT NULL DEFAULT 0, isolation_mode TEXT,
			model_variant_overrides TEXT, skills_manifest_hash TEXT,
			prompt_template_hash TEXT, agent_prompt_hash TEXT, prompt_text TEXT,
			prompt_source TEXT, abtest_pair_id TEXT, extras TEXT,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (40)`,
		`INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		   VALUES ('old-event-1', 'repo@main', 'repo', '/wt', 'state_change', '{}', 1),
		          ('old-event-2', 'repo@main', 'repo', '/wt', 'state_change', '{}', 2)`,
		`INSERT INTO spawn_inputs (instance_id, profile_name, created_at)
		   VALUES ('old-inst-1', 'max', 1)`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed exec: %v\nstmt: %s", err, s)
		}
	}
}
