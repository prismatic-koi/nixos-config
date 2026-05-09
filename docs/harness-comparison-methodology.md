# Harness Comparison Methodology: pi vs opencode

This document records the methodology used for a startup timing and cost efficiency
comparison between the pi and opencode harnesses in May 2026, so that a follow-up
analysis can be run consistently once more data has accumulated.

## Context

Prism switched from opencode as the primary harness to pi in early May 2026. After
approximately one week of pi usage, an initial comparison was run on 2026-05-10 to
establish baseline metrics across startup speed, session reliability, and token
efficiency.

At the time of the first analysis:
- **opencode**: ~113 finished sessions in the DB (accumulated over several months)
- **pi**: ~80 sessions (approximately one week of data)

---

## Data source

All queries run against the prism SQLite database via `prism db query`. The database
is not directly mountable inside the coordinator container; use the `prism db` CLI
which proxies through the host-API socket transparently.

Key tables used:

| Table | Purpose |
|---|---|
| `sessions` | Instance metadata: harness, started_at, ended_at |
| `spawn_outcome` | Aggregated per-session metrics: tokens, cost, turn counts, timing |
| `spawn_inputs` | Spawn-time parameters: agent flag, harness flag, profile |
| `harness_frames` | Raw wire frames for pi sessions (hello, session_status, msg_assistant, etc.) |
| `agent_events` | Normalised events for opencode sessions (msg_assistant, state_change, etc.) |
| `agent_status` | Model ID per session (root_model_id is the most reliable field) |

---

## Metric 1: Time to healthy session (spawn → hello)

**What it measures**: wall-clock time from when prism writes the session row
(`sessions.started_at`) to when the harness sends its first `hello` frame
(`harness_frames` where `type = 'hello'` and `direction = 'in'`). This is the
point at which the harness is up, the sidecar handshake is complete, and the
session is ready to accept prompts.

**Why it matters**: includes bwrap startup, the harness binary initialising, and
the socket/HTTP handshake. It is the pure infrastructure cost of spawning a session,
before any API call is made. It directly affects how quickly parallel review agents
become useful.

**Query pattern**:
```sql
WITH first_hello AS (
  SELECT instance_id, MIN(created_at) as hello_at
  FROM harness_frames
  WHERE type = 'hello' AND direction = 'in'
  GROUP BY instance_id
)
SELECT
  s.harness,
  COUNT(*) as n,
  MIN(fh.hello_at - s.started_at) as min_ms,
  ROUND(AVG(fh.hello_at - s.started_at)) as avg_ms,
  MAX(fh.hello_at - s.started_at) as max_ms
FROM sessions s
JOIN first_hello fh ON fh.instance_id = s.instance_id
WHERE (fh.hello_at - s.started_at) BETWEEN 500 AND 60000
GROUP BY s.harness
```

**Filter rationale**: `BETWEEN 500 AND 60000` excludes sub-500ms values (likely
clock skew or resumed sessions with a stale `started_at`) and values over 60s
(sessions that never became healthy or had infrastructure failures).

**May 2026 results**:
| harness | n | min | avg | max |
|---|---|---|---|---|
| opencode | 356 | 1668ms | ~5500ms | 10334ms |
| pi | 36 | 1304ms | ~4459ms | 7617ms |

**Live timing test (2026-05-10)** — single session each, spawned back-to-back on
the same machine in the same conditions:
| harness | spawn → hello | spawn → first message |
|---|---|---|
| pi | 1179ms | 5004ms |
| opencode | 13047ms | 17000ms |

The live test is more representative of the true difference; the historical averages
include significant variance from machine load, bwrap warmth, and session type mix.

**Note on `started_at`**: this timestamp is written by `prism spawn` in the
coordinator session before the worker tmux window is created. It therefore includes
tmux window setup time on top of the harness startup itself. Both harnesses pay this
equally, so it does not affect the relative comparison.

---

## Metric 2: Time to first message (spawn → first msg_assistant)

**What it measures**: wall-clock time from `sessions.started_at` to the first
`msg_assistant` frame arriving in `harness_frames` (pi) or `agent_events`
(opencode). This includes harness startup plus the first LLM API round-trip.

**Why it matters**: from the user's perspective this is when the agent is visibly
doing something. It combines infrastructure latency and API latency.

