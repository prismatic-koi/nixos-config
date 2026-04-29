package review

// run.go — synchronous and asynchronous review run orchestration.
//
// Run() drives the full synchronous review loop: round numbering, group
// registration, per-agent spawn, readiness gating, polling, and result
// aggregation.
//
// RunAsync() is the fire-and-return path used by the host-API /review
// endpoint. It mirrors Run() in spawn/readiness but returns an AsyncResult
// instead of blocking for poll completion.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// Run executes the review. It returns the aggregated results and an error.
//
// Each agent is spawned as its own independent top-level tmux session named
// <parent>~review-<N>-<agent.Name>. Previous rounds' sessions are NOT killed
// — they persist until prism cleanup is invoked on the parent. This is a
// deliberate behaviour change from the old multi-window round model.
//
// On SIGINT, only the current round's in-progress sessions are killed by the
// caller via KillSessionsByNames (using the session names from onSessionsCreated).
// Previous rounds remain untouched.
func Run(ctx context.Context, opts Opts, onSessionsCreated func(sessionNames []string)) ([]AgentResult, error) {
	if opts.ParentSession == "" {
		return nil, fmt.Errorf("parent session name is required")
	}

	// Resolve worktree path.
	worktree := opts.Worktree
	if worktree == "" {
		return nil, fmt.Errorf("worktree path is required")
	}

	// Open DB.
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Determine round number from DB. We do NOT kill previous review sessions —
	// they persist until prism cleanup on the parent (deliberate).
	round := NextRoundNumber(d, opts.ParentSession)
	roundPrefix := fmt.Sprintf("%s~review-%d-", opts.ParentSession, round)

	repo := deriveRepo(opts.ParentSession)

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Agents()
	}

	// Resolve the runtime active profile once for the whole round so every
	// agent in this fan-out spawns with the same models (#1207). Errors are
	// surfaced — a corrupt state file would otherwise silently leak the
	// pf.Default profile into review agents.
	activeProfile, _, profErr := config.ResolveActiveProfile(opts.ProfilesFile, "")
	if profErr != nil {
		return nil, fmt.Errorf("resolve active profile: %w", profErr)
	}

	// Register a session group for this review round. Every spawned agent
	// session will carry this group_id, enabling GroupCompleted-based
	// termination detection and GroupResults-based result aggregation.
	// Fail fast if RegisterGroup fails — no sessions are spawned without a
	// group to belong to (AC: edge-case).
	groupID, groupErr := d.RegisterGroup(opts.ParentSession)
	if groupErr != nil {
		return nil, fmt.Errorf("register review group: %w", groupErr)
	}

	// Spawn each agent as its own independent top-level tmux session.
	// spawnErr[i] is non-nil if agent i failed to spawn. Agents that fail to
	// spawn are excluded from polling; they receive an error AgentResult.
	agentSessions := make([]string, len(agents))
	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))

	for i, ag := range agents {
		// Per-agent session: <parent>~review-<N>-<agent.Name>
		agentSession := roundPrefix + ag.Name
		agentSessions[i] = agentSession

		// Build the prompt for the review agent.
		// Inject the worktree path into PRCtx so agents know where the
		// branch is checked out (and that it is read-only).
		prCtxWithWorktree := opts.PRCtx
		if prCtxWithWorktree != nil && !prCtxWithWorktree.FetchFailed {
			// Shallow-copy so we don't mutate the shared PRCtx.
			ctxCopy := *prCtxWithWorktree
			ctxCopy.WorktreePath = worktree
			prCtxWithWorktree = &ctxCopy
		}
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree)

		// Resolve the per-agent config blob. Each agent gets its own hardened
		// opencode.json that declares only that one review agent.
		//
		// In sandboxed mode (podman or bwrap) a missing or empty blob means the
		// sandbox falls back to the host config (wrong agent identity).
		// ResolveAgentConfigContent surfaces this as an explicit error to
		// prevent silent wrong-agent spawns. activeProfile (resolved above)
		// is overlaid on the blob so review agents honour the runtime active
		// profile (#1207).
		agentConfigContent, configErr := ResolveAgentConfigContent(opts.IsolationMode, opts.ProfilesFile, ag.Name, activeProfile)
		if configErr != nil {
			spawnErr[i] = configErr
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), configErr))
			}
			continue
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. The bwrap harness
		// checks for file existence (os.Stat) rather than reading ConfigContent
		// from the session state, so it must be written here at spawn time.
		// This mirrors the pattern in cmd/spawn.go:386-394 for regular bwrap spawns.
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts.
		// sandbox-exec mode does NOT yet use this path — it has no bwrap-equivalent
		// mount mechanism. Config delivery for sandbox-exec is deferred to #1016.
		//
		// D3 (issue #1133): the bwrap-only gate routes the write through
		// Isolator.WriteHarnessConfigBlob so the dispatch goes via the
		// registered isolator instead of the package-level WriteOpencodeConfig
		// helper. The gate stays narrowed to bwrap to preserve the original
		// behaviour (sandbox-exec config delivery remains deferred to #1016).
		if opts.IsolationMode == string(config.IsolationBwrap) && agentConfigContent != "" {
			iso, isoErr := container.For(config.IsolationMode(opts.IsolationMode), container.ConstructorOpts{Name: agentSession})
			if isoErr != nil {
				spawnErr[i] = fmt.Errorf("review: resolve isolator for bwrap agent %s: %w", ag.Name, isoErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
			if writeErr := iso.WriteHarnessConfigBlob(agentSession, agentConfigContent); writeErr != nil {
				spawnErr[i] = fmt.Errorf("review: write opencode config for bwrap agent %s: %w", ag.Name, writeErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
		}

		// Spawn the per-agent session via the shared primitive. SpawnSession
		// handles DB seed (with root_agent_name from AgentRole), port
		// allocation, tmux session creation, and sidecar startup — keeping
		// review.go free of direct db/tmux/sidecar machinery (#859).
		//
		// WorktreeReadOnly=true ensures review containers cannot modify the
		// branch under review (satisfies the [security] acceptance criterion).
		spawnOpts := session.SpawnOpts{
			SessionName:      agentSession,
			Repo:             repo,
			Worktree:         worktree,
			AgentRole:        ag.Name,
			Prompt:           prompt,
			ConfigContent:    agentConfigContent,
			Layout:           session.LayoutAgentOnly,
			IsolationMode:    opts.IsolationMode,
			PluginHostPath:   opts.PluginHostPath,
			WorktreeReadOnly: true,
			GroupID:          groupID,
			RuntimeEnvVars:   opts.RuntimeEnvVars,
			HarnessName:      opts.Harness,
		}
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnSessErr))
			}
			// Clean up this agent's resources (sidecar, DB row, tmux session).
			// SpawnSession may have partially progressed; be defensive so a
			// second spawn attempt with the same name doesn't see stale state.
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("spawn session for %s: %w", ag.Name, spawnSessErr)
			continue
		}

		// Capture the spawn time so the readiness-gate phase below can
		// reset it once the agent is actually ready, and so the polling
		// phase has a sensible fallback if the gate is somehow skipped.
		// The "started" progress line is emitted by the readiness gate,
		// not here — see #1051 for why "spawned" is not the same as
		// "ready".
		spawnTimes[i] = time.Now()
	}

	// Per-agent readiness gate (#1051 Piece A). Runs in parallel goroutines
	// so one slow agent does not delay the others. Updates spawnErr[i] for
	// agents whose gate trips on timeout, and emits "<role> started" /
	// "<role> failed to start: not ready within <timeout>" via OnProgress.
	gateReviewAgents(d, agents, agentSessions, spawnErr, spawnTimes,
		opts.ReadinessTimeout, opts.OnProgress)

	// Check if all agents failed to spawn (or to become ready) — surface
	// a combined error. With the readiness gate in place, "all failed"
	// covers both pre-spawn failures (config errors, SpawnSession errors)
	// and never-came-up failures (readiness timeouts).
	allFailed := true
	for _, se := range spawnErr {
		if se == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		return nil, fmt.Errorf("all review agents failed to spawn")
	}

	// Build the subset of agents that successfully spawned AND became ready
	// for polling and SIGINT notification.
	var liveAgents []Agent
	var liveSessions []string
	var liveSpawnTimes []time.Time
	for i, se := range spawnErr {
		if se == nil {
			liveAgents = append(liveAgents, agents[i])
			liveSessions = append(liveSessions, agentSessions[i])
			liveSpawnTimes = append(liveSpawnTimes, spawnTimes[i])
		}
	}

	// Notify the caller with all successfully-ready session names for SIGINT handling.
	if onSessionsCreated != nil {
		onSessionsCreated(liveSessions)
	}

	// Poll DB until all live agents finish or timeout. Uses GroupCompleted
	// for termination detection instead of per-session name-based polling.
	liveResults, pollErr := pollAgents(ctx, d, liveAgents, liveSessions, opts.Timeout, liveSpawnTimes, opts.OnProgress, groupID)

	// Sessions persist — do NOT kill them here. The user can re-read them later.
	// Containers, tmux sessions, and sidecars remain alive until prism cleanup
	// is invoked on the parent, which cascades via KillReviewSessionsForParent.

	// Merge live results with spawn-failure results, preserving original agent order.
	results := make([]AgentResult, len(agents))
	liveIdx := 0
	for i, ag := range agents {
		if spawnErr[i] != nil {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: failed to spawn agent: %v", spawnErr[i]),
				IsError: true,
			}
		} else {
			results[i] = liveResults[liveIdx]
			liveIdx++
		}
	}

	return results, pollErr
}

