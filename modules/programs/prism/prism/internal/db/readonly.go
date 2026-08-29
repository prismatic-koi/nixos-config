// Read-only query / introspection helpers used by the `prism db` surface.
//
// These primitives are deliberately separate from db.Open(): they do NOT run
// schema migrations and they open a fresh handle in SQLite read-only mode
// (`?mode=ro`). The host-side handler for `GET /db/query` re-opens its own
// read-only handle on every request rather than sharing the sidecar's
// writable handle — keeping the safety boundary obvious in the code.
//
// SQL parsing is intentionally avoided. Read-only enforcement is delegated to
// SQLite via `?mode=ro`, which surfaces the unambiguous "attempt to write a
// readonly database" error on any DML, DDL, or PRAGMA-with-side-effects.
//
// Single-statement detection uses a small, hand-rolled tokenizer (see
// IsSingleStatement) that handles single-quoted strings, double-quoted
// identifiers, line comments, and block comments correctly. This is the
// equivalent of sqlite3_complete() for our purposes — counting unquoted
// semicolons that separate complete statements — and avoids reaching for
// regex.

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // register sqlite3 driver
)

// OpenReadOnly opens a fresh SQLite handle in read-only mode against path.
// The connection-pool DSN sets a busy_timeout so concurrent writers (the
// sidecar) cannot starve the read; it does NOT configure WAL or foreign_keys
// (read-only handles do not need them).
//
// The caller owns the returned *sql.DB and must Close() it when done.
func OpenReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("db.OpenReadOnly: empty path")
	}
	dsn := "file:" + path +
		"?mode=ro" +
		"&_pragma=busy_timeout(5000)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open read-only: %w", err)
	}
	// Validate that the DB file is reachable / openable. sql.Open is lazy;
	// PingContext forces a real connection so callers see "no such file"
	// or "unable to open database" errors immediately rather than at first
	// query time.
	if err := conn.PingContext(context.Background()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: open read-only: %w", err)
	}
	return conn, nil
}

// QueryResult is the structured return value of RunQuery.
//
// Rows is always non-nil, even for empty result sets — empty becomes [][]any{}
// rather than nil so JSON marshalling produces "rows": [] (never "rows": null).
//
// Each cell carries the raw value from the SQLite row scan:
//   - INTEGER → int64
//   - REAL    → float64
//   - TEXT    → string
//   - BLOB    → []byte
//   - NULL    → nil
//
// Rendering decisions (hex vs base64 for BLOB, "NULL" string vs JSON null) are
// the caller's responsibility — see cmd/db_query.go.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	ElapsedMs int64    `json:"elapsedMs"`
}

// RunQuery executes a single read-only SQL statement against conn and returns
// columns, rows, and the elapsed time in ms. It uses QueryContext so a
// cancelled / timed-out context interrupts the running statement (the
// modernc.org/sqlite driver translates ctx.Done into sqlite3_interrupt).
//
// RunQuery does NOT validate that sqlText contains a single statement — that
// is the caller's job (use IsSingleStatement). The CLI/host-API rejects
// multi-statement input before calling RunQuery so the error surface is
// uniform regardless of which path was taken.
func RunQuery(ctx context.Context, conn *sql.DB, sqlText string) (*QueryResult, error) {
	rows, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("db: read columns: %w", err)
	}

	out := &QueryResult{
		Columns: cols,
		Rows:    [][]any{}, // never nil — empty result must marshal as []
	}

	for rows.Next() {
		// Allocate fresh holders per row. Using []any with *any pointers gives
		// us back the driver's native types (int64 / float64 / string /
		// []byte / nil) intact, which is exactly what we want for downstream
		// rendering.
		holders := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("db: scan row: %w", err)
		}
		out.Rows = append(out.Rows, holders)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Tables returns the names of all user tables in the database, sorted
// lexicographically and excluding internal sqlite_* tables.
func Tables(ctx context.Context, conn *sql.DB) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		 WHERE type='table' AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// SchemaEntry is one ordered row of a Schema() result.
//
// Type is "table" or "index". Name is the object name. SQL is the original
// CREATE statement as recorded in sqlite_master.sql.
type SchemaEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// Schema returns CREATE TABLE / CREATE INDEX statements from sqlite_master in
// a deterministic order: tables before indexes, each group sorted by name.
//
// When tableFilter is non-empty, only the matching table's CREATE TABLE row
// is returned (indexes for that table are NOT included — `prism db schema
// <table>` prints only that table's DDL). When tableFilter is empty, all user
// tables and indexes are returned.
//
// Internal sqlite_* objects are excluded. Rows where sql IS NULL (auto-indexes
// for PRIMARY KEY / UNIQUE constraints) are excluded — they have no
// printable DDL.
func Schema(ctx context.Context, conn *sql.DB, tableFilter string) ([]SchemaEntry, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if tableFilter != "" {
		rows, err = conn.QueryContext(ctx, `
			SELECT type, name, sql FROM sqlite_master
			 WHERE type='table' AND name = ? AND sql IS NOT NULL
			   AND name NOT LIKE 'sqlite_%'`, tableFilter)
	} else {
		rows, err = conn.QueryContext(ctx, `
			SELECT type, name, sql FROM sqlite_master
			 WHERE type IN ('table','index') AND sql IS NOT NULL
			   AND name NOT LIKE 'sqlite_%'`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SchemaEntry
	for rows.Next() {
		var e SchemaEntry
		if err := rows.Scan(&e.Type, &e.Name, &e.SQL); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Deterministic order: tables before indexes, alpha within each group.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			// "index" < "table" lexicographically, but we want tables first.
			return entries[i].Type == "table" && entries[j].Type == "index"
		}
		return entries[i].Name < entries[j].Name
	})

	return entries, nil
}

