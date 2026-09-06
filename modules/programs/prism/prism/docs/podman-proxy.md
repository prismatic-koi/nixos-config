# Podman proxy — security spec

<!-- doclint-ignore: CgroupBudget, MaxContainers, resource_limits -->
<!-- doclint-ignore: networkmode_host, networkmode_colon, networkmode_slash, networkmode_whitespace -->
<!-- doclint-ignore: pidmode_host, ipcmode_host, utsmode_host, usernsmode_host, cgroupnsmode_host -->
<!-- doclint-ignore: AGENTS.md -->
<!--
  The identifiers in the doclint-ignore lists above are intentionally
  unresolvable against the static source:

  - `CgroupBudget` is a hypothetical field used in the field-admission
    walkthrough in §4. It does not exist in the current struct and is
    not expected to; changing it to a real field would break the point
    of the walkthrough.

  - `MaxContainers` is the name §8.3 gives to a cap that does NOT
    exist, when it records the missing container-count bound. Naming
    the absent knob is the point of that entry, so it must stay
    unresolvable until someone implements it.

  - `resource_limits` is a libpod specgen field name. §8.3 cites it to
    explain why the podman CLI's `--cpus` never reaches this proxy's
    `NanoCpus` cap. It is deliberately absent from this source tree —
    the libpod body shape is not admitted, which is the residual that
    entry records.

  - `<field>_host` / `<field>_colon` / `<field>_slash` /
    `<field>_whitespace` are audit reason tokens constructed at runtime
    by `denyIfUnsafeModeValue` as `strings.ToLower(fieldName) + "_host"`
    (etc.). The literal `"_host"`/`"_colon"`/`"_slash"`/`"_whitespace"`
    suffixes appear in policy.go but the composed tokens do not — grep
    for the components, not the whole token.

  - `AGENTS.md` is a cross-boundary reference to the repo-root file. In
    a full checkout the basename resolves; in the nix sandbox where only
    the prism subtree is copied in, it does not exist. The reference is
    correct; only the sandbox visibility differs.
-->

This document is the formal security specification for the per-session
filtering podman API socket proxy that ships with prism. It is the reference
that future docker-/podman-API admission work must be audited against, and
the place to start when debugging a rejection in the wild.

The proxy is implemented in `modules/programs/prism/prism/internal/podmanproxy/`
and is wired into the per-session prism sidecar
(`modules/programs/prism/prism/internal/sidecar/podman_proxy.go`).

For the operational story — when to enable, how, prerequisites per platform —
see the "Podman support for workers" section in the repo root `AGENTS.md`.

## 1. What the proxy is

A worker agent — by design — runs inside `bwrap` (Linux) or `sandbox-exec`
(Darwin), with the host's real podman socket NOT exposed inside the sandbox.
When a session is spawned with `--containers`, prism's sidecar starts a
per-session HTTP reverse proxy bound to a Unix socket at
`<XDG_STATE_HOME>/prism/run/<sessionDirName>/podman.sock`. The sandbox
profile exposes only that socket to the worker. The proxy reverse-proxies
the host's real podman socket, but enforces a **default-deny** policy at
six layers before any byte reaches the upstream.

The agent's `CONTAINER_HOST` and `DOCKER_HOST` env vars point at the
filtered socket. The real podman socket path appears in NEITHER the rendered
bwrap argv NOR the rendered sandbox-exec profile. That absence is asserted
by tests in `internal/sidecar` and the platform-specific
`cmd/agent_run_sandbox_exec_darwin_test.go` / bwrap test files.

## 2. Threat model

This table is the shipped threat model. Where a threat is covered by a
typed struct field, the canonical reference is the struct in
`internal/podmanproxy/policy.go`. The comments on each admission record
the rationale.

