# A.1 — Isolation coupling review and registry-shape proposal

Status: proposal (no code changes).
Issue: #1073.
Track: A (isolation), Wave 1 anchor read.
Source corpus: `modules/programs/prism/prism/docs/architecture-inventory.md`,
specifically **§6 (Isolation-mode coupling map)** — every cited site below maps
to a row in §6.1–§6.21 (or §6.22 for the display-only appendix). All file:line
citations are taken from the inventory; the inventory was the authoritative
read, not an independent re-grep.

## 1. Context

The architecture inventory landed in #1071 surfaced one dominant pressure point:
`cmd → internal/config:IsolationBwrap` is the single most-frequent cross-package
coupling (27 occurrences), and the four `Isolation*` constants between them
dominate the top-20 cross-package hot-spots. Mode dispatch is currently spread
across 17+ files; per-mode `if isoMode == X` branches appear in
`cmd/spawn.go`, `cmd/switch.go`, `cmd/restore.go`, `cmd/pr.go`, `cmd/cleanup.go`,
`cmd/review.go`, `cmd/agent_run.go`, `cmd/sidecar.go`, `cmd/concurrency.go`,
`internal/session/{session,spawn,sidecar}.go`, `internal/sidecar/sidecar.go`,
`internal/container/{container,bwrap,sandbox_exec,concurrency}.go`,
`internal/archive/archive.go`, `internal/review/review.go`, `internal/db/db.go`.

