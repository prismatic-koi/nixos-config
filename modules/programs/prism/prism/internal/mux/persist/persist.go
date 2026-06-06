// Package persist is the snapshot/restore layer that keeps the prism-native
// multiplexer's in-memory model alive across process restarts (#2156).
//
// The on-disk artefact is a single JSON document at
// `$XDG_STATE_HOME/prism/mux/session.json` (falling back to
// `$HOME/.local/state/prism/mux/session.json`). It is rewritten:
//
//   - On a configurable interval (DefaultInterval = 30 s), so an unexpected
//     daemon crash loses at most ~30 s of structural change to the session
//     tree.
//   - Once more on graceful shutdown (SIGTERM, `prismd mux stop`), so the
//     post-restart tree matches the pre-shutdown tree exactly when the
//     daemon comes back cleanly.
//
// Atomic writes
// -------------
//
// Save writes to a sibling tempfile in the same directory and renames it
// over the target path. The rename is atomic on every filesystem prism
// supports (ext4, btrfs, zfs, APFS), so a crash mid-write cannot leave a
// torn JSON document where the next startup will trip on it. The tempfile
// is removed on every failure path before Save returns.
//
// Schema versioning
// -----------------
//
// The wire format carries a top-level `schema_version` integer so future
// format changes do not silently corrupt the user's state. The current
// version is [CurrentSchemaVersion]. A snapshot whose schema_version does
// not match is rejected with [ErrUnknownSchemaVersion]; an older daemon
// reading a newer file falls back to an empty tree rather than guessing
// at fields it does not understand, and a newer daemon will explicitly
// branch on the version when v2 lands.
//
// Corrupt-snapshot handling
// -------------------------
//
// [Load] is strict: any failure (truncation, invalid JSON, schema
// mismatch, internal tree-invariant violation) returns a wrapped sentinel
// — [ErrCorrupt] or [ErrUnknownSchemaVersion]. The server's startup path
// uses [LoadOrEmpty] instead, which logs the failure and returns a fresh
// empty tree. Per the AC on #2156, corrupt persisted state must never
// crash the daemon — at worst it costs the user their session-tree
// layout, which they can rebuild interactively.
//
// What survives a restart
// -----------------------
//
// The persist layer reconstructs the multiplexer's structural model — the
// repo cluster / session / review-subsession hierarchy, pane names per
// session, focus pointers. It does NOT itself re-attach to or revive any
// child PTY process. Whether the work in a pane survives depends on what
// was running there:
//
//   - `pi` (the prism agent harness) survives in the sense that matters
//     for the user: each pi instance carries its own session_id and can
//     be re-attached by re-launching `pi --resume <session_id>`. The
//     reattachment itself is wired up in PR #2157 (lifecycle), which is
//     where the prismd mux process learns about subprocess IDs and can
//     decide per pane whether to re-spawn.
//   - `nvim` does NOT naturally survive process exit. Restored sessions
//     whose pane was an nvim instance render as "process exited" stubs
//     in the renderer (#2152); the user re-launches nvim manually if
//     they want it back.
//   - Transient shells (`bash`, `zsh`) do not survive — same path as
//     nvim: the pane reappears in the sidebar but with no live process
//     behind it until the user re-runs whatever they had open.
//
// The renderer documents the visual treatment for "session restored,
// process exited" panes; this package is concerned only with the
// structural restore.
//
// Concurrency
// -----------
//
// Save calls *pane.SessionTree.MarshalJSON, which acquires the tree's
// RLock — concurrent mutators are not blocked, and Save observes a
// consistent snapshot atomically. The [Snapshotter] goroutine and the
// server's mutator goroutines are therefore safe to run concurrently
// without external coordination.
package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// CurrentSchemaVersion is the schema version this package emits on Save
// and accepts on Load. A snapshot with any other value is rejected with
// [ErrUnknownSchemaVersion]. Bump this — and the matching branch in
// decode — whenever the wire format changes.
const CurrentSchemaVersion = 1

// DefaultInterval is the default periodic-snapshot interval used by
// [Snapshotter] when Interval is zero. 30 s is the figure named in the
// #2156 spec: short enough that an unplanned crash loses at most one
// half-minute of structural change to the session tree, long enough that
// the I/O cost is invisible in steady state.
const DefaultInterval = 30 * time.Second

// snapshotBaseName is the directory under $XDG_STATE_HOME/prism that
// holds mux state. Kept separate from the prism DB and other prism state
// files so a `find $XDG_STATE_HOME/prism/mux -delete` only nukes mux
// state.
const snapshotBaseName = "mux"

// snapshotFileName is the basename of the snapshot file inside
// snapshotBaseName.
const snapshotFileName = "session.json"

