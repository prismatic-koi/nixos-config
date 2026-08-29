package db

// Tests for ScrubSecrets — remediation for rows written before the capture
// path redacted anything.
//
// This file is an INTERNAL test (package db, not db_test) because the scrub
// path has to be exercised against rows that were written raw. Every
// production write now redacts, so an external test cannot create the "row
// already holds a credential" state that the scrub exists to fix. The helper
// below inserts straight through the connection to build it.
//
// SECURITY: every credential value here is synthetic.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/payload"
)

const (
	scrubFakeToken   = "SYNTHETIC-SCRUB-VALUE-00000000000"
	scrubFakeSession = "prism-test@scrub"
)

func openScrubTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// insertRawEvent writes an agent_events row bypassing WriteEvent, so the
// payload lands in the column exactly as given. This reproduces the state of
// a database written before the redaction control existed.
func insertRawEvent(t *testing.T, d *DB, eventType, rawPayload string) {
	t.Helper()
	const q = `
INSERT INTO agent_events (id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id)
VALUES (?, ?, ?, ?, NULL, ?, ?, ?, NULL)`
	if _, err := d.conn.Exec(q,
		uuid.New().String(), scrubFakeSession, "prism-test-repo", "/tmp/prism-test",
		eventType, rawPayload, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert raw event: %v", err)
	}
}

