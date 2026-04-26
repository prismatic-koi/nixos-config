# C.1 — A/B-readiness review and schema-delta proposal

**Track:** C (A/B testing) — Wave 1 anchor read.
**Issue:** #1075.
**Source corpus:** `docs/architecture-inventory.md` §8 (A/B / experimentation surface), read cover-to-cover.
**Related:** design doc #1072 (`docs/reviews/000-design-narrow-review-series.md`); RFC #691 §5 (`prism spawn --abtest`); sibling Track C issues C.2 (#1085, per-role model variation), C.3 (#1086, outcome capture), C.4 (#1087, prompt + skills as spawn inputs).

---

## 1. Context recap

The architecture inventory's §8 lays out the per-spawn experimentation surface as it exists today. Three observations from §8 frame this proposal:

- §8.1 enumerates the spawn-time flags that *vary* per-spawn behaviour. Most of those flag values are not persisted — only their resolved effects are.
- §8.3 enumerates the persistence shape across `agent_status`, `sessions`, `agent_events`, and the auxiliary tables (`bus_messages`, `pending_merges`, `session_groups`). The shape captures the *outcome* of variation but not the *intent*.
- §8.7 summarises per-axis variation support, and §8.6 enumerates `prism stats` subcommands. There is no `prism stats compare` subcommand; comparison is reconstructible only from logs.

RFC #691 §5 introduces `prism spawn --abtest <harness-a> <harness-b> "<task>"` as a first-class user-facing surface. For that to be useful, two preconditions must hold:

1. The *inputs* to each side of the experiment must be persisted in a structured way, so a comparison query is a join — not a log-trawl.
2. The set of comparison metrics must be classified by whether they are meaningful across harnesses (opencode HTTP+SSE vs PI JSONL-over-stdio) or only within one.

This document addresses both. It is a proposal-only document — no Go file or schema migration is added outside `docs/reviews/`.

Sibling Track C issues build on the schema sketch here:

- **C.2** will detail per-role model variation. The schema below leaves a `model_variant_overrides` JSON column hook that C.2 can flesh out.
- **C.3** will detail outcome capture (verdicts, rubric scores). The schema below adds an `outcome_summary` JSON hook on `sessions` that C.3 can replace with a richer shape.
- **C.4** will detail skills/prompt manifest persistence. The schema below adds a `skills_manifest_hash` and `prompt_template_hash` column hook that C.4 can specify in detail.

These hooks are deliberately thin — this proposal does not pre-empt the sibling issues' detailed designs.

---

## 2. Per-flag persistence audit

Source: inventory §8.1. Every flag listed there is addressed below. Three persistence states are used:

- **Input persisted** — the user-supplied flag value (or the fact the flag was passed) is written to a DB column or event payload.
- **Resolved-only persisted** — the *effect* of the flag (e.g. resolved model id) is written, but the *input value* is not. The intent cannot be recovered without log inspection.
- **Not persisted** — neither the input nor the effect lands in any DB table.

