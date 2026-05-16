package main

// db_query.go — `iris db query` subcommand.
//
// Mirrors prism's cmd/db_query.go, with one critical iris-specific
// addition spelled out by issue #1698: a **pre-execution keyword check**
// that rejects write statements BEFORE the DB is opened. The pre-check is
// belt-and-braces with SQLite's own read-only mode (which would also
// reject the write at execution time); together they make the failure
// surface unambiguous and the error message actionable.
//
// # Read-only enforcement layers
//
// 1. isWriteStatement() walks the SQL once, skipping comments and string
//    literals, and rejects the input if the first non-comment keyword is
//    one of INSERT / UPDATE / DELETE / REPLACE / DROP / CREATE / ALTER /
//    TRUNCATE / PRAGMA / ATTACH / DETACH / REINDEX / VACUUM / BEGIN /
//    COMMIT / ROLLBACK / SAVEPOINT / RELEASE. The check returns the
//    rejected keyword so the error message is specific.
//
// 2. The DB is then opened in SQLite read-only mode via db.OpenReadOnly
//    (?mode=ro). Even if a write keyword somehow slipped past the
//    pre-check (e.g. a future SQLite verb we don't yet know about),
//    SQLite would surface the "attempt to write a readonly database"
//    error at execution time.
//
// # No proxy path
//
// Unlike prism's db_query.go, iris does not proxy through a host-API
// socket — iris CLI surfaces read the DB file directly per the carve-out
// in issue #1676. This keeps the implementation simpler and avoids the
// "no daemon? no debug" trap.
//
// # JSON output parity with prism
//
// The JSON wire shape (`{"columns": [...], "rows": [...], "elapsedMs": N}`)
// and per-cell coercion (int64 → number, float64 → number, string →
// string, bool → bool, []byte → base64 string, nil → null) match prism's
// behaviour byte-for-byte. Tests in db_query_test.go assert this.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// dbQueryDefaultTimeout caps a single `iris db query` invocation. 5s is
// generous for forensic queries against a single-user iris DB; a runaway
// SELECT is interrupted rather than blocking the CLI indefinitely.
const dbQueryDefaultTimeout = 5 * time.Second

// dbQueryRowDisplayLimit truncates table-mode output after this many
// rows. JSON mode is never truncated — `--json` is documented as "all
// rows" for scripting use.
const dbQueryRowDisplayLimit = 50

var dbQueryCmd = &cobra.Command{
	Use:   "query [SQL | -]",
	Short: "Run a single read-only SQL statement against the iris database",
	Long: `Run a single read-only SQL statement against the iris database.

The SQL text may be passed as the first positional argument, or read from
stdin by passing "-". Stdin is convenient for multi-line queries that
would be awkward to quote on argv:

    iris db query - <<'SQL'
      SELECT session_name, started_at FROM sessions
       WHERE harness = 'pi' AND iris_state = 'active'
       ORDER BY started_at DESC LIMIT 10;
    SQL

Read-only enforcement is enforced in two layers:

  1. A keyword pre-check rejects INSERT / UPDATE / DELETE / DROP /
     CREATE / ALTER / REPLACE / TRUNCATE / PRAGMA / ATTACH / DETACH /
     REINDEX / VACUUM and transaction-control verbs BEFORE the database
     is opened, with a clear "iris db query is read-only — use sqlite3
     directly for writes" error.
  2. The DB is then opened in SQLite read-only mode (?mode=ro) as
     defence-in-depth.

Multi-statement input (more than one SQL statement separated by ";") is
rejected at the CLI layer before any SQL runs.

By default output is rendered as an aligned table with a truncation
notice when there are more than 50 rows. Use --json to emit structured
JSON of the form {"columns": [...], "rows": [...], "elapsedMs": N};
--json emits all rows.`,
	Args:          cobra.MaximumNArgs(1),
	RunE:          runDBQuery,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	dbQueryCmd.Flags().Bool("json", false, "Emit structured JSON to stdout instead of an aligned table")
	dbQueryCmd.Flags().Duration("timeout", dbQueryDefaultTimeout, "Statement timeout (Go duration, e.g. 5s, 100ms)")
	dbCmd.AddCommand(dbQueryCmd)
}

