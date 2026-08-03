// Capture-path secret redaction at DB-write time (issue #2589).
//
// The harness redacts a frame before it reaches the socket, which is where the
// primary control belongs — the secret then never leaves the agent process.
// This file is the second control. It runs on the last line before an INSERT,
// so it covers:
//
//   - a frame from a harness that has no redactor of its own, present or
//     future;
//   - an event this process writes directly, without a harness in the path
//     (hook events, audit rows, backfills);
//   - a harness whose redactor was misconfigured or silently disabled.
//
// Both controls use the same rules — see internal/payload/redact.go.
package db

import (
	"sync"

	"github.com/prismatic-koi/prism/internal/payload"
)

var (
	processRedactorOnce sync.Once
	processRedactor     *payload.Redactor
)

// ProcessRedactor returns the redactor built from this process's environment.
// It is built once, on first use, and shared by every DB handle that does not
// carry its own.
//
// Building from the environment is deliberate: the process that writes an
// event holds the same credential values that a captured command could have
// printed, so the value layer knows the exact strings to look for.
func ProcessRedactor() *payload.Redactor {
	processRedactorOnce.Do(func() {
		processRedactor = payload.NewEnvRedactor()
	})
	return processRedactor
}

// SetRedactor overrides the redactor this DB handle applies at write time.
//
// Production code does not call this — Open leaves the handle on
// ProcessRedactor. It exists so a test can install a redactor built from
// synthetic values without touching the process environment.
//
// Passing nil restores the process default. To switch redaction off entirely
// for a test, pass payload.NewRedactor(nil) with no values; the shape layer
// still runs, which is the production guarantee under test.
func (d *DB) SetRedactor(r *payload.Redactor) {
	d.redactorMu.Lock()
	defer d.redactorMu.Unlock()
	d.redactor = r
}

// redactorFor returns the redactor this handle must apply.
func (d *DB) redactorFor() *payload.Redactor {
	d.redactorMu.RLock()
	r := d.redactor
	d.redactorMu.RUnlock()
	if r != nil {
		return r
	}
	return ProcessRedactor()
}

// redactPayload applies the write-time redaction to a raw payload string.
func (d *DB) redactPayload(s string) string {
	return d.redactorFor().Redact(s)
}
