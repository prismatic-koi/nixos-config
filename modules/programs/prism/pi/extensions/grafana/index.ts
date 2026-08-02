// pi extension: Grafana MCP stdio bridge.
//
// This file is a thin wiring shell. It supplies the one dependency the
// extension core cannot import for itself — `typebox`'s Type.Unsafe — plus the
// real stdio MCP connector, and forwards pi's `session_start` and
// `before_agent_start` events into extension.ts, where all the behaviour (and
// all the test coverage) lives.
//
// The split exists because `typebox` only resolves inside pi's own runtime, so
// anything reachable only through this file cannot be unit tested. Keeping
// this shell free of logic keeps the untestable surface to a handful of lines.
// The notion extension already uses this shape.
//
// On session_start the extension registers exactly ONE tool,
// `activate_grafana`. It reads two environment variables and nothing else — no
// sops bundle read, no `mcp-grafana` child process. When the tool is called it
// then:
//
//   1. Reads the selected sops config bundle from GRAFANA_MCP_CONFIG_PATH.
//   2. Spawns the nixpkgs `mcp-grafana` binary (PI_GRAFANA_MCP_BIN) as a
//      per-session stdio child process with GRAFANA_URL /
//      GRAFANA_SERVICE_ACCOUNT_TOKEN in its environment.
//   3. Performs the MCP `initialize` handshake over stdio.
//   4. Enumerates `tools/list` and registers each returned tool via
//      pi.registerTool() (full surface, no scoping).
//
// A role named in GRAFANA_MCP_EAGER_ROLES skips the tool call and activates
// from the first before_agent_start instead. See extension.ts and issue #2532
// for why the role is read there and not in this factory.
//
// SECURITY: nothing here — logs, notifications, error messages — may quote
// the API key or the raw bundle contents.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { createStdioMcpSession } from "./mcp-client.ts"
import { createGrafanaExtension } from "./extension.ts"
import {
  readAgentRoleFromArgv,
  type GatewayHost,
  type GatewayToolSpec,
} from "../mcp-activation/activation.ts"

// Type.Unsafe passes the raw JSON Schema straight through to the LLM and
// arguments straight through to mcp-grafana, unvalidated on our side. Same
// posture as the atlassian/notion extensions: mcp-grafana is the validating
// authority and pi is a transport here, not a gate.
function wrapSchema(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

export default async function grafanaExtension(pi: ExtensionAPI): Promise<void> {
  const extension = createGrafanaExtension({
    wrapSchema,
    connect: (opts) => createStdioMcpSession(opts),
  })

  // pi's registerTool is generic over the TypeBox schema type; the core hands
  // back an already-wrapped schema, so the host adapter is a straight cast.
  const host: GatewayHost = {
    registerTool(tool: GatewayToolSpec) {
      pi.registerTool(tool as unknown as Parameters<ExtensionAPI["registerTool"]>[0])
    },
  }

  // NOTE: this extension MUST NOT call pi.registerFlag("agent", ...). prism
  // always loads prism.ts, which owns that flag, and pi treats the same flag
  // name owned by two different extension paths as a fatal startup conflict
  // (process.exit(1)). The role is read from argv instead — see
  // readAgentRoleFromArgv for the full reproduction and the #2068 history.

  pi.on("session_start", async (_event, ctx) => {
    // session_start (not before_agent_start): registration must happen
    // exactly once per session. before_agent_start fires per turn.
    try {
      extension.onSessionStart(host, ctx)
    } catch (err) {
      // Non-blocking: errors here must not prevent pi from starting.
      const msg = err instanceof Error ? err.message : String(err)
      console.error("[grafana-mcp] ERROR: extension init failed:", msg)
    }
  })

  pi.on("before_agent_start", async (_event, ctx) => {
    // The core guards itself so only the first turn does the eager check.
    try {
      await extension.onBeforeAgentStart(host, ctx, readAgentRoleFromArgv())
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      console.error("[grafana-mcp] ERROR: eager activation failed:", msg)
    }
  })
}
