package db

// open_batching_test.go — tests for the batched open sequence (issue #2612).
//
// Before #2612, db.Open ran the declarative schema, the schema_version seed,
// all 38 migrations, and the post-migration index block statement by statement
// in autocommit. Under journal_mode=WAL with synchronous=FULL every one of those
// commits fsyncs the WAL, which cost 73 fsyncs on a fresh file.
//
// Since #2612 the open sequence runs inside one transaction when batchableOpen
// says that is safe. It is not safe when a table-rebuild migration still has
// work to do: those four toggle PRAGMA foreign_keys, which SQLite silently
// ignores inside a transaction, and they open their own transaction, which
// cannot nest. The tests below pin three things:
//
//  1. batchableOpen classifies each database shape correctly.
//  2. A rebuild migration handed a transaction fails loudly instead of
//     rebuilding a table with foreign-key enforcement still on.
//  3. The set of migrations that rebuild a table is exactly the set
//     batchableOpen knows about, so a future rebuild migration cannot be
//     added without updating the probe.

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// rebuildMigrations names every migration that must run in autocommit. Keep
// this in step with the version checks in batchableOpen: each entry needs a
// clause there that refuses the batched path while the migration still has
// work to do.
var rebuildMigrations = []string{
	"migrateV8toV9",
	"migrateV23toV24",
	"migrateV25toV26",
	"migrateV37ToV38",
}

