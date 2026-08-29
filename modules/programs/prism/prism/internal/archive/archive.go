// Package archive copies a harness's on-disk session subtree into a prism
// archive directory at cleanup time.
//
// # Directory layout
//
//	~/.local/share/prism/archive/
//	  <repo>/
//	    <startedAtISO>_<instanceID>/
//	      session.jsonl  (when the harness wrote conversation data)
//	      manifest.json
//	      agent-run.log  (when present)
//
// The pre-fix layout placed `session.jsonl` under a `raw/` subdirectory and
// ran a separate Export step that byte-copied it next to `manifest.json`.
// That two-stage flow was an artefact of multi-harness opencode support;
// with pi as the sole remaining harness, Archive writes `session.jsonl`
// directly into the per-session archive directory in one step. Pre-fix
// archives on disk are left as-is — `prism archive` only prints the recorded
// path.
//
// # Atomicity
//
// The copy is performed under a temp directory (.tmp-<instanceID>/) that is
// renamed to the final name only on success. A partial copy is cleaned up on
// any error so that prism cleanup never leaves a half-written archive behind.
//
// # File permissions
//
// Archive directories are created with mode 0700; individual files are written
// with mode 0600. These are personal session traces that may contain secrets.
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrAlreadyExists is returned by Run when the final archive directory already
// exists. Callers that need to distinguish "already archived" from other
// failures (e.g. to propagate a non-zero exit code) can check with
// errors.Is(err, ErrAlreadyExists).
var ErrAlreadyExists = fmt.Errorf("archive directory already exists")

const (
	// ArchiveVersion stamps the directory layout format. Increment when the
	// layout changes in a backward-incompatible way.
	ArchiveVersion = 1

	// PiMonoVersion is the pi-mono JSONL trace format version. The manifest
	// records it so the format stays consistent across archives.
	PiMonoVersion = 3

	archiveDirMode  = 0o700
	archiveFileMode = 0o600
)

// Params holds the inputs needed to run an archive copy.
type Params struct {
	// InstanceID is the session's UUID (from sessions.instance_id).
	InstanceID string
	// SessionName is the prism session name (e.g. "nixos-config@feature").
	SessionName string
	// AgentRole is the value of sessions.agent_role (may be empty).
	AgentRole string
	// RootAgentName is the value of sessions.root_agent_name (may be empty).
	RootAgentName string
	// Harness is the harness name, e.g. "pi".
	Harness string
	// HarnessSessionID is the harness session identifier (from sessions.harness_session_id).
	// May be empty when the harness failed to start.
	HarnessSessionID string
	// HarnessVersion is the harness binary version string.
	// May be empty when the binary could not be queried.
	HarnessVersion string
	// Repo is the repository name (from sessions.repo).
	Repo string
	// Worktree is the absolute worktree path (from sessions.worktree).
	Worktree string
	// StartedAt is when the session started.
	StartedAt time.Time
	// EndedAt is when the session ended (set just before archive is called).
	EndedAt time.Time
	// EndState is the terminal end state, e.g. "finished" or "interrupted".
	EndState string
	// GroupID is the session_groups.group_id (may be empty).
	GroupID string
	// PrismVersion is the git SHA or version of the prism binary. May be empty.
	PrismVersion string
	// IsolationMode is "bwrap", "sandbox-exec", or "host" (see
	// config.ValidIsolationModes).
	IsolationMode string
	// AgentRunLogPath is the absolute path to the harness log file
	// (~/.local/state/prism/run/<session>/agent-run.log). When non-empty and the
	// file exists, it is copied into the archive as agent-run.log. Missing files
	// are silently skipped — sessions that never reached agent-run will not
	// have this file.
	AgentRunLogPath string
	// ArchiveRoot overrides the archive root (~/.local/share/prism/archive).
	// When empty the XDG-derived default is used. Tests inject this.
	ArchiveRoot string
	// Copier is called to populate the archive directory with harness
	// session files. It receives the per-session archive directory itself
	// (e.g. .../<repo>/<startedAtISO>_<instanceID>/), NOT a subdirectory.
	// It is required: Run returns an error if Copier is nil.
	// Sessions with no HarnessSessionID (harness failed to start) should
	// provide a no-op Copier that leaves the directory empty.
	Copier func(ctx context.Context, archiveDir string) error
}

