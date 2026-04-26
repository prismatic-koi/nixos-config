# D.2 — File-Size Hot-Spots: Split Candidates

> **Track D — Cross-cutting cleanups.**
> Related: inventory §5.1 (files ≥2000 LoC) · design doc #1072 · D.1 (function-size hot-spots) · D.3 (test-naming audit)

## Context recap

The architecture inventory (`docs/architecture-inventory.md` §5.1) identified seven files that account for a disproportionate share of the codebase's total line count. Large files are not an error in themselves, but experience with this codebase suggests they accumulate multiple distinct concerns whose boundaries eventually become load-bearing — making even localised changes riskier than they need to be.

This document proposes a within-package file split for each of the seven files. **All proposed splits are same-package only.** No new Go packages are introduced here; where a natural grouping might warrant a new package, this document flags it and defers to a separate issue. No code changes are included in this document.

The primary targets (>2000 LoC) are:

| File | LoC | Top 3 functions by size |
|---|---|---|
| `internal/sidecar/sidecar.go` | 3794 | `hostAPIHandler` (1086), `Run` (372), `handleMessageUpdated` (257) |
| `internal/db/db.go` | 3214 | `Open` (683), `QueryEvents` (~106), `Prune` |
| `cmd/stats.go` | 2141 | `renderSessionCompactTable` (164), `runStatsAsks` (139), `runStatsHistorical` (132) |
| `internal/review/review.go` | 2071 | `RunAsync` (235), `Run` (245), `buildReviewPrompt` (150) |

Secondary targets (1000–2000 LoC):

| File | LoC |
|---|---|
| `internal/container/container.go` | 1753 |
| `cmd/checkin.go` | 1680 |
| `cmd/switch.go` | 1229 |

---

## 1. `internal/sidecar/sidecar.go` (3794 LoC)

### Concerns currently mixed

1. **Lifecycle and startup** — `Run()` (372 LoC) drives the complete session boot sequence: Darwin TCP listener binding, podman container creation, `WaitHealthy` / `CreateSession` sequencing, host-API server startup, merge-queue watcher launch, SSE loop, and startup-connect timeout.

2. **SSE event dispatch** — `HandleEvent()` is the router; it delegates to twelve `handle*` methods covering server events, session lifecycle, permission/question flows, and message-update handling. This is the core state-machine logic.

3. **Host-API HTTP server** — `hostAPIHandler()` (1086 LoC) defines the entire HTTP API exposed to sandboxed agents: ten routes (`/spawn`, `/review`, `/cleanup`, `/switch`, `/list-sessions`, `/checkin`, `/prompt`, `/merge`, `/merges`, `/merges/cancel`), role-based access guards, request parsing, subprocess delegation to the prism binary, and log-follow streaming. This alone is 28 % of the file.

4. **Coordinator notification** — `notifyCoordinator()`, `validateOrRefreshCoordinatorSID()`, `deliverNotificationViaHTTP()`, and `buildNotifyPromptBody()` form a self-contained HTTP-delivery subsystem that is only invoked from the event-handler path on session finish.

5. **Dashboard integration** — `pushDashboardEvent()`, `dashboardSocketPath()`, and `touchDashboardSentinel()` are low-coupling helpers whose only job is to emit state-change signals to the dashboard process.

6. **State machine internals and helpers** — `upsertState()`, `writeStateChangeWithSID()`, `writeEvent()`, `cancelIdleTimer()`, `cancelRecoveryTimer()`, `isReviewAgentSession()`, `writeStartupError()`, `notifyParentWorkerOnStartupFailure()`, and the timer/clock abstractions live in the file but have no structural dependency on the host-API concern.

### Proposed split

