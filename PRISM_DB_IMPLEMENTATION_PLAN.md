# Prism DB Implementation Plan

A staged plan for migrating prism's agent state, message history, and inter-agent
communication from tmux primitives to a prism-owned SQLite database (`prism.db`).

Each stage is:
- **Non-breaking** — either purely additive, or a transparent replacement where the
  user notices no behaviour change
- **Single-agent, single-session deliverable** — scoped so one build agent can
  complete it in one session
- **Independently testable** — clear acceptance criteria at the end

Stages must be executed in order. Do not skip stages.

---

## Background: What Is Being Fixed

Prism currently has several architectural weaknesses:

1. Agent state (`active`, `waiting`, `finished`, etc.) is stored as tmux window
   options (`@agent_state`) — volatile, lost on tmux restart, not queryable
2. `prism checkin` uses `tmux capture-pane` + TUI chrome stripping — fragile, lossy,
   loses history when opencode restarts
3. `prism list-sessions` polls tmux windows — slow, no repo scoping
4. `prism prompt` delivers messages via keystroke injection (`SendKeysDelayed`) — racey,
   fragile, depends on the target pane being in the right state
5. Session persistence (`prism save`/`prism restore`) reads opencode's internal SQLite
   DB directly — brittle, opencode's schema may change without notice
6. The `@prism_waiting` counter is maintained by increment/decrement in `prism notify`
   and can drift out of sync on crashes
7. The dashboard queries tmux on every render tick, causing visible popup latency
8. Session names truncate branch path components (`feat/my-thing` → `my-thing`),
   which is confusing for multi-component branch names

The fix: a prism-owned SQLite database (`~/.local/state/prism/prism.db`) as the single
source of truth for all session state, message history, and inter-agent communication.

---

## Database Schema

The schema below is the target. It is created in full during Stage 1. All stages after
that add behaviour on top of this schema without altering it.

Enable WAL mode on creation. All tables use `CREATE TABLE IF NOT EXISTS`.

```sql
CREATE TABLE agent_events (
  id           TEXT PRIMARY KEY,      -- uuid
  session_name TEXT NOT NULL,         -- tmux session: "nixos-config@main"
  repo         TEXT NOT NULL,         -- bare root basename: "nixos-config"
  worktree     TEXT NOT NULL,         -- full worktree path: "/home/ben/code/nixos-config/main"
  opencode_sid TEXT,                  -- opencode session ID (NULL until session.created fires)
  type         TEXT NOT NULL,         -- see event types below
  payload      TEXT NOT NULL,         -- JSON, shape varies by type
  created_at   INTEGER NOT NULL       -- unix timestamp ms
);
CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

CREATE TABLE agent_status (
  session_name TEXT PRIMARY KEY,      -- tmux session name
  repo         TEXT NOT NULL,         -- bare root basename
  worktree     TEXT NOT NULL,         -- full worktree path
  state        TEXT NOT NULL,         -- 'active'|'waiting'|'finished'|'compacting'|'error'|'idle'
  title        TEXT,                  -- current session title, updated on session.updated
  opencode_sid TEXT,                  -- opencode session ID (for restore)
  last_seen    INTEGER NOT NULL,      -- unix timestamp ms, updated on every event
  ended_at     INTEGER                -- unix timestamp ms, NULL = session alive
);

CREATE TABLE bus_messages (
  id           TEXT PRIMARY KEY,      -- uuid
  from_session TEXT NOT NULL,
  to_session   TEXT NOT NULL,
  repo         TEXT NOT NULL,
  text         TEXT NOT NULL,
  urgency      TEXT NOT NULL DEFAULT 'normal',   -- 'normal' | 'interrupt'
  sent_at      INTEGER NOT NULL,
  delivered_at INTEGER                -- NULL = pending
);
CREATE INDEX IF NOT EXISTS idx_bus_pending ON bus_messages(to_session, delivered_at)
  WHERE delivered_at IS NULL;

CREATE TABLE schema_version (
  version INTEGER NOT NULL
);
```

### Event types for `agent_events.type`

| Type                  | Description                                                    |
|-----------------------|----------------------------------------------------------------|
| `msg_user`            | User turn in the conversation                                  |
| `msg_assistant`       | Assistant turn                                                 |
| `tool_call`           | Tool invocation (name + truncated args)                        |
| `tool_result`         | Tool result (truncated)                                        |
| `thinking`            | Thinking/reasoning step if exposed by opencode                 |
| `state_change`        | Agent state transition (active/waiting/finished/etc.)          |
| `compaction`          | Session compacted                                              |
| `permission_ask`      | Permission requested (tool, pattern)                           |
| `error`               | Session error                                                  |
| `tmux_session_start`  | tmux session created (written by tmux hook, not plugin)        |
| `tmux_session_end`    | tmux session closed (written by tmux hook, not plugin)         |