| Flag | Reaches | Persistence today | A/B gap |
|---|---|---|---|
| `--branch` | `SpawnOpts.Worktree`, tmux session name suffix | resolved-only — branch name is recoverable from `agent_status.worktree` (path) and the tmux session name; the flag *value* itself is not stored as a column | low — branch is identifiable from worktree path, adequate for A/B grouping |
| `--pr <n>` | drives `--branch` resolution via `gh pr view` | not persisted as PR number; only the resulting branch is observable | medium — knowing two runs were spawned against the same PR requires inferring from worktree name. Add `pr_number INTEGER` on `spawn_inputs` (see §4) |
| `--repo <name-or-path>` | `SpawnOpts.Repo` | input persisted — `agent_status.repo`, `sessions.repo`, `agent_events.repo` | none |
| `--agent <name>` | `SpawnOpts.AgentRole`, `harness.EffectiveModel(role)`, per-role config blob | input persisted — `agent_status.root_agent_name`, `sessions.root_agent_name`, `agent_status.agent_name` (last-seen) | low — captured. `agent_name` last-seen vs `root_agent_name` distinction is intact |
| `--attach` | tmux client switch only | not persisted — UX-only flag, no per-spawn semantic difference | none — out of scope for A/B (UX, not behaviour) |
| `--profile <name>` | `config.BuildConfigContent` → per-session `opencode.json` blob | **resolved-only** — the resulting `model_id` lands on `agent_status` and on `msg_assistant` event payloads, but the profile *name* (e.g. `anthropic`, `gemini-hybrid`) is lost | **high** — A/B comparison by profile is the canonical use case ("compare anthropic profile vs gemini-hybrid on the same task"). Must be persisted |
| `--model <id>` | `StartSidecarOpts.Model` → harness adapter `agentModel` field | **resolved-only** — the post-override model id lands on `agent_status.model_id` / `root_model_id`, but whether the user *explicitly* passed `--model` (vs took the profile default) is lost | **high** — distinguishing "anthropic profile, default model" from "anthropic profile, model-overridden to opus" requires the input value |
| `--variant <name>` | `StartSidecarOpts.Variant`, applied across all agents in `BuildConfigContent` | **not persisted** — the variant name (`high`, `max`, `minimal`) is consumed during config construction and discarded; `model_id` doesn't encode it | **high** — variant is a primary A/B axis (same model, different tier). Must be persisted |
| `--isolation <mode>` | `SpawnOpts.IsolationMode`, every downstream isolator | input persisted — `agent_status.isolation_mode` | none |
| `--host-mode` | deprecated alias for `--isolation host` | input persisted — `agent_status.host_mode` plus `isolation_mode='host'` | low — back-compat shim only |
| `--harness <name>` | `SpawnOpts.Harness`, sidecar argv `--harness`, harness adapter selection | input persisted — `agent_status.harness`, `sessions.harness` | none. This is the critical column for cross-harness A/B |
| `--ignore-concurrency-cap` | concurrency-cap probe in `cmd/spawn.go` only | not persisted — bypass flag, doesn't influence per-spawn behaviour after the cap check | none — out of scope for A/B |
| *(positional prompt)* | `SpawnOpts.Prompt` → `PRISM_INITIAL_PROMPT` env var → harness `DeliverInitialPrompt` (host) or container CMD `--prompt` (podman/bwrap) | **resolved-only** — appears in the first `msg_user` event payload. No per-spawn `prompt_template_hash` or `prompt_text` column on `sessions` | **medium** — for A/B correctness, the prompt *as delivered* must be recoverable as a single field. Today recovery is by replaying the first `msg_user` event from `agent_events`, which is brittle if the user prepended context before submitting |

### 2.1 Proxy-spawn parity caveat

Inventory §8.1 flagged that the host-API proxy-spawn path (`internal/sidecar/sidecar.go:3101-3219`) accepts `branch`, `prompt`, `agent`, `profile`, `host_mode`, `harness`, `isolation`, `ignore_concurrency_cap` — but **not** `--model` or `--variant`. [uncertain — inventory §8.1 carries this flag; verify against the proxy request struct before C.2 specifies per-role overrides over the proxy path.] Either the proxy-spawn shape needs to grow these two fields before per-role A/B works for proxy-spawned sessions, or the schema delta below applies only to direct-CLI spawns until then. This proposal assumes the proxy will grow the fields; C.2 should confirm.

---

## 3. Per-column persistence audit

Source: inventory §8.3. Every column in the four persistence tables (plus the three auxiliary tables) is addressed below with its A/B-relevance.

### 3.1 `agent_status` (one row per session, keyed by `session_name`)

| Column | A/B-relevance | Gap |
|---|---|---|
| `session_name` (PK) | grouping key — runs `run-A` and `run-B` are identified by their session names | none |
| `repo` | comparison must hold repo constant | none |
| `worktree` | comparison must hold worktree const-ish (or compare across worktrees deliberately) | none |
| `state` | terminal state is itself a universal A/B metric (finished vs error vs interrupted) | none |
| `title` | free-form human label; not A/B-relevant | none |
| `agent_name` | last-seen agent role; useful for sessions that traverse roles | none |
| `model_id` | last-seen model id — A/B *outcome* signal, not input | none for outcome; see §3.5 for input-side gap |
| `root_agent_name` | spawn-time agent role (input value) | none |
| `root_model_id` | resolved root model id (resolved value, not input flag) | input flag value missing — see §2 row for `--model` |
| `host_mode` | back-compat with pre-v10 rows | none |
| `isolation_mode` | input persisted | none |
| `instance_id` | per-incarnation UUID; the join key against `sessions` and `agent_events` | none |
| `last_seen` | universal latency metric (time-to-last-event) | none |
| `ended_at` | universal — time-to-finished derivable as `ended_at - sessions.started_at` | none |
| `harness` | input persisted; the cross-harness axis | none |
| `harness_session_id` | opencode SID / PI session ID; harness-specific | none — useful for harness-internal joins, not for A/B comparison directly |
| `harness_port` | runtime infra; not A/B-relevant | none |
| `group_id` | foreign key to `session_groups`; today only used for review fan-out | **medium** — `--abtest` needs `session_groups` rows for the abtest pair (see §4.4) |

