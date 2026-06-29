# Agents Project Guidance: nixos-config

This document provides guidance for AI agents on how to interact with this NixOS configuration repository.

## Application Configuration Lives in This Repo

Many applications — including prism, zsh, neovim, and others — are configured as Nix modules in this repository rather than having config files in `~/.config/` or other dotfile locations. **Before reading external config files or fetching docs to understand how an application is configured, search this repo first.**

For example:
- pi (agent runtime) configuration → `modules/programs/prism/pi.nix`
- Custom agent markdown files → `modules/programs/prism/agents/`
- Skills → `modules/programs/prism/skills/`
- Agent files at `~/.config/prism/agents/` and skills at `~/.config/prism/skills/` — **never edit these directly, they are overwritten on every switch**

If you need to understand how something is configured, `grep` or `glob` within the working directory before reaching outside it.

## Project Overview

- **Primary Goal:** This repository manages the personal NixOS configurations for multiple machines, all intended for a single user (`prismatic-koi`).
- **Design Philosophy:**
    - Configurations are managed with Nix Flakes.
    - Home Manager and NixOS options are often configured together within the same module for simplicity.
    - The system aims for impermanence, with state managed via `impermanence`.
    - Secrets are managed with `sops-nix` and an age key.
    - The `unstable` channel is preferred for packages. Overlays are used to pin packages to `stable` or other versions only when necessary.

## Prism

Prism is a tmux-based AI development environment that is developed and configured within this repository. Its source code and configuration are located under `modules/programs/prism/`:

- **Go CLI source**: `modules/programs/prism/prism/` — the `prism` binary (spawn, checkin, prompt, dashboard, etc.)
- **Tmux configuration**: `modules/programs/prism/tmux.nix`
- **Pi agent configuration**: `modules/programs/prism/pi.nix`
- **Custom agents**: `modules/programs/prism/agents/`
- **Skills**: `modules/programs/prism/skills/`

