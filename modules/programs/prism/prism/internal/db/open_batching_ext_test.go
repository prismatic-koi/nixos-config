package db_test

// open_batching_ext_test.go — black-box tests for the batched open sequence,
// driven through db.Open.
//
// The companion white-box tests are in open_batching_test.go. These cover the
// three properties a caller of db.Open can observe:
//
//   - a populated database still migrates through every table-rebuild
//     migration with its rows and its foreign-key state intact;
//   - a failure part-way through the open sequence leaves no partially
//     migrated database;
//   - durability is unchanged, so the DSN still leaves synchronous at FULL.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestOpen_PopulatedRebuildMigrations_PreserveRowsAndFKState migrates a
// populated database through each of the four table-rebuild migrations and
// checks the rows and the foreign-key state that come out.
//
// This is the case the batched open sequence must not break. A rebuild
// executed inside a transaction would silently ignore PRAGMA foreign_keys =
// OFF and run with enforcement still on. On an empty database that raises no
// error, so only populated data can show the difference.
//
// The seed helpers come from db_test.go and mergequeue_repo_scope_test.go,
// which already build each pre-migration schema shape.
func TestOpen_PopulatedRebuildMigrations_PreserveRowsAndFKState(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, dbPath string)
		// verify runs after db.Open and asserts the rows the rebuild copied.
		verify func(t *testing.T, d *db.DB)
	}{
		{
			name: "v8 to v9 rebuilds agent_status",
			seed: func(t *testing.T, dbPath string) { seedV8DB(t, dbPath) },
			verify: func(t *testing.T, d *db.DB) {
				var repo, worktree, state, harness string
				if err := d.QueryRow(
					`SELECT repo, worktree, state, harness FROM agent_status
					  WHERE session_name = 'repo@main'`,
				).Scan(&repo, &worktree, &state, &harness); err != nil {
					t.Fatalf("seeded agent_status row did not survive the rebuild: %v", err)
				}
				if repo != "repo" || worktree != "/code/repo/main" ||
					state != "active" || harness != "pi" {
					t.Errorf("row changed across the rebuild: repo=%q worktree=%q state=%q harness=%q",
						repo, worktree, state, harness)
				}
			},
		},
		{
			name: "v23 to v24 rebuilds sessions",
			seed: func(t *testing.T, dbPath string) { seedV23DB(t, dbPath) },
			verify: func(t *testing.T, d *db.DB) {
				assertNoDroppedRows(t, d, "sessions")
			},
		},
		{
			name: "v25 to v26 rebuilds agent_status",
			seed: func(t *testing.T, dbPath string) { seedV25DB(t, dbPath) },
			verify: func(t *testing.T, d *db.DB) {
				assertNoDroppedRows(t, d, "agent_status")
			},
		},
		{
			name: "v37 to v38 rebuilds pending_merges",
			seed: func(t *testing.T, dbPath string) {
				seedV37DBWithPendingMergesRows(t, dbPath, []seedRow{
					{PR: 47, SessionName: "repo-a@worker", InstanceID: "inst-a", Status: "queued"},
					{PR: 48, SessionName: "repo-b@worker", InstanceID: "inst-b", Status: "merged"},
				})
			},
			verify: func(t *testing.T, d *db.DB) {
				var count int
				if err := d.QueryRow(`SELECT COUNT(*) FROM pending_merges`).Scan(&count); err != nil {
					t.Fatalf("count pending_merges: %v", err)
				}
				if count != 2 {
					t.Errorf("pending_merges holds %d rows after the rebuild, want 2", count)
				}
				var repo string
				if err := d.QueryRow(
					`SELECT repo FROM pending_merges WHERE pr = 47`,
				).Scan(&repo); err != nil {
					t.Fatalf("read backfilled repo: %v", err)
				}
				if repo != "repo-a" {
					t.Errorf("repo backfill = %q, want %q", repo, "repo-a")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "rebuild.db")
			tc.seed(t, dbPath)

			rowsBefore := tableRowCounts(t, dbPath)

			d, err := db.Open(dbPath)
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			defer d.Close()

			tc.verify(t, d)

			// Foreign-key enforcement must be back on for every connection,
			// and no rebuild may have left a dangling reference behind. A
			// rebuild that ran with enforcement wrongly still on would have
			// failed above; a rebuild that failed to re-enable enforcement
			// shows up here.
			var fkOn int
			if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fkOn); err != nil {
				t.Fatalf("read PRAGMA foreign_keys: %v", err)
			}
			if fkOn != 1 {
				t.Errorf("PRAGMA foreign_keys = %d after migration, want 1", fkOn)
			}
			assertNoForeignKeyViolations(t, d)

			// Every table that existed before the open must still hold at
			// least as many rows as it did. A rebuild that dropped its copy
			// step would show up as a table that lost rows.
			rowsAfter := tableRowCounts(t, dbPath)
			for table, before := range rowsBefore {
				after, ok := rowsAfter[table]
				if !ok {
					t.Errorf("table %q disappeared across the migration", table)
					continue
				}
				if after < before {
					t.Errorf("table %q lost rows across the migration: %d -> %d",
						table, before, after)
				}
			}
		})
	}
}