### 3.2 `sessions` (one row per incarnation, keyed by `instance_id`)

| Column | A/B-relevance | Gap |
|---|---|---|
| `instance_id` (PK) | join key | none |
| `session_name` | denormalised for query convenience | none |
| `agent_role` | input persisted | none |
| `root_agent_name` | input persisted | none |
| `repo` | input persisted | none |
| `worktree` | input persisted | none |
| `harness` | input persisted | none |
| `harness_session_id` | harness-specific session ID | none |
| `group_id` | grouping key for `--abtest` | see §3.1 |
| `started_at` | universal latency origin | none |
| `ended_at` | universal latency endpoint | none |
| `end_state` | universal terminal-state metric | none |
| `archive_path` | post-mortem artefact location | none |
| `prism_version` | tooling-version axis | low — not a primary A/B axis but useful as a covariate |

### 3.3 `agent_events` (one row per event)

| Column | A/B-relevance | Gap |
|---|---|---|
| `id` (PK) | event id | none |
| `session_name` | join key | none |
| `repo`, `worktree` | denormalised | none |
| `harness_session_id` | harness-specific join key (renamed from `opencode_sid` in v14→v15) | none |
| `type` | the universal axis for event-based metrics (token cost from `msg_assistant`, tool count from `tool_call`, etc.) | none |
| `payload` | TEXT JSON — schema lives in `internal/payload/`; **shape is harness-specific** (see §5) | **medium** — A/B queries that read fields out of `payload` are inherently harness-specific unless a normalised projection exists |
| `created_at` | universal — drives time-to-first-event, inter-event latency | none |
| `instance_id` | join key against `sessions` | none |

### 3.4 Auxiliary tables

| Table | A/B-relevance | Gap |
|---|---|---|
| `bus_messages` | cross-session prompt delivery; comparison signal if A/B sides exchanged messages with a third session | low — useful as covariate, not primary axis |
| `pending_merges` | merge-queue rows keyed by PR | low — `merged_at` is a final-stage outcome signal worth surfacing in C.3 |
| `session_groups` | review-group rows; today populated only by review fan-out | **high** — `--abtest` must populate this table to make the abtest pair queryable as a unit. Today no spawn path other than review creates groups |

### 3.5 Summary of input-side gaps

The fields that are *consumed* at spawn time but never reach the DB:

