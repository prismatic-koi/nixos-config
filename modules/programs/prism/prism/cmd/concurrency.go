package cmd

// concurrency.go — shared concurrency-cap check used by spawn, pr, and review.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/container"
)

// checkConcurrencyCap checks the soft container concurrency cap.
// Returns a non-nil error when the cap is exceeded and ignoreCap is false.
// When ignoreCap is true and the cap is exceeded, writes a warning to stderr
// and returns nil (the caller should proceed with the spawn).
// When the cap is not exceeded, returns nil without side effects.
//
// Must be called BEFORE any container-creation side effects (no worktree, no
// DB row, no tmux session on refusal).
//
// cmd is the cobra.Command that owns the --ignore-concurrency-cap flag.
// callerName is used in warning/error messages (e.g. "spawn", "pr", "review").
// containerMode must be true — callers that run in host-mode or that cannot
// create containers should skip this check.
func checkConcurrencyCap(cmd *cobra.Command, callerName string, containerMode bool) error {
	if !containerMode {
		return nil
	}

	ignoreCap, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	res := container.CheckCap(dbPath(), container.DefaultConcurrencyCap, nil)

	if res.PodmanFailed {
		fmt.Fprintf(os.Stderr, "[prism %s] warning: podman ps failed — concurrency check is using DB-only count (may be imprecise)\n", callerName)
	}

	if !res.Exceeded {
		return nil
	}

	if ignoreCap {
		fmt.Fprint(os.Stderr, container.FormatExceededWarning(res))
		return nil
	}

	return fmt.Errorf("%s", container.FormatExceededError(res))
}
