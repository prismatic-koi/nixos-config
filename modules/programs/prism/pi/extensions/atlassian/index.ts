// pi extension: Atlassian MCP client bridge.
//
// This file is a thin wiring shell. It supplies the dependencies the extension
// core cannot import for itself — `typebox`'s Type.Unsafe, the real Streamable
// HTTP MCP connector, and the real auth functions — and forwards pi's
// `session_start`, `before_agent_start` and `/login-atlassian` events into
// extension.ts, where all the behaviour (and all the test coverage) lives.
//
// Keep logic out of this file. `typebox` only resolves inside pi's own
// runtime, so anything reachable only from here cannot be unit tested.
//
// DEFERRED REGISTRATION (issue #2532)
//
// On session_start this extension registers exactly ONE tool,
// `activate_atlassian`. It opens no connection and reads no token file. The
// full ~31-tool surface used to sit in the prompt prefix of every session on
// this machine, whether or not the session ever touched Jira.
//
// When `activate_atlassian` is called — or, for a role named in
// ATLASSIAN_MCP_EAGER_ROLES, at the first `before_agent_start` — the core:
// 1. Loads OAuth tokens from disk (or reports that /login-atlassian is needed).
// 2. Connects to mcp.atlassian.com via the Streamable HTTP MCP transport.
// 3. Calls tools/list to enumerate all available tools.
// 4. Registers each tool via pi.registerTool() with slim field-drop applied
//    to all responses (ported from opencode/mcp-atlassian-slim-proxy.mjs).
//
// The eager-roles default is [ "coordinator" ]: the coordinator files Jira
// tickets in most sessions, so it would pay the activation cost nearly every
// time. See nx.programs.prism.pi.atlassian.eagerRoles.
//
// A failure now surfaces inside a tool call instead of at startup. That is a
// smaller blast radius, not a larger one — an unreachable mcp.atlassian.com
// returns an error tool result and the pi session keeps running.
//
// Auth: OAuth PKCE + dynamic client registration via the Atlassian MCP auth server.
// The API token path (ATLASSIAN_EMAIL + ATLASSIAN_API_TOKEN) only exposes 2
// Teamwork Graph tools, not the full Jira/Confluence CRUD surface.
//
// Cross-process cache coherence (#2389). Each tool invocation goes through
// `callToolWithAuthRetry` (retry.ts), which delegates to `getValidAccessToken`
// (auth.ts) whose mtime-aware cache picks up token rotations from sibling pi
// sessions without requiring /reload. A 401 response invalidates the cache
// and retries once; a second 401 is surfaced to the caller unchanged.
//
// See UPSTREAM.md for auth method rationale and token storage location.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { readAgentRoleFromArgv } from "../mcp-activation/activation.ts"
import { createAtlassianExtension, type ToolHost, type ToolSpec } from "./extension.ts"
import { createMcpSession, getDefaultCloudId } from "./mcp-client.ts"
import {
  loadTokens,
  getValidAccessToken,
  loginAtlassian,
  invalidateCache,
} from "./auth.ts"

// Type.Unsafe passes the raw JSON Schema through to the LLM with no TypeBox
// validation, so arguments are forwarded to Atlassian unvalidated. Accepted
// posture: the MCP server is the validating authority and pi is a transport
// here, not a gate.
function wrapSchema(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

export default async function atlassianExtension(pi: ExtensionAPI): Promise<void> {
  const extension = createAtlassianExtension({
    wrapSchema,
    connect: (accessToken) => createMcpSession(accessToken),
    login: (callbacks) => loginAtlassian(callbacks),
    loadTokens,
    getValidAccessToken,
    invalidateCache,
    getDefaultCloudId,
  })

  // pi's registerTool is generic over the TypeBox schema type; the core hands
  // back an already-wrapped schema, so the host adapter is a straight cast.
  const host: ToolHost = {
    registerTool(tool: ToolSpec) {
      pi.registerTool(tool as unknown as Parameters<ExtensionAPI["registerTool"]>[0])
    },
  }

  pi.registerCommand("login-atlassian", {
    description: "Log in to Atlassian MCP (OAuth PKCE flow)",
    async handler(_args, ctx) {
      await extension.onLoginCommand(host, ctx)
    },
  })

  // NOTE: this extension MUST NOT call pi.registerFlag("agent", ...). prism
  // always loads prism.ts, which owns that flag, and pi treats the same flag
  // name owned by two different extension paths as a fatal startup conflict
  // (process.exit(1)). The role is read from argv instead — see
  // readAgentRoleFromArgv for the full reproduction and the #2068 history.

  pi.on("session_start", async (_event, ctx) => {
    // session_start (not before_agent_start): registration must happen exactly
    // once per session, and before_agent_start fires once per TURN.
    try {
      extension.onSessionStart(host, ctx)
    } catch (err) {
      // Non-blocking: errors here must not prevent pi from starting.
      const msg = err instanceof Error ? err.message : String(err)
      console.error("[atlassian-mcp] ERROR: extension init failed:", msg)
    }
  })

  pi.on("before_agent_start", async (_event, ctx) => {
    // The core guards itself so only the first turn does the eager check.
    try {
      await extension.onBeforeAgentStart(host, ctx, readAgentRoleFromArgv())
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      console.error("[atlassian-mcp] ERROR: eager activation failed:", msg)
    }
  })
}
