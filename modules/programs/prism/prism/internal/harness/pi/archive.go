package pi

// archive.go — PI implementation of harness/archive.ArchiveAdapter (B6.PI).
//
// PI stores session data as JSONL files on disk.
// Unlike some harnesses, PI has no SQLite database — it is a pure flat-file store.
//
// Source path layout (authoritative reference: docs/pi-rpc-interface.md Q5,
// confirmed against pi 0.75.3 dist/core/session-manager.js line 213):
//
//	~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl
//
// The <encoded-cwd> directory name is derived from the session's working
// directory (p.Worktree) via EncodePiCWD. The formula mirrors pi's own JS:
//
//	--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--
//
// Strip the leading '/' (or '\'), replace every '/', '\', and ':' with '-',
// wrap in "--…--". Example:
//
//	/home/ben/code/nixos-config/main → --home-ben-code-nixos-config-main--
//
// The session UUID (HarnessSessionID) is embedded in the filename, NOT used as
// a directory name. The adapter locates the session file by scanning the
// encoded-cwd directory for an entry matching "*_<HarnessSessionID>.jsonl".
//
// For sandbox-exec sessions (IsolationMode == "sandbox-exec"), PI writes to the
// per-session staging HOME instead of the real home directory (bug #1538 fix).
// The staging HOME is ~/.local/state/prism/sessions/<instance_id>/home/.
// The worktree path that pi sees as its CWD is the same as the host worktree
// path (sandbox-exec mounts it at its native path, only $HOME is remapped).
// Therefore the encoded-cwd is derived from p.Worktree in both cases; only the
// sessions root base (home) differs between host and sandbox-exec modes.
//
// Archive copies the single matched JSONL file into rawDir/session.jsonl so
// that Export (which expects raw/session.jsonl) finds it without further change.
// Export currently performs a near-identity normalisation pass (PI's on-disk
// JSONL is already pi-mono v3 shaped).

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/container"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	"github.com/prismatic-koi/prism/internal/session"
)

// piArchiveAdapter implements harness/archive.ArchiveAdapter for PI.
type piArchiveAdapter struct{}

// NewArchiveAdapter returns an ArchiveAdapter for the PI harness.
func NewArchiveAdapter() harnessarchive.ArchiveAdapter {
	return &piArchiveAdapter{}
}

// SourcePath returns the host-side PI session JSONL file path for the session.
//
// PI stores sessions at:
//
//	~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl
//
// where <encoded-cwd> is derived from p.Worktree via EncodePiCWD, and <uuid>
// matches p.HarnessSessionID. The adapter scans the encoded-cwd directory for
// a file whose name ends in "_<HarnessSessionID>.jsonl" and returns its path.
// If no match is found, a sentinel path inside the encoded-cwd dir is returned;
// Archive treats non-existent paths as a no-op.
//
// When HarnessSessionID is empty (harness failed to start), or Worktree is
// empty (non-worktree session), SourcePath returns the sessions root; Archive's
// os.IsNotExist handling treats it as a no-op.
//
// For sandbox-exec sessions (IsolationMode == "sandbox-exec"), PI writes under
// the staging HOME (<stagingHome>/.pi/agent/sessions/...) instead of the real
// home directory. The worktree path pi uses as its CWD is the same host path in
// both modes (sandbox-exec mounts the worktree at its native path), so the
// encoded-cwd is derived from p.Worktree in both cases. Bug #1538 fix: only
// the home base differs for sandbox-exec sessions.
//
// For bwrap sessions (IsolationMode == "bwrap"), PI writes into the per-session
// pi-agent staging directory bind-mounted into the sandbox at
// $PI_CODING_AGENT_DIR — on the host that resolves to
// <XDG_STATE_HOME>/prism/run/<sessionDirHash>/pi-agent/. Pi defaults
// --session-dir to <PI_CODING_AGENT_DIR>/sessions/, so the JSONL lands at
// <run>/pi-agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl — note there is no
// ".pi/agent" join under the run dir (the staging dir IS the agent dir). The
// session-dir hash is computed via session.SessionDirName (sha256 prefix). See
// bug #1814 for the analysis.
//
// See docs/pi-rpc-interface.md Q5 for the authoritative path specification.
func (a *piArchiveAdapter) SourcePath(p harnessarchive.SourceParams) (string, error) {
	sessionsRoot, err := piSessionsRoot(p)
	if err != nil {
		return "", err
	}

	if p.HarnessSessionID == "" || p.Worktree == "" {
		// No session ID or no worktree: return sessions root.
		// Archive's os.IsNotExist tolerance handles the missing-dir case.
		return sessionsRoot, nil
	}

	// Compute the encoded-cwd directory name from the worktree path.
	encodedCWD := EncodePiCWD(p.Worktree)
	cwdDir := filepath.Join(sessionsRoot, encodedCWD)

	// Scan the encoded-cwd directory for a file matching *_<HarnessSessionID>.jsonl.
	suffix := "_" + p.HarnessSessionID + ".jsonl"
	entries, err := os.ReadDir(cwdDir)
	if os.IsNotExist(err) {
		// Directory doesn't exist yet — return a sentinel path that Archive
		// will treat as missing (os.IsNotExist → no-op).
		return filepath.Join(cwdDir, "session_"+p.HarnessSessionID+".jsonl"), nil
	}
	if err != nil {
		return "", fmt.Errorf("pi archive: scan session dir %q: %w", cwdDir, err)
	}

	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(cwdDir, e.Name()), nil
		}
	}

	// No matching file — return a sentinel; Archive's IsNotExist path applies.
	log.Printf("pi archive: SourcePath: no file matching *%s in %q — session may not have written any data", suffix, cwdDir)
	return filepath.Join(cwdDir, "session_"+p.HarnessSessionID+".jsonl"), nil
}

