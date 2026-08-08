package exporter_test

// The security boundary of #2699 section 5, enforced mechanically.
//
// There is an exact in-tree precedent: internal/sidecar/host_api.go splits
// /stats ("aggregate counts", all roles) from /db/query ("row-level
// conversation content", coordinator-only). The exporter is a /stats-class
// surface. The operative rule:
//
//	The exporter may only SELECT aggregate functions (COUNT, SUM, MAX) and
//	closed-set grouping columns. It must never read a raw TEXT body column.
//
// These tests read the package source with go/parser and assert on every
// string literal in it, so a statement added later cannot dodge them by
// living outside exporter.AllSQL.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/exporter"
)

// forbiddenColumns is the #2699 section 5 table. Reading any of these is a
// data leak: they hold prompt bodies, conversation frames, inter-session
// message text, or operator-derived free-form strings.
var forbiddenColumns = []string{
	"payload",          // agent_events.payload, harness_frames.payload
	"prompt_text",      // spawn_inputs.prompt_text
	"text",             // bus_messages.text, pending_replay_deliveries.text
	"title",            // agent_status.title
	"issue_ref",        // agent_status.issue_ref
	"extras",           // spawn_inputs.extras
	"error",            // pending_merges.error
	"rubric_breakdown", // spawn_outcome.rubric_breakdown
}

// writeKeywords must never appear in any statement the exporter issues. The
// read-only DSN is the enforcement; this is the second lock.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "REPLACE", "UPSERT",
	"CREATE", "DROP", "ALTER", "TRUNCATE",
	"ATTACH", "DETACH", "VACUUM", "REINDEX",
	"BEGIN", "COMMIT", "ROLLBACK", "PRAGMA",
}

// identifier matches a bare SQL identifier so a check on "text" does not
// fire on the word "context" inside a longer token.
var identifierRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func identifiers(sqlText string) []string {
	raw := identifierRe.FindAllString(sqlText, -1)
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		out = append(out, strings.ToLower(id))
	}
	return out
}

// TestExporterSQL_ReadsNoRawTextBodyColumn is the AC: "A test asserts the SQL
// contains no raw TEXT body column."
func TestExporterSQL_ReadsNoRawTextBodyColumn(t *testing.T) {
	if len(exporter.AllSQL) == 0 {
		t.Fatal("exporter.AllSQL is empty; the boundary test would pass vacuously")
	}
	for _, stmt := range exporter.AllSQL {
		ids := identifiers(stmt)
		for _, forbidden := range forbiddenColumns {
			for _, id := range ids {
				if id == forbidden {
					t.Errorf("SQL reads the forbidden column %q (see #2699 section 5):\n%s", forbidden, stmt)
				}
			}
		}
	}
}

func TestExporterSQL_IssuesOnlySelects(t *testing.T) {
	for _, stmt := range exporter.AllSQL {
		trimmed := strings.TrimSpace(stmt)
		if !strings.HasPrefix(strings.ToUpper(trimmed), "SELECT") {
			t.Errorf("statement does not start with SELECT:\n%s", stmt)
		}
		upper := strings.ToUpper(stmt)
		for _, kw := range writeKeywords {
			if regexp.MustCompile(`\b` + kw + `\b`).MatchString(upper) {
				t.Errorf("statement contains the write keyword %q:\n%s", kw, stmt)
			}
		}
	}
}

// The projection is pinned exactly. A reviewer confirming the boundary can
// read this list instead of re-deriving it from the SQL.
func TestExporterSQL_TailProjectionIsCursorAndTypeOnly(t *testing.T) {
	stmt := exporter.AgentEventsTailSQL
	upper := strings.ToUpper(stmt)
	selectIdx := strings.Index(upper, "SELECT")
	fromIdx := strings.Index(upper, "FROM")
	if selectIdx < 0 || fromIdx < 0 || fromIdx < selectIdx {
		t.Fatalf("cannot find the projection in:\n%s", stmt)
	}
	projection := strings.TrimSpace(stmt[selectIdx+len("SELECT") : fromIdx])
	var cols []string
	for _, c := range strings.Split(projection, ",") {
		cols = append(cols, strings.ToLower(strings.TrimSpace(c)))
	}
	want := []string{"rowid", "type"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Errorf("tail projection is %v, want exactly %v — rowid is the cursor and type is a closed-set label (#2699 section 6)",
			cols, want)
	}
}

