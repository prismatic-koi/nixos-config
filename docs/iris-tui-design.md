# Iris bubbletea-native TUI — design

> **Status:** Design doc tracker. Filed as `docs/iris-tui-design.md` in the
> repository with a short GitHub issue body summarising and linking back to
> this file.
>
> Child issues land sequentially per the phasing section. Each child PR uses
> `Refs #<tracker>` while in progress; the last child PR uses
> `Closes #<tracker>`.

## Why

Today's `iris tui` is a multi-session dashboard with a thin prompt strip. It
shows session-level state changes but cannot render assistant text, tool
calls, streaming output, or any conversational content. To interact with an
iris session in any substantive way the user has to fall back to `iris
checkin` on the CLI.

Compare with prism's tmux+pi TUI, which renders all of the above richly
because it IS pi. The price prism pays: a hard dependency on tmux as the
window manager and on pi's local terminal rendering. Iris was designed to
separate those concerns — iris owns session orchestration and sandboxed tool
execution as a daemon; it should own its own UI on top, free of tmux and
free of pi's terminal output as a rendering surface.

This design covers the final layer that makes iris usable as the
day-to-day driver. It is a non-backwards-compatible rewrite of the existing
iris TUI surface; there are no real iris conversations yet to migrate.

## Goals

- **Iris owns the screen end-to-end.** Bubbletea renders every assistant
  message, every tool call, every state transition. No reliance on pi's
  terminal output. No reliance on tmux as a window manager.
- **One program, multiple modes.** `iris` (bare) IS the TUI. Today's
  `iris tui` / `iris dashboard` / `iris switch` collapse into a single
  program with internal overlays for picker / dashboard-overview / popups.
- **Single-conversation focus, multi-session awareness.** The primary view
  is one session's conversation. A persistent sidebar lists all active
  iris sessions with state.
- **No tmux integration anywhere in iris.** All existing tmux-touching
  code in `cmd/iris/` and `internal/iris/` is ripped out. The
  `--tmux-format` flag on `iris sessions status` is removed; prism's
  status-right loses its iris segment with no replacement.
- **Hard cutover.** The current dashboard-based TUI is replaced wholesale
  in child PR 1+2. No feature flag, no transition period.
- **Scripting subcommands stay.** `iris spawn`, `iris prompt`, `iris
  checkin`, `iris cleanup`, `iris logs`, `iris merge*`, `iris review`,
  `iris escalate`, `iris investigate`, `iris feedback`, `iris archive`,
  `iris sessions list`, `iris sessions status` (sans `--tmux-format`)
  all stay as CLI subcommands. They are useful in shell pipelines and to
  agents.

## Non-goals