// insertRawEventsBatch inserts n agent_events rows in a single transaction,
// so the whole seed costs one commit (one fsync) instead of n. It is the
// batched form of insertRawEvent: every row is byte-identical to what
// insertRawEvent writes, except for the per-row random id. Batching bulk
// test-database row writes keeps internal/db off the go-test fsync-timeout
// path; see docs/test-database-fsync.md.
func insertRawEventsBatch(t *testing.T, d *DB, n int, eventType, rawPayload string) {
	t.Helper()
	tx, err := d.conn.Begin()
	if err != nil {
		t.Fatalf("insertRawEventsBatch: begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	const q = `
INSERT INTO agent_events (id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id)
VALUES (?, ?, ?, ?, NULL, ?, ?, ?, NULL)`
	stmt, err := tx.Prepare(q)
	if err != nil {
		t.Fatalf("insertRawEventsBatch: prepare: %v", err)
	}
	defer stmt.Close()
	createdAt := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(
			uuid.New().String(), scrubFakeSession, "prism-test-repo", "/tmp/prism-test",
			eventType, rawPayload, createdAt,
		); err != nil {
			t.Fatalf("insertRawEventsBatch: insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("insertRawEventsBatch: commit: %v", err)
	}
}

// insertRawHarnessFrame is insertRawEvent for the raw wire archive.
func insertRawHarnessFrame(t *testing.T, d *DB, frameType, rawPayload string) {
	t.Helper()
	const q = `
INSERT INTO harness_frames (id, session_name, instance_id, direction, type, payload, created_at)
VALUES (?, ?, NULL, ?, ?, ?, ?)`
	if _, err := d.conn.Exec(q,
		uuid.New().String(), scrubFakeSession, HarnessFrameDirectionIn,
		frameType, rawPayload, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert raw harness frame: %v", err)
	}
}

func allEventPayloads(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.conn.Query(`SELECT payload FROM agent_events ORDER BY rowid`)
	if err != nil {
		t.Fatalf("select payloads: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func allFramePayloads(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.conn.Query(`SELECT payload FROM harness_frames ORDER BY rowid`)
	if err != nil {
		t.Fatalf("select frame payloads: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func scrubTestRedactor() *payload.Redactor {
	return payload.NewRedactor(map[string]string{"GITHUB_TOKEN": scrubFakeToken})
}

func TestScrubSecrets_RewritesRowsWrittenBeforeTheControlExisted(t *testing.T) {
	d := openScrubTestDB(t)

	insertRawEvent(t, d, "tool_result", `{"output":"GITHUB_TOKEN=`+scrubFakeToken+`"}`)
	insertRawEvent(t, d, "msg_assistant", `{"text":"the key is `+scrubFakeToken+`"}`)
	clean := `{"output":"PASS\nok\n"}`
	insertRawEvent(t, d, "tool_result", clean)
	insertRawHarnessFrame(t, d, "tool_result", `{"type":"tool_result","output":"`+scrubFakeToken+`"}`)

	report, err := d.ScrubSecrets(scrubTestRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}

	if report.EventsScanned != 3 {
		t.Errorf("EventsScanned = %d, want 3", report.EventsScanned)
	}
	if report.EventsRewritten != 2 {
		t.Errorf("EventsRewritten = %d, want 2", report.EventsRewritten)
	}
	if report.HarnessFramesScanned != 1 {
		t.Errorf("HarnessFramesScanned = %d, want 1", report.HarnessFramesScanned)
	}
	if report.HarnessFramesRewritten != 1 {
		t.Errorf("HarnessFramesRewritten = %d, want 1", report.HarnessFramesRewritten)
	}
	if report.DryRun {
		t.Error("DryRun = true for a live pass")
	}
	if got, want := report.Changed(), 3; got != want {
		t.Errorf("Changed() = %d, want %d", got, want)
	}

	for _, p := range allEventPayloads(t, d) {
		if strings.Contains(p, scrubFakeToken) {
			t.Errorf("event row still carries the credential value: %s", p)
		}
	}
	for _, p := range allFramePayloads(t, d) {
		if strings.Contains(p, scrubFakeToken) {
			t.Errorf("harness frame row still carries the credential value: %s", p)
		}
	}

	// The untouched row must be byte-identical.
	payloads := allEventPayloads(t, d)
	if payloads[2] != clean {
		t.Errorf("clean row was modified:\n got %q\nwant %q", payloads[2], clean)
	}
	// The rewritten rows must name the variable.
	if !strings.Contains(payloads[0], "[redacted:GITHUB_TOKEN]") {
		t.Errorf("rewritten row does not name the variable: %s", payloads[0])
	}
}

func TestScrubSecrets_DryRunReportsWithoutWriting(t *testing.T) {
	d := openScrubTestDB(t)

	raw := `{"output":"` + scrubFakeToken + `"}`
	insertRawEvent(t, d, "tool_result", raw)
	insertRawHarnessFrame(t, d, "tool_result", raw)

	report, err := d.ScrubSecrets(scrubTestRedactor(), true)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if !report.DryRun {
		t.Error("DryRun = false for a dry-run pass")
	}
	if report.EventsRewritten != 1 || report.HarnessFramesRewritten != 1 {
		t.Errorf("dry run under-reported: events %d, frames %d", report.EventsRewritten, report.HarnessFramesRewritten)
	}

	if got := allEventPayloads(t, d); got[0] != raw {
		t.Errorf("dry run modified the event row: %q", got[0])
	}
	if got := allFramePayloads(t, d); got[0] != raw {
		t.Errorf("dry run modified the frame row: %q", got[0])
	}
}

func TestScrubSecrets_IsIdempotent(t *testing.T) {
	d := openScrubTestDB(t)
	insertRawEvent(t, d, "tool_result", `{"output":"`+scrubFakeToken+`"}`)

	if _, err := d.ScrubSecrets(scrubTestRedactor(), false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	after := allEventPayloads(t, d)

	second, err := d.ScrubSecrets(scrubTestRedactor(), false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Changed() != 0 {
		t.Errorf("second pass rewrote %d rows; a marker must not match any rule", second.Changed())
	}
	if got := allEventPayloads(t, d); got[0] != after[0] {
		t.Errorf("second pass changed the row: %q then %q", after[0], got[0])
	}
}

func TestScrubSecrets_ShapeLayerCoversAValueTheEnvironmentDoesNotHold(t *testing.T) {
	d := openScrubTestDB(t)

	shaped := "ghp_" + strings.Repeat("A", 36)
	insertRawEvent(t, d, "tool_result", `{"output":"`+shaped+`"}`)

	// No value knowledge at all — the operator running the scrub does not
	// have the credential in their shell.
	report, err := d.ScrubSecrets(payload.NewShapeOnlyRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if report.EventsRewritten != 1 {
		t.Fatalf("EventsRewritten = %d, want 1", report.EventsRewritten)
	}
	if got := allEventPayloads(t, d); strings.Contains(got[0], shaped) {
		t.Errorf("shape-matched credential survived the scrub: %s", got[0])
	}
}

func TestScrubSecrets_EmptyDatabaseIsANoOp(t *testing.T) {
	d := openScrubTestDB(t)
	report, err := d.ScrubSecrets(scrubTestRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if report.EventsScanned != 0 || report.HarnessFramesScanned != 0 || report.Changed() != 0 {
		t.Errorf("empty database produced %+v", report)
	}
}

// TestScrubSecrets_PagesPastTheBatchSize covers the cursor arithmetic: a
// bug there would silently leave rows past the first page unscrubbed.
func TestScrubSecrets_PagesPastTheBatchSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-page scrub in -short mode")
	}
	d := openScrubTestDB(t)

	const rows = scrubPageSize*2 + 7
	insertRawEventsBatch(t, d, rows, "tool_result", `{"output":"`+scrubFakeToken+`"}`)

	report, err := d.ScrubSecrets(scrubTestRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if report.EventsScanned != rows {
		t.Errorf("EventsScanned = %d, want %d", report.EventsScanned, rows)
	}
	if report.EventsRewritten != rows {
		t.Errorf("EventsRewritten = %d, want %d", report.EventsRewritten, rows)
	}
	for i, p := range allEventPayloads(t, d) {
		if strings.Contains(p, scrubFakeToken) {
			t.Fatalf("row %d survived the scrub: %s", i, p)
		}
	}
}

func TestScrubSecrets_NilRedactorFallsBackToTheProcessDefault(t *testing.T) {
	d := openScrubTestDB(t)

	shaped := "ghp_" + strings.Repeat("B", 36)
	insertRawEvent(t, d, "tool_result", `{"output":"`+shaped+`"}`)

	// The process default knows no synthetic value, so assert on the shape
	// layer: the test must not depend on the developer's environment.
	if _, err := d.ScrubSecrets(nil, false); err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if got := allEventPayloads(t, d); strings.Contains(got[0], shaped) {
		t.Errorf("nil redactor did not fall back to the process default: %s", got[0])
	}
}

// TestScrubSecrets_ShapeCannotSpanAJSONDelimiter is the bulk-rewrite half of
// the JSON-delimiter-span guard. The scrub rewrites historical rows
// in place, so a spanning match here corrupts data that has no other copy.
func TestScrubSecrets_ShapeCannotSpanAJSONDelimiter(t *testing.T) {
	d := openScrubTestDB(t)

	spanning := []string{
		`{"a":"-----BEGIN A PRIVATE KEY-----","b":[1,"-----END A PRIVATE KEY-----"]}`,
		`{"tool":"edit","args":{"a_old":"x -----BEGIN RSA PRIVATE KEY-----","z_new":"-----END RSA PRIVATE KEY-----"},"n":[1,2]}`,
	}
	for _, raw := range spanning {
		insertRawEvent(t, d, "tool_result", raw)
		insertRawHarnessFrame(t, d, "tool_result", raw)
	}

	report, err := d.ScrubSecrets(payload.NewShapeOnlyRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if report.Changed() != 0 {
		t.Errorf("scrub rewrote %d rows; no field holds a complete block so nothing should change", report.Changed())
	}

	for i, got := range allEventPayloads(t, d) {
		if !json.Valid([]byte(got)) {
			t.Errorf("event row %d is not valid JSON after the scrub: %s", i, got)
		}
		if got != spanning[i] {
			t.Errorf("event row %d was rewritten:\n  got:  %s\n  want: %s", i, got, spanning[i])
		}
	}
	for i, got := range allFramePayloads(t, d) {
		if !json.Valid([]byte(got)) {
			t.Errorf("frame row %d is not valid JSON after the scrub: %s", i, got)
		}
		if got != spanning[i] {
			t.Errorf("frame row %d was rewritten:\n  got:  %s\n  want: %s", i, got, spanning[i])
		}
	}
}

// TestScrubSecrets_CompleteBlockInOneFieldIsStillScrubbed pins that refusing
// to span did not switch the shape layer off in the bulk path.
func TestScrubSecrets_CompleteBlockInOneFieldIsStillScrubbed(t *testing.T) {
	d := openScrubTestDB(t)

	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\\nc3ludGhldGlj\\n-----END OPENSSH PRIVATE KEY-----"
	insertRawEvent(t, d, "tool_result", `{"output":"`+pem+`","keep":"untouched"}`)

	report, err := d.ScrubSecrets(payload.NewShapeOnlyRedactor(), false)
	if err != nil {
		t.Fatalf("ScrubSecrets: %v", err)
	}
	if report.EventsRewritten != 1 {
		t.Fatalf("EventsRewritten = %d, want 1", report.EventsRewritten)
	}

	got := allEventPayloads(t, d)[0]
	if !json.Valid([]byte(got)) {
		t.Fatalf("scrubbed row is not valid JSON: %s", got)
	}
	if strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("a complete block inside one field was not scrubbed: %s", got)
	}
	if !strings.Contains(got, "untouched") {
		t.Errorf("a neighbouring field was damaged: %s", got)
	}
}
