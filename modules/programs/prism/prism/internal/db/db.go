// Package db provides the prism SQLite database layer.
//
// The database is located at $XDG_STATE_HOME/prism/prism.db, falling back to
// $HOME/.local/state/prism/prism.db. All tables (agent_events, agent_status,
// bus_messages, session_groups, schema_version) are created on Open if they do
// not already exist.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/agent"
	_ "modernc.org/sqlite" // register sqlite3 driver
)

const (
	// PortRangeStart is the first port in the allocation range (inclusive).
	PortRangeStart = 14000
	// PortRangeEnd is the last port in the allocation range (inclusive).
	PortRangeEnd = 14999
)

// DB wraps a SQLite connection.
type DB struct {
	conn *sql.DB
	path string
}

// Path returns the filesystem path of the database file.
func (d *DB) Path() string { return d.path }

// QueryRow executes a query that returns at most one row. Exposed for testing.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.conn.QueryRow(query, args...)
}

// Event represents a row in the agent_events table.
type Event struct {
	ID          string
	SessionName string
	Repo        string
	Worktree    string
	OpencodeSID *string
	Type        string
	Payload     string // raw JSON
	CreatedAt   time.Time
}

// Status represents a row in the agent_status table.
type Status struct {
	SessionName      string
	Repo             string
	Worktree         string
	State            string
	Title            *string
	AgentName        *string
	ModelID          *string
	RootAgentName    *string
	RootModelID      *string
	HostMode         bool
	IsolationMode    string // "podman", "bwrap", or "host"; "" means not recorded (back-compat)
	InstanceID       *string
	LastSeen         time.Time
	EndedAt          *time.Time
	Harness          *string
	HarnessSessionID *string
	HarnessPort      *int
	// GroupID is the session_groups.group_id this session belongs to, or nil
	// when this session is not part of a group. Populated by SpawnSession
	// when opts.GroupID is non-empty (see #849 §3.1 and #859).
	GroupID *string
}

// EffectiveIsolationMode returns the effective isolation mode for this session.
// When IsolationMode is non-empty it is returned directly. Otherwise the mode
// is derived from HostMode for back-compat with pre-v10 DB rows:
// HostMode=true → "host", HostMode=false → "podman".
func (s Status) EffectiveIsolationMode() string {
	if s.IsolationMode != "" {
		return s.IsolationMode
	}
	if s.HostMode {
		return "host"
	}
	return "podman"
}

