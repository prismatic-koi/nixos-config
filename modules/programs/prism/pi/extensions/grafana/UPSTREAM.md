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
  `GRAFANA_SERVICE_ACCOUNT_TOKEN` from its own process environment at startup.

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
GRAFANA_SERVICE_ACCOUNT_TOKEN=<service-account-token>
```

The extension reads the file when the family is activated (see "Deferred
registration" below), parses it line-by-line, and passes the two values through
to the `mcp-grafana` child process's env. The env-file shape is deliberately
trivial to parse and is what mcp-grafana would consume itself if invoked from a
shell.

`config-loader.ts` keeps bundle CONTENT out of its exception messages. Since
issue #2532 those messages travel into a tool result, and therefore into the
model's transcript, so a malformed line is reported by line number only.

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

## Sandbox-exec (Darwin) — the secrets.d carve-out

Darwin has no bind mounts, so the delivery problem is the inverse of the bwrap
one: the file is already at the path the env var names, and the question is
whether the sandbox may READ it. By default it may not. `sandbox_exec.go` §3c
denies the entire `secrets.d` subtree and re-allows a hand-maintained
inventory of secret NAMES in `collectSecretsDAllowlistNames`. Until issue
#2746 grafana was not on that list, so `pi.grafana.enable = true` on a
sandbox-exec host was rejected at eval time by an assertion.

#2746 removed the assertion and added the bundle to the inventory. The
mechanism mirrors the `gitlab_token` carve-out (#2668):

- `Config.GrafanaConfigPath` carries the same path prism injects as
  `GRAFANA_MCP_CONFIG_PATH`. The sandbox-exec spawn path
  (`cmd/agent_run_sandbox_exec_darwin.go`) copies it off the role-filtered
  agent env map.
- `collectSecretsDAllowlistNames` resolves that path with `EvalSymlinks`,
  extracts the `secrets.d/<N>/<name>` name, and emits ONE `require-not`
  exception for it. The regex matches any generation counter, so the grant
  survives a sops rotation mid-session.
- When the path is empty the exception is absent and the bundle stays denied.
  That covers a host with grafana disabled AND every review role, whose
  `GRAFANA_MCP_CONFIG_PATH` is stripped by `internal/config/agent_env_roles.go`
  (#2533) — so the file grant tracks the tool capability with no second list.

Every other name under `secrets.d/<N>/` — `github_token`, the role PATs,
`aws-config`, `workkube` — stays denied. The paired positive/negative
integration tests live in
`internal/integration/sandbox_exec_grafana_config_darwin_test.go`, per
`sandbox-exec-testing.md`; the profile-shape unit tests live in
`internal/container/grafana_sandbox_test.go`.

Darwin host-mode sessions have no SBPL profile and were never affected.

## Deferred registration (issue #2532)

The extension registers exactly ONE tool at `session_start`:
`activate_grafana`. It reads two environment variables and nothing else — no
sops bundle read, no `mcp-grafana` child process, no `tools/list`. Everything
else happens on the first call to `activate_grafana`.

Why: the 65 tool schemas sit in the Anthropic `tools` array, which is the first
segment of the cached prompt prefix. Every session paid about 26400 cached
tokens for them (issue #2531), and most sessions never call a Grafana tool.

The mechanism is native to pi and needs no third-party MCP CLI:

- An unregistered tool never reaches the wire. The request `tools` array is
  built from `agent.state.tools`.
- `registerTool()` works after startup and refreshes the registry immediately.
- A tool registered mid-session AUTO-ACTIVATES, so "defer registration
  entirely" is the correct shape; `setActiveTools` is not used here.
- `emitBeforeAgentStart` is awaited before the request is built, so the eager
  path registers in time for the turn that triggered it.

Two rules follow, because activation invalidates the Anthropic prompt cache
once (tools sit in front of every cache breakpoint):

1. Activate the whole family in one call. Never one tool at a time.
2. Never deactivate to tidy up. pi has no `unregisterTool`.

The shared state machine lives in `../mcp-activation/activation.ts` and is used
by all three MCP extensions. `nx.programs.prism.pi.grafana.eagerRoles` names
the agent roles that skip the tool call and activate from their first
`before_agent_start`; it defaults to `[ ]`.

The role is read from `process.argv` by `readAgentRoleFromArgv`
(`../mcp-activation/activation.ts`), NOT from `pi.getFlag("agent")`.

That distinction is load-bearing, and getting it wrong is fatal. pi scopes
`getFlag` to the extension that registered the flag, so using it would force
each MCP extension to call `pi.registerFlag("agent", ...)`. pi's
`detectExtensionConflicts` (`dist/core/resource-loader.js`) records a conflict
whenever two different extension PATHS own the same flag name,
`addExtensionConflictDiagnostics` pushes it into `extensionsResult.errors`,
`main.js` maps every such error to `{type: "error"}`, and startup then calls
`process.exit(1)`. prism always loads `prism.ts` — which owns `--agent` — via
`--extension`, so a second registration stops EVERY session on EVERY machine
from starting. Reproduced against the pinned pi 0.82.1:

```
Error: Failed to load extension ".../grafana/index.ts": Flag "--agent"
conflicts with .../prism.ts                                  (exit code 1)
```

This repo has been bitten by it before — see the #2068 post-mortem note in
`modules/programs/prism/pi.nix`, which names `--agent` as the conflicting flag
that "broke the entire prism↔pi integration surface on every bwrap session".

argv is the better source regardless: prism emits `--agent <role>` directly
(`internal/container/pi_invocation.go`), so the value is available with no
cross-extension coupling and no ordering hazard. `mcp-activation/
activation.test.ts` pins this three ways: a source guard asserting no MCP
extension calls `registerFlag`, argv-parsing unit tests, and an integration
test that loads `prism.ts` and `grafana/index.ts` together under the real pi
binary and asserts no conflict diagnostic.

NIX LAYOUT NOTE. `mcp-activation` is copied into each provider's derivation and
the provider's own files move down one level, so the store tree is
`$out/grafana/index.ts` next to `$out/mcp-activation/activation.ts`. That is
what makes the relative import `../mcp-activation/activation.ts` resolve
identically in the source tree and in the nix store.

## Tool surface

Full surface for v1 — on activation the extension calls `tools/list` and
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
   machine; we log a debug line and return. Not even `activate_grafana` is
   registered.
2. **Config file missing or malformed** — surfaced as an `{isError:true}`
   result from `activate_grafana`; no tools registered; session proceeds.
3. **`mcp-grafana` child fails to spawn, exits, or errors during initialise /
   tools/list** — surfaced as an `{isError:true}` result from
   `activate_grafana`; the child is torn down; no tools registered; session
   proceeds.

Since #2532 these failures land inside a tool call rather than at
`session_start`. That is a SMALLER blast radius, not a larger one: a broken
provider now produces one bad tool result instead of a startup-time error, and
the gateway stays retryable so a fixed config works without a restart. On the
eager path there is no tool result to carry the message, so a failure is
surfaced via `ctx.ui.notify` instead.

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
  `tools/list` on activation and registers whatever the current binary version
  returns. If the new tools change what the family covers, update
  `ACTIVATE_GRAFANA_DESCRIPTION` in `extension.ts` too: that description is the
  only Grafana text a non-eager session ever sees, so it is what the agent uses
  to decide whether to activate.
- **Darwin support.** See "Sandbox-exec (Darwin) — the secrets.d carve-out"
  above. A new sops bundle needs no code change; the carve-out derives the
  secret name from the configured path.