| # | Threat | Mitigation | Canonical reference |
|---|---|---|---|
| T1 | Agent reads any host file via `-v /host/path:/x` | `HostConfig.Binds` policy rejects sources outside the per-session allowlist (worktree + bare repo + scratch dir). Symlinks resolved via `filepath.EvalSymlinks` before the prefix check. | `policy.go::checkHostConfig` (Binds branch), `isAllowedBindSource`, `canonicalisePath` |
| T2 | Same exfil via `HostConfig.Mounts` with `Type=bind` | Same allowlist check applied to every `Mounts[]` entry with `Type=bind`. `Type` is itself value-allowlisted to `{bind, volume, tmpfs}` via an inline `switch` in `checkHostConfig` (no named variable — the `case` branches in `policy.go` are the spec). Anything outside the allowlist (including the podman-specific `glob`) hits the `default:` branch and denies. | `policy.go::hostConfigMount`, `policy.go::checkHostConfig` (Mounts branch / `switch m.Type`) |
| T3 | Same exfil via `Mounts` with `Type=volume` and `VolumeOptions.DriverConfig.Name="local"` (a "named-volume" that is functionally a bind to a host path via `device=…`) | `Mounts[].VolumeOptions.DriverConfig` is DENIED when present. Same shape closed at the volumes/create endpoint: `Driver=local` with non-empty `DriverOpts` rejected. | `policy.go::hostConfigVolumeOptions`, `inspectVolumeCreate` |
| T4 | Exfil via `POST /containers/{id}/archive` (`podman cp` write) | Archive endpoint's `path` query parameter is checked against the same bind-source allowlist with the same symlink-canonicalisation. | `endpoints.go::endpointPolicyArchive`, `policy.go::inspectArchive` |
| T5 | Container escape via `HostConfig.Privileged: true` | Boolean check. Any `true` rejects. | `policy.go::checkHostConfig` (Privileged branch) |
| T6 | Capability escape via `HostConfig.CapAdd: [SYS_ADMIN]` and equivalents | `CapAdd` allowlisted against `Config.AllowedCaps`. Default empty = deny-all. | `policy.go::checkHostConfig` (CapAdd branch), `Config.AllowedCaps` |
| T7 | Namespace escape via `NetworkMode=host` (or `PidMode`/`IpcMode`/`UTSMode`/`UsernsMode`/`CgroupnsMode`=host) | All six `*Mode` fields use a literal value allowlist. `NetworkMode` allows `{"", bridge, none, default, slirp4netns, pasta}` plus a user-defined-name regex. The other five allow `{"", "private"}` only. | `policy.go::checkNetworkMode`, `policy.go::checkSimpleNamespaceMode`, the package-level variables `networkModeFixedLiterals`, `networkNameRegex`, `simpleNamespaceLiterals` |
| T8 | Cgroup-rule escape via `HostConfig.DeviceCgroupRules` | DENIED when non-empty. This is a parallel cgroup-rule mechanism to `Devices`. With `CAP_MKNOD` in the default capset, the agent can mknod host disks. | `policy.go::checkHostConfig` (DeviceCgroupRules branch) |
| T9 | Device passthrough via `HostConfig.Devices` or `DeviceRequests` (GPU / nvidia-container-runtime) | Both DENIED when non-empty. | `policy.go::checkHostConfig` (Devices, DeviceRequests branches) |
| T10 | `MaskedPaths` / `ReadonlyPaths` override re-exposing `/proc/keys`, `/proc/sysrq-trigger`, and similar paths | Both DENIED when present (pointer-to-slice distinguishes "absent" from "explicitly empty"). | `policy.go::checkHostConfig` (MaskedPaths, ReadonlyPaths branches) |
| T11 | Transitive mount inheritance via `HostConfig.VolumesFrom` | DENIED when non-empty — impossible to audit the source container's mounts transitively. | `policy.go::checkHostConfig` (VolumesFrom branch) |
| T12 | Non-namespaced sysctl write via `HostConfig.Sysctls` | DENIED when non-empty. Some sysctls are not namespaced. | `policy.go::checkHostConfig` (Sysctls branch) |
| T13 | Sandbox bypass via `HostConfig.SecurityOpt: [seccomp=unconfined]` / `apparmor=unconfined` / `no-new-privileges=false` / `label=disable` / `systempaths=unconfined` | Allowlisted against `Config.AllowedSecurityOpts`. Default empty = deny-all. | `policy.go::checkHostConfig` (SecurityOpt branch), `Config.AllowedSecurityOpts` |
| T14 | Resource exhaustion via `HostConfig.Memory=10TB`, `CpuQuota=very-large`, `NanoCpus=very-large` | Cap-strict-mode: when the corresponding `Config.Max*` cap is set, the field MUST be present and a positive value within the cap. The docker-semantic "0 means unbounded" bypass is closed for all three. The sidecar sets `MaxMemoryBytes` and `MaxNanoCpus` for every session, so this mitigation is active in production. See §8.1 for the values and for the cost they put on the client. | `policy.go::checkHostConfig` (Memory, CpuQuota, NanoCpus branches), `Config.MaxMemoryBytes` / `MaxCPUQuota` / `MaxNanoCpus` |
| T15 | Post-create cap relax via `POST /containers/{id}/update` resetting Memory to 0 | The `update` endpoint runs through the same cap-strict-mode check as `create`. | `endpoints.go::endpointPolicyUpdate`, `policy.go::inspectUpdate` |
| T16 | Privilege escalation via `POST /containers/{id}/exec` body's own `Privileged: true` | `exec` body is parsed with `containerExecBody` and `Privileged: true` is rejected. | `policy.go::inspectExec`, `containerExecBody.Privileged` |
| T17 | Log-driver shipping container logs off the host via `LogConfig.Type=syslog`/`splunk`/`fluentd`/`gelf`/`awslogs` | `LogConfig.Type` is value-allowlisted to local-only drivers `{json-file, none, journald, k8s-file, passthrough, passthrough-tty}`. | `policy.go::checkLogConfigType`, `logConfigTypeAllowlist` |
| T18 | `Mount.Type=glob` — podman-specific value that calls `filepath.Glob(Source)` on the host | `Mount.Type` value allowlist is case-sensitive `{bind, volume, tmpfs}` (inline `switch` in `checkHostConfig`). Everything else (`glob`, `image`, `npipe`, `artifact`, `ramfs`, `devpts`, empty string, case variants, whitespace, unicode zero-width space) hits the `default:` branch and rejects. | `policy.go::checkHostConfig` (Mounts branch — `switch m.Type` / `case "bind"`, `case "volume"`, `case "tmpfs"`, `default:`) |
| T19 | Raw-socket bypass: agent runs `curl --unix-socket /run/user/$UID/podman/podman.sock ...` to talk to the real socket directly | No path exists — the real socket is bound NEITHER into the bwrap mount tree NOR into the sandbox-exec SBPL allow list. The only socket reachable from inside the sandbox is the filtered one. | bwrap profile in `internal/container/bwrap.go`. SBPL in `internal/container/sandbox_exec.go`. Absence asserted by negative-mutation tests |
| T20 | CLI-wrapper bypass: agent runs a custom HTTP client bypassing `podman`/`docker` CLI | Same answer as T19. The wrapper IS the only reachable surface. |
| T21 | Build context smuggle via `POST /build` of arbitrary-content tar | No new escape: `build` is bounded by what the sandbox already exposes. Build endpoint is `endpointAllow` (query-only and opaque body). No size cap in v1. Revisit if abuse appears. | `endpoints.go` (build endpoint) |
| T22 | Schema drift: a new docker-/podman-API field upstream introduces a new escape vector without anyone in this repo noticing | `json.Decoder.DisallowUnknownFields()` runs on every parsed body. A new unknown field rejects with 403 and audit reason `unknown_field:<json error>` until it is admitted via the field-admission process (§4). | `policy.go::decodeStrict`, plus every typed struct |
| T23 | Proxy itself has a parsing bug | Default-deny — every unknown endpoint, unknown field, unknown enumerable value, malformed JSON, missing required value rejects before forwarding. Test suite exercises every documented escape and asserts it is blocked, plus a negative-control meta-test that verifies the positive tests are not no-ops. | `proxy_security_test.go::TestSecurity_NegativeControl_RootAllowlistPasses` |
| T24 | Storage exhaustion after the session ends: a volume or an image the agent created outlives the session on the shared host | PARTIAL. A volume created through `POST /volumes/create` gets the per-session name prefix (`Config.VolumeNamePrefix`), and `prism cleanup` removes every volume with that prefix. Images are NOT swept, and two volume gaps stay open — see §8.3. | `policy.go::applyVolumeNamePolicy`, `cmd/cleanup_sweep.go::sweepVolumesWithRunner` |