func runDBQuery(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout <= 0 {
		timeout = dbQueryDefaultTimeout
	}

	sqlText, err := readDBQuerySQL(args, cmd.InOrStdin())
	if err != nil {
		return err
	}

	// Layer 1 of read-only enforcement: keyword pre-check, BEFORE we even
	// open the DB. The error message is specific (names the rejected
	// keyword) and points the operator at sqlite3 for legitimate writes.
	if kw, ok := isWriteStatement(sqlText); ok {
		return fmt.Errorf("iris db query is read-only — rejected write keyword %q. Use sqlite3 directly for writes (e.g. `sqlite3 %s`).", kw, resolveDBPath())
	}

	// Single-statement guard. Done at the CLI layer per the AC: "is
	// rejected at the CLI layer with a clear error before any SQL runs".
	ok, parseErr := db.IsSingleStatement(sqlText)
	if parseErr != nil {
		return fmt.Errorf("iris db query: %w", parseErr)
	}
	if !ok {
		return fmt.Errorf("iris db query: input must contain exactly one SQL statement (use separate invocations for multi-statement queries)")
	}

	dbPath := resolveDBPath()
	if err := ensureDBExists(dbPath); err != nil {
		return fmt.Errorf("iris db query: %w", err)
	}

	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("iris db query: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	start := time.Now()
	res, err := db.RunQuery(ctx, conn, sqlText)
	if err != nil {
		// Defence-in-depth (layer 2) error path: even if a write keyword
		// slipped past isWriteStatement, SQLite returns the "readonly
		// database" error here and we surface it with the same prefix as
		// the pre-check failure.
		if db.IsReadOnlyError(err) {
			return fmt.Errorf("iris db query is read-only — SQLite rejected the write at execution time. Use sqlite3 directly for writes: %w", err)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("iris db query: timeout exceeded (%s)", timeout)
		}
		// SQLite syntax / runtime error — surface the raw driver
		// message (no stack trace) per the AC: "the CLI exits non-zero
		// with the underlying SQLite error message (no stack trace)".
		return fmt.Errorf("iris db query: %w", err)
	}
	res.ElapsedMs = time.Since(start).Milliseconds()

	return renderDBQueryResult(cmd.OutOrStdout(), res, jsonMode)
}

// readDBQuerySQL resolves the SQL text from argv or stdin.
//
//   - One arg, not "-":   the arg itself is the SQL.
//   - One arg, "-":       read from stdin.
//   - No args, stdin TTY: refuse with a clear error rather than hang.
//   - No args, stdin pipe/file: read from stdin (mirrors `sqlite3 < f.sql`).
func readDBQuerySQL(args []string, stdin io.Reader) (string, error) {
	if len(args) == 1 && args[0] != "-" {
		return strings.TrimSpace(args[0]), nil
	}
	if len(args) == 1 && args[0] == "-" {
		return readAllOrErr(stdin)
	}
	if f, ok := stdin.(*os.File); ok {
		fi, statErr := f.Stat()
		if statErr == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			return "", fmt.Errorf("iris db query: SQL is required as an argument (or use \"-\" to read from stdin)")
		}
	}
	return readAllOrErr(stdin)
}

func readAllOrErr(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("iris db query: stdin is unavailable")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("iris db query: read stdin: %w", err)
	}
	out := strings.TrimSpace(string(b))
	if out == "" {
		return "", fmt.Errorf("iris db query: empty input — pass a SQL statement on argv or via stdin")
	}
	return out, nil
}

