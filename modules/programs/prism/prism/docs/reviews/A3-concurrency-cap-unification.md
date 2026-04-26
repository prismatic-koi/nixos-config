# A.3 — Per-mode concurrency-cap unification proposal

Status: proposal (no code changes).
Issue: #1078.
Track: A (isolation), Wave 2 follow-up to A.1 (#1073, landed in #1097).
Source corpus: `modules/programs/prism/prism/docs/architecture-inventory.md`
§6.15, plus the implementation files cited inline. This proposal is presented
as an addition to A.1's `Isolator` interface superset — the `Cap()` method
declared in A.1 §4.1 is the shape A.3 fleshes out and unifies the rendering
around.

## 1. Context

A.1 (`docs/reviews/A1-isolation-registry-shape.md`) proposed widening the
`Isolator` interface so every per-mode dispatch in the codebase collapses
into either an `Isolator` method call or a registry lookup. One of the
methods named in A.1 §4.1 is `Cap(ctx, dbPath) CapStatus` — declared, not
specified. **This document specifies it.**

Today there are two parallel cap implementations and one missing one:

- `cmd/concurrency.go:34-56` — `checkConcurrencyCap` (podman), guards the
  `DefaultConcurrencyCap = 6` ceiling, merges live `podman ps` output with
  DB-active rows.
- `cmd/concurrency.go:71-138` — `checkBwrapConcurrencyCap` (bwrap), guards
  the `Config.BwrapConcurrencyCap` ceiling (default 20 per
  `internal/config/config.go:153-158`), DB-only.
- Sandbox-exec — no cap, tracked in #1018 (open, pre-dates this review series).

Both existing checks answer the same question — "how many active sessions
of this isolation mode are running, and is that under the limit?" — but
they differ in:

1. **Source of truth.** Podman merges a live process probe (`podman ps`)
   with the DB; bwrap is DB-only.
2. **Cap source.** Podman uses a compiled-in constant
   (`container.DefaultConcurrencyCap`); bwrap reads `Config.BwrapConcurrencyCap`
   at check time.
3. **Message rendering.** Podman renders via
   `container.FormatExceededError` / `FormatExceededWarning`
   (`internal/container/concurrency.go:178-201`); bwrap renders inline in
   `cmd/concurrency.go:99-138`.
4. **Role inference.** Podman uses `roleFor` (`internal/container/concurrency.go:42-53`);
   bwrap re-implements an equivalent block inline at
   `cmd/concurrency.go:106-115` and again at `:124-133`.

The cost of leaving this split is paid once per new isolation mode. Sandbox-exec
already needs its third parallel implementation per #1018's AC list; without
unification, a fourth mode (firejail, kvm, windows-sandbox) inherits the same
shape and the same duplicate rendering.

## 2. Side-by-side comparison of the current implementations

The two checks are structurally similar but differ in every detail. Each row
below pairs a podman site with the equivalent bwrap site so the divergence
is visible at a glance.

| Concern | Podman (`checkConcurrencyCap`) | Bwrap (`checkBwrapConcurrencyCap`) |
|---|---|---|
| Entry signature | `checkConcurrencyCap(cmd *cobra.Command, callerName string, conCapped bool) error` (`cmd/concurrency.go:34`) | `checkBwrapConcurrencyCap(cmd *cobra.Command, callerName string) error` (`cmd/concurrency.go:71`) |
| Mode predicate at call site | Caller passes `conCapped` boolean derived from `isolationMode == config.IsolationPodman` (`cmd/spawn.go:290`, `cmd/pr.go:82`, `cmd/review.go:179`) | Caller wraps in `if isolationMode == config.IsolationBwrap { … }` (`cmd/spawn.go:294-298`, `cmd/pr.go:86-89`, `cmd/review.go:183-186`) |
| Cap value | Compiled-in constant `container.DefaultConcurrencyCap = 6` (`internal/container/concurrency.go:24-30`) | Runtime `config.Load().BwrapConcurrencyCap`, default `DefaultBwrapConcurrencyCap = 20` (`internal/config/config.go:112-117`, `:153-158`) |
| Uncapped sentinel | None (cap is always 6) | `cap == 0` short-circuits to `nil` (`cmd/concurrency.go:73-76`) |
| Source of truth | Union of: DB rows where `ended_at IS NULL` (`AllActiveStatus`, `internal/db/db.go:1710-1717`) + live `podman ps --format {{.Names}}` (`internal/container/concurrency.go:131-148`); deduplicated by container name (`internal/container/concurrency.go:109-126`) | DB-only: `ActiveBwrapSessionCount` filtering on `isolation_mode = 'bwrap'` (`internal/db/db.go:1719-1731`); detail list via `ActiveBwrapSessions` (`internal/db/db.go:1733-1742`) |
| Live-probe failure handling | `runPodmanPS` returns `(nil, false)`; check falls back to DB-only and sets `CheckResult.PodmanFailed = true`; caller writes a "podman ps failed — using DB-only count" warning (`cmd/concurrency.go:42-44`) | N/A — no live probe to fail. DB-open failure is treated as non-fatal: warning to stderr and `return nil` (`cmd/concurrency.go:78-83`) |
| In-flight session shape | `container.InFlightSession{Name, Role}` (`internal/container/concurrency.go:32-40`); `Role` derived by `roleFor` from `root_agent_name` or `@main` heuristic (`:42-53`) | `db.Status` row (full schema), with role inferred inline by checking `RootAgentName` then `@main` heuristic (`cmd/concurrency.go:106-115`, `:124-133`) — duplicates `roleFor` logic in two places |
| Cap-exceeded predicate | `Count >= Cap` (`internal/container/concurrency.go:172`) — note `>=`, so cap 6 means the 6th attempt is refused | `count < cap` returns nil; `count >= cap` triggers the exceeded path (`cmd/concurrency.go:93-95`) — same semantics |
| Error message | `container.FormatExceededError(res)` returns `"error: prism concurrency cap reached (%d agent containers already in flight)\n\nActive containers:\n  …\n\nHint: …"` (`internal/container/concurrency.go:178-189`) | Inline `strings.Builder` returns `"error: prism bwrap concurrency cap reached (%d bwrap sessions already in flight)\n\nActive bwrap sessions:\n  …\n\nHint: …"` (`cmd/concurrency.go:121-138`) — same structure, different nouns |
| Warning message (`--ignore-concurrency-cap`) | `container.FormatExceededWarning(res)` returns `"[prism] warning: concurrency cap exceeded (%d/%d in-flight containers) — proceeding because --ignore-concurrency-cap was passed\n[prism] in-flight containers:\n[prism]   …"` (`internal/container/concurrency.go:191-201`) | Inline `strings.Builder` returns `"[prism] warning: bwrap concurrency cap exceeded (%d/%d bwrap sessions in flight) — proceeding because --ignore-concurrency-cap was passed\n[prism] active bwrap sessions:\n[prism]   …"` (`cmd/concurrency.go:101-118`) — same structure, different nouns |
| Test seam | `container.CheckCap(dbPath, cap, podmanPS func() ([]string, bool))` accepts a `podmanPS` injection point (`internal/container/concurrency.go:164-176`) | None — bwrap path goes straight to `db.Open(dbPath())` (`cmd/concurrency.go:78`); test coverage relies on a real DB fixture |
| Hint footer | `"Hint: wait for a worker to finish and be cleaned up, or re-run with\n      --ignore-concurrency-cap to bypass this guard."` (`internal/container/concurrency.go:186-187`) | Same hint, duplicated verbatim (`cmd/concurrency.go:136-137`) |
| Caller invocation pattern | `if err := checkConcurrencyCap(cmd, "spawn", conCapped); err != nil { return err }` — boolean parameter, single call regardless of mode | `if isolationMode == config.IsolationBwrap { if err := checkBwrapConcurrencyCap(cmd, "spawn"); err != nil { return err } }` — outer mode branch |

The two implementations agree on:

- The cap is a *soft* guard — `--ignore-concurrency-cap` always lets the
  caller through with a warning.
- The check must run BEFORE any side effects (worktree, DB row, tmux session).
- The role-inference rules: prefer `root_agent_name`, otherwise apply the
  `@main` heuristic to label coordinators, otherwise `"unknown"`.
- The warning/error message shape (cap value, in-flight count, listing of
  active sessions, footer hint).
- The test-versus-prod separation of concerns is at least *intended* the same
  way — both checks short-circuit on transient failures (DB open error,
  podman ps failure) rather than blocking spawn.

The two implementations disagree on:

- Whether the live process probe is part of the source of truth.
- Whether the cap value is configurable (bwrap is, podman is not).
- Whether the formatter is exported and shared (podman yes, bwrap no).
- Where the role-inference helper lives (`container.roleFor` vs inline
  duplication twice in `cmd/concurrency.go`).

## 3. Sandbox-exec gap analysis

Sandbox-exec has **no concurrency cap today**. The `cmd/concurrency.go`
file does not reference it; `cmd/spawn.go:290-298`, `cmd/pr.go:82-89`, and
`cmd/review.go:179-186` all skip the cap path when
`isolationMode == config.IsolationSandboxExec`. The `internal/db/db.go`
package has no `ActiveSandboxExecSessionCount` analog of the bwrap helpers
at `:1719-1742`. The `Config` struct at `internal/config/config.go:57-123`
has no `SandboxExecConcurrencyCap` field.

This is the gap #1018 ("prism: sandbox-exec concurrency cap and lifecycle
hardening") closes. Per #1018's "Concurrency cap" section, the planned
shape is:

- `db.ActiveSandboxExecSessionCount() (int, error)` — DB-only, filtering
  on `isolation_mode = 'sandbox-exec'`. Direct symmetry with
  `ActiveBwrapSessionCount` at `internal/db/db.go:1719-1731`.
- `db.ActiveSandboxExecSessions() ([]Status, error)` — DB-only, full row
  list for the error message. Direct symmetry with `ActiveBwrapSessions`
  at `internal/db/db.go:1733-1742`.
- `Config.SandboxExecConcurrencyCap` field with
  `DefaultSandboxExecConcurrencyCap = 20` constant. Direct symmetry with
  `BwrapConcurrencyCap` and `DefaultBwrapConcurrencyCap` at
  `internal/config/config.go:112-117` and `:153-158`.
- `cmd/concurrency.go:checkSandboxExecConcurrencyCap` — explicitly named
  in #1018's body as "symmetric to `checkBwrapConcurrencyCap`. If
  extracting a generic helper feels cleaner, do it — both shapes are
  acceptable."

That last clause is the relevant one for A.3. **#1018 has explicitly
permitted the generic-helper shape this proposal advocates** — the
implementer's choice is between adding a third parallel function or
adopting A.3's `Isolator.Cap()` shape if it has landed by the time #1018
is implemented. See §6 for the sequencing recommendation.

The natural source-of-truth for sandbox-exec is **DB-only**, mirroring
bwrap, because there is no equivalent of `podman ps` for sandbox-exec
processes — they appear as ordinary host processes, not as named, tagged
container objects. **[uncertain]** A future refinement could add a
`pgrep`-style scan for `agent-run --sandbox-exec` processes to give
sandbox-exec a live-probe component analogous to podman's, but this is
out of scope for both #1018 and A.3 — the DB-only shape is sufficient for
the immediate cap requirement, and the `Cap()` interface declared below
leaves room for a per-isolator implementation to add a live probe later
without changing the interface.

## 4. Proposed `Isolator.Cap()` shape

This section specifies the `Cap()` method that A.1 §4.1 declared. It slots
into A.1's interface superset between `Available()` and
`WriteHarnessConfigBlob()`; no other interface method changes.

### 4.1 The `Cap` method on `Isolator`

```go
// Cap returns the soft concurrency-cap descriptor for this isolator.
// It is the unified replacement for cmd/concurrency.go's parallel
// checkConcurrencyCap (podman) and checkBwrapConcurrencyCap (bwrap)
// functions, and the natural home for sandbox-exec's cap once #1018 lands.
//
// dbPath is the path to prism.db; the implementation may use it to count
// active sessions of this isolation mode, may merge with a live process
// probe (podman), or may ignore it entirely (host: uncapped).
//
// The returned CapStatus carries the count, limit, exceeded flag, the
// in-flight session list (for inclusion in the user-facing message), and
// any per-mode probe-failure context. The caller uses CapStatus.Render(...)
// to produce the warning/error string — rendering lives on CapStatus, not
// in cmd/concurrency.go, so all isolators emit the same message shape.
//
// Implementations must NOT have side effects: Cap is called speculatively
// from cmd/spawn.go, cmd/pr.go, and cmd/review.go before any worktree, DB
// row, or tmux session is created.
//
// Cites:
//   - cmd/concurrency.go:34-56 (podman checkConcurrencyCap)
//   - cmd/concurrency.go:71-138 (bwrap checkBwrapConcurrencyCap)
//   - internal/container/concurrency.go:131-160 (CheckCap)
//   - internal/db/db.go:1719-1742 (ActiveBwrapSessionCount/Sessions)
//   - #1018 (sandbox-exec gap)
Cap(ctx context.Context, dbPath string) CapStatus
```

### 4.2 The `CapStatus` value type

```go
// CapStatus is the outcome of an Isolator.Cap probe. It generalises
// container.CheckResult (internal/container/concurrency.go:150-162) over all
// isolation modes; CheckResult will be deleted in favour of CapStatus.
type CapStatus struct {
    // Mode is the isolation mode that produced this status. Used by Render
    // for the noun in messages ("agent containers" vs "bwrap sessions" vs
    // "sandbox-exec sessions"). Implementations set this to their own
    // Isolator.Name() value.
    Mode config.IsolationMode

    // Limit is the configured cap. Zero means uncapped (no Cap call should
    // produce Exceeded == true when Limit == 0). For podman: 6 today
    // (DefaultConcurrencyCap). For bwrap: Config.BwrapConcurrencyCap.
    // For sandbox-exec (per #1018): Config.SandboxExecConcurrencyCap.
    // For host: 0.
    Limit int

    // Count is the number of in-flight sessions of this mode at probe time.
    Count int

    // Exceeded is true when Count >= Limit and Limit > 0. False when
    // Limit == 0 regardless of Count.
    Exceeded bool

    // InFlight is the per-session detail list rendered into the
    // warning/error message. May be empty if the live probe failed and the
    // implementation chose not to enumerate; in that case Note carries the
    // explanatory message.
    InFlight []InFlightSession

    // Note carries any non-fatal context from the probe — e.g. the podman
    // implementation sets Note to "podman ps failed — using DB-only count"
    // when runPodmanPS returns false. The caller surfaces Note as a
    // separate stderr warning before applying the Exceeded check.
    // Empty when the probe ran cleanly.
    Note string
}

// InFlightSession is the per-session detail row rendered in the cap
// warning/error message. It generalises container.InFlightSession
// (internal/container/concurrency.go:32-40) and the inline session-rendering
// blocks at cmd/concurrency.go:106-115 and :124-133, and is shared across
// all isolators.
type InFlightSession struct {
    // Name is the prism session name (e.g. "nixos-config@feature").
    Name string
    // Role is the inferred role label ("coordinator", "worker", a
    // root-agent name, or "unknown"). Implementations should call the
    // shared roleFor helper (currently container.roleFor at
    // internal/container/concurrency.go:42-53; promoted to a package-level
    // helper alongside the new types).
    Role string
}
```

### 4.3 The `Render` methods

The two formatters (`FormatExceededError`, `FormatExceededWarning`) move
onto `CapStatus` so cmd/concurrency.go contains zero rendering logic.

```go
// RenderError returns the error string shown when Exceeded is true and
// --ignore-concurrency-cap was NOT passed. Symmetric across modes.
//
// Replaces:
//   - container.FormatExceededError (internal/container/concurrency.go:178-189)
//   - the inline strings.Builder block at cmd/concurrency.go:121-138
func (s CapStatus) RenderError() string

// RenderWarning returns the warning string shown when Exceeded is true and
// --ignore-concurrency-cap WAS passed. Symmetric across modes.
//
// Replaces:
//   - container.FormatExceededWarning (internal/container/concurrency.go:191-201)
//   - the inline strings.Builder block at cmd/concurrency.go:101-118
func (s CapStatus) RenderWarning() string
```

`RenderError` and `RenderWarning` both consult `s.Mode` to choose the
noun (`"agent containers"` for podman, `"bwrap sessions"` for bwrap,
`"sandbox-exec sessions"` for sandbox-exec, etc.). The hint footer is
unchanged: `"Hint: wait for a worker to finish and be cleaned up, or
re-run with --ignore-concurrency-cap to bypass this guard."`.

### 4.4 What `cmd/concurrency.go` looks like afterwards

After A.3 lands, `cmd/concurrency.go` is one ~30-LoC function — not two:

```go
// checkConcurrencyCap enforces the per-isolator soft concurrency cap.
// Returns a non-nil error when the cap is exceeded and ignoreCap is false.
// Writes a stderr warning and returns nil when the cap is exceeded and
// ignoreCap is true.
//
// callerName is used in stderr warnings ("[prism spawn] …", etc.).
//
// Replaces both checkConcurrencyCap and checkBwrapConcurrencyCap; called
// unconditionally for any isolation mode (no outer "if mode == X" guard
// at the call site). Modes that are uncapped (host today; sandbox-exec
// before #1018; podman if DefaultConcurrencyCap is ever changed to 0)
// produce CapStatus{Limit: 0} and short-circuit the Exceeded check.
func checkConcurrencyCap(cmd *cobra.Command, callerName string, iso container.Isolator) error
```

The three call sites (`cmd/spawn.go:290-298`, `cmd/pr.go:82-89`,
`cmd/review.go:179-186`) collapse from a per-mode `if`/`else` block to a
single line each:

```go
if err := checkConcurrencyCap(cmd, "spawn", iso); err != nil { return err }
```

## 5. Per-mode implementation sketches (signatures only)

Per the AC, signatures only — no method bodies. Each isolator owns its
own `Cap` implementation; there is no single shared body that branches on
mode internally. The shared *helpers* (DB queries, role inference,
rendering) live below the interface.

### 5.1 `podmanIsolator.Cap`

```go
// Cap probes the podman concurrency cap by merging DB-active sessions with
// live podman ps output. Sets Note when podman ps fails so the caller can
// emit the "podman ps failed — using DB-only count (may be imprecise)"
// warning currently emitted at cmd/concurrency.go:42-44.
//
// Wraps container.CheckCap (internal/container/concurrency.go:164-176) and
// container.ListInFlight (:55-129). Both helpers stay in
// internal/container/ as podmanIsolator-private detail; CheckCap's signature
// shrinks to private after the public CheckCap callers migrate.
//
// Limit is container.DefaultConcurrencyCap (6) until that value moves to
// Config.PodmanConcurrencyCap; that move is OUT OF SCOPE for A.3 and is
// tracked separately if/when needed.
func (p *podmanIsolator) Cap(ctx context.Context, dbPath string) CapStatus
```

### 5.2 `bwrapIsolator.Cap`

```go
// Cap probes the bwrap concurrency cap from the DB only. Reads the cap
// value from config.Load().BwrapConcurrencyCap; returns CapStatus{Limit: 0}
// when the cap is configured to 0 (uncapped sentinel matching today's
// behaviour at cmd/concurrency.go:73-76).
//
// Calls db.ActiveSessionCountForMode("bwrap") and
// db.ActiveSessionsForMode("bwrap") — the per-mode-parameterised helpers
// that supersede ActiveBwrapSessionCount and ActiveBwrapSessions (see §5.4).
func (b *bwrapIsolator) Cap(ctx context.Context, dbPath string) CapStatus
```

### 5.3 `sandboxExecIsolator.Cap`

```go
// Cap probes the sandbox-exec concurrency cap from the DB only. Reads the
// cap value from config.Load().SandboxExecConcurrencyCap; returns
// CapStatus{Limit: 0} when 0 (uncapped sentinel).
//
// Calls db.ActiveSessionCountForMode("sandbox-exec") and
// db.ActiveSessionsForMode("sandbox-exec"). The per-mode-parameterised
// helpers replace the bwrap-specific pair without growing a third parallel
// pair (see §5.4).
//
// [uncertain — once a live process probe for sandbox-exec is added (e.g.
// scanning for "prism agent-run --sandbox-exec" host processes), it merges
// here exactly the way podman ps merges into podmanIsolator.Cap. Until
// then, DB-only is sufficient and matches #1018's requested shape.]
func (s *sandboxExecIsolator) Cap(ctx context.Context, dbPath string) CapStatus
```

### 5.4 Supporting changes in `internal/db`

The bwrap-specific helpers at `internal/db/db.go:1719-1742` become
mode-parameterised so a third per-mode pair is not needed for sandbox-exec
or any future mode:

```go
// ActiveSessionCountForMode returns the number of agent_status rows where
// ended_at IS NULL AND isolation_mode = mode.
//
// Replaces ActiveBwrapSessionCount (internal/db/db.go:1719-1731). Callable
// for any IsolationMode value; returns 0 when no rows match.
func (d *DB) ActiveSessionCountForMode(mode config.IsolationMode) (int, error)

// ActiveSessionsForMode returns the agent_status rows where
// ended_at IS NULL AND isolation_mode = mode, suitable for cap-error
// detail listings.
//
// Replaces ActiveBwrapSessions (internal/db/db.go:1733-1742).
func (d *DB) ActiveSessionsForMode(mode config.IsolationMode) ([]Status, error)
```

`ActiveBwrapSessionCount` and `ActiveBwrapSessions` are removed once
`bwrapIsolator.Cap` is in place — there are no other callers
(`rg "ActiveBwrap" modules/programs/prism/prism/` confirms two call
sites, both inside `cmd/concurrency.go:checkBwrapConcurrencyCap`).

### 5.5 Supporting changes in `internal/container`

`container.CheckCap`, `container.ListInFlight`,
`container.FormatExceededError`, `container.FormatExceededWarning`, and
the `container.CheckResult` / `container.InFlightSession` types are no
longer the right shape for the cmd layer to call directly. Two paths:

- **Path A (preferred):** Promote `CapStatus` and `InFlightSession` into
  `internal/container` (or a new `internal/container/cap.go`). Demote
  `CheckCap`, `ListInFlight`, `FormatExceededError`, `FormatExceededWarning`
  to package-private helpers used only by `podmanIsolator.Cap`. The cmd
  layer calls `iso.Cap(ctx, dbPath)`, never the container helpers
  directly.
- **Path B:** Move `CapStatus` and `InFlightSession` up to
  `internal/isolation` (a new package, if A.1's registry is also moved
  there). Less disruptive to A.1's eventual layout but requires deciding
  the package boundary at A.3 time.

A.3 picks Path A. The package boundary question (Path B) belongs to A.1's
implementation work, not A.3's — A.3's interface can move with the
registry later without re-shaping `Cap`.

### 5.6 Supporting changes in `internal/config`

#1018 will add `Config.SandboxExecConcurrencyCap` and
`DefaultSandboxExecConcurrencyCap = 20` regardless of whether A.3 has
landed. A.3 makes no further changes to `internal/config/config.go`. The
podman cap stays as the compiled-in `container.DefaultConcurrencyCap = 6`
constant; if Ben wants to make it user-tunable in the future, that is a
separate one-line addition to `Config` and a one-line change to
`podmanIsolator.Cap`.

## 6. Relationship with #1018 — explicit sequencing statement

**Recommendation: #1018 ships first using a podman-shaped (parallel)
sandbox-exec cap. A.3 then refactors all three modes onto
`Isolator.Cap()` in a follow-up.**

Reasoning:

1. **#1018's scope is broader than the cap.** Per the issue body, #1018
   covers concurrency cap **and** kqueue-based lifecycle hardening **and**
   Nix module options. Holding it on A.3 would block the lifecycle work
   (sandbox-exec processes do not currently die when the tmux pane is
   killed — a real correctness problem for users on Darwin) on a
   refactoring proposal whose implementation is itself a follow-up to A.1.
2. **#1018's body explicitly permits both shapes.** The "Concurrency
   cap" bullet in #1018 reads: "*If extracting a generic helper feels
   cleaner, do it — both shapes are acceptable.*" Adding a third
   parallel `checkSandboxExecConcurrencyCap` is a 30-LoC change that
   mirrors the bwrap one verbatim and unblocks the lifecycle work
   immediately.
3. **A.3 absorbs the third implementation cheaply.** Once A.3 lands, the
   three parallel `check*ConcurrencyCap` functions and their inline
   formatters delete in one PR. The cost of the "wasted" parallel
   sandbox-exec implementation is one diff entry — the function and its
   ~30 LoC are removed, not rewritten in place.
4. **A.3 depends on A.1's `Isolator` superset being implemented.** A.1 is
   landed as a *proposal* (#1097). Its interface implementation work is
   itself a follow-up wave of issues (per A.1 §7 phases R.1–L.6). A.3
   cannot implement `Isolator.Cap()` until at minimum A.1's Phase 1
   (registry skeleton, R.1–R.3) and Phase 3 D.2 (the `Cap()` migration
   itself) are filed and landed. That sequencing is several PRs out;
   #1018 should not wait on it.
5. **The risk of going A.3-first is high; the cost of going A.3-second is
   low.** Going A.3-first means #1018 cannot land until A.1's
   implementation skeleton lands, which in turn requires Track E
   synthesis to set the implementation order. That is at least one full
   wave of work. Going A.3-second means the third parallel function
   exists for one wave then is deleted.

A.3 does **not** block #1018. A.3 also does not require any rebase of
#1018 — when A.3's implementation PR lands after #1018, the three
parallel functions and their inline formatters are removed in one diff,
and the three call sites in `cmd/spawn.go`, `cmd/pr.go`, `cmd/review.go`
collapse from per-mode `if` blocks to single-line `iso.Cap` calls (per
§4.4).

## 7. Migration order (one wave, after #1018)

Each step here is an independent PR. The order respects compile-time
dependencies; nothing in this list is gated on Track E synthesis beyond
A.1's implementation having reached at least Phase 1 (registry skeleton).

1. **A.3.1 — Add `CapStatus` / `InFlightSession` types and `RenderError` /
   `RenderWarning` methods** in `internal/container/cap.go` (Path A from
   §5.5). Promote `roleFor` to a package-level helper. No callers change
   yet; pure addition. Tests: unit tests for `RenderError` / `RenderWarning`
   exercising the three modes' nouns.
2. **A.3.2 — Add `db.ActiveSessionCountForMode` and
   `db.ActiveSessionsForMode`** alongside the existing
   `ActiveBwrapSessionCount` / `ActiveBwrapSessions`. Both old and new
   coexist. Tests: behavioural parity test against bwrap.
3. **A.3.3 — Add `Isolator.Cap(ctx, dbPath) CapStatus`** to A.1's
   interface superset. Implement on the three isolators (`podmanIsolator`,
   `bwrapIsolator`, `sandboxExecIsolator`); host-mode returns
   `CapStatus{Mode: IsolationHost, Limit: 0}`. Tests: per-isolator unit
   tests using fake DBs and (for podman) the existing `podmanPS` test
   injection point.
4. **A.3.4 — Replace `checkConcurrencyCap`, `checkBwrapConcurrencyCap`,
   and (after #1018) `checkSandboxExecConcurrencyCap` in
   `cmd/concurrency.go` with one unified `checkConcurrencyCap(cmd,
   callerName, iso)`.** Update the three call sites in `cmd/spawn.go`,
   `cmd/pr.go`, `cmd/review.go`. Tests: integration smoke that the
   end-to-end flow refuses spawn at the cap boundary for each mode.
5. **A.3.5 — Delete dead code:** `container.CheckCap` (public),
   `container.ListInFlight` (public, if no out-of-package callers),
   `container.FormatExceededError`, `container.FormatExceededWarning`,
   `container.CheckResult`, `container.InFlightSession` (the old type),
   `db.ActiveBwrapSessionCount`, `db.ActiveBwrapSessions`,
   `db.ActiveSandboxExecSessionCount`, `db.ActiveSandboxExecSessions`.
   The cleanup PR is intentionally last so each prior step is reversible
   in isolation.

## 8. Sites that do NOT fit `Cap()`

Two surfaces touch the cap concept but do not belong on `Cap()`:

1. **`--ignore-concurrency-cap` flag.** Lives on the cobra command, not on
   the isolator. The flag is owned by `cmd/spawn.go`, `cmd/pr.go`, and
   `cmd/review.go` (each registers it independently); the unified
   `checkConcurrencyCap` helper reads it from the supplied `*cobra.Command`
   exactly as today. The isolator only reports status; the *policy* of
   honouring the override is a cmd-layer concern. No interface change
   needed.
2. **`role` inference for the in-flight listing.** Today's `container.roleFor`
   (`internal/container/concurrency.go:42-53`) and the inline duplicates
   in `cmd/concurrency.go:106-115, 124-133` should consolidate into one
   package-level helper alongside the new `CapStatus` / `InFlightSession`
   types. It is not interface surface — every isolator's `Cap`
   implementation just calls the helper.

## 9. Open questions and `[uncertain]` flags

1. **Sandbox-exec live probe.** §3 — DB-only is the natural starting
   shape, but a `pgrep`-style scan for `agent-run --sandbox-exec` host
   processes could give sandbox-exec a live-probe component analogous to
   podman's. Out of scope for #1018 and A.3; the `Cap()` interface
   accommodates it without change if it is ever added.
2. **Per-mode versus uniform default cap.** Today: podman 6 (compiled-in),
   bwrap 20 (configurable), sandbox-exec 20 (configurable per #1018).
   Whether to homogenise on a single configurable
   `IsolationConcurrencyCap[mode]` map is a Track E synthesis decision,
   not an A.3 decision. A.3's `Cap()` shape supports either layout — the
   per-isolator implementation reads whatever value the config layer
   provides.
3. **Path A vs Path B for `CapStatus` package home (§5.5).** A.3 picks
   Path A (`internal/container/cap.go`). If A.1's implementation work
   moves the registry to a new `internal/isolation` package, `CapStatus`
   moves with it — no interface change, just an import-path swap.
4. **Whether `container.CheckCap` / `container.ListInFlight` have any
   out-of-package callers worth preserving.** A grep at the time of this
   proposal shows the only callers are in `cmd/concurrency.go` and the
   test file `internal/container/concurrency_test.go`. If a future caller
   needs the live-probe-only view, it can call
   `podmanIsolator.Cap(...)` and read `CapStatus.InFlight`. **[uncertain
   — verify at A.3.5 implementation time that no consumer outside this
   tree depends on the public `container.CheckCap` symbol.]**
5. **`CapStatus.Note` versus a typed error.** §4.2 — using a string
   `Note` is the simplest representation that preserves today's
   "podman ps failed" stderr message. A typed `ProbeWarning` value would
   be more precise but adds shape without immediate callers; the `Note`
   string is sufficient for the one warning type in scope.

## 10. Out of scope

- **Implementation.** A.3 is a proposal. The implementation lands as a
  follow-up wave (§7) after #1018 ships and after A.1's interface
  superset implementation reaches Phase 1.
- **Making the podman cap configurable.** §5.6 — orthogonal to the
  unification; can land independently any time after A.3.4.
- **Live-probe addition for sandbox-exec.** §3, §9 — left to a future
  refinement.
- **The broader `Isolator` interface superset.** Proposed in A.1; A.3
  only adds the `Cap()` method specification.
- **#1018's lifecycle hardening (kqueue watcher, parent-PID liveness
  fallback).** A.3 is the cap-only proposal; the lifecycle work is
  entirely #1018's domain and does not touch the cap surface.

## 11. Acceptance-criteria self-check

- [x] Document at `modules/programs/prism/prism/docs/reviews/A3-concurrency-cap-unification.md` exists.
- [x] Side-by-side comparison of the current podman and bwrap cap implementations with file:line citations — §2.
- [x] Sandbox-exec gap explicitly addressed and #1018 referenced — §3.
- [x] Proposed unified shape presented as a concrete Go interface signature — §4.1 (`Isolator.Cap`), §4.2 (`CapStatus`), §4.3 (`Render*` methods).
- [x] Per-mode implementation sketches (signatures) for podman, bwrap, sandbox-exec — §5.1, §5.2, §5.3.
- [x] Explicit statement on dependency relationship with #1018 — §6: A.3 does NOT block #1018; #1018 ships first with a parallel sandbox-exec cap, A.3 refactors all three modes in a follow-up. No rebase required.
- [x] `[uncertain]` flag where the right source-of-truth for sandbox-exec is unclear — §3 (sandbox-exec live probe), §9 (consolidated open questions).
- [x] Document contains zero implementation work — markdown only, no Go file added or modified outside `docs/reviews/`.
