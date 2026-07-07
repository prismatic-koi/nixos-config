package cmd

// prism db query — run a single read-only SQL statement.
//
// Implements issue #1467. Highlights:
//   - Read-only enforcement is delegated to SQLite via ?mode=ro. We do NOT
//     attempt to parse SQL to allowlist statements.
//   - Single-statement guard is performed at the CLI layer (and again on the
//     host-API side) using the db.IsSingleStatement tokenizer — equivalent
//     to sqlite3_complete() for our purposes.
//   - Statement timeout (default 5s, override via --timeout) is plumbed
//     through context so SQLite's interrupt fires when the deadline elapses.
//   - When PRISM_HOST_API is set the request is proxied to the host sidecar's
//     GET /db/query endpoint. Otherwise we open a fresh read-only handle on
//     the local DB. Rendering happens identically on both paths.

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

// dbQueryDefaultTimeout is the default --timeout value. 5 s is generous
// enough for any sane forensic query but short enough that a runaway
// SELECT does not block the CLI for an unbounded amount of time.
const dbQueryDefaultTimeout = 5 * time.Second

// dbQueryRowDisplayLimit is the row-truncation cap applied to table-mode
// output. JSON mode is never truncated; the issue is explicit that "JSON
// mode emits all rows".
const dbQueryRowDisplayLimit = 50

