---
name: prism-db
description: Query the prism SQLite database. Load this skill when you need to analyse agent sessions, review session metrics, debug agent behaviour, or access the prism event log.
---

# Prism Database

The prism SQLite database records every agent interaction: tool calls, messages, token counts, timing metrics, errors, permission events, and state changes across all sessions.

## Database location

```
~/.local/state/prism/prism.db
```

On NixOS, `XDG_STATE_HOME` is set to `$HOME/.local/state`. The database path is always `$XDG_STATE_HOME/prism/prism.db`.

## Access — read-only URI required

**All access MUST use SQLite read-only mode:**

```bash
sqlite3 "file:~/.local/state/prism/prism.db?mode=ro" "<query>"
```

The `?mode=ro` flag enforces read-only access at the SQLite engine level. `INSERT`, `UPDATE`, `DELETE`, `DROP`, and all other write operations will be rejected with "attempt to write a readonly database". This is a safety requirement, not a suggestion.

## Schema

```sql
-- Immutable event log: every agent interaction is recorded here.
CREATE TABLE agent_events (
  id           TEXT PRIMARY KEY,
  session_name TEXT NOT NULL,    -- e.g. "nixos-config@fix-login"
  repo         TEXT NOT NULL,    -- e.g. "nixos-config"
  worktree     TEXT NOT NULL,    -- filesystem path to the worktree
  opencode_sid TEXT,             -- opencode session ID (NULL for legacy events)
  type         TEXT NOT NULL,    -- one of the 11 event types below
  payload      TEXT NOT NULL,    -- JSON, structure depends on type
  created_at   INTEGER NOT NULL  -- unix milliseconds
);
CREATE INDEX idx_events_session ON agent_events(session_name, created_at DESC);
CREATE INDEX idx_events_repo    ON agent_events(repo, type, created_at DESC);

-- Current state of each tmux session (one row per session).
CREATE TABLE agent_status (
  session_name    TEXT PRIMARY KEY,
  repo            TEXT NOT NULL,
  worktree        TEXT NOT NULL,
  state           TEXT NOT NULL,    -- "active", "waiting", "idle", "finished", etc.
  title           TEXT,             -- task title/description
  opencode_sid    TEXT,
  agent_name      TEXT,             -- current agent (may change during session)
  model_id        TEXT,             -- current model
  root_agent_name TEXT,             -- agent at session start (stable)
  root_model_id   TEXT,             -- model at session start (stable)
  opencode_port   INTEGER,
  host_mode       INTEGER NOT NULL DEFAULT 0,
  last_seen       INTEGER NOT NULL, -- unix milliseconds
  ended_at        INTEGER           -- unix milliseconds, NULL if still running
);

-- Inter-session message bus.
CREATE TABLE bus_messages (
  id           TEXT PRIMARY KEY,
  from_session TEXT NOT NULL,
  to_session   TEXT NOT NULL,
  repo         TEXT NOT NULL,
  text         TEXT NOT NULL,
  urgency      TEXT NOT NULL DEFAULT 'normal',
  sent_at      INTEGER NOT NULL,   -- unix milliseconds
  delivered_at INTEGER              -- unix milliseconds, NULL if pending
);

CREATE TABLE schema_version (
  version INTEGER NOT NULL
);
```

## Event types and payload structures

All payloads are JSON. Field names are camelCase.

| Type | Description | Key payload fields |
|------|-------------|--------------------|
| `msg_user` | User/prompt message | `messageId` (TEXT), `text` (TEXT), `agent` (TEXT), `model` (TEXT) |
| `msg_assistant` | Assistant response | `messageId` (TEXT), `text` (TEXT), `agent` (TEXT), `model` (TEXT), `inputTokens` (INTEGER), `outputTokens` (INTEGER), `cacheReadTokens` (INTEGER), `cacheWriteTokens` (INTEGER), `durationMs` (INTEGER), `ttftMs` (INTEGER), `contextWindowPct` (REAL) |
| `tool_call` | Tool invocation | `tool` (TEXT), `args` (TEXT — JSON string), `messageId` (TEXT), `durationMs` (INTEGER) |
| `tool_result` | Tool output | `tool` (TEXT), `result` (TEXT), `messageId` (TEXT) |
| `permission_ask` | Permission prompt shown | `tool` (TEXT), `patterns` (TEXT), `messageId` (TEXT) |
| `permission_denied` | Permission denied | `tool` (TEXT), `messageId` (TEXT) |
| `thinking` | Extended thinking output | `text` (TEXT), `messageId` (TEXT) |
| `state_change` | Agent state transition | `state` (TEXT) |
| `compaction` | Context compaction occurred | `note` (TEXT) |
| `error` | Error event | `note` (TEXT) |
| `subagent_start` | Subagent invoked | `agent` (TEXT), `description` (TEXT), `messageId` (TEXT) |
| `subagent_end` | Subagent completed | `agent` (TEXT), `durationMs` (INTEGER), `messageId` (TEXT) |

