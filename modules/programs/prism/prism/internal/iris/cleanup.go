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
// Iris does not depend on tmux — cleanup makes no tmux calls.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
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
	// KillFn, when non-nil, is invoked once per session being cleaned up
	// (parent and each recursive child) BEFORE the archive step. It is the
	// hook by which the CLI talks to the iris daemon's session_kill
	// endpoint so a live pi child is terminated before its DB row is
	// archived (kill-then-archive per #1692). The returned string is a
	// short human-readable summary suitable for printing (e.g. "killed
	// (state=finished)", "skipped (daemon not running ...)") and is
	// recorded on the per-session result. A nil KillFn means "skip the
	// kill step" — used by tests and by `iris cleanup --skip-kill`.
	KillFn func(sessionName string) string

	// depth is the internal recursion depth used to bound the
	// parent → review-children traversal at 2 (parent → review children →
	// no further nesting). Callers MUST NOT set this — it is incremented
	// internally when CleanupSession recurses into review-group children.
	// A child encountered at depth >= maxCleanupDepth-1 that itself has a
	// session_groups row is skipped with a warning rather than recursed
	// into. See #1699.
	depth int
}

// maxCleanupDepth bounds the recursion in CleanupSession. The intended
// shape is parent (depth 0) → review children (depth 1) → no further
// nesting. A child encountered at depth 1 with its own session_groups
// row triggers a warn-and-skip.
const maxCleanupDepth = 2

