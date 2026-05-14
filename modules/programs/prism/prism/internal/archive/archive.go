// Package archive copies a harness's on-disk session subtree into a prism
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
//	      manifest.json
//
// # Storage backend
//
// opencode stores all session state in a single SQLite database
// (~/.local/share/opencode/opencode-stable.db). The archive package queries
// that database directly — per-file JSON under storage/session/ is the legacy
// layout and is no longer written by current opencode releases.
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
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register sqlite3 driver
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
	// StorageRoot is retained for podman DB path resolution. For podman sessions
	// the DB lives at <StorageRoot>/../opencode-stable.db (i.e. the parent of the
	// per-container storage/ directory). For host/bwrap/sandbox-exec it is
	// unused when DBPath is also empty. Tests may inject this alongside DBPath.
	StorageRoot string
	// DBPath overrides the path to opencode-stable.db. When empty the default
	// path (~/.local/share/opencode/opencode-stable.db) is used for
	// host/bwrap/sandbox-exec, or the per-container equivalent for podman.
	// Tests inject this to point at a temp file.
	DBPath string
	// ArchiveRoot overrides the archive root (~/.local/share/prism/archive).
	// When empty the XDG-derived default is used. Tests inject this.
	ArchiveRoot string
	// Copier, when non-nil, is called instead of the default exportSessionFromDB
	// to populate rawDir with harness session files. When nil and
	// HarnessSessionID is non-empty, the built-in opencode exportSessionFromDB
	// fallback is used for backward compatibility. Tests may also inject this
	// to supply a custom copy function without setting a storage root.
	Copier func(ctx context.Context, rawDir string) error
}

