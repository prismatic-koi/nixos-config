package main

// db_test.go — unit + integration tests for the `iris db` family
// (`iris db tables / schema / query`).
//
// All tests use iristest.NewIsolated to redirect HOME / XDG_STATE_HOME
// to a per-test tempdir so the suite is safe in CI's nix sandbox
// ($HOME=/homeless-shelter) and never touches the developer's real
// ~/.local/state/iris.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// withIsolatedDB wires the iris-test iso onto the package-level testDBPath
// hook so RunE-driven cobra invocations see the per-test iris DB. The
// returned cleanup restores the hook to "" for the next test.
func withIsolatedDB(t *testing.T, iso *iristest.Isolated) {
	t.Helper()
	SetTestDBPath(iso.Paths.DB)
	t.Cleanup(func() { SetTestDBPath("") })
}

// seedDBSession inserts a single sessions row so `iris db tables` etc.
// have real data to render. Returns the inserted session_name.
func seedDBSession(t *testing.T, iso *iristest.Isolated, suffix string) string {
	t.Helper()
	name := iristest.SessionName(suffix)
	role := "worker"
	worktree := iso.Root + "/worktree"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:  uuid.NewString(),
		SessionName: name,
		AgentRole:   &role,
		Repo:        "iris-test",
		Worktree:    worktree,
		Harness:     "pi",
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seedDBSession: insert: %v", err)
	}
	return name
}

// runCommand executes a fresh cobra command tree under a captured stdout
// buffer. Returns the captured stdout and the cobra error (if any). The
// global rootCmd is reused — we reset args and reparent stdout per call.
//
// Cobra retains *flag* state on the underlying *pflag.FlagSet across
// Execute() calls (the flag library does not clear values when args are
// re-parsed). To avoid leakage between test invocations we walk every
// flag on dbCmd and its registered children and restore each to its
// default value before invoking the command.
func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetCobraFlags(dbCmd)
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		// Restore stdin/stdout/stderr and args to defaults so a
		// subsequent test starts clean.
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		rootCmd.SetIn(nil)
		resetCobraFlags(dbCmd)
	})
	err := rootCmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// resetCobraFlags walks every flag on c and its subcommands and restores
// each to its DefValue. Required because cobra/pflag retains the last
// parsed value across Execute() calls when the same *Command instance
// is reused (as it is here, via the package-global rootCmd / dbCmd).
func resetCobraFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	for _, child := range c.Commands() {
		resetCobraFlags(child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris db tables
// ─────────────────────────────────────────────────────────────────────────────

// TestDBTables_ListsTables asserts that `iris db tables` prints at least
// the sessions and agent_events tables (created by db.Open() migrations)
// and excludes sqlite_* internals.
func TestDBTables_ListsTables(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "tables")
	if err != nil {
		t.Fatalf("iris db tables: %v", err)
	}

	want := []string{"sessions"}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "sqlite_master") || strings.Contains(out, "sqlite_sequence") {
		t.Errorf("output should exclude sqlite_* internals:\n%s", out)
	}
	// One name per line, sorted.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("tables not sorted: %q before %q", lines[i-1], lines[i])
		}
	}
}

// TestDBTables_JSON asserts that `iris db tables --json` emits a JSON
// array of strings.
func TestDBTables_JSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "tables", "--json")
	if err != nil {
		t.Fatalf("iris db tables --json: %v", err)
	}

	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		t.Fatalf("expected JSON array, got: %s", out)
	}
	if len(names) == 0 {
		t.Fatalf("expected at least one table in JSON output, got 0")
	}
	found := false
	for _, n := range names {
		if n == "sessions" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'sessions' in JSON table list: %v", names)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris db schema
// ─────────────────────────────────────────────────────────────────────────────

// TestDBSchema_NamedTable asserts that `iris db schema sessions` prints
// the CREATE TABLE statement for sessions and nothing else.
func TestDBSchema_NamedTable(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "schema", "sessions")
	if err != nil {
		t.Fatalf("iris db schema sessions: %v", err)
	}
	if !strings.Contains(strings.ToUpper(out), "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE in output:\n%s", out)
	}
	if !strings.Contains(out, "sessions") {
		t.Errorf("expected 'sessions' in output:\n%s", out)
	}
	// Should end with semicolon (paste-able into sqlite3).
	trimmed := strings.TrimSpace(out)
	if !strings.HasSuffix(trimmed, ";") {
		t.Errorf("expected trailing semicolon, got:\n%s", out)
	}
}