**Key caveat**: API latency (~3–4s for Sonnet) dominates for fast-starting
harnesses like pi, making this metric noisier than spawn→hello for infrastructure
comparisons. Rate-limit delays inflate this dramatically — filter to sessions where
the first message arrived within 60s to exclude those.

**Query pattern**:
```sql
WITH first_msg AS (
  SELECT instance_id, MIN(created_at) as msg_at
  FROM harness_frames
  WHERE type = 'msg_assistant' AND direction = 'in'
  GROUP BY instance_id
),
per_session AS (
  SELECT s.harness, (fm.msg_at - s.started_at) as ms
  FROM sessions s
  JOIN first_msg fm ON fm.instance_id = s.instance_id
  WHERE (fm.msg_at - s.started_at) BETWEEN 500 AND 60000
)
SELECT harness, COUNT(*) as n, MIN(ms) as min_ms, ROUND(AVG(ms)) as avg_ms, MAX(ms) as max_ms
FROM per_session
GROUP BY harness
```

**Note for follow-up**: opencode uses `agent_events` not `harness_frames` for
`msg_assistant`. If comparing across harnesses, query both tables or use
`spawn_outcome.time_to_first_event_ms` as a rough proxy (though that measures
time to first DB event, not first LLM token, so it reads very low — 3–17ms —
and is not useful for this comparison).

**May 2026 results** (≤60s filter):
| harness | n | min | avg | max |
|---|---|---|---|---|
| opencode | 323 | 4248ms | 7896ms | 18519ms |
| pi | 35 | 5030ms | 6223ms | 9089ms |

---

## Metric 3: Token efficiency and estimated cost

### Token fields

Both harnesses populate the following fields in `spawn_outcome`:

| Field | Meaning |
|---|---|
| `tokens_input_total` | Fresh (non-cached) input tokens |
| `tokens_output_total` | Output tokens |
| `tokens_cache_read_total` | Prompt cache hits (cheap) |
| `tokens_cache_write_total` | Prompt cache writes |
| `cost_usd_total` | Self-reported cost — **unreliable**, patchy coverage |

**Known data quality issues at May 2026:**
- `cost_usd_total` is populated on only 4/113 opencode sessions and 38/80 pi
  sessions. Do not use it directly.
- Pi's `tokens_input_total` reads anomalously low (~76 avg) because pi reports
  nearly all context as `cache_read` rather than fresh input. This is correct
  behaviour (pi caches aggressively) but makes the raw input number misleading.
- Pi's `model_id` is NULL in `agent_status` for almost all sessions at this
  point; it is not flowing through from the wire frames. Defaulting to
  `anthropic/claude-sonnet-4-6` is a reasonable assumption but should be
  verified as pi's model reporting improves.

### Estimated cost query

Apply current API pricing to the token breakdown per session, using
`agent_status.root_model_id` for model detection (with a Sonnet 4.6 default for
NULLs). The CASE logic handles the three current price tiers:

```sql
WITH session_model AS (
  SELECT
    s.instance_id, s.harness,
    COALESCE(ast.root_model_id, 'anthropic/claude-sonnet-4-6') as model
  FROM sessions s
  LEFT JOIN agent_status ast ON ast.instance_id = s.instance_id
),
priced AS (
  SELECT
    sm.harness, sm.model,
    so.msg_assistant_count as turns,
    (so.tokens_input_total  / 1e6 * CASE WHEN sm.model LIKE '%sonnet%' THEN 3.0  WHEN sm.model LIKE '%opus%' AND (sm.model LIKE '%-4-5%' OR sm.model LIKE '%-4-6%' OR sm.model LIKE '%-4-7%') THEN 5.0  ELSE 15.0 END +
     so.tokens_output_total / 1e6 * CASE WHEN sm.model LIKE '%sonnet%' THEN 15.0 WHEN sm.model LIKE '%opus%' AND (sm.model LIKE '%-4-5%' OR sm.model LIKE '%-4-6%' OR sm.model LIKE '%-4-7%') THEN 25.0 ELSE 75.0 END +
     so.tokens_cache_read_total  / 1e6 * CASE WHEN sm.model LIKE '%sonnet%' THEN 0.30 WHEN sm.model LIKE '%opus%' AND (sm.model LIKE '%-4-5%' OR sm.model LIKE '%-4-6%' OR sm.model LIKE '%-4-7%') THEN 0.50 ELSE 1.50 END +
     so.tokens_cache_write_total / 1e6 * CASE WHEN sm.model LIKE '%sonnet%' THEN 3.75 WHEN sm.model LIKE '%opus%' AND (sm.model LIKE '%-4-5%' OR sm.model LIKE '%-4-6%' OR sm.model LIKE '%-4-7%') THEN 6.25 ELSE 18.75 END
    ) as est_cost
  FROM sessions s
  JOIN spawn_outcome so ON s.instance_id = so.instance_id
  JOIN session_model sm ON sm.instance_id = s.instance_id
  WHERE so.end_state = 'finished'
    AND so.msg_assistant_count > 0
    AND (so.tokens_output_total > 0 OR so.tokens_cache_read_total > 0)
)
SELECT
  harness, model,
  COUNT(*) as sessions,
  ROUND(AVG(turns)) as avg_turns,
  ROUND(SUM(est_cost), 2) as est_total_usd,
  ROUND(AVG(est_cost), 4) as est_avg_per_session,
  ROUND(SUM(est_cost) / SUM(turns), 4) as est_cost_per_turn
FROM priced
GROUP BY harness, model
ORDER BY harness, sessions DESC
```

