package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// openRWForSeed opens a writable handle on a fresh temp DB so tests can seed
// fixtures, then returns the path for the test to re-open in read-only mode.
func openRWForSeed(t *testing.T) (path string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "ro-test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return path, func() {}
}

func TestIsSingleStatement(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    bool
		wantErr bool
	}{
		{name: "single without semi", in: "SELECT 1", want: true},
		{name: "single with semi", in: "SELECT 1;", want: true},
		{name: "single with trailing whitespace", in: "  SELECT 1;\n  ", want: true},
		{name: "two statements", in: "SELECT 1; SELECT 2", want: false},
		{name: "two statements both terminated", in: "SELECT 1; SELECT 2;", want: false},
		{name: "semicolon inside single quote", in: "SELECT ';'", want: true},
		{name: "doubled apostrophe inside string", in: "SELECT 'it''s ok'", want: true},
		{name: "semicolon inside double-quoted ident", in: `SELECT "a;b" FROM t`, want: true},
		{name: "semicolon inside line comment", in: "SELECT 1 -- ; not a real statement\n", want: true},
		{name: "semicolon inside block comment", in: "SELECT 1 /* ; nope */", want: true},
		{name: "block comment then second stmt", in: "SELECT 1 /* ; */; SELECT 2", want: false},
		{name: "empty input", in: "", wantErr: true},
		{name: "whitespace only", in: "   \n\t  ", wantErr: true},
		{name: "comments only", in: "-- nothing here\n/* still nothing */", wantErr: true},
		{name: "unterminated string", in: "SELECT 'oops", wantErr: true},
		{name: "unterminated block comment", in: "SELECT 1 /* never ends", wantErr: true},
		{name: "trailing semicolons no extra stmt", in: "SELECT 1;;;", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := IsSingleStatement(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got ok=%v err=nil", ok)
				}
				return
			}
			if err != nil && tc.want {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("got ok=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestOpenReadOnly_RejectsWrites(t *testing.T) {
	path, _ := openRWForSeed(t)

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	// Any DML should fail with the read-only sentinel.
	_, err = conn.ExecContext(context.Background(),
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES ('x','x','x','x','pi',1)`)
	if err == nil {
		t.Fatal("expected error on INSERT against read-only handle")
	}
	if !IsReadOnlyError(err) {
		t.Fatalf("got %v, want read-only error", err)
	}
}

func TestRunQuery_EmptyResultEmitsEmptySlice(t *testing.T) {
	path, _ := openRWForSeed(t)

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	res, err := RunQuery(context.Background(), conn, "SELECT name FROM sqlite_master WHERE 1=0")
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if res.Rows == nil {
		t.Fatal("Rows must not be nil for empty result; got nil")
	}
	if len(res.Rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(res.Rows))
	}
	if len(res.Columns) != 1 || res.Columns[0] != "name" {
		t.Fatalf("Columns = %v", res.Columns)
	}
}

func TestRunQuery_BLOBAndNullPreserved(t *testing.T) {
	path, _ := openRWForSeed(t)

	// Seed a row with a BLOB and a NULL via the writable handle.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open writable: %v", err)
	}
	defer w.Close()
	if _, err := w.conn.Exec(`CREATE TABLE blobs (b BLOB, n INTEGER)`); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	if _, err := w.conn.Exec(`INSERT INTO blobs (b, n) VALUES (X'cafebabe', NULL)`); err != nil {
		t.Fatalf("insert blob row: %v", err)
	}

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	res, err := RunQuery(context.Background(), conn, "SELECT b, n FROM blobs")
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	bs, ok := row[0].([]byte)
	if !ok {
		t.Fatalf("col 0 type = %T, want []byte", row[0])
	}
	if string(bs) != "\xca\xfe\xba\xbe" {
		t.Fatalf("blob bytes = % x, want ca fe ba be", bs)
	}
	if row[1] != nil {
		t.Fatalf("col 1 = %#v, want nil for NULL", row[1])
	}
}

func TestTables_ExcludesSqliteInternal(t *testing.T) {
	path, _ := openRWForSeed(t)

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	names, err := Tables(context.Background(), conn)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "sqlite_") {
			t.Fatalf("Tables returned internal table %q", n)
		}
	}
	// Spot-check a couple of well-known prism tables.
	got := make(map[string]bool, len(names))
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"agent_events", "agent_status", "sessions"} {
		if !got[want] {
			t.Errorf("Tables missing expected table %q (got %v)", want, names)
		}
	}
	// Sorted check.
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Tables not sorted: %v", names)
		}
	}
}

func TestSchema_DeterministicOrderAndFilter(t *testing.T) {
	path, _ := openRWForSeed(t)

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	// Full schema.
	all, err := Schema(context.Background(), conn, "")
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("Schema returned 0 entries; want at least the prism schema")
	}
	// Tables before indexes.
	seenIndex := false
	for _, e := range all {
		if e.Type == "index" {
			seenIndex = true
		}
		if seenIndex && e.Type == "table" {
			t.Fatalf("Schema interleaves: table after index")
		}
		if strings.HasPrefix(e.Name, "sqlite_") {
			t.Fatalf("Schema includes internal %q", e.Name)
		}
	}

	// Determinism: a second call must return identical entries.
	all2, err := Schema(context.Background(), conn, "")
	if err != nil {
		t.Fatalf("Schema 2: %v", err)
	}
	if len(all2) != len(all) {
		t.Fatalf("Schema length changed across calls: %d vs %d", len(all), len(all2))
	}
	for i := range all {
		if all[i] != all2[i] {
			t.Fatalf("Schema entry %d differs across calls:\n  %+v\n  %+v", i, all[i], all2[i])
		}
	}

	// Filter to a single table.
	one, err := Schema(context.Background(), conn, "agent_events")
	if err != nil {
		t.Fatalf("Schema(agent_events): %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("Schema(agent_events) returned %d entries, want 1", len(one))
	}
	if one[0].Name != "agent_events" || one[0].Type != "table" {
		t.Fatalf("got %+v, want type=table name=agent_events", one[0])
	}
	if !strings.Contains(one[0].SQL, "CREATE TABLE") {
		t.Fatalf("DDL did not contain CREATE TABLE: %q", one[0].SQL)
	}
}

func TestRunQuery_TimeoutCancels(t *testing.T) {
	path, _ := openRWForSeed(t)

	conn, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer conn.Close()

	// A recursive CTE without a stop condition runs effectively forever.
	// With a tight context deadline the driver should cancel via
	// sqlite3_interrupt and surface a context-related error.
	ctx, cancel := context.WithTimeout(context.Background(), 50*1000*1000) // 50ms
	defer cancel()
	_, err = RunQuery(ctx, conn,
		"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r) SELECT n FROM r")
	if err == nil {
		t.Fatal("expected context-cancellation error, got nil")
	}
}
