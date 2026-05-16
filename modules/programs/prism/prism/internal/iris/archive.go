package iris

// archive.go — standalone iris session archive operation (#1697).
//
// ArchiveSession is the standalone analogue of the archive step inside
// CleanupSession. It copies the pi JSONL file for a session into the iris
// archive tree and returns the destination path — without touching the
// sessions row, the run directory, the per-session log file, or the
// worktree. The session can keep running afterwards.
//
// This is a deliberately narrower contract than CleanupSession:
//
//   - Cleanup is a teardown verb. It marks the row ended, removes run
//     artefacts, and (optionally) deletes the worktree. It is a one-shot
//     destructive operation.
//   - Archive is a copy verb. It snapshots the pi JSONL into the archive
//     tree at the documented path. It is non-destructive and may be re-run
//     while the session is still active.
//
// The two share the same path layout
// (<ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl) and the same
// archive-writing helper (archiveSessionJSONL) so a session that is later
// cleaned up will overwrite/match the path produced by an earlier
// standalone archive — no parallel layouts to keep in sync.
//
// Daemon dependency: none. ArchiveSession reads iris.db directly and
// performs a file copy. It works whether or not the iris daemon is running.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/db"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ArchiveConfig holds the parameters needed to run a standalone archive
// for a single session. It is a strict subset of CleanupConfig — the
// fields that drive cleanup-only behaviour (run dir removal, worktree
// removal) are deliberately absent.
type ArchiveConfig struct {
	// Database is the open iris DB.
	Database *db.DB
	// ArchiveRoot is the root of the iris archive tree
	// (e.g. ~/code/archives/iris/). Session JSONL files are copied to
	// <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl.
	ArchiveRoot string
	// PIAgentDir is the base directory where pi stores session files
	// (default: ~/.pi/agent/). When empty, defaults to ~/.pi/agent/.
	PIAgentDir string
}

// ArchiveResult records the outcome of a single ArchiveSession call.
type ArchiveResult struct {
	// Session is the resolved sessions row.
	Session *db.Session
	// ArchivePath is the destination of the archived session JSONL when
	// the archive step actually wrote a file. Empty when the source JSONL
	// did not exist (pi never wrote one) — see Skipped/SkipReason.
	ArchivePath string
	// Skipped is true when the archive step was a no-op because there was
	// nothing to archive (no harness session id on the row, or no JSONL on
	// disk). Skipped + nil error is the documented "empty JSONL → exit 0
	// with informative message" case from the spec.
	Skipped bool
	// SkipReason is a short human-readable explanation when Skipped=true.
	SkipReason string
}

// ArchiveSession copies the pi JSONL file for sessionName into the archive
// tree at the documented path and returns an ArchiveResult describing the
// outcome.
//
// Behaviour:
//
//   - The session row is resolved by session_name (most-recent incarnation,
//     matching the cleanup path). For instance-id lookups, callers should
//     resolve themselves and use ArchiveSessionRow.
//   - The session row is NOT modified. ArchiveSession is read-only against
//     the DB; the session keeps running after the call returns.
//   - When the source JSONL does not exist (pi never wrote one, or the
//     session was constructed without a worktree), ArchiveSession returns a
//     result with Skipped=true and a non-empty SkipReason. This is not an
//     error — callers should surface the reason to the user and exit 0.
//   - On success, ArchivePath points at
//     <ArchiveRoot>/<sessionName>/<instance_id>/raw/session.jsonl.
func ArchiveSession(ctx context.Context, cfg ArchiveConfig, sessionName string) (*ArchiveResult, error) {
	if cfg.Database == nil {
		return nil, errors.New("iris archive: Database is required")
	}
	if cfg.ArchiveRoot == "" {
		return nil, errors.New("iris archive: ArchiveRoot is required")
	}
	if sessionName == "" {
		return nil, errors.New("iris archive: session name is required")
	}

	sess, err := cfg.Database.MostRecentSessionForName(sessionName)
	if err != nil {
		return nil, fmt.Errorf("iris archive: lookup session %q: %w", sessionName, err)
	}
	if sess == nil {
		return nil, fmt.Errorf("iris archive: session %q not found", sessionName)
	}
	return ArchiveSessionRow(ctx, cfg, sess)
}