---

## Stage 1: DB Foundation + `prism event` Command ✅ COMPLETE

**Goal**: Lay the database infrastructure. Create the `internal/db` package, add the
`prism event` command, and wire in tmux lifecycle hooks. Nothing changes in existing
behaviour. This is purely additive.

---

## Stage 2: Plugin Writes to DB ✅ COMPLETE

**Goal**: Extend `prism-hooks.ts` to write all opencode events to `prism.db` via
`bun:sqlite`. The existing `prism notify` calls remain in place alongside the new DB
writes — this is a parallel write, not a replacement. After this stage the DB is being
populated live, but nothing reads from it yet.

---

## Stage 3: Session Naming Fix ✅ COMPLETE

**Goal**: Fix `sessionNameFor()` to use the full branch name with `--` substitution for
`/`. This is a one-time breaking change for sessions with multi-component branch names.
Accept the breakage.

---

## Stage 4: `prism status` + `prism list-sessions` from DB ✅ COMPLETE

**Goal**: Swap the data source for `prism status` and `prism list-sessions` from tmux
polling to DB queries. Both commands behave identically from the user's perspective.
After this stage, `@prism_waiting` is no longer the source of truth for waiting count.

---

## Stage 5: `prism checkin` from DB ✅ COMPLETE

**Goal**: Replace `tmux capture-pane` screen scraping in `prism checkin` with DB
queries. The output format changes (timestamps, tool call summaries) — this is an
intentional improvement, not a regression.

---

## Stage 6: `prism save` / `prism restore` from DB ✅ COMPLETE

**Goal**: Replace the `sessions.json` JSON file with DB queries for session persistence.
`prism save` is retired (body becomes a no-op). `prism restore` reads from
`agent_status`. The `SavedSession` struct and file I/O helpers are removed.

---

## Stage 7: Bus + `prism prompt` via DB

**Goal**: Replace keystroke injection in `prism prompt` with a write to `bus_messages`.
The plugin delivers normal-urgency messages on `session.idle` and interrupt-urgency
messages via a polling interval. `SendKeysWhenReady` and `SendKeysDelayed` are retired.

### Files involved

- **Modified**: `modules/programs/prism/prism/cmd/prompt.go`
- **Modified**: `modules/programs/prism/opencode/plugins/prism-hooks.ts`
- **Modified**: `modules/programs/prism/prism/internal/tmux/tmux.go` (to remove/stub
  `SendKeysWhenReady` and `SendKeysDelayed`)

### `prism prompt` changes

Current behaviour: uses `SendKeysDelayed` to inject keystrokes into the target tmux pane.

New behaviour:
1. Open DB
2. Check target session: `db.CurrentStatus(sessionName)`
   - If `ended_at IS NOT NULL`: return error `"session <name> has ended — escalate to
     user to restart if needed"`
   - If `state == 'waiting'`: return existing error (preserved from current behaviour)
3. Write a `bus_messages` row:
   - `id`: new UUID
   - `from_session`: derived from current process's worktree (same logic as plugin)
   - `to_session`: target session name
   - `repo`: derived from target session's `agent_status.repo`
   - `text`: the prompt text
   - `urgency`: `'interrupt'` if `--urgent` flag is set, otherwise `'normal'`
   - `sent_at`: now
   - `delivered_at`: NULL
4. Touch sentinel file `$XDG_STATE_HOME/prism/bus/<session-name>.signal`
   (create the `bus/` directory if it does not exist)
5. Exit 0

The `--urgent` flag must be added to the `prism prompt` command flags.

### Plugin delivery changes

In `prism-hooks.ts`, add delivery logic:

> **Before implementing**: verify the exact `client.session.prompt` and
> `client.tui.showToast` call signatures against the opencode plugin API type
> definitions. Check the `@opencode-ai/plugin` package types installed at
> `~/.config/opencode/node_modules/@opencode-ai/plugin/` or the opencode plugin SDK
> docs. The shapes shown in this section are illustrative and must be confirmed before
> use — neither call appears in the current `prism-hooks.ts`.

**On `session.idle`** (normal-urgency delivery):
```typescript
const pending = pendingMsgs.all(sessionName, 'normal');
for (const msg of pending) {
  await client.session.prompt({ path: { id: opencodeSID }, body: { parts: [{ type: 'text', text: msg.text }] } });
  markDelivered.run(Date.now(), msg.id);
}
```

