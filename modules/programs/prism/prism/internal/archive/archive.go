// Package archive copies opencode's on-disk session subtree into a prism
// archive directory at cleanup time.
//
// # Directory layout
//
//	~/.local/share/prism/archive/
//	  <repo>/
//	    <startedAtISO>_<instanceID>/
//	      raw/
//	        session.json
//	        messages/msg_*.json
//	        parts/msg_<id>/prt_*.json
//	        tool-output/tool_*         (only when parts reference sidecar files)
//	      manifest.json
//
// # Atomicity
//
// The copy is performed under a temp directory (.tmp-<instanceID>/) that is
// renamed to the final name only on success. A partial copy is cleaned up on
// any error so that prism cleanup never leaves a half-written archive behind.
//
// # File permissions
//
// Archive directories are created with mode 0700; individual files are copied
// with mode 0600. These are personal session traces that may contain secrets.
package archive

import (
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

	// PiMonoVersion is the pi-mono JSONL trace format version targeted by the
	// next child PR (#995 child 3). Written here even though this PR does not
	// produce JSONL — the manifest format must be consistent across PRs.
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
	// Harness is the harness name, e.g. "opencode".
	Harness string
	// HarnessSessionID is the opencode ses_<ULID> (from sessions.harness_session_id).
	// May be empty when opencode failed to start.
	HarnessSessionID string
	// HarnessVersion is the opencode binary version string (e.g. "1.1.30").
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
	// IsolationMode is "podman", "bwrap", "sandbox-exec", or "host".
	IsolationMode string
	// AgentRunLogPath is the absolute path to the bwrap harness log file
	// (~/.local/state/prism/run/<session>/agent-run.log). When non-empty and the
	// file exists, it is copied into the archive as agent-run.log. Missing files
	// are silently skipped — bwrap sessions that never reached agent-run will not
	// have this file.
	AgentRunLogPath string
	// StorageRoot overrides the host opencode storage root. When empty the
	// default (~/.local/share/opencode/storage) is used. Tests inject this to
	// point at a temp dir.
	StorageRoot string
	// ArchiveRoot overrides the archive root (~/.local/share/prism/archive).
	// When empty the XDG-derived default is used. Tests inject this.
	ArchiveRoot string
}

// Run copies the opencode session subtree for p into the archive directory and
// returns the absolute path of the archive directory on success.
//
// Atomicity: the copy is written to a temp directory under <archiveRoot>/<repo>/
// and renamed to the final name only when the entire copy succeeds. On any
// error the temp directory is removed and Run returns a non-nil error.
//
// Idempotency: if the target archive directory already exists when Run is
// called, Run returns archive.ErrAlreadyExists and leaves the existing
// directory intact.
//
// Sessions with no HarnessSessionID (opencode failed to start) still produce
// an archive directory containing manifest.json and an empty raw/ directory.
func Run(p Params) (archivePath string, err error) {
	archiveRoot, storageRoot, err := resolvePaths(p)
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

	// Create raw/ subdirectory.
	rawDir := filepath.Join(tmpDir, "raw")
	if mkErr := os.Mkdir(rawDir, archiveDirMode); mkErr != nil {
		return "", fmt.Errorf("archive: create raw dir: %w", mkErr)
	}

	// Copy opencode session files (only when we have a harness_session_id).
	if p.HarnessSessionID != "" {
		if copyErr := copySessionFiles(p.HarnessSessionID, storageRoot, rawDir); copyErr != nil {
			return "", copyErr
		}
	}

	// Copy the agent-run log (bwrap harness stdout/stderr) when it exists.
	// Missing files are silently skipped — bwrap sessions that never reached
	// agent-run will not have this file, and non-bwrap sessions never create it.
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

// resolvePaths returns (archiveRoot, storageRoot) for the params, applying
// XDG defaults when the override fields are empty.
func resolvePaths(p Params) (archiveRoot, storageRoot string, err error) {
	// Archive root: $XDG_DATA_HOME/prism/archive
	if p.ArchiveRoot != "" {
		archiveRoot = p.ArchiveRoot
	} else {
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return "", "", fmt.Errorf("archive: resolve home: %w", homeErr)
			}
			dataHome = filepath.Join(home, ".local", "share")
		}
		archiveRoot = filepath.Join(dataHome, "prism", "archive")
	}

	// Storage root: $HOME/.local/share/opencode/storage or per-container path.
	if p.StorageRoot != "" {
		storageRoot = p.StorageRoot
	} else {
		storageRoot, err = resolveStorageRoot(p.IsolationMode, p.SessionName)
		if err != nil {
			return "", "", err
		}
	}

	return archiveRoot, storageRoot, nil
}