// Run exports the opencode session state for p into the archive directory and
// returns the absolute path of the archive directory on success.
//
// Atomicity: the export is written to a temp directory under <archiveRoot>/<repo>/
// and renamed to the final name only when the entire export succeeds. On any
// error the temp directory is removed and Run returns a non-nil error.
//
// Idempotency: if the target archive directory already exists when Run is
// called, Run returns archive.ErrAlreadyExists and leaves the existing
// directory intact.
//
// Sessions with no HarnessSessionID (opencode failed to start) still produce
// an archive directory containing manifest.json and an empty raw/ directory.
func Run(p Params) (archivePath string, err error) {
	archiveRoot, dbPath, err := resolvePaths(p)
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

	// Export opencode session data from SQLite (only when we have a harness_session_id).
	if p.Copier != nil {
		if copyErr := p.Copier(context.Background(), rawDir); copyErr != nil {
			return "", copyErr
		}
	} else if p.HarnessSessionID != "" {
		if exportErr := exportSessionFromDB(p.HarnessSessionID, dbPath, rawDir); exportErr != nil {
			return "", exportErr
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

// resolvePaths returns (archiveRoot, dbPath) for the params, applying XDG
// defaults when the override fields are empty.
func resolvePaths(p Params) (archiveRoot, dbPath string, err error) {
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

	// DB path: explicit override wins, then derive from StorageRoot (for
	// callers that pre-resolve via ArchivePaths, e.g. podman), then fall back
	// to the isolation-mode switch.
	if p.DBPath != "" {
		dbPath = p.DBPath
	} else if p.StorageRoot != "" {
		// StorageRoot is <dataDir>/storage; the DB lives at <dataDir>/opencode-stable.db.
		dbPath = filepath.Join(filepath.Dir(p.StorageRoot), "opencode-stable.db")
	} else {
		dbPath, err = resolveDBPath(p.IsolationMode, p.SessionName)
		if err != nil {
			return "", "", err
		}
	}

	return archiveRoot, dbPath, nil
}

// resolveDBPath returns the path to opencode-stable.db for the given isolation
// mode and session name.
//
//   - host / bwrap / sandbox-exec: $HOME/.local/share/opencode/opencode-stable.db
//   - unknown / empty: returns an error
//
// bwrap and sandbox-exec both use the host-shared opencode data directory.
// exportSessionFromDB scopes all queries to the specific harness_session_id, so
// concurrent sessions in the same DB are not affected.
func resolveDBPath(isolationMode, _ string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("archive: resolve home for db path: %w", err)
	}

	switch isolationMode {
	case "host", "bwrap", "sandbox-exec":
		return filepath.Join(home, ".local", "share", "opencode", "opencode-stable.db"), nil
	default:
		return "", fmt.Errorf("archive: unsupported isolation mode %q", isolationMode)
	}
}

// resolveStorageRoot returns the host-side opencode storage root for the given
// isolation mode.
func resolveStorageRoot(isolationMode, _ string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("archive: resolve home for storage root: %w", err)
	}

	switch isolationMode {
	case "host", "bwrap", "sandbox-exec":
		return filepath.Join(home, ".local", "share", "opencode", "storage"), nil
	default:
		return "", fmt.Errorf("archive: unsupported isolation mode %q", isolationMode)
	}
}

// exportSessionFromDB queries opencode-stable.db for the session identified by
// harnessSessionID and writes JSON files into rawDir:
//
//   - raw/session.json        — the session row
//   - raw/messages/msg_*.json — one file per message row
//   - raw/parts/msg_<id>/prt_*.json — one file per part row, grouped by message
//
// Graceful no-ops:
//   - If the DB file does not exist, a single info log is emitted and raw/ is
//     left empty (not an error).
//   - If harnessSessionID is not found in the DB, a single info log is emitted
//     and raw/ is left empty (not an error).
// CopySessionFiles exports the opencode session identified by harnessSessionID
// from the SQLite DB at dbPath into rawDir. It is exported for use by the
// opencode ArchiveAdapter implementation in internal/harness/opencode/archive.go.
func CopySessionFiles(harnessSessionID, dbPath, rawDir string) error {
	return exportSessionFromDB(harnessSessionID, dbPath, rawDir)
}

func exportSessionFromDB(harnessSessionID, dbPath, rawDir string) error {
	// If the DB doesn't exist at all (fresh install, never run) — graceful no-op.
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		log.Printf("[prism] archive: legacy archive DB not found at %s — skipping session export", dbPath)
		return nil
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("archive: open legacy archive DB: %w", err)
	}
	defer db.Close()

	// Export session row.
	sessionData, err := querySessionJSON(db, harnessSessionID)
	if err != nil {
		return fmt.Errorf("archive: query session: %w", err)
	}
	if sessionData == nil {
		// Session not in DB — log and leave raw/ empty.
		log.Printf("[prism] archive: session %q not found in legacy archive DB — skipping session export", harnessSessionID)
		return nil
	}

	// Write raw/session.json.
	if writeErr := writeJSONFile(filepath.Join(rawDir, "session.json"), sessionData); writeErr != nil {
		return fmt.Errorf("archive: write session.json: %w", writeErr)
	}

	// Export messages.
	messages, err := queryMessagesJSON(db, harnessSessionID)
	if err != nil {
		return fmt.Errorf("archive: query messages: %w", err)
	}

	if len(messages) > 0 {
		msgDir := filepath.Join(rawDir, "messages")
		if mkErr := os.Mkdir(msgDir, archiveDirMode); mkErr != nil {
			return fmt.Errorf("archive: create messages dir: %w", mkErr)
		}
		for msgID, msgData := range messages {
			fname := msgID + ".json"
			if writeErr := writeJSONFile(filepath.Join(msgDir, fname), msgData); writeErr != nil {
				return fmt.Errorf("archive: write messages/%s: %w", fname, writeErr)
			}
		}
	}

	// Export parts — grouped by message_id.
	partsByMsg, err := queryPartsJSON(db, harnessSessionID)
	if err != nil {
		return fmt.Errorf("archive: query parts: %w", err)
	}

	if len(partsByMsg) > 0 {
		partsDir := filepath.Join(rawDir, "parts")
		if mkErr := os.Mkdir(partsDir, archiveDirMode); mkErr != nil {
			return fmt.Errorf("archive: create parts dir: %w", mkErr)
		}
		for msgID, parts := range partsByMsg {
			msgPartDir := filepath.Join(partsDir, msgID)
			if mkErr := os.Mkdir(msgPartDir, archiveDirMode); mkErr != nil {
				return fmt.Errorf("archive: create parts/%s dir: %w", msgID, mkErr)
			}
			for partID, partData := range parts {
				fname := partID + ".json"
				if writeErr := writeJSONFile(filepath.Join(msgPartDir, fname), partData); writeErr != nil {
					return fmt.Errorf("archive: write parts/%s/%s: %w", msgID, fname, writeErr)
				}
			}
		}
	}

	return nil
}