// IsSingleStatement returns true when sqlText contains exactly one SQL
// statement (after stripping comments and whitespace).
//
// The detection is a hand-rolled lexer that walks the input once and tracks
// quoting / comment state, counting only unquoted semicolons that separate
// complete statements. This is the equivalent of repeatedly calling
// sqlite3_complete() for our purposes, and avoids reaching for regex, which
// is the wrong primitive here.
//
// The function returns:
//   - (true, nil)   for inputs containing exactly one statement, with or
//     without a trailing semicolon. Empty / whitespace-only /
//     comments-only inputs return (false, errEmpty).
//   - (false, nil)  for inputs containing two or more statements separated
//     by semicolons.
//   - (false, err)  for inputs that hit a malformed-token state (unterminated
//     string, etc.).
//
// Callers must treat (false, _) as "reject — multi-statement or
// unparseable" and propagate the error for diagnostics.
func IsSingleStatement(sqlText string) (bool, error) {
	const (
		stateNormal = iota
		stateSingleQuote
		stateDoubleQuote
		stateBacktick
		stateLineComment
		stateBlockComment
	)
	state := stateNormal

	// Track whether the current statement has any non-whitespace,
	// non-comment content. Bumps to true on the first real token; the
	// counter increments only when a semicolon ends a non-empty statement.
	statementHasContent := false
	completedStatements := 0

	flushStatement := func() {
		if statementHasContent {
			completedStatements++
			statementHasContent = false
		}
	}

	for i := 0; i < len(sqlText); i++ {
		c := sqlText[i]
		switch state {
		case stateNormal:
			switch {
			case c == '\'':
				state = stateSingleQuote
				statementHasContent = true
			case c == '"':
				state = stateDoubleQuote
				statementHasContent = true
			case c == '`':
				state = stateBacktick
				statementHasContent = true
			case c == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-':
				state = stateLineComment
				i++ // skip the second dash
			case c == '/' && i+1 < len(sqlText) && sqlText[i+1] == '*':
				state = stateBlockComment
				i++ // skip the *
			case c == ';':
				flushStatement()
			case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
				// whitespace — does not bump statementHasContent
			default:
				statementHasContent = true
			}
		case stateSingleQuote:
			if c == '\'' {
				// Doubled '' is an embedded apostrophe, not the end.
				if i+1 < len(sqlText) && sqlText[i+1] == '\'' {
					i++
					continue
				}
				state = stateNormal
			}
		case stateDoubleQuote:
			if c == '"' {
				if i+1 < len(sqlText) && sqlText[i+1] == '"' {
					i++
					continue
				}
				state = stateNormal
			}
		case stateBacktick:
			if c == '`' {
				if i+1 < len(sqlText) && sqlText[i+1] == '`' {
					i++
					continue
				}
				state = stateNormal
			}
		case stateLineComment:
			if c == '\n' {
				state = stateNormal
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(sqlText) && sqlText[i+1] == '/' {
				i++
				state = stateNormal
			}
		}
	}

	// EOF: any unclosed string / block comment is a parse error.
	switch state {
	case stateSingleQuote, stateDoubleQuote, stateBacktick:
		return false, fmt.Errorf("unterminated string literal")
	case stateBlockComment:
		return false, fmt.Errorf("unterminated /* ... */ comment")
	}

	// Final flush — handles input without a trailing semicolon.
	flushStatement()

	if completedStatements == 0 {
		return false, fmt.Errorf("input contains no SQL statement")
	}
	if completedStatements > 1 {
		return false, nil
	}
	return true, nil
}

// IsReadOnlyError returns true when err looks like the SQLite "attempt to
// write a readonly database" error. The CLI layer surfaces this with a
// dedicated message rather than the raw driver string so the user sees
// something actionable. Match is a substring on the lowercased message —
// modernc.org/sqlite returns variants like:
//
//	"SQL logic error: attempt to write a readonly database (8)"
//	"attempt to write a readonly database"
func IsReadOnlyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "readonly database") ||
		strings.Contains(msg, "read-only database")
}