// writeKeywords is the set of SQL verbs that mutate state. The pre-check
// uses an exact match against the first non-comment, non-whitespace
// token. Anything not in this set falls through to SQLite's read-only
// handle — which itself rejects writes — so an unknown future verb is
// caught at layer 2 rather than silently allowed.
//
// Sourced from the SQLite "SQL Language" reference plus the standard
// transaction-control verbs. Kept in upper-case; the lookup is
// case-insensitive at the call site.
var writeKeywords = map[string]struct{}{
	"INSERT":    {},
	"UPDATE":    {},
	"DELETE":    {},
	"REPLACE":   {},
	"DROP":      {},
	"CREATE":    {},
	"ALTER":     {},
	"TRUNCATE":  {},
	"PRAGMA":    {}, // some PRAGMAs have side effects; reject the verb wholesale
	"ATTACH":    {},
	"DETACH":    {},
	"REINDEX":   {},
	"VACUUM":    {},
	"ANALYZE":   {},
	"BEGIN":     {},
	"COMMIT":    {},
	"END":       {}, // alias for COMMIT
	"ROLLBACK":  {},
	"SAVEPOINT": {},
	"RELEASE":   {},
}

// isWriteStatement returns (keyword, true) when sqlText's first
// non-comment, non-whitespace token is one of writeKeywords. Otherwise
// returns ("", false).
//
// The walker handles:
//
//   - Leading whitespace.
//   - Line comments ("-- …" through end of line).
//   - Block comments ("/* … */", may span newlines).
//
// It does NOT need to handle string literals — we only inspect the very
// first token, and SQL verbs cannot legally be quoted as identifiers and
// still parse as a statement (a quoted "INSERT" is an identifier in a
// position where SQLite expects a statement and would fail at parse
// time). Keeping the walker simple — first non-comment token only — is
// deliberate.
func isWriteStatement(sqlText string) (string, bool) {
	i := 0
	for i < len(sqlText) {
		c := sqlText[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-':
			// Line comment — skip through end-of-line or EOF.
			i += 2
			for i < len(sqlText) && sqlText[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sqlText) && sqlText[i+1] == '*':
			// Block comment — skip through */ or EOF.
			i += 2
			for i+1 < len(sqlText) && !(sqlText[i] == '*' && sqlText[i+1] == '/') {
				i++
			}
			if i+1 < len(sqlText) {
				i += 2 // consume the */
			} else {
				// Unterminated block comment — fall through; the SQLite
				// parser will surface the error.
				return "", false
			}
		default:
			// First real token: read until non-identifier character, then
			// uppercase and compare.
			start := i
			for i < len(sqlText) && isKeywordChar(sqlText[i]) {
				i++
			}
			token := strings.ToUpper(sqlText[start:i])
			if _, ok := writeKeywords[token]; ok {
				return token, true
			}
			return "", false
		}
	}
	// Whitespace / comments only — not a write statement (will fail the
	// IsSingleStatement check downstream).
	return "", false
}

// isKeywordChar reports whether c is a character that can appear inside
// a SQL keyword. Letters, digits, and underscore — the standard
// identifier-character set, which is a superset of keyword characters.
func isKeywordChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		c == '_'
}

// renderDBQueryResult writes res to w in either JSON or aligned-table
// mode. JSON mode preserves prism's wire shape byte-for-byte.
func renderDBQueryResult(w io.Writer, res *db.QueryResult, jsonMode bool) error {
	if jsonMode {
		return renderDBQueryJSON(w, res)
	}
	return renderDBQueryTable(w, res)
}