| Proposed file | Contents |
|---|---|
| `sidecar.go` | `Sidecar` struct + fields, `Config`, `Clock`/`Timer` interfaces, `New()`, `Shutdown()` — the structural core |
| `run.go` | `Run()` — session lifecycle and startup sequencing |
| `events.go` | `HandleEvent()` + all `handle*` methods (`handleServerConnected`, `handleSessionStatus`, `handleSessionIdle`, `handleSessionCreated`, `handleSessionUpdated`, `handleSessionError`, `handleSessionCompacted`, `handleSessionDeleted`, `handlePermissionAsked`, `handlePermissionReplied`, `handleMessageUpdated`, `handleMessagePartUpdated`) |
| `state.go` | `upsertState()`, `writeStateChangeWithSID()`, `writeStateChange()`, `writeEvent()`, `writeStartupError()`, `cancelIdleTimer()`, `cancelRecoveryTimer()`, `currentDBState()` |
| `host_api.go` | `hostAPIHandler()`, `hostAPIServeLogsTail()`, `hostAPIServeLogsFollow()`, `worktreePathForSession()`, `isKnownReviewAgent()`, `knownReviewAgentNames()` |
| `notify.go` | `notifyCoordinator()`, `notifyParentWorkerOnStartupFailure()`, `reviewAgentParentSession()`, `validateOrRefreshCoordinatorSID()`, `deliverNotificationViaHTTP()`, `buildNotifyPromptBody()` |
| `dashboard.go` | `pushDashboardEvent()`, `dashboardSocketPath()`, `touchDashboardSentinel()` |
| `helpers.go` | `strPtr()`, `truncate()`, `marshalTruncated()`, `isHighImpactCommand()`, `extractBashCommand()`, `extractMessageIDFromPayload()`, `repoFromSession()`, `isCoordinator()`, `isCoordinatorSession()`, `isHostAPITerminalState()`, `isReviewAgentSession()`, `parseSpawnSessionName()` |

### Public-surface impact

None. All proposed splits are within the `sidecar` package. Exported identifiers (`Sidecar`, `Config`, `Clock`, `Timer`, `RealClock`, `New`, `Run`, `HandleEvent`, `Shutdown`, `IdleDebounce`, `DefaultStartupConnectTimeout`, `ReconnectRecoveryDelay`, `ErrorResumeDebounce`) remain in the package with identical signatures. Callers in `cmd/` and `internal/session/` see no change.

### Independence flag

**[uncertain] Partially depends on Track B.** `Run()` and `hostAPIHandler()` both contain explicit `container != nil` / `Container == nil` checks that encode the podman-vs-bwrap-vs-host distinction. B.3 (sidecar lifecycle for stdio harnesses) may restructure `Run()` to accommodate the PI harness; if that work lands before this split, the resulting `run.go` will look different from what is described above. The split of `events.go`, `state.go`, `notify.go`, `dashboard.go`, and `helpers.go` is independent of Track B and safe to do now.

`host_api.go` is also safe to extract now — the route handlers do not change shape under any Track B scenario (they delegate to the prism binary). The split is cosmetic relative to B.3.

**Recommended approach:** extract `dashboard.go`, `notify.go`, `helpers.go`, `state.go`, and `events.go` as an independent standalone PR. Hold `run.go` and `host_api.go` until after B.3 lands (or do them speculatively and accept a rebase).

---

## 2. `internal/db/db.go` (3214 LoC)

### Concerns currently mixed

1. **Schema definition and migration engine** — The `schema` constant (95 LoC DDL), the `Open()` function (683 LoC), and the full v1→v20 migration chain represent schema lifecycle management. The migration code is densely documented and nearly mechanical — each version block follows the same pattern — but it dominates the file numerically.

2. **Session lifecycle CRUD** — `UpsertStatus` and its five specialised variants (`UpsertStatusWithAgent`, `UpsertStatusSeedRootAgentName`, `UpsertStatusWithRootAgent`, `UpsertStatusIfNotTerminal`, `UpsertStatusInterruptedOverrideFinished`), `SetEnded`, `MarkAllEnded`, `ClearEnded`, `SetInstanceID`, `SetGroupID`, `ClearInstanceID`, `SetHostMode`, `SetIsolationMode`, `RefreshWorktree`, `UpdateHarnessSessionID`, `UpdateRootModelID`, `AllocatePort`, `ReleasePort`, `CurrentStatus`, `AllActiveStatus`, `AllActiveStatusForRepo`, `ActiveBwrapSessionCount`, `ActiveBwrapSessions`, `AllStatusesWithPrefix`, `WaitingCount` are all `agent_status`-table operations.

3. **Event log CRUD** — `WriteEvent`, `QueryEvents`, `QueryEventsByMessageIDs`, `AllSessionEvents`, `EventsSince`, `QueryDoomLoopEvents`, `QueryPermissionEvents`, `QueryAuditEvents` cover the `agent_events` table.

4. **Bus message CRUD** — `WriteBusMessage`, `WriteBusMessageDelivered`, `WriteBusMessageFailed`, `PurgeBusMessages`, `PurgeStaleInstanceMessages` and the scan helper `scanBusMessage` cover the `bus_messages` table.