// RunAsync spawns the review agents (same as Run's spawn phase), registers a
// group, starts a detached monitor process, and returns immediately with an
// AsyncResult. The monitor process will poll for group completion and deliver
// aggregated results to opts.WorkerSession via prism prompt.
//
// Unlike Run, RunAsync does NOT block while agents execute. The caller should
// display Ack to the worker and proceed without waiting for review results.
//
// opts.WorkerSession must be set to the session name that will receive the
// delivery prompt when the review completes.
//
// prismBinary is passed to StartMonitorProcess; pass "" to use os.Executable().
func RunAsync(opts Opts, prismBinary string) (*AsyncResult, error) {
	if opts.ParentSession == "" {
		return nil, fmt.Errorf("parent session name is required")
	}
	if opts.WorkerSession == "" {
		return nil, fmt.Errorf("worker session name is required for async review")
	}

	worktree := opts.Worktree
	if worktree == "" {
		return nil, fmt.Errorf("worktree path is required")
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Agents()
	}

	// In-progress guard: reject if a round is already active for this parent.
	// We check for any group with at least one non-terminal member.
	activeGroupID, activeErr := ActiveReviewGroupForParent(d, opts.ParentSession)
	if activeErr != nil {
		// Non-fatal: log and proceed (better to allow duplicate than block falsely).
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not check for active review group: %v\n", activeErr)
	} else if activeGroupID != "" {
		// Determine the round number for a useful error message.
		members, _ := d.GroupMembersForParent(opts.ParentSession)
		activeRound := ReviewRoundForGroup(members)
		if activeRound > 0 {
			return nil, fmt.Errorf("prism review: round %d is already in progress for this PR (group %s).\n"+
				"Wait for it to complete or cancel the sessions with `prism cleanup`.",
				activeRound, activeGroupID)
		}
		return nil, fmt.Errorf("prism review: a review round is already in progress for session %q (group %s).\n"+
			"Wait for it to complete or cancel the sessions with `prism cleanup`.",
			opts.ParentSession, activeGroupID)
	}

	// Determine round number from DB.
	round := NextRoundNumber(d, opts.ParentSession)
	roundPrefix := fmt.Sprintf("%s~review-%d-", opts.ParentSession, round)
	repo := deriveRepo(opts.ParentSession)

	// Resolve the runtime active profile once for the round (#1207). See
	// the matching block in Run() above for rationale.
	activeProfile, _, profErr := config.ResolveActiveProfile(opts.ProfilesFile, "")
	if profErr != nil {
		return nil, fmt.Errorf("resolve active profile: %w", profErr)
	}

	// Register session group.
	groupID, groupErr := d.RegisterGroup(opts.ParentSession)
	if groupErr != nil {
		return nil, fmt.Errorf("register review group: %w", groupErr)
	}

	// Spawn each agent.
	agentSessions := make([]string, len(agents))
	spawnErr := make([]error, len(agents))

	for i, ag := range agents {
		agentSession := roundPrefix + ag.Name
		agentSessions[i] = agentSession

		prCtxWithWorktree := opts.PRCtx
		if prCtxWithWorktree != nil && !prCtxWithWorktree.FetchFailed {
			ctxCopy := *prCtxWithWorktree
			ctxCopy.WorktreePath = worktree
			prCtxWithWorktree = &ctxCopy
		}
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree)

		agentConfigContent, configErr := ResolveAgentConfigContent(opts.IsolationMode, opts.ProfilesFile, ag.Name, activeProfile)
		if configErr != nil {
			spawnErr[i] = configErr
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), configErr))
			}
			continue
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. Mirrors the pattern in
		// cmd/spawn.go:386-394 for regular bwrap spawns.
		// sandbox-exec mode does NOT yet use this path — config delivery for
		// sandbox-exec is deferred to #1016.
		//
		// D3 (issue #1133): the bwrap-only gate routes the write through
		// Isolator.WriteHarnessConfigBlob so the dispatch goes via the
		// registered isolator. The gate stays narrowed to bwrap to preserve
		// the original behaviour.
		if opts.IsolationMode == string(config.IsolationBwrap) && agentConfigContent != "" {
			iso, isoErr := container.For(config.IsolationMode(opts.IsolationMode), container.ConstructorOpts{Name: agentSession})
			if isoErr != nil {
				spawnErr[i] = fmt.Errorf("review: resolve isolator for bwrap agent %s: %w", ag.Name, isoErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
			if writeErr := iso.WriteHarnessConfigBlob(agentSession, agentConfigContent); writeErr != nil {
				spawnErr[i] = fmt.Errorf("review: write opencode config for bwrap agent %s: %w", ag.Name, writeErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
		}

		spawnOpts := session.SpawnOpts{
			SessionName:      agentSession,
			Repo:             repo,
			Worktree:         worktree,
			AgentRole:        ag.Name,
			Prompt:           prompt,
			ConfigContent:    agentConfigContent,
			Layout:           session.LayoutAgentOnly,
			IsolationMode:    opts.IsolationMode,
			PluginHostPath:   opts.PluginHostPath,
			WorktreeReadOnly: true,
			GroupID:          groupID,
			RuntimeEnvVars:   opts.RuntimeEnvVars,
			HarnessName:      opts.Harness,
		}
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnSessErr))
			}
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("spawn session for %s: %w", ag.Name, spawnSessErr)
			continue
		}

		// "started" is now emitted by the readiness gate below, not here.
		// See #1051 — "spawned" is not the same as "opencode is ready".
	}

	// Per-agent readiness gate (#1051 Piece A). Runs concurrently so one
	// slow agent does not delay the others. Updates spawnErr[i] for agents
	// whose gate trips, and emits "<role> started" or
	// "<role> failed to start: not ready within <timeout>" via OnProgress.
	// spawnTimes is unused by RunAsync (the monitor process owns timing
	// from this point on), but the gate signature requires it; allocate a
	// throwaway slice so the in-loop write does not blow up.
	gateSpawnTimes := make([]time.Time, len(agents))
	gateReviewAgents(d, agents, agentSessions, spawnErr, gateSpawnTimes,
		opts.ReadinessTimeout, opts.OnProgress)

	// Check if all agents failed to spawn or to become ready.
	allFailed := true
	for _, se := range spawnErr {
		if se == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		return nil, fmt.Errorf("all review agents failed to spawn")
	}

	// Collect successfully-spawned-and-ready sessions, plus the failure
	// list for the Ack (#1051 Piece C) so the worker sees which agents
	// did not come up and why.
	var liveSessions []string
	var liveAgents []Agent
	type failedAgent struct {
		name   string
		reason string
	}
	var failures []failedAgent
	for i, se := range spawnErr {
		if se == nil {
			liveSessions = append(liveSessions, agentSessions[i])
			liveAgents = append(liveAgents, agents[i])
		} else {
			failures = append(failures, failedAgent{
				name:   agents[i].Name,
				reason: failureReason(se),
			})
		}
	}

	// Start the detached monitor process.
	monitorOpts := MonitorOpts{
		GroupID:       groupID,
		WorkerSession: opts.WorkerSession,
		PRNumber:      opts.PRNumber,
		Round:         round,
		Agents:        liveAgents,
		AgentSessions: liveSessions,
		DBPath:        dbPath,
		Timeout: opts.Timeout * 2, // 2x per-agent timeout as group monitor limit
	}
	if startErr := StartMonitorProcess(monitorOpts, prismBinary); startErr != nil {
		// Monitor failed to start — not fatal for spawning, but warn loudly.
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not start monitor process: %v\n"+
			"Review results will NOT be delivered automatically.\n"+
			"Check agent progress with: prism checkin %s~review-%d-<agent>\n",
			startErr, opts.ParentSession, round)
	}

	// Transition the worker session to "reviewing" so that:
	//   1. The coordinator does not receive a premature "has finished" notification.
	//   2. The dashboard and `prism list-sessions` display the worker as awaiting
	//      review results rather than finished or idle.
	// This write uses the still-open DB handle (d). The sidecar's upsertState
	// path is intentionally bypassed here: the sidecar is running in the worker
	// session and will respect the reviewing state when it checks currentDBState()
	// before firing the coordinator notification.
	//
	// Non-fatal: if the update fails (e.g. session row not found) the monitor
	// will still deliver results and the worker will still receive them — only
	// the interim display state is affected.
	if workerStatus, stErr := d.CurrentStatus(opts.WorkerSession); stErr == nil && workerStatus != nil {
		if err := d.UpsertStatus(opts.WorkerSession, workerStatus.Repo, workerStatus.Worktree,
			string(agent.StateReviewing), nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "[prism review] warning: could not set worker state to reviewing: %v\n", err)
		}
	} else if stErr != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not look up worker session %q: %v\n", opts.WorkerSession, stErr)
	}

	// Build acknowledgement message (#1051 Piece C: surface partial-success).
	failurePairs := make([][2]string, 0, len(failures))
	for _, f := range failures {
		failurePairs = append(failurePairs, [2]string{f.name, f.reason})
	}
	ack := buildAsyncAck(opts.PRNumber, round, groupID, liveSessions, failurePairs, opts.WorkerSession)

	return &AsyncResult{
		GroupID:      groupID,
		SessionNames: liveSessions,
		Round:        round,
		Ack:          ack,
	}, nil
}

