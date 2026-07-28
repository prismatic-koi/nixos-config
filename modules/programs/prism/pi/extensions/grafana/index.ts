// pi extension: Grafana MCP stdio bridge.
//
// On session_start, this extension:
//
//   1. Reads the selected sops config bundle from GRAFANA_MCP_CONFIG_PATH.
//   2. Spawns the nixpkgs `mcp-grafana` binary (PI_GRAFANA_MCP_BIN) as a
//      per-session stdio child process with GRAFANA_URL / GRAFANA_SERVICE_ACCOUNT_TOKEN in
//      its environment.
//   3. Performs the MCP `initialize` handshake over stdio.
//   4. Enumerates `tools/list` and registers each returned tool via
//      pi.registerTool() (full surface, no scoping).
//
// Every failure path is non-blocking: pi sessions MUST NOT be prevented from
// starting when grafana is unconfigured, its bundle is malformed, or the
// mcp-grafana child dies. See UPSTREAM.md "Failure modes and graceful
// degradation" for the three degradation shapes.
//
// SECURITY: nothing here — logs, notifications, error messages — may quote
// the API key or the raw bundle contents. `errorLog` and `ctx.ui.notify`
// see only paths and exception messages.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import {
  createStdioMcpSession,
  type McpTool,
  type StdioMcpSession,
} from "./mcp-client.ts"
import { GrafanaConfigError, loadGrafanaBundle } from "./config-loader.ts"

let _session: StdioMcpSession | null = null

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
// Convert MCP JSON Schema to a TypeBox-compatible schema.
// Same posture as the atlassian/notion extensions: Type.Unsafe passes the raw
// JSON Schema straight through to the LLM and arguments straight through to
// mcp-grafana, unvalidated on our side. mcp-grafana is the validating
// authority.
// ---------------------------------------------------------------------------

function mcpSchemaToTypebox(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

// ---------------------------------------------------------------------------
// Register all tools from the MCP session into pi.
// ---------------------------------------------------------------------------

function registerTools(pi: ExtensionAPI, tools: McpTool[]): void {
  if (tools.length === 0) {
    warn("tools/list returned 0 tools — nothing registered")
    return
  }

  log(`registering ${tools.length} tools`)

  for (const tool of tools) {
    const toolName = tool.name
    const schema = mcpSchemaToTypebox(tool.inputSchema ?? { type: "object", properties: {} })

    pi.registerTool({
      name: toolName,
      label: toolName,
      description: tool.description ?? toolName,
      parameters: schema,
      async execute(_toolCallId, params, _signal) {
        const active = _session
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
}

// ---------------------------------------------------------------------------
// Extension initialisation
// ---------------------------------------------------------------------------

interface NotifyContext {
  ui: { notify: (msg: string, type?: string) => void }
}

/**
 * Full extension init: load config, spawn child, handshake, register tools.
 * Never throws — every failure path notifies and returns cleanly so pi
 * finishes session_start regardless.
 */
async function initExtension(pi: ExtensionAPI, ctx: NotifyContext): Promise<void> {
  const configPath = process.env.GRAFANA_MCP_CONFIG_PATH ?? ""
  const binPath = process.env.PI_GRAFANA_MCP_BIN ?? ""

  if (configPath === "") {
    log("GRAFANA_MCP_CONFIG_PATH not set — grafana MCP not configured, skipping init")
    return
  }
  if (binPath === "") {
    // The nix module sets both env vars atomically. If one is missing the
    // other should be too, but be explicit about the failure mode.
    warn("PI_GRAFANA_MCP_BIN not set — cannot spawn mcp-grafana")
    ctx.ui.notify(
      "Grafana MCP: PI_GRAFANA_MCP_BIN not set; check nx.programs.prism.pi.grafana config.",
      "warning",
    )
    return
  }

  // Load the bundle from the sops-decrypted file.
  let bundle
  try {
    bundle = loadGrafanaBundle(configPath)
  } catch (err) {
    if (err instanceof GrafanaConfigError) {
      warn(`config load failed: ${err.message}`)
      ctx.ui.notify(`Grafana MCP: ${err.message}`, "warning")
    } else {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`unexpected config-load error: ${msg}`)
      ctx.ui.notify(`Grafana MCP: unexpected error loading config: ${msg}`, "error")
    }
    return
  }

  // Spawn mcp-grafana and complete the MCP handshake. createStdioMcpSession
  // tears the child down itself on handshake failure, so we just have to
  // clear our own reference and notify.
  let session: StdioMcpSession
  try {
    session = await createStdioMcpSession({
      binPath,
      grafanaUrl: bundle.url,
      grafanaApiKey: bundle.apiKey,
      extraEnv: bundle.extraEnv,
    })
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`failed to start mcp-grafana: ${msg}`)
    ctx.ui.notify(`Grafana MCP unavailable: ${msg}`, "error")
    _session = null
    return
  }

  _session = session

  // tools/list — do NOT let a failure here take the pi session down.
  let tools: McpTool[]
  try {
    tools = await session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`tools/list failed: ${msg}`)
    ctx.ui.notify(`Grafana MCP: tools/list failed: ${msg}`, "error")
    session.close()
    _session = null
    return
  }

  registerTools(pi, tools)
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

export default async function grafanaExtension(pi: ExtensionAPI): Promise<void> {
  pi.on("session_start", async (_event, ctx) => {
    // session_start (not before_agent_start): registration must happen
    // exactly once per session. before_agent_start fires per turn.
    try {
      await initExtension(pi, ctx)
    } catch (err) {
      // Belt-and-braces — initExtension already catches every named failure
      // path; anything reaching here is a bug we still refuse to crash on.
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`extension init failed unexpectedly: ${msg}`)
    }
  })
}