5. **Session incarnation table** — `InsertSession`, `UpdateSessionEnded`, `UpdateSessionArchivePath`, and related queries on the `sessions` table (the immutable-per-incarnation record, separate from the mutable `agent_status` row).

6. **Session group + merge queue** — `RegisterGroup`, `GroupCompleted`, `GroupResults`, `CoordinatorForRepo`, `ConsecutiveSidecarFailures`, and the merge-queue accessors (`AbandonWatchingMerges`, `MergeQueueHead`, `CancelMerge`, etc., referenced elsewhere in the codebase) cover two logically separate concerns that were both added recently.

7. **Maintenance / pruning** — `Prune()`, `MarkAllEnded()`, `isTerminalState()` are maintenance operations with no transactional dependency on the CRUD operations above.

### Proposed split

| Proposed file | Contents |
|---|---|
| `db.go` | `DB` struct, `Open()` (schema + migrations), `Close()`, `Path()`, `QueryRow()` — the connection layer |
| `status.go` | All `agent_status`-table operations: `UpsertStatus` family, `SetEnded`, `MarkAllEnded`, `ClearEnded`, `SetInstanceID`, `SetGroupID`, `ClearInstanceID`, `SetHostMode`, `SetIsolationMode`, `RefreshWorktree`, `UpdateHarnessSessionID`, `UpdateRootModelID`, `AllocatePort`, `ReleasePort`, `CurrentStatus`, `AllActiveStatus*`, `ActiveBwrap*`, `AllStatusesWithPrefix`, `WaitingCount`, `CoordinatorForRepo`; helpers `queryStatuses()`, `scanStatus()`, `portAvailable()`, `isTerminalState()` |
| `events.go` | `WriteEvent`, `QueryEvents`, `QueryEventsByMessageIDs`, `AllSessionEvents`, `EventsSince`, `QueryDoomLoopEvents`, `QueryPermissionEvents`, `QueryAuditEvents`, `ConsecutiveSidecarFailures` |
| `bus.go` | `WriteBusMessage`, `WriteBusMessageDelivered`, `WriteBusMessageFailed`, `PurgeBusMessages`, `PurgeStaleInstanceMessages`; helper `scanBusMessage()` |
| `sessions.go` | `InsertSession`, `UpdateSessionEnded`, `UpdateSessionArchivePath`; the `Session` type and any sessions-table query helpers |
| `groups.go` | `RegisterGroup`, `GroupCompleted`, `GroupResults`; the `GroupMemberResult` type and group-specific helpers |
| `mergequeue.go` | All merge-queue operations on `pending_merges` (`AbandonWatchingMerges`, `MergeQueueHead`, `CancelMerge`, and any others) — [uncertain: see note below] |
| `types.go` | `Event`, `Status`, `BusMessage`, `Session`, `GroupMemberResult`, `EffectiveIsolationMode()` method — shared data types |
| `maintenance.go` | `Prune()` |

> [uncertain] The merge-queue operations in `db.go` are accessed by `internal/mergequeue/mergequeue.go`. Before extracting `mergequeue.go`, confirm whether those operations are already fully defined in `db.go` or partially in `mergequeue/`. If the latter, consolidating them first into `db.go` then extracting `db/mergequeue.go` is cleaner than a partial extraction.

### Public-surface impact

None. All proposed splits are within the `db` package. Exported types (`DB`, `Event`, `Status`, `BusMessage`, `Session`, `GroupMemberResult`, `PortRangeStart`, `PortRangeEnd`) and all exported methods remain identical. Callers throughout `cmd/`, `internal/sidecar/`, `internal/review/`, `internal/session/`, and `internal/mergequeue/` see no change.

### Independence flag

**Safe to do standalone.** `db.go` has no Track A/B dependencies — it is the data layer, not the mode-dispatch or harness layer. The `Open()` function does contain migration blocks that reference schema columns added by harness-agnostic refactors (e.g. `harness`, `harness_session_id`, `isolation_mode`), but those are already present and the migration list only grows; the split does not need to anticipate future migration content.

---

## 3. `cmd/stats.go` (2141 LoC)

### Concerns currently mixed

1. **Top-level command wiring and entry points** — `init()` registers `statsCmd` with cobra and attaches flags; `runStats()` is the router that dispatches to the per-subcommand handlers based on flag combinations (`--days`, `--doomloops`, `--denials`, `--asks`).

2. **Per-incarnation detail and compact rendering** — `renderIncarnationDetail()` (127 LoC), `renderSessionDetail()` (130 LoC), and `renderSessionCompactTable()` (164 LoC) render rich multi-block output for a single session incarnation. They own the token/cost/turn breakdown display logic.