// buildAsyncAck constructs the acknowledgement message returned to the worker
// immediately after spawning the review agents. failures is a list of
// (agentName, reason) pairs for agents that did not become ready — see
// #1051 AC-5: "Coordinators reading the Ack should be able to see
// `Spawned: 3, Failed: 2 (review-goal: not ready within 30s, …)`."
func buildAsyncAck(prNumber string, round int, groupID string, sessionNames []string, failures [][2]string, workerSession string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Review in progress — PR #%s, round %d (group: %s)\n\n", prNumber, round, groupID))

	// Spawned/Failed summary line — the headline scan target for operators
	// reading the Ack at a glance. Always emit both numbers, even when
	// Failed is 0, so the format is stable across runs.
	sb.WriteString(fmt.Sprintf("Spawned: %d", len(sessionNames)))
	if len(failures) > 0 {
		// Inline reasons in the same order as the failures slice (which
		// preserves the original agent order from the spawn loop).
		var parts []string
		for _, f := range failures {
			parts = append(parts, fmt.Sprintf("%s: %s", f[0], f[1]))
		}
		sb.WriteString(fmt.Sprintf(", Failed: %d (%s)", len(failures), strings.Join(parts, ", ")))
	} else {
		sb.WriteString(", Failed: 0")
	}
	sb.WriteString("\n\n")

	if len(sessionNames) > 0 {
		sb.WriteString(fmt.Sprintf("Spawned %d review agents:\n", len(sessionNames)))
		for _, name := range sessionNames {
			sb.WriteString(fmt.Sprintf("  • %s\n", name))
		}
	}
	if len(failures) > 0 {
		sb.WriteString(fmt.Sprintf("\n%d agent(s) failed to start. Inspect the per-session startup log for details:\n", len(failures)))
		for _, f := range failures {
			sb.WriteString(fmt.Sprintf("  • %s — %s\n", f[0], f[1]))
		}
		sb.WriteString("\nStartup logs:\n")
		sb.WriteString("  prism logs <session> --startup            # spawn-time breadcrumbs\n")
		sb.WriteString("  prism logs <session> --agent-run          # bwrap stderr (if it got that far)\n")
	}
	sb.WriteString(fmt.Sprintf("\nResults will be delivered to session %q via prism prompt when all agents complete.\n", workerSession))
	sb.WriteString("\n**Do NOT commit, merge, or announce completion** until the review-complete prompt arrives.\n")
	if len(sessionNames) > 0 {
		sb.WriteString(fmt.Sprintf("\nYou may monitor progress with:\n  prism checkin %s~review-%d-review-goal\n", workerSession, round))
	}
	return sb.String()
}

// LookupParentSession looks up the parent session from the environment or DB.
// When called from inside a container, PRISM_SESSION_NAME is set to the
// agent's session name. We use that directly (the worker's session name).
func LookupParentSession() string {
	// Try PRISM_SESSION_NAME first (set when running inside a container/agent).
	if s := os.Getenv("PRISM_SESSION_NAME"); s != "" {
		return s
	}
	// Fall back to TMUX current session.
	sess, err := tmux.CurrentSession()
	if err != nil {
		return ""
	}
	return sess
}