1. `--profile` name — the profile *value* (resolved to a config blob) is not stored.
2. `--model` flag value — distinguished from "model resolved by profile defaults".
3. `--variant` flag value — `high` / `max` / `minimal`.
4. `--pr` number — the PR the worktree is rooted on (recoverable but not as a column).
5. Prompt template / prompt-as-delivered — recoverable from first `msg_user` event but not as a single field.
6. Skills set in effect at spawn time — not visible to the Go side at all (loaded by opencode at runtime; see C.4).
7. Per-role model overrides (future, C.2).
8. The `--harness`-config-blob applied (so a profile that resolves to opencode-specific or PI-specific config can be diffed). [uncertain — confirm what the per-session config blob looks like for PI; PI's config shape is not yet integrated.]

These gaps are what the schema delta in §4 fills.

---

## 4. Proposed schema delta

Two complementary changes. **§4.1 adds a new `spawn_inputs` table** to capture the input side of the variation. **§4.2 adds three columns to `sessions`** for cheap-to-join derived fields. **§4.3 adds one column to `agent_events`** for harness-normalised projection. **§4.4 adds a `kind` column to `session_groups`** to distinguish review-groups from abtest-groups.

The migrations are additive — no existing data is altered. `spawn_inputs` is keyed on `instance_id` so it joins one-to-one with `sessions` and `agent_status`.

### 4.1 New table: `spawn_inputs`

Captures the *intent* of a spawn — every flag value the user passed, in the form they passed it. Resolved values continue to live on `agent_status` and `sessions`.

```sql
CREATE TABLE IF NOT EXISTS spawn_inputs (
    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,

    -- Inputs as the user passed them (NULL = flag not passed; default in effect).
    profile_name           TEXT,
    model_flag             TEXT,    -- value of --model, NULL if not passed
    variant_flag           TEXT,    -- value of --variant, NULL if not passed
    agent_flag             TEXT,    -- value of --agent (denormalised from sessions.agent_role for self-containedness)
    harness_flag           TEXT,    -- value of --harness (denormalised from sessions.harness)
    isolation_flag         TEXT,    -- value of --isolation
    host_mode_flag         INTEGER NOT NULL DEFAULT 0,
    pr_number              INTEGER, -- value of --pr, NULL if branch was specified directly
    branch_flag            TEXT,    -- value of --branch (if explicitly passed; NULL = auto-timestamped)
    ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,

    -- Hooks for sibling Track C issues; populated as those land.
    -- C.2 will define the JSON shape: { "review-context": "claude-opus-4-7", "review-code": "anthropic/claude-sonnet-4-6" }.
    model_variant_overrides TEXT,   -- JSON; NULL if no per-role overrides
    -- C.4 will define the hash + manifest shape for skills and prompt template.
    skills_manifest_hash    TEXT,   -- e.g. SHA-256 of the union of skill names + skill content hashes
    prompt_template_hash    TEXT,   -- hash of the prompt-template the user invoked (when prompts move to templates)

    -- Prompt delivered to the harness, captured once at spawn so A/B comparison
    -- doesn't depend on replaying the first msg_user event.
    prompt_text            TEXT,    -- NULL = no initial prompt
    prompt_source          TEXT,    -- 'cli-positional' | 'cli-stdin' | 'proxy-spawn' | 'review-fanout' | NULL

    -- Free-form per-spawn JSON blob for forward-compat: anything we want to capture
    -- without a migration. Sibling proposals MAY use this temporarily before earning
    -- a dedicated column.
    extras                 TEXT,    -- JSON object; NULL if nothing extra

    created_at             INTEGER NOT NULL  -- ms epoch, mirrors sessions.started_at
);

CREATE INDEX IF NOT EXISTS idx_spawn_inputs_profile ON spawn_inputs(profile_name);
CREATE INDEX IF NOT EXISTS idx_spawn_inputs_harness_profile ON spawn_inputs(harness_flag, profile_name);
```

**Rationale per addition:**

- `profile_name`, `model_flag`, `variant_flag` — fill the three highest-priority gaps from §2 / §3.5. Without these, the A/B query "compare gemini-hybrid vs anthropic for the same task" is impossible without log inspection.
- `agent_flag`, `harness_flag`, `isolation_flag`, `host_mode_flag` — denormalised from `sessions` and `agent_status`. Strictly redundant. Worth carrying so that `spawn_inputs` is self-contained for export and so a comparison query never needs more than a single join.
- `pr_number`, `branch_flag` — closes §2's `--pr` and `--branch` gaps with minimal cost.
- `ignore_concurrency_cap` — auditability; useful when comparing runs that did vs did not bypass the cap.
- `model_variant_overrides`, `skills_manifest_hash`, `prompt_template_hash` — hooks for C.2 / C.4. Defined as nullable here so the column exists for future use without forcing C.2 / C.4 to ship a migration alongside their proposals. **C.2 / C.4 owners may rename or refactor these as they specify the exact shape; the goal here is only to reserve the slot.**
- `prompt_text`, `prompt_source` — closes §2's prompt gap. `prompt_source` distinguishes the four delivery paths (`cli-positional` direct, `cli-stdin`, host-API `proxy-spawn`, `review-fanout`) which today are silently collapsed.
- `extras` (JSON) — explicit pressure-relief valve. New A/B axes can land in `extras` for one release cycle before earning a dedicated column.
- `created_at` — mirrors `sessions.started_at` so `spawn_inputs` is independently time-orderable.

**ON DELETE CASCADE** ties the row's lifetime to its `sessions` row — when a session is purged, its inputs go too. Today nothing purges `sessions`, so this is forward-looking.

### 4.2 Additions to `sessions`

Three derived-value columns that are cheap to join into a comparison view:

```sql
ALTER TABLE sessions ADD COLUMN root_model_id TEXT;
ALTER TABLE sessions ADD COLUMN abtest_pair_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN outcome_summary TEXT;  -- JSON; placeholder for C.3
```

- `root_model_id` — denormalises `agent_status.root_model_id` onto the per-incarnation row so the join `sessions ⋈ spawn_inputs` is sufficient to render a comparison without a third join into `agent_status`.
- `abtest_pair_id` — explicit FK to `session_groups`. Distinguishes "this session is half of an abtest pair" from "this session is part of a review group" without overloading `group_id`. Both columns can be set if an abtest pair *itself* spawns review fan-outs.
- `outcome_summary` — JSON hook for C.3. C.3 will define the precise shape (verdict, rubric scores, derived metrics). Placeholder so C.3 doesn't need to ship a migration.

### 4.3 Addition to `agent_events`

One column to support harness-normalised projections without rewriting payloads:

```sql
ALTER TABLE agent_events ADD COLUMN normalised_payload TEXT;  -- JSON; harness-agnostic projection of payload
```

Today `payload` carries opencode plugin output verbatim (per inventory §8.4 and the Track B.5 framing in #1083). For *universal* comparison metrics (see §5.1), the sidecar's per-harness adapter can write a normalised projection into `normalised_payload` at event-write time. Comparison queries hit `normalised_payload`; harness-specific queries continue to hit `payload`.

This column is **nullable and additive** — existing rows get NULL, the original `payload` is untouched, and B.5's eventual decision (translate-in-adapter vs widen-payload-struct vs version-the-schema) governs whether the column gets populated retroactively. The column is reserved here so C.1's downstream implementation does not block on B.5.

[uncertain — the precise schema of `normalised_payload` is a B.5 deliverable. This proposal only reserves the slot.]

### 4.4 Addition to `session_groups`

Today `session_groups` rows are populated only by review fan-out (per inventory §8.8). `--abtest` (RFC #691 §5) needs to populate the same table with a different *kind* of group, and consumer code (dashboard, `prism stats`) must distinguish them.

```sql
ALTER TABLE session_groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'review';
-- valid values today: 'review', 'abtest'. Forward-compat for other group kinds.

ALTER TABLE session_groups ADD COLUMN spawn_command TEXT;
-- the verbatim command the user typed, e.g.
--   prism spawn --abtest opencode pi --branch feature-x "prompt"
-- Useful for reproducing the experiment.
```

`DEFAULT 'review'` keeps existing rows valid without backfill. New abtest groups insert with `kind = 'abtest'`.

### 4.5 Security considerations for the implementation phase

RFC #691's `[security]` AC requires that "the sidecar's per-harness adapter does not expose harness credentials or auth tokens in session records, logs, or the prism dashboard." Three fields proposed in §4.1 / §3.5 carry credential-exposure risk that the implementation phase must address — flagged here so the synthesis pass (E.1) and the implementation issue inherit the constraint:

- **`spawn_inputs.prompt_text`** — captures the prompt as delivered. Today the same content is recoverable from the first `msg_user` event in `agent_events`, so no net-new exposure surface is created by this column. The implementer should mirror whatever scrubbing (if any) the existing `msg_user` write path applies. If the existing path applies none, that gap is a separate issue and should not block this column landing.
- **`spawn_inputs.extras`** (JSON pressure-relief valve) — has no documented hygiene rules. The implementation issue must define an explicit allow-list or scrubbing pass before any caller writes to `extras`. Treat `extras` as a no-credentials zone by convention.
- **The `--harness`-config-blob persistence contemplated in §3.5 item 8** — per-session config blobs may contain `Authorization` headers, API keys, or token-bearing fields depending on harness. If a future revision of this delta adds a column to persist that blob (this proposal deliberately does not), the implementer must scrub credential-bearing keys before write and must not surface the blob in the dashboard.

These constraints are out-of-scope for this proposal to enforce — they belong in the implementation issue Track E will file. The note is here so the constraint travels with the schema design.

### 4.6 What this delta does *not* add (deferred to siblings)

- A `spawn_outcome` table or `verdict` / `rubric_score` columns on `sessions` — **C.3** owns this. The `outcome_summary` JSON hook is a placeholder.
- A `skills` table or per-skill rows linked to `spawn_inputs` — **C.4** owns this. The `skills_manifest_hash` column is a placeholder; C.4 may add a sister `skills_manifest` table.
- A per-role model-override table — **C.2** owns this. The `model_variant_overrides` JSON column is a placeholder; C.2 may normalise it into rows.
- An ergonomic `prism stats compare` command — see §6 (the *shape* of growth is described; the implementation is left for a downstream issue Track E will prioritise).

---

## 5. Cross-harness comparison-metric taxonomy

The `--abtest opencode pi` motivating use case requires classifying every comparison metric by whether it is meaningful when the two sides emit *different event-stream shapes* (opencode HTTP+SSE plugin events vs PI JSONL-over-stdio RPC events).

The classifier is: **does the metric's definition depend on a payload field that is harness-specific?**

- If the metric is computable from `agent_status` columns, `sessions` columns, or `agent_events.created_at` / `type` alone — **universal**.
- If the metric requires reading `agent_events.payload` *unless* §4.3's `normalised_payload` projection is populated — **harness-specific until normalisation lands; universal thereafter**.
- If the metric requires correlating semantic content between two sides (e.g. "did both runs use the same tool with the same arguments") — **harness-specific** for the foreseeable future.

### 5.1 Universal metrics

Computable across any harness from columns and event types alone:

| Metric | Derived from | Notes |
|---|---|---|
| Time-to-first-event (TTFE) | `min(agent_events.created_at) - sessions.started_at` | Universal — both harnesses write the readiness-triggering event. |
| Time-to-finished | `sessions.ended_at - sessions.started_at` | Universal — `state` transitions are harness-agnostic per inventory §8.5. |
| Terminal state | `sessions.end_state` / `agent_status.state` | Universal. |
| Error rate | `count(agent_events WHERE type='error') / sessions.duration` | Universal — both harnesses surface errors; sidecar writes the `error` event regardless of harness. |
| Tool-call count | `count(agent_events WHERE type='tool_call')` | Universal — both harnesses have tools; the *count* is harness-agnostic even though the tool *names* and *arguments* are not. |
| Tool-error count | `count(agent_events WHERE type='tool_error')` | Universal — see tool-call. |
| Permission-ask count / denial count | `count(agent_events WHERE type IN ('permission_ask','permission_denied'))` | [uncertain — PI's permission model has not been integrated; if PI does not surface a permission step, this metric is harness-specific in practice. RFC #691 does not commit PI to a permission-ask shape.] |
| Compaction count | `count(agent_events WHERE type='compaction')` | [uncertain — PI's session-compaction semantics are unstudied; PI may not have a 1:1 equivalent of opencode's compaction event. If PI emits no compaction event, the metric is degenerate-zero on PI and effectively only meaningful intra-opencode.] |
| State-transition pattern | sequence of `agent_events WHERE type='state_change'` payloads | Universal — `agent_status.state` values are harness-agnostic per inventory §8.5; PI's events map onto the same state machine per RFC #691 §3. |
| Token cost (total) | `sum` over `agent_events WHERE type='msg_assistant'` of payload-derived token counts, **assuming `normalised_payload` projection** | Universal *in principle* (RFC #691 §5 explicitly calls token cost out as a comparison axis); requires §4.3's `normalised_payload` to be populated by both adapters. [uncertain — PI's RPC event has not been confirmed to carry per-message token counts in the same shape opencode does; if PI reports cost only at session end or aggregates differently, normalisation is non-trivial.] |
| Doom-loop detection | `count(agent_events WHERE type='doom_loop_detected')` | Today the doom-loop detector is a sidecar-side heuristic over `msg_assistant` events. [uncertain — whether the same heuristic fires meaningfully on PI's event stream depends on §5.2's *content correlation* working; the *detector's verdict* itself is a universal output once the detector runs.] |
| Turn count (assistant turns) | `count(agent_events WHERE type='msg_assistant')` | Universal in count; the *content* of each message is harness-shaped. |

### 5.2 Harness-specific metrics

Require the harness to be held constant (or require schema normalisation that does not exist today):

| Metric | Why harness-specific | Universalisation path |
|---|---|---|
| Tool-call argument shape | `payload.ToolCall.Args` is opencode plugin-shaped per inventory §8.4; PI tool-call argument JSON will differ structurally | Adapter-side projection into `normalised_payload`: `{tool_name, args_json}` with a per-tool name-mapping. Requires per-tool work. Likely one-tool-at-a-time as comparison demand grows. |
| Tool-call result content | `payload.ToolResult.Output` is harness-shaped (truncation behaviour, escape behaviour, error encoding all differ) | Same as above; harder because tool *output* schemas are tool-specific not just harness-specific. |
| Subagent invocations | `payload.SubagentStart` / `SubagentEnd` exist in opencode's payload struct catalogue but the writer is uncertain (per inventory §8.4 [uncertain] note); PI may not expose a subagent concept at all | If PI lacks subagents, the metric is opencode-only by definition. |
| `audit` event content | Audit-trail entry structure is opencode-plugin-shaped | Defer until B.5 lands; audit content is a low-priority comparison axis. |
| `thinking` event content | Opencode-specific concept (assistant-thinking blocks). PI may or may not surface internal reasoning | If PI surfaces it under a different name, normalise to a `reasoning_text` projection; otherwise opencode-only. [uncertain — PI's reasoning-trace exposure is unknown at the time of writing.] |
| Response-text similarity (did A and B produce equivalent answers?) | Requires content-level diffing of `msg_assistant` text; harness-agnostic *in principle* but the prompt-context and assistant-style differ enough that string-level comparison is rarely informative | Higher-level: rubric-scored verdicts (C.3) are the right frame, not raw text diffing. |
| Per-tool latency | Requires reading `tool_call.created_at` and matching to the corresponding `tool_result.created_at` via tool-call-id, which is in `payload` | Universalisation requires `normalised_payload.tool_call_id` per §4.3. Once that lands, latency is universal. |

### 5.3 The taxonomy in one sentence

**Anything computable from `agent_status` columns, `sessions` columns, `agent_events.created_at`, or `agent_events.type` is universal. Anything that reaches into `agent_events.payload` is harness-specific until a `normalised_payload` projection (§4.3) covers the field, and B.5 governs the projection schema.**

---

## 6. `prism stats` surface gaps

Source: inventory §8.6. Each existing subcommand and the shape of growth needed.

| Subcommand | Today | A/B-shaped growth |
|---|---|---|
| `prism stats summary` | global counts and active-session table | Add `--group-by harness`, `--group-by profile`, `--group-by variant`. Once `spawn_inputs` lands, these are one-line `GROUP BY` additions. |
| `prism stats incarnations` | sessions aggregated by `instance_id` | Add `--abtest-pairs` filter to render only sessions that belong to an `--abtest` group (`session_groups.kind='abtest'`). |
| `prism stats session <id-or-name>` | per-session detail (turns, tokens, tools, model history) | Add a "spawn inputs" block at the top of the per-session view (the row from `spawn_inputs`). Cheap; no schema change beyond §4.1. |
| `prism stats model` | events grouped by `model_id` | Add `--profile` and `--variant` axes. Today the same `model_id` resolved from two different profiles is indistinguishable — `spawn_inputs.profile_name` fixes that. Add `--harness` axis explicitly (today recoverable but not surfaced as a column in the output). |
| `prism stats historical` | per-day event counts | Add `--by-harness`, `--by-profile` group-bys for historical comparison. Low priority. |
| `prism stats asks` / `denials` | permission aggregations | Add `--by-harness` group-by — but per §5.1, this metric is `[uncertain]` for PI. Document the limitation in the help text. |
| `prism stats doomloops` | doom-loop aggregations | Same caveat as `asks`/`denials` — PI applicability is `[uncertain]`. |
| **`prism stats compare <run-A> <run-B>`** | **does not exist** | **New subcommand.** Renders a side-by-side table over the universal metrics from §5.1 plus a "spawn inputs diff" block highlighting which `spawn_inputs` columns differ between A and B. Out-of-scope for this proposal to specify the exact table shape; in-scope to flag that this is the user-facing surface that the schema delta unlocks. |
| **`prism stats abtest <group_id>`** | **does not exist** | **New subcommand.** Same as `compare` but takes a `session_groups.group_id` (with `kind='abtest'`) and renders all members. Distinguishes from `compare` because an abtest pair has structured group membership; ad-hoc comparison can be of any two `instance_id`s. |

`prism dashboard` (not strictly a `stats` subcommand but in §8.6) already shows `state`, `model`, `repo`, `agent role`, `isolation mode` columns. To support abtest grouping per RFC #691 §5 ("visually linked, independently navigable"), the dashboard needs to read `sessions.abtest_pair_id` and render paired sessions adjacent or with a visual link. Scope-wise this is dashboard work, not `stats` work — flagged here for completeness.

`prism checkin` (also from §8.6) is already group-aware (it walks review-group members for a parent session). Once `session_groups.kind` lands, the same mechanism extends to abtest groups with no schema gymnastics — `prism checkin <abtest-parent>` walks both abtest sides. The work is in `prism checkin` recognising the new `kind`, not in the schema.

---

## 7. Open questions and uncertainty flags

Collected in one place for the synthesis pass (E.1) and for the sibling Track C issues:

1. **[uncertain]** Does `--model` / `--variant` reach the proxy-spawn path? Inventory §8.1 carries this flag; until confirmed, the schema delta in §4.1 captures the fields but proxy-spawned sessions may have NULLs in `model_flag` / `variant_flag`. C.2 should resolve.
2. **[uncertain]** Does PI emit per-message token counts in a shape that maps to opencode's? §5.1's "token cost (total)" universality depends on this; if PI reports cost only at session end, the universality holds at the *session* grain but not at the *message* grain.
3. **[uncertain]** Does PI have a permission-ask / permission-denied event in its JSONL stream, or are permissions handled out-of-band? §5.1 flags both metrics as `[uncertain]` pending PI integration.
4. **[uncertain]** Does PI emit a compaction-equivalent event? Same caveat as permissions.
5. **[uncertain]** Does PI surface assistant-thinking / reasoning content as a structured event, or is it interleaved into the message stream? §5.2 flags this.
6. **[uncertain]** Where exactly does the existing prompt-as-delivered live for review fan-out? `internal/review/review.go` builds its own prompts; whether `--prompt` propagation through review pairs cleanly with the `prompt_source='review-fanout'` distinction in §4.1 needs to be confirmed during implementation.
7. **[uncertain]** Whether `subagent_start` / `subagent_end` events are actually being written today (per inventory §8.4 [uncertain] flag). If they are not, the universality classification in §5.2 for subagents is hypothetical until the writer lands.
8. **[uncertain]** Whether `tool_error` has a payload struct (per inventory §8.4 [uncertain] flag). Affects whether tool-error count via §5.1 needs a payload reader or just an event-type count (the count form is what §5.1 uses, so this is a low-impact uncertainty).
9. **Open question for C.2:** Should `model_variant_overrides` be a JSON column on `spawn_inputs` (as proposed in §4.1) or a normalised side-table `spawn_role_overrides(instance_id, role, model_id, variant)`? The JSON form is cheaper to ship; the normalised form is friendlier to indexed queries. C.2 decides.
10. **Open question for C.3:** Should `outcome_summary` on `sessions` (as proposed in §4.2) become a separate `spawn_outcomes` table once C.3 designs the verdict / rubric shape in detail? The JSON column is a cheap forward-compat bet; C.3 may want to refactor.
11. **Open question for C.4:** What is the granularity of `skills_manifest_hash`? Hash of the *list of skill names*, hash of the *content of each skill at the time of spawn*, or a composite? C.4 decides.
12. **Open question for the implementation phase:** Should the `spawn_inputs` row be written by `cmd/spawn.go` directly (synchronous, before `SpawnSession` returns) or by the sidecar on its first event (asynchronous, alongside the existing `tmux_session_start` hook)? Synchronous gives stronger invariants ("if a session exists, its inputs exist"); asynchronous is consistent with how `sessions` itself is populated today via the tmux hook. Either works for A/B querying; the synthesis pass should pick.

---

## 8. Summary

- **§2** addresses every flag in inventory §8.1, classifying each as input-persisted / resolved-only / not-persisted. The high-impact gaps are `--profile` name, `--model` flag value, `--variant` value, and the prompt-as-delivered.
- **§3** addresses every column in inventory §8.3 (`agent_status`, `sessions`, `agent_events`) plus the auxiliary tables (`bus_messages`, `pending_merges`, `session_groups`). The persistence shape captures outcomes well; intent is what is missing.
- **§4** proposes a concrete schema delta as SQL sketches: a new `spawn_inputs` table keyed on `instance_id`, three additive columns on `sessions`, one additive column on `agent_events` (reserved for B.5's normalised payload projection), and a `kind` column on `session_groups` to distinguish review groups from abtest groups. §4.5 carries forward RFC #691's `[security]` AC by flagging credential-exposure constraints the implementation phase must address (notably for `prompt_text`, `extras`, and any future per-harness config-blob persistence).
- **§5** classifies comparison metrics as universal (computable across any harness from columns / event types alone) vs harness-specific (depending on `payload` content). Token cost, tool-call count, time-to-first-event, time-to-finished, error rate, terminal state, and state-transition pattern are universal; tool-call argument shape, tool-call result content, audit content, and thinking content are harness-specific. Cross-harness `[uncertain]` flags are marked where PI's event shape has not been stress-tested.
- **§6** identifies the `prism stats` surface gaps: the existing subcommands need `--group-by harness/profile/variant` axes, and two new subcommands (`stats compare`, `stats abtest`) are needed to render the comparison query the schema delta makes possible. `prism dashboard` and `prism checkin` need only consumer-side awareness of `session_groups.kind='abtest'`; no schema work for them.
- **§7** collects all open questions and `[uncertain]` flags for the synthesis pass (E.1) and the sibling Track C issues (C.2, C.3, C.4) to consume.

The schema delta is deliberately conservative — JSON hooks for the C.2 / C.3 / C.4 axes rather than fully-normalised tables, leaving the sibling proposals room to specify their own shapes. The universal-metric set is the durable contract that survives any re-shaping the sibling issues do.