3. **Aggregate / historical statistics** — `runStatsHistorical()` (132 LoC), `runStatsSummary()` (114 LoC), and `runStatsIncarnations()` (130 LoC) query across many sessions and render time-bucketed or cross-session aggregate tables.

4. **Specialised event-log queries** — `runStatsDoomLoops()` (102 LoC), `runStatsDenials()` (128 LoC), `runStatsAsks()` (139 LoC) each query a specific event type, aggregate it, and render a dedicated table. They share no code with the per-session or historical paths.

5. **Metrics computation** — `collectMetrics()` (104 LoC), `sessionMetrics` struct and its methods (`totalCost`, `duration`, `avgTurnDuration`, `longestTurnDuration`, `totalToolCalls`, `isLegacy`), `groupEventsByHarnessSessionID()`, `collectModelMetrics()` and `modelMetrics` are pure computation logic with no I/O. They are used by multiple rendering paths.

6. **Per-model breakdown** — `runStatsModel()` (29 LoC) and `renderModelBreakdown()` (66 LoC) are self-contained. Their `init()` block registers a separate `stats model` subcommand.

7. **Formatting helpers** — `formatTokenCount()`, `formatCost()`, `formatDurationLong()`, `agentShortName()`, `truncateStr()`, `percentileFloat64()`, `splitModel()`, `formatAgentSummary()`, `formatLatency()` are pure string/number formatters with no I/O.

### Proposed split

| Proposed file | Contents |
|---|---|
| `stats.go` | Cobra command registration (`statsCmd`, both `init()` blocks), `runStats()` router, `parseSinceFlag()`, `legacySentinel` constant, `modelCosts` map |
| `stats_metrics.go` | `sessionMetrics` struct + methods, `collectMetrics()`, `groupEventsByHarnessSessionID()`, `collectModelMetrics()`, `modelMetrics` struct, `computeTurnCost()` |
| `stats_render.go` | `renderIncarnationDetail()`, `renderSessionDetail()`, `renderSessionCompactTable()`, `runStatsDetail()`, `resolveSessionArg()` |
| `stats_aggregate.go` | `runStatsIncarnations()`, `runStatsSummary()`, `runStatsHistorical()`, `runStatsSession()` |
| `stats_events.go` | `runStatsDoomLoops()`, `runStatsDenials()`, `runStatsAsks()` |
| `stats_model.go` | `runStatsModel()`, `renderModelBreakdown()` |
| `stats_format.go` | `formatTokenCount()`, `formatCost()`, `formatDurationLong()`, `agentShortName()`, `truncateStr()`, `percentileFloat64()`, `splitModel()`, `formatAgentSummary()`, `formatLatency()` |

### Public-surface impact

None. `cmd` is an executable-only package with no exported symbols consumed by other packages. All splits are within the package boundary.

### Independence flag

**Safe to do standalone.** `stats.go` has no coupling to Track A, B, or C work in progress. Metrics and rendering are independent of isolation mode, harness shape, or A/B experimentation schema.

---

## 4. `internal/review/review.go` (2071 LoC)

### Concerns currently mixed

1. **PR context fetching** — `FetchPRContext()`, `FetchPRContextWithOpts()`, `FetchPRContextOpts`, `PRContext`, `prViewJSON`, and supporting helpers (`diffFilePath()`, `parseLinkedIssues()`, `runGitInWorktree()`, `runGH()`, `truncateDiff()`) constitute a self-contained GitHub CLI integration layer. They are invoked once before any agent is spawned and have no dependency on the spawn/poll machinery.

2. **Agent definition and configuration** — `Agent`, `Agents()`, `AgentsByName()`, `CheckAgentAvailability()`, `agentNames()`, `FormatAgentDisplayName()`, `ResolveAgentConfigContent()`, `knownReviewAgentNames()` (and its DB-backed counterparts) define the static catalogue of review agents and how to resolve per-agent configs.

3. **Synchronous run orchestration** — `Run()` (245 LoC) drives the full synchronous review loop: round numbering, group registration, per-agent spawn, readiness gating (via `gateReviewAgents` in `readiness.go`), polling, and result aggregation.

4. **Asynchronous run orchestration** — `RunAsync()` (235 LoC) is the fire-and-return path used by the host-API `/review` endpoint. It mirrors `Run()` in spawn/readiness but returns an `AsyncResult` instead of blocking for poll completion. The `buildAsyncAck()` helper and `AsyncResult` type belong here.