// TestDBSchema_All asserts that `iris db schema --all` prints DDL for
// multiple tables / indexes.
func TestDBSchema_All(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "schema", "--all")
	if err != nil {
		t.Fatalf("iris db schema --all: %v", err)
	}
	// At least two CREATE statements (sessions + agent_events at minimum).
	upper := strings.ToUpper(out)
	if got := strings.Count(upper, "CREATE TABLE"); got < 2 {
		t.Errorf("expected ≥2 CREATE TABLE statements with --all, got %d:\n%s", got, out)
	}
}

// TestDBSchema_RequiresArgOrAll asserts that running `iris db schema`
// with neither a table arg nor --all is a usage error.
func TestDBSchema_RequiresArgOrAll(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runCommand(t, "db", "schema")
	if err == nil {
		t.Fatalf("expected error for bare `iris db schema`, got nil")
	}
	if !strings.Contains(err.Error(), "table") && !strings.Contains(err.Error(), "--all") {
		t.Errorf("expected helpful 'table or --all' error, got: %v", err)
	}
}

// TestDBSchema_TableAndAllMutuallyExclusive asserts that passing both a
// table arg and --all is a usage error.
func TestDBSchema_TableAndAllMutuallyExclusive(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runCommand(t, "db", "schema", "sessions", "--all")
	if err == nil {
		t.Fatalf("expected error for `iris db schema sessions --all`, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got: %v", err)
	}
}

