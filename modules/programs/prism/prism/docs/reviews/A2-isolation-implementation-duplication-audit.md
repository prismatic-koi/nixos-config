# A.2 — bwrap / sandbox-exec / podman implementation duplication audit

Status: proposal (no code changes).
Issue: #1077.
Track: A (isolation), Wave 2 follow-up to A.1.
Source corpus:

- `modules/programs/prism/prism/docs/reviews/A1-isolation-registry-shape.md`
  (the registry-shape proposal that landed in #1097).
- `modules/programs/prism/prism/docs/architecture-inventory.md` §6
  (the isolation-mode coupling map).
- The three implementation files cover-to-cover:
  - `internal/container/bwrap.go` (734 LoC; `BuildArgs` at 488 LoC).
  - `internal/container/sandbox_exec.go` (240 LoC).
  - `internal/container/container.go` (1753 LoC; `buildRunArgs` at 555 LoC).
- The parent design doc:
  `modules/programs/prism/prism/docs/reviews/000-design-narrow-review-series.md`.

## 1. Context and method

A.1 (#1073, landed in #1097) proposed widening `Isolator` so that every
per-mode branch in the cmd-layer collapses into either a method call on
`Isolator` or a registry lookup. A.2's job is one layer down: now that the
shape of the seam is known, **what is duplicated below the seam between the
three concrete implementations**, and which of those duplications can be
extracted as shared helpers without changing behaviour.

For each behavioural concern the audit:

1. Cites the per-mode current implementation as `file:line` for all three
   modes (or notes that a mode does not implement it).
2. Tags the concern `must-differ`, `incidentally-different`, or
   `already-shared`.
3. For `incidentally-different`: presents a proposed shared helper signature
   and identifies whether the helper lives **on** A.1's `Isolator` superset or
   **below** it (e.g. as a free function in `internal/container`, or as a
   small subordinate interface).
4. For `must-differ`: documents the reason in 1–3 sentences.
5. Carries an `[uncertain]` flag where the worker cannot determine without
   running both modes.

Where useful, the per-concern entry also notes the migration cost and any
podman-outlier behaviour that decides whether podman moves with the helper or
stays duplicated.

**Terminology.** "Already-shared" means the implementations actively call a
single helper today (`MinimalIsolatedExecEnv`, `NameForSession`,
`writeGitconfig`, `writeSshConfig`). "Incidentally-different" means the bodies
look similar but each mode has its own copy. "Must-differ" means the bodies
look similar but the design intent (or the host platform) requires the
divergence and a shared helper would either re-introduce the divergence or
hide a legitimate per-mode decision.

The corpus is the three files plus their documented call paths. Where a
concern's lifecycle spans into `cmd/agent_run.go` (signals, PTY, stderr
tee), the audit cites that file too because the bwrap and sandbox-exec paths
use it as their lifecycle host — `Manager.Shutdown`/`HasExited`/`DumpLogs`
are stubs for both of those modes.

## 2. Per-concern table

The eight concerns from #1077, plus a small handful of supporting concerns
that surface during the read (temp-file path naming, credential env-var
selection, `--port`/`--hostname` invocation tail, allowed_signers). The
supporting concerns are kept separate so the eight required ones remain
clean rows; they are tagged the same way.

| # | Concern | podman | bwrap | sandbox-exec | Tag |
|---|---|---|---|---|---|
| 1 | Mount preparation (volume / bind / ro-bind / SBPL `file-read*`) | `container.go:988-1450` (`buildRunArgs` mount block) | `bwrap.go:191-481` (system roots, sensitive-`/etc` shadow, conditional `--ro-bind`/`--bind`, AWS/Kube/SSH/opencode allowlists, clipboard cache, host-API socket dir) | `sandbox_exec.go:85-108` (read-only system roots + `(deny ...)` shadow) | **incidentally-different** (system-root + sensitive-shadow rules) and **must-differ** (per-mode mount syntax). See §3.1. |
| 2 | Env allow-list — host process env passed to the sandbox harness | n/a (podman receives env via `--env K=V` in `buildRunArgs`; the podman binary itself inherits the sidecar's full env) | `bwrap.go:682-684` and `cmd/agent_run.go:454-456` (both alias `container.MinimalIsolatedExecEnv`) | `cmd/agent_run.go:601` (calls `container.MinimalIsolatedExecEnv` directly before `syscall.Exec`) | **already-shared** between bwrap and sandbox-exec via `sandbox_exec.go:246` (`MinimalIsolatedExecEnv`); podman's `--env` injection is **must-differ** because it targets a different boundary. See §3.2. |
| 3 | gitconfig writing | `container.go:526-625` `writeGitconfig(isolationPodman)` called at `container.go:687` | same function, called as `writeGitconfig(isolationBwrap)` at `container.go:860` (`PrepareBwrap`) | not implemented — `PrepareSandboxExec` (`container.go:896-903`) does **not** call `writeGitconfig`; per file-level comment at `sandbox_exec.go:6-12` this is deferred to PR 3 (#1017) | **already-shared** between podman and bwrap via the `mode isolationMode` parameter; **incidentally-different** as a not-yet-shared concern between bwrap and sandbox-exec. See §3.3. |
| 4 | Terminfo (`TERM` value the sandbox interior sees) | `container.go:1247` hardcodes `TERM=xterm-256color`; explicit comment at `:1244-1246` says "Do NOT pass through the host's $TERM" | `bwrap.go:529-535` passes through host `TERM` (fallback `xterm-256color` only when unset); `bwrap.go:540-542` also passes through `COLORTERM` when set | not set inside the SBPL profile; the host `TERM`/`COLORTERM` are inherited via `MinimalIsolatedExecEnv`'s allow-list (`sandbox_exec.go:252-253`) | **must-differ** (podman is constrained by the container image's terminfo set); **incidentally-different** between bwrap and sandbox-exec (both resolve to "host TERM"). See §3.4. |
| 5 | Worktree mount semantics | `container.go:1000-1007, 1432-1450` (Dst `/workspace`; `:Z` SELinux relabel; ro vs rw via `WorktreeReadOnly`; bare-repo at `/prism-git`; gitdir-pointer overlay) | `bwrap.go:256-279` (Dst == Src; bare-repo and worktree gitdir at host paths; **no** `WorktreeReadOnly` handling — see `bwrap.go:148-150` deferred-comment) | not mounted (PR 2 minimal profile; `sandbox_exec.go:73-79` and `:163-166` document deferral to #1017) | **must-differ** (path conventions: podman remap to `/workspace` is dictated by the container image's `$HOME`/CWD; bwrap shares the host namespace, so Dst==Src is correct); see §3.5. |
| 6 | Signals (forwarding, process group, foreground TTY) | podman handles signals internally via `podman attach`; sidecar's `Shutdown` (`container.go:815-831`) calls `Isolator.Shutdown` which `exec.CommandContext("podman", "stop", …)` (`isolator.go:109-122`) | `cmd/agent_run.go:281-317` sets `Setpgid: true`, `tcsetpgrpForeground` (`:377-387`), `forwardSignalsToBwrap` (`:402-431`) — TERM/INT/HUP/WINCH forwarded to the bwrap process group; `bwrap.go:689-712` SIGTERM-then-SIGKILL fallback when invoked via the (unused-in-prod) Manager `Run` path | none — `runAgentRunSandboxExec` (`cmd/agent_run.go:516-616`) `syscall.Exec`s sandbox-exec directly; the OS hands the controlling tty back to sandbox-exec/opencode without supervision; `sandboxExecIsolator.Shutdown` (`sandbox_exec.go:210`) is an explicit no-op with comment pointing at PR 4 (#1018) | **must-differ** for podman vs the rest (podman's signal model is foreign — `podman attach` mediates everything); **incidentally-different** between bwrap and sandbox-exec (`syscall.Exec`-only lifecycle is acknowledged at `sandbox_exec.go:204-209` as "deferred to PR 4 (#1018)", which is the right pointer to a future shared lifecycle helper, not a present one). See §3.6. |
| 7 | Shutdown (the "stop the running session" call) | `isolator.go:109-122` runs `podman stop --time 10` then `podman rm --force`; called from `Manager.Shutdown` (`container.go:815-831`) which also unlinks all the temp files at `:821-828` | `bwrap.go:689-712` SIGTERM-then-30s-grace-then-SIGKILL on the supervised child (only used when invoked via `bwrap.Run`; in the production agent-run flow, the bwrap process is owned by the tmux pane, so signals come from `cmd/agent_run.go:402-431`) | `sandbox_exec.go:210` no-op (`Shutdown is a no-op for sandbox-exec`); production lifecycle is the tmux pane | **must-differ** for podman (it owns a long-lived container resource that survives process death); **incidentally-different** between bwrap and sandbox-exec, both of which currently delegate to "tmux pane death takes the process down". See §3.7. |
| 8 | Readiness (signalling that the harness is ready to receive a TUI attach) | sidecar `WaitHealthy` polls `http://127.0.0.1:<port>/global/health` (`container.go:740-783`); `setupFullLayout` (`session.go:643-653`) prepends a 240-iteration shell-poll loop (`buildReadinessWaitCmd` `session.go:723-734`) for `mode == "podman"` only | no readiness wait — `session.go:597-599` and `:637-638` document that bwrap pane runs `prism agent-run` immediately and the sidecar does not write a ready file for bwrap | no readiness wait — sandbox-exec falls into the same `setupFullLayout` `else` branch (`session.go:643`); `internal/sidecar/sidecar.go` does not mention sandbox-exec at all (verified by grep) | **must-differ** for podman (HTTP healthcheck targets the gvproxy/pasta-bridged port; bwrap and sandbox-exec share the host net namespace and don't need a separate gate); **already-shared** between bwrap and sandbox-exec by virtue of both being "no readiness wait". See §3.8. |

Supporting concerns (same tagging discipline, but not in the eight required rows):

| # | Concern | podman | bwrap | sandbox-exec | Tag |
|---|---|---|---|---|---|
| S1 | Temp-file path naming for per-session artefacts | `container.go:367-411, 438-440, 635-637` (`prism-gitdir-`, `prism-ssh-config-`, `prism-gitconfig-`, `prism-allowed-signers-`, `prism-opencode-config-`, `prism-claude-creds-`, `prism-wt-gitdir-`) | reuses the same Manager helpers via `PrepareBwrap` (`container.go:847-881`) | `sandbox_exec.go:128-130` (`prism-sandbox-exec-profile-`) — separate helper, same `os.TempDir()` + `m.name` shape | **already-shared** (in the sense that all three live on `Manager` and reuse `m.name`); the **shape** of the helper is identical and could be hoisted to a single `m.tempPath(stem string) string`. See §3.S1. |
| S2 | `--port` / `--hostname` invocation tail | `container.go:1522-1539` (`Image, opencode, --port ContainerPort, --hostname 0.0.0.0, [--agent], [--prompt]`) | `bwrap.go:608-636` (`opencode, --port AllocatedPort∥ContainerPort, --hostname 127.0.0.1, [--agent], [--prompt]`) | `sandbox_exec.go:172-194` (same shape, same fallback rule, same `127.0.0.1`) | **incidentally-different** between bwrap and sandbox-exec (identical tail); **must-differ** for podman (`0.0.0.0` is required because the container has its own net namespace and pasta NATs in; `ContainerPort` not `AllocatedPort` because the port mapping is done by `--publish`, not by the harness). See §3.S2. |
| S3 | `cfg.AgentEnvVars` injection (with `KUBECONFIG`/`AWS_CONFIG_FILE` suppression) | `container.go:1466-1481` (sorted-key emission, two-key suppression) | `bwrap.go:501-516` (verbatim copy of the same logic, translated `--env` → `--setenv`) | not implemented (PR 2 minimal profile) | **incidentally-different** between podman and bwrap; **must-differ** in syntax only. See §3.S3. |
| S4 | `cfg.RuntimeEnv` (harness runtime env) injection | `container.go:1518-1520` (`--env K=V` per entry) | `bwrap.go:573-576` (`--setenv K V` per entry) | not implemented | **incidentally-different**; same shape as S3. |
| S5 | `PRISM_SPAWN_PATH` / `PRISM_BARE_ROOT` / `PRISM_SESSION_NAME` / `PRISM_HOST_API` injection | `container.go:1257-1259, 1283-1294` | `bwrap.go:550-602` | not implemented | **incidentally-different**; the *values* differ (podman uses canonical `/workspace` and `/prism-git`; bwrap uses the host paths). See §3.S5. |
| S6 | SSH-key + ssh-config + allowed_signers mounting | `container.go:1357-1412` (`--volume … :ro`) | `bwrap.go:330-381` (`--ro-bind`) | not implemented (PR 3 / #1017) | **incidentally-different** (the *what to mount* and *under which canonical name* are identical — `access-key`, `signing-key`, `signing-key.pub`, `allowed_signers`, `known_hosts`, `config`); the *how* differs by mount syntax. See §3.S6. |
| S7 | `prepareVolumeDirs` (eager directory creation for bind sources) | `container.go:919-985` called with `perSessionOpencode=true` from `Create` (`container.go:712`) | same function called with `perSessionOpencode=false` from `PrepareBwrap` (`container.go:874`) | not called by `PrepareSandboxExec` (no bind sources in PR 2); the `host-API socket dir` block at `:962-979` is the only branch that would matter and is unconditional on mode (skipped only when `HostAPISockPath == ""`) | **already-shared** (already a single function with a boolean knob); the only further consolidation would be adding a sandbox-exec branch when #1017 lands. |

## 3. Per-concern detail

### 3.1 Mount preparation

**Tag:** **incidentally-different** for the *what to mount* set; **must-differ**
for the syntax (podman volume strings, bwrap argv pairs, SBPL `(subpath ...)`).

**What is the same.** The three modes mount overlapping sets of host
artefacts at canonical in-sandbox paths (`$HOME/.aws/{config,credentials,sso,cli}`,
`$HOME/.kube/config`, `$HOME/.ssh/{access-key,signing-key,signing-key.pub,
known_hosts,config,allowed_signers}`, `$HOME/.cache/{opencode,bun,prism/clipboard}`,
`$HOME/.claude`, `$HOME/.mcp-auth`, opencode config allowlist with
`agents/` excluded for review-* roles, `/nix/var/nix/daemon-socket`, the
worktree, the bare-repo dir + worktree gitdir). The conditionality rules
(`os.Stat` for optional dirs, `filepath.EvalSymlinks` for sops-managed
symlinks, the review-`agents/` exclusion at `bwrap.go:422-424` /
`container.go:1325, 1335-1340`) are also identical.

**What is genuinely different.**

- **Mount syntax** — `--volume SRC:DST[:Z][:ro]` (podman) vs
  `--ro-bind|--bind SRC DST` (bwrap) vs `(subpath "<path>")` inside `(allow
  file-read* …)` (sandbox-exec). Must-differ — these are the per-mode
  argument grammars.
- **SELinux relabel** — only podman emits `:Z`, because podman is the only
  mode that may run on an SELinux-enforcing host with a separate user
  namespace.
- **Dst-vs-Src remap** — podman remaps everything under `$HOME → /root`,
  worktree → `/workspace`, bare-repo → `/prism-git`. Bwrap and sandbox-exec
  share the host namespace and use Dst==Src for the worktree, host paths
  for everything else. Must-differ at the value level; the *decision* of
  "what canonical sandbox path does this artefact live at" is, however,
  shared across bwrap and sandbox-exec (and identical for all the
  `$HOME/.aws/...`, `$HOME/.ssh/...`, etc. cases).
- **Sensitive-/etc shadowing** — bwrap uses `--tmpfs /etc/wireguard`
  (`bwrap.go:232-239`), sandbox-exec uses `(deny file-read* file-write*
  (subpath "/private/etc/wireguard") (subpath "/private/etc/wpa_supplicant"))`
  (`sandbox_exec.go:101-107`). The *list* of subtrees is the same in
  intent. The *path translation* (`/etc/wireguard` → `/private/etc/wireguard`
  on macOS) is correct per platform; podman never sees the host `/etc` so
  the list does not apply.

**Proposed shared helper(s).**

A.1's `Isolator` does **not** want a `Mount(src, dst, opts)` method on the
interface — the abstraction would be too leaky (think `:Z`, ro-bind vs bind,
SBPL subpath grammar). Instead, **below** the A.1 interface, in
`internal/container/mounts.go` (new file), introduce a per-mode-agnostic
description of what to mount, plus per-mode emitters:

```go
// internal/container/mounts.go (new file).
package container

// MountSpec describes one host artefact that needs to appear inside the
// sandbox interior at a canonical path. It is mode-agnostic — each isolator
// emits the syntax it needs.
type MountSpec struct {
    HostPath        string // e.g. <home>/.config/aws/readonly-config; "" → skip
    SandboxPath     string // canonical sandbox path; e.g. <sandboxHome>/.aws/config
    ReadOnly        bool
    EvalSymlinks    bool   // true for sops symlinks; resolution failure → skip silently
    OptionalIfMissing bool // true for AWS / Kube / Claude — skip if HostPath does not exist
    SELinuxRelabel  bool   // ignored by bwrap / sandbox-exec; podman emits :Z
}

// StandardSandboxMounts returns the canonical mount set for a given session
// configuration, agnostic of isolation mode. The caller (each Isolator) walks
// the slice and emits the per-mode syntax via its own appender.
func StandardSandboxMounts(cfg Config, sandboxHome string, isReview bool) []MountSpec
```

Each isolator then walks `StandardSandboxMounts` and appends mode-specific
syntax (a `podmanIsolator.appendVolume(args, spec)`, `bwrapIsolator.appendBind(args, spec)`,
`sandboxExecIsolator.appendAllow(profile, spec)`). The decision tree (which
files to mount, which canonical sandbox names, which optional-by-stat
behaviour) collapses into one place; the per-mode emitters become small
mechanical functions.

This helper lives **below** A.1's `Isolator` interface — A.1's interface is
the seam, and mount emission is one mode-private detail behind the seam. The
package-internal `MountSpec` shape is the right level of sharing because
"emit a mount" is the smallest unit each isolator naturally has in common.

**Migration cost.** The `buildRunArgs` mount block (~450 LoC of the 555-LoC
function), the `BuildArgs` mount block in bwrap (~290 LoC of 488), and the
sandbox-exec subpath list are the three sites the helper consolidates.
Decomposition is mostly mechanical because the conditionality rules are
already enumerated and consistent. The **podman-outlier** parts (`:Z`
relabel, `/workspace` remap, `/prism-git` layout, `--mount type=bind` for
directories vs `--volume` for files at `container.go:1349-1354`) stay
inside `podmanIsolator`'s emitter and are not part of the shared spec.

**[uncertain]** The `internal/sidecar/sidecar.go:512-547` host-API socket
dir bind is special-cased today as a per-session-directory bind for
security-isolation reasons (#960). It fits cleanly as a `MountSpec`, but
the **directory-vs-file** distinction (`bwrap.go:585-602`,
`container.go:1283-1294`) needs a second `MountSpec` field
(`MountAsDirectory bool`) to model accurately, and that field is bwrap/podman-only
by current design. Worth verifying when this is implemented.

### 3.2 Env allow-list

**Tag:** **already-shared** between bwrap and sandbox-exec; **must-differ** for
podman.

**What is shared today.** `MinimalIsolatedExecEnv` at `sandbox_exec.go:246`
is the canonical helper. It is called by:

- `bwrap.go:682-684` (`minimalBwrapExecEnv` is a thin alias).
- `cmd/agent_run.go:454-456` (the bwrap dispatch path; same alias).
- `cmd/agent_run.go:601` (the sandbox-exec dispatch path; calls `MinimalIsolatedExecEnv`
  directly).

The allow-list is `PATH, HOME, USER, LOGNAME, TERM, COLORTERM, LANG, LC_ALL`
(`sandbox_exec.go:248-256`).

**Why podman is must-differ.** The podman `--env` flag injects values into
the *container's* env (`container.go:1452-1481`); the host process env that
the `podman` binary itself sees is whatever the sidecar inherited and is
not filtered. Different boundary, different intent. A unified helper would
hide that the podman call site is in a different lifecycle phase
(container *creation* vs sandbox *exec*).

**Proposed shape.** No change — the helper is already shared at the right
boundary, and a future move would be to expose it as
`Isolator.HostExecEnv(host []string) []string` in A.1's superset (with
podman returning `host` unmodified). That sits **on** the A.1 interface,
not below — A.1 §4.5 does not list it explicitly today; this audit
suggests adding it during Phase 2 of A.1's extraction order (alongside the
capability reads).

### 3.3 gitconfig writing

**Tag:** **already-shared** between podman and bwrap;
**incidentally-different** as a not-yet-implemented concern between bwrap and
sandbox-exec.

**What is shared today.** `Manager.writeGitconfig(mode isolationMode)` at
`container.go:526-625` already takes a mode discriminator and substitutes
`sandboxHome(mode)` into the embedded `signingKey`/`allowedSignersFile`
paths (`container.go:565-567`). Called as:

- `writeGitconfig(isolationPodman)` from `Create` (`container.go:687`).
- `writeGitconfig(isolationBwrap)` from `PrepareBwrap` (`container.go:860`).

The `isolationMode` enum (`container.go:72-77`) only has `isolationPodman`
and `isolationBwrap` — it predates sandbox-exec.

**What is incidentally-different.** `PrepareSandboxExec` at
`container.go:896-903` does **not** call `writeGitconfig`. The file-level
godoc at `sandbox_exec.go:6-12` and the `PrepareSandboxExec` comment at
`container.go:892-895` both note this is deferred to PR 3 (#1017). When
#1017 lands, sandbox-exec needs the same gitconfig logic with
`sandboxHome(...)` returning the host user `$HOME` (because sandbox-exec
shares the host UID/HOME, the same as bwrap).

**Proposed shape.** Extend `isolationMode` with `isolationSandboxExec`
(make it the same constant value as `isolationBwrap` if the sandbox-home
value is identical, *or* declare a third constant and have `sandboxHome`
return the host home for both). The cleanest answer is the third constant —
it documents the intent clearly even when the substituted value matches
bwrap. The helper signature does not change.

When A.1 lands, this helper moves **on** the `Isolator` interface as
`Isolator.WriteGitconfig(workspace, signersPath, sigKeyPath string) error`
(per A.1 §4.5). Until then it stays as the per-mode-parameter helper on
`Manager`.

### 3.4 Terminfo

**Tag:** **must-differ** between podman and the rest;
**incidentally-different** between bwrap and sandbox-exec.

**Why podman must-differ.** `container.go:1244-1247` explicitly hardcodes
`TERM=xterm-256color` and the comment forbids host pass-through — the
container image's terminfo set is bounded, and `tmux-256color` is not in
the base image's `ncurses-base`. Passing through host TERM on podman would
break the TUI for hosts whose tmux pane sets non-trivial TERM values.

**Why bwrap and sandbox-exec are incidentally-different.** Both share the
host filesystem (the host's `/usr/share/terminfo` or `/nix/store/.../terminfo`
is reachable inside the sandbox), so passing through host TERM is correct.
Bwrap does this explicitly via `--setenv TERM <hostval>` (`bwrap.go:529-535`)
and `--setenv COLORTERM <hostval>` (`bwrap.go:540-542`). Sandbox-exec does
it implicitly via `MinimalIsolatedExecEnv`'s allow-list (`TERM` and
`COLORTERM` are both in the allow-list at `sandbox_exec.go:252-253`) — the
sandbox interior inherits the harness env wholesale because there is no
`--clearenv` equivalent in this PR.

**Proposed shape.** No shared helper is needed. The mechanism diverges
(explicit `--setenv` vs inherited via env-allow-list), but the **outcome**
is identical: host TERM reaches the sandbox interior. The decision of
whether to pass-through TERM is a `Capabilities` concern in A.1 (a flag
like `Capabilities.PassesHostTERM bool`), and `bwrap.go:529-535` could
collapse into one shared "set TERM if PassesHostTERM" helper if PR 3 of
sandbox-exec ever wires `--clearenv`-equivalent behaviour. Today it is
moot — sandbox-exec inherits, bwrap explicitly forwards, and both end up
with the same value. Lives **below** A.1's interface (helper if introduced)
or **as a `Capabilities` flag** if the divergence persists.

### 3.5 Worktree mount semantics

**Tag:** **must-differ.**

**Why.** The three modes use three different mount conventions because the
host-vs-sandbox path conventions are different:

- **podman** mounts the worktree at `/workspace` and the bare-repo at
  `/prism-git`. The container image's `$HOME` is `/root` and the agent
  runs as the container user, so paths like `/home/user/code/...` are
  not addressable inside. The remap is forced by the image's filesystem
  layout. It also maintains `WorktreeReadOnly` (`container.go:1003-1007`)
  and writes a `gitdir` overlay (`container.go:660-674, 1432-1450`) to
  fix up git's commondir pointer through the remap.
- **bwrap** mounts the worktree Dst==Src and the bare-repo dir Dst==Src
  (`bwrap.go:256-279`); the sandbox shares the host UID/HOME so the host
  paths are visible at the same locations inside. No `gitdir` overlay is
  needed because git's commondir pointer already points at a host path.
  `WorktreeReadOnly` is **not** implemented for bwrap (`bwrap.go:148-150`
  documents the deferral).
- **sandbox-exec** does not mount the worktree at all in PR 2 (file
  godoc at `sandbox_exec.go:6-12` and `:163-166`); PR 3 (#1017) is the
  designated landing for stage-HOME and worktree wiring.

**Reason it is must-differ.** The path conventions are dictated by the host
platform (Linux container image vs Linux user namespace vs macOS user
namespace) and by whether the harness sees the host filesystem layout
(bwrap, sandbox-exec) or a fresh container layout (podman). A shared
helper would need to abstract over both cases, which is exactly what the
`MountSpec` shape in §3.1 does — it expresses the desired
`(HostPath, SandboxPath)` pair without forcing a single convention.

**[uncertain]** Whether `WorktreeReadOnly` should land in bwrap and
sandbox-exec mirrors a parallel concern (review-agent semantics on
non-podman modes). Out of scope for A.2; flagged here so a future
implementation issue inherits the question.

### 3.6 Signals (forwarding, process group, foreground TTY)

**Tag:** **must-differ** for podman; **incidentally-different** between
bwrap and sandbox-exec.

**Podman is must-differ.** `podman attach` mediates the PTY, and the
container has its own init process. Signals from the tmux pane reach
`podman attach`, which forwards them; `Manager.Shutdown` (`container.go:815-831`)
stops the container via `podman stop`, not via signal forwarding to the
container PID.

**Bwrap implementation.** `cmd/agent_run.go` is the production bwrap
lifecycle:
- `:286` sets `Setpgid: true` so bwrap is its own process group leader.
- `:313` `tcsetpgrpForeground(int(os.Stdin.Fd()), bwrapCmd.Process.Pid)` —
  hand the controlling-tty foreground to bwrap so keypresses and SIGWINCH
  flow to it.
- `:317` `forwardSignalsToBwrap` (`:402-431`) — TERM/INT/HUP/WINCH forwarded
  to the bwrap process group via `syscall.Kill(-pid, sig)`.
- `:342-344` restores the original foreground pgid after wait.

**Sandbox-exec implementation.** `runAgentRunSandboxExec` at
`cmd/agent_run.go:516-616` does **not** install signal forwarding, **does
not** set up a process group, and does **not** manage TTY foreground. It
calls `syscall.Exec("/usr/bin/sandbox-exec", args, env)` at `:611`, which
replaces the agent-run process image with sandbox-exec — the kernel hands
the controlling TTY to sandbox-exec automatically (because agent-run was
the foreground process), and sandbox-exec's child (opencode) inherits the
TTY. There is no supervision and no shared helper.

**Why this is incidentally-different.** Both modes intend to give the agent
the TTY foreground and let it die when the pane dies. Bwrap uses a
supervised child (`exec.Cmd` + `Wait` + signal forwarding) because PR 4
(#1018) of sandbox-exec is explicitly slated to replace the `syscall.Exec`
model with the same supervised-child + lifecycle hardening
(`sandbox_exec.go:204-209`). When that lands, the supervisor block —
`Setpgid`, `tcsetpgrpForeground`/`tcsetpgrpRestore`, `forwardSignalsTo*`
— will be **identical** between the two modes.

**Proposed shape (today, deferred to #1018 in scope).** Hoist the
supervisor as a free function in `cmd/agent_run.go` (or a new
`internal/supervisor` package):

```go
// SuperviseChild runs cmd as the foreground process group on stdinFd,
// forwarding SIGTERM/SIGINT/SIGHUP/SIGWINCH to the process group, and
// restores the original foreground pgid on exit. cmd must already be
// configured with cmd.SysProcAttr.Setpgid = true. Returns cmd.Wait()'s
// error.
//
// Used by both the bwrap and sandbox-exec dispatch paths in agent-run
// once #1018 lands the supervised-child sandbox-exec lifecycle.
func SuperviseChild(cmd *exec.Cmd, stdinFd int, onWinch func()) error
```

A.1 does **not** propose this on the `Isolator` interface — the supervisor
is a `cmd/agent_run.go` concern (the in-pane lifecycle), not an isolator
concern (the build-and-launch lifecycle). So the helper lives **below**
A.1's interface, alongside `forwardSignalsToBwrap`/`tcsetpgrp*` in either
`cmd/agent_run.go` or a small new `internal/agentrun` package.

**[uncertain]** Whether SIGWINCH handling for sandbox-exec needs to be
identical to bwrap's. The bwrap path's `onWinch` is `nil` today
(`cmd/agent_run.go:317`); a future Bubble Tea–related fix may want to
resize a slave PTY, in which case the helper signature must accept a
non-nil callback. Cannot determine from a static read whether the
sandbox-exec path has the same Bubble Tea TIOCGWINSZ requirement (the
`syscall.Exec` model passes the existing terminal directly, which may be
sufficient).

### 3.7 Shutdown

**Tag:** **must-differ** for podman; **incidentally-different** between
bwrap and sandbox-exec.

**Podman.** `podmanIsolator.Shutdown` (`isolator.go:109-122`) runs
`podman stop --time 10` then `podman rm --force`. The container is a
long-lived resource that survives the sidecar process and the tmux pane,
so explicit teardown is required.

**Bwrap.** `bwrapIsolator.Shutdown` (`bwrap.go:689-712`) sends SIGTERM,
waits 30s, then SIGKILL. *But* this method is only reached when bwrap is
launched via `bwrapIsolator.Run` (`bwrap.go:652-666`), which is **not**
the production path — production is `cmd/agent_run.go` invoking bwrap as
a supervised child of the tmux pane. In production, "shutdown" happens
when the tmux pane dies (the kernel sends SIGHUP → `forwardSignalsToBwrap`
→ bwrap process group). The Manager-level `bwrapIsolator.Shutdown` is
present for future-test use and to satisfy the interface.

**Sandbox-exec.** `sandboxExecIsolator.Shutdown` (`sandbox_exec.go:210`) is
a no-op with comment pointing at PR 4 (#1018). Production "shutdown" is
again "tmux pane dies → kernel signals sandbox-exec".

**Proposed shape.** None today — the bwrap and sandbox-exec shutdown
behaviours genuinely are "do nothing because the OS handles it". The shared
helper that *does* matter is the tmux-pane-death lifecycle (signal forwarding
in §3.6), and that already collapses to one supervisor function. When
#1018 lands the supervised-child sandbox-exec path, the lifecycle becomes
"send TERM, wait grace, send KILL" for both; at that point the body of
`bwrapIsolator.Shutdown` (`bwrap.go:689-712`) is the right shared
implementation. Lives **below** A.1's interface (free function
`gracefulShutdown(*exec.Cmd, gracePeriod time.Duration)`); A.1's
`Isolator.Shutdown()` method is the public API, the body is shared.

### 3.8 Readiness

**Tag:** **must-differ** for podman; **already-shared** between bwrap and
sandbox-exec (both are "no readiness wait").

**Podman.** Two layers:
- The sidecar polls `http://127.0.0.1:<port>/global/health` via
  `Manager.WaitHealthy` (`container.go:740-783`).
- The tmux agent pane prepends a 240-iteration shell-poll loop
  (`buildReadinessWaitCmd` `session.go:723-734`) that waits for the
  sidecar's `ready` file before running `podman attach`.

The HTTP health-check is required because the container has its own net
namespace and pasta NATs in — the harness inside the container may take
several seconds before the listener is up, and `podman attach` would race
the listener.

**Bwrap.** No readiness wait. `session.go:597-599, 637-638` document the
reason: bwrap shares the host net namespace and the agent pane runs
`prism agent-run` directly (no `podman attach` to race). The sidecar still
runs (for SSE, state machine, host-API), but does not write a ready file
for bwrap sessions. `internal/sidecar/sidecar.go:638-680`'s
"startup-connect timeout" is the bwrap-only failsafe for the *sidecar's*
connection to opencode, not a pane-side readiness gate.

**Sandbox-exec.** No readiness wait. `setupFullLayout` falls into the
`else` branch at `session.go:643` because the predicate is `mode ==
"podman"`. `internal/sidecar/sidecar.go` does not mention `sandbox-exec`
at all — the sidecar treats sandbox-exec sessions identically to bwrap
sessions (no container, no health-check, agent-run as the pane command).

**Why already-shared.** Both bwrap and sandbox-exec converge on "the agent
pane runs `prism agent-run` immediately, no gate". The current
implementation expresses that as the absence of code; A.1 §4.3 proposes a
`Capabilities.NeedsReadinessWait bool` flag to encode the predicate
explicitly so the `mode == "podman"` literal at `session.go:643`
collapses into a capability read.

**Proposed shape.** A.1 already covers this — the `NeedsReadinessWait` flag
in `Capabilities` lives **on** the A.1 superset (or, more precisely, on
the `Capabilities` struct that hangs off the interface). No new helper
proposed here; the audit just confirms the absence-of-code in
sandbox-exec is the correct shared behaviour with bwrap, not a gap.

**[uncertain]** Whether sandbox-exec on macOS needs a different gate when
opencode is slower to bind on Darwin (e.g. notarisation/codesign delays
on first run). Cannot determine from a static read; flagged for E.1
synthesis.

### 3.S1 Temp-file path naming

**Tag:** **already-shared** in spirit; the helper shape is identical, but
expressed as seven separate `Manager` methods (`gitdirFilePath`,
`sshConfigFilePath`, `gitconfigFilePath`, `allowedSignersFilePath`,
`opencodeConfigFilePath`, `claudeCredentialsFilePath`,
`worktreeGitdirFilePath` at `container.go:367-411, 438-440, 635-637`) plus
one for sandbox-exec (`sandboxExecProfilePath` at `sandbox_exec.go:128-130`).

Every helper has the form:

```go
func (m *Manager) <stem>FilePath() string {
    return filepath.Join(os.TempDir(), "prism-<stem>-"+m.name)
}
```

**Proposed shape.**

```go
// internal/container/temppath.go (new file or fold into container.go).
func (m *Manager) tempPath(stem string) string {
    return filepath.Join(os.TempDir(), "prism-"+stem+"-"+m.name)
}
```

The seven existing methods can either delegate to `tempPath` (preserving
public API) or be replaced by inline `m.tempPath("gitdir")` calls. Lives
**below** A.1's interface (purely a Manager-internal shape).

**Migration cost.** Trivial. No behaviour change. The `EnsureRemoved` and
`Shutdown` cleanup blocks (`container.go:317-325, 821-828`) currently list
each temp file individually — after the helper, they could iterate a
constant slice of stems, which would also catch the case of a future
`tempPath` consumer being added without a corresponding cleanup line.

### 3.S2 `--port` / `--hostname` invocation tail

**Tag:** **incidentally-different** between bwrap and sandbox-exec;
**must-differ** for podman.

The two non-podman tails are byte-identical aside from the harness binary
position:

- `bwrap.go:608-636` — `"--", "opencode", "--port", "<port>", "--hostname", "127.0.0.1", ["--agent", role,] ["--prompt", text]`
- `sandbox_exec.go:172-194` — same trailing tuple, just preceded by
  `"sandbox-exec", "-f", profilePath, "opencode"`.

Both apply the same `AllocatedPort ∥ ContainerPort` fallback rule
(`bwrap.go:621-624`, `sandbox_exec.go:178-181`).

**Proposed shape.**

```go
// internal/container/harness_args.go (new file).
//
// HarnessInvocation returns the trailing arg slice that launches opencode
// with the per-session port, role, and initial prompt. The leading args
// (sandbox-exec wrapper, bwrap binary, etc.) are the isolator's responsibility.
func HarnessInvocation(cfg Config) []string {
    port := cfg.AllocatedPort
    if port == 0 {
        port = ContainerPort
    }
    args := []string{"opencode",
        "--port", fmt.Sprintf("%d", port),
        "--hostname", "127.0.0.1",
    }
    if cfg.AgentRole != "" {
        args = append(args, "--agent", cfg.AgentRole)
    }
    if cfg.InitialPrompt != "" {
        args = append(args, "--prompt", cfg.InitialPrompt)
    }
    return args
}
```

Bwrap calls it after `"--"`. Sandbox-exec calls it after the wrapper.
Podman calls a slight variant (substitutes `0.0.0.0` for `127.0.0.1`,
hardcodes `ContainerPort`, prepends `Image`); a `HarnessInvocation` with
parameters could absorb that, *or* `podman` keeps its inline tail —
either works because the divergence is small.

Lives **below** A.1's interface. The `Capabilities.HostNetworkNamespace`
flag (not in A.1 §4.3 today; this audit suggests adding it) selects
`127.0.0.1` vs `0.0.0.0` automatically.

**[uncertain]** Whether the harness invocation tail should be the harness
abstraction's responsibility (`internal/harness/opencode/adapter.go`) rather
than the container package's. Track B.1 (harness coupling) is the right
venue; A.2 only flags the duplication.

### 3.S3 / 3.S4 `cfg.AgentEnvVars` and `cfg.RuntimeEnv` injection

**Tag:** **incidentally-different** between podman and bwrap.

The two emitters are structurally identical (sorted-key emission for
`AgentEnvVars` with the `KUBECONFIG`/`AWS_CONFIG_FILE` suppression rule;
verbatim emission for `RuntimeEnv`):

- `container.go:1452-1481, 1518-1520` (podman) — `--env K=V`.
- `bwrap.go:485-516, 573-576` (bwrap) — `--setenv K V`.

Sandbox-exec does not implement either today (PR 2 is minimal; #1017 lands
both).

**Proposed shape.**

```go
// EnvInjector emits the per-session environment variables (AgentEnvVars +
// RuntimeEnv + per-mode prism context vars) in the order each isolator
// expects them. The caller passes a per-mode appender that handles the
// syntax (--env K=V vs --setenv K V vs setenv inside SBPL).
type EnvAppender func(args []string, k, v string) []string

func AppendStandardEnv(args []string, cfg Config, appender EnvAppender) []string
```

Lives **below** A.1's interface. The `KUBECONFIG`/`AWS_CONFIG_FILE`
suppression rule is in one place. The credentialEnvVars walk
(`container.go:1612-1673` / `bwrap.go:485-488`) is also a candidate to
fold in — both call `m.credentialEnvVars()` and then iterate the result
with mode-specific emitters; same `EnvAppender` shape applies.

**Migration cost.** Small. Two call sites → one helper + per-mode
appender. The sandbox-exec call site is added when #1017 lands.

### 3.S5 `PRISM_*` context env vars

**Tag:** **incidentally-different** in *value*; the *set of variables* is
the same (`PRISM_SPAWN_PATH`, `PRISM_BARE_ROOT`, `PRISM_SESSION_NAME`,
`PRISM_HOST_API`).

- podman uses canonical container paths: `/workspace`, `/prism-git`,
  `/var/run/prism-host/<sock>` (`container.go:1257-1259, 1283-1294`).
- bwrap uses host paths: `cfg.Worktree`, `cfg.BareRoot`,
  `unix://<host-sock>` (`bwrap.go:550-602`).

The decision of *which path the sandbox sees* is the per-mode bit — the
*set of vars to set* is shared.

**Proposed shape.** A small per-mode helper:

```go
// SandboxContextEnv returns the four PRISM_* env vars in (key, value) pairs.
// The caller emits them with its own appender. Each isolator computes the
// values from its own perspective on the sandbox path layout.
func (b *bwrapIsolator) SandboxContextEnv(cfg Config) [][2]string
func (p *podmanIsolator) SandboxContextEnv(cfg Config) [][2]string
```

Lives **below** A.1's interface, as a small per-isolator method that the
shared `AppendStandardEnv` (§3.S3) iterates. The set of keys is shared;
the values are per-mode.

### 3.S6 SSH-key + ssh-config + allowed_signers mounting

**Tag:** **incidentally-different** between podman and bwrap.

Both modes mount the same canonical SSH artefacts under
`<sandboxHome>/.ssh/{access-key, signing-key, signing-key.pub,
allowed_signers, known_hosts, config}`. The decision tree (which keys to
look up via `cfg.SshAccessKeyName`/`cfg.SshSigningKeyName`, which fall back
to `prismatic-koi-ed25519`/`prismatic-koi-ed25519-signingkey`, which
optional-by-resolution-failure) is identical:

- `container.go:1357-1412` (podman; `--volume … :ro`).
- `bwrap.go:330-381` (bwrap; `--ro-bind`).

The `m.allowedSignersReady` gate (`container.go:1401-1403` /
`bwrap.go:359-362`) is also identical.

Sandbox-exec does not mount SSH today (PR 2); PR 3 (#1017) is the landing.

**Proposed shape.** This is exactly what `MountSpec` (§3.1) collapses.
The decision tree becomes a `StandardSandboxMounts(cfg).SSH()` slice and
each isolator emits via its own appender. Lives **below** A.1's interface.

## 4. Migration-order proposal

The shared helpers are largely independent — each can land as its own PR
without touching the others. Suggested order (lowest-risk first):

1. **§3.S1 `tempPath(stem)` helper.** Trivial, pure refactor. Establishes a
   pattern for the rest. Touches `container.go` and `sandbox_exec.go` only.
2. **§3.S2 `HarnessInvocation(cfg)` helper.** Pure refactor. Touches
   `container.go:1522-1539`, `bwrap.go:608-636`, `sandbox_exec.go:172-194`.
3. **§3.2 `Isolator.HostExecEnv` (or equivalent)** if A.1 has landed; else
   leave as-is — already shared at the right boundary.
4. **§3.S3/§3.S4 `AppendStandardEnv` helper.** Consolidates the
   `AgentEnvVars` + `RuntimeEnv` emission. Two call sites today, three when
   #1017 lands.
5. **§3.3 Extend `writeGitconfig` to handle sandbox-exec.** Add the third
   mode value (or reuse `isolationBwrap` if `sandboxHome` returns the same
   value); add the `PrepareSandboxExec` call. Behavioural change only for
   sandbox-exec; bwrap and podman unaffected.
6. **§3.6 `SuperviseChild` helper** — depends on #1018 (PR 4 of
   sandbox-exec) introducing the supervised-child sandbox-exec lifecycle.
   Until then there is nothing to share. When #1018 lands, factor the
   bwrap supervisor block into `SuperviseChild` and have both dispatch
   paths call it.
7. **§3.1 / §3.S6 `MountSpec` shape and `StandardSandboxMounts` walk.**
   This is the biggest extraction (collapses ~700 LoC across the three
   files). Best left until after #1017 has wired sandbox-exec's mounts —
   then all three modes converge on the same spec at the same time.
8. **§3.S5 per-mode `SandboxContextEnv`.** Folds into the §3.S3 helper.
   Same timing.
9. **§3.4 / §3.7 / §3.8** — no extraction proposed; the divergences are
   genuine (terminfo, shutdown semantics, readiness wait). The
   `Capabilities` flags A.1 proposes (`PassesHostTERM` if added,
   `NeedsReadinessWait`) handle the call-site dispatch.

**Dependencies.** Items 1, 2, 3, 4 are mutually independent and can land in
parallel. Item 5 depends on no others. Item 6 depends on #1018. Items 7
and 8 depend on #1017 (sandbox-exec staging) and ideally land *after* it
so the sandbox-exec emitter is built once, not twice. None of this work
depends on A.1 landing first — the helpers all live below the proposed
interface, in `internal/container`, and A.1's interface widens later to
expose the polished surface.

## 5. Where each helper lives relative to A.1's `Isolator` superset

A summary table for ease of reference:

| Helper / change | Lives ON A.1's `Isolator`? | Or BELOW it? |
|---|---|---|
| `MountSpec` shape + `StandardSandboxMounts` walk + per-mode appenders (§3.1, §3.S6) | No | **Below** — `internal/container/mounts.go` (new file). The per-mode appender is a method on each isolator struct, not on the interface. |
| `MinimalIsolatedExecEnv` (§3.2) | A.1 superset *should* expose it as `Isolator.HostExecEnv` (Phase 2 add) | The body lives **below** in `sandbox_exec.go:246` (already shared). |
| `writeGitconfig` (§3.3) | **On** — A.1 §4.5 proposes `Isolator.WriteGitconfig(...)` | The body stays in `container.go:526-625` and continues to use `sandboxHome(mode)`. |
| TERM passthrough (§3.4) | **As a `Capabilities` flag** — `Capabilities.PassesHostTERM` (suggested addition) | If a helper materialises, it lives below in `internal/container/env.go`. |
| Worktree mount semantics (§3.5) | No | **Below**, expressed via `MountSpec`. |
| `SuperviseChild` (§3.6) | No — agent-run concern | **Below A.1**, in `cmd/agent_run.go` or new `internal/agentrun/supervisor.go`. |
| Graceful shutdown body (§3.7) | The interface method `Shutdown()` is **on** A.1 | The shared body (`SIGTERM → grace → SIGKILL`) is a free function in `internal/container/lifecycle.go`. |
| Readiness wait (§3.8) | **As `Capabilities.NeedsReadinessWait`** — A.1 §4.3 already lists this | No body to share (absence-of-code). |
| `tempPath(stem)` (§3.S1) | No | **Below**, on `Manager`. |
| `HarnessInvocation(cfg)` (§3.S2) | No | **Below**, in `internal/container/harness_args.go`. |
| `AppendStandardEnv` (§3.S3/S4) | No | **Below**, in `internal/container/env.go`. The per-mode appender stays on each isolator. |
| `SandboxContextEnv` per-mode (§3.S5) | No | **Below**, as a per-isolator method. |

## 6. Open questions and `[uncertain]` flags

Consolidated for visibility:

1. **§3.1 — `MountSpec` directory-vs-file distinction.** The host-API
   socket-dir bind needs a directory mount, not a file mount. A second
   `MountSpec` field (`MountAsDirectory bool`) would model it; verifying
   the field is needed by both bwrap and podman (and sandbox-exec when
   #1017 lands) requires the implementation to actually attempt unification.
2. **§3.4 — TERM handling.** Whether sandbox-exec's implicit-pass-through
   (via `MinimalIsolatedExecEnv`) is *equivalent* to bwrap's
   explicit-`--setenv` forward, or whether the `--clearenv`-equivalent
   behaviour expected in #1017 will create a divergence. Cannot tell from a
   static read; would need a Darwin-host run.
3. **§3.5 — `WorktreeReadOnly` for bwrap and sandbox-exec.** Today only
   podman implements it; bwrap defers (`bwrap.go:148-150`) and
   sandbox-exec is silent. Out of A.2 scope; flagged for the parallel
   review-agent track.
4. **§3.6 — SIGWINCH on sandbox-exec.** The bwrap `forwardSignalsToBwrap`
   passes `nil` for `onWinch`; sandbox-exec's `syscall.Exec` model passes
   the existing terminal directly. Whether a future supervised-child
   sandbox-exec lifecycle (#1018) will need a SIGWINCH callback for
   Bubble Tea's TIOCGWINSZ-on-stdout requirement is unknown without
   running it on macOS.
5. **§3.8 — Readiness on sandbox-exec.** Whether macOS notarisation /
   first-run codesign delays warrant a sandbox-exec-specific gate. Cannot
   determine from a static read.
6. **§3.S2 — Harness invocation tail ownership.** Whether
   `HarnessInvocation(cfg)` belongs in `internal/container` or
   `internal/harness/opencode/adapter.go`. Track B.1 (harness coupling) is
   the right venue.
7. **Timing of `MountSpec` rollout.** Optimistic order says "land after
   #1017 so the sandbox-exec emitter is built once". The pessimistic
   alternative is to land it *before* #1017 so #1017 has a target shape
   to extend. Either is defensible; the audit's preference is post-#1017
   to avoid double-implementation.

## 7. Acceptance-criteria self-check

- [x] Document at `modules/programs/prism/prism/docs/reviews/A2-isolation-implementation-duplication-audit.md` exists.
- [x] All eight behavioural concerns covered (mount-prep §3.1; env-allow-list §3.2; gitconfig §3.3; terminfo §3.4; worktree §3.5; signals §3.6; shutdown §3.7; readiness §3.8) plus seven supporting concerns (§3.S1–§3.S6).
- [x] For each concern, file:line citations are given for all three modes (or the absence is documented — e.g. sandbox-exec does not implement gitconfig today; sandbox-exec is not mentioned in `internal/sidecar/sidecar.go`).
- [x] Each concern is tagged `must-differ`, `incidentally-different`, or `already-shared`. The §2 table is the consolidated tagging; §3 expands each.
- [x] For every `incidentally-different` concern, a proposed shared helper signature is presented (§3.1 `MountSpec`/`StandardSandboxMounts`; §3.3 extended `writeGitconfig`; §3.6 `SuperviseChild`; §3.S1 `tempPath`; §3.S2 `HarnessInvocation`; §3.S3/S4 `AppendStandardEnv`; §3.S5 `SandboxContextEnv`; §3.S6 falls under §3.1).
- [x] For every `must-differ` concern, the reason is documented in 1–3 sentences (§3.4 podman terminfo, §3.5 worktree path conventions, §3.6 podman signal model, §3.7 podman container lifecycle, §3.8 podman HTTP healthcheck).
- [x] `[uncertain]` flags are present where the worker could not determine the answer without running both modes — consolidated in §6.
- [x] Document contains zero implementation work — markdown only, no Go file added or modified outside `docs/reviews/`.
- [x] PR body will include `Closes #1077`.
- [x] `go build ./...` and `go test ./...` from `modules/programs/prism/prism/` continue to pass (pre-write baseline ran clean; no Go source modified).
