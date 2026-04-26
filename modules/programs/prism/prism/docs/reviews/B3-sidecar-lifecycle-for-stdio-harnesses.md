# B.3 — Sidecar lifecycle review for stdio-pipe harnesses

Track B (harness) Wave 2 follow-up to [B.1](B1-harness-transport-and-lifecycle-assumptions.md).
Parent design: [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md)
(issue #1072). This document is a **proposal** — it carries opinions, weighs
trade-offs, and recommends a direction. It does **not** ship code.

## Context recap

B.1 catalogued every harness coupling site by transport shape (`[http-only]` vs
`[transport-agnostic]`). It identified eight HTTP-shape assumption classes and
named the **sidecar lifecycle inversion** (its eighth "wrong place" site) as
the deliverable B.3 owns:

> Today the sidecar is a sibling process of the harness; the spawn flow starts
> both and they meet over HTTP. For stdio the sidecar must own the harness as
> a child process so it can hold the pipe.
> — B.1, "Sites that 'live in the wrong place if the harness uses pipes
> instead of ports'", item 8.

The relevant inventory anchors:

- §9 (per-spawn data flow) — `prism spawn` → `session.SpawnSession` →
  `setupFullLayout` → `StartSidecarWithOpts` → `exec.Command(self,
  "sidecar", …)` → sidecar dials harness on its allocated port.
- §10.1 (lifecycle map for `prism spawn`) — the side-effect catalogue this
  review uses as its baseline column.
- §11 (external boundaries) — particularly §11.5 (`opencode` binary, launched
  inside container/bwrap/sandbox-exec or directly in the host pane), §11.10
  (host-API per-session Unix socket), and §11.13 (allocated session port).
- RFC #691 §3 (multi-harness):
  > the sidecar holds the stdin/stdout pipe to the PI container process and
  > consumes the JSON Lines event stream directly. The user still sees the PI
  > TUI via `podman attach` on a PTY — the RPC pipe is a separate file
  > descriptor.

The structural change RFC #691 §3 implies for stdio: **whoever owns the JSONL
pipe must be the parent of the harness process.** Today nothing in prism is
the parent of opencode in the way that PI demands of its sidecar. In podman
mode opencode is a container PID 1 reachable via `podman attach`; in
bwrap/sandbox-exec mode opencode is a great-grandchild of the tmux pane (pane
shell → `prism agent-run` → bwrap → opencode); in host mode opencode is a
direct child of the tmux pane shell. The sidecar reaches each of those by
**dialling**, never by spawning. That has to change for any harness whose
event stream is on a stdin/stdout pipe rather than on a TCP socket.

This review identifies two viable shapes for that change, walks them through
each isolation mode, and recommends one — with the explicit acknowledgement
that several mode/shape cells need prototyping against a real PI binary
before the recommendation can be committed to code.

## The two shapes

### Shape A — sidecar-as-parent

The sidecar binary is the launcher of the harness process. `prism agent-run`
either disappears for stdio harnesses or becomes a thin re-exec into the
sidecar. The process tree for a bwrap-isolated PI session is:

```
tmux pane (sh)
  └── prism sidecar --session … --harness pi --isolation bwrap
        │   (holds harness stdin/stdout pipe; consumes JSONL events)
        └── bwrap --bind … --setenv … pi --mode rpc
              └── pi (PID 1 inside the sandbox)
                    │   stdin: pipe from sidecar
                    │   stdout: pipe to sidecar (JSONL events)
                    │   stderr: pipe to sidecar (logs)
                    └── (PI's own TUI bridging machinery — see below)
```

For podman-isolated PI the picture is similar but with `podman run` instead of
bwrap, and the pipe wiring crosses the container boundary via podman's
`--interactive --attach=stdin --attach=stdout` (or equivalent). The sidecar
binds the host-API Unix socket and the per-spawn DB row exactly as today; the
only new responsibility is owning the harness's pipe. The sidecar is no longer
"the SSE consumer" — it is "the JSONL consumer" — and the lifecycle of the
harness ends when the sidecar's child reaper sees it exit, rather than when
the sidecar's SSE stream returns EOF. `WaitHealthy` is replaced by "spawn the
child and wait until the first JSONL frame arrives, or until the child
process exits." `OnReady` (the readiness file) fires when that first frame
arrives. SIGTERM cascade: the sidecar's signal handler kills its harness
child first, then closes the host-API socket, then exits. Restore
(`prism restore`) re-spawns the sidecar, which re-spawns the harness — the
recovery shape is unchanged, only the inner layer of the cascade is new.

### Shape B — agent-run-as-sidecar-host

`prism agent-run` keeps its position as the tmux-pane-owned launcher of the
sandbox, but it grows the sidecar's responsibilities (SSE/JSONL consumption,
state-machine writes, host-API server, merge-queue watcher, etc.) into the
same process. The standalone `prism sidecar` process is not used for stdio
harnesses. The process tree for a bwrap-isolated PI session is:

