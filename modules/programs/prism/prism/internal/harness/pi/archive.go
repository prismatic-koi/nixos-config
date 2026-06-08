package pi

// archive.go — PI implementation of harness/archive.ArchiveAdapter (B6.PI).
//
// PI stores session data as JSONL files on disk.
// Unlike some harnesses, PI has no SQLite database — it is a pure flat-file store.
//
// Source path layout (authoritative reference: pi 0.78.0
// dist/core/session-manager.js — see getDefaultSessionDirPath at line 217 and
// SessionManager.newSession at line 562 for the file-naming):
//
//	<piSessionsRoot>/<encoded-cwd>/<timestamp>_<uuid>.jsonl
//
// where <piSessionsRoot> is:
//
//	$PI_CODING_AGENT_DIR/sessions/      when PI_CODING_AGENT_DIR is set in the
//	                                    prism CLI's environment (matches pi's
//	                                    own ENV_AGENT_DIR honouring at startup)
//	~/.pi/agent/sessions/               otherwise — matches pi's getDefaultAgentDir
//
// The <encoded-cwd> directory name is derived from the session's working
// directory (p.Worktree) via EncodePiCWD. The formula mirrors pi's own JS
// (session-manager.js line 221):
//
//	--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--
//
// Strip the leading '/' (or '\'), replace every '/', '\', and ':' with '-',
// wrap in "--…--". Example:
//
//	/home/ben/code/nixos-config/main → --home-ben-code-nixos-config-main--
//
// The session UUID (HarnessSessionID) is embedded in the filename, NOT used as
// a directory name. Pi 0.78 may produce multiple JSONLs in the same
// <encoded-cwd> directory — one per `pi` invocation in that cwd — but each
// file's filename carries the UUID that pi's SessionManager.newSession used
// for that invocation, and the same UUID is mirrored into the JSONL's first
// "session" record as its `id` field. The prism PI extension emits that ID
// via the `session_status` frame's `session_id` field (pi/extensions/prism.ts),
// which the sidecar writes into sessions.harness_session_id. Therefore the
// adapter locates the right file by scanning the encoded-cwd directory for
// an entry whose name ends in `_<HarnessSessionID>.jsonl`.
//
// For sandbox-exec sessions (IsolationMode == "sandbox-exec"), PI writes to the
// per-session staging HOME instead of the real home directory (bug #1538 fix).
// The staging HOME is ~/.local/state/prism/sessions/<instance_id>/home/.
// The worktree path that pi sees as its CWD is the same as the host worktree
// path (sandbox-exec mounts it at its native path, only $HOME is remapped).
// Therefore the encoded-cwd is derived from p.Worktree in both cases; only the
// sessions root base (home) differs between host and sandbox-exec modes.
// PI_CODING_AGENT_DIR set on the host has no effect on sandbox-exec mode: the
// in-sandbox launcher points pi at a staging-home path that the host-side
// adapter already resolves via the staging-home formula, not via the host's
// environment.
//
// Archive copies the matched JSONL file directly into archiveDir/session.jsonl
// (single file, no `raw/` indirection). The opencode raw-archive → pi-mono v3
// translation step that motivated the previous two-stage Export flow has been
// removed along with opencode itself; PI's on-disk JSONL is already pi-mono v3
// shaped, so the byte-copy IS the archive.

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
//	<piSessionsRoot>/<encoded-cwd>/<timestamp>_<uuid>.jsonl
//
// where <piSessionsRoot> honours $PI_CODING_AGENT_DIR (see piSessionsRoot),
// <encoded-cwd> is derived from p.Worktree via EncodePiCWD, and <uuid>
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
// For bwrap sessions (IsolationMode == "bwrap"), PI writes into the host's
// $PI_CODING_AGENT_DIR/sessions/ (or ~/.pi/agent/sessions/ when the env var
// is unset) the same way as host mode. The sandbox overlays that host
// directory onto $PI_CODING_AGENT_DIR/sessions/ inside the namespace (see
// container.appendPIBwrapMounts), so writes pass through to
// <piSessionsRoot>/<encoded-cwd>/<ts>_<uuid>.jsonl on the host. This is the
// #1985 fix that restored the global per-cwd history users expect; before
// that fix bwrap pointed at
// <XDG_STATE_HOME>/prism/run/<sessionDirHash>/pi-agent/sessions/ which was
// torn down with the prism session (see bugs #1538 / #1814 for context).
//
// See pi 0.78 dist/core/session-manager.js (getDefaultSessionDirPath line 217,
// SessionManager.newSession line 562) for the authoritative path formula.
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
// which pi creates one subdirectory per encoded CWD. The two distinct branches
// are:
//
//	host / bwrap / podman / "" → $PI_CODING_AGENT_DIR/sessions when the env var
//	                              is set; else <home>/.pi/agent/sessions
//	sandbox-exec               → <stagingHome>/.pi/agent/sessions
//
// PI_CODING_AGENT_DIR mirrors pi's own ENV_AGENT_DIR honouring (pi reads it
// as the agent data root at startup; see pi 0.78 dist/core/session-manager.js
// getDefaultAgentDir and getDefaultSessionDirPath). The prism developer host
// sets PI_CODING_AGENT_DIR=/run/prism/pi-agent system-wide, so the unset
// branch is fallback-only — but it still matches pi's behaviour on hosts
// where the env var is not set.
//
// Sandbox-exec is unaffected by PI_CODING_AGENT_DIR on the host: the
// in-sandbox launcher points pi at a path under the per-session staging
// HOME (see container.appendPISandboxExecConfig), so the host-side adapter
// resolves the staging-home formula directly regardless of the operator's
// host environment.
//
// Before #1985 bwrap pointed at
// <XDG_STATE_HOME>/prism/run/<sessionDirHash>/pi-agent/sessions/, but that
// directory was torn down with the prism session, taking the per-cwd history
// with it. The bwrap launch now overlay-mounts the host's PI sessions root
// onto $PI_CODING_AGENT_DIR/sessions/ inside the sandbox (see
// container.appendPIBwrapMounts), so the host-side root is the same as host
// mode and the bwrap branch collapses into the default.
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

	default:
		// host, bwrap, podman, empty IsolationMode — all resolve to the
		// host PI data root: $PI_CODING_AGENT_DIR/sessions when set,
		// else <home>/.pi/agent/sessions.
		return hostPISessionsRoot()
	}
}

