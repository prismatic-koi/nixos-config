# Podman proxy — security spec

<!-- doclint-ignore: CgroupBudget -->
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
(`modules/programs/prism/prism/internal/sidecar/podman_proxy.go`). The
combined train is tracked by [#2317](https://github.com/prismatic-koi/nixos-config/issues/2317);
the eight implementation PRs are #2326, #2327, #2328, #2329, #2330, #2331,
#2332, and the closing docs/housekeeping PR.

For the operational story — when to enable, how, prerequisites per platform —
see the "Podman support for workers" section in the repo root `AGENTS.md`.

## 1. What the proxy is

A worker agent — by design — runs inside `bwrap` (Linux) or `sandbox-exec`
(Darwin), with the host's real podman socket NOT exposed inside the sandbox.
When a session is spawned with `--containers`, prism's sidecar starts a
per-session HTTP reverse proxy bound to a Unix socket at
`<XDG_STATE_HOME>/prism/run/<sessionDirName>/podman.sock`, and the sandbox
profile exposes only that socket to the worker. The proxy reverse-proxies
the host's real podman socket, but enforces a **default-deny** policy at
six layers before any byte reaches the upstream.

The agent's `CONTAINER_HOST` and `DOCKER_HOST` env vars point at the
filtered socket. The real podman socket path appears in NEITHER the rendered
bwrap argv NOR the rendered sandbox-exec profile — that absence is asserted
by tests in `internal/sidecar` and the platform-specific
`cmd/agent_run_sandbox_exec_darwin_test.go` / bwrap test files.

## 2. Threat model

This table is the shipped model, not the design. It supersedes #2317 §4 —
which was written before review-security cycles 2–6 surfaced four
structural classes of permissiveness that were closed in PR #2326. Where a
threat is covered by a typed struct field, the canonical reference is the
struct in `internal/podmanproxy/policy.go`; the comments on each admission
record the rationale.

| # | Threat | Mitigation | Canonical reference |
|---|---|---|---|
| T1 | Agent reads any host file via `-v /host/path:/x` | `HostConfig.Binds` policy rejects sources outside the per-session allowlist (worktree + bare repo + scratch dir). Symlinks resolved via `filepath.EvalSymlinks` before the prefix check. | `policy.go::checkHostConfig` (Binds branch), `isAllowedBindSource`, `canonicalisePath` |
| T2 | Same exfil via `HostConfig.Mounts` with `Type=bind` | Same allowlist check applied to every `Mounts[]` entry with `Type=bind`. `Type` is itself value-allowlisted to `{bind, volume, tmpfs}` via an inline `switch` in `checkHostConfig` (no named variable — the `case` branches in `policy.go` are the spec); anything outside the allowlist (including the podman-specific `glob`) hits the `default:` branch and denies. | `policy.go::hostConfigMount`, `policy.go::checkHostConfig` (Mounts branch / `switch m.Type`) |
| T3 | Same exfil via `Mounts` with `Type=volume` and `VolumeOptions.DriverConfig.Name="local"` (a "named-volume" that is functionally a bind to a host path via `device=…`) | `Mounts[].VolumeOptions.DriverConfig` is DENIED when present. Same shape closed at the volumes/create endpoint: `Driver=local` with non-empty `DriverOpts` rejected. | `policy.go::hostConfigVolumeOptions`, `inspectVolumeCreate` |
| T4 | Exfil via `POST /containers/{id}/archive` (`podman cp` write) | Archive endpoint's `path` query parameter is checked against the same bind-source allowlist with the same symlink-canonicalisation. | `endpoints.go::endpointPolicyArchive`, `policy.go::inspectArchive` |
| T5 | Container escape via `HostConfig.Privileged: true` | Boolean check; any `true` rejects. | `policy.go::checkHostConfig` (Privileged branch) |
| T6 | Capability escape via `HostConfig.CapAdd: [SYS_ADMIN]` etc. | `CapAdd` allowlisted against `Config.AllowedCaps`; default empty = deny-all. | `policy.go::checkHostConfig` (CapAdd branch), `Config.AllowedCaps` |
| T7 | Namespace escape via `NetworkMode=host` (or `PidMode`/`IpcMode`/`UTSMode`/`UsernsMode`/`CgroupnsMode`=host) | All six `*Mode` fields use a literal value allowlist. `NetworkMode` allows `{"", bridge, none, default, slirp4netns, pasta}` plus a user-defined-name regex. The other five allow `{"", "private"}` only. | `policy.go::checkNetworkMode`, `policy.go::checkSimpleNamespaceMode`, the package-level variables `networkModeFixedLiterals`, `networkNameRegex`, `simpleNamespaceLiterals` |
| T8 | Cgroup-rule escape via `HostConfig.DeviceCgroupRules` | DENIED when non-empty. This is a parallel cgroup-rule mechanism to `Devices`; with `CAP_MKNOD` in the default capset, the agent could mknod host disks. | `policy.go::checkHostConfig` (DeviceCgroupRules branch) |
| T9 | Device passthrough via `HostConfig.Devices` or `DeviceRequests` (GPU / nvidia-container-runtime) | Both DENIED when non-empty. | `policy.go::checkHostConfig` (Devices, DeviceRequests branches) |
| T10 | `MaskedPaths` / `ReadonlyPaths` override re-exposing `/proc/keys`, `/proc/sysrq-trigger`, etc. | Both DENIED when present (pointer-to-slice distinguishes "absent" from "explicitly empty"). | `policy.go::checkHostConfig` (MaskedPaths, ReadonlyPaths branches) |
| T11 | Transitive mount inheritance via `HostConfig.VolumesFrom` | DENIED when non-empty — impossible to audit the source container's mounts transitively. | `policy.go::checkHostConfig` (VolumesFrom branch) |
| T12 | Non-namespaced sysctl write via `HostConfig.Sysctls` | DENIED when non-empty. Some sysctls are not namespaced. | `policy.go::checkHostConfig` (Sysctls branch) |
| T13 | Sandbox bypass via `HostConfig.SecurityOpt: [seccomp=unconfined]` / `apparmor=unconfined` / `no-new-privileges=false` / `label=disable` / `systempaths=unconfined` | Allowlisted against `Config.AllowedSecurityOpts`; default empty = deny-all. | `policy.go::checkHostConfig` (SecurityOpt branch), `Config.AllowedSecurityOpts` |
| T14 | Resource exhaustion via `HostConfig.Memory=10TB`, `CpuQuota=very-large`, `NanoCpus=very-large` | Cap-strict-mode: when the corresponding `Config.Max*` cap is set, the field MUST be present and a positive value within the cap. The docker-semantic "0 means unbounded" bypass is closed for all three. | `policy.go::checkHostConfig` (Memory, CpuQuota, NanoCpus branches), `Config.MaxMemoryBytes` / `MaxCPUQuota` / `MaxNanoCpus` |
| T15 | Post-create cap relax via `POST /containers/{id}/update` resetting Memory to 0 | The `update` endpoint runs through the same cap-strict-mode check as `create`. | `endpoints.go::endpointPolicyUpdate`, `policy.go::inspectUpdate` |
| T16 | Privilege escalation via `POST /containers/{id}/exec` body's own `Privileged: true` | `exec` body is parsed with `containerExecBody` and `Privileged: true` is rejected. | `policy.go::inspectExec`, `containerExecBody.Privileged` |
| T17 | Log-driver shipping container logs off the host via `LogConfig.Type=syslog`/`splunk`/`fluentd`/`gelf`/`awslogs` | `LogConfig.Type` is value-allowlisted to local-only drivers `{json-file, none, journald, k8s-file, passthrough, passthrough-tty}`. | `policy.go::checkLogConfigType`, `logConfigTypeAllowlist` |
| T18 | `Mount.Type=glob` — podman-specific value that calls `filepath.Glob(Source)` on the host | `Mount.Type` value allowlist is case-sensitive `{bind, volume, tmpfs}` (inline `switch` in `checkHostConfig`); everything else (`glob`, `image`, `npipe`, `artifact`, `ramfs`, `devpts`, empty string, case variants, whitespace, unicode zero-width space) hits the `default:` branch and rejects. | `policy.go::checkHostConfig` (Mounts branch — `switch m.Type` / `case "bind"`, `case "volume"`, `case "tmpfs"`, `default:`) |
| T19 | Raw-socket bypass: agent runs `curl --unix-socket /run/user/$UID/podman/podman.sock ...` to talk to the real socket directly | No path exists — the real socket is bound NEITHER into the bwrap mount tree NOR into the sandbox-exec SBPL allow list. The only socket reachable from inside the sandbox is the filtered one. | bwrap profile in `internal/container/bwrap.go`; SBPL in `internal/container/sandbox_exec.go`; absence asserted by negative-mutation tests |
| T20 | CLI-wrapper bypass: agent runs a custom HTTP client bypassing `podman`/`docker` CLI | Same answer as T19. The wrapper IS the only reachable surface. |
| T21 | Build context smuggle via `POST /build` of arbitrary-content tar | No new escape: `build` is bounded by what the sandbox already exposes. Build endpoint is `endpointAllow` (query-only and opaque body). No size cap in v1; revisit if abuse appears. | `endpoints.go` (build endpoint) |
| T22 | Schema drift: a new docker-/podman-API field upstream introduces a new escape vector without anyone in this repo noticing | `json.Decoder.DisallowUnknownFields()` runs on every parsed body. A new unknown field rejects with 403 and audit reason `unknown_field:<json error>` until it is admitted via the field-admission process (§4). | `policy.go::decodeStrict`, plus every typed struct |
| T23 | Proxy itself has a parsing bug | Default-deny — every unknown endpoint, unknown field, unknown enumerable value, malformed JSON, missing required value rejects before forwarding. Test suite exercises every documented escape and asserts it is blocked, plus a negative-control meta-test that verifies the positive tests are not no-ops. | `proxy_security_test.go::TestSecurity_NegativeControl_RootAllowlistPasses` |

**Network egress** is not restricted: containers get whatever network the
host podman gives them (default: full internet). This is strictly broader
than what the sandbox would otherwise allow at the policy layer
(`(allow network*)` in sandbox-exec, no `--unshare-net` in bwrap), so no
*new* egress capability is introduced. If we later want egress restrictions
they belong in a follow-up issue.

## 3. Default-deny is enforced at six layers

The proxy enforces default-deny at six independent layers. Each layer was
added or inverted in response to a class of finding that the prior layers
could not cover. The shipped model is materially stronger than the original
design (#2317 §4) — that design described the body-content layer (layer 4)
and assumed a permissive parser; cycles 2–6 added the four layers around
it.

| # | Layer | Mechanism | First added |
|---|---|---|---|
| 1 | **Endpoint** | Positive allowlist via `classifyRequest` in `internal/podmanproxy/endpoints.go`; the last branch of every method helper is `endpointDeny`. Any unknown path/method pair returns 403 with a friendly JSON envelope that names the rejected endpoint. | PR #2326 cycle 1 |
| 2 | **Field-name** | `json.Decoder.DisallowUnknownFields()` + typed struct per body-bearing endpoint. The structs (`hostConfig`, `containerCreateBody`, `containerExecBody`, `volumeCreateBody`, `networkCreateBody`) are the canonical security spec. Every admitted field is annotated `INSPECTED` (policy-checked), `DENIED` (rejected when non-empty/non-default), or `FORWARDED` (admitted as opaque `json.RawMessage`, bytes pass to the upstream unmodified). A future docker-API field that introduces an escape vector is rejected by default until it is explicitly admitted. | PR #2326 cycle 5 |
| 3 | **Field-value** | Per-field literal allowlists (+ `NetworkMode` user-defined-name regex `networkNameRegex`) for enumerable values: `Mount.Type` (inline `switch` in `checkHostConfig` Mounts loop), the six `*Mode` fields (`networkModeFixedLiterals` for `NetworkMode`; `simpleNamespaceLiterals` for `PidMode`/`IpcMode`/`UTSMode`/`UsernsMode`/`CgroupnsMode`), `LogConfig.Type` (`logConfigTypeAllowlist`). Anything outside the allowlist denies. Closes the class where the field is admitted but a dangerous value of that field forwards (cycle-6 CRITICAL: `Mount.Type=glob` was originally forwarded because the parser was deny-list — it only rejected if `Type != "bind" && Type != "volume"`). | PR #2326 cycle 6 |
| 4 | **Body-content** | `checkHostConfig` walks the parsed HostConfig and rejects dangerous values: `Privileged: true`, host-namespace modes, non-empty `Devices` / `DeviceCgroupRules` / `DeviceRequests` / `VolumesFrom`, present `MaskedPaths` / `ReadonlyPaths`, non-empty `Sysctls`, `Mounts[].VolumeOptions.DriverConfig` non-nil, `CapAdd` outside allowlist, `SecurityOpt` outside allowlist, resource caps in strict mode. | PR #2326 cycles 1 + 2 + 4 |
| 5 | **Path-resolution** | `filepath.EvalSymlinks` + lexical `filepath.Clean` on both bind sources AND allowlist entries before the prefix comparison. Relative paths, broken symlink chains, and non-existent sources all deny. Closes the cycle-3 CRITICAL where the agent planted a symlink inside an allowed prefix pointing at `/etc/passwd` and the lexical prefix-match passed. See §5 for the residual TOCTOU. | PR #2326 cycle 3 |
| 6 | **Query** | `PUT /containers/{id}/archive` `path` query parameter is checked against `AllowedBindSources` with the same canonicalised-prefix logic as `Binds`. The other endpoint with a security-relevant query (`POST /containers/create?name=<…>`) is covered by the cycle-7 container-name auto-prefix policy in `applyContainerNamePolicy`. | PR #2326 cycle 1 (archive) + PR #2332 (Name query) |

## 4. Field-admission process

The cycle-5 schema inversion means any new docker-/podman-API field
introduced upstream is rejected by default until it has been audited
against the threat table above and explicitly admitted to the relevant
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
   // CpuShares; no escape vector against #2317 §4 / the threat table.
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
     checked by a dedicated `check<Field>` helper (e.g.
     `checkNetworkMode`, `checkLogConfigType`).
   - **Inline `switch` with explicit `case` branches and a `default:`
     deny** — used by `Mount.Type` inside `checkHostConfig`'s Mounts
     loop. Best when the per-value action varies (`case "bind"` runs
     the bind-source policy, `case "volume"` runs the DriverConfig
     check, `case "tmpfs"` is a no-op forward, `default:` denies).

   Either pattern is acceptable; pick whichever matches the shape of
   the policy decision the field needs.

The single most important reviewer task on a change to `policy.go` is
**reading the struct and confirming the rationale comments**, not guessing
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
  informally; will become a Step-N+1 issue if/when the gap is exercised.
- **Darwin**: equivalent SBPL restriction on the scratch directory via
  `(deny file-issue-extension*)` / `(deny file-link*)` clauses. Same
  status: future hardening, not gated on a concrete report.

The `internal/podmanproxy/doc.go` package doc carries the same note;
both surfaces are intentionally redundant so the residual is hard to
miss.

## 6. Verification — the test suite as the spec

The body of evidence for "this policy is enforced" is the test suite at
`internal/podmanproxy/proxy_security_test.go` — 83 top-level test
functions at this writing, many with multiple subtests, all green under
`-race`. (The companion files `proxy_test.go` and
`proxy_name_prefix_test.go` add a further ~40 top-level tests covering
the constructor, lifecycle, and cycle-7 Name-prefix policy.) The key
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
it fail → re-apply the fix → see it pass) was applied to every cycle-2-
through-6 closure in PR #2326; commit messages in that PR carry the
per-cycle matrix.

Tests for sidecar wiring live at
`internal/sidecar/podman_proxy_test.go` (Step 3) and assert that:

- `containers_enabled=0` sessions do NOT bind a `podman.sock` listener
  in the per-session run dir, and emit NO audit-log file.
- `containers_enabled=1` sessions DO bind the listener and the audit
  file appears at `<sessionDir>/podman-proxy.log`.
- A request to the filtered socket reaches the (fake) upstream and the
  audit log records the call.

Tests for the bwrap profile additions live at
`internal/container/bwrap_test.go` (Step 4); tests for the sandbox-exec
profile additions live at
`internal/container/sandbox_exec_podman_proxy_test.go` and
`internal/container/sandbox_exec_podman_proxy_prepare_test.go`, plus the
integration test under `internal/integration/` per the
[sandbox-exec testing convention](sandbox-exec-testing.md) (Step 5).

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
| `endpoint` | Full request path (with the docker/podman API version prefix, e.g. `/v1.41/...` or `/v5/libpod/...`). |
| `decision` | `allow` (forwarded upstream) or `deny` (synthesised response from the proxy). |
| `reason` | Structured token naming the policy check that fired. Two shapes: (a) bare body-policy tokens — e.g. `host_bind:<path>`, `privileged`, `cap_add:<cap>`, `mount_bind:<source>`, `mount_volume_driver_config`, `mount_type_not_allowed:<type>`, `networkmode_host` / `networkmode_colon` / `networkmode_slash` / `networkmode_whitespace`, the same suffix family on `pidmode_*` / `ipcmode_*` / `utsmode_*` / `usernsmode_*` / `cgroupnsmode_*` — emitted by `policy.go::checkHostConfig` and friends; (b) endpoint-prefixed schema errors — `create_top:`, `create_hostconfig:`, `update:`, `exec:`, `volumes_create:`, `networks_create:`, `archive_path:`, `archive_missing_path`, `endpoint_not_allowed:` — followed by an `unknown_field:<json error>` / `malformed_body:<reason>` suffix when the strict JSON decode rejects the body. Grep the audit log for these exact tokens; do not paraphrase. |

### Common rejection classes

Reason strings below are the **exact tokens** the proxy writes to the
audit log — grep them verbatim. Each entry cites the `policy.go`
callsite that formats the token so you can verify the format from the
source if your version of the proxy drifts.

| Symptom | `reason` token | Fix |
|---|---|---|
| Worker tries `-v /etc:/host alpine ...` | `host_bind:/etc` (`policy.go::checkHostConfig` Binds branch) | Expected. T1. |
| Worker tries a `Mounts[]` entry with `Type=bind` and a forbidden Source | `mount_bind:<source>` (`checkHostConfig` Mounts loop, `case "bind"`) | Expected. T2. |
| Worker tries a `Mounts[]` entry with `Type=volume` and `VolumeOptions.DriverConfig` set | `mount_volume_driver_config` (`checkHostConfig` Mounts loop, `case "volume"`) | Expected. T3 (local-driver bind-volume escape). |
| Worker tries a `Mounts[]` entry with a `Type` outside `{bind, volume, tmpfs}` (`glob`, `image`, `npipe`, case variants …) | `mount_type_not_allowed:<type>` (`checkHostConfig` Mounts loop, `default:`) | Expected. T18. |
| Worker tries `--privileged` | `privileged` (`checkHostConfig` Privileged branch) | Expected. T5. |
| Worker tries `--cap-add SYS_ADMIN` (with the default empty `AllowedCaps`) | `cap_add:SYS_ADMIN` (`checkHostConfig` CapAdd branch) | Expected. T6. If the workload genuinely needs a cap, that's a `Config.AllowedCaps` discussion — file an issue. |
| Worker tries `--network=host` | `networkmode_host` (`denyIfUnsafeModeValue` via `checkNetworkMode`; the same helper emits `networkmode_colon` / `networkmode_slash` / `networkmode_whitespace` for the other categorised value-level rejections, and `network_mode_invalid:<value>` when no class matches and the user-defined-name regex fails) | Expected. T7. |
| Worker tries `--pid=host` / `--ipc=host` / `--uts=host` / `--userns=host` / `--cgroupns=host` | `pidmode_host` / `ipcmode_host` / `utsmode_host` / `usernsmode_host` / `cgroupnsmode_host` (same `denyIfUnsafeModeValue` formatter on the five sibling fields; same `_colon` / `_slash` / `_whitespace` suffix family applies, plus `<field>_invalid:<value>` as the catch-all) | Expected. T7. |
| Worker tries a brand-new docker-/podman-API field this struct doesn't admit | `create_hostconfig:unknown_field:json: unknown field "NewField"` (or the matching `create_top:` / `update:` / `exec:` / `volumes_create:` / `networks_create:` prefix per endpoint) | Field-admission process (§4). |
| Worker gets `503` with `"podman socket unavailable: ..."` envelope | Audit log shows `decision=allow` then nothing — the proxy accepted policy-wise but the upstream isn't there | Bring the upstream up: Darwin `podman machine start`; NixOS `systemctl --user status podman.socket`. |
| Worker gets `400` with `"malformed_body:empty"` or similar | `create_top:malformed_body:empty` (or the matching endpoint prefix) | Client is sending an empty / non-JSON `POST /containers/create` body. Bug in the client, not the proxy. |

### When to escalate

Most rejections are *correct* — the agent attempted an escape and the
proxy blocked it. Escalation paths:

- **A legitimate workflow needs a field that's currently denied** —
  file an issue, follow §4. Do NOT loosen the policy in a worker PR
  without an audit.
- **The audit log shows the proxy accepted a request that should have
  been denied** — that's a P0 security bug against this package.
  Reproduce in a test, open an issue, escalate.
- **The proxy returns 5xx (not 4xx) when the policy should fire** —
  parsing or upstream-handling bug. File an issue with the failing
  request body.

## 8. Cycle history — six rounds of structural hardening

PR #2326 went through six review cycles. Each cycle's review-security
agent found a different *class* of permissiveness, not just a single
missing check; each cycle closed its findings AND inverted one layer of
the default-deny model so the next layer down became the next surface to
audit. The canonical security spec is now the typed structs and value
allowlists in `policy.go`, not the deny-list comments scattered through
old commit messages.

| Cycle | Class of finding | Layer inverted |
|---|---|---|
| 1 | Initial implementation + AC coverage | Endpoint allowlist (positive `classifyRequest` table) |
| 2 | 5 missing-field denials (`Memory=0` bypass, `NanoCpus` unparsed, `/update` resets caps, exec-`Privileged` bypass, `SecurityOpt` unfiltered) | Body-content policy expanded |
| 3 | 1 CRITICAL — bind-source check was purely lexical; symlinks bypassed it | Path resolution: `filepath.EvalSymlinks` on bind sources |
| 4 | 5 missing-field denials (`DeviceCgroupRules`, `MaskedPaths`/`ReadonlyPaths`, `CgroupnsMode`, `VolumesFrom`/`DeviceRequests`, local-driver volume bind) | Body-content policy hardened further |
| 5 | Pattern recognised: the body parser was permissive at the FIELD-NAME layer | **Field-name layer:** `json.Decoder.DisallowUnknownFields()` + typed allowlist structs at every body-bearing endpoint |
| 6 | One level down: admitted fields had VALUE-LEVEL deny-lists (`Mount.Type` forwarded everything that wasn't `bind`/`volume`, including the podman-specific `glob` Type → CRITICAL) | **Field-value layer:** per-field value allowlists for enumerable fields |

After cycle 6 the body-inspection model is default-deny at both the
field-name and the field-value layers. A future docker-API field or
value that introduces an escape vector is rejected by default until it
is explicitly admitted via the field-admission process (§4).

The cycle-7 work landed in PR #2332 (Step 7) as the container-name
auto-prefix policy on `POST /containers/create` — the same six-layer
shape applied to the `Name` field and the `?name=` URL query, so the
cleanup sweep can deterministically locate every container belonging
to a session.

## 9. Configuration knobs

The proxy's `Config` struct in `internal/podmanproxy/proxy.go` carries
the per-session knobs that the sidecar wires. The Step 3 wiring lives
in `internal/sidecar/podman_proxy.go::runPodmanProxyIfEnabled`, which
constructs a `podmanproxy.Config` literal inline (no separate builder
function — the `runPodmanProxyIfEnabled` body itself is the
spec) and sets:

- `AllowedBindSources` — the per-session worktree path, the bare repo
  path, and the per-session scratch directory.
- `ContainerNamePrefix` — `"prism-" + sessionName + "-"`, which
  activates the cycle-7 auto-prefix policy and lets the cleanup sweep
  (`cmd/cleanup_sweep.go::sweepOrphanContainersForSession`, called
  from `cmd/cleanup.go`) find every container belonging to the
  session.
- `AllowedCaps` — empty by default; deny-all.
- `AllowedSecurityOpts` — empty by default; deny-all.
- `MaxMemoryBytes` / `MaxCPUQuota` / `MaxNanoCpus` — unset by default
  (no cap-strict-mode); when set, the cap is enforced AND the field is
  required.
- `AuditWriter` — the `os.File` for `<sessionDir>/podman-proxy.log`.

Out-of-tree callers may construct the `Config` differently. The proxy
package itself imposes no policy beyond what the `Config` declares.

## 10. Linger decision

**No `loginctl enable-linger <user>` for v1.** The proxy is only
used by interactive prism sessions — when the user has a graphical
session, the user-systemd instance is running, and the rootless
podman socket unit is socket-activated on first use. Users running
long-lived background workers that need podman across SSH-out can
re-evaluate later.

This is documented here rather than enforced in nix because the
decision is operational, not structural: enabling linger requires no
code change, and disabling it later requires no code change. No
host in this flake currently sets it, and this PR did not change
that.

## 11. References

- Parent issue: [#2317](https://github.com/prismatic-koi/nixos-config/issues/2317) — design, threat-table sketch, parent ACs.
- Step 1 PR: [#2326](https://github.com/prismatic-koi/nixos-config/pull/2326) — the proxy package itself, plus the six-cycle hardening history.
- Step 2 PR: [#2327](https://github.com/prismatic-koi/nixos-config/pull/2327) — DB migration v36→v37.
- Step 3 PR: [#2328](https://github.com/prismatic-koi/nixos-config/pull/2328) — sidecar wiring.
- Step 4 PR: [#2329](https://github.com/prismatic-koi/nixos-config/pull/2329) — bwrap profile.
- Step 5 PR: [#2331](https://github.com/prismatic-koi/nixos-config/pull/2331) — sandbox-exec profile.
- Step 6 PR: [#2330](https://github.com/prismatic-koi/nixos-config/pull/2330) — `--containers` spawn flag.
- Step 7 PR: [#2332](https://github.com/prismatic-koi/nixos-config/pull/2332) — orphan-container cleanup sweep + auto-prefix on `Name`.
- Canonical structs: [`internal/podmanproxy/policy.go`](../internal/podmanproxy/policy.go) (`hostConfig`, `containerCreateBody`, `containerExecBody`, `volumeCreateBody`, `networkCreateBody`).
- Package doc: [`internal/podmanproxy/doc.go`](../internal/podmanproxy/doc.go) — the in-tree summary of the threat model; references `#2317 §4` directly. The shipped threat table in this document (§2) explicitly supersedes `#2317 §4` after cycles 2–6. Consider `doc.go` the inline summary and this document the canonical reference.
- Sidecar wiring: [`internal/sidecar/podman_proxy.go`](../internal/sidecar/podman_proxy.go) — `runPodmanProxyIfEnabled`.
- Cleanup sweep: [`cmd/cleanup_sweep.go`](../cmd/cleanup_sweep.go) — `sweepOrphanContainersForSession` (called from `cmd/cleanup.go`).
- Sandbox-exec testing convention: [`docs/sandbox-exec-testing.md`](sandbox-exec-testing.md).
