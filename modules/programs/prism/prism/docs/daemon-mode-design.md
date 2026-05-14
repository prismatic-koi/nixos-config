# Daemon-mode design: daemon-supervised pi RPC + per-tool sandboxing

**Status:** Design proposal (issue #1599). Prescriptive for PoC scope once
accepted; open questions listed in §11 are explicitly deferred.

**Author:** worker agent (prism session `daemon-mode-design`)
**Date:** 2026-05-15

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
   `--rpc` mode, not as a tmux-hosted process).
2. **Per-tool sandboxing** (the sandbox boundary moves from "wrap the pi
   process" to "wrap each tool call").

It might appear that these could be adopted independently. They cannot, for
the following reason:

**The daemon must own tool dispatch to sandbox each call.**

In the current model, the sidecar owns the sandbox lifecycle: it starts bwrap
or sandbox-exec and pi lives inside. Tool calls are pi's own business — the
sidecar observes them via SSE events but does not intercept them.

In the new model, pi runs unsandboxed. If the daemon does not own tool
dispatch, there is no layer at which to insert the per-call sandbox. The
daemon must be the intermediary between pi's tool call requests and the
actual tool execution — it receives a tool call from pi via the RPC protocol,
executes it in a restricted subprocess, and returns the result. This is only
possible because pi runs in `--rpc` mode: the daemon is the transport layer
that pi's tool calls flow through.

Conversely, the per-tool sandbox is only safe if the daemon owns the
execution context. If pi ran unsandboxed inside tmux (without the daemon's
RPC layer), model-generated tool calls would execute unrestricted on the host.
The daemon is the enforcement point.

The two ideas form a single coherent system: the daemon owns the loop
(spawn, supervise, dispatch, sandbox, return), and the loop is what makes
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
│  │               │   │  │ pi --rpc child process               │  │ │
│  │               │   │  │  (JSON-lines in/out via stdin/stdout) │  │ │
│  │               │   │  └──────────────────────────────────────┘  │ │
│  └───────────────┘   │  tool call dispatcher                      │ │
│                      │   ├── read/edit/write → worktree subprocess │ │
│                      │   ├── bash → restricted subprocess          │ │
│                      │   └── MCP → restricted subprocess           │ │
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
pi --rpc [--worktree <path>] [--agent <role>]
```

In `--rpc` mode, pi reads JSON-line frames from stdin and writes JSON-line
frames to stdout. The daemon holds both ends of the pipe. The exact flag name
and argument shape is subject to pi's own RPC protocol; the daemon must be
compiled against a known pi version or discover the flags at startup. See
§11 (Open questions) for the version-pinning question.

The spawn sequence is:

1. **Daemon receives a `spawn` request** from a client (IPC, CLI wrapper, or
   internal trigger). The request names a worktree path, a role
   (`worker`/`coordinator`/`review-*`), and optional config overrides.
2. **Daemon allocates a session record** in the DB (same schema as today,
   `agent_status` + `sessions` tables). The session is assigned an
   `instance_id` (UUID) and a `state` of `spawning`.
3. **Daemon resolves credentials** for the session (§7). Credentials are
   placed into the pi child's environment, not into a fake-home mount.
4. **Daemon starts the pi child process** via `os.StartProcess` or
   `exec.Cmd`. The child's stdin/stdout are connected to the daemon's
   per-session pipe goroutines. stderr is captured to a log file.
5. **Pi sends its first RPC frame** (a `hello` or session-ready signal,
   per pi's RPC protocol). The daemon transitions the session to `active`
   and marks it ready.
6. **Supervisor goroutine runs** for the lifetime of the child (§3.4).

### 3.3 The JSON-line RPC protocol surface

The daemon consumes and produces JSON-line frames on the child's
stdin/stdout. The pi RPC protocol is documented in the pi monorepo at
`packages/coding-agent/docs/rpc.md`. The daemon is the *client* that sends
prompts and receives events; pi is the *server* that processes them.

Key frame types the daemon consumes from pi (pi → daemon):

| Frame type | Meaning |
|---|---|
| `hello` | Pi is ready; carries pi version, session ID |
| `state_change` | Agent lifecycle transition (active, waiting, finished, error) |
| `tool_call` | Pi requests a tool invocation; `{id, name, args}` |
| `msg_assistant` | Streaming assistant text fragment |
| `turn_start` / `turn_end` | Turn boundary markers with token usage |
| `session_shutdown` | Pi process is about to exit cleanly |
| `provider_error` | LLM provider returned a non-OK response |
| `auto_retry_start` / `auto_retry_end` | Provider retry lifecycle |

Key frame types the daemon sends to pi (daemon → pi):

| Frame type | Meaning |
|---|---|
| `hello_ack` | Daemon acknowledges pi; carries session metadata |
| `tool_result` | Result of a tool invocation the daemon executed |
| `prompt` | Deliver a user message; `{text, deliver_as, images}` |
| `set_model` | Switch pi's active model live |
| `register_provider` | Configure a provider (API key, base URL) |
| `set_active_tools` | Restrict the tool set the LLM can call |
| `abort` | Trigger pi's abort handler |

This frame catalogue is directly derived from the existing pi wire protocol
specified in `docs/pi-wire-protocol.md`. The daemon-mode design reuses that
protocol verbatim — the difference is that in daemon mode the daemon *is*
the full intermediary, not just the sidecar observer.

The one substantive addition is the `tool_result` frame: today pi executes
tools itself and the sidecar only observes. In daemon mode, pi emits a
`tool_call` frame and **waits** for a `tool_result` frame from the daemon
before proceeding. The daemon is responsible for executing the tool (possibly
sandboxed) and returning the result. This is the core of the per-tool sandbox
mechanism.

### 3.4 Supervised lifecycle

Each pi child has a **supervisor goroutine** that:

- Reads stdout frames and dispatches them (tool calls go to the tool
  dispatcher; state frames go to DB writes; message frames go to the
  event fan-out).
- Writes stdin frames (prompt deliveries, tool results, model switches).
- Monitors `cmd.Wait()` for the child process to exit.
- Applies a **restart policy** (§3.4.1) when the child exits unexpectedly.

#### 3.4.1 Restart policy

| Exit condition | Action |
|---|---|
| Clean `session_shutdown` frame + exit 0 | Transition to `finished`. No restart. |
| Exit 0 without `session_shutdown` | Log anomaly. Transition to `finished`. No restart. |
| Non-zero exit, restart count < N | Transition to `error`, wait `backoff(N)`, restart child. |
| Non-zero exit, restart count ≥ N (circuit breaker) | Transition to `error`. No further restarts. Notify coordinator if applicable. |
| Context cancelled (daemon shutdown) | Send `abort` to pi, wait up to `shutdownTimeout`, then SIGTERM/SIGKILL. |

The restart count threshold `N` and the backoff schedule are configurable
in the daemon's config file (open question: §11.2). The current prism
`DefaultSidecarCircuitBreakerThreshold = 3` is the precedent.

#### 3.4.2 In-flight tool calls at restart

When the child exits with an in-flight tool call (a `tool_call` frame was
received but no `tool_result` has been sent back):

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

The following table enumerates every tool pi exposes today, based on the
opencode configuration in `modules/programs/prism/opencode.nix` and the
opencode agent definitions in `modules/programs/prism/opencode/agents/`.

**Sandbox decision key:**

- **Host** — executed directly on the host, no subprocess isolation.
- **Worktree-scoped subprocess** — executed in a subprocess with its working
  directory pinned to the session's worktree; the subprocess does not have
  access to anything outside that tree.
- **Restricted subprocess** — executed in a subprocess with explicit
  capability restrictions (bwrap on Linux, sandbox-exec on macOS); the
  exact profile is described in the Notes column.
- **Daemon-internal** — implemented entirely inside the daemon, no
  subprocess.

| Tool | Sandbox decision | Mounts / network | Credential plan | Notes |
|---|---|---|---|---|
| `read` | Worktree-scoped subprocess | Read-only bind of worktree directory only. No network. | None required. | Path traversal enforcement: paths are normalised and checked against the worktree root before the subprocess runs. Absolute paths outside the worktree are rejected before execution. |
| `edit` | Worktree-scoped subprocess | Read-write bind of worktree directory only. No network. | None required. | Same path enforcement as `read`. The subprocess may write only within the worktree. |
| `write` | Worktree-scoped subprocess | Read-write bind of worktree directory only. No network. | None required. | Same as `edit`. `write` creates files; the path enforcement is identical. |
| `patch` | Worktree-scoped subprocess | Read-write bind of worktree directory only. No network. | None required. | `patch` is an alias for structured edit (apply a unified diff). Identical sandbox profile to `edit`. |
| `grep` | Worktree-scoped subprocess | Read-only bind of worktree directory only. No network. | None required. | Pattern search. Read-only bind suffices. |
| `glob` | Worktree-scoped subprocess | Read-only bind of worktree directory only. No network. | None required. | File pattern matching. Read-only bind suffices. |
| `list` | Worktree-scoped subprocess | Read-only bind of worktree directory only. No network. | None required. | Directory listing. Read-only bind suffices. |
| `bash` | Restricted subprocess (bwrap / sandbox-exec) | Worktree directory (RW) + specific credential mounts as needed (see §7) + network (host, scoped by role — see Notes). | LLM API keys, GitHub token (role-scoped), Atlassian credentials injected per-call (§7). | The bash tool has the widest attack surface. The sandbox profile mirrors the current bwrap/sandbox-exec profile but scoped to the tool call, not the entire pi process. Commands are filtered by the opencode permission system before reaching this layer; the subprocess restriction is a defence-in-depth layer. Network access: outbound is permitted for `git push`, `gh`, `aws`, etc. Inbound connections are blocked. |
| `webfetch` | Restricted subprocess | No filesystem mounts (read-only temp dir only). Outbound network permitted (HTTP/HTTPS only). | None required. | Fetches a URL and returns the body. The subprocess has no write access to the worktree. A content-size cap is enforced by the daemon to prevent large-payload attacks. |
| `task` | Daemon-internal → triggers `session_spawn` | Inherits the parent session's worktree. | Inherited from parent session. | The `task` tool spawns a subagent session. In daemon mode this becomes a `session_spawn` call on the daemon itself rather than a `prism spawn` shell invocation. The subagent is a full pi RPC child. `task` is disabled for review agents and investigate agents (as today). |
| Atlassian MCP (`atlasian_*`) | Restricted subprocess (MCP proxy) | No filesystem mounts. Outbound network to `https://mcp.atlassian.com/v1/mcp` only. | ATLASSIAN_SITE, ATLASSIAN_EMAIL, ATLASSIAN_API_TOKEN injected at call time (§7). `mcp-atlassian-slim-proxy.mjs` is the current shim; this pattern continues. | The MCP proxy process is started per-call (or kept alive as a sidecar to the daemon — open question §11.4). The daemon injects credentials only for the duration of the call, then zeros the environment. Write operations (`atlasian_create*`, `atlasian_edit*`, etc.) are gated by the role permission table in the daemon (mirrors the current opencode permission config). |

**Notes on `bash` sandbox profile (bwrap, Linux):**

The current `internal/container/mounts.go:StandardSandboxMounts` defines the
canonical mount set for a pi session. In daemon mode this set is applied
*per bash tool call* rather than once at session start. The mounts for a
bash call are:

- Worktree directory (RW bind).
- `~/.gitconfig` and the synthesised per-session `.git` pointer (RO bind).
- SSH agent socket, if the command requires git push (conditional).
- GitHub token via env var (role-scoped, §7).
- LLM API keys are **not** needed for bash calls — they are injected into
  pi's process environment directly (§7) and bash subprocesses do not
  need them.
- AWS credentials (`~/.aws/config` RO, `~/.aws/sso` RW, `~/.aws/cli` RW)
  — conditional, present only when the session role requires AWS access.

The full set is derived from the same `StandardSandboxMounts` logic,
narrowed to what the command actually needs. Fine-grained per-command
credential injection (giving a `git push` command only a GitHub token and
nothing else) is an aspirational improvement; v1 of daemon mode uses the
same per-role mount set as today.

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
| Model reads credentials outside worktree via `read`/`edit`/`write`/`grep`/`glob`/`list` | These tools run in a worktree-scoped subprocess with no access to `~` or any other path. Path traversal is enforced before execution. |
| Model accumulates credentials via early tool calls for later exfiltration | Credentials are injected per-call, not per-session. A `bash` call that reads a GitHub token gets only that token for that call; it cannot access LLM API keys it was not explicitly given. |
| Model reads pi's own in-memory API keys via filesystem | Pi's environment is not accessible via any tool call (the tool subprocess is a separate process with a different environment). |
| Model establishes persistent sidecar via `bash` | Bash subprocess lives only for the duration of the tool call. The daemon waits for subprocess exit before returning `tool_result`. Detached processes (background `&`) are killed when the subprocess group is reaped. |

**Attack classes that become WEAKER under the new model compared to the
current model:**

| Attack class | Why it is weaker | Mitigation |
|---|---|---|
| **Pi itself is a direct attack surface on the host** | Pi runs unsandboxed. A compromised pi binary can read `~/.ssh`, `~/.aws`, `~/.claude`, etc. directly without any tool call — it has the full host environment. In the current model, pi is sandboxed and sees only the fake home. | Pi is a first-party binary (or a pinned version of a trusted dependency). The daemon enforces `tool_result` boundaries even when pi is compromised in a behavioural (not binary) sense — i.e., prompt injection that controls pi's *actions* is still sandboxed at the tool level. Binary compromise of pi is a supply-chain risk mitigated by version pinning and checksum verification. |
| **Pi's environment contains LLM API keys for the full session** | In the current model, LLM API keys are injected into the fake home environment for the session. In the new model, they are injected into pi's process environment on the host and are accessible for the entire session duration. A compromised or misbehaving pi binary could exfiltrate them. | The daemon can be designed to inject API keys via `register_provider` RPC frames at session start (keeping them out of the process environment entirely — open question §11.5). |
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

The new model is a net improvement for the primary threat (model-generated
tool calls that abuse the agent's execution context) at the cost of
weakened containment of pi itself. This trade-off is appropriate when pi
is a trusted binary and the attack surface we care about is adversarial
model outputs, not compromised pi code.

---

## 7. Per-tool credential brokering

### 7.1 Current architecture (4-PAT architecture)

Today, credentials are injected into the pi process environment at session
start via `internal/container/credentials.go:credentialEnvVars()`. The
current 4-PAT GitHub token architecture selects one of:

```
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR
PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR
PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER
```

based on the repo's GitHub account (derived from the git remote URL) and
the session role.

Additional credentials forwarded today:
- `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`
- `GITHUB_COPILOT_TOKEN`, `DEEPSEEK_API_KEY`, `OPENROUTER_API_KEY`
- `ATLASSIAN_SITE`, `ATLASSIAN_EMAIL`, `ATLASSIAN_API_TOKEN`

### 7.2 Per-tool credential plan in daemon mode

The daemon maintains a **credential store** populated from the host
environment at daemon startup (or refreshed on demand). Each tool call
receives only the credentials it needs:

| Tool | Credentials injected at call time |
|---|---|
| `read`, `edit`, `write`, `patch`, `grep`, `glob`, `list` | None. These tools do not need credentials. |
| `bash` | GitHub token (role-scoped, from 4-PAT architecture). LLM API keys are **not** injected — bash subprocesses should not make LLM calls. AWS credentials (optional, injected if the session role requires them). Atlassian credentials are **not** injected into bash — they are only available via the MCP proxy. |
| `webfetch` | None. HTTP requests use the OS network stack without authentication. (If a future use case requires authenticated fetches, credentials would be injected per-call.) |
| `task` | The spawned subagent session receives its own credential set via the normal session-spawn path (§3.2, step 3). |
| Atlassian MCP | `ATLASSIAN_SITE`, `ATLASSIAN_EMAIL`, `ATLASSIAN_API_TOKEN`. These are injected into the MCP proxy subprocess environment for the duration of the call, then the environment is zeroed on process exit. |

### 7.3 Pi's own credentials

Pi itself needs LLM API keys to call providers. In v1 of daemon mode, LLM
API keys are injected into pi's process environment at spawn time (same as
today). The daemon selects the key set based on the session's configured
provider list and the `register_provider` RPC sequence.

An improvement for v2 or later: the daemon injects API keys via `register_provider`
frames rather than env vars, keeping them out of pi's environment entirely and
allowing per-session key rotation without restarting pi. This is listed as an
open question (§11.5).

### 7.4 Credential scoping summary

The key improvement over the current model:

- **Atlassian credentials** are no longer visible to pi or to bash
  subprocesses. They are injected only into the MCP proxy subprocess for
  the duration of an MCP tool call.
- **GitHub token** is injected into bash subprocesses on demand, not into
  pi's environment. A model that uses `bash` to exfiltrate the GitHub token
  cannot then use it in a subsequent non-bash context.
- **LLM API keys** remain in pi's environment in v1 (necessary for pi to
  function). This is the primary remaining credential exposure in the new
  model and is called out explicitly in the threat model (§6.3).

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
   history from the DB via pi's own session-continuation mechanism
   (open question: §11.6 — does pi's `--rpc` mode support session
   continuation from a DB-supplied event log?).
