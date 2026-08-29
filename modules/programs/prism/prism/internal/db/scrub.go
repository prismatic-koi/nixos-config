// Remediation for rows written before the capture path redacted anything.
//
// The write-time control in redact.go protects new rows only. Rows already in
// prism.db hold whatever the agents printed, and the 90-day prune is far too
// slow a remedy for a live credential. ScrubSecrets rewrites those rows in
// place.
//
// Coverage:
//
//   - agent_events.payload   — every captured event, every type.
//   - harness_frames.payload — the raw wire archive.
//
// NOT covered: on-disk session archives. `prism cleanup` copies a session's
// harness transcript out of the pi sessions root
// ($PI_CODING_AGENT_DIR/sessions/, else ~/.pi/agent/sessions/) and into the
// archive directory named by sessions.archive_path. Pi writes that transcript
// itself; no prism redaction has ever run over it, before or after this
// change. Those files are outside the database and are not touched here. See
// docs/secret-redaction.md.
package db

import (
	"fmt"

	"github.com/prismatic-koi/prism/internal/payload"
)

// scrubPageSize is the number of rows read into memory per page. The rows are
// read, redacted, and written back one page at a time so a large database
// does not have to fit in memory.
const scrubPageSize = 500

// ScrubReport counts what a scrub pass examined and changed. It never
// carries a credential value, a payload, or a row identifier.
type ScrubReport struct {
	// EventsScanned is the number of agent_events rows read.
	EventsScanned int
	// EventsRewritten is the number of agent_events rows whose payload
	// changed.
	EventsRewritten int
	// HarnessFramesScanned is the number of harness_frames rows read.
	HarnessFramesScanned int
	// HarnessFramesRewritten is the number of harness_frames rows whose
	// payload changed.
	HarnessFramesRewritten int
	// DryRun records whether the pass wrote anything back.
	DryRun bool
}

// Changed reports whether the pass found any row that needs a rewrite.
func (r ScrubReport) Changed() int {
	return r.EventsRewritten + r.HarnessFramesRewritten
}

// ScrubSecrets rewrites every stored payload that carries a credential.
//
// r supplies the rules. Pass nil to use the process default
// (ProcessRedactor), which reads the credential values out of this process's
// environment — run the scrub from a shell that has the same credentials the
// agents had, or the value layer has nothing to match and only the shape
// layer applies.
//
// When dryRun is true the pass reads and compares but writes nothing, so the
// report says how many rows a real pass would change.
//
// Each page is written in its own transaction. A failure part-way through
// leaves the pages already committed scrubbed and the rest untouched; a
// re-run is safe and finishes the job, because redaction is idempotent —
// a marker does not match any rule.
func (d *DB) ScrubSecrets(r *payload.Redactor, dryRun bool) (ScrubReport, error) {
	if r == nil {
		r = ProcessRedactor()
	}
	report := ScrubReport{DryRun: dryRun}

	events, rewrittenEvents, err := d.scrubTable("agent_events", r, dryRun)
	if err != nil {
		return report, err
	}
	report.EventsScanned = events
	report.EventsRewritten = rewrittenEvents

	frames, rewrittenFrames, err := d.scrubTable("harness_frames", r, dryRun)
	if err != nil {
		return report, err
	}
	report.HarnessFramesScanned = frames
	report.HarnessFramesRewritten = rewrittenFrames

	return report, nil
}

// scrubTable pages through one table's payload column by rowid.
//
// table is never user input — the two call sites above pass a literal — so
// interpolating it into the statement carries no injection risk. The payload
// values themselves are always bound as parameters.
func (d *DB) scrubTable(table string, r *payload.Redactor, dryRun bool) (scanned, rewritten int, err error) {
	type row struct {
		rowID   int64
		payload string
	}

	selectQ := fmt.Sprintf(
		"SELECT rowid, payload FROM %s WHERE rowid > ? ORDER BY rowid LIMIT ?",
		table,
	)
	updateQ := fmt.Sprintf("UPDATE %s SET payload = ? WHERE rowid = ?", table)

	var cursor int64
	for {
		rows, qErr := d.conn.Query(selectQ, cursor, scrubPageSize)
		if qErr != nil {
			return scanned, rewritten, fmt.Errorf("db: scrub %s: select: %w", table, qErr)
		}

		var page []row
		for rows.Next() {
			var rec row
			if scanErr := rows.Scan(&rec.rowID, &rec.payload); scanErr != nil {
				rows.Close() //nolint:errcheck
				return scanned, rewritten, fmt.Errorf("db: scrub %s: scan: %w", table, scanErr)
			}
			page = append(page, rec)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close() //nolint:errcheck
			return scanned, rewritten, fmt.Errorf("db: scrub %s: rows: %w", table, rowsErr)
		}
		rows.Close() //nolint:errcheck

		if len(page) == 0 {
			return scanned, rewritten, nil
		}
		cursor = page[len(page)-1].rowID
		scanned += len(page)

		var changed []row
		for _, rec := range page {
			// RedactJSON, not Redact — a flat pass over a stored JSON
			// document lets the private-key shape span a delimiter and
			// corrupt the row. The scrub rewrites in place, so there is
			// no way back. See payload.Redactor.RedactJSON.
			cleaned := r.RedactJSON(rec.payload)
			if cleaned != rec.payload {
				changed = append(changed, row{rowID: rec.rowID, payload: cleaned})
			}
		}
		rewritten += len(changed)

		if dryRun || len(changed) == 0 {
			continue
		}

		tx, txErr := d.conn.Begin()
		if txErr != nil {
			return scanned, rewritten, fmt.Errorf("db: scrub %s: begin tx: %w", table, txErr)
		}
		for _, rec := range changed {
			if _, execErr := tx.Exec(updateQ, rec.payload, rec.rowID); execErr != nil {
				tx.Rollback() //nolint:errcheck
				return scanned, rewritten, fmt.Errorf("db: scrub %s: update: %w", table, execErr)
			}
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return scanned, rewritten, fmt.Errorf("db: scrub %s: commit: %w", table, commitErr)
		}
	}
}