```
tmux pane (sh)
  └── prism agent-run --session …
        │   (now also: holds JSONL pipe, runs host-API server,
        │    writes agent_status, runs merge-queue watcher)
        └── bwrap --bind … --setenv … pi --mode rpc
              └── pi (PID 1 inside the sandbox)
                    │   stdin/stdout/stderr piped back to agent-run
                    └── (TUI bridging — see below)
```

For podman-isolated PI under Shape B, `prism agent-run` would have to be
introduced for podman too (it does not exist on the podman path today),
because the tmux pane would need to launch *something* prism-aware that owns
the pipe rather than running `podman attach` directly. That makes Shape B
strictly broader in surface area than Shape A on the podman path. For host
mode under Shape B the host-mode launch script (`buildDirectOpencodeCmd`)
would either be replaced by a `prism agent-run --session … --isolation host`
invocation, or host mode would keep its current shape and run a separate
sidecar — splitting host mode away from the unified Shape B story. For
sandbox-exec the change is the smallest because `prism agent-run` already
exists there and uses `syscall.Exec`; Shape B turns the `syscall.Exec` into
an `exec.Cmd.Start` + pipe-read loop and folds the sidecar in. Lifetime of
the new agent-run-host process is the lifetime of the tmux pane: when the
pane is killed, SIGHUP propagates to agent-run, which kills the harness
child, closes the host-API socket, and exits. Restore is the same as today
plus killing the previous in-pane host (the sidecar role is now embodied in
the pane process, not a detached process).

## Per-shape side-effects

All four isolation modes plus the cross-cutting concerns. Cells use:

- ✓ — works with no change beyond the shape's own structural change.
- ✗ — needs adaptation; severity noted.
- `[uncertain]` — cannot be predicted from prism source alone; needs
  prototyping against a real PI binary or a representative stdio harness.

