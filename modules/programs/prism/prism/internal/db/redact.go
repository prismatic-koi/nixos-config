// Capture-path secret redaction at DB-write time.
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
	"os"
	"sync"

	"github.com/prismatic-koi/prism/internal/config"
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
//
// It also reads the credential files that prism gives to sessions
// (HostCredentialFiles). The sidecar of a session does not hold the path
// variable of such a file, so its environment alone does not give the value.
func ProcessRedactor() *payload.Redactor {
	processRedactorOnce.Do(func() {
		processRedactor = newProcessRedactor()
	})
	return processRedactor
}

func newProcessRedactor() *payload.Redactor {
	return payload.NewRedactorFromEnvironAndFiles(os.Environ(), HostCredentialFiles())
}

// HostCredentialFiles returns the credential files that this host gives to
// sessions as a file, keyed by marker name: today the UniFi API key in
// unifi.json (issue #3472). The value layer of a host process reads them for
// every session, whatever its repo: a host-mode session of any repo can read
// the key file, so control 2 must know the value in all of them. A file that
// cannot be read gives no value, and the shape layer still runs.
func HostCredentialFiles() map[string]string {
	u, err := config.LoadUniFi()
	if err != nil || u.APIKeyPath == "" {
		return nil
	}
	return map[string]string{payload.UniFiAPIKeyName: u.APIKeyPath}
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

// redactPayload applies the write-time redaction to a raw payload column.
//
// It uses RedactJSON, not Redact: both payload columns hold a JSON document,
// and a flat text pass lets the multi-line private-key shape span a JSON
// delimiter — which either stores invalid JSON or silently deletes the fields
// between the two halves of the match. Redaction runs at INSERT, so the
// intact original is never stored and the damage is permanent. See
// payload.Redactor.RedactJSON for the reproducer.
//
// A payload that is not a single JSON value falls back to the flat pass
// inside RedactJSON, which is correct: there are no delimiters to protect.
func (d *DB) redactPayload(s string) string {
	return d.redactorFor().RedactJSON(s)
}