The stated long-term direction (per design doc #1072) is bwrap (Linux) and
sandbox-exec (Darwin) as primary modes, with podman deprecated when sandbox-exec
reaches parity. **Removal must be cheap; today it is not.** New modes
(firejail, kvm, windows-sandbox) should require implementing one interface and
registering, not surgery across the codebase.

The current `Isolator` interface (`internal/container/isolator.go:30-56`) is
deliberately narrow — `BuildRunArgs`, `Run`, `Shutdown`, `HasExited`,
`DumpLogs`. It was sized to wrap the podman path that existed before bwrap and
sandbox-exec arrived. Bwrap and sandbox-exec implementations exist
(`bwrap.go`, `sandbox_exec.go`) but they are reached through a parallel set of
methods on `Manager` (`PrepareBwrap`, `PrepareSandboxExec`) and through
free-standing functions, not through `Isolator`. The interface name "Isolator"
is therefore slightly misleading today — it is currently more "PodmanRunner" in
practice. This review's central proposal is to widen that interface so that
**every per-mode branch in the corpus collapses into either an `Isolator`
method call or a registry lookup** — and to identify the small set of sites
that legitimately do not fit either.

## 2. Method

I read inventory §6.1 through §6.22 cover-to-cover, treating each enumerated
file:line entry as one classification target. Each site is tagged with one of:

- **(a) survives unchanged** — the site does not branch on mode and does not
  need to know about modes. Typically display-only or agnostic plumbing.
- **(b) moves to `Isolator`** — the site's branch on mode would collapse to a
  single method call against an `Isolator` instance returned by an
  `IsolationRegistry`.
- **(c) needs new abstraction beyond `Isolator`** — the site addresses a
  cross-cutting concern that `Isolator` is the wrong shape for (registry
  lookup, capability declaration, persisted-mode resolution, etc.).
- **(d) deletable** — incidental coupling that exists only because of the
  deprecated `ContainerMode bool` / `--host-mode` flag / `EffectiveIsolationMode`
  fallback path. Out of scope for A.1's interface design (covered by **A.4**),
  but tagged here so the count of "real" sites is honest.

For (b), I cite at least one site per proposed interface method per AC. For (c)
I propose where the new abstraction lives. Sites I cannot confidently classify
without further investigation are flagged `[uncertain]` with a one-line reason.

## 3. Per-site classification

Sites are grouped by inventory subsection. Each row preserves the inventory's
file:line citation. The "tag" column gives the (a)/(b)/(c)/(d) classification;
the "→" column names the proposed destination (an `Isolator` method, a registry
helper, or a deletion target).

### 3.1 §6.1 `internal/config/config.go` (mode definitions)

| Site | Tag | → |
|---|---|---|
| `config.go:18-43` — `IsolationMode` type + four constants + `ValidIsolationModes` | (a) | Stays. Becomes the **registry key set**: `IsolationRegistry.Names()` returns these. The constants themselves remain in `config` because they are persisted. |
| `config.go:46-84` — `Config` struct including deprecated `ContainerMode bool` | (d) | `ContainerMode` → A.4 deletion. `DefaultIsolationMode` stays. |
| `config.go:112-114` — `BwrapConcurrencyCap` field | (b) | The *value* still lives in `Config` (it is user-tunable). The *use* of it moves into `bwrapIsolator.Cap()`. See §4.2 (`Cap`). |
| `config.go:154` — `"500ms is enough to flatten the podman burst"` comment | (a) | Stays as documentation of an existing podman-only knob. |
| `config.go:257-266` — `load()` derives mode from `ContainerMode` for back-compat | (d) | A.4 deletion target. |
| `config.go:313` — `"500ms is enough"` comment | (a) | Surface only. |
| `config.go:352-360` — `(Config).EffectiveIsolationMode()` fallback method | (d) | A.4 deletion target. Until A.4, the `IsolationRegistry.Resolve(cfg)` helper (§4.2) wraps it so callers stop calling it directly. |

### 3.2 §6.2 `internal/config/profiles.go`

| Site | Tag | → |
|---|---|---|
| `profiles.go:88` — comment "to podman run as --memory ..." | (a) | Surface only. |
| `profiles.go:97-107` — `ContainerResources` struct (podman-shaped fields) | (c) | The struct itself stays where it is, but the *interpretation* of the fields belongs to the isolator that consumes them. Add `Isolator.ApplyResources(ContainerResources)` so each isolator decides what it does with `MemoryMax`/`PidsLimit` (podman maps to flags; bwrap and sandbox-exec ignore or warn). See §4.2 (`ApplyResources`). [uncertain — could equally well live as a separate `ResourceApplier` interface; the answer depends on whether non-podman modes ever gain real resource limits]. |

### 3.3 §6.3 `internal/container/` package

| Site | Tag | → |
|---|---|---|
| `isolator.go:1-98` — `Isolator` interface + `podmanIsolator` impl | (b) | The interface widens (§4); `podmanIsolator` becomes one of three registered implementations. |
| `isolator.go:97,113,118,130,152` — `exec.CommandContext(ctx, "podman", ...)` calls | (a) | Survive unchanged inside `podmanIsolator`. |
| `bwrap.go:1-733` — entire `bwrapIsolator` impl | (b) | Stays in place; the file is the bwrap implementation of the widened interface. The 488-LoC `BuildArgs` is an A.2/D.1 problem, not an A.1 problem. |
| `bwrap.go:79-100, 142-150, 178-258, ...` — design comments | (a) | Surface only. |
| `sandbox_exec.go:1-240` — entire `sandboxExecIsolator` impl | (b) | Stays in place; registered as the third implementation. Stub `Run`/`Shutdown` continue to satisfy the interface (the real exec path is in `cmd/agent_run.go` — see §6.8 below). |
| `sandbox_exec.go:86-103, 154-172, ...` — bwrap-symmetry comments | (a) | Surface only. |
| `sandbox_exec.go:246` — `MinimalIsolatedExecEnv` exported helper | (a) | Stays as a shared helper. A.2 may consolidate further; A.1 leaves it alone. |
| `concurrency.go:5-12, 131-160` — `runPodmanPS` + cap-check | (b) | Moves into `podmanIsolator.Cap()` (§4.2). The DB-side count stays in `internal/db`. |
| `concurrency.go:136` — `exec.CommandContext(ctx, "podman", "ps", ...)` | (b) | Internal to `podmanIsolator`. |
| `container.go:1` — package doc names podman | (a) | Update text only. |
| `container.go:46, 63-65, 80-81` — dual-identity comments | (a) | Surface. |
| `container.go:212-241` — `Config` struct fields documented as podman-specific | (c) | Same as §6.2 — field semantics belong to the isolator; see `ApplyResources`. |
| `container.go:242-246` — `NameForSession(sessionName)` | (a) | Stays as a shared utility. Used by both podman (container name) and bwrap (process name). |
| `container.go:332-358` — `EnsureRemoved` (podman inspect/stop/rm) | (b) | Move into `podmanIsolator.EnsureRemoved()` or fold into `Shutdown`. Cleanup callers (§6.12) then call `IsolationRegistry.For(mode).EnsureRemoved(name)` instead of branching on the literal "podman". |
| `container.go:518-526, 589, 710, 723, 786, 813, 927-987, 1093-1147, 1216-1399, 1511-1734` — podman-specific code/comments inside Manager | (b) | These are the bulk of the 555-LoC `buildRunArgs` and surrounding podman lifecycle. Most belong inside `podmanIsolator`; the residual `Manager` surface becomes a thin coordinator that delegates to the registered isolator. D.1 will decompose `buildRunArgs`; A.1 only proposes that the *destination* of that decomposition is `podmanIsolator`. |
| `container.go:526-625` — `(*Manager).writeGitconfig(mode isolationMode)` branches on unexported `isolationMode` | (b) | Either: (i) `Isolator.WriteGitconfig(workspace string)` so each isolator picks its own canonical path; or (ii) leave `Manager.writeGitconfig` as a path-only helper and have the isolator hand it the path it wants. Prefer (i) — see §4.2 (`WriteGitconfig`). |
| `container.go:642-760` — `(*Manager).Create` (podman-only) | (b) | Becomes the body of `podmanIsolator.Create()` or the post-`Run` setup; `Manager.Create` reduces to `registry.For(mode).Create(...)`. |
| `container.go:813-832` — `(*Manager).Shutdown` already delegates to `Isolator.Shutdown` | (a) | Already the right shape. |
| `container.go:833-882` — `(*Manager).PrepareBwrap()` | (b) | Becomes `bwrapIsolator.Prepare()` (or fold into `Run`). The `Manager` parallel-path goes away. |
| `container.go:884-905` — `(*Manager).PrepareSandboxExec()` | (b) | Becomes `sandboxExecIsolator.Prepare()`. |
| `container.go:1700-1734` — `CheckAvailability()` (podman-only) | (b) | Move to `podmanIsolator.Available()`. Callers (`cmd/spawn.go:280`) become `registry.For(mode).Available()`. See §4.2 (`Available`). |

### 3.4 §6.4 `internal/session/` package

| Site | Tag | → |
|---|---|---|
| `session.go:73-76, 80-83` — `Opts.ContainerMode` + `Opts.IsolationMode` | (d) for `ContainerMode`; (a) for `IsolationMode` | A.4 deletes `ContainerMode`. |
| `session.go:240-244` — `effectiveIsolationMode(opts)` fallback | (d) | A.4 target; in the meantime wraps via `IsolationRegistry.Resolve(opts)`. |
| `session.go:252-296` (`BuildOpencodeCmd` switch) — emits `podman attach …`, `prism agent-run …`, or direct opencode | (b) | Each branch becomes `Isolator.AgentPaneCmd(opts)` returning the shell command string. The switch collapses to `registry.For(mode).AgentPaneCmd(opts)`. See §4.2 (`AgentPaneCmd`). The `host` fallback corresponds to a `hostIsolator` that returns the direct-opencode command — see §4.4 on whether "host" is an isolator at all. |
| `session.go:323, 342-343, 482, 497-547` — per-mode pane-layout comments | (a) | Surface only — the layout decisions ride on top of the `AgentPaneCmd` result. |
| `session.go:514-523` — `setupFullLayout` builds `StartSidecarOpts{ContainerMode: mode == "podman", IsolationMode: mode}` | (b) | After the registry move, the call shape becomes `registry.For(mode).SidecarFlags()` returning the slice of sidecar argv extensions; the caller no longer assembles per-mode flag combinations by hand. See §4.2 (`SidecarFlags`). |
| `session.go:539-547` — `mode == "podman"` for readiness-wait command | (b) | The readiness-wait choice belongs to the isolator (or to its declared transport shape). Add `Isolator.NeedsReadinessWait() bool` *or* fold into a `Capabilities()` struct (see §4.3). [uncertain — readiness-wait is partly a *harness*-shape concern (Track B.1), not a pure isolator concern. A.1 proposes the isolator owns the *yes/no* flag for now and Track B reconciles.] |
| `session.go:559-589` — persists `isolation_mode` and `host_mode` to the DB | (a) | The persistence stays. The *value* came from registry resolution upstream. `host_mode` itself is the A.4 deprecation target. |
| `sidecar.go:49-128` — per-session run/state/log dir comments | (a) | Surface. |
| `sidecar.go:215-221` — `StartSidecarOpts.ContainerMode` (deprecated) + `IsolationMode` | (d) for `ContainerMode`; (a) for `IsolationMode` | A.4. |
| `sidecar.go:311-340` — `StartSidecarWithOpts` argv builder branches on mode | (b) | Replaced by `Isolator.SidecarFlags(opts)`. The current shape (`--isolation-mode X`, conditional `--container`, conditional `--port`) maps cleanly onto a per-isolator method that returns `[]string`. The non-conditional `--isolation-mode` flag survives because the sidecar still needs to know which registry entry to look up after re-exec. |
| `spawn.go:60-83` — `SpawnOpts.ContainerMode/IsolationMode` | (d)/(a) | Same as session.go. |
| `spawn.go:107, 259, 349, 368-460` — `spawnAgentOnlyLayout` per-mode branches | (b) | Same shape: `host_mode` resolution goes through `IsolationRegistry.Resolve`; agent command via `Isolator.AgentPaneCmd`; per-mode DB column writes are downstream of the resolved mode. |
| `spawn.go:369-374` — `mode := opts.IsolationMode; if "" && ContainerMode { mode = "podman" }; if "" { resolveFromConfig() }` | (d)/(b) | Becomes `mode := registry.Resolve(opts, cfg)` — single helper, single source of truth. |
| `spawn.go:440-460` — write `host_mode` so `prism agent-run` reads back correct mode | (a) | Stays — agent-run needs to read this on its side too (§6.8). The write itself is mode-agnostic plumbing. |
| `readiness.go:7` — comment naming bwrap/podman/host | (a) | Surface. |
| `startup_log.go:8, 43` — comments naming bwrap | (a) | Surface. |

### 3.5 §6.5 `internal/sidecar/sidecar.go`

| Site | Tag | → |
|---|---|---|
| `sidecar.go:27, 72-76, 153-183, 242, 337, 452-467, 512-552, 609-755, 2944, 3548, 3598` — block comments + per-mode branches in `Run`, host-API socket bind, startup-timeout goroutine, `[timing]` markers | (b) + (c) | Most of these are **dispatch on `s.cfg.Container == nil`** (i.e. "am I in podman mode?"). After registry: replace `s.cfg.Container == nil` with a `Capabilities()` query (e.g. `iso.Capabilities().OwnsContainerLifecycle`). The host-API socket bind is mode-agnostic when the sidecar is asked "do I need a host-API socket here?" — that is `iso.Capabilities().NeedsHostAPISocket`. The startup-connect timeout firing only in bwrap mode is `iso.Capabilities().NeedsStartupConnectTimeout`. **(c)** because these are not single-method-call sites — they are scattered within long function bodies, and the *cleanest* shape is a `Capabilities` struct rather than a long parade of yes/no methods on the interface. See §4.3. |
| `sidecar.go:3101, 3122, 3148, 3190, 3219` — `/spawn` host-API request handler (`host_mode` + `isolation` JSON fields, mutual exclusion check) | (b) | The mutual-exclusion check and the `req.HostMode → IsolationHost` mapping become a single `IsolationRegistry.ResolveSpawnRequest(req)` helper. The handler itself does not branch further on the resolved mode — it just hands the value to the spawn code path, which already goes through `BuildOpencodeCmd`/`StartSidecarWithOpts` (§6.4). |

### 3.6 §6.6 `cmd/sidecar.go`

| Site | Tag | → |
|---|---|---|
| `sidecar.go:11-15, 78-95` — flag declarations + Long-help text | (a) | Help text references the registered names. The flags themselves stay. |
| `sidecar.go:121-138` — flag-parsing block produces `podmanMode`, `bwrapMode`, `sandboxExecMode`, `needsHostAPI`, `useContainerHarness` booleans | (b) + (c) | Replaced by `iso := registry.For(isolationMode); needsHostAPI := iso.Capabilities().NeedsHostAPISocket; useContainerHarness := iso.Capabilities().UsesContainerHarness`. The local booleans collapse into capability queries. |
| `sidecar.go:194-348` — branches across the file: container Config built only for podman; host-API path computed for podman/bwrap/sandbox-exec; `OnReady` callback set only for podman; harness adapter constructed in container or host mode; restart-loop goroutine started for podman/bwrap | (b) + (c) | Each "set if mode is X" becomes a capability check. Where the per-mode path is genuinely structural (e.g. constructing a `container.Config` only for podman), the construction itself moves *into* the isolator's setup phase. The restart-loop firing for podman/bwrap is `iso.Capabilities().RestartOnExit`. |

### 3.7 §6.7 `cmd/spawn.go`

| Site | Tag | → |
|---|---|---|
| `spawn.go:17, 83, 112` — flag definitions and config plumbing | (a) | Stay. |
| `spawn.go:146-148` — `--isolation` and deprecated `--host-mode` flags | (a) for `--isolation`, (d) for `--host-mode` | A.4 removes `--host-mode`. |
| `spawn.go:161-198` — `resolveIsolationMode(...)` (mutual-exclusion check, validation, config fallback) | (b) | Becomes `registry.ResolveFromFlags(isolationFlag, hostModeFlag, cfg)`. The validation against `ValidIsolationModes` becomes "is this a registered name?". |
| `spawn.go:212-225` — `checkBwrapPlatform`, `checkSandboxExecPlatform` | (b) | Becomes `iso.Available()` (§4.2). The platform check is one form of availability — bwrap is "available" only on Linux, sandbox-exec only on Darwin. |
| `spawn.go:275` — `effectiveContainerMode := isolationMode == config.IsolationPodman` | (d) | A.4. Until then, the value is computed once via `iso.Capabilities().IsContainer` so callers stop comparing strings. |
| `spawn.go:279-294` — per-mode availability + concurrency-cap probes | (b) | `iso.Available()` + `iso.Cap(ctx, db).Check(ignoreFlag)`. The single registry-driven code path replaces the parallel `CheckAvailability` (podman) + `checkBwrapConcurrencyCap` (bwrap) sites. |
| `spawn.go:311-357` — config-blob load required when sandboxed | (b) | `iso.Capabilities().NeedsConfigBlob`. Same predicate appears at `cmd/restore.go`, `cmd/pr.go`, `cmd/review.go`, `internal/review/review.go:1230` — all collapse to one capability query. |
| `spawn.go:383-435` — bwrap-only opencode.json on-disk write | (b) | Becomes `iso.WriteHarnessConfigBlob(workspace, blob)`. Only `bwrapIsolator` needs to write to disk; podman injects via env-var, sandbox-exec via env-var. The branch goes away. See §4.2 (`WriteHarnessConfigBlob`). |

### 3.8 §6.8 `cmd/agent_run.go`

| Site | Tag | → |
|---|---|---|
| `agent_run.go:3-13, 34-69, 91-142` — entry point, branches on `IsolationBwrap`/`IsolationSandboxExec` | (b) + (c) | The dispatch `switch` becomes `iso.AgentRun(ctx, opts)`. **(c)** because `agent-run` is invoked as a *separate process* from the spawn flow — the registry has to be re-instantiated in this process from the persisted mode. That is fine (the registry is stateless, it's a name → constructor map), but the cmd's job becomes "look up the persisted mode, dispatch to `iso.AgentRun`". `host` and `podman` are not valid modes here; the registry returns an error for them, replacing the manual `else` arm. |
| `agent_run.go:160-350` — bwrap dispatch path: socket lookup, env, find binary, PTY, exec, signal forwarding, stderr tee | (b) | Body of `bwrapIsolator.AgentRun()`. |
| `agent_run.go:434-497` — `minimalBwrapExecEnv` (env-var allow-list) | (a) | Helper inside the bwrap implementation. |
| `agent_run.go:474-489` — `validateSandboxExecArgs` shape check | (a) | Helper inside the sandbox-exec implementation. |
| `agent_run.go:492-619` — `runAgentRunSandboxExec` (sandbox-exec via `syscall.Exec`) | (b) | Body of `sandboxExecIsolator.AgentRun()`. |
| `agent_run.go:653-685` — `findBwrap` (locate binary) | (a) | Helper inside the bwrap implementation. |

### 3.9 §6.9 `cmd/switch.go`

| Site | Tag | → |
|---|---|---|
| `switch.go:432, 628-991, 1027-1212` — `handleBareRepo`/`handleRegularRepo`/`handleCloneRepo` each branch on `isoMode == IsolationBwrap` to write opencode config to disk | (b) | Same as `cmd/spawn.go:383-435` — `iso.WriteHarnessConfigBlob(workspace, blob)`. The three sites use the same predicate; one method serves them all. |
| `switch.go:1069-1108` — `effectiveContainerMode` + `sandboxed` boolean + host-only env-var pass-through | (b) + (d) | `effectiveContainerMode` is (d). `sandboxed` becomes `iso.Capabilities().NeedsConfigBlob` (the predicate is identical). The host-only env-var pass-through belongs to a `hostIsolator` (§4.4). |

### 3.10 §6.10 `cmd/restore.go`

| Site | Tag | → |
|---|---|---|
| `restore.go:8-10, 43, 89-92, 125-127, 269-292, 306, 314-403` — restore path mirrors spawn for config-blob injection and bwrap on-disk write | (b) | Same set of method calls as `cmd/spawn.go`: `registry.For(mode).AgentPaneCmd`, `iso.Capabilities().NeedsConfigBlob`, `iso.WriteHarnessConfigBlob`. The `host_mode` back-compat read goes through `registry.Resolve(dbRow)` — see §4.2 (`Resolve`). |

### 3.11 §6.11 `cmd/review.go`

| Site | Tag | → |
|---|---|---|
| `review.go:149, 158-179, 257-285, 323-351` — derives `isoMode` from parent worker session DB row, applies bwrap concurrency cap, requires profiles when sandboxed, comments on virtiofs/podman corner cases | (b) | All the same methods as spawn: `registry.Resolve(parentRow)`, `iso.Cap(ctx, db).Check(ignoreFlag)`, `iso.Capabilities().NeedsConfigBlob`. The virtiofs comment is (a). |

### 3.12 §6.12 `cmd/cleanup.go`

| Site | Tag | → |
|---|---|---|
| `cleanup.go:763, 792-868, 882-979` — branches on persisted `isolation_mode`; `isHostMode(sessionName)` reads `host_mode` from DB; `stopAndRemoveChildContainers` and `removeContainerIfExists` invoke `podman stop`/`podman rm` for podman sessions | (b) + (d) | The persisted-mode read goes through `registry.Resolve(dbRow)`. The podman stop/rm sites become `iso.EnsureRemoved(name)`, which is a no-op for `bwrapIsolator`/`sandboxExecIsolator`/`hostIsolator` (or returns an error if the implementation has nothing to clean — *see open questions §6*). `isHostMode` is the A.4 deprecation surface. |

### 3.13 §6.13 `cmd/pr.go`

| Site | Tag | → |
|---|---|---|
| `pr.go:77-170` — same per-mode pattern as spawn | (b) | Same set of method calls as `cmd/spawn.go`. The fact that this site is structurally identical to spawn/restore/pr is the strongest argument for the registry shape — each cmd file currently re-implements the same dispatch table. |

### 3.14 §6.14 `cmd/merge.go`, `cmd/merges.go`

| Site | Tag | → |
|---|---|---|
| `merge.go:62, merges.go:90, merges.go:137` — when running inside a bwrap sandbox, proxy merge enqueue/list/cancel to the host sidecar via host-API socket (#1043) | (c) | This is **not** an `Isolator` concern — it is an *inside-the-sandbox* concern: "am I currently executing inside a sandboxed environment, and if so, do I have a host-API socket to proxy through?". The registry on the *outside* knows nothing about the executing process's own boxedness. Proposal: a small `internal/sandboxenv` package exposing `IsInsideSandbox() bool` and `HostAPISocket() (string, bool)` — populated from env vars set by the bwrap launcher. The merge commands consult it directly. [uncertain — this could equally well be a method on `Isolator` if we keep the inside-process-also-uses-registry idea; my preference is to keep `Isolator` outside-the-sandbox-only because the inside-the-sandbox view is genuinely different]. |

### 3.15 §6.15 `cmd/concurrency.go`

| Site | Tag | → |
|---|---|---|
| `concurrency.go:5-122` — `checkConcurrencyCap` (podman-only) and `checkBwrapConcurrencyCap` (bwrap-only) | (b) | Both collapse into `iso.Cap(ctx, db).Check(ignoreFlag)`. The `Cap` return value (§4.2) carries enough context to render the warning/error message symmetrically. **This is the core deliverable of A.3** — A.1's job is to declare the shape; A.3 implements and unifies the message rendering. |

### 3.16 §6.16 `cmd/logs.go`

| Site | Tag | → |
|---|---|---|
| `logs.go:11, 51, 73, 169` — `--agent-run` flag selects bwrap harness log; help text and error guidance reference bwrap | (b) | `iso.LogPaths()` returns the per-mode log file set; the `--agent-run` flag is a convenience alias for `bwrapIsolator.LogPaths().Harness`. The help text generates from registered isolators. See §4.2 (`LogPaths`). [uncertain — LogPaths might overlap with the archive-side `StorageRoot`; A.6 / Track B.6 may want to merge them under one "session paths" struct]. |

### 3.17 §6.17 `cmd/reset.go`

| Site | Tag | → |
|---|---|---|
| `reset.go:8-10, 43, 89-92, 126-182` — `prism reset` runs `podman ps` / `podman rm -f` over `prism-` prefixed containers | (b) | Becomes `podmanIsolator.Reset()` (or fold into the broader `EnsureRemoved`). For bwrap and sandbox-exec the equivalent is "kill any orphan agent-run processes" — that is a real implementation but distinct enough from podman that it earns its own method on the relevant isolator. `prism reset` then iterates `for _, name := range registry.Names() { registry.For(name).Reset() }`. [uncertain — `Reset()` semantics for bwrap might be "do nothing" until orphaned-agent-run cleanup is implemented; A.1 names the shape, the implementation lands later]. |

### 3.18 §6.18 `internal/db/db.go`

| Site | Tag | → |
|---|---|---|
| `db.go:72-97` — `Status.HostMode` field, `Status.IsolationMode` field, `EffectiveIsolationMode()` method | (d) for `HostMode` and the fallback method; (a) for `IsolationMode` | A.4 removes `HostMode` and the fallback method. `IsolationMode` stays as the canonical persisted column. |
| `db.go:164-172, 395-554, 1414-1442, 1696-2317, 2542-2683` — `agent_status` schema includes `host_mode` and `isolation_mode` columns; migrations; `SetHostMode`/`SetIsolationMode` writers; `scanStatus` reader with NULL→0/false back-compat | (a) for `isolation_mode`; (d) for `host_mode` | Schema column survives. The writers stay generic. The reader's `host_mode` handling is the A.4 deprecation surface. |
| `db.go:1720-1740` — `ActiveBwrapSessionCount` and `ActiveBwrapSessions` filter by `isolation_mode = 'bwrap'` | (b) | Becomes `db.ActiveSessionCountForMode(mode IsolationMode)` and `db.ActiveSessionsForMode(mode)`. Called from `iso.Cap(ctx, db).Check(...)` — the per-mode literal disappears from the SQL builder. |
| `db.go:73` comment | (a) | Update text only. |

### 3.19 §6.19 `internal/archive/archive.go`

| Site | Tag | → |
|---|---|---|
| `archive.go:90-95, 189-191, 236, 248-276` — `Params.IsolationMode`; archive copies `agent-run.log` (bwrap-only); `resolveStorageRoot` switches on `case "host", "bwrap":` vs `case "podman":` | (b) | `iso.ArchiveStorageRoot(home, sessionName) string` returns the per-mode storage root; the `switch` collapses to one method call. The bwrap-only `agent-run.log` copy becomes `iso.ExtraArchiveFiles() []string` (or similar). See §4.2 (`ArchiveStorageRoot`, `ExtraArchiveFiles`). |

(Note: the `internal/archive/archive.go:279-285` `containerNameForSession` helper that re-implements `internal/container.NameForSession` to dodge a circular import is a candidate for moving the name-derivation into the isolator itself: `podmanIsolator.ResourceName(sessionName) string`. That removes the circular-import workaround. [uncertain — depends on whether `archive` should depend on `container` at all; the current isolation between the two is deliberate].)

### 3.20 §6.20 `internal/review/review.go`

| Site | Tag | → |
|---|---|---|
| `review.go:27, 59, 623-628, 740-789, 987-1021, 1203, 1214-1230` — `Opts.IsolationMode` plumbing; per-agent ResolveAgentConfigContent + bwrap-only on-disk write inside `Run`/`RunAsync` | (b) | The bwrap-only on-disk write is the same `iso.WriteHarnessConfigBlob` site as spawn/switch/restore/pr. |
| `review.go:1230` — `needsConfig := isolationMode == IsolationPodman \|\| IsolationBwrap \|\| IsolationSandboxExec` | (b) | `iso.Capabilities().NeedsConfigBlob`. Identical to `cmd/spawn.go:311-357`. |

### 3.21 §6.21 `internal/harness/opencode/adapter.go`

| Site | Tag | → |
|---|---|---|
| `opencode/adapter.go:93` — comment naming `podman attach` | (a) | Surface only. |
| `harness/harness.go:83` — comment naming `podman --env` | (a) | Surface only. |

### 3.22 §6.22 Flat-appendix display sites

| Site | Tag | → |
|---|---|---|
| `internal/dashboard/sessions.go` — display-only formatting reading `Status.IsolationMode` | (a) | Stays. |
| `cmd/list_sessions.go`, `cmd/sessions.go`, `cmd/stats.go` — read-only display formatting | (a) | Stays. |
| `cmd/event.go` — tmux event hooks read isolation mode to decide whether to seed certain status columns | (b) | The decision-on-mode part is a capability query (`iso.Capabilities().EmitsTmuxStatusColumns` or similar). The display formatting itself is (a). |

### 3.23 Classification roll-up

By tag (counting inventory sites, grouping repeated branches in long files as one):

- **(a) survives unchanged:** ~20 sites — primarily comments, surface text, the existing constants/enum, `IsolationMode` schema column, display formatting, `NameForSession`, `MinimalIsolatedExecEnv`.
- **(b) moves to `Isolator` interface or a registry helper:** ~35 sites — every `if isoMode == X` and every `switch isoMode` outside the implementation files themselves.
- **(c) needs a new abstraction beyond `Isolator`:**
  - The `Capabilities` struct (§4.3) — sidecar.go, cmd/sidecar.go, cmd/event.go feature flags.
  - `internal/sandboxenv` (§3.14) — inside-the-sandbox identity for `cmd/merge.go`/`merges.go`.
  - `IsolationRegistry.Resolve(...)` helpers (§4.2) — replace the back-compat fallback paths cleanly while A.4 is in flight.
  - `ApplyResources` (§3.2) — possibly a separate `ResourceApplier` interface; uncertain.
- **(d) deletable / A.4 deprecation surface:** `Config.ContainerMode`, `EffectiveIsolationMode`, `Opts.ContainerMode`, `StartSidecarOpts.ContainerMode`, `SpawnOpts.ContainerMode`, `--host-mode` flag, `Status.HostMode`, the load()-time and effectiveIsolationMode-time fallback paths. Counted but not designed-around in A.1.

## 4. Proposed `Isolator` interface superset

This section is the AC-required concrete Go signature. It is presented as one
file (`internal/container/isolator.go` plus a new `internal/container/registry.go`)
to make the surface visible at a glance. Method ordering reflects the
roughly-lifecycle order from spawn-time to teardown-time.

### 4.1 The interface

```go
// Isolator is the per-mode seam. One implementation per isolation mode
// (podmanIsolator, bwrapIsolator, sandboxExecIsolator, hostIsolator).
//
// Implementations are registered with the package-level IsolationRegistry at
// init time. All cross-package mode dispatch goes through the registry; no
// caller outside this package compares an isolation-mode string literal.
type Isolator interface {
    // Identity ----------------------------------------------------------------

    // Name returns the canonical mode name as persisted in the database and
    // accepted by --isolation. Stable; the value is the registry key.
    // Cites: internal/config/config.go:18-43 (the IsolationMode constants).
    Name() config.IsolationMode

    // Capabilities returns the per-mode feature flags consulted by callers
    // that today branch on the literal mode value. See the Capabilities
    // struct (§4.3) for the full set.
    // Cites: internal/sidecar/sidecar.go:153-183, 512-547, 638-680;
    //        cmd/sidecar.go:121-138, 194-348;
    //        cmd/spawn.go:275, 311-357; cmd/switch.go:1069-1108;
    //        cmd/event.go (display-flag site from §6.22);
    //        internal/review/review.go:1230.
    Capabilities() Capabilities

    // Pre-spawn checks --------------------------------------------------------

    // Available reports whether this isolator can run on the current host.
    // Returns a nil error for "yes". Returns a wrapped, user-facing error
    // describing the missing prerequisite when not available — the caller
    // surfaces it verbatim.
    //
    // For podman: checks the binary, socket, and image presence (replaces
    // CheckAvailability at internal/container/container.go:1700-1734).
    // For bwrap: requires Linux + bwrap binary (replaces checkBwrapPlatform
    // at cmd/spawn.go:212-225).
    // For sandbox-exec: requires Darwin + the sandbox-exec binary (replaces
    // checkSandboxExecPlatform at cmd/spawn.go:212-225).
    // For host: always nil.
    Available(ctx context.Context) error

    // Cap returns the soft concurrency-cap descriptor for this isolator.
    // The descriptor carries (current, limit, exceeded, formatted message)
    // so cmd/concurrency.go renders one warning/error shape regardless of mode.
    //
    // For podman: wraps the existing container.CheckCap helper
    // (cmd/concurrency.go:34-56, internal/container/concurrency.go:131-160).
    // For bwrap: wraps the BwrapConcurrencyCap path
    // (cmd/concurrency.go:71-122, internal/db.go:1720-1740).
    // For sandbox-exec and host: Limit==0 (uncapped) — matches today's behaviour.
    //
    // The actual unification is the deliverable of A.3; A.1 declares the shape.
    Cap(ctx context.Context, dbPath string) CapStatus

    // Spawn-time setup --------------------------------------------------------

    // WriteHarnessConfigBlob writes the harness configuration blob (e.g. the
    // role-specific opencode.json) to disk if and only if this isolator
    // requires the blob to be reachable from the sandboxed filesystem.
    //
    // Cites: cmd/spawn.go:383-435 (bwrap on-disk write inside spawnRun);
    //        cmd/switch.go:432, :628-991, :1027-1212 (three switch handlers);
    //        cmd/restore.go:269-292 (restore path);
    //        cmd/pr.go:77-170 (pr path);
    //        internal/review/review.go:740-789, 987-1021 (review per-agent path).
    //
    // For podman and sandbox-exec: no-op (they inject the blob via env-var).
    // For bwrap: writes <workspace>/opencode.json (today's behaviour).
    // For host: no-op (host-mode opencode reads ~/.config/opencode directly).
    WriteHarnessConfigBlob(workspace string, blob []byte) error

    // WriteGitconfig writes a per-session .gitconfig under the workspace if
    // the isolation method needs one. Cites: internal/container/container.go:526-625
    // (Manager.writeGitconfig switches on the unexported isolationMode enum).
    //
    // For podman: writes the canonical container path (root user mapping).
    // For bwrap: writes the host user path.
    // For sandbox-exec: writes the host user path (sandbox-exec sees host UID).
    // For host: no-op.
    WriteGitconfig(workspace, signersPath, sigKeyPath string) error

    // ApplyResources is the per-mode interpretation of the ContainerResources
    // struct (config.ContainerResources). Podman maps the values to
    // --memory/--memory-swap/--pids-limit; bwrap and sandbox-exec ignore or
    // log a warning. Returns the modified RunPlan so the caller can append
    // the produced flags/env to the launch command.
    //
    // Cites: internal/config/profiles.go:97-107 (ContainerResources);
    //        internal/container/container.go:212-241 (Manager Config fields).
    //
    // [uncertain — could equally well be a separate ResourceApplier interface
    // if non-podman modes never gain real resource limits. Kept on Isolator
    // for now to avoid premature splitting.]
    ApplyResources(plan *RunPlan, res config.ContainerResources)

    // Argv & launch ----------------------------------------------------------

    // SidecarFlags returns the per-mode argv extensions appended to the
    // `prism sidecar` command line for sessions that use this mode.
    //
    // Cites: internal/session/sidecar.go:311-340 (StartSidecarWithOpts argv
    // builder); internal/session/session.go:514-523 (setupFullLayout call site).
    //
    // Today: --container (podman only), --port (podman, bwrap, sandbox-exec
    // when the harness uses HTTP+SSE), --isolation-mode (always — survives
    // because the spawned sidecar still needs to look up its own isolator).
    SidecarFlags(opts SidecarFlagOpts) []string

    // AgentPaneCmd returns the shell command string emitted into the tmux
    // agent pane for this session.
    //
    // Cites: internal/session/session.go:252-308 (BuildOpencodeCmd switch).
    //
    // For podman: "podman attach --sig-proxy=false <container-name>"
    // For bwrap, sandbox-exec: "prism agent-run --session <session-name>"
    // For host: the direct opencode invocation (preserves today's behaviour).
    AgentPaneCmd(opts AgentPaneOpts) string

    // BuildRunArgs and Run survive from the existing interface — they are the
    // argv-and-launch contract that is already in place today and works for
    // the podman path. Bwrap and sandbox-exec also implement them after
    // PrepareBwrap/PrepareSandboxExec fold in.
    BuildRunArgs() []string
    Run(ctx context.Context, args []string) error

    // AgentRun runs the in-sandbox dispatch from `prism agent-run`. Only
    // bwrap and sandbox-exec implement this meaningfully; podman and host
    // return an error ("agent-run is not used in this isolation mode").
    //
    // Cites: cmd/agent_run.go:34-69 (current dispatch switch);
    //        cmd/agent_run.go:160-350 (bwrap body);
    //        cmd/agent_run.go:492-619 (sandbox-exec body).
    AgentRun(ctx context.Context, opts AgentRunOpts) error

    // Lifecycle --------------------------------------------------------------

    // Shutdown stops the isolated process cleanly. Existing method.
    // Cites: internal/container/container.go:813-832 (already delegates).
    Shutdown()

    // EnsureRemoved makes sure no residual resource (container, process,
    // ephemeral file) for the named session remains. Idempotent.
    //
    // Cites: internal/container/container.go:332-358 (EnsureRemoved);
    //        cmd/cleanup.go:792-868, :882-979 (per-mode stop/rm branches).
    //
    // For podman: podman stop && podman rm.
    // For bwrap, sandbox-exec: kill orphan agent-run processes by name.
    // For host: no-op.
    EnsureRemoved(ctx context.Context, sessionName string) error

    // Reset performs the heavier "wipe everything matching prism-*" cleanup
    // invoked by `prism reset`.
    //
    // Cites: cmd/reset.go:126-182 (today: podman ps / podman rm -f only).
    //
    // For podman: today's behaviour. For others: a stub that may grow into
    // orphan-agent-run reaping over time. [uncertain — may merge with
    // EnsureRemoved if the semantic gap proves narrow.]
    Reset(ctx context.Context) error

    // HasExited is unchanged. Cites: internal/container/container.go via
    // the existing interface; one of the original five methods.
    HasExited() (bool, int)

    // DumpLogs is unchanged. One of the original five.
    DumpLogs()

    // Diagnostics ------------------------------------------------------------

    // LogPaths returns the per-mode log file set (sidecar log, harness log,
    // agent-run log). Today the --agent-run convenience alias in cmd/logs.go
    // hard-codes the bwrap path; this method removes the hard-coding.
    //
    // Cites: cmd/logs.go:11, 51, 73, 169.
    //
    // [uncertain — overlaps with the archive-side StorageRoot path resolution;
    // a follow-up may unify the two under one "session paths" struct.]
    LogPaths(sessionName string) LogPaths

    // Archive ----------------------------------------------------------------

    // ArchiveStorageRoot returns the per-mode opencode storage root used by
    // the archive copy step.
    //
    // Cites: internal/archive/archive.go:259-273 (resolveStorageRoot switch).
    //
    // For podman: $HOME/.local/share/opencode/prism-sessions/<container>/storage
    // For bwrap, sandbox-exec, host: $HOME/.local/share/opencode/storage
    ArchiveStorageRoot(home, sessionName string) (string, error)

    // ExtraArchiveFiles returns paths that the archive should copy in
    // addition to the harness storage subtree.
    //
    // Cites: internal/archive/archive.go:189-191 (bwrap-only agent-run.log copy).
    //
    // For bwrap: returns the agent-run.log path. For others: empty.
    ExtraArchiveFiles(sessionName string) []string
}
```

### 4.2 The registry

```go
// IsolationRegistry maps isolation mode names to their constructors.
// Implementations register at init() time.
type IsolationRegistry struct { /* unexported map[config.IsolationMode]Constructor */ }

type Constructor func(opts ConstructorOpts) Isolator

// Register is called from init() in each isolator file.
func Register(name config.IsolationMode, c Constructor) { /* … */ }

// For returns the registered isolator for mode, or an error if mode is unknown.
func For(mode config.IsolationMode, opts ConstructorOpts) (Isolator, error)

// Names returns the registered mode names (replaces ValidIsolationModes).
// Cites: internal/config/config.go:18-43.
func Names() []config.IsolationMode

// Resolve picks the effective mode from the various sources of truth:
// flag value, deprecated --host-mode flag, persisted host_mode column, config
// fallback. Wraps today's logic in one place so the deprecation surface lives
// here and goes away cleanly when A.4 lands.
//
// Cites: cmd/spawn.go:164-198 (resolveIsolationMode);
//        internal/session/session.go:240-244 (effectiveIsolationMode);
//        internal/session/spawn.go:369-374;
//        cmd/restore.go:43, :89-92, :125-127;
//        internal/db/db.go:72-97 (Status.EffectiveIsolationMode).
func Resolve(input ResolveInput) (config.IsolationMode, error)
```

### 4.3 The capability struct

A flat struct, not a parade of methods, because the caller usually wants two or
three flags at once and a single `iso.Capabilities()` call is cheaper to read.

```go
type Capabilities struct {
    // IsContainer: replaces (isoMode == IsolationPodman) tests outside the
    // package. Cites: cmd/spawn.go:275, cmd/switch.go:1069, cmd/pr.go:77.
    IsContainer bool

    // OwnsContainerLifecycle: the sidecar runs container create/stop/rm.
    // Cites: internal/sidecar/sidecar.go:153-183 (s.cfg.Container == nil branch).
    OwnsContainerLifecycle bool

    // NeedsConfigBlob: the harness config blob must be supplied (env-var or
    // on-disk). Cites: cmd/spawn.go:311-357, cmd/switch.go:1069-1108,
    // cmd/restore.go:269-292, cmd/pr.go:77-170, internal/review/review.go:1230.
    NeedsConfigBlob bool

    // NeedsHostAPISocket: the sidecar binds the host-API socket for this mode.
    // Cites: internal/sidecar/sidecar.go:512-547; cmd/sidecar.go:233-249.
    NeedsHostAPISocket bool

    // UsesContainerHarness: the harness adapter is constructed in container
    // mode (podman) vs host mode (everything else).
    // Cites: cmd/sidecar.go:288-301.
    UsesContainerHarness bool

    // RestartOnExit: the sidecar restart-loop goroutine is started for this
    // mode. Cites: cmd/sidecar.go:348.
    RestartOnExit bool

    // NeedsStartupConnectTimeout: the sidecar's bwrap-only startup-connect
    // timeout fires for this mode. Cites: internal/sidecar/sidecar.go:638-680.
    NeedsStartupConnectTimeout bool

    // NeedsReadinessWait: the agent-pane command should be prefixed by the
    // readiness-wait shell command. Cites: internal/session/session.go:539-547.
    // [uncertain — partly a harness concern; reconciled by Track B.1.]
    NeedsReadinessWait bool

    // EmitsTmuxStatusColumns: the tmux event hooks should seed isolation-
    // specific status columns for sessions in this mode.
    // Cites: cmd/event.go (per §6.22 flat appendix).
    EmitsTmuxStatusColumns bool
}
```

### 4.4 The `host` mode question

`host` is currently a member of `IsolationMode` but is not actually an
isolation mechanism — the agent runs unsandboxed. Two viable shapes:

- **Option A: `hostIsolator` is a registered no-op.** Most methods return
  empty/no-op; `AgentPaneCmd` returns the direct opencode invocation;
  `Capabilities()` is mostly false. **Pro:** every caller goes through
  `registry.For(mode)` uniformly — no special-case for host. **Con:** the
  no-op implementation grows over time.
- **Option B: `host` is not registered; callers handle it as the absence of
  isolation.** **Pro:** keeps `Isolator` honest. **Con:** every caller still
  has an `if mode == "host"` arm — defeating the point.

**Proposal: Option A.** The cost of one extra small file
(`hostIsolator.go`) is much lower than the cost of an `if mode == host` arm
in every cmd file. The registered host isolator becomes the single seam where
host-mode behaviour is described.

### 4.5 What the four AC-required citations look like in summary

For each proposed addition to `Isolator`, the AC requires at least one current
site that would move to it. Compact roll-up:

| Method | Cites at least |
|---|---|
| `Name()` | `internal/config/config.go:18-43` |
| `Capabilities()` | `internal/sidecar/sidecar.go:153-183`, `cmd/sidecar.go:121-138`, `cmd/spawn.go:311-357` |
| `Available(ctx)` | `internal/container/container.go:1700-1734` (CheckAvailability), `cmd/spawn.go:212-225` (checkBwrapPlatform/checkSandboxExecPlatform), `cmd/spawn.go:279-294` |
| `Cap(ctx, db)` | `cmd/concurrency.go:34-56` (podman), `cmd/concurrency.go:71-122` (bwrap), `internal/db/db.go:1720-1740` |
| `WriteHarnessConfigBlob(...)` | `cmd/spawn.go:383-435`, `cmd/switch.go:432`, `cmd/restore.go:269-292`, `cmd/pr.go:77-170`, `internal/review/review.go:740-789, 987-1021` |
| `WriteGitconfig(...)` | `internal/container/container.go:526-625` |
| `ApplyResources(...)` | `internal/config/profiles.go:97-107`, `internal/container/container.go:212-241` |
| `SidecarFlags(...)` | `internal/session/sidecar.go:311-340`, `internal/session/session.go:514-523` |
| `AgentPaneCmd(...)` | `internal/session/session.go:252-308` |
| `BuildRunArgs()` | existing — `internal/container/isolator.go:31-35` |
| `Run(ctx, args)` | existing — `internal/container/isolator.go:37-41` |
| `AgentRun(ctx, opts)` | `cmd/agent_run.go:34-69, 160-350, 492-619` |
| `Shutdown()` | existing — `internal/container/container.go:813-832` |
| `EnsureRemoved(ctx, name)` | `internal/container/container.go:332-358`, `cmd/cleanup.go:792-868, 882-979` |
| `Reset(ctx)` | `cmd/reset.go:126-182` |
| `HasExited()` | existing |
| `DumpLogs()` | existing |
| `LogPaths(name)` | `cmd/logs.go:11, 51, 73, 169` |
| `ArchiveStorageRoot(home, name)` | `internal/archive/archive.go:259-273` |
| `ExtraArchiveFiles(name)` | `internal/archive/archive.go:189-191` |

## 5. Sites that do NOT fit `Isolator` and where they live

The (c)-tagged sites from §3 in one place:

1. **`Capabilities` flat struct (§4.3).** Lives next to the interface in
   `internal/container/isolator.go` (or a dedicated `capabilities.go`). The
   alternative — adding ten yes/no methods to `Isolator` — would make the
   interface unwieldy and would make capability lookups noisier at call sites.

2. **`IsolationRegistry` and `Resolve` helpers (§4.2).** Live in a new
   `internal/container/registry.go`. The registry owns the mode name → constructor
   mapping. `Resolve` owns the deprecation surface (host_mode flag, ContainerMode
   bool, persisted host_mode column) so that A.4 has one file to touch.

3. **`internal/sandboxenv` (§3.14).** Inside-the-sandbox identity for
   `cmd/merge.go`, `cmd/merges.go`. The outside-the-sandbox `Isolator` does not
   know whether the *current* process is sandboxed; that question has its own
   answer (env vars set by the launcher). New small package.

4. **`ResourceApplier` (possible alternative shape for `ApplyResources`).**
   Currently proposed as a method on `Isolator`. If non-podman modes never
   gain real resource limits, this could be split into a separate interface
   that only `podmanIsolator` implements. [uncertain — see §3.2.]

5. **`db.ActiveSessionCountForMode(mode)` / `db.ActiveSessionsForMode(mode)`.**
   The mode-literal-in-SQL surface (`internal/db/db.go:1720-1740`) belongs in
   the DB layer's API, not in `Isolator`, because the SQL is mode-agnostic
   plumbing once parameterised. `Isolator.Cap` calls these; the per-mode
   policy lives in `Cap`, the per-mode SQL filter is just a parameter.

6. **`LogPaths` / `ArchiveStorageRoot` / `ExtraArchiveFiles` overlap.** All
   three are filesystem-path queries. They might consolidate into a
   `SessionPaths` struct returned by one `iso.Paths(sessionName) SessionPaths`
   method. A.1 keeps them separate to preserve the inventory's clear delineation
   between "logs" and "archive" sites; a future unification is fair game.
   [uncertain.]

## 6. Open questions and `[uncertain]` flags

Collected for visibility:

1. **`ApplyResources` vs separate `ResourceApplier` interface (§3.2, §4.1).**
   Depends on whether non-podman modes ever gain real resource limits.
2. **`NeedsReadinessWait` (§4.3).** Partly a harness-shape concern, not a pure
   isolator concern. Track B.1 is the right venue to reconcile.
3. **`merge.go`/`merges.go` isolator-vs-sandboxenv split (§3.14).** Could
   alternatively be a method on `Isolator`; preference is the dedicated
   sandboxenv package because the inside-the-sandbox view is genuinely
   different from the outside view.
4. **`Reset()` semantics for non-podman modes (§3.17).** Today the bwrap
   equivalent is "do nothing"; orphan-agent-run reaping is a future
   implementation. A.1 declares the shape; the cleanup work lands later.
5. **`LogPaths` / `ArchiveStorageRoot` overlap (§4.1, §5.6).** May consolidate
   into a single `SessionPaths` method.
6. **Removing the `archive.go:279-285` `containerNameForSession` workaround
   (§3.19).** Depends on whether `internal/archive` should be allowed to
   import `internal/container`; today it deliberately does not.
7. **`host` as a registered isolator vs special-cased (§4.4).** Recommended
   Option A but acknowledging Option B is defensible.
8. **`hostIsolator.AgentRun()` returning an error.** The cleanest answer if
   `host` is registered; needs a friendly error string the cmd-layer surfaces.
9. **What happens to `internal/container.Manager` after the methods migrate
   into the isolators?** Probably a thin coordinator that owns the volume
   directories and per-session temp file lifetimes; or it disappears entirely
   and those concerns also move into the isolator. A.1 does not commit; A.2
   is the right venue to decide.

## 7. Proposed extraction order

The order respects dependencies between moves. Each step is intended to be
landable as one PR with no behaviour change. Steps grouped under the same
header are independent of each other; later groups depend on earlier ones.

### Phase 1 — Registry skeleton (no behaviour change)

1. **R.1: Add `IsolationRegistry`, `Register`, `For`, `Names`** in
   `internal/container/registry.go`. Register `podmanIsolator` (already exists),
   plus stub `bwrapIsolator`, `sandboxExecIsolator`, `hostIsolator` whose
   `Capabilities()` returns the right flags but whose other methods either
   delegate to today's free functions or return "not implemented yet".
2. **R.2: Add `Capabilities` struct** populated correctly per mode.
3. **R.3: Add `IsolationRegistry.Resolve`** that wraps today's
   `cmd/spawn.go:resolveIsolationMode`, `internal/session.effectiveIsolationMode`,
   `cmd/restore.go` per-row resolver. No call sites change yet — the helper is
   on standby.

These three steps add code without removing any. After R.1–R.3 the registry
exists and passes tests but no cmd/internal site reads it yet.

### Phase 2 — Capability reads (low-risk, repeated pattern)

The capability reads are the lowest-risk migration because they replace a
boolean computation with another boolean. Each can land independently.

4. **C.1: `cmd/spawn.go:311-357` `NeedsConfigBlob` → `iso.Capabilities()`.**
5. **C.2: `cmd/switch.go:1069-1108` `sandboxed` → `iso.Capabilities()`.**
6. **C.3: `cmd/restore.go:269-292`, `cmd/pr.go:77-170`, `internal/review/review.go:1230`** — same predicate.
7. **C.4: `cmd/sidecar.go:121-138, 194-348`** — `useContainerHarness`, `needsHostAPI`, `OnReady` set conditions, restart-loop start condition, etc.
8. **C.5: `internal/sidecar/sidecar.go:153-183, 512-547, 638-680`** —
   `OwnsContainerLifecycle`, `NeedsHostAPISocket`, `NeedsStartupConnectTimeout`.

After Phase 2, every "if mode is X then enable feature Y" branch outside the
implementation files reads from `Capabilities()`.

### Phase 3 — Single-method dispatches (medium risk)

Each of these swaps a small switch for a single method call. Order independent.

9. **D.1: `Available()`** — `cmd/spawn.go:212-225, 279-294` (replaces
   `CheckAvailability`, `checkBwrapPlatform`, `checkSandboxExecPlatform`).
10. **D.2: `Cap()`** — `cmd/concurrency.go` (replaces the two parallel cap functions). **Coordinates with A.3.**
11. **D.3: `WriteHarnessConfigBlob()`** — `cmd/spawn.go:383-435`,
    `cmd/switch.go:432, 628-991, 1027-1212`, `cmd/restore.go:269-292`,
    `cmd/pr.go:77-170`, `internal/review/review.go:740-789, 987-1021`.
12. **D.4: `AgentPaneCmd()`** — `internal/session/session.go:252-308`.
13. **D.5: `SidecarFlags()`** — `internal/session/sidecar.go:311-340`,
    `internal/session/session.go:514-523`.
14. **D.6: `ArchiveStorageRoot()` and `ExtraArchiveFiles()`** —
    `internal/archive/archive.go:189-191, 248-276`.
15. **D.7: `LogPaths()`** — `cmd/logs.go:11, 51, 73, 169`.

### Phase 4 — Lifecycle migrations (higher risk; require care with concurrent sessions)

16. **L.1: `EnsureRemoved()`** — `internal/container/container.go:332-358` plus the
    `cmd/cleanup.go:792-868, 882-979` per-mode branches. Must keep idempotency.
17. **L.2: `WriteGitconfig()`** — `internal/container/container.go:526-625`.
    Internal to `container`, but the `Manager.writeGitconfig(mode)` switch goes away.
18. **L.3: `Reset()`** — `cmd/reset.go:126-182`. Low-risk in itself; podman
    body is intact, others remain stubs.
19. **L.4: Fold `Manager.PrepareBwrap` and `Manager.PrepareSandboxExec`** into
    the respective isolators (`internal/container/container.go:833-882, 884-905`).
    `Manager` reduces to a coordinator over `Isolator`.
20. **L.5: Fold `Manager.Create`** (the podman path,
    `internal/container/container.go:642-760`) into `podmanIsolator`. The
    `Manager` surface shrinks further; `buildRunArgs` decomposition (D.1 from
    Track D) is the natural follow-up here.
21. **L.6: `AgentRun()`** — `cmd/agent_run.go:34-69, 160-350, 492-619`. The
    cmd file becomes a thin dispatcher: `iso, _ := registry.For(persistedMode); return iso.AgentRun(ctx, opts)`.

### Phase 5 — Inside-the-sandbox abstraction (decoupled from above)

22. **S.1: Add `internal/sandboxenv`** with `IsInsideSandbox()` and
    `HostAPISocket()` helpers. Migrate `cmd/merge.go:62`, `cmd/merges.go:90, 137`.
    No registry coupling; can land alongside Phase 2.

### Phase 6 — A.4 deprecation

23. **A.4 lands.** Removes `Config.ContainerMode`, `Opts.ContainerMode`,
    `--host-mode` flag, `Status.HostMode`, `EffectiveIsolationMode`,
    `effectiveIsolationMode`. The `IsolationRegistry.Resolve` body simplifies
    correspondingly. After A.4, `host_mode` columns get a final migration to
    backfill `isolation_mode` from `host_mode` and the column is dropped.

### Dependency summary

- Phase 1 is a prerequisite for everything else. It is purely additive.
- Phase 2 (capability reads) is independent of Phases 3–4. It could land
  in parallel with Phase 3.
- Phase 3 items D.1–D.7 are mutually independent.
- Phase 4 items L.1–L.6 mostly depend on Phase 3 having moved the *single-method*
  surface out of `Manager`. L.4 and L.5 specifically require D.1, D.2, D.5
  to be in place because once `Manager.Create`/`PrepareBwrap`/`PrepareSandboxExec`
  fold in, callers must already be reaching them via the registry.
- Phase 5 is independent of Phases 1–4 and could land at any time after
  Phase 1.
- Phase 6 (A.4) depends on Phases 2–4 having migrated the legacy fallback
  callers; until then `Resolve` carries the deprecated paths.

## 8. Relationship with sibling Track A issues

- **A.2 (duplication audit, #1077)** consumes A.1's interface as the *target
  shape* for shared helpers extracted from `bwrap.go`/`sandbox_exec.go`. A.2
  may further decompose `BuildArgs` into helper-shaped methods on top of A.1.
- **A.3 (concurrency-cap unification, #1078)** is the implementation of
  `Isolator.Cap()` declared in §4.1. A.1 declares the shape; A.3 unifies the
  warning/error rendering and removes `cmd/concurrency.go`'s two parallel
  functions. **Phase 3 D.2 above is A.3's natural landing point.**
- **A.4 (deprecation surface, #1079)** removes the `ContainerMode`/`host_mode`
  fallbacks that `IsolationRegistry.Resolve` currently wraps. A.1's Phase 6 is
  A.4's domain.

## 9. Out of scope

- **Implementation of the interface superset.** A.1 is a proposal; the
  implementation is a separate wave of follow-up issues filed after Track E
  synthesis (§7 enumerates the suggested ordering, but the issues themselves
  are not filed by A.1).
- **Bwrap-vs-sandbox-exec implementation duplication** — A.2.
- **Concurrency-cap unification** — A.3.
- **Deprecation surface removal** — A.4.
- **Harness coupling** — Track B.
- **A/B testing surface** — Track C.

## 10. Acceptance-criteria self-check

- [x] Document at `modules/programs/prism/prism/docs/reviews/A1-isolation-registry-shape.md` exists and references inventory §6 by name (header of §1, opening of §3, every per-site row).
- [x] Every site enumerated in inventory §6.1 through §6.21 is addressed in §3.1 through §3.21; §6.22 is addressed in §3.22; sites are listed individually with file:line citations preserved from the inventory.
- [x] Proposed `Isolator` interface superset is presented as a concrete Go interface signature in §4.1.
- [x] For each proposed addition to `Isolator`, the document cites at least one current site (file:line) that would move to it — see the per-method roll-up at §4.5.
- [x] Sites that would NOT fit the interface are identified (the (c) tag throughout §3 and the consolidated list in §5) with proposed alternative homes.
- [x] Proposed extraction order respecting dependencies is in §7.
- [x] `[uncertain]` flags are present where the right shape is not obvious; consolidated in §6.
- [x] Document contains zero implementation work — markdown only, no Go file added or modified outside `docs/reviews/`.