// BusMessage represents a row in the bus_messages table.
type BusMessage struct {
	ID           string
	FromSession  string
	ToSession    string
	ToInstanceID *string
	Repo         string
	Text         string
	Urgency      string
	SentAt       time.Time
	DeliveredAt  *time.Time
	FailedAt     *time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS agent_events (
  id           TEXT PRIMARY KEY,
  session_name TEXT NOT NULL,
  repo         TEXT NOT NULL,
  worktree     TEXT NOT NULL,
  opencode_sid TEXT,
  type         TEXT NOT NULL,
  payload      TEXT NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

CREATE TABLE IF NOT EXISTS session_groups (
  group_id       TEXT PRIMARY KEY,
  parent_session TEXT NOT NULL,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS agent_status (
  session_name      TEXT PRIMARY KEY,
  repo              TEXT NOT NULL,
  worktree          TEXT NOT NULL,
  state             TEXT NOT NULL,
  title             TEXT,
  agent_name        TEXT,
  model_id          TEXT,
  root_agent_name   TEXT,
  root_model_id     TEXT,
  host_mode         INTEGER NOT NULL DEFAULT 0,
  isolation_mode    TEXT,
  instance_id       TEXT,
  last_seen         INTEGER NOT NULL,
  ended_at          INTEGER,
  harness           TEXT NOT NULL DEFAULT 'opencode',
  harness_session_id TEXT,
  harness_port      INTEGER,
  group_id          TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS bus_messages (
  id               TEXT PRIMARY KEY,
  from_session     TEXT NOT NULL,
  to_session       TEXT NOT NULL,
  to_instance_id   TEXT,
  repo             TEXT NOT NULL,
  text             TEXT NOT NULL,
  urgency          TEXT NOT NULL DEFAULT 'normal',
  sent_at          INTEGER NOT NULL,
  delivered_at     INTEGER,
  failed_at        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_bus_pending ON bus_messages(to_session, delivered_at)
  WHERE delivered_at IS NULL;

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);
`

// Open opens (or creates) the prism database at path.
// It creates parent directories as needed, enables WAL mode, enforces foreign
// keys, runs the full schema, and sets schema_version=11 if the table is empty.
// Pending migrations are applied in order: v1→v2 adds agent_name/model_id;
// v2→v3 adds root_agent_name/root_model_id; v3→v4 adds opencode_port to
// agent_status; v4→v5 adds host_mode to agent_status; v5→v6 adds instance_id
// to agent_status and to_instance_id to bus_messages; v6→v7 adds failed_at to
// bus_messages; v7→v8 adds harness, harness_session_id, and harness_port to
// agent_status; v8→v9 adds the session_groups table and group_id FK column
// (with ON DELETE SET NULL) to agent_status via rename-and-recreate so that
// the REFERENCES clause is present in the schema metadata and fully enforced
// by PRAGMA foreign_keys = ON on both fresh and migrated databases;
// v9→v10 adds isolation_mode TEXT to agent_status (nullable, back-compat);
// v10→v11 drops the legacy opencode_port and opencode_sid columns from
// agent_status (harness-agnostic equivalents harness_port and harness_session_id
// have been the canonical columns since v8; the legacy names were dual-written
// for back-compat and are now removed);
// v11→v12 adds a partial unique index to enforce at most one active coordinator
// per repo (§6.1 from #849): UNIQUE (repo) WHERE root_agent_name='coordinator'
// AND ended_at IS NULL. The IF NOT EXISTS guard makes this idempotent so that
// databases already at v12 (e.g. from a re-run) do not fail.
// v12→v13 is a one-shot maintenance migration that ends (sets ended_at=now,
// in milliseconds) any agent_status rows whose session_name matches legacy
// malformed review-session patterns from a historical recursive-review bug
// (#826): doubled ~review, back-to-back ~review~review, or bare ~review-N-review
// (no role suffix). Only rows where ended_at IS NULL and last_seen IS NULL,
// zero, or older than 7 days are touched. The 7-day threshold is also expressed
// in milliseconds ((unixepoch('now') - 604800) * 1000) to match the column unit.
// Rows already with ended_at set are left alone (idempotent).
// v13→v14 is a one-shot backfill that populates agent_status.last_seen from
// MAX(agent_events.created_at) for sessions where last_seen IS NULL or 0 (i.e.
// the column was never populated by a live WriteEvent call). It is idempotent:
// sessions that already have a non-zero last_seen are left untouched. Rows with
// no matching agent_events remain at 0 (COALESCE preserves the NOT NULL
// constraint). This fixes the gap described in issue #824 for pre-existing rows.
//
// PRAGMA foreign_keys = ON: SQLite foreign-key enforcement is off by default.
// It is set explicitly here — the single constructor through which all prism
// DB connections are opened — so every connection benefits automatically.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("db: create parent dirs: %w", err)
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}

	// Enable WAL mode for better concurrent read/write performance.
	if _, err := conn.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set WAL mode: %w", err)
	}

	// Wait up to 5 seconds before returning SQLITE_BUSY when the DB is locked
	// by another process (e.g. the plugin writing concurrently).
	if _, err := conn.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set busy timeout: %w", err)
	}

	// Enforce foreign-key constraints. SQLite disables FK enforcement by
	// default; this must be set per connection, which is why it lives here
	// (the single constructor used for every prism DB connection).
	if _, err := conn.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: enable foreign keys: %w", err)
	}

	// Create all tables.
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: apply schema: %w", err)
	}

	// Set schema_version=11 if the table is empty. Fresh databases have all
	// current columns from the schema above. The v11→v12, v12→v13, and
	// v13→v14 migrations run immediately below; on a fresh DB they are
	// effectively no-ops (the index already exists and there are no rows to
	// backfill), so starting at 11 rather than 14 is safe.
	var count int
	if err := conn.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: check schema_version: %w", err)
	}
	if count == 0 {
		if _, err := conn.Exec("INSERT INTO schema_version (version) VALUES (11)"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("db: set schema_version: %w", err)
		}
	}

	// Apply pending migrations.
	var version int
	if err := conn.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: read schema_version: %w", err)
	}
	if version == 1 {
		// Migration v1 → v2: add agent_name and model_id to agent_status.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN agent_name TEXT",
			"ALTER TABLE agent_status ADD COLUMN model_id TEXT",
			"UPDATE schema_version SET version = 2",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v1→v2: %w", err)
			}
		}
		version = 2
	}
	if version == 2 {
		// Migration v2 → v3: add root_agent_name and root_model_id to agent_status.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN root_agent_name TEXT",
			"ALTER TABLE agent_status ADD COLUMN root_model_id TEXT",
			"UPDATE schema_version SET version = 3",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v2→v3: %w", err)
			}
		}
		version = 3
	}
	if version == 3 {
		// Migration v3 → v4: add opencode_port to agent_status.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN opencode_port INTEGER",
			"UPDATE schema_version SET version = 4",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v3→v4: %w", err)
			}
		}
		version = 4
	}
	if version == 4 {
		// Migration v4 → v5: add host_mode to agent_status.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN host_mode INTEGER NOT NULL DEFAULT 0",
			"UPDATE schema_version SET version = 5",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v4→v5: %w", err)
			}
		}
		version = 5
	}
	if version == 5 {
		// Migration v5 → v6: add instance_id to agent_status and
		// to_instance_id to bus_messages for session instance isolation.
		// Both columns are nullable so existing rows are unaffected.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN instance_id TEXT",
			"ALTER TABLE bus_messages ADD COLUMN to_instance_id TEXT",
			"UPDATE schema_version SET version = 6",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v5→v6: %w", err)
			}
		}
		version = 6
	}
	if version == 6 {
		// Migration v6 → v7: add failed_at to bus_messages for honest delivery
		// tracking. NULL means not yet attempted or delivered; a non-NULL value
		// records the ms timestamp when delivery exhausted all retries.
		// Additive — existing rows are unaffected.
		migrations := []string{
			"ALTER TABLE bus_messages ADD COLUMN failed_at INTEGER",
			"UPDATE schema_version SET version = 7",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v6→v7: %w", err)
			}
		}
		version = 7
	}
	if version == 7 {
		// Migration v7 → v8: add harness columns to agent_status for multi-harness
		// support (RFC #691). harness defaults to 'opencode' so existing rows
		// retain their implicit harness assignment without data loss.
		// harness_session_id and harness_port are nullable parallels of
		// opencode_sid and opencode_port; both old and new columns are written
		// simultaneously (dual-write) from this schema version onward.
		// Additive — existing rows are unaffected.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN harness TEXT NOT NULL DEFAULT 'opencode'",
			"ALTER TABLE agent_status ADD COLUMN harness_session_id TEXT",
			"ALTER TABLE agent_status ADD COLUMN harness_port INTEGER",
			"UPDATE schema_version SET version = 8",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v7→v8: %w", err)
			}
		}
		version = 8
	}
	if version == 8 {
		// Migration v8 → v9: introduce session_groups table and add group_id FK
		// column to agent_status. group_id is nullable so existing rows are
		// unaffected (they receive NULL). The FK is enforced with ON DELETE SET
		// NULL so that deleting a session_groups row clears group_id on member
		// sessions without removing their history.
		//
		// SQLite does not support adding a column with a REFERENCES clause via
		// ALTER TABLE ADD COLUMN. We therefore use the recommended rename-and-
		// recreate pattern: create a new table with the REFERENCES clause, copy
		// all rows across, drop the old table, and rename the new one. This is
		// wrapped in a transaction with PRAGMA foreign_keys = OFF (required by
		// the SQLite docs for schema changes) so the intermediate state — where
		// the old table has no FK and the new table exists alongside it — is
		// never visible to concurrent readers and does not trigger spurious FK
		// violations. foreign_keys is re-enabled immediately after.
		//
		// See https://www.sqlite.org/lang_altertable.html#otheralter
		if err := func() error {
			if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
				return fmt.Errorf("disable FK: %w", err)
			}
			tx, err := conn.Begin()
			if err != nil {
				return fmt.Errorf("begin tx: %w", err)
			}
			defer tx.Rollback() //nolint:errcheck
			steps := []string{
				// Create the session_groups table first (the FK target must
				// exist before we can reference it).
				`CREATE TABLE IF NOT EXISTS session_groups (
				  group_id       TEXT PRIMARY KEY,
				  parent_session TEXT NOT NULL,
				  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
				)`,
				// Recreate agent_status with the REFERENCES clause.
				`CREATE TABLE agent_status_new (
				  session_name      TEXT PRIMARY KEY,
				  repo              TEXT NOT NULL,
				  worktree          TEXT NOT NULL,
				  state             TEXT NOT NULL,
				  title             TEXT,
				  opencode_sid      TEXT,
				  agent_name        TEXT,
				  model_id          TEXT,
				  root_agent_name   TEXT,
				  root_model_id     TEXT,
				  opencode_port     INTEGER,
				  host_mode         INTEGER NOT NULL DEFAULT 0,
				  instance_id       TEXT,
				  last_seen         INTEGER NOT NULL,
				  ended_at          INTEGER,
				  harness           TEXT NOT NULL DEFAULT 'opencode',
				  harness_session_id TEXT,
				  harness_port      INTEGER,
				  group_id          TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
				)`,
				// Copy all existing rows; new group_id column gets NULL.
				`INSERT INTO agent_status_new
				  SELECT session_name, repo, worktree, state, title,
				         opencode_sid, agent_name, model_id,
				         root_agent_name, root_model_id, opencode_port,
				         host_mode, instance_id, last_seen, ended_at,
				         harness, harness_session_id, harness_port, NULL
				  FROM agent_status`,
				"DROP TABLE agent_status",
				"ALTER TABLE agent_status_new RENAME TO agent_status",
				"UPDATE schema_version SET version = 9",
			}
			for _, s := range steps {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("step %q: %w", s[:min(40, len(s))], err)
				}
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
				return fmt.Errorf("re-enable FK: %w", err)
			}
			return nil
		}(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("db: migration v8→v9: %w", err)
		}
		version = 9
	}
	if version == 9 {
		// Migration v9 → v10: add isolation_mode TEXT to agent_status.
		// Nullable so existing rows receive NULL (back-compat with pre-10 data).
		// When NULL, callers derive the mode from host_mode for back-compat.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN isolation_mode TEXT",
			"UPDATE schema_version SET version = 10",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v9→v10: %w", err)
			}
		}
		version = 10
	}
	if version == 10 {
		// Migration v10 → v11: drop the legacy opencode_port and opencode_sid
		// columns from agent_status. Data in these columns was dual-written to
		// harness_port and harness_session_id since v8; however for databases
		// that were not actively used after v8 the harness columns may still be
		// NULL while the legacy columns carry data. Back-fill first so that no
		// data is lost, then drop the legacy columns.
		// SQLite supports ALTER TABLE DROP COLUMN since 3.35 (2021).
		migrations := []string{
			// Back-fill harness_session_id from opencode_sid where not already set.
			`UPDATE agent_status SET harness_session_id = opencode_sid
			  WHERE harness_session_id IS NULL AND opencode_sid IS NOT NULL`,
			// Back-fill harness_port from opencode_port where not already set.
			`UPDATE agent_status SET harness_port = opencode_port
			  WHERE harness_port IS NULL AND opencode_port IS NOT NULL`,
			"ALTER TABLE agent_status DROP COLUMN opencode_port",
			"ALTER TABLE agent_status DROP COLUMN opencode_sid",
			"UPDATE schema_version SET version = 11",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v10→v11: %w", err)
			}
		}
		version = 11
	}
	if version == 11 {
		// Migration v11 → v12: add a partial unique index enforcing at most one
		// active coordinator per repo (§6.1 from #849). The index is partial so
		// that:
		//   - ended coordinators (ended_at IS NOT NULL) are excluded, allowing
		//     a new coordinator to start for the same repo after the previous one ends.
		//   - sessions without root_agent_name='coordinator' are unaffected.
		// CREATE INDEX IF NOT EXISTS makes this idempotent.
		migrations := []string{
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
			   ON agent_status (repo)
			   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL`,
			"UPDATE schema_version SET version = 12",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v11→v12: %w", err)
			}
		}
		version = 12
	}
	if version == 12 {
		// Migration v12 → v13: one-shot maintenance cleanup of agent_status rows
		// whose session_name matches legacy malformed review-agent patterns
		// produced by a historical recursive-review bug (#826).
		//
		// Patterns matched (three LIKE clauses cover all observed shapes):
		//   %~review-%~review%  — doubled ~review with no role suffix
		//                         e.g. ~review-1-review~review-1-review
		//   %~review~review%    — back-to-back ~review (older variant)
		//                         e.g. ~review-3~review
		//   %~review-%-review   — bare review suffix with no role component
		//                         e.g. ~review-1-review (trailing, no ~prefix)
		//
		// The current valid shape, <parent>~review-<N>-review-<role>, has a
		// non-empty role suffix (e.g. "-code", "-goal") and does NOT end in
		// "-review" with nothing after it, so it is NOT matched by the third
		// pattern.  It also contains no back-to-back ~review~review or
		// doubled ~review-%~review, so it is not matched by the first two.
		//
		// ended_at and last_seen are both stored in Unix milliseconds throughout
		// the codebase (time.Now().UnixMilli() / time.UnixMilli()), so:
		//   - ended_at is set to unixepoch('now') * 1000 (ms)
		//   - the 7-day staleness threshold is (unixepoch('now') - 604800) * 1000
		//     where 604800 is 7 × 86400 seconds.
		//
		// Only rows where ended_at IS NULL and last_seen is NULL, zero, or older
		// than 7 days are touched — this avoids accidentally closing any session
		// that might still be active.  Rows that already have ended_at set are
		// left alone (the WHERE ended_at IS NULL guard makes this idempotent).
		migrations := []string{
			`UPDATE agent_status
			   SET ended_at = unixepoch('now') * 1000
			 WHERE ended_at IS NULL
			   AND (last_seen IS NULL OR last_seen = 0
			        OR last_seen < ((unixepoch('now') - 604800) * 1000))
			   AND (session_name LIKE '%~review-%~review%'
			    OR  session_name LIKE '%~review~review%'
			    OR  session_name LIKE '%~review-%-review')`,
			"UPDATE schema_version SET version = 13",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v12→v13: %w", err)
			}
		}
		version = 13
	}
	if version == 13 {
		// Migration v13 → v14: one-shot backfill of agent_status.last_seen for
		// rows where last_seen is NULL or 0 (column was never populated by a live
		// WriteEvent call). We set last_seen = MAX(agent_events.created_at) for
		// the owning session. The WHERE guard (last_seen IS NULL OR last_seen = 0)
		// makes this idempotent — sessions that already have a real last_seen
		// value are left untouched. Rows with no matching agent_events get NULL
		// from the subquery; COALESCE(..., 0) keeps them at 0 so the NOT NULL
		// constraint is satisfied.
		migrations := []string{
			`UPDATE agent_status
			   SET last_seen = COALESCE(
			         (SELECT MAX(created_at) FROM agent_events
			           WHERE agent_events.session_name = agent_status.session_name),
			         0)
			 WHERE last_seen IS NULL OR last_seen = 0`,
			"UPDATE schema_version SET version = 14",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v13→v14: %w", err)
			}
		}
		version = 14
	}

	return &DB{conn: conn, path: path}, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// currentStateOf looks up the current agent state for sessionName. Returns
