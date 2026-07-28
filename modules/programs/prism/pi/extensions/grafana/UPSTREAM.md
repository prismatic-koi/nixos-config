# Upstream Sync State — pi Grafana MCP Extension

This extension is original code (not vendored from an upstream); it was written
for issue #2452. It bridges pi to the `mcp-grafana` Go binary from nixpkgs
(<https://github.com/grafana/mcp-grafana>), packaged as `pkgs.mcp-grafana`, via
a **stdio** MCP transport.

## Why stdio (and not HTTP like atlassian / notion)

The sibling `../atlassian/` and `../notion/` extensions talk to hosted remote
MCP servers (`mcp.atlassian.com`, `mcp.notion.com`) over Streamable HTTP with
OAuth. `mcp-grafana` is different: it is a **local Go binary** that talks to a
Grafana instance's REST API using a static service-account token. There is no
hosted-remote path to fall back to; the binary MUST be spawned per session.
The mcp-grafana binary defaults to `-transport stdio` when invoked with no
arguments, which is exactly the shape MCP clients expect when they own the
child-process lifecycle.

So `mcp-client.ts` here is a hand-rolled **stdio** MCP client. It is NOT a
copy of the atlassian/notion HTTP client — the transports are different in
kind, not degree:

- Framing: JSON-RPC over newline-delimited JSON on stdout (LSP-style Content-
  Length framing is NOT used by the MCP stdio transport; each message is a
  single line of JSON terminated by `\n`).
- Session lifecycle: bound to the child process. When the child exits, the
  session is dead — there is no `Mcp-Session-Id` to replay.
- Auth: no bearer token, no refresh flow. The binary reads `GRAFANA_URL` and
  `GRAFANA_API_KEY` from its own process environment at startup.

## Config bundle in sops

Both the Grafana URL and API key are secret (the repo is public), so they
travel together as one atomic, **selectable** config bundle keyed
`grafana_config_<name>` inside `modules/programs/prism/secrets/grafana.sops.yaml`.
Today there is exactly one bundle — `grafana_config_home` — pointing at the
self-hosted Grafana instance. Adding a second bundle later is a new
`grafana_config_work` entry plus a one-line selector change on the relevant
machine; no schema change.

The bundle format is `KEY=VALUE` lines (dotenv-style), one per line:

```
GRAFANA_URL=<grafana-instance-url>
GRAFANA_API_KEY=<service-account-token>
```

The extension reads the file at session_start, parses it line-by-line, and
passes the two values through to the `mcp-grafana` child process's env. The
env-file shape is deliberately trivial to parse and is what mcp-grafana would
consume itself if invoked from a shell.

## Env-var routing and the sandbox reachability gotcha

Two pieces of information have to reach the extension inside a
prism-spawned bwrap sandbox:

1. The absolute host path to the selected sops-decrypted bundle
   (`GRAFANA_MCP_CONFIG_PATH`).
2. The absolute host path to the `mcp-grafana` Nix-store binary
   (`PI_GRAFANA_MCP_BIN`).

Both are delivered through `nx.programs.prism.agent.envVars`, NOT through
`environment.sessionVariables` or a zsh alias. `agent.envVars` is the channel
that actually reaches prism-spawned agents (it is serialised as
`agent_env_vars` in profiles.json and applied by all three isolators). Values
are injected verbatim — no shell in the loop — so a `$(cat <path>)`
sessionVariables shape works in login shells but silently breaks for
prism-spawned bwrap agents. That failure mode is the reason this extension
does not read the URL / token from separate env vars: the ONLY way to keep
the token off disk-plaintext AND make it reachable inside bwrap is to bind
the sops-decrypted file itself and let the extension parse it.

The bind is added by `internal/container/bwrap.go`, which reads
`cfg.AgentEnvVars["GRAFANA_MCP_CONFIG_PATH"]` and emits
`--ro-bind <EvalSymlinks(env-var-path)> <env-var-path>` (Src≠Dst — same
shape as the AWS / kube XDG binds in `mounts.go`). Src is the sops-resolved
concrete file at `/run/secrets.d/<N>/<name>` (EvalSymlinks pins the inode
for mid-session sops rotation safety, mirroring the SSH signing-key bind).
Dst is the env-var path exactly — the sops-nix symlink `/run/secrets/<name>`
— so the extension's `readFileSync(process.env.GRAFANA_MCP_CONFIG_PATH)`
finds a real file at that path inside the sandbox. Binding Dst==Src (as an
earlier iteration did) leaves the env-var path unreachable in the sandbox
namespace and every session ENOENTs silently. When the env var is unset
(grafana disabled) the bind is a no-op.

## Sandbox-exec (Darwin) — not supported in v1

The extension is Linux-only in v1. `nx.programs.prism.pi.grafana.enable = true`
on Darwin fails at eval time via a `pi.nix` assertion. The reason is
`sandbox_exec.go` §3c: the entire `secrets.d` subtree is denied by default
with named re-allow exceptions maintained by hand in
`collectSecretsDAllowlistNames`. Adding grafana to that allowlist is a
deliberate audit-required change that this PR intentionally defers — m4mac
(the sole Darwin host in this flake) leaves grafana disabled per the issue's
design decisions, so there is no forcing function for the sandbox-exec work.
When a Darwin host wants grafana, the follow-up is: add the grafana secret
name to `collectSecretsDAllowlistNames` behind a config gate, add the
positive+negative test pair per `sandbox-exec-testing.md`, and drop the
assertion.

## Tool surface

Full surface for v1 — the extension calls `tools/list` at startup and
registers every returned tool via `pi.registerTool()` with no filtering.
mcp-grafana returns roughly 30-40 tools spanning dashboards, alerting,
data-source queries (Prometheus, Loki, ClickHouse, Elasticsearch, Athena,
CloudWatch, ...), and admin operations. Scoping / field-slimming can come
later if the surface proves noisy in practice; the extension already routes
every tool call through a single `execute` shim, which is the right seam for
a future `slim.ts`.

## Failure modes and graceful degradation

Per the issue's edge-case ACs, the extension MUST NOT prevent a pi session
from starting when anything on the grafana path fails. The three
degradation paths:

1. **`GRAFANA_MCP_CONFIG_PATH` unset** — grafana is not configured on this
   machine; we log a debug line and return.
2. **Config file missing or malformed** — surfaced via `ctx.ui.notify` as a
   warning; no tools registered; session proceeds.
3. **`mcp-grafana` child fails to spawn, exits, or errors during initialise /
   tools/list** — surfaced via `ctx.ui.notify` as an error; the child is
   torn down; no tools registered; session proceeds.

Once tools ARE registered, a runtime `tools/call` failure (child died,
stdio pipe broken, timeout) is surfaced to the caller as a `{isError:true}`
tool result, not as an exception. The session continues.

## Future updates

- **Adding a new instance selector.** Add a new bundle to
  `grafana.sops.yaml` keyed `grafana_config_<name>`, then set
  `nx.programs.prism.pi.grafana.config = "<name>"` on the relevant machine.
  No code change; the Nix module reads the selector and points the sops
  secret at the corresponding key.
- **Rotating a token.** Edit `grafana.sops.yaml` in-place with sops; run
  `nh switch`; new sessions pick up the new token immediately (bwrap
  bind-mounts are inode-based on Linux, so mid-session rotations survive
  cleanly — the same argument that `bwrap.go`'s SSH signing-key comment
  spells out at length).
- **New mcp-grafana tools.** Automatic — the extension enumerates
  `tools/list` at every session_start and registers whatever the current
  binary version returns.
- **Darwin support.** See "Sandbox-exec (Darwin) — not supported in v1" above.