## Timestamps

All timestamps in the database are **unix milliseconds** (not seconds).

- Human-readable output: `datetime(created_at/1000, 'unixepoch', 'localtime')`
- Comparison against current time: `strftime('%s','now') * 1000`

Example:

```sql
-- Events in the last 7 days
WHERE created_at > (strftime('%s','now','-7 days') * 1000)
```

## Example queries

### List all sessions in the last 7 days

```sql
SELECT session_name, state, title, root_agent_name, root_model_id,
       datetime(last_seen/1000, 'unixepoch', 'localtime') as last_active,
       CASE WHEN ended_at IS NOT NULL
            THEN datetime(ended_at/1000, 'unixepoch', 'localtime')
            ELSE 'running' END as ended
FROM agent_status
WHERE last_seen > (strftime('%s','now','-7 days') * 1000)
ORDER BY last_seen DESC;
```

### Session summary metrics (tokens, cost, duration, tool calls)

```sql
SELECT
  COUNT(CASE WHEN type='msg_assistant' THEN 1 END) as turns,
  SUM(CASE WHEN type='msg_assistant' THEN json_extract(payload,'$.inputTokens') END) as input_tokens,
  SUM(CASE WHEN type='msg_assistant' THEN json_extract(payload,'$.outputTokens') END) as output_tokens,
  COUNT(CASE WHEN type='tool_call' THEN 1 END) as tool_calls,
  COUNT(CASE WHEN type='compaction' THEN 1 END) as compactions,
  COUNT(CASE WHEN type='error' THEN 1 END) as errors,
  MIN(created_at) as first_event_ms,
  MAX(created_at) as last_event_ms
FROM agent_events
WHERE session_name = '<session>';
```

### Tool call frequency for a session

```sql
SELECT json_extract(payload,'$.tool') as tool, COUNT(*) as count,
       AVG(json_extract(payload,'$.durationMs')) as avg_duration_ms
FROM agent_events
WHERE session_name = '<session>' AND type = 'tool_call'
GROUP BY tool ORDER BY count DESC;
```

### Detect potential doom loops (same tool+args repeated)

```sql
SELECT json_extract(payload,'$.tool') as tool,
       json_extract(payload,'$.args') as args,
       COUNT(*) as repetitions
FROM agent_events
WHERE session_name = '<session>' AND type = 'tool_call'
GROUP BY tool, args
HAVING COUNT(*) >= 3
ORDER BY repetitions DESC;
```

### Find sessions with high compaction count (context pressure indicator)

```sql
SELECT session_name, COUNT(*) as compactions
FROM agent_events
WHERE type = 'compaction'
  AND created_at > (strftime('%s','now','-7 days') * 1000)
GROUP BY session_name
HAVING COUNT(*) >= 2
ORDER BY compactions DESC;
```

### Find sessions with permission denials

```sql
SELECT session_name,
       json_extract(payload,'$.tool') as tool,
       COUNT(*) as denials
FROM agent_events
WHERE type = 'permission_denied'
  AND created_at > (strftime('%s','now','-7 days') * 1000)
GROUP BY session_name, tool
ORDER BY denials DESC;
```

### Get conversation flow for a session (drill-down)

```sql
SELECT type,
       datetime(created_at/1000, 'unixepoch', 'localtime') as time,
       CASE
         WHEN type = 'msg_user' THEN substr(json_extract(payload,'$.text'), 1, 200)
         WHEN type = 'msg_assistant' THEN substr(json_extract(payload,'$.text'), 1, 200)
         WHEN type = 'tool_call' THEN json_extract(payload,'$.tool') || ': ' || substr(json_extract(payload,'$.args'), 1, 150)
         WHEN type = 'error' THEN json_extract(payload,'$.note')
         WHEN type = 'compaction' THEN json_extract(payload,'$.note')
         WHEN type = 'state_change' THEN json_extract(payload,'$.state')
         ELSE payload
       END as summary
FROM agent_events
WHERE session_name = '<session>'
ORDER BY created_at;
```

## CLI alternatives

These commands are often faster than raw SQL for common tasks:

| Command | What it does |
|---------|--------------|
| `prism stats` | Summary table of all active sessions |
| `prism stats <session>` | Detailed metrics for one session |
| `prism stats --days N` | Historical aggregate over N days |
| `prism stats model --days N` | Per-model performance breakdown |
| `prism checkin <session>` | Reconstructed conversation history with tool calls inline |
| `prism checkin <session> --last N` | Last N turns only |
| `prism checkin <session> --verbose` | Full tool args/results without truncation |
| `prism list-sessions` | All active sessions with state |