// ("", nil) when no row exists (fresh insert path — caller should skip
// transition validation).
func (d *DB) currentStateOf(sessionName string) (agent.AgentState, error) {
	var state string
	err := d.conn.QueryRow(
		"SELECT state FROM agent_status WHERE session_name = ?", sessionName,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: read current state: %w", err)
	}
	return agent.AgentState(state), nil
}

// checkTransition reads the current state for sessionName and validates that
// transitioning to toState is permitted. When the transition is invalid it
// logs a warning to stderr (including session name, from→to pair, and caller
// context) and returns — it does not return an error, so callers are never
// blocked by an invalid transition.
//
// Same-state "transitions" (fromState == toState) are always silently skipped:
// they represent metadata-only upserts (title, harness_session_id, model_id,
// last_seen) where the state value does not actually change, so there is
// nothing to validate.
//
// Callers pass a short context string (e.g. "UpsertStatus", "pane-died") that
// is included in the log line to help locate the call site.
func (d *DB) checkTransition(sessionName string, toState agent.AgentState, callerCtx string) {
	fromState, err := d.currentStateOf(sessionName)
	if err != nil {
		// Non-fatal: if we can't read the current state we skip validation.
		fmt.Fprintf(os.Stderr, "[prism] %s: could not read current state for %q: %v\n",
			callerCtx, sessionName, err)
		return
	}
	if fromState == "" {
		// No prior row — fresh insert; no transition to validate.
		return
	}
	if fromState == toState {
		// Same-state update — metadata-only refresh, nothing to validate.
		return
	}
	if err := agent.Transition(fromState, toState); err != nil {
		fmt.Fprintf(os.Stderr, "[prism] %s: invalid transition for session %q: %v\n",
			callerCtx, sessionName, err)
	}
}

// UpsertStatus inserts or updates the agent_status row for sessionName.
// repo and worktree are always overwritten on conflict. title, harnessSessionID,
// agentName, and modelID are updated only when non-nil (COALESCE).
func (d *DB) UpsertStatus(sessionName, repo, worktree, state string, title *string, harnessSessionID *string) error {
	return d.UpsertStatusWithAgent(sessionName, repo, worktree, state, title, harnessSessionID, nil, nil)
}

// UpsertStatusWithAgent is like UpsertStatus but also accepts agentName and
// modelID, which are written to agent_status.agent_name and agent_status.model_id
// using COALESCE (only overwriting when non-nil). root_agent_name and root_model_id
// are NOT touched by this method — use UpsertStatusWithRootAgent for session creation.
func (d *DB) UpsertStatusWithAgent(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, agentName *string, modelID *string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusWithAgent")
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, agent_name, model_id, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'opencode', ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  agent_name         = COALESCE(excluded.agent_name, agent_name),
  model_id           = COALESCE(excluded.model_id, model_id),
  last_seen          = excluded.last_seen,
  harness            = 'opencode',
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, agentName, modelID, now, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status: %w", err)
	}
	return nil
}

// UpdateRootModelID unconditionally sets root_model_id for sessionName to the
// given model value. Unlike UpsertStatusWithRootAgent (which falls back to the
// existing value when the incoming value is nil), this method always overwrites,
// allowing the current session's model to replace a stale value from a prior
// session.
//
// It is a no-op when no row exists for sessionName (returns nil).
// Called by the sidecar when a completed assistant message from the root agent
// reveals the current model, so that coordinator notifications always reflect
// the live model configuration.
func (d *DB) UpdateRootModelID(sessionName, modelID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET root_model_id = ? WHERE session_name = ?",
		modelID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: update root_model_id: %w", err)
	}
	return nil
}

// UpsertStatusSeedRootAgentName is like UpsertStatus but also writes
// rootAgentName to root_agent_name when it is non-empty. On conflict (update),
// root_agent_name is written via COALESCE — the existing value is preserved if
// the incoming rootAgentName is empty.
//
// This is the spawn-time seeding path: called when we know the agent role at
// session creation time (before the sidecar's first upsertState() call fires),
// so that the DB row has a non-NULL root_agent_name from the first moment.
// The sidecar will later write the same value idempotently via
// UpsertStatusWithRootAgent (COALESCE preserves the already-set value).
func (d *DB) UpsertStatusSeedRootAgentName(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, rootAgentName string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusSeedRootAgentName")
	now := time.Now().UnixMilli()
	// When rootAgentName is empty, fall back to leaving root_agent_name as-is
	// (COALESCE with NULL excluded value preserves existing). When non-empty,
	// write it, but still use COALESCE so a later sidecar write of the same
	// value doesn't produce a spurious update.
	var rootAgentNamePtr *string
	if rootAgentName != "" {
		rootAgentNamePtr = &rootAgentName
	}
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, root_agent_name, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, 'opencode', ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  root_agent_name    = COALESCE(excluded.root_agent_name, root_agent_name),
  last_seen          = excluded.last_seen,
  harness            = 'opencode',
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, rootAgentNamePtr, now, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status seed root agent name: %w", err)
	}
	return nil
}