5. **Result aggregation and formatting** — `BuildResults()`, `FormatResults()`, `FindingsFilePath()`, `resolveSizeBudget()`, `AgentResult`, `VerdictKind`, `AssessPassed()`, `extractAssistantText()` turn raw DB rows into structured findings. This is pure post-processing with no spawn-time dependency.

6. **Session lifecycle helpers** — `NextRoundNumber()`, `KillSessionPrefix()`, `KillSessionsByNames()`, `KillCurrentRoundSessions()`, `LookupParentSession()`, `IsPerAgentSession()`, `KillReviewSessionsForParent()`, `KillReviewSessionsForParentWithDB()`, `CleanupReviewSessionsForParent()`, `cleanupAgentSession()`, `isTerminalAgentState()`, `pollAgents()`, `isTerminalState()` are session management utilities. Several (`pollAgents`, `buildResults`) are called only from `Run()` / `RunAsync()` but are separately testable.

7. **Test-export shims** — `BuildReviewPromptForTest()`, `TruncateDiffForTest()`, `ParseLinkedIssuesForTest()`, `DiffFilePathForTest()`, `BuildAsyncAckForTest()`, `PollAgentsForTest()` are exported wrappers for unexported functions to enable unit testing without `_test.go` being in a separate package. These should move to a dedicated `export_test.go` rather than a split file.

### Proposed split

| Proposed file | Contents |
|---|---|
| `review.go` | Package doc, constants (`DiffMaxBytes`, `DiffMaxLines`, `DiffInlineMaxLines`, `DiffInlineMaxBytes`), `PRContext`, `FetchPRContextOpts`, `Opts`, `AgentResult`, `AsyncResult`, `VerdictKind`, type aliases; `FetchPRContext()`, `FetchPRContextWithOpts()` |
| `agents.go` | `Agent`, `Agents()`, `AgentsByName()`, `CheckAgentAvailability()`, `agentNames()`, `FormatAgentDisplayName()`, `ResolveAgentConfigContent()` |
| `run.go` | `Run()`, `RunAsync()`, `buildAsyncAck()`, `LookupParentSession()` |
| `poll.go` | `pollAgents()`, `buildResults()`, `BuildResults()`, `isTerminalState()`, `isTerminalAgentState()` |
| `results.go` | `FormatResults()`, `FindingsFilePath()`, `resolveSizeBudget()`, `AssessPassed()`, `extractAssistantText()`, `failureReason()` |
| `prompt.go` | `buildReviewPrompt()`, `sanitisePRNumber()`, `sortStrings()`, `deriveRepo()`, `formatDuration()`, `defaultDBPath()` |
| `context.go` | `FetchPRContextWithOpts` internals: `prViewJSON`, `diffFilePath()`, `parseLinkedIssues()`, `runGitInWorktree()`, `runGH()`, `truncateDiff()` |
| `lifecycle.go` | `NextRoundNumber()`, `KillSessionPrefix()`, `KillSessionsByNames()`, `KillCurrentRoundSessions()`, `KillReviewSessionsForParent()`, `KillReviewSessionsForParentWithDB()`, `CleanupReviewSessionsForParent()`, `cleanupAgentSession()`, `IsPerAgentSession()` |
| `export_test.go` | All `*ForTest` shims, moved from `review.go` |

> Note: `readiness.go` already exists as a separate file in the package. The split above respects this existing boundary.

### Public-surface impact

None. All splits are within the `review` package. Exported symbols consumed by `cmd/review.go` (`Run`, `RunAsync`, `FetchPRContext`, `FetchPRContextWithOpts`, `Agents`, `AgentsByName`, `FormatResults`, etc.) retain identical signatures. The `export_test.go` relocation is convention-only and does not change test semantics.

### Independence flag

**Safe to do standalone.** `review.go` has no Track A/B dependencies. It does consume `container.NameForSession` for bwrap config write, which is covered by Track A's isolation-registry work, but the call site itself does not change shape under any Track A proposal.

---

## 5. `internal/container/container.go` (1753 LoC)

### Concerns currently mixed

1. **Podman container lifecycle** — `Create()` (90 LoC), `WaitHealthy()`, `isHealthy()`, `hasExited()`, `dumpLogs()`, `Shutdown()`, `EnsureRemoved()` are the core container management operations. They delegate the actual subprocess calls to the `Isolator` interface.

