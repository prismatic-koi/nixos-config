package cmd

// concurrency.go — shared concurrency-cap checks used by spawn, pr, and review.
//
// checkConcurrencyCap enforces the podman container cap.
// checkBwrapConcurrencyCap enforces the bwrap session cap.
// checkSandboxExecConcurrencyCap enforces the sandbox-exec session cap.
// All honour the --ignore-concurrency-cap flag on the supplied cobra.Command.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
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
// conCapped must be true for modes that consume a container slot —
// currently "podman" only. Pass false for "host" and "bwrap" modes (no cap needed).
func checkConcurrencyCap(cmd *cobra.Command, callerName string, conCapped bool) error {
	if !conCapped {
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

// checkSandboxExecConcurrencyCap checks the soft sandbox-exec session concurrency cap.
// Returns a non-nil error when the cap is exceeded and --ignore-concurrency-cap
// is not set. When the flag is set and the cap is exceeded, writes a warning to
// stderr and returns nil.
//
// The cap value is read from config.Load().SandboxExecConcurrencyCap. A value
// of 0 means uncapped — the check always passes.
//
// Must be called BEFORE any session-creation side effects (no worktree, no DB
// row, no tmux session on refusal).
//
// cmd is the cobra.Command that owns the --ignore-concurrency-cap flag.
// callerName is used in warning/error messages (e.g. "spawn").
func checkSandboxExecConcurrencyCap(cmd *cobra.Command, callerName string) error {
	cap := config.Load().SandboxExecConcurrencyCap
	if cap == 0 {
		// 0 means uncapped.
		return nil
	}

	d, err := db.Open(dbPath())
	if err != nil {
		// Non-fatal: if we can't open the DB we skip the cap check rather than
		// blocking spawn on a DB error.
		fmt.Fprintf(os.Stderr, "[prism %s] warning: could not open DB for sandbox-exec cap check: %v\n", callerName, err)
		return nil
	}
	defer d.Close()

	count, err := d.ActiveSandboxExecSessionCount()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism %s] warning: could not count active sandbox-exec sessions: %v\n", callerName, err)
		return nil
	}

	if count < cap {
		return nil
	}

	// Cap exceeded — fetch session list for the error/warning message.
	sessions, listErr := d.ActiveSandboxExecSessions()

	ignoreCap, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	if ignoreCap {
		var sb strings.Builder
		fmt.Fprintf(&sb, "[prism] warning: sandbox-exec concurrency cap exceeded (%d/%d sandbox-exec sessions in flight) — proceeding because --ignore-concurrency-cap was passed\n", count, cap)
		if listErr == nil {
			sb.WriteString("[prism] active sandbox-exec sessions:\n")
			for _, s := range sessions {
				role := s.RootAgentName
				roleStr := "unknown"
				if role != nil && *role != "" {
					roleStr = *role
				} else if strings.HasSuffix(s.SessionName, "@main") {
					roleStr = "coordinator"
				}
				fmt.Fprintf(&sb, "[prism]   %-40s (%s)\n", s.SessionName, roleStr)
			}
		}
		fmt.Fprint(os.Stderr, sb.String())
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "error: prism sandbox-exec concurrency cap reached (%d sandbox-exec sessions already in flight)\n", count)
	if listErr == nil {
		sb.WriteString("\nActive sandbox-exec sessions:\n")
		for _, s := range sessions {
			role := s.RootAgentName
			roleStr := "unknown"
			if role != nil && *role != "" {
				roleStr = *role
			} else if strings.HasSuffix(s.SessionName, "@main") {
				roleStr = "coordinator"
			}
			fmt.Fprintf(&sb, "  %-40s (%s)\n", s.SessionName, roleStr)
		}
	}
	sb.WriteString("\nHint: wait for a worker to finish and be cleaned up, or re-run with\n")
	sb.WriteString("      --ignore-concurrency-cap to bypass this guard.")
	return fmt.Errorf("%s", sb.String())
}

// checkBwrapConcurrencyCap checks the soft bwrap session concurrency cap.
// Returns a non-nil error when the cap is exceeded and --ignore-concurrency-cap
// is not set. When the flag is set and the cap is exceeded, writes a warning to
// stderr and returns nil.
//
// The cap value is read from config.Load().BwrapConcurrencyCap. A value of 0
// means uncapped — the check always passes.
//
// Must be called BEFORE any session-creation side effects (no worktree, no DB
// row, no tmux session on refusal).
//
// cmd is the cobra.Command that owns the --ignore-concurrency-cap flag.
// callerName is used in warning/error messages (e.g. "spawn", "pr", "review").
func checkBwrapConcurrencyCap(cmd *cobra.Command, callerName string) error {
	cap := config.Load().BwrapConcurrencyCap
	if cap == 0 {
		// 0 means uncapped.
		return nil
	}

	d, err := db.Open(dbPath())
	if err != nil {
		// Non-fatal: if we can't open the DB we skip the cap check rather than
		// blocking spawn on a DB error.
		fmt.Fprintf(os.Stderr, "[prism %s] warning: could not open DB for bwrap cap check: %v\n", callerName, err)
		return nil
	}
	defer d.Close()

	count, err := d.ActiveBwrapSessionCount()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism %s] warning: could not count active bwrap sessions: %v\n", callerName, err)
		return nil
	}

	if count < cap {
		return nil
	}

	// Cap exceeded — fetch session list for the error/warning message.
	sessions, listErr := d.ActiveBwrapSessions()

	ignoreCap, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	if ignoreCap {
		var sb strings.Builder
		fmt.Fprintf(&sb, "[prism] warning: bwrap concurrency cap exceeded (%d/%d bwrap sessions in flight) — proceeding because --ignore-concurrency-cap was passed\n", count, cap)
		if listErr == nil {
			sb.WriteString("[prism] active bwrap sessions:\n")
			for _, s := range sessions {
				role := s.RootAgentName
				roleStr := "unknown"
				if role != nil && *role != "" {
					roleStr = *role
				} else if strings.HasSuffix(s.SessionName, "@main") {
					roleStr = "coordinator"
				}
				fmt.Fprintf(&sb, "[prism]   %-40s (%s)\n", s.SessionName, roleStr)
			}
		}
		fmt.Fprint(os.Stderr, sb.String())
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "error: prism bwrap concurrency cap reached (%d bwrap sessions already in flight)\n", count)
	if listErr == nil {
		sb.WriteString("\nActive bwrap sessions:\n")
		for _, s := range sessions {
			role := s.RootAgentName
			roleStr := "unknown"
			if role != nil && *role != "" {
				roleStr = *role
			} else if strings.HasSuffix(s.SessionName, "@main") {
				roleStr = "coordinator"
			}
			fmt.Fprintf(&sb, "  %-40s (%s)\n", s.SessionName, roleStr)
		}
	}
	sb.WriteString("\nHint: wait for a worker to finish and be cleaned up, or re-run with\n")
	sb.WriteString("      --ignore-concurrency-cap to bypass this guard.")
	return fmt.Errorf("%s", sb.String())
}
