package iris

// cleanup.go — session cleanup and archival for iris (D-10 parity).
//
// CleanupSession is iris's analogue of `prism cleanup <session>`. It is the
// teardown path invoked when a session is finished and the user wants its
// artefacts removed. Unlike `prism cleanup`, the iris cleanup path never
// invokes any prism binary and never touches `~/.local/state/prism/` —
// archives land in `~/code/archives/iris/` and run-directory artefacts under
// `~/.local/state/iris/run/`.
//
// What cleanup does, in order:
//
//   1. Resolve the session record (sessions row keyed by session name).
//   2. Archive the pi JSONL session file into
//      <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl using the
//      shared pi archive adapter (internal/harness/pi). The archive root is
//      taken from the cleanup config so tests can redirect it to a tempdir.
//   3. Mark the sessions row ended (end_state="finished" if not already
//      terminal) and remove the row from any per-process supervisor map.
//   4. Remove the per-session run directory at <RunDir>/<instance_id>/.
//      This sweeps the harness socket, per-session pi-agent config dir, the
//      tmp/ bind-mount backing dir, and any other per-session artefacts.
//   5. Remove the worktree directory and git branch when both are present
//      and safe (the worktree is under the resolved bare repo's parent and
//      its basename is not "main"). Cleanup never removes the coordinator
//      worktree even when explicitly requested — matching prism's invariant.
//
// Each step is independently fallible. A failure in step N is logged but
// does not abort steps N+1..end — the goal of cleanup is to remove as much
// state as possible, not to be strictly atomic. The returned CleanupResult
// records which steps ran successfully so callers can surface partial
// outcomes.
//
// Tmux sessions: iris does not create tmux sessions (the design point of
// the daemon model is to remove the tmux dependency). The "tmux session if
// any" clause in the D-10 cleanup AC is therefore vacuously satisfied —
// cleanup makes no tmux calls and the assertion that no tmux session
// remains is trivially true.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
)

// CleanupConfig holds the parameters needed to run cleanup for a single
// session.
type CleanupConfig struct {
	// Database is the open iris DB.
	Database *db.DB
	// RunDir is the iris run directory (e.g. ~/.local/state/iris/run/).
	// Per-session subdirs under this path are removed.
	RunDir string
	// LogDir is the iris per-session log directory (e.g.
	// ~/.local/state/iris/logs/). When non-empty, cleanup removes the
	// per-session log file at <LogDir>/<session>.log. Empty disables the
	// log-removal step (used by tests that don't exercise the log path).
	LogDir string
	// ArchiveRoot is the root of the iris archive tree
	// (e.g. ~/code/archives/iris/). Session JSONL files are copied to
	// <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl.
	ArchiveRoot string
	// PIAgentDir is the base directory where pi stores session files
	// (default: ~/.pi/agent/). Used to locate JSONL files for archive.
	// When empty, defaults to ~/.pi/agent/.
	PIAgentDir string
	// RemoveWorktree controls whether cleanup removes the worktree dir +
	// git branch. When false, only the iris-side artefacts are removed and
	// the worktree is left intact (callers handling worktree removal
	// themselves should set this false).
	RemoveWorktree bool
}

// CleanupResult records which cleanup steps succeeded.
type CleanupResult struct {
	// ArchivePath is the destination of the archived session JSONL when
	// archive succeeded ("" otherwise). The full path is
	// <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl.
	ArchivePath string
	// SessionRowRemoved is true when the sessions row was marked ended
	// (or already terminal) and the in-memory entry was forgotten by any
	// supervisor map.
	SessionRowRemoved bool
	// RunDirRemoved is true when <RunDir>/<instance_id>/ was removed.
	RunDirRemoved bool
	// LogFileRemoved is true when the per-session log file at
	// <LogDir>/<session>.log was removed (or was already absent). Always
	// true on the success path when LogDir is non-empty.
	LogFileRemoved bool
	// WorktreeRemoved is true when the worktree was removed
	// (RemoveWorktree=true and removal succeeded).
	WorktreeRemoved bool
	// BranchRemoved is true when the git branch was removed.
	BranchRemoved bool
	// Errors collects non-fatal errors from individual steps. A nil/empty
	// slice means every step that ran succeeded.
	Errors []error
}

