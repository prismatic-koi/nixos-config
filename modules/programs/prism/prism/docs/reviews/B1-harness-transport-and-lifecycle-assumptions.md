# B.1 — Harness coupling review: transport-and-lifecycle assumptions

Track B (harness) Wave 1 anchor read for the narrow architecture-review series
defined in [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md)
and parent design issue #1072. This document is the harness-track lens on the
architecture inventory at [`../architecture-inventory.md`](../architecture-inventory.md),
specifically section §7 (the "harness coupling map") cover-to-cover.

This is a **classification document**. It surfaces assumptions and their
locations. It does **not** propose solutions — those live in B.2 (registry /
transport-shape declaration), B.3 (sidecar lifecycle for stdio harnesses),
B.4 (harness group abstraction), B.5 (payload schema), and B.6 (archive
pipeline).

## Context recap

The inventory tags coupling sites as `[opencode-only / multi-harness via interface / harness-agnostic]`.
That lens answers "is this code aware of opencode by name?" but it does not
answer the related-but-distinct question this review needs: **"would this code
still make sense if the harness spoke JSONL-over-stdio instead of HTTP+SSE?"**

Per the parent design (§"Background and motivation", point 2) and RFC #691,
PI is the imminent second harness, and it uses
**JSONL-over-stdin/stdout RPC** rather than HTTP+SSE. The user attaches to PI's
TUI via `podman attach`, but the **sidecar (or whatever is in the sidecar's
role) holds the stdin/stdout pipe** and consumes JSONL events directly. That
shape is fundamentally different from "dial a separately-running HTTP server
and consume an SSE stream" — and the `Harness` interface plus its surrounding
caller code carries assumptions about the HTTP shape that have never been
stress-tested against any non-HTTP harness.

This review introduces a fourth tag dimension on top of the inventory's
existing labels:

- **`[transport-agnostic]`** — the site uses the `harness.Harness` interface
  through methods that genuinely have no HTTP or port shape baked in (e.g.
  `Subscribe`, `MapEvent`, `ExtractEventType`, `ExtractMessage`,
  `ConfigEnvVar`, `RuntimeEnv`, `EffectiveModel`, `ValidateAgentRole`,
  `ContainerCommand`, `ConfigMountPath`).
- **`[http-only]`** — the site's shape only makes sense if the harness is
  HTTP-server-shaped. This includes the `port` parameter on `HealthCheck`,
  the `--port` flag plumbing, `WaitHealthy`, the SSE reconnection logic, the
  readiness file, the `--publish` port binding, and the host-API listener.
- **`[opencode-only]`** — the site references opencode by name (string
  literal, env var prefix, hard-coded path, payload field name) and would
  not work for any other harness without code change.
- **`[uncertain]`** — the worker cannot determine, from the code alone,
  whether the site's transport-shape is portable to a stdio harness without
  prototyping against PI. Flagged with a one-line reason.

Note that `[opencode-only]` and `[http-only]` are independent axes:
`internal/payload/payload.go` is `[opencode-only]` but transport-agnostic on
the wire (its field-name coupling is to opencode's plugin output schema, not
to HTTP); `internal/sse/client.go` is `[http-only]` but `[harness-agnostic]`
in the inventory's lens (it doesn't know what harness it is talking to).

## The eight HTTP-shape assumption classes

