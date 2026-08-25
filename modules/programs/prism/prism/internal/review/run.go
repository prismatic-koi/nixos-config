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
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/profile"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// Run executes the review. It returns the aggregated results and an error.
//
// Each agent is spawned as its own independent top-level tmux session named
// <parent>~review-<N>-<agent.Name>. Previous rounds' sessions are NOT killed
// here — that is a deliberate behaviour change from the old multi-window round
// model, and it still holds.
//
// What releases them has changed (#2649). A round's sessions are now released
// automatically 15 minutes after its review-complete prompt is delivered; they
// are no longer held until prism cleanup of the parent. That release gates on
// session_groups.delivered_at, which only the async delivery paths write
// (MonitorFunc and the recovery watcher), so it does not apply to a round this
// synchronous entry point ran. Run has no production caller today — RunAsync
// is the live path — so no round actually reaches that state.
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

	// Determine round number from DB. We do NOT kill previous review sessions
	// here (deliberate). Since #2649 they are released automatically 15 minutes
	// after their round is delivered — see the Run doc comment for why that
	// release does not cover a round this function ran.
	round := NextRoundNumber(d, opts.ParentSession)
	roundPrefix := fmt.Sprintf("%s~review-%d-", opts.ParentSession, round)

	// sessionname.Repo, not the old local deriveRepo: the old helper split on
	// "@" alone, so a non-worktree parent such as `obsidian` gave each of its
	// review agents a repo of its own — `obsidian~investigate-v2` instead of
	// `obsidian` (issue #2658). The repo written here is what every later
	// repo-scoped query reads.
	repo := sessionname.Repo(opts.ParentSession)

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Agents()
	}

	// Resolve the runtime active profile once for the whole round so every
	// agent in this fan-out spawns with the same models (#1207). Errors are
	// surfaced — a corrupt state file would otherwise silently leak the
	// pf.Default profile into review agents.
	//
	// Issue #2097: the parent worker's spawn-time profile (recorded on
	// `spawn_inputs.profile_name` by #2090) is fed as the highest-precedence
	// input via profile.InheritFromParent. Before #2097 we passed "" here,
	// so the resolution silently fell through to state-file > nix-default —
	// every review fan-out since `--abtest` (#1216) shipped ran on the host
	// default regardless of the parent's `--profile` choice. The #1207
	// single-resolve-per-round invariant is preserved: this call happens
	// once, outside the agent loop, so all 5 reviewers share one profile.
	activeProfile, profErr := profile.InheritFromParent(d, opts.ParentSession, opts.ProfilesFile)
	if profErr != nil {
		return nil, fmt.Errorf("resolve active profile: %w", profErr)
	}

	// RequireSlot gate: validate that the active profile defines a slot for
	// every review agent before spawning any. This is an all-or-nothing check
	// (#1224): if any slot is missing we fail the entire fan-out with a clear
	// error rather than silently falling back to the legacy model chain.
	for _, ag := range agents {
		if slotErr := config.RequireSlot(opts.ProfilesFile, activeProfile, ag.Name); slotErr != nil {
			return nil, fmt.Errorf("review fan-out aborted: profile %q is missing a required slot: %w", activeProfile, slotErr)
		}
	}

	// Register a session group for this review round. Every spawned agent
	// session will carry this group_id, enabling GroupCompleted-based
	// termination detection and GroupResults-based result aggregation.
	// Fail fast if RegisterGroup fails — no sessions are spawned without a
	// group to belong to (AC: edge-case).
	//
	// PRNumber and round are persisted on session_groups so the worker
	// sidecar's review-completion recovery watcher (#1709 reopen) can
	// reconstruct the delivery message when the detached monitor subprocess
	// dies before delivering.
	groupID, groupErr := d.RegisterGroupWithPR(opts.ParentSession, opts.PRNumber, round)
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
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree, ag.Name)

		// The role rubric arrives via the agent's system prompt (prism.ts,
		// before_agent_start), not this Go-built prompt (issue #2534). Surface
		// a missing/empty role file so the degraded (rubric-less) prompt is
		// visible rather than silent — the agent still starts.
		if roleDefinitionMissing(ag.Name) && opts.OnProgress != nil {
			opts.OnProgress(fmt.Sprintf("%s: role definition missing or empty at %s — starting with a degraded system prompt", FormatAgentDisplayName(ag.Name), roleDefinitionPath(ag.Name)))
		}

		// Spawn the per-agent session via the shared primitive. SpawnSession
		// handles DB seed (with root_agent_name from AgentRole), port
		// allocation, tmux session creation, and sidecar startup — keeping
		// review.go free of direct db/tmux/sidecar machinery (#859).
		//
		// WorktreeReadOnly=true ensures review containers cannot modify the
		// branch under review (satisfies the [security] acceptance criterion).
		//
		// Pi is the sole harness. Use it directly unless --harness was
		// explicitly passed (opts.HarnessExplicit).
		agentHarnessName := opts.Harness
		if !opts.HarnessExplicit {
			agentHarnessName = "pi"
		}
		agentH, agentHErr := harness.New(agentHarnessName, "", nil, "", "")
		if agentHErr != nil {
			spawnErr[i] = fmt.Errorf("review: unknown harness %q for agent %s: valid harnesses: %s",
				agentHarnessName, ag.Name, strings.Join(harness.Names(), ", "))
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %s", FormatAgentDisplayName(ag.Name), sanitizeSpawnError(opts.PRNumber, ag.Name, spawnErr[i])))
			}
			continue
		}
		// Build the per-reviewer SpawnOpts via the shared builder so the
		// audit-row shape, isolation-mode propagation, and #2097
		// ProfileName inheritance are guaranteed identical between Run
		// (sync) and RunAsync (async monitor). See spawn_opts.go for the
		// field-level rationale.
		spawnOpts := newReviewerSpawnOpts(reviewerSpawnInput{
			AgentName:          ag.Name,
			AgentSession:       agentSession,
			Prompt:             prompt,
			Repo:               repo,
			Worktree:           worktree,
			PromptTemplateHash: ReviewPromptTemplateHash(),
			IsolationMode:      opts.IsolationMode,
			PluginHostPath:     opts.PluginHostPath,
			GroupID:            groupID,
			HarnessName:        agentHarnessName,
			HarnessHandle:      agentH,
			ModelsByRole:       opts.ModelsByRole,
			PIExtensionDir:     opts.PIExtensionDir,
			ProfileName:        activeProfile,
			InvokerSession:     opts.ParentSession,
		})
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %s", FormatAgentDisplayName(ag.Name), sanitizeSpawnError(opts.PRNumber, ag.Name, spawnSessErr)))
			}
			// Clean up this agent's resources (sidecar, DB row, tmux session).
			// SpawnSession may have partially progressed; be defensive so a
			// second spawn attempt with the same name doesn't see stale state.
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession, db.ReapCauseSpawnFailure, sanitizeSpawnError(opts.PRNumber, ag.Name, spawnSessErr))
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

	// Do NOT kill the sessions here. Since #2649 a round is released
	// automatically 15 minutes after its review-complete prompt is delivered,
	// and prism cleanup of the parent still cascades via
	// KillReviewSessionsForParent. Neither is this function's job. Re-reading a
	// past round does not need a live session either way: the release and the
	// cascade both preserve every agent_events row, so
	// `prism checkin <parent>~review-<N>-<agent>` keeps working.

	// Merge live results with spawn-failure results, preserving original agent order.
	results := make([]AgentResult, len(agents))
	liveIdx := 0
	for i, ag := range agents {
		if spawnErr[i] != nil {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: failed to spawn agent: %s", sanitizeSpawnError(opts.PRNumber, ag.Name, spawnErr[i])),
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
		proglog.Warnf("[prism review] warning: could not check for active review group: %v\n", activeErr)
	} else if activeGroupID != "" {
		// Determine the round number for a useful error message. Filter
		// members to just the active group's rows before extracting the
		// round — without this filter, ReviewRoundForGroup returns the
		// first round it encounters across ALL groups for this parent,
		// which is almost always round 1 even when the stuck group is
		// round N>1 (#1962 Bug B).
		allMembers, _ := d.GroupMembersForParent(opts.ParentSession)
		activeMembers := make([]db.Status, 0, len(allMembers))
		for _, m := range allMembers {
			if m.GroupID != nil && *m.GroupID == activeGroupID {
				activeMembers = append(activeMembers, m)
			}
		}
		activeRound := ReviewRoundForGroup(activeMembers)
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
	// See the matching call in Run() above for why this is sessionname.Repo.
	repo := sessionname.Repo(opts.ParentSession)

	// Resolve the runtime active profile once for the round (#1207). See
	// the matching block in Run() above for rationale, including the
	// #2097 inheritance of the parent worker's spawn-time profile.
	activeProfile, profErr := profile.InheritFromParent(d, opts.ParentSession, opts.ProfilesFile)
	if profErr != nil {
		return nil, fmt.Errorf("resolve active profile: %w", profErr)
	}

	// RequireSlot gate: same all-or-nothing check as in Run() (#1224). Validate
	// every review agent slot before registering a group or spawning anything.
	for _, ag := range agents {
		if slotErr := config.RequireSlot(opts.ProfilesFile, activeProfile, ag.Name); slotErr != nil {
			return nil, fmt.Errorf("review fan-out aborted: profile %q is missing a required slot: %w", activeProfile, slotErr)
		}
	}

	// Register session group. PRNumber and round are persisted on
	// session_groups so the worker sidecar's review-completion recovery
	// watcher (#1709 reopen) can reconstruct the delivery message when the
	// detached monitor subprocess dies before delivering.
	groupID, groupErr := d.RegisterGroupWithPR(opts.ParentSession, opts.PRNumber, round)
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
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree, ag.Name)

		if roleDefinitionMissing(ag.Name) && opts.OnProgress != nil {
			opts.OnProgress(fmt.Sprintf("%s: role definition missing or empty at %s — starting with a degraded system prompt", FormatAgentDisplayName(ag.Name), roleDefinitionPath(ag.Name)))
		}

		// Pi is the sole harness. Use it directly unless --harness was explicitly passed.
		asyncAgentHarnessName := opts.Harness
		if !opts.HarnessExplicit {
			asyncAgentHarnessName = "pi"
		}
		asyncAgentH, asyncAgentHErr := harness.New(asyncAgentHarnessName, "", nil, "", "")
		if asyncAgentHErr != nil {
			spawnErr[i] = fmt.Errorf("review: unknown harness %q for agent %s: valid harnesses: %s",
				asyncAgentHarnessName, ag.Name, strings.Join(harness.Names(), ", "))
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %s", FormatAgentDisplayName(ag.Name), sanitizeSpawnError(opts.PRNumber, ag.Name, spawnErr[i])))
			}
			continue
		}
		// Build the per-reviewer SpawnOpts via the shared builder so
		// RunAsync and Run produce structurally identical SpawnOpts.
		// See spawn_opts.go for the field-level rationale.
		spawnOpts := newReviewerSpawnOpts(reviewerSpawnInput{
			AgentName:          ag.Name,
			AgentSession:       agentSession,
			Prompt:             prompt,
			Repo:               repo,
			Worktree:           worktree,
			PromptTemplateHash: ReviewPromptTemplateHash(),
			IsolationMode:      opts.IsolationMode,
			PluginHostPath:     opts.PluginHostPath,
			GroupID:            groupID,
			HarnessName:        asyncAgentHarnessName,
			HarnessHandle:      asyncAgentH,
			ModelsByRole:       opts.ModelsByRole,
			PIExtensionDir:     opts.PIExtensionDir,
			ProfileName:        activeProfile,
			InvokerSession:     opts.ParentSession,
		})
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %s", FormatAgentDisplayName(ag.Name), sanitizeSpawnError(opts.PRNumber, ag.Name, spawnSessErr)))
			}
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession, db.ReapCauseSpawnFailure, sanitizeSpawnError(opts.PRNumber, ag.Name, spawnSessErr))
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("spawn session for %s: %w", ag.Name, spawnSessErr)
			continue
		}

		// "started" is now emitted by the readiness gate below, not here.
		// See #1051 — "spawned" is not the same as "agent is ready".
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
				reason: failureReason(opts.PRNumber, agents[i].Name, se),
			})
		}
	}

	// Start the detached monitor process.
	//
	// Agents/AgentSessions carry the FULL spawned set, not just the agents
	// that came up (#2613). ClassifyRound walks exactly this list to decide
	// how many agents the round expected, so passing only the live set made
	// the round shrink to fit its own failures: four live agents that all
	// passed produced Expected=4, Missing=0, and therefore a "complete"
	// round that printed "All 5 review agents passed" and consumed one of
	// the worker's three cycles. That is the exact failure #2573 closed for
	// mid-round reaps, reopened on the spawn-time path. An agent that never
	// came up now stays in the expected set and is reported as missing, with
	// the cause its cleanup recorded.
	//
	// The other consumers of these fields tolerate the wider set:
	// forceTerminateStuckMembers skips rows that are already closed or
	// terminal, and the poll loop keys off GroupCompleted(groupID), not this
	// list.
	expectedAgents, expectedSessions := expectedRoundSet(agents, agentSessions, spawnErr)
	monitorOpts := MonitorOpts{
		GroupID:       groupID,
		WorkerSession: opts.WorkerSession,
		PRNumber:      opts.PRNumber,
		Round:         round,
		Agents:        expectedAgents,
		AgentSessions: expectedSessions,
		DBPath:        dbPath,
		Timeout:       opts.Timeout * 2, // 2x per-agent timeout as group monitor limit
		// Release this round's agent sessions ReapGracePeriod after the
		// review-complete prompt lands (#2649). Without this the round's five
		// sessions hold a concurrency slot and a harness port until a human
		// notices.
		ReapAfterDelivery: true,
	}
	if startErr := StartMonitorProcess(monitorOpts, prismBinary); startErr != nil {
		// Monitor failed to start — not fatal for spawning, but warn loudly.
		proglog.Warnf("[prism review] warning: could not start monitor process: %v\n"+
			"Review results will NOT be delivered automatically.\n"+
			"Check agent progress with: prism checkin %s~review-%d-<agent>\n",
			startErr, opts.ParentSession, round)
	}

	// Transition the worker session to "reviewing" so that:
	//   1. The coordinator does not receive a premature "has finished" notification.
	//   2. The dashboard and `prism sessions list` display the worker as awaiting
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
			proglog.Warnf("[prism review] warning: could not set worker state to reviewing: %v\n", err)
		}
	} else if stErr != nil {
		proglog.Warnf("[prism review] warning: could not look up worker session %q: %v\n", opts.WorkerSession, stErr)
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

// expectedRoundSet returns the (agents, sessions) pair the round's monitor
// must treat as its EXPECTED set.
//
// It returns the full spawned set. spawnErr is accepted, and deliberately not
// used to filter, because filtering by it is the defect this function exists
// to prevent (#2613): ClassifyRound derives RoundStatus.Expected from the
// list it is given, so a monitor handed only the agents that came up counts a
// four-agent round as complete. Four PASS verdicts then render as "All 5
// review agents passed" and consume one of the worker's three cycles, while
// the fifth dimension was never examined. That is the same silent-pass
// failure #2573 closed for a mid-round reap, on the spawn-time path.
//
// An agent that failed to spawn or to become ready stays in the expected set
// and surfaces as a missing verdict, classified by the cause its cleanup
// recorded. The round is therefore incomplete, does not count toward the
// 3-cycle limit, and reads as unreviewed rather than as a pass.
func expectedRoundSet(agents []Agent, agentSessions []string, spawnErr []error) ([]Agent, []string) {
	_ = spawnErr // see the doc comment: the expected set is never filtered.
	return agents, agentSessions
}
