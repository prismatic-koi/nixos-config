// pi-agnostic core of the Grafana MCP extension.
//
// index.ts is a thin wiring shell that supplies the one dependency this module
// cannot import for itself — `typebox`'s Type.Unsafe — and forwards pi's
// `session_start` and `before_agent_start` events in here.
//
// Why the split. `index.ts` imports `typebox`, which only resolves inside pi's
// own runtime, so anything reachable only through `index.ts` cannot be unit
// tested. The deferral gate is exactly the logic that most needs coverage, so
// it lives here behind a small injected-dependency surface. This mirrors the
// notion extension, which already uses this shape.
//
// DEFERRED REGISTRATION (issue #2532)
//
// This extension used to spawn `mcp-grafana`, read the sops config bundle, run
// `tools/list`, and register all 65 tools at `session_start`. That cost about
// 26400 cached tokens in the prompt prefix of EVERY session, and most sessions
// never call a Grafana tool.
//
// It now registers one tool at `session_start` — `activate_grafana` — and does
// all of that work only when the tool is called. So:
//
//   - the sops bundle is not read until activation;
//   - the `mcp-grafana` child process does not start until activation;
//   - a role listed in GRAFANA_MCP_EAGER_ROLES activates from the first
//     `before_agent_start`.
//
// The role is read from argv (`readAgentRoleFromArgv`), NOT via
// `pi.getFlag("agent")` — `getFlag` is unreachable without `registerFlag`, and
// registering `--agent` in a second extension makes pi exit 1. See
// ../mcp-activation/activation.ts for the reproduction.
//
// Every failure path stays non-blocking: a pi session MUST NOT be prevented
// from starting — or from continuing — when grafana is unconfigured, its
// bundle is malformed, or the mcp-grafana child dies. See UPSTREAM.md
// "Failure modes and graceful degradation". The failure now surfaces inside a
// tool call rather than at startup, which is a smaller blast radius, not a
// larger one.
//
// SECURITY: nothing here — logs, notifications, tool results — may quote the
// API key or the raw bundle contents. `errorLog`, `ctx.ui.notify` and the
// activation tool result see only paths and exception messages, and
// config-loader.ts is careful to keep bundle content out of its exception
// messages for exactly this reason.

import {
  createActivationGateway,
  isEagerRole,
  type GatewayHost,
  type ToolResult,
} from "../mcp-activation/activation.ts"
import { GrafanaConfigError, loadGrafanaBundle, type GrafanaBundle } from "./config-loader.ts"
import type { McpCallResult, McpTool, StdioMcpSessionOptions } from "./mcp-client.ts"

// ---------------------------------------------------------------------------
// Host-shaped types
// ---------------------------------------------------------------------------

export interface GrafanaSessionLike {
  listTools(): Promise<McpTool[]>
  callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult>
  close(): void
}

export interface NotifyContext {
  ui: { notify: (msg: string, type?: string) => void }
}

export interface GrafanaExtensionDeps {
  /** Wrap a raw MCP JSON Schema for the host's tool registry (typebox in pi). */
  wrapSchema: (schema: Record<string, unknown>) => unknown
  /** Spawn mcp-grafana and complete the MCP handshake. */
  connect: (opts: StdioMcpSessionOptions) => Promise<GrafanaSessionLike>
  /** Read the sops bundle. Defaults to the real config loader. */
  loadBundle?: (path: string) => GrafanaBundle
  /** Environment source. Defaults to process.env. */
  env?: Record<string, string | undefined>
}

// ---------------------------------------------------------------------------
// The description the model sees
//
// This is the ONLY Grafana text in the prompt prefix of a non-eager session,
// so it has to name the capability areas well enough that the agent knows when
// to reach for the family — and it has to stay short, because it is paid for
// by every session.
// ---------------------------------------------------------------------------

export const ACTIVATE_GRAFANA_DESCRIPTION =
  "Reveal the Grafana MCP tool family (about 65 tools): dashboards, datasources, " +
  "Prometheus and Loki queries and metadata, Pyroscope profiles, alert rules and " +
  "routing, OnCall schedules, incidents, Sift investigations, annotations, snapshots, " +
  "and the Grafana HTTP API. Only this tool is registered until you call it; calling " +
  "it registers the full surface and returns the number of tools available. " +
  "No arguments."

