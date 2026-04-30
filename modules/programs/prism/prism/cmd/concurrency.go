package cmd

// concurrency.go — unified concurrency-cap check used by spawn, pr, and review.
//
// checkConcurrencyCap enforces the per-isolator soft concurrency cap via
// iso.Cap(ctx, dbPath).Check(ignoreCap). It is the single entry point that
// replaces the per-mode helpers (checkConcurrencyCap, checkBwrapConcurrencyCap,
// checkSandboxExecConcurrencyCap) and their inline message-rendering blocks.
//
// A.3 (#1134): unified cap via Isolator.Cap().

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// checkConcurrencyCap enforces the per-isolator soft concurrency cap.
// Returns a non-nil error when the cap is exceeded and --ignore-concurrency-cap
// is not set. Writes a stderr warning and returns nil when the flag is set and
// the cap is exceeded. When the cap is not exceeded, returns nil without side
// effects.
//
// Must be called BEFORE any session-creation side effects (no worktree, no DB
// row, no tmux session on refusal).
//
// cmd is the cobra.Command that owns the --ignore-concurrency-cap flag.
// mode is the resolved isolation mode. callerName is used in error/warning
// messages (e.g. "spawn", "pr", "review").
func checkConcurrencyCap(cmd *cobra.Command, callerName string, mode config.IsolationMode) error {
	iso, err := container.For(mode, container.ConstructorOpts{})
	if err != nil {
		return fmt.Errorf("[prism %s] could not look up isolator for mode %q: %w", callerName, mode, err)
	}

	ignoreCap, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	return iso.Cap(context.Background(), dbPath()).Check(ignoreCap)
}