// Sentinel errors. The server's startup path matches with errors.Is so
// the "corrupt → start fresh" branch is one line.
var (
	// ErrCorrupt is returned by [Load] when the snapshot file exists but
	// cannot be decoded into a valid session tree. The wrapped error
	// describes the specific failure (JSON syntax error, truncation,
	// tree-invariant violation). Persisted state having become corrupt
	// is treated as a "start with an empty tree" condition by the
	// server, not a crash.
	ErrCorrupt = errors.New("persist: snapshot is corrupt")

	// ErrUnknownSchemaVersion is returned by [Load] when the snapshot's
	// schema_version field is missing, zero, or does not match
	// [CurrentSchemaVersion]. Distinct from [ErrCorrupt] because the
	// remediation differs: corrupt state means "fall back to empty";
	// unknown version means "the file is well-formed but is from a
	// daemon I do not understand", which is the same fallback today but
	// will become a migration hook when v2 lands.
	ErrUnknownSchemaVersion = errors.New("persist: unknown schema version")
)

// fileFormat is the on-disk wire format. Kept package-private — callers
// pass and receive *pane.SessionTree, and the schema-version wrapper is
// invisible to them. Tree is held as json.RawMessage so the
// schema-version check happens before the tree is decoded — a snapshot
// from a future daemon with an incompatible tree shape rejects cleanly
// at the version check, not partway through pane invariant validation.
type fileFormat struct {
	SchemaVersion int             `json:"schema_version"`
	Tree          json.RawMessage `json:"tree"`
}

// DefaultPath returns the canonical snapshot path for the current user.
// Resolution order, matching the existing prism state-file conventions
// (`internal/config/active_profile.go`, `internal/feedback/feedback.go`):
//
//  1. $XDG_STATE_HOME/prism/mux/session.json — if XDG_STATE_HOME is set.
//  2. $HOME/.local/state/prism/mux/session.json — fallback.
//
// XDG_STATE_HOME is checked first so the nix-build sandbox (where
// HOME=/homeless-shelter is intentionally unwritable) and the test
// suite (which sets XDG_STATE_HOME=t.TempDir()) both work without
// touching $HOME.
//
// Returns ("", error) if neither environment variable is discoverable.
func DefaultPath() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "prism", snapshotBaseName, snapshotFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("persist: cannot resolve snapshot path — neither XDG_STATE_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "state", "prism", snapshotBaseName, snapshotFileName), nil
}

// Save writes the tree to path atomically. The parent directory is
// created with 0o755 if it does not exist; the file is written with
// 0o644.
//
// Atomicity: the data is written to a sibling tempfile in the same
// directory (filepath.Dir(path)) and then renamed onto path. Same-
// directory rename is atomic on every POSIX-conforming filesystem
// prism cares about, so a crash mid-Save cannot leave a torn file at
// path — the next Load sees either the previous good snapshot or the
// new one, never half of either.
//
// On any failure the tempfile is removed before Save returns, so a
// failed Save does not leave debris next to the snapshot file.
func Save(path string, tree *pane.SessionTree) error {
	if path == "" {
		return errors.New("persist: Save: empty path")
	}
	if tree == nil {
		return errors.New("persist: Save: nil tree")
	}

	treeJSON, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("persist: marshal tree: %w", err)
	}
	wire := fileFormat{
		SchemaVersion: CurrentSchemaVersion,
		Tree:          treeJSON,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("persist: marshal wire format: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persist: create snapshot dir %s: %w", dir, err)
	}

	// Atomic write: tempfile in the same directory + rename.
	tmp, err := os.CreateTemp(dir, snapshotFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("persist: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("persist: write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("persist: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("persist: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("persist: rename %s → %s: %w", tmpPath, path, err)
	}
	return nil
}

// Load reads a snapshot from path and returns the reconstructed tree.
//
// Return contract:
//   - (tree, nil) — snapshot exists, is well-formed, and round-tripped
//     cleanly through the pane package's invariant checks.
//   - (nil, os.ErrNotExist) — wrapped, via errors.Is(err, os.ErrNotExist).
//     First-run is not an error condition; the server's startup path
//     handles this with `errors.Is(err, os.ErrNotExist)`.
//   - (nil, ErrUnknownSchemaVersion) — the file decoded but
//     schema_version is not [CurrentSchemaVersion].
//   - (nil, ErrCorrupt) — every other failure mode: invalid JSON,
//     truncation, missing schema_version field, tree-invariant
//     violation reported by pane.UnmarshalJSON. The underlying error
//     is wrapped so callers can inspect the cause without losing
//     sentinel discoverability.
//
// Load never panics, never partially populates a tree, and never
// touches the on-disk file other than reading it.
func Load(path string) (*pane.SessionTree, error) {
	if path == "" {
		return nil, errors.New("persist: Load: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// os.ErrNotExist passes through verbatim so callers can branch
		// on it with errors.Is — first-run is not an error.
		return nil, err
	}

	var wire fileFormat
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("%w: decode wire format: %v", ErrCorrupt, err)
	}
	if wire.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d",
			ErrUnknownSchemaVersion, wire.SchemaVersion, CurrentSchemaVersion)
	}
	if len(wire.Tree) == 0 {
		// Schema version says "this is mine" but the tree payload is
		// missing entirely — that's corruption, not a missing-file
		// fallthrough.
		return nil, fmt.Errorf("%w: schema_version %d present but tree payload is empty",
			ErrCorrupt, wire.SchemaVersion)
	}

	tree := pane.New()
	if err := json.Unmarshal(wire.Tree, tree); err != nil {
		// pane.UnmarshalJSON returns ErrInconsistent for invariant
		// violations and json syntax errors for parse failures. Both
		// surface to the caller as ErrCorrupt — the consumer
		// (Snapshotter / server) treats them identically.
		return nil, fmt.Errorf("%w: decode tree: %v", ErrCorrupt, err)
	}
	return tree, nil
}

