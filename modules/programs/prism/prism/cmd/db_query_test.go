package cmd

// Tests for prism db query rendering and the local execution path.
//
// These tests run against a fresh temp DB. Some seed a writable handle to
// insert fixtures (BLOB, NULL) before exercising the read-only command
// path.

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// unsetHostAPIForTest clears PRISM_HOST_API so the local code path is exercised
// rather than the proxy. Restored on test cleanup.
func unsetHostAPIForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
}

func newTestDBForCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cli-test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return path
}

func TestRenderDBQueryJSON_EmptyRowsIsEmptySlice(t *testing.T) {
	res := &db.QueryResult{
		Columns:   []string{"a", "b"},
		Rows:      nil,
		ElapsedMs: 7,
	}
	var buf bytes.Buffer
	if err := renderDBQueryJSON(&buf, res); err != nil {
		t.Fatalf("renderDBQueryJSON: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"rows":[]`) {
		t.Fatalf("expected rows:[]; got %s", got)
	}
	// Round-trip must be valid JSON.
	var dec struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		ElapsedMs int64    `json:"elapsedMs"`
	}
	if err := json.Unmarshal(buf.Bytes(), &dec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dec.Rows == nil {
		t.Fatal("Rows decoded as nil")
	}
	if len(dec.Rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(dec.Rows))
	}
}

func TestRenderDBQueryJSON_BlobBase64_NullJSONNull(t *testing.T) {
	res := &db.QueryResult{
		Columns: []string{"b", "n"},
		Rows: [][]any{
			{[]byte{0xca, 0xfe, 0xba, 0xbe}, nil},
		},
	}
	var buf bytes.Buffer
	if err := renderDBQueryJSON(&buf, res); err != nil {
		t.Fatalf("renderDBQueryJSON: %v", err)
	}
	var dec struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &dec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dec.Rows) != 1 || len(dec.Rows[0]) != 2 {
		t.Fatalf("unexpected shape %v", dec.Rows)
	}
	gotB, ok := dec.Rows[0][0].(string)
	if !ok {
		t.Fatalf("col 0 type %T, want string (base64)", dec.Rows[0][0])
	}
	if gotB != "yv66vg==" {
		t.Errorf("base64 = %q, want yv66vg==", gotB)
	}
	if dec.Rows[0][1] != nil {
		t.Errorf("col 1 = %v, want JSON null", dec.Rows[0][1])
	}
}

func TestFormatCellTable_NullAndBlob(t *testing.T) {
	if got := formatCellTable(nil); got != "NULL" {
		t.Errorf("nil → %q, want NULL", got)
	}
	if got := formatCellTable([]byte{0xca, 0xfe}); got != "0xcafe" {
		t.Errorf("blob → %q, want 0xcafe", got)
	}
	if got := formatCellTable(int64(42)); got != "42" {
		t.Errorf("int64 → %q, want 42", got)
	}
	if got := formatCellTable("hi"); got != "hi" {
		t.Errorf("string → %q, want hi", got)
	}
}

func TestRenderDBQueryTable_NullAndBlobAppearLiterally(t *testing.T) {
	res := &db.QueryResult{
		Columns: []string{"b", "n", "s"},
		Rows: [][]any{
			{[]byte{0xde, 0xad}, nil, "ok"},
		},
		ElapsedMs: 3,
	}
	var buf bytes.Buffer
	if err := renderDBQueryTable(&buf, res); err != nil {
		t.Fatalf("renderDBQueryTable: %v", err)
	}
	got := buf.String()
	// We only assert the literal substrings appear — colour codes from
	// lipgloss may surround "NULL", but the substring is still present.
	if !strings.Contains(got, "0xdead") {
		t.Errorf("output missing 0xdead: %q", got)
	}
	if !strings.Contains(got, "NULL") {
		t.Errorf("output missing NULL: %q", got)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("output missing ok: %q", got)
	}
	if !strings.Contains(got, "1 row") {
		t.Errorf("footer missing row count: %q", got)
	}
}

