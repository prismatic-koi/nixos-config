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
- opens an audit-log file at
  `<XDG_STATE_HOME>/prism/podman-audit/<instance_id>/podman-proxy.log`
  (one JSON line per request: timestamp, method, endpoint, decision,
  reason). The log sits outside the session work dir because the Darwin
  sandbox grants the agent write access over that whole subpath, and the
  agent is the subject of the record. `prism cleanup` removes the live
  audit directory for the session it cleans, but not before the archive
  step copies the log into the session's archive directory as
  `podman-proxy.log`, next to `agent-run.log` — the log is a retained
  record, not a live-debugging aid only. See `docs/podman-proxy.md` §7.

The `--containers` flag is independent of `--isolation`. Combining
`--containers --isolation host` produces a warning (host mode has direct
podman access already, the proxy is redundant) but does not error — see
#2330 for the rationale.

## First: the podman CLI cannot create containers or volumes

**`podman run` and `podman volume create` return 403 through this
proxy.** The podman CLI posts libpod bodies, and the proxy's typed
structs model the docker-compat shapes, so the request is rejected at
the schema layer:

```
podman run ...            -> create_top:unknown_field:json: unknown field "command"
podman volume create ...  -> volumes_create:unknown_field:json: unknown field "Label"
```

This is pre-existing behaviour, not a regression. It means the resource
caps and the volume-name policy below are reachable from the
**docker-compat surface only** — `DOCKER_HOST` plus a docker-API client.
Every reason string in the next two sections is unreachable from the
podman CLI, because the request never survives the decode that precedes
them. `docs/podman-proxy.md` §8.3 has the detail and the conditions to
close it. The work is tracked in issue **#2946** — do not file a
duplicate.

## Resource caps: memory and CPU limits are mandatory

**Scope: docker-compat surface only. See the section above.**

**A docker-API `POST /containers/create` must set both
`HostConfig.Memory` and `HostConfig.NanoCpus`. A body that omits either
one gets a 403.** On a docker-API client those are `--memory` and
`--cpus`.

```bash
# Correct, with DOCKER_HOST and a docker-API client.
docker run --rm --memory 512m --cpus 1 alpine echo hello

# Rejected: audit reason memory_required.
docker run --rm --cpus 1 alpine echo hello

# Rejected: audit reason nano_cpus_required.
docker run --rm --memory 512m alpine echo hello
```

The two 403 reason strings for a missing field are **`memory_required`**
and **`nano_cpus_required`**.

The per-container caps are 4 GiB of memory (`MaxMemoryBytes`) and 2 CPUs
(`MaxNanoCpus`). A request above a cap gets 403 with reason
`memory_over_cap` or `nano_cpus_over_cap`. A request that passes `0` for
either gets `memory_nonpositive` or `nano_cpus_nonpositive`, because `0`
means "unbounded" in docker semantics and bypasses the cap.

A container started through the proxy is a host process. It runs outside
the agent's sandbox, so the caps are the only bound on what it consumes
— and only on the fields they name. `MemorySwap` is forwarded and
uncapped.

**The caps are per container, and nothing caps the container COUNT.** N
containers consume up to N × 4 GiB and N × 2 CPUs. Issue #872 is the
post-mortem of a host crash that 15 concurrent containers caused, and
its conclusion was that per-container caps do not address fan-out. Keep
your container count small, and read `docs/podman-proxy.md` §8.3 before
you rely on the caps alone.

Do NOT add `--cpu-quota` alongside `--cpus`, and do not set
`Config.MaxCPUQuota`. The two fields express the same limit, clients
refuse to send both, and a configured cap makes its field mandatory — so
a proxy with both caps set rejects every create request. See
`docs/podman-proxy.md` §8.1.

## Per-session naming and cleanup

Containers and volumes both carry a per-session prefix. **You cannot
work it out from your session name** — it carries your session
incarnation's instance ID, which the sandbox does not expose. Omit the
name and the proxy injects `<prefix><8 hex chars>` for you. Supply a name
outside the prefix and the request gets 403
(`name_prefix_mismatch_body` for a container,
`volume_name_prefix_mismatch` for a volume) — and **the 403 message
states the prefix verbatim**, so a rejected request is the fastest way to
learn it.

Two ways to get the prefix from inside the sandbox:

