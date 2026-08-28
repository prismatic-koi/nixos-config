// Package podmanproxy implements a default-deny HTTP filter proxy for the
// docker / podman REST API socket.
//
// The proxy is a self-contained library with no prism-specific imports
// beyond the standard library. A longer-running owner embeds it (for
// example, the per-session prism sidecar) and wires it to a per-session
// Unix socket and the host's real podman socket. This package performs
// no session bookkeeping, no DB writes, and no isolation-mode
// reasoning. Those concerns live in the caller.
//
// # Threat model
//
// The proxy is the security boundary between an untrusted agent that
// controls one end of the socket and the host's real container API on
// the other end. The proxy blocks these escapes: host-path bind mounts,
// --privileged, host network / pid / ipc / uts / userns modes, cap-add
// SYS_ADMIN / SYS_PTRACE and similar, host device passthrough, podman cp
// exfiltration, and raw-socket fallback (the only socket exposed to the
// agent is the filter).
//
// # Default-deny — endpoint, body, and field layers
//
// Every request is denied unless it matches an explicit allow rule.
// Five classes of allow are enforced:
//
//   - Endpoint-level: any path/method pair that does not appear in
//     the allowlist returns 403 with a friendly JSON envelope that
//     names the rejected endpoint.
//   - Body-level: containers/create / containers/{id}/update /
//     containers/{id}/exec / volumes/create / networks/create
//     bodies are parsed and policy-checked.
//   - Field-level: every parsed body runs with
//     json.Decoder.DisallowUnknownFields(). The typed struct for
//     each body shape (hostConfig, containerCreateBody,
//     containerExecBody, volumeCreateBody, networkCreateBody) is
//     the security spec — every admitted field is annotated as
//     INSPECTED (policy-checked), DENIED (rejected when non-empty),
//     or FORWARDED (admitted as opaque json.RawMessage and passed
//     to the upstream unmodified). A future docker-API field that
//     introduces an escape vector is rejected by default until it
//     is explicitly admitted.
//   - Value-level: for admitted enumerable fields (Mount.Type,
//     NetworkMode, PidMode/IpcMode/UTSMode/UsernsMode/CgroupnsMode,
//     LogConfig.Type), the value MUST match an explicit literal
//     allowlist; NetworkMode additionally accepts a user-defined
//     name regex. Anything outside the allowlist denies. Value
//     allowlists live in policy.go alongside the inspector
//     functions (for example, checkNetworkMode and
//     checkLogConfigType) and are the canonical security spec for
//     enumerable values.
//   - Query-level: PUT containers/{id}/archive requires its `path`
//     query parameter to fall under the same prefix allowlist as
//     bind sources.
//
// # Field admission process
//
// When a workflow surfaces a HostConfig (or top-level / exec /
// update / volume / network) field that the current allowlist does
// not admit, the request fails with a 403 containing "unknown field
// <Name>" in the message. The process for admitting a new field:
//
//  1. File an issue describing the workflow and the field.
//  2. Audit the field against the threat table in
//     docs/podman-proxy.md §2. Classify as INSPECTED / DENIED /
//     FORWARDED.
//  3. Open a PR that adds the field to the appropriate struct in
//     policy.go with a rationale comment matching the existing
//     style. INSPECTED additions need a policy check + a non-vacuous
//     test pair (positive + revert-and-watch-fail).
//  4. The struct definition is the canonical spec. Audit a change by
//     reading the struct, not by guessing what the proxy admits.
//
// Streaming endpoints — /containers/{id}/attach, /exec/{id}/start, and
// the follow=1 variant of /containers/{id}/logs — are forwarded without
// body parsing. They have no JSON body to inspect and the dangerous
// fields all live on containers/create, so the endpoint allowlist alone
// is sufficient.
//
// # Bind-source symlink resolution and residual TOCTOU
//
// The bind-source allowlist check resolves symlinks (via
// filepath.EvalSymlinks) on both the requested source and the
// allowlist entries before the prefix comparison. This closes the
// purely-lexical bypass where an agent with write access to a path
// inside an allowed prefix could plant a symlink to /etc/passwd (or
// any other forbidden host path) and have the kernel follow it when
// runc mounts the bind.
//
// A residual TOCTOU window remains: the symlink resolution runs at
// policy time, mount(2) runs in podman/runc some milliseconds later.
// The agent can race the gap by swapping a safe symlink for a
// dangerous one between the two moments. For v1 this residual is
// acceptable because (a) the agent is otherwise sandboxed, (b) the
// bind target inside the container is fixed at create time so a
// changed symlink cannot retarget the agent's reach, and (c) the
// sandbox layer (bwrap / sandbox-exec) sits in front of the proxy.
// A full close of the window needs either kernel-level filesystem
// freezing or filesystem snapshots. Both are out of scope for this
// proxy library.
//
// The correct architectural home for the mitigation is the
// surrounding sandbox profile: mount the per-session scratch
// directory with `nosymfollow` (Linux) or the equivalent SBPL
// restriction (Darwin) where the platform permits.
//
// # JSON error envelope
//
// Every synthesised error response (denial or upstream-unavailable) uses
// the docker-compatible envelope:
//
//	{"message": "..."}
//
// This shape is the lowest common denominator across the docker CLI,
// podman, and the various Go client libraries (containers/buildah,
// docker/docker, fsouza/go-dockerclient). The upstream-unavailable
// envelope additionally carries actionable text naming the platform
// commands the operator should run to bring the socket back up.
package podmanproxy