**Network egress** is not restricted: containers get whatever network the
host podman gives them (default: full internet). This is strictly broader
than what the sandbox otherwise allows at the policy layer
(`(allow network*)` in sandbox-exec, no `--unshare-net` in bwrap), so no
*new* egress capability is introduced. If we later want egress restrictions
they belong in a follow-up issue.

## 3. Default-deny is enforced at six layers

The proxy enforces default-deny at six independent layers. Each layer
covers a class of escape that the other layers do not.

| # | Layer | Mechanism |
|---|---|---|
| 1 | **Endpoint** | Positive allowlist via `classifyRequest` in `internal/podmanproxy/endpoints.go`. The last branch of every method helper is `endpointDeny`. Any unknown path/method pair returns 403 with a friendly JSON envelope that names the rejected endpoint. |
| 2 | **Field-name** | `json.Decoder.DisallowUnknownFields()` + typed struct per body-bearing endpoint. The structs (`hostConfig`, `containerCreateBody`, `containerExecBody`, `volumeCreateBody`, `networkCreateBody`) are the canonical security spec. Every admitted field is annotated `INSPECTED` (policy-checked), `DENIED` (rejected when non-empty/non-default), or `FORWARDED` (admitted as opaque `json.RawMessage`, bytes pass to the upstream unmodified). A future docker-API field that introduces an escape vector is rejected by default until it is explicitly admitted. |
| 3 | **Field-value** | Per-field literal allowlists (+ `NetworkMode` user-defined-name regex `networkNameRegex`) for enumerable values: `Mount.Type` (inline `switch` in `checkHostConfig` Mounts loop), the six `*Mode` fields (`networkModeFixedLiterals` for `NetworkMode`. `simpleNamespaceLiterals` for `PidMode`/`IpcMode`/`UTSMode`/`UsernsMode`/`CgroupnsMode`), `LogConfig.Type` (`logConfigTypeAllowlist`). Anything outside the allowlist denies. This closes the class where a field is admitted but a dangerous value of that field forwards. For example, `Mount.Type=glob` must not forward. If a deny-list rejected only `Type != "bind" && Type != "volume"`, then `glob` passes through. |
| 4 | **Body-content** | `checkHostConfig` walks the parsed HostConfig and rejects dangerous values: `Privileged: true`, host-namespace modes, non-empty `Devices` / `DeviceCgroupRules` / `DeviceRequests` / `VolumesFrom`, present `MaskedPaths` / `ReadonlyPaths`, non-empty `Sysctls`, `Mounts[].VolumeOptions.DriverConfig` non-nil, `CapAdd` outside allowlist, `SecurityOpt` outside allowlist, resource caps in strict mode. |
| 5 | **Path-resolution** | `filepath.EvalSymlinks` + lexical `filepath.Clean` on both bind sources AND allowlist entries before the prefix comparison. Relative paths, broken symlink chains, and non-existent sources all deny. This closes the class where the agent plants a symlink inside an allowed prefix pointing at `/etc/passwd`. Symlink resolution denies a planted source that a purely lexical prefix-match accepts. See §5 for the residual TOCTOU. |
| 6 | **Query** | `PUT /containers/{id}/archive` `path` query parameter is checked against `AllowedBindSources` with the same canonicalised-prefix logic as `Binds`. The other endpoint with a security-relevant query (`POST /containers/create?name=<…>`) is covered by the container-name auto-prefix policy in `applyContainerNamePolicy`. |

### Per-session naming

Two endpoints carry a per-session name policy. Both work the same way,
and both exist so `prism cleanup` can find what the session created:

| Endpoint | Config field | Policy function | Deny reason |
|---|---|---|---|
| `POST /containers/create` | `ContainerNamePrefix` | `applyContainerNamePolicy` | `name_prefix_mismatch_query`, `name_prefix_mismatch_body` |
| `POST /volumes/create` | `VolumeNamePrefix` | `applyVolumeNamePolicy` | `volume_name_prefix_mismatch` |

The sidecar sets both fields to `prism-<sessionName>-`. A request with
no name gets `prism-<sessionName>-<8 hex chars>`. A request with a name
outside the prefix returns 403.

The container endpoint takes its name from two channels (the `?name=`
query and the body `Name`), so the policy checks and injects into both.
The volume endpoint takes its name from the body alone.

## 4. Field-admission process

The schema inversion means any new docker-/podman-API field introduced
upstream is rejected by default. Admission requires an audit
against the threat table above and an explicit addition to the relevant
typed struct in `internal/podmanproxy/policy.go`.

A workflow that surfaces a needed field will see a 403 response with a body
like:

```json
{"message": "containers/create HostConfig: unknown field \"NewField\""}
```

and a matching audit-log line containing
`reason=create_hostconfig:unknown_field:json: unknown field "NewField"`.