2. **Sandbox preparation (bwrap / sandbox-exec)** — `PrepareBwrap()` and `PrepareSandboxExec()` are preparation-only paths (no `podman run`). They write the same credential/config temp files and return an argument list. Conceptually distinct from lifecycle management.

3. **Credential and config file generation** — `writeSshConfig()`, `writeGitconfig()`, `writeClaudeCredentials()`, `WriteOpencodeConfig()`, `opencodeConfigFilePath()`, `sandboxHome()`, `sandboxExecProfilePath()` (via `sandbox_exec.go`), and `credentialEnvVars()` form a file-generation subsystem. These functions write the artefacts that `Create()`, `PrepareBwrap()`, and `PrepareSandboxExec()` depend on, but they are entirely separate in logic.

4. **Volume/bind-mount argument construction** — `buildRunArgs()` (565 LoC) constructs the `podman run` argument list. It is the single largest function in the file and mixes volume-mount logic, port binding, environment injection, and opencode command construction. It belongs to the podman-specific path.

5. **Directory preparation** — `prepareVolumeDirs()` eagerly creates host directories referenced as bind-mount sources. It is called by both `Create()` (podman) and `PrepareBwrap()` (bwrap), so it straddles both concerns.

6. **Naming and path helpers** — `NameForSession()`, `containerName()`, `sandboxHome()`, `gitdirFilePath()`, `sshConfigFilePath()`, `gitconfigFilePath()`, `allowedSignersFilePath()`, `opencodeConfigFilePath()`, `claudeCredentialsFilePath()`, `worktreeGitdirFilePath()` are pure path derivations.

7. **Availability and credential helpers** — `CheckAvailability()`, `githubAccountFromBareRoot()`, `githubAccountFromURL()`, `redactArgs()`, `IsNoSuchContainerError()`, `credentialEnvVars()` are cross-cutting helpers.

### Proposed split

| Proposed file | Contents |
|---|---|
| `container.go` | `Config`, `Manager` struct, `New()`, `isolationMode` type, `sandboxHome()`, `NameForSession()`, `containerName()`, path helpers (all `*FilePath()` methods), `CheckAvailability()`, `IsNoSuchContainerError()` |
| `lifecycle.go` | `Create()`, `WaitHealthy()`, `isHealthy()`, `hasExited()`, `dumpLogs()`, `Shutdown()`, `EnsureRemoved()` |
| `run_args.go` | `buildRunArgs()`, `prepareVolumeDirs()` |
| `credentials.go` | `writeSshConfig()`, `writeGitconfig()`, `writeClaudeCredentials()`, `WriteOpencodeConfig()`, `credentialEnvVars()`, `githubAccountFromBareRoot()`, `githubAccountFromURL()`, `redactArgs()` |
| `bwrap.go` | `PrepareBwrap()` _(already exists as a separate file — see note below)_ |
| `sandbox_exec.go` | `PrepareSandboxExec()` _(already exists as a separate file)_ |

> [uncertain] The package already has `bwrap.go` and `sandbox_exec.go` as separate files. Check what currently lives in each before proposing further splits — the bwrap-specific `bwrapIsolator.BuildArgs()` (488 LoC per D.1) already lives in a separate file. The split above is for the parts of `container.go` that still mix concerns, not a re-split of existing files.

### Public-surface impact

None. All splits are within the `container` package. Exported symbols (`Manager`, `Config`, `NameForSession`, `WriteOpencodeConfig`, `OpencodeConfigFilePath`, `CheckAvailability`, `New`, `ContainerPort`, `Image`, `DefaultHealthCheckTimeout`) retain identical signatures.

### Independence flag

**[uncertain] Depends on Track A (A.1/A.2).** Track A proposes an `Isolator` interface registry that would restructure how the four isolation modes are dispatched. The `container` package is the primary target of that refactor — `buildRunArgs()` in particular is the podman-specific argument builder that will change shape significantly when bwrap and sandbox-exec modes are extracted into separate registerable `Isolator` implementations.

**Recommended approach:** defer the `run_args.go` extraction until after Track A's shape is clear. The extraction of `lifecycle.go` and `credentials.go` is lower-risk and can proceed independently. `bwrap.go` and `sandbox_exec.go` already exist as separate files; their further decomposition is Track A's domain (D.1 covers `BuildArgs` function-size specifically).

---

## 6. `cmd/checkin.go` (1680 LoC)

### Concerns currently mixed

