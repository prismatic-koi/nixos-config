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
//	--harness <name>    Runtime harness (default: "pi")
//	--timeout <dur>     Per-agent timeout (default: 10m)
//	--only <csv>        Run only the named agents (e.g. review-goal,review-code)
//	--rebase            Inline-rebase onto the PR's base ref (fetch + rebase + force-push) before review

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
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
	reviewCmd.Flags().String("harness", "pi", "Runtime harness to use for review agents")
	reviewCmd.Flags().Duration("timeout", 10*time.Minute, "Maximum time to wait per agent")
	reviewCmd.Flags().String("only", "", "Comma-separated list of agent names to run (e.g. review-goal,review-code)")
	reviewCmd.Flags().Bool("ignore-concurrency-cap", false, config.IgnoreConcurrencyCapHelp)
	reviewCmd.Flags().Int("diff-inline-max", 0,
		"Max diff lines to inline in agent prompts (0 = use PRISM_REVIEW_DIFF_INLINE_MAX env var or default 500)")
	reviewCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable, e.g. review-context=google/gemini-2.5-pro)")
	reviewCmd.Flags().Bool("rebase", false, "Fetch the PR's base ref (default origin/main), rebase HEAD onto it, and force-push before running the review")
	reviewCmd.Flags().Bool("wait", false, "Block until the review group reaches a terminal state (all-PASS, any-FAIL, or no-start)")
	reviewCmd.Flags().Duration("wait-timeout", defaultReviewWaitTimeout, "Timeout for --wait. Ignored when --wait is not set.")
	reviewCmd.Flags().Bool("json", false, "Emit the terminal verdict as a JSON object on stdout (only useful with --wait). Suppresses textual output.")
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
	rebaseFlag, _ := cmd.Flags().GetBool("rebase")
	diffInlineMaxFlag, _ := cmd.Flags().GetInt("diff-inline-max")
	modelOverrideRaw, _ := cmd.Flags().GetStringArray("model-override")
	modelsByRole, err := parseModelOverrides(modelOverrideRaw)
	if err != nil {
		return err
	}

	// Validate harness BEFORE any session state is created.
	if _, ok := harness.Lookup(harnessFlag); !ok {
		return fmt.Errorf("unknown harness %q: valid harnesses: %s", harnessFlag, strings.Join(harness.Names(), ", "))
	}

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
			return fmt.Errorf("prism review: --only must be one of: %s (got: %q)",
				strings.Join(agentNameStrings(allAgents), ", "), onlyFlag)
		}
		var err error
		agents, err = review.AgentsByName(allAgents, names)
		if err != nil {
			return fmt.Errorf("prism review: %w", err)
		}
	} else {
		agents = allAgents
	}

	// Load cfg. It is needed below for SidecarPluginPath even
	// when the actual isolation mode is overridden from the DB.
	cfg := config.Load()

	// Container-mode detection: when PRISM_HOST_API is set the process is
	// running inside a container where tmux is not available. Route the review
	// through the host sidecar instead of calling review.RunAsync() directly.
	//
	// The pre-flight rebase gate runs on the host side, inside the subprocess
	// spawned by the sidecar's /review handler (which sets cmd.Dir to the
	// session's worktree). When the gate refuses, the host subprocess exits
	// non-zero, the sidecar emits ReviewSentinelFailed, and proxyReviewAsync
	// returns an error to the container worker — same UX as a host-direct
	// refusal. See internal/review/preflight.go for the gate implementation.
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
		// is the buffered Ack copy; we parse the group_id from it so that
		// --wait can poll the group via the host-API probe (issue #1500
		// review-code feedback: --wait was silently dropped on this proxy
		// path before this fix).
		waitFlag, _ := cmd.Flags().GetBool("wait")
		jsonFlag, _ := cmd.Flags().GetBool("json")
		waitTimeout, _ := cmd.Flags().GetDuration("wait-timeout")
		// Suppress the streamed-stdout print when --wait --json is set so
		// the JSON terminal status is the only thing on stdout. The Ack is
		// still buffered in ackOutput so we can parse the group_id from it.
		quietStdout := waitFlag && jsonFlag
		ackOutput, err := proxyReviewAsync(apiURL, prNumber, agentNames, timeoutStr, rebaseFlag, quietStdout)
		if err != nil {
			return fmt.Errorf("prism review: host API: %w", err)
		}
		if waitFlag {
			groupID := parseReviewGroupIDFromAck(ackOutput)
			if groupID == "" {
				return fmt.Errorf("prism review --wait: could not parse group_id from review acknowledgement; cannot poll for completion")
			}
			return waitForReviewTerminal(prNumber, groupID, jsonFlag, waitTimeout)
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
	// so this process is running on the host. Reading PRISM_SPAWN_PATH or
	// falling back to /workspace here would use the container-internal path,
	// which does not exist on the host and causes
	// `statfs /workspace: no such file or directory` in podman.
	worktree, wtErr := resolveReviewWorktree(parentSession)
	if wtErr != nil {
		return wtErr
	}

	// Pre-flight PR-existence/state gate (#2040). Runs FIRST — before the
	// rebase gate — because it is cheaper (one `gh pr view`, no fetch) and
	// more fundamental (no point rebasing onto a PR that does not exist).
	// On any non-OPEN outcome (missing / CLOSED / MERGED / transient gh
	// error) it returns a *review.PRStateError with a ready-to-display Msg.
	// Like the rebase gate, this check does NOT spawn any agents and does
	// NOT touch the prism DB, so a fast-fail here cannot register a review
	// group or move the review-cycle counter. Transient gh errors are
	// surfaced distinctly from "PR does not exist" — we never silently
	// proceed to spawn agents against an unverified target.
	if prStateErr := review.CheckPRState(prNumber); prStateErr != nil {
		return prStateErr
	}

	// Pre-flight rebase gate (#1518, #2304). Runs BEFORE any review-agent
	// sessions are spawned and BEFORE any DB rows are written for this round,
	// so a gate failure (refusal, fetch failure, conflict abort, missing
	// upstream) cannot increment the review-cycle counter — that counter is
	// derived from per-agent session rows by review.NextRoundNumber, and no
	// such rows exist on the gate-failure path.
	//
	// Strict ancestor check: `git merge-base --is-ancestor <remote>/<base> HEAD`.
	// The PR's actual base ref is discovered via `gh pr view --json baseRefName`
	// (issue #2304) so PRs targeting non-main bases (long-lived integration
	// branches, release branches, environment branches) are checked against
	// the correct upstream. On any lookup failure (gh missing, unauthenticated,
	// network error, no PR for branch, empty baseRefName) we fall back
	// silently to "main" — preserving today's behaviour for invocations not
	// tied to a discoverable PR.
	//
	// On --rebase, the gate also performs fetch + rebase + force-push inline;
	// on rebase conflict it aborts the rebase and restores HEAD before
	// returning a non-zero error. Never leaves the worktree mid-rebase.
	baseBranch := review.ResolvePRBaseRef(prNumber) // "" on any failure → Preflight defaults to "main"
	if gateErr := review.Preflight(review.PreflightOpts{
		Worktree:   worktree,
		Branch:     baseBranch,
		Rebase:     rebaseFlag,
		OnProgress: progressLineEager,
	}); gateErr != nil {
		return gateErr
	}

	// Pre-flight formatter gate (#2556). Runs AFTER the rebase gate — it
	// diffs against the same <remote>/<branch> ref the rebase gate just
	// fetched and verified as an ancestor, so the file list reflects only
	// this branch's own changes. Like the rebase gate, it runs BEFORE any
	// review-agent session is spawned and BEFORE any DB rows are written for
	// this round, so a refusal here cannot increment the review-cycle
	// counter — NextRoundNumber derives the counter from per-agent session
	// rows, and RunAsync has not been called yet on this path.
	//
	// Fail-open: if gofmt or nixfmt is not on PATH, that language's check is
	// skipped with a progress warning rather than blocking the review. See
	// internal/review/formatgate.go for the full rationale.
	if gateErr := review.FormatGate(review.FormatGateOpts{
		Worktree:   worktree,
		Branch:     baseBranch,
		OnProgress: progressLineEager,
	}); gateErr != nil {
		return gateErr
	}

	// Resolve the effective isolation mode for spawning review agents.
	// Priority: parent session's DB-recorded mode > machine default.
	// Using the machine default (cfg.DefaultIsolationMode) is wrong on hosts
	// where the machine default is "podman" but the calling worker session runs
	// as "bwrap" — prism agent-run rejects review agents spawned with the wrong
	// mode. resolveParentIsolationMode returns "" only when the DB cannot be
	// read or the session has no row; the caller falls back to the machine
	// default in that case.
	isoMode, isoErr := container.Resolve(container.ResolveInput{
		ConfigDefault: cfg.DefaultIsolationMode,
	})
	if isoErr != nil {
		return isoErr
	}
	// isoMode may be overridden below by the parent session's DB-recorded mode.
	if dbIsoMode := resolveParentIsolationMode(parentSession); dbIsoMode != "" {
		isoMode = config.IsolationMode(dbIsoMode)
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isoMode)

	// Concurrency cap checks: BEFORE any container-creation side effects.
	// Both checks run after the DB isolation-mode lookup so that the cap
	// decisions reflect the session's actual mode rather than the machine
	// default. The PRISM_HOST_API proxy-out branch above already returned, so
	// by this point we are guaranteed to be on the host.
	//
	// A.3 (#1134): unified cap via iso.Cap(ctx, dbPath).Check(ignoreCap).
	if err := checkConcurrencyCap(cmd, "review", isoMode); err != nil {
		return err
	}

	if len(agents) == 0 {
		return fmt.Errorf("prism review: no agents to run")
	}

	// Pre-flight: verify that the required agent definitions exist.
	// By the time we reach here, PRISM_HOST_API is guaranteed to be unset (the
	// proxy-out branch above returned early if it was set), so this process is
	// always running on the host. The agent definition files are on the host
	// filesystem and CheckAgentAvailability can inspect them correctly.
	//
	// prismAgentRoleValidator checks that the agent .md definition file exists
	// under ~/.config/prism/agents/ (the canonical agent location after the
	// runtime was removed).
	if err := review.CheckAgentAvailability(agents, prismAgentRoleValidator); err != nil {
		return fmt.Errorf("prism review: %w", err)
	}

	// Construct harness adapter for runtime env vars (e.g. pi extension config).
	// harnessFlag was validated via harness.Lookup above; the error is unreachable.
	h, _ := harness.New(harnessFlag, "", nil, "", "")

	// progressLine writes and flushes a single progress line to stdout.
	// Flushing after each write is critical: the enclosing bash tool invocation
	// makes stdout a pipe (not a TTY), so Go's default buffering would hold
	// lines until the buffer fills. os.Stdout.Sync() forces an immediate flush.
	progressLine := progressLineEager

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

	// Derive the diff file storage directory from the worktree.
	//
	// All sandbox isolation modes (bwrap, sandbox-exec) mount the worktree at
	// its host path (Dst==Src), so any file written under the worktree on the
	// host is reachable at the same absolute path inside the sandbox — no
	// namespace translation required. Host-mode agents share the host filesystem
	// directly.
	//
	// We use a hidden subdirectory (<worktree>/.prism-review/) to keep the diff
	// file out of git status. The directory is cleaned up automatically when the
	// worktree is removed by `prism cleanup`.
	diffStateDir := filepath.Join(worktree, ".prism-review")

	prCtx := review.FetchPRContextWithOpts(review.FetchPRContextOpts{
		PRNumber:       prNumber,
		Round:          prCtxRound,
		InlineMaxLines: inlineMaxLines,
		Worktree:       worktree,
		StateDir:       diffStateDir,
	})

	// Build run options.
	opts := review.Opts{
		PRNumber:        prNumber,
		ParentSession:   parentSession,
		WorkerSession:   parentSession, // async delivery goes back to this session
		Worktree:        worktree,
		Agents:          agents,
		Harness:         harnessFlag,
		HarnessExplicit: cmd.Flags().Changed("harness"),
		Timeout:         timeoutFlag,
		PluginHostPath:  cfg.SidecarPluginPath,
		PIExtensionDir:  cfg.PIExtensionDir,
		IsolationMode:   string(isoMode),
		OnProgress:      progressLine,
		PRCtx:           &prCtx,
		RuntimeEnvVars:  h.RuntimeEnv(),
		ModelsByRole:    modelsByRole,
	}

	// Load profiles for any sandboxed mode (bwrap or sandbox-exec) so each
	// agent receives its per-role harness-config JSON via BuildConfigContent.
	// Surface a missing profiles.json as an explicit error rather than silently
	// spawning agents with the wrong model.
	if isoCaps.NeedsConfigBlob {
		pf, pfErr := config.LoadProfiles()
		if pfErr != nil {
			return fmt.Errorf("prism review: %s mode requires profiles.json but it could not be loaded: %w\nhint: ensure the system has been rebuilt with the prism NixOS module enabled", isoMode, pfErr)
		}
		opts.ProfilesFile = pf
	}

	// RunAsync spawns the agents, registers the group, starts the monitor, and
	// returns immediately. No blocking poll — the monitor process handles that.
	result, runErr := review.RunAsync(opts, "")
	if runErr != nil {
		return fmt.Errorf("prism review: %w", runErr)
	}

	// --wait: block until the review group reaches a terminal state.
	waitFlag, _ := cmd.Flags().GetBool("wait")
	jsonFlag, _ := cmd.Flags().GetBool("json")
	waitTimeout, _ := cmd.Flags().GetDuration("wait-timeout")
	if waitFlag {
		// In --wait mode and --json, we suppress the ack to keep stdout
		// JSON-only. In textual mode the ack is still useful so the user
		// can see which agents were spawned before the wait begins.
		if !jsonFlag {
			fmt.Print(result.Ack)
			_ = os.Stdout.Sync()
		}
		return waitForReviewTerminal(prNumber, result.GroupID, jsonFlag, waitTimeout)
	}

	// Print acknowledgement to stdout immediately.
	fmt.Print(result.Ack)
	_ = os.Stdout.Sync()
	return nil
}

