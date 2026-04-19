package cmd

// prism review <pr-number> — platform-native review primitive.
//
// Spawns review agent sessions as independent top-level tmux sessions named
// <parent-session>~review-N-<agent> (N = 1-indexed round number), polls the
// prism DB until all agents reach the "finished" state, reads their last
// msg_assistant event, and returns aggregated findings to stdout.
//
// Sessions persist until prism cleanup is invoked on the parent — this allows
// re-reading review-security's findings tomorrow without re-running.
//
// Flags:
//
//	--harness <name>    Runtime harness (default: "opencode")
//	--timeout <dur>     Per-agent timeout (default: 10m)
//	--only <csv>        Run only the named agents (e.g. review-goal,review-code)

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
	Long: `Spawn review agent sessions as independent top-level tmux sessions and poll
the prism DB until all agents complete. Returns aggregated findings to stdout.

Each agent gets its own session named <parent-session>~review-N-<agent> where N
is incremented on each invocation. Previous rounds' sessions persist until
prism cleanup is invoked on the parent.

Exit code 0 = all agents passed. Non-zero = one or more agents failed or errored.`,
	Args: cobra.ExactArgs(1),
	RunE: runReview,
}

func init() {
	reviewCmd.Flags().String("harness", "opencode", "Runtime harness to use for review agents")
	reviewCmd.Flags().Duration("timeout", 10*time.Minute, "Maximum time to wait per agent")
	reviewCmd.Flags().String("only", "", "Comma-separated list of agent names to run (e.g. review-goal,review-code)")
	reviewCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	rootCmd.AddCommand(reviewCmd)
}

func runReview(cmd *cobra.Command, args []string) error {
	prNumber := args[0]

	harnessFlag, _ := cmd.Flags().GetString("harness")
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")
	onlyFlag, _ := cmd.Flags().GetString("only")
	onlyChanged := cmd.Flags().Changed("only")

	// Resolve the full agent list and apply --only filtering up-front.
	// This is done before the container-mode branch so that validation
	// (empty CSV, unknown names) is consistent across both paths and no
	// sessions are spawned before we know the agent list is valid.
	allAgents := agentsForHarness(harnessFlag)
	var agents []review.Agent
	if onlyChanged {
		// --only was explicitly set. Validate it produces at least one token.
		names := splitCSV(onlyFlag)
		if len(names) == 0 {
			return fmt.Errorf("prism review: --only flag requires at least one agent name (got empty value %q)\navailable: %s",
				onlyFlag, strings.Join(agentNameStrings(allAgents), ", "))
		}
		var err error
		agents, err = review.AgentsByName(allAgents, names)
		if err != nil {
			return fmt.Errorf("prism review: %w", err)
		}
	} else {
		agents = allAgents
	}

	// Concurrency cap check: BEFORE any container-creation side effects.
	// We check inside the container-mode guard below (PRISM_HOST_API set
	// means we're already inside a container; the host sidecar handles the cap
	// for the actual spawn). Only apply the check on the host path.
	// Load cfg now for the cap check; it is re-used below for ContainerMode.
	cfg := config.Load()
	isoMode := cfg.EffectiveIsolationMode()
	conCapped := isoMode == config.IsolationPodman || isoMode == config.IsolationBwrap
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL == "" {
		// Running on host — check the cap before spawning review containers.
		if err := checkConcurrencyCap(cmd, "review", conCapped); err != nil {
			return err
		}
	}

	// Container-mode detection: when PRISM_HOST_API is set the process is
	// running inside a container where tmux is not available. Route the review
	// through the host sidecar instead of calling review.Run() directly.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		// Agent list was already resolved and validated above (client-side,
		// inside the container). This ensures that the --only flag is applied
		// correctly before forwarding.
		agentNames := make([]string, len(agents))
		for i, ag := range agents {
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

	// Resolve worktree path from the DB. By the time control reaches here we
	// know PRISM_HOST_API is unset (the proxy-out branch above did not fire),
	// so this process is running on the host regardless of cfg.ContainerMode.
	// cfg.ContainerMode is a Nix-time flag meaning "this host spawns workers
	// inside containers"; it does NOT mean "this process is running inside a
	// container". Reading PRISM_SPAWN_PATH or falling back to /workspace here
	// would use the container-internal path, which does not exist on the host
	// and causes `statfs /workspace: no such file or directory` in podman.
	worktree, wtErr := resolveReviewWorktree(parentSession)
	if wtErr != nil {
		return wtErr
	}

	if len(agents) == 0 {
		return fmt.Errorf("prism review: no agents to run")
	}

	// Pre-flight: verify that the required opencode agent definitions exist.
	// By the time we reach here, PRISM_HOST_API is guaranteed to be unset (the
	// proxy-out branch above returned early if it was set), so this process is
	// always running on the host regardless of cfg.ContainerMode. The agent
	// definition files are on the host filesystem and CheckAgentAvailability
	// can inspect them correctly. cfg.ContainerMode is a Nix-time flag meaning
	// "this host spawns workers in containers"; it is NOT a runtime signal
	// meaning "this process is running inside a container" — using it as such
	// would incorrectly skip this check on Darwin hosts with container_mode=true.
	if err := review.CheckAgentAvailability(agents); err != nil {
		return fmt.Errorf("prism review: %w", err)
	}

	// progressLine writes and flushes a single progress line to stdout.
	// Flushing after each write is critical: the enclosing bash tool invocation
	// makes stdout a pipe (not a TTY), so Go's default buffering would hold
	// lines until the buffer fills. os.Stdout.Sync() forces an immediate flush.
	progressLine := func(line string) {
		fmt.Fprintln(os.Stdout, line)
		_ = os.Stdout.Sync()
	}

	// Fetch PR context once before spawning any review-agent sessions.
	// FetchPRContext handles gh failures gracefully — a failed fetch logs a
	// warning and returns a PRContext with FetchFailed=true. The review run
	// continues in either case; agents fall back to git-based discovery when
	// the context is absent.
	prCtx := review.FetchPRContext(prNumber, 0, 0)

	// Build run options.
	effectiveContainerMode := isoMode == config.IsolationPodman
	opts := review.Opts{
		PRNumber:       prNumber,
		ParentSession:  parentSession,
		Worktree:       worktree,
		Agents:         agents,
		Harness:        harnessFlag,
		Timeout:        timeoutFlag,
		PluginHostPath: cfg.SidecarPluginPath,
		ContainerMode:  effectiveContainerMode,
		OnProgress:     progressLine,
		PRCtx:          &prCtx,
	}

	// Load profiles for container mode — passed through to review.Run so
	// each agent's sidecar receives its own per-agent hardened config blob.
	// In container mode a missing or malformed profiles.json means the
	// per-agent opencode.json cannot be mounted, which causes the container
	// to fall back to the image default (build agent). Surface this as an
	// explicit error rather than silently spawning broken review containers.
	if effectiveContainerMode {
		pf, pfErr := config.LoadProfiles()
		if pfErr != nil {
			return fmt.Errorf("prism review: container mode requires profiles.json but it could not be loaded: %w\nhint: ensure the system has been rebuilt with the prism NixOS module enabled", pfErr)
		}
		opts.ProfilesFile = pf
	}

	// Set up context with signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// currentRoundSessions holds the session names spawned in this invocation.
	// It is written once by Run's onSessionsCreated callback (before polling
	// begins) and read by the SIGINT handler. Protected by the assumption that
	// the callback fires before the goroutine needs to read it (the goroutine
	// only acts on a signal, which arrives after spawning is complete).
	var currentRoundSessions []string

	// Install SIGTERM/SIGINT handler. On signal, kill only the current round's
	// in-progress sessions — previous rounds' persisted sessions remain untouched.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		review.KillCurrentRoundSessions(currentRoundSessions)
		cancel()
	}()

	// Run the review.
	results, runErr := review.Run(ctx, opts, func(sessionNames []string) {
		currentRoundSessions = sessionNames
		for _, name := range sessionNames {
			fmt.Fprintf(os.Stderr, "[prism review] agent session: %s\n", name)
		}
	})

	signal.Stop(sigCh)

	if runErr != nil && runErr != context.Canceled {
		return fmt.Errorf("prism review: %w", runErr)
	}

	// Print a blank line to separate progress output from the aggregated summary.
	fmt.Fprintln(os.Stdout)
	_ = os.Stdout.Sync()

	// Format and print results.
	output, allPassed := review.FormatResults(results, prNumber)
	fmt.Print(output)

	if !allPassed {
		os.Exit(1)
	}
	return nil
}

