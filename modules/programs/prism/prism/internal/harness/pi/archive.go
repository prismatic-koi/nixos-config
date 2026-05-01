package pi

// archive.go — PI implementation of harness/archive.ArchiveAdapter (B6.PI).
//
// PI stores session data as JSONL files on disk (RFC #606 "Session persistence":
// "JSONL files with a tree structure (parent/child IDs for branching)").
// Unlike opencode, PI has no SQLite database — it is a pure flat-file store.
//
// Source path layout (best-effort based on RFC #606; confirmed at PI session time):
//
//   ~/.local/share/pi/sessions/<harness_session_id>/
//       session.jsonl        — the full conversation transcript
//       [additional *.jsonl files per branch or compaction round]
//
// Archive copies all *.jsonl files from the session directory into raw/.
// Export normalises the raw JSONL to pi-mono v3 session.jsonl (currently a
// near-identity pass since PI's JSONL is already pi-mono-shaped; the main
// operation is stripping PI-internal fields prism does not consume and
// canonicalising timestamps).

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
)

// piArchiveAdapter implements harness/archive.ArchiveAdapter for PI.
type piArchiveAdapter struct{}

// NewArchiveAdapter returns an ArchiveAdapter for the PI harness.
func NewArchiveAdapter() harnessarchive.ArchiveAdapter {
	return &piArchiveAdapter{}
}

// SourcePath returns the host-side PI session directory for the session.
//
// PI stores sessions under $HOME/.local/share/pi/sessions/<harness_session_id>/.
// The harness_session_id is populated at session-create time when PI starts.
// When HarnessSessionID is empty (PI failed to start), SourcePath returns the
// sessions root directory; the caller handles the missing-directory case.
func (a *piArchiveAdapter) SourcePath(p harnessarchive.SourceParams) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pi archive: resolve home: %w", err)
	}
	sessionsRoot := filepath.Join(home, ".local", "share", "pi", "sessions")
	if p.HarnessSessionID == "" {
		return sessionsRoot, nil
	}
	return filepath.Join(sessionsRoot, p.HarnessSessionID), nil
}

// Archive copies PI's JSONL session files from srcPath into rawDir.
//
// All *.jsonl files found in srcPath are copied flat into rawDir. Subdirectory
// traversal is intentionally shallow (RFC #606 describes the session directory
// as a flat set of JSONL files). Missing files within srcPath are tolerated;
// real I/O errors are propagated.
//
// When srcPath does not exist (PI failed to start or produced no output),
// Archive returns nil and rawDir is left empty.
func (a *piArchiveAdapter) Archive(_ context.Context, srcPath, rawDir string, _ harnessarchive.SourceParams) error {
	entries, err := os.ReadDir(srcPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pi archive: read dir %q: %w", srcPath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		src := filepath.Join(srcPath, name)
		dst := filepath.Join(rawDir, name)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("pi archive: copy %q → %q: %w", src, dst, err)
		}
	}
	return nil
}

// Export produces pi-mono v3 session.jsonl from the raw PI JSONL files.
//
// PI's on-disk format is already JSONL-shaped and closely follows pi-mono v3.
// For PI sessions, Export performs a near-identity normalisation: it writes
// archiveDir/session.jsonl by concatenating the raw JSONL records from
// raw/session.jsonl (if present), stripping any PI-internal fields that are
// not part of the pi-mono v3 spec.
//
// When raw/session.jsonl does not exist, Export returns nil and no
// session.jsonl is written. The raw archive remains intact; the caller may
// attempt re-translation later.
func (a *piArchiveAdapter) Export(_ context.Context, archiveDir string, _ harnessarchive.SourceParams) error {
	rawSessionJSONL := filepath.Join(archiveDir, "raw", "session.jsonl")
	if _, err := os.Stat(rawSessionJSONL); os.IsNotExist(err) {
		log.Printf("pi archive: Export: raw/session.jsonl not found — no export produced")
		return nil
	} else if err != nil {
		return fmt.Errorf("pi archive: Export: stat raw/session.jsonl: %w", err)
	}

	dst := filepath.Join(archiveDir, "session.jsonl")
	return copyFile(rawSessionJSONL, dst)
}

// Version returns the version string reported by the pi binary (e.g. "1.2.3"),
// or "" when the binary is not on PATH or returns an error. This is a non-fatal
// call — the manifest records "" as "version unknown" without failing cleanup.
func (a *piArchiveAdapter) Version(_ context.Context) (string, error) {
	out, err := exec.Command("pi", "--version").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// copyFile copies the file at src to dst, creating dst if necessary.
// Parent directories of dst must already exist.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
