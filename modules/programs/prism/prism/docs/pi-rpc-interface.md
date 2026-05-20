# Pi `--rpc` mode audit and interface contract

**Status:** D-1 deliverable (issue #1626). Prescriptive ground truth for D-3 and
downstream daemon issues. All claims are grounded in the binary, source code, or
the existing wire-protocol doc; every citation is reproducible.

**Pi version audited:** `0.72.1` (Nix store path:
`/nix/store/vmyapz1s657gbn3vn8iyifj6vlg5i9sm-pi-coding-agent-0.72.1/`)

**Audit date:** 2026-05-15

---

## Executive summary

**Verdict: viable with named upstream pi changes.**

Pi 0.72.1 ships a working `--mode rpc` flag that reads JSON-line commands from
stdin and writes JSON-line events and responses to stdout. Session continuation
via `--session <path>` is confirmed. Provider registration via an extension hook
(`pi.registerProvider()`) exists and is implemented in the current prism
extension. The credential store path `~/.pi/agent/` is confirmed.

The **one design assumption that does not hold** is the central premise of
§3.3 and the daemon's tool-dispatch model: pi does **not** emit a `tool_call`
frame and wait for a `tool_result` frame from the daemon before proceeding.
Pi's RPC protocol is an **observation + steering** protocol, not a
**delegation** protocol. The daemon cannot intercept tool execution and
substitute its own sandbox. Pi executes every tool call internally, in its own
process, without waiting for an external `tool_result` response. Extensions
can only *observe* tool calls (via the `tool_call` hook) or *block* them
(returning `{ block: true, reason: "..." }`) — they cannot redirect execution
to an external sandbox and return a result.

This is a material deviation from the design doc's §3.3 frame table and the
architectural premise of §2 ("the daemon must own tool dispatch"). D-3 and all
downstream sandbox issues (D-4, D-5) cannot proceed as designed until either:

1. Pi gains a `--delegate-tools` mode where `tool_call` frames are emitted
   on stdout and the process *waits* for a `tool_result` frame on stdin before
   executing the tool; or
2. The daemon design is revised to use a different sandboxing strategy that
   does not require intercepting pi's tool calls at the RPC layer.

D-2 (directory skeleton) can proceed independently. D-3 and beyond are blocked
pending a decision on which path to take.

---

## Q1 — Does pi have an `--rpc` mode?

**Verdict: yes.**

### Evidence

**CLI probe** (`pi --help`, binary version 0.72.1):

```
--mode <mode>    Output mode: text (default), json, or rpc
```

The flag is `--mode rpc`, not `--rpc`. The design doc's §3.2 shows the spawn
command as `pi --rpc [--worktree <path>]`; the correct flag is
`--mode rpc` (no `--rpc` shorthand exists).

**Functional verification:**

```bash
$ echo '{"type":"get_state"}' | timeout 5 pi --mode rpc --no-session
{"type":"response","command":"get_state","success":true,"data":{"model":{...},"isStreaming":false,...}}
```

**Source confirmation** (`dist/cli/args.js` line 27):

```javascript
if (mode === "text" || mode === "json" || mode === "rpc") {
    result.mode = mode;
}
```

**Mode implementation** (`dist/modes/rpc/rpc-mode.js`): `runRpcMode()` takes
over stdout, reads JSONL from stdin, dispatches commands through a
`handleCommand()` switch, and streams agent events to stdout. The process stays
alive indefinitely (`return new Promise(() => {})`).

**When RPC mode was introduced:** Not determinable from the installed binary
(no changelog embedded). The mode is fully implemented and documented in 0.72.1
and is the current production version pinned by this repo's
`overlays/default.nix` line 40.

**Worktree argument:** The design doc references `--worktree <path>` as a flag
that scopes the session to a specific directory. This flag does **not exist** in
pi 0.72.1. Pi uses the process working directory (`cwd`) as its worktree — the
daemon must `chdir()` to the correct worktree before spawning the pi child, or
pass an initial prompt that references the worktree. There is no `--worktree`
flag.

### Implications for design doc

- **§3.2**: Replace `pi --rpc [--worktree <path>] [--agent <role>]` with
  `pi --mode rpc [--session <path>] [--session-dir <dir>]`. Remove the
  `--worktree` flag reference. The worktree is set via `cwd` of the spawned
  process, not a CLI flag.
- **§11.1** (open question about `--rpc` flag): resolved. The flag is
  `--mode rpc`.

---

## Q2 — Does pi support delegating tool execution back to the caller?

**Verdict: no.**

This is the critical finding. The design doc's §3.3 states:

> Pi emits a `tool_call` frame and **waits** for a `tool_result` frame from
> the daemon before proceeding.

This does not match pi's actual implementation.

### Evidence

**RpcCommand type** (`dist/modes/rpc/rpc-types.d.ts`): The `RpcCommand` union
type lists all valid commands that can be sent *to* pi on stdin. There is no
`tool_result` command type. The complete list is: `prompt`, `steer`,
`follow_up`, `abort`, `new_session`, `get_state`, `set_model`, `cycle_model`,
`get_available_models`, `set_thinking_level`, `cycle_thinking_level`,
`set_steering_mode`, `set_follow_up_mode`, `compact`, `set_auto_compaction`,
`set_auto_retry`, `abort_retry`, `bash`, `abort_bash`, `get_session_stats`,
`export_html`, `switch_session`, `fork`, `clone`, `get_fork_messages`,
`get_last_assistant_text`, `set_session_name`, `get_messages`, `get_commands`.

**Tool execution is internal to pi.** `dist/core/agent-session.js` lines
170–215 install `agent.beforeToolCall` and `agent.afterToolCall` hooks. These
hooks can *observe* and *block* tool calls but cannot *redirect* them to an
external executor:

```javascript
// beforeToolCall — called before pi executes a tool
this.agent.beforeToolCall = async ({ toolCall, args }) => {
    // Calls extension runner's tool_call hook
    return await runner.emitToolCall({ type: "tool_call", ... });
};
```

**`ToolCallEventResult` type** (`dist/core/extensions/types.d.ts` line 714):

```typescript
export interface ToolCallEventResult {
    /** Block tool execution. To modify arguments, mutate `event.input` in place instead. */
    block?: boolean;
    reason?: string;
}
```

The only effect an extension can have on a tool call is:
- `block: true` → pi returns an error result to the LLM (`"Tool execution was
  blocked"`), as confirmed by `@mariozechner/pi-agent-core/dist/agent-loop.js`
  lines 349–353.
- `undefined` → pi executes the tool normally in its own process.

An extension **cannot** return a substitute tool result (e.g. the output of a
sandboxed subprocess). The `tool_result` frame in `docs/pi-wire-protocol.md`
is an *outbound* frame emitted by the prism extension to the sidecar over the
harness socket — it is *not* an RPC command the daemon can send to pi over
stdin.

**Tool events on stdout** (from `dist/core/agent-session.js` lines 439–470 and
`docs/rpc.md` §Events): pi emits `tool_execution_start`, `tool_execution_update`,
and `tool_execution_end` events on stdout. These are **observation events** only;
they do not pause tool execution waiting for a response.

**What is required to make delegation possible:**

Pi would need a new operating mode — call it `--delegate-tools` — in which:

1. When the LLM requests a tool call, pi emits a `tool_call` frame on stdout
   (as an RPC event) and then *suspends* that tool call, waiting for a
   corresponding `tool_result` frame to arrive on stdin before proceeding.
2. The `tool_result` frame on stdin becomes a new `RpcCommand` type carrying
   `{ type: "tool_result", id: "<tool_call_id>", success: bool, output: string }`.
3. Pi resumes the conversation with the provided result as if it had executed
   the tool itself.

This is a substantial upstream pi change. It does not exist in 0.72.1.

### Implications for design doc

- **§2** ("the daemon must own tool dispatch"): The foundational premise of the
  two-component design is not supported by pi's current API. The daemon cannot
  own tool dispatch without an upstream pi change.
- **§3.3** (frame table): The `tool_call` / `tool_result` row in the
  daemon→pi table does not exist. The `tool_result` frame currently exists only
  in the prism extension's outbound direction (extension→sidecar, per the
  existing `pi-wire-protocol.md`), not as an RPC command pi accepts on stdin.
