package db_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/db"
)

// hotPathIndexNames lists every index added by the v31→v32 migration (#1864).
// Maps each index name to its table; the test asserts pragma_index_list
// reports the index against the right table.
var hotPathIndexNames = map[string]string{
	"idx_events_instance":          "agent_events",
	"idx_events_created_at":        "agent_events",
	"idx_agent_status_active":      "agent_status",
	"idx_agent_status_group_id":    "agent_status",
	"idx_agent_status_instance_id": "agent_status",
	"idx_agent_status_repo_active": "agent_status",
}

// TestHotPathIndexes_ExistAfterOpen verifies that every index added by the
// v31→v32 migration (#1864) is present on a freshly opened DB. The check
// uses pragma_index_list(<table>) rather than scanning sqlite_master so
// that index visibility is asserted from the same metadata the planner
// consults — if the index is reported here, EXPLAIN QUERY PLAN can use it.
func TestHotPathIndexes_ExistAfterOpen(t *testing.T) {
	d := openTestDB(t)
	raw := openRawSQLite(t, d.Path())

	// Build the set of indexes present per table.
	type indexKey struct{ table, name string }
	seen := map[indexKey]bool{}
	for _, table := range []string{"agent_events", "agent_status"} {
		rows, err := raw.Query(fmt.Sprintf("PRAGMA index_list(%s)", table))
		if err != nil {
			t.Fatalf("pragma index_list(%s): %v", table, err)
		}
		for rows.Next() {
			// PRAGMA index_list columns: seq, name, unique, origin, partial.
			var seq, isUnique, partial int
			var name, origin string
			if err := rows.Scan(&seq, &name, &isUnique, &origin, &partial); err != nil {
				rows.Close()
				t.Fatalf("scan pragma index_list(%s): %v", table, err)
			}
			seen[indexKey{table, name}] = true
		}
		rows.Close()
	}

	for idx, table := range hotPathIndexNames {
		if !seen[indexKey{table, idx}] {
			t.Errorf("index %q not found on table %q (pragma index_list)", idx, table)
		}
	}
}

// openRawSQLite opens a second connection to the same DB file using the
// modernc.org/sqlite driver directly, so that the test can run
// metadata-only queries (PRAGMA / EXPLAIN QUERY PLAN) without paying for
// the db package's connection setup. WAL journal mode (set by db.Open via
// DSN) makes the parallel read safe.
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