// reviewGroupIDInAck matches the "(group: <uuid>)" segment in the Ack
// header line emitted by review.RunAsync, e.g.:
//
//	Review in progress — PR #1533, round 1 (group: 5103f218-...-2448dac6ab26)
//
// We pull the group_id out of the streamed Ack so the in-sandbox --wait
// path can poll the group via the sidecar's /groups/poll endpoint without
// the sidecar having to thread the ID back through a side-channel.
var reviewGroupIDInAck = regexp.MustCompile(`\(group:\s+([0-9a-f-]{36})\)`)

// parseReviewGroupIDFromAck extracts the review group_id from the buffered
// Ack output. Returns "" when no match is found — the caller surfaces this
// as an error so --wait does not silently hang on a missing ID.
func parseReviewGroupIDFromAck(ack string) string {
	m := reviewGroupIDInAck.FindStringSubmatch(ack)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// progressLineEager writes and flushes a single progress line to stdout.
// Flushing after each write is critical: the enclosing bash tool invocation
// makes stdout a pipe (not a TTY), so Go's default buffering would hold
// lines until the buffer fills. os.Stdout.Sync() forces an immediate flush.
//
// Used by both the pre-flight rebase gate (review.Preflight) and the
// review-agent spawn loop (review.Opts.OnProgress).
func progressLineEager(line string) {
	fmt.Fprintln(os.Stdout, line)
	_ = os.Stdout.Sync()
}

// agentsForHarness returns the agent list for the given harness.
func agentsForHarness(harness string) []review.Agent {
	return review.Agents()
}

// prismAgentRoleValidator checks that a <role>.md definition file exists under
// ~/.config/prism/agents/, returning an error if it is absent.
func prismAgentRoleValidator(role string) error {
	dir := prismAgentRolePath(role)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf(
			"prism: agent role %q is not available — no definition file at %s\n"+
				"hint: ensure the system has been rebuilt with the prism NixOS module",
			role, dir,
		)
	}
	return nil
}

// resolveReviewWorktree returns the host-side worktree path for parentSession
// by looking it up in the prism DB. It is called from runReview after the
// PRISM_HOST_API proxy-out branch, so we are always on the host. Using
// PRISM_SPAWN_PATH or a /workspace fallback here would pass the
// container-internal mount path to podman, causing a
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

// resolveParentIsolationMode returns the effective isolation mode for
// parentSession by looking it up in the prism DB. It returns "" only when the
// DB cannot be opened or the session has no row, signalling to the caller that
// it should fall back to cfg.DefaultIsolationMode.
func resolveParentIsolationMode(parentSession string) string {
	d, dbErr := openDB()
	if dbErr != nil {
		return ""
	}
	defer d.Close()
	status, stErr := d.CurrentStatus(parentSession)
	if stErr != nil || status == nil {
		return ""
	}
	if status.IsolationMode == "" || status.IsolationMode == "podman" {
		// Pre-v10 rows have no isolation_mode; legacy rows with "podman" fall
		// back to bwrap since podman isolation has been removed.
		return "bwrap"
	}
	return status.IsolationMode
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

See: modules/programs/prism/agents/coordinator.md`)
	}
	return nil
}
