# Design — narrow architecture-review series (post-inventory)

## Summary

Following the architecture inventory landed in #1071 (closed #1065), this design defines a narrow architecture-review series that turns the inventory's pressure-points into actionable, parallelisable review work. Each child issue produces a single focused proposal document; no code changes ship from any of them. Implementation issues will be filed as a follow-up wave once the proposals land and the synthesis pass (Track E) prioritises them.

This document is the **planning artefact** for the review series. It captures the design from issue #1072 verbatim, plus an index of the child issues that have been filed to action it. All reviews described here are zero-functionality-impact; the downstream implementation work they motivate is **additive** — no existing flow regresses.

## Background and motivation

The inventory (`modules/programs/prism/prism/docs/architecture-inventory.md`) made three pressure-points unmistakable:

1. **`cmd → internal/config:IsolationBwrap` is the single most-frequent cross-package coupling** (27 occurrences). Mode dispatch is currently spread across 17+ files; adding or removing an isolation mode is a wide change. The stated long-term direction is to keep bwrap and sandbox-exec as the primary modes (Linux and Darwin) and **deprecate podman once sandbox-exec reaches bwrap parity**. Removal must be cheap. Today it is not.
2. **The `Harness` interface exists but has only ever been exercised by one implementation (opencode).** PI is the imminent second harness (#691). Crucially, opencode and PI use *different transport shapes*: **opencode is HTTP+SSE, PI is JSONL-over-stdin/stdout RPC**. The interface and the surrounding sidecar machinery have implicit HTTP-server-shaped assumptions baked in. These have not been stress-tested against a non-HTTP harness yet — we are planning in advance so PI integration does not require structural surgery. Track B is designed to surface and address these implicit assumptions.
3. **Spawn-time inputs (`--profile`, `--variant`, `--model`, prompt template, skills set) are not persisted** — only the resolved `model_id` is. There is currently no way to do `compare run-A vs run-B` without reconstructing intent from logs. A/B testing as a first-class workflow (per #691 §5) requires structured input persistence and outcome capture.

These three concerns share a structural root: prism needs **fine-grained variation at the per-spawn level along the axes of harness × isolation × model × agent/role × prompt × skill, with structured outcome capture**. That is the unifying observation of the inventory's §1 framing and the lens the review series adopts.

## Stated long-term goals (recap)

- **Isolation:** bwrap (Linux) and sandbox-exec (Darwin) are the long-term primary modes. Podman stays in the tree until sandbox-exec parity is reached, then is removed in one surgical PR. **Any podman-deprecation work is gated on sandbox-exec parity, tracked by #1012, #1017, and #1018.** New modes (firejail, kvm, windows-sandbox, etc.) should require implementing one interface and registering, not surgery across the codebase.
- **Harness:** opencode and PI as parallel first-class harnesses. The harness abstraction must accommodate **different transport shapes** (HTTP+SSE vs JSONL-over-stdin/stdout) at the harness-or-harness-group level. Future harnesses (Claude Code, Codex) come for free if the shape is right.
- **A/B testing:** structured experimentation as a routine workflow. Per-role model variation. Persisted spawn inputs alongside resolved values. Comparison rendering. None of this exists today.

## Non-goals for this design

- No code changes from this issue or any child review issue. Implementation issues are filed as a follow-up wave.
- No removal of podman before sandbox-exec parity is confirmed (gated separately on #1012 / #1017 / #1018).
- No commitment to PI integration timing — the harness reviews produce proposals; #691's phased plan governs implementation.
- No prejudgement of harness-grouping shape (HTTPHarness vs StdioHarness shared base) — that is one of the questions a child review answers.

## Track structure

Five tracks. Tracks A, B, D run in parallel; Track C depends on B's results before its implementation phase becomes meaningful (its review phase can start in parallel); Track E synthesises after A/B/C/D land.

### Track A — Isolation: from four-modes-spread-everywhere to one-place dispatch

Pays down per-mode duplication so podman removal is a one-PR change when sandbox-exec parity is reached.

- **A.1 — Isolation coupling review and registry-shape proposal.** Reads inventory §6 cover-to-cover. Tags every site by whether it would survive in a registry-based world. Proposes an `Isolator` interface superset.
- **A.2 — Bwrap / sandbox-exec / podman duplication audit.** Reads `bwrap.go` (734 LoC) and `sandbox_exec.go` side-by-side; identifies what *must* differ vs what is incidentally different. Proposes shared helpers to extract.
- **A.3 — Per-mode concurrency-cap unification.** Today: parallel `checkConcurrencyCap` (podman) and `checkBwrapConcurrencyCap` (bwrap), no cap for sandbox-exec. Proposes one `Isolator.Cap()` shape. Notes dependency on / overlap with #1018.
- **A.4 — Deprecation surface review** (`ContainerMode bool`, `--host-mode` flag, `EffectiveIsolationMode` fallback). Costs and gating for deletion.

### Track B — Harness: from opencode-only-and-HTTP-shaped to genuinely pluggable

Pays down opencode-shape and HTTP-shape coupling so PI's stdin/stdout RPC model fits without surgery.

- **B.1 — Harness coupling review surfacing transport-and-lifecycle assumptions.** Reads inventory §7 cover-to-cover. Looks for **HTTP-server-shaped** assumptions (not just opencode-named assumptions): `WaitHealthy`, `OnReady`, `--port` flag, `HealthCheck(ctx, port int)` (note the `port` parameter — that's an HTTP assumption *inside the interface*), the host-API HTTP server, the SSE reconnection logic, the readiness-wait file. Output: a list of "this code lives in the wrong place if the harness uses pipes instead of ports".
- **B.2 — Harness construction site refactor proposal: registry plus transport-shape declaration.** Replaces today's hard-coded `if/else` adapter construction. The registry registers `(name, New func, transport shape)` where transport shape is one of `{http-port, stdio-pipe, fallback-screen-scrape}`. Container manager and agent-pane command consult the transport shape, not the harness name.
- **B.3 — Sidecar lifecycle review for stdio harnesses.** Today the sidecar is started by the spawn flow as a sibling process. For PI the sidecar would need to be the launcher of the PI process so it owns the pipe. Two viable shapes: (a) sidecar becomes parent of agent-run, (b) `prism agent-run` itself takes on sidecar duties for stdio harnesses. Review which has fewer side-effects on existing podman/bwrap/host paths.
- **B.4 — Harness *group* abstraction.** Articulates the question explicitly: do http-shaped harnesses (opencode, likely Claude Code) share enough machinery to warrant a shared base, separate from stdio-shaped harnesses (PI, likely Codex)? Or does that create the wrong abstraction prematurely? Answers — does not presuppose.
- **B.5 — Payload schema decoupling.** `internal/payload/payload.go` field names match opencode plugin output verbatim. When PI starts emitting events: translate to opencode's shape inside the PI adapter, widen the payload struct, or version the schema? Surfaces the trade-off explicitly.
- **B.6 — Archive-and-export pipeline decoupling.** `internal/archive/archive.go` and `internal/piexport/piexport.go` are opencode-shaped. Proposes a per-harness `ArchiveAdapter`.

### Track C — A/B testing: from spawn-time variation to structured experimentation

Reviews are zero-functionality-impact; downstream implementation is **additive** (no existing flow regresses).

- **C.1 — A/B-readiness review and schema delta.** What gets persisted vs what should be persisted. Includes the cross-harness dimension: which comparison metrics make sense across different event-stream shapes (universal: token cost, tool-call count, time-to-first-event, error rate, agent-state transitions; harness-specific: tool-call-result content correlation).
- **C.2 — Per-role model variation proposal.** Today `harness.Harness.EffectiveModel(role)` exists but the opencode adapter resolves a single model field at construction. Proposes a per-role override map (CLI surface, config-file surface, persistence surface).
- **C.3 — Outcome-capture review.** "Verdict" today exists for review groups (`db.GroupResults`) but not for ordinary spawns. Proposes `spawn_outcome` shape and the per-axis aggregations a comparison view would need.
- **C.4 — Prompt and skills as first-class spawn inputs.** Skills load via opencode at runtime; the Go side is unaware of them as a structured concept. Proposes capturing a skills manifest hash on the `sessions` row so an A/B comparison can attribute a behavioural difference to a skills delta.

### Track D — Cross-cutting cleanups (independent)

- **D.1 — Function-size hot-spots.** Decomposition strategy for `(*Sidecar).hostAPIHandler` (1054 LoC), `Open` (683), `(*Manager).buildRunArgs` (555), `(*bwrapIsolator).BuildArgs` (488).
- **D.2 — File-size hot-spots.** Split candidates for the seven files >2000 LoC.
- **D.3 — Test-naming audit.** Eleven packages have non-zero test LoC but zero `Test*` functions per the inventory's regex. Either non-canonical naming or regex error. One-PR clarification.

### Track E — Synthesis (after A/B/C/D land)

- **E.1 — Post-narrow-review synthesis and prioritisation.** Reads outputs of tracks A–D and produces a prioritised roadmap, separating "blocks podman removal", "blocks PI integration", "blocks A/B testing", and "incidental cleanup". This is the sequencing artefact that turns proposal documents into a small set of implementation issues.

## Sequencing

- **Wave 1 (parallel):** A.1, B.1, C.1 — the three anchor reads, one per track. Each one independently establishes the lens the rest of its track uses.
- **Wave 2 (parallel):** A.2, A.3, A.4, B.2, B.3, B.4, B.5, B.6, C.2, C.3, C.4 — once the anchors land. Many of these can run simultaneously; they touch different code regions.
- **Track D** can run any time, alongside Wave 1 or Wave 2. D.3 is tiny; D.1 and D.2 may be obviated by Track A/B structural changes, so prefer to file but defer those until after Wave 1 lands.
- **Track E** waits for A, B, C, and D.

### Inter-track dependencies

- Track C's review phase can run in parallel with Track B, but its implementation phase depends on Track B's transport-shape and payload-schema outcomes — comparison metrics and outcome capture must accommodate both HTTP+SSE and stdio-pipe event streams.
- Track A's podman-deprecation surface review (A.4) is informational only here; the actual deprecation is gated externally on the sandbox-exec parity track (#1012, #1017, #1018).
- Track A.3 (concurrency-cap unification) overlaps with #1018 and should reference it.
- Track E is the sole consumer of A, B, C, and D outputs; nothing in Wave 1 or Wave 2 depends on E.

## Worker shape per child issue

Each child issue is a *proposal* issue, not an implementation issue. The deliverable is a markdown document under `modules/programs/prism/prism/docs/reviews/<NNN>-<slug>.md`, opinion-bearing this time, with concrete file:line citations from the inventory. The narrative tone is the worker's call; the structure is per-issue but typically: context recap, current-state observations with citations, proposal(s) with rationale, open questions, dependencies on or relationships with other proposals.

## Child issues

The following child issues have been filed to action this design:

| Track | Scope | Issue |
|---|---|---|
| A.1 | Isolation registry shape | #1073 |
| A.2 | Isolation duplication audit | #1077 |
| A.3 | Concurrency cap unification | #1078 |
| A.4 | Deprecation surface | #1079 |
| B.1 | Harness transport-and-lifecycle assumptions | #1074 |
| B.2 | Harness registry + transport-shape | #1080 |
| B.3 | Sidecar lifecycle for stdio harnesses | #1081 |
| B.4 | Harness group abstraction | #1082 |
| B.5 | Payload schema decoupling | #1083 |
| B.6 | Archive pipeline decoupling | #1084 |
| C.1 | A/B-readiness schema delta | #1075 |
| C.2 | Per-role model variation | #1085 |
| C.3 | Outcome capture | #1086 |
| C.4 | Prompt and skills as spawn inputs | #1087 |
| D.1 | Function-size hot-spots | #1088 |
| D.2 | File-size hot-spots | #1089 |
| D.3 | Test-naming audit | #1076 |
| E.1 | Synthesis & prioritisation | #1090 |

## Out of scope

- No code changes.
- No proposals or recommendations beyond what is summarised here — those live in the child issues.
- No re-reading of inventory contents to add new pressure-points; this design is the curation.

## Related

- #1065 (closed) / #1071 (merged) — the architecture inventory.
- #1072 — this design issue.
- #691 — RFC: multi-harness support.
- #606 — RFC: PI coding agent support.
- #1012 / #1017 / #1018 — sandbox-exec parity track (gates podman deprecation).
- #783 — merge queue (referenced for incidental context only).
