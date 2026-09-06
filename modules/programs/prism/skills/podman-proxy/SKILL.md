---
name: podman-proxy
description: |
  Operational guide for the prism podman-proxy — the filtered podman API
  socket a worker gets from `prism spawn --containers`. Load this skill when
  you spawn or run a worker with `--containers`, when a container workflow
  hits a proxy 403 / 503, when you need to admit a new docker-/podman-API
  field, or when you debug the podman-proxy audit log. The full security spec
  is `modules/programs/prism/prism/docs/podman-proxy.md`; this skill is the
  operational summary.
---

# Podman support for workers

A worker spawned with `prism spawn --containers` gets a filtered podman API
socket exposed inside its sandbox so it can spin up throwaway containers
(integration-test Postgres, build-matrix toolchains, etc.) without escaping
the sandbox. The real host podman socket is NEVER bound into the worker's
sandbox — the only socket reachable is a per-session **filtering proxy**
that enforces a default-deny policy at six layers before any byte reaches
the upstream. The full security spec — threat table, layer-by-layer
model, field-admission process for future docker-/podman-API fields, and
the audit-log debugging notes — lives at
`modules/programs/prism/prism/docs/podman-proxy.md`; this skill is the
operational summary.

The feature is opt-in and per-spawn: omit `--containers` and the worker
behaves identically to before the train landed (no proxy goroutine, no
`CONTAINER_HOST` / `DOCKER_HOST` env, no scratch bind, no audit-log file).
The train is tracked by #2317 and shipped as #2326, #2327, #2328, #2329,
#2330, #2331, #2332, and the closing docs/housekeeping PR.

## Using `--containers`

```bash
prism spawn --containers --prompt 'run the test suite against a throwaway postgres'
```

## Resource caps: `--memory` and `--cpus` are mandatory

**Every `podman run` through the proxy must pass `--memory` and
`--cpus`. A command that omits either one gets a 403.**

```bash
# Correct.
podman run --rm --memory 512m --cpus 1 alpine echo hello

# Rejected: audit reason memory_required.
podman run --rm --cpus 1 alpine echo hello

# Rejected: audit reason nano_cpus_required.
podman run --rm --memory 512m alpine echo hello
```

The two 403 reason strings for a missing field are **`memory_required`**
and **`nano_cpus_required`**.

The per-container caps are 4 GiB of memory (`MaxMemoryBytes`) and 2 CPUs
(`MaxNanoCpus`). A request above a cap gets 403 with reason
`memory_over_cap` or `nano_cpus_over_cap`. A request that passes `0` for
either gets `memory_nonpositive` or `nano_cpus_nonpositive`, because `0`
means "unbounded" in docker semantics and bypasses the cap.

A container started through the proxy is a host process. It runs outside
the agent's sandbox, so the caps are the only bound on what it consumes.

Do NOT add `--cpu-quota` alongside `--cpus`, and do not set
`Config.MaxCPUQuota`. The two fields express the same limit, clients
refuse to send both, and a configured cap makes its field mandatory — so
a proxy with both caps set rejects every create request. See
`docs/podman-proxy.md` §8.1.

## Per-session naming and cleanup

Containers and volumes both carry the per-session prefix
`prism-<session>-`. Omit the name and the proxy injects
`prism-<session>-<8 hex chars>`. Supply a name outside the prefix and the
request gets 403 (`name_prefix_mismatch_body` for a container,
`volume_name_prefix_mismatch` for a volume).

`prism cleanup` then removes, for a session that enabled containers:

- containers matching `prism-<session>-<8 hex chars>`,
- volumes whose name starts with `prism-<session>-`,
- every image the session pulled through its own proxy.

The image sweep replays a per-session ledger. It is not a prune, because
the image store is shared. An image another container still uses stays in
place and cleanup continues. The three counts appear in the
`prism cleanup --json` envelope as `containers_swept`, `volumes_swept`,
and `images_swept`.

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

## Default-deny at six layers (summary)

The proxy's job is to make the threat table in
`docs/podman-proxy.md` true for an attacker controlling the agent. Six
independent layers do that work; each was added or inverted in response
to a class of finding in one of the six review-security cycles of PR #2326:

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
   allowlist, resource caps in strict mode — which is now the
   production setting, see the section above).
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

## Field-admission process

The cycle-5 schema inversion makes every NEW docker-/podman-API field
upstream **rejected by default** until it has been audited and admitted
to the relevant typed struct in `internal/podmanproxy/policy.go`. A
workflow that surfaces a needed field sees a 403 with `"unknown field
<Name>"` in the response body and matching audit-log line. The process:

1. File an issue describing the workflow and the field.
2. Audit the field against the threat table in
   `docs/podman-proxy.md` §2. Classify as `INSPECTED` / `DENIED` / `FORWARDED`.
3. Open a follow-up PR adding the field to the struct in `policy.go`
   with a rationale comment matching the existing style. `INSPECTED`
   additions need a positive + revert-and-watch-fail test pair as
   proof the new check is not a no-op.

Do NOT loosen the policy in a worker PR without an audit — the struct
is the security spec, and the cycle-6 history demonstrates that quiet
field admissions are how CRITICALs ship.

## Platform prerequisites

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
across SSH-out can re-evaluate later. See `docs/podman-proxy.md`,
"Linger decision".

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

## Debugging rejections

Every request the proxy sees writes exactly one JSON line to
`<XDG_STATE_HOME>/prism/sessions/<instance_id>/podman-proxy.log` —
resolve the `<instance_id>` for a session by reading the
`agent_status.instance_id` column from `prism.db` (e.g. `sqlite3
~/.local/state/prism/prism.db "SELECT instance_id FROM agent_status
WHERE session_name = '<session>'"`). The structured `reason` field names the
specific policy check that fired; see `docs/podman-proxy.md` §7
for the common rejection classes and how to read them.

The same directory holds `podman-images.log`, the per-session image
ledger. It carries one JSON line per admitted `POST /images/create`.
Read it to find out which images `prism cleanup` will remove.