// querySessionJSON fetches the session row for harnessSessionID from the
// opencode DB and returns it as raw JSON bytes. Returns (nil, nil) when the
// session is not found.
func querySessionJSON(db *sql.DB, harnessSessionID string) ([]byte, error) {
	var dataStr string
	// The session table stores the full row; we select the columns we need and
	// marshal them to JSON rather than reading a pre-serialised blob.
	row := db.QueryRow(`
		SELECT id, project_id, parent_id, slug, directory, title, version,
		       share_url, summary_additions, summary_deletions, summary_files,
		       summary_diffs, revert, permission, time_created, time_updated,
		       time_compacting, time_archived
		  FROM session
		 WHERE id = ?`, harnessSessionID)

	var (
		id, projectID, slug, directory, title, version string
		parentID, shareURL, summaryDiffs, revert, permission *string
		summaryAdditions, summaryDeletions, summaryFiles    *int64
		timeCreated, timeUpdated                            int64
		timeCompacting, timeArchived                        *int64
	)
	err := row.Scan(
		&id, &projectID, &parentID, &slug, &directory, &title, &version,
		&shareURL, &summaryAdditions, &summaryDeletions, &summaryFiles,
		&summaryDiffs, &revert, &permission, &timeCreated, &timeUpdated,
		&timeCompacting, &timeArchived,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = dataStr // unused; we use individual fields

	out := map[string]any{
		"id":               id,
		"projectId":        projectID,
		"parentId":         parentID,
		"slug":             slug,
		"directory":        directory,
		"title":            title,
		"version":          version,
		"shareUrl":         shareURL,
		"summaryAdditions": summaryAdditions,
		"summaryDeletions": summaryDeletions,
		"summaryFiles":     summaryFiles,
		"summaryDiffs":     summaryDiffs,
		"revert":           revert,
		"permission":       permission,
		"timeCreated":      timeCreated,
		"timeUpdated":      timeUpdated,
		"timeCompacting":   timeCompacting,
		"timeArchived":     timeArchived,
	}
	return json.Marshal(out)
}

// queryMessagesJSON returns all messages for the session as a map of
// messageID → raw JSON bytes. The data column already contains the full
// message JSON as stored by opencode.
func queryMessagesJSON(db *sql.DB, sessionID string) (map[string][]byte, error) {
	rows, err := db.Query(`
		SELECT id, data FROM message WHERE session_id = ?
		ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]byte)
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		result[id] = []byte(data)
	}
	return result, rows.Err()
}

// queryPartsJSON returns all parts for the session as a nested map of
// messageID → (partID → raw JSON bytes). The data column already contains
// the full part JSON as stored by opencode.
func queryPartsJSON(db *sql.DB, sessionID string) (map[string]map[string][]byte, error) {
	rows, err := db.Query(`
		SELECT id, message_id, data FROM part WHERE session_id = ?
		ORDER BY message_id, time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]map[string][]byte)
	for rows.Next() {
		var id, messageID, data string
		if err := rows.Scan(&id, &messageID, &data); err != nil {
			return nil, err
		}
		if result[messageID] == nil {
			result[messageID] = make(map[string][]byte)
		}
		result[messageID][id] = []byte(data)
	}
	return result, rows.Err()
}

// writeJSONFile writes data to path with mode 0600. The destination directory
// must already exist.
func writeJSONFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
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