// UpsertStatusWithRootAgent is like UpsertStatusWithAgent but also writes
// root_agent_name and root_model_id. On conflict (update), root_agent_name and
// root_model_id prefer the incoming (excluded) value via COALESCE — the sidecar
// is authoritative and can correct a stale or wrong value on every state update.
//
// The Go sidecar is the authoritative source of root_agent_name: it calls this
// method on every state transition when Config.AgentRole is set. Because the
// sidecar value takes precedence, a row written with the wrong root_agent_name
// (e.g. from a legacy or race-condition write) is corrected on the very next
// sidecar call. The TypeScript plugin (prism-hooks.ts) does not write
// root_agent_name.
func (d *DB) UpsertStatusWithRootAgent(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, agentName *string, modelID *string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusWithRootAgent")
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'opencode', ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  agent_name         = COALESCE(excluded.agent_name, agent_name),
  model_id           = COALESCE(excluded.model_id, model_id),
  root_agent_name    = COALESCE(excluded.root_agent_name, root_agent_name),
  root_model_id      = COALESCE(excluded.root_model_id, root_model_id),
  last_seen          = excluded.last_seen,
  harness            = 'opencode',
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, agentName, modelID, agentName, modelID, now, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status with root agent: %w", err)
	}
	return nil
}

// UpsertStatusIfNotTerminal upserts the state for sessionName only when the
// current state is not already a terminal state (finished, interrupted, or
// deleted) and the session has not yet been ended (ended_at IS NULL). Returns
// (true, nil) if the update was applied, (false, nil) if the session was
// already in a terminal state, has been ended, or did not exist, or
// (false, err) on a database error.
//
// This is used by the pane-died hook to transition active sessions to
// "interrupted" without clobbering a clean "finished" that was written first,
// and without acting on sessions that have already been ended by cleanup.
func (d *DB) UpsertStatusIfNotTerminal(sessionName, state string) (bool, error) {
	// Snapshot the current state before the write so that the advisory
	// transition check (below) sees the from-state rather than the newly
	// written to-state.
	fromState, _ := d.currentStateOf(sessionName)
	now := time.Now().UnixMilli()
	const q = `
UPDATE agent_status
SET state = ?, last_seen = ?
WHERE session_name = ?
  AND ended_at IS NULL
  AND state NOT IN ('finished', 'interrupted', 'deleted')`
	res, err := d.conn.Exec(q, state, now, sessionName)
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: rows affected: %w", err)
	}
	// Validate only when the write was actually applied (n > 0) and we have a
	// prior state to validate from. When the SQL WHERE clause suppresses the
	// write (session already terminal), there is no transition to check.
	if n > 0 && fromState != "" {
		if terr := agent.Transition(fromState, agent.AgentState(state)); terr != nil {
			fmt.Fprintf(os.Stderr, "[prism] UpsertStatusIfNotTerminal: invalid transition for session %q: %v\n",
				sessionName, terr)
		}
	}
	return n > 0, nil
}

// UpsertStatusInterruptedOverrideFinished transitions the session to
// "interrupted", allowing it to override a "finished" state in addition to the
// active states that UpsertStatusIfNotTerminal covers.  This is used by the
// pane-died hook when the pane exited with a non-zero exit code: a non-zero
// exit means the process was killed or crashed, so even a prior "finished" that
// the plugin wrote should be corrected to "interrupted".
//
// "deleted" is still left intact — a deleted session should not be resurrected.
// "interrupted" is also left alone to avoid a no-op double-write.
//
// Returns (true, nil) if the update was applied, (false, nil) if the row did
// not exist, was already interrupted or deleted, or has ended_at set.
func (d *DB) UpsertStatusInterruptedOverrideFinished(sessionName string) (bool, error) {
	// Snapshot the current state before the write so that the advisory
	// transition check (below) sees the from-state rather than the newly
	// written to-state.
	fromState, _ := d.currentStateOf(sessionName)
	now := time.Now().UnixMilli()
	const q = `
UPDATE agent_status
SET state = 'interrupted', last_seen = ?
WHERE session_name = ?
  AND ended_at IS NULL
  AND state NOT IN ('interrupted', 'deleted')`
	res, err := d.conn.Exec(q, now, sessionName)
	if err != nil {
		return false, fmt.Errorf("db: upsert status interrupted override finished: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: upsert status interrupted override finished: rows affected: %w", err)
	}
	// Validate only when the write was actually applied (n > 0) and we have a
	// prior state to validate from. When the SQL WHERE clause suppresses the
	// write (session already interrupted or deleted), there is no transition
	// to check.
	if n > 0 && fromState != "" {
		if terr := agent.Transition(fromState, agent.StateInterrupted); terr != nil {
			fmt.Fprintf(os.Stderr, "[prism] UpsertStatusInterruptedOverrideFinished: invalid transition for session %q: %v\n",
				sessionName, terr)
		}
	}
	return n > 0, nil
}

// SetEnded marks the session as ended by setting ended_at to now.
func (d *DB) SetEnded(sessionName string) error {
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		"UPDATE agent_status SET ended_at = ? WHERE session_name = ?",
		now, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set ended: %w", err)
	}
	return nil
}

// MarkAllEnded marks every row in agent_status where ended_at IS NULL as ended
// by setting ended_at = now (Unix milliseconds). The state column is intentionally
// left unchanged — ended_at IS NULL is the canonical "active session" filter used
// throughout the codebase; state captures the last known agent state before teardown
// and is not overwritten here.
//
// It is used by `prism reset` to atomically close all live sessions in one
// query rather than iterating over them individually.
//
// Returns the number of rows updated and any database error.
// When there are no rows with ended_at IS NULL, returns (0, nil) — not an error.
func (d *DB) MarkAllEnded() (int64, error) {
	now := time.Now().UnixMilli()
	res, err := d.conn.Exec(
		"UPDATE agent_status SET ended_at = ? WHERE ended_at IS NULL",
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: rows affected: %w", err)
	}
	return n, nil
}

// ClearEnded clears the ended_at timestamp for sessionName, making the session
// visible again to AllActiveStatus and the dashboard (which both filter
// WHERE ended_at IS NULL). Called when a session resumes from a terminal state
// so that the resumed session re-appears in all active-session views.
func (d *DB) ClearEnded(sessionName string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET ended_at = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: clear ended: %w", err)
	}
	return nil
}

// AllocatePort picks the lowest unused port from the range PortRangeStart–PortRangeEnd,
// writes it to agent_status.harness_port for sessionName, and returns it.
//
// A port is considered "in use" if it is assigned to a session whose ended_at IS NULL
// and harness_port IS NOT NULL. Ports assigned to ended sessions (ended_at IS NOT NULL)
// are reclaimed and available for reuse.
//
// In addition to the DB check, each candidate port is probed at the OS level via
// a brief TCP listen on 127.0.0.1:<port>. This prevents conflicts with non-prism
// processes that happen to be using a port in the range.
//
// Returns an error if all ports in the range are exhausted or if the session does
// not exist in agent_status.
func (d *DB) AllocatePort(sessionName string) (int, error) {
	// Collect ports currently assigned to active (non-ended) sessions.
	rows, err := d.conn.Query(
		"SELECT harness_port FROM agent_status WHERE ended_at IS NULL AND harness_port IS NOT NULL",
	)
	if err != nil {
		return 0, fmt.Errorf("db: allocate port: query used ports: %w", err)
	}
	usedPorts := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, fmt.Errorf("db: allocate port: scan port: %w", err)
		}
		usedPorts[p] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("db: allocate port: iterate ports: %w", err)
	}

	// Find the lowest unused port that is also available at the OS level.
	for port := PortRangeStart; port <= PortRangeEnd; port++ {
		if usedPorts[port] {
			continue
		}
		if !portAvailable(port) {
			continue
		}
		// Write the allocated port to agent_status.
		res, err := d.conn.Exec(
			"UPDATE agent_status SET harness_port = ? WHERE session_name = ?",
			port, sessionName,
		)
		if err != nil {
			return 0, fmt.Errorf("db: allocate port: update: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("db: allocate port: rows affected: %w", err)
		}
		if n == 0 {
			return 0, fmt.Errorf("db: allocate port: session %q not found in agent_status", sessionName)
		}
		return port, nil
	}

	return 0, fmt.Errorf("db: allocate port: all ports in range %d–%d are exhausted", PortRangeStart, PortRangeEnd)
}

