package cmd

// prism review <pr-number> — platform-native review primitive.
//
// Spawns review agent sessions as independent top-level tmux sessions named
// <parent-session>~review-N-<agent> (N = 1-indexed round number), registers a
// group in session_groups, and returns immediately with an acknowledgement.
// A background monitor process watches the group for completion and delivers
// the aggregated results to the worker session via prism prompt.
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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

var reviewCmd = &cobra.Command{
	Use:   "review <pr-number>",
	Short: "Spawn review agents for a PR and return immediately (async)",
	Long: `Spawn review agent sessions as independent top-level tmux sessions, register
a group, and return immediately with a review-in-progress acknowledgement.

Each agent gets its own session named <parent-session>~review-N-<agent> where N
is incremented on each invocation. A background monitor process watches for
group completion and delivers aggregated results to this worker via prism prompt.

Previous rounds' sessions persist until prism cleanup is invoked on the parent.

Do NOT commit, merge, or announce completion until the review-complete prompt arrives.`,
	Args: cobra.ExactArgs(1),
	RunE: runReview,
}

func init() {
	reviewCmd.Flags().String("harness", "opencode", "Runtime harness to use for review agents")
	reviewCmd.Flags().Duration("timeout", 10*time.Minute, "Maximum time to wait per agent")
	reviewCmd.Flags().String("only", "", "Comma-separated list of agent names to run (e.g. review-goal,review-code)")
	reviewCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	reviewCmd.Flags().Int("diff-inline-max", 0,
		"Max diff lines to inline in agent prompts (0 = use PRISM_REVIEW_DIFF_INLINE_MAX env var or default 500)")
	reviewCmd.Flags().Int("size-budget", 0, "Max inline size (bytes) for full per-agent findings before overflow-to-file (default 20480; overridden by PRISM_REVIEW_SIZE_BUDGET env var)")
	rootCmd.AddCommand(reviewCmd)
}