1. **Command entry and routing** — `init()` registers `checkinCmd`; `runCheckin()` is the router that dispatches to the per-mode handlers (review-group summary, raw-events path, session-turn path, no-arg list, legacy screen-scrape).

2. **Rich turn rendering** — `renderCheckinTurns()` (291 LoC) is the primary display path: it renders assistant-message turns with tool-call summaries, state-change events, and permission events in an interleaved chronological view. Heavily uses lipgloss for styling.

3. **Raw/verbose event rendering** — `renderCheckinEventsRaw()` (141 LoC), `renderChildEvent()`, `renderChildEventVerbose()`, `renderChildEventsDefault()` are the forensic-mode renderers. They are selected by `--types` or `--verbose` flags and are structurally separate from the turn-centric path.

4. **Review-round rendering** — `runCheckinReviewRounds()` (165 LoC) and `runCheckinReviewRoundsByGroup()` (117 LoC) render the multi-agent review summary. They are selected only when the session argument ends with `~review` or belongs to a session_group and represent a distinct display concern.

5. **No-arg session list** — `runCheckinNoArg()` (68 LoC), `printSessionTable()` (56 LoC) show all active sessions when invoked with no session argument.

6. **Tool-call result extraction helpers** — `toolKeyArg()`, `toolResultSummary()`, `bashResultSummary()`, `isErrorResult()`, `matchCountSummary()`, `firstMeaningfulLine()`, `extractBashCommand()`, `extractStringField()`, `extractMessageID()` are payload-parsing functions used exclusively by the rendering paths.

7. **Legacy screen-scrape fallback** — `runCheckinSessionLegacy()` (31 LoC) calls `tmux.CapturePaneText` when no DB rows exist. It is a thin fallback wrapper with no shared logic with the DB-backed paths.

8. **Proxy path** — `renderProxiedCheckin()` (139 LoC) handles the case where the sidecar returns checkin data over the host-API `/checkin` endpoint rather than reading the DB directly. Structurally distinct from the local-DB path.

### Proposed split

| Proposed file | Contents |
|---|---|
| `checkin.go` | Command wiring (`checkinCmd`, `init()`, `runCheckin()`), `runCheckinSession()`, `runCheckinSessionRaw()` — top-level dispatch |
| `checkin_turns.go` | `renderCheckinTurns()` — the primary interleaved-turn display path |
| `checkin_raw.go` | `renderCheckinEventsRaw()`, `renderChildEvent()`, `renderChildEventVerbose()`, `renderChildEventsDefault()`, `renderProxiedCheckin()` |
| `checkin_review.go` | `runCheckinReviewRounds()`, `runCheckinReviewRoundsByGroup()` |
| `checkin_list.go` | `runCheckinNoArg()`, `printSessionTable()` |
| `checkin_tools.go` | `toolKeyArg()`, `toolResultSummary()`, `bashResultSummary()`, `isErrorResult()`, `matchCountSummary()`, `firstMeaningfulLine()`, `extractBashCommand()`, `extractStringField()`, `extractMessageID()`, `turnLabel()`, `formatDuration()` |
| `checkin_legacy.go` | `runCheckinSessionLegacy()` |

### Public-surface impact

None. `cmd` is an executable-only package with no exported symbols consumed by other packages.

### Independence flag

**Safe to do standalone.** No coupling to Tracks A, B, or C. The review-round rendering in `checkin_review.go` uses `review.FormatResults` and DB `GroupResults`, but those APIs do not change under any current review-series proposal.

---

## 7. `cmd/switch.go` (1229 LoC)

### Concerns currently mixed

1. **TUI picker (bubbletea model)** — `pickerModel`, `Init()`, `Update()`, `View()`, `pick()`, `fuzzyMatch()`, `refilter()`, `pickString()` implement a fuzzy-match list picker using the charmbracelet/bubbletea library. This is a standalone UI component with no dependency on session or git logic.

2. **Text input model** — `inputModel`, its `Init()`, `Update()`, `View()`, `value()`, `promptInput()`, `promptBranchInput()` implement a text input field with branch-sanitise preview. Structurally independent of the picker.

3. **Session management** — `ensureAndSwitch()` (133 LoC) and `allocatePortForSession()` are the core session-management functions: liveness check, instance-ID generation, port allocation, session creation, and client attachment. They consume DB, tmux, and session package APIs.

4. **Project entry discovery** — `projectEntries()`, `expandHome()`, `switchProjectLocations()`, `switchProjectSpecific()`, `switchWorktreeExcludeSet()` scan the filesystem and config to build the top-level project list.