The issue spec (#1074, "Specific things to look for") names eight classes
explicitly. Each gets a dedicated subsection here before the per-§7-site
classification.

### 1. `HealthCheck(ctx context.Context, port int) error` — the interface itself bakes in HTTP

`internal/harness/harness.go:32` — `[http-only]` — `[structural]`.

The `Harness` interface's `HealthCheck` method takes a `port int` parameter:

```go
HealthCheck(ctx context.Context, port int) error
```

This is the most structurally-significant HTTP assumption in the harness
abstraction layer because it lives **on the interface itself**, not in a
caller. A stdio-pipe harness has no port to health-check on. The opencode
adapter's implementation (`internal/harness/opencode/adapter.go:121`) walks
straight into `healthURL := fmt.Sprintf("http://127.0.0.1:%d/global/health", port)`
— there is no way to honour the contract "verify the agent runtime is ready
to accept requests on the given port" for a process whose readiness signal is
"the JSONL pipe is open and responsive."

**The question this raises:** what is the harness-agnostic equivalent of
`HealthCheck(ctx, port int)`? The shape probably needs to be either
parameter-free (`HealthCheck(ctx) error` — the harness already knows where
to probe, since it constructed itself with whatever endpoint info it needs)
or pushed inside the harness adapter entirely, so callers no longer pass a
port at all. B.2 owns the actual answer.

### 2. `WaitHealthy` and `OnReady` callback in the sidecar Run loop

`internal/sidecar/sidecar.go:412` (`mgr.WaitHealthy(ctx)`),
`:447` (`[timing] WaitHealthy:` log),
`:164-168` (`OnReady` field in `Config`),
`:493-507` (`OnReady` invocation sites) — `[http-only]` (the lifecycle shape).

The `Container.WaitHealthy(ctx)` call inside `(*Sidecar).Run` is podman-
specific (it polls `podman healthcheck` for the container's HEALTHCHECK
command, which itself runs the opencode HTTP probe). After it returns,
`OnReady` fires — and `OnReady`'s only job is to write the readiness file
that unblocks `podman attach` in the agent pane.

This entire block is shaped by the assumption that:

1. There is a separately-running process (the harness) with a port to probe.
2. The sidecar's job is to wait for that probe to succeed.
3. After the probe succeeds, a third process (the agent pane running
   `podman attach`) needs to be unblocked.

For a stdio harness where the sidecar **launches** the harness as a child
process and **owns** its stdin/stdout, none of those three conditions hold.
"Healthy" becomes "the process is launched and the pipe is wired"; there is
no separate party that needs an unblock signal — the sidecar **is** the
party. See class 4 below for the readiness-file half of this.

### 3. `--port` flag allocation in `cmd/sidecar.go` and `internal/session/sidecar.go`

`cmd/sidecar.go:95` (flag declaration),
`:198-199` (validation in podman mode),
`:215` (`AllocatedPort: port` on container.Config),
`:239-241` (validation in bwrap/sandbox-exec mode) — `[http-only]`.

`internal/session/sidecar.go:212-213` (`Port int` field in `StartSidecarOpts`),
`:262` (`StartSidecar(sessionName, port)` signature),
`:301` (`opencodeURL := fmt.Sprintf("http://localhost:%d", opts.Port)`),
`:321, :338` (`cmdArgs = append(cmdArgs, "--port", strconv.Itoa(opts.Port))`)
— `[http-only]`.

Port allocation is threaded from the spawn flow all the way through to the
sidecar argv, the sidecar's `OpencodeURL`, the container's `--publish`
binding, the bwrap argv (`opencode --port <n>`), and the persisted
`harness_port` DB column. **None of this plumbing is needed if the harness
talks JSONL on stdin/stdout** — there is no port to allocate, no URL to
construct, and no `--port` flag to forward.

The `--port` flag's life-cycle is the single most pervasive HTTP-shape
assumption outside the `Harness` interface itself, because it has been wired
through six call sites independent of the harness adapter. None of those call
sites consult the harness to ask "do you need a port?" — they all assume the
answer is yes.

### 4. The readiness-wait file (`sidecar.ready`) and the agent-pane wait command

`cmd/sidecar.go:256-280` (`onReady` closure that writes
`prismSession.SidecarReadyPath(sessionName)` and the comment block:
"In bwrap and sandbox-exec modes the sidecar does NOT write the readiness
signal: 'prism agent-run' in the tmux pane starts immediately without
waiting. In host mode there is no readiness file at all."),
`internal/sidecar/sidecar.go:164-168` (the `OnReady` field doc) —
`[http-only]` (because it only fires in podman mode, which is the only mode
where the agent pane has a `podman attach` to gate on a separately-launched
HTTP server).

Inventory §7.2 lists this as `[harness-agnostic]` in the "mode-specific, not
harness-specific" sense — and it is true that today this code is **mode**-
aware rather than **harness**-aware. But the reason the file exists at all
is: in podman mode the agent pane needs a way to wait for the
separately-running opencode HTTP server before running `podman attach`,
because attaching too early lands you in an empty TTY that opencode hasn't
populated yet. That is the HTTP-server-with-its-own-lifetime shape.

For a stdio harness the sidecar **is** the launcher, so there is no
"separately-running process" the pane needs to wait for; the sidecar can
write whatever readiness signal it likes (or none) on its own schedule.
[uncertain — for PI specifically, the user-visible `podman attach` still
needs to wait until PI's TUI has rendered, so *some* readiness signal may
still be required even when transport is stdio. The exact gating semantics
depend on how PI's TUI interacts with `podman attach`'s PTY handover; this
needs prototyping before B.3 can commit to "drop the readiness file".]

### 5. The host-API HTTP server (`hostAPIHandler`) and per-session Unix socket

`internal/sidecar/sidecar.go:2611-3700` (the `hostAPIHandler` function and
all `mux.HandleFunc("/spawn", …)`, `/cleanup`, `/prompt`, `/list-sessions`,
`/checkin`, `/logs`, `/review`, `/switch`, `/merge`, `/merges`,
`/merges/cancel` registrations),
`:524-585` (the listener setup — Unix socket plus Darwin TCP fallback) —
`[http-only]` (the API surface) but **`[transport-agnostic]` with respect to
the harness** (the host-API server proxies between the agent and prism CLI;
the harness itself never touches it).

This is the "HTTP-shaped because the harness happens to be HTTP-shaped, but
conceptually independent of the harness transport" category from the issue
spec. The host-API exists so that an agent inside a sandbox (podman / bwrap /
sandbox-exec) can call back out to `prism prompt`, `prism spawn`, etc.,
without breaking the sandbox boundary. None of its endpoints relay anything
*to* the harness — they relay between the agent (running inside the harness)
and the host's prism binary.

So while `hostAPIHandler` is undeniably HTTP-shaped, switching the harness
transport from HTTP+SSE to JSONL-over-stdio does **not** require changing
the host-API server. The two are layered: the host-API is between agent code
and host code; the harness transport is between sidecar code and harness
code. They happen to both be HTTP today; only one of them must remain HTTP
on harness-shape grounds (and even that one — the host-API — could be
something else entirely if there were a reason).

This site is called out here for completeness, not because B.2/B.3/B.4 need
to act on it. It is a **non-issue** for the harness review.

### 6. The SSE reconnection logic at `internal/sse/`

`internal/sse/client.go:1-343` — `[http-only]` and `[harness-agnostic]`
(the package literally does not know the word "opencode").

The package is a generic SSE client with retry/backoff/heartbeat logic. It
is invoked exclusively from `internal/harness/opencode/adapter.go:444-476`
(`Subscribe`). The opencode adapter wires `sse.Client` into the harness's
`Subscribe(ctx) (<-chan HarnessEvent, error)` method, which **is** harness-
agnostic on the surface — `Subscribe` returns a channel of opaque
`HarnessEvent` records.

So the SSE client itself does not bleed HTTP-shape into the rest of the
codebase; the opencode adapter is the sole consumer, and the rest of the
sidecar consumes the channel without caring whether it came from SSE or
JSONL. This is the cleanest example of the interface successfully isolating
transport.

The catch: the **failure modes** the SSE client handles (retry on 502,
backoff on connection-refused, heartbeat warning when no events arrive) are
HTTP-server-shaped failure modes. A stdio harness's failure mode is "the
process exited" or "the pipe returned EOF" — qualitatively different signals
that need different handling inside the PI adapter. The interface signature
(`Subscribe`) accommodates that, but the **patterns the sidecar relies on**
(silent reconnect, infinite retry until ctx cancellation) need re-thinking
for stdio. See class 7 below for the related `DeliverPrompt` shape question.

### 7. `DeliverPrompt` / `DeliverInitialPrompt` semantics — fire-and-forget vs status code

`internal/harness/harness.go:42-45` (interface methods),
`internal/harness/opencode/adapter.go:355-442` (HTTP implementation that
returns errors for non-2xx responses) — `[http-only]` semantics inside an
otherwise transport-agnostic interface.

The interface signature looks transport-agnostic:

```go
DeliverInitialPrompt(ctx context.Context, prompt, role string) error
DeliverPrompt(ctx context.Context, prompt string) error
```

But the *meaning* of the returned `error` differs sharply between transports:

- **HTTP+SSE (opencode):** `error == nil` means the server returned a 2xx
  status code, i.e. the prompt was *accepted* (and the assistant turn will
  follow asynchronously via SSE). `error != nil` means the HTTP request
  failed — non-2xx, network error, timeout, marshal failure, etc.
- **JSONL-over-stdio (PI):** writing to the harness's stdin pipe is
  fire-and-forget. The harness either receives the bytes (no acknowledgement
  comes back synchronously) or the pipe is closed (in which case the write
  returns an error from the OS). There is no analogue of HTTP's status code
  — "accepted" is implicit in "the write succeeded".

The interface signature does not currently express this distinction. Callers
of `DeliverInitialPrompt` (`internal/sidecar/sidecar.go:498`) treat
`err == nil` as "prompt is on its way to the agent" and log on error; that
contract will hold for both transports, but the **error frequency, shape,
and retry semantics** differ. A stdio harness should arguably never see a
transient error from `DeliverPrompt` (the pipe is either open or
permanently broken); the opencode adapter sees transient errors all the
time (network blips, opencode restart races).

[uncertain — whether PI's RPC has any synchronous acknowledgement at all (a
JSON ack-line on stdout, for instance) is a PI-specific question. If it
does, the current `error` return value can carry it; if it does not, the
interface contract needs explicit documentation that `nil` means "bytes
written" not "command processed".]

### 8. Container-launch assumptions: TCP-listening process expected

`internal/container/container.go:997` (`portBinding := fmt.Sprintf("127.0.0.1:%d:%d", cfg.AllocatedPort, ContainerPort)`),
`:1115-1116` (`"--publish", portBinding,`),
`:41-45` (`ContainerPort` constant doc: "port opencode serve listens on
inside the container"),
`internal/container/bwrap.go:608-635` (the bwrap argv terminator block:
`"opencode", "--port", fmt.Sprintf("%d", opencodePort), "--hostname", "127.0.0.1"`,
plus the conditional `--prompt` append) — `[http-only]` for the port/publish
plumbing; `[opencode-only]` for the binary name and the `--prompt`/`--port`
flag names (covered already in inventory §7.14).

The container manager's argv builder (`(*Manager).buildRunArgs`) and the
bwrap isolator's argv builder (`(*bwrapIsolator).BuildArgs`) both assume the
agent invocation is a process that:

1. Binds a TCP port on a known interface.
2. Has its port advertised to the host via `--publish` (podman) or no
   publishing (bwrap, where the host is literally the same network namespace).
3. Receives any initial prompt via a CLI flag (`--prompt`) rather than via
   the transport channel.

For a stdio harness the entire `--port`/`--publish`/`--hostname` triplet is
meaningless. The `--prompt` flag may also be meaningless if PI's
initial-prompt convention is "write the first JSONL line to stdin" rather
than "pass `--prompt <text>` on the command line." The `Harness` interface
does have `ContainerCommand() string` (returning the agent invocation
string), so a PI adapter could return `"pi --mode rpc"` and skip
`--port`/`--prompt`/`--hostname`, but the container manager **also** appends
`--publish` based on `cfg.AllocatedPort` being non-zero and **also** wires
in mounted opencode state directories — neither of which it would consult
the harness about today.

**`prism agent-run` (the bwrap/sandbox-exec entry point):**
`cmd/agent_run.go:67-353` — the command exec's bwrap (or, on Darwin,
sandbox-exec), passes the host PTY through, forwards signals, then waits
for the child to exit and exits with the same status. There is **no** code
path here that holds a pipe through to a downstream JSONL consumer. Today
this is fine because the bwrap/sandbox-exec child is opencode, which
listens on a TCP port the sidecar dials separately.

For a stdio harness in a sandbox, `prism agent-run` would need to either
(a) hold the harness's stdin/stdout open and forward JSONL frames back to
the sidecar via some other channel (Unix socket?), or (b) be replaced
entirely by the sidecar launching the harness directly, which is the
"sidecar lifecycle inversion" described in #1074's background ("today the
sidecar is started by the spawn flow as a sibling process that dials a
separately-running HTTP server. For PI the sidecar … is the *parent* of the
harness process"). B.3 owns this question. [uncertain — whether (a) is even
viable in a podman + `podman attach` world, given the user-visible TTY is
already attached to the container's main process; bridging stdin/stdout
JSONL from inside the same container that the user is interacting with via
PTY may require splitting the harness from its TUI in some way that is
PI-specific and not knowable from the prism source.]

## Per-§7-site classification

Tags follow the format `[inventory tag] / [transport-tag]` where the
transport-tag is the new dimension this review introduces.

### §7.1 — `internal/harness/harness.go` (interface)

- `harness.go:23-106` (interface declaration). Methods split as follows:
  - `ContainerCommand() string` — `[multi-harness via interface] / [transport-agnostic]`
    on the surface; the value returned by opencode happens to embed `--port`/`--hostname`,
    but the **interface itself** does not require this. A stdio harness can return `"pi --mode rpc"`.
  - `HealthCheck(ctx, port int) error` — `[multi-harness via interface] / [http-only]`.
    See class 1 above. **The `port` parameter is a transport assumption baked
    into the interface itself.**
  - `ConfigMountPath() string` — `[multi-harness via interface] / [transport-agnostic]`.
  - `DeliverInitialPrompt`, `DeliverPrompt` — `[multi-harness via interface] / [http-only]`
    in semantic shape. See class 7 above.
  - `Subscribe(ctx) (<-chan HarnessEvent, error)` — `[multi-harness via interface] / [transport-agnostic]`.
    The cleanest interface method; opaque event channel is exactly the shape
    a JSONL adapter would also produce.
  - `MapEvent`, `ExtractMessage`, `ExtractEventType` — `[multi-harness via interface] / [transport-agnostic]`.
  - `CreateSession(ctx) (string, error)` — `[multi-harness via interface] / [http-only]`
    in opencode's implementation (it is a `GET /session` round-trip); the
    interface signature itself is agnostic. [uncertain — whether PI has a
    "session" concept that maps cleanly to a session ID at all is a PI-
    specific question; if not, this method's contract needs review.]
  - `SessionID() string` — `[multi-harness via interface] / [transport-agnostic]`.
  - `ConfigEnvVar`, `RuntimeEnv`, `ValidateAgentRole`, `EffectiveModel` —
    `[multi-harness via interface] / [transport-agnostic]`.

Inventory §7.1's classification of the whole file as `[multi-harness via
interface]` stands. The new lens identifies that **two methods**
(`HealthCheck`, `DeliverPrompt`/`DeliverInitialPrompt`) carry HTTP-shape
assumptions inside their signatures or implied semantics, plus `CreateSession`
carries an opencode-shape assumption that may not generalise.

### §7.2 — `internal/sidecar/sidecar.go`

- `sidecar.go:22` (import of `harness`) — `[multi-harness via interface] / [transport-agnostic]`.
- `sidecar.go:119-188` (`Config struct` fields) — split:
  - `OpencodeURL string` (`:123`) — `[opencode-only] / [http-only]`. The
    field name is opencode; the value is a URL.
  - `Container *container.Config` (`:155`) — `[multi-harness via interface] / [transport-agnostic]`
    *as a field*; see §7.14 for the field contents' transport coupling.
  - `HostAPISockPath`, `HostAPITCPPort` (`:159-163`) — `[transport-agnostic]`
    relative to the harness (host-API server is harness-independent — see
    class 5).
  - `OnReady func()` (`:168`) — `[multi-harness via interface] / [http-only]`
    in lifecycle shape (see class 2 / class 4).
  - `InitialPrompt string` (`:175`) — `[multi-harness via interface] / [transport-agnostic]`
    on its own; the *delivery mechanism* (`opencode --prompt` on the CLI in
    container mode) is opencode-shaped (see §7.14, bwrap).
  - `StartupConnectTimeout time.Duration` (`:187`) — `[harness-agnostic] / [http-only]`.
    Doc literally says "the duration the sidecar waits for the first SSE
    event before concluding the harness never bound to its port" — the
    "port" framing is the assumption.
- `sidecar.go:336-509` (`(*Sidecar).Run` container-mode block) —
  `[multi-harness via interface] / [http-only]`. This block is the canonical
  example of an HTTP-shape lifecycle: TCP listener bind → `WaitHealthy` (HTTP
  probe) → `OnReady` (readiness-file write to unblock `podman attach`) →
  `CreateSession` (HTTP GET) → `DeliverInitialPrompt` (no-op in container
  mode, but conceptually an HTTP POST). Five distinct HTTP-shape steps in
  150 lines. See classes 2, 4, and 7 above.
- `sidecar.go:609-704` (the bwrap-mode startup-connect-timeout goroutine) —
  `[harness-agnostic] / [http-only]`. The doc comment is explicit:
  "The harness never bound to its port within the timeout window."
  A stdio harness has no port to bind, but it does have a "process is
  alive but emitting no events" failure mode; the timeout structure may
  port over, but its trigger condition would have to change.
- `sidecar.go:524-585` (host-API listener setup) — `[transport-agnostic]`
  relative to the harness (see class 5).
- `sidecar.go:615` (`s.harness.Subscribe(sseCtx)`) and `:706-708` (event
  loop) — `[multi-harness via interface] / [transport-agnostic]`. The event
  loop is the cleanest harness-agnostic call site in the file.
- `sidecar.go:2611-3700` (host-API handler) — `[transport-agnostic]`
  relative to the harness (see class 5).
- `sidecar.go:1300-1556` (event handlers — inventory's call-out at §7.2)
  — `[multi-harness via interface] / [transport-agnostic]` for the dispatch
  (`s.cfg.Harness.ExtractEventType` / `MapEvent`); `[opencode-only] /
  [transport-agnostic]` for the payload schema (see §7.19).

### §7.3 — `cmd/sidecar.go`

- `cmd/sidecar.go:95` (`--port` flag declaration), `:198-199` (validation
  in podman mode), `:215` (`AllocatedPort: port`), `:239-241` (validation in
  bwrap/sandbox-exec) — `[opencode-only] / [http-only]`. See class 3.
- `cmd/sidecar.go:81-82` (cobra long-doc: "podman attach to bridge the
  container PTY (RFC #691)") — `[harness-agnostic] / [http-only]` in lifecycle
  shape (the attach-to-PTY pattern is HTTP-shape-adjacent — see class 4).
- `cmd/sidecar.go:256-280` (`onReady` closure writing `sidecar.ready`) —
  `[harness-agnostic] / [http-only]`. See class 4.
- `cmd/sidecar.go:282-321` (concrete adapter construction; sidecar.Config
  assembly) — `[opencode-only] / [http-only]` (the `OpencodeURL` field is
  populated; the `opencodeURL` local is built from `port`).

### §7.4 — `cmd/spawn.go`

- `spawn.go:148` (`--harness` flag declaration with `opencode` default and
  allow-list) — `[opencode-only] / [transport-agnostic]`. The flag plumbing
  itself is transport-agnostic; the allow-list is opencode-only. (Inventory
  §7.4 already covers the allow-list angle.)

### §7.5 — `cmd/spawn_harness_test.go`

Skipped by the inventory. Skipped here for the same reason — tests do not
themselves carry runtime transport assumptions.

### §7.6 — `cmd/review.go`

- `cmd/review.go` — passes `harness` through to spawned review sessions —
  `[multi-harness via interface] / [transport-agnostic]`. The review pipeline
  spawns review-agent sessions; the spawn itself is transport-agnostic. The
  bus-delivery hop back to the parent goes via the host-API (class 5,
  transport-agnostic relative to harness) and the parent's harness
  `DeliverPrompt` (class 7). The review pipeline itself does not introduce
  new HTTP-shape assumptions beyond those already classified.
  [uncertain — inventory §7.6 already flagged this as needing a fuller trace
  pass; that limitation carries forward to this review.]

### §7.7 — `cmd/agent_run.go`

- `cmd/agent_run.go:67-353` (whole `runAgentRun`, including the bwrap
  process-supervisor PTY/signal handling) — `[opencode-only] / [http-only]`
  in lifecycle shape. The command exec's the sandbox child, supervises it
  for PTY/signal handling, and exits with its status. See class 8: there is
  no machinery here for piping stdin/stdout JSONL through to the sidecar.
- `cmd/agent_run.go:159-164` (port resolution from `status.HarnessPort` and
  population of `ctrCfg.AllocatedPort`) — `[opencode-only] / [http-only]`.
- `cmd/agent_run.go:189-205` (harness adapter constructed inline as
  `opencode.New("", nil, "", "")` purely to extract `RuntimeEnv()` for the
  container config) — `[opencode-only] / [transport-agnostic]`. The
  `RuntimeEnv()` call itself is harness-agnostic; the choice to instantiate
  opencode is hard-coded.
- `cmd/agent_run.go:207-212` (initial prompt env-var read; inventory §7.7's
  call-out about `--prompt` on the opencode invocation) — `[opencode-only] /
  [http-only]` in delivery shape (the `--prompt` flag is the
  "fire-and-forget over CLI" hack that lets opencode start its session
  before the sidecar has had a chance to call `DeliverInitialPrompt`).

### §7.8 — `cmd/restore.go`

- `restore.go:269-345` (restore path) — `[multi-harness via interface] /
  [http-only]`. Plumbs `harness` and `harness_session_id` through
  `session.SpawnOpts` (transport-agnostic in shape), but the restore also
  re-allocates a port and re-issues the `--port`/sidecar/host-API setup
  (class 3, http-only).

### §7.9 — `cmd/event.go`

- `cmd/event.go:138, :222, :347, :447, :498, :551, :609` (event type
  literals) — `[opencode-only] / [transport-agnostic]`. The event names
  match opencode's plugin output; the transport is unrelated.

### §7.10 — `cmd/checkin.go`, `cmd/stats.go`, `cmd/prompt.go`, `cmd/list_sessions.go`, `cmd/sessions.go`

- All `[opencode-only] / [transport-agnostic]`. These commands read events
  from `agent_events` (the schema is opencode-shaped, but transport-
  agnostic) and dispatch to other sessions (via the bus / host-API, which
  are themselves transport-agnostic relative to the harness — class 5).
- `cmd/prompt.go` specifically — already classified by inventory §7.23 as
  `[multi-harness via interface]`. The CLI is harness-agnostic; the
  underlying delivery (host-API → sidecar → harness `DeliverPrompt`) hits
  class 7's HTTP-shape semantic ambiguity but is **structurally** transport-
  agnostic.

### §7.11 — `internal/db/db.go`

- `db.go:169` (`harness TEXT NOT NULL DEFAULT 'opencode'`) — `[opencode-only]
  (default) / [transport-agnostic]`.
- `db.go:218-225` (migration v7→v8 added `harness`, `harness_session_id`,
  `harness_port`) — `[multi-harness via interface] / [http-only]`. The
  `harness_port` column is a transport assumption — a stdio harness has no
  port to record.
- `db.go:246-250` (migration v14→v15 renamed `opencode_sid` to
  `harness_session_id`) — `[opencode-only] (legacy schema) / [transport-agnostic]`.
- `db.go:252` (migration v15→v16 sessions table) — `[multi-harness via
  interface] / [transport-agnostic]`.

### §7.12 — `internal/session/session.go`, `internal/session/spawn.go`, `internal/session/sidecar.go`

- `session/session.go:73` (comment naming `opencode --agent <name> --port
  <n>`) — `[opencode-only] / [http-only]` (mentions `--port`).
- `session/session.go` (`Opts.Harness`, `Opts.HarnessConfigEnvVarName`,
  `Opts.RuntimeEnvVars`) — `[multi-harness via interface] / [transport-agnostic]`
  in plumbing; `[opencode-only]` in concrete construction (inventory's
  classification stands).
- `session/sidecar.go:211-358` (`StartSidecarOpts`, `StartSidecar`,
  `StartSidecarWithOpts`) — see class 3. The `Port int` field at `:213`,
  the `--port` argv assembly at `:321` and `:338`, and the URL construction
  at `:301` are all `[http-only]`.
- `session/spawn.go` (`SpawnOpts.Harness` plumbing) — `[multi-harness via
  interface] / [transport-agnostic]`.

### §7.13 — `internal/dashboard/sessions.go` and `view.go`

- `[opencode-only] / [transport-agnostic]`. Display layer; no transport
  assumptions beyond the names it shows.

### §7.14 — `internal/container/container.go`, `internal/container/bwrap.go`

- `container/container.go:41-45` (`ContainerPort` constant) — `[opencode-only]
  / [http-only]`.
- `container/container.go:997` (`portBinding`), `:1115-1116` (`--publish`)
  — `[multi-harness via interface] (in code; consults `cfg.AllocatedPort`)
  / [http-only]` (the publishing concept assumes a TCP port). See class 8.
- `container/container.go:1283-1288` (host-API TCP env injection
  `PRISM_HOST_API=http://host.containers.internal:%d`) — `[transport-agnostic]`
  relative to the harness; this is class 5's host-API channel, which is
  HTTP for unrelated reasons.
- `container/container.go` (opencode state-dir mounts at `:1015-1037`,
  auth.json overlay at `:1149-1151`, opencode plugin/cache mounts at
  `:1033-1037`, etc.) — `[opencode-only] / [transport-agnostic]`. Storage
  layout, not transport.
- `container/bwrap.go:299-441` (opencode storage / config / cache mounts)
  — `[opencode-only] / [transport-agnostic]`. Storage layout.
- `container/bwrap.go:608-635` (the bwrap argv terminator: `"opencode",
  "--port", fmt.Sprintf("%d", opencodePort), "--hostname", "127.0.0.1"`,
  with `--prompt` conditionally appended) — `[opencode-only] / [http-only]`.
  See class 8.
- `container/sandbox_exec.go:147-172` (`args := []string{"sandbox-exec",
  "-f", profilePath, "opencode"}`) — `[opencode-only] / [http-only]`. The
  binary is hard-coded; sandbox-exec inherits the same `--port`/`--hostname`/
  `--prompt` shape as bwrap.

### §7.15 — `internal/archive/version.go`

- `[opencode-only] / [transport-agnostic]`. `exec.Command("opencode",
  "--version")` is opencode-only; it does not touch transport.

### §7.16 — `internal/archive/archive.go`

- `[opencode-only] / [transport-agnostic]`. Archive layout copies opencode's
  on-disk storage; transport-independent. (B.6 owns the harness-aware
  redesign.)

### §7.17 — `internal/piexport/piexport.go`

- `[opencode-only] / [transport-agnostic]`. Translation from opencode raw
  archive shape to pi-mono format; pure on-disk transformation, no transport.

### §7.18 — `internal/opencode/session.go`

- `[opencode-only] / [transport-agnostic]`. `LatestSessionForDir` scans
  opencode's storage tree; transport-independent.

### §7.19 — `internal/payload/payload.go`

- `payload.go:1-13` (package doc: "the plugin (TypeScript) marshals event
  payloads to match these struct field names exactly") — `[opencode-only] /
  [transport-agnostic]`. Schema coupling is to opencode's plugin output, not
  to HTTP. (B.5 owns the schema-decoupling proposal.)

### §7.20 — `internal/review/review.go`, `internal/review/monitor.go`, `cmd/review.go`

- `[multi-harness via interface] / [transport-agnostic]` for the dispatch
  (bus messages via `internal/db.WriteBusMessage`, host-API hops);
  `[opencode-only]` for the assumption that the parent (worker /
  coordinator) is opencode (inventory's existing classification). Transport-
  agnostic relative to the harness because the bus and host-API are
  harness-independent. [uncertain — see inventory §7.6 / §7.20: a fuller
  grep pass on review.go to confirm no `--port` / opencode-URL references
  outside what is already captured was not done in this review.]

### §7.21 — `internal/mergequeue/watcher.go`

- `[opencode-only] (in description) / [transport-agnostic]` (in code,
  consistent with inventory §7.21). Merge-completion notifications go via
  the prism bus / host-API; the path is harness-independent.

### §7.22 — `cmd/clipboard.go`

- `[opencode-only] / [transport-agnostic]`. Clipboard staging directory
  layout matches opencode's drag-drop expectations; no transport assumption.

### §7.23 — `cmd/prompt.go`

- `[multi-harness via interface] / [transport-agnostic]` (CLI plumbing) but
  hits class 7 (`DeliverPrompt` semantics) at the harness boundary.
  Inventory's classification stands for the CLI; the semantic hand-off is
  the new concern.

### §7.24 — Summary of harness-coupling shape

This subsection in the inventory is itself a recap. The transport-shape
re-classification of every site mentioned here is captured in the per-site
listings above; no new sites are introduced.

## Sites that "live in the wrong place if the harness uses pipes instead of ports"

Drawn from the eight classes and the per-§7-site listings, the concrete
"would not exist or would have to move" sites are:

1. **`Harness.HealthCheck(ctx, port int)` signature**
   (`internal/harness/harness.go:32`) — the `port` parameter has no
   meaning for stdio.
2. **The entire `--port` plumbing chain** — `cmd/sidecar.go:95, :198-199,
   :215, :239-241`; `internal/session/sidecar.go:213, :301, :321, :338`;
   `cmd/agent_run.go:159-164`; `internal/container/container.go:997,
   :1115-1116`; `internal/container/bwrap.go:621-627`;
   `internal/db/db.go` `harness_port` column (added in v7→v8).
3. **`OpencodeURL` field on `sidecar.Config`** (`internal/sidecar/sidecar.go:123`).
   A stdio harness has no URL.
4. **The `WaitHealthy` → `OnReady` → readiness-file → `podman attach`
   coordination** (`internal/sidecar/sidecar.go:412-509` + `cmd/sidecar.go:256-280`).
   For a stdio harness the sidecar is the launcher; the pane has no
   separately-running process to wait for.
5. **The bwrap-mode "startup-connect" timeout that watches for a port
   binding** (`internal/sidecar/sidecar.go:609-704`). The timeout
   *concept* may survive (with a different trigger condition); the
   "harness never bound to its port" framing does not.
6. **`prism agent-run`'s walk-away-after-exec model**
   (`cmd/agent_run.go:67-353`). For stdio, either the sidecar replaces
   `agent-run` as the launcher (B.3 option a) or `agent-run` itself takes
   on sidecar duties (B.3 option b); either way today's "exec the sandbox
   binary and wait for exit" loses the JSONL pipe.
7. **The `ContainerCommand()` return value coupled with podman's
   `--publish` / bwrap's `--port`/`--hostname` argv terminators**
   (`internal/container/container.go:1115-1116`,
   `internal/container/bwrap.go:608-635`). The container manager appends
   port-publishing flags based on `cfg.AllocatedPort`, not on the harness's
   transport shape.
8. **Sidecar lifecycle inversion** (the conceptual one, not a code site).
   Today the sidecar is a sibling process of the harness; the spawn flow
   starts both and they meet over HTTP. For stdio the sidecar must own the
   harness as a child process so it can hold the pipe. This crosses code in
   `cmd/spawn.go`, `internal/session/sidecar.go:StartSidecarWithOpts`,
   `cmd/sidecar.go`, and `internal/sidecar/sidecar.go:Run`. B.3 owns this.

## Sites that survive a transport switch unchanged

For balance — these are the parts of the harness plumbing that already work
for any transport:

- The `Subscribe(ctx) (<-chan HarnessEvent, error)` interface method and
  the sidecar's event-loop consumption of it
  (`internal/sidecar/sidecar.go:615, :706-708`).
- `MapEvent`, `ExtractEventType`, `ExtractMessage` on the interface.
- The `harness` and `harness_session_id` DB columns (the latter, post-
  migration v14→v15).
- The host-API HTTP server (class 5) — independent of harness transport.
- The bus message path (cross-session prompt delivery via DB).
- The `--harness` CLI flag plumbing (`cmd/spawn.go:148`,
  `internal/session/spawn.go`, `internal/session/sidecar.go`'s
  `--harness <name>` argv assembly).
- `Harness.ConfigEnvVar`, `RuntimeEnv`, `ValidateAgentRole`,
  `EffectiveModel`, `ConfigMountPath` — all transport-agnostic on signature
  and semantics.

## Open questions and `[uncertain]` flags

Consolidated from the per-section flags above:

- **Class 4 (readiness file):** does PI's TUI need a separate readiness
  signal for `podman attach` even when transport is stdio? Cannot determine
  without prototyping PI inside a podman container. (B.3 input.)
- **Class 7 (`DeliverPrompt` semantics):** does PI's RPC have a synchronous
  acknowledgement on stdout, and if so what shape? Cannot determine without
  the PI source / RFC #606 spec specifics. (B.4 / B.5 input.)
- **Class 8 (`prism agent-run` for stdio):** is it viable to keep
  `agent-run` as the bwrap-equivalent process supervisor and bridge JSONL
  back to the sidecar via Unix socket inside the same container the user is
  attached to via PTY? Or does the stdio model require splitting harness
  process from TUI process inside the container? Cannot determine without
  PI prototyping. (B.3 input.)
- **§7.1 `CreateSession`:** does PI have a "session" concept that maps to a
  single string ID, or is its lifecycle different (e.g. one process == one
  session, no session-creation step)? Cannot determine without RFC #606 detail.
- **§7.6 / §7.20 (review pipeline trace):** inventory flagged §7.6 as not
  exhaustively traced; that limitation carries forward — there may be
  transport-shape assumptions buried in `internal/review/review.go` that
  this review missed. Mitigation: the bus and host-API paths are
  transport-agnostic, so any missed assumption is most likely an
  opencode-name reference rather than a transport-shape one.
- **§7.21 (mergequeue watcher):** the watcher is started inside the sidecar
  for coordinator sessions (`internal/sidecar/sidecar.go:587-607`); whether
  any of its delivery hops carry HTTP-shape assumptions beyond what the
  bus/host-API path already captures was not separately traced.

## What this review deliberately does not do

- It does **not** propose a registry shape — that is B.2.
- It does **not** propose a sidecar-as-launcher refactor — that is B.3.
- It does **not** propose a harness-group taxonomy (HTTPHarness vs
  StdioHarness shared base) — that is B.4.
- It does **not** propose a payload schema — that is B.5.
- It does **not** propose an archive-pipeline refactor — that is B.6.

The classification above is the input those issues consume. Where this
review carries `[uncertain]` flags, those are unresolved questions the
respective B.x issues will need to answer (most likely by prototyping
against PI per RFC #606 / RFC #691).

## Related

- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md) §7.
- Design: [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: #1074. Parent design: #1072.
- RFCs: #691 (multi-harness support), #606 (PI coding agent support).
- Sibling Track B issues: #1080 (B.2), #1081 (B.3), #1082 (B.4), #1083
  (B.5), #1084 (B.6).