**Interrupt-urgency delivery (polling interval)**:

After `deriveSessionName()` succeeds and prepared statements are initialised, start a
polling interval for interrupt delivery:

```typescript
setInterval(async () => {
  if (!opencodeSID || !sessionName) return;
  const urgent = pendingMsgs.all(sessionName, 'interrupt');
  for (const msg of urgent) {
    await client.tui.showToast({ body: { message: `Urgent from ${msg.from_session}: ${msg.text.slice(0, 100)}`, variant: 'warning' } });
    await client.session.prompt({ path: { id: opencodeSID }, body: { parts: [{ type: 'text', text: msg.text }] } });
    markDelivered.run(Date.now(), msg.id);
  }
}, 2000);
```

This replaces the `fs.watch()` sentinel approach. The 2-second interval means interrupt
messages are delivered within 2 seconds of being written, with no startup race and no
platform-specific file-watch behaviour. The sentinel file
`$XDG_STATE_HOME/prism/bus/<session-name>.signal` is still touched by `prism prompt` —
it is used by Stage 8's dashboard watcher (Go side). Keep the touch in `prism prompt`.
The plugin no longer watches it.

**For initial spawn prompt** (the first message sent when an agent is spawned):
In `ensureAndSwitchSession` (or wherever the initial keystroke injection happens), replace
the `SendKeysWhenReady` call with a `bus_messages` write + sentinel touch. The plugin
delivers it on the first `session.idle` after `session.created`.

### Retirement

Remove `SendKeysWhenReady` and `SendKeysDelayed` from `internal/tmux/tmux.go`, or stub
them to return an error. Add a comment: `// Retired in Stage 7. Use bus_messages + sentinel instead.`

### Acceptance criteria

- `prism prompt <session> "hello"` writes a row to `bus_messages` and the target agent
  receives the message (delivered by the plugin on next idle)
- `prism prompt <session> --urgent "urgent thing"` delivers within 2 seconds via toast + prompt
- `prism prompt <ended-session> "hello"` returns an appropriate error
- The initial prompt on `prism spawn` is delivered via bus, not keystrokes
- No `SendKeysDelayed` calls remain in non-test code
- `go test ./...` passes

---

## Stage 8: Dashboard from DB + Sentinel Watcher

**Goal**: Replace tmux polling in the dashboard with DB reads and a sentinel file watcher.
Fix the blank-frame-before-first-render bug. Popup and persistent dashboard instances
both read from the same DB. `prism notify` window colouring calls are removed from the
plugin.

### Files involved

- **Modified**: `modules/programs/prism/prism/cmd/dashboard.go` (or the Bubble Tea
  model files — search for `fetchSessions` or `tmux.Sessions()` in the dashboard code)
- **Modified**: `modules/programs/prism/opencode/plugins/prism-hooks.ts`

### Dashboard changes

Find `fetchSessions()` (or equivalent) in the dashboard code. It currently calls
`tmux.Sessions()`.

Replace with `db.AllActiveStatus()`. Map `Status` fields to the existing dashboard
session model. If the dashboard model has fields not in `Status` (e.g. git diff stats),
populate them as empty/zero initially and fill them lazily.

**Skeleton render on startup**: The current dashboard has a blank frame before the first
`WindowSizeMsg` is received. Fix this by rendering a skeleton/loading state immediately
in the `Init()` function before any data is fetched. The skeleton should show the
header and a "loading..." placeholder row.

**Sentinel watcher**: Add a file watcher for `$XDG_STATE_HOME/prism/bus/.dashboard.signal`.
When this file is touched, trigger a re-fetch of session state from DB.

In the `prism event` command (Go), after every write to `agent_events`, touch
`$XDG_STATE_HOME/prism/bus/.dashboard.signal`. This gives the dashboard instant updates
on any state change.

Implement the watcher using a goroutine with `fsnotify` (if already a dependency) or
`inotify` directly. Send a `RefreshMsg` to the Bubble Tea program when the sentinel is
touched.

**Remove polling timer**: If there is a ticker driving periodic refresh, remove it.
Refreshes are now event-driven via the sentinel.

### Plugin changes

Remove window colouring calls from `prism-hooks.ts`. These are the calls to `prism notify`
that set tmux window colours based on agent state. The `@agent_title` set/clear calls
may remain (they are used by the tmux status bar for the window title display) — only
the colour-setting calls are removed.

Concretely: find any call in the plugin that invokes `prism notify <state>` and delete
it. (The DB writes added in Stage 2 remain.)

### Acceptance criteria