// CleanupResult records which cleanup steps succeeded.
type CleanupResult struct {
	// SessionName is the session this result describes. Set on every
	// result (parent and each child) so callers can attribute errors and
	// summary lines to a specific session.
	SessionName string
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
	// KillSummary is the one-line summary returned by KillFn for this
	// session, or "" when KillFn was nil (kill step skipped). Recorded
	// for both the parent and each child so the CLI can render a
	// per-session kill line.
	KillSummary string
	// Children holds the per-child CleanupResult for each review-group
	// child cleaned up as part of this session's teardown. Empty for
	// sessions with no review-group children (the common, non-review
	// case). The slice is ordered by child session_name ascending for a
	// deterministic CLI rendering.
	Children []*CleanupResult
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

	res := &CleanupResult{SessionName: sessionName}

	// Step 1 (issue #1699): recurse into review-group children BEFORE this
	// session's own archive / row-removal / run-dir steps. Children are
	// cleaned up via their own CleanupSession invocation so each one gets
	// the kill-then-archive treatment (#1692 + #1697). Recursion is bounded
	// at maxCleanupDepth — a child encountered at depth 1 that itself has a
	// session_groups row triggers a warn-and-skip rather than a third
	// level.
	//
	// Errors here are non-fatal: if session_groups is unreadable, cleanup
	// falls back to parent-only with a warning (per the AC).
	res.Children = cleanupReviewGroupChildren(ctx, cfg, sessionName, res)

	// Step 1b (issue #1699): kill the running pi child for this session
	// via the injected KillFn. Runs BEFORE archive so the kill-then-archive
	// invariant from #1692 is preserved at every recursion level (parent
	// and each child).
	if cfg.KillFn != nil {
		res.KillSummary = cfg.KillFn(sessionName)
	}

	// Step 2: archive the pi JSONL. Delegates to the shared helper used by
	// the standalone `iris archive` subcommand (#1697) so cleanup-archived
	// and standalone-archived sessions land at the same path.
	archivePath, _, archErr := archiveSessionJSONL(ctx, ArchiveConfig{
		Database:    cfg.Database,
		ArchiveRoot: cfg.ArchiveRoot,
		PIAgentDir:  cfg.PIAgentDir,
	}, sess)
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

// cleanupReviewGroupChildren is the issue #1699 recursion step. It looks
// up every review-group child of parentSession via
// session_groups.parent_session → agent_status.group_id, and recursively
// cleans each child up via CleanupSession with depth+1.
//
// Recursion bound: when cfg.depth is already >= maxCleanupDepth-1, a child
// that itself has a session_groups row triggers a warn-and-skip rather
// than a deeper recursion. This caps the traversal at parent → review
// children (no further nesting). The cap is deliberately conservative;
// nothing in iris's current shape spawns review-of-review groups, and a
// child unexpectedly carrying its own group is treated as an anomaly to
// surface rather than to silently recurse into.
//
// Errors are non-fatal: a failed session_groups lookup logs a warning and
// returns an empty children slice (parent-only fallback per the AC). A
// child whose CleanupSession returns an error has the error appended to
// the parent's Errors slice; the surviving children are still processed.
//
// The returned slice is ordered by child session_name ascending so the
// CLI output is deterministic regardless of map / iteration order in the
// underlying DB query.
func cleanupReviewGroupChildren(ctx context.Context, cfg CleanupConfig, parentSession string, parentRes *CleanupResult) []*CleanupResult {
	members, err := cfg.Database.GroupMembersForParent(parentSession)
	if err != nil {
		// session_groups unreadable — fall back to parent-only with a
		// warning (per the AC). Do NOT propagate the error: the parent
		// cleanup should still proceed.
		log.Printf("[iris] cleanup: review-group lookup for parent %q failed: %v — falling back to parent-only cleanup", parentSession, err)
		parentRes.Errors = append(parentRes.Errors, fmt.Errorf("review-group lookup: %w", err))
		return nil
	}
	if len(members) == 0 {
		return nil
	}

	// Sort by session_name for deterministic CLI output (the DB query has
	// no inherent ordering for GroupMembersForParent).
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.SessionName)
	}
	sort.Strings(names)

	out := make([]*CleanupResult, 0, len(names))
	for _, childName := range names {
		// Recursion-depth guard: at depth >= maxCleanupDepth-1 (i.e. when
		// THIS call is already at the child layer), refuse to recurse
		// further into a grandchild that carries its own group. Log a
		// warning so the anomaly surfaces in operator output.
		if cfg.depth >= maxCleanupDepth-1 {
			hasGroup, hgErr := cfg.Database.HasReviewGroup(childName)
			if hgErr != nil {
				log.Printf("[iris] cleanup: depth-guard HasReviewGroup(%q) failed: %v — proceeding without grandchild recursion", childName, hgErr)
			} else if hasGroup {
				log.Printf("[iris] cleanup: WARNING: child %q (depth=%d) has its own review group — skipping deeper recursion (maxCleanupDepth=%d)", childName, cfg.depth+1, maxCleanupDepth)
				parentRes.Errors = append(parentRes.Errors, fmt.Errorf("child %q has its own review group; skipped deeper recursion", childName))
				continue
			}
		}

		// Skip children whose sessions row is missing entirely (already
		// cleaned up by an earlier pass, or never inserted). Without this
		// guard the recursive call would return a "session not found"
		// error and pollute the parent's Errors slice with noise for what
		// is the expected "already-cleaned-up child → skip gracefully" AC.
		childSess, lookupErr := cfg.Database.MostRecentSessionForName(childName)
		if lookupErr != nil {
			log.Printf("[iris] cleanup: child %q lookup failed: %v — skipping", childName, lookupErr)
			parentRes.Errors = append(parentRes.Errors, fmt.Errorf("child %q lookup: %w", childName, lookupErr))
			continue
		}
		if childSess == nil {
			// No sessions row — the child has already been cleaned up
			// (DB row removed) or was registered in agent_status only.
			// Surface a stub result for visibility and continue.
			log.Printf("[iris] cleanup: child %q has no sessions row — already cleaned up; skipping", childName)
			out = append(out, &CleanupResult{
				SessionName: childName,
				KillSummary: "skipped (already cleaned up)",
			})
			continue
		}

		childCfg := cfg
		childCfg.depth = cfg.depth + 1
		childRes, cErr := CleanupSession(ctx, childCfg, childName)
		if cErr != nil {
			log.Printf("[iris] cleanup: child %q: %v", childName, cErr)
			parentRes.Errors = append(parentRes.Errors, fmt.Errorf("child %q: %w", childName, cErr))
			// Even on a top-level error, record a stub so the CLI
			// reports which child was attempted.
			out = append(out, &CleanupResult{SessionName: childName, Errors: []error{cErr}})
			continue
		}
		out = append(out, childRes)
	}
	return out
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