### Walkthrough — admitting a hypothetical new field

Suppose podman 6.0 adds a new HostConfig field `CgroupBudget` (made up for
this example) that takes an integer share-weight.

1. **File an issue** describing the workflow that wants the field and the
   docker/podman API reference for it.
2. **Audit the field against the threat table in §2.** For `CgroupBudget`,
   the question is: can a large value exhaust host resources (→ T14:
   resource-exhaustion class), can it reach into another cgroup
   hierarchy (→ T8: cgroup-rule class), or can it interact with namespace
   escapes (→ T7)? Classify accordingly. In this case it's a per-container
   CPU weighting and behaves like the existing `CpuShares` admission, so
   the answer is FORWARDED.
3. **Open a PR adding the field to `hostConfig`** in `policy.go`:

   ```go
   // FORWARDED — per-container CPU share weighting, equivalent to
   // CpuShares; no escape vector against the threat table in §2.
   // Admitted under the same rationale as CpuShares — podman 6.0
   // renamed the field. Audit ticket: #NNNN.
   CgroupBudget json.RawMessage `json:"CgroupBudget"`
   ```

   The rationale comment matches the existing style: classification,
   one-line justification, ticket reference.
4. **Field requiring a policy check?** If the audit classifies the new
   field as INSPECTED rather than FORWARDED, add a typed field (not
   `json.RawMessage`), extend `checkHostConfig` with the policy, and add
   a positive + revert-and-watch-fail test pair as proof the new check is
   not a no-op (see §6).
5. **Field requiring a value allowlist?** If the field is enumerable (a
   handful of well-known values), follow one of the two patterns
   already in `policy.go`:

   - **Package-level `map[string]struct{}` allowlist** — used by
     `networkModeFixedLiterals`, `simpleNamespaceLiterals`, and
     `logConfigTypeAllowlist`. Best when the field is enumerable AND
     checked by a dedicated `check<Field>` helper (for example,
     `checkNetworkMode` or `checkLogConfigType`).
   - **Inline `switch` with explicit `case` branches and a `default:`
     deny** — used by `Mount.Type` inside `checkHostConfig`'s Mounts
     loop. Best when the per-value action varies (`case "bind"` runs
     the bind-source policy, `case "volume"` runs the DriverConfig
     check, `case "tmpfs"` is a no-op forward, `default:` denies).

   Either pattern is acceptable. Pick whichever matches the shape of
   the policy decision the field needs.

The single most important reviewer task on a change to `policy.go` is
**to read the struct and confirm the rationale comments**. Do not guess
what the proxy admits. The struct is the security spec.

## 5. Residual TOCTOU between `EvalSymlinks` and `mount(2)`

The bind-source allowlist resolves symlinks at policy time. The container
runtime (runc, crun) calls `mount(2)` some milliseconds later. An agent
that wins the race can swap a resolved-safe symlink for a dangerous one
between the two moments.

For v1 this residual is **accepted** because:

- The agent is otherwise sandboxed by bwrap / sandbox-exec.
- The bind target inside the container is fixed at create time, so a
  changed symlink cannot retarget the agent's reach to a different
  in-container path.
- The sandbox layer sits in front of the proxy, so the agent's filesystem
  reach inside the sandbox is already constrained.

The correct architectural home for the proper mitigation is the surrounding
sandbox profile, not the proxy. Two future directions:

- **Linux**: mount the per-session scratch directory with `nosymfollow`
  via `bwrap --bind-try`/`--ro-bind-try` extensions or a paired
  `mount --make-rslave` / `mount -o nosymfollow` pre-step. Tracked
  informally. Will become a Step-N+1 issue if/when the gap is exercised.
- **Darwin**: equivalent SBPL restriction on the scratch directory via
  `(deny file-issue-extension*)` / `(deny file-link*)` clauses. Same
  status: future hardening, not gated on a concrete report.

The `internal/podmanproxy/doc.go` package doc carries the same note.
Both surfaces are intentionally redundant so the residual is hard to
miss.

## 6. Verification — the test suite as the spec

The body of evidence for "this policy is enforced" is the test suite at
`internal/podmanproxy/proxy_security_test.go`. That file holds 83
top-level test functions at this writing, many with multiple subtests,
all green under `-race`. The companion files `proxy_test.go` and
`proxy_name_prefix_test.go` add a further ~40 top-level tests that
cover the constructor, lifecycle, and Name-prefix policy. The key
meta-test is:

```go
// TestSecurity_NegativeControl_RootAllowlistPasses mutates Config.AllowedBindSources
// to ["/"] and asserts that the same /etc/passwd bind request that returns
// 403 in TestSecurity_HostBindOutsideAllowlist_Denied now PASSES to the
// upstream. This proves the positive denial tests are not no-ops from an
// unrelated policy code path.
```

This is the load-bearing "tests are not vacuous" check. The
revert-and-watch-fail discipline (revert the fix → re-run the test → see
it fail → re-apply the fix → see it pass) applies to every policy check
in this package.

Tests for sidecar wiring live at
`internal/sidecar/podman_proxy_test.go` and assert that:

- `containers_enabled=0` sessions do NOT bind a `podman.sock` listener
  in the per-session run dir, and emit NO audit-log file.
- `containers_enabled=1` sessions DO bind the listener and the audit
  file appears at `<sessionDir>/podman-proxy.log`.
- A request to the filtered socket reaches the (fake) upstream and the
  audit log records the call.

Tests for the bwrap profile additions live at
`internal/container/bwrap_test.go`. Tests for the sandbox-exec
profile additions live at
`internal/container/sandbox_exec_podman_proxy_test.go` and
`internal/container/sandbox_exec_podman_proxy_prepare_test.go`, plus the
integration test under `internal/integration/` per the
[sandbox-exec testing convention](sandbox-exec-testing.md).