4. **Clients that were connected** receive a connection reset. Reconnecting
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
- The DB records the `tool_call` event (written when the call arrived) but
  no corresponding `tool_result`. This is a detectable orphan state. The
  daemon's restore path should write a synthetic `tool_result` with
  `success=false` and `output="daemon restarted mid-call"` for any
  `tool_call` event without a matching `tool_result` in the last session
  incarnation.

### 9.2 Pi writes to the host filesystem outside of tool calls

In the new model, pi runs unsandboxed on the host. Pi may — intentionally
or via a future feature addition — perform direct filesystem I/O outside of
the tool call mechanism:

**Examples of current known pi I/O:**
- pi writes conversation history to `~/.pi/agent/sessions/<session_id>/`
  (the `harness_session_id` captured in the DB's `sessions` table).
- pi may write logs to its own log directory.
- pi may write cache files under `~/.cache/pi/` or similar.

**Examples of hypothetical future pi I/O:**
- A future pi version might implement a scratchpad feature with direct file
  writes.
- A future pi version might write telemetry to disk.

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

**Prevention boundary:** If pi is later found to be a vector for adversarial
direct writes (e.g. a prompt injection that exploits a pi feature that does
direct I/O), the mitigation is a namespace wrapper around the pi process
that grants write access only to pi's own state directories, not to the
worktree. This wrapper would be lighter-weight than the current whole-sandbox
approach and would not require a fake-home dance. This is deferred to a
future issue and not part of the v1 daemon mode design.

---

## 10. Coexistence strategy

### 10.1 Codename

The new system ships under the codename **`iris`** (placeholder — final
name TBD, see §11.7). All runtime artefacts use distinct paths to avoid
colliding with the existing prism system:

| Artefact | Current prism path | Codename (`iris`) path |
|---|---|---|
| Binary | `prism` | `iris` |
| DB | `~/.local/state/prism/prism.db` | `~/.local/state/iris/iris.db` |
| Daemon socket | (does not exist today) | `~/.local/state/iris/iris.sock` |
| Run directory | `~/.local/state/prism/run/` | `~/.local/state/iris/run/` |
| Config file | `~/.config/prism/config.json` | `~/.config/iris/config.json` |
| Log files | `~/.local/state/prism/logs/` | `~/.local/state/iris/logs/` |
| Archive root | `~/code/archives/prism/` | `~/code/archives/iris/` |

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

---

## 11. Open questions

The following decisions are explicitly deferred to the PoC or later design
phases. They are listed here rather than resolved unilaterally because they
require empirical data, external input, or design review that is beyond the
scope of this initial design doc.

**11.1 Pi `--rpc` mode: does it exist, and what is the exact interface?**
This design assumes pi supports a `--rpc` (or equivalent) mode where it
reads prompts from stdin and writes events and tool call requests to stdout.
The PI wire protocol doc (`docs/pi-wire-protocol.md`) describes the
extension-to-sidecar protocol, but the daemon-mode design requires the
daemon to *be* the sidecar — meaning pi's `--rpc` mode must support the
daemon being the tool executor. Whether this mode exists in the current pi
binary, what the exact flag is, and whether tool execution can be
delegated back to the caller are open questions that must be resolved
before the PoC can begin.

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

**11.4 MCP proxy lifecycle: per-call vs long-lived.**
The current `mcp-atlassian-slim-proxy.mjs` is configured as a per-session
MCP server. In daemon mode, should the MCP proxy be started per-call (clean
credential injection, no persistent state) or kept alive as a long-lived
process and handed calls via MCP's own protocol? Per-call is simpler and
cleaner for credential scoping; long-lived is faster (no Node.js startup
latency per call).

**11.5 LLM API keys: env var vs `register_provider` RPC.**
In v1, LLM API keys are injected into pi's process environment. A v2
improvement injects them via `register_provider` RPC frames, keeping them
out of the process environment. This requires pi to support `register_provider`
before the first `prompt` frame. Whether this is feasible depends on pi's
RPC protocol evolution (see §11.1).

**11.6 Session continuation after daemon restart.**
When a session is re-spawned after a daemon crash, can the new pi child
pick up the conversation where the old one left off? This requires pi to
accept a session continuation argument (e.g. a path to its own session
state directory) so it can replay conversation history. Whether pi's
`--rpc` mode supports this is an open question.

**11.7 Codename selection.**
The codename `iris` is a placeholder used in this document. The actual
codename should be chosen before the PoC begins to avoid confusion in the
codebase. The codename must not collide with any existing prism subcommand
or package name.

**11.8 Daemon process privilege and multi-user.**
This design assumes a single-user daemon (one daemon instance per user,
owned by that user). Multi-user support (a system-level daemon serving
multiple users) is explicitly out of scope, but the socket path and DB
path choices should not preclude it if the requirement arises later.

---

## 12. Child-issue breakdown

Each child issue is independently shippable. Dependencies are listed; an
issue may not be started until all its dependencies are merged.

| Issue | Title | Depends on | Closure policy |
|---|---|---|---|
| **D-0** | **This design doc** (issue #1599) | — | Merged once coordinator review passes. |
| **D-1** | Pi `--rpc` mode audit and interface contract | D-0 | Closed when a written interface spec exists for how pi supports daemon-delegated tool execution. May result in upstream pi changes if the mode does not yet exist. |
| **D-2** | Codename selection and directory skeleton | D-0 | Closed when the codename is chosen and the `iris/` package skeleton (binary entrypoint, config loading, DB open) builds and passes `go test ./...`. |
| **D-3** | Daemon core: spawn, supervise, JSON-RPC loop | D-1, D-2 | Closed when the daemon can spawn a pi `--rpc` child, receive `tool_call` frames, execute the tool, and return `tool_result`. No sandbox, no IPC socket — bare RPC loop only. |
| **D-4** | Per-tool sandbox: worktree-scoped subprocesses for read/edit/write/patch/grep/glob/list | D-3 | Closed when these six tools execute in a worktree-scoped subprocess with path traversal enforcement, and a test demonstrates that a path traversal attempt is rejected. |
| **D-5** | Per-tool sandbox: bash restricted subprocess (bwrap on Linux, sandbox-exec on macOS) | D-4 | Closed when bash tool calls execute in a restricted subprocess with the mount set from §5, and the sandbox-exec testing convention (see `docs/sandbox-exec-testing.md`) is followed on Darwin. |
| **D-6** | Daemon IPC socket: client connect/subscribe/fan-out | D-3 | Closed when a CLI client can connect to the daemon socket, subscribe to a session, and receive live `session_event` frames. |
| **D-7** | Per-tool credential brokering: Atlassian MCP subprocess isolation | D-5 | Closed when Atlassian MCP calls inject credentials only into the MCP proxy subprocess and the daemon's main environment does not contain Atlassian credentials. |
| **D-8** | Bubbletea TUI: session list + subscribe + prompt deliver | D-6 | Closed when a TUI connects to the daemon, shows a session list, allows selection, and shows live output without tmux. |
| **D-9** | Restore after daemon restart: orphan detection + re-spawn | D-3 | Closed when a daemon restart re-spawns sessions that were `active` in the DB, and orphaned `tool_call` events without `tool_result` receive synthetic failure results. |
| **D-10** | Parity gate: feature checklist from §10.3 | D-3 through D-9 | Closed when all items in the §10.3 parity checklist pass end-to-end tests. Signals readiness for rename. |
| **D-11** | Migration and rename: `iris` → `prism`, remove legacy code | D-10 | Closed when the binary is named `prism`, paths are updated, and the legacy `internal/container/`, `internal/sidecar/`, `internal/session/session.go` codepaths are deleted. |

Issues D-3 through D-9 can be worked in parallel after their immediate
dependencies land. D-10 and D-11 are strictly sequential — D-10 gates D-11.

D-1 is on the **critical path**: if pi does not support daemon-delegated
tool execution, the entire design changes. D-1 should be the first issue
actioned after this design doc is accepted.
