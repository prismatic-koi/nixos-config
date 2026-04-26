# C.3 — Outcome-capture review (verdict, rubric, comparison aggregations)

**Track:** C (A/B testing) — Wave 2 follow-up.
**Issue:** #1086.
**Source corpus:** `docs/architecture-inventory.md` §8.4 (event types), §8.5 (status columns), §8.6 (aggregations); `internal/db/db.go` (`GroupMemberResult`, `GroupResults`); `internal/review/review.go` (`VerdictKind`, `AssessPassed`); `cmd/stats.go` (per-axis aggregations today).
**Builds on:** C.1 (#1075, landed in #1095) — extends C.1's `sessions.outcome_summary` JSON hook into a structured `spawn_outcome` table.
**Related:** design doc #1072 (`docs/reviews/000-design-narrow-review-series.md`); RFC #691 §5 (`prism spawn --abtest`); sibling Wave 2 issues C.2 (#1085, per-role model variation), C.4 (#1087, prompt + skills as spawn inputs).

---

## 1. Context recap

C.1 established the *input* side of the A/B story: a `spawn_inputs` table keyed on `instance_id` captures every flag value the user passed, plus thin JSON hooks for sibling Track C work (`outcome_summary`, `model_variant_overrides`, `skills_manifest_hash`, `prompt_template_hash`). C.1 deliberately deferred the outcome-side detail to this proposal — `sessions.outcome_summary` is a JSON column placeholder, marked as "C.3 owns this" in C.1 §4.6.

Today the only *outcome* signals prism captures per spawn are:

- `agent_status.state` and `sessions.end_state` — the high-level lifecycle terminals (`finished` / `error` / `interrupted` / `deleted`). Per inventory §8.5 there is no rubric-grading column.
- `db.GroupResults(groupID)` — used by review fan-out (`internal/review/review.go`) to read per-member terminal state and last assistant message. `GroupMemberResult` (`internal/db/db.go:2395`) carries `{SessionName, RootAgent, State, LastMessage}`. No verdict, no rubric.
- `review.AssessPassed(text)` (`internal/review/review.go:1749`) — parses an explicit `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` marker out of an agent's last assistant message. Returns `(passed bool, kind VerdictKind)` where `VerdictKind` is `{None, Pass, Fail}`. **The assessment runs at read time, on demand, in the review caller — no verdict column is written to any DB table.**
- Per-event metrics derivable from `agent_events` (token cost from `msg_assistant.cost`, tool-call count from `type='tool_call'` rows, error count from `type='error'`, latency from `created_at`). These power `prism stats session` and friends per inventory §8.6 but are not summarised onto a per-spawn outcome row.

For comparison-style A/B testing (`prism spawn --abtest opencode pi "<task>"`, RFC #691 §5) to be useful, every spawn needs:

1. An *outcome record* — a stable place to hang "did this run succeed?" beyond "did the process exit cleanly?" — that survives across review-style and ordinary spawns.
2. *Per-axis aggregations* that summarise the run on a fixed set of universal dimensions, so a side-by-side `prism stats compare` query is a join, not a re-derivation from raw events.

This document addresses both. It is proposal-only — no Go file, no schema migration, no command implementation lands here. The deliverables are: (a) a `spawn_outcome` SQL sketch that extends C.1's foundation, (b) a per-axis aggregation table for at least the seven axes called out in #1086's scope, (c) a `prism stats compare` CLI sketch, and (d) an explicit position on rubric grading.

---

## 2. Outcome dimensions

The issue framing names three:

### 2.1 Process-level — "how did the OS-shaped lifecycle end?"

What can prism observe by watching the agent process and the surrounding tmux+sidecar plumbing.

| Signal | Captured today? | Source |
|---|---|---|
| Terminal state (`finished` / `error` / `interrupted` / `deleted`) | yes | `agent_status.state` and `sessions.end_state` (denormalised at session-end time). |
| `started_at` / `ended_at` (wall-clock) | yes | `sessions.started_at`, `sessions.ended_at`. |
| Duration | derivable | `ended_at - started_at`. |
| Exit code of the agent process | **no** | The sidecar observes process exit but does not write the exit code to any column or event. The state transition to `finished` / `error` is the only persisted artefact. **[gap]** |
| Number of `interrupted` transitions during the run | derivable | Count of `state_change` events with `state="interrupted"` in `agent_events`. Not currently surfaced. |
| Number of `compaction` events | derivable | Count of `type='compaction'` events in `agent_events`. Surfaced by `prism stats summary` but not on a per-session outcome row. |
| Number of `error` events | derivable | Count of `type='error'` events in `agent_events`. |
| Permission asks / denials | derivable | Counts of `type='permission_ask'` / `type='permission_denied'` events. Aggregated cross-session by `prism stats asks` / `denials` but not summarised per-session. |

**Process-level is the cheapest dimension to capture cleanly** — every signal here is either already on a column or one `COUNT(*)` away from being on one. The only true gap is per-session exit code, which would need a new sidecar write at process-exit time. **[uncertain — whether the sidecar's process-exit observation point has access to the exit code at all under all isolation modes (host, bwrap, podman); under podman the process is a container and exit-code propagation is mediated by `podman wait`. Verify against `internal/sidecar/sidecar.go` and `internal/container/podman.go` before C.3's implementation phase.]**

### 2.2 Agent-level — "did the agent achieve its goal?"

What the *external world* observed about the agent's work product. These are bus-and-`gh`-observable signals beyond mere process exit.

| Signal | Captured today? | Source / Path |
|---|---|---|
| Did the agent open a PR? | partial | `pending_merges` rows are keyed on PR number; their existence implies a PR was opened *and queued for merge*. PRs opened without `prism merge` are invisible to the DB. **[gap — partial]** |
| Did the PR get merged? | partial | `pending_merges.merged_at` is non-null when the merge-queue completed. Same caveat — only PRs that went through `prism merge` are observable. **[gap — partial]** |
| Did the agent push a branch? | **no** | The branch-push event happens out-of-band via `gh` invocations from inside the worktree. Prism would have to poll `git ls-remote` or watch a post-push hook. **[gap]** |
| Did the user reply positively in a follow-up? | **no** | `bus_messages` records the inter-session prompt traffic, but there is no sentiment column and no convention for "reply to last spawn outcome". Even a manual rubric reply is just another `bus_messages` row indistinguishable from any other prompt. **[gap]** |
| Did the agent's work pass review? | partial | If a review group was spawned for the session (`db.HasReviewGroup`, `db.GroupResults`), the per-member `AssessPassed(LastMessage)` outcomes can be aggregated into a "passed/failed review" verdict. But that verdict is computed on demand by the review caller — there is no column on the *parent* session that records "I was reviewed and the review verdict was X". **[gap — derivable but not stored]** |
| Did the worker hit the doom-loop detector? | derivable | `count(agent_events WHERE type='doom_loop_detected')`. Aggregated cross-session by `prism stats doomloops` but not summarised per-session. |

**Agent-level is the dimension where prism is most blind today.** The strongest agent-level signal that already exists — review-group verdict — is not surfaced as a per-spawn outcome. The PR-and-merge story works only when the merge-queue path is taken; PRs opened by the agent and merged manually by the human are invisible.

For C.3's purposes the proposal below stores the *capturable* agent-level signals (PR number from merge-queue, merge timestamp, review-verdict roll-up when a child review group exists) and explicitly leaves the rest as `[gap]` flags rather than fabricating coverage.

### 2.3 Rubric-level — "what is the synthesised judgement on the run?"

A *post-hoc* judgement of the run's quality, structurally distinct from process-level (which is mechanical) and agent-level (which is observable). The canonical example is review fan-out today: a review agent reads the full session transcript and emits `<verdict>PASS</verdict>` or `<verdict>FAIL</verdict>` plus blocking-issue narrative, which `AssessPassed` parses.

The same shape — synthetic verdict from an LLM that read the run — could be applied to *any* spawn after termination, not just review spawns. A `prism rubric <session>` invocation is the obvious next step but is **not** in scope here (issue #1086 names this out-of-scope). What *is* in scope: the *shape* of where a rubric output would land if and when the mechanism is built. See §6.

Rubric outputs come in (at least) three flavours:

- **Binary verdict** (`pass` / `fail`) — what `review.AssessPassed` produces today.
- **Numeric score** (e.g. 0.0–1.0, or 1–5 stars) — what a future "did this run satisfy its acceptance criteria?" grader might produce.
- **Structured rubric** (a per-criterion vector — "code quality: 4, test coverage: 3, scope discipline: 5") — what a multi-axis grader might produce.

The schema below carves out room for all three without committing to any specific grading mechanism. **The mechanism question is C.3-deferred; the storage question is in scope.**

---

## 3. Position on rubric grading

**Rubric grading is deferred. The schema in §4 reserves the slots; the mechanism is out of scope.**

The case for deferral, argued rather than punted:

1. **Mechanism design depends on the grader's *prompt* and that prompt's persistence story belongs to C.4 (#1087, prompt + skills as spawn inputs).** A `prism rubric <session>` invocation is itself a spawn — it loads a grading prompt, runs an agent, parses output. The prompt template for that grader is exactly the kind of artefact C.4 will design persistence for. Building the rubric mechanism before C.4 lands risks designing the prompt-storage shape twice.
2. **The verdict-parsing shape is already proven** by `review.AssessPassed` — `<verdict>PASS</verdict>` markers, default-to-fail, parse-from-last-assistant-message. A rubric mechanism should reuse that contract verbatim for the binary-verdict flavour, and the numeric/structured flavours are extensions of the same parser. There is no design risk in waiting to build the mechanism — the *interface* (what the grader emits, how the parser reads it) is already settled by review.
3. **The blocking decision for A/B usefulness is the comparison view, not the rubric.** The seven aggregation axes in §5 (token cost, tool-call count, time-to-first-event, time-to-finished, error count, state-transition pattern, permission-denial count) are all computable today from `agent_events` without any rubric. `prism stats compare run-A run-B` becomes valuable *immediately* on those axes. Adding a rubric column later is a strictly additive enrichment.
4. **The mechanism has a non-trivial cost surface that deserves its own design.** A grader is itself a spawn, with token cost, isolation choices, model choice, prompt design, and output parsing. Rolling that surface into C.3 would balloon the proposal into the implementation work the issue's "no implementation work" AC explicitly forbids.

**What in scope means concretely:** §4's `spawn_outcome` table includes `rubric_verdict TEXT`, `rubric_score REAL`, and `rubric_breakdown TEXT` (JSON) columns — all nullable, all defaulting to NULL — so that a future `prism rubric` (or any other grader, e.g. `prism review` writing the verdict back to the parent's outcome row) has a place to land without a follow-on migration. The columns are documented as "populated by a future grader; NULL for runs that have not been graded".

**What deferred means concretely:** no `prism rubric` command, no grader prompt, no "automatic post-spawn review" trigger is proposed here. A separate issue under Track C (or Track E synthesis) will own that mechanism, and will reuse the columns this proposal reserves.

This is the same shape C.1 took for `outcome_summary` and `model_variant_overrides` — reserve the slot, defer the mechanism. It worked there; it should work here.

---

## 4. Proposed `spawn_outcome` shape

**Decision: a separate `spawn_outcome` table, keyed on `instance_id`, replacing C.1's `sessions.outcome_summary` JSON column.**

Rationale for the table-vs-column choice (this is the one structural decision in this proposal worth surfacing):

- **`sessions.outcome_summary` JSON** (C.1's placeholder) is fine for a single small JSON blob. It becomes painful as soon as a comparison query needs to filter, sort, or group by an outcome axis — every such query has to `json_extract(outcome_summary, '$.tokens_total') > N`, every index has to be a JSON-expression index, and a future migration (e.g. adding a "merged_at" axis) requires an in-place JSON rewrite for back-fill.
- **A `spawn_outcome` table** with typed columns gives indexable axes for free, makes the comparison view in §6 a clean two-table join (`spawn_inputs ⋈ spawn_outcome` keyed on `instance_id`), and matches the symmetry C.1 chose for `spawn_inputs` (also a sister table to `sessions`, also keyed on `instance_id`).
- **Cost of replacing the JSON hook:** zero today — `outcome_summary` is reserved-but-empty as of #1095, no writer has been built. The migration is "drop the column, add the table". If the JSON hook had already been populated for any session, this proposal would recommend keeping both and migrating asynchronously; since it has not, a clean replacement is the cheaper path.

C.1's `sessions.outcome_summary` therefore drops; the replacement is below.

### 4.1 New table: `spawn_outcome`

```sql
CREATE TABLE IF NOT EXISTS spawn_outcome (
    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,

    -- Process-level outcome (§2.1). Populated when the session reaches a
    -- terminal state. NULL columns indicate the run is still in progress
    -- or the signal was not capturable for this run.

    end_state              TEXT,    -- denormalised from sessions.end_state for self-containedness
    exit_code              INTEGER, -- agent process exit code; NULL if unobservable under isolation mode (see §2.1 [uncertain])
    duration_ms            INTEGER, -- ended_at - started_at, in milliseconds
    interrupted_count      INTEGER NOT NULL DEFAULT 0,  -- count of state_change events with state='interrupted' during the run
    compaction_count       INTEGER NOT NULL DEFAULT 0,
    error_event_count      INTEGER NOT NULL DEFAULT 0,  -- count of agent_events.type='error'
    permission_ask_count   INTEGER NOT NULL DEFAULT 0,
    permission_denied_count INTEGER NOT NULL DEFAULT 0,
    doom_loop_count        INTEGER NOT NULL DEFAULT 0,

    -- Agent-level outcome (§2.2). All optional; presence of a non-NULL
    -- pr_number indicates the run was tracked by the merge-queue path.

    pr_number              INTEGER, -- PR opened during/by this run (from pending_merges); NULL if no PR or PR opened out-of-band
    pr_merged_at           INTEGER, -- ms epoch; NULL if PR never merged or merged out-of-band
    review_group_id        TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
                                    -- the review group spawned to grade this session, when one exists
    review_verdict         TEXT,    -- 'pass' | 'fail' | 'mixed' | NULL; rolled up from review_group's per-member AssessPassed
    review_pass_count      INTEGER, -- number of review-group members that returned VerdictPass
    review_fail_count      INTEGER, -- number of review-group members that returned VerdictFail
    review_none_count      INTEGER, -- number of review-group members that returned VerdictNone (no marker)

    -- Rubric-level outcome (§2.3). Reserved for a future grader (see §3).
    -- All NULL until a grading mechanism (out of scope here) populates them.

    rubric_verdict         TEXT,    -- 'pass' | 'fail' | NULL; mirrors review.VerdictKind
    rubric_score           REAL,    -- numeric score (e.g. 0.0-1.0); NULL if not graded or grader emits no score
    rubric_breakdown       TEXT,    -- JSON; per-criterion structured rubric (e.g. {"code_quality": 4, "scope": 5}); NULL if not graded
    rubric_grader          TEXT,    -- session_name of the grading spawn; NULL if not graded

    -- Per-axis aggregations (§5). Pre-computed at session-end time so the
    -- comparison query is a join not a recomputation over agent_events.
    -- Computed-once, never updated; if events arrive after the row is written
    -- (rare; only via late tmux-hook backfill) the row is regenerated.

    tokens_input_total       INTEGER NOT NULL DEFAULT 0,
    tokens_output_total      INTEGER NOT NULL DEFAULT 0,
    tokens_cache_read_total  INTEGER NOT NULL DEFAULT 0,
    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
    cost_usd_total           REAL    NOT NULL DEFAULT 0,
    tool_call_count          INTEGER NOT NULL DEFAULT 0,
    tool_error_count         INTEGER NOT NULL DEFAULT 0,
    msg_assistant_count      INTEGER NOT NULL DEFAULT 0,  -- assistant turns
    time_to_first_event_ms   INTEGER, -- min(agent_events.created_at) - sessions.started_at; NULL if no events recorded
    time_to_finished_ms      INTEGER, -- == duration_ms when state='finished'; NULL otherwise

    -- Audit fields.

    computed_at            INTEGER NOT NULL,  -- ms epoch when this row was last (re)computed
    schema_version         INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_spawn_outcome_end_state    ON spawn_outcome(end_state);
CREATE INDEX IF NOT EXISTS idx_spawn_outcome_pr_number    ON spawn_outcome(pr_number);
CREATE INDEX IF NOT EXISTS idx_spawn_outcome_review_group ON spawn_outcome(review_group_id);
```

**Rationale per column block:**

- **Process-level columns** (§2.1) are all derivable from `agent_events` + `sessions` columns at session-end time. None require new event types; this is purely a roll-up. Storing them on `spawn_outcome` rather than recomputing per query keeps the comparison view fast and lets indexes serve the `WHERE end_state = ...` and `ORDER BY error_event_count DESC` patterns.
- **Agent-level columns** (§2.2) are *deliberately conservative* — only the signals that are *actually* capturable today land here. `pr_number` and `pr_merged_at` come from `pending_merges`. `review_*` columns come from rolling up `db.GroupResults` over `AssessPassed`. The §2.2 gaps (push-without-PR, follow-up sentiment, manual merge) are not synthesised — they remain `NULL`, with the `[gap]` flags documented in §2.2 carrying through.
- **Rubric-level columns** are reserved per §3 and default to NULL.
- **Per-axis aggregation columns** (§5) are pre-computed at session-end so a comparison query touches at most three tables (`spawn_inputs`, `spawn_outcome`, `sessions` for `started_at`/`ended_at`) instead of fanning out to `agent_events`. Token totals come from summing `payload.MsgAssistant.{InputTokens,OutputTokens,CacheReadTokens,CacheWriteTokens,Cost}` across all `msg_assistant` events for the instance.
- **`computed_at` and `schema_version`** make late-event backfill detectable and let a future `prism stats outcome --recompute` operate idempotently.

### 4.2 What this delta does *not* add

- **No `verdict_text` or `rubric_narrative` columns.** The narrative explanation (the prose a reviewer or grader emits alongside their verdict) lives in the *grading session's* `msg_assistant` events. Duplicating it onto `spawn_outcome` would create a synchronisation hazard. The `review_group_id` and `rubric_grader` foreign keys give the comparison view a way to *navigate* to the narrative without storing it twice.
- **No "user satisfaction" column.** `[gap]` per §2.2 — `bus_messages` does not carry a sentiment signal today, and inferring sentiment from a free-form follow-up prompt is the kind of heuristic that belongs in the rubric mechanism (deferred, §3) not the schema.
- **No per-tool breakdown.** `tool_call_count` is a single integer; "how many `bash` calls vs `edit` calls" is recoverable from `agent_events.payload` and is harness-specific (per C.1 §5.2). Deferred to B.5's `normalised_payload` projection landing.
- **No "branch pushed" column.** `[gap]` per §2.2 — observing a push requires either a post-push hook or a `git ls-remote` poll, neither of which exists today. Adding the column without a writer would lie about coverage.

### 4.3 Write path (informational; out of scope to implement)

For C.3's reviewers: the row is written **once at session-end time**, by a new `db.WriteSpawnOutcome(instanceID)` call invoked from the same code path that sets `sessions.ended_at`. Late events (a post-end-of-session `tmux_session_end` arriving after the row is written, which is rare but observable) trigger a `bumped computed_at` recompute. The write is idempotent; an `INSERT OR REPLACE` is sufficient.

This proposal does **not** specify which package owns the writer — that is an implementation-issue decision. The natural candidates are `internal/session/` (which already owns session-end semantics) or `internal/db/` (which owns the cross-cutting roll-up query). C.3 deliberately leaves that open.

---

## 5. Per-axis aggregation table

The seven axes named in #1086's scope, plus the universal-metrics overlap with C.1 §5.1, expanded into a single table. Each row gives: how the metric is computed today, where it lands on `spawn_outcome` (per §4.1), whether it is universal across harnesses (per C.1's classifier), and what the gap is — if any.

| # | Axis | Computed from | `spawn_outcome` column | Universal? (per C.1 §5.1) | Capturable today? |
|---|---|---|---|---|---|
| 1 | **Token cost** | `sum(MsgAssistant.{InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, Cost})` over `agent_events WHERE type='msg_assistant'` for the instance | `tokens_input_total`, `tokens_output_total`, `tokens_cache_read_total`, `tokens_cache_write_total`, `cost_usd_total` | universal *in principle* — depends on §4.3's `normalised_payload` for cross-harness; opencode-only otherwise. | yes for opencode (cost lives in `payload.MsgAssistant.Cost`); **[uncertain]** for PI per C.1 §5.1 — PI may report cost only at session-end or not at all. |
| 2 | **Tool-call count** | `count(agent_events WHERE type='tool_call' AND instance_id=...)` | `tool_call_count` | universal — count is harness-agnostic per C.1 §5.1. | yes. |
| 3 | **Time-to-first-event (TTFE)** | `min(agent_events.created_at) - sessions.started_at` | `time_to_first_event_ms` | universal per C.1 §5.1. | yes. |
| 4 | **Time-to-finished** | `sessions.ended_at - sessions.started_at` (when `end_state='finished'`); NULL otherwise | `time_to_finished_ms` | universal per C.1 §5.1. | yes. |
| 5 | **Error count** | `count(agent_events WHERE type='error' AND instance_id=...)` | `error_event_count` | universal per C.1 §5.1. | yes. |
| 6 | **State-transition pattern** | the *sequence* of `state_change` event payloads, plus the *count* of `interrupted` transitions during the run | `interrupted_count` (count form); the full sequence is not stored on `spawn_outcome` (it remains in `agent_events` for queries that want it) | universal per C.1 §5.1 — `agent_status.state` values are harness-agnostic. | yes. |
| 7 | **Permission-denial count** | `count(agent_events WHERE type='permission_denied' AND instance_id=...)` | `permission_denied_count` | **[uncertain]** for cross-harness per C.1 §5.1 — PI's permission model may not surface a permission-denied event. Within opencode: universal. | yes for opencode; **[uncertain]** for PI. |

**Beyond the seven explicitly named axes**, `spawn_outcome` captures three more that fall naturally out of the same roll-up and have direct A/B value:

| # | Axis | Computed from | `spawn_outcome` column | Notes |
|---|---|---|---|---|
| 8 | **Tool-error count** | `count(agent_events WHERE type='tool_error' AND instance_id=...)` | `tool_error_count` | Universal per C.1 §5.1. Cheap addition; useful for "did A's tool calls succeed more often than B's". |
| 9 | **Assistant turn count** | `count(agent_events WHERE type='msg_assistant' AND instance_id=...)` | `msg_assistant_count` | Universal in count; content is harness-shaped. |
| 10 | **Doom-loop count** | `count(agent_events WHERE type='doom_loop_detected' AND instance_id=...)` | `doom_loop_count` | Universal *output* of a sidecar-side heuristic (C.1 §5.1 [uncertain] flag carries through — the heuristic itself may not fire usefully on PI's event stream). |

**Two axes from §2.1 that are stored but not "aggregations" in the per-event sense:**

- `compaction_count` — stored on `spawn_outcome`, surfaced because a high count is a comparison-relevant signal ("harness A's runs compact 3× more often than harness B's").
- `permission_ask_count` — stored alongside `permission_denied_count` so the comparison view can show ask-and-denial together (an A/B side that asks rarely-and-is-denied vs a side that asks often-and-is-allowed is a substantively different behaviour profile).

### 5.1 Axes that are *not* currently capturable from existing data (per AC [edge-case])

| Axis | Why not capturable | Where it lands when it becomes capturable |
|---|---|---|
| Per-spawn agent-process exit code | Sidecar does not write the exit code to any event or column today; under podman, exit-code propagation depends on `podman wait` semantics (see §2.1 [uncertain]) | `spawn_outcome.exit_code` (column reserved) |
| "Did the agent push a branch (without opening a PR)?" | No post-push hook, no `git ls-remote` poll (§2.2) | Would need a new `branch_pushed_at` column or a new event type; not reserved here, treat as future enrichment |
| User satisfaction / follow-up sentiment | `bus_messages` carries no sentiment signal; no convention for "this prompt is a reply to last spawn outcome" (§2.2) | Would belong in the rubric mechanism (deferred, §3) or as a separate `bus_messages.sentiment` column; not reserved here |
| Per-tool breakdown of tool-call count | `agent_events.payload.ToolCall.Name` exists but is harness-specific until C.1's `normalised_payload` lands | When B.5 + `normalised_payload` ship: a `spawn_outcome.tool_call_breakdown TEXT` (JSON `{"bash": 12, "edit": 5}`) column would land; not reserved here |
| Token cost per *role* in multi-role runs | `payload.MsgAssistant` has `agent_name` per turn but `spawn_outcome` aggregates total. Per-role roll-up is recoverable from `agent_events` but not summarised. | When C.2 lands per-role variation, a sister `spawn_outcome_role` table keyed on `(instance_id, role)` would land; not reserved here |
| PR-merged-out-of-band | `pending_merges` is the only source; manual `gh pr merge` invocations are invisible | Would need either a `gh` API poll on session-end or a webhook listener; not reserved here |
| Did the agent escalate to coordinator? | No event type for "spawned an escalation prompt"; recoverable only from `bus_messages` body inspection | Would need either a new event type or a coordinator-side annotation; not reserved here |

The principle: **reserve a slot when the writer's path is plausibly cheap and the signal is high-value (exit code, rubric); do not reserve when the writer would itself be a substantial design decision (push detection, sentiment analysis)**. Under-reserving is forgiving — adding a column later is a one-line ALTER. Over-reserving creates schema noise that future readers misinterpret as "this is captured" when it is in fact NULL-forever.

---

## 6. `prism stats compare` subcommand shape

### 6.1 CLI signature

```
prism stats compare [flags] <run-A> <run-B> [<run-C> ...]
```

Where each `<run-X>` is either:

- a session name (`prism-1086-c3-outcome-capture-2026-04-26T1430Z`),
- a 36-char `instance_id` (or unambiguous prefix),
- a `session_groups.group_id` with `kind='abtest'` (resolves to *all* members of the group; combinable with other args).

Flags:

```
  --axes <list>     comma-separated axes to render. Default: all 10 axes from §5.
                    Names match spawn_outcome column names with the _total / _count
                    suffixes stripped (e.g. "tokens_input,cost_usd,tool_call,duration").
  --format <fmt>    table | json | csv. Default: table.
  --diff-only       hide rows where every run has the same value (useful for
                    --abtest pairs that came from identical inputs).
  --sort <axis>     order columns by this axis's value descending. Default: input order.
  --include-inputs  prepend a "spawn inputs" block showing the spawn_inputs row
                    for each run (so the comparison shows *what varied* alongside
                    *what differed*). Default: on for 2-run comparisons, off for 3+.
  --include-rubric  include rubric_* columns even when they are NULL. Default: off
                    (rubric columns are hidden until the grader mechanism lands).
```

### 6.2 Output format (default: table)

```
$ prism stats compare prism-task-anthropic-2026-04-26T1430Z prism-task-gemini-2026-04-26T1432Z

Spawn inputs (differences only):
                              run-A                      run-B
profile_name                  anthropic                  gemini-hybrid
model_flag                    -                          -
variant_flag                  -                          -
harness_flag                  opencode                   opencode
prompt_text                   "implement feature X"      "implement feature X"

Process-level outcomes:
                              run-A                      run-B
end_state                     finished                   finished
duration_ms                   847,332 (14m 7s)           1,204,889 (20m 4s)
exit_code                     0                          0
interrupted_count             0                          1
compaction_count              0                          1
error_event_count             0                          2

Agent-level outcomes:
                              run-A                      run-B
pr_number                     1099                       1100
pr_merged_at                  2026-04-26T15:02:11Z       (not merged)
review_verdict                pass                       mixed
review_pass_count             5                          3
review_fail_count             0                          2

Per-axis aggregations:
                              run-A                      run-B            Δ
tokens_input_total            142,889                    218,447          +52.9%
tokens_output_total            18,221                     26,103          +43.3%
cost_usd_total                $1.4622                    $1.0411          -28.8%
tool_call_count                   47                         63          +34.0%
tool_error_count                   2                          5          +150.0%
msg_assistant_count               18                         24          +33.3%
time_to_first_event_ms         1,204                      1,891          +57.0%
time_to_finished_ms          847,332                  1,204,889          +42.2%
permission_ask_count               3                          5          +66.7%
permission_denied_count            0                          1          +∞
doom_loop_count                    0                          0           —

Rubric-level outcomes: (none recorded; pass --include-rubric to show NULLs)
```

The Δ column shows percentage delta of run-B relative to run-A (B's vantage; pairwise only). For 3+ runs the Δ column is omitted and a `MIN/MAX` annotation appears at the row level.

### 6.3 Output format (json)

```json
{
  "runs": [
    {"label": "run-A", "session_name": "...", "instance_id": "...", "spawn_inputs": {...}, "spawn_outcome": {...}},
    {"label": "run-B", "session_name": "...", "instance_id": "...", "spawn_inputs": {...}, "spawn_outcome": {...}}
  ],
  "diffs": {
    "spawn_inputs": ["profile_name", "harness_flag"],
    "spawn_outcome": ["duration_ms", "interrupted_count", ...]
  }
}
```

The JSON form is the source of truth; the table is a renderer over the same payload. Comparison tooling (e.g. a future `prism dashboard` comparison panel) consumes the JSON form.

### 6.4 Sister command: `prism stats abtest <group_id>`

Per C.1 §6: `prism stats abtest <group_id>` is the same as `compare` but takes a `session_groups.group_id` with `kind='abtest'` and renders all members of the group. The implementation is `compare` with the args resolved from `session_groups`. C.3 does not propose a separate output format — `abtest` reuses §6.2 / §6.3 verbatim.

### 6.5 Out of scope for this proposal

- The exact column-width / colour-coding rules of the table renderer.
- Whether `--diff-only` should show a "sameness summary" line ("3 axes identical") at the bottom.
- Pagination and terminal-width handling for very wide multi-run comparisons.
- Whether `prism stats compare` accepts a `--baseline <run>` flag to fix one run as the comparison anchor (vs the default first-positional-arg-as-baseline).
- Whether non-existent or still-running runs should error or render placeholders.

These are renderer-implementation choices the implementation issue will settle. The CLI signature, the axis set, and the JSON shape are the contract this proposal locks in.

---

## 7. Open questions and uncertainty flags

Collected for the synthesis pass (E.1) and for the implementation phase. Flags inherited from C.1 are marked with `[from C.1]`.

1. **[uncertain]** Per-spawn process exit code under podman — `podman wait` semantics need confirmation. If the exit code is not reliably observable, `spawn_outcome.exit_code` will be NULL for podman runs and the column documentation must say so. (§2.1)
2. **[uncertain]** PI's per-message token-cost shape — does it map onto opencode's `MsgAssistant.{InputTokens,OutputTokens,Cost}` fields, or report only at session end? Affects `cost_usd_total` universality. **[from C.1 §7.2]**
3. **[uncertain]** PI's permission-event shape — does it surface `permission_ask` / `permission_denied` events, or handle permissions out-of-band? Affects `permission_ask_count` and `permission_denied_count`. **[from C.1 §7.3]**
4. **[uncertain]** PI's compaction-equivalent — affects `compaction_count`. **[from C.1 §7.4]**
5. **Open question for C.4 (#1087):** Does the rubric mechanism's grading prompt count as a "skills_manifest" input or a "prompt_template" input? Both for any grader that loads skills. The persistence shape C.4 settles on dictates how a rubric-grader's run can itself be A/B-compared.
6. **Open question for the implementation phase:** When a session is *deleted* (via `prism reset` or `prism cleanup`), should its `spawn_outcome` row delete with it (current proposal: yes, via `ON DELETE CASCADE`) or be retained with a tombstone for historical comparison? Argument for retention: a comparison query that references a deleted-but-historic run shouldn't silently drop the row. Argument against: the comparison columns (token cost, etc.) lose their context without the underlying events. The proposal defaults to CASCADE; the implementation issue may revisit.
7. **Open question for the implementation phase:** Should the `spawn_outcome` write be triggered by the *same* code path that writes `sessions.ended_at`, or by a follow-up periodic sweep? The synchronous form is simpler; the sweep form is more resilient to crash-during-write. The proposal recommends synchronous with idempotent re-write on later events (per §4.3) but leaves the choice open.
8. **Open question for E.1 synthesis:** When the rubric mechanism (deferred per §3) eventually lands, should it write directly to `spawn_outcome.rubric_*` columns, or to a separate `rubric_runs` table that links via `rubric_grader` (the grading session_name)? The columns-on-spawn_outcome shape is cheaper to query; the side-table shape allows multiple gradings of the same spawn (which is plausibly useful — "grade this with the strict rubric and the lenient rubric"). The proposal reserves the columns for the single-grading case; the side-table is non-blocking future work.
9. **Open question for the implementation phase:** Should `review_verdict` be `'pass' | 'fail' | 'mixed' | NULL` (proposed), or expand to `'pass' | 'fail' | 'mixed' | 'partial' | 'inconclusive' | NULL`? The richer enum lets the rolled-up verdict carry more nuance from per-member `VerdictNone` outcomes. Tracked here because the choice affects every `WHERE review_verdict = ...` query downstream.

---

## 8. Summary

- **§2** decomposes outcome into three dimensions: process-level (cheap, mostly capturable today), agent-level (mostly gaps, with PR-and-merge from `pending_merges` and review-verdict from `db.GroupResults` as the only well-supported signals), rubric-level (storage-shape proposed, mechanism deferred).
- **§3** argues for deferring the rubric *mechanism* (the grader, the prompt, the trigger) on four grounds: it depends on C.4's prompt-persistence design, the verdict-parsing contract is already settled by `review.AssessPassed`, the comparison view is valuable on the seven non-rubric axes alone, and the mechanism has a non-trivial cost surface that deserves its own design issue.
- **§4** proposes a `spawn_outcome` table keyed on `instance_id`, replacing C.1's reserved `sessions.outcome_summary` JSON column. The table covers process-level signals, the capturable-today agent-level signals, the rubric reservation, and pre-computed per-axis aggregations. Columns for the §2 gaps are deliberately *not* reserved — adding them later is cheap, and reserving NULL-forever columns lies about coverage.
- **§5** maps every one of the seven axes named in #1086's scope (token cost, tool-call count, time-to-first-event, time-to-finished, error count, state-transition pattern, permission-denial count) to its computation source, its `spawn_outcome` column, and its cross-harness universality classification (carrying through C.1 §5.1's [uncertain] flags for PI). Three additional natural-fall-out axes (tool-error, assistant turn count, doom-loop) are also covered. §5.1 catalogues the axes that are **not** capturable from existing data and explains why each is held out of the schema.
- **§6** proposes `prism stats compare <run-A> <run-B> [<run-C> ...]` with three render formats (table, json, csv), a default axis set, a `--diff-only` flag, and a sister `prism stats abtest <group_id>` form. The JSON form is the contract; the table is a renderer over it. Renderer details are out of scope.
- **§7** collects all open questions, including four uncertainty flags inherited from C.1 (PI's token-cost / permission-event / compaction shape / proxy-spawn parity) and four implementation-phase choices the synthesis pass should arbitrate.

The schema delta is deliberately additive on top of C.1: a new sister table to `sessions` (alongside `spawn_inputs`), no changes to existing tables beyond dropping the reserved-but-empty `sessions.outcome_summary` column. The seven-axis aggregation set is the durable contract — the columns this proposal reserves are the ones the comparison view in §6 commits to surfacing in the first cut.