// Run exports the session state for p into the archive directory and returns
// the absolute path of the archive directory on success.
//
// Copier (p.Copier) is required and must not be nil. It is responsible for
// populating the per-session archive directory with harness session files
// (e.g. session.jsonl for pi). Run returns an error immediately if Copier is
// nil.
//
// Atomicity: the export is written to a temp directory under <archiveRoot>/<repo>/
// and renamed to the final name only when the entire export succeeds. On any
// error the temp directory is removed and Run returns a non-nil error.
//
// Idempotency: if the target archive directory already exists when Run is
// called, Run returns archive.ErrAlreadyExists and leaves the existing
// directory intact.
//
// Sessions with no HarnessSessionID (harness failed to start) still produce
// an archive directory containing manifest.json (and no session.jsonl),
// as long as Copier is a no-op for that case.
func Run(p Params) (archivePath string, err error) {
	if p.Copier == nil {
		return "", fmt.Errorf("archive: Copier is required but was nil")
	}

	archiveRoot, err := resolveArchiveRoot(p)
	if err != nil {
		return "", err
	}

	// Validate p.Repo to prevent path traversal: it must not contain any
	// path separator or resolve outside archiveRoot. A repo name like
	// "../../evil" would otherwise escape the archive root via filepath.Join.
	cleanRepo := filepath.Clean(p.Repo)
	if cleanRepo != p.Repo || strings.ContainsRune(p.Repo, os.PathSeparator) || p.Repo == ".." {
		return "", fmt.Errorf("archive: invalid repo name %q (must not contain path separators or be '..')", p.Repo)
	}

	// Build the final archive directory path: <archiveRoot>/<repo>/<startedAtISO>_<instanceID>/
	dirName := p.StartedAt.UTC().Format("20060102T150405Z") + "_" + p.InstanceID
	repoDir := filepath.Join(archiveRoot, p.Repo)
	finalDir := filepath.Join(repoDir, dirName)
	tmpDir := filepath.Join(repoDir, ".tmp-"+p.InstanceID)

	// Secondary containment check: repoDir must be a direct child of archiveRoot
	// (guards against any edge case not caught by the string checks above).
	if filepath.Dir(repoDir) != filepath.Clean(archiveRoot) {
		return "", fmt.Errorf("archive: repo dir %q is not a direct child of archive root %q", repoDir, archiveRoot)
	}

	// Check whether target already exists before touching anything.
	if _, statErr := os.Stat(finalDir); statErr == nil {
		return "", fmt.Errorf("%w: %s", ErrAlreadyExists, finalDir)
	}

	// Ensure the repo-level directory exists (0700; world-private).
	if mkErr := os.MkdirAll(repoDir, archiveDirMode); mkErr != nil {
		return "", fmt.Errorf("archive: create repo dir %s: %w", repoDir, mkErr)
	}

	// Create temp dir.
	if mkErr := os.Mkdir(tmpDir, archiveDirMode); mkErr != nil {
		if os.IsExist(mkErr) {
			// Stale temp dir from a previous failed run — remove and retry.
			_ = os.RemoveAll(tmpDir)
			if mkErr2 := os.Mkdir(tmpDir, archiveDirMode); mkErr2 != nil {
				return "", fmt.Errorf("archive: create temp dir %s: %w", tmpDir, mkErr2)
			}
		} else {
			return "", fmt.Errorf("archive: create temp dir %s: %w", tmpDir, mkErr)
		}
	}

	// On any error after this point, remove the temp dir.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	// Delegate session file population to the caller-provided Copier. It
	// writes directly into the per-session archive directory. Pi's on-disk
	// JSONL is already the final format, so there is no `raw/` normalisation
	// subdirectory.
	if copyErr := p.Copier(context.Background(), tmpDir); copyErr != nil {
		return "", copyErr
	}

	// Copy the agent-run log (harness stdout/stderr) when it exists.
	// Missing files are silently skipped — sessions that never reached
	// agent-run will not have this file.
	if p.AgentRunLogPath != "" {
		if _, statErr := os.Stat(p.AgentRunLogPath); statErr == nil {
			dst := filepath.Join(tmpDir, "agent-run.log")
			if copyErr := copyFile(p.AgentRunLogPath, dst); copyErr != nil {
				return "", fmt.Errorf("archive: copy agent-run log: %w", copyErr)
			}
		}
	}

	// Write manifest.json.
	if writeErr := writeManifest(p, tmpDir); writeErr != nil {
		return "", writeErr
	}

	// Atomic rename.
	if renErr := os.Rename(tmpDir, finalDir); renErr != nil {
		return "", fmt.Errorf("archive: rename temp to final: %w", renErr)
	}

	return finalDir, nil
}