## 7. Troubleshooting — reading the audit log

Every request the proxy sees writes exactly one JSON line to:

```
<XDG_STATE_HOME>/prism/sessions/<instanceID>/podman-proxy.log
```

Each line has the shape:

```json
{"timestamp":"2026-06-30T11:58:42.123456Z","method":"POST","endpoint":"/v5/libpod/containers/create","decision":"deny","reason":"host_bind:/etc"}
```

| Field | Meaning |
|---|---|
| `timestamp` | RFC3339Nano UTC at request-receipt time. |
| `method` | HTTP method. |
| `endpoint` | Full request path (with the docker/podman API version prefix, for example `/v1.41/...` or `/v5/libpod/...`). |
| `decision` | `allow` (forwarded upstream) or `deny` (synthesised response from the proxy). |
| `reason` | Structured token naming the policy check that fired. Two shapes: (a) bare body-policy tokens — for example `host_bind:<path>`, `privileged`, `cap_add:<cap>`, `mount_bind:<source>`, `mount_volume_driver_config`, `mount_type_not_allowed:<type>`, `networkmode_host` / `networkmode_colon` / `networkmode_slash` / `networkmode_whitespace`, the same suffix family on `pidmode_*` / `ipcmode_*` / `utsmode_*` / `usernsmode_*` / `cgroupnsmode_*` — emitted by `policy.go::checkHostConfig` and friends. (b) endpoint-prefixed schema errors — `create_top:`, `create_hostconfig:`, `update:`, `exec:`, `volumes_create:`, `networks_create:`, `archive_path:`, `archive_missing_path`, `endpoint_not_allowed:` — followed by an `unknown_field:<json error>` / `malformed_body:<reason>` suffix when the strict JSON decode rejects the body. Grep the audit log for these exact tokens. Do not paraphrase. |

### Common rejection classes

Reason strings below are the **exact tokens** the proxy writes to the
audit log — grep them verbatim. Each entry cites the `policy.go`
callsite that formats the token. If your version of the proxy drifts,
verify the format from the source.

| Symptom | `reason` token | Fix |
|---|---|---|
| Worker tries `-v /etc:/host alpine ...` | `host_bind:/etc` (`policy.go::checkHostConfig` Binds branch) | Expected. T1. |
| Worker tries a `Mounts[]` entry with `Type=bind` and a forbidden Source | `mount_bind:<source>` (`checkHostConfig` Mounts loop, `case "bind"`) | Expected. T2. |
| Worker tries a `Mounts[]` entry with `Type=volume` and `VolumeOptions.DriverConfig` set | `mount_volume_driver_config` (`checkHostConfig` Mounts loop, `case "volume"`) | Expected. T3 (local-driver bind-volume escape). |
| Worker tries a `Mounts[]` entry with a `Type` outside `{bind, volume, tmpfs}` (`glob`, `image`, `npipe`, case variants …) | `mount_type_not_allowed:<type>` (`checkHostConfig` Mounts loop, `default:`) | Expected. T18. |
| Worker tries `--privileged` | `privileged` (`checkHostConfig` Privileged branch) | Expected. T5. |
| Worker tries `--cap-add SYS_ADMIN` (with the default empty `AllowedCaps`) | `cap_add:SYS_ADMIN` (`checkHostConfig` CapAdd branch) | Expected. T6. If the workload genuinely needs a cap, that's a `Config.AllowedCaps` discussion — file an issue. |
| Worker tries `--network=host` | `networkmode_host` (`denyIfUnsafeModeValue` via `checkNetworkMode`. The same helper emits `networkmode_colon` / `networkmode_slash` / `networkmode_whitespace` for the other categorised value-level rejections, and `network_mode_invalid:<value>` when no class matches and the user-defined-name regex fails) | Expected. T7. |
| Worker tries `--pid=host` / `--ipc=host` / `--uts=host` / `--userns=host` / `--cgroupns=host` | `pidmode_host` / `ipcmode_host` / `utsmode_host` / `usernsmode_host` / `cgroupnsmode_host` (same `denyIfUnsafeModeValue` formatter on the five sibling fields. Same `_colon` / `_slash` / `_whitespace` suffix family applies, plus `<field>_invalid:<value>` as the catch-all) | Expected. T7. |
| Worker tries a brand-new docker-/podman-API field this struct does not admit | `create_hostconfig:unknown_field:json: unknown field "NewField"` (or the matching `create_top:` / `update:` / `exec:` / `volumes_create:` / `networks_create:` prefix per endpoint) | Field-admission process (§4). |
| Worker gets `503` with `"podman socket unavailable: ..."` envelope | Audit log shows `decision=allow` then nothing — the proxy accepted policy-wise but the upstream is not there | Bring the upstream up: Darwin `podman machine start`. NixOS `systemctl --user status podman.socket`. |
| Worker gets `400` with `"malformed_body:empty"` or similar | `create_top:malformed_body:empty` (or the matching endpoint prefix) | Client is sending an empty / non-JSON `POST /containers/create` body. Bug in the client, not the proxy. |
| Worker runs `podman run` or `podman volume create` and gets 403 | `create_top:unknown_field:json: unknown field "command"` for a container, `volumes_create:unknown_field:json: unknown field "Label"` for a volume (`policy.go::decodeStrict`) | Pre-existing. The podman CLI posts a libpod body that the docker-shaped structs do not admit, so the request dies at the schema layer. The cap and name-prefix checks below are never reached. See §8.3. |
| **docker-API client** posts `POST /containers/create` with no `HostConfig.Memory` | `memory_required` (`checkOneResourceCap`) | Expected. Set a memory limit at or below 4 GiB (`--memory` on a docker-API client). See §8.1. |
| **docker-API client** posts `POST /containers/create` with no `HostConfig.NanoCpus` | `nano_cpus_required` (`checkOneResourceCap`) | Expected. Set a CPU limit at or below 2 (`--cpus` on a docker-API client). See §8.1. |
| **docker-API client** asks for more memory or more CPU than the cap allows | `memory_over_cap` / `nano_cpus_over_cap` (`checkOneResourceCap`) | Expected. Lower the request. If the workload genuinely needs more, that is a `Config.MaxMemoryBytes` / `Config.MaxNanoCpus` discussion — file an issue. |
| **docker-API client** sets `Memory` or `NanoCpus` to `0` | `memory_nonpositive` / `nano_cpus_nonpositive` (`checkOneResourceCap`) | Expected. `0` means "unbounded" in docker semantics, which bypasses the cap. Pass a positive value. |
| **docker-API client** posts `POST /volumes/create` with a name outside the session prefix | `volume_name_prefix_mismatch` (`policy.go::applyVolumeNamePolicy`) | Expected. Omit the name to receive an auto-prefixed one, or start the name with `prism-<session>-`. |
| Worker runs `podman pull <image>` and gets 403 | `endpoint_not_allowed:POST images/pull` (`handler.go` default branch) | Pre-existing. The podman CLI pulls through the libpod endpoint `POST /images/pull`, which the endpoint allowlist does not admit. A docker-API client that pulls through `POST /images/create` works. See §8.3. |

