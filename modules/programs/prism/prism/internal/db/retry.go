package db

import (
	"strings"
	"time"
)

// IsSQLiteBusy reports whether err is a SQLite SQLITE_BUSY or SQLITE_LOCKED
// error. These errors indicate that a concurrent writer holds the write lock
// and the caller should retry after a short backoff.
//
// The check is performed by string-matching the error message rather than
// importing modernc.org/sqlite directly, avoiding a package-level dependency
// on the driver in higher-level code. The modernc driver formats these errors
// as:
//
//	"db: upsert status: database is locked (5) (SQLITE_BUSY)"
//	"db: upsert status: database is locked (6) (SQLITE_LOCKED)"
//
// "database is locked" is common to both, so we match on that phrase.
func IsSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked")
}

// WithBusyRetry calls fn up to attempts times, sleeping backoff between each
// retry if fn returns an SQLITE_BUSY / SQLITE_LOCKED error. It returns the
// last error (or nil on success). Non-BUSY errors short-circuit immediately
// without further retries.
//
// The canonical call site uses attempts=3 and backoff=10ms, which matches the
// existing inline retry in the /review pre-emptive write (host_api.go).
func WithBusyRetry(attempts int, backoff time.Duration, fn func() error) error {
	var last error
	for i := 0; i < attempts; i++ {
		last = fn()
		if last == nil {
			return nil
		}
		if !IsSQLiteBusy(last) {
			// Non-transient failure — no point retrying.
			return last
		}
		if i < attempts-1 {
			time.Sleep(backoff)
		}
	}
	return last
}