var dbQueryCmd = &cobra.Command{
	Use:   "query [SQL | -]",
	Short: "Run a single read-only SQL statement",
	Long: `Run a single read-only SQL statement against the prism database.

The SQL text may be passed as the first positional argument, or read from
stdin by passing "-". The latter is convenient for multi-line queries that
would be awkward to quote on argv.

Read-only enforcement is delegated to SQLite (?mode=ro). Any write statement
fails with a clear "read-only" error and a non-zero exit code. Multi-statement
input (more than one SQL statement separated by ";") is rejected at the CLI
layer before any SQL runs.

By default output is rendered as an aligned table. Use --json to emit
structured JSON of the form {"columns": [...], "rows": [...], "elapsedMs": N}.
JSON mode emits all rows; table mode may truncate very long output for
readability.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDBQuery,
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

	// Single-statement guard. Done at the CLI layer per the AC: "is rejected
	// at the CLI layer with a clear error before any SQL runs".
	ok, parseErr := db.IsSingleStatement(sqlText)
	if parseErr != nil {
		return fmt.Errorf("db query: %w", parseErr)
	}
	if !ok {
		return fmt.Errorf("db query: input must contain exactly one SQL statement (use separate invocations for multi-statement queries)")
	}

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runDBQueryProxy(cmd, apiURL, sqlText, timeout, jsonMode)
	}
	return runDBQueryLocal(cmd, sqlText, timeout, jsonMode)
}

// readDBQuerySQL resolves the SQL text from argv or stdin.
//
//   - With no positional args: stdin is used (matches `sqlite3 < file.sql`
//     behaviour) — but only when stdin is non-empty; otherwise we surface a
//     clear error so a forgotten argument doesn't hang the CLI.
//   - With "-": stdin is used.
//   - With one arg that's not "-": the arg itself is the SQL.
func readDBQuerySQL(args []string, stdin io.Reader) (string, error) {
	if len(args) == 1 && args[0] != "-" {
		return strings.TrimSpace(args[0]), nil
	}
	if len(args) == 1 && args[0] == "-" {
		return readAllOrErr(stdin)
	}
	// No args — fall back to stdin only when it's a redirected file/pipe.
	// On a TTY we refuse rather than silently hang.
	if f, ok := stdin.(*os.File); ok {
		fi, statErr := f.Stat()
		if statErr == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			return "", fmt.Errorf("db query: SQL is required as an argument (or use \"-\" to read from stdin)")
		}
	}
	return readAllOrErr(stdin)
}

func readAllOrErr(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("db query: stdin is unavailable")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("db query: read stdin: %w", err)
	}
	out := strings.TrimSpace(string(b))
	if out == "" {
		return "", fmt.Errorf("db query: empty input — pass a SQL statement on argv or via stdin")
	}
	return out, nil
}

// runDBQueryLocal opens a read-only handle on the local DB and runs the
// statement. Used when PRISM_HOST_API is unset (i.e. we're on the host).
func runDBQueryLocal(cmd *cobra.Command, sqlText string, timeout time.Duration, jsonMode bool) error {
	conn, err := db.OpenReadOnly(dbPath())
	if err != nil {
		return fmt.Errorf("db query: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	start := time.Now()
	res, err := db.RunQuery(ctx, conn, sqlText)
	if err != nil {
		if db.IsReadOnlyError(err) {
			return fmt.Errorf("db query: read-only: %w", err)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("db query: timeout exceeded (%s)", timeout)
		}
		return fmt.Errorf("db query: %w", err)
	}
	res.ElapsedMs = time.Since(start).Milliseconds()

	return renderDBQueryResult(cmd.OutOrStdout(), res, jsonMode)
}

// runDBQueryProxy proxies the request to the host-API sidecar.
func runDBQueryProxy(cmd *cobra.Command, apiURL, sqlText string, timeout time.Duration, jsonMode bool) error {
	params := map[string]string{
		"sql":     sqlText,
		"timeout": timeout.String(),
	}
	raw, err := proxyReadToHostAPI(apiURL, "/db/query", params)
	if err != nil {
		return fmt.Errorf("db query: %w", err)
	}
	var res db.QueryResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("db query: unmarshal proxy response: %w", err)
	}
	// Defensive: a JSON null for rows would defeat the empty-result AC.
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	return renderDBQueryResult(cmd.OutOrStdout(), &res, jsonMode)
}

// renderDBQueryResult writes res to w in either JSON or aligned-table mode.
//
// JSON mode preserves the wire shape end-to-end (the wire shape is itself
// the AC-defined shape: {"columns": [...], "rows": [...], "elapsedMs": N}).
// Table mode formats per cell using formatCellTable below.
func renderDBQueryResult(w io.Writer, res *db.QueryResult, jsonMode bool) error {
	if jsonMode {
		return renderDBQueryJSON(w, res)
	}
	return renderDBQueryTable(w, res)
}

// renderDBQueryJSON emits {"columns": [...], "rows": [...], "elapsedMs": N}.
//
// BLOB cells are converted to base64 to match standard JSON encoding
// conventions; NULL stays as JSON null.
func renderDBQueryJSON(w io.Writer, res *db.QueryResult) error {
	// Walk the rows once and convert []byte → base64 string. This must
	// happen before json.Marshal because encoding/json defaults to
	// base64-encoding []byte but does NOT differentiate it from string —
	// we want explicit base64 in a string field for round-tripping.
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

// dimNullStyle is the style used to render NULL cells in table mode. Faint
// (dimmed) per the AC: "NULL values are rendered as the literal string NULL
// (dimmed in table mode)".
var dimNullStyle = lipgloss.NewStyle().Faint(true)

// renderDBQueryTable formats res as an aligned text table with a styled
// header row and a footer showing row count and elapsed time. Long output is
// truncated after dbQueryRowDisplayLimit rows with a "... (N more)" tail.
func renderDBQueryTable(w io.Writer, res *db.QueryResult) error {
	if len(res.Columns) == 0 {
		fmt.Fprintf(w, "(no columns) — %d row(s) — %dms\n", len(res.Rows), res.ElapsedMs)
		return nil
	}

	// Pre-compute the per-row formatted cell values once so column-width
	// measurement and printing share the same strings.
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

	// Compute per-column widths from header + body. Use rune counts so
	// multibyte content lines up sensibly.
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

	// Header row.
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	for i, c := range res.Columns {
		if i > 0 {
			fmt.Fprint(w, "  ")
		}
		fmt.Fprint(w, headerStyle.Render(padRunes(c, widths[i])))
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
			// Style NULL cells dim per the AC. The literal string "NULL"
			// is the canonical sentinel emitted by formatCellTable.
			if cell == "NULL" {
				fmt.Fprint(w, dimNullStyle.Render(padded))
			} else {
				fmt.Fprint(w, padded)
			}
		}
		fmt.Fprintln(w)
	}

	if truncated {
		more := len(res.Rows) - displayCount
		fmt.Fprintln(w, dimNullStyle.Render(fmt.Sprintf("... (%d more — pass --json to see all)", more)))
	}

	// Footer.
	footerStyle := lipgloss.NewStyle().Faint(true)
	fmt.Fprintln(w, footerStyle.Render(fmt.Sprintf("(%d row%s, %dms)", len(res.Rows), pluralS(len(res.Rows)), res.ElapsedMs)))
	return nil
}

// formatCellTable converts a raw scanned value to its string form for table
// output. Per the issue ACs:
//
//   - NULL → "NULL"
//   - BLOB → "0x<hex>"
//   - everything else → fmt.Sprintf("%v", v) with explicit handling for
//     int64 / float64 / bool / string so the output is predictable.
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