**Isolation modes.** The valid prism isolation modes are `bwrap` (Linux), `sandbox-exec` (Darwin), and `host` — the source of truth is `config.ValidIsolationModes` in `modules/programs/prism/prism/internal/config/config.go`. Podman is **not** a prism isolation mode; it was removed some time ago (issue #2189 cleaned up the stale references). The only remaining `podman` strings in prism source are legacy DB-row fallbacks (and the schema migrations that backfill them) which convert old `isolation_mode='podman'` rows to bwrap at read time — do not reintroduce podman as a mode, and do not "fix" those fallbacks without a dedicated audit. (Podman the container runtime is still used on these machines — `modules/programs/podman.nix` — that is unrelated to prism isolation.)

When making changes to prism Go source, always build and test before committing:

```bash
# From modules/programs/prism/prism/
go build ./...
go test ./...
```

This is faster than a full nix build and should be the first check for any prism code change.

**Prism Go-source and `.nix` changes: the homeless-shelter gate is enforced by CI.**

The gate now runs in CI, not locally. Any PR that touches `modules/programs/prism/prism/**`, `pkgs/prism.nix`, `**/go.mod`, `**/go.sum`, or `.github/workflows/pr-gate.yml` triggers:

- the `go-tests` CI job — runs `go test ./... -race` from `modules/programs/prism/prism/` on a Linux runner with bwrap available; and
- the `nix-build-prism-checked` CI job — builds prism with `runChecks = true` so the test suite executes inside the nix sandbox (`$HOME=/homeless-shelter`).

Both jobs must pass before merge. They live in `.github/workflows/pr-gate.yml`. The required status check on `main` is `pr-gate`, which is a fan-in job that explicitly fails if either `go-tests` or `nix-build-prism-checked` did not succeed — so a failure in either is a hard block.

**Pipeline split (issue #1494).** Go test execution is split from the default `nix build` so that local `nh switch` and `nix build .#prism` are fast:

- `.#prism` — default attribute, `runChecks = false` (so `doCheck = false`). Used by `nixosConfigurations`, `darwinConfigurations`, and local `nh switch`. No Go tests run.
- `pkgs.prism.override { runChecks = true; }` — same derivation with `doCheck = true`. Runs the Go suite inside the nix sandbox so the homeless-shelter signal is preserved. Built by the `nix-build-prism-checked` CI job. Not exposed as a flake output, so `nix flake check` does not pay for the test phase on every PR.

**Local pre-PR self-check (recommended).** Before pushing a prism-touching PR, run:

```bash
# From modules/programs/prism/prism/
gofmt -l .
go build ./...
go test ./...

# From the repo root
nix build .#prism
```

This catches build/test failures fast. The full homeless-shelter signal is then exercised by CI on the PR. `gofmt -l .` matches the `gofmt check` step in `pr-gate.yml` (added in #2282) — list mode (`-l`) shows what would change without mutating; once you're ready to fix any reported files, re-run with `-w .`.

**Test-suite isolation (issue #1608).** The test suite under `modules/programs/prism/prism/internal/sidecar/` is fully isolated from host bus / DB / tmux state:

- Tests that construct a `sidecar.Sidecar` must use `sidecartest.NewIsolated(t, ...)` which redirects `$XDG_STATE_HOME` to a `t.TempDir()` and sets the `PRISM_TEST_MODE_RESTRICT_HOSTAPI` guard.
- Test session names use the `prism-test@` prefix, never `nixos-config@main` or any other slug that matches a live coordinator on the developer's host.
- Running `go test ./...` will not deliver any notification to a live coordinator, write to the real `prism.db`, create files under `$XDG_STATE_HOME/prism/run/`, or invoke `tmux` against the host server.

If you add a new test that exercises notification delivery, use `sidecartest.NewIsolated` — do not construct a `sidecar.Config` that touches the host environment. If you want to reproduce CI's checked build locally:

```bash
# From the repo root
nix build --impure --no-link \
  --expr '(builtins.getFlake (toString ./.)).packages.x86_64-linux.prism.override { runChecks = true; }'
```

The `go-tests` job catches race conditions and integration-test failures the nix sandbox masks (e.g. tests that `t.Skip` when bwrap is unavailable). The `nix-build-prism-checked` job catches the homeless-shelter failure class.

**Why this gate exists — the homeless-shelter failure class.** The Nix build runs the test suite inside a sandbox where `$HOME=/homeless-shelter`, an intentionally unwritable path. This catches tests that touch the user's actual home directory and pass in a normal dev shell but fail in the sandbox:

- `os.MkdirAll` on a path derived from `$HOME` or an unset `$XDG_STATE_HOME`
- `os.UserHomeDir()` followed by a write
- Opening a Unix socket under `~/.local/state/...`

This is not a hypothetical: PR #1455 (`TestDeliverToSession_PiPath_DeliverAsForwarded`) introduced exactly this failure. `go test ./...` passed, `prism review` passed, the PR merged — and main went red on the next `nh switch` because the test created a directory under `$HOME`. The fix was a one-liner, but the break surfaced only in the Nix sandbox.

**Scope — this gate applies only to prism-touching PRs.** PRs that touch only non-prism files (other modules, dotfiles, docs) do **not** trigger the `go-tests` or `nix-build-prism-checked` jobs. The relaxation introduced in #1441 stands for those paths.

### In-sandbox nix validation on this repo (flake CLI and trusted-settings)

This repo's `flake.nix` has a `nixConfig` block (`extra-substituters` /
`extra-trusted-public-keys`). Any flake-CLI nix command therefore makes nix
consult `~/.local/share/nix/trusted-settings.json` in the **real** home
(inside a sandbox, `XDG_DATA_HOME` points at the real `~/.local/share`).
`--no-accept-flake-config` does NOT avoid the lookup.

As of issue #2201 the sandbox-exec profile grants **read-only** access to
that single file, so flake-CLI commands (`nix build .#prism --dry-run`,
`nix flake metadata`, the pre-PR `nix build .#prism`, …) work inside worker
sandboxes. Notes:

- The allowance applies to sandboxes **spawned after the fix is deployed**
  (next `nh switch`). In a sandbox pre-dating it, flake CLI fails with
  `error: opening file "…/trusted-settings.json": Operation not permitted`.
  The sanctioned fallback for eval-level validation in that case is the
  non-flake pattern, which does not process flake `nixConfig` and so
  sidesteps the read entirely:

  ```bash
  # From the repo root
  nix-instantiate --eval --expr 'builtins.getFlake (toString ./.)'
  ```

- A `warning: ignoring untrusted flake configuration setting` from the flake
  CLI is harmless — it means the host trust list does not (yet) cover this
  repo's settings; eval proceeds without the extra substituters.
- Do not try to accept-and-persist flake config from inside a sandbox
  (e.g. answering an interactive trust prompt with "permanently mark"):
  the write path to `trusted-settings.json` is still denied, by design.
- Never reach for env overrides when nix misbehaves in a sandbox — see the
  next section.

### When `nix build` fails inside a sandbox

The local `nix build .#prism` is a *pre-PR* check, not the authoritative
gate — CI runs the homeless-shelter build (`nix-build-prism-checked`) on
every prism-touching PR and that is the build that must be green for merge.
A worker MAY push without a green local build provided the PR description
says so.

**If `nix build .#prism` fails inside a worker sandbox, escalate via
`prism escalate`. Do not attempt environment workarounds.**

Specifically, do NOT override any of `XDG_DATA_HOME`, `NIX_STORE_DIR`,
`NIX_DATA_DIR`, or `HOME` to try to "isolate" or "reset" nix between
retries. Pointing nix's local profile / trust DB / daemon-socket linkage at
an empty tempdir forces nix to bootstrap a fresh single-user store, which
opens large numbers of file descriptors and adds real pressure to the
host-wide FD pool. During the #2180 incident the host had no headroom to
absorb that: the root cause was kitty's kitten config watcher recursively
kqueue-watching `/nix/store` — one open FD per store entry — which had
pre-consumed nearly the entire system-wide `kern.maxfiles` pool (see issue
#2198). The env-override retries were marginal extra pressure on an
already-exhausted pool, and once `kern.num_files` reaches `kern.maxfiles`,
*every* process on the host that calls `open()` fails (Karabiner, Chrome,
Finder, the agent's own harness) and recovery requires a reboot. This
actually happened — see issue #2180 for the incident retrospective (its
causal story is superseded by #2198). The guidance stands regardless of
the root cause: escalate, don't work around.

The pi extension's pre-tool-call deny list (`BLOCKED_BASH_PATTERNS` in
`modules/programs/prism/pi/extensions/prism.ts`) also blocks this command
shape as defence in depth; if you see that block fire, the correct response
is still `prism escalate`, not a different workaround.

### Darwin FD-exhaustion defences (#2180 class)

`modules/darwin/sysctls.nix` is the **Layer 2** (host-wide) defence against
#2180-class FD exhaustion (parent: #2181). It raises `kern.maxfiles` to
524288 and `kern.maxfilesperproc` to 262144 via a boot-time launchd daemon
(`RunAtLoad`, so the values survive reboots — `/etc/sysctl.conf` is not
read at boot on modern macOS) paired with an activation script (so the
values apply during `darwin-rebuild switch` without a reboot). Both paths
are idempotent and never lower a sysctl that is already at or above target.
This is headroom only — the root-cause leak is the kitten `/nix/store`
watcher, tracked in #2198. Do not raise these values further to absorb
that leak.

The **Layer 1** (per-process) defence pairs with it: agents spawned via the
bwrap and sandbox-exec exec paths get a bounded `RLIMIT_NOFILE` — defaults
soft 8192 / hard 16384, named constants `DefaultAgentMaxOpenFilesSoft` /
`DefaultAgentMaxOpenFilesHard` in `internal/config` (issue #2190). The hard
cap is kernel-enforced: an agent cannot raise it with `ulimit -n` from
inside its sandbox. Tune per-machine via the `agentMaxOpenFilesSoft` /
`agentMaxOpenFilesHard` options on `modules/programs/prism/prism-tui.nix`
(rendered into config.json as `agent_max_open_files_soft` /
`agent_max_open_files_hard`). Host-mode agents are deliberately uncapped and
inherit the host's limits. Layer 1 makes the "one agent runs away with FDs"
class structurally impossible; it is defence-in-depth, not the #2180
root-cause fix.

### sandbox-exec testing convention

Any change to `internal/container/sandbox_exec.go::generateProfile`,
`Manager.PrepareSandboxExec`, or `Manager.PrepareSessionWorkDir` must be paired
with a Darwin-only integration test under `internal/integration/` that invokes
`/usr/bin/sandbox-exec` against a Nix-built test binary, plus a negative test
that mutates the profile to prove the positive is not a no-op. Substring
assertions on profile content are necessary but not sufficient. See
`modules/programs/prism/prism/docs/sandbox-exec-testing.md` for the full
convention and helpers (issue #1192).

### stdout-capture testing convention

Any test helper that redirects `os.Stdout` or `os.Stderr` through an
`os.Pipe` must drain the read end concurrently with the function under test —
otherwise a single write larger than the kernel pipe buffer (16 pages ≈ 64 KiB
on Linux) deadlocks the writer until `go test`'s timeout fires. The
`agent-context` JSON output (~69 KiB) is the current worst offender and the
reason this gap surfaced. See
`modules/programs/prism/prism/docs/stdout-capture-testing.md` for the full
convention and the canonical `captureStdout` helper (issue #1798).

### Setting WIP aside — do not use git stash

**Worker-class prism sessions must never use `git stash`** (any subcommand
— `stash -u`, `stash pop`, `stash apply`, `stash list`, …). In the
bare+worktree layout the stash stack (`refs/stash` + its reflog) lives in
the shared bare repo, so it is repo-wide, not per-worktree. Two sessions
that stash concurrently race on a single LIFO stack — `git stash pop` takes
whatever is at `stash@{0}`, which may belong to another worktree. On
2026-06-11 two concurrent workers' pops crossed and silently swapped their
WIP (issue #2202). The pi extension's deny list (`BLOCKED_BASH_PATTERNS` in
`modules/programs/prism/pi/extensions/prism.ts`) blocks `git stash` for
worker-class agents as defence in depth; the coordinator — the single
session on the main worktree — is exempt and is then the only prism writer
to the shared stack.

Sanctioned WIP-set-aside patterns — both are worktree-local:

- **Temp commit** (preferred — commit history is disposable on squash-merged
  branches):

  ```bash
  git add -A && git commit -m wip   # set WIP aside
  # ... do the other thing (e.g. rerun a test against HEAD~1 via a checkout) ...
  git reset --soft HEAD~1           # restore: changes return, staged
  ```

- **Patch file**:

  ```bash
  git diff > /tmp/wip.patch && git restore .   # set WIP aside
  # ... do the other thing ...
  git apply /tmp/wip.patch                     # restore
  ```

The "verify your tests aren't no-ops" discipline (revert fix → rerun test →
re-apply) is the common trigger for reaching for a stash — use one of the
patterns above instead.

### Podman support for workers

A worker spawned with `prism spawn --containers` gets a filtered podman API
socket exposed inside its sandbox so it can spin up throwaway containers
(integration-test Postgres, build-matrix toolchains, etc.) without escaping
the sandbox. The real host podman socket is NEVER bound into the worker's
sandbox — the only socket reachable is a per-session **filtering proxy**
that enforces a default-deny policy at six layers before any byte reaches
the upstream. The full security spec — threat table, layer-by-layer
model, field-admission process for future docker-/podman-API fields, and
the audit-log debugging notes — lives at
[`modules/programs/prism/prism/docs/podman-proxy.md`](modules/programs/prism/prism/docs/podman-proxy.md);
this section is the operational summary.

The feature is opt-in and per-spawn: omit `--containers` and the worker
behaves identically to before the train landed (no proxy goroutine, no
`CONTAINER_HOST` / `DOCKER_HOST` env, no scratch bind, no audit-log file).
The train is tracked by #2317 and shipped as #2326, #2327, #2328, #2329,
#2330, #2331, #2332, and the closing docs/housekeeping PR.

#### Using `--containers`

```bash
prism spawn --containers --prompt 'run the test suite against a throwaway postgres'
```

The flag flips two DB columns at spawn time —
`spawn_inputs.containers_flag = 1` (audit) and
`agent_status.containers_enabled = 1` (runtime gate). The sidecar reads
the runtime gate on startup and conditionally:

- binds a fourth Unix listener at
  `<XDG_STATE_HOME>/prism/run/<sessionDirName>/podman.sock`,
- exposes that socket inside the sandbox (bwrap bind / sandbox-exec
  SBPL literal allow),
- injects `CONTAINER_HOST=unix://<…>/podman.sock` and the matching
  `DOCKER_HOST` env vars,
- conditionally binds a per-session scratch dir at
  `<sessionDir>/container-scratch/` into the sandbox, RW, so the agent
  has a writable place to point bind mounts that does not need to live
  in the worktree, and
- opens an audit-log file at `<sessionDir>/podman-proxy.log` (one JSON
  line per request: timestamp, method, endpoint, decision, reason).

The `--containers` flag is independent of `--isolation`. Combining
`--containers --isolation host` produces a warning (host mode has direct
podman access already, the proxy is redundant) but does not error — see
#2330 for the rationale.

#### Default-deny at six layers (summary)

The proxy's job is to make the threat table in
[`docs/podman-proxy.md`](modules/programs/prism/prism/docs/podman-proxy.md#2-threat-model)
true for an attacker controlling the agent. Six independent layers do
that work; each was added or inverted in response to a class of finding
in one of the six review-security cycles of PR #2326:

1. **Endpoint layer** — positive allowlist via `classifyRequest`; any
   unknown path/method pair rejects.
2. **Field-name layer** — `json.Decoder.DisallowUnknownFields()` + typed
   struct per body-bearing endpoint. The structs in
   `internal/podmanproxy/policy.go` (`hostConfig`,
   `containerCreateBody`, `containerExecBody`, `volumeCreateBody`,
   `networkCreateBody`) are the canonical security spec — every
   admitted field is annotated `INSPECTED` / `DENIED` / `FORWARDED`
   with a rationale comment.
3. **Field-value layer** — per-field literal allowlists for
   enumerable values (`Mount.Type`, the six `*Mode` fields,
   `LogConfig.Type`). Closes the cycle-6 CRITICAL where
   `Mount.Type=glob` was forwarded because the parser was deny-list,
   not allowlist.
4. **Body-content layer** — `checkHostConfig` walks the parsed
   HostConfig and rejects dangerous values (`Privileged: true`, host
   namespaces, non-empty `Devices`/`DeviceCgroupRules`/`DeviceRequests`/
   `VolumesFrom`, present `MaskedPaths`/`ReadonlyPaths`, non-empty
   `Sysctls`, cap-add outside allowlist, `SecurityOpt` outside
   allowlist, resource caps in strict mode).
5. **Path-resolution layer** — `filepath.EvalSymlinks` on both bind
   sources AND allowlist entries before the prefix comparison.
   Relative paths, broken symlink chains, and non-existent sources all
   deny. Closes the cycle-3 CRITICAL symlink-bypass.
6. **Query layer** — `PUT /containers/{id}/archive` `path` query
   parameter is checked against the same bind-source allowlist with
   the same canonicalised-prefix logic. `POST /containers/create?name=`
   is covered by the container-name auto-prefix policy.

A residual TOCTOU window remains between `EvalSymlinks` and runc's
`mount(2)`. It is accepted for v1 because the agent is otherwise
sandboxed and the bind target inside the container is fixed at create
time. Future hardening directions (Linux `nosymfollow` /
Darwin SBPL restriction on the scratch dir) are documented in the proxy
doc, §5.

#### Field-admission process

The cycle-5 schema inversion makes every NEW docker-/podman-API field
upstream **rejected by default** until it has been audited and admitted
to the relevant typed struct in `internal/podmanproxy/policy.go`. A
workflow that surfaces a needed field sees a 403 with `"unknown field
<Name>"` in the response body and matching audit-log line. The process:

1. File an issue describing the workflow and the field.
2. Audit the field against the threat table in
   [`docs/podman-proxy.md` §2](modules/programs/prism/prism/docs/podman-proxy.md#2-threat-model).
   Classify as `INSPECTED` / `DENIED` / `FORWARDED`.
3. Open a follow-up PR adding the field to the struct in `policy.go`
   with a rationale comment matching the existing style. `INSPECTED`
   additions need a positive + revert-and-watch-fail test pair as
   proof the new check is not a no-op.

Do NOT loosen the policy in a worker PR without an audit — the struct
is the security spec, and the cycle-6 history demonstrates that quiet
field admissions are how CRITICALs ship.

#### Platform prerequisites

**NixOS.** `virtualisation.podman.enable = true` (already set in
`modules/programs/podman.nix`) wires the rootless socket-activated user
unit, so the upstream socket is available as soon as the user systemd
instance has `podman.socket` in `sockets.target`. The user account
needs to be in the `podman` group — this is set in
`modules/system/users.nix` so it applies to every NixOS host built
from this flake. If the upstream socket is not active when the proxy
first dials, the proxy stays up and synthesises a friendly 503 with
actionable text per request until the upstream comes up:

```
{"message": "podman socket unavailable: <reason>; on macOS run 'podman machine start', on Linux check 'systemctl --user status podman.socket'"}
```

**No linger for v1.** `loginctl enable-linger <user>` is NOT set on any
host in this flake. The proxy is only used by interactive prism
sessions; users running long-lived background agents that need podman
across SSH-out can re-evaluate later. See
[`docs/podman-proxy.md` §10](modules/programs/prism/prism/docs/podman-proxy.md#10-linger-decision).

**Darwin.** `nix-darwin` ships no first-class podman-machine module. The
machine is user-managed:

```bash
podman machine init      # one-time setup
podman machine start     # per boot
```

If the machine is not running when the worker dials, the proxy returns
the same friendly 503 envelope above. Bring the machine up and retry
— no restart of the worker session is required, the proxy re-dials
lazily.

An optional launchd user agent that runs `podman machine start || true`
at login is an obvious follow-up if friction warrants. It is
intentionally NOT shipped in this train.

#### Debugging rejections

Every request the proxy sees writes exactly one JSON line to
`<XDG_STATE_HOME>/prism/sessions/<instance_id>/podman-proxy.log` —
resolve the `<instance_id>` for a session by reading the
`agent_status.instance_id` column from `prism.db` (e.g. `sqlite3
~/.local/state/prism/prism.db "SELECT instance_id FROM agent_status
WHERE name = '<session>'"`). The structured `reason` field names the
specific policy check that fired; see
[`docs/podman-proxy.md` §7](modules/programs/prism/prism/docs/podman-proxy.md#7-troubleshooting--reading-the-audit-log)
for the common rejection classes and how to read them.

### File naming and organisation

Names of files and directories should be in lowercase, with dashes between words — kebab case, not camel case.
For instance, it should be `all-packages.nix`, not `allPackages.nix` or `AllPackages.nix`.

### Formatting

All Nix files should be formatted using `nixfmt`:
```
nixfmt .
```

### Secrets Management

- Secrets are co-located with the modules that use them (e.g., `modules/qutebrowser/secrets/`).
- The public age key for encryption is located in the root `.sops.yaml` file. Do not ask for this key.
- When adding a new secret, create a new `.sops.yaml` file in the appropriate module directory.

### GitHub repository rules

Direct push to `main` is blocked by the repository ruleset. All changes must go through a pull request. Never attempt to push directly to main.

**Merge method:** The only allowed merge method is squash merge. Coordinators merging a PR must use:

```bash
gh pr merge <number> --squash
```

Never use `--merge` (creates a merge commit, rejected by the ruleset) or `--rebase` (creates individual commits rather than a squash, also rejected by the ruleset).

**Branch deletion:** Do not pass `--delete-branch` to `gh pr merge`. Branch deletion after merge is handled automatically by GitHub (`delete_branch_on_merge` is enabled at the repo level). Passing `--delete-branch` may cause an API error if the branch is already gone.

**Build agents:** If you are working on a feature branch, open the PR with `gh pr create` and do not attempt to merge it. The coordinator on `@main` handles merging.

### General Workflow Principles

- **Atomic Changes:** Group all related modifications (e.g., creating a new module, importing it, and removing the old package entry) into a single logical change and commit them together.
- **Git Tracking for Nix:** New files must be added to Git (and ideally committed) *before* Nix commands (like `nix build` or `nix flake check`) can recognize them.
- **Efficiency:** Build commands can be time-consuming. Use them judiciously, only after a complete set of related changes has been applied, and then await user feedback before further iterations. Do not use them as part of an iterative debugging process unless explicitly instructed.
- **Trusting User Feedback:** If the user confirms a fix, trust that feedback and move on, rather than attempting further "fixes" based on assumptions.