// TestHotPathIndexes_QueryPlanUsesIndex captures EXPLAIN QUERY PLAN for the
// three hot-path queries called out in #1864 and asserts each plan reports
// `SEARCH … USING INDEX <name>` rather than `SCAN`. This proves the new
// indexes are not just present, they are actually selected by the SQLite
// query planner for the relevant access pattern.
//
// To make the planner's cost model prefer the index, the test populates the
// DB with enough rows that a full scan would obviously lose: ~10k events
// across 50 instances and ~500 agent_status rows across multiple groups
// and isolation modes. The numbers mirror the AC's "populated DB" sizing.
func TestHotPathIndexes_QueryPlanUsesIndex(t *testing.T) {
	d := openTestDB(t)
	raw := openRawSQLite(t, d.Path())

	// Populate sessions + agent_events. 50 instances, 200 events each.
	const nInstances = 50
	const nEventsPerInstance = 200

	now := time.Now()
	for i := 0; i < nInstances; i++ {
		iid := fmt.Sprintf("inst-%03d", i)
		sessionName := fmt.Sprintf("repo@s%03d", i)
		if err := d.InsertSession(db.Session{
			InstanceID:  iid,
			SessionName: sessionName,
			Repo:        "repo",
			Worktree:    "/tmp/worktree",
			Harness:     "pi",
			StartedAt:   now.Add(-time.Hour),
		}); err != nil {
			t.Fatalf("InsertSession[%d]: %v", i, err)
		}
		for j := 0; j < nEventsPerInstance; j++ {
			eventType := "tool_call"
			if j%17 == 0 {
				eventType = "msg_assistant"
			}
			iidCopy := iid
			if err := d.WriteEvent(db.Event{
				ID:          uuid.New().String(),
				SessionName: sessionName,
				Repo:        "repo",
				Worktree:    "/tmp/worktree",
				InstanceID:  &iidCopy,
				Type:        eventType,
				Payload:     `{"inputTokens":1,"outputTokens":1}`,
				CreatedAt:   now.Add(-time.Hour).Add(time.Duration(j) * time.Second),
			}); err != nil {
				t.Fatalf("WriteEvent[%d,%d]: %v", i, j, err)
			}
		}
	}

	// Populate agent_status. ~500 rows: half ended, half active; spread
	// across 5 isolation modes and 20 group_ids; some carry instance_id.
	const nStatuses = 500
	for i := 0; i < nStatuses; i++ {
		sessionName := fmt.Sprintf("repo@status-%04d", i)
		// Seed with state="active". The row is created with last_seen via
		// UpsertStatus; below we patch the columns we need for the planner.
		if err := d.UpsertStatus(sessionName, "repo", "/tmp/worktree", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus[%d]: %v", i, err)
		}
		// Half the rows get ended_at set; half stay active.
		if i%2 == 0 {
			if err := d.SetEnded(sessionName); err != nil {
				t.Fatalf("SetEnded[%d]: %v", i, err)
			}
		}
		// Set isolation_mode round-robin across 5 modes.
		modes := []string{"podman", "bwrap", "host", "sandbox-exec", "none"}
		if err := d.SetIsolationMode(sessionName, modes[i%len(modes)]); err != nil {
			t.Fatalf("SetIsolationMode[%d]: %v", i, err)
		}
		// 80% of rows get an instance_id; round-robin across a 20-group set.
		if i%5 != 0 {
			iid := fmt.Sprintf("inst-status-%04d", i)
			if err := d.SetInstanceID(sessionName, iid); err != nil {
				t.Fatalf("SetInstanceID[%d]: %v", i, err)
			}
		}
		if i%4 == 0 {
			gid := fmt.Sprintf("group-%03d", i%20)
			// Register the group first so the FK is satisfied.
			if _, err := raw.Exec(
				`INSERT OR IGNORE INTO session_groups (group_id, parent_session) VALUES (?, ?)`,
				gid, "parent@main",
			); err != nil {
				t.Fatalf("insert group_id[%d]: %v", i, err)
			}
			if err := d.SetGroupID(sessionName, gid); err != nil {
				t.Fatalf("SetGroupID[%d]: %v", i, err)
			}
		}
	}

	// Refresh planner stats so the cost model picks the new indexes.
	if _, err := raw.Exec("ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// Each case: a representative hot-path query, the expected index name,
	// and bind arguments. The plan text is normalised to ignore the noisy
	// id/parent/notused columns of EXPLAIN QUERY PLAN and to ignore
	// whitespace.
	cases := []struct {
		name      string
		query     string
		args      []any
		wantIndex string
	}{
		{
			name: "WriteSpawnOutcome_aggregation",
			// The WriteSpawnOutcome aggregation query from sessions.go,
			// trimmed to the access pattern (we only care about the plan).
			query: `SELECT
				    COUNT(*),
				    MIN(created_at)
				FROM agent_events
				WHERE instance_id = ?`,
			args:      []any{"inst-017"},
			wantIndex: "idx_events_instance",
		},
		{
			name:      "GroupCompleted_by_group_id",
			query:     `SELECT COUNT(*) FROM agent_status WHERE group_id = ? AND state NOT IN ('finished','error','deleted') AND ended_at IS NULL`,
			args:      []any{"group-007"},
			wantIndex: "idx_agent_status_group_id",
		},
		{
			name:      "ActiveSessionCountForMode",
			query:     `SELECT COUNT(*) FROM agent_status WHERE ended_at IS NULL AND isolation_mode = ?`,
			args:      []any{"podman"},
			wantIndex: "idx_agent_status_active",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, raw, tc.query, tc.args...)
			if !strings.Contains(plan, "SEARCH") {
				t.Errorf("plan does not contain SEARCH (expected index %q to be used):\n%s",
					tc.wantIndex, plan)
			}
			if strings.Contains(plan, "SCAN agent_events") || strings.Contains(plan, "SCAN agent_status") {
				t.Errorf("plan still reports a full SCAN (expected index %q to be used):\n%s",
					tc.wantIndex, plan)
			}
			if !strings.Contains(plan, tc.wantIndex) {
				// Not strictly fatal — SQLite may pick an equally good
				// alternate index — but the AC asks us to assert on the
				// specific index, so surface it loudly.
				t.Errorf("plan does not reference expected index %q:\n%s",
					tc.wantIndex, plan)
			}
		})
	}
}

// explainQueryPlan runs EXPLAIN QUERY PLAN and returns the concatenated
// detail column for each row, one per line. The detail column carries the
// human-readable "SEARCH … USING INDEX …" / "SCAN …" text we assert on.
func explainQueryPlan(t *testing.T, raw *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := raw.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		// EXPLAIN QUERY PLAN columns: id, parent, notused, detail.
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan EXPLAIN QUERY PLAN: %v", err)
		}
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate EXPLAIN QUERY PLAN: %v", err)
	}
	return b.String()
}