- Cross-session conversation views (no "show me what every agent is doing
  at once" mosaic). The sidebar surfaces state at a glance; the user
  navigates to one session for content.
- Inline session spawning from within the TUI. Spawn happens via `iris
  spawn` on the CLI. The TUI selects from existing sessions.
- Configurable themes beyond the existing palette. Not a UX toolkit.
- Prism dashboard parity. `prism dashboard` is a separate system surface
  for prism sessions and stays exactly as-is.

## Surface

### The one program

`iris` (no args) launches the bubbletea TUI program. Whether this is
implemented by aliasing `iris` → `iris tui` at the cobra level, or by
moving the TUI implementation into the root command and removing the `tui`
subcommand entirely, is a worker decision in child PR 1. The user-facing
behaviour is the same either way:

- `iris` — launch TUI, focused on the most recently active session.
- `iris <session-name>` — launch TUI, focused on the named session.
- `iris --help` — help text and exit.
- `iris <subcommand> ...` — CLI scripting operation; never launches the
  TUI.

Removed entirely: `iris tui` (becomes the root), `iris dashboard`,
`iris switch`, `iris switch-tui`. The dashboard-overview, the session
picker, and any other navigation views become overlays inside the one
program.

### Layout

```
┌──────────┬──────────────────────────────────────────────┐
│ sidebar  │ conversation                                 │
│          │                                              │
│ session1 │ [12:34:56] ● active                          │
│ session2 │                                              │
│ session3 │ user: hello                                  │
│ session4 │                                              │
│ session5 │ assistant: Hi, how can I help...             │
│          │                                              │
│          │   → bash: ls -la /tmp ✓ (3 files)            │
│          │   → read: /tmp/foo.txt ✓ (1.2 KiB)           │
│          │                                              │
│          │ assistant: I see three files in /tmp...      │
│          │                                              │
│          ├──────────────────────────────────────────────┤
│          │ session4 · active · claude-sonnet-4 · $0.05  │
│          ├──────────────────────────────────────────────┤
│          │ > prompt:                                    │
└──────────┴──────────────────────────────────────────────┘
```

- **Sidebar (left):** list of active iris sessions. Selected session
  highlighted. Each row shows session name + colour-coded state.
- **Conversation (right top):** rich rendering of `agent_events` for the
  focused session. Scrollable. See "Renderer" below.
- **Status line:** focused session's iris_state, model, cost/usage,
  tool-execution indicator.
- **Prompt strip (right bottom):** type → enter sends `prompt_deliver`
  for the focused session.

### Renderer — event-to-visual mapping

| Event type | Visual |
|---|---|
| `msg_assistant` | Wrapped text; "assistant:" marker. Streaming-aware (coalesce by message_id when pi 0.72.1 emits deltas; treat one-shot `message` events as complete turns). |
| `tool_call` | One-line card: `→ <tool>: <key-arg>`. |
| `tool_result` | Indented below its `tool_call`: `✓` / `✗ <error>` + summary. |
| `state_change` | Dim status line: `● <state>` with timestamp. |
| `turn_start` / `turn_end` | Implicit visual separator (vertical spacing). |
| `session_status` | Suppressed in conversation; visible only in sidebar's per-session row. |
| `extension_error` | Prominent error block — these are fatal-class and need to be loud. |

The existing narrative-view renderer in `cmd/iris/checkin.go` +
`internal/iris/narrative/` is the starting point. The TUI's renderer
shares the same event-type → visual-line mapping, with additional support
for streaming (live updates as new rows arrive on the
`session_subscribe` channel).

### Overlays

The current `iris tui` already has overlay machinery
(`internal/iris/tui/overlay.go`). Reuse it for:

- **Picker** (`C-f`) — choose a session to focus.
- **Dashboard-overview** (`C-w`) — a transient full-screen multi-session
  state view, dismissed back to the conversation view on any keystroke.
- **Help** (`?`) — keybinding reference.

The tmux-coexistence logic in those handlers is removed (see "Tmux
removal" below); iris always owns these bindings.

## Tmux removal — concrete sites

### Code to delete

- `cmd/iris/dashboard.go` — entire file. Today execs `tmux new-session
  -ds iris-dashboard` and `tmux attach-session`.
- `cmd/iris/switch.go`, `cmd/iris/switch_tui.go` — entire files. Picker
  collapses into the in-TUI overlay.
- `internal/iris/dashboard/` — entire package. Currently a tmux-hosted
  dashboard implementation. Replaced by the bubbletea program in child
  PR 2+.
- `cmd/iris/sessions_status.go --tmux-format` — flag removed. Prism's
  status-right loses its iris segment; `modules/programs/prism/tmux.nix`
  is updated to drop the iris invocation from the status-right
  composition.

### Comments / coexistence logic to strip

- `internal/iris/tui/model.go` — comments about tmux popup bindings on
  `C-f` / `C-w`. The coexistence reasoning goes away; iris owns these
  bindings unconditionally.
- `internal/iris/tui/overlay.go` — same.
- `cmd/iris/merges.go`, `cmd/iris/escalate.go`, `cmd/iris/prompt.go`,
  `cmd/iris/review.go` — comments referring to "tmux attach to ..."
  workflows; audit and update.
- `internal/iris/cleanup.go` — any tmux-related cleanup paths; audit.
- `internal/iris/supervisor.go` — comments about historical
  tmux-coexistence (e.g. line 769, line 1283-1285); update to reflect
  iris no longer touches tmux.

### Nix configuration

- `modules/programs/prism/tmux.nix`:
  - Lines around 212/229 — bindings for `prefix+i` (iris switch popup),
    `C-q` (iris dashboard popup), `prefix+I` (persistent iris-dashboard
    tmux session). All removed.
  - Line 143 — `iris sessions status --waiting --tmux-format` in
    status-right composition. Removed.

Net effect on prism's tmux config: iris is invisible to tmux. Tmux
remains as it is for prism's own use.

### What iris no longer does

Today some agents and operators rely on:

- Tmux popups to open the iris dashboard / switch / etc.
- The status-right iris waiting-count segment.

Both go away. The user invokes `iris` directly in a terminal window or
tmux pane like any other interactive program. Tmux may still launch
iris (e.g. via a prism-side binding that opens `iris` in a new pane),
but iris itself does not depend on tmux and does not assume tmux is
present.

## Phasing — child issues

Each child is its own PR; later children depend on earlier ones landing.

### Child 1 — tmux excision + command consolidation

- Delete `cmd/iris/dashboard.go`, `cmd/iris/switch.go`,
  `cmd/iris/switch_tui.go`, `internal/iris/dashboard/`.
- Make `iris` (no args) launch the bubbletea TUI program currently
  implemented as `iris tui`. The cleanest implementation is probably
  to move the TUI implementation to the root command's `Run` function
  and remove the `tui` subcommand, but `iris` → `iris tui` aliasing
  via a `Run` shim on the root command is also acceptable. Worker's
  call.
- `iris <session-name>` (single positional, not matching a known
  subcommand) opens the TUI focused on that session.
- Drop `--tmux-format` from `iris sessions status`. Drop the iris
  invocations from `modules/programs/prism/tmux.nix`.
- Strip tmux references from iris source comments.
- The TUI in this PR is functionally unchanged from today's `iris
  tui` — same layout, same renderer. Subsequent PRs make it useful.
- Worker validates: `iris` launches; `iris sessions list` and other
  scripting subcommands still work; nh switch on a navi-like host
  produces a working iris with no tmux dependencies.

**Branch hint:** `iris-tmux-excision`

**Scope estimate:** medium PR. Lots of deletion, some cobra
restructuring, a few imports adjusted.

### Child 2 — renderer plumbing

- Refactor the right-pane event renderer in
  `internal/iris/tui/model.go` so it dispatches per-event-type via a
  clean visitor pattern.
- Implement the visual mappings in the renderer table above:
  `msg_assistant` rich text, `tool_call` one-line cards, `tool_result`
  indented summary, `state_change` dim line, `extension_error`
  prominent block.
- Suppress `session_status` events from the conversation view; they
  remain in the sidebar's per-session row.
- Implement the status-line bottom strip with focused session's state,
  model, cost (if available).
- No streaming yet — render `msg_assistant` events as complete rows
  when they arrive. Streaming is child 3.

**Branch hint:** `iris-renderer`

**Hard dependency:** child 1 (must land first). **Soft dependency:**
#1764 (pi event vocabulary fix) — until that lands, no
`msg_assistant` events arrive at all and the renderer has nothing to
render. Child 2 can be developed against test fixtures in the
meantime; merging it without #1764 produces no user-visible
improvement.

### Child 3 — streaming text

- Once #1764's diagnosis is in, handle pi 0.72.1's actual event
  vocabulary. Render text deltas as they arrive; coalesce into
  complete-message rows once the turn ends.
- If pi emits only complete `message` events (the JSONL evidence
  suggests this), child 3 collapses to "ensure complete messages
  render immediately on arrival" — possibly already done in child 2.

**Hard dependency:** #1764 + child 2. Possibly empty if #1764
resolves it.

### Child 4 — tool-call cards

- Replace one-line tool placeholders with multi-line cards: tool name,
  args (truncated), result preview, expand/collapse on `tab`.
- Distinct visual for in-flight tool execution vs completed.

**Hard dependency:** child 2.

### Child 5 — history and scroll

- Bubbletea viewport with full conversation scrollback. PgUp/PgDn
  scroll history. Latest auto-tails.
- Lazy-load older `agent_events` from DB when scrolling past the
  in-memory window.

**Hard dependency:** child 2.

### Child 6 — sidebar polish

- Waiting-state indicator.
- Last-event timestamp per session.
- Last-assistant-message preview (optional).

**Hard dependency:** child 1. Independent of children 2-5.

### Child 7 — coordinator-only affordances

- When focused session is a coordinator, surface coordinator-specific
  signals (escalations arriving, merge-queue notifications) more
  prominently.
- Possibly a dedicated coordinator-events overlay (`C-e` or similar).

**Hard dependency:** child 2 (needs the renderer infrastructure).

The final child PR closes the tracker.

## Open questions

- **Streaming-or-batched.** Resolved by #1764's worker turn. Child 3's
  scope is contingent.

- **Help / discoverability for the in-TUI overlays.** The current TUI
  has a help overlay on `?`; expand it to cover the new conversation
  view's bindings.

- **What happens when no sessions exist?** Today's TUI shows an empty
  session list. The new TUI should show a helpful prompt — "no
  sessions; run `iris spawn --worktree <path> --role <role>`" or
  similar. Minor; can land in any child PR.

## References

- #1625 — daemon-mode rollout tracker. Iris-as-daemon is its scope;
  iris-as-TUI is this doc's scope. Distinct but related.
- #1764 — pi 0.72.1 event vocabulary mismatch. Blocking for child 2's
  user-visible impact.
- prism's tmux + pi rendering — the visual reference iris's
  conversation view replicates without inheriting tmux as a
  dependency.