// ---------------------------------------------------------------------------
// Logging
//
// SECURITY: none of these is ever handed a credential. `log` is additionally
// gated on GRAFANA_MCP_DEBUG so a normal session stays quiet.
// ---------------------------------------------------------------------------

function log(msg: string, ...args: unknown[]): void {
  if (process.env.GRAFANA_MCP_DEBUG === "1") {
    console.error("[grafana-mcp]", msg, ...args)
  }
}

function warn(msg: string, ...args: unknown[]): void {
  console.error("[grafana-mcp] WARN:", msg, ...args)
}

function errorLog(msg: string, ...args: unknown[]): void {
  console.error("[grafana-mcp] ERROR:", msg, ...args)
}

// ---------------------------------------------------------------------------
// Extension
// ---------------------------------------------------------------------------

/**
 * Build a Grafana extension instance.
 *
 * Returned as a factory rather than module-level mutable state so each pi
 * session (and each test) gets its own session / gateway pair with no
 * cross-contamination.
 */
export function createGrafanaExtension(deps: GrafanaExtensionDeps) {
  const env = deps.env ?? process.env
  const loadBundle = deps.loadBundle ?? loadGrafanaBundle

  let session: GrafanaSessionLike | null = null
  let toolHost: GatewayHost | null = null
  let eagerChecked = false

  /**
   * Register the MCP tool surface.
   *
   * This is the single choke point through which Grafana tools reach pi.
   * Returns the number of tools registered.
   */
  function registerTools(host: GatewayHost, tools: McpTool[]): number {
    log(`registering ${tools.length} tools`)

    for (const tool of tools) {
      const toolName = tool.name
      const schema = deps.wrapSchema(
        tool.inputSchema ?? { type: "object", properties: {} },
      )

      host.registerTool({
        name: toolName,
        label: toolName,
        description: tool.description ?? toolName,
        parameters: schema,
        async execute(_toolCallId, params, _signal): Promise<ToolResult> {
          const active = session
          if (!active) {
            return {
              content: [{ type: "text", text: "Grafana MCP: no active session" }],
              isError: true,
            }
          }

          try {
            log(`calling ${toolName}`)
            const callParams = (params ?? {}) as Record<string, unknown>
            const result = await active.callTool(toolName, callParams)

            if (result.isError) {
              const raw =
                result.content?.map((c) => c.text ?? "").join("\n") ??
                "Unknown MCP tool error"
              return { content: [{ type: "text", text: raw }], isError: true }
            }

            return { content: result.content }
          } catch (err) {
            // A tool failure must never kill the pi session — surface it to
            // the model as an error result instead.
            const msg = err instanceof Error ? err.message : String(err)
            errorLog(`tool call failed for ${toolName}: ${msg}`)
            return {
              content: [{ type: "text", text: `Grafana MCP error: ${msg}` }],
              isError: true,
            }
          }
        },
      })
    }

    log(`registered ${tools.length} Grafana tools`)
    return tools.length
  }

  /**
   * The deferred work: read the bundle, spawn the child, handshake,
   * `tools/list`, register. Rejects on any failure; the gateway converts the
   * rejection into an error tool result and stays retryable.
   *
   * NOTHING in here runs at session_start. That is the whole point of #2532,
   * and grafana-extension.test.ts asserts it.
   */
  async function activate(): Promise<number> {
    const host = toolHost
    if (!host) {
      // Unreachable in pi: the gateway tool cannot be called before
      // onSessionStart registered it, and onSessionStart sets toolHost first.
      throw new Error("internal error: no tool host bound")
    }

    const configPath = env.GRAFANA_MCP_CONFIG_PATH ?? ""
    const binPath = env.PI_GRAFANA_MCP_BIN ?? ""

    // SECURITY: GrafanaConfigError messages name the path and, at worst, a
    // line number — never the bundle's contents. config-loader.ts keeps it
    // that way deliberately, because this message now reaches a tool result
    // and therefore the model's transcript, not just a UI notification.
    let bundle: GrafanaBundle
    try {
      bundle = loadBundle(configPath)
    } catch (err) {
      if (err instanceof GrafanaConfigError) throw err
      const msg = err instanceof Error ? err.message : String(err)
      throw new Error(`unexpected error loading config at ${configPath}: ${msg}`)
    }

    // Spawn mcp-grafana and complete the MCP handshake. createStdioMcpSession
    // tears the child down itself on handshake failure, so we only have to
    // clear our own reference.
    let opened: GrafanaSessionLike
    try {
      opened = await deps.connect({
        binPath,
        grafanaUrl: bundle.url,
        grafanaApiKey: bundle.apiKey,
        extraEnv: bundle.extraEnv,
      })
    } catch (err) {
      session = null
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`failed to start mcp-grafana: ${msg}`)
      throw new Error(`failed to start mcp-grafana: ${msg}`)
    }

    session = opened

    let tools: McpTool[]
    try {
      tools = await opened.listTools()
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`tools/list failed: ${msg}`)
      opened.close()
      session = null
      throw new Error(`tools/list failed: ${msg}`)
    }

    if (tools.length === 0) {
      // The gateway reports this as a failure. Drop the child so a retry
      // starts from a clean state rather than leaking a process per attempt.
      warn("tools/list returned 0 tools — nothing registered")
      opened.close()
      session = null
      return 0
    }

    return registerTools(host, tools)
  }

  const gateway = createActivationGateway({
    family: "grafana",
    label: "Grafana",
    description: ACTIVATE_GRAFANA_DESCRIPTION,
    wrapSchema: deps.wrapSchema,
    activate,
  })

  /**
   * True when both nix-injected env vars are present. The extension self-gates
   * on these two alone: absent gives zero tools — not even the gateway.
   */
  function isConfigured(ctx?: NotifyContext): boolean {
    const configPath = env.GRAFANA_MCP_CONFIG_PATH ?? ""
    const binPath = env.PI_GRAFANA_MCP_BIN ?? ""

    if (configPath === "") {
      log("GRAFANA_MCP_CONFIG_PATH not set — grafana MCP not configured, skipping init")
      return false
    }
    if (binPath === "") {
      // The nix module sets both env vars atomically. If one is missing the
      // other should be too, but be explicit about the failure mode.
      warn("PI_GRAFANA_MCP_BIN not set — cannot spawn mcp-grafana")
      ctx?.ui.notify(
        "Grafana MCP: PI_GRAFANA_MCP_BIN not set; check nx.programs.prism.pi.grafana config.",
        "warning",
      )
      return false
    }
    return true
  }

  return {
    gatewayToolName: gateway.toolName,

    /** True once the full Grafana surface has been registered. */
    isActive(): boolean {
      return gateway.isActive()
    },

    /**
     * pi `session_start`. Registers `activate_grafana` and nothing else.
     * Reads two environment variables; touches no file and spawns no process.
     */
    onSessionStart(host: GatewayHost, ctx: NotifyContext): void {
      if (!isConfigured(ctx)) return
      toolHost = host
      gateway.register(host)
    },

    /**
     * pi `before_agent_start`. Fires once per TURN, so the eager check is
     * guarded to run at most once per session.
     *
     * `role` is read from argv by the caller (`readAgentRoleFromArgv`), NOT
     * from `pi.getFlag("agent")` — registering that flag in a second extension
     * is a FATAL startup conflict. See ../mcp-activation/activation.ts.
     *
     * argv is readable in a factory prologue, so `before_agent_start` is not
     * forced by flag binding. It is still the right hook: registration from
     * here takes effect on the CURRENT turn, because `emitBeforeAgentStart` is
     * awaited before the request is built, and it keeps the eager check on the
     * same path the gateway tool uses.
     */
    async onBeforeAgentStart(
      host: GatewayHost,
      ctx: NotifyContext,
      role: string | undefined,
    ): Promise<void> {
      if (eagerChecked) return
      eagerChecked = true

      if (!isConfigured()) return
      if (!isEagerRole(role, env.GRAFANA_MCP_EAGER_ROLES)) return

      // The host is normally bound by onSessionStart; bind defensively in
      // case a future pi orders the hooks differently.
      toolHost ??= host
      gateway.register(host)

      log(`role "${role}" is in GRAFANA_MCP_EAGER_ROLES — activating eagerly`)
      const outcome = await gateway.run()
      if (outcome.status === "failed") {
        ctx.ui.notify(outcome.message, "error")
      }
    },
  }
}

export type GrafanaExtension = ReturnType<typeof createGrafanaExtension>