- **§2, §3.3, §4.1, §5, §6.2, §6.3, §7.2** all assume the daemon intercepts
  tool calls. None of this is implementable without the upstream change
  described above.
- **§11.1** should be amended: the open question is not "does `--rpc` exist"
  but "does pi support tool delegation via a `tool_result` stdin command" — and
  the answer is no.

---

## Q3 — What is the exact frame catalogue?

**Verdict: partial.** The frames that exist as wire-protocol frames (used by
the prism extension over the harness socket) are confirmed. The frames assumed
to exist as RPC commands (sent by the daemon to pi on stdin) are refuted for
the delegation-specific ones.

### Architecture clarification

There are **two separate protocols** that the design doc conflates:

1. **Pi RPC protocol** (stdin/stdout of the `pi --mode rpc` process): commands
   and events between the daemon and pi. Documented in `docs/rpc.md` (bundled
   with the pi binary).

2. **Prism harness wire protocol** (Unix socket / TCP between the prism
   sidecar and the prism TypeScript extension running inside pi): events and
   control frames between the sidecar and the extension. Documented in
   `docs/pi-wire-protocol.md` in this repo.

The design doc's §3.3 tables appear to describe a merged view of both. The
frame-by-frame check below clarifies which protocol each frame belongs to, and
whether it exists.

