# E.1 — Post-narrow-review synthesis and prioritisation

**Status:** synthesis (no code changes).
**Issue:** [#1090](https://github.com/prismatic-koi/nixos-config/issues/1090).
**Refs:** [#1072](https://github.com/prismatic-koi/nixos-config/issues/1072) (design doc — narrow architecture-review series).
**Source corpus:** every landed proposal under `modules/programs/prism/prism/docs/reviews/` for tracks A, B, C, D, plus the design doc at
[`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).

This document is the synthesis pass for the narrow architecture-review series. It reads
every proposal that has landed under tracks A, B, C, and D, extracts every recommendation,
categorises each across five dimensions (goal alignment, sequencing, cost, risk,
independence), records cross-proposal dependencies, resolves conflicts where the source
proposals disagree, and produces a prioritised roadmap with implementation issues filed
for the top recommendations.

## 1. Frame

The series is in service of three stated long-term goals (per
[`000-design-narrow-review-series.md`](000-design-narrow-review-series.md) §"Stated long-term goals"):

- **G1 — Cheap podman removal.** Bwrap (Linux) and sandbox-exec (Darwin) are the long-term
  primary modes. Podman stays in the tree until sandbox-exec parity is reached, then is
  removed in one surgical PR. **Gated externally on the sandbox-exec parity track:**
  [#1012](https://github.com/prismatic-koi/nixos-config/issues/1012),
  [#1017](https://github.com/prismatic-koi/nixos-config/issues/1017),
  [#1018](https://github.com/prismatic-koi/nixos-config/issues/1018).
- **G2 — Multi-harness with PI.** opencode = HTTP+SSE; PI = JSONL-over-stdin/stdout RPC.
  The harness abstraction must accommodate different transport shapes. RFC #691 / #606.
- **G3 — Structured A/B testing as a routine workflow.** Per-role model variation,
  persisted spawn inputs alongside resolved values, structured outcome capture, comparison
  rendering.

Every recommendation below is evaluated against how much it advances one or more of these
goals. Where a recommendation is purely structural cleanup with no goal alignment, it is
labelled **G0** and ranked accordingly.

### 1.1 What has already landed (as of writing)

Four anchor-read proposals have already landed on `main` and are not "open recommendations"
— they are foundations the rest of the synthesis builds on:

- **A.1** ([#1073](https://github.com/prismatic-koi/nixos-config/issues/1073)) — landed in
  PR #1097. The `Isolator` interface superset and `IsolationRegistry` shape proposal.
- **B.1** ([#1074](https://github.com/prismatic-koi/nixos-config/issues/1074)) — landed in
  PR #1096. Harness transport-shape classification.
- **B.2** ([#1080](https://github.com/prismatic-koi/nixos-config/issues/1080)) — landed in
  PR #1102. Harness registry + `TransportShape` enum.
- **C.1** ([#1075](https://github.com/prismatic-koi/nixos-config/issues/1075)) — landed in
  PR #1095. `spawn_inputs` table + cross-harness comparison-metric taxonomy.

All four are **proposal documents** — none of them shipped runtime code beyond the
markdown deliverable. The "implementation work the proposal motivates" is what this
synthesis prioritises.

## 2. Cross-proposal recommendation matrix

Every recommendation from every A.x, B.x, C.x, D.x proposal document, categorised across
five dimensions:

- **Goal:** which of G1 / G2 / G3 / G0 (cleanup) the recommendation serves. Multiple goals
  separated by `+`.
- **Seq:** what must land first. `–` for "no prerequisites within the series".
- **Cost:** rough size — `S` (one PR), `M` (a few PRs), `L` (phased train).
- **Risk:** what existing flow is affected. `low` / `med` / `high`.
- **Indep:** can it ship without coordinating with other proposals' implementation
  surface? `Y` / `N` / `partial`.

Recommendations are grouped by source proposal. Where a single proposal has phases or a
named sub-recommendation list, each is listed as its own row. Rows are referenced by
short codes (e.g. **A1.R1**) for use by the dependency graph and roadmap below.

### 2.1 Track A — isolation

| Code | Recommendation | Source | Goal | Seq | Cost | Risk | Indep |
|---|---|---|---|---|---|---|---|
| A1.R1 | Add `IsolationRegistry`, `Register`, `For`, `Names`; register `podmanIsolator` (existing) plus stub `bwrapIsolator`, `sandboxExecIsolator`, `hostIsolator`. | [A1 §7 Phase 1](A1-isolation-registry-shape.md) | G1+G0 | – | S | low | Y |
| A1.R2 | Add `Capabilities` struct populated correctly per mode. | A1 §7 Phase 1 | G1+G0 | A1.R1 | S | low | Y |
| A1.R3 | Add `IsolationRegistry.Resolve` wrapping today's `resolveIsolationMode`, `effectiveIsolationMode`, restore-row resolver. | A1 §7 Phase 1 | G1+G0 | A1.R1 | S | low | Y |
| A1.C1-C5 | Capability-read migrations (5 sites): `cmd/spawn.go:311-357`; `cmd/switch.go:1069-1108`; `cmd/restore.go`/`pr.go`/`review.go`; `cmd/sidecar.go:121-138, 194-348`; `internal/sidecar/sidecar.go:153-183, 512-547, 638-680`. Each replaces `if mode == X` with `iso.Capabilities()` reads. | A1 §7 Phase 2 | G1 | A1.R1, A1.R2 | M | low | Y |
| A1.D1-D7 | Single-method dispatch migrations: `Available()`, `Cap()`, `WriteHarnessConfigBlob()`, `AgentPaneCmd()`, `SidecarFlags()`, archive paths, `LogPaths()`. Each is a small switch → method-call swap. | A1 §7 Phase 3 | G1 | A1.R1, A1.R2 | M | med | Y (each independent) |
| A1.L1-L6 | Lifecycle migrations: `EnsureRemoved()`, `WriteGitconfig()`, `Reset()`, fold `PrepareBwrap`/`PrepareSandboxExec`, fold `Manager.Create` into podmanIsolator, `AgentRun()` dispatch. | A1 §7 Phase 4 | G1 | A1.D1-D7 | L | med | partial |
| A1.S1 | `internal/sandboxenv` package for inside-the-sandbox `IsInsideSandbox()` / `HostAPISocket()` helpers; migrate `cmd/merge.go`/`cmd/merges.go`. | A1 §7 Phase 5 | G0 | – | S | low | Y |
| A2.S1 | `m.tempPath(stem, suffix)` helper; consolidates seven `Manager` temp-path methods. | [A2 §3.S1, §4](A2-isolation-implementation-duplication-audit.md) | G0 | – | S | low | Y |
| A2.S2 | `HarnessInvocation(cfg)` helper; consolidates `--port`/`--hostname` tail. | A2 §3.S2, §4 | G0 | – | S | low | Y |
| A2.E1 | `AppendStandardEnv` helper for `AgentEnvVars` + `RuntimeEnv` injection. | A2 §3.S3-S4, §4 | G0 | – | S | low | Y |
| A2.G1 | Extend `writeGitconfig` for sandbox-exec (third mode value or reuse `bwrap`). | A2 §3.3, §4 | G1+G0 | (gated on #1017) | S | low | partial |
| A2.M1 | `MountSpec` shape + `StandardSandboxMounts` walk + per-mode appenders; consolidates ~700 LoC across three implementations. | A2 §3.1, §3.S6, §4 | G1+G0 | (best after #1017 lands sandbox-exec mounts) | L | med | N |
| A2.SUP | `SuperviseChild(cmd, stdinFd, onWinch)` shared signal/PTY supervisor. | A2 §3.6, §4 | G0 | (gated on #1018) | S | med | N |
| A2.GR | `gracefulShutdown(*exec.Cmd, gracePeriod)` shared shutdown body for bwrap/sandbox-exec once #1018 lands. | A2 §3.7, §4 | G0 | (gated on #1018) | S | low | N |
| A3.IF | `Isolator.Cap(ctx, dbPath) CapStatus` interface method + `CapStatus`/`InFlightSession` types + `Render*` methods consolidating warning/error rendering. | [A3 §4](A3-concurrency-cap-unification.md) | G1+G0 | A1.R1 (registry), #1018 (sandbox-exec parallel impl) | M | low | partial |
| A3.DB | `db.ActiveSessionCountForMode(mode)` and `db.ActiveSessionsForMode(mode)`; replace bwrap-specific helpers. | A3 §5.4 | G1+G0 | – | S | low | Y |
| A3.DD | Delete dead code: `container.CheckCap`, `ListInFlight`, `FormatExceededError/Warning`, `CheckResult`, old `InFlightSession`, bwrap-specific DB helpers, sandbox-exec-specific helpers. | A3 §7.5 | G0 | A3.IF | S | low | N |
| A4.PA | Phase A — backfill SQL migration: `UPDATE agent_status SET isolation_mode = CASE WHEN host_mode = 1 THEN 'host' ELSE 'podman' END WHERE isolation_mode IS NULL;`. | [A4 §5 Phase A](A4-deprecation-surface-review.md) | G0 | A1.R1 (registry) | S | low | Y |
| A4.PB | Phase B — remove `Config.ContainerMode`, `parsedConfig.ContainerMode`, `(Config).EffectiveIsolationMode()`, related load-time fallbacks; set `defaults().DefaultIsolationMode = IsolationHost`. | A4 §5 Phase B | G0 | A4.PA, A1.R1 | S | low | Y |
| A4.PC | Phase C — drop `Opts.ContainerMode`, `StartSidecarOpts.ContainerMode`, `SpawnOpts.ContainerMode`; collapse `--container` to `IsolationMode` test. | A4 §5 Phase C | G0 | A4.PA | S | low | Y |
| A4.PD | Phase D — `--host-mode` flag and host-API `host_mode` field deprecation cycle then drop. | A4 §5 Phase D | G0 | (one-release deprecation period) | S | med | Y |
| A4.PE | Phase E — drop `Status.EffectiveIsolationMode`, `Status.HostMode`, `SetHostMode`, `host_mode` column (table rebuild migration). | A4 §5 Phase E | G0 | A4.PA, A4.PB; coordinate with A3.IF | M | med | partial |

### 2.2 Track B — harness

| Code | Recommendation | Source | Goal | Seq | Cost | Risk | Indep |
|---|---|---|---|---|---|---|---|
| B3.S1 | Stage 1 — stdio-harness-aware sidecar startup, behind a transport-shape gate. The sidecar gains a `TransportShape` check at the top of `Run()`; HTTP-shape harnesses are unaffected. No behaviour change for existing harnesses. | [B3 §"Staged implementation suggestion"](B3-sidecar-lifecycle-for-stdio-harnesses.md) | G2 | B.2 (landed) | M | med | Y |
| B3.S2 | Stage 2 — fake stdio-harness path verification under bwrap (test-only harness writes a few JSONL frames). | B3 §"Staged implementation suggestion" | G2 | B3.S1 | S | low | Y |
| B3.S3 | Stage 3 — PI integration on podman; resolves Open Question 1 (`podman exec -i` for JSONL + `podman attach` for TUI). | B3 §"Staged implementation suggestion" | G2 | B3.S1, B3.S2; PI prototyping | L | high | N |
| B3.S4 | Stage 4 — PI integration on bwrap/sandbox-exec/host. Conditional on PI's TUI bridging story working in those modes (PI-side concern). | B3 §"Staged implementation suggestion" | G2 | B3.S3 | L | high | N |
| B4.DEFER | **Defer** the harness-group abstraction (`HTTPHarnessBase`, `StdioHarnessBase`) until the second harness of each shape exists. Recommendation is "no shared base now". | [B4 §5](B4-harness-group-abstraction.md) | G2 | – | – (deferral) | – | n/a |
| B5.TR | Adopt **Translate** as the default payload-schema strategy: PI adapter normalises to opencode-shaped payload at write time. Information loss bounded by existing zero-means-unavailable convention. | [B5 §6 recommendation](B5-payload-schema-decoupling.md) | G2 | B3.S3 (need real PI traces to validate) | M | med | partial |
| B5.NORM | C.1 reserved an `agent_events.normalised_payload` column for B.5; under Translate, the column is **not needed** because all rows are already opencode-shaped on the wire. Recommendation: leave the column reserved but unpopulated; revisit if Widen/Version is later adopted for any axis. | B5 §"Path forward" + C1 §4.3 cross-ref | G2 | B5.TR | S | low | Y |
| B6.IF | `ArchiveAdapter` interface (`SourcePath`, `Archive`, `Export`, `Version`) + opencode adapter (Step 1). | [B6 §4, §5 Step 1](B6-archive-and-export-pipeline-decoupling.md) | G2+G0 | – | M | med | Y |
| B6.WIRE | Wire adapter through cleanup; remove `archive.HarnessVersion()` shim (Step 2). | B6 §5 Step 2 | G2+G0 | B6.IF | S | low | Y |
| B6.PI | PI adapter implementation (Step 3). | B6 §5 Step 3 | G2 | B6.WIRE, B3.S3 | M | med | N |
| B6.LSF | Decision on `internal/opencode.LatestSessionForDir` — recommend delete (currently unused by production code). | B6 §"Open questions" #4 | G0 | – | S | low | Y |
| B6.PIMONO | pi-mono v3 stays the canonical export format (`session.jsonl`); each adapter's `Export` produces it. | B6 §"Canonical export format" | G2 | B6.IF | – | – | Y |

### 2.3 Track C — A/B testing

| Code | Recommendation | Source | Goal | Seq | Cost | Risk | Indep |
|---|---|---|---|---|---|---|---|
| C2.CLI | Adopt `--model-override role=model` (repeated flag) as the CLI surface for per-role model variation. Precedence: `--model-override` > `--model` > profile bucket. | [C2 §4.4, §8](C2-per-role-model-variation.md) | G3 | C.1 (landed) | S | low | Y |
| C2.CFG | Optional `agent_overrides` extension to `ProfileEntry` for named profiles. | C2 §3.2, §8 | G3 | C2.CLI | S | low | Y |
| C2.PLB | Plumbing: replace `agentModel string` in opencode adapter with `modelsByRole map[string]string`; update `EffectiveModel` to consult prism's per-role map first. | C2 §3.3, §5 | G3 | C2.CLI | M | med | partial |
| C2.PSP | Persist user-supplied per-role override map as JSON in `spawn_inputs.model_variant_overrides` (column already reserved by C.1). | C2 §3.4 | G3 | C2.CLI | S | low | Y |
| C2.PROXY | Add `ModelOverrides` to host-API `/spawn` proxy request struct; propagate through repeated `--model-override role=model` arg list. | C2 §3.3 | G3 | C2.PLB | S | low | partial |
| C2.REV | `review.Opts.ModelsByRole map[string]string` field; review fan-out distributes per-role entries to per-agent SpawnOpts. | C2 §3.3 | G3 | C2.PLB | S | low | partial |
| C3.OUT | `spawn_outcome` table keyed on `instance_id`, replacing C.1's reserved `sessions.outcome_summary` JSON column. Covers process-level signals (end_state, exit_code, durations, counts), capturable agent-level signals (PR number, merge timestamp, review verdict roll-up), pre-computed per-axis aggregations (token cost, tool-call count, TTFE, time-to-finished, error count, permission counts, doom-loop count). | [C3 §4](C3-outcome-capture.md) | G3 | C.1 (landed) | M | med | Y |
| C3.RUB | **Defer** rubric-grading mechanism. Reserve `rubric_verdict`, `rubric_score`, `rubric_breakdown`, `rubric_grader` columns for a future grader. No `prism rubric` command in scope. | C3 §3 | G3 | – | – (deferral) | – | n/a |
| C3.CMP | New `prism stats compare <run-A> <run-B> [<run-C>...]` subcommand and sister `prism stats abtest <group_id>`. JSON form is the contract; table is renderer over it. | C3 §6 | G3 | C3.OUT | M | low | Y |
| C3.STATS | Existing `prism stats` subcommands grow `--group-by harness/profile/variant` axes. | C1 §6 + C3 §6 | G3 | C3.OUT, C2.PSP | S | low | Y |
| C4.SK | Persist skills manifest hash as `spawn_inputs.skills_manifest_hash` (column already reserved by C.1) using `nix:<store-basename>` when the directory is a nix-store path, `sha256:<hex>` content hash otherwise. Capture **at spawn time** in `cmd/spawn.go`. | [C4 §2](C4-prompt-and-skills-as-spawn-inputs.md) | G3 | C.1 (landed) | S | low | Y |
| C4.PT | `spawn_inputs.prompt_template_hash` is **NULL** for free-form prompts and `<template-name>:<source-sha>` for genuinely templated paths (today: only `review-fanout`). Build-time `-ldflags '-X'` embeds the source SHA. | C4 §3 | G3 | – | S | low | Y |
| C4.AP | Add new column `spawn_inputs.agent_prompt_hash TEXT` for the agent-role system prompt manifest (same `nix:` / `sha256:` / `git:` shape as skills). One small schema migration on top of C.1. | C4 §5.1 | G3 | C.1 (landed) | S | low | Y |
| C4.SRC | `prompt_source` capture: extend `resolvePrompt` to return source discriminator; propagate through proxy via hidden `--prompt-source` flag; review fan-out sets `'review-fanout'` explicitly. | C4 §4.2 | G3 | – | S | low | Y |

### 2.4 Track D — cross-cutting cleanups

| Code | Recommendation | Source | Goal | Seq | Cost | Risk | Indep |
|---|---|---|---|---|---|---|---|
| D1.HOST | Decompose `(*Sidecar).hostAPIHandler` (1054 LoC) into route-grouped sub-functions: session-inspection, lifecycle, review, merge-queue. Plus extract `buildCheckinTimeline`, `streamReviewSubprocess`, `buildSpawnArgs`. | [D1 §2](D1-function-size-hot-spots.md) | G0 | (wait for B3.S3) | M | med | partial |
| D1.OPEN | Decompose `db.Open` (683 LoC) into `openAndConfigure`, `seedSchemaVersionIfEmpty`, `runMigrations`, plus per-version `migrateVNtoVN1` functions. Optionally driven by a `[]migrationFn` dispatch table. | D1 §3 | G0 | – | M | low | Y |
| D1.RUN | Decompose `(*Manager).buildRunArgs` (555 LoC) into eight per-concern `appendXxx` helpers. | D1 §4 | G0 | A1+A2 (because content moves into podmanIsolator first) | M | med | N |
| D1.BWRAP | Decompose `(*bwrapIsolator).BuildArgs` (488 LoC) into five per-concern `appendXxx` helpers. | D1 §5 | G0 | A2.M1 (mount unification first) | M | med | N |
| D2.SIDECAR | Split `internal/sidecar/sidecar.go` (3794 LoC) into `sidecar.go`, `run.go` (wait for B.3), `events.go`, `state.go`, `host_api.go` (wait for B.3), `notify.go`, `dashboard.go`, `helpers.go`. Non-Run/non-host_api parts safe now. | [D2 §1](D2-file-size-hot-spots.md) | G0 | partial — `run.go`/`host_api.go` wait for B.3 | M | low | partial |
| D2.DB | Split `internal/db/db.go` (3214 LoC) into per-concern files: `db.go` (Open), `status.go`, `events.go`, `bus.go`, `sessions.go`, `groups.go`, `mergequeue.go`, `types.go`, `maintenance.go`. | D2 §2 | G0 | – | M | low | Y |
| D2.STATS | Split `cmd/stats.go` (2141 LoC) into `stats.go`, `stats_metrics.go`, `stats_render.go`, `stats_aggregate.go`, `stats_events.go`, `stats_model.go`, `stats_format.go`. | D2 §3 | G0 | – | M | low | Y |
| D2.REVIEW | Split `internal/review/review.go` (2071 LoC) into `review.go`, `agents.go`, `run.go`, `poll.go`, `results.go`, `prompt.go`, `context.go`, `lifecycle.go`, `export_test.go`. | D2 §4 | G0 | – | M | low | Y |
| D2.CTR | Split `internal/container/container.go` (1753 LoC). Extract `lifecycle.go`, `credentials.go` now; defer `run_args.go` until A.1/A.2 reshape it. | D2 §5 | G0 | partial — A.1/A.2 for `run_args.go` only | M | low | partial |
| D2.CHECKIN | Split `cmd/checkin.go` (1680 LoC) into per-mode files. | D2 §6 | G0 | – | M | low | Y |
| D2.SWITCH | Split `cmd/switch.go` (1229 LoC). TUI / project-discovery / dashboard parts independent now; session-management parts wait for A.1. | D2 §7 | G0 | partial — A.1 for `switch_session.go` only | M | low | partial |
| D3.NOOP | **No action.** All eleven flagged packages use canonical `Test*` naming; the inventory regen script's regex was buggy. Fix landed under #1076 (already corrected in `architecture-inventory.md`). | [D3 §3, §4](D3-test-naming-audit.md) | G0 | – | – | – | n/a |

### 2.5 D.3 — explicit "no actionable recommendations" record

Per the AC's edge-case requirement that tracks producing no actionable work be recorded
explicitly: **D.3 produced no implementation work.** Its outcome is "the eleven packages
flagged by the inventory regex have correct test naming; the regex itself was buggy and
is already fixed in `architecture-inventory.md` §14.2 step 12 under #1076." There are
no follow-up issues to file from D.3. The D3.NOOP row above records this in the matrix.

## 3. Cross-proposal dependency graph

Edges read as "X must land before Y can usefully land" (or "X significantly informs Y's
shape"). External gates (issues outside the narrow review series) are tagged `EXT`.

```
LANDED ANCHORS
  A.1 (#1097)  ───┐
  B.1 (#1096)  ───┤
  B.2 (#1102)  ───┤
  C.1 (#1095)  ───┘

NEW EDGES (source → consumer)

  A1.R1 ─── A1.R2, A1.R3, A1.C1-C5, A1.D1-D7, A3.IF, A4.PA
  A1.R2 ─── A1.C1-C5
  A1.D1-D7 ──── A1.L1-L6
  A1 (registry) ─── D2.SIDECAR (run.go), D2.CTR (run_args.go), D2.SWITCH (switch_session.go)

  A2.S1, A2.S2, A2.E1 ─── (independent, anytime)
  A2.M1 ──── D1.RUN, D1.BWRAP        (mount unification first lets D.1 cleanly decompose what remains)
  EXT #1017 ─── A2.G1, A2.M1         (sandbox-exec staging is the gate)
  EXT #1018 ─── A2.SUP, A2.GR, A3.IF (lifecycle hardening / sandbox-exec parallel cap impl)

  A3.IF ──── A3.DD
  A3.DB ──── A3.IF
  A4.PA ──── A4.PB, A4.PC, A4.PE
  A4.PB ──── A4.PE
  A3.IF ─── A4.PE   (coordinate; either order works, prefer A4.PE first)

  B.2 ─── B3.S1
  B3.S1 ─── B3.S2 ─── B3.S3 ─── B3.S4
  B3.S3 ─── B5.TR, B6.PI       (need real PI traces / PI on-disk shape)
  B6.IF ─── B6.WIRE ─── B6.PI

  C.1 ─── C2.CLI, C3.OUT, C4.SK, C4.AP
  C2.CLI ─── C2.CFG, C2.PLB, C2.PSP
  C2.PLB ─── C2.PROXY, C2.REV
  C3.OUT ─── C3.CMP, C3.STATS
  C2.PSP ─── C3.STATS

  D1.OPEN ─── (independent)
  D1.HOST ─── (wait for B3.S3 final route surface)
  D1.RUN ─── A1+A2 (content moves into podmanIsolator first)
  D1.BWRAP ─── A2.M1 (mount unification first)
  D2.DB ─── (independent — pairs nicely with D1.OPEN)
  D2.STATS ─── (independent)
  D2.REVIEW ─── (independent)
  D2.CHECKIN ─── (independent)
  D2.SIDECAR ──── (parts independent; run.go/host_api.go wait for B.3)
  D2.CTR ──── (parts independent; run_args.go waits for A.1/A.2)
  D2.SWITCH ──── (parts independent; switch_session.go waits for A.1)
```

A more compact rendering of just the "blocking" edges (A→B means A must land first):

```
       ┌──────── G1 (cheap podman removal) ─────────┐
       │                                             │
       │  A1.R1─R3 ──┬─→ A1.C1-C5 ──┬─→ A1.D1-D7 ──→ A1.L1-L6
       │            │               └─→ A3.DB ─→ A3.IF ─→ A3.DD
       │            └─→ A4.PA ─→ A4.PB ─→ A4.PC, A4.PE
       │                                             │
       │ EXT #1017 ─→ A2.G1, A2.M1 ─→ D1.RUN, D1.BWRAP
       │ EXT #1018 ─→ A2.SUP, A2.GR
       │                                             │
       └─────────────────────────────────────────────┘

       ┌──────── G2 (multi-harness PI) ─────────┐
       │                                         │
       │ B.2 ─→ B3.S1 ─→ B3.S2 ─→ B3.S3 ─→ B3.S4
       │                          │   └─→ B5.TR
       │                          └─→ B6.PI
       │ B6.IF ─→ B6.WIRE ─→ B6.PI               │
       │                                         │
       └─────────────────────────────────────────┘

       ┌──────── G3 (A/B testing) ──────────┐
       │                                     │
       │ C.1 ─→ C2.CLI ─→ C2.PLB ─→ C2.PROXY, C2.REV
       │       └─→ C2.CFG, C2.PSP
       │ C.1 ─→ C3.OUT ─→ C3.CMP, C3.STATS
       │ C.1 ─→ C4.SK, C4.AP, C4.PT, C4.SRC
       │                                     │
       └─────────────────────────────────────┘

       ┌──────── G0 (cleanup, no goal) ────────────────┐
       │ D1.OPEN, D2.DB, D2.STATS, D2.REVIEW, D2.CHECKIN
       │ A1.S1, A2.S1, A2.S2, A2.E1, B6.LSF
       └────────────────────────────────────────────────┘
```

## 4. Conflicts and resolutions

The proposals are mostly complementary, but several places have either explicit
conflict or evolution-of-decision. Each is recorded explicitly per the AC.

### 4.1 C.3 supersedes C.1's `sessions.outcome_summary` JSON column

**Conflict:** C.1 (landed) reserved `sessions.outcome_summary TEXT` as a JSON hook for
C.3 (C.1 §4.2). C.3 (this synthesis) explicitly replaces that column with a separate
`spawn_outcome` table keyed on `instance_id` (C.3 §4).

**Resolution: C.3's `spawn_outcome` table wins.** C.3 §4 argues this in detail: the
table form gives indexable typed columns, makes the comparison view in C.3 §6 a clean
two-table join, and matches C.1's symmetry choice for `spawn_inputs`. C.1 §4.6 already
flagged this column as "C.3 owns this; placeholder so C.3 doesn't need to ship a
migration alongside their proposals" — so C.3 superseding it is exactly the contract
C.1 envisioned. The implementation issue (filed below) drops the column and adds the
table; no data migration is needed because the column has zero writers today.

### 4.2 B.5 Translate vs C.1's `agent_events.normalised_payload` column

**Tension:** C.1 reserved `agent_events.normalised_payload TEXT` (C.1 §4.3) "to support
harness-normalised projections without rewriting payloads", deferring to B.5 for the
projection schema. B.5 (this series) recommends Translate-first, in which the PI adapter
writes opencode-shaped JSON directly into `payload` — meaning **the column has no
populator** under the recommended path.

**Resolution: leave the column reserved but unpopulated.** Recorded as **B5.NORM** in
the matrix. The column is non-blocking (NULLable, additive). If a future proposal
flips to Widen (per-axis, per B.5 §6 step 5) or Version, the column is already there.
Until then, comparison queries hit `payload` directly (which is opencode-shaped under
Translate, regardless of the writing harness).

### 4.3 A.3 ordering vs #1018

**Tension:** A.3 unifies the per-mode concurrency cap into `Isolator.Cap()`. #1018
(open, external) adds the sandbox-exec concurrency cap in parallel-implementation form
("symmetric to `checkBwrapConcurrencyCap`").

**Resolution (A.3's own recommendation, ratified here):** **#1018 ships first with a
parallel sandbox-exec cap; A.3 then refactors all three modes onto `Isolator.Cap()` in
a follow-up.** A.3 §6 argues this in detail: #1018's broader scope (kqueue lifecycle
hardening) shouldn't block on the A.3 refactor; #1018 explicitly permits both shapes;
the "wasted" parallel sandbox-exec function is one diff entry to delete after A.3
lands. The roadmap reflects this — A.3 is sequenced after the natural #1018 landing.

### 4.4 A.4 phase B vs A.4 phase E vs A.3 — DB read-side coordination

**Tension:** A.4 phase E removes `Status.EffectiveIsolationMode()`. A.3's per-mode cap
implementation also reads `Status.IsolationMode` (post-fallback). If A.3 lands first and
acquires a dependency on the deprecated reader, A.4 phase E's removal becomes harder.

**Resolution (A.4 §5 phase E and A.3 §6 both already note this):** preferred ordering
is **A.4 phase E first**, then A.3 against the canonical `isolation_mode` column.
A.3's implementation should explicitly read `Status.IsolationMode` (not `EffectiveIsolationMode()`)
so that A.4 phase E is forward-compatible. The roadmap respects this ordering; if a
worker accidentally lands A.3 first, A.4 phase E inherits a tiny extra step (rewrite
A.3's Status reader to drop the fallback) — non-fatal.

### 4.5 D.1 buildRunArgs decomposition vs A.1 podmanIsolator extraction

**Tension:** D.1 §4 proposes decomposing `(*Manager).buildRunArgs` (555 LoC) into
eight `appendXxx` helpers. A.1 §3.3 proposes **moving** much of `buildRunArgs` body
into `podmanIsolator` as part of the registry migration (A.1 phase 4 L.5).

**Resolution: A.1 wins; D.1.RUN as written is partly subsumed.** D.1's own §4
Independence flag explicitly says "Wait for A.2 (bwrap/sandbox-exec/podman duplication
audit)." Once A.1's L.5 lands, much of `buildRunArgs` lives inside `podmanIsolator`
and the remaining shrunk function is a thin coordinator. The D.1 helpers are still
useful as the *decomposition shape* of `podmanIsolator.Create`'s body, just under a
new home. D.1.RUN's value is preserved; only its location moves.

### 4.6 D.1 hostAPIHandler decomposition vs B.3 sidecar lifecycle reshape

**Tension:** D.1 §2 proposes decomposing `hostAPIHandler` (1054 LoC). B.3 §"Code
regions that change under Shape A" notes that the sidecar's `Run` block reshapes for
stdio harnesses, which may touch the host-API mounting code.

**Resolution:** D.1's own §2 Independence flag says "Wait for B.3" for the parts of
the decomposition tied to lifecycle. The `buildCheckinTimeline` extraction is safe
standalone. **Defer hostAPIHandler decomposition until B.3 stage 1 lands** (B3.S1);
the route-grouping shape is robust to B.3 outcomes but the relative timing avoids
churn. The roadmap places D1.HOST after B3.S1.

### 4.7 No deeper conflicts identified

The remaining proposals are complementary or mutually independent. Where two
proposals touch overlapping code (e.g. A.1's `WriteHarnessConfigBlob` and A.2's
mount unification both touch the same on-disk-write seam), the proposals already
note the coordination and assign clear ownership.

## 5. Prioritised roadmap

The roadmap is **opinion-bearing**. Recommendations are ordered by how much they
advance the three long-term goals weighted by independence and cost. Within
each tier, items are listed in the order a worker should pick them up; the
rationale is given per row.

### 5.1 Tier 1 — file as implementation issues now (top priorities)

These are the highest-leverage next steps. They are **goal-aligned**, **mostly
independent**, and **shippable in a small number of PRs each**. The first three
of these are filed as implementation issues by this synthesis pass (per the AC).

| Order | Code | Why it leads |
|---|---|---|
| **1** | **A1.R1+R2+R3** — registry skeleton + Capabilities + Resolve | Foundation for **G1**. Unlocks A.3 (cap unification), A.4 phases B/C/E (back-compat removal), and most of A.1's downstream phases. Smallest possible prerequisite that everything in track A's implementation column depends on. The proposal is detailed; the implementation is largely mechanical (interface declaration, registry singleton, three stub isolators). One PR realistic; two PRs comfortable. **Filed below as issue #1.** |
| **2** | **B3.S1** — stdio-harness-aware sidecar startup behind a transport-shape gate | Foundation for **G2**. The Run-loop transport-shape gate is the seam every subsequent stdio-harness step lands on. Existing harnesses (opencode HTTP) are unaffected because the gate selects today's path for `TransportHTTPPort`. Critically, it can land *before* PI exists and validates. **Filed below as issue #2.** |
| **3** | **C2.CLI + C2.PSP + C2.PLB** — per-role model variation end-to-end | Direct **G3** delivery. The first user-facing A/B feature, depending only on landed C.1. `--model-override role=model` flag, persistence into `spawn_inputs.model_variant_overrides`, plumbing through opencode adapter's per-role map. Self-contained scope; high user value. **Filed below as issue #3.** |
| 4 | **A4.PA** — backfill `isolation_mode` SQL migration | Smallest possible A.4 step; unlocks the rest of A.4 (deprecation removal). Pairs naturally with item 1 above (after registry exists, the back-compat shims can drain). |
| 5 | **C3.OUT + C3.CMP** — `spawn_outcome` table + `prism stats compare` | Direct G3 delivery; depends only on landed C.1 (and supersedes one column there per §4.1). Makes A/B comparison a concrete user-facing query. |
| 6 | **C4.SK + C4.AP** — skills + agent-prompt manifest hashes | Direct G3 delivery; small additive columns; unblocks honest A/B attribution of "skills set delta" and "agent prompt content delta" between runs. |

### 5.2 Tier 2 — file later, in roadmap order

These are the next-priority follow-ups once Tier 1 lands. They preserve goal
alignment but are larger or more dependent.

| Order | Code | Notes |
|---|---|---|
| 7 | **A1.C1-C5** | Capability-read migrations. Mechanical, low-risk, easy to spread across small PRs. |
| 8 | **A1.D1-D7** | Single-method dispatch migrations. Each item lands as its own small PR. |
| 9 | **A4.PB + A4.PC** | Back-compat shim removals. Unblocks reading `Status.IsolationMode` directly. |
| 10 | **A3.IF + A3.DB + A3.DD** | After #1018 ships and after item 9 (A4.PB), unify the three concurrency caps. |
| 11 | **A4.PE** | Drop `host_mode` column; coordinate with item 10. |
| 12 | **A2.S1 + A2.S2 + A2.E1** | Small standalone helper extractions (tempPath, HarnessInvocation, AppendStandardEnv). Anytime. |
| 13 | **A1.L1-L6** | Lifecycle migrations (EnsureRemoved, WriteGitconfig, AgentRun, etc.). Larger PRs; benefits from items 7-8 having landed. |
| 14 | **B3.S2** | Fake-stdio-harness path verification under bwrap. |
| 15 | **B3.S3** | PI integration on podman. Unlocks B5.TR validation and B6.PI. |
| 16 | **B6.IF + B6.WIRE** | ArchiveAdapter interface and opencode adapter; can land alongside / before B3.S3. |
| 17 | **B5.TR + B6.PI** | After B3.S3, the PI adapter side: payload schema (Translate) and archive adapter PI implementation. |
| 18 | **B3.S4** | PI integration on bwrap/sandbox-exec/host. Conditional on PI's TUI bridging story. |
| 19 | **C2.CFG + C2.PROXY + C2.REV** | Per-role-variation extensions: profile-file shape, proxy-spawn parity, review fan-out support. |
| 20 | **C3.STATS** | Add `--group-by harness/profile/variant` axes to existing stats subcommands. |
| 21 | **C4.PT + C4.SRC** | `prompt_template_hash` for review fan-out; `prompt_source` capture across paths. |
| 22 | **A4.PD** | `--host-mode` flag deprecation cycle then drop (one-release wait). |
| 23 | **A2.G1 + A2.M1 + A2.SUP + A2.GR** | After #1017 / #1018 land, the biggest A.2 extractions: gitconfig sandbox-exec, MountSpec unification, SuperviseChild, gracefulShutdown. |

### 5.3 Tier 3 — cleanup, file as standalone issues anytime

These are pure G0 cleanup with no goal alignment. They are filed when convenient
and have no roadmap urgency. They pair well together as small batched PRs.

| Code | Notes |
|---|---|
| D1.OPEN + D2.DB | Decompose `db.Open` and split `db.go` into per-concern files. Pair them. |
| D2.STATS + D2.REVIEW + D2.CHECKIN | File-size splits. Each is a single-PR effort. |
| D2.SIDECAR (parts) | Split `events.go`, `state.go`, `notify.go`, `dashboard.go`, `helpers.go` from `sidecar.go`. Defer `run.go`/`host_api.go` until B.3 stage 1 lands. |
| D2.CTR (parts) | Extract `lifecycle.go`, `credentials.go`. Defer `run_args.go` until A.1/A.2 reshape it. |
| D2.SWITCH (parts) | Extract TUI / project-discovery / dashboard parts. Defer `switch_session.go` until A.1 lands. |
| D1.HOST | Decompose `hostAPIHandler` into route-grouped sub-functions. Wait for B3.S1. |
| D1.RUN + D1.BWRAP | Subsumed in shape by A.1 L.5 + A.2 M1. Land them as the natural follow-up to those. |
| A1.S1 | `internal/sandboxenv` package. Anytime. |
| B6.LSF | Delete `internal/opencode.LatestSessionForDir` (currently unused by production code). One-line PR. |

### 5.4 Tier 4 — explicit deferrals (no follow-up issue)

These are the proposals' own explicit "defer" recommendations. They are not
filed and not roadmap'd; they have re-evaluation triggers documented in source.

- **B4.DEFER** — harness-group abstraction (`HTTPHarnessBase`, `StdioHarnessBase`).
  Re-evaluate when a second harness of each shape exists. (B.4 §7 lists the
  triggers explicitly.)
- **C3.RUB** — rubric-grading mechanism (the `prism rubric` command). Re-evaluate
  when C.4 lands (prompt persistence is the unblocker).
- **D3.NOOP** — D.3 produced no actionable work. Recorded for completeness;
  no issue to file.

### 5.5 Per-ordering rationale (high-level)

Reading the order top-to-bottom:

- **Items 1, 2, 3 lead** because each opens its track's full implementation lane
  with a small, self-contained, goal-aligned PR. Item 1 unlocks A.3, A.4, all of
  A.1's downstream — track A is wedged on it. Item 2 unlocks every subsequent
  PI-integration step. Item 3 is the first concrete user-facing A/B feature.
- **Items 4-6 round out tier 1** because each is small, independent, and
  goal-aligned. They are filed-or-not depending on coordinator capacity, but
  every one of them is shippable.
- **Tier 2 ordering** respects the dependency graph. Items 7-8 are A.1's
  capability and dispatch migrations, naturally following item 1. Item 9 (A.4
  phases B/C) unblocks the cap unification in item 10, which itself depends on
  external #1018. Items 14-18 form B.3's staged train, gated on PI prototyping
  at item 15. Items 19-21 are C track follow-ups.
- **Tier 3** is cleanup with no goal alignment. The pairings (D1.OPEN+D2.DB)
  exist because the same author touching `db.go` should do both at once.
- **Tier 4** records the explicit deferrals so they don't appear as "missed
  work" later — they are the proposals' own conclusions that grouping/grading
  abstractions are premature today.

## 6. Implementation issues filed

Per the AC, **at minimum the top 3 recommendations are filed as separate,
self-contained GitHub implementation issues**. Each issue names its dependencies,
references the source proposal(s), and contains enough context for a worker to
action it without reading this synthesis document.

| # | Issue | Recommendation | Source | Tier |
|---|---|---|---|---|
| 1 | [#1120](https://github.com/prismatic-koi/nixos-config/issues/1120) | A1.R1 + A1.R2 + A1.R3 — IsolationRegistry skeleton + Capabilities + Resolve | [A1 §4.1, §4.2, §4.3, §7 Phase 1](A1-isolation-registry-shape.md) | 1 |
| 2 | [#1121](https://github.com/prismatic-koi/nixos-config/issues/1121) | B3.S1 — stdio-harness-aware sidecar startup, behind a transport-shape gate | [B3 §"Staged implementation suggestion" Stage 1, §"Position 3"](B3-sidecar-lifecycle-for-stdio-harnesses.md) | 1 |
| 3 | [#1122](https://github.com/prismatic-koi/nixos-config/issues/1122) | C2.CLI + C2.PSP + C2.PLB — per-role model variation end-to-end | [C2 §3.1, §3.3, §3.4, §4.4, §8](C2-per-role-model-variation.md) | 1 |

These three issues are the **minimum-three** filed by E.1 per the AC. Tier 2 and
Tier 3 follow-ups remain unfiled and will be picked up by the coordinator as
implementation capacity allows; the per-recommendation context in §2 plus the
source proposals provide everything a future filing pass needs.

## 7. Summary

- **§2** — Cross-proposal recommendation matrix. Every recommendation from every
  A.x, B.x, C.x, D.x proposal categorised across goal alignment, sequencing, cost,
  risk, independence. D3.NOOP records D.3's "no action" outcome explicitly.
- **§3** — Cross-proposal dependency graph. ASCII edge list; the foundational
  edges are `A1.R1 → everything in track A`, `B.2 → B3.S1 → B3.S* → B5/B6 PI bits`,
  `C.1 → all of C.2/C.3/C.4`. External gates (#1017, #1018) are tagged.
- **§4** — Conflicts and resolutions. Seven discussed: C.3-supersedes-C.1 column;
  B.5-Translate-vs-C.1-normalised_payload; A.3-vs-#1018 ordering; A.3-vs-A.4
  read-side coordination; D.1.RUN-subsumed-by-A.1.L.5; D.1.HOST-defer-for-B.3.S1;
  no deeper conflicts.
- **§5** — Prioritised roadmap, four tiers. Tier 1 is the next-action set
  (six items, top three filed as issues by this PR). Tier 2 is the follow-up
  train. Tier 3 is independent G0 cleanup. Tier 4 is explicit deferrals.
- **§6** — Issues filed. Three minimum per AC.

The synthesis is **opinion-bearing**: it picks A.1 registry skeleton + B.3 stage-1
gate + C.2 per-role models as the three highest-leverage next moves, defends the
ordering against the dependency graph, and resolves all surface conflicts in
favour of the more recent proposals where evolution has occurred (C.3 supersedes
C.1's `outcome_summary` JSON column; B.5 Translate makes C.1's `normalised_payload`
column reserved-but-unpopulated). Where proposals' own self-recommendations
are clear (B.4 defer, C.3 rubric defer, D.3 no action, A.3 ships after #1018),
this synthesis ratifies them rather than re-litigating.

## Related

- Design doc:
  [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: [#1090](https://github.com/prismatic-koi/nixos-config/issues/1090).
  Parent design: [#1072](https://github.com/prismatic-koi/nixos-config/issues/1072).
- Source proposals:
  [A1](A1-isolation-registry-shape.md),
  [A2](A2-isolation-implementation-duplication-audit.md),
  [A3](A3-concurrency-cap-unification.md),
  [A4](A4-deprecation-surface-review.md),
  [B1](B1-harness-transport-and-lifecycle-assumptions.md),
  [B2](B2-harness-registry-and-transport-shape.md),
  [B3](B3-sidecar-lifecycle-for-stdio-harnesses.md),
  [B4](B4-harness-group-abstraction.md),
  [B5](B5-payload-schema-decoupling.md),
  [B6](B6-archive-and-export-pipeline-decoupling.md),
  [C1](C1-ab-readiness-and-schema-delta.md),
  [C2](C2-per-role-model-variation.md),
  [C3](C3-outcome-capture.md),
  [C4](C4-prompt-and-skills-as-spawn-inputs.md),
  [D1](D1-function-size-hot-spots.md),
  [D2](D2-file-size-hot-spots.md),
  [D3](D3-test-naming-audit.md).
- External gates: #1012, #1017, #1018 (sandbox-exec parity track — gates podman-as-mode removal).
- RFCs: #691 (multi-harness support), #606 (PI coding agent support).