| Concern | Shape A (sidecar-as-parent) | Shape B (agent-run-as-sidecar-host) |
|---|---|---|
| **Podman path** | ✗ Sidecar must launch the container with stdin/stdout attached (e.g. `podman run -i -a stdin -a stdout`). The current `internal/sidecar/sidecar.go:404 mgr.Create(ctx)` (which uses `podman run -d` detached, then `podman healthcheck`) becomes a non-detached launch with pipe wiring. `WaitHealthy` is replaced by "first frame seen on stdout pipe". The `--publish` / TCP listener wiring (B.1 class 3) is dropped for stdio harnesses but stays for HTTP harnesses — the choice is per-harness, not per-mode. | ✗ A `prism agent-run` shape must be introduced for podman (it has no agent-run today). The tmux pane would run `prism agent-run --session …` instead of `podman attach …`, and agent-run would itself fork `podman run -i -a stdin -a stdout`. This is more code movement than Shape A on the podman path because the existing launcher (`podman attach`) and the existing supervisor (`prism sidecar`) both need to be reshaped. |
| **Bwrap path** | ✗ The bwrap argv terminator (`internal/container/bwrap.go:608-635`, the `opencode --port … --hostname …` block) is replaced for stdio harnesses by `pi --mode rpc` (or whatever the harness's stdio invocation is). The sidecar process forks bwrap directly via `exec.Cmd` with stdin/stdout/stderr connected to the parent. `cmd/agent_run.go` is no longer in the chain for stdio — the tmux pane runs `prism sidecar` instead of `prism agent-run`. The PTY-bridging dance in `cmd/agent_run.go:264-353` (`tcsetpgrpForeground`, `forwardSignalsToBwrap`, the stderr-tee goroutine) is **lost**, which is consequential for the user-visible TUI — see TUI bridging row. | ✓ Smallest delta on the bwrap path of any mode/shape pair. `prism agent-run` is already the launcher; today it `exec.Cmd.Start`s bwrap with stdin/stdout from the host PTY. Under Shape B it instead pipes bwrap's stdin/stdout, runs the JSONL-consumption loop in a goroutine, and bridges the harness's TUI fds back to the PTY (mechanism depends on the harness — see TUI bridging row). The host-API server, state-machine, and merge-queue watcher all move into agent-run. |
| **Sandbox-exec path** | ✗ Same shape as bwrap — sidecar forks `sandbox-exec` directly. `cmd/agent_run.go:516-616` `runAgentRunSandboxExec` is bypassed for stdio harnesses. The `syscall.Exec` model that sandbox-exec uses today (`cmd/agent_run.go:611`) is incompatible with "this process must keep holding pipes" — Shape A inherently means the supervising process is the sidecar, which uses `exec.Cmd.Start` not `syscall.Exec`, and that re-introduces the PTY/signal-forwarding complexity that bwrap's path already absorbed (#1018). | ✗ `runAgentRunSandboxExec`'s `syscall.Exec` model would have to become `exec.Cmd.Start` + supervised child, identically to what the bwrap path already does today. This is a known direction for sandbox-exec parity (#1018) and Shape B accelerates it whether or not stdio harnesses are added. The mechanical work is bounded; the lifecycle complexity is not new. |
| **Host path** | ✗ Today the host-mode tmux pane runs `opencode` directly as a child of the pane shell (`buildDirectOpencodeCmd`); the sidecar dials it. Under Shape A for stdio the sidecar would have to be the launcher of the harness, which means the host-mode tmux pane runs `prism sidecar` instead of `opencode` — exactly inverting host-mode's current "no prism wrapper, just the harness" simplicity. The user-visible TTY moves from "opencode talking to my terminal" to "sidecar's child harness talking through pipes that the sidecar then re-presents to my terminal" — a non-trivial reshaping of host mode. | ✗ Equivalent reshaping: the tmux pane runs `prism agent-run --session … --isolation host` instead of `opencode` directly. Slightly cleaner than Shape A in that there is one process visible to the user (`agent-run` rather than the deeper Shape A tree), but the same fundamental change to host mode. |
| **Cleanup** | ✓ The cleanup path (`cmd/cleanup.go` → `killSidecar` → `removeContainerIfExists`) keeps its existing two-phase shape. SIGTERM to the sidecar PID propagates to the harness child via the OS process group; the container removal step is redundant for stdio (the harness exited when its parent did) but keeping it is harmless. The `agent_status.ended_at` write and bus-message purge happen in the sidecar's signal handler exactly as today. | ✗ `killSidecar` no longer has a target PID for stdio sessions because the "sidecar" lives inside the tmux pane process. Cleanup must instead `tmux kill-session` (which sends SIGHUP to the pane shell and cascades to agent-run → harness) and then run the DB writes from the cleanup CLI itself. The signal-handler-driven `agent_status.ended_at` write becomes harder to keep — the pane process may get SIGKILL before its handler runs. [uncertain — whether tmux's pane-died hook fires reliably enough to substitute for the sidecar's signal handler in this role. Today the hook is used as a back-stop, not the primary path; making it primary is a behaviour change that needs prototyping.] |
| **Restore** | ✓ `cmd/restore.go` already kills the stale sidecar PID (`killSidecar`) and re-spawns a fresh `prism sidecar` via `StartSidecarWithOpts`. Under Shape A the re-spawned sidecar re-launches its harness child — recovery is identical to today's shape from the orchestrator's perspective. | ✗ Restore must re-create the tmux pane and run `prism agent-run` inside it, *and* the host-API socket and state-machine writes only come back online once that pane process is alive. The detached-sidecar shape today means the host-API is back within ~1s of restore even before the agent pane attaches; under Shape B there is a window where the session has no host-API server even though restore returned success. Probably acceptable, but a behaviour shift. |
| **Host-API server** | ✓ Lives in the sidecar; the sidecar still binds the per-session Unix socket and (on Darwin) the TCP listener for `host.containers.internal` reachability. No change beyond what is already in `internal/sidecar/sidecar.go:524-585`. | ✗ Lives in `prism agent-run` instead. The Unix socket is bound by the pane process, which means the socket disappears when the pane dies — same reachability as today (the container is also dead at that point) but a different process owns the binding. Darwin TCP-listener-before-`mgr.Create` (`internal/sidecar/sidecar.go:349-375`) needs to move into agent-run too. |
| **TUI bridging** | ✗✗ **The hardest cell.** Today the user sees the harness's TUI via `podman attach` (podman) or via the pane shell directly running opencode (bwrap/sandbox-exec/host). Under Shape A the sidecar holds the harness's stdin/stdout for JSONL — those fds are unavailable for the user's PTY. The sidecar must bridge the TUI on a *separate* fd-set: for podman, RFC #691 §3 says the user attaches via `podman attach` which connects to PI's PTY (a different fd from the JSONL pipe); for bwrap/sandbox-exec/host there is no `podman attach` analogue and the bridge mechanism is **harness-specific** (PI must offer some out-of-band TUI access — a separate TTY device, an ANSI stream on a third fd, a tmux/screen session inside the sandbox, etc.). [uncertain — whether PI's TUI/RPC fd separation in non-podman modes is solved at the harness level or the prism level. RFC #691 §3 only addresses the podman case explicitly. Without prototyping PI in bwrap, neither shape can confirm the bridge works for non-podman.] | ✗✗ Same fundamental problem. Shape B's `prism agent-run` holds the JSONL pipe, so the user's PTY in the tmux pane cannot also be the harness's TUI. The bridging machinery has to live somewhere — either inside agent-run (which then has to multiplex its own TTY between "bytes destined for the user" and "JSONL frames destined for the state machine") or in the harness itself. Shape B's only advantage here is that the bridge logic is co-located with the user's PTY, not in a detached sidecar — which makes terminal-resize forwarding (SIGWINCH) more natural. [uncertain — same prototyping requirement as Shape A.] |
| **`podman attach` for the user-visible TUI** | ✓ for podman if the harness is podman-only or follows RFC #691 §3's "JSONL on stdin/stdout, TUI on the container's PTY" split. The sidecar uses `podman run -i -a stdin -a stdout` (or starts the container detached and uses `podman exec` for the JSONL fds — see Open question 1) for the JSONL pipe; the tmux pane keeps its current `podman attach --sig-proxy=false <name>` for the TUI. The two fd-sets are independent inside the container. ✗ for bwrap/sandbox-exec/host: there is no `podman attach` and no equivalent — the question is moot; see TUI bridging row. | ✓ for podman, with a caveat: under Shape B the tmux pane runs `prism agent-run` rather than `podman attach`, and agent-run is the one running `podman attach` internally (or splitting fds inside the container as Shape A does). The result the user sees is the same. ✗ for bwrap/sandbox-exec/host as in Shape A. |

### Reading the "TUI bridging" row carefully

For HTTP-shape harnesses (opencode today, presumably Claude Code) **neither
shape changes anything** — the harness is a separate listening process; the
sidecar dials it; the user's TTY is wired to the harness's own PTY by
whichever launcher is appropriate (`podman attach` for podman, the pane shell
for bwrap/sandbox-exec/host). The HTTP-shape harness never had its
stdin/stdout owned by anyone, so neither does it lose anything when the
sidecar grows the ability to own a stdio harness's pipe.

The TUI-bridging cell is hard **only for stdio harnesses**, and only because
the same physical fd cannot simultaneously be (a) a user-visible PTY
delivering escape sequences for the TUI and (b) a JSONL pipe delivering
events for the state machine. The two roles must live on different fds;
prism cannot solve that without harness cooperation. RFC #691 §3 says PI
solves it by giving the JSONL pipe and the user-visible PTY separate fds
inside the container — for podman the user attaches to the container's PTY
(`podman attach`) while the sidecar's `podman exec` (or its `podman run -i`
fds) hold the JSONL pipe. For non-podman modes the same fd-separation has
to exist somewhere. **This is a constraint on PI, not on the choice of
Shape A vs Shape B.** Both shapes succeed or fail on this constraint
together.

## The `podman attach` question, addressed explicitly

Per the AC: "the document explicitly addresses whether `podman attach` for
the user-visible TUI continues to work in each shape."

- **Shape A, podman, HTTP harness (opencode):** ✓ Unchanged. The sidecar
  still dials `http://127.0.0.1:<port>/global/health`; the tmux pane still
  runs `podman attach --sig-proxy=false <container>` (after the readiness
  wait). Nothing about Shape A's "sidecar can also be a parent" capability
  alters the HTTP path.
- **Shape A, podman, stdio harness (PI):** ✓ if PI's
  TUI/RPC-fd separation works as RFC #691 §3 describes — the JSONL pipe is
  on the container's stdin/stdout (held by the sidecar, e.g. via `podman
  exec -i`); the user's TUI is on the container's main PTY (held by
  `podman attach` from the tmux pane). The sidecar must launch the
  container in a way that leaves the main PTY available for `podman
  attach` — meaning detached startup, then `podman exec` for JSONL — rather
  than `podman run -i -a stdin -a stdout` which would consume the main PTY
  for the JSONL fds and leave nothing for `podman attach`. Open question 1
  in §"Open questions" below.
- **Shape A, bwrap/sandbox-exec/host, stdio harness:** there is no `podman
  attach`. The question is moot for these modes — the user-visible TUI
  reaches the tmux pane via whatever mechanism the harness offers (RFC
  #691 §3 is silent on non-podman modes, so this is per-harness and
  unknowable from prism source alone — `[uncertain]`). For the HTTP
  harness on bwrap/sandbox-exec/host the answer is also unchanged from
  today: the pane shell runs `prism agent-run` (or `opencode` directly for
  host) and the harness's TUI takes over the pane PTY, exactly as today.
- **Shape B, podman, HTTP harness:** ✓. The tmux pane runs `prism
  agent-run`, which itself runs `podman attach` for the TUI and dials the
  HTTP endpoint for events. Same end-user behaviour; one extra prism
  process in the chain.
- **Shape B, podman, stdio harness:** ✓ if `prism agent-run` can multiplex
  the user's PTY with its own JSONL-consumption loop. Mechanically
  feasible (agent-run already has signal/PTY plumbing in
  `cmd/agent_run.go:264-353`), but requires the same harness-side fd
  separation as Shape A. Open question 1 applies identically.
- **Shape B, bwrap/sandbox-exec/host, stdio harness:** moot — no `podman
  attach`. Same `[uncertain]` flag as Shape A for those modes.

**Summary for the hard constraint** ("the user-visible TUI must continue to
work"): both shapes preserve `podman attach` for HTTP harnesses on the
podman path with no changes; both shapes preserve it for stdio harnesses on
the podman path *if* PI's fd separation matches RFC #691 §3; both shapes
share the same `[uncertain]` flag for stdio-on-non-podman because that
question is upstream of the shape choice.

## Recommendation: Shape A, with two staged steps

Recommend **Shape A (sidecar-as-parent)** for stdio harnesses. The reasoning,
in order of weight:

1. **Smaller blast radius on existing modes.** Shape A leaves
   `cmd/agent_run.go`'s today-shape intact for all HTTP-harness paths
   (which is every active path today). Shape B forces a substantial
   refactor of `agent_run.go` even for HTTP harnesses (folding the sidecar
   into it) before any stdio benefit is realised, and it forces the
   introduction of an agent-run on the podman path where none exists
   today. Shape A's structural changes are concentrated at the
   sidecar-spawn interface (one well-defined seam: `StartSidecarWithOpts`
   + `internal/sidecar/sidecar.go:Run` startup block) rather than spread
   across two seams.
2. **Cleanup, restore, and host-API stay where they are.** Shape A
   preserves the detached-sidecar shape that cleanup, restore, and the
   host-API server are all built around. Shape B's "sidecar lives inside
   the pane" model creates new failure modes for cleanup
   (`tmux kill-session` → SIGKILL race against the state-machine write)
   and a window during restore where the host-API is unavailable.
3. **The merge-queue watcher belongs in a long-lived process.** The
   coordinator's merge-queue watcher
   (`internal/sidecar/sidecar.go:587-607`) polls on a 45s ticker and
   drives PRs through a multi-minute merge lifecycle. Embedding it in a
   pane process whose lifetime is bounded by the user's tmux session
   (Shape B) makes coordinator-restart-during-merge a more frequent
   event, where today the sidecar survives across pane attaches and
   detaches.
4. **Shape A is symmetrical between HTTP and stdio.** The sidecar already
   *is* the long-lived process; the only change is that it can also be a
   parent (not just a dialler). Shape B asks for a deeper structural
   shift: the sidecar role is moved into a different process that
   currently does something else (`agent_run` is a launcher, not a
   long-lived consumer).
5. **Sandbox-exec parity work (#1018) is helped, not hurt.** Both shapes
   require sandbox-exec to move from `syscall.Exec` to a supervised
   child, but Shape A's locus of supervision (the sidecar) is one
   already-supervised process; Shape B's locus (`agent_run`) is the
   process that is *being* supervised today. Adding "and now also
   supervises a child" to a process that the parent supervises is a
   layering concern Shape A avoids.

The recommendation is qualified: it is **conditional on PI's fd-separation
solution working as RFC #691 §3 describes, on at least the podman path**.
If PI cannot separate JSONL fds from the user-visible PTY then no shape of
prism's lifecycle can rescue stdio harnesses — neither Shape A nor Shape B.
The first prototyping milestone for this work is therefore "stand up PI
inside a podman container with prism's sidecar holding the JSONL fds via
`podman exec -i` while the tmux pane runs `podman attach`." Until that
milestone is met, the recommendation should be re-examined.

### Staged implementation suggestion (informational, not part of B.3 scope)

Shape A is large enough that one PR is unlikely to land it. The following is
a sketch — actual issues would be filed by Track E synthesis (#1090):

1. **Stage 1: stdio-harness-aware sidecar startup, behind a transport-shape
   gate.** The sidecar gains a `harness.TransportShape` enum check at the
   top of `Run()`; for `stdio-pipe` shape the WaitHealthy/CreateSession
   block is skipped and a "spawn the harness child + wire pipes" block
   runs instead. HTTP-shape harnesses are unaffected. No behaviour change
   for any existing harness.
2. **Stage 2: stdio-harness path verification with a fake stdio harness
   under bwrap.** A test-only stdio harness (writes a few JSONL frames to
   stdout and exits) exercises the sidecar's new launcher path under
   bwrap without requiring a real PI. This catches the structural
   regressions before PI integration begins.
3. **Stage 3: PI integration on podman.** The first real harness; this is
   the milestone where Open question 1 (`podman exec -i` for JSONL +
   `podman attach` for TUI) gets resolved by prototyping. Stages 1 and 2
   are pre-requisites; the registry shape from B.2 is also a
   pre-requisite (without it the sidecar cannot dispatch on transport
   shape).
4. **Stage 4: PI integration on bwrap/sandbox-exec/host.** Conditional on
   PI's TUI bridging story working in those modes — which is upstream of
   prism. May require a PI-side change rather than a prism-side change.

## Code regions that change under Shape A

File:line citations that the implementation work would touch. This is the
mechanical scope only; the design choices that would govern each change live
elsewhere (B.2 for the registry, B.4 for the harness-group abstraction, etc.).

- **`internal/sidecar/sidecar.go:339-509`** — the `Run()` startup block.
  Today this is a podman-shaped sequence: `mgr.Create` → `WaitHealthy` →
  `CreateSession` → `OnReady` → `DeliverInitialPrompt`. Under Shape A this
  block becomes a per-transport-shape branch:
  - HTTP-shape: today's code, unchanged.
  - Stdio-shape: spawn the harness as `exec.Cmd` with stdin/stdout pipes;
    wire `s.harness.Subscribe(ctx)` to read from the stdout pipe; treat
    "first frame received" as the readiness signal that fires `OnReady`;
    treat "child process exited" as terminal (writes `StateError` if the
    exit happens before any frame, otherwise `StateFinished`).
- **`internal/sidecar/sidecar.go:609-704`** — the bwrap-mode startup-connect
  timeout. For stdio harnesses the trigger condition changes from "no port
  binding within timeout" to "no first JSONL frame within timeout" or "child
  exited unexpectedly". The timeout duration probably stays the same.
  `s.cfg.OpencodeURL` references in this block (B.1 §7.2 lists `:656`)
  become a per-shape value (URL for HTTP, a description like
  "stdio-pipe(pid=…)" for stdio).
- **`internal/sidecar/sidecar.go:412 mgr.WaitHealthy`** — only fires for
  HTTP-shape harnesses (since stdio harnesses do not have a port).
  Conditional on transport shape.
- **`internal/sidecar/sidecar.go:493-507`** — `OnReady` invocation. Stays
  for both shapes; the trigger differs (HTTP: container is healthy; stdio:
  first JSONL frame). The readiness file's *purpose* (unblock `podman
  attach` in the agent pane) is unchanged for podman; for non-podman modes
  Shape A's stdio path may not need a readiness file at all (B.1 class 4
  flagged this as `[uncertain]` pending PI prototyping).
- **`internal/sidecar/sidecar.go:497-502`** — the `DeliverInitialPrompt`
  goroutine. For stdio harnesses the "delivery" is "write the first JSONL
  frame to the harness's stdin pipe." The interface signature stays;
  the opencode adapter and the (future) PI adapter implement it
  differently. B.1 class 7 flagged the semantic ambiguity (HTTP returns
  acceptance via 2xx; stdio is fire-and-forget) — left for B.4/B.5.
- **`internal/harness/harness.go:32`** —
  `HealthCheck(ctx context.Context, port int) error`. The `port` parameter
  is meaningless for stdio. Signature change owned by B.2 (registry +
  transport shape).
- **`internal/harness/harness.go:42-45`** — `DeliverInitialPrompt` /
  `DeliverPrompt` semantics; ditto, B.4/B.5.
- **`internal/sidecar/sidecar.go:119-188`** — `Config struct`. The
  `OpencodeURL` field is replaced (or supplemented) by a transport-shape
  field. Specifics owned by B.2.
- **`cmd/sidecar.go:106-365`** — `runSidecar`. The `--opencode-url` flag
  remains for HTTP-shape harnesses; stdio-shape harnesses get a different
  set of flags (the harness binary path, stdio-mode flags, etc.) — exact
  shape owned by B.2.
- **`internal/session/sidecar.go:269-401`** — `StartSidecarWithOpts`. The
  `--port` argv assembly (B.1 §7.12, lines `:321` / `:338`) is conditional
  on transport shape. The `--opencode-url` argument
  (`internal/session/sidecar.go:301`) becomes per-shape too.
- **`cmd/agent_run.go:67-353`** — `runAgentRun` for bwrap. **Bypassed**
  for stdio harnesses: the tmux pane's agent window runs `prism sidecar`
  instead of `prism agent-run` for stdio sessions. `BuildOpencodeCmd`
  (`internal/session/session.go:265-308`) gets a fourth case for "stdio
  bwrap" that emits `prism sidecar --session …` rather than `prism
  agent-run --session …`. The `agent-run.go` file itself is unchanged for
  HTTP-harness sessions.
- **`cmd/agent_run.go:516-616`** — `runAgentRunSandboxExec`. Same as
  bwrap: bypassed for stdio harnesses. The `syscall.Exec` model is left
  alone for HTTP-harness sandbox-exec sessions; the stdio path goes via
  the sidecar's new launcher block.
- **`internal/session/session.go:265-308`** — `BuildOpencodeCmd`. Adds
  per-shape branching: for stdio harnesses the agent-window command
  becomes `prism sidecar --session …` (not `podman attach`, not `prism
  agent-run`, not `opencode`). The HTTP-harness branches are unchanged.
- **`internal/container/container.go:642-760` `(*Manager).Create`** — for
  podman, the launch invocation must not consume the container's main PTY
  for the JSONL fds (open question 1). The change is confined to the
  podman startup mode used when `transportShape == stdio`. HTTP-harness
  podman sessions use today's `podman run -d` shape unchanged.
- **`internal/container/bwrap.go:608-635`** — the bwrap argv terminator.
  For stdio-shape harnesses, the `opencode --port … --hostname …` block
  is replaced by the harness's stdio invocation (`pi --mode rpc` or
  whatever the registered transport-shape declares). Owned in mechanism
  by B.2.
- **`internal/db/db.go:218-225`** — the `harness_port` column added in
  v7→v8. Stays nullable; for stdio-harness sessions it is NULL. No
  migration needed.
- **`internal/sidecar/sidecar.go:587-607`** — merge-queue watcher startup.
  Unchanged under Shape A — the watcher remains in the long-lived sidecar
  process.

Approximate scope: ~10 source files touched, ~400-600 LoC of net change
(most of it in `internal/sidecar/sidecar.go:Run` and the per-shape branches
of `BuildOpencodeCmd` / `(*Manager).Create`). Well below the 1000-LoC
threshold that would suggest a wholly different design direction.

## The bifurcation question — uniform lifecycle vs split

Per the AC: "the document explicitly addresses the bifurcation question:
uniform lifecycle vs split."

Three positions are coherent. The recommended one is the third.

### Position 1: Fully uniform — one lifecycle for all transport shapes

The sidecar is always the parent of the harness, even for HTTP-shape
harnesses. Today's "spawn flow starts both as siblings, sidecar dials" model
is replaced by "spawn flow starts the sidecar; sidecar starts the harness."
This is technically possible — the sidecar could `podman run` opencode
detached and then dial the local port itself — but it gains nothing for
HTTP-shape harnesses (the `podman healthcheck` and `WaitHealthy` paths
already work, and the sidecar already owns the container's lifetime via
`mgr.Shutdown()`) and it forces a structural rewrite of the podman startup
sequence with no observable behaviour change. **Reject this position** as
unjustified churn.

### Position 2: Fully split — separate lifecycle code paths for HTTP and stdio

`internal/sidecar/sidecar.go:Run` is split into `runHTTP` and `runStdio`
methods, dispatched from a top-level shape check. Each method is internally
coherent; neither one carries the other's complexity. This is the cleanest
possible code structure but it forfeits the existing sharing on the *back*
half of the loop — the SSE event channel consumption (`:706-708`), the
host-API server setup (`:524-585`), the merge-queue watcher (`:587-607`),
the signal-handling shutdown — none of which are transport-specific.
Splitting at the top of `Run()` duplicates those blocks in both methods;
keeping them shared requires a `runStartup` / `runMainLoop` decomposition
that the split-method design does not naturally produce. **Reject this
position** because the back-half sharing is real and load-bearing.

### Position 3 (recommended): One lifecycle, one branch point

The lifecycle stays uniform — one `Run()` method, one shutdown path, one
host-API server, one merge-queue watcher. The single transport-shape branch
lives at the top of `Run()` (today's `if s.cfg.Container != nil` block at
`:346`) and toggles the **launch-and-readiness sequence**:

- HTTP-shape: today's `mgr.Create` → `WaitHealthy` → `CreateSession` →
  `OnReady` → `DeliverInitialPrompt`.
- Stdio-shape: spawn-harness-as-child → wire pipes → wait-for-first-frame
  → `OnReady` → write-first-prompt-frame.

After that branch point both shapes converge on `s.harness.Subscribe(ctx)`
returning a channel of `harness.HarnessEvent` — the SSE adapter reads from
the HTTP stream, the JSONL adapter reads from the stdout pipe; the
sidecar's main loop does not care which. The `Subscribe` interface method
is already the cleanest harness-agnostic call site in the file (B.1 §7.2's
classification). The bifurcation lives where it is justified — in the
launch sequence — and nowhere else.

This position implies the registry shape from B.2 must declare, per
harness, whether the sidecar should take the HTTP-launch branch or the
stdio-launch branch. That is exactly the role B.2 already proposes for
transport-shape declaration; the lifecycle work in B.3 consumes it.

## Open questions and `[uncertain]` flags

Consolidated and labelled. Each is a question that cannot be answered from
prism source alone.

1. **`podman exec -i` vs `podman run -i -a`:** for Shape A on the podman
   path with a stdio harness, which podman invocation gives the sidecar
   the container stdin/stdout fds without consuming the container's main
   PTY (which `podman attach` from the tmux pane needs)? `[uncertain]`
   — needs prototyping inside a podman container with a real PI binary.
   RFC #691 §3 implies the answer is "the JSONL pipe is a separate file
   descriptor" but does not commit to an implementation mechanism.
2. **TUI bridging for stdio harnesses on non-podman modes:** RFC #691 §3
   addresses only the podman case. For bwrap/sandbox-exec/host, what
   carries the user-visible TUI? `[uncertain]` — almost certainly a
   PI-side concern (the harness must offer some out-of-band TUI access
   when the JSONL pipe is the only thing on stdin/stdout) rather than a
   prism-side one. Until PI is prototyped in bwrap, neither shape can be
   committed to for non-podman stdio.
3. **Readiness signal for stdio harnesses:** B.1 class 4 already flagged
   this. For podman stdio under Shape A, does PI's TUI need a separate
   readiness signal for `podman attach` even when the sidecar has
   already received the first JSONL frame? `[uncertain]` — depends on
   whether PI's TUI rendering completes by the time the first JSONL
   frame is emitted, or whether there is a window between
   first-frame-emitted and TUI-rendered where `podman attach` would land
   on a blank screen.
4. **`DeliverInitialPrompt` semantics for stdio:** B.1 class 7 flagged
   this. Does PI's RPC have any synchronous acknowledgement on stdout
   ("got it, processing"), or is writing to its stdin truly
   fire-and-forget? `[uncertain]` — affects whether the
   `DeliverInitialPrompt` interface contract for stdio is "bytes
   written" or "command processed". B.4/B.5 own the resolution.
5. **Cleanup signal handling for Shape A stdio:** when the sidecar
   receives SIGTERM during a stdio session, what cleanup order is right
   for the harness child? Today (HTTP shape): SIGTERM →
   `(*Sidecar).Shutdown` → `mgr.Shutdown` (stops/removes container) →
   process exit. For stdio Shape A: SIGTERM → close harness's stdin (so
   it sees EOF and exits cleanly) → wait briefly for child exit →
   SIGTERM the child if still running → SIGKILL if still running. The
   exact timeouts and the choice of whether to grace the child via
   stdin-close before signalling are PI-specific. `[uncertain]` —
   unspecified by RFC #691; default to a 5s grace window and revisit
   after PI integration.
6. **Restore for stdio harnesses on Shape A:** when restore re-spawns the
   sidecar, the sidecar re-spawns the harness. Today's restore path
   assumes the previous container is gone and a fresh one is created;
   for stdio Shape A on bwrap/sandbox-exec/host the previous harness
   process is also gone (it was killed when its parent sidecar died).
   For podman the previous container may still be around (created
   detached, not killed by the sidecar's death) — which means restore
   must `podman rm -f` it before the new launch. `[uncertain]` only in
   that the exact restore-time container lifecycle for stdio-on-podman
   is sensitive to which podman invocation Shape A settles on (open
   question 1).
7. **Coordinator vs worker on stdio:** the merge-queue watcher
   (`internal/sidecar/sidecar.go:587-607`) is started in coordinator
   sessions. If a stdio harness is ever used as a coordinator, the
   watcher's lifecycle is tied to the sidecar's lifecycle — same as
   today. No `[uncertain]` flag here, but worth noting that Shape A's
   "sidecar stays detached" property is what keeps this
   straightforward; Shape B would have to find a new home for the
   watcher in coordinator sessions.

## What this review deliberately does not do

- It does **not** pick a registry shape. B.2 owns that. Shape A is
  expressed in terms B.2 can fulfil (transport-shape declaration on the
  registry) but does not constrain B.2's implementation.
- It does **not** propose a harness-group taxonomy. B.4 owns that. The
  HTTP-vs-stdio distinction this document uses is a transport-shape
  distinction, not a harness-group commitment.
- It does **not** propose a payload-schema decoupling. B.5 owns that. The
  events the sidecar receives via `Subscribe` are an opaque
  `HarnessEvent` channel under Shape A — the schema beneath those events
  is B.5's domain.
- It does **not** propose an archive-pipeline decoupling. B.6 owns that.
- It does **not** propose code changes. The implementation work this
  review motivates lives in a future Track-E-prioritised issue, not in
  this PR.

## Related

- B.1: [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
  — the classification this review consumes.
- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md)
  §9 (per-spawn data flow), §10.1 (`prism spawn` lifecycle), §11 (external
  boundaries).
- Design: [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: #1081. Parent design: #1072.
- RFCs: #691 (multi-harness support, especially §3), #606 (PI coding agent
  support).
- Sibling Track B issues: #1080 (B.2 — registry + transport-shape, the
  prerequisite), #1082 (B.4 — harness group abstraction), #1083 (B.5 —
  payload schema), #1084 (B.6 — archive pipeline). Track A Wave 2: #1077
  (A.2), #1078 (A.3), #1079 (A.4). Sandbox-exec parity: #1018.
