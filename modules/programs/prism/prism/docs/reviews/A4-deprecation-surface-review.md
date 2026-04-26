# A.4 — Deprecation surface review (`ContainerMode bool`, `--host-mode` flag, `EffectiveIsolationMode` fallback)

Status: proposal (no code changes).
Issue: #1079.
Track: A (isolation), Wave 2 follow-up to A.1 (#1073 / merged as #1097).
Source corpus: `modules/programs/prism/prism/docs/architecture-inventory.md`
(specifically **§6.1 and §6.18**), the A.1 registry-shape proposal at
`docs/reviews/A1-isolation-registry-shape.md` (notes the (d)-tagged sites this
review is responsible for), and a fresh re-read of every cited site.

## 1. Context

Three back-compat paths survive from the pre-`IsolationMode` era:

1. **`Config.ContainerMode bool`** — the boolean knob that predated
   `DefaultIsolationMode`. Today's `load()` derives one from the other in
   either direction, and `EffectiveIsolationMode()` falls back to it when the
   new field is empty.
2. **`--host-mode` flag** — the deprecated CLI alias for `--isolation host`.
   Carries an explicit "deprecated" note in the flag help and in
   `resolveIsolationMode`'s mutual-exclusion error.
3. **`EffectiveIsolationMode()` fallbacks** — both `(Config).EffectiveIsolationMode()`
   (config side) and `(db.Status).EffectiveIsolationMode()` (DB side). They
   reconstruct the mode when the canonical field is empty/NULL.

A.1's registry-shape proposal tagged each of these as **(d) deletable** and
moved on; A.4 is the dedicated audit. The central question A.4 answers per
issue #1079 is: **how cheaply can each of these be removed today, and what
gates the cheaper-not-yet removals?**

The design doc (#1072 → `docs/reviews/000-design-narrow-review-series.md`)
gates **podman-as-a-mode** removal on sandbox-exec parity (#1012 design,
#1017 staging, #1018 lifecycle hardening). **A.4 is not about podman-as-a-mode.**
A.4 covers the back-compat *shims* that the introduction of `IsolationMode`
left behind, all of which can in principle be removed independently of
whether podman itself stays.

## 2. Method

I re-read the three sites named in the inventory at §6.1 and §6.18 plus
`cmd/spawn.go`'s `resolveIsolationMode`, then `rg`'d the corpus for every
`ContainerMode`, `--host-mode`/`"host-mode"`, `EffectiveIsolationMode`, and
`HostMode` mention to confirm no back-compat site was missed. The grep
turns up 33 files; non-test sites are 16 and are catalogued individually
below. Test sites are summarised in §3.10 because each test exercises a
production path already enumerated.

For each site I record:

- **What it handles** — which old artefact (config file shape, DB row, CLI
  invocation) it exists to support.
- **Removal cost** — how much production code would change, plus what
  persisted data would need to be dealt with.
- **Migration shape if any** — the concrete one-shot fix or schema
  migration needed before removal can land cleanly.
- **Removal gate** — what must be true before removal proceeds (sandbox-exec
  parity, an empty NULL-row population, a release boundary, etc.).

`[uncertain]` flags accompany any cost estimate that depends on data that
cannot be observed from inside this worker (e.g. NULL-row counts on
machines other than my own).

## 3. Per-site catalogue

### 3.1 `Config.ContainerMode bool` — config-file back-compat

**Sites (production):**

- `internal/config/config.go:73-79` — `Config.ContainerMode bool` field
  declaration with `Deprecated:` note and the back-compat invariant
  `ContainerMode == (DefaultIsolationMode == "podman")`.
- `internal/config/config.go:138` — `parsedConfig.ContainerMode *bool`
  pointer field (used to distinguish "absent" from "false"; the pointer
  shape is itself a back-compat affordance).
- `internal/config/config.go:254-268` — `load()` — when
  `default_isolation_mode` is present and valid, derive
  `ContainerMode` from it; **else** when `parsed.ContainerMode != nil`,
  set `ContainerMode` from the parsed value and derive
  `DefaultIsolationMode` (`true → IsolationPodman`,
  `false → IsolationHost`).
- `internal/config/config.go:349-361` — `(Config).EffectiveIsolationMode()`
  consults `ContainerMode` only when `DefaultIsolationMode` is empty.

**Sites (downstream consumers — what would need to migrate):**

- `internal/session/session.go:88-103, 254-296, 354-371, 619` — `Opts.ContainerMode bool`
  is a *parameter* of `BuildOpencodeCmd`/spawn, not a config-file value.
  This shim has the same name but a *different* shape from
  `Config.ContainerMode`: see §3.5.
- `cmd/spawn.go:275`, `cmd/switch.go:1068-1108`, `cmd/pr.go:76-77, 169` —
  `effectiveContainerMode := isolationMode == config.IsolationPodman`
  computed locally and passed to downstream `session.SpawnOpts`. These
  three sites are the call-graph *gateway* between `Config.ContainerMode`
  (config side, deprecated) and the in-memory `Opts.ContainerMode` (runtime
  side, also (d)-tagged but separately).

**What it handles:** a `~/.config/prism/config.json` whose only isolation
key is `"container_mode": true` (or `false`) — i.e. a config file written
before `default_isolation_mode` was introduced.

**Removal cost:** **moderate**.

- The struct field, the `parsedConfig` pointer field, the `load()` derive
  branch, and the `EffectiveIsolationMode()` consult of `ContainerMode`
  delete cleanly (≈ 25 LoC across one file).
- Every downstream caller that reads `Config.ContainerMode` becomes a
  `cfg.DefaultIsolationMode == config.IsolationPodman` test (or moves to
  the registry per A.1 §4.2 `Resolve`). Today these are the three
  `effectiveContainerMode := ...` sites listed above and the
  `cmd/review_test.go` (tests #758) that asserts the host-API path is
  reached *regardless of* `cfg.ContainerMode` — those tests stay valid as
  long as the behavioural assertion ("PRISM_HOST_API == \"\" causes the
  check to run") survives, even if the field is gone.
- **Persisted artefacts:** any `~/.config/prism/config.json` carrying only
  `"container_mode": …` becomes a no-op for that key after removal — the
  config loads with `DefaultIsolationMode == ""` and the *DB-side*
  `EffectiveIsolationMode()` still kicks in for any active session that
  had its mode resolved against this config previously. **[uncertain]** —
  I cannot enumerate the set of `config.json` files in the wild from this
  worker. The Nix-managed flake (`modules/programs/prism/`) writes
  `default_isolation_mode` directly, so any host configured via this
  flake is unaffected. The risk is hand-edited config files on machines
  not managed by this flake.

**Migration shape:** none required for the field itself (deletion is
backward-compatible at the JSON-decode level: an unknown `container_mode`
key is silently dropped). The downstream `effectiveContainerMode := ...`
gateway sites should land first or together with the deletion so no
caller dereferences a removed field.

**Removal gate:**

- **Hard prerequisite:** at least one downstream consumer must have moved
  off `Config.ContainerMode` before the field is removed (otherwise the
  build breaks). In practice this means landing the A.1 Phase 2/3
  migrations (capability reads, `Resolve`) before A.4's `ContainerMode`
  removal step.
- **Soft prerequisite:** a release-note and a one-cycle deprecation period
  letting users notice that `container_mode` in their JSON is dead. This
  is purely advisory; the JSON shape tolerates the unknown key.
- **Independent of sandbox-exec parity** (#1012/#1017/#1018). `Config.ContainerMode`
  has no relationship to the question of which modes ship.

### 3.2 `(Config).EffectiveIsolationMode()` fallback method

**Sites (production):**

- `internal/config/config.go:349-361` — declaration and body. Returns
  `DefaultIsolationMode` when set; else `IsolationPodman` if
  `ContainerMode == true`; else `IsolationHost`.

**Callers in production:**

- `cmd/spawn.go:202` — final fallback in `resolveIsolationMode` after
  `--isolation` and `--host-mode` flag handling.
- `cmd/switch.go:1068`, `cmd/pr.go:76`, `cmd/review.go:170` — derive the
  effective machine-default mode when no per-spawn flag is in play.

**What it handles:** a `Config` whose `DefaultIsolationMode` is empty —
i.e. an old config file (per §3.1) or compiled-in defaults (`defaults()`
returns `DefaultIsolationMode == ""`).

**Removal cost:** **moderate**, but interlocked with §3.1.

- The method itself is small (8 LoC). Removal alone is trivial.
- The interlocking concern is that **`defaults()` does not set
  `DefaultIsolationMode`**: it leaves the zero value (empty string).
  Today's effective fallback is `host` (because `ContainerMode` is also
  zero). If `EffectiveIsolationMode()` is removed, `defaults()` must
  pick a concrete mode — likely `IsolationHost` (the safest choice on
  any platform) or branched by `runtime.GOOS`. **[uncertain]** —
  whether `defaults()` should cross-compile-detect a sane default or
  always pick `host` is a design decision that should land alongside
  removal, not as part of it.

**Migration shape:**

1. Set `defaults().DefaultIsolationMode = config.IsolationHost` (or a
   `runtime.GOOS`-aware equivalent).
2. Update the four callers to consult `cfg.DefaultIsolationMode` directly
   (or a registry-side `Resolve` per A.1 §4.2).
3. Delete the method.

**Removal gate:**

- **Hard prerequisite:** §3.1 (`Config.ContainerMode`) removal lands
  first or together. Removing `EffectiveIsolationMode()` while
  `ContainerMode` survives leaves the back-compat path orphaned (no
  caller, but the field still exists and confuses readers).
- **Independent of sandbox-exec parity.** The fallback predates and is
  unrelated to bwrap/sandbox-exec parity; removing it does not change
  which modes are valid.

### 3.3 `(db.Status).EffectiveIsolationMode()` fallback method

**Sites (production):**

- `internal/db/db.go:86-98` — declaration and body. Returns
  `IsolationMode` when non-empty; else `"host"` if `HostMode == true`;
  else `"podman"`.
- `internal/db/db.go:2307-2315` — `scanStatus` reader populates
  `Status.HostMode` and `Status.IsolationMode` from `sql.Null*` columns,
  treating NULL as zero/empty.

**Callers in production:**

- `cmd/agent_run.go:126` — agent-run consults the DB row to decide
  whether to dispatch the bwrap or sandbox-exec branch.
- `cmd/cleanup.go:874-879` — `isolationModeFromDB` wraps it for
  cleanup's per-mode resource teardown.
- `cmd/restore.go` (range `:43, :89-92, :125-127, :306, :314-403`) —
  restore re-derives the mode from the persisted row before re-spawning.

**What it handles:** an `agent_status` row whose `isolation_mode` column
is NULL — i.e. a pre-v10 row (the v9→v10 migration at
`internal/db/db.go:551-566` adds the column NULLABLE so existing rows
receive NULL).

**Removal cost:** **low to moderate**, depending on persisted-data state.

- The method is 12 LoC.
- The `scanStatus` reader's NULL handling for `isolation_mode` is the
  same back-compat shim and would also drop.
- The `host_mode` column itself (and the `Status.HostMode` field, and
  `SetHostMode` writer) is a separate but adjacent concern: cleanup's
  `hostModeFromDB` (`cmd/cleanup.go:863-868`) reads it to short-circuit
  podman teardown for host-mode sessions. After the fallback method is
  removed, `hostModeFromDB` becomes dead because cleanup branches purely
  on `isolation_mode == "host"`. **[uncertain]** — see §3.7 for the
  `host_mode` column drop, which is a separate migration with its own
  data shape.

**Sanity check (this worker's local DB):** I ran the suggested
`SELECT COUNT(*) FROM agent_status WHERE isolation_mode IS NULL` against
my local DB at `~/.local/state/prism/prism.db` (schema_version 20). The
query was executed by the user on the host because the worker bwrap
sandbox does not expose the DB. Result:

| Metric | Value |
|---|---|
| `total_rows` | 887 |
| `isolation_mode_null_rows` | 550 |
| **`isolation_mode_null_active_rows`** (NULL **and** `ended_at IS NULL`) | **0** |
| `host_mode_true_rows` | 11 |
| `host_mode_true_iso_null_rows` | 3 |
| `schema_version` | 20 |

`isolation_mode` value distribution:

| Value | Count |
|---|---|
| `<NULL>` | 550 |
| `bwrap` | 318 |
| `podman` | 14 |
| `host` | 5 |

**Interpretation:** every NULL `isolation_mode` row on this machine is an
**archived** session (`ended_at IS NOT NULL`). The
`(db.Status).EffectiveIsolationMode()` fallback **never fires for any
active session today on this machine** because no active row has a NULL
`isolation_mode`. The 550 NULL rows are pre-v10 archive rows that survive
because the v9→v10 migration deliberately allowed NULL for back-compat.

The three `host_mode_true_iso_null_rows` are the specific historical
intersection where `EffectiveIsolationMode()` would have returned
`"host"` via the `HostMode=true` branch — all three are archived (since
`isolation_mode_null_active_rows == 0`). They confirm the historical use
case the fallback was designed for, but no live session currently
depends on it.

**[uncertain]** — this is one machine's DB. Other machines (in particular
ones that haven't spawned a fresh session since v9→v10 landed) might
plausibly carry NULL active rows. Risk: low (the `default_isolation_mode`
spawn path has been the default since #1097 landed and writes
`isolation_mode` unconditionally on every new spawn — see
`internal/session/session.go:655-671` and
`internal/session/spawn.go:567-580`), but not zero.

**Migration shape:** a one-shot backfill executed before the method is
removed:

```sql
UPDATE agent_status
SET isolation_mode = CASE WHEN host_mode = 1 THEN 'host' ELSE 'podman' END
WHERE isolation_mode IS NULL;
```

This mirrors today's runtime fallback exactly. Run it as part of a
schema migration `vN→vN+1` so the data shape is moved forward atomically
with the code change. Whether to *also* tighten the column to `NOT NULL`
in the same migration is a follow-up question (§5).

**Removal gate:**

- **Hard prerequisite:** the SQL backfill above lands as a schema
  migration **before** the fallback method is removed.
- **Hard prerequisite:** the (d)-tagged session-side fallback callers
  (§3.5) move to reading `Opts.IsolationMode` directly first, so the
  removal does not strand a caller mid-flight.
- **Independent of sandbox-exec parity.** The fallback is about reading
  pre-v10 rows correctly; it does not say anything about which modes are
  valid going forward.

### 3.4 `--host-mode` CLI flag

**Sites (production):**

- `cmd/spawn.go:147` — flag declaration `Bool("host-mode", false, "Deprecated alias …")`.
- `cmd/spawn.go:62, 71, 78, 166, 170, 174, 197-198` — proxy and direct
  flag-resolution sites (`proxySpawn`, `resolveIsolationMode`).
- `internal/sidecar/sidecar.go:3122, 3148-3149, 3190-3191, 3219-3220` —
  host-API `/spawn` request: `host_mode` JSON field, mutual-exclusion
  rejection, and conditional `--host-mode` re-emission when proxying to
  the host-side `prism spawn`.
- `cmd/spawn.go:18` — comment in the cmd doc-block listing
  `--host-mode    deprecated alias for --isolation host`.

**Sites (tests):**

- `cmd/spawn_harness_test.go:39, 80, 115, 162, 212` — test scaffolding
  declares the flag on a synthetic cobra command.
- `cmd/hostapi_test.go:271, 279, 350, 413, 464, 506, 530, 547, 554-560,
  722` — exercises proxy flag-pass-through, mutual-exclusion rejection,
  and headless cleanup naming.

**What it handles:** an old CLI invocation
`prism spawn --branch foo --host-mode` written by a user (or a script,
or a coordinator session muscle-memory) that predates `--isolation`.

**Removal cost:** **low at the cmd boundary, moderate at the host-API boundary.**

- The flag declaration plus the four resolution branches in `cmd/spawn.go`
  delete in ≈ 20 LoC.
- The host-API `/spawn` shape change is the substantive one: removing
  the `host_mode bool` JSON field is a wire-protocol change. Inside this
  repo every caller is `proxySpawn`, but coordinator sessions running an
  older `prism` binary against a newer sidecar would silently lose the
  setting. The mutual-exclusion check at sidecar:3148-3151 disappears.
- The five test files that declare the flag for parity with production
  need to drop the declaration and any `Set("host-mode", "true")` lines.

**Migration shape:** a soft client-side migration (one release: keep the
flag, accept it, ignore it with a stderr warning). The host-API field
must remain decodable for one release after the cmd flag is gone, so an
older client sending `"host_mode": true` is silently mapped to
`"isolation": "host"` instead of failing the handler. Then both can drop
together.

**Removal gate:**

- **Soft prerequisite:** one-release deprecation period during which the
  flag accepts `true` and converts to `--isolation host` with a stderr
  warning. The host-API mirror does the same on the wire.
- **Independent of sandbox-exec parity.** `--host-mode` is unrelated to
  which sandbox modes ship; it is purely a legacy alias.

### 3.5 `Opts.ContainerMode bool` (session, sidecar, spawn opts) and the in-flight derivation

**Sites (production):**

- `internal/session/session.go:88-103` — `Opts.ContainerMode bool`
  documented as "kept for callers that have not migrated".
- `internal/session/session.go:254-296` — `BuildOpencodeCmd` falls back
  to `opts.ContainerMode` (mapped to `"podman"`) when
  `opts.IsolationMode == ""`.
- `internal/session/session.go:354-371, 619` — `agentPaneEnvVars` skips
  env injection when `opts.ContainerMode == true`; `setupFullLayout`
  builds `StartSidecarOpts{ContainerMode: mode == "podman"}`.
- `internal/session/sidecar.go:214-216, 236, 319` —
  `StartSidecarOpts.ContainerMode bool` plumbing through the sidecar
  argv builder; conditional `--container` flag emission.
- `internal/session/spawn.go:84-91, 416, 490, 509, 538` —
  `SpawnOpts.ContainerMode bool` field plus `mode := opts.IsolationMode;
  if mode == "" && opts.ContainerMode { mode = "podman" }` derivation.
- `cmd/spawn.go:275, 434`, `cmd/switch.go:1093`, `cmd/pr.go:169`,
  `cmd/restore.go` (per `effectiveContainerMode` flow) — gateway sites
  that compute `effectiveContainerMode := isolationMode == config.IsolationPodman`
  and pass it through `Opts.ContainerMode` for back-compat with callers
  that still read it.

**Sites (tests):**

- `cmd/agent_default_test.go:164, 166, 175-184` — five table-driven
  cases that exercise `Opts.ContainerMode` directly.
- `internal/session/session_test.go:135-151, 257-266` — assert that
  `agentPaneEnvVars` skips for `ContainerMode=true` and that the legacy
  fallback returns the `podman attach` command.
- `internal/session/spawn_test.go:340` — exercises the `ContainerMode`
  field for completeness.

**What it handles:** in-process callers that build a `session.Opts` (or
`SpawnOpts`/`StartSidecarOpts`) without populating `IsolationMode` —
purely an in-memory back-compat shape. Distinct from §3.1: this version
of `ContainerMode` never lived in JSON, only in Go.

**Removal cost:** **moderate**.

- Each `Opts.ContainerMode` site is small (≈ 1-3 LoC), but there are
  ~12 production sites and ~15 test assertions, all of which need to
  flip together. The fallback derivation in `BuildOpencodeCmd` and
  `spawnAgentOnlyLayout` is the only behavioural change: callers that
  *only* set `ContainerMode` (not `IsolationMode`) currently work; after
  removal they would emit a host-mode command. **[uncertain]** — there
  are no in-tree such callers, but the tests at
  `cmd/agent_default_test.go:175-184` model the pattern.
- The conditional `--container` emission at
  `internal/session/sidecar.go:319` collapses to "if
  `opts.IsolationMode == "podman"`".

**Migration shape:** none for the data — purely a code change. All call
sites must move to populating `Opts.IsolationMode` before the field is
removed. The A.1 §4.2 `Resolve` helper is the natural funnel: every
gateway site (`cmd/spawn.go:275`, `cmd/switch.go:1068`, `cmd/pr.go:76`,
`cmd/restore.go:43`, `internal/session/spawn.go:490`) calls
`registry.Resolve(...)` once and propagates the resolved mode through
`IsolationMode` only.

**Removal gate:**

- **Hard prerequisite:** all gateway sites moved to setting
  `Opts.IsolationMode` and dropping the parallel `ContainerMode`
  assignment. This is the bulk of the work and lives naturally inside
  the A.1 Phase 2/3 migration sequence.
- **Independent of sandbox-exec parity.** This is a purely structural
  cleanup of a Go field shape; no platform-mode question is involved.

### 3.6 `Status.HostMode` field, `SetHostMode` writer, `host_mode` column

**Sites (production):**

- `internal/db/db.go:72` — `Status.HostMode bool` field.
- `internal/db/db.go:164` — schema column `host_mode INTEGER NOT NULL DEFAULT 0`.
- `internal/db/db.go:395-397` — v4→v5 migration adding the column to
  pre-v5 rows.
- `internal/db/db.go:512, 526` — referenced inside the v8→v9 column-rebuild
  migration (preserved through the table re-create).
- `internal/db/db.go:1414-1431` — `(*DB).SetHostMode` writer.
- `internal/db/db.go:1696-2683` — every `SELECT … host_mode, isolation_mode …`
  query enumerated in the `agent_status` reader paths
  (lines 1696, 1713, 1738, 1747, 1765, 2542, 2683).
- `internal/db/db.go:2307-2310` — NULL-tolerant scan of `host_mode`.
- `internal/session/session.go:670-671`, `internal/session/spawn.go:576-578` —
  `d.SetHostMode(name, true)` is called when the resolved mode is `host`.
- `cmd/cleanup.go:863-868` — `hostModeFromDB` reads `Status.HostMode`
  for the cleanup short-circuit.

**Sites (tests):**

- `internal/session/session_test.go:359-360, 380-381, 394-395` —
  asserts `host_mode` is set to 1 in the host-mode spawn path.

**What it handles:** the pre-v10 schema where `isolation_mode` did not
exist and "is this session host-mode?" was the only question the DB
could answer. Today every spawn path writes `isolation_mode`
unconditionally; `host_mode` is double-written for back-compat with
pre-v10 readers (which no longer exist on machines past v10).

**Removal cost:** **moderate to high.**

- The column drop is a SQLite `ALTER TABLE` migration. SQLite supports
  `DROP COLUMN` since 3.35 — the project already uses table-rebuild
  migrations (see v8→v9 at `internal/db/db.go:395-554`), so the same
  pattern fits.
- Every `SELECT` query naming `host_mode` (seven sites in `db.go`) needs
  the column dropped from the projection. Mechanical change.
- `SetHostMode`, `Status.HostMode`, `hostModeFromDB`,
  `session.go:670-671` and `spawn.go:576-578` all delete.
- The cleanup short-circuit at `cmd/cleanup.go:792-868, 882-979`
  (per inventory §6.12) becomes a `case "host": return nil` arm or
  collapses into the per-mode dispatch the A.1 registry provides.

**Migration shape:**

1. Backfill (per §3.3): `UPDATE agent_status SET isolation_mode = CASE WHEN host_mode = 1 THEN 'host' ELSE 'podman' END WHERE isolation_mode IS NULL;`.
2. New schema-version migration: rebuild `agent_status` without
   `host_mode`, copying all other columns through. This must be in the
   same upgrade step that removes the writer (otherwise a stale binary
   could try to write a missing column).
3. Code change: drop the field, the writer, the SELECTs' projection
   entries, the callers. One commit.

**Removal gate:**

- **Hard prerequisite:** §3.3 backfill landed and verified (no
  `isolation_mode IS NULL` rows survive among rows the system still
  reads). The §3.3 sanity check above shows zero active NULL rows on
  this machine, but archived NULL rows still exist (550 of them) —
  whether the backfill needs to touch archived rows depends on whether
  any read path consults archived `isolation_mode`. Today no production
  caller does (cleanup, restore, agent-run all filter to active rows),
  but `cmd/list_sessions.go` and the dashboard read archived rows for
  display — those would render `<empty>` for archived NULL rows after
  the fallback method is removed. **[uncertain]** — the backfill is
  cheap; running it across all rows is the safer call.
- **Hard prerequisite:** §3.3 (`Status.EffectiveIsolationMode`) removed
  first, so no caller reads `HostMode` to derive a mode any more.
- **Soft prerequisite:** one release where `host_mode` is read but no
  longer written, to catch any out-of-tree consumer (none known).
- **Independent of sandbox-exec parity.** The column is back-compat for
  a pre-v10 schema, not for any particular isolation mode.

### 3.7 Comment-only and documentation surface

These exist purely as text and have zero runtime cost, but enumerate
them for a complete deletion checklist:

- `cmd/spawn.go:18` — doc-block: `--host-mode  deprecated alias for --isolation host`.
- `internal/config/config.go:73-83, 254-267, 349-352` — Deprecated:
  notes and back-compat-derivation comments.
- `internal/db/db.go:73, 86-89, 215-216, 222-223, 552-554, 1414-1417,
  2307-2312` — comments naming the back-compat shape.
- `internal/session/session.go:74-75, 88-103, 149-167, 254, 273, 289,
  352, 414-435, 569` — comments naming "host-mode" and
  "ContainerMode = false" as interchangeable conditions.
- `internal/session/sidecar.go:214-216, 236` — `StartSidecarOpts.ContainerMode`
  doc comment.
- `internal/session/spawn.go:15, 84-91, 117, 142, 206, 238, 348, 360, 392`
  — `SpawnOpts.ContainerMode` doc comment and host-mode-flavoured
  diagnostic strings (#1064 hint text).
- `internal/sidecar/sidecar.go:470, 1696, 1700, 3101` — block comments
  referencing host-mode and the `host_mode` request field.

**Removal cost:** trivial; piggy-backs on the substantive change in the
same file.

### 3.8 Test-side surface

Tests that exercise the deprecated paths:

- `cmd/agent_default_test.go:164, 166, 175-184` — `TestBuildOpencodeCmd_ContainerMode`.
- `cmd/spawn_harness_test.go:39, 80, 115, 162, 212` — flag-declaration
  scaffolding.
- `cmd/hostapi_test.go:271-279, 350, 413, 464, 506, 530-560, 722` —
  proxy and mutual-exclusion tests.
- `cmd/restore_test.go` — restore + isolation-mode interaction
  (27 mentions, mostly the *new* `IsolationMode` plumbing — back-compat
  surface is the `EffectiveIsolationMode` calls in the helper at
  `cmd/restore.go:43`).
- `cmd/review_test.go:381-422` — documents fix for #758
  (`!cfg.ContainerMode` guard removal); the test asserts the *behaviour*
  outlives the field removal (PRISM_HOST_API == "" causes the agent
  availability check to run regardless of `cfg.ContainerMode`).
- `cmd/isolation_test.go:1-end` — exercises `resolveIsolationMode`
  including the `--host-mode` arm.
- `internal/session/session_test.go:135-151, 236-266, 359-360, 380-395, 471-476`
  — host-mode and ContainerMode-fallback assertions.
- `internal/session/spawn_test.go:340` — `ContainerMode` parameterised case.
- `internal/session/initial_prompt_test.go` — host-mode prompt-file plumbing
  (12 mentions; tests are about the prompt-file shape, not about the
  back-compat derivation, so they survive the §3.5 removal as-is once
  callers populate `IsolationMode = "host"`).
- `internal/db/db_test.go:328-395` — exercises `SetHostMode` /
  `host_mode` column directly; deletes alongside §3.6.
- `internal/sidecar/sidecar_test.go` — host-API `host_mode` field in the
  request shape; deletes alongside §3.4 wire-protocol change.
- `internal/config/config_test.go` — round-trip tests for the config
  back-compat derivation; deletes alongside §3.1.
- `internal/review/review_test.go`, `internal/harness/opencode/adapter_test.go`,
  `cmd/cleanup_container_test.go`, `cmd/pr_config_write_test.go` — minor
  ContainerMode references that delete in concert.

**Removal cost:** the tests are the *easy* indicator of where the back-compat
surface lives. Each test that exercises the removed shim deletes (or, for
behavioural tests like `cmd/review_test.go:381-422`, is rewritten to drop
the `cfg.ContainerMode` parameterisation while keeping the behavioural
assertion).

## 4. Sanity check — local DB

Performed on host (worker bwrap sandbox does not expose
`~/.local/state/prism/prism.db`). Schema version: 20. Full result table is
in §3.3.

**Headline finding:** `isolation_mode_null_active_rows == 0` on this
machine. The `(db.Status).EffectiveIsolationMode()` fallback fires for
zero live sessions today; it is currently keeping 550 archived rows
display-correct only.

**[uncertain]** — extrapolating to other deployment environments: the
`default_isolation_mode` writer paths (`session.go:655-671`,
`spawn.go:567-580`) write `isolation_mode` unconditionally on every
spawn since #1097, so any machine that has spawned a fresh session
recently is in the same shape. Machines that have not spawned since the
v9→v10 migration shipped might still have NULL active rows. Risk is low
but observable; the SQL backfill in §3.3 mitigates it cleanly.

## 5. Proposed removal sequence

The order respects the cross-site dependencies surfaced in §3. Each step
is intended to land as one PR with no behaviour change.

### Phase A — purely additive (prerequisites)

1. **Land A.1's registry skeleton (already proposed in #1097).** Steps
   R.1–R.3 from `A1-isolation-registry-shape.md` §7. The
   `IsolationRegistry.Resolve(...)` helper becomes the single seam for
   the back-compat fallbacks while the rest of A.4 is in flight.
2. **Backfill SQL migration** (vN→vN+1):
   `UPDATE agent_status SET isolation_mode = CASE WHEN host_mode = 1 THEN 'host' ELSE 'podman' END WHERE isolation_mode IS NULL;`.
   Affects archived rows per §3.3; safe because the value mirrors the
   runtime fallback already in use. Land *before* any reader change so a
   downgrade still works.

### Phase B — `Config.ContainerMode` and the config-side fallback (independent of sandbox-exec parity)

3. **Migrate gateway callers** at `cmd/spawn.go:275`, `cmd/switch.go:1068`,
   `cmd/pr.go:76`, `cmd/restore.go:43` to call
   `registry.Resolve(cfg, …)` instead of
   `cfg.EffectiveIsolationMode()`. Drops one of the two back-compat
   readers immediately; `Resolve` carries the back-compat in one place
   (per A.1 §4.2).
4. **Set `defaults().DefaultIsolationMode = config.IsolationHost`** in
   `internal/config/config.go:162-181`. This makes the compiled-in
   default explicit instead of relying on
   `EffectiveIsolationMode()`'s `host` fallback.
5. **Remove `Config.ContainerMode`, `parsedConfig.ContainerMode`, the
   `load()` derive branch, and `(Config).EffectiveIsolationMode()`** —
   one commit, ≈ 35 LoC across `internal/config/config.go`. Tests at
   `internal/config/config_test.go` delete in the same commit.

### Phase C — `Opts.ContainerMode` cleanup (independent of sandbox-exec parity)

6. **Migrate every gateway site** (per §3.5) to set
   `Opts.IsolationMode` only, drop the
   `ContainerMode: effectiveContainerMode` parallel assignment.
7. **Migrate `BuildOpencodeCmd` and `spawnAgentOnlyLayout`** to read
   only `Opts.IsolationMode`, deleting the
   `if mode == "" && opts.ContainerMode { mode = "podman" }` shim.
8. **Drop `Opts.ContainerMode`, `StartSidecarOpts.ContainerMode`,
   `SpawnOpts.ContainerMode`** plus the conditional `--container`
   sidecar argv emission (collapses to `if opts.IsolationMode == "podman"`).
9. **Test cleanup:** delete or rewrite per §3.8.

### Phase D — `--host-mode` flag (independent of sandbox-exec parity, but with a release-note step)

10. **Land a stderr-deprecation warning** on `--host-mode` and
    `host_mode: true` (host-API). Flag still works; both paths
    transparently rewrite to `--isolation host` /
    `"isolation": "host"`. One release.
11. **Drop the `--host-mode` flag** from `cmd/spawn.go:147` and the
    cmd-side resolution in `proxySpawn` and `resolveIsolationMode`.
    Test scaffolding in `cmd/spawn_harness_test.go` and `cmd/hostapi_test.go`
    drops the same.
12. **Drop the `host_mode` field** from the `/spawn` request shape in
    `internal/sidecar/sidecar.go:3122-3221`. The mutual-exclusion check
    at `:3148-3151` deletes (only one shape left). Wire-protocol break
    landed in the same release.

### Phase E — `Status.EffectiveIsolationMode` and `host_mode` column (independent of sandbox-exec parity, but interlocked with Phase A backfill)

13. **Migrate the three callers** of `Status.EffectiveIsolationMode()`
    (`cmd/agent_run.go:126`, `cmd/cleanup.go:874-879`, `cmd/restore.go`
    range) to read `Status.IsolationMode` directly. Safe because Phase
    A backfill guarantees no NULL `isolation_mode` rows.
14. **Delete `Status.EffectiveIsolationMode()`** and the NULL-handling
    branch in `scanStatus` for `isolation_mode`.
15. **Drop `Status.HostMode`, `SetHostMode`, `hostModeFromDB`** and the
    `session.go:670` / `spawn.go:576` writers.
16. **Drop the `host_mode` column** with a table-rebuild migration
    (vM→vM+1). Update every `SELECT … host_mode, isolation_mode …`
    projection in `internal/db/db.go` (seven sites). Delete the v4→v5
    migration's add-column step? **No** — historical migrations stay
    intact; only the current schema and the projection lists change.

### Phase F — explicit non-action

17. **Do NOT propose removal of podman-as-a-mode.** That is gated on
    sandbox-exec parity per #1012/#1017/#1018 and the design doc; A.4
    is silent on it. The `IsolationPodman` constant, the
    `podmanIsolator`, and every podman-specific code path under §6.3
    of the inventory survive A.4 unchanged.

### Dependency summary

- Phase A (1, 2) prerequisites *all* later phases. Pure addition.
- Phases B, C, D are mutually independent and can land in parallel.
- Phase E depends on Phase A's backfill (for safety) and on Phase B
  finishing (because B removes `cfg.EffectiveIsolationMode` and the
  same removal pattern then applies symmetrically to the DB-side
  method). E does **not** depend on D — the host-API flag and the
  DB-column drop are independent shapes.
- Sandbox-exec parity (#1012/#1017/#1018) gates **none** of the above.
  Every back-compat shim under review here is independent of which
  sandbox modes ship. The gate exists only for the separately-tracked
  podman-as-a-mode removal, which is out of scope per §3 / Phase F.

## 6. Open questions and `[uncertain]` flags (consolidated)

1. **Hand-edited `config.json` with only `container_mode` set (§3.1).**
   I cannot enumerate config files in the wild from this worker. Risk
   is bounded because the Nix-managed flake writes
   `default_isolation_mode` directly; affected hosts are those
   configured outside the flake. **Mitigation:** keep the JSON-decode
   tolerant of an unknown `container_mode` key after the field is
   gone (the default decoder already does this); add an explicit
   release-note callout.

2. **`defaults()` should pick which mode (§3.2).** `IsolationHost` is
   the safest universal choice. A `runtime.GOOS`-aware default
   (`bwrap` on Linux, `sandbox-exec` on Darwin once parity is reached,
   `host` elsewhere) is more useful but more opinionated. Suggest
   `IsolationHost` for A.4's removal step and a follow-up issue if
   the platform-aware default proves desirable.

3. **NULL `isolation_mode` rows on machines other than this one
   (§3.3).** The §3.3 sanity check shows zero active NULL rows here.
   Other machines may differ. **Mitigation:** the SQL backfill is
   idempotent and safe to run unconditionally; landing it as Phase A
   step 2 covers all machines uniformly.

4. **Whether to tighten `isolation_mode` to `NOT NULL` after the
   backfill.** Phase A step 2 backfills; the column stays NULLABLE.
   Tightening is a separate migration — **deferred**, because the
   `scanStatus` reader already treats NULL as the empty string (a
   harmless representation) once the fallback method is gone.

5. **Whether the `host_mode` column drop (Phase E step 16) should also
   delete the v4→v5 migration code.** No. Historical migrations stay
   intact; future databases starting fresh skip them anyway because
   the current schema already declares the column-free table.

6. **Tests at `cmd/review_test.go:381-422` (#758 fix).** The behavioural
   assertion ("PRISM_HOST_API == \"\" causes the agent availability
   check regardless of `cfg.ContainerMode`") survives the field
   removal but the parameterisation does not. Suggested rewrite: drop
   the `cfg.ContainerMode` parameter, keep the behavioural assertion,
   add a comment naming #758 as the historical reason for the test.

7. **Wire-protocol break at `/spawn` (Phase D step 12).** Removing
   `host_mode` from the JSON shape is a breaking change for any
   non-prism client speaking the API. **[uncertain]** — there are no
   such clients in this repo. Risk acknowledged; the deprecation
   warning in step 10 covers it.

## 7. Acceptance-criteria self-check

- [x] Document at `modules/programs/prism/prism/docs/reviews/A4-deprecation-surface-review.md` exists.
- [x] Catalogues every back-compat site introduced for the
  `IsolationMode` migration with file:line citations: §3.1 (config
  ContainerMode), §3.2 (config-side EffectiveIsolationMode), §3.3
  (DB-side EffectiveIsolationMode), §3.4 (`--host-mode` flag), §3.5
  (Opts.ContainerMode), §3.6 (`Status.HostMode` /
  `host_mode` column), §3.7 (comment surface), §3.8 (test surface).
- [x] For each site, records what it handles, removal cost, migration
  shape if any, removal gate (per-site headers in §3).
- [x] Sanity-check result included with concrete numbers (§3.3 and §4):
  `isolation_mode_null_active_rows = 0` on schema_version 20 with 887
  total rows. Worker did not run the query directly (sandbox restriction);
  user ran it on the host and the numbers are recorded verbatim.
- [x] Proposed removal sequence with explicit gating (§5). Phase F
  records the explicit non-action on podman-as-a-mode and the gate on
  sandbox-exec parity (#1012/#1017/#1018) for that separate concern.
- [x] `[uncertain]` flags carried where removal cost depends on
  unknown deployment-time data (§3.1 hand-edited configs, §3.3 other
  machines' DBs, §3.5 unknown out-of-tree callers, §6 consolidation).
- [x] Document does **not** propose removal of podman-as-a-mode (§5
  Phase F is explicit; the `IsolationPodman` constant and the
  `podmanIsolator` are out of scope).
- [x] Document contains zero implementation work — markdown only, no
  Go file added or modified outside `docs/reviews/`.