// TestRunDBQueryLocal_RejectsWrite verifies that the LOCAL (non-proxy) path
// surfaces the read-only sentinel as a clear error.
func TestRunDBQueryLocal_RejectsWrite(t *testing.T) {
	unsetHostAPIForTest(t)
	path := newTestDBForCLI(t)
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	// Invoke runDBQuery directly with a stub cobra Command.
	cmd := dbQueryCmd
	cmd.SetContext(context.Background())
	err := runDBQuery(cmd, []string{
		"INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES ('x','x','x','x','opencode',1)",
	})
	if err == nil {
		t.Fatal("expected error on write attempt, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read") {
		t.Fatalf("error %q does not mention read-only", err)
	}
}

func TestRunDBQuery_RejectsMultiStatement(t *testing.T) {
	unsetHostAPIForTest(t)
	path := newTestDBForCLI(t)
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	cmd := dbQueryCmd
	cmd.SetContext(context.Background())
	err := runDBQuery(cmd, []string{"SELECT 1; SELECT 2"})
	if err == nil {
		t.Fatal("expected error on multi-statement input")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("error %q does not mention single-statement", err)
	}
}

func TestRunDBQuery_HappyPath(t *testing.T) {
	unsetHostAPIForTest(t)
	path := newTestDBForCLI(t)
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	// Capture stdout via cobra's OutOrStdout/SetOut.
	var buf bytes.Buffer
	cmd := dbQueryCmd
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetOut(nil) })

	if err := runDBQuery(cmd, []string{"SELECT 1 AS one"}); err != nil {
		t.Fatalf("runDBQuery: %v", err)
	}
	if !strings.Contains(buf.String(), "one") {
		t.Errorf("output missing column header: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "1 row") {
		t.Errorf("output missing footer: %q", buf.String())
	}
}

func TestRunDBQuery_StdinDash(t *testing.T) {
	unsetHostAPIForTest(t)
	path := newTestDBForCLI(t)
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })

	var buf bytes.Buffer
	cmd := dbQueryCmd
	cmd.SetOut(&buf)
	cmd.SetIn(strings.NewReader("SELECT 42 AS answer\n"))
	cmd.SetContext(context.Background())
	t.Cleanup(func() {
		cmd.SetOut(nil)
		cmd.SetIn(nil)
	})

	if err := runDBQuery(cmd, []string{"-"}); err != nil {
		t.Fatalf("runDBQuery: %v", err)
	}
	if !strings.Contains(buf.String(), "42") {
		t.Errorf("output missing answer: %q", buf.String())
	}
}

// TestRenderDBSchema_TextHasSemicolon verifies the text renderer terminates
// each entry with a semicolon (paste-able into sqlite3).
func TestRenderDBSchema_TextHasSemicolon(t *testing.T) {
	entries := []db.SchemaEntry{
		{Type: "table", Name: "t", SQL: "CREATE TABLE t (x INTEGER)"},
		{Type: "index", Name: "i", SQL: "CREATE INDEX i ON t(x);"},
	}
	var buf bytes.Buffer
	if err := renderDBSchema(&buf, entries, false); err != nil {
		t.Fatalf("renderDBSchema: %v", err)
	}
	got := buf.String()
	if strings.Count(got, ";") < 2 {
		t.Errorf("expected ≥ 2 semicolons in output: %q", got)
	}
}

func TestRenderDBSchema_JSONKeyedByName(t *testing.T) {
	entries := []db.SchemaEntry{
		{Type: "table", Name: "t1", SQL: "CREATE TABLE t1 (a)"},
		{Type: "index", Name: "i1", SQL: "CREATE INDEX i1 ON t1(a)"},
	}
	var buf bytes.Buffer
	if err := renderDBSchema(&buf, entries, true); err != nil {
		t.Fatalf("renderDBSchema: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["t1"] != "CREATE TABLE t1 (a)" {
		t.Errorf("got[t1] = %q", got["t1"])
	}
	if got["i1"] == "" {
		t.Errorf("missing i1 entry")
	}
}

func TestRenderDBTables_PlainAndJSON(t *testing.T) {
	names := []string{"alpha", "beta"}
	var buf bytes.Buffer
	if err := renderDBTables(&buf, names, false); err != nil {
		t.Fatalf("renderDBTables: %v", err)
	}
	if buf.String() != "alpha\nbeta\n" {
		t.Errorf("plain = %q, want alpha\\nbeta\\n", buf.String())
	}
	buf.Reset()
	if err := renderDBTables(&buf, names, true); err != nil {
		t.Fatalf("renderDBTables: %v", err)
	}
	var got []string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("got %v", got)
	}
	// Empty input → JSON empty array, never null.
	buf.Reset()
	if err := renderDBTables(&buf, nil, true); err != nil {
		t.Fatalf("renderDBTables(nil): %v", err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Errorf("empty json = %q, want []", buf.String())
	}
}