### When to escalate

Most rejections are *correct* — the agent attempted an escape and the
proxy blocked it. Escalation paths:

- **A legitimate workflow needs a field that's currently denied** —
  file an issue, follow §4. Do NOT loosen the policy in a worker PR
  without an audit.
- **The audit log shows the proxy accepted a request that must be
  denied** — that is a P0 security bug against this package.
  Reproduce in a test, open an issue, escalate.
- **The proxy returns 5xx (not 4xx) when the policy must fire** —
  parsing or upstream-handling bug. File an issue with the failing
  request body.

## 8. Configuration knobs

The proxy's `Config` struct in `internal/podmanproxy/proxy.go` carries
the per-session knobs that the sidecar wires. The sidecar wiring lives
in `internal/sidecar/podman_proxy.go::runPodmanProxyIfEnabled`, which
constructs a `podmanproxy.Config` literal inline (no separate builder
function — the `runPodmanProxyIfEnabled` body itself is the spec) and
sets:

- `AllowedBindSources` — the per-session worktree path, the bare repo
  path, and the per-session scratch directory.
- `ContainerNamePrefix` and `VolumeNamePrefix` — both
  `container.ResourceNamePrefixForSession(sessionName)`, which
  activates the auto-prefix policy and lets the cleanup sweep
  (`cmd/cleanup_sweep.go::sweepSessionResourcesForSession`, called
  from `cmd/cleanup.go`) find every container and every volume that
  belongs to the session.

  The helper is NOT a plain `"prism-" + sessionName + "-"`
  concatenation. It builds on `container.NameForSession`, which folds
  `@`, `/`, `.`, and `~` to `-`. Podman validates a container or volume
  name against `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`, and a session name is
  `<repo>@<branch>`, so an unsanitised prefix produces names podman
  refuses to create. Session `nixos-config@main` therefore gets the
  prefix `prism-nixos-config-main-`. Wherever this document writes
  `prism-<session>-`, read `<session>` as the folded form.
- `AllowedCaps` — empty by default. Deny-all.
- `AllowedSecurityOpts` — empty by default. Deny-all.
- `MaxMemoryBytes` — `4294967296` (4 GiB per container).
- `MaxNanoCpus` — `2000000000` (2 CPUs per container).
- `MaxCPUQuota` — left at `0`. Read §8.1 before you change this.
- `AuditWriter` — the `os.File` for `<sessionDir>/podman-proxy.log`.

Out-of-tree callers can construct the `Config` differently. The proxy
package itself imposes no policy beyond what the `Config` declares.

### 8.1 Resource caps make memory and CPU limits mandatory

**Scope: this section describes the docker-compat surface only.** It
applies to a client that posts a docker-API `POST /containers/create`
body, which means `DOCKER_HOST` plus a docker-API client. The podman
CLI does not reach these checks at all — its create request is rejected
earlier, at the schema layer. Read §8.3 before you use the podman CLI
against this proxy.

**A docker-API `POST /containers/create` must set both
`HostConfig.Memory` and `HostConfig.NanoCpus`. A body that omits either
one gets a 403.** With a docker-API client, those are the `--memory`
and `--cpus` flags.

```bash
# Correct, with DOCKER_HOST and a docker-API client. Both caps are
# declared and both are within the limits.
docker run --rm --memory 512m --cpus 1 alpine echo hello

# Rejected with 403. No memory limit.
docker run --rm --cpus 1 alpine echo hello
```

This is the cost of the cap, not a defect. A configured cap puts
`checkOneResourceCap` into strict mode: the matching `HostConfig` field
becomes mandatory on create, and an absent field returns 403. The strict
reading is deliberate. In docker semantics `Memory: 0` means
"unbounded". If the cap rejected only the values above the limit,
`Memory: 0` stays an open bypass.

The two reason strings the audit log records for a missing field are
`memory_required` and `nano_cpus_required`. See §7 for the full list.
Every reason string in this section is reachable from the docker-compat
surface only.

**Why a container needs a cap at all.** A container the agent starts
through the proxy is a host process. It runs outside the agent's bwrap
or sandbox-exec sandbox, so no sandbox limit applies to it. The caps are
the only bound on what ONE container consumes, and only on the fields
they name. Read §8.3 for the bounds that do not exist.