// resolveStorageRoot returns the host-side opencode storage root for the given
// isolation mode and session name.
//
//   - host / bwrap / sandbox-exec: $HOME/.local/share/opencode/storage
//   - podman: $HOME/.local/share/opencode/prism-sessions/<containerName>/storage
//   - unknown / empty: returns an error
//
// bwrap / sandbox-exec note: both modes run on the host filesystem namespace
// and bind-mount (bwrap) or use (sandbox-exec) the *shared*
// ~/.local/share/opencode/ directory — not a per-session sub-dir. The
// per-session isolation used by the podman path (Darwin virtiofs WAL-mode
// workaround) does not apply to either mode. The shared root is correct here
// because copySessionFiles scopes its reads to the specific harness_session_id,
// so only files belonging to this session are copied even when the storage pool
// contains concurrent sessions.
func resolveStorageRoot(isolationMode, sessionName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("archive: resolve home for storage root: %w", err)
	}

	switch isolationMode {
	case "host", "bwrap", "sandbox-exec":
		return filepath.Join(home, ".local", "share", "opencode", "storage"), nil
	case "podman":
		containerName := containerNameForSession(sessionName)
		return filepath.Join(home, ".local", "share", "opencode", "prism-sessions", containerName, "storage"), nil
	default:
		return "", fmt.Errorf("archive: unsupported isolation mode %q", isolationMode)
	}
}

// containerNameForSession returns the podman container name for a session,
// mirroring the logic in internal/container.NameForSession without importing
// that package (to keep archive lean and avoid a circular dependency).
func containerNameForSession(sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "@", "-")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, ".", "-")
	safe = strings.ReplaceAll(safe, "~", "-")
	return "prism-" + safe
}

// copySessionFiles copies the opencode session subtree rooted at storageRoot
// into rawDir. The subtree consists of:
//
//   - storage/session/<projectID>/ses_<id>.json  → raw/session.json
//   - storage/message/ses_<id>/*.json             → raw/messages/msg_*.json
//   - storage/part/msg_<id>/prt_*.json           → raw/parts/msg_<id>/prt_*.json
//   - storage/tool-output/tool_*                 → raw/tool-output/tool_* (referenced by parts)
func copySessionFiles(harnessSessionID, storageRoot, rawDir string) error {
	sessionFile, projectID, err := findSessionFile(harnessSessionID, storageRoot)
	if err != nil {
		return err
	}

	// Copy session.json → raw/session.json
	if copyErr := copyFile(sessionFile, filepath.Join(rawDir, "session.json")); copyErr != nil {
		return fmt.Errorf("archive: copy session.json: %w", copyErr)
	}
	_ = projectID // projectID found but not needed further; session path was already determined

	// Copy messages.
	msgDir := filepath.Join(storageRoot, "message", harnessSessionID)
	rawMsgDir := filepath.Join(rawDir, "messages")
	if err := copyDirFlat(msgDir, rawMsgDir); err != nil {
		return fmt.Errorf("archive: copy messages: %w", err)
	}

	// Copy parts — one subdirectory per message ID.
	partBaseDir := filepath.Join(storageRoot, "part")
	rawPartDir := filepath.Join(rawDir, "parts")

	// List message IDs from the messages we just copied — these are the
	// subdirectory names under storage/part/ that belong to this session.
	// Rather than globbing message IDs from the DB, we enumerate the
	// part directories that are prefixed by our message files.
	msgIDs, msgListErr := listDirEntries(msgDir)
	if msgListErr != nil {
		// Non-fatal: if there are no messages, there are no parts.
		msgIDs = nil
	}

	// Build the set of msg_* IDs by stripping the .json suffix from message filenames.
	type partRef struct {
		msgID string
		srcDir string
	}
	var partDirs []partRef
	for _, f := range msgIDs {
		msgID := strings.TrimSuffix(f, ".json")
		srcDir := filepath.Join(partBaseDir, msgID)
		if _, statErr := os.Stat(srcDir); statErr == nil {
			partDirs = append(partDirs, partRef{msgID: msgID, srcDir: srcDir})
		}
	}

	// Collect tool-output references while copying parts.
	toolOutputIDs := map[string]bool{}

	for _, pd := range partDirs {
		destDir := filepath.Join(rawPartDir, pd.msgID)
		if mkErr := os.MkdirAll(destDir, archiveDirMode); mkErr != nil {
			return fmt.Errorf("archive: create parts/%s dir: %w", pd.msgID, mkErr)
		}
		partFiles, listErr := listDirEntries(pd.srcDir)
		if listErr != nil {
			return fmt.Errorf("archive: list parts/%s: %w", pd.msgID, listErr)
		}
		for _, pf := range partFiles {
			src := filepath.Join(pd.srcDir, pf)
			dst := filepath.Join(destDir, pf)
			if copyErr := copyFile(src, dst); copyErr != nil {
				return fmt.Errorf("archive: copy part %s/%s: %w", pd.msgID, pf, copyErr)
			}
			// Scan the part file for tool-output references.
			ids := toolOutputIDsFromPart(src)
			for _, id := range ids {
				toolOutputIDs[id] = true
			}
		}
	}

	// Copy tool-output files referenced by parts.
	if len(toolOutputIDs) > 0 {
		toolOutputSrcDir := filepath.Join(storageRoot, "tool-output")
		toolOutputDstDir := filepath.Join(rawDir, "tool-output")
		if mkErr := os.MkdirAll(toolOutputDstDir, archiveDirMode); mkErr != nil {
			return fmt.Errorf("archive: create tool-output dir: %w", mkErr)
		}
		for toolID := range toolOutputIDs {
			src := filepath.Join(toolOutputSrcDir, toolID)
			dst := filepath.Join(toolOutputDstDir, toolID)
			if _, statErr := os.Stat(src); os.IsNotExist(statErr) {
				// Tool output file missing — not fatal, just skip.
				continue
			}
			if copyErr := copyFile(src, dst); copyErr != nil {
				return fmt.Errorf("archive: copy tool-output %s: %w", toolID, copyErr)
			}
		}
	}

	return nil
}