// resolveArchiveRoot returns the archive root path for the params, applying the
// XDG default (~/.local/share/prism/archive) when ArchiveRoot is empty.
func resolveArchiveRoot(p Params) (string, error) {
	if p.ArchiveRoot != "" {
		return p.ArchiveRoot, nil
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", fmt.Errorf("archive: resolve home: %w", homeErr)
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "prism", "archive"), nil
}

// copyFile copies the file at src to dst with mode 0600.
// The destination directory must already exist.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %s → %s: %w", src, dst, err)
	}
	return out.Close()
}

// manifest is the JSON structure written to manifest.json.
type manifest struct {
	ArchiveVersion   int     `json:"archiveVersion"`
	PiMonoVersion    int     `json:"piMonoVersion"`
	InstanceID       string  `json:"instanceId"`
	SessionName      string  `json:"sessionName"`
	AgentRole        string  `json:"agentRole"`
	RootAgentName    string  `json:"rootAgentName"`
	Harness          string  `json:"harness"`
	HarnessSessionID string  `json:"harnessSessionId"`
	HarnessVersion   string  `json:"harnessVersion"`
	Repo             string  `json:"repo"`
	Worktree         string  `json:"worktree"`
	StartedAt        string  `json:"startedAt"`
	EndedAt          string  `json:"endedAt"`
	EndState         string  `json:"endState"`
	GroupID          *string `json:"groupId"`
	PrismVersion     string  `json:"prismVersion"`
}

// writeManifest serialises the manifest and writes it to <dir>/manifest.json
// with mode 0600.
func writeManifest(p Params, dir string) error {
	var groupID *string
	if p.GroupID != "" {
		g := p.GroupID
		groupID = &g
	}

	m := manifest{
		ArchiveVersion:   ArchiveVersion,
		PiMonoVersion:    PiMonoVersion,
		InstanceID:       p.InstanceID,
		SessionName:      p.SessionName,
		AgentRole:        p.AgentRole,
		RootAgentName:    p.RootAgentName,
		Harness:          p.Harness,
		HarnessSessionID: p.HarnessSessionID,
		HarnessVersion:   p.HarnessVersion,
		Repo:             p.Repo,
		Worktree:         p.Worktree,
		StartedAt:        p.StartedAt.UTC().Format(time.RFC3339),
		EndedAt:          p.EndedAt.UTC().Format(time.RFC3339),
		EndState:         p.EndState,
		GroupID:          groupID,
		PrismVersion:     p.PrismVersion,
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("archive: marshal manifest: %w", err)
	}
	data = append(data, '\n')

	manifestPath := filepath.Join(dir, "manifest.json")
	if writeErr := os.WriteFile(manifestPath, data, archiveFileMode); writeErr != nil {
		return fmt.Errorf("archive: write manifest.json: %w", writeErr)
	}
	return nil
}