func runReview(cmd *cobra.Command, args []string) error {
	prNumber := args[0]

	// Coordinator guard: prism review is a worker-only command.
	// Check before spawning any sessions so that a coordinator hitting this
	// guard does not create orphaned review-agent sessions.
	// Detection: DB-backed (root_agent_name == "coordinator") with a name-suffix
	// heuristic fallback for pre-migration rows (session ends with "@main").
	// A NULL root_agent_name row (pre-migration) falls back to the heuristic
	// with a deprecation log — not a hard failure.
	if err := rejectIfCoordinator(); err != nil {
		return err
	}

	harnessFlag, _ := cmd.Flags().GetString("harness")
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")
	onlyFlag, _ := cmd.Flags().GetString("only")
	onlyChanged := cmd.Flags().Changed("only")
	diffInlineMaxFlag, _ := cmd.Flags().GetInt("diff-inline-max")
	sizeBudgetFlag, _ := cmd.Flags().GetInt("size-budget")

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

	// Concurrency cap checks: BEFORE any container-creation side effects.
	// We check inside the container-mode guard below (PRISM_HOST_API set
	// means we're already inside a container; the host sidecar handles the cap
	// for the actual spawn). Only apply the checks on the host path.
	// Load cfg now for the cap check; it is re-used below for ContainerMode.
	cfg := config.Load()
	isoMode := cfg.EffectiveIsolationMode()
	// bwrap sessions are plain host processes — only podman triggers the container cap.
	conCapped := isoMode == config.IsolationPodman
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL == "" {
		// Running on host — check the caps before spawning review sessions.
		if err := checkConcurrencyCap(cmd, "review", conCapped); err != nil {
			return err
		}
		if isoMode == config.IsolationBwrap {
			if err := checkBwrapConcurrencyCap(cmd, "review"); err != nil {
				return err
			}
		}
	}

	// Container-mode detection: when PRISM_HOST_API is set the process is
	// running inside a container where tmux is not available. Route the review
	// through the host sidecar instead of calling review.RunAsync() directly.
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
		// proxyReviewAsync streams each output line to os.Stdout as it
		// arrives — no further printing is needed here. The returned string
		// is the buffered copy used for error context only.
		_, err := proxyReviewAsync(apiURL, prNumber, agentNames, timeoutStr)
		if err != nil {
			return fmt.Errorf("prism review: host API: %w", err)
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

	// Pre-flight: verify that the required agent definitions exist.
	// By the time we reach here, PRISM_HOST_API is guaranteed to be unset (the
	// proxy-out branch above returned early if it was set), so this process is
	// always running on the host regardless of cfg.ContainerMode. The agent
	// definition files are on the host filesystem and CheckAgentAvailability
	// can inspect them correctly. cfg.ContainerMode is a Nix-time flag meaning
	// "this host spawns workers in containers"; it is NOT a runtime signal
	// meaning "this process is running inside a container" — using it as such
	// would incorrectly skip this check on Darwin hosts with container_mode=true.
	//
	// The harness adapter's ValidateAgentRole method encapsulates the
	// harness-specific check (for opencode: agent .md files in the agents
	// directory). This keeps opencode-specific filesystem paths out of
	// cmd/ and review/ packages.
	h := opencode.New("", nil, "", "")
	if err := review.CheckAgentAvailability(agents, h.ValidateAgentRole); err != nil {
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

	// Resolve the inline-max threshold from flag → env var → default.
	// PRISM_REVIEW_DIFF_INLINE_MAX overrides the compiled-in default.
	// The --diff-inline-max flag takes precedence over the env var.
	inlineMaxLines := diffInlineMaxFlag
	if inlineMaxLines <= 0 {
		if envVal := os.Getenv("PRISM_REVIEW_DIFF_INLINE_MAX"); envVal != "" {
			if n, err := strconv.Atoi(envVal); err == nil && n > 0 {
				inlineMaxLines = n
			}
		}
	}

	// Fetch PR context once before spawning any review-agent sessions.
	// FetchPRContextWithOpts handles gh failures gracefully — a failed fetch logs a
	// warning and returns a PRContext with FetchFailed=true. The review run
	// continues in either case; agents fall back to git-based discovery when
	// the context is absent.
	//
	// The round number is determined here (pre-spawn) so the diff file path
	// is deterministic and matches the round that will be created below.
	// We open a separate DB handle just to peek at the round number —
	// RunAsync will open its own handle when it actually registers the group.
	prCtxRound := 1
	if dRound, dErr := openDB(); dErr == nil {
		prCtxRound = review.NextRoundNumber(dRound, parentSession)
		dRound.Close()
	}

	prCtx := review.FetchPRContextWithOpts(review.FetchPRContextOpts{
		PRNumber:       prNumber,
		Round:          prCtxRound,
		InlineMaxLines: inlineMaxLines,
		Worktree:       worktree,
	})

	// Build run options.
	effectiveContainerMode := isoMode == config.IsolationPodman
	opts := review.Opts{
		PRNumber:       prNumber,
		ParentSession:  parentSession,
		WorkerSession:  parentSession, // async delivery goes back to this session
		Worktree:       worktree,
		Agents:         agents,
		Harness:        harnessFlag,
		Timeout:        timeoutFlag,
		PluginHostPath: cfg.SidecarPluginPath,
		ContainerMode:  effectiveContainerMode,
		IsolationMode:  string(isoMode),
		OnProgress:     progressLine,
		PRCtx:          &prCtx,
		RuntimeEnvVars: h.RuntimeEnv(),
		SizeBudget:     sizeBudgetFlag,
	}

	// Load profiles for container mode — passed through to review.RunAsync so
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

	// RunAsync spawns the agents, registers the group, starts the monitor, and
	// returns immediately. No blocking poll — the monitor process handles that.
	result, runErr := review.RunAsync(opts, "")
	if runErr != nil {
		return fmt.Errorf("prism review: %w", runErr)
	}

	// Print acknowledgement to stdout immediately.
	fmt.Print(result.Ack)
	_ = os.Stdout.Sync()
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

// rejectIfCoordinator returns an error if the invoking session is a coordinator.
// It resolves the current session name from PRISM_SESSION_NAME (set inside
// containers/agents) or the current tmux session, then uses a DB-backed lookup
// (root_agent_name == "coordinator") with a name-suffix heuristic fallback
// for pre-migration rows. When no session can be identified, the check is
// skipped (no error) so that ad-hoc invocations outside any session are not
// blocked.
func rejectIfCoordinator() error {
	// Resolve the caller's session name.
	callerSession := review.LookupParentSession()
	if callerSession == "" {
		// Cannot determine session — skip the guard to avoid blocking
		// ad-hoc (non-tmux) invocations.
		return nil
	}

	// Open the DB for the lookup. A DB open failure is non-fatal: we fall
	// back to the name-suffix heuristic inside IsCoordinatorSession.
	d, dbErr := openDB()
	if dbErr != nil {
		d = nil
	}
	if d != nil {
		defer d.Close()
	}

	if session.IsCoordinatorSession(callerSession, d) {
		return fmt.Errorf(`prism review: this command is for worker sessions only.

Coordinators do not run review directly. To review a PR, spawn a session on the
PR branch and let that session run the review:

  prism pr <number> --prompt 'review this PR'

Wait for the finish notification from that spawned session before reporting back.

See: modules/programs/prism/opencode/agents/coordinator.md`)
	}
	return nil
}
