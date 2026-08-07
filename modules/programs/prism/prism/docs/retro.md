# `prism retro` — a retrospective surface for coordinators

<!--
  This document is the design that the four child issues under #2529
  implement. It is landing incrementally: issue #2582 (part 1) fixed the
  sandbox stats stub, and issue #2583 (part 2) added the `prism retro`
  command itself — window totals (section 1), the trains table (section 2),
  the waste signals (section 5), `--json`, and the FK-based train resolution.
  The per-train review-cycle detail (section 3) and the fixed-overhead
  accounting (section 4) are not built yet. Read this before you pick up the
  remaining child issues.
-->

Tracking issue: [#2529](https://github.com/prismatic-koi/nixos-config/issues/2529).

## 1. Why this surface is needed

A coordinator asked to review a morning of work has no usable path today.

- `prism stats --days N` can report "no events in the last N days" while
  `agent_events` holds well over 100,000 rows for that window.
- The remaining path is raw SQL against `turn_end` payloads with a JSON
  extraction function. This path needs table and column knowledge the
  operator does not have.

The aggregation unit is also wrong for the question a coordinator asks.
`prism stats` reports one row per incarnation. The unit a coordinator
reasons about is the **train**: a worker session plus its
`~review-N-<agent>` children, rolled up as one piece of work. Today, that
roll-up is assembled by hand.

## 2. Corrections to the tracking issue

The tracking issue states some premises that this design corrects. Each
correction is a finding, not an opinion — the file:line references let a
reader verify each one directly.

### 2.1 The sandbox failure was a rendering stub, not a data gap (fixed by #2582)

`prism stats <session>` inside a sandbox used to fail not because the data
was unreachable, but because `renderIncarnationDetail` in
`cmd/stats_render.go` printed one fixed line instead of rendering the token
fields:

```
token data requires host DB access (use prism stats on host for full detail)
```

Issue #2582 (part 1/4 of this tracking issue) fixed this: both the
host-direct and sandbox-proxy detail renderers now read token/cost data from
the persisted-or-computed `spawn_outcome` row via `db.CompareRunOutcome`, so
`prism stats <session>` renders identical Token Usage output inside and
outside a sandbox. This section is kept as the finding that motivated the
fix, and as the precedent for the pattern below.

The host API already serves the data this stub needed. `internal/sidecar/
host_api.go` exposes `/stats` (session-level and event-level views),
`/db/query`, `/db/schema`, and `/db/tables`. Sandbox routing through this
socket is an established pattern — `prism stats compare`, `prism stats
abtest`, `prism stats --abtest`, and now `prism stats <session>` itself, use
it (issue #2098) to render byte-identical output on the host path and the
sandbox path. `prism retro` must copy that pattern, not invent a new one.

### 2.2 Most of the data this design needs already exists

The `spawn_outcome` table (`internal/db/db.go`, roughly lines 168-206)
carries one row per session instance with the fields a retrospective needs:

- `tokens_input_total`, `tokens_output_total`, `tokens_cache_read_total`,
  `tokens_cache_write_total`, `cost_usd_total`
- `tool_call_count`, `tool_error_count`
- `permission_ask_count`, `permission_denied_count`
- `doom_loop_count`, `compaction_count`, `error_event_count`
- `review_group_id`, `review_verdict`, `review_pass_count`,
  `review_fail_count`, `review_none_count`
- `pr_number`

Output sections 1 (window totals), 2 (trains table), and 5 (waste signals)
are close to a `GROUP BY` over this one table, joined to `session_groups`
for train membership. Section 3 (review-cycle detail) needs a further join
on `session_groups.round` and a per-agent breakdown; see section 4 below for
why that join has an open dependency.

### 2.3 Section 4 needs a second, event-level query path

Section 4 (fixed-overhead accounting) asks for the shared cached-prefix size
per agent role and the total scaffolding tokens for the window. Those facts
do not live in `spawn_outcome`. `spawn_outcome` holds one summary row per
session; the fixed-overhead numbers are per-turn facts inside `msg_assistant`
payloads in `agent_events` — specifically, the cache-read and cache-write
token counts on each agent's first turn.

This breaks the tracking issue's own constraint: "must read only aggregate
event rows; no transcript reads." Reading `msg_assistant` payloads for the
first turn of every agent in the window is an event-level read, not an
aggregate-row read. This design keeps the constraint for sections 1, 2, 3,
and 5, and states plainly that section 4 is the one exception, scoped to a
single field (first-turn cache tokens) rather than a general transcript
read. Child issue 4 must implement this path as a second query, separate
from the `spawn_outcome` aggregation the other sections use.

### 2.4 The "cleaned-up review children" edge case does not occur

The tracking issue lists an edge case: a train whose review children were
cleaned up must still report the worker row, with review data marked
unavailable. This cannot happen. `prism cleanup` (`cmd/cleanup.go`) removes
the worktree, the branch, and the tmux session; it does not delete the
`spawn_outcome` row. The row persists in the database after cleanup, which
is exactly the property `prism stats --group-by profile|model|...` already
relies on for long-term querying. This design drops that edge case from the
acceptance criteria of the child issues. If a future change makes cleanup
delete `spawn_outcome` rows, this edge case returns and must be re-added.

## 3. Train identity: follow the foreign key, not the session name

A train is a worker session plus its `~review-N-<agent>` children, rolled up
as one row.

`session_groups` has the shape `(group_id PK, parent_session NOT NULL,
pr_number, round, delivered_at)`. `parent_session` is the friendly session
name the operator already types. `prism retro <session-name>` resolves with:

```sql
SELECT * FROM session_groups WHERE parent_session = ?
```

`group_id` never surfaces to the operator. The operator types a session
name; the command never asks for a UUID. This is a hard requirement, not a
convenience: any interface that asks the operator to supply a `group_id`
fails the "must not require the operator to know a table or column name"
constraint in the tracking issue.

`round` is a native column on `session_groups`. Use it directly to group
review cycles — do not infer round number from session-name parsing.
`delivered_at` is the authoritative end-of-life signal for a round. A round
with `delivered_at IS NULL` is still in progress or never completed; a round
with `delivered_at` set has been delivered to the worker. Section 3 (child
issue 3) uses `delivered_at` to state, per round, whether it counted toward
the review outcome or not.

### 3.1 Investigators are solo trains

An investigator session (`<invoker>~investigate-<slug>`) has no row in
`session_groups` — it is not a review group member and it is not a worker
with review children. It is identifiable only by its naming convention. Do
not attribute an investigator to a worker train; the database records no
such link, and inventing one would be a guess presented as data. Each
investigator is a train of one: itself, with no review-cycle section.

### 3.2 A/B legs are two separate trains

An A/B pair (`prism spawn --abtest`) produces two independent trains, not
one. They exist to be compared against each other — `prism stats compare`
and `prism stats abtest` already do that comparison. `prism retro` reports
each leg as its own row in the trains table; it does not merge them.

## 4. Command surface

```bash
prism retro                     # last 24h, current repo, all trains
prism retro --since 2026-08-02  # explicit window (ISO 8601 or YYYY-MM-DD)
prism retro --days 7            # relative window
prism retro <train-session>     # deep dive on a single train
prism retro --repo <name>       # cross-repo scoping
prism retro --json              # scriptable, same data
```

`prism retro <train-session>` resolves the same way `prism checkin` and
`prism stats` already resolve a session argument — see `resolveSessionArg`
in `cmd/stats_render.go` — so the resolution rules an operator already knows
apply here without change.

## 5. Output sections and their data source

Sections are numbered to match the tracking issue, in reading order.

| # | Section | Primary source | Notes |
|---|---|---|---|
| 1 | Window totals | `spawn_outcome`, aggregated | States the context-re-read share: cache-read tokens as a percentage of total token volume. |
| 2 | Trains table | `spawn_outcome` joined to `session_groups` | One row per train: name, profile tier, worker cost, review cost, review-cycle count, total, share of window. |
| 3 | Review-cycle detail | `agent_events` (turns/cost/tokens) joined to `session_groups.round`; verdicts via `db.GroupResultsAll` + `review.ClassifyRound` (NOT `spawn_outcome`, NOT the live `db.GroupResults`) | Per cycle, per agent: cost, turns, verdict. Landed as issue #2584 — see section 6. |
| 4 | Fixed-overhead accounting | `agent_events`, `msg_assistant` payloads, first turn only | The one section that reads event-level data. See section 2.3. |
| 5 | Waste signals | `spawn_outcome`, aggregated | `doom_loop_count`, `tool_error_count`, `permission_ask_count`, `permission_denied_count`, stalls, non-converging cycles. Zero counts are stated explicitly, not omitted. |

`--json` emits the same data as the table form: snake_case keys, RFC 3339
timestamps, empty collections as `[]`.

## 6. Sequencing and why it is ordered this way

Four child issues, in this order. Each issue leaves `main` shippable on
merge — no child depends on a child that has not landed, except where
stated.

1. **Discoverability and stats proxy fix.** Fixes `cmd/stats_render.go:141`
   so the sandbox proxy path renders real token data instead of the stub.
   Updates the `prism` skill to document `prism db`, and to state the
   sandbox `sqlite3` trap: running `sqlite3` directly against the database
   path inside a sandbox opens an empty shadow database and returns zero
   rows for every table, silently, with no error. This issue has no
   dependencies and ships first because sections 1, 2, and 5 of `prism
   retro` are unusable from a sandbox until the token data actually
   renders.

2. **`prism retro` core.** Sections 1, 2, and 5, plus `--json` and the
   empty-window edge case. Implements the FK-based train resolution and the
   investigator/A-B special cases from section 3 above. Depends on nothing
   beyond issue 1 landing so the sandbox path works end to end. Landed as
   issue #2583 (this part carries `Closes #2529`, since #2585 — the original
   part 4 — was closed as not-planned). Per-session token/cost/waste data
   comes from `db.CompareRunOutcome`, which returns the persisted
   `spawn_outcome` row or, for a terminal session with no row yet, an
   on-the-fly `ComputeSpawnOutcome` aggregation over `agent_events`. That
   fallback is what makes review-agent sessions countable regardless of
   whether cleanup has written their rows (`WriteSpawnOutcomeCascade`, #2591)
   or the rows predate that change.

3. **Section 3, review-cycle detail.** Depended on
   [#2573](https://github.com/prismatic-koi/nixos-config/issues/2573) (merged
   as PR #2580), which classifies a round with a missing verdict (no-start or
   mid-round infrastructure failure). Landed as issue #2584. Two corrections
   surfaced after a dogfood retro, before implementation started:

   - **Route taken for per-agent metrics: `agent_events`, not
     `spawn_outcome`.** Review-agent sessions almost never carry a
     `spawn_outcome` row (measured coverage before #2591:
     41 of 3,384, 1.2% — `WriteSpawnOutcome` is called for the parent
     instance only, with no cascade to review children before that issue).
     `internal/db/retro.go`'s `SessionEventAggregates` aggregates turns and
     token/cost totals directly from `agent_events`, COALESCEd against NULL
     fields, keyed by `session_name`.
   - **Verdicts: `db.GroupResultsAll`, not the live `db.GroupResults`.**
     `GroupResults` filters `WHERE ended_at IS NULL` — correct for the
     in-flight monitor loop, wrong for a historical read: by the time an
     operator runs `prism retro`, every review-agent `agent_status` row for a
     completed round is closed (measured on the live DB: 3,290 of 3,290
     review `agent_status` rows are closed — issue #2594). Reading
     `GroupResults` here would return an empty map for every historical round
     and mislabel every agent as having no verdict. `GroupResultsAll` (same
     query, no `ended_at` filter) plus `review.ClassifyRound` — the
     same classifier #2573 uses for the live delivery message — keeps the
     historical read and the live read on one classification, so a round
     #2573 marks non-counting renders that way here too.
   - **"No review data recorded" is restored and distinct from a recorded
     zero.** A round whose `session_groups` row was registered but never got
     an `agent_status` member (the group's own field is empty) renders "no
     review data recorded for this round"; an agent that ran and genuinely
     cost $0.00 (subscription profile) renders that cost, not "no data".
     These never collapse into the same message (issue #2584 correction 2 —
     supersedes section 2.4 above, which is only about `spawn_outcome` row
     survival, not about whether a round ever got members in the first
     place).

4. **Section 4, fixed-overhead accounting.** The event-level query path over
   `msg_assistant` payloads described in section 2.3. Depended on issue 2
   because it extends the same command surface. Closed as not-planned
   (#2585): its measurement premise did not reproduce.

### 6.1 Closure policy

The original plan had only the last part carry `Closes #2529`. That changed
when part 4 (#2585) was closed as not-planned: it carried the closure, so
the responsibility moved to part 2 (#2583), which now carries `Closes #2529`.
Parts 1 and 3 carry `Refs #2529`.

## 7. Constraints carried over from the tracking issue

- The command must work inside a sandbox by routing through the host-API
  socket, the same way `prism db` and `prism stats compare` already do.
- The command must not require the operator to know a table or column name.
- Sections 1, 2, 3, and 5 read only aggregate rows from `spawn_outcome` and
  `session_groups`. Section 4 is the sole exception — see section 2.3.
- `--json` emits the same data as the table, snake_case keys, RFC 3339
  timestamps, empty collections as `[]`.

## 8. Known limitation

This design comes from one retrospective on one morning of work. The
section list is the part most likely to need revision once the command runs
against different shapes of work. Expect to refine it after the four child
issues land.

## 9. References

- Tracking issue: [#2529](https://github.com/prismatic-koi/nixos-config/issues/2529)
- Review-verdict classification in flight:
  [#2573](https://github.com/prismatic-koi/nixos-config/issues/2573)
- `internal/db/db.go` — `spawn_outcome` and `session_groups` schema
- `internal/sidecar/host_api.go` — `/stats`, `/db/query`, `/db/schema`,
  `/db/tables` handlers
- `cmd/stats_render.go` — the sandbox rendering stub and
  `resolveSessionArg`
- `cmd/cleanup.go` — confirms `spawn_outcome` rows persist after cleanup