**Why `MaxCPUQuota` stays 0.** `NanoCpus` and `CpuQuota` are two ways to
express the same CPU limit. Docker and podman clients refuse to send
`--cpus` together with `--cpu-quota`. A configured cap makes its field
mandatory. Thus a proxy with both caps set rejects every create request.
The client sends one of the two fields, the other one is absent, and the
absent one returns 403. Enforce one CPU cap only.

### 8.2 Volume sweep at cleanup

`prism cleanup` removes two classes of resource for a session with
`agent_status.containers_enabled = 1`. A session that never enabled
containers issues no podman command at all.

| Class | Match rule | podman command | Runs on |
|---|---|---|---|
| Containers | Strict shape `prism-<session>-<8 hex chars>` | `podman rm -f`, one batch | every teardown path |
| Volumes | Name prefix `prism-<session>-` | `podman volume rm`, one batch | hard cleanup only |

**The volume sweep runs on the hard-cleanup paths only.** A soft close
keeps the worktree, the branch, and the transcript, so the session can
be reopened. It keeps the data volumes for the same reason. The soft
paths are `prism close`, a coordinator session, a non-worktree session,
and `--keep-worktree`.

A container is stateless runtime, and it is swept on every path. A
volume is not. The volume rule is a plain name prefix precisely so it
reaches user-named data volumes.

A soft-closed session reaches hard cleanup eventually, and the volume
sweep runs then. So the cost of the narrower scope is a volume that
leaks for longer, not one that leaks forever. That is the same trade the
sibling guard makes below.

Images are NOT swept. See §8.3.

The counts appear in the `prism cleanup --json` envelope as
`containers_swept` and `volumes_swept`. Both keys are absent when the
session did not enable containers. `volumes_swept` is also absent on a
soft close. That keeps "this path did not consider volumes"
distinguishable from "this path found none".

Two properties of the sweep:

- **The volume rule is a prefix, the container rule is a strict shape.**
  The volume policy admits a user-chosen name as long as it carries the
  prefix, and such a volume holds data that must not outlive the
  session. A prefix match reaches those names. The container sweep does
  not need to, because a user-named container holds no data. The
  invariant that follows: every name the volume policy admits must be
  reachable by the sweep, including the name that is exactly the prefix.
- **A live sibling session keeps its volumes.** Session `foo` and
  session `foo-bar` produce prefixes where one contains the other, so a
  volume named `prism-foo-bar-data` matches both. The sweep reads the
  live sessions from the database and leaves any name a sibling also
  claims. The cost is a leaked volume. The alternative is the loss of
  another session's data.

### 8.3 Residuals

The residuals below are accepted for this version, in the same sense as
the residual TOCTOU in §5. Read the first one before you use the podman
CLI against this proxy at all, and the container-count one before
`--containers` becomes the default.

**The libpod create surface is not admitted, so the podman CLI cannot
create a container or a volume.** This is the largest residual, and it
sits underneath every other statement in §8.1 and §8.2.

`normalisePath` strips the `libpod/` prefix, so a libpod request routes
to the same classifier as its docker-compat twin. The BODY shapes do not
match. `podman run` posts a libpod specgen body — `command`,
`resource_limits`, no `HostConfig` — and `containerCreateBody` runs with
`DisallowUnknownFields`, so the request is rejected at decode:

```
create_top:unknown_field:json: unknown field "command"
```

`podman volume create` fails the same way, on `Label`:

```
volumes_create:unknown_field:json: unknown field "Label"
```

The consequence is that the resource caps in §8.1 and the volume-name
policy in §8.2 are reachable from the docker-compat surface only. Every
reason string those sections name is unreachable from the podman CLI,
because the request never survives the decode that precedes them.

Note also that on the libpod surface `--cpus` maps to
`resource_limits.cpu.{quota,period}` — the `CpuQuota` expression, not
`NanoCpus`. So `MaxNanoCpus`, the CPU cap this proxy enforces, is the
field the podman CLI never sends even when the body is admitted.