5. **Worktree-level navigation** — `handleBareRepo()` (87 LoC), `activeReviewSessionEntries()` (79 LoC), `handleReviewGroupPick()` (75 LoC), `handleRegularRepo()` (108 LoC) are the second-level pick handlers that navigate into worktrees, review rounds, and regular repos.

6. **Clone / init flow** — `handleCloneRepo()` (48 LoC) handles the `[+ clone repo]` special entry by prompting for a URL and running `git clone`.

7. **Session bootstrap and config injection** — `injectContainerConfig()`, `ensureSwitchDashSession()` (186 LoC of layout scripting) are the infrastructure helpers called during session creation. `ensureSwitchDashSession()` in particular embeds a substantial amount of hardcoded tmux layout script.

### Proposed split

| Proposed file | Contents |
|---|---|
| `switch.go` | Cobra command wiring (`switchCmd`, `init()`), runtime-config accessors (`switchWorktreeExcludeSet`, `switchProjectLocations`, `switchProjectSpecific`), `entry` type |
| `switch_picker.go` | `pickerModel`, `fuzzyMatch()`, `refilter()`, `pick()`, `pickString()` — the bubbletea list picker |
| `switch_input.go` | `inputModel`, `promptInput()`, `promptBranchInput()` — the text input component |
| `switch_projects.go` | `projectEntries()`, `expandHome()` — project discovery |
| `switch_worktree.go` | `handleBareRepo()`, `activeReviewSessionEntries()`, `handleReviewGroupPick()` — worktree navigation |
| `switch_session.go` | `ensureAndSwitch()`, `allocatePortForSession()`, `injectContainerConfig()`, `handleRegularRepo()`, `handleCloneRepo()` — session management |
| `switch_dash.go` | `ensureSwitchDashSession()` — dashboard session layout script |

### Public-surface impact

None. `cmd` is an executable-only package with no exported symbols consumed by other packages.

### Independence flag

**[uncertain] Partially depends on Track A.** `ensureAndSwitch()` and `handleRegularRepo()` both contain explicit `config.IsolationBwrap` / `config.IsolationPodman` checks for deciding which session path to take. Track A proposes an `Isolator` registry that would consolidate these dispatch points. If Track A lands first, `switch_session.go` will look different from the sketch above.

The TUI components (`switch_picker.go`, `switch_input.go`), project discovery (`switch_projects.go`), and dashboard session (`switch_dash.go`) are completely independent of Track A and safe to extract now.

---

## Open questions

1. **[uncertain] `db.go` merge-queue boundary.** The `pending_merges` table operations may be partially implemented in `internal/mergequeue/mergequeue.go` and partially in `db.go`. Before extracting `db/mergequeue.go`, audit which functions are in each package and whether consolidation first (all in `db`, or all in `mergequeue`) is cleaner. This is a cross-package concern that may warrant a separate issue.

2. **[uncertain] `sidecar.go` / Track B interaction.** B.3 explicitly reviews the sidecar lifecycle for stdio harnesses. `Run()` will change if the PI harness requires the sidecar to be the process parent. How much of the proposed `run.go` would survive B.3 intact is unclear. This uncertainty does not block extracting the non-lifecycle concerns (`events.go`, `state.go`, `notify.go`, etc.).

3. **[uncertain] `container.go` / Track A interaction.** `buildRunArgs()` (565 LoC) is the single largest function remaining in `container.go` after D.1's function-size work. A.1 and A.2 explicitly target the isolation-mode dispatch that `buildRunArgs()` embodies. Extracting it into `run_args.go` before Track A shapes the `Isolator` interface risks a large rebase. Flag for post-A.2 review.

4. **[uncertain] `cmd/switch.go` / Track A interaction.** The explicit `config.IsolationBwrap` / `config.IsolationPodman` checks in `ensureAndSwitch()` and `handleRegularRepo()` are cited in the architecture inventory as contributing to the 27-occurrence coupling count. Track A's registry proposal aims to remove these scattered checks. Extracting `switch_session.go` before Track A's dispatch shape is clear may require an immediate follow-up rework.

5. **Export-test shims in `review.go`.** The seven `*ForTest` exported functions are a code smell — they exist because the functions they wrap are unexported and the tests are in a `review_test` (black-box) package. Relocating them to `export_test.go` (a convention for this pattern in the Go standard library) is a micro-cleanup that should accompany whichever PR splits `review.go`, not a blocking prerequisite.
