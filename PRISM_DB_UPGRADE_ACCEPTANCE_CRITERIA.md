# Prism DB Upgrade — Project Acceptance Criteria

This document covers the prism DB upgrade: a 9-stage migration that replaces tmux
primitives (window options, keystroke injection, screen scraping) with a prism-owned
SQLite database (`prism.db`) as the single source of truth for agent state, message
history, and inter-agent communication.

**Scope**: This refers to prism, the AI TUI environment under
`modules/programs/prism/`, not the nixos-config repository generally.

---

## Invariants

These must be true at all times, across every stage. No stage is allowed to break them.

- **opencode must never crash due to prism plugin errors.** All DB writes in
  `prism-hooks.ts` are wrapped in try/catch. Plugin errors are logged to console but
  never propagate.
- **`prism.db` absence must be handled gracefully.** If `prism.db` does not exist, the
  plugin skips all DB writes and continues. Go commands that open the DB return a clear
  error rather than panicking.
- **Non-project sessions must not pollute the DB.** Sessions without a `.bare` marker
  in their ancestry (scratchpad, prism-dashboard, etc.) must not produce `agent_status`
  or `agent_events` rows.
- **`go build ./...` must pass at every stage.** No stage may be committed with a broken
  Go build.
- **`go test ./...` must pass at every stage.** No stage may be committed with failing
  tests.
- **All `.nix` files must be formatted with `nixfmt` before committing.** Run
  `nixfmt .` after any `.nix` change.
- **Stages must be executed in order.** Later stages depend on DB rows written by
  earlier ones.

---

## Final State

### From the user's perspective

- `prism checkin <session>` shows timestamped conversation history with tool call
  summaries — no more screen-scraped TUI output
- `prism list-sessions` is repo-scoped by default and reads from DB; `--all` shows
  all repos
- `prism prompt <session> "..."` writes to a message bus; the target agent receives it
  on next idle. `--urgent` delivers within 2 seconds via toast and prompt injection
- `prism status --waiting --tmux-format` reads from DB — always accurate, never drifts
- The dashboard renders a skeleton frame immediately on open, then updates in ~1 second
  on any state change (sentinel-driven, no polling)
- Session names use full branch paths with `--` substituting `/`
  (e.g. `nixos-config@feat--my-thing`)
- `prism save` and `prism notify` no longer exist

### From a code perspective

- `modules/programs/prism/prism/internal/db/` exists and is the canonical DB layer
- `~/.local/state/prism/prism.db` is the single source of truth for session state,
  events, and the message bus
- `~/.local/state/prism/bus/` holds sentinel files used by the Go-side dashboard watcher
- `prism-hooks.ts` writes all opencode session events to `prism.db` in addition to (or
  instead of, for retired paths) tmux primitives
- `sessions.json` does not exist and is not referenced anywhere
- `@prism_waiting`, `@agent_state` tmux window options are not read or written anywhere
- `SendKeysDelayed`, `SendKeysWhenReady`, `CapturePaneScreen` do not exist in non-test code
- `prism notify` command does not exist
- `prism save` command does not exist
- `--repo` flag on `prism spawn` does not exist

---

## Regression Checks

These commands must continue to work (or be replaced by their documented successors)
throughout the upgrade.

| Command | Expected behaviour |
|---|---|
| `prism spawn <worktree>` | Opens a new tmux session and starts opencode in the worktree |
| `prism checkin <session>` | Returns conversation history (format improves in Stage 5) |
| `prism list-sessions` | Lists active sessions for the current repo |
| `prism list-sessions --all` | Lists all active sessions across all repos |
| `prism status --waiting --tmux-format` | Returns waiting session count as a number |
| `prism prompt <session> "text"` | Delivers message to target agent |
| `prism prompt <session> --urgent "text"` | Delivers within ~2 seconds via toast and prompt injection |
| `prism restart` | Kills tmux server and restores sessions from DB |
| `prism restore` | Restores sessions from DB without killing existing tmux sessions |
| `prism dashboard` | Opens dashboard (skeleton frame on open, DB-sourced data) |
| `prism event tmux-session-start --session s --worktree /p` | Writes DB row; exits 0 |
| Opening opencode in a worktree | Plugin starts without crash; DB row written |

---

## Data Integrity Checks

Run these against `~/.local/state/prism/prism.db` to verify DB health.

