// Package proglog is a minimal log-level shim for prism's stderr diagnostics.
//
// Prism scatters a number of `fmt.Fprintf(os.Stderr, "[prism ...] ...", ...)`
// and `fmt.Fprintf(os.Stderr, "[timing] ...")` call sites throughout the CLI.
// Many of these are transient retry/probe diagnostics that recover
// automatically — they flash briefly in the tmux pane and add noise. A few
// are genuine user-impacting errors that must always be visible.
//
// This package introduces a single configuration knob — the PRISM_LOG_LEVEL
// environment variable — and four leveled emit functions:
//
//	proglog.Errorf(format, args...) // level=error, always on
//	proglog.Warnf (format, args...) // level=warn,  on at warn/info/debug
//	proglog.Infof (format, args...) // level=info,  on at info/debug
//	proglog.Debugf(format, args...) // level=debug, on at debug
//
// Each function writes to os.Stderr with fmt.Fprintf semantics iff the
// message's level is at or below the effective level. The effective level
// is read once from PRISM_LOG_LEVEL on first use and cached via sync.Once.
//
// Recognised values (case-insensitive): "error", "warn", "info", "debug".
// Unrecognised or unset → "error".
//
// Design constraints (see issue #1818):
//
//   - No log framework dependencies. Just fmt.Fprintf gated on a cached level.
//   - No prefix added by the helper itself. Callers preserve their existing
//     `[prism ...]` / `[timing]` prefix in the format string so downstream
//     grep diagnostics keep working.
//   - No timestamp, no structured fields, no rotation. Stderr only.
package proglog

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Level is the verbosity level of a single message or the effective threshold.
// Lower values are more important; a message at level N is emitted iff
// effective >= N.
type Level int

const (
	// LevelError is the default and the lowest (most important) level.
	LevelError Level = 0
	// LevelWarn is for recoverable issues worth surfacing.
	LevelWarn Level = 1
	// LevelInfo is for informational progress messages.
	LevelInfo Level = 2
	// LevelDebug is for transient retry/probe diagnostics and timing markers.
	LevelDebug Level = 3
)

// envVar is the environment variable consulted on first use.
const envVar = "PRISM_LOG_LEVEL"

var (
	once     sync.Once
	cached   Level
	writerMu sync.Mutex
	writer   io.Writer = os.Stderr
)

// ParseLevel maps a case-insensitive string to a Level. An unrecognised or
// empty value maps to LevelError. The returned bool indicates whether the
// input was a recognised level name — callers that want to log "fell back to
// default" can use it, but the helper itself never warns on parse failure.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError, true
	case "warn", "warning":
		return LevelWarn, true
	case "info":
		return LevelInfo, true
	case "debug":
		return LevelDebug, true
	default:
		return LevelError, false
	}
}

// effective returns the cached effective level, reading PRISM_LOG_LEVEL on
// first call. The env var is consulted exactly once per process; subsequent
// changes to the environment have no effect.
func effective() Level {
	once.Do(func() {
		lvl, _ := ParseLevel(os.Getenv(envVar))
		cached = lvl
	})
	return cached
}

// emit writes a single formatted message to the configured writer iff the
// message's level is at or below the effective level.
func emit(msgLevel Level, format string, args ...any) {
	if effective() < msgLevel {
		return
	}
	writerMu.Lock()
	w := writer
	writerMu.Unlock()
	_, _ = fmt.Fprintf(w, format, args...)
}

// Errorf writes an error-level message. Always emitted (even at the default
// level).
func Errorf(format string, args ...any) { emit(LevelError, format, args...) }

// Warnf writes a warning-level message. Emitted when PRISM_LOG_LEVEL is
// warn, info, or debug.
func Warnf(format string, args ...any) { emit(LevelWarn, format, args...) }

// Infof writes an info-level message. Emitted when PRISM_LOG_LEVEL is info
// or debug.
func Infof(format string, args ...any) { emit(LevelInfo, format, args...) }

// Debugf writes a debug-level message. Emitted only when PRISM_LOG_LEVEL=debug.
func Debugf(format string, args ...any) { emit(LevelDebug, format, args...) }