// agentsForHarness returns the agent list for the given harness.
// Currently only "opencode" is supported as a harness.
func agentsForHarness(harness string) []review.Agent {
	switch harness {
	case "opencode", "":
		return review.Agents()
	default:
		// Unknown harness — return opencode agents as fallback.
		fmt.Fprintf(os.Stderr, "[prism review] warning: unknown harness %q, using opencode\n", harness)
		return review.Agents()
	}
}

// resolveReviewWorktree returns the host-side worktree path for parentSession
// by looking it up in the prism DB. It is called from runReview after the
// PRISM_HOST_API proxy-out branch, so we are always on the host regardless of
// cfg.ContainerMode. Using PRISM_SPAWN_PATH or a /workspace fallback here
// would pass the container-internal mount path to podman, causing a
// "statfs /workspace: no such file or directory" error on the host.
func resolveReviewWorktree(parentSession string) (string, error) {
	d, dbErr := openDB()
	if dbErr != nil {
		return "", fmt.Errorf("prism review: open db: %w", dbErr)
	}
	status, stErr := d.CurrentStatus(parentSession)
	d.Close()
	if stErr != nil {
		return "", fmt.Errorf("prism review: lookup session %q: %w", parentSession, stErr)
	}
	if status == nil {
		return "", fmt.Errorf("prism review: parent session %q not found in DB\nhint: the session must be registered in the prism DB", parentSession)
	}
	if status.Worktree == "" {
		return "", fmt.Errorf("prism review: no worktree path for session %q", parentSession)
	}
	return status.Worktree, nil
}

// splitCSV splits a comma-separated string and trims whitespace.
// Trailing commas, leading commas, and empty tokens after trimming are ignored.
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

// agentNameStrings returns a slice of agent names from an Agent slice.
func agentNameStrings(agents []review.Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}
