# PI harness wire protocol — design specification

**Status:** P2.WIRE design (issue #1208). Prescriptive: the implementations in
P2.SIDECAR (#1209) and P2.EXTENSION (#1210) treat this document as the
authoritative spec, not a starting point for negotiation. Frame schemas,
field names, value enums, and failure semantics are normative.

**Scope:** the wire protocol used by the prism sidecar to talk to a PI
session. This is a from-scratch design — it does not match opencode's
HTTP/SSE pattern. Rationale and full discussion: design conversation
2026-04-29.

**Audience:** the workers implementing P2.SIDECAR (Go, sidecar side) and
P2.EXTENSION (TypeScript, PI extension side); reviewers verifying that the
two implementations agree on the wire.

## 1. Architecture overview

prism integrates PI as a harness using a **bidirectional Unix socket**
(Linux) / **localhost TCP listener** (Darwin), separate from the existing
host-API socket that already carries `prism spawn`/`prism prompt`/`prism
cleanup` calls from inside the sandbox. The new socket carries the harness
event/control protocol — PI's hook events flow out, and prism's prompt/model
control flows in.

The sidecar **binds** the socket and waits. The PI **extension** (a
TypeScript file loaded by PI on startup, see #1210) connects out to the
socket on its `session_start` hook. JSONL frames flow in both directions for
the lifetime of the PI session. Disconnection signals session termination.

```
prism sidecar (host)                          tmux pane: PI (sandboxed)
─────────────────────                          ─────────────────────────
binds pipe.sock                                pi --mode rpc + extension
       │                                              │
       │  ◀── extension dials on session_start ─────  │
       │                                              │
       │  ◀── hello (protocol_version=2, …) ────────  │
       │  ─── hello_ack (session_name, role, …) ───▶  │
       │                                              │
       │  ◀── state_change, tool_call, tool_result, ──│ (PI hooks
       │      msg_assistant, turn_*, provider_error,  │  → frames)
       │      auto_retry_*, session_shutdown          │
       │                                              │
       │  ─── prompt, set_model, register_provider, ─▶│ (prism intent
       │      set_active_tools, abort                 │  → PI runtime)
       │                                              │
       │  ◀── connection close = session terminating  │
```

This document specifies §2 the transport layer, §3 framing rules, §4 the
version handshake, §5 the frame catalogue (extension → sidecar), §6 the
frame catalogue (sidecar → extension), §7 failure modes, §8 forward-compat
and version 2 extension rules, §9 cross-references to existing prism
subsystems.

## 2. Transport layer

### 2.1 Linux: per-session Unix socket

- Path: `~/.local/state/prism/run/<hash>/pipe.sock` where `<hash>` is the
  12-hex-char SHA-256 prefix of the session name produced by
  `internal/session/sidecar.go:SessionDirName`. This is the same per-session
  directory as the existing host-API socket
  (`internal/session/sidecar.go:SidecarHostAPIPath`); the two sockets
  co-locate so that one bind-mount (Linux bwrap) or one `(subpath ...)`
  rule (Darwin sandbox-exec) covers both.
- Resolved by a new helper `session.SidecarHarnessPipePath(sessionName)
  (string, error)` that mirrors `SidecarHostAPIPath` exactly — same hash,
  same parent directory, different filename.
- Mode: 0o600. Parent directory mode: 0o700. Same as host-API.
- The sun_path budget analysis in
  [`internal/session/sidecar.go`](../internal/session/sidecar.go)
  applies unchanged: the per-session directory name is a 12-hex prefix
  precisely so socket paths fit Darwin's 104-byte and Linux's 108-byte
  `sun_path` limits regardless of session-name length. Adding `pipe.sock`
  alongside `hostapi.sock` is well within the existing budget — see #1050
  for the full path arithmetic and the regression test in
  `internal/session/sidecar_test.go`
  (`TestSidecarHostAPIPath_LengthInvariant_*`).

### 2.2 Darwin: localhost TCP listener

- TCP listener on `127.0.0.1:0` (OS-allocated port). The listener binds on
  `127.0.0.1` (loopback only). Unlike the podman/gvproxy path — where the
  listener binds `0.0.0.0` so gvproxy's bridge interface (`192.168.127.254`)
  can reach it from the container VM — `sandbox-exec` runs **directly on the
  host**. Both the sidecar and the sandboxed extension share the host
  loopback, so binding only `127.0.0.1` is both sufficient and more secure
  (the port is not reachable from external network interfaces).
- The allocated port is captured at sidecar startup — *before* the
  extension is launched — and exposed as `tcp://127.0.0.1:<port>` via
  the env var (§2.4). **Note:** `host.containers.internal` is a
  podman/gvproxy convention (resolved by gvproxy inside the container VM)
  and does **not** resolve on bare macOS; using it for sandbox-exec causes
  `ENOTFOUND` on every dial.
- Rationale: macOS `sandbox-exec` does not support reliable Unix-socket
  bind-mounts into the sandbox in the same way bwrap does on Linux;
  virtiofs returned ENOTSUP for cross-VM Unix-socket connect() in #661
  and the same constraint applies here. Falling back to TCP on Darwin is
  the established prism pattern.
- Failure to bind is **fatal**. Silent fallback would reproduce the
  ENOTSUP class of bug. The sidecar must abort startup with a clear
  error if the listener cannot bind.

### 2.3 Sandbox exposure

- **Linux bwrap:** the sidecar pre-creates `~/.local/state/prism/run/<hash>/`
  before bwrap launches (this directory already exists for the host-API
  socket). The existing bind-mount entry that exposes the host-API socket
  directory inside the sandbox covers `pipe.sock` automatically — both
  sockets live in the same directory by design (§2.1). No new bind-mount
  is needed; P2.SIDECAR must verify and document this in a comment at
  `internal/container/bwrap.go`.
- **Darwin sandbox-exec:** the existing SBPL `(subpath ...)` rule for the
  per-session directory (`internal/container/sandbox_exec.go`) covers the
  pipe socket path if Linux. On Darwin the actual transport is TCP, so
  no path-based rule is needed for the pipe — but the SBPL profile must
  permit outbound TCP to the allocated port. The existing host-API TCP
  exception covers this if the port is on the same listener pattern; if
  not, a new SBPL rule must be added.
- **Linux non-bwrap (host mode):** the extension dials the absolute socket
  path directly. No sandbox plumbing.

### 2.4 Environment variable injection

The sandbox process environment must contain:

```
PRISM_HARNESS_PIPE=unix:///home/ben/.local/state/prism/run/abc123def456/pipe.sock
```

or on Darwin (sandbox-exec, where both sidecar and extension run on the host loopback):

```
PRISM_HARNESS_PIPE=tcp://127.0.0.1:54321
```

> **Note:** `host.containers.internal` is a podman/gvproxy convention and
> resolves only inside a container VM where gvproxy injects the hostname. It
> does **not** resolve on bare macOS. The sandbox-exec path always uses
> `127.0.0.1` instead.

The URI scheme is the discriminator: `unix://` for a filesystem path,
`tcp://` for a host:port. Extensions that handle both schemes are required
to parse the URI and select the appropriate dialler — see §6.4 of #1210 for
the TypeScript implementation contract.

The variable name `PRISM_HARNESS_PIPE` is chosen to be neutral with respect
to harness identity; future stdio-or-socket-bearing harnesses (Claude Code,
etc.) can reuse it.

### 2.5 Connection lifetime

- The sidecar binds the socket on startup and **listens before** PI is
  launched. The first frame received from the extension serves as the
  readiness signal that gates `OnReady` (the `~/.local/state/prism/run/
  <session>-sidecar.ready` file consumed by the agent pane).
- The sidecar keeps the listener **open across connection drops**. After
  an unexpected disconnect (no preceding `session_shutdown`), the sidecar
  waits up to `pipeDisconnectTimeout` (30 s) for the extension to
  reconnect. This window covers the `/new` scenario: PI's `/new` command
  triggers a `session_start` event which closes and re-opens the socket.
- The extension **must not** call `connect()` when a connection is already
  live. The `session_start` handler guards against duplicate connections
  (`if (socket !== null || connected) return`). See §7.2 for the full
  reconnect protocol.
- The connection persists for the lifetime of the PI session. A clean
  `session_shutdown` causes the sidecar to break its accept loop and exit.
  See §7.2 for drop handling.

## 3. Framing

The wire format is **strict JSONL**: one JSON object per line, **`\n`
(LF) delimited**. This matches PI's own RPC framing convention documented
at [`packages/coding-agent/docs/rpc.md`][pi-rpc] in the PI monorepo, which
explicitly notes that Unicode line separators (U+2028, U+2029) are **not**
record separators because they appear inside valid JSON strings. Any client
or server that splits on Unicode separators is non-compliant.

[pi-rpc]: file:///nix/store/...pi-monorepo/packages/coding-agent/docs/rpc.md

Rules:

- Each frame is a single JSON object terminated by exactly one `\n`.
- An optional trailing `\r` before `\n` is permitted on input
  (`\r\n` becomes `\n` after stripping). Output writes `\n` only.
- Line length is **unbounded**. Tool outputs in particular can be very
  large; a fixed-size line buffer is non-compliant. Implementations must
  use a streaming parser or a dynamically-grown buffer. Concretely:
  - Go (P2.SIDECAR): `bufio.Scanner` is **insufficient** — its default
    `MaxScanTokenSize` is 64 KiB. P2.SIDECAR must either call
    `Scanner.Buffer` to set a generous max (≥ 16 MiB), or use
    `bufio.Reader.ReadBytes('\n')` directly.
  - TypeScript (P2.EXTENSION): Node's `readline` is non-compliant
    because it splits on Unicode separators. P2.EXTENSION must
    implement a buffered chunk reader that splits on `\n` only — the
    existing `attachJsonlReader` pattern in PI's RPC client (see the
    [PI RPC docs §"Example: Interactive Client (Node.js)"][pi-rpc])
    is the reference implementation.
- Both directions use the same framing. Frames sent by the sidecar look
  identical on the wire to frames sent by the extension; only the `type`
  field disambiguates them.
- Every frame **must** have a `type` field of type `string`. Frames
  missing `type` are parse errors (§7.3).
- Field-name convention: `snake_case` for protocol fields. PI's internal
  RPC types use camelCase; the extension is responsible for translating
  between the two — that is the extension's job, not the sidecar's
  (§9.1).
- Numeric values are JSON numbers; strings are JSON strings; booleans are
  JSON booleans. No nested binary encoding.

## 4. Protocol version handshake

The handshake is a strict two-frame exchange that runs **before** any
domain frames. It establishes the protocol version and exchanges session
metadata.

### 4.1 First frame: extension → sidecar

```json
{"type":"hello","protocol_version":2,"harness":"pi","harness_version":"0.66.1"}
```

- `type` (string, required) — literal `"hello"`.
- `protocol_version` (integer, required) — the wire protocol version the
  extension speaks. Current version is **2** (bumped from 1 in #1434;
  extension now emits `state_change{finished}` directly at turn
  boundaries instead of `state_change{idle}`). See §8 for evolution.
- `harness` (string, required) — the harness identifier. For PI this is
  the literal `"pi"`. The sidecar uses this to validate that the connected
  extension matches the harness configured for the session.
- `harness_version` (string, required) — the PI package version (e.g.
  `"0.66.1"`). Used for diagnostic logging only in v1; the sidecar does
  **not** branch on harness version. Reserved for future use.

The extension **must** send `hello` as its first frame, before any
event frame. Sending any other frame first is a protocol violation
(§7.4) and the sidecar closes the connection.

### 4.2 First frame: sidecar → extension

In response, the sidecar sends:

```json
{"type":"hello_ack","protocol_version":2,"session_name":"prism@feature","session_role":"worker","isolation_mode":"sandbox-exec","instance_id":"01HZ..."}
```

- `type` (string, required) — literal `"hello_ack"`.
- `protocol_version` (integer, required) — the wire protocol version the
  **sidecar** speaks. Must match the extension's version exactly; a
  mismatch is rejected per §4.3. Current value is `2`. See §8.
- `session_name` (string, required) — the prism session name (e.g.
  `"<repo>@<branch>"`). The extension surfaces this in PI's session-name
  display (§3.5 of #1210) so the user sees the prism context.
- `session_role` (string, required) — `"worker"` or `"coordinator"`,
  derived from the sidecar's `Config.AgentRole`. The extension may
  branch on this to adjust behaviour (e.g. coordinators do not auto-start
  the merge-queue watcher from inside PI; that lives in the sidecar).
- `isolation_mode` (string, optional) — the effective isolation mode for
  this session: `"sandbox-exec"`, `"bwrap"`, or `"host"`. Absent (or
  empty string) when the sidecar is older and does not populate this
  field; the extension treats absent/empty as `"host"`. Used by the
  extension to show a `(host)` suffix in the status bar when the session
  is running without a sandbox (see §5.11).
- `instance_id` (string, required) — the UUID for this session
  incarnation, matching the value the sidecar uses for `to_instance_id`
  on bus messages (`internal/sidecar/sidecar.go:Config.InstanceID`).
  Surfaced for diagnostic logging on the extension side.

The extension **must** receive and validate `hello_ack` before sending any
domain frame. If `hello_ack` is missing required fields the extension logs
a clear error and disconnects.

### 4.3 Mismatch handling

- If `hello.protocol_version` is **higher** than the sidecar's max
  supported version: the sidecar logs the version skew, sends a clear
  rejection (`{"type":"error","code":"protocol_version_unsupported",
  "message":"sidecar supports protocol_version=2; extension sent 3"}`),  <!-- example -->
  closes the connection. The session transitions to `error`.
- If `hello.protocol_version` is **lower** than the sidecar's minimum
  supported version: same behaviour as above, with a `code` of
  `"protocol_version_too_old"`.
- If `harness` is not the registered harness for this session: same
  behaviour, `code` of `"harness_mismatch"`.
- Versions are not negotiated; `hello.protocol_version` must exactly
  equal the sidecar's supported version (currently `2`). A mismatch
  results in rejection regardless of direction. §8 describes future
  evolution.

### 4.4 Why this shape

- A single explicit handshake frame in each direction is the smallest
  protocol-level commitment that lets us evolve the protocol. We do not
  attempt to peek-and-decide based on the first frame's content.
- The handshake is complete in one round-trip — there is no separate
  capability-negotiation step. v1 has no capability flags. v2 may add
  them as additive optional fields on `hello`/`hello_ack` (§8.2).
- The handshake is fail-closed: if either side cannot complete it, the
  connection is closed. There is no degraded mode.

## 5. Frame catalogue — extension → sidecar

This section is **normative**. Every field name, every value enum, every
optional/required marker is part of the spec. The sidecar implementation
in P2.SIDECAR consumes these frames; the extension implementation in
P2.EXTENSION produces them.

For each frame:

- **`type`** is the literal string in the heading.
- All frames use `snake_case` field names.
- Required fields are marked **required**; optional fields may be omitted
  entirely (the sidecar treats absent and `null` identically).

### 5.1 `hello` (handshake)

See §4.1.

```json
{"type":"hello","protocol_version":2,"harness":"pi","harness_version":"0.66.1"}
```

### 5.2 `state_change`

PI lifecycle state transitions, mapped to prism's `agent.AgentState` enum.

```json
{"type":"state_change","state":"active"}
```

- `state` (string, required) — one of:
  - `"active"` — agent is processing (entered on `before_agent_start`,
    or on `auto_retry_start`/`turn_start` after an idle).
  - `"waiting"` — agent is waiting on out-of-band input (e.g.
    permission prompt, extension UI dialog). Extension emits this when
    PI raises an `extension_ui_request` of `select`/`confirm`/`input`/
    `editor`.
  - `"finished"` — turn-boundary terminal signal (protocol v2, #1434).
    Emitted by the extension at the end of each clean turn
    (`stopReason=stop`, no pending messages, no pending review call).
    The sidecar applies a **2 s debounce** (`handleSessionFinished`,
    `internal/sidecar/sidecar.go:IdleDebounce`) before writing
    `StateFinished` and calling `notifyCoordinator()`. A `turn_start`
    frame arriving within the 2 s window cancels the debounce and
    writes `StateActive` instead — the session does not transition to
    `StateFinished`. This applies uniformly to all session types
    (workers, coordinators, and review agents).
  - `"error"` — terminal: the session has hit an unrecoverable error.
    Emitted on `auto_retry_end` with `success=false`, or on extension
    catch-all error handlers.

**Removed in protocol v2:** `state_change{idle}` is no longer emitted
by the extension at turn boundaries. Any sidecar receiving this frame
from a pre-v2 extension would have rejected the connection at the
`hello_ack` handshake due to the protocol version mismatch, so no
silent state corruption can occur.

The sidecar validates the value against `agent.ValidTransitions`
(`internal/agent/machine.go`). Invalid transitions are logged and the
event is still recorded; the sidecar does not tear down on illegal
transitions.

### 5.3 `tool_call`

```json
{"type":"tool_call","id":"call_abc123","name":"bash","args":{"command":"ls -la"}}
```

- `id` (string, required) — PI's tool-call ID (corresponds to
  `toolCallId` in PI's `tool_execution_*` events).
- `name` (string, required) — the tool name (`bash`, `read`, `edit`, …).
- `args` (object, required) — the tool arguments. **Truncation:** the
  extension truncates large arg fields per existing prism conventions —
  same byte budget as opencode's `tool_call` events. Concretely the
  extension applies a per-field byte cap of **8 KiB** (matching
  `internal/payload/payload.go` `ToolCall` semantics) before serialising;
  truncated string fields are replaced with the first 8 KiB followed by
  the literal sentinel `"…[truncated]"`. The sidecar accepts whatever
  arrives without further truncation.

The frame maps directly into a `tool_call` row in `agent_events` (existing
schema). No translation required on the sidecar side beyond promoting
`args` from JSON object to JSON-encoded string for the DB column.

### 5.4 `tool_result`

```json
{"type":"tool_result","id":"call_abc123","success":true,"output":"total 48\ndrwxr-xr-x ..."}
```

- `id` (string, required) — matches the corresponding `tool_call.id`.
- `success` (boolean, required) — true if PI's `tool_execution_end.isError`
  was false; false otherwise.
- `output` (string, required) — the tool output as a string. **Truncation:**
  same convention as `tool_call.args` — the extension caps `output` at
  **8 KiB** with a `"…[truncated]"` sentinel suffix. Tool outputs that
  exceed the cap remain available to PI in full (PI writes them to its
  own `fullOutputPath` if applicable, see PI RPC docs §"bash"); only the
  wire copy delivered to prism is truncated.

The frame maps directly into a `tool_result` row in `agent_events`.

### 5.5 `msg_assistant`

A streaming fragment of an assistant message. PI emits these as
`message_update` events with a `text_delta` `assistantMessageEvent`
(see PI RPC docs §"message_update"); the extension translates each
delta into one `msg_assistant` frame.

```json
{"type":"msg_assistant","text":"Here is "}
```

- `text` (string, required) — the delta text. May be empty (treated as a
  zero-length fragment; contributes nothing to the coalesced text).

The sidecar **coalesces** fragments within a turn rather than writing one
row per fragment. Text from each `msg_assistant` frame is accumulated in
memory between `turn_start` and `turn_end`. On `turn_end`, a single
`msg_assistant` event is written to `agent_events` with:

- `text` set to the concatenation of all fragment texts in order.
- Token and cost fields (`inputTokens`, `outputTokens`, `cacheReadTokens`,
  `cacheWriteTokens`, `cost`) populated from the `turn_end` frame's `usage`
  object (zero when absent).

This produces one `msg_assistant` row per turn instead of ~50 fragmented
rows. The payload schema matches `internal/payload/payload.go:MsgAssistant`.

**Edge cases:**

- If `turn_end` arrives with no preceding `msg_assistant` frames (empty
  accumulator), a `msg_assistant` row is still written with empty `text`.
- If `session_shutdown` or a connection drop arrives with a non-empty
  accumulator, a partial `msg_assistant` row is written with the accumulated
  text and zero token/cost fields — the partial text is not silently
  discarded.
- The accumulator resets on each `turn_start`; multiple turns in a single
  session each produce their own single `msg_assistant` event.

### 5.6 `turn_start`

```json
{"type":"turn_start"}
```

- No required fields beyond `type`.

A turn begins — emitted on PI's `turn_start` hook. The sidecar persists
this as a `turn_start` event row.

### 5.7 `turn_end`

```json
{"type":"turn_end","usage":{"input":100,"output":50,"cache_read":40,"cache_write":5,"cost":0.0015}}
```

- `usage` (object, optional) — token and cost accounting for the turn.
  Fields:
  - `input` (integer) — input tokens consumed.
  - `output` (integer) — output tokens emitted.
  - `cache_read` (integer) — cached input tokens read.
  - `cache_write` (integer) — input tokens written to cache.
  - `cost` (number) — turn cost in USD as a JSON number (not a string).

Fields are **optional within `usage`**: any one of them may be absent
when PI does not have the data (e.g. cache fields are zero/absent for
non-cache-eligible providers). The sidecar persists whatever arrives
into the existing `agent_events` `turn_end` row; consumers that read
`internal/db/db.go:TokenTurn` already tolerate missing fields.

### 5.8 `provider_error`

A non-OK response from PI's underlying provider (`after_provider_response`
hook with non-OK status).

```json
{"type":"provider_error","provider":"anthropic","status_code":529,"message":"529 overloaded_error: Overloaded"}
```

- `provider` (string, required) — provider name (`"anthropic"`,
  `"openai"`, `"google"`, …).
- `status_code` (integer, required) — HTTP status code.
- `message` (string, required) — the provider's error message, verbatim
  from PI's `after_provider_response` hook.

Successful provider responses are **not** emitted (high-volume, not
useful for prism). The sidecar persists this as a new event type
(`provider_error`) on `agent_events` — see §9.2 for the DB schema
implications.

### 5.9 `auto_retry_start` / `auto_retry_end`

Bubbled directly from PI's `auto_retry_start` / `auto_retry_end` events
(PI RPC docs §"auto_retry_start"). The wire frames preserve PI's payload
shape but with prism's `snake_case` convention.

```json
{"type":"auto_retry_start","attempt":1,"max_attempts":3,"delay_ms":2000,"error_message":"529 overloaded_error: Overloaded"}
```

- `attempt` (integer, required) — 1-indexed retry attempt number.
- `max_attempts` (integer, required) — total attempts before final failure.
- `delay_ms` (integer, required) — delay before this attempt, in
  milliseconds.
- `error_message` (string, required) — the error that triggered the retry.

```json
{"type":"auto_retry_end","success":true,"attempt":2}
```

- `success` (boolean, required) — true if the retry succeeded.
- `attempt` (integer, required) — final attempt number reached.
- `final_error` (string, optional) — present when `success=false`,
  contains the final error message after retries were exhausted.

The sidecar persists both as new event types on `agent_events`.

### 5.10 `session_shutdown`

Process-exit signal. Emitted by the extension on PI's `session_shutdown`
hook (fired when the PI process is about to exit). This is a **process-exit
only** signal — it is never emitted at turn boundaries (#1432).
Per-turn completion is signalled via `state_change{finished}` (§5.2).

```json
{"type":"session_shutdown"}
```

- No required fields beyond `type`.

Sidecar response:

1. Flush any partial accumulator (in-progress assistant turn text).
2. Cancel any in-flight finished-debounce or recovery timer.
3. Write `StateFinished` to the DB and call `notifyCoordinator()`.
4. Close the socket connection cleanly.

This is the **clean** termination path. Connection drops without a
preceding `session_shutdown` are handled per §7.2.

### 5.11 `session_status`

Status snapshot emitted by the extension to inform the sidecar of the
current session identity and review-cycle progress. This frame is sent
immediately after the `hello_ack` handshake completes and again at the
start of each turn (`turn_start`), so the sidecar always has an up-to-date
picture of the session state.

```json
{"type":"session_status","role":"worker","branch":"fix-login-redirect","review_cycles":0,"pr_number":"","session_id":"ses_01J..."}
{"type":"session_status","role":"review","branch":"fix-login-redirect","review_cycles":2,"pr_number":"42","session_id":"ses_01J..."}
```

Fields:

- `role` (string, required) — session role as received in `hello_ack`
  (`"coordinator"`, `"worker"`, `"review"`, or `""` when unknown).
- `branch` (string, required) — branch label extracted from the
  `session_name`: the part after `@` when the name contains `@`, otherwise
  the full `session_name`.
- `review_cycles` (integer, required) — current review-cycle count for the
  detected PR. Zero when no PR has been detected yet.
- `pr_number` (string, required) — the PR number most recently detected by
  the review-cycle tracker, or `""` when unknown.
- `session_id` (string, required) — the PI session ID from
  `ctx.sessionManager.getSessionId()`. Used by the sidecar to populate
  `sessions.harness_session_id` so that `prism cleanup` can locate the
  correct session directory (`~/.pi/agent/sessions/<session_id>/`) for
  archiving. May be `""` in the post-handshake frame when no turn has
  started yet; the `turn_start` frame always carries the real ID. Added
  in #1538 to fix PI archiving.

Sidecar behaviour: when `session_id` is non-empty, the sidecar calls
`db.UpdateHarnessSessionID(sessionName, session_id)` to record the PI
session ID. The frame is also stored as a raw event per §8.2 forward-compat
rules.

The extension also calls `ctx.ui.setStatus("prism", text)` with a
human-readable summary at the same points. The status text format is:

```
[coordinator] main
[worker] fix-login-redirect
[review] fix-login-redirect · PR#42 · 2 cycles
[coordinator] obsidian (host)
[coordinator] obsidian (host) · PR#42 · 2 cycles
```

When `isolation_mode` from `hello_ack` is `"sandbox-exec"` or `"bwrap"`,
no isolation suffix is appended (sandboxed sessions are the normal case).
When `isolation_mode` is `"host"` or absent/empty (treated as host), the
suffix ` (host)` is appended after the branch label and before any PR/cycle
info. This surfaces unsandboxed sessions visually so users can tell them
apart from sandboxed ones at a glance.

### 5.12 `tool_progress`

```json
{"type":"tool_progress","id":"call_abc123","name":"bash"}
```

Mid-tool heartbeat. The extension emits this frame on a fixed cadence
(default 30s, configurable via `PRISM_TOOL_HEARTBEAT_INTERVAL_MS`) while a
tool execution is in flight, so that long-running invocations such as
`nix build .#prism` or `go test -count=20` don't silence the wire long
enough to trip the sidecar's per-session inactivity watchdog (#1728).

- `id` (string, required) — the `tool_call.id` of the in-flight call.
- `name` (string, required) — the tool name (e.g. `"bash"`). Informational
  only; the sidecar does not branch on it.

The first heartbeat fires only after the configured cadence has elapsed,
so fast tools (completing in < cadence) never produce a `tool_progress`
frame on the wire.

**Sidecar behaviour:** `tool_progress` resets the inactivity watchdog
via the standard inbound-frame `touchActivity` path, but is **not**
written to `agent_events`. Downstream consumers (narrative renderer,
checkin, TUI) therefore never see the frame — it is purely a liveness
signal between the extension and the sidecar. See `handlePipeFrame`'s
explicit `tool_progress` case for the no-op-on-events contract.

Introduced in #1761.

## 6. Frame catalogue — sidecar → extension

Frames sent by the sidecar to the extension. These represent **prism
intent** that the extension translates into PI runtime calls.

### 6.1 `hello_ack` (handshake)

See §4.2.

### 6.2 `prompt`

Deliver a user message into the PI session.

```json
{"type":"prompt","text":"please run the tests","deliver_as":"steer","images":[]}
```

- `text` (string, required) — the prompt text.
- `deliver_as` (string, required) — one of:
  - `"steer"` — queue as a steering message (PI's `steer` RPC command).
    Delivered after the current assistant turn finishes its tool calls,
    before the next LLM call.
  - `"followUp"` — queue as a follow-up message (PI's `follow_up`
    command). Delivered after the agent finishes.
  - `"nextTurn"` — deliver immediately as the next turn input (PI's
    `prompt` command, with `streamingBehavior` resolved per PI's idle
    state).

  These three values map 1:1 to the three delivery modes PI's RPC supports
  (PI RPC docs §"prompt"/"steer"/"follow_up"); the extension is the layer
  that calls the right runtime API.
- `images` (array of object, optional) — if non-empty, each element is an
  `ImageContent` block matching PI's image format:

  ```json
  {"type":"image","data":"<base64>","mime_type":"image/png"}
  ```

  Note `mime_type` (snake_case on the wire) versus PI's internal
  `mimeType` — translation is the extension's responsibility.

The extension's PI runtime call:

| `deliver_as` | PI RPC call (idle) | PI RPC call (streaming) |
|---|---|---|
| `steer` | `pi.sendUserMessage(text, {deliverAs: "steer"})` | `pi.sendUserMessage(text, {deliverAs: "steer"})` |
| `followUp` | `pi.sendUserMessage(text, {deliverAs: "followUp"})` | `pi.sendUserMessage(text, {deliverAs: "followUp"})` |
| `nextTurn` | `pi.sendUserMessage(text)` *(bare call, idle path)* | `pi.sendUserMessage(text, {deliverAs: "followUp"})` *(resolved mid-stream)* |

**`nextTurn` resolution detail:** PI's `sendUserMessage` requires an explicit
`deliverAs` whenever the runtime is streaming a turn. Calling it without
`deliverAs` mid-stream throws `"Agent is already processing. Specify
streamingBehavior ('steer' or 'followUp') to queue the message."` — the
throw would propagate into the dispatcher's outer try/catch and silently
drop the frame. The extension therefore queries `ctx.isIdle()` at delivery
time and routes:
- **Idle** (`isIdle() === true` or `ctx.isIdle` not a function on older
  runtimes): calls bare `sendUserMessage(text)` — equivalent to the
  `nextTurn` PI RPC command, scheduling the message for the next turn.
- **Streaming** (`isIdle() === false`): calls
  `sendUserMessage(text, {deliverAs: "followUp"})` — queues the message
  to be delivered after the current turn finishes, which is the correct
  semantic for notification deliveries (coordinator finish-notifications,
  merge-queue outcomes, startup-failure alerts) that arrive mid-stream.

Callers that want deterministic post-turn queuing regardless of idle state
should send `deliver_as: "followUp"` explicitly (which is what
`notifyCoordinator`, `notifyParentWorkerOnStartupFailure`, and the
merge-queue watcher all do).

### 6.3 `set_model`

Switch PI's active model live (mid-session).

```json
{"type":"set_model","provider":"anthropic","model":"claude-sonnet-4-20250514","thinking":"medium"}
```

- `provider` (string, required) — PI provider identifier.
- `model` (string, required) — PI model identifier.
- `thinking` (string, required) — one of `"off"`, `"low"`, `"medium"`,
  `"high"`, `"xhigh"`. (Note: `"minimal"` is omitted from the wire
  protocol enum even though PI's RPC accepts it; prism standardises on
  the five-level scale and the extension maps `"low"` to PI's
  `"low"` directly. `"xhigh"` is OpenAI codex-max only — see PI RPC
  docs §"set_thinking_level".)

The extension calls `pi.setModel(...)` then `pi.setThinkingLevel(...)`.

The decision logic about *when* to swap models — coordinator-vs-worker
scope, retry-on-failure, etc. — lives in the sidecar (P3.LIVE in a later
issue), not in this protocol. From this document's perspective the
sidecar decides; the extension obeys.

### 6.4 `register_provider`

Register a provider configuration with PI's runtime, matching PI's
`pi.registerProvider()` call.

```json
{"type":"register_provider","name":"anthropic","config":{"api_key_env":"ANTHROPIC_API_KEY","base_url":"https://api.anthropic.com"}}
```

- `name` (string, required) — provider name.
- `config` (object, required) — provider config blob. The shape mirrors
  PI's `pi.registerProvider()` config (see PI extension docs in the
  pi-monorepo for the exact field set). The wire protocol does **not**
  validate `config`'s internal shape — the extension forwards it verbatim
  to PI. This deliberately keeps the protocol decoupled from PI's
  per-provider config evolution.

### 6.5 `set_active_tools`

Configure which tools PI exposes to the LLM.

```json
{"type":"set_active_tools","tools":["read","bash","edit","grep","glob"]}
```

- `tools` (array of string, required) — tool names. The extension calls
  `pi.setActiveTools(tools)`.

### 6.6 `abort`

Trigger PI's abort handler — equivalent to PI's `abort` RPC command.

```json
{"type":"abort"}
```

- No required fields beyond `type`.

The extension calls PI's abort path. The state transition that follows
(active → idle → finished, or active → error) is reported back via
ordinary `state_change` frames; `abort` itself produces no acknowledgement
on this protocol.

## 7. Failure modes

### 7.1 Sidecar binds before PI starts; extension's first frame is the readiness signal

The sidecar binds the socket (Linux) or TCP listener (Darwin) **before**
launching PI. Specifically: socket bind happens in `runStartupSocketPipe`
(introduced by P2.SIDECAR) at the same point in the sidecar's `Run` flow
where opencode's `mgr.Create` runs today
(`internal/sidecar/sidecar.go:runStartupHTTP`).

The order is:

1. Sidecar binds `pipe.sock`.
2. Sidecar writes `PRISM_HARNESS_PIPE` into the sandbox env via
   `cmd/agent_run.go` (Linux bwrap) or
   `internal/container/sandbox_exec.go` (Darwin sandbox-exec).
3. Sidecar launches PI inside the sandbox (P2.AGENTRUN issue).
4. PI loads the prism extension; the extension dials the socket on its
   `session_start` hook.
5. The extension's `hello` frame arrives at the sidecar.
6. The sidecar replies `hello_ack` and **calls `OnReady`** (writes
   `~/.local/state/prism/run/<session>-sidecar.ready`). The agent pane,
   which is polling for this file, unblocks and runs `podman attach` /
   the equivalent for non-podman modes.
7. Domain frames flow.

The first frame **is** the readiness signal. There is no separate health
check, no port probe, no HTTP poll. This is a deliberate inversion of
opencode's pattern, where readiness is signalled by HTTP `/global/health`
returning 2xx.

If no frame arrives within the startup timeout
(`Config.StartupConnectTimeout`, default `DefaultStartupConnectTimeout` =
5 minutes; same default as bwrap-mode opencode in
`internal/sidecar/sidecar.go`), the sidecar transitions the session to
`error` via `writeStartupError` and tears down. This mirrors the existing
behaviour for opencode-on-bwrap.

### 7.2 Connection dropped mid-session

The sidecar distinguishes two kinds of disconnect:

**Clean shutdown** — a `session_shutdown` frame (§5.10) precedes the drop.
The sidecar marks the session `finished` immediately (no debounce on the
clean shutdown path) and exits its accept loop. No reconnect is attempted.

**Unexpected drop** — the connection closes (FIN or RST) without a
preceding `session_shutdown`. This is the normal path for PI's `/new`
command, which triggers a `session_start` event in the extension and
causes the old socket connection to close before the extension opens a
new one.

On an unexpected drop the sidecar:

1. Flushes the partial accumulator (any in-progress assistant turn is
   written as a `msg_assistant` event with whatever text has accumulated
   so far).
2. Logs the drop.
3. Re-enters `Accept` on the **same listener** (which remains open) with
   a `pipeDisconnectTimeout` (30 s) deadline.

If a new connection arrives within the window, the full P2.WIRE handshake
is replayed (`hello` / `hello_ack`) and the frame loop resumes. The
`OnReady` callback is **not** re-fired on subsequent connections.

If no connection arrives before the timeout expires (or the context is
cancelled), the session transitions to `error` state and the sidecar
returns an error.

The extension **must** guard `connect()` so it only fires when not
already connected (`if (socket !== null || connected) return`). This
prevents a duplicate connection if `session_start` fires while a live
connection exists.

### 7.3 Frame parse error

A line that is not valid JSON, or is valid JSON but not an object, or is
an object missing the `type` field:

- Log the error with the first 200 bytes of the offending line truncated
  and base64'd so it cannot corrupt log streams.
- Skip the frame. **Do not** tear down the connection.
- Increment a per-session parse-error counter (exposed via existing
  diagnostic surfaces; out of scope for v1 to define).

This matches the existing behaviour in
`internal/sidecar/sidecar.go:runStartupStdio` for stdio-pipe harnesses
(see the `parse frame` log line and the `continue` after `json.Unmarshal`
fails), which is the precedent worth following here.

### 7.4 Protocol violation (handshake failure, unexpected frame order)

A protocol violation is more severe than a parse error: the wire is
syntactically valid but semantically wrong (e.g. extension sent a
`state_change` before `hello`; `hello.protocol_version` is not 1; etc.).

Sidecar behaviour:

1. Send a single `error` frame to the extension:
   ```json
   {"type":"error","code":"protocol_violation","message":"<details>"}
   ```
   `code` is one of `protocol_version_unsupported`, `protocol_version_too_old`,
   `harness_mismatch`, `pre_handshake_frame`, or `unknown`. `message` is
   human-readable.
2. Close the connection.
3. Mark the session `error`.

The extension's response to receiving an `error` frame: log and disconnect.
There is no recovery path. The extension does **not** retry the handshake.

### 7.5 Backpressure

Both directions write **synchronously**. If a write blocks because the peer
is slow to read, the writing side stalls.

This is a deliberate v1 simplification:

- The sidecar's outbound queue (the channel feeding the writer goroutine
  in P2.SIDECAR) is unbuffered. If the extension stops reading, the
  sidecar's prompt-delivery code blocks until either the extension reads
  or the connection is dropped.
- The extension's outbound queue is the natural backpressure point of
  Node's writable stream — `socket.write()` returning `false` means the
  in-kernel buffer is full and the application should pause. The
  extension should not call `socket.write` faster than the kernel
  drains; PI's hook callback chain naturally throttles itself once the
  socket buffer fills.

**Document, do not engineer around.** v1 ships with synchronous writes
and accepts that a pathologically slow peer will stall the other side.
If this becomes a real problem in production we revisit with a measured
design rather than speculatively bounded queues.

Failure mode if a stall lasts longer than the read deadline: the reader
side observes the lack of forward progress as a connection-level timeout
and treats it as §7.2 (connection dropped).

### 7.6 Crash recovery

If the sidecar crashes:

- The socket's filesystem entry remains until the process exits (the
  kernel frees it on close). On restart, the sidecar must `unlink` any
  stale `pipe.sock` before binding — the same pattern the host-API socket
  already uses.
- The extension on the other end observes the connection drop (kernel
  RST or EOF on read) and PI continues running standalone. PI's TUI
  remains visible to the user. Restarting the sidecar via
  `prism restore` re-creates the socket but does **not** re-establish
  a connection to the still-running PI — the extension will not retry.
  The user sees a session in `error` state and must spawn a new session.
  This is the same UX as opencode-after-sidecar-crash today.

If PI / the extension crashes:

- The sidecar observes connection drop per §7.2.

## 8. Compatibility with opencode and forward-compat

### 8.1 Translation into `agent_events`

The frame schema is designed to translate cleanly into the existing
`agent_events` DB rows defined in
`internal/db/db.go` and `internal/payload/payload.go`. Specifically:

| Wire frame | DB event type | Notes |
|---|---|---|
| `state_change` | `state_change` | direct map; payload is `payload.StateChange`. |
| `tool_call` | `tool_call` | direct map; `payload.ToolCall` accepts these fields. |
| `tool_result` | `tool_result` | direct map; `payload.ToolResult`. |
| `msg_assistant` | `msg_assistant` | direct map; `payload.MsgAssistant`. |
| `turn_start` | `turn_start` | new event type — see §9.2. |
| `turn_end` | `turn_end` | new event type — see §9.2. |
| `provider_error` | `provider_error` | new event type — see §9.2. |
| `auto_retry_start` | `auto_retry_start` | new event type — see §9.2. |
| `auto_retry_end` | `auto_retry_end` | new event type — see §9.2. |
| `session_shutdown` | (no row) | drives the sidecar's terminal-state write only. |

Any **PI-side renaming** (translating PI's camelCase hook event names
into prism's snake_case wire field names — e.g. `toolCallId` → `id`,
`partialResult` → `output`, `mimeType` → `mime_type`) is the
extension's job, not the sidecar's. The sidecar accepts the wire as-is
and writes it to the DB.

The one exception: **archive normalisation**. The opencode raw-archive
→ pi-mono-v3 trace translation in `internal/piexport/piexport.go`
already lives in the archive adapter, not the sidecar. PI archive
normalisation belongs there too; that work tracks separately under
issue #1143.

### 8.2 Future protocol versions (additive, not breaking)

Version 2 of this protocol — when it ships — is constrained to be
**additive** with respect to v1. The constraints:

- **No breaking changes to existing frame types.** The `tool_call`
  frame's required fields (`id`, `name`, `args`) are stable forever.
  Removing or renaming a v1 field is a v3 conversation.
- **New optional fields** may be added to existing frame types in v2.
  Both sides must tolerate unknown fields silently (§3 already requires
  this for forward compat — JSON parsing ignores unknown fields by
  convention in both Go's `encoding/json` with a non-strict decoder
  and TypeScript's structural typing).
- **New frame types** may be added in v2. Both sides must tolerate
  unknown `type` values: the receiver logs and skips, exactly per
  §7.3's parse-error path. The sidecar's existing
  `runStartupStdio` already implements this behaviour for the stdio
  transport (`default: writeEvent(frame.Type, json.RawMessage(line))`)
  and the same shape is reused.
- **Capability flags** may be added as optional fields on `hello`/
  `hello_ack` in v2. For example, `hello.capabilities = ["images_v2"]`
  signals to the sidecar that this extension supports a richer image
  payload. The sidecar's `hello_ack` echoes the intersection of its
  own capability set with the extension's. Frames gated on a capability
  must be omitted entirely if the capability is not in the negotiated
  set — they cannot be sent and silently ignored.
- **Version selection** at handshake: the sidecar publishes a `(min, max)`
  supported version range; the extension publishes a single value
  (`hello.protocol_version`). The sidecar accepts the connection iff
  `min ≤ hello.protocol_version ≤ max`, and `hello_ack.protocol_version`
  echoes the lower of `hello.protocol_version` and the sidecar's max.
  For v1, both sides hard-code `1`.
- **Deprecation path:** when v3 deprecates a v1 field, the sidecar at
  v3 still accepts and translates it, but emits a deprecation log
  line. v4 may remove support after at least one full release cycle
  with the deprecation warning in place.

This is cheap to nail down now and expensive to retrofit later. Workers
on P2.SIDECAR and P2.EXTENSION must implement the parse-and-skip-unknown
behaviour even though v1 has no unknown frame types — the cost is one
`default:` branch in each direction's switch statement, and it is the
single most important forward-compat guarantee in the protocol.

### 8.3 Sharing event-handling code paths between harnesses

The sidecar's downstream event-handling code (finished debounce, state
machine, DB writes, dashboard sentinel touches) is **harness-agnostic**
by design. The same code paths handle opencode SSE events and PI socket
frames once they are normalised into `agent_events` writes.

Specifically, the following sidecar internals are shared:

- `internal/sidecar/sidecar.go:IdleDebounce` (the 2 s finished-debounce
  window; used by both `handleSessionFinished` on the PI path and
  `handleSessionIdle` on the opencode SSE path).
- `internal/sidecar/sidecar.go:upsertState` and `writeStateChange` (the
  state-machine writes).
- `internal/sidecar/sidecar.go:writeEvent` (the generic event writer).
- The merge-queue watcher (`internal/mergequeue/watcher.go`).
- The host-API server (`internal/sidecar/host_api.go`).

P2.SIDECAR is required to call into these existing helpers from
`runStartupSocketPipe`, not duplicate them. The branch point in `Run` is
the launch-and-readiness sequence only, exactly per the recommendation in
B.3 §"Position 3" of the
[stdio-harness lifecycle review](reviews/B3-sidecar-lifecycle-for-stdio-harnesses.md).

## 9. Cross-references

### 9.1 Existing prism subsystems this design depends on

- [`reviews/B3-sidecar-lifecycle-for-stdio-harnesses.md`](reviews/B3-sidecar-lifecycle-for-stdio-harnesses.md) —
  the lifecycle review whose Position 3 recommendation (one `Run` method,
  one branch point at the launch-and-readiness sequence) governs how
  `runStartupSocketPipe` integrates with `runStartupHTTP`. The PI socket
  protocol is an instance of that recommendation: the sidecar is not the
  parent of PI (PI runs in the agent pane under bwrap/sandbox-exec), but
  the sidecar holds the inbound channel and translates events into the
  shared sidecar back-half. This is the **socket** variant of the
  abstract "non-HTTP transport" the B.3 review classifies — it is closer
  in shape to opencode-on-HTTP (sidecar-as-server-or-client; harness
  runs separately) than to stdio-pipe (sidecar-as-parent-of-harness),
  which is why the new transport shape is `TransportSocketPipe`, not
  `TransportStdioPipe` (#1209 §3).
- The host-API socket pattern in
  [`internal/sidecar/sidecar.go`](../internal/sidecar/sidecar.go) and
  [`internal/session/sidecar.go`](../internal/session/sidecar.go) — the
  pipe socket reuses the same per-session directory layout
  (`SessionDirName`), the same sun_path-budget reasoning (#1050), the
  same Darwin TCP-fallback pattern. P2.SIDECAR should add
  `SidecarHarnessPipePath` adjacent to `SidecarHostAPIPath` and verify
  the sun_path invariant test (`TestSidecarHostAPIPath_LengthInvariant_*`)
  is extended or duplicated for the new path.
- PI's RPC framing convention is documented at
  [`packages/coding-agent/docs/rpc.md`][pi-rpc] in the PI monorepo. The
  JSONL framing rules in §3 are deliberately identical so the
  extension can reuse PI's own framing utilities where feasible.

### 9.2 Sibling issues

- **P2.SIDECAR (#1209)** — Go-side implementation. Adds:
  - `harness.TransportSocketPipe` enum value (parallel to the existing
    `TransportHTTPPort` / `TransportStdioPipe` / `TransportFallbackScreenScrape`).
  - `session.SidecarHarnessPipePath`.
  - `sidecar.runStartupSocketPipe` (parallel to `runStartupHTTP` and
    `runStartupStdio`).
  - New `agent_events.type` values: `provider_error`, `auto_retry_start`,
    `auto_retry_end`, `turn_start`, `turn_end`. Schema migration:
    add a comment in `internal/db/db.go` documenting these as accepted
    types; no migration is needed because `type` is an open string column.
  - New `payload` structs for the new event types (mirroring the wire
    schema in §5.7-§5.9 here).
- **P2.EXTENSION (#1210)** — TypeScript-side implementation. Loads on
  PI's `session_start`, dials `PRISM_HARNESS_PIPE`, performs the
  handshake (§4), bridges PI hooks (§5) and inbound commands (§6).
- **P2.AGENTRUN** (issue TBD) — wires `prism agent-run` to launch
  `pi --mode rpc --extension <prism-extension-path>` for PI sessions,
  and injects `PRISM_HARNESS_PIPE` into the sandbox env.
- **P2.SPAWN** (issue TBD) — wires `prism spawn --harness pi` to
  register the PI harness in the registry with shape
  `TransportSocketPipe`.
- **P3.LIVE** (issue TBD) — owns the sidecar logic that decides
  *when* to send `set_model`, *what* scope to swap (worker vs
  coordinator), *what* retry policy to apply on failure. The
  protocol-level frame is documented here (§6.3); the policy that
  drives it is not.
- **#1143** — archive normalisation for PI sessions, owned by the
  archive adapter (not the sidecar). Out of scope here.

### 9.3 Out of scope for this document

- The sidecar's own state machine internals (finished-debounce timing,
  reconnect-after-crash policy beyond §7.6, etc.) — these are
  documented in `internal/sidecar/sidecar.go` and apply
  unchanged.
- The PI extension's loading mechanism (how `pi --extension <path>`
  resolves the file, how the file is mounted into the sandbox) — owned
  by P2.AGENTRUN.
- The exact Go function signatures of `runStartupSocketPipe` and
  `SidecarHarnessPipePath` — owned by P2.SIDECAR; this document
  prescribes only the wire behaviour.
- The exact TypeScript class structure of the PI extension — owned
  by P2.EXTENSION.
- Multi-connection support (e.g. a "dashboard tail" client connecting
  alongside the extension). Explicitly non-goal for v1.
- Encryption / authentication on the socket. The socket is filesystem-
  permission-protected (mode 0o600 on a directory mode 0o700) and lives
  inside the user's home directory on a single-user system. No
  cryptographic protocol layer is added.

---

## 10. Iris daemon-mode tool dispatch frames (D-3)

Added in D-3 (`d3-iris-daemon-core`). These four frame types extend the
existing harness socket protocol to support the `registerTool()` override
mechanism described in `docs/daemon-mode-design.md §3.3.2`.

When pi is spawned by the iris daemon, the prism extension detects the
`IRIS_DAEMON_SOCK` environment variable (§3.4 of the design doc) and
overrides all seven canonical built-in tools with iris dispatch shims. Each
shim's `execute()` callback communicates with the iris daemon over the same
per-session harness socket used for the existing handshake and observation
frames.

The daemon is the server; the extension is the client. The daemon dispatches
tool calls to host subprocesses (D-3: unsandboxed; D-4/D-5: sandboxed) and
streams results back.

Wire format is JSON-line (one JSON object per `\n`-terminated line), identical
to the existing framing rules in §3. All field names use `snake_case`.

### 10.1 `tool_exec` (extension → daemon)

The iris override's `execute()` callback sends this frame to the iris daemon
when the pi LLM emits a tool call.

```json
{"type":"tool_exec","id":"call_abc123","name":"bash","args":{"command":"echo hello"}}
```

- `type` (string, required) — literal `"tool_exec"`.
- `id` (string, required) — the pi-supplied tool call ID (`toolCallId` in
  pi's `execute()` callback). Used for response correlation. Multiple
  `tool_exec` frames may be in flight simultaneously on a single connection;
  `id` is the only correlation key.
- `name` (string, required) — the tool name. One of the seven canonical
  built-ins: `read`, `bash`, `edit`, `write`, `grep`, `find`, `ls`.
- `args` (object, required) — the tool arguments as passed to `execute()`.
  The schema matches pi's built-in tool parameter schema for the named tool.

The daemon begins executing the tool immediately upon receiving this frame.

### 10.2 `tool_exec_update` (daemon → extension)

Sent by the iris daemon to stream partial tool output during long-running
tool calls, especially `bash`. Zero or more update frames may precede the
terminal `tool_exec_result` frame.

```json
{"type":"tool_exec_update","id":"call_abc123","content":"partial output chunk"}
```

- `type` (string, required) — literal `"tool_exec_update"`.
- `id` (string, required) — matches the in-flight `tool_exec.id`.
- `content` (string, required) — a partial stdout/stderr chunk from the
  tool subprocess.

The extension forwards these as `onUpdate` callbacks to pi, which streams
the partial output to the LLM context.

### 10.3 `tool_exec_result` (daemon → extension)

Terminates a tool call. Sent after all `tool_exec_update` frames (if any).
No further frames for the same `id` will be sent after this frame.

```json
{
  "type": "tool_exec_result",
  "id": "call_abc123",
  "success": true,
  "is_error": false,
  "output": "hello\n"
}
```

- `type` (string, required) — literal `"tool_exec_result"`.
- `id` (string, required) — matches the originating `tool_exec.id`.
- `success` (boolean, required) — `true` when the tool subprocess exited
  with code 0 and no error occurred; `false` otherwise.
- `is_error` (boolean, required) — `true` when the result should be
  surfaced as a tool error to the LLM (mirrors pi's `isError` convention
  for tool results). Always `true` when `success` is `false`.
- `output` (string, required) — combined stdout+stderr from the tool
  subprocess, or `"aborted"` when the call was terminated by a
  `tool_abort` frame.
- `details` (object, optional) — structured metadata about the result
  (e.g. exit code, signal). Absent on most calls.

The extension returns `output` to pi as the tool result, surfacing it to
the LLM as if the built-in had executed.

### 10.4 `tool_abort` (extension → daemon)

Sent by the iris override's `execute()` when the `AbortSignal` fires during
a long-running tool call. The daemon kills the tool subprocess (and all
descendants in its process group) and returns a `tool_exec_result` frame with
`success: false, is_error: true, output: "aborted"`.

```json
{"type":"tool_abort","id":"call_abc123"}
```

- `type` (string, required) — literal `"tool_abort"`.
- `id` (string, required) — the `id` of the in-flight `tool_exec` to abort.
  If no matching in-flight call exists, the frame is silently ignored.

### 10.5 Concurrency model

Multiple `tool_exec` frames may be in flight simultaneously on a single
connection. Pi's `executionMode: "parallel"` for overridden tools allows
concurrent dispatch. The daemon spawns one goroutine per `tool_exec` and
correlates responses by `id`. There is no ordering guarantee between results
for different `id` values.

The `tool_abort` frame targets a specific `id`; it does not abort all
in-flight calls.

### 10.6 Interaction with existing observation frames

The `tool_exec` / `tool_exec_result` frames are the **dispatch** channel —
they drive actual tool execution. They coexist with (but are orthogonal to)
the existing `tool_call` / `tool_result` observation frames (§5.3 / §5.4)
which the prism extension continues to emit to the sidecar for DB logging.

In iris daemon mode the sidecar role is taken by the iris daemon, which:
- Writes a `tool_call` event to `agent_events` when it receives `tool_exec`.
- Writes a `tool_result` event to `agent_events` when it sends
  `tool_exec_result`.

These DB writes satisfy the AC requirement that every tool dispatch is
recorded for auditability and D-9 orphan detection.