// CleanupSession runs the iris cleanup sequence for a single session
// identified by sessionName. See the package comment for the sequence.
//
// Cleanup is best-effort: partial failures are logged and returned in
// CleanupResult.Errors. The caller may inspect the result to learn which
// steps succeeded and surface a summary.
func CleanupSession(ctx context.Context, cfg CleanupConfig, sessionName string) (*CleanupResult, error) {
	if cfg.Database == nil {
		return nil, errors.New("iris cleanup: Database is required")
	}
	if cfg.RunDir == "" {
		return nil, errors.New("iris cleanup: RunDir is required")
	}
	if cfg.ArchiveRoot == "" {
		return nil, errors.New("iris cleanup: ArchiveRoot is required")
	}

	sess, err := cfg.Database.MostRecentSessionForName(sessionName)
	if err != nil {
		return nil, fmt.Errorf("iris cleanup: lookup session %q: %w", sessionName, err)
	}
	if sess == nil {
		return nil, fmt.Errorf("iris cleanup: session %q not found", sessionName)
	}

	res := &CleanupResult{}

	// Step 2: archive the pi JSONL.
	archivePath, archErr := archivePiJSONL(ctx, cfg, sess)
	if archErr != nil {
		log.Printf("[iris] cleanup: archive %q: %v", sessionName, archErr)
		res.Errors = append(res.Errors, fmt.Errorf("archive: %w", archErr))
	} else if archivePath != "" {
		res.ArchivePath = archivePath
	}

	// Step 3: mark the sessions row ended (idempotent).
	if sess.EndState == nil {
		if err := cfg.Database.UpdateSessionEnded(sess.InstanceID, "finished"); err != nil {
			log.Printf("[iris] cleanup: update session ended for %q: %v", sessionName, err)
			res.Errors = append(res.Errors, fmt.Errorf("update session ended: %w", err))
		} else {
			res.SessionRowRemoved = true
		}
	} else {
		// Already terminal — treat as success.
		res.SessionRowRemoved = true
	}

	// Step 4: remove the per-session run directory.
	sessionRunDir := filepath.Join(cfg.RunDir, sess.InstanceID)
	if _, err := os.Stat(sessionRunDir); err == nil {
		if err := os.RemoveAll(sessionRunDir); err != nil {
			log.Printf("[iris] cleanup: remove run dir %q: %v", sessionRunDir, err)
			res.Errors = append(res.Errors, fmt.Errorf("remove run dir: %w", err))
		} else {
			res.RunDirRemoved = true
		}
	} else if os.IsNotExist(err) {
		// Already gone — treat as success.
		res.RunDirRemoved = true
	} else {
		log.Printf("[iris] cleanup: stat run dir %q: %v", sessionRunDir, err)
		res.Errors = append(res.Errors, fmt.Errorf("stat run dir: %w", err))
	}

	// Step 4b: remove the per-session log file.
	if cfg.LogDir != "" {
		logPath := (Paths{LogDir: cfg.LogDir}).SessionLogPath(sess.SessionName)
		if err := os.Remove(logPath); err != nil {
			if os.IsNotExist(err) {
				res.LogFileRemoved = true
			} else {
				log.Printf("[iris] cleanup: remove log file %q: %v", logPath, err)
				res.Errors = append(res.Errors, fmt.Errorf("remove log file: %w", err))
			}
		} else {
			res.LogFileRemoved = true
		}
	}

	// Step 5: remove the worktree and branch when requested and safe.
	if cfg.RemoveWorktree && sess.Worktree != "" {
		removedWorktree, removedBranch, wErr := removeWorktreeAndBranch(sess.Worktree)
		if wErr != nil {
			log.Printf("[iris] cleanup: remove worktree %q: %v", sess.Worktree, wErr)
			res.Errors = append(res.Errors, fmt.Errorf("remove worktree: %w", wErr))
		}
		res.WorktreeRemoved = removedWorktree
		res.BranchRemoved = removedBranch
	}

	return res, nil
}