// findSessionFile scans the storage/session/ subtree to locate the session JSON
// file for harnessSessionID (a ses_<ULID> value). Returns the absolute path and
// projectID (directory name under storage/session/).
func findSessionFile(harnessSessionID, storageRoot string) (path, projectID string, err error) {
	sessionBaseDir := filepath.Join(storageRoot, "session")
	projectDirs, listErr := listDirEntries(sessionBaseDir)
	if listErr != nil {
		return "", "", fmt.Errorf("archive: list session base dir: %w", listErr)
	}

	fileName := harnessSessionID + ".json"
	for _, proj := range projectDirs {
		candidate := filepath.Join(sessionBaseDir, proj, fileName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, proj, nil
		}
	}
	return "", "", fmt.Errorf("archive: session file for %q not found under %s", harnessSessionID, sessionBaseDir)
}

// copyDirFlat copies all regular files in srcDir into dstDir (created if
// absent). Subdirectories within srcDir are ignored. If srcDir does not exist,
// a (possibly empty) dstDir is still created and nil is returned. A real I/O
// error (e.g. permission denied) reading srcDir is returned to the caller.
func copyDirFlat(srcDir, dstDir string) error {
	if mkErr := os.MkdirAll(dstDir, archiveDirMode); mkErr != nil {
		return fmt.Errorf("create %s: %w", dstDir, mkErr)
	}
	entries, err := listDirEntries(srcDir)
	if err != nil {
		// listDirEntries returns nil for os.IsNotExist; any remaining error is
		// a real I/O failure (e.g. EACCES) that we propagate.
		return fmt.Errorf("read %s: %w", srcDir, err)
	}
	for _, name := range entries {
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)
		if copyErr := copyFile(src, dst); copyErr != nil {
			return copyErr
		}
	}
	return nil
}

// listDirEntries returns the names of all directory entries directly under dir.
// Returns nil (not an error) when dir does not exist.
func listDirEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
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

// toolOutputIDsFromPart parses the part JSON at path and returns any
// tool-output file IDs referenced (e.g. "tool_<ulid>"). Returns nil on any
// parse error (best-effort; caller handles missing tool-output gracefully).
//
// The returned IDs are validated to be plain file names (no path separator,
// no ".." components) to prevent path traversal when used in filepath.Join.
// Any crafted asset value containing "/" or ".." is silently dropped.
func toolOutputIDsFromPart(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	// We only need the "type" and "asset" (or similar) fields — use a
	// minimal struct rather than full unmarshalling.
	var part struct {
		Type  string `json:"type"`
		Asset string `json:"asset"` // tool-output references: "tool_<ulid>"
	}
	if err := json.Unmarshal(data, &part); err != nil {
		return nil
	}

	if part.Type != "tool" || !strings.HasPrefix(part.Asset, "tool_") {
		return nil
	}

	// Validate: toolID must be a plain filename with no path separators or
	// ".." to prevent traversal outside the tool-output directory.
	toolID := part.Asset
	if toolID != filepath.Base(toolID) || strings.ContainsRune(toolID, os.PathSeparator) {
		return nil
	}

	return []string{toolID}
}

// manifest is the JSON structure written to manifest.json.
type manifest struct {
	ArchiveVersion  int     `json:"archiveVersion"`
	PiMonoVersion   int     `json:"piMonoVersion"`
	InstanceID      string  `json:"instanceId"`
	SessionName     string  `json:"sessionName"`
	AgentRole       string  `json:"agentRole"`
	RootAgentName   string  `json:"rootAgentName"`
	Harness         string  `json:"harness"`
	HarnessSessionID string `json:"harnessSessionId"`
	HarnessVersion  string  `json:"harnessVersion"`
	Repo            string  `json:"repo"`
	Worktree        string  `json:"worktree"`
	StartedAt       string  `json:"startedAt"`
	EndedAt         string  `json:"endedAt"`
	EndState        string  `json:"endState"`
	GroupID         *string `json:"groupId"`
	PrismVersion    string  `json:"prismVersion"`
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