// hostPISessionsRoot returns the host-side PI sessions directory:
// $PI_CODING_AGENT_DIR/sessions when the env var is set (non-empty), else
// <UserHomeDir>/.pi/agent/sessions. Mirrors pi 0.78's own data-root
// resolution: pi honours ENV_AGENT_DIR (the same variable) and falls back to
// ~/.pi/agent/ when it is unset.
//
// Exported variants belong elsewhere if needed; this helper is package-local
// because internal/container has its own copy (piResumeHostSessionsRoot) — the
// two implementations MUST stay in sync. They cannot share a helper without
// introducing an import cycle (internal/harness/pi already imports
// internal/container).
func hostPISessionsRoot() (string, error) {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pi archive: resolve home: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), nil
}

// Archive copies PI's JSONL session file from srcPath into archiveDir/session.jsonl.
//
// srcPath is expected to be a single file (the session JSONL), as returned by
// SourcePath. If the path does not exist or is a directory (the latter occurs
// when HarnessSessionID was empty and SourcePath returned the sessions root),
// Archive returns nil and archiveDir is left empty — preserving the no-op
// contract for sessions where PI never started.
//
// archiveDir is the per-session archive directory itself (e.g.
// .../<repo>/<startedAtISO>_<instanceID>/) — Archive writes
// `<archiveDir>/session.jsonl` directly, with no `raw/` subdirectory. The
// pre-fix two-stage layout (raw/session.jsonl copied here, then a separate
// Export step that byte-copied it to <archiveDir>/session.jsonl) collapsed
// into a single step when opencode was removed from the codebase, because pi
// is the only remaining harness and pi's on-disk JSONL is already pi-mono v3
// shaped — no normalisation pass remains.
func (a *piArchiveAdapter) Archive(_ context.Context, srcPath, archiveDir string, _ harnessarchive.SourceParams) error {
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

	dst := filepath.Join(archiveDir, "session.jsonl")
	if err := copyFile(srcPath, dst); err != nil {
		return fmt.Errorf("pi archive: copy %q → %q: %w", srcPath, dst, err)
	}
	return nil
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
// for its session storage. The formula mirrors pi 0.78
// dist/core/session-manager.js line 221:
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