// ReleasePort sets harness_port = NULL for the given session.
// Returns an error if the session does not exist in agent_status.
// Calling ReleasePort on a session whose harness_port is already NULL is
// idempotent and returns nil.
func (d *DB) ReleasePort(sessionName string) error {
	res, err := d.conn.Exec(
		"UPDATE agent_status SET harness_port = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: release port: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: release port: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("db: release port: session %q not found in agent_status", sessionName)
	}
	return nil
}

// SetHostMode sets the host_mode column for the given session to 1 (true) or
// 0 (false). Called by spawn when --host-mode is passed, so that cleanup can
// skip container teardown for host-mode sessions.
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) SetHostMode(sessionName string, hostMode bool) error {
	val := 0
	if hostMode {
		val = 1
	}
	_, err := d.conn.Exec(
		"UPDATE agent_status SET host_mode = ? WHERE session_name = ?",
		val, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set host_mode: %w", err)
	}
	return nil
}

// SetIsolationMode records the resolved isolation mode for the given session.
// mode is one of "podman", "bwrap", or "host". This is persisted so that
// prism restore can re-spawn the session in the same isolation mode.
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) SetIsolationMode(sessionName, mode string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET isolation_mode = ? WHERE session_name = ?",
		mode, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set isolation_mode: %w", err)
	}
	return nil
}

// portAvailable checks whether a TCP port is available on localhost by
// attempting a brief listen. Returns true if the port is free.
func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// RefreshWorktree unconditionally updates the repo and worktree columns for an
// existing agent_status row. It also resets state to idle and refreshes
// last_seen, making it useful for prism restore (which needs a clean idle state
// regardless of what the prior session left behind). Unlike UpsertStatus, this
// does not insert a new row when none exists — it is a no-op for unknown sessions.
//
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) RefreshWorktree(sessionName, repo, worktree string) error {
	// RefreshWorktree is an administrative reset (correcting corrupted values
	// during prism restore), not a normal lifecycle transition. It bypasses
	// the state machine advisory check intentionally — idle is not a valid
	// to-state in ValidTransitions, so calling checkTransition here would
	// always produce spurious warnings on every restore invocation.
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		`UPDATE agent_status
		    SET repo = ?, worktree = ?, state = ?, last_seen = ?
		  WHERE session_name = ?`,
		repo, worktree, "idle", now, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: refresh worktree: %w", err)
	}
	return nil
}

// WriteEvent inserts an event row into agent_events and, when a matching
// agent_status row exists for e.SessionName, bumps last_seen to
// MAX(last_seen, e.CreatedAt) in the same transaction. Writing an event for
// an unknown session_name (no agent_status row) is not an error — the event
// is still recorded and no last_seen update is attempted.
func (d *DB) WriteEvent(e Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	createdAt := e.CreatedAt.UnixMilli()

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: write event: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const insertQ = `
INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insertQ, e.ID, e.SessionName, e.Repo, e.Worktree, e.OpencodeSID, e.Type, e.Payload, createdAt); err != nil {
		return fmt.Errorf("db: write event: insert: %w", err)
	}

	// Bump last_seen only when a matching agent_status row exists. The MAX
	// guard ensures we never move last_seen backward (e.g. for out-of-order
	// event replays or backfill writes with old timestamps).
	const updateQ = `
UPDATE agent_status
   SET last_seen = MAX(last_seen, ?)
 WHERE session_name = ?`
	if _, err := tx.Exec(updateQ, createdAt, e.SessionName); err != nil {
		return fmt.Errorf("db: write event: update last_seen: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: write event: commit: %w", err)
	}
	return nil
}

// QueryEvents returns up to limit events for the given session, ordered by
// created_at ASC. before and after are event IDs used for cursor-based
// pagination — pass nil for open-ended queries. types filters by event type;
// pass nil to return all types.
//
// When limit > 0 and neither before nor after is set (a plain "last N" query),
// the most-recent N events are returned (newest first in the DB query, then
// reversed to produce chronological ASC order in the result).
// When after is set (forward pagination from a cursor), events are fetched
// ASC from that point. When before is set (backward pagination), events are
// fetched DESC up to the cursor then reversed to chronological order.
func (d *DB) QueryEvents(sessionName string, limit int, before, after *string, types []string) ([]Event, error) {
	args := []any{sessionName}
	var conditions []string
	conditions = append(conditions, "session_name = ?")

	if after != nil {
		var afterTS int64
		err := d.conn.QueryRow(
			"SELECT created_at FROM agent_events WHERE id = ?", *after,
		).Scan(&afterTS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("QueryEvents: cursor event %q not found", *after)
		}
		if err != nil {
			return nil, fmt.Errorf("db: resolve after cursor: %w", err)
		}
		conditions = append(conditions, "created_at > ?")
		args = append(args, afterTS)
	}

	if before != nil {
		var beforeTS int64
		err := d.conn.QueryRow(
			"SELECT created_at FROM agent_events WHERE id = ?", *before,
		).Scan(&beforeTS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("QueryEvents: cursor event %q not found", *before)
		}
		if err != nil {
			return nil, fmt.Errorf("db: resolve before cursor: %w", err)
		}
		conditions = append(conditions, "created_at < ?")
		args = append(args, beforeTS)
	}

	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(placeholders, ",")+")")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Choose ordering strategy:
	//   - "last N" (no cursor): fetch newest N rows with DESC, then reverse.
	//   - forward pagination (after cursor): fetch ASC from cursor.
	//   - backward pagination (before cursor): fetch DESC up to cursor, then reverse.
	//   - no limit: fetch all ASC.
	reverseResult := false
	orderDir := "ASC"
	if limit > 0 && after == nil {
		// Covers both the plain "last N" case and the --before cursor case.
		orderDir = "DESC"
		reverseResult = true
	}

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
		where + " ORDER BY created_at " + orderDir
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events: %w", err)
	}

	if reverseResult {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}

	return events, nil
}

// QueryEventsByMessageIDs returns all events for sessionName whose payload
// contains a "messageId" field matching one of the provided IDs. Only events
// of the specified types are returned; pass nil for types to return all types.
// Results are ordered by created_at ASC.
//
// This is used by checkin's secondary query to fetch tool_call, tool_result,
// permission_ask, permission_denied, and thinking events that belong to a set
// of user message turns retrieved by the primary query.
func (d *DB) QueryEventsByMessageIDs(sessionName string, messageIDs []string, types []string) ([]Event, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	args := []any{sessionName}
	conditions := []string{"session_name = ?"}

	// Build the IN clause for messageIds using JSON_EXTRACT.
	idPlaceholders := make([]string, len(messageIDs))
	for i, id := range messageIDs {
		idPlaceholders[i] = "?"
		args = append(args, id)
	}
	conditions = append(conditions, "JSON_EXTRACT(payload, '$.messageId') IN ("+strings.Join(idPlaceholders, ",")+")")

	if len(types) > 0 {
		typePlaceholders := make([]string, len(types))
		for i, t := range types {
			typePlaceholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(typePlaceholders, ",")+")")
	}

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY created_at ASC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query events by message IDs: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events by message IDs: %w", err)
	}
	return events, nil
}

// CurrentStatus returns the agent_status row for sessionName, or nil if not found.
func (d *DB) CurrentStatus(sessionName string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE session_name = ?`
	row := d.conn.QueryRow(q, sessionName)
	s, err := scanStatus(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: current status: %w", err)
	}
	return s, nil
}