// LoadOrEmpty is the convenience wrapper for the server's startup path:
// it calls [Load], logs any failure to logger (or [log.Default] if
// logger is nil), and returns an empty tree on every error path so the
// daemon can come up cleanly even when the snapshot is corrupt or from
// an unknown schema version.
//
// Per the #2156 AC, corrupt persisted state must never crash the
// daemon. LoadOrEmpty is the API that enforces that contract — the
// caller does not have to write the fallback branch.
//
// The returned tree is always non-nil. A missing snapshot is treated
// as a silent first-run condition (no log line); any other failure is
// logged at INFO level on logger.
func LoadOrEmpty(path string, logger *log.Logger) *pane.SessionTree {
	if logger == nil {
		logger = log.Default()
	}
	tree, err := Load(path)
	if err == nil {
		return tree
	}
	if errors.Is(err, os.ErrNotExist) {
		// First-run is not a failure condition — no log line.
		return pane.New()
	}
	switch {
	case errors.Is(err, ErrUnknownSchemaVersion):
		logger.Printf("persist: %s has unknown schema version, starting with empty tree: %v", path, err)
	case errors.Is(err, ErrCorrupt):
		logger.Printf("persist: %s is corrupt, starting with empty tree: %v", path, err)
	default:
		// Permission denied, EIO, etc. — log and fall back.
		logger.Printf("persist: cannot read %s, starting with empty tree: %v", path, err)
	}
	return pane.New()
}

// Snapshotter periodically writes a tree to disk and writes one final
// snapshot on graceful shutdown.
//
// The zero value is not ready for use; populate Path and Tree before
// calling [Snapshotter.Run]. Interval defaults to [DefaultInterval] when
// zero; Logger defaults to [log.Default] when nil.
type Snapshotter struct {
	// Path is the absolute snapshot path. Typically the result of
	// [DefaultPath]; tests pass a t.TempDir() path.
	Path string

	// Tree is the session tree to snapshot. The Snapshotter does not
	// own the tree; the server retains exclusive ownership of mutating
	// operations. Save uses the tree's RLock, so concurrent reads /
	// writes by the server are race-free.
	Tree *pane.SessionTree

	// Interval is the periodic-snapshot interval. Zero (the default)
	// means [DefaultInterval] = 30 s.
	Interval time.Duration

	// Logger receives one line on every periodic-snapshot failure and
	// one line on a final-snapshot failure. Nil (the default) means
	// [log.Default].
	Logger *log.Logger
}

// Run blocks until ctx is cancelled, writing the tree to disk every
// Interval (default [DefaultInterval]) and once more on shutdown.
//
// Shutdown semantics: when ctx is cancelled, Run writes one final
// snapshot before returning. The final-snapshot error is returned
// from Run so the caller can decide whether to propagate it (the
// server in #2153 logs and continues — losing the very last snapshot
// to e.g. a full disk is not a reason to keep the daemon alive past
// shutdown).
//
// Periodic-snapshot errors are logged on Logger but never returned —
// a transient I/O failure should not abort the long-running goroutine.
// The next tick will retry.
//
// Run returns nil on a clean ctx-cancel with a successful final
// snapshot, ctx.Err() wrapped together with the final-snapshot error
// otherwise, or just the final-snapshot error if it failed but ctx
// was already cancelled (the common case).
func (s *Snapshotter) Run(ctx context.Context) error {
	if s.Path == "" {
		return errors.New("persist: Snapshotter.Run: empty Path")
	}
	if s.Tree == nil {
		return errors.New("persist: Snapshotter.Run: nil Tree")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Graceful-shutdown final snapshot. We deliberately do
			// not check ctx again here — a final snapshot on
			// shutdown is the contract.
			if err := Save(s.Path, s.Tree); err != nil {
				logger.Printf("persist: final snapshot on shutdown failed: %v", err)
				return fmt.Errorf("persist: final snapshot: %w", err)
			}
			return nil
		case <-ticker.C:
			if err := Save(s.Path, s.Tree); err != nil {
				// Periodic failures are logged but do not abort
				// the goroutine — the next tick retries, and a
				// transient ENOSPC / EIO should not take the
				// daemon down with it.
				logger.Printf("persist: periodic snapshot failed: %v", err)
			}
		}
	}
}
