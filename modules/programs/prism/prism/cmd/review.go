package cmd

// prism review <pr-number> — platform-native review primitive.
//
// Spawns review agent sessions in a dedicated tmux session named
// <parent-session>~review-N (N = 1-indexed round number), polls the prism DB
// until all agents reach the "finished" state, reads their last msg_assistant
// event, and returns aggregated findings to stdout.
//
// Flags:
//
//	--harness <name>    Runtime harness (default: "opencode")
//	--keep              Keep the review session open after completion
//	--timeout <dur>     Per-agent timeout (default: 10m)
//	--only <csv>        Run only the named agents (e.g. review)

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/review"
)

var reviewCmd = &cobra.Command{
	Use:   "review <pr-number>",
	Short: "Run review agents against a PR and return aggregated findings",
	Long: `Spawn review agent sessions in a dedicated tmux session and poll the
prism DB until all agents complete. Returns aggregated findings to stdout.

The review session is named <parent-session>~review-N where N is incremented
on each invocation. Previous ~review-* sessions are killed before starting.

Exit code 0 = all agents passed. Non-zero = one or more agents failed or errored.`,
	Args: cobra.ExactArgs(1),
	RunE: runReview,
}

func init() {
	reviewCmd.Flags().String("harness", "opencode", "Runtime harness to use for review agents")
	reviewCmd.Flags().Bool("keep", false, "Keep the review session open after completion (for debugging)")
	reviewCmd.Flags().Duration("timeout", 10*time.Minute, "Maximum time to wait per agent")
	reviewCmd.Flags().String("only", "", "Comma-separated list of agent names to run (e.g. review)")
	rootCmd.AddCommand(reviewCmd)
}