```bash
# 1. Let the proxy name a volume, then read the name back.
curl -s --unix-socket "${DOCKER_HOST#unix://}" \
  -X POST -H 'Content-Type: application/json' -d '{}' \
  http://d/v1.41/volumes/create
# -> {"Name":"prism-<32 hex>-<session>-1a2b3c4d", ...}

# 2. Send a name you know is wrong and read the 403.
docker volume create pgdata
# -> ... does not start with the required prefix "prism-<32 hex>-<session>-"
```

**A volume you name in a container mount obeys the same rule.** A
`containers/create` body reaches a named volume through four
named-volume channels (see the "Per-session naming" table in
`docs/podman-proxy.md` §3 for the canonical list of the four, and the
deny reason for each). All four must start with the session prefix, or
the request gets 403.
These four named-volume channels REFUSE only — they never inject — so learn the
prefix first (above) and name the volume correctly in the request:

```bash
# Correct: the mount names an in-prefix volume, so the sweep finds it.
PREFIX=prism-3f2a1b0c123456789abcdef012345678-nixos-config-main-
docker run --rm --memory 512m --cpus 1 \
  -v "${PREFIX}pgdata":/var/lib/postgresql/data postgres:16

# Rejected: bind_volume_name_prefix_mismatch.
docker run --rm --memory 512m --cpus 1 -v pgdata:/data postgres:16
```

**The docker-compat `volumes` map key is a mount spec, not a container
path.** podman appends every key of that map to its `-v` list
verbatim, so `{"Volumes":{"/etc:/x":{}}}` is a host bind and
`{"Volumes":{"myvol:/data":{}}}` names a volume. A key with a host-path
source is checked against the bind allowlist
(`create_volumes_host_bind:<path>`), the same allowlist `HostConfig.Binds`
uses. A key with NO colon is a bare destination, names nothing, and
stays admitted — that is docker's own `{"/data":{}}` shape.

The rule also blocks a cross-session attach on all four named-volume channels:
another session's volume name on any of them is refused. It is not a
general isolation guarantee — `volumes/prune` and `DELETE
/volumes/{name}` are plain allows, so an agent can still remove any
volume on the host by name.

**The prefix is `prism-<instance token>-<folded session name>-`.** The
token is this session incarnation's instance ID (a UUID) with its hyphens
removed, 32 lowercase hex characters. The session-name half is FOLDED:
`container.ResourceNamePrefixForOwner` maps `@`, `/`, `.`, and `~` to
`-`, because podman validates a resource name against
`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` and rejects the rest.

The token is there because a name-only prefix cannot express identity.
Two distinct live sessions collide under it by FOLDING (`repo@feat/x` and
`repo@feat-x` fold to one prefix) and by NESTING (`foo` is a strict
prefix of `foo-bar`, so `prism-foo-bar-data` starts with `prism-foo-`),
so each could attach and sweep the other's volumes. Issue #2951 closed
both. The session-name half is DECORATION now — it is there so an
operator reading `podman ps` can tell which session a container came
from, and nothing decides ownership from it. `docs/podman-proxy.md` §3
carries the detail.

`prism cleanup` then removes, for a session that enabled containers,
every container and volume whose name carries the instance token of any
incarnation of that session that the database still holds — so a session
that was opened again after a close does not leak the volumes of its
earlier incarnation. An
incarnation older than the ninety-day `sessions` retention window is the
exception: its resources are skipped, with a warning naming the
resource, but they still leak. See "Known gaps" below. A resource
created before instance-ID naming carries no token and is swept by the
old rule instead (strict `prism-<session>-<8 hex chars>`
for a container, plain `prism-<session>-` prefix for a volume).

**Sweeping them is not the same as reaching them. A new incarnation makes
your earlier volumes unreachable by name.** The instance ID is per
INCARNATION, not per session name. `prism restart` and a `prism restore`
after a reboot keep the instance ID, so your prefix, your volumes, and the
container-scratch directory survive them. A new incarnation starts when
the session is opened again after it ended (for example, `prism switch`
after a close), and when `prism restore` cannot stop the old sidecar in
time. Then your prefix changes, and a mount that names a volume of the
earlier incarnation is refused with one of the three mount-channel reasons
above. The data is still on the host and cleanup still removes it. You
cannot attach it again.