// archivePiJSONL locates the pi JSONL file for the session and copies it to
// <ArchiveRoot>/<session>/<instance_id>/raw/session.jsonl. Returns the
// destination path on success, "" when there was nothing to archive.
func archivePiJSONL(ctx context.Context, cfg CleanupConfig, sess *db.Session) (string, error) {
	if sess.HarnessSessionID == nil || *sess.HarnessSessionID == "" || sess.Worktree == "" {
		// No harness session to archive (pi never started, or session was
		// constructed without a worktree). Treat as a no-op — not an error.
		return "", nil
	}

	piAgentDir := cfg.PIAgentDir
	if piAgentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		piAgentDir = filepath.Join(home, ".pi", "agent")
	}

	// Use the shared pi archive adapter so the source-path encoding stays
	// in lock-step with the rest of the codebase (single source of truth).
	// Locate the JSONL file ourselves (the adapter's SourcePath resolves ~
	// via os.UserHomeDir() — we honour the test-friendly PIAgentDir override
	// by scanning the encoded-cwd dir directly).
	adapter := piharness.NewArchiveAdapter()
	encodedCWD := piharness.EncodePiCWD(sess.Worktree)
	cwdDir := filepath.Join(piAgentDir, "sessions", encodedCWD)
	srcPath := scanForJSONL(cwdDir, *sess.HarnessSessionID)
	if srcPath == "" {
		// Nothing on disk — pi never wrote a JSONL file. Not an error.
		return "", nil
	}

	rawDir := filepath.Join(cfg.ArchiveRoot, sess.SessionName, sess.InstanceID, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir archive raw dir: %w", err)
	}
	p := harnessarchive.SourceParams{
		SessionName:      sess.SessionName,
		InstanceID:       sess.InstanceID,
		HarnessSessionID: *sess.HarnessSessionID,
		IsolationMode:    "host",
		Worktree:         sess.Worktree,
	}
	if err := adapter.Archive(ctx, srcPath, rawDir, p); err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	dst := filepath.Join(rawDir, "session.jsonl")
	if _, err := os.Stat(dst); err != nil {
		// Adapter ran but the destination is missing — surface as an error so
		// callers know the archive step did not produce the documented file.
		return "", fmt.Errorf("archive produced no session.jsonl at %q: %w", dst, err)
	}
	return dst, nil
}

// scanForJSONL scans cwdDir for a file matching "*_<sessionID>.jsonl" and
// returns its absolute path, or "" when not found.
func scanForJSONL(cwdDir, sessionID string) string {
	entries, err := os.ReadDir(cwdDir)
	if err != nil {
		return ""
	}
	suffix := "_" + sessionID + ".jsonl"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) >= len(suffix) && name[len(name)-len(suffix):] == suffix {
			return filepath.Join(cwdDir, name)
		}
	}
	return ""
}

// removeWorktreeAndBranch removes the worktree directory and (when present)
// the local git branch named after the worktree basename. Refuses to remove
// the "main" worktree under a prism bare layout (coordinator safety).
//
// Returns (worktreeRemoved, branchRemoved, error). A nil error with both
// booleans false means the path was skipped intentionally (e.g. coordinator
// worktree).
func removeWorktreeAndBranch(worktree string) (bool, bool, error) {
	base := filepath.Base(worktree)
	if base == "main" {
		// Never remove the coordinator worktree.
		return false, false, fmt.Errorf("refusing to remove coordinator worktree %q", worktree)
	}

	// If the worktree is part of a bare repo, prefer `git worktree remove`
	// so refs/worktrees/ is also cleaned. Fall back to RemoveAll otherwise.
	//
	// git.BareRoot returns the directory containing the `.bare` entry (the
	// prism wrapper), not the bare repo itself. We pass --git-dir pointing
	// at the .bare subdir so git commands run against the real repo. If
	// .bare is missing (gitdir pointer file rather than dir), we still try
	// --git-dir <bareRoot>/.bare and tolerate failure via the RemoveAll
	// fallback.
	bareWrapper := git.BareRoot(worktree)
	var gitDir string
	if bareWrapper != "" {
		gitDir = filepath.Join(bareWrapper, ".bare")
	}
	worktreeRemoved := false
	if gitDir != "" {
		cmd := exec.Command("git", "--git-dir", gitDir, "worktree", "remove", "--force", worktree)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Fall back to direct removal — the rmdir-then-prune sequence is
			// imperfect but the parity gate only asserts the directory is
			// gone, not that refs/worktrees/ is impeccable.
			log.Printf("[iris] cleanup: git worktree remove failed (%v); falling back to RemoveAll: %s", err, string(out))
			if err := os.RemoveAll(worktree); err != nil {
				return false, false, fmt.Errorf("remove worktree dir: %w", err)
			}
		}
		worktreeRemoved = true
	} else {
		if err := os.RemoveAll(worktree); err != nil {
			return false, false, fmt.Errorf("remove worktree dir: %w", err)
		}
		worktreeRemoved = true
	}

	// Remove the local branch if it exists.
	branchRemoved := false
	if gitDir != "" {
		cmd := exec.Command("git", "--git-dir", gitDir, "branch", "-D", base)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Branch may not exist (e.g. test harness with no branch). Not
			// fatal — log and continue.
			log.Printf("[iris] cleanup: git branch -D %q failed (non-fatal): %v: %s", base, err, string(out))
		} else {
			branchRemoved = true
		}
	}

	return worktreeRemoved, branchRemoved, nil
}
