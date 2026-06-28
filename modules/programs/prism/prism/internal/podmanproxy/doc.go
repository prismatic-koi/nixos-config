// Package podmanproxy implements a default-deny HTTP filter proxy for the
// docker / podman REST API socket.
//
// The proxy is a self-contained library with no prism-specific imports
// beyond the standard library. It is intended to be embedded by a longer-
// running owner (for example, the per-session prism sidecar — Step 3 of
// the train tracked by issue #2317) which wires it up to a per-session
// Unix socket and the host's real podman socket. This package itself
// performs no session bookkeeping, no DB writes, and no isolation-mode
// reasoning — those concerns live in the caller.
//
// # Threat model
//
// The proxy is the security boundary between an untrusted agent that
// controls one end of the socket and the host's real container API on
// the other end. The escapes the proxy is designed to block are listed
// in #2317 §4: host-path bind mounts, --privileged, host network / pid /
// ipc / uts / userns modes, cap-add SYS_ADMIN / SYS_PTRACE / etc., host
// device passthrough, podman cp exfiltration, and raw-socket fallback
// (the only socket exposed to the agent IS the filter).
//
// # Default-deny
//
// Every request is denied unless it matches an explicit allow rule.
// Three classes of allow are enforced:
//
//   - Endpoint-level: any path/method pair that does not appear in the
//     allowlist returns 403 with a friendly JSON envelope that names the
//     rejected endpoint.
//   - Body-level: containers/create requests are parsed and their
//     HostConfig.* fields are checked. Any Binds source outside the
//     configured prefix allowlist, any Mounts of type "bind" outside the
//     allowlist, any Privileged: true, any host {Network,Pid,Ipc,UTS,Userns}
//     mode, any non-empty Devices, any CapAdd entry outside the cap
//     allowlist, or any resource cap above the configured upper bound
//     returns 403.
//   - Query-level: PUT containers/{id}/archive requires its `path` query
//     parameter to fall under the same prefix allowlist as bind sources.
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
// Closing the window properly requires either kernel-level fs
// freezing or filesystem snapshots, both out of scope for a Step-1
// library PR.
//
// Step 3 (sidecar wiring) and later should evaluate mounting the
// per-session scratch directory with `nosymfollow` (Linux) or the
// equivalent SBPL restriction (Darwin) where the platform permits.
// That moves the TOCTOU mitigation from this proxy into the
// surrounding sandbox profile, which is the correct architectural
// home for it.
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