// renderDBQueryJSON emits {"columns": [...], "rows": [...], "elapsedMs": N}.
//
// BLOB cells are converted to base64 to match standard JSON encoding
// conventions; NULL stays as JSON null. The output matches prism's
// renderDBQueryJSON byte-for-byte (modulo the actual data) so a script
// consuming both surfaces sees one wire shape.
func renderDBQueryJSON(w io.Writer, res *db.QueryResult) error {
	cooked := make([][]any, len(res.Rows))
	for i, row := range res.Rows {
		newRow := make([]any, len(row))
		for j, cell := range row {
			switch v := cell.(type) {
			case []byte:
				newRow[j] = base64.StdEncoding.EncodeToString(v)
			default:
				newRow[j] = v
			}
		}
		cooked[i] = newRow
	}
	out := struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		ElapsedMs int64    `json:"elapsedMs"`
	}{
		Columns:   res.Columns,
		Rows:      cooked,
		ElapsedMs: res.ElapsedMs,
	}
	if out.Rows == nil {
		out.Rows = [][]any{}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// dbQueryNullStyle is the style used to render NULL cells (and the
// truncation tail) in table mode. Faint per the AC: "NULL values are
// rendered as the literal string NULL (dimmed in table mode)".
var dbQueryNullStyle = lipgloss.NewStyle().Faint(true)

// dbQueryHeaderStyle bolds the header row. The fg colour mirrors iris's
// other CLI tables (see list_sessions.go) so the `iris db` family feels
// like a member of the iris CLI rather than an alien import.
var dbQueryHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cliColSecondary))

// renderDBQueryTable formats res as an aligned text table with a styled
// header row and a footer showing row count and elapsed time. Long
// output is truncated after dbQueryRowDisplayLimit rows.
func renderDBQueryTable(w io.Writer, res *db.QueryResult) error {
	if len(res.Columns) == 0 {
		fmt.Fprintf(w, "(no columns) — %d row(s) — %dms\n", len(res.Rows), res.ElapsedMs)
		return nil
	}

	formatted := make([][]string, len(res.Rows))
	truncated := false
	displayCount := len(res.Rows)
	if displayCount > dbQueryRowDisplayLimit {
		displayCount = dbQueryRowDisplayLimit
		truncated = true
	}
	for i := 0; i < displayCount; i++ {
		row := res.Rows[i]
		out := make([]string, len(res.Columns))
		for j := range res.Columns {
			if j < len(row) {
				out[j] = formatCellTable(row[j])
			}
		}
		formatted[i] = out
	}

	widths := make([]int, len(res.Columns))
	for i, c := range res.Columns {
		widths[i] = len([]rune(c))
	}
	for i := 0; i < displayCount; i++ {
		row := formatted[i]
		for j, cell := range row {
			n := len([]rune(cell))
			if n > widths[j] {
				widths[j] = n
			}
		}
	}

	// Header.
	for i, c := range res.Columns {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprint(w, dbQueryHeaderStyle.Render(padRunes(c, widths[i])))
	}
	fmt.Fprintln(w)

	// Body.
	for i := 0; i < displayCount; i++ {
		row := formatted[i]
		for j := 0; j < len(res.Columns); j++ {
			if j > 0 {
				fmt.Fprint(w, "  ")
			}
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			padded := padRunes(cell, widths[j])
			if cell == "NULL" {
				fmt.Fprint(w, dbQueryNullStyle.Render(padded))
			} else {
				fmt.Fprint(w, padded)
			}
		}
		fmt.Fprintln(w)
	}

	if truncated {
		more := len(res.Rows) - displayCount
		fmt.Fprintln(w, dbQueryNullStyle.Render(fmt.Sprintf("... (%d more — pass --json to see all)", more)))
	}

	footerStyle := lipgloss.NewStyle().Faint(true)
	fmt.Fprintln(w, footerStyle.Render(fmt.Sprintf("(%d row%s, %dms)", len(res.Rows), pluralS(len(res.Rows)), res.ElapsedMs)))
	return nil
}

// formatCellTable converts a raw scanned value to its string form for
// table output, matching prism's formatCellTable byte-for-byte:
//
//   - NULL → "NULL"
//   - BLOB → "0x<hex>"
//   - everything else → predictable per-type formatting.
func formatCellTable(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return "0x" + hex.EncodeToString(t)
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// padRunes right-pads s with spaces to width n in runes.
func padRunes(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