// ArchiveSessionByInstanceID is the instance-id lookup variant of
// ArchiveSession. The argument must be the full 36-char UUID. Returns the
// same error shape as ArchiveSession when the row is not found.
func ArchiveSessionByInstanceID(ctx context.Context, cfg ArchiveConfig, instanceID string) (*ArchiveResult, error) {
	if cfg.Database == nil {
		return nil, errors.New("iris archive: Database is required")
	}
	if cfg.ArchiveRoot == "" {
		return nil, errors.New("iris archive: ArchiveRoot is required")
	}
	if instanceID == "" {
		return nil, errors.New("iris archive: instance id is required")
	}

	sess, err := cfg.Database.SessionByInstanceID(instanceID)
	if err != nil {
		return nil, fmt.Errorf("iris archive: lookup session by instance %q: %w", instanceID, err)
	}
	if sess == nil {
		return nil, fmt.Errorf("iris archive: instance %q not found", instanceID)
	}
	return ArchiveSessionRow(ctx, cfg, sess)
}

// ArchiveSessionRow runs the archive copy for a pre-resolved sessions row.
// It is exported so callers that have already done their own lookup (e.g.
// CleanupSession or a future bulk-archive pass) can reuse the copy logic
// without paying for a second DB query. See ArchiveSession for behaviour.
func ArchiveSessionRow(ctx context.Context, cfg ArchiveConfig, sess *db.Session) (*ArchiveResult, error) {
	res := &ArchiveResult{Session: sess}

	path, skipReason, err := archiveSessionJSONL(ctx, cfg, sess)
	if err != nil {
		return res, fmt.Errorf("iris archive: %w", err)
	}
	if path == "" {
		res.Skipped = true
		res.SkipReason = skipReason
		return res, nil
	}
	res.ArchivePath = path
	return res, nil
}

// archiveSessionJSONL is the shared archive-writing helper used by both
// ArchiveSession (standalone) and CleanupSession (teardown). It locates
// the pi JSONL file for the session and copies it to
// <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl.
//
// Returns:
//
//   - (path, "", nil) on success — path is the destination session.jsonl.
//   - ("", reason, nil) when there is nothing to archive — reason is a
//     short human-readable explanation suitable for printing to the user.
//   - ("", "", err) on a real failure (mkdir / copy / adapter error).
//
// No archive directory is created when there is nothing to archive — the
// destination tree is touched only after a source file is located.
func archiveSessionJSONL(ctx context.Context, cfg ArchiveConfig, sess *db.Session) (string, string, error) {
	if sess.HarnessSessionID == nil || *sess.HarnessSessionID == "" || sess.Worktree == "" {
		// No harness session to archive (pi never started, or session was
		// constructed without a worktree). Treat as a documented no-op.
		return "", "no harness session id or worktree on sessions row", nil
	}

	piAgentDir := cfg.PIAgentDir
	if piAgentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve home: %w", err)
		}
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}

	// Use the shared pi archive adapter so the source-path encoding stays
	// in lock-step with the rest of the codebase. Locate the JSONL file
	// ourselves so the test-friendly PIAgentDir override is honoured.
	adapter := piharness.NewArchiveAdapter()
	encodedCWD := piharness.EncodePiCWD(sess.Worktree)
	cwdDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
	srcPath := scanForJSONL(cwdDir, *sess.HarnessSessionID)
	if srcPath == "" {
		// Nothing on disk — pi never wrote a JSONL file. Documented no-op:
		// must not create the archive directory tree.
		return "", fmt.Sprintf("no pi JSONL on disk under %s", cwdDir), nil
	}

	rawDir := filepath.Join(cfg.ArchiveRoot, sess.SessionName, sess.InstanceID, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir archive raw dir: %w", err)
	}
	p := harnessarchive.SourceParams{
		SessionName:      sess.SessionName,
		InstanceID:       sess.InstanceID,
		HarnessSessionID: *sess.HarnessSessionID,
		IsolationMode:    "host",
		Worktree:         sess.Worktree,
	}
	if err := adapter.Archive(ctx, srcPath, rawDir, p); err != nil {
		return "", "", fmt.Errorf("archive: %w", err)
	}
	dst := filepath.Join(rawDir, "session.jsonl")
	if _, err := os.Stat(dst); err != nil {
		// Adapter ran but the destination is missing — surface as an error
		// so callers know the archive step did not produce the documented
		// file.
		return "", "", fmt.Errorf("archive produced no session.jsonl at %q: %w", dst, err)
	}
	return dst, "", nil
}
