package exporter_test

// The security boundary of the exporter's SQL surface, enforced mechanically.
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

// forbiddenColumns is the SQL boundary's forbidden-column table. Reading any of these is a
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

// payloadNumericJSONPaths is the closed allowlist of agent_events.payload
// JSON fields the exporter may read. The SQL boundary records payload as
// "aggregate numbers out, never emit the string": the message body must never
// leave this surface, but these per-turn scalar fields — the model id and the
// numbers the cost/token counters need — are the "aggregate numbers" it
// permits. Every entry here is either a closed-set label ($.model) or a
// number ($.cost and the four token counts). NONE is a free-text body field;
// $.text (the message body) is deliberately absent and must stay absent.
//
// The exporter reads payload ONLY through JSON_EXTRACT of one of these paths
// (see CostEventsTailSQL). A bare `payload` read, or a JSON_EXTRACT of any
// other path, still fails the forbidden-column scan below — the carve-out is
// scoped to exactly this list. This mirrors the narrowing of the
// spawn_inputs whole-table ban to a column-level ban.
var payloadNumericJSONPaths = []string{
	"model",
	"inputTokens",
	"outputTokens",
	"cacheReadTokens",
	"cacheWriteTokens",
	"cost",
}

// allowedPayloadExtractRe matches JSON_EXTRACT(payload, '$.<field>') for a
// field on the allowlist above, with an optional table qualifier
// (ae.payload). It is used to strip the sanctioned numeric reads before the
// forbidden-column scan, so an allowlisted extract does not trip the bare
// `payload` ban while every other use of payload still does.
var allowedPayloadExtractRe = regexp.MustCompile(
	`(?i)JSON_EXTRACT\(\s*(?:[a-z_][a-z0-9_]*\.)?payload\s*,\s*'\$\.(?:` +
		strings.Join(payloadNumericJSONPaths, "|") + `)'\s*\)`)

// stripAllowedPayloadExtracts removes every allowlisted JSON_EXTRACT(payload,
// …) expression from a statement, so a scan for a bare `payload` read sees
// only the payload references the exporter is NOT allowed to make.
func stripAllowedPayloadExtracts(stmt string) string {
	return allowedPayloadExtractRe.ReplaceAllString(stmt, " ")
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
		// Strip the sanctioned numeric/model payload extracts first: the SQL
		// boundary permits "aggregate numbers out" of agent_events.payload
		// (see payloadNumericJSONPaths). Any other read of payload — a bare
		// column, or a non-allowlisted JSON path — survives the strip and is
		// caught below.
		ids := identifiers(stripAllowedPayloadExtracts(stmt))
		for _, forbidden := range forbiddenColumns {
			for _, id := range ids {
				if id == forbidden {
					t.Errorf("SQL reads the forbidden column %q (see #2699 section 5):\n%s", forbidden, stmt)
				}
			}
		}
	}
}