- The dashboard opens with a skeleton/loading frame immediately (no blank frame)
- Sessions are rendered from DB, not from tmux polling
- When a session changes state (e.g. from `active` to `waiting`), the dashboard updates
  within ~1 second (sentinel-triggered, not poll-driven)
- Both the popup form (`prism dashboard --popup`) and the persistent form render
  identically and consistently
- No `tmux.Sessions()` call remains in dashboard code
- Window colours are no longer set by the plugin (verify: run a session through idle/active
  cycle and check that no `prism notify` calls appear in the plugin logs)
- `go test ./...` passes

---

## Stage 9: Cleanup

**Goal**: Remove all retired code, update documentation, and verify persistence config.
This is a housekeeping stage — no new functionality.

### Changes

**Remove `prism save` command entirely**:
- Delete `cmd/save.go`
- Remove the command from the cobra root command registration
- Update `prism restart` (`cmd/restart.go`) to remove the `runSave()` call — save is no
  longer needed at restart time
- Remove `saveStatePath()`, `writeSessions()`, `loadSessions()`, `SavedSession` struct
  if not already removed in Stage 6

**Remove `prism notify` command**:
- Delete `cmd/notify.go` (or equivalent)
- Remove the command from the cobra root command registration
- Remove any references to `prism notify` from tmux config and other scripts

**Remove `--repo` flag from `prism spawn`**:
- Find the `--repo` flag in `cmd/spawn.go` (or equivalent) and delete it
- Update any usage strings or help text

**Update prism skill docs**:
- `modules/programs/prism/opencode/skills/prism/SKILL.md`
- Remove all mention of `--repo` flag from `prism spawn` documentation
- Update `prism checkin` docs to reflect new flags (`--last`, `--from`, `--before`,
  `--types`, `--verbose`) and new output format
- Update `prism prompt` docs to mention `--urgent` flag
- Remove any mention of `sessions.json`

**Verify persistence** (no config changes needed):
- Verify `ls ~/.local/state/prism/` shows `prism.db` (it is covered by the existing
  directory persistence — no config change needed).
- Verify `~/.local/state/prism/sessions.json` is removed from persisted paths (if it
  was ever explicitly listed — if not, it is already absent).

**Remove dead code**:
- `SavedSession` struct (if not already removed in Stage 6)
- `writeSessions` / `loadSessions` functions (if not already removed)
- `CapturePaneScreen` function in `internal/tmux` (if no longer referenced)
- `SendKeysWhenReady` / `SendKeysDelayed` stubs in `internal/tmux` (if not removed in Stage 7)
- Any remaining `@prism_waiting` references in tmux config or Go code
- Any remaining `@agent_state` references in tmux config or Go code

**Final format pass**:
- Run `nixfmt .` on any modified `.nix` files
- Run `go fmt ./...` and `go vet ./...` on the prism module

### Acceptance criteria

- `prism save` returns "unknown command" error
- `prism notify` command no longer exists (returns "unknown command" error)
- `prism spawn --repo` returns "unknown flag" error
- Prism skill docs accurately describe the current interface
- `go build ./...` passes with no warnings
- `go test ./...` passes
- `nixfmt .` produces no changes (already formatted)
- `grep -r "sessions.json" modules/programs/prism/` returns no results
- `grep -r "prism notify" modules/programs/prism/` returns no results (except comments)
- `grep -r "SendKeysDelayed\|SendKeysWhenReady" modules/programs/prism/prism/` returns
  no results in non-test, non-comment code
- Verify `ls ~/.local/state/prism/` shows `prism.db` (it is covered by the existing
  directory persistence — no config change needed).
- `ls ~/.local/state/prism/sessions.json` returns "no such file or directory" (or the
  file exists but is stale — delete it manually if present).

---

## Summary Table

| Stage | What changes                          | Breaking? | Behaviour change?          |
|-------|---------------------------------------|-----------|----------------------------|
| 1     | `internal/db`, `prism event`, hooks   | No        | None — additive only       |
| 2     | Plugin writes to DB                   | No        | None — parallel write      |
| 3     | Session naming fix (`--` for `/`)     | Yes       | Session names change once  |
| 4     | `status` + `list-sessions` from DB    | No        | None — transparent swap    |
| 5     | `checkin` from DB                     | No        | Output format improves     |
| 6     | `save`/`restore` from DB              | No        | None — transparent swap    |
| 7     | Bus + `prompt` via DB                 | No        | None — same effect         |
| 8     | Dashboard from DB + sentinel          | No        | Faster refresh, no blank   |
| 9     | Cleanup                               | No        | Dead code removed          |

---

*Last updated: 2026-04-02*