To close this, the libpod specgen shape needs its own typed struct and a
mapping from `resource_limits` onto the cap checks. That is an admission
of a new body shape, so it needs the field-admission audit in §4. The
issue is filed: [#2946](https://github.com/prismatic-koi/nixos-config/issues/2946).

**A volume created implicitly by a container mount.** `checkHostConfig`
admits a named volume as the source of a bind (`Binds:
["myvol:/data"]`) and as a `Mounts` entry of `Type=volume`. Podman
creates a named volume that does not yet exist when a container mounts
it. That path never sends `POST /volumes/create`, so
`applyVolumeNamePolicy` never runs and the volume carries no
`prism-<session>-` prefix. `sweepVolumesWithRunner` matches on the
prefix, so it never removes the volume. A docker-API
`run -v leak:/data alpine true` therefore leaves a volume behind.

The podman CLI cannot reach this path at all. Its create request is
rejected earlier, per the first residual above.

To close this, the policy must reject a named-volume mount whose name
is outside the prefix. That is a new deny path on an admitted field, so
it needs the field-admission audit in §4 and its own issue.

**Images are not swept at all. This work is deferred.** An image the
agent pulls stays in the shared host image store after the session ends.
Nothing in cleanup removes it.

An earlier revision of this change carried an image sweep. It was cut
because the design was not safe yet, and because it delivered almost
nothing in practice. Two conditions must both be met before image
sweeping returns:

1. **The libpod pull endpoint needs admission.** The podman CLI pulls
   through `POST /images/pull`, which the endpoint allowlist does not
   admit, so `podman pull` returns 403 today. Only a client that speaks
   the docker API reaches `POST /images/create`. A sweep built on the
   docker-compat surface alone therefore removes nothing for the podman
   workflow this repo mandates. Admitting an endpoint is a policy
   change, so it needs the field-admission audit in §4. The issue is
   filed: [#2946](https://github.com/prismatic-koi/nixos-config/issues/2946).

2. **The record of what to remove must be somewhere the agent cannot
   write.** The cut revision kept the record in a file under the
   per-session work dir. On Darwin that is the wrong place: the
   sandbox-exec profile grants `file-read* file-write*` over `(subpath
   <sessionDir>)`, which is the session's only writable grant, so the
   agent can append any reference it likes. Cleanup then runs
   `podman rmi` on it against the shared store, after the session has
   ended. That is a deferred arbitrary-image deletion primitive, and no
   amount of validation at write time closes it, because the agent
   bypasses the writer entirely. Linux is not affected — bwrap binds
   only `<sessionDir>/container-scratch` — but the record must be safe
   on both platforms.

A database table satisfies condition 2 structurally, because the agent
has no write path to the database. That is the expected shape when this
returns.

**No bound on the NUMBER of containers.** `MaxMemoryBytes` and
`MaxNanoCpus` are per container. Nothing limits how many containers one
session creates, so N containers consume up to N × 4 GiB and N × 2 CPUs.
The proxy has no `MaxContainers` equivalent.

This matters because the prior art is a host crash. Issue #872 carries
the post-mortem of the crash on 2026-04-19, which 15 concurrent
containers on a 32 GiB host caused. Its conclusion is explicit.
Per-container caps do nothing about a fan-out pattern. The structural
fix is "refusing to start the N+1'th container". The per-container caps
in §8.1 raise the floor, and they do not close that gap.

`DefaultBwrapConcurrencyCap` in `internal/config/config.go` bounds prism
SESSIONS, not containers per session, so it does not cover this path.

Read this before you make `--containers` the default. The count cap is
the part of the availability story that is still open.

**`MemorySwap` is forwarded and uncapped.** It sits in the FORWARDED
block of `hostConfig`. A body that passes the `Memory` cap can still set
`--memory-swap -1` and take unbounded swap on top of the 4 GiB ceiling. T14 reads "active in production", and that is true of the
fields the caps name, not of every memory-adjacent field.

**`CpuQuota` is inspected but unenforced, and `CpuPeriod` is
forwarded.** `MaxCPUQuota` stays 0, so `checkOneResourceCap` returns
`policy:resource_cap_disabled` for that field. A body can carry an in-cap
`NanoCpus` together with a large `CpuQuota` and `CpuPeriod` pair. Which
of the two wins depends on podman's own CPU-limit precedence. That
precedence is unverified. Confirm it before
`--containers` becomes the default.

**The sibling guard in §8.2 is cleanup correctness, not a defence.** It
stops one session's cleanup from destroying a live sibling's volumes. It
is not a control against a hostile agent. `POST /volumes/prune` and
`DELETE /volumes/{name}` are plain allows in `endpoints.go`, so an agent
can already remove any volume on the host by name. Do not read the guard
as an isolation guarantee.

**The name sanitiser can collide.** `NameForSession` folds `@`, `/`,
`.`, and `~` all to `-`, so `repo@feat/x` and `repo@feat-x` produce the
same prefix. `siblingVolumePrefixes` treats a prefix equal to its own as
"this is me" and skips it, so a colliding live session's volumes are
swept rather than spared. This is a pre-existing collision class — the
sandbox container name already collides through the same helper — and it
is not introduced here.

## 9. Linger decision

**No `loginctl enable-linger <user>` for v1.** The proxy is only
used by interactive prism sessions. When the user has a graphical
session, the user-systemd instance is running, and the rootless
podman socket unit is socket-activated on first use. Users running
long-lived background workers that need podman across SSH-out can
re-evaluate later.

This is documented here rather than enforced in nix because the
decision is operational, not structural. Enabling linger requires no
code change, and disabling it later requires no code change. No
host in this flake currently sets it, and this PR did not change
that.

## 10. References

- Parent issue: [#2317](https://github.com/prismatic-koi/nixos-config/issues/2317) — design, threat-table sketch, parent ACs.
- Step 1 PR: [#2326](https://github.com/prismatic-koi/nixos-config/pull/2326) — the proxy package itself.
- Step 2 PR: [#2327](https://github.com/prismatic-koi/nixos-config/pull/2327) — DB migration v36→v37.
- Step 3 PR: [#2328](https://github.com/prismatic-koi/nixos-config/pull/2328) — sidecar wiring.
- Step 4 PR: [#2329](https://github.com/prismatic-koi/nixos-config/pull/2329) — bwrap profile.
- Step 5 PR: [#2331](https://github.com/prismatic-koi/nixos-config/pull/2331) — sandbox-exec profile.
- Step 6 PR: [#2330](https://github.com/prismatic-koi/nixos-config/pull/2330) — `--containers` spawn flag.
- Step 7 PR: [#2332](https://github.com/prismatic-koi/nixos-config/pull/2332) — orphan-container cleanup sweep + auto-prefix on `Name`.
- Canonical structs: [`internal/podmanproxy/policy.go`](../internal/podmanproxy/policy.go) (`hostConfig`, `containerCreateBody`, `containerExecBody`, `volumeCreateBody`, `networkCreateBody`).
- Package doc: [`internal/podmanproxy/doc.go`](../internal/podmanproxy/doc.go) — the in-tree summary of the threat model. This document (§2) is the canonical threat table. `doc.go` is the inline summary.
- Sidecar wiring: [`internal/sidecar/podman_proxy.go`](../internal/sidecar/podman_proxy.go) — `runPodmanProxyIfEnabled`.
- Cleanup sweep: [`cmd/cleanup_sweep.go`](../cmd/cleanup_sweep.go) — `sweepSessionResourcesForSession` (called from `cmd/cleanup.go`).
- Sandbox-exec testing convention: [`docs/sandbox-exec-testing.md`](sandbox-exec-testing.md).