// TestExporterSQL_PayloadIsReadOnlyViaAllowlistedNumericExtract pins the
// payload narrowing: agent_events.payload may be read, but ONLY through
// JSON_EXTRACT of an allowlisted numeric/model field. A bare payload read or
// a JSON path outside the allowlist must still fail. This is the payload
// analogue of TestExporterSource_WholeTableBanIsLimitedToSpawnInputs.
func TestExporterSQL_PayloadIsReadOnlyViaAllowlistedNumericExtract(t *testing.T) {
	// 1. The allowlist itself must contain no free-text body field. $.text is
	//    the message body; if it ever appears here the carve-out has been
	//    widened into a content leak.
	for _, path := range payloadNumericJSONPaths {
		for _, banned := range []string{"text", "payload", "prompt"} {
			if strings.EqualFold(path, banned) {
				t.Errorf("payloadNumericJSONPaths contains the free-text field %q; the carve-out is for aggregate numbers only (#2699 section 5)", path)
			}
		}
	}

	// 2. After stripping the allowlisted extracts, no statement may name
	//    payload at all — that would be a bare read or a non-allowlisted path.
	for _, stmt := range exporter.AllSQL {
		for _, id := range identifiers(stripAllowedPayloadExtracts(stmt)) {
			if id == "payload" {
				t.Errorf("a statement reads payload outside the allowlisted JSON_EXTRACT form (bare column or non-allowlisted path):\n%s", stmt)
			}
		}
	}

	// 3. The carve-out must not be vacuous: at least one statement must read
	//    payload through the allowlisted form, or this test proves nothing.
	found := false
	for _, stmt := range exporter.AllSQL {
		if allowedPayloadExtractRe.MatchString(stmt) {
			found = true
		}
	}
	if !found {
		t.Error("no statement reads payload via the allowlisted JSON_EXTRACT form; the carve-out test is vacuous")
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

// A counter computed as a full-table aggregate decreases at the prune
// horizon and Prometheus reads that as a counter reset.
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
	//
	// spawn_inputs and bus_messages are deliberately NOT in this list. The
	// remaining two whole-table entries — harness_frames and
	// pending_replay_deliveries — exist to hold free-text bodies, so every
	// column in them is sensitive and a blanket table-name ban is correct.
	// spawn_inputs is mostly closed-set operator config (profile_name,
	// isolation_flag, model_flag, agent_flag, ...) with exactly two free-text
	// columns among them — prompt_text and extras — and both are already
	// caught by the forbiddenColumns identifier scan above. bus_messages is
	// similar: repo, urgency, sent_at, delivered_at are all closed-set or
	// numeric, and the one free-text column — text, the message body the SQL
	// boundary bans — is likewise already caught there. Banning either bare
	// table name added nothing for the real hazard and instead made
	// profile_name (spawn_inputs) and the bus-backlog gauge
	// (bus_messages.repo) unreachable from this package. Do NOT restore
	// either name to this list as a "security fix" without re-reading the SQL
	// boundary first — the column-level ban below is the correct granularity
	// for both tables.
	unambiguous := []string{"prompt_text", "rubric_breakdown", "issue_ref", "harness_frames", "pending_replay_deliveries"}

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

// TestExporterSource_WholeTableBanIsLimitedToSpawnInputs is the
// security AC: harness_frames and pending_replay_deliveries remain banned as
// whole tables (they hold nothing but free-text bodies), while spawn_inputs
// — narrowed to a column-level ban — is reachable by name. This pins the
// narrowing to exactly one table so a future edit that widens it again is
// caught here, not discovered downstream.
func TestExporterSource_WholeTableBanIsLimitedToSpawnInputs(t *testing.T) {
	stillBanned := []string{"harness_frames", "pending_replay_deliveries"}
	for _, lit := range packageStringLiterals(t) {
		lower := strings.ToLower(lit)
		for _, name := range stillBanned {
			if strings.Contains(lower, name) {
				t.Errorf("a string literal names %q, which must remain banned as a whole table (#2699 section 5, #2720):\n%s", name, lit)
			}
		}
	}

	// spawn_inputs itself must be reachable — the whole point of the narrowing — but
	// its two sensitive columns must still be unreadable, enforced by the
	// forbiddenColumns scan in TestExporterSQL_ReadsNoRawTextBodyColumn.
	foundSpawnInputs := false
	for _, stmt := range exporter.AllSQL {
		if strings.Contains(strings.ToLower(stmt), "spawn_inputs") {
			foundSpawnInputs = true
		}
	}
	if !foundSpawnInputs {
		t.Error("expected at least one statement in exporter.AllSQL to name spawn_inputs (profile_name enrichment, #2720); found none")
	}
	for _, forbidden := range []string{"prompt_text", "extras"} {
		for _, stmt := range exporter.AllSQL {
			for _, id := range identifiers(stmt) {
				if id == forbidden {
					t.Errorf("statement reads the forbidden spawn_inputs column %q:\n%s", forbidden, stmt)
				}
			}
		}
	}
}

// TestExporterSource_BusMessagesWholeTableBanIsColumnLevel is the
// analogue of the spawn_inputs test above: bus_messages must be reachable by
// name (the bus-backlog gauge needs it), but its one free-text column —
// text, the inter-session message body the SQL boundary bans — must still be
// unreadable, enforced by the forbiddenColumns scan in
// TestExporterSQL_ReadsNoRawTextBodyColumn.
func TestExporterSource_BusMessagesWholeTableBanIsColumnLevel(t *testing.T) {
	foundBusMessages := false
	for _, stmt := range exporter.AllSQL {
		if strings.Contains(strings.ToLower(stmt), "bus_messages") {
			foundBusMessages = true
		}
	}
	if !foundBusMessages {
		t.Error("expected at least one statement in exporter.AllSQL to name bus_messages (bus-backlog gauge, #2702); found none")
	}
	for _, stmt := range exporter.AllSQL {
		for _, id := range identifiers(stmt) {
			if id == "text" {
				t.Errorf("statement reads the forbidden bus_messages column %q:\n%s", "text", stmt)
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
