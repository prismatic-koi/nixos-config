package cmd

// worktree_rollback.go — shared caller-level unwind for freshly created
// worktrees.
//
// The three worktree-creating front doors (`prism spawn`, `prism pr`, and
// the `prism switch` create-new-worktree flow) each register this rollback
// immediately after git.CreateWorktree. If any later step of the spawn
// fails, the freshly created worktree is removed and the branch is deleted
// when — and only when — it was freshly forked by that CreateWorktree call
// and still has no commits beyond its fork point. Reused worktrees
// (for example, the `prism spawn --branch main` coordinator-reuse path)
// never arm the rollback because they never call CreateWorktree.

import (
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// rollbackCreatedWorktree unwinds a worktree created earlier in a failed
// spawn/pr/switch flow. It is a no-op when created is nil (nothing was
// created by this call — e.g. a reuse path) and when a live tmux session
// exists for the worktree (the failure happened downstream of session
// creation, e.g. a tmux attach error, so the worktree is in use and must be
// left alone).
//
// Rollback failures are logged, never returned — the original error that
// triggered the unwind must remain the error reported to the caller. label
// names the calling command for the log line.
func rollbackCreatedWorktree(bareRoot string, created *git.CreatedWorktree, label string) {
	if created == nil {
		return
	}
	// A live tmux session for this worktree means session creation
	// succeeded and the failure is downstream (e.g. attach). The session
	// owns the worktree — removing it would pull the directory out from
	// under a running agent. `prism cleanup` handles that class instead.
	if tmux.HasSession(session.NameFor(created.Path, bareRoot)) {
		proglog.Warnf("[%s] not rolling back worktree %q: a tmux session exists for it\n",
			label, created.Path)
		return
	}
	if err := git.RollbackCreatedWorktree(bareRoot, *created); err != nil {
		proglog.Warnf("[%s] warning: rollback of worktree %q failed: %v\n",
			label, created.Path, err)
	}
}