// TestDBSchema_NoSuchTable asserts that an unknown table name returns a
// clear "not found" error.
func TestDBSchema_NoSuchTable(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runCommand(t, "db", "schema", "this_table_does_not_exist")
	if err == nil {
		t.Fatalf("expected error for unknown table, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

// TestDBSchema_JSON asserts that `--json` emits an object keyed by name.
func TestDBSchema_JSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "schema", "sessions", "--json")
	if err != nil {
		t.Fatalf("iris db schema sessions --json: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("expected JSON object, got: %s", out)
	}
	ddl, ok := m["sessions"]
	if !ok {
		t.Fatalf("expected 'sessions' key in JSON: %v", m)
	}
	if !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE in sessions DDL: %q", ddl)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris db query — happy paths
// ─────────────────────────────────────────────────────────────────────────────

// TestDBQuery_BasicSelect runs a simple SELECT against a seeded sessions
// row and asserts the human-readable table contains the session name.
func TestDBQuery_BasicSelect(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)
	name := seedDBSession(t, iso, "basic")

	out, err := runCommand(t, "db", "query", "SELECT session_name FROM sessions")
	if err != nil {
		t.Fatalf("iris db query: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("expected session_name %q in output:\n%s", name, out)
	}
	if !strings.Contains(out, "1 row") {
		t.Errorf("expected '1 row' footer in output:\n%s", out)
	}
}

// TestDBQuery_JSON asserts the JSON wire shape matches prism's:
// {"columns": [...], "rows": [...], "elapsedMs": N}.
func TestDBQuery_JSON(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)
	seedDBSession(t, iso, "json-shape")

	out, err := runCommand(t, "db", "query", "--json", "SELECT session_name, repo FROM sessions")
	if err != nil {
		t.Fatalf("iris db query --json: %v", err)
	}

	var got struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		ElapsedMs int64    `json:"elapsedMs"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expected JSON, got %q: %v", out, err)
	}
	wantCols := []string{"session_name", "repo"}
	if fmt.Sprint(got.Columns) != fmt.Sprint(wantCols) {
		t.Errorf("columns mismatch: got %v want %v", got.Columns, wantCols)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got.Rows))
	}
	if got.Rows[0][1] != "iris-test" {
		t.Errorf("expected repo='iris-test' at rows[0][1], got %v", got.Rows[0][1])
	}
}

// TestDBQuery_JSON_EmptyResult asserts that an empty result set serialises
// as "rows": [] (not null) — matches prism's wire-shape guarantee.
func TestDBQuery_JSON_EmptyResult(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "query", "--json", "SELECT session_name FROM sessions WHERE 1 = 0")
	if err != nil {
		t.Fatalf("iris db query --json: %v", err)
	}
	if !strings.Contains(out, `"rows":[]`) {
		t.Errorf("expected \"rows\":[] in empty-result JSON, got: %s", out)
	}
}

// TestDBQuery_JSON_NullCoercion asserts NULL columns serialise as JSON
// null (matches prism byte-for-byte).
func TestDBQuery_JSON_NullCoercion(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	out, err := runCommand(t, "db", "query", "--json", "SELECT NULL AS x")
	if err != nil {
		t.Fatalf("iris db query --json: %v", err)
	}
	if !strings.Contains(out, `"rows":[[null]]`) {
		t.Errorf("expected \"rows\":[[null]] in NULL output, got: %s", out)
	}
}

// TestDBQuery_Stdin asserts that `iris db query -` reads from stdin.
func TestDBQuery_Stdin(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)
	name := seedDBSession(t, iso, "stdin")

	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	rootCmd.SetIn(strings.NewReader("SELECT session_name FROM sessions"))
	rootCmd.SetArgs([]string{"db", "query", "-"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetIn(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("iris db query -: %v", err)
	}
	if !strings.Contains(stdout.String(), name) {
		t.Errorf("expected %q in stdin-fed output:\n%s", name, stdout.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris db query — security: write-keyword rejection BEFORE opening the DB
// ─────────────────────────────────────────────────────────────────────────────

// TestDBQuery_RejectsWriteStatements walks every keyword the AC names
// explicitly (INSERT / UPDATE / DELETE / DROP / CREATE / ALTER) plus a
// few extras (REPLACE / PRAGMA / ATTACH / DETACH / BEGIN / COMMIT /
// ROLLBACK / VACUUM / REINDEX / SAVEPOINT / RELEASE). Each should be
// rejected non-zero with a "read-only" / write-keyword error.
//
// Per the AC, the rejection happens BEFORE the DB is opened. We assert
// this indirectly by pointing testDBPath at a path that does NOT exist —
// if the pre-check fired, we never see "iris database not found". If
// only the open-time check fired, we would.
func TestDBQuery_RejectsWriteStatements(t *testing.T) {
	iso := iristest.NewIsolated(t)
	// Deliberately point at a non-existent DB to prove the pre-check
	// fires BEFORE the DB is opened. (NewIsolated has created a real DB
	// under iso.Paths.DB, but we override here.)
	bogusDB := filepath.Join(iso.Root, "definitely-does-not-exist.db")
	SetTestDBPath(bogusDB)
	t.Cleanup(func() { SetTestDBPath("") })

	cases := []struct {
		name string
		sql  string
	}{
		{"insert", "INSERT INTO sessions (session_name) VALUES ('x')"},
		{"update", "UPDATE sessions SET worktree='y' WHERE session_name='x'"},
		{"delete", "DELETE FROM sessions WHERE session_name='x'"},
		{"drop", "DROP TABLE sessions"},
		{"create", "CREATE TABLE foo (a INT)"},
		{"alter", "ALTER TABLE sessions ADD COLUMN q TEXT"},
		{"replace", "REPLACE INTO sessions (session_name) VALUES ('x')"},
		{"pragma", "PRAGMA journal_mode = DELETE"},
		{"attach", "ATTACH DATABASE '/tmp/x.db' AS other"},
		{"detach", "DETACH DATABASE other"},
		{"begin", "BEGIN TRANSACTION"},
		{"commit", "COMMIT"},
		{"rollback", "ROLLBACK"},
		{"vacuum", "VACUUM"},
		{"reindex", "REINDEX sessions"},
		{"savepoint", "SAVEPOINT sp"},
		{"release", "RELEASE sp"},
		// Case insensitivity.
		{"lowercase-insert", "insert into sessions (session_name) values ('x')"},
		// Leading whitespace.
		{"leading-ws", "   \n  UPDATE sessions SET worktree='y'"},
		// Leading block comment.
		{"block-comment", "/* evil */ DROP TABLE sessions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use the cobra `--` separator so SQL that begins with "--"
			// (a line comment) is not mis-parsed as a flag.
			out, err := runCommand(t, "db", "query", "--", tc.sql)
			if err == nil {
				t.Fatalf("expected error for %q, got nil (out=%s)", tc.sql, out)
			}
			msg := err.Error()
			if !strings.Contains(msg, "read-only") {
				t.Errorf("expected 'read-only' in error for %q, got: %v", tc.sql, err)
			}
			// Pre-check signal: we should NOT see the "iris database not
			// found" message, because the rejection happens before the DB
			// is opened.
			if strings.Contains(msg, "iris database not found") {
				t.Errorf("write-rejection should fire BEFORE DB-open; got 'not found' for %q: %v", tc.sql, err)
			}
		})
	}

	// Separate sub-test for the leading-line-comment case. Cobra treats
	// a positional starting with "--" as a flag, so we feed this case
	// via stdin instead — the SQL still hits isWriteStatement before
	// any DB open.
	t.Run("line-comment", func(t *testing.T) {
		var stdout bytes.Buffer
		rootCmd.SetOut(&stdout)
		rootCmd.SetErr(&stdout)
		rootCmd.SetIn(strings.NewReader("-- evil\nDELETE FROM sessions"))
		rootCmd.SetArgs([]string{"db", "query", "-"})
		t.Cleanup(func() {
			rootCmd.SetOut(nil)
			rootCmd.SetErr(nil)
			rootCmd.SetIn(nil)
			rootCmd.SetArgs(nil)
		})
		err := rootCmd.ExecuteContext(context.Background())
		if err == nil {
			t.Fatalf("expected error for line-comment write, got nil")
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("expected 'read-only' in error, got: %v", err)
		}
	})
}

// TestDBQuery_AllowsSelect_AfterWriteKeywordCheck is a defensive
// counter-test: SELECT must not trigger the write-keyword check.
func TestDBQuery_AllowsSelect_AfterWriteKeywordCheck(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	if _, err := runCommand(t, "db", "query", "SELECT 1"); err != nil {
		t.Fatalf("plain SELECT was rejected: %v", err)
	}
	// Common SELECT forms with comments / WITH must also pass.
	if _, err := runCommand(t, "db", "query", "--", "-- this is a SELECT\nSELECT 1"); err != nil {
		t.Fatalf("SELECT with leading line comment was rejected: %v", err)
	}
	if _, err := runCommand(t, "db", "query", "/* preface */ SELECT 1"); err != nil {
		t.Fatalf("SELECT with leading block comment was rejected: %v", err)
	}
	if _, err := runCommand(t, "db", "query", "WITH cte AS (SELECT 1) SELECT * FROM cte"); err != nil {
		t.Fatalf("WITH-style SELECT was rejected: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iris db query — error paths
// ─────────────────────────────────────────────────────────────────────────────

// TestDBQuery_MissingDB asserts that pointing the CLI at a non-existent
// DB returns a clear "not found" error (the operator should be pointed
// at the daemon and the expected path).
func TestDBQuery_MissingDB(t *testing.T) {
	iso := iristest.NewIsolated(t)
	bogus := filepath.Join(iso.Root, "no-such-db.sqlite")
	SetTestDBPath(bogus)
	t.Cleanup(func() { SetTestDBPath("") })

	_, err := runCommand(t, "db", "query", "SELECT 1")
	if err == nil {
		t.Fatalf("expected error for missing DB, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("expected DB path %q in error, got: %v", bogus, err)
	}
}

// TestDBTables_MissingDB and TestDBSchema_MissingDB confirm the same
// behaviour for the other subcommands.
func TestDBTables_MissingDB(t *testing.T) {
	iso := iristest.NewIsolated(t)
	bogus := filepath.Join(iso.Root, "no-such-db.sqlite")
	SetTestDBPath(bogus)
	t.Cleanup(func() { SetTestDBPath("") })

	_, err := runCommand(t, "db", "tables")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error for missing DB, got: %v", err)
	}
}

func TestDBSchema_MissingDB(t *testing.T) {
	iso := iristest.NewIsolated(t)
	bogus := filepath.Join(iso.Root, "no-such-db.sqlite")
	SetTestDBPath(bogus)
	t.Cleanup(func() { SetTestDBPath("") })

	_, err := runCommand(t, "db", "schema", "--all")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error for missing DB, got: %v", err)
	}
}

// TestDBQuery_InvalidSQL asserts that a syntactically invalid SQL
// statement is rejected with the SQLite error message (no stack trace).
func TestDBQuery_InvalidSQL(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runCommand(t, "db", "query", "SELECT * FROM not_a_real_table_xxx")
	if err == nil {
		t.Fatalf("expected error for invalid SQL, got nil")
	}
	msg := err.Error()
	// SQLite emits "no such table" for unknown table refs — surface it.
	if !strings.Contains(msg, "no such table") {
		t.Errorf("expected SQLite 'no such table' error, got: %v", err)
	}
	// No goroutine / stack-trace markers.
	if strings.Contains(msg, "goroutine ") || strings.Contains(msg, ".go:") {
		t.Errorf("error should not contain a stack trace: %v", err)
	}
}

// TestDBQuery_MultiStatement_Rejected asserts that two statements
// separated by a semicolon are rejected at the CLI layer.
func TestDBQuery_MultiStatement_Rejected(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	_, err := runCommand(t, "db", "query", "SELECT 1; SELECT 2")
	if err == nil {
		t.Fatalf("expected error for multi-statement input, got nil")
	}
	if !strings.Contains(err.Error(), "one SQL statement") {
		t.Errorf("expected 'one SQL statement' in error, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Concurrent reader vs. iris daemon writer (WAL coexistence)
// ─────────────────────────────────────────────────────────────────────────────

// TestDBQuery_ConcurrentWithDaemonWrites simulates the AC's daemon-running
// scenario: a writer connection (the "daemon") performs N session
// inserts while a reader (`iris db query` semantics, via db.OpenReadOnly)
// repeatedly counts rows. Neither side should error; the read connection
// must NOT block writes (SQLite WAL mode is set by iris.OpenDB).
func TestDBQuery_ConcurrentWithDaemonWrites(t *testing.T) {
	iso := iristest.NewIsolated(t)
	withIsolatedDB(t, iso)

	// Verify WAL is set — without it, the AC's "does not interfere"
	// guarantee does not hold. iris.OpenDB sets WAL during migrations.
	var journalMode string
	if err := iso.DB.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("expected WAL journal mode, got %q (concurrent-reader AC requires WAL)", journalMode)
	}

	const writerIters = 30
	const readerIters = 60

	var wg sync.WaitGroup
	writerErr := make(chan error, 1)
	readerErr := make(chan error, 1)

	// Writer: simulates the daemon by inserting sessions through the
	// existing writable handle (iso.DB).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writerIters; i++ {
			role := "worker"
			if err := iso.DB.InsertSession(db.Session{
				InstanceID:  uuid.NewString(),
				SessionName: fmt.Sprintf("%s-%d", iristest.SessionName("concurrent"), i),
				AgentRole:   &role,
				Repo:        "iris-test",
				Worktree:    fmt.Sprintf("%s/wt-%d", iso.Root, i),
				Harness:     "pi",
				StartedAt:   time.Now().UTC(),
			}); err != nil {
				writerErr <- fmt.Errorf("writer insert %d: %w", i, err)
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
		writerErr <- nil
	}()

	// Reader: opens a fresh ?mode=ro handle per iteration, mirroring how
	// each `iris db query` invocation behaves.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < readerIters; i++ {
			conn, err := db.OpenReadOnly(iso.Paths.DB)
			if err != nil {
				readerErr <- fmt.Errorf("reader open %d: %w", i, err)
				return
			}
			res, err := db.RunQuery(context.Background(), conn, "SELECT COUNT(*) FROM sessions")
			conn.Close()
			if err != nil {
				readerErr <- fmt.Errorf("reader query %d: %w", i, err)
				return
			}
			if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
				readerErr <- fmt.Errorf("reader query %d: malformed row shape: %v", i, res.Rows)
				return
			}
			time.Sleep(500 * time.Microsecond)
		}
		readerErr <- nil
	}()

	wg.Wait()
	if err := <-writerErr; err != nil {
		t.Fatalf("writer failed: %v", err)
	}
	if err := <-readerErr; err != nil {
		t.Fatalf("reader failed: %v", err)
	}

	// Final assertion: all inserts landed.
	var finalCount int64
	if err := iso.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&finalCount); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if finalCount < writerIters {
		t.Errorf("expected ≥%d sessions after concurrent test, got %d", writerIters, finalCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit-level coverage of the write-keyword check
// ─────────────────────────────────────────────────────────────────────────────

// TestIsWriteStatement_Table is a direct unit test of isWriteStatement.
// It covers the table-driven cases that the higher-level
// TestDBQuery_RejectsWriteStatements asserts behaviour for, but at the
// per-function level so a regression in the walker is caught even if the
// cobra wiring is otherwise broken.
func TestIsWriteStatement_Table(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		wantKw  string
		wantHit bool
	}{
		{"plain insert", "INSERT INTO x VALUES (1)", "INSERT", true},
		{"lowercase update", "update x set a=1", "UPDATE", true},
		{"leading ws", "   \n\t DELETE FROM x", "DELETE", true},
		{"line comment first", "-- explain\nDROP TABLE x", "DROP", true},
		{"block comment first", "/* hi */ ALTER TABLE x ADD COLUMN q INT", "ALTER", true},
		{"nested comments not supported but pass through", "/* outer */ CREATE TABLE x (a INT)", "CREATE", true},
		{"select passes", "SELECT 1", "", false},
		{"with-cte passes", "WITH cte AS (SELECT 1) SELECT * FROM cte", "", false},
		{"explain passes", "EXPLAIN SELECT 1", "", false},
		{"empty input", "", "", false},
		{"only comments", "-- nothing here", "", false},
		// Common write verbs.
		{"replace", "REPLACE INTO x VALUES (1)", "REPLACE", true},
		{"pragma", "PRAGMA journal_mode", "PRAGMA", true},
		{"attach", "ATTACH 'x.db' AS y", "ATTACH", true},
		{"detach", "DETACH y", "DETACH", true},
		{"begin", "BEGIN", "BEGIN", true},
		{"commit", "COMMIT", "COMMIT", true},
		{"end (commit alias)", "END", "END", true},
		{"rollback", "ROLLBACK", "ROLLBACK", true},
		{"vacuum", "VACUUM", "VACUUM", true},
		{"reindex", "REINDEX", "REINDEX", true},
		{"savepoint", "SAVEPOINT sp", "SAVEPOINT", true},
		{"release", "RELEASE sp", "RELEASE", true},
		{"analyze", "ANALYZE", "ANALYZE", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := isWriteStatement(tc.sql)
			if hit != tc.wantHit {
				t.Errorf("hit mismatch: got %v want %v (kw=%q)", hit, tc.wantHit, got)
			}
			if hit && got != tc.wantKw {
				t.Errorf("kw mismatch: got %q want %q", got, tc.wantKw)
			}
		})
	}
}

// TestResolveDBPath_UsesIrisPaths is a small smoke test that the
// dbPath() default resolution lands under XDG_STATE_HOME. iristest
// pre-sets that env var so we just call ResolvePaths() and compare.
func TestResolveDBPath_UsesIrisPaths(t *testing.T) {
	iso := iristest.NewIsolated(t)
	SetTestDBPath("") // ensure default branch
	t.Cleanup(func() { SetTestDBPath("") })

	want := iris.ResolvePaths().DB
	if got := resolveDBPath(); got != want {
		t.Errorf("resolveDBPath default mismatch: got %q want %q", got, want)
	}
	// Defensive: env var should agree.
	if !strings.HasPrefix(want, os.Getenv("XDG_STATE_HOME")) {
		t.Errorf("iris DB path %q not under XDG_STATE_HOME=%q", want, os.Getenv("XDG_STATE_HOME"))
	}
	_ = iso
}