**Pricing used** (current as of 2026-05-10 — verify against
https://platform.claude.com/docs/en/about-claude/pricing before re-running):

| Model | Input | Output | Cache read | Cache write (5-min) |
|---|---|---|---|---|
| Sonnet 4.x | $3/MTok | $15/MTok | $0.30/MTok | $3.75/MTok |
| Opus 4.5/4.6/4.7 | $5/MTok | $25/MTok | $0.50/MTok | $6.25/MTok |
| Opus 4/4.1 | $15/MTok | $75/MTok | $1.50/MTok | $18.75/MTok |

Note: github-copilot sessions are billed differently in practice but are included
here at API rates to estimate what they would cost on the direct API.

**May 2026 results** (anthropic/sonnet-4.6, direct API comparable):
| harness | sessions | avg turns | est. avg/session | est. cost/turn |
|---|---|---|---|---|
| opencode | 35 | 137 | $6.88 | $0.050 |
| pi | 39 | 55 | $3.62 | $0.066 |

Pi is ~47% cheaper per session and uses ~60% fewer turns despite a higher cost-per-turn.
The higher per-turn cost is expected: pi uses extended thinking, which spends more
tokens per turn but reaches correct answers faster, reducing total turns.

---

## Reliability metrics (not fully analysed in May 2026 — suggested for follow-up)

The following fields in `spawn_outcome` were noted but not deeply analysed due to
small sample size. Include in the follow-up:

```sql
SELECT
  s.harness,
  COUNT(*) as sessions,
  ROUND(AVG(so.doom_loop_count), 3) as avg_doom_loops,
  ROUND(AVG(so.error_event_count), 3) as avg_errors,
  ROUND(AVG(so.permission_ask_count), 3) as avg_permission_asks,
  ROUND(AVG(so.interrupted_count), 3) as avg_interrupts,
  COUNT(CASE WHEN so.end_state = 'finished' THEN 1 END) * 100.0 / COUNT(*) as pct_finished
FROM sessions s
JOIN spawn_outcome so ON s.instance_id = so.instance_id
WHERE s.started_at > [cutoff_epoch_ms]
GROUP BY s.harness
```

---

## Suggested follow-up checklist

When re-running this analysis (target: ~2026-05-24 or later):

- [ ] Check whether pi's `model_id` is now populated in `agent_status` — if so,
      remove the COALESCE default and verify actual model distribution
- [ ] Verify pricing against https://platform.claude.com/docs/en/about-claude/pricing
      (models and prices change)
- [ ] Filter sessions to a consistent date range if comparing harness periods
      directly (e.g. `WHERE s.started_at > [unix_ms for 2026-05-06]` for pi-era only)
- [ ] Check whether opencode logs are now being written to the prism per-session
      directory — if so, the startup phase breakdown (JS init, plugin load, MCP
      proxy, etc.) becomes available for the spawn→hello gap analysis
- [ ] Run a fresh live timing test (same prompt, both harnesses back-to-back) to
      get a point-in-time comparison unaffected by historical session mix
- [ ] Include the reliability metrics section above now that sample size is larger
