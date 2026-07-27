// pi extension: Notion MCP client bridge.
//
// This file is a thin wiring shell. It supplies the two dependencies the
// extension core cannot import for itself — `typebox`'s Type.Unsafe and the
// real MCP connector — and forwards pi's `session_start` event and the
// `/login-notion` command into extension.ts, where all the behaviour (and all
// the test coverage) lives.
//
// The split exists because `typebox` only resolves inside pi's own runtime, so
// anything reachable only through this file cannot be unit tested. Keeping
// this shell free of logic keeps the untestable surface to a handful of lines.
//
// On session_start the extension:
// 1. Checks the repo-scoping gate (scope.ts). Outside the allowlist it
//    registers nothing and opens no connection.
// 2. Loads OAuth tokens (or notifies the user to run /login-notion).
// 3. Connects to https://mcp.notion.com/mcp via Streamable HTTP.
// 4. Calls tools/list and registers each tool.
//
// Auth: OAuth 2.0 authorization code + PKCE (S256) with persisted dynamic
// client registration. See auth.ts — it is NOT a copy of the Atlassian one,
// because Notion revokes the entire grant if a rotated refresh token is
// replayed.
//
// See UPSTREAM.md for endpoint provenance and the auth-method rationale.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { loginNotion } from "./auth.ts"
import { createNotionExtension, type ToolHost, type ToolSpec } from "./extension.ts"
import { createMcpSession } from "./mcp-client.ts"

// Type.Unsafe passes the raw JSON Schema through to the model with no TypeBox
// validation, so arguments are forwarded to Notion unvalidated. Same accepted
// posture as the Atlassian extension: the MCP server is the validating
// authority and pi is a transport here, not a gate.
function wrapSchema(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

export default async function notionExtension(pi: ExtensionAPI): Promise<void> {
  const extension = createNotionExtension({
    wrapSchema,
    connect: (accessToken) => createMcpSession(accessToken),
    login: (callbacks) => loginNotion(callbacks),
  })

  // pi's registerTool is generic over the TypeBox schema type; the core hands
  // back an already-wrapped schema, so the host adapter is a straight cast.
  const host: ToolHost = {
    registerTool(tool: ToolSpec) {
      pi.registerTool(tool as unknown as Parameters<ExtensionAPI["registerTool"]>[0])
    },
  }

  pi.registerCommand("login-notion", {
    description: "Log in to Notion MCP (OAuth 2.0 authorization code + PKCE)",
    async handler(_args, ctx) {
      await extension.onLoginCommand(host, ctx)
    },
  })

  pi.on("session_start", async (_event, ctx) => {
    // session_start (not before_agent_start): registration must happen exactly
    // once per session, and before_agent_start fires once per TURN.
    try {
      await extension.onSessionStart(host, ctx)
    } catch (err) {
      // Non-blocking: errors here must not prevent pi from starting.
      const msg = err instanceof Error ? err.message : String(err)
      console.error("[notion-mcp] ERROR: extension init failed:", msg)
    }
  })
}