// piSessionsRoot returns the per-mode sessions directory — the directory under
// which pi creates one subdirectory per encoded CWD. The three branches are:
//
//	host         → <home>/.pi/agent/sessions
//	bwrap        → <XDG_STATE_HOME>/prism/run/<sessionDirHash>/pi-agent/sessions
//	sandbox-exec → <stagingHome>/.pi/agent/sessions
//
// The bwrap branch falls through to the host branch when SessionName is empty
// (no way to derive a sessionDirHash), matching the no-op contract used by
// SourcePath when HarnessSessionID or Worktree is empty.
func piSessionsRoot(p harnessarchive.SourceParams) (string, error) {
	switch {
	case p.IsolationMode == "sandbox-exec" && p.InstanceID != "":
		// sandbox-exec: PI writes into the per-session staging HOME, not
		// the real home directory. Use the same path formula as
		// container.SandboxExecStagingHomePath.
		stagingHome, err := container.SandboxExecStagingHomePath(p.InstanceID)
		if err != nil {
			return "", fmt.Errorf("pi archive: resolve sandbox-exec staging home: %w", err)
		}
		return filepath.Join(stagingHome, ".pi", "agent", "sessions"), nil

	case p.IsolationMode == "bwrap" && p.SessionName != "":
		// bwrap: PI writes into the per-session pi-agent staging dir at
		// <XDG_STATE_HOME>/prism/run/<sessionDirHash>/pi-agent/sessions/.
		// There is no .pi/agent join — the staging dir IS the agent dir.
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("pi archive: resolve home for bwrap state dir: %w", err)
			}
			stateHome = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(
			stateHome, "prism", "run",
			session.SessionDirName(p.SessionName),
			"pi-agent", "sessions",
		), nil

	default:
		// host (and any fallback — empty IsolationMode, podman, or a
		// bwrap/sandbox-exec session missing its identifier): use the real
		// home directory.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("pi archive: resolve home: %w", err)
		}
		return filepath.Join(home, ".pi", "agent", "sessions"), nil
	}
}

// Archive copies PI's JSONL session file from srcPath into rawDir/session.jsonl.
//
// srcPath is expected to be a single file (the session JSONL), as returned by
// SourcePath. If the path does not exist or is a directory (the latter occurs
// when HarnessSessionID was empty and SourcePath returned the sessions root),
// Archive returns nil and rawDir is left empty — preserving the no-op contract
// for sessions where PI never started.
//
// The destination is always named "session.jsonl" so that Export's expectation
// is met regardless of the timestamp prefix in the source filename.
func (a *piArchiveAdapter) Archive(_ context.Context, srcPath, rawDir string, _ harnessarchive.SourceParams) error {
	fi, err := os.Stat(srcPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pi archive: stat %q: %w", srcPath, err)
	}
	if fi.IsDir() {
		// SourcePath returned a directory (e.g. sessions root when
		// HarnessSessionID is empty). Nothing to copy — no-op.
		return nil
	}

	dst := filepath.Join(rawDir, "session.jsonl")
	if err := copyFile(srcPath, dst); err != nil {
		return fmt.Errorf("pi archive: copy %q → %q: %w", srcPath, dst, err)
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

// EncodePiCWD encodes an absolute directory path to the directory name pi uses
// for its session storage. The formula mirrors pi 0.75.3
// dist/core/session-manager.js line 213:
//
//	--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--
//
// In Go terms: strip the leading '/' or '\', replace every occurrence of '/',
// '\', and ':' with '-', then wrap in "--…--".
//
// Exported because the encoding formula mirrors pi's own JS and downstream
// packages may need to reproduce it to locate a session's JSONL file.
func EncodePiCWD(cwd string) string {
	// Strip leading slash or backslash.
	stripped := strings.TrimLeft(cwd, "/\\")
	// Replace path separators and Windows drive colons with dashes.
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
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