### Frames assumed in §3.3: pi → daemon direction

| Frame type (design doc) | Status | Evidence |
|---|---|---|
| `hello` | **refuted** (as RPC event) | Not in `docs/rpc.md`. In the harness wire protocol (§4.1 of `pi-wire-protocol.md`), `hello` is the first frame the prism *extension* sends to the *sidecar* over the socket. There is no `hello` event emitted by pi on stdout in `--mode rpc`. |
| `state_change` | **partial** | Not a direct RPC event. Pi emits `agent_start`, `agent_end`, `turn_start`, `turn_end` as lifecycle events. `state_change` is a wire-protocol frame emitted by the prism extension to the sidecar (§5.2 of `pi-wire-protocol.md`), translated from pi's hook events. In RPC mode, the equivalent events are `agent_start` / `agent_end`. |
| `tool_call` | **confirmed** (as extension wire frame) | As a daemon→pi RPC command: does not exist. As an extension→sidecar wire frame (§5.3 of `pi-wire-protocol.md`): implemented in the prism extension, derived from `tool_execution_start` (`prism.ts:2343`). The extension emits `tool_call` to the sidecar; the sidecar records it in the DB. Pi never waits for a `tool_result` response on stdin. Verified for pi 0.72.1 in issue #1764: the extension API event `tool_execution_start` carries `{toolCallId, toolName, args}` and fires once per tool invocation; the extension translates it to a `msg_assistant`-style observation frame. The shape is pinned by `dist/core/extensions/types.d.ts:519` in pi 0.72.1. |
| `msg_assistant` | **confirmed** (as extension wire frame) | The prism extension forwards assistant text to the sidecar via two complementary code paths (issue #1764, pi 0.72.1): (a) **streaming** — `message_update` events with `assistantMessageEvent.type === "text_delta"` are forwarded one frame per delta (`prism.ts:isAssistantTextDeltaEvent`); (b) **backstop** — a `message_end` event with `message.role === "assistant"` is consulted when no `text_delta` was forwarded for that message, and the extension emits one `msg_assistant` frame per `{type:"text", text}` block in `message.content` (`prism.ts:extractAssistantText`). The per-message `currentAssistantSawDelta` flag prevents double-emission when both paths fire. The streaming-only model documented in the pre-#1764 version of this row was incomplete: non-streaming providers (or any provider that goes `start → done` without intermediate text_delta) bypass path (a) entirely. The AssistantMessageEvent union (`start`, `text_start`, `text_delta`, `text_end`, `thinking_start`, `thinking_delta`, `thinking_end`, `toolcall_start`, `toolcall_delta`, `toolcall_end`, `done`, `error`) is pinned by `node_modules/@mariozechner/pi-ai/dist/types.d.ts:185-225` and asserted by the regression suite in `prism.test.ts`. |
| `turn_start` | **confirmed** | Emitted by pi in RPC mode (`docs/rpc.md` §turn_start / turn_end). |
| `turn_end` | **confirmed** | Emitted by pi in RPC mode; carries `message` and `toolResults`. The `usage` field is nested in the message. |
| `session_shutdown` | **refuted** (as RPC event) | Not emitted by pi as an RPC stdout event. Pi exits when stdin closes (EOF). The prism extension emits `session_shutdown` to the sidecar on pi's `session_shutdown` hook (§5.10 of `pi-wire-protocol.md`), but this is the harness socket protocol, not the RPC stdout. |
| `provider_error` | **partial** | In RPC mode, provider errors appear via `auto_retry_start` / `auto_retry_end` events (`docs/rpc.md` §auto_retry_start). The prism extension also emits `provider_error` to the sidecar (§5.8 of `pi-wire-protocol.md`). No direct `provider_error` event on the RPC stdout. |
| `auto_retry_start` | **confirmed** | Emitted by pi in RPC mode (`docs/rpc.md` §auto_retry_start). Fields: `attempt`, `maxAttempts`, `delayMs`, `errorMessage` (camelCase, not the snake_case from `pi-wire-protocol.md`). |
| `auto_retry_end` | **confirmed** | Emitted by pi in RPC mode (`docs/rpc.md` §auto_retry_end). |

### Frames assumed in §3.3: daemon → pi direction

| Frame type (design doc) | Status | Evidence |
|---|---|---|
| `hello_ack` | **refuted** (as RPC command) | No `hello_ack` RPC command type. In the harness wire protocol, `hello_ack` is sent by the sidecar to the extension over the socket (§4.2 of `pi-wire-protocol.md`). No handshake exists in the RPC stdin/stdout protocol. |
| `tool_result` | **refuted** (as RPC command) / **confirmed** (as extension wire frame) | Not in `RpcCommand` union type (`dist/modes/rpc/rpc-types.d.ts`). See Q2. As an extension→sidecar wire frame (§5.4 of `pi-wire-protocol.md`): emitted by the prism extension on `tool_execution_end` (`prism.ts:2416`), with `{id, success, output, truncated?}` derived from the event's `result.content` (text blocks concatenated via `coerceToolOutput`). Verified for pi 0.72.1 in issue #1764: the extension API event `tool_execution_end` carries `{toolCallId, toolName, result, isError}` and fires once per tool completion. Shape pinned by `dist/core/extensions/types.d.ts:533` in pi 0.72.1. |
| `prompt` | **confirmed** | `{"type":"prompt","message":"...","streamingBehavior":"steer"|"followUp"}` — fully implemented (`docs/rpc.md` §prompt). |
| `set_model` | **confirmed** | `{"type":"set_model","provider":"anthropic","modelId":"claude-sonnet-4-20250514"}` — functional, verified by probe. **Field name is `modelId` not `model`** (design doc §3.3 shows `model`; the actual RPC command uses `modelId`). |
| `register_provider` | **partial** (see Q4) | Exists as a harness wire-protocol frame handled by the prism extension, not as an RPC stdin command. |
| `set_active_tools` | **partial** | As an RPC stdin command: does not exist in `RpcCommand` types. As a harness wire-protocol frame: exists and is implemented in the prism extension (lines 1145–1160 of `prism.ts`), which calls `api.setActiveTools(tools)`. Not a direct pi RPC command. |
| `abort` | **confirmed** | `{"type":"abort"}` — exists as an RPC command (`docs/rpc.md` §abort). |

### Additional RPC events not in §3.3

Pi emits several events in `--mode rpc` that are not listed in the design doc:

| RPC event | Meaning |
|---|---|
| `agent_start` | Agent begins processing a prompt |
| `agent_end` | Agent finishes; contains all messages for the run |
| `message_start` | Assistant message begins |
| `message_update` | Streaming text/thinking/tool-call delta |
| `message_end` | Assistant message complete |
| `tool_execution_start` | Tool begins execution (with `toolCallId`, `toolName`, `args`) |
| `tool_execution_update` | Streaming tool output (partial result) |
| `tool_execution_end` | Tool completes (with result and `isError`) |
| `queue_update` | Pending steer/follow-up queue changed |
| `compaction_start` / `compaction_end` | Context compaction lifecycle |
| `extension_error` | Extension threw an error |
| `extension_ui_request` | Extension requested user interaction (select/confirm/input/editor/notify/...) |
| `response` | Acknowledgement to any command (carries `success`, `command`, optional `data` or `error`) |

---

## Q4 — Does `register_provider` exist as a runtime frame, or only as static config?

**Verdict: yes, as a runtime harness wire-protocol frame; not as a pi RPC
stdin command.**

### Evidence

`register_provider` exists in **two distinct places**:

1. **Prism extension** (`prism.ts` lines 1123–1142): The prism TypeScript
   extension handles a `register_provider` frame received over the harness
   socket (the sidecar → extension channel). When received, it calls
   `api.registerProvider(name, config)` which maps to `pi.registerProvider()`
   in pi's extension runtime.

2. **Extension runner** (`dist/core/extensions/runner.d.ts` lines 94–95):
   `pi.registerProvider()` is available as an extension API. Extensions can
   call it during `session_start` (static config) or at any later point
   (runtime). The extension runner's `bindCore()` method flushes any
   `pendingProviderRegistrations` queued during extension load, then replaces
   the queue with a live `registerProvider` function that takes effect
   immediately.

`register_provider` is **not** in the `RpcCommand` union type. It cannot be
sent to pi via stdin in RPC mode. The mechanism for the daemon to configure a
provider dynamically is:
- Via the `register_provider` frame on the prism harness socket (current path),
  which the prism extension proxies to `pi.registerProvider()`.
- Or, in daemon mode (future), via the same mechanism if the prism extension is
  still loaded and the harness socket still exists.

**Runtime vs. static:** The extension runner's `bindCore()` flush model means
`register_provider` can be sent at any time during the session (it is not
limited to a startup window). The design doc's §11.5 question "whether pi's
`--rpc` mode supports `register_provider` before the first `prompt` frame" is
moot: it is not an RPC-mode concept at all; it is a harness socket concept.

### Implications for design doc

- **§11.5**: The open question is resolved partially. `register_provider` as a
  *runtime* wire frame is already implemented (via the prism extension). As a
  direct pi RPC command (stdin), it does not exist and is not needed — the
  prism extension already proxies it. The daemon would send `register_provider`
  on the harness socket, the extension forwards it to pi's runtime. The
  existing architecture handles this; no new pi feature is needed.
- **§6.4 of `pi-wire-protocol.md`**: The `register_provider` frame spec there
  is normative and implemented correctly in the prism extension.

---

## Q5 — Does pi support session continuation from an external event log?

**Verdict: yes, via `--session <path>` (file path) or `--session <uuid-prefix>`
with `--session-dir <dir>`.**

### Evidence

**CLI flags** (`dist/cli/args.js`):

```
--session <path|id>   Use specific session file or partial UUID
--continue, -c        Continue previous session (most recent in cwd)
--resume, -r          Interactive session picker
--session-dir <dir>   Directory for session storage and lookup
--fork <path|id>      Fork a session into a new session
```

**Functional verification:**

```bash
$ SESSION_FILE=~/.pi/agent/sessions/--home-ben--/2026-05-07T05-33-46-366Z_019e00ed-...jsonl
$ echo '{"type":"get_state"}' | timeout 5 pi --mode rpc --session "$SESSION_FILE"
{"type":"response","command":"get_state","success":true,"data":{"messageCount":6,"sessionId":"019e00ed-..."}}
```

A prior session with 6 messages was loaded and its `sessionId` matches. This
confirms conversation history is fully restored.

**Session file format** (`dist/core/session-manager.js`): Sessions are
append-only JSONL files. The first line is a header:

```json
{"type":"session","version":3,"id":"<uuid>","timestamp":"<ISO>","cwd":"<abs-path>"}
```

Subsequent lines are typed entries (model changes, user messages, assistant
messages, tool calls, tool results, thinking blocks, compaction summaries, etc.)
forming a tree (branching via fork). The full conversation history is in the
file.

**Session directory layout** (`dist/core/session-manager.js` line 213):

```
~/.pi/agent/sessions/
  <encoded-cwd>/           # e.g. --home-ben-code-nixos-config-main--
    <timestamp>_<uuid>.jsonl
    <timestamp>_<uuid>.jsonl
    ...
```

The `<encoded-cwd>` is computed as `--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--`.
**This is not the same as `~/.pi/agent/sessions/<session_id>/`** as claimed
in §5.11 of `pi-wire-protocol.md`, which references
`~/.pi/agent/sessions/<session_id>/`. The session UUID is *embedded in the
filename*, not used as a directory name.

**UUID-prefix matching** (`dist/main.js` lines 106–125): `--session <prefix>`
does a prefix-match against session UUIDs in the session directory. It searches
the cwd-local session directory first, then all session directories globally.
The match is against the filename's UUID portion, not the directory name.

**Daemon restart continuation** (relevant to §11.6): To re-spawn pi after a
daemon crash and continue from the same conversation, the daemon must:

1. Know the session JSONL file path (stored as `harness_session_id` in the
   prism DB — though the design doc's claim that `harness_session_id` is a
   session UUID needs refinement; the prism extension sends the session UUID,
   which prism records; the full file path must be reconstructed from the UUID
   and the session directory).
2. Spawn `pi --mode rpc --session <file-path>`.

Pi will load all prior conversation history from the JSONL file. The new pi
instance picks up exactly where the prior one left off.

**The design doc's §8.2 open question** about whether pi's `--rpc` mode
supports session continuation is resolved: yes it does, via `--session`.

### Implications for design doc

- **§9.2**: "pi writes conversation history to `~/.pi/agent/sessions/<session_id>/`"
  is incorrect. The actual path is
  `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl`.
  The `harness_session_id` in the DB records the UUID; the daemon must
  reconstruct the full file path from it. The session directory path depends
  on the session's `cwd`, which must also be stored.
- **§5.11 of `pi-wire-protocol.md`**: Same correction — `prism cleanup`'s
  reference to `~/.pi/agent/sessions/<session_id>/` is incorrect; the session
  file path is `~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl`.
- **§11.6**: Resolved. Pi supports session continuation via `--session <path>`.
  The file path (not just the UUID) must be preserved across daemon restarts.
  The daemon should store the full JSONL path in the DB at session creation time.
- **§8.2**: The restore logic described is implementable. The daemon stores the
  session file path at spawn time; on restart it re-spawns with `--session
  <path>`.

---

## Q6 — Where does pi store its credentials and session state?

**Verdict: confirmed with corrections to the design doc's path claim.**

### Credential paths

Pi's credential and config resolution follows this priority chain
(`dist/core/auth-storage.d.ts`, `dist/config.js`):

1. **Runtime override** — CLI `--api-key <key>` flag, or `AuthStorage.setRuntimeApiKey()`.
2. **`~/.pi/agent/auth.json`** — primary credential store. Contains API keys
   and OAuth tokens per provider, stored as `Record<string, AuthCredential>`.
   Protected at mode 0o600. Pi uses file locking for concurrent access.
   OAuth tokens are auto-refreshed by pi when they expire (with locking to
   prevent races between concurrent pi instances).
3. **Environment variables** — standard provider env vars (`ANTHROPIC_API_KEY`,
   `OPENROUTER_API_KEY`, etc.) — resolved after `auth.json`, as a fallback.
4. **`~/.pi/agent/models.json`** — custom provider definitions with embedded
   API keys (via `fallbackResolver`).

The `PI_CODING_AGENT_DIR` environment variable overrides the agent config
directory (`~/.pi/agent/`). This is what prism's bwrap bind-mount sets when
staging a per-session pi config directory (see `pi_invocation.go`).

### Session state paths

All paths are relative to the agent directory (`getAgentDir()` =
`$PI_CODING_AGENT_DIR` or `~/.pi/agent/`):

| Path | Content | Written by |
|---|---|---|
| `~/.pi/agent/auth.json` | API keys and OAuth tokens (JSON, mode 0o600) | Pi on login / token refresh |
| `~/.pi/agent/settings.json` | User settings (model, theme, tool config, etc.) | Pi's `/settings` command |
| `~/.pi/agent/models.json` | Custom provider definitions | Pi's model config |
| `~/.pi/agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl` | Append-only conversation tree | Pi (every message, every tool call, every turn) |
| `~/.pi/agent/atlassian-mcp-oauth.json` | Atlassian MCP OAuth tokens | Pi's `/login-atlassian` |
| `~/.pi/agent/pi-debug.log` | Debug log (when enabled) | Pi (optional) |
| `/tmp/pi-bash-<id>.log` | Full bash command output when truncated | Pi's bash executor (`dist/core/bash-executor.js` line 34) |

**Confirmed:** `~/.pi/agent/` is the canonical config directory. The design
doc's §9.2 and §7.3 are correct in naming this directory.

**Correction:** The design doc's §9.2 says "pi writes conversation history to
`~/.pi/agent/sessions/<session_id>/`". The actual format is
`~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl` — files within
a per-cwd directory, not files within a per-session-ID directory. See Q5.

**No new write paths outside of tool calls:** In daemon mode (pi running
unsandboxed), all pi writes occur to the paths above. Pi does not write to the
worktree outside of tool calls. The `bash` executor writes to `/tmp/pi-bash-*.log`
for large outputs; these are transient and not sensitive.

**Atlassian MCP OAuth tokens** (`~/.pi/agent/atlassian-mcp-oauth.json`):
In the current bwrap model, this file is bind-mounted read-write (see
`pi_invocation.go` lines for `atlasMCPPath`). In daemon mode, pi reads and
writes this file directly from the host with no sandboxing required.

### Implications for design doc

- **§9.2, §7.3**: The credential directory `~/.pi/agent/` is confirmed.
  The session path claim (`~/.pi/agent/sessions/<session_id>/`) needs
  correction to `~/.pi/agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl`.
- **§8.1**: The design doc claims pi's session path is the `harness_session_id`
  in the DB. This is accurate if the DB stores the full JSONL file path.
  Currently the DB stores the session UUID (from `session_status.session_id`);
  the daemon implementation must store the full path, not just the UUID.

---

## Appendix A — complete RPC command reference

For implementer convenience, the full `RpcCommand` union type from
`dist/modes/rpc/rpc-types.d.ts` (pi 0.72.1):

| Command type | Key fields |
|---|---|
| `prompt` | `message`, optional `images`, optional `streamingBehavior` (`"steer"` or `"followUp"`) |
| `steer` | `message`, optional `images` |
| `follow_up` | `message`, optional `images` |
| `abort` | — |
| `new_session` | optional `parentSession` (path) |
| `get_state` | — |
| `set_model` | `provider`, `modelId` |
| `cycle_model` | — |
| `get_available_models` | — |
| `set_thinking_level` | `level` (`"off"`, `"minimal"`, `"low"`, `"medium"`, `"high"`, `"xhigh"`) |
| `cycle_thinking_level` | — |
| `set_steering_mode` | `mode` (`"all"` or `"one-at-a-time"`) |
| `set_follow_up_mode` | `mode` (`"all"` or `"one-at-a-time"`) |
| `compact` | optional `customInstructions` |
| `set_auto_compaction` | `enabled` (boolean) |
| `set_auto_retry` | `enabled` (boolean) |
| `abort_retry` | — |
| `bash` | `command` (string) — executes a shell command and injects output into LLM context |
| `abort_bash` | — |
| `get_session_stats` | — |
| `export_html` | optional `outputPath` |
| `switch_session` | `sessionPath` |
| `fork` | `entryId` |
| `clone` | — |
| `get_fork_messages` | — |
| `get_last_assistant_text` | — |
| `set_session_name` | `name` |
| `get_messages` | — |
| `get_commands` | — |

**Note on `set_model` field name:** The design doc §3.3 and `pi-wire-protocol.md`
§6.3 show `{"type":"set_model","provider":"anthropic","model":"claude-sonnet-4-20250514"}`.
The actual RPC command uses `modelId`, not `model`:
`{"type":"set_model","provider":"anthropic","modelId":"claude-sonnet-4-20250514"}`.
The prism extension's wire-to-RPC translation must account for this
(`pi.setModel()` takes a `Model` object, not a string, so the extension
must first look up the model by `modelId` from the registry).

---

## Appendix B — design doc sections affected

The following design doc sections contain assumptions that diverge from pi's
actual interface. This is a flag for a follow-up refresh of the design doc;
the corrections are not made in this PR.

| Section | Claim | Actual |
|---|---|---|
| §3.2 | `pi --rpc [--worktree <path>]` | `pi --mode rpc`; no `--worktree` flag; use process `cwd` |
| §3.3 (frame table) | `tool_call` emitted pi→daemon; daemon sends `tool_result` to pi | Tool calls are executed internally; `tool_result` is not an RPC command |
| §3.3 (frame table) | `hello` / `hello_ack` in RPC stdout/stdin | These are harness socket frames only; no RPC handshake |
| §3.3 (frame table) | `session_shutdown` emitted pi→daemon via RPC | No RPC `session_shutdown` event; pi exits on stdin EOF |
| §3.3 (frame table) | `state_change` emitted pi→daemon via RPC | No RPC `state_change` event; pi emits `agent_start`/`agent_end` |
| §3.3 (frame table) | `set_active_tools` frame daemon→pi | Exists on harness socket only; not an RPC stdin command |
| §3.3 (frame table) | `register_provider` frame daemon→pi | Exists on harness socket only; not an RPC stdin command |
| §3.3 (frame table) | `set_model` with field `model` | Field is `modelId` in the RPC command |
| §9.2 | `~/.pi/agent/sessions/<session_id>/` | `~/.pi/agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl` |
| §5.11 (pi-wire-protocol.md) | `prism cleanup` locates `~/.pi/agent/sessions/<session_id>/` | Same correction as §9.2 |
| §11.1 | Open question: does `--rpc` mode exist? | Resolved: flag is `--mode rpc` |
| §11.5 | Open question: does `register_provider` work as RPC frame? | Resolved: it exists on the harness socket, not as RPC stdin |
| §11.6 | Open question: does pi support session continuation? | Resolved: yes via `--session <path>` |
