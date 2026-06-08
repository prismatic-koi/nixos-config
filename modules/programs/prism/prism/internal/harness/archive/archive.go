// Package archive defines the ArchiveAdapter interface for harness-specific
// archive operations.
//
// Each harness implementation (e.g. pi) provides an ArchiveAdapter that
// knows how to locate the harness's on-disk session storage, copy session files
// into the prism raw archive directory, export the archive to a downstream
// format, and report its own version string.
//
// The adapter is registered alongside the harness registration via
// harness.Registration.ArchiveAdapterFactory, and retrieved at cleanup time via
// harness.ArchiveAdapterFor(harnessName).
package archive

import "context"

// SourceParams holds the inputs needed to locate a harness session's on-disk
// storage root.
type SourceParams struct {
	// SessionName is the prism session name (e.g. "nixos-config@feature").
	SessionName string
	// InstanceID is the session's UUID (from sessions.instance_id).
	InstanceID string
	// HarnessSessionID is the harness-specific session ID
	// (e.g. pi's ses_<ULID>, from sessions.harness_session_id).
	// May be empty when the harness failed to start.
	HarnessSessionID string
	// IsolationMode is "podman", "bwrap", "sandbox-exec", or "host".
	IsolationMode string
	// Worktree is the absolute path of the session's worktree (e.g.
	// "/home/ben/code/nixos-config/feature"). Used by harnesses that
	// organise on-disk session storage by cwd (e.g. pi encodes the cwd into
	// the session directory name). May be empty for non-worktree sessions or
	// when the harness does not need it.
	Worktree string
}

// ArchiveAdapter is the interface each harness implements to plug into the
// prism archive pipeline.
//
// The three methods map directly to the three stages of the cleanup archive
// flow:
//
//  1. SourcePath — locate the harness session storage on the host filesystem.
//  2. Archive    — copy session files from the storage root into the archive dir.
//  3. Version    — report the harness binary version for the manifest.
//
// The historical Export step (separate "normalisation" pass that translated
// the raw archive into a downstream format) was removed when opencode left
// the codebase — pi is the only remaining harness and its on-disk JSONL is
// already in the downstream format, so Archive writes the final layout in a
// single step. If a second harness lands in future, this interface can
// re-grow an Export method then, with a real implementation.
type ArchiveAdapter interface {
	// SourcePath returns the host-side storage root directory for the session
	// described by p. The returned path is used as the srcPath argument to
	// Archive. An empty StorageRoot override in p means the adapter should
	// derive the path from p.IsolationMode and p.SessionName.
	SourcePath(p SourceParams) (string, error)

	// Archive copies the harness session files rooted at srcPath into
	// archiveDir. archiveDir is the per-session archive directory itself
	// (e.g. .../<repo>/<startedAtISO>_<instanceID>/) and already exists
	// when Archive is called. On success, archiveDir contains the
	// harness-specific session artifacts in their final on-disk layout —
	// no further normalisation step is run.
	Archive(ctx context.Context, srcPath, archiveDir string, p SourceParams) error

	// Version returns the harness binary version string (e.g. "1.1.30"), or
	// "" when the binary is not on PATH or returns an error. Called once per
	// cleanup session to populate the manifest's harnessVersion field.
	Version(ctx context.Context) (string, error)
}