// #2699 section 3: a counter computed as a full-table aggregate decreases at
// the prune horizon and Prometheus reads that as a counter reset.
//
// COUNT and SUM are therefore banned outright. MAX is allowed in exactly one
// statement, and only because its result initialises and clamps a CURSOR —
// which is allowed to move in both directions — never a counter value.
func TestExporterSQL_NoCounterComesFromAFullTableAggregate(t *testing.T) {
	for _, stmt := range exporter.AllSQL {
		upper := strings.ToUpper(stmt)
		for _, fn := range []string{"COUNT", "SUM", "AVG", "TOTAL", "GROUP_CONCAT"} {
			if regexp.MustCompile(`\b` + fn + `\s*\(`).MatchString(upper) {
				t.Errorf("statement uses the aggregate %s(); counters must be produced by tailing, not aggregating (#2699 section 3):\n%s",
					fn, stmt)
			}
		}
		if regexp.MustCompile(`\bMAX\s*\(`).MatchString(upper) && stmt != exporter.AgentEventsMaxRowIDSQL {
			t.Errorf("MAX() is permitted only in AgentEventsMaxRowIDSQL, where it positions the cursor:\n%s", stmt)
		}
	}
}

// A statement declared outside exporter.AllSQL would escape every test
// above. Walk the package source and prove none exists.
func TestExporterSQL_AllSQLCoversEveryStatementInThePackage(t *testing.T) {
	known := map[string]bool{}
	for _, s := range exporter.AllSQL {
		known[normaliseSQL(s)] = true
	}

	for _, lit := range packageStringLiterals(t) {
		if !looksLikeSQL(lit) {
			continue
		}
		if !known[normaliseSQL(lit)] {
			t.Errorf("the package contains a SQL statement that is not in exporter.AllSQL, so the boundary tests do not cover it:\n%s", lit)
		}
	}
}

// Defence in depth for the read-only guarantee: no string literal anywhere
// in the package spells a write statement, and none names a forbidden column
// unambiguously.
func TestExporterSource_ContainsNoWriteStatementAndNoForbiddenColumnLiteral(t *testing.T) {
	// Only the unambiguous names. "text", "title", "error", and "extras"
	// are ordinary English words that appear in log messages, so they are
	// checked inside SQL statements only (see the tests above).
	unambiguous := []string{"prompt_text", "rubric_breakdown", "issue_ref", "harness_frames", "spawn_inputs", "pending_replay_deliveries", "bus_messages"}

	for _, lit := range packageStringLiterals(t) {
		upper := strings.ToUpper(lit)
		if looksLikeSQL(lit) {
			for _, kw := range writeKeywords {
				if regexp.MustCompile(`\b` + kw + `\b`).MatchString(upper) {
					t.Errorf("a SQL literal in the package contains the write keyword %q:\n%s", kw, lit)
				}
			}
		}
		lower := strings.ToLower(lit)
		for _, name := range unambiguous {
			if strings.Contains(lower, name) {
				t.Errorf("a string literal names %q, which the exporter must never read (#2699 section 5):\n%s", name, lit)
			}
		}
	}
}

func looksLikeSQL(s string) bool {
	upper := strings.ToUpper(s)
	return regexp.MustCompile(`\bSELECT\b`).MatchString(upper) &&
		regexp.MustCompile(`\bFROM\b`).MatchString(upper)
}

func normaliseSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// packageStringLiterals returns every string-literal value in the
// non-test Go source of internal/exporter. It parses rather than greps so a
// doc comment that NAMES a forbidden column — sql.go has one, deliberately —
// is not mistaken for a query that reads it.
func packageStringLiterals(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse internal/exporter: %v", err)
	}
	pkg, ok := pkgs["exporter"]
	if !ok {
		t.Fatalf("package exporter not found in the parsed directory; got %v", keys(pkgs))
	}

	var out []string
	files := 0
	for _, f := range pkg.Files {
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			out = append(out, v)
			return true
		})
	}
	if files == 0 {
		t.Fatal("parsed no source files; the boundary tests would pass vacuously")
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