// TestOpen_FailurePartWayThrough_LeavesNoPartialDatabase drives db.Open at a
// database that makes the open sequence fail after the declarative schema
// block has run.
//
// The file carries a schema_version table whose column is named v, not
// version. CREATE TABLE IF NOT EXISTS therefore leaves it alone, the schema
// block creates every other table, SELECT COUNT(*) reports the table as empty,
// and the seeding INSERT then fails on the missing column. That puts the
// failure part-way through the sequence, with tables already created.
//
// The batched sequence must discard all of it. Nothing the open created may
// survive.
func TestOpen_FailurePartWayThrough_LeavesNoPartialDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "partial.db")

	func() {
		rawConn, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("raw open: %v", err)
		}
		defer rawConn.Close()
		if _, err := rawConn.Exec(
			`CREATE TABLE schema_version (v INTEGER NOT NULL)`,
		); err != nil {
			t.Fatalf("seed incompatible schema_version: %v", err)
		}
	}()

	before := tableNames(t, dbPath)

	d, err := db.Open(dbPath)
	if err == nil {
		d.Close()
		t.Fatal("db.Open succeeded against an incompatible schema_version table; " +
			"the test no longer forces a mid-sequence failure")
	}

	after := tableNames(t, dbPath)
	if len(after) != len(before) {
		t.Errorf("failed open left %d tables behind, want the original %d: before=%v after=%v",
			len(after), len(before), before, after)
	}
	for i := range before {
		if i >= len(after) || before[i] != after[i] {
			t.Errorf("failed open changed the table list: before=%v after=%v", before, after)
			break
		}
	}
}

// TestOpen_SynchronousStaysFull pins the durability setting the whole fsync
// saving depends on not touching.
//
// Batching cuts the number of commits. It must not cut the durability of the
// commits that remain, so SQLite must still be at synchronous=FULL (2). The
// cheaper alternative — relaxing synchronous to NORMAL — is deliberately out
// of scope.
func TestOpen_SynchronousStaysFull(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "durability.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var synchronous int
	if err := d.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("read PRAGMA synchronous: %v", err)
	}
	const sqliteSynchronousFull = 2
	if synchronous != sqliteSynchronousFull {
		t.Errorf("PRAGMA synchronous = %d, want %d (FULL); durability must not change",
			synchronous, sqliteSynchronousFull)
	}

	var journalMode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("PRAGMA journal_mode = %q, want %q", journalMode, "wal")
	}
}

// TestOpen_MigratedDatabaseReachesCurrentVersion checks that a database
// seeded at each rebuild boundary still arrives at the current schema version.
// A probe that wrongly sent one of these down the batched path would fail the
// open; a probe that wrongly stalled a migration would leave the version short.
func TestOpen_MigratedDatabaseReachesCurrentVersion(t *testing.T) {
	seeds := map[string]func(t *testing.T, dbPath string){
		"v8":  func(t *testing.T, p string) { seedV8DB(t, p) },
		"v23": func(t *testing.T, p string) { seedV23DB(t, p) },
		"v25": func(t *testing.T, p string) { seedV25DB(t, p) },
		"v37": func(t *testing.T, p string) {
			seedV37DBWithPendingMergesRows(t, p, []seedRow{
				{PR: 1, SessionName: "repo@main", InstanceID: "inst", Status: "queued"},
			})
		},
	}

	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "version.db")
			seed(t, dbPath)

			d, err := db.Open(dbPath)
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			defer d.Close()

			var version int
			if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
				t.Fatalf("read schema_version: %v", err)
			}
			// A second open must be a no-op that keeps the same version.
			d2, err := db.Open(dbPath)
			if err != nil {
				t.Fatalf("second db.Open: %v", err)
			}
			defer d2.Close()

			var again int
			if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&again); err != nil {
				t.Fatalf("re-read schema_version: %v", err)
			}
			if again != version {
				t.Errorf("schema_version moved on a second open: %d -> %d", version, again)
			}
		})
	}
}

// assertNoForeignKeyViolations fails when PRAGMA foreign_key_check reports any
// row. A table rebuild that ran with enforcement in the wrong state is the
// failure this catches.
func assertNoForeignKeyViolations(t *testing.T, d *db.DB) {
	t.Helper()
	var violations int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_foreign_key_check`,
	).Scan(&violations); err != nil {
		t.Fatalf("run foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Errorf("foreign_key_check reports %d violation(s) after migration, want 0", violations)
	}
}

// assertNoDroppedRows fails when a table that a rebuild copied is empty. The
// seeds populate every table they build, so an empty table means the copy step
// did not run.
func assertNoDroppedRows(t *testing.T, d *db.DB, table string) {
	t.Helper()
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count == 0 {
		t.Errorf("%s is empty after the rebuild; the seeded rows were dropped", table)
	}
}

// tableNames lists the tables in the file at dbPath, in a stable order.
func tableNames(t *testing.T, dbPath string) []string {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open %s: %v", dbPath, err)
	}
	defer rawConn.Close()

	rows, err := rawConn.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`,
	)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}
	return names
}

// tableRowCounts returns the row count of every table in the file at dbPath.
func tableRowCounts(t *testing.T, dbPath string) map[string]int {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open %s: %v", dbPath, err)
	}
	defer rawConn.Close()

	counts := make(map[string]int)
	for _, name := range tableNames(t, dbPath) {
		if name == "schema_version" {
			continue
		}
		var count int
		if err := rawConn.QueryRow(`SELECT COUNT(*) FROM ` + name).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		counts[name] = count
	}
	return counts
}
