package session

// Per-agent startup log (#1051 Piece B).
//
// SpawnSession writes a small, append-mode log file to the per-session run
// directory describing the spawn-time sequence: which session, role, port,
// isolation mode; when the sidecar was started; when the tmux pane was
// created; and a pointer to where bwrap stderr will land (agent-run.log)
// after the pane hands control to `prism agent-run`. The intent is forensic:
// when an agent fails to come up and the operator inspects logs, this file
// is the breadcrumb trail covering the gap between "session created in DB"
// and "first SSE event arrives at the sidecar".
//
// The file lives in the same directory as agent-run.log:
//
//	$XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/agent-startup.log
//
// The per-session subdirectory uses the same SessionDirName-derived 12-hex
// SHA-256 prefix as the host-API socket and agent-run.log so all three files
// are co-located on disk and discoverable from a single `ls`. See
// internal/session/sidecar.go:SessionDirName for the formula and #1050 for
// the sun_path budget that motivated the hashing.
//
// Pre-creating the directory at SpawnSession time means the file path is
// always valid even if `prism agent-run` never reaches its own log-open call
// (the failure mode #1051 reports — opencode never binds and the per-session
// run dir was never created at all).

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AgentStartupLogPath returns the agent-startup.log file path for the named
// session. It lives next to AgentRunLogPath so the two are co-located on disk
// and discoverable from a single `ls $XDG_STATE_HOME/prism/run/<session>/`.
func AgentStartupLogPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "agent-startup.log"), nil
}

// startupLogger wraps an *os.File with the conventions used across the
// agent-startup.log family of writes: timestamps in RFC3339, line-prefixed
// "[startup] " marker so the file greps coherently against bwrap stderr
// later, and tolerant best-effort writes (a write error never fails the
// caller — the spawn must continue even when the log is unwritable).
type startupLogger struct {
	f *os.File
}

// openStartupLog returns a startupLogger writing to AgentStartupLogPath, or
// nil when the file cannot be created. The directory is mkdir'd 0o700 so the
// per-session run dir is in place before any later writers (agent-run, the
// sidecar) attempt to create files inside it. All errors are reported to
// stderr as warnings — the spawn never fails because the log is unwritable.
func openStartupLog(sessionName string) *startupLogger {
	path, err := AgentStartupLogPath(sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent-startup log path: %v — continuing without startup log\n", err)
		return nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		fmt.Fprintf(os.Stderr, "warning: agent-startup log dir %s: %v — continuing without startup log\n", filepath.Dir(path), mkErr)
		return nil
	}
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if openErr != nil {
		fmt.Fprintf(os.Stderr, "warning: agent-startup log %s: %v — continuing without startup log\n", path, openErr)
		return nil
	}
	return &startupLogger{f: f}
}

// log writes a single timestamped line. Safe to call on a nil receiver — when
// the startup log could not be opened, all .log() calls are no-ops.
func (s *startupLogger) log(format string, args ...any) {
	if s == nil || s.f == nil {
		return
	}
	line := fmt.Sprintf("[%s] [startup] %s\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		fmt.Sprintf(format, args...),
	)
	_, _ = s.f.WriteString(line)
}

// close releases the file handle. Safe to call on nil.
func (s *startupLogger) close() {
	if s == nil || s.f == nil {
		return
	}
	_ = s.f.Close()
}

// AgentStartupLogExists reports whether an agent-startup.log file exists for
// the named session. Used by `prism logs` to surface a pointer when the
// regular sidecar log is empty or stuck on SSE-retry noise — see
// cmd/logs.go for the pointer text.
func AgentStartupLogExists(sessionName string) bool {
	path, err := AgentStartupLogPath(sessionName)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}