// AllActiveStatus returns all agent_status rows where ended_at IS NULL.
func (d *DB) AllActiveStatus() ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE ended_at IS NULL`
	return d.queryStatuses(q)
}

// AllActiveStatusForRepo returns all active agent_status rows for repo.
func (d *DB) AllActiveStatusForRepo(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE ended_at IS NULL AND repo = ?`
	return d.queryStatuses(q, repo)
}

// AllStatusesWithPrefix returns all agent_status rows (active and ended)
// whose session_name starts with the given prefix. Used by `prism checkin
// <parent>~review` to enumerate all review rounds including completed ones.
//
// The prefix is matched using LIKE with proper escaping of SQL LIKE wildcard
// characters (`%`, `_`, `\`) so that session names containing these characters
// are handled correctly.
func (d *DB) AllStatusesWithPrefix(prefix string) ([]Status, error) {
	// Escape LIKE special characters in the prefix so literal underscores and
	// percent signs in session names are matched exactly, not as wildcards.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE session_name LIKE ? ESCAPE '\'`
	return d.queryStatuses(q, escaped+"%")
}

// AllSessionEvents returns all events for a session, ordered by created_at ASC.
// Unlike QueryEvents, this has no limit — it returns the full event history.
func (d *DB) AllSessionEvents(sessionName string) ([]Event, error) {
	return d.QueryEvents(sessionName, 0, nil, nil, nil)
}

// EventsSince returns all events across all sessions created after sinceMs
// (Unix milliseconds), ordered by created_at ASC. Used by `prism stats --days`.
func (d *DB) EventsSince(sinceMs int64) ([]Event, error) {
	const q = `
SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at
FROM agent_events
WHERE created_at >= ?
ORDER BY created_at ASC`
	rows, err := d.conn.Query(q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("db: events since: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events since: %w", err)
	}
	return events, nil
}

// WaitingCount returns the number of active sessions with state='waiting'.
func (d *DB) WaitingCount() (int, error) {
	var n int
	err := d.conn.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE state = 'waiting' AND ended_at IS NULL",
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: waiting count: %w", err)
	}
	return n, nil
}

// SetInstanceID writes a UUID instance_id to the agent_status row for
// sessionName. Called on tmux-session-start to uniquely identify this session
// incarnation.
func (d *DB) SetInstanceID(sessionName, instanceID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET instance_id = ? WHERE session_name = ?",
		instanceID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set instance_id: %w", err)
	}
	return nil
}

// SetGroupID writes a group_id to the agent_status row for sessionName. Called
// by SpawnSession when opts.GroupID is non-empty to associate the new session
// with a session_groups entry (Issue E hook — see #849 §3.1 and #860).
//
// No-op when sessionName has no agent_status row (the UPDATE affects zero rows
// and returns nil).
func (d *DB) SetGroupID(sessionName, groupID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ?",
		groupID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set group_id: %w", err)
	}
	return nil
}

// ClearInstanceID sets instance_id to NULL for sessionName. Called on
// tmux-session-end to mark the session incarnation as over.
func (d *DB) ClearInstanceID(sessionName string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET instance_id = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: clear instance_id: %w", err)
	}
	return nil
}

// PurgeStaleInstanceMessages deletes undelivered and unfailed bus_messages
// addressed to toSession whose to_instance_id does not match
// currentInstanceID. This purges messages written to a previous incarnation
// of the session that never got delivered. Messages with to_instance_id IS
// NULL (legacy / no instance tagging), delivered messages
// (delivered_at IS NOT NULL), and failed-delivery audit records
// (failed_at IS NOT NULL) are all left intact.
//
// It is safe to call when no matching rows exist — the operation is a no-op
// and returns nil.
func (d *DB) PurgeStaleInstanceMessages(toSession, currentInstanceID string) error {
	const q = `
DELETE FROM bus_messages
WHERE to_session = ?
  AND delivered_at IS NULL
  AND failed_at IS NULL
  AND to_instance_id IS NOT NULL
  AND to_instance_id != ?`
	if _, err := d.conn.Exec(q, toSession, currentInstanceID); err != nil {
		return fmt.Errorf("db: purge stale instance messages: %w", err)
	}
	return nil
}

// PurgeBusMessages deletes undelivered and unfailed bus_messages rows where
// from_session or to_session matches sessionName. Delivered messages
// (delivered_at IS NOT NULL) and failed messages (failed_at IS NOT NULL) are
// left untouched so that delivery audit records survive session cleanup. It is
// safe to call when no matching rows exist — the operation is a no-op and
// returns nil.
func (d *DB) PurgeBusMessages(sessionName string) error {
	const q = `
DELETE FROM bus_messages
WHERE delivered_at IS NULL
  AND failed_at IS NULL
  AND (from_session = ? OR to_session = ?)`
	if _, err := d.conn.Exec(q, sessionName, sessionName); err != nil {
		return fmt.Errorf("db: purge bus messages: %w", err)
	}
	return nil
}

// WriteBusMessage inserts a new row into bus_messages with delivered_at=NULL.
// When msg.ToInstanceID is non-nil, it is written to to_instance_id so that
// delivery can be filtered to the correct session incarnation.
func (d *DB) WriteBusMessage(msg BusMessage) error {
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = time.Now().UnixMilli()
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt)
	if err != nil {
		return fmt.Errorf("db: write bus message: %w", err)
	}
	return nil
}

// WriteBusMessageDelivered inserts a new row into bus_messages with
// delivered_at set to now. This is used for audit-trail writes when a prompt
// was delivered via HTTP (so the plugin doesn't need to deliver it again).
func (d *DB) WriteBusMessageDelivered(msg BusMessage) error {
	now := time.Now().UnixMilli()
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = now
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt, now)
	if err != nil {
		return fmt.Errorf("db: write bus message delivered: %w", err)
	}
	return nil
}

// WriteBusMessageFailed inserts a new row into bus_messages with failed_at set
// to now and delivered_at=NULL. This records a notification that was attempted
// but could not be delivered after all retries were exhausted. It is the
// authoritative signal that a notification was silently lost.
func (d *DB) WriteBusMessageFailed(msg BusMessage) error {
	now := time.Now().UnixMilli()
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = now
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt, now)
	if err != nil {
		return fmt.Errorf("db: write bus message failed: %w", err)
	}
	return nil
}

// UpdateHarnessSessionID unconditionally sets harness_session_id for sessionName
// to the given sid value. Unlike UpsertStatus (which only updates harness_session_id
// via COALESCE when non-nil), this always overwrites — allowing the sidecar to
// keep the stored SID current when the user creates a new harness session
// mid-conversation (e.g. via /continue or TUI restart).
//
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) UpdateHarnessSessionID(sessionName, sid string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET harness_session_id = ? WHERE session_name = ?",
		sid, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: update harness_session_id: %w", err)
	}
	return nil
}

// QueryDoomLoopEvents returns doom_loop_detected events from agent_events,
// ordered by created_at DESC. Optional filters:
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
func (d *DB) QueryDoomLoopEvents(sessionName string, sinceMs int64) ([]Event, error) {
	args := []any{}
	conditions := []string{"type = 'doom_loop_detected'"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
		where + " ORDER BY created_at DESC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query doom loop events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan doom loop event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate doom loop events: %w", err)
	}
	return events, nil
}

// QueryPermissionEvents returns permission_denied or permission_ask events from
// agent_events, ordered by created_at ASC. Optional filters:
//   - eventType: must be "permission_denied" or "permission_ask"
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
func (d *DB) QueryPermissionEvents(eventType, sessionName string, sinceMs int64) ([]Event, error) {
	args := []any{eventType}
	conditions := []string{"type = ?"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
		where + " ORDER BY created_at ASC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query permission events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan permission event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate permission events: %w", err)
	}
	return events, nil
}