// openRawForTest opens a database with the sqlite driver and no prism schema.
func openRawForTest(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestBatchableOpen_Classification drives batchableOpen over the database
// shapes that decide the batched-versus-autocommit choice.
func TestBatchableOpen_Classification(t *testing.T) {
	tests := []struct {
		name string
		// setup runs against a brand-new database file. An empty setup
		// leaves the file empty, which is the fresh-open case.
		setup string
		want  bool
	}{
		{
			name:  "empty file takes the batched path",
			setup: "",
			want:  true,
		},
		{
			name:  "schema_version table present but empty behaves as fresh",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);`,
			want:  true,
		},
		{
			name: "current version has no pending migration at all",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (39);`,
			want: true,
		},
		{
			name: "v8 reaches the agent_status rebuild in v8 to v9",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (8);`,
			want: false,
		},
		{
			name: "v1 also reaches the v8 to v9 rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (1);`,
			want: false,
		},
		{
			name: "v9 has passed the v8 to v9 rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (9);`,
			want: true,
		},
		{
			name: "v23 with sessions.outcome_summary reaches the sessions rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (23);
			        CREATE TABLE sessions (instance_id TEXT PRIMARY KEY, outcome_summary TEXT);`,
			want: false,
		},
		{
			name: "v23 without sessions.outcome_summary skips the sessions rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (23);
			        CREATE TABLE sessions (instance_id TEXT PRIMARY KEY);`,
			want: true,
		},
		{
			name: "v24 never reaches the v23 to v24 rebuild even with the column",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (24);
			        CREATE TABLE sessions (instance_id TEXT PRIMARY KEY, outcome_summary TEXT);`,
			want: true,
		},
		{
			name: "v25 with agent_status.host_mode reaches the agent_status rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (25);
			        CREATE TABLE agent_status (session_name TEXT PRIMARY KEY, host_mode INTEGER NOT NULL DEFAULT 0);`,
			want: false,
		},
		{
			name: "v25 without agent_status.host_mode skips the agent_status rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (25);
			        CREATE TABLE agent_status (session_name TEXT PRIMARY KEY);`,
			want: true,
		},
		{
			name: "v37 with a repo-less pending_merges reaches the pending_merges rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (37);
			        CREATE TABLE pending_merges (pr INTEGER PRIMARY KEY, session_name TEXT NOT NULL);`,
			want: false,
		},
		{
			name: "v37 with a repo-bearing pending_merges skips the rebuild",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (37);
			        CREATE TABLE pending_merges (repo TEXT NOT NULL, pr INTEGER NOT NULL, PRIMARY KEY (repo, pr));`,
			want: true,
		},
		{
			name: "v37 with no pending_merges yet is safe: the schema block creates the new shape",
			setup: `CREATE TABLE schema_version (version INTEGER NOT NULL);
			        INSERT INTO schema_version (version) VALUES (37);`,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.db")
			conn := openRawForTest(t, path)
			if tc.setup != "" {
				if _, err := conn.Exec(tc.setup); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			if got := batchableOpen(conn); got != tc.want {
				t.Errorf("batchableOpen = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRebuildMigrations_RefuseTransaction is the enforcement behind the
// promise batchableOpen makes. Each rebuild migration receives a *sql.Tx with
// its rebuild branch reachable, and each must refuse.
//
// Without the refusal the failure would be silent: PRAGMA foreign_keys = OFF
// does nothing inside a transaction, so the rebuild would drop and recreate a
// table with foreign-key enforcement still on. That raises no error on an
// empty database, so a suite built from fresh files would stay green and the
// break would appear only on a populated database.
func TestRebuildMigrations_RefuseTransaction(t *testing.T) {
	tests := []struct {
		name string
		// setup creates the table shape that makes the rebuild branch
		// reachable, inside the transaction.
		setup   string
		version int
		run     func(e sqlExecutor, version *int) error
	}{
		{
			name:    "v8 to v9 rebuilds agent_status",
			setup:   `CREATE TABLE agent_status (session_name TEXT PRIMARY KEY);`,
			version: 8,
			run:     migrateV8toV9,
		},
		{
			name:    "v23 to v24 rebuilds sessions",
			setup:   `CREATE TABLE sessions (instance_id TEXT PRIMARY KEY, outcome_summary TEXT);`,
			version: 23,
			run:     migrateV23toV24,
		},
		{
			name:    "v25 to v26 rebuilds agent_status",
			setup:   `CREATE TABLE agent_status (session_name TEXT PRIMARY KEY, host_mode INTEGER NOT NULL DEFAULT 0);`,
			version: 25,
			run:     migrateV25toV26,
		},
		{
			name:    "v37 to v38 rebuilds pending_merges",
			setup:   `CREATE TABLE pending_merges (pr INTEGER PRIMARY KEY, session_name TEXT NOT NULL);`,
			version: 37,
			run:     migrateV37ToV38,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "refuse.db")
			conn := openRawForTest(t, path)
			if _, err := conn.Exec(tc.setup); err != nil {
				t.Fatalf("setup: %v", err)
			}

			tx, err := conn.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback() //nolint:errcheck

			version := tc.version
			err = tc.run(tx, &version)
			if err == nil {
				t.Fatal("migration accepted a transaction; it must refuse and run in autocommit")
			}
			if !strings.Contains(err.Error(), "must run in autocommit") {
				t.Errorf("error %q does not explain the autocommit requirement", err)
			}
			if version != tc.version {
				t.Errorf("schema version advanced to %d despite the refusal; want %d", version, tc.version)
			}

			// Control: the same call against the raw connection must get past
			// the guard. The minimal table shapes above do not carry every
			// column the rebuild copies, so the call still fails — but on the
			// SQL, not on the guard. That is what proves the refusal above is
			// about the transaction and not about the table shape.
			//
			// The matching positive control on real data is
			// TestOpen_PopulatedRebuildMigrations_PreserveRowsAndFKState in
			// open_batching_ext_test.go, which drives each rebuild through
			// db.Open over a populated database.
			version = tc.version
			if err := tc.run(conn, &version); err != nil &&
				strings.Contains(err.Error(), "must run in autocommit") {
				t.Errorf("migration refused the raw connection as well: %v; "+
					"the guard must reject a transaction only", err)
			}
		})
	}
}

// TestRebuildMigrationSet_MatchesProbe walks db.go and collects every
// migration whose body rebuilds a table, toggles PRAGMA foreign_keys, or opens
// its own transaction. That set must equal rebuildMigrations.
//
// This test is a heuristic: it detects rebuilds only in string literals inside
// the function body, not migrations that assemble their SQL from package-level
// constants, fmt.Sprintf calls, or concatenated literals. The real backstop is
// the errRebuildNeedsAutocommit executor assertion, which fails the open loudly
// if a rebuild runs inside a transaction rather than corrupting the database.
// This guard keeps the probe honest. A new migration that rebuilds a table
// would otherwise run inside the batched transaction with foreign-key
// enforcement silently still on, and no fresh-file test would notice.
func TestRebuildMigrationSet_MatchesProbe(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dbGoPath := filepath.Join(filepath.Dir(thisFile), "db.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, dbGoPath, nil, 0)
	if err != nil {
		t.Fatalf("parse db.go: %v", err)
	}

	// Markers are matched against string literals and call expressions in the
	// function body only. Comments are not part of the body AST, so prose that
	// merely mentions "DROP TABLE" does not produce a hit.
	sqlMarkers := []string{"PRAGMA FOREIGN_KEYS", "DROP TABLE", "RENAME TO"}

	var found, guarded []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "migrateV") || fn.Body == nil {
			continue
		}
		var rebuilds, refuses bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				upper := strings.ToUpper(node.Value)
				for _, marker := range sqlMarkers {
					if strings.Contains(upper, marker) {
						rebuilds = true
					}
				}
			case *ast.CallExpr:
				switch fun := node.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "Begin" {
						rebuilds = true
					}
				case *ast.Ident:
					if fun.Name == "errRebuildNeedsAutocommit" {
						refuses = true
					}
				}
			}
			return true
		})
		if rebuilds {
			found = append(found, fn.Name.Name)
		}
		if refuses {
			guarded = append(guarded, fn.Name.Name)
		}
	}

	want := append([]string(nil), rebuildMigrations...)
	sort.Strings(want)
	sort.Strings(found)
	sort.Strings(guarded)

	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Errorf(
			"migrations that rebuild a table = %v, want %v.\n"+
				"A new entry means the migration must stay in autocommit: add a clause to "+
				"batchableOpen that refuses the batched path while it has work to do, guard "+
				"its rebuild branch with errRebuildNeedsAutocommit, and add it to "+
				"rebuildMigrations.",
			found, want,
		)
	}
	if strings.Join(guarded, ",") != strings.Join(want, ",") {
		t.Errorf(
			"migrations guarded by errRebuildNeedsAutocommit = %v, want %v; "+
				"every rebuild migration must refuse a transaction",
			guarded, want,
		)
	}
}

// TestOpenSequence_BatchedAndAutocommitProduceIdenticalSchema builds two fresh
// databases, one through the batched transaction and one statement by
// statement in autocommit, and compares the result.
//
// The batched path is new; the autocommit path is what db.Open did before
// #2612. Byte-identical sqlite_master and schema_version output is the
// evidence that batching changed the cost of an open and nothing else.
func TestOpenSequence_BatchedAndAutocommitProduceIdenticalSchema(t *testing.T) {
	dir := t.TempDir()

	batchedConn := openRawForTest(t, filepath.Join(dir, "batched.db"))
	if err := runOpenSequenceBatched(batchedConn); err != nil {
		t.Fatalf("batched open sequence: %v", err)
	}

	var batchedVersion int
	if err := batchedConn.QueryRow("SELECT version FROM schema_version").Scan(&batchedVersion); err != nil {
		t.Fatalf("read batched schema_version: %v", err)
	}
	if batchedVersion < 38 {
		t.Errorf("batched database version %d, want >= 38", batchedVersion)
	}

	autocommitConn := openRawForTest(t, filepath.Join(dir, "autocommit.db"))
	if err := runOpenSequence(autocommitConn); err != nil {
		t.Fatalf("autocommit open sequence: %v", err)
	}

	batched := dumpSchemaForTest(t, batchedConn)
	autocommit := dumpSchemaForTest(t, autocommitConn)
	if batched != autocommit {
		t.Errorf("batched and autocommit open sequences disagree.\nbatched:\n%s\nautocommit:\n%s",
			batched, autocommit)
	}
}

// dumpSchemaForTest renders schema_version and the whole of sqlite_master as
// one stable, comparable string.
func dumpSchemaForTest(t *testing.T, conn *sql.DB) string {
	t.Helper()

	var version int
	if err := conn.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}

	rows, err := conn.Query(
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`,
	)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "schema_version=%d\n", version)
	for rows.Next() {
		var typ, name, tblName, stmt string
		if err := rows.Scan(&typ, &name, &tblName, &stmt); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", typ, name, tblName, stmt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return b.String()
}
