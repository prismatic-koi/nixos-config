# Daemon-mode design: daemon-supervised pi RPC + per-tool sandboxing

**Status:** Design proposal (issue #1599). Prescriptive for PoC scope once
accepted; open questions listed in §11 are explicitly deferred.

**Author:** worker agent (prism session `daemon-mode-design`)
**Date:** 2026-05-15

**D-1 refresh (issue #1630, PR #1628):** §2, §3.2, §3.3, §3.4–§3.5 (new),
§5, §6.1, §6.3, §7.1, §7.2, §8.2, §9.2, §10.1, §11 updated to reflect the
D-1 audit findings (`pi-rpc-interface.md`). The headline change: the §2
mechanism pivots from RPC-delegation (`tool_call`/`tool_result` interception)
to `pi.registerTool()` override — the prism extension overrides the seven
built-in tools with shims that forward to the iris daemon over the per-session
harness socket.

---

## Table of contents

1. [Motivation](#1-motivation)
2. [Why these two changes are one design](#2-why-these-two-changes-are-one-design)
3. [Daemon process model](#3-daemon-process-model)
4. [Client IPC surface](#4-client-ipc-surface)
5. [Tool inventory and per-tool sandbox decisions](#5-tool-inventory-and-per-tool-sandbox-decisions)
6. [Threat model](#6-threat-model)
7. [Per-tool credential brokering](#7-per-tool-credential-brokering)
8. [Persistence model](#8-persistence-model)
9. [Edge cases](#9-edge-cases)
10. [Coexistence strategy](#10-coexistence-strategy)
11. [Open questions](#11-open-questions)
12. [Child-issue breakdown](#12-child-issue-breakdown)

---

## 1. Motivation

### 1.1 tmux as an external dependency is leaky

tmux is the current session host for every pi process. It provides a visual
shell pane, but it is a poor lifecycle manager:

- **Persistence across reboots is weak.** `prism restore` re-creates tmux
  sessions after a reboot, but the pi processes themselves are gone. The
  restore script polls readiness and is brittle when sessions fail to start.
- **Capture-pane is already legacy.** `prism checkin` reads from the prism
  SQLite DB (`internal/db`), not from tmux capture-pane. `cmd/checkin_legacy.go`
  is the tombstone of an approach that was already abandoned. tmux's remaining
  job as a data source is zero.
- **Crash recovery is manual.** If a tmux session dies or is accidentally
  destroyed, the sidecar orphans and the DB is left in a stale `active` state
  that the circuit breaker must clean up.
- **tmux is a hard system dependency.** Non-tmux environments (CI, headless
  servers, web UI consumers) cannot host pi sessions without shimming around tmux.

### 1.2 The `C-w` switcher UX problem

The current `C-w` popup (`prism dashboard --popup`) shows session status but
navigating between sessions requires switching tmux windows/sessions. The
flow is:

1. Open popup (opens a tmux display-popup).
2. Select a session.
3. Popup closes, tmux switches to that session's window.
4. Repeat.

This is lossy: if you navigate away and back, context about what the previous
session was doing is in the scrollback buffer, not in a structured pane. There
is no split view, no side-by-side comparison, no ability to watch two sessions
simultaneously. A daemon-owned IPC surface enables a bubbletea TUI where the
switcher can live-preview any session's output in a side panel without tmux
window switching.

### 1.3 The fake-home symlink pain

The current bwrap/sandbox-exec sandboxing strategy wraps the *entire* pi
process in a container. To give pi access to credentials, git config, SSH
keys, AWS tokens, and Atlassian OAuth tokens, the sandbox must reconstruct a
fake home directory with bind-mounts or symlinks pointing at the real
artefacts. This "fake-home dance" has been a chronic source of bugs:

- Claude credentials on macOS are in the Keychain, not on disk, so
  `writeClaudeCredentials()` in `internal/container/credentials.go` must
  extract them and write a temp file before each session start.
- AWS SSO tokens must be mounted read-write because the CLI refreshes them
  at runtime (see `mounts.go`: `awsSSOReadOnly = false`).
- Atlassian OAuth tokens live in `~/.mcp-auth` and must be mounted
  conditionally (only when the directory exists).
- The Atlassian MCP extension's OAuth token store at
  `~/.pi/agent/atlassian-mcp-oauth.json` must be touched (created if absent)
  and bind-mounted read-write so that tokens written by `/login-atlassian`
  inside the sandbox persist to the host path across sessions.
- The per-session gitconfig and `.git` pointer file must be synthesised and
  mounted over the worktree's `.git` to fix path resolution inside the
  sandbox (`credentials.go` comment: "GIT_COMMON_DIR breaks ref lookup").
- On Darwin, Unix sockets cannot be reliably bind-mounted across the
  sandbox boundary (issue #661: virtiofs returned ENOTSUP), requiring a
  TCP fallback for every inter-process communication path.

Every one of these is a workaround for the same root cause: pi needs the
host environment, but the sandbox is trying to prevent it from having the
host environment. The new model inverts this: pi runs on the host with the
real environment; the sandbox applies only to the tool calls that need it.

### 1.4 The multi-client (TUI + web UI) opportunity

Today there is one UI surface per session: a tmux pane. A second viewer
(a web browser, a mobile client, a second terminal) cannot attach without
separate tooling. A central daemon with a structured IPC surface makes
multiple clients a first-class primitive. A bubbletea TUI and a web UI
become siblings, both consumers of the same daemon's event stream, with
no tmux dependency on either side.

---

## 2. Why these two changes are one design

The proposal has two components:

1. **Daemon-supervised pi subprocesses** (pi runs as a daemon-managed child in
   `--mode rpc` mode, not as a tmux-hosted process).
2. **Per-tool sandboxing** (the sandbox boundary moves from "wrap the pi
   process" to "wrap each tool call").

It might appear that these could be adopted independently. They cannot, for
the following reason:

**The daemon must own tool dispatch to sandbox each call.**

In the current model, the sidecar owns the sandbox lifecycle: it starts bwrap
or sandbox-exec and pi lives inside. Tool calls are pi's own business — the
sidecar observes them via SSE events but does not intercept them.

In the new model, pi runs unsandboxed. If the daemon does not own tool
dispatch, there is no layer at which to insert the per-call sandbox.

A structural consequence of pi running unsandboxed on the host is that iris
has no hostapi-equivalent surface — there is no sandbox boundary around the
pi process that iris CLI invocations need to puncture in order to reach the
daemon. See §5.1 for the three audit invariants that lock this property in
writing.

**Mechanism (post-D-1 pivot):** The D-1 audit (`pi-rpc-interface.md`)
confirmed that pi 0.72.1 does *not* support daemon-delegated tool execution
via the RPC channel — the `tool_result` stdin command assumed by the original
§3.3 does not exist, and extensions can only observe or block tool calls, not
redirect them to an external executor. The mechanism is therefore:

The **prism extension** (loaded into pi at session start) calls
`pi.registerTool()` for each of pi's seven built-in tools (`read`, `bash`,
`edit`, `write`, `grep`, `find`, `ls`), mirroring each built-in's full
`ToolDefinition` (schema, label, description, prompt snippets) but replacing
the `execute()` callback with a shim that:

1. Forwards the tool call to the iris daemon over the **per-session harness
   socket** (the existing `PRISM_HARNESS_PIPE` channel) as a `tool_exec`
   request frame.
2. Awaits a `tool_exec_result` response frame from the daemon.
3. Returns the result to pi as the tool's output.

Pi's tool registry uses last-registered-wins semantics
(`agent-session.js:1830`), so the override takes effect immediately. The LLM
sees the same tool surface as today. The daemon runs the actual subprocess
inside a per-call sandbox and returns the result. Execution is daemon-mediated
without any upstream pi change.

This is the correct substitution for the original RPC-delegation premise: the
daemon still owns tool dispatch and is the enforcement point for per-call
sandboxing. The interception point is inside pi's extension runtime rather
than on pi's stdin/stdout pipe.

Conversely, the per-tool sandbox is only safe if the daemon owns the
execution context. If pi ran unsandboxed inside tmux (without the daemon's
extension overrides), model-generated tool calls would execute unrestricted on
the host. The daemon is the enforcement point.

The two ideas form a single coherent system: the daemon owns the loop
(spawn, supervise, override, sandbox, return), and the loop is what makes
both the UX improvements and the tighter security boundary achievable.

---

## 3. Daemon process model

### 3.1 Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│  daemon (long-lived, one per user)                                  │
│                                                                     │
│  ┌───────────────┐   ┌────────────────────────────────────────────┐ │
│  │ session table │   │ per-session supervisor goroutine           │ │
│  │  name → state │   │  ┌──────────────────────────────────────┐  │ │
│  │               │   │  │ pi --mode rpc child process          │  │ │
│  │               │   │  │  (JSON-lines in/out via stdin/stdout) │  │ │
│  │               │   │  │  prism extension loaded:             │  │ │
│  │               │   │  │   registerTool() overrides active    │  │ │
│  │               │   │  └──────────────────────────────────────┘  │ │
│  └───────────────┘   │  tool call dispatcher (via harness socket) │ │
│                      │   ├── read/edit/write/grep/find/ls → worktree│ │
│                      │   ├── bash → restricted subprocess          │ │
│                      │   └── MCP → (separate; see §5)              │ │
│                      └────────────────────────────────────────────┘ │
│                                                                     │
│  IPC socket: ~/.local/state/<codename>/<codename>.sock              │
└─────────────────────────────────────────────────────────────────────┘
        ▲              ▲
        │              │
  bubbletea TUI    web UI (future)
```

### 3.2 Spawning a pi session

The daemon spawns each pi instance as a child process:

```
pi --mode rpc [--session <path>] [--agent <role>]
```

In `--mode rpc`, pi reads JSON-line commands from stdin and writes JSON-line
events to stdout. The daemon holds both ends of the pipe. The `--mode rpc`
flag is confirmed in pi 0.72.1 (`pi-rpc-interface.md` §Q1).

There is **no `--worktree` flag** in pi 0.72.1. Pi uses the process working
directory (`cwd`) as its worktree. The daemon `chdir()`s to the session's
worktree before calling `os.StartProcess` / `exec.Cmd` so that pi's file
operations are automatically scoped to the correct directory.

The spawn sequence is:

1. **Daemon receives a `spawn` request** from a client (IPC, CLI wrapper, or
   internal trigger). The request names a worktree path, a role
   (`worker`/`coordinator`/`review-*`), and optional config overrides.
2. **Daemon allocates a session record** in the DB (same schema as today,
   `agent_status` + `sessions` tables). The session is assigned an
   `instance_id` (UUID) and a `state` of `spawning`.
3. **Daemon resolves credentials** for the session (§7). Credentials are
   placed into the pi child's environment, not into a fake-home mount.
4. **Daemon writes a per-session pi config** that loads the prism extension
   (pointing at the extension file on disk). The host's
   `~/.config/pi/settings.json` does **not** auto-load the prism extension;
   iris writes a scoped config for each session so that the extension is
   active only within iris-managed sessions. See §3.4 for the escape-hatch
   policy.
5. **Daemon starts the pi child process** via `os.StartProcess` or
   `exec.Cmd`. The child's cwd is set to the session worktree. The child's
   stdin/stdout are connected to the daemon's per-session pipe goroutines.
   stderr is captured to a log file. The `IRIS_DAEMON_SOCK` env var is set
   to the path of the per-session harness socket.
6. **Pi loads the prism extension**; the extension dials the harness socket,
   performs the `hello`/`hello_ack` handshake, and calls `pi.registerTool()`
   for all seven built-ins (gated on `IRIS_DAEMON_SOCK` — see §3.4).
7. **Extension calls `pi.getAllTools()` and asserts the tool surface** (§3.5).
   Any fatal divergence aborts the session before any LLM turn runs.
8. **Pi sends its first RPC events** on stdout. The daemon monitors these
   for the `agent_start` event, then transitions the session to `active`
   and marks it ready.
9. **Supervisor goroutine runs** for the lifetime of the child (§3.4 lifecycle
   section, now renumbered as §3.6 below).

### 3.3 Protocol surfaces

D-1's audit (`pi-rpc-interface.md` §Q3) found that the design doc's original
§3.3 frame table conflated two separate, distinct protocols. They are
documented separately here.

#### 3.3.1 Pi RPC protocol (stdin/stdout of `pi --mode rpc`)

The pi RPC protocol is an **observation + steering** protocol, not a
**delegation** protocol. The daemon sends commands (prompts, model switches,
abort) and receives lifecycle events. Tool execution is **not** part of this
protocol — it flows through the harness socket instead (§3.3.2).

**Pi → daemon direction (events on stdout):**

| RPC event | Meaning |
|---|---|
| `agent_start` | Agent begins processing a prompt |
| `agent_end` | Agent finishes; contains all messages for the run |
| `turn_start` | Turn boundary: a new LLM turn begins |
| `turn_end` | Turn boundary: turn complete; carries message and tool results |
| `message_start` | Assistant message begins |
| `message_update` | Streaming text/thinking/tool-call delta |
| `message_end` | Assistant message complete |
| `tool_execution_start` | Tool begins execution (observation only; execution is via §3.3.2) |
| `tool_execution_update` | Streaming tool output delta |
| `tool_execution_end` | Tool completes (observation only) |
| `auto_retry_start` | Provider retry begins; fields: `attempt`, `maxAttempts`, `delayMs`, `errorMessage` (camelCase) |
| `auto_retry_end` | Provider retry ends; fields: `success`, `attempt`, optional `finalError` |
| `queue_update` | Pending steer/follow-up queue changed |
| `compaction_start` / `compaction_end` | Context compaction lifecycle |
| `extension_error` | Extension threw an error |
| `extension_ui_request` | Extension requested user interaction |
| `response` | Acknowledgement to any command; carries `success`, `command`, optional `data` or `error` |

**Daemon → pi direction (commands on stdin):**

| RPC command | Meaning |
|---|---|
| `prompt` | Deliver a user message; fields: `message`, optional `images`, optional `streamingBehavior` |
| `steer` | Queue a steering message; fields: `message`, optional `images` |
| `follow_up` | Queue a follow-up message; fields: `message`, optional `images` |
| `set_model` | Switch pi's active model; fields: `provider`, **`modelId`** (not `model`) |
| `abort` | Trigger pi's abort handler |
| `bash` | User-initiated shell command injected into LLM context; runs in pi's own process, **not** sandboxed by iris (see §6.1) |
| `get_state` | Request current session state |
| `compact` | Compact context; optional `customInstructions` |

**Frames removed from the original design doc's table:**
- `tool_call` / `tool_result` — these are **not** pi RPC frames (confirmed by
  D-1's `RpcCommand` union type audit). `tool_call` / `tool_result` remain as
  *harness socket* frames (§3.3.2 and `pi-wire-protocol.md` §5.3/§5.4).
- `hello` / `hello_ack` — these are harness socket handshake frames (§4 of
  `pi-wire-protocol.md`). No handshake exists in the pi RPC stdin/stdout
  protocol.
- `session_shutdown` — pi exits on stdin EOF; no RPC event is emitted. The
  prism extension emits `session_shutdown` to the sidecar over the harness
  socket on pi's `session_shutdown` hook.
- `state_change` — not a direct RPC event. The prism extension emits
  `state_change` to the sidecar over the harness socket, translated from
  pi's `agent_start`/`agent_end` RPC events.
- `register_provider` / `set_active_tools` — these exist on the harness socket
  only (§6.4/§6.5 of `pi-wire-protocol.md`). They are not pi RPC stdin
  commands.

#### 3.3.2 Iris harness-socket protocol (per-session Unix socket)

The prism extension running inside pi communicates with the iris daemon over
the existing per-session harness socket (`PRISM_HARNESS_PIPE`). This is the
same socket used by the current prism sidecar (`pi-wire-protocol.md`). In
daemon mode, iris is the server rather than the sidecar.

The existing frame catalogue (`pi-wire-protocol.md` §5 and §6) is inherited
unchanged. The following frame types are **added** in daemon mode to support
the `registerTool()` override mechanism:

**Extension → daemon (new frames):**

| Frame type | Meaning |
|---|---|
| `tool_exec` | Tool invocation forwarded from the overridden built-in shim; fields: `id` (tool-call ID), `name` (tool name), `args` (tool arguments object) |

**Daemon → extension (new frames):**

| Frame type | Meaning |
|---|---|
| `tool_exec_result` | Result of a tool invocation executed by the daemon in a sandbox; fields: `id` (matching `tool_exec.id`), `success` (boolean), `output` (string) |

The extension's overridden tool shim:
1. Sends a `tool_exec` frame to the iris daemon.
2. Awaits the matching `tool_exec_result` frame.
3. Returns `output` to pi as the tool result (or surfaces an error if
   `success=false`).

The existing `tool_call` and `tool_result` frames (`pi-wire-protocol.md`
§5.3/§5.4) continue to be emitted by the extension to the daemon for DB
logging — they are observation frames that record tool activity in
`agent_events`, independent of the `tool_exec`/`tool_exec_result` dispatch
channel.

> **TODO:** Add the `tool_exec` / `tool_exec_result` frame schemas as a new
> section in `pi-wire-protocol.md` (follow-up to this PR; the frames are
> normatively defined here for D-3 implementers).

### 3.4 Extension load policy and escape hatch

The iris daemon controls the prism extension load policy. Specifically:

- **The daemon writes a per-session pi config** that loads the prism extension.
  The host's `~/.config/pi/settings.json` is not modified. Extension loading
  is scoped to iris-managed sessions only.

- **The extension's `registerTool()` calls are gated on `IRIS_DAEMON_SOCK`.**
  When the env var is set (i.e. pi was spawned by iris), the extension's
  `session_start` handler connects to the harness socket and overrides all
  seven built-in tools with the iris dispatch shims. When the env var is
  absent, the `session_start` handler is a no-op: no tools are overridden, the
  harness socket is not contacted, and pi runs with vanilla built-in tool
  implementations.

- **Emergency escape hatch:** a user can always run plain `pi` outside iris
  control by ensuring `IRIS_DAEMON_SOCK` is not set in their environment.
  This is useful for:
  - Debugging iris itself without interference from the extension.
  - Recovering from a broken daemon (pi continues to function with full
    built-in tools).
  - Auditing pi's vanilla behaviour independent of iris.

  Because the extension is a no-op without the env var, the escape hatch
  does not require any extension removal or config change — clearing the
  env var is sufficient.

### 3.5 Tool surface check at session start

After the prism extension's own `registerTool()` calls complete, and before
any LLM turn runs, iris calls `pi.getAllTools()` and asserts that the
LLM-facing tool surface is exactly as expected. This check is the primary
defence against unauthorised tool access.

**Three fatal conditions:**

1. **Unknown built-in:** a tool with `sourceInfo.source === "builtin"` has a
   name that is not in the canonical seven (`read`, `bash`, `edit`, `write`,
   `grep`, `find`, `ls`). An unknown built-in means pi has added a new tool
   that iris has not reviewed or sandboxed — it would execute unsandboxed.

2. **Unauthorised extension-registered tool:** a tool with
   `sourceInfo.source === "extension"` was registered by an extension whose
   name is not on the iris allowlist. The initial allowlist is:
   `prism`, `atlassian`, `anthropic-oauth`. Any tool from an extension
   outside this list is fatal — iris cannot sandbox or validate it. Updating
   the allowlist is a config change, not a code change.

3. **Failed override of a canonical built-in:** one of the seven canonical
   tools, after `registerTool()` calls have run, still resolves to the
   original built-in implementation rather than the iris override shim. This
   means the sandboxing mechanism silently failed for that tool.

**On any fatal condition:**

The extension surfaces a clear user-facing message identifying the failing
tool and the resolution:
- *Unknown built-in* → update iris's tool allowlist or upgrade iris to
  support the new tool.
- *Unauthorised extension tool* → add the extension to the iris allowlist
  or remove the extension.
- *Failed override* → indicates an iris bug or a pi API change; unset
  `IRIS_DAEMON_SOCK` to use vanilla pi while the issue is resolved.

The session is aborted before any LLM turn runs. **There is no
degraded-operation mode.** The session either runs with the full per-tool
sandbox or does not run at all. A partial sandbox (some tools overridden,
some not) is worse than no sandbox because it gives a false sense of
security.

### 3.6 Supervised lifecycle

Each pi child has a **supervisor goroutine** that:

- Reads stdout frames and dispatches them (tool-execution observation frames
  go to DB writes; state frames go to DB writes; message frames go to the
  event fan-out).
- Writes stdin frames (prompt deliveries, model switches).
- Monitors `cmd.Wait()` for the child process to exit.
- Applies a **restart policy** (§3.6.1) when the child exits unexpectedly.

#### 3.6.1 Restart policy

| Exit condition | Action |
|---|---|
| Clean `session_shutdown` (harness socket) + exit 0 | Transition to `finished`. No restart. |
| Exit 0 without `session_shutdown` | Log anomaly. Transition to `finished`. No restart. |
| Non-zero exit, restart count < N | Transition to `error`, wait `backoff(N)`, restart child. |
| Non-zero exit, restart count ≥ N (circuit breaker) | Transition to `error`. No further restarts. Notify coordinator if applicable. |
| Context cancelled (daemon shutdown) | Send `abort` to pi via RPC stdin, wait up to `shutdownTimeout`, then SIGTERM/SIGKILL. |

The restart count threshold `N` and the backoff schedule are configurable
in the daemon's config file (open question: §11.2). The current prism
`DefaultSidecarCircuitBreakerThreshold = 3` is the precedent.

#### 3.6.2 In-flight tool calls at restart

When the child exits with an in-flight tool call (a `tool_exec` frame was
received on the harness socket but no `tool_exec_result` has been sent back):

- If the tool subprocess is still running, the daemon sends SIGTERM to it
  and waits for `toolSubprocessKillTimeout` (default: 5 s).
- The tool result is discarded — it will never be delivered to pi because
  pi is gone.
- If the session restarts, the new pi child begins a fresh session; the
  orphaned tool call is not replayed. The conversation history from the DB
  is available for context, but the tool invocation is lost.

See also §9.1 for the daemon-crash variant of this edge case.

---

## 4. Client IPC surface

### 4.1 Transport

The daemon exposes a Unix domain socket:

```
~/.local/state/<codename>/<codename>.sock
```

(On Darwin, a localhost TCP listener on a well-known or OS-allocated port is
a fallback, following the same Darwin convention as the existing prism
host-API and pi harness pipe.)

The socket accepts multiple simultaneous connections. Each connection is an
independent client session on the same daemon.

### 4.2 Wire protocol

Client↔daemon messages are JSON-line frames, same framing as the pi RPC
protocol. Both sides must tolerate unknown `type` values (log and skip)
for forward compatibility.

#### 4.2.1 Client → daemon frames

| Frame type | Meaning |
|---|---|
| `sessions_list` | Request the list of all active sessions |
| `session_subscribe` | Subscribe to events from a named session |
| `session_unsubscribe` | Unsubscribe |
| `session_spawn` | Spawn a new pi session |
| `session_kill` | Kill/cleanup a session |
| `prompt_deliver` | Deliver a prompt to a session |
| `ping` | Keepalive |

#### 4.2.2 Daemon → client frames

| Frame type | Meaning |
|---|---|
| `sessions_snapshot` | Current state of all sessions (response to `sessions_list`) |
| `session_event` | Event from a subscribed session (carries raw DB event) |
| `session_state` | State transition for a subscribed session |
| `session_spawned` | Acknowledgement for `session_spawn` |
| `error` | Error response to a request |
| `pong` | Keepalive response |

### 4.3 Multiple concurrent clients

Multiple clients (TUI instance, web UI, CLI `prism prompt`, etc.) can
subscribe to the same session simultaneously. The daemon maintains a per-session
**fan-out set**: when a new event is written to the DB, the daemon broadcasts
it to all subscribers in the fan-out set.

A client that disconnects is silently removed from the fan-out set. No
session state is affected by client disconnection — the pi child continues
running regardless of whether any client is currently attached.

A client that reconnects after a gap can request a **replay** of events since
a given `event_id` via a `session_subscribe` frame with a `since_event_id`
field. The daemon services the replay from the DB, then streams live events.

### 4.4 TUI attachment model

The bubbletea TUI connects to the daemon socket on startup, sends
`sessions_list`, renders the session list, and subscribes to whichever
session the user selects. The TUI pane for a session renders the event
stream in real time. Switching sessions means sending `session_unsubscribe`
and a new `session_subscribe`; no tmux session switching occurs.

The TUI's "send a prompt" action sends a `prompt_deliver` frame to the
daemon, which forwards it to pi as a `prompt` RPC frame.

### 4.5 Web UI attachment model

A future web UI can connect over a WebSocket proxy in front of the daemon
socket, or the daemon can optionally expose an HTTP/WebSocket endpoint
natively. The message schema is the same; the transport differs. This is
an open question (§11.3).

---

## 5. Tool inventory and per-tool sandbox decisions

§5.1 below documents iris's structural immunity to the prism hostapi-class of
failure and the three grep-verifiable invariants that preserve it. The table
in this section is the positive description of which tools are sandboxed and
how; §5.1 is the negative description — the IPC surface that iris
deliberately does **not** expose into any sandbox.

The following table enumerates the built-in tools pi exposes, sourced from the
[pi README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md)
(`--tools` flag documentation and the "Available built-in tools" list). The pi
tool set must not be conflated with opencode's tool set — they differ
substantially. Notably, pi explicitly ships without sub-agents (`task`), without
a todo tool (`todowrite`), and without `glob` or `webfetch`.

The seven pi built-in tools are: `read`, `bash`, `edit`, `write`, `grep`,
`find`, `ls`.

**MCP-bridge tools.** In addition to the seven built-in tools, the Atlassian
MCP extension (added in #1587, gated on `m4mac` hardware via #1588) is a
**separate category** of tooling from the seven canonical built-ins. MCP-bridge
tools are registered through `pi.registerTool()` by the atlassian extension
itself — they route their own HTTP traffic directly to the Atlassian API and
do not pass through the iris daemon's per-tool sandbox dispatch. The atlassian
extension is on the iris allowlist (`prism`, `atlassian`, `anthropic-oauth`;
see §3.5), so its tools pass the tool surface check. The exact sandbox profile
for MCP-bridge tool execution is deferred to a future child issue.

**anthropic-oauth tool-name obfuscation.** The `anthropic-oauth` extension
obfuscates tool names as `t_<md5>` in outbound Anthropic API requests and
reverses the mapping on inbound response streams. This obfuscation is
implemented at the Anthropic provider's wire layer
(`anthropic-oauth/index.ts:117`, `transforms.ts`). Pi's internal tool
registry, `pi.getAllTools()`, and the iris override mechanism all operate on
the original (pre-obfuscation) tool names. The MD5 obfuscation is transparent
to iris's override mechanism — iris registers tools under their original names
and the obfuscation layer operates beneath iris's visibility. No iris
workaround is required.

**Explicitly excluded from every per-tool mount set** (pi-process concerns;
pi reads these directly from the host in the iris model):

- `~/.claude`, `~/.mcp-auth`, `~/.pi/agent/*`, `~/.cache/bun`, `~/.config/pi/*`
- `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`

These paths and env vars are pi's own credential surface. In the iris model pi
runs unsandboxed on the host and reads them directly — there is no need to
mount them into any per-tool sandbox, and doing so would re-introduce the
fake-home complexity this design is specifically eliminating.

**Sandbox decision key:**

- **Worktree-scoped subprocess** — executed in a subprocess with its working
  directory and filesystem view restricted to the session's worktree.
- **Restricted subprocess** — executed in a subprocess with explicit capability
  restrictions (bwrap on Linux, sandbox-exec on macOS).

**Network access: permitted for all tools.** The per-tool sandbox does not
restrict outbound network access. The security boundary enforced by the
per-tool sandbox is **what data is reachable for exfiltration** (filesystem
isolation), not whether data can leave the machine. Restricting network at
the tool subprocess level would break legitimate tool operations (`git push`,
`gh`, `aws`, Nix fetches) without providing a meaningful security gain, since
the LLM already has unrestricted network access through pi's own process.

| Tool | Sandbox type | Mounts | Credentials | Network |
|---|---|---|---|---|
| `read` | Worktree-scoped subprocess | Worktree: **RO** bind; per-session `/tmp`: RW tmpfs; `/nix/store`: RO; `/etc`: RO (with `/etc/wireguard`, `/etc/wpa_supplicant`, `/etc/ssh` shadowed by tmpfs); `/bin`: RO; `/run/current-system`: RO; `~/.nix-profile`: RO | None | Permitted |
| `grep` | Worktree-scoped subprocess | Same as `read` (worktree RO) | None | Permitted |
| `find` | Worktree-scoped subprocess | Same as `read` (worktree RO) | None | Permitted |
| `ls` | Worktree-scoped subprocess | Same as `read` (worktree RO) | None | Permitted |
| `edit` | Worktree-scoped subprocess | Same as `read` but worktree: **RW** bind | None | Permitted |
| `write` | Worktree-scoped subprocess | Same as `edit` (worktree RW) | None | Permitted |
| `bash` | Restricted subprocess (bwrap on Linux, sandbox-exec on macOS) | All of the above (worktree RW) **plus:** `~/.cache/nix` (RW); `/nix/var/nix/daemon-socket` (RW); synthesised `~/.gitconfig` (RO); `~/.ssh/access-key`, `~/.ssh/signing-key`, `~/.ssh/signing-key.pub`, `~/.ssh/allowed_signers`, `~/.ssh/known_hosts` (all five, RO, same remap as current bwrap); synthesised `~/.ssh/config` (RO); `~/.aws/{config,credentials,sso,cli}` (mixed RO/RW per current rules in `internal/container/mounts.go`); `~/.kube/config` (RO) | Role-scoped `GITHUB_TOKEN` (from 4-PAT architecture). AWS credentials injected conditionally when the session role requires them. **LLM API keys are not injected.** | Permitted |

**Implementation note — reusing `internal/container/mounts.go` plumbing.**
The iris implementation reuses the `MountSpec` emission machinery from
`internal/container/mounts.go`: the `MountSpec` struct, `appendBwrapBind`,
`EvalSymlinks`/`OptionalIfMissing` semantics, and the sensitive-`/etc` tmpfs
shadowing pattern. However, the iris mount list is **rebuilt per-tool** rather
than using `StandardSandboxMounts` wholesale — `StandardSandboxMounts` carries
the full per-process mount set (pi's own credentials, fake home, etc.) that
the iris model deliberately eliminates. Each tool's mount list is assembled
from the subset described in the table above.

### 5.1 Why iris has no hostapi-equivalent surface

**Iris is structurally immune to the prism hostapi-class of failure because
pi runs unsandboxed on the host; only individual tool subprocesses are
sandboxed, and those subprocesses have no IPC path back to the iris daemon.**

In prism, the entire pi session runs inside a sandbox (bwrap on Linux,
sandbox-exec on Darwin, or a container). From inside that sandbox the agent
can still invoke `prism spawn`, `prism prompt`, `prism cleanup`, etc. by
proxying over a Unix socket that the sidecar bind-mounts into the sandbox.
That is the prism host-API surface — and it is easy to forget to wire or
break silently (PR #1647, `fix(feedback): proxy to host-API when invoked
from inside a sandbox`, is a recent example of the failure class).

Iris does not have an equivalent surface because the iris pi session is
never wrapped in a sandbox in the first place (see §2 "the daemon must own
tool dispatch to sandbox each call" and §9.2 "Pi writes to the host
filesystem outside of tool calls" for the structural foundations). The pi
process dials `~/.local/state/iris/iris.sock` directly as a host-resident
process; only the per-tool subprocesses are sandboxed, and they have no
business reaching the daemon.

The three invariants below are grep-verifiable. An auditor running these
commands should see the stated results; any deviation is a regression that
reintroduces the hostapi-class failure surface into iris.

**Invariant 1 — no iris CLI subcommand acquires an `IRIS_HOST_API`-style env
var or proxy path.**

```bash
grep -r IRIS_ modules/programs/prism/prism/cmd/iris/ \
              modules/programs/prism/prism/internal/iris/
```

Only four `IRIS_*` env vars should appear:

- `IRIS_DAEMON_SOCK` — set by the supervisor on the **pi child process**
  (`internal/iris/supervisor.go::buildEnv`) so the prism extension running
  inside pi knows where to dial the per-session harness socket. This is a
  pi-process env var, not a sandbox-process env var; it is never forwarded
  into any tool sandbox.
- `IRIS_SESSION_NAME` — set by the supervisor on the **pi child process**
  (`internal/iris/supervisor.go::buildEnv`, issues #1693 / #1704) so
  worker-side CLIs (`iris escalate`, future `iris prompt` from within a
  session) can identify their calling session without an extra RPC. This
  is the iris analogue of prism's `PRISM_SESSION_NAME`. Like
  `IRIS_DAEMON_SOCK`, it is a pi-process env var only — it is never
  forwarded into any tool sandbox (invariant 2 pins this structurally).
- `IRIS_PARITY_TEST_MODE` — the parity-test harness guard
  (`internal/iris/iristest/iristest.go:54`,
  `internal/iris/parity/parity_isolation_test.go`). Test-only.
- `IRIS_FEEDBACK_ENDPOINT` — host-side opt-in upstream POST target for
  `iris feedback` (`cmd/iris/feedback.go`, issue #1721). Read directly via
  `os.Getenv` from the user's interactive shell when `iris feedback`
  records an entry; if set, the entry is POSTed to the named URL after the
  local-first JSONL write succeeds. This is **not** a daemon-proxy path:
  the supervisor never sets it on the pi child, the credential broker
  never forwards it into any tool sandbox (invariant 2's allowlist is
  closed), and a sandboxed subprocess cannot use it to call back into the
  daemon. It is a configuration knob for the operator's own shell, in the
  same shape as `PRISM_FEEDBACK_ENDPOINT` for prism.

There is no `IRIS_HOST_API`, no `IRIS_CLI_PROXY`, and no analogue of prism's
host-API socket path that a sandboxed subprocess could use to call back into
the daemon.

**Invariant 2 — `credential_broker.go::ResolveBash` does not forward
`IRIS_DAEMON_SOCK`, `IRIS_SESSION_NAME`, or any other `IRIS_*` env var into
the bash sandbox.**

The `ResolveBash` function (`internal/iris/credential_broker.go` lines
~118-199, the env-allowlist body in particular) emits a closed list of
environment entries: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`,
`TERM`, the four `GIT_{AUTHOR,COMMITTER}_{NAME,EMAIL}` vars, `NIX_CONFIG`,
optionally `GITHUB_TOKEN`, and `AWS_CONFIG_FILE` /
`AWS_SHARED_CREDENTIALS_FILE`. No `IRIS_*` entry appears, so the bash
sandbox cannot inherit `IRIS_DAEMON_SOCK` or `IRIS_SESSION_NAME` from the
pi parent process. Any `iris …` invocation from inside the bash sandbox
would fail loudly (the daemon socket path env var is unset, and the socket
itself is not mounted — see invariant 3) rather than silently succeed by
proxy.

The immunity is structural — `ResolveBash` builds the env from a fixed
allowlist, not a block-list — so adding a new `IRIS_*` env var to the
pi-child environment (as #1693 did for `IRIS_SESSION_NAME`) cannot silently
widen the bash-sandbox env surface. A regression in the allowlist would
show up immediately under the grep audit below, and
`credential_broker_iris_env_test.go::TestCredentialBroker_IRISEnvNeverLeaks`
pins the same property as a structural Go test (including a forward-
looking synthetic `IRIS_FUTURE_VAR` so future iris env additions inherit
the guarantee).

Audit:

```bash
sed -n '118,205p' modules/programs/prism/prism/internal/iris/credential_broker.go \
  | grep -n IRIS_
```

Expected: zero matches.

**Invariant 3 — the bash and file sandbox mount sets do not include a
`~/.local/state/iris/` entry.**

```bash
grep -r "local/state/iris" \
  modules/programs/prism/prism/internal/iris/bash_sandbox_linux.go \
  modules/programs/prism/prism/internal/iris/bash_sandbox_darwin.go \
  modules/programs/prism/prism/internal/iris/file_sandbox_linux.go \
  modules/programs/prism/prism/internal/iris/file_sandbox_darwin.go
```

Expected: zero matches. The mount-set builders to inspect are:

- `internal/iris/bash_sandbox_linux.go::bashToolMountsLinux` (lines
  ~99-245) — the Linux bwrap mount list for the bash tool.
- `internal/iris/bash_sandbox_darwin.go::GenerateBashSBPLProfile` (lines
  ~140-280) — the Darwin sandbox-exec profile for the bash tool.
- `internal/iris/file_sandbox_linux.go` — the worktree-scoped Linux
  mount set for `read`/`grep`/`find`/`ls`/`edit`/`write`.
- `internal/iris/file_sandbox_darwin.go` — the Darwin counterpart.

Neither the iris state directory (`~/.local/state/iris/`), nor the daemon
socket (`~/.local/state/iris/iris.sock`), nor the iris DB
(`~/.local/state/iris/iris.db`), nor any per-session harness socket under
`~/.local/state/iris/run/<instance_id>/` is bind-mounted into any tool
sandbox. The file sandboxes are scoped to the session worktree and have no
IPC surface at all; the bash sandbox carries credentials and developer
tooling paths but no iris-internal paths.

**Contrast with prism's hostapi.** Prism's hostapi exists because prism
wraps the entire pi session in a sandbox and then has to puncture that
sandbox to let CLI invocations reach the daemon. Iris doesn't wrap the pi
session at all — so there is nothing to puncture, and no surface to forget
to wire.

**Scope of this section.** This section documents the contract; it does not
add automated enforcement. The three invariants are intended for manual
grep audit during review. An automated lint check is a possible future
addition but is out of scope here. If a future use case requires iris to
support a pi-running-inside-a-broader-sandbox topology, this design
decision should be reopened then; today there is no such use case.

---

## 6. Threat model

### 6.1 Threat actor

The primary threat actor is **model-generated tool calls** — a language
model (or an adversarial prompt injected into the model's context) that
generates tool invocations intended to exfiltrate credentials, modify
files outside the worktree, establish network persistence, or otherwise
abuse the agent's execution context.

A secondary threat actor is **compromised or malicious pi binary code** —
though pi is a first-party dependency, a supply-chain compromise of the
pi package or its dependencies is worth considering at the architecture
level.

The threat model does **not** include:

- A malicious human with shell access to the same machine (they already
  have the credentials directly).
- Cryptographic attacks on the IPC socket (see §4.1 — the socket is
  filesystem-permission-protected at 0600, same as the existing host-API
  socket).
- Kernel-level exploits or container escapes (mitigated by OS patching,
  not by this architecture).

**Note on user-initiated `bash` via the pi RPC command.** Pi's RPC protocol
includes a `bash` command (Appendix A of `pi-rpc-interface.md`) that executes
a shell command and injects the output into the LLM's context. This is a
**user-initiated** action routed through the daemon → pi RPC channel — it is
not a model-generated tool call. The `bash` RPC command runs in pi's own
process on the host, unsandboxed by iris. This is intentional: the threat
model targets *model-generated* tool calls, not commands explicitly sent by
the daemon or user to pi's RPC channel. User-initiated `bash` via the RPC
command is semantically equivalent to the user running a shell command
themselves.

### 6.2 Current model: per-process sandbox

Today, the entire pi process runs inside a bwrap (Linux) or sandbox-exec
(macOS) container. The sandbox boundary is the pi process boundary.

**Attack classes defended against by the current model:**

| Attack class | Current defence |
|---|---|
| Model reads credentials outside the worktree | Fake home has only the credentials needed by the agent; real `~` is not mounted. |
| Model writes files outside the worktree | Only the worktree directory is mounted read-write. |
| Model executes arbitrary network calls | Outbound network is permitted (required for `git push`, `gh`, LLM API calls). Inbound is blocked. |
| Model spawns persistent daemons | The bwrap/sandbox-exec namespace is destroyed when pi exits. |
| Model reads host config files | Not mounted (e.g. `~/.zshrc`, `~/.ssh/config`). |

**Attack classes the current model does NOT defend against:**

| Attack class | Reason |
|---|---|
| Model reads all credentials in the fake home simultaneously | All credentials needed by any tool are mounted at session start, available for the entire session. A model that reads `~/.claude` early in a session and stores the key has it for later exfiltration via a `bash` call. |
| Model reads pi's own in-memory API keys | Pi holds API keys in its process memory; they cannot be removed mid-session without terminating pi. |
| SSH / git credentials accessible to all bash calls | SSH keys and GitHub token are mounted once; every bash call in the session can read them. |

### 6.3 New model: per-tool sandbox

In daemon mode, pi runs **unsandboxed on the host**. The sandbox moves to
the tool call boundary. Each tool invocation executes in an isolated
subprocess that receives only the credentials it needs for that specific
call.

**Attack classes defended against by the new model:**

| Attack class | New defence |
|---|---|
| Model reads credentials outside worktree via `read`/`edit`/`write`/`grep`/`find`/`ls` | These tools run in a worktree-scoped subprocess with no access to `~` or any other path. Path traversal is enforced before execution. |
| Model accumulates credentials via early tool calls for later exfiltration | Credentials are injected per-call, not per-session. A `bash` call that reads a GitHub token gets only that token for that call; it cannot access LLM API keys it was not explicitly given. |
| Model reads pi's own in-memory API keys via filesystem | Pi's environment is not accessible via any tool call (the tool subprocess is a separate process with a different environment). |
| Model establishes persistent sidecar via `bash` | Bash subprocess lives only for the duration of the tool call. The daemon waits for subprocess exit before returning `tool_exec_result`. Detached processes (background `&`) are killed when the subprocess group is reaped. |
| Model registers an unauthorised tool via a hostile extension | Defended by the tool surface check at session start (§3.5): any tool from an extension not on the iris allowlist aborts the session before any LLM turn runs. |
| Model exploits an unreviewed new built-in tool | Defended by the tool surface check at session start (§3.5): any unknown built-in (not in the canonical seven) aborts the session. |

**Network access is permitted.** The per-tool sandbox does not restrict
outbound network for tool subprocesses. The sandbox boundary is filesystem
isolation (what data is reachable), not network isolation. See §5 for
rationale.

**Attack classes that become WEAKER under the new model compared to the
current model:**

| Attack class | Why it is weaker | Mitigation |
|---|---|---|
| **Pi itself is a direct attack surface on the host** | Pi runs unsandboxed. A compromised pi binary can read `~/.ssh`, `~/.aws`, `~/.claude`, etc. directly without any tool call — it has the full host environment. In the current model, pi is sandboxed and sees only the fake home. | Pi is a first-party binary (or a pinned version of a trusted dependency). The daemon enforces `tool_exec_result` boundaries even when pi is compromised in a behavioural (not binary) sense — i.e., prompt injection that controls pi's *actions* is still sandboxed at the tool level. Binary compromise of pi is a supply-chain risk mitigated by version pinning and checksum verification. |
| **Pi reads LLM credentials from `~/.pi/agent/` with no sandbox boundary** | In the current model, `~/.pi/agent/` is bind-mounted into the sandbox (contained). In the new model, pi runs on the host and reads `~/.pi/agent/` directly — no different from today's bind-mount in terms of what pi can access, but there is no sandbox boundary around pi itself. A compromised pi binary has the same access. | Pi is a first-party trusted binary. The attack surface is model-generated tool calls, not pi's own credential reads. This is the same trade-off named in §6.3. |
| **Outbound network from pi is unrestricted** | Pi runs on the host with full network access. In the current model, the bwrap/sandbox-exec network restriction also limits pi's own outbound calls. | Pi's own network calls are LLM API calls to known providers. Restricting pi's outbound network is at odds with pi functioning at all. This is an accepted trade-off in the daemon model. |
| **Model can influence pi's own file I/O** | If a future version of pi does direct filesystem I/O outside of tool calls (e.g. logging, caching, telemetry), that I/O is unrestricted on the host. The current model's fake-home limits this to the mounted artefacts. | See §9.2 for how this case is handled architecturally. |

**Summary comparison:**

| Dimension | Current (per-process) | New (per-tool) |
|---|---|---|
| Pi binary sandbox | ✅ Pi is sandboxed | ❌ Pi runs on host |
| Tool call isolation | ❌ All credentials visible for whole session | ✅ Per-call credential injection |
| Credential accumulation risk | ❌ High (all credentials mounted at session start) | ✅ Lower (credentials scoped to calls that need them) |
| Worktree path traversal | ✅ Enforced by mount boundary | ✅ Enforced by path normalisation + subprocess bind |
| Pi supply-chain risk | ✅ Lower (pi is contained) | ❌ Higher (pi is on host) |
| LLM API key exposure | ⚠️ Available in pi's env (in fake home) | ⚠️ Available in pi's env (on host) |
| Fake-home credential bugs | ❌ Chronic source of bugs | ✅ Eliminated |
| Unauthorised tool registration | ❌ No check | ✅ Tool surface check at session start |

The new model is a net improvement for the primary threat (model-generated
tool calls that abuse the agent's execution context) at the cost of
weakened containment of pi itself. This trade-off is appropriate when pi
is a trusted binary and the attack surface we care about is adversarial
model outputs, not compromised pi code.

---

## 7. Per-tool credential brokering

### 7.1 Current architecture (4-PAT architecture)

Today, pi runs inside a bwrap (Linux) or sandbox-exec (macOS) sandbox. The
sandbox uses `--clearenv` to wipe the host environment and then explicitly
re-introduces only what the agent needs via `--setenv` pairs.

The role-scoped GitHub token is selected from the 4-PAT architecture:

```
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR   — prismatic-koi + coordinator
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER        — prismatic-koi + worker
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR — thankyou-payroll + coordinator
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER      — thankyou-payroll + worker
```

based on the repo's GitHub account (derived from the git remote URL) and
the session role, and injected as `GITHUB_TOKEN` into the sandbox.

In the iris model, `ANTHROPIC_API_KEY` and `OPENROUTER_API_KEY` are read by
pi directly from the host environment at startup. They are **not** propagated
into any tool subprocess — bash subprocesses must not make LLM calls, and the
file tools have no need for them. Pi also reads credentials from its own
config directory (`~/.pi/agent/`), accessible directly from the host without
any bind-mount.

### 7.2 Per-tool credential plan in daemon mode

The daemon maintains a **credential store** populated from the host
environment at daemon startup (or refreshed on demand). Each tool call
receives only the credentials it needs:

| Tool | Credentials injected at call time |
|---|---|
| `read`, `edit`, `write`, `grep`, `find`, `ls` | **None.** These tools do not need credentials. |
| `bash` | Role-scoped `GITHUB_TOKEN` (from 4-PAT architecture). AWS credentials (from `~/.aws/{config,credentials,sso,cli}`) injected conditionally when the session role requires them. `ANTHROPIC_API_KEY` and `OPENROUTER_API_KEY` are **not** injected — bash subprocesses must not make LLM calls. |

### 7.3 Pi's own credentials

In the current architecture, pi reads LLM credentials from its own config
directory (`~/.pi/agent/`) — either subscription tokens stored there by
`/login`, or API keys written into pi's settings. The sandbox bind-mounts
`~/.pi/agent/` so pi can read its own credentials directly.

In daemon mode, pi runs unsandboxed and has direct access to `~/.pi/agent/`
without any bind-mount. Pi reads its own credentials (`~/.pi/agent/auth.json`,
`~/.pi/agent/settings.json`, `~/.pi/agent/atlassian-mcp-oauth.json`) directly
from the host. The provider and model are configured via `--provider` and
`--model` CLI flags at spawn time. No separate credential injection is
required for the basic daemon-mode design.

This is the §1.3 "fake-home pain" elimination: pi's credentials are pi's
business, and the daemon does not mediate them. The `PI_CODING_AGENT_DIR`
override remains available for advanced configuration.

### 7.4 Credential scoping summary

The key improvement over the current model:

- **GitHub token** is injected into bash subprocesses on demand, not into
  pi's process environment for the entire session. A model that uses `bash`
  to exfiltrate the GitHub token cannot access it in any other tool call context.
- **LLM API keys** (`ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`) remain in pi's
  process environment (necessary for pi to make provider calls). They are not
  propagated into any tool subprocess. This is the primary remaining credential
  exposure in the new model and is called out explicitly in the threat model
  (§6.3).
- **Atlassian MCP OAuth tokens** (`~/.pi/agent/atlassian-mcp-oauth.json`)
  are part of pi's on-disk credential surface. In daemon mode, pi runs
  unsandboxed and reads this file directly without a bind-mount.

---

## 8. Persistence model

### 8.1 What state is persisted

The prism SQLite DB (`~/.local/state/prism/prism.db` today; the codename
variant uses a distinct path — see §10) is the source of truth for all
session state. This does not change in daemon mode. The DB already stores:

- `agent_status`: current state of each session (active, idle, finished, etc.)
- `sessions`: immutable per-incarnation records (instance_id, start time,
  end time, archive path, worktree, harness_session_id, etc.)
- `agent_events`: full event log (tool calls, messages, state changes)
- `bus_messages`: cross-session prompt delivery queue
- `pending_merges`: merge queue state
- `session_groups`: review group membership

The daemon adds no new persistent state beyond what is already in the DB.
The daemon's in-memory state (subscriber fan-out sets, per-session pipe
goroutines) is reconstructed on startup from the DB.

### 8.2 What happens across daemon restart

When the daemon restarts (crash, upgrade, user `systemctl restart`):

1. **In-flight pi children are orphaned.** The daemon held the stdin/stdout
   pipes; when the daemon process exits, the pipes break. Pi receives EOF
   on stdin and exits (or receives SIGHUP if the daemon was the process
   group leader).
2. **DB state is consistent.** The daemon writes state transitions to the
   DB synchronously before returning. An in-flight write at crash time may
   leave the DB in the last committed state; the WAL journal handles
   recovery.
3. **On daemon startup, `prism restore` logic runs.** Sessions in `active`
   or `spawning` state in the DB are treated as candidates for restart.
   The daemon attempts to re-spawn each with the same worktree and role.
   This mirrors the current `prism restore` command (`cmd/restore.go`).
   The new pi child starts a fresh session, but picks up conversation
   history from the DB via pi's session-continuation mechanism:
   `pi --mode rpc --session <full-jsonl-path>` (confirmed by D-1 §Q5).
4. **The daemon must store the full JSONL file path** in the DB at session
   creation time — not just the session UUID. The actual pi session path
   is `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl`
   (D-1 §Q5 correction; see also §9.2). The `harness_session_id` column
   in the `sessions` table must store this full path to enable restart
   continuation.
5. **Clients that were connected** receive a connection reset. Reconnecting
   clients receive a `sessions_snapshot` with the current state.

### 8.3 What happens across machine reboot

On reboot, the daemon does not restart automatically unless configured as
a systemd user service (or launchd agent on macOS). When the daemon starts
after a reboot:

- All pi children are gone (the reboot killed them).
- The DB is consistent (SQLite survives reboots by design).
- Sessions in `active` state are stale. The daemon applies the same
  restore logic as §8.2.

A systemd user service definition (or launchd plist) that starts the
daemon on login is part of the NixOS/home-manager configuration, not the
Go codebase. This is a separate deliverable (§12, child issue D-6).

### 8.4 tmux's remaining role

In daemon mode, tmux's only remaining optional role is hosting a human-readable
shell fallback pane — a `bash` pane the user can type in manually. This is
not required for daemon operation and can be omitted in headless deployments.
The daemon does not depend on tmux at all; the TUI is a bubbletea program
that connects to the daemon socket.

---

## 9. Edge cases

### 9.1 In-flight tool call when the daemon crashes

If the daemon crashes while a tool subprocess is running:

- The tool subprocess continues running (it is a child of the daemon
  process, so it receives SIGHUP when the daemon exits; whether it exits
  depends on whether it ignores SIGHUP).
- The pi child, whose stdin/stdout pipes to the daemon broke, exits (EOF on
  stdin causes pi to shut down cleanly, per pi's RPC convention).
- On daemon restart and session restore (§8.2), a new pi child is spawned.
  The tool result from the orphaned subprocess is lost — it will never be
  delivered to the new pi child, which has no memory of the original tool
  call.
- The DB records the `tool_call` event (written when the `tool_exec` frame
  arrived on the harness socket) but no corresponding `tool_result`. This
  is a detectable orphan state. The daemon's restore path should write a
  synthetic `tool_result` with `success=false` and
  `output="daemon restarted mid-call"` for any `tool_call` event without a
  matching `tool_result` in the last session incarnation.

### 9.2 Pi writes to the host filesystem outside of tool calls

In the new model, pi runs unsandboxed on the host. Pi may — intentionally
or via a future feature addition — perform direct filesystem I/O outside of
the tool call mechanism.

**Confirmed pi write paths (D-1 §Q6):**
- `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl` — the
  actual session JSONL path (D-1 correction; not `~/.pi/agent/sessions/<session_id>/`
  as the original design doc claimed). The `<encoded-cwd>` is
  `--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--`. Pi writes
  every message, tool call, and turn to this append-only file.
- `~/.pi/agent/auth.json`, `~/.pi/agent/settings.json`,
  `~/.pi/agent/atlassian-mcp-oauth.json` — pi's own config and credential
  files. Written by pi on login, settings changes, and OAuth token refresh.
- `/tmp/pi-bash-<id>.log` — full bash command output when output exceeds
  pi's in-memory truncation threshold.

**Architectural position:**

Direct pi I/O outside of tool calls is **accepted with rationale** in the
daemon mode design. The rationale is:

1. Pi is a first-party trusted binary. Its own file I/O is within its
   sanctioned scope.
2. The threat model targets model-generated tool calls, not pi's own
   internal bookkeeping.
3. Attempting to sandbox pi's own I/O would re-introduce the fake-home
   problems this design is specifically trying to eliminate.

**Audit mechanism:** The daemon logs all `tool_call` and `tool_result` frames
to the DB. Any file writes that do not correspond to a logged `tool_call`
event are attributable to pi itself. This provides an audit trail even
without enforcement.

---

## 10. Coexistence strategy

### 10.1 Codename

The new system ships under the codename **`iris`** (confirmed in the D-2
tracker, §11.7 resolved). All runtime artefacts use distinct paths to avoid
colliding with the existing prism system:

| Artefact | Current prism path | Codename (`iris`) path |
|---|---|---|
| Binary | `prism` | `iris` |
| DB | `~/.local/state/prism/prism.db` | `~/.local/state/iris/iris.db` |
| Daemon socket | (does not exist today) | `~/.local/state/iris/iris.sock` |
| Run directory | `~/.local/state/prism/run/` | `~/.local/state/iris/run/` |
| Per-session tmpdir | (N/A — whole-pi sandbox) | `~/.local/state/iris/run/<instance_id>/tmp/` |
| Config file | `~/.config/prism/config.json` | `~/.config/iris/config.json` |
| Log files | `~/.local/state/prism/logs/` | `~/.local/state/iris/logs/` |
| Archive root | `~/code/archives/prism/` | `~/code/archives/iris/` |

The per-session tmpdir (`~/.local/state/iris/run/<instance_id>/tmp/`) is the
host-side backing store for the in-sandbox `/tmp` mount. Each tool subprocess
sees a fresh `/tmp` backed by this directory; the directory is created at
session start and deleted at session teardown.

### 10.2 Parallel operation

Both systems can run simultaneously on the same machine:

- `prism` spawns sessions into tmux, uses bwrap/sandbox-exec wrapping pi.
- `iris` spawns sessions via the daemon, uses per-tool sandboxing.

A user can run `prism spawn` and `iris spawn` on the same repo from the
same terminal without conflict. The DB paths, socket paths, and tmux
session names (iris uses a distinct prefix) do not collide.

### 10.3 Feature parity definition

The codename system reaches **parity** when it can perform all of the
following without invoking the legacy `prism` system:

- [ ] Spawn worker and coordinator sessions.
- [ ] Deliver prompts to running sessions.
- [ ] Show the dashboard (session list with state, branch, timing).
- [ ] `checkin` (read conversation history from a session).
- [ ] Run review (`prism review` equivalent — spawns 5 review agents).
- [ ] Merge queue (coordinator-side `pending_merges` watcher).
- [ ] Archive sessions.
- [ ] Restore sessions after reboot.
- [ ] `cleanup` (kill a session and its artefacts).

### 10.4 Migration and rename

Once parity is reached:

1. A migration PR renames the `iris` binary to `prism`, updates all config
   paths, and removes the legacy `prism` codebase.
2. Existing prism DB data is migrated (the schema is shared by design —
   the codename system uses the same DB schema with a different file path,
   so migration is a copy + path repoint, not a schema translation).
3. The legacy tmux-based session infrastructure (`internal/session/session.go`,
   `internal/container/`, `internal/sidecar/`) is deleted.

The rename is a single atomic PR. The codename exists only to allow parallel
operation during development; it is not a long-lived fork.

D-11 is user-gated: the parity checklist (D-10) is necessary but not
sufficient. D-11 sits as a deliberate manual decision after iris has been
battle-tested in real coordinator use. The coordinator (`@prismatic-koi`) is
the sole arbiter of when "battle-tested" is satisfied.

---

## 11. Open questions

The following decisions are explicitly deferred to the PoC or later design
phases. They are listed here rather than resolved unilaterally because they
require empirical data, external input, or design review that is beyond the
scope of this initial design doc.

**Environmental snapshot note.** The tool inventory in §5 and the credential
descriptions in §7 have been updated to reflect the three cleanups that landed
after the initial draft of this document:

- **Opencode removal** — #1609 removed the opencode runtime and harness
  wiring; #1617 removed the opencode archive/export plumbing. Pi has been
  the exclusive agent harness since those PRs merged.
- **Atlassian-CLI retirement** — #1602/#1604 removed the standalone `atlassian`
  CLI in favour of the pi atlassian-mcp MCP extension; #1606 removed the
  associated SOPS secrets.
- **Speculative env-var audit** — #1605 pruned five speculative keys
  (`OPENAI_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`,
  `GITHUB_COPILOT_TOKEN`, `DEEPSEEK_API_KEY`) from `forwardKeys` in
  `internal/container/credentials.go`.

**11.1 Pi `--rpc` mode: does it exist, and what is the exact interface?**
**Resolved by `pi-rpc-interface.md` (D-1, PR #1628).** The flag is `--mode rpc`.
Pi does not support daemon-delegated tool execution via the RPC channel;
the override mechanism via `pi.registerTool()` replaces the original
RPC-delegation premise. §3.2 and §3.3 are updated accordingly.

**11.2 Restart policy parameters: N and backoff schedule.**
The circuit-breaker threshold `N` and the backoff schedule for session
restart are configurable. What are the right defaults for the daemon model?
The current prism value is 3 consecutive failures. The daemon model may
warrant different values because the restart cost is lower (no tmux session
creation).

**11.3 Web UI transport: daemon socket proxy vs native HTTP.**
Should the daemon expose a WebSocket/HTTP endpoint natively, or should the
web UI connect through a thin proxy that bridges WebSocket to the Unix
socket? Native HTTP adds complexity to the daemon; a proxy is an extra
process. Decision deferred to the web UI design phase.

**11.4 (reserved)** — Previously referred to MCP proxy lifecycle for Atlassian
MCP as an opencode-specific integration. Atlassian MCP is now a pi extension
(#1587, gated on `m4mac` via #1588). The proxy lifecycle question for
daemon-mode MCP-bridge tools is deferred to the child issue that implements
MCP-bridge sandbox support. This question number is reserved to avoid
renumbering downstream references.

**11.5 `register_provider` RPC for dynamic provider configuration.**
**Resolved by `pi-rpc-interface.md` (D-1, PR #1628).** `register_provider`
exists as a harness socket frame handled by the prism extension (§6.4 of
`pi-wire-protocol.md`), which proxies it to `pi.registerProvider()` at
runtime. It is not a pi RPC stdin command. The daemon sends
`register_provider` on the harness socket; the existing architecture handles
this without any new pi feature.

**11.6 Session continuation after daemon restart.**
**Resolved by `pi-rpc-interface.md` (D-1, PR #1628).** Pi supports session
continuation via `pi --mode rpc --session <full-jsonl-path>`. The daemon
must store the full JSONL file path (not just the session UUID) in the DB at
session creation time. See §8.2.

**11.7 Codename selection.**
**Resolved: codename is `iris`** (confirmed in the D-2 tracker, #1625).
All paths, binary names, and artefacts in this document use `iris`. This
design doc refresh (PR #1630) is the resolving artifact.

**11.8 Daemon process privilege and multi-user.**
This design assumes a single-user daemon (one daemon instance per user,
owned by that user). Multi-user support (a system-level daemon serving
multiple users) is explicitly out of scope, but the socket path and DB
path choices should not preclude it if the requirement arises later.

**11.9 Iris extension allowlist: update policy.**
The initial iris extension allowlist is `prism`, `atlassian`, `anthropic-oauth`
(§3.5). Adding a new extension requires updating the allowlist. The update
is a config change, not a code change — the allowlist is read from the iris
config file at daemon startup. The policy for *who* may authorise an addition
(daemon owner vs. code review vs. automatic) is deferred to the D-2 config
design.

---

## 12. Child-issue breakdown

Each child issue is independently shippable. Dependencies are listed; an
issue may not be started until all its dependencies are merged.

| Issue | Title | Depends on | Closure policy |
|---|---|---|---|
| **D-0** | **This design doc** (issue #1599) | — | Merged once coordinator review passes. |
| **D-1** | Pi `--mode rpc` audit and interface contract | D-0 | Closed. `pi-rpc-interface.md` delivered (PR #1628). The audit found tool delegation via RPC is not supported; §2/§3.3 updated to the `pi.registerTool()` override mechanism. |
| **D-2** | Codename selection and directory skeleton | D-0 | Closed when the codename is chosen and the `iris/` package skeleton (binary entrypoint, config loading, DB open) builds and passes `go test ./...`. |
| **D-3** | Daemon core: spawn, supervise, harness-socket tool dispatch loop | D-1, D-2 | Closed when the daemon can spawn a pi `--mode rpc` child with the prism extension loaded, receive `tool_exec` frames on the harness socket, execute the tool, and return `tool_exec_result`. No sandbox, no client IPC socket — bare dispatch loop only. |
| **D-4** | Per-tool sandbox: worktree-scoped subprocesses for read/edit/write/grep/find/ls | D-3 | Closed when these six tools execute in a worktree-scoped subprocess with path traversal enforcement, and a test demonstrates that a path traversal attempt is rejected. |
| **D-5** | Per-tool sandbox: bash restricted subprocess (bwrap on Linux, sandbox-exec on macOS) | D-4 | Closed when bash tool calls execute in a restricted subprocess with the mount set from §5, and the sandbox-exec testing convention (see `docs/sandbox-exec-testing.md`) is followed on Darwin. |
| **D-6** | Daemon IPC socket: client connect/subscribe/fan-out | D-3 | Closed when a CLI client can connect to the daemon socket, subscribe to a session, and receive live `session_event` frames. |
| **D-7** | Per-tool credential brokering: bash subprocess credential injection | D-5 | Closed when the bash tool subprocess receives only the GitHub token (role-scoped) and AWS credentials (when applicable), and a test confirms LLM API keys are absent from the bash subprocess environment. |
| **D-8** | Bubbletea TUI: session list + subscribe + prompt deliver | D-6 | Closed when a TUI connects to the daemon, shows a session list, allows selection, and shows live output without tmux. |
| **D-9** | Restore after daemon restart: orphan detection + re-spawn | D-3 | Closed when a daemon restart re-spawns sessions that were `active` in the DB, and orphaned `tool_call` events without `tool_result` receive synthetic failure results. |
| **D-10** | Parity gate: feature checklist from §10.3 | D-3 through D-9 | Closed when all items in the §10.3 parity checklist pass end-to-end tests. Signals readiness for rename. |
| **D-11** | Migration and rename: `iris` → `prism`, remove legacy code | D-10 **AND** explicit user authorisation | Closed when the binary is named `prism`, paths are updated, and the legacy `internal/container/`, `internal/sidecar/`, `internal/session/session.go` codepaths are deleted. |

Issues D-3 through D-9 can be worked in parallel after their immediate
dependencies land. D-10 and D-11 are strictly sequential — D-10 gates D-11.
D-11 is additionally user-gated (§10.4).