// QueryAuditEvents returns audit events from agent_events, ordered by
// created_at DESC. Optional filters:
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
//   - pattern: when non-empty, restrict to events whose payload command field
//     contains this substring (case-insensitive)
//   - limit: when > 0, return at most this many events (default 20 when both
//     limit==0 and sessionName=="")
//
// Note: audit events are subject to the same 90-day Prune() threshold as all
// other agent_events rows. For the forensic use-case described in issue #642,
// 90 days is sufficient, but audit events are not retained indefinitely.
func (d *DB) QueryAuditEvents(sessionName string, sinceMs int64, pattern string, limit int) ([]Event, error) {
	args := []any{}
	conditions := []string{"type = 'audit'"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	if pattern != "" {
		conditions = append(conditions, "LOWER(JSON_EXTRACT(payload, '$.command')) LIKE ?")
		args = append(args, "%"+strings.ToLower(pattern)+"%")
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
		where + " ORDER BY created_at DESC"

	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	} else if sessionName == "" {
		// Default: return the last 20 audit events when no session filter.
		q += " LIMIT 20"
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query audit events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("db: scan audit event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate audit events: %w", err)
	}
	return events, nil
}

// Prune deletes agent_events older than olderThan, and delivered or failed
// bus_messages older than olderThan. It does NOT delete agent_status rows or
// undelivered/unfailed bus_messages.
func (d *DB) Prune(olderThan time.Duration) error {
	threshold := time.Now().Add(-olderThan).UnixMilli()

	if _, err := d.conn.Exec(
		"DELETE FROM agent_events WHERE created_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune agent_events: %w", err)
	}

	if _, err := d.conn.Exec(
		"DELETE FROM bus_messages WHERE delivered_at IS NOT NULL AND delivered_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages (delivered): %w", err)
	}

	if _, err := d.conn.Exec(
		"DELETE FROM bus_messages WHERE failed_at IS NOT NULL AND failed_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages (failed): %w", err)
	}

	return nil
}

// queryStatuses is a helper that runs a SELECT on agent_status and scans rows.
func (d *DB) queryStatuses(q string, args ...any) ([]Status, error) {
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query statuses: %w", err)
	}
	defer rows.Close()

	var statuses []Status
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan status: %w", err)
		}
		statuses = append(statuses, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate statuses: %w", err)
	}
	return statuses, nil
}

// scanner abstracts *sql.Row and *sql.Rows for scanStatus.
type scanner interface {
	Scan(dest ...any) error
}

func scanStatus(s scanner) (*Status, error) {
	var st Status
	var lastSeen int64
	var endedAt sql.NullInt64
	var hostMode sql.NullInt64
	var instanceID sql.NullString
	var harness sql.NullString
	var harnessSessionID sql.NullString
	var harnessPort sql.NullInt64
	var isolationMode sql.NullString
	var groupID sql.NullString
	err := s.Scan(
		&st.SessionName, &st.Repo, &st.Worktree, &st.State,
		&st.Title, &st.AgentName, &st.ModelID,
		&st.RootAgentName, &st.RootModelID, &hostMode, &isolationMode, &instanceID, &lastSeen, &endedAt,
		&harness, &harnessSessionID, &harnessPort, &groupID,
	)
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		g := groupID.String
		st.GroupID = &g
	}
	st.LastSeen = time.UnixMilli(lastSeen)
	if endedAt.Valid {
		t := time.UnixMilli(endedAt.Int64)
		st.EndedAt = &t
	}
	// host_mode: treat NULL (rows written before migration) as 0/false.
	if hostMode.Valid {
		st.HostMode = hostMode.Int64 != 0
	}
	// isolation_mode: NULL means not recorded (pre-v10 row); callers derive
	// the mode from host_mode for back-compat in that case.
	if isolationMode.Valid {
		st.IsolationMode = isolationMode.String
	}
	if instanceID.Valid {
		id := instanceID.String
		st.InstanceID = &id
	}
	if harness.Valid {
		h := harness.String
		st.Harness = &h
	}
	if harnessSessionID.Valid {
		hsid := harnessSessionID.String
		st.HarnessSessionID = &hsid
	}
	if harnessPort.Valid {
		hp := int(harnessPort.Int64)
		st.HarnessPort = &hp
	}
	return &st, nil
}

// ConsecutiveSidecarFailures returns the number of consecutive non-successful
// sidecar runs for the given session, counting from the most recent terminal
// state_change event backward. A "successful" run is one whose terminal
// state_change payload carries "finished"; all other terminal states
// ("interrupted", "error", "deleted") are counted as failures.
//
// The count stops (and the current value is returned) as soon as a "finished"
// state_change is encountered, or when there are no more state_change events
// with terminal states to examine.
//
// Sessions with no recorded terminal state_change events return (0, nil) —
// they are treated as having zero consecutive failures so that new or
// pre-existing sessions are always restored normally.
//
// The limit parameter caps how many events to fetch from the DB. A value of
// 0 (or any non-positive value) uses the default internal cap of 10, which
// is more than sufficient for any realistic circuit-breaker threshold. Callers
// should pass a value at least as large as the configured threshold so the
// query covers the full window; passing 0 is safe and will use the cap.
//
// If the DB query itself fails, the error is returned alongside a count of 0
// so callers can fall back to restoring the session normally (non-fatal path).
func (d *DB) ConsecutiveSidecarFailures(sessionName string, limit int) (int, error) {
	if limit <= 0 {
		limit = 10 // safe upper bound; more than any sane threshold value
	}
	const q = `
SELECT JSON_EXTRACT(payload, '$.state') AS state
FROM agent_events
WHERE session_name = ?
  AND type = 'state_change'
  AND JSON_EXTRACT(payload, '$.state') IN ('finished', 'interrupted', 'error', 'deleted')
ORDER BY created_at DESC, rowid DESC
LIMIT ?`
	rows, err := d.conn.Query(q, sessionName, limit)
	if err != nil {
		return 0, fmt.Errorf("db: consecutive sidecar failures: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return 0, fmt.Errorf("db: consecutive sidecar failures: scan: %w", err)
		}
		if state == "finished" {
			// A successful run: stop counting.
			break
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("db: consecutive sidecar failures: iterate: %w", err)
	}
	return count, nil
}

// GroupMemberResult holds the terminal state and last assistant message for a
// single member of a session group. Used by GroupResults to aggregate outcomes.
type GroupMemberResult struct {
	SessionName string
	RootAgent   string // from root_agent_name; empty when not set
	State       string // terminal state: finished / interrupted / error / deleted
	LastMessage string // last assistant turn from agent_events; empty when none
}

// terminalStates is the set of agent states that indicate a session has stopped
// working and will not make further progress. Note: this includes "deleted"
// (cleaned up mid-run) intentionally — a deleted session will never complete,
// so it counts as terminal for GroupCompleted purposes. This differs from
// review.go's isTerminalState, which omits "deleted" because prism review
// uses separate cleanup detection logic.
var terminalStates = []string{"finished", "interrupted", "error", "deleted"}

// isTerminalState reports whether state is a terminal agent state.
func isTerminalState(state string) bool {
	for _, s := range terminalStates {
		if state == s {
			return true
		}
	}
	return false
}

// RegisterGroup inserts a new row into session_groups and returns the generated
// group_id. parent_session identifies the session that owns this group (e.g.
// the worker running `prism review`).
func (d *DB) RegisterGroup(parentSession string) (string, error) {
	groupID := uuid.New().String()
	const q = `INSERT INTO session_groups (group_id, parent_session) VALUES (?, ?)`
	if _, err := d.conn.Exec(q, groupID, parentSession); err != nil {
		return "", fmt.Errorf("db: register group: %w", err)
	}
	return groupID, nil
}

// GroupCompleted reports whether every agent_status row with this group_id has
// reached a terminal state (finished, interrupted, error, or deleted).
// Returns (true, nil) when all members are terminal (including the case where
// there are no members yet — caller should guard against that if needed).
// Returns (false, nil) when at least one member is still running.
// Returns (false, err) on a database error.
func (d *DB) GroupCompleted(groupID string) (bool, error) {
	// Build the NOT IN list for terminal states.
	placeholders := make([]string, len(terminalStates))
	args := make([]any, 0, 1+len(terminalStates))
	args = append(args, groupID)
	for i, s := range terminalStates {
		placeholders[i] = "?"
		args = append(args, s)
	}
	q := `SELECT COUNT(*) FROM agent_status WHERE group_id = ? AND state NOT IN (` +
		strings.Join(placeholders, ",") + `)`
	var nonTerminalCount int
	if err := d.conn.QueryRow(q, args...).Scan(&nonTerminalCount); err != nil {
		return false, fmt.Errorf("db: group completed: %w", err)
	}
	return nonTerminalCount == 0, nil
}

// GroupResults returns the terminal state and last assistant message for every
// member of the group, keyed by session_name. It is intended for use after
// GroupCompleted returns true. Members that are still active are included but
// their State may be non-terminal.
//
// LastMessage is populated from the most recent msg_assistant event payload
// for each session. It is empty when no such event has been recorded.
func (d *DB) GroupResults(groupID string) (map[string]GroupMemberResult, error) {
	// Fetch each member's session_name, state, and root_agent_name.
	const statusQ = `
SELECT session_name, state, COALESCE(root_agent_name, '')
FROM agent_status
WHERE group_id = ?`
	rows, err := d.conn.Query(statusQ, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: group results: query statuses: %w", err)
	}
	defer rows.Close()

	results := make(map[string]GroupMemberResult)
	for rows.Next() {
		var r GroupMemberResult
		if err := rows.Scan(&r.SessionName, &r.State, &r.RootAgent); err != nil {
			return nil, fmt.Errorf("db: group results: scan status: %w", err)
		}
		results[r.SessionName] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: group results: iterate statuses: %w", err)
	}

	// For each member, fetch the last msg_assistant event payload.
	for name, r := range results {
		const msgQ = `
SELECT payload FROM agent_events
WHERE session_name = ? AND type = 'msg_assistant'
ORDER BY created_at DESC, rowid DESC
LIMIT 1`
		var payload string
		err := d.conn.QueryRow(msgQ, name).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("db: group results: last message for %q: %w", name, err)
		}
		r.LastMessage = payload
		results[name] = r
	}

	return results, nil
}

