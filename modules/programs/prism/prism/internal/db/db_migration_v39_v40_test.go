package db_test

// Tests for the v39→v40 migration that adds title provenance and the
// issue/ticket reference to agent_status, and clears every title whose
// provenance cannot be established (#2683).
//
// The migration follows the muted-column template (v33→v34): idempotent
// ALTER TABLE statements, each guarded by a pragma_table_info check, plus
// one UPDATE that is naturally idempotent (a second run matches only rows
// still carrying a NULL source).

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV39DB creates a v39 DB. The flags simulate "the declarative schema
// block already added the column", in which case the migration's
// pragma_table_info guard must detect it and skip the ALTER TABLE.
func seedV39DB(t *testing.T, dbPath string, withTitleSource, withIssueRef bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v39 db: %v", err)
	}
	defer rawConn.Close()

	titleSourceCol := ""
	if withTitleSource {
		titleSourceCol = ",\n\t\t  title_source TEXT"
	}
	issueRefCol := ""
	if withIssueRef {
		issueRefCol = ",\n\t\t  issue_ref TEXT"
	}

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL,
		  instance_id TEXT
		);
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		  pr_number TEXT,
		  round INTEGER,
		  delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT,
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  isolation_mode TEXT,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'pi',
		  harness_session_id TEXT, harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  muted INTEGER NOT NULL DEFAULT 0,
		  containers_enabled INTEGER NOT NULL DEFAULT 0` + titleSourceCol + issueRefCol + `
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS sessions (
		  instance_id TEXT PRIMARY KEY, session_name TEXT NOT NULL, agent_role TEXT,
		  root_agent_name TEXT, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  harness TEXT NOT NULL, harness_session_id TEXT,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  started_at INTEGER NOT NULL, ended_at INTEGER, end_state TEXT,
		  archive_path TEXT, prism_version TEXT, parent_session TEXT
		);
		CREATE TABLE IF NOT EXISTS pending_merges (
		  repo TEXT NOT NULL, pr INTEGER NOT NULL, session_name TEXT NOT NULL,
		  instance_id TEXT NOT NULL, queue_position INTEGER NOT NULL, status TEXT NOT NULL,
		  title TEXT, error TEXT, queued_at INTEGER NOT NULL, last_checked_at INTEGER,
		  merged_at INTEGER, ended_at INTEGER, PRIMARY KEY (repo, pr)
		);
		CREATE TABLE IF NOT EXISTS pending_replay_deliveries (
		  session_name TEXT NOT NULL, delivery_id TEXT NOT NULL, text TEXT NOT NULL,
		  deliver_as TEXT NOT NULL, source TEXT NOT NULL, queued_at INTEGER NOT NULL,
		  PRIMARY KEY (session_name, delivery_id)
		);
		CREATE TABLE IF NOT EXISTS spawn_inputs (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    profile_name TEXT, model_flag TEXT, variant_flag TEXT, agent_flag TEXT,
		    harness_flag TEXT, isolation_flag TEXT, host_mode_flag INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER, branch_flag TEXT, ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,
		    containers_flag INTEGER NOT NULL DEFAULT 0, isolation_mode TEXT,
		    model_variant_overrides TEXT, skills_manifest_hash TEXT, prompt_template_hash TEXT,
		    agent_prompt_hash TEXT, prompt_text TEXT, prompt_source TEXT,
		    abtest_pair_id TEXT, extras TEXT, created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS spawn_outcome (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    end_state TEXT, exit_code INTEGER, duration_ms INTEGER,
		    interrupted_count INTEGER NOT NULL DEFAULT 0,
		    compaction_count INTEGER NOT NULL DEFAULT 0,
		    error_event_count INTEGER NOT NULL DEFAULT 0,
		    permission_ask_count INTEGER NOT NULL DEFAULT 0,
		    permission_denied_count INTEGER NOT NULL DEFAULT 0,
		    doom_loop_count INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER, pr_merged_at INTEGER,
		    review_group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		    review_verdict TEXT, review_pass_count INTEGER,
		    review_fail_count INTEGER, review_none_count INTEGER,
		    rubric_verdict TEXT, rubric_score REAL, rubric_breakdown TEXT, rubric_grader TEXT,
		    tokens_input_total INTEGER NOT NULL DEFAULT 0,
		    tokens_output_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_read_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
		    cost_usd_total REAL NOT NULL DEFAULT 0,
		    tool_call_count INTEGER NOT NULL DEFAULT 0,
		    tool_error_count INTEGER NOT NULL DEFAULT 0,
		    msg_assistant_count INTEGER NOT NULL DEFAULT 0,
		    time_to_first_event_ms INTEGER, time_to_finished_ms INTEGER,
		    computed_at INTEGER NOT NULL, schema_version INTEGER NOT NULL DEFAULT 1
		);
		CREATE TABLE IF NOT EXISTS harness_frames (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, instance_id TEXT,
		  direction TEXT NOT NULL, type TEXT, payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (39);
	`)
	if err != nil {
		t.Fatalf("seed v39 db: %v", err)
	}
}

// seedV39Row inserts a pre-migration agent_status row with the given title
// (pass nil for no title) using the v39 column set.
func seedV39Row(t *testing.T, dbPath, sessionName string, title *string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open to seed row: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Exec(
		`INSERT INTO agent_status (session_name, repo, worktree, state, title, last_seen)
		 VALUES (?, ?, ?, 'active', ?, 1234)`,
		sessionName, "home-ops", "/repo", title,
	); err != nil {
		t.Fatalf("seed agent_status row %q: %v", sessionName, err)
	}
}

// TestMigration_V39ToV40_AddsBothColumns exercises the body-runs branch: a
// v39 DB with neither column. Both ALTER TABLEs execute and the version is
// bumped.
func TestMigration_V39ToV40_AddsBothColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_body_runs.db")
	seedV39DB(t, dbPath, false, false)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v < 40 {
		t.Errorf("schema_version after migration: got %d, want >= 40", v)
	}
	for _, col := range []string{"title_source", "issue_ref"} {
		info, ok := readColumnShape(t, d, "agent_status", col)
		if !ok {
			t.Fatalf("agent_status.%s: column missing after migration", col)
		}
		if info.cType != "TEXT" {
			t.Errorf("agent_status.%s.type = %q, want TEXT", col, info.cType)
		}
		if info.notNull != 0 {
			t.Errorf("agent_status.%s.notnull = %d, want 0 (nullable)", col, info.notNull)
		}
	}
}

// TestMigration_V39ToV40_BodySkips_ColumnsPreExist exercises the
// body-skips branch: both columns already exist. Each must appear exactly
// once — no duplicate-add and no error.
func TestMigration_V39ToV40_BodySkips_ColumnsPreExist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_body_skips.db")
	seedV39DB(t, dbPath, true, true)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v < 40 {
		t.Errorf("schema_version: got %d, want >= 40", v)
	}
	for _, col := range []string{"title_source", "issue_ref"} {
		if n := countColumn(t, d, "agent_status", col); n != 1 {
			t.Errorf("agent_status.%s count = %d, want 1", col, n)
		}
	}
}

// TestMigration_V39ToV40_BodyMixed guards against one pragma check
// short-circuiting the whole migration body: only title_source pre-exists,
// so the first ALTER must be skipped and the second must run.
func TestMigration_V39ToV40_BodyMixed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_body_mixed.db")
	seedV39DB(t, dbPath, true, false)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	for _, col := range []string{"title_source", "issue_ref"} {
		if n := countColumn(t, d, "agent_status", col); n != 1 {
			t.Errorf("agent_status.%s count = %d, want 1", col, n)
		}
	}
}

// TestMigration_V39ToV40_Idempotent verifies a second Open is a no-op.
func TestMigration_V39ToV40_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_idem.db")
	seedV39DB(t, dbPath, false, false)

	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}
	d1.Close()

	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	if v := readVersion(t, d2); v < 40 {
		t.Errorf("schema_version after second open: got %d, want >= 40", v)
	}
	for _, col := range []string{"title_source", "issue_ref"} {
		if n := countColumn(t, d2, "agent_status", col); n != 1 {
			t.Errorf("agent_status.%s count after idempotent open = %d, want 1", col, n)
		}
	}
}

// TestMigration_V39ToV40_FreshDB verifies a fresh database ends at v40 with
// both columns present via the declarative schema block.
func TestMigration_V39ToV40_FreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh_v40.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open (fresh): %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v < 40 {
		t.Errorf("schema_version on fresh DB: got %d, want >= 40", v)
	}
	for _, col := range []string{"title_source", "issue_ref"} {
		if n := countColumn(t, d, "agent_status", col); n != 1 {
			t.Errorf("agent_status.%s count on fresh DB = %d, want 1", col, n)
		}
	}
}

// TestMigration_V39ToV40_ExistingRowReadsCorrectly covers the AC that an
// existing row with no issue_ref or title_source is still readable through
// the Go API after migration, with both new fields nil and every other
// field intact.
func TestMigration_V39ToV40_ExistingRowReadsCorrectly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_existing_row.db")
	seedV39DB(t, dbPath, false, false)
	seedV39Row(t, dbPath, "home-ops@feature", nil)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	st, err := d.CurrentStatus("home-ops@feature")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: pre-migration row missing after migration")
	}
	if st.TitleSource != nil {
		t.Errorf("TitleSource = %q, want nil on a pre-migration row", *st.TitleSource)
	}
	if st.IssueRef != nil {
		t.Errorf("IssueRef = %q, want nil on a pre-migration row", *st.IssueRef)
	}
	if st.Repo != "home-ops" || st.Worktree != "/repo" || st.State != "active" {
		t.Errorf("pre-migration row fields corrupted: %+v", st)
	}

	// The list read path scans the same columns and must agree.
	all, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("AllActiveStatus returned %d rows, want 1", len(all))
	}
	if all[0].TitleSource != nil || all[0].IssueRef != nil {
		t.Errorf("AllActiveStatus row has non-nil new fields: %+v", all[0])
	}
}

// TestMigration_V39ToV40_ClearsUnattributableTitles covers the AC that the
// stale opencode titles are cleared. The seeded row reproduces the measured
// case from the issue: `home-ops@main` carrying "Renovate PR #2887
// app-template v5 upgrade review", a title describing work that finished two
// days before pi's earliest retained event.
func TestMigration_V39ToV40_ClearsUnattributableTitles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_stale_titles.db")
	seedV39DB(t, dbPath, false, false)
	stale := "Renovate PR #2887 app-template v5 upgrade review"
	seedV39Row(t, dbPath, "home-ops@main", &stale)
	other := "some other pre-provenance title"
	seedV39Row(t, dbPath, "aws-databases@main", &other)
	seedV39Row(t, dbPath, "nixos-config@main", nil)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	for _, name := range []string{"home-ops@main", "aws-databases@main", "nixos-config@main"} {
		st, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", name, err)
		}
		if st == nil {
			t.Fatalf("CurrentStatus(%q): row missing", name)
		}
		if st.Title != nil {
			t.Errorf("%s: title = %q after migration, want NULL — every pre-provenance title is cleared", name, *st.Title)
		}
		if st.TitleSource != nil {
			t.Errorf("%s: title_source = %q, want NULL", name, *st.TitleSource)
		}
	}
}

// respawnLikeSpawnSession reproduces internal/session.SpawnSession's
// spawn-time seeding decision exactly: derive a fallback title from the new
// prompt ONLY when the row currently has no title, then upsert.
//
// The guard is the whole point. Passing a fresh title unconditionally would
// make this test vacuous — a non-nil incoming title always wins the
// COALESCE, so the assertion would hold whether or not the migration
// cleared anything. The resurrection bug lives precisely in the nil branch:
// with a stale title still present, SpawnSession passes nil, and
// `title = COALESCE(excluded.title, title)` preserves the stale value
// forever.
func respawnLikeSpawnSession(t *testing.T, d *db.DB, sessionName, prompt string) {
	t.Helper()
	var seedTitle *string
	existing, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus during respawn: %v", err)
	}
	if existing == nil || existing.Title == nil || *existing.Title == "" {
		if prompt != "" {
			seedTitle = &prompt
		}
	}
	if err := d.UpsertStatusSeedRootAgentName(
		sessionName, "home-ops", "/repo", "idle", seedTitle, nil, "coordinator", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
}

// TestMigration_V39ToV40_ClearedTitleIsNotResurrectedOnRespawn is the second
// half of that AC, and the reason clearing alone is not enough to state.
//
// UpsertStatusSeedRootAgentName applies `title = COALESCE(excluded.title,
// title)`, which is exactly why the stale titles survived every respawn for
// months: SpawnSession passes nil whenever a title already exists, and
// COALESCE then keeps the old one. This test drives that real decision (see
// respawnLikeSpawnSession) over a migrated row and asserts the old title
// does not come back — it is gone from the DB, so there is nothing for
// COALESCE to preserve, and the seeder writes a fresh fallback instead.
func TestMigration_V39ToV40_ClearedTitleIsNotResurrectedOnRespawn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_no_resurrect.db")
	seedV39DB(t, dbPath, false, false)
	stale := "Renovate PR #2887 app-template v5 upgrade review"
	seedV39Row(t, dbPath, "home-ops@main", &stale)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	fresh := "Upgrade the ingress controller"
	respawnLikeSpawnSession(t, d, "home-ops@main", fresh)

	st, err := d.CurrentStatus("home-ops@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil {
		t.Fatal("title is nil after respawn, want the fresh fallback")
	}
	if *st.Title == stale {
		t.Fatalf("the stale opencode title was resurrected on respawn: %q", *st.Title)
	}
	if *st.Title != fresh {
		t.Errorf("title = %q, want the fresh fallback %q", *st.Title, fresh)
	}
	if st.TitleSource == nil || *st.TitleSource != "fallback" {
		t.Errorf("title_source = %v, want \"fallback\"", st.TitleSource)
	}

	// And a SECOND respawn must not bring it back either. This is the
	// "subsequent respawn does not resurrect" half of the AC: by now the row
	// carries a real fallback title, so the seeder passes nil and COALESCE
	// preserves that — the opencode title has no path back.
	respawnLikeSpawnSession(t, d, "home-ops@main", "A different prompt entirely")
	st, err = d.CurrentStatus("home-ops@main")
	if err != nil {
		t.Fatalf("CurrentStatus after second respawn: %v", err)
	}
	if st.Title == nil || *st.Title == stale {
		t.Fatalf("the stale opencode title came back on a second respawn: %v", st.Title)
	}
	if *st.Title != fresh {
		t.Errorf("title = %q after a second respawn, want the existing fallback %q preserved", *st.Title, fresh)
	}
}

// TestMigration_V39ToV40_DoesNotClearAttributedTitles verifies the UPDATE is
// scoped by title_source and not a blanket wipe. A row that already carries
// a provenance — which on a real DB means a title written after this
// migration — is left completely alone, including on a second Open.
func TestMigration_V39ToV40_DoesNotClearAttributedTitles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v39_keeps_attributed.db")
	seedV39DB(t, dbPath, true, true)

	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawConn.Exec(
		`INSERT INTO agent_status (session_name, repo, worktree, state, title, title_source, last_seen)
		 VALUES ('nixos-config@main', 'nixos-config', '/repo', 'active', 'Operator chose this name', 'human', 1234)`,
	); err != nil {
		rawConn.Close()
		t.Fatalf("seed human-titled row: %v", err)
	}
	rawConn.Close()

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	st, err := d.CurrentStatus("nixos-config@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.Title == nil || *st.Title != "Operator chose this name" {
		t.Errorf("title = %v, want the human title preserved", st.Title)
	}
	if st.TitleSource == nil || *st.TitleSource != "human" {
		t.Errorf("title_source = %v, want \"human\"", st.TitleSource)
	}
}