func runReview(cmd *cobra.Command, args []string) error {
	prNumber := args[0]

	harnessFlag, _ := cmd.Flags().GetString("harness")
	keepFlag, _ := cmd.Flags().GetBool("keep")
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")
	onlyFlag, _ := cmd.Flags().GetString("only")

	// Container-mode detection: when PRISM_HOST_API is set the process is
	// running inside a container where tmux is not available. Route the review
	// through the host sidecar instead of calling review.Run() directly.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		// Resolve the agent list client-side (inside the container), where
		// ENHANCED_REVIEW and PRISM_HOST_API are set. This ensures that
		// env-var-driven agent selection (e.g. ENHANCED_REVIEW=true) uses the
		// container's environment, not the host sidecar's environment, and
		// that the --only flag is applied correctly before forwarding.
		allAgents := agentsForHarness(harnessFlag)
		var selectedAgents []review.Agent
		if onlyFlag != "" {
			var err error
			names := splitCSV(onlyFlag)
			selectedAgents, err = review.AgentsByName(allAgents, names)
			if err != nil {
				return fmt.Errorf("prism review: %w", err)
			}
		} else {
			selectedAgents = allAgents
		}
		agentNames := make([]string, len(selectedAgents))
		for i, ag := range selectedAgents {
			agentNames[i] = ag.Name
		}

		timeoutStr := ""
		if timeoutFlag > 0 {
			timeoutStr = timeoutFlag.String()
		}
		output, passed, err := proxyReview(apiURL, prNumber, agentNames, timeoutStr)
		if err != nil {
			return fmt.Errorf("prism review: host API: %w", err)
		}
		fmt.Print(output)
		if output != "" && !strings.HasSuffix(output, "\n") {
			fmt.Println()
		}
		if !passed {
			os.Exit(1)
		}
		return nil
	}

	// Resolve the parent session name.
	parentSession := review.LookupParentSession()
	if parentSession == "" {
		return fmt.Errorf("prism review: could not determine parent session name\nhint: run from inside a tmux session or set PRISM_SESSION_NAME")
	}

	// Load config for container mode (must be before DB block to gate it).
	cfg := config.Load()

	// Resolve worktree path.
	var worktree string
	if cfg.ContainerMode {
		worktree = os.Getenv("PRISM_SPAWN_PATH")
		if worktree == "" {
			worktree = "/workspace"
		}
	} else {
		// Validate parent session exists in DB.
		d, dbErr := openDB()
		if dbErr != nil {
			return fmt.Errorf("prism review: open db: %w", dbErr)
		}
		status, stErr := d.CurrentStatus(parentSession)
		d.Close()
		if stErr != nil {
			return fmt.Errorf("prism review: lookup session %q: %w", parentSession, stErr)
		}
		if status == nil {
			return fmt.Errorf("prism review: parent session %q not found in DB\nhint: the session must be registered in the prism DB", parentSession)
		}
		worktree = status.Worktree
		if worktree == "" {
			return fmt.Errorf("prism review: no worktree path for session %q", parentSession)
		}
	}

	// Determine agents list.
	allAgents := agentsForHarness(harnessFlag)
	var agents []review.Agent
	if onlyFlag != "" {
		var err error
		names := splitCSV(onlyFlag)
		agents, err = review.AgentsByName(allAgents, names)
		if err != nil {
			return fmt.Errorf("prism review: %w", err)
		}
	} else {
		agents = allAgents
	}

	if len(agents) == 0 {
		return fmt.Errorf("prism review: no agents to run")
	}

	// Pre-flight: verify that the required opencode agent definitions exist.
	// Skip in container mode — the check cannot inspect the container filesystem.
	if !cfg.ContainerMode {
		if err := review.CheckAgentAvailability(agents); err != nil {
			return fmt.Errorf("prism review: %w", err)
		}
	}

	// Build run options.
	opts := review.Opts{
		PRNumber:       prNumber,
		ParentSession:  parentSession,
		Worktree:       worktree,
		Agents:         agents,
		Harness:        harnessFlag,
		Timeout:        timeoutFlag,
		Keep:           keepFlag,
		PluginHostPath: cfg.SidecarPluginPath,
		ContainerMode:  cfg.ContainerMode,
	}

	// Load config content for container mode.
	if cfg.ContainerMode {
		pf, pfErr := config.LoadProfiles()
		if pfErr == nil {
			roleConfig, roleErr := config.ContainerConfigForRole(pf, "review")
			if roleErr == nil && roleConfig != "" {
				opts.ConfigContent = roleConfig
			}
		}
	}

	// Set up context with signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Install SIGTERM/SIGINT handler. Kill all ~review-* sessions for the parent
	// and cancel the context. KillReviewSessionsForParent is idempotent and
	// covers all rounds without needing the specific session name, avoiding
	// any data race on a shared variable.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		review.KillReviewSessionsForParent(parentSession)
		cancel()
	}()

	// Run the review.
	results, runErr := review.Run(ctx, opts, func(sessionName string) {
		fmt.Fprintf(os.Stderr, "[prism review] review session: %s\n", sessionName)
	})

	signal.Stop(sigCh)

	if runErr != nil && runErr != context.Canceled {
		return fmt.Errorf("prism review: %w", runErr)
	}

	// Format and print results.
	output, allPassed := review.FormatResults(results, prNumber)
	fmt.Print(output)

	if !allPassed {
		os.Exit(1)
	}
	return nil
}

// agentsForHarness returns the agent list for the given harness.
// When ENHANCED_REVIEW=true is set in the environment, the five-agent enhanced
// set is returned. Otherwise the single default agent is used (back-compat).
// Currently only "opencode" is supported as a harness.
func agentsForHarness(harness string) []review.Agent {
	switch harness {
	case "opencode", "":
		if os.Getenv("ENHANCED_REVIEW") == "true" {
			return review.EnhancedAgents()
		}
		return review.DefaultAgents()
	default:
		// Unknown harness — return opencode agents as fallback.
		fmt.Fprintf(os.Stderr, "[prism review] warning: unknown harness %q, using opencode\n", harness)
		if os.Getenv("ENHANCED_REVIEW") == "true" {
			return review.EnhancedAgents()
		}
		return review.DefaultAgents()
	}
}

// splitCSV splits a comma-separated string and trims whitespace.
func splitCSV(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}