func scanBusMessage(s scanner) (BusMessage, error) {
	var m BusMessage
	var sentAt int64
	var deliveredAt sql.NullInt64
	var failedAt sql.NullInt64
	var toInstanceID sql.NullString
	err := s.Scan(
		&m.ID, &m.FromSession, &m.ToSession, &toInstanceID, &m.Repo, &m.Text, &m.Urgency,
		&sentAt, &deliveredAt, &failedAt,
	)
	if err != nil {
		return BusMessage{}, err
	}
	m.SentAt = time.UnixMilli(sentAt)
	if deliveredAt.Valid {
		t := time.UnixMilli(deliveredAt.Int64)
		m.DeliveredAt = &t
	}
	if failedAt.Valid {
		t := time.UnixMilli(failedAt.Int64)
		m.FailedAt = &t
	}
	if toInstanceID.Valid {
		id := toInstanceID.String
		m.ToInstanceID = &id
	}
	return m, nil
}

// CoordinatorForRepo returns the agent_status row for the active coordinator
// session of repo (i.e. the row where repo = repo AND root_agent_name =
// "coordinator" AND ended_at IS NULL). Returns nil when no coordinator exists.
// When multiple rows match (schema violation), the most-recently-seen row is
// returned and a duplicate is silently tolerated.
func (d *DB) CoordinatorForRepo(repo string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE repo = ? AND root_agent_name = 'coordinator' AND ended_at IS NULL
ORDER BY last_seen DESC
LIMIT 1`
	row := d.conn.QueryRow(q, repo)
	s, err := scanStatus(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: coordinator for repo: %w", err)
	}
	return s, nil
}

// RootAgentName returns the root_agent_name for sessionName, or "" when the
// row does not exist or root_agent_name is NULL (pre-migration row).
// The second return value (rowExists) distinguishes the two empty cases:
//   - ("", false, nil)  — no agent_status row found (new/unknown session)
//   - ("", true, nil)   — row found but root_agent_name is NULL (pre-migration)
//   - (name, true, nil) — row found with a populated root_agent_name
func (d *DB) RootAgentName(sessionName string) (name string, rowExists bool, err error) {
	var ns sql.NullString
	const q = `SELECT root_agent_name FROM agent_status WHERE session_name = ?`
	if scanErr := d.conn.QueryRow(q, sessionName).Scan(&ns); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: root agent name: %w", scanErr)
	}
	if !ns.Valid {
		return "", true, nil
	}
	return ns.String, true, nil
}

// IsGroupMember returns true when sessionName has a non-NULL group_id in
// agent_status (i.e. it belongs to a session group). Returns false for
// pre-migration rows where group_id is NULL.
func (d *DB) IsGroupMember(sessionName string) (bool, error) {
	var groupID sql.NullString
	const q = `SELECT group_id FROM agent_status WHERE session_name = ?`
	if err := d.conn.QueryRow(q, sessionName).Scan(&groupID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("db: is group member: %w", err)
	}
	return groupID.Valid && groupID.String != "", nil
}

// HasReviewGroup returns true when sessionName is the parent_session of at
// least one row in session_groups. This is the DB-backed way to detect whether
// a session has spawned a review group (replacing the "~review" name heuristic).
func (d *DB) HasReviewGroup(parentSession string) (bool, error) {
	var count int
	const q = `SELECT COUNT(*) FROM session_groups WHERE parent_session = ?`
	if err := d.conn.QueryRow(q, parentSession).Scan(&count); err != nil {
		return false, fmt.Errorf("db: has review group: %w", err)
	}
	return count > 0, nil
}

// AllGroupParents returns a map of group_id → parent_session for all rows in
// session_groups. This is the efficient batch counterpart to ParentSessionFor:
// callers that need parent attribution for a large set of sessions can fetch the
// whole map in one query rather than issuing N individual lookups.
//
// The returned map only contains groups registered in session_groups; sessions
// whose group_id is NULL (pre-migration rows) or whose group_id has no matching
// session_groups row are absent from the map (callers should fall back to the
// name heuristic for those).
func (d *DB) AllGroupParents() (map[string]string, error) {
	const q = `SELECT group_id, parent_session FROM session_groups`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: all group parents: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var groupID, parent string
		if err := rows.Scan(&groupID, &parent); err != nil {
			return nil, fmt.Errorf("db: all group parents: scan: %w", err)
		}
		result[groupID] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: all group parents: iterate: %w", err)
	}
	return result, nil
}

// ParentSessionFor returns the authoritative parent session name for the given
// session. It is the single named source of truth for parent attribution,
// used by both the dashboard (via AllGroupParents + StatusToAgentSession) and
// prism list-sessions (via AllGroupParents in the renderSessionTable sort key).
//
// Resolution order:
//  1. DB-backed (post-migration): looks up session_groups.parent_session via
//     agent_status.group_id. This is the most reliable source — it records the
//     actual caller at spawn time.
//  2. Name-heuristic fallback (pre-migration rows where group_id IS NULL):
//     strips the "~…" suffix from the session name and returns the prefix as
//     the parent name. e.g. "nixos-config@main~review-1-review-code" → parent
//     is "nixos-config@main".
//
// Returns "" when no parent can be determined (top-level sessions, sessions
// with no group_id and no "~" in the branch component, or DB errors).
func (d *DB) ParentSessionFor(sessionName string) string {
	// Step 1: try DB-backed group_id → session_groups.parent_session.
	const q = `
SELECT sg.parent_session
FROM agent_status AS a
JOIN session_groups AS sg ON a.group_id = sg.group_id
WHERE a.session_name = ?`
	var parent string
	err := d.conn.QueryRow(q, sessionName).Scan(&parent)
	if err == nil && parent != "" {
		return parent
	}
	// err == sql.ErrNoRows or group_id IS NULL: fall through to name heuristic.

	// Step 2: name-heuristic fallback — strip the "~…" suffix from the branch
	// component.  Session names are of the form "repo@branch~suffix" where
	// "~suffix" marks a depth-2 review session.  The parent is "repo@branch".
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		branch := sessionName[idx+1:] // e.g. "main~review-1-review-code"
		if tildeIdx := strings.Index(branch, "~"); tildeIdx >= 0 {
			return sessionName[:idx] + "@" + branch[:tildeIdx]
		}
	}
	return ""
}

// GroupMembersForParent returns all agent_status rows whose group_id belongs
// to a session_groups row with parent_session = parentSession.
func (d *DB) GroupMembersForParent(parentSession string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, host_mode, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE group_id IN (SELECT group_id FROM session_groups WHERE parent_session = ?)`
	return d.queryStatuses(q, parentSession)
}