So do not park state that must outlive the incarnation in a proxy-named
volume or in the container-scratch directory: both are keyed by the
instance ID. Hold that data in the session worktree, which every
incarnation shares, or re-create the volume under the new prefix and
re-seed it.

The two counts appear in the `prism cleanup --json` envelope as
`containers_swept` and `volumes_swept`.

### Known gaps

All five are accepted for this version. `docs/podman-proxy.md` §8.3
carries the detail and the conditions to close each one.

- **A resource whose owning incarnation is older than ninety days is
  skipped, and still leaks.** The sweep reads its token set from the
  `sessions` table, and `db.Prune` deletes a row ninety days after that
  incarnation ended. A close ends an incarnation; a `prism restore`
  continues it.
  The resource then matches no token the sweep holds, so cleanup skips
  it. `collectSweepable` warns when this happens — naming the resource
  and stating that its owning incarnation is not in the database — but
  the warning does not reach the resource; it still leaks. A
  long-lived session name that is closed and opened again often and is
  hard-cleaned rarely is the case that reaches it. Remove such a volume by hand with `podman
  volume rm`. Issue #2972 Part 1 shipped the warning; Part 2 tracks the
  retention question that would let the resource be reached again.

- **A resource created BEFORE instance-ID naming cannot be attributed.**
  Its name carries the legacy prefix and no token, so nothing recovers
  which of two colliding sessions created it. Cleanup still sweeps such a
  resource for its owning session in the ordinary case. When a LIVE
  session's legacy prefix equals or extends this one's, it leaves the
  name in place and warns rather than risking another session's data.
  Remove it by hand once the colliding session has ended. The population
  is bounded and does not grow.

- **An ANONYMOUS volume is not swept.** A docker-API
  `run -v /data ...`, a `Type=volume` mount with an empty `Source`, a
  docker-compat `Volumes` map key with no colon, or a libpod `volumes`
  entry with an empty `Name` makes the runtime create a
  volume and name it itself. The proxy has no name to police, so the
  volume carries no prefix and the sweep never finds it. Name the volume
  instead — see the section above — and the sweep reaches it. A NAMED
  volume created
  implicitly by a mount is no longer a gap: issue #2954 closed the
  `Binds` and `Type=volume` channels, and issue #2958 closed the
  libpod `volumes` array.
- **Images are not swept.** An image you pull stays in the shared host
  image store after the session ends. Two things must land first: the
  libpod `POST /images/pull` endpoint needs admission (which is also why
  `podman pull` returns 403 today, with audit reason
  `endpoint_not_allowed:POST images/pull`, and which issue **#2946**
  tracks), and the record of what to remove needs a home the agent
  cannot write to. A file under the session work dir is not one, because
  the Darwin sandbox grants the agent write access over that whole
  subpath — the `podman-audit` root the audit log uses is such a home.
- **No cap on the container count.** See the note above. The memory and
  CPU caps bound one container each, not the session's total.

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

### A field that carries a name opens a channel

If the field carries a container name or a volume name, the audit has a
fourth step: add a row to `namePolicyChannels` in `policy.go`, take the
audit reason from that row, and add a row to the "Per-session naming"
table in `docs/podman-proxy.md` §3. The channel set is declared once, in
that array, and the `podman-channel-table` doclint rule fails the build
while the table disagrees with it. A stale count in prose (a phrase of
the shape "the N named-volume channels") fails the
`podman-channel-count` rule the same way, wherever it lives — this
skill included. `docs/doclint.md` records
the rules and the count phrases they recognise. Issue #2974 is the
background.

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

**Read the audit log from a host shell.** No sandboxed session has read
access to the `podman-audit` root — that is the point of the location, and
an in-sandbox read gets EPERM. A `host`-mode session reads it directly.

Every request the proxy sees writes exactly one JSON line to
`<XDG_STATE_HOME>/prism/podman-audit/<instance_id>/podman-proxy.log` —
resolve the `<instance_id>` for a session by reading the
`agent_status.instance_id` column from `prism.db` (e.g. `sqlite3
~/.local/state/prism/prism.db "SELECT instance_id FROM agent_status
WHERE session_name = '<session>'"`). The structured `reason` field names the
specific policy check that fired; see `docs/podman-proxy.md` §7
for the common rejection classes and how to read them.
