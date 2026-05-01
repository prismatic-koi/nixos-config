package opencode

// archive.go — opencode implementation of harness/archive.ArchiveAdapter.
//
// The four methods map onto the existing opencode-specific logic:
//
//   - SourcePath: resolves the storage root (used to derive the DB path)
//   - Archive:    calls archive.CopySessionFiles → exportSessionFromDB
//   - Export:     calls piexport.Translate
//   - Version:    calls exec.Command("opencode", "--version")

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/archive"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	"github.com/prismatic-koi/prism/internal/piexport"
)

// opencodeArchiveAdapter implements harness/archive.ArchiveAdapter for opencode.
type opencodeArchiveAdapter struct{}

// NewArchiveAdapter returns an ArchiveAdapter for the opencode harness.
func NewArchiveAdapter() harnessarchive.ArchiveAdapter {
	return &opencodeArchiveAdapter{}
}

// SourcePath returns the host-side opencode storage root for the session.
//
// Isolation mode mapping:
//   - host / bwrap / sandbox-exec: $HOME/.local/share/opencode/storage
//   - podman: $HOME/.local/share/opencode/prism-sessions/<containerName>/storage
func (a *opencodeArchiveAdapter) SourcePath(p harnessarchive.SourceParams) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("opencode archive: resolve home: %w", err)
	}

	switch p.IsolationMode {
	case "host", "bwrap", "sandbox-exec":
		return filepath.Join(home, ".local", "share", "opencode", "storage"), nil
	case "podman":
		containerName := containerNameForSession(p.SessionName)
		return filepath.Join(home, ".local", "share", "opencode", "prism-sessions", containerName, "storage"), nil
	default:
		return "", fmt.Errorf("opencode archive: unsupported isolation mode %q", p.IsolationMode)
	}
}

// containerNameForSession returns the podman container name for a session,
// mirroring the logic in internal/container.NameForSession and the copy in
// internal/archive/archive.go:containerNameForSession.
func containerNameForSession(sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "@", "-")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, ".", "-")
	safe = strings.ReplaceAll(safe, "~", "-")
	return "prism-" + safe
}

// Archive exports the opencode session from the SQLite DB into rawDir.
// srcPath is the storage root returned by SourcePath; the DB lives at
// <parent of srcPath>/opencode-stable.db.
//
// When p.HarnessSessionID is empty (opencode failed to start), this is a no-op
// and rawDir is left empty.
func (a *opencodeArchiveAdapter) Archive(_ context.Context, srcPath, rawDir string, p harnessarchive.SourceParams) error {
	if p.HarnessSessionID == "" {
		return nil
	}
	// The opencode-stable.db lives one level above the storage/ directory.
	dbPath := filepath.Join(filepath.Dir(srcPath), "opencode-stable.db")
	return archive.CopySessionFiles(p.HarnessSessionID, dbPath, rawDir)
}

// Export translates the raw archive at archiveDir into pi-mono JSONL format by
// calling piexport.Translate. Failure is non-fatal — the caller logs the error
// and the raw archive remains intact for re-translation later.
func (a *opencodeArchiveAdapter) Export(_ context.Context, archiveDir string, _ harnessarchive.SourceParams) error {
	return piexport.Translate(archiveDir)
}

// Version returns the version string reported by the opencode binary (e.g.
// "1.1.30"), or "" when the binary is not on PATH or returns an error.
func (a *opencodeArchiveAdapter) Version(_ context.Context) (string, error) {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		// Non-fatal: return empty string with nil error so callers treat the
		// empty string as "version unknown" rather than an error condition.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