```sql
-- All active sessions (ended_at IS NULL)
SELECT session_name, state, last_seen, datetime(last_seen/1000, 'unixepoch', 'localtime')
FROM agent_status
WHERE ended_at IS NULL
ORDER BY last_seen DESC;

-- Orphaned rows: ended_at IS NULL but last_seen > 7 days ago (likely stale)
SELECT session_name, state, datetime(last_seen/1000, 'unixepoch', 'localtime') AS last_seen_at
FROM agent_status
WHERE ended_at IS NULL
  AND last_seen < (strftime('%s', 'now') - 7 * 86400) * 1000;

-- Pending (undelivered) bus messages
SELECT id, from_session, to_session, urgency, text, datetime(sent_at/1000, 'unixepoch', 'localtime') AS sent
FROM bus_messages
WHERE delivered_at IS NULL
ORDER BY sent_at;

-- Event counts per session (most recent 10 sessions)
SELECT session_name, COUNT(*) AS event_count, MAX(datetime(created_at/1000, 'unixepoch', 'localtime')) AS last_event
FROM agent_events
GROUP BY session_name
ORDER BY MAX(created_at) DESC
LIMIT 10;

-- Verify schema version
SELECT version FROM schema_version;
-- Expected: 1

-- Verify WAL mode is on
PRAGMA journal_mode;
-- Expected: wal

-- Check for sessions with opencode_sid populated (proxy for Stage 2 working)
SELECT COUNT(*) AS sessions_with_sid
FROM agent_status
WHERE opencode_sid IS NOT NULL AND ended_at IS NULL;

-- Check for duplicate session_name rows in agent_status (should be 0)
SELECT session_name, COUNT(*) AS n
FROM agent_status
GROUP BY session_name
HAVING n > 1;
```

---

## Retired Components

The following must **not** exist after Stage 9 is complete.

### Commands

- `prism save` — removed in Stage 9 (body was no-op since Stage 6)
- `prism notify` — removed in Stage 9
- `prism spawn --repo <flag>` — flag removed in Stage 9

### Files

- `~/.local/state/prism/sessions.json` — must not exist (or be stale and deletable)
- `modules/programs/prism/prism/cmd/save.go`
- `modules/programs/prism/prism/cmd/notify.go`

### Go symbols

- `SavedSession` struct
- `writeSessions()` function
- `loadSessions()` function
- `saveStatePath()` function
- `SendKeysDelayed()` in `internal/tmux`
- `SendKeysWhenReady()` in `internal/tmux`
- `CapturePaneScreen()` in `internal/tmux` (or any equivalent screen-scraping function)

### tmux options

- `@prism_waiting` — not read or written anywhere
- `@agent_state` — not read or written anywhere

### tmux hooks (in `tmux.nix`)

- `after-refresh-client` hook calling `prism save` — removed in Stage 6

### Plugin patterns

- Any call to `prism notify <state>` in `prism-hooks.ts` — removed in Stage 8
- `fs.watch()` on a sentinel file in `prism-hooks.ts` — never introduced (replaced by
  `setInterval` in Stage 7)

---

## Cross-Stage Consistency Checks

These span multiple stages and must be verified holistically once all stages are done.

**Session name parity**: `sessionNameFor()` in Go (Stage 3) and `deriveSessionName()`
in the plugin (Stage 2, updated in Stage 3) must produce identical output for the same
worktree. Verify by spawning a session and comparing the tmux session name with the
`session_name` column written by the plugin.

**No double-counting of waiting sessions**: `prism status --waiting` reads from
`agent_status.state = 'waiting'`. The old `@prism_waiting` tmux counter is gone. Verify
that manually toggling a session's state in the DB is immediately reflected in
`prism status --waiting` output.

**Bus message delivery lifecycle**: A message written by `prism prompt` must appear in
`bus_messages` with `delivered_at IS NULL`, then transition to `delivered_at IS NOT NULL`
after the plugin delivers it. Verify no messages are stuck pending indefinitely for live
sessions.

**Prune does not over-delete**: `db.Prune(90 * 24 * time.Hour)` deletes:
- `agent_events` rows older than 90 days
- `bus_messages` rows where `delivered_at IS NOT NULL` and `delivered_at` is older
  than 90 days

It must **not** delete `agent_status` rows or undelivered `bus_messages` rows
(`delivered_at IS NULL`). Verify after a manual prune run that `agent_status` is
intact and that pending bus messages are preserved.

**`prism restore` idempotency**: Running `prism restore` twice must not result in
duplicate tmux sessions. The `HasSession()` guard must prevent re-creating sessions that
already exist.

**Dashboard and `prism list-sessions` agree**: Both read from `agent_status`. They must
show the same set of active sessions. Verify that a session visible in
`prism list-sessions` is also visible in the dashboard, and vice versa.

**No `sessions.json` references in code**: After Stage 9:
```bash
grep -r "sessions.json" modules/programs/prism/   # must return nothing
grep -r "prism notify"   modules/programs/prism/   # must return nothing (except comments)
grep -r "prism save"     modules/programs/prism/   # must return nothing (except comments)
grep -r "@prism_waiting" modules/programs/prism/   # must return nothing
grep -r "@agent_state"   modules/programs/prism/   # must return nothing
```

---

*Last updated: 2026-04-02*
