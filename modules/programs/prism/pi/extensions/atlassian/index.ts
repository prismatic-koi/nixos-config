// pi extension: Atlassian MCP client bridge.
//
// On session_start, this extension:
// 1. Loads OAuth tokens from disk (or prompts the user to login).
// 2. Connects to mcp.atlassian.com via the Streamable HTTP MCP transport.
// 3. Calls tools/list to enumerate all available tools.
// 4. Registers each tool via pi.registerTool() with slim field-drop applied
//    to all responses (ported from opencode/mcp-atlassian-slim-proxy.mjs).
//
// Auth: OAuth PKCE + dynamic client registration via the Atlassian MCP auth server.
// The API token path (ATLASSIAN_EMAIL + ATLASSIAN_API_TOKEN) only exposes 2
// Teamwork Graph tools, not the full Jira/Confluence CRUD surface.
//
// See UPSTREAM.md for auth method rationale and token storage location.

import type { ExtensionAPI } from "@mariozechner/pi-coding-agent"
import { Type } from "typebox"
import { createMcpSession, type McpSession, type McpTool } from "./mcp-client.ts"
import { loadTokens, getValidAccessToken, loginAtlassian, type AtlassianTokens } from "./auth.ts"
import { slimMcpResultContent } from "./slim.ts"

let _session: McpSession | null = null
let _tokens: AtlassianTokens | null = null

function log(msg: string, ...args: unknown[]): void {
  if (process.env.ATLASSIAN_MCP_DEBUG === "1") {
    console.error("[atlassian-mcp]", msg, ...args)
  }
}

function warn(msg: string, ...args: unknown[]): void {
  console.error("[atlassian-mcp] WARN:", msg, ...args)
}

function errorLog(msg: string, ...args: unknown[]): void {
  console.error("[atlassian-mcp] ERROR:", msg, ...args)
}

// ---------------------------------------------------------------------------
// Convert MCP JSON Schema to a TypeBox-compatible schema.
// We use Type.Unsafe() which passes the raw JSON Schema through to the LLM
// without TypeBox validation — sufficient for our use case since we forward
// the args directly to the MCP server.
// ---------------------------------------------------------------------------

function mcpSchemaToTypebox(inputSchema: Record<string, unknown>) {
  // Type.Unsafe allows us to pass any JSON Schema object through TypeBox
  // without building a full TypeBox tree. The LLM receives the schema as-is.
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

// ---------------------------------------------------------------------------
// Register all tools from the MCP session into pi
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
        // Ensure we have a live session with a valid token
        if (!_session) {
          return {
            content: [{ type: "text", text: "Atlassian MCP: no active session" }],
            isError: true,
          }
        }

        if (!_tokens) {
          return {
            content: [{ type: "text", text: "Atlassian MCP: no auth tokens" }],
            isError: true,
          }
        }

        // Refresh token if needed
        try {
          const { token, tokens: updatedTokens } = await getValidAccessToken(_tokens)
          _tokens = updatedTokens
          _session.updateToken(token)
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          errorLog(`token refresh failed for ${toolName}: ${msg}`)
          return {
            content: [{ type: "text", text: `Atlassian auth error: ${msg}` }],
            isError: true,
          }
        }

        try {
          log(`calling ${toolName}`)
          const result = await _session.callTool(
            toolName,
            params as Record<string, unknown>,
          )

          if (result.isError) {
            // MCP tool-level error — surface to LLM without crashing
            const raw = result.content
              ?.map((c) => c.text ?? "")
              .join("\n") ?? "Unknown MCP tool error"
            return {
              content: [{ type: "text", text: raw }],
              isError: true,
            }
          }

          // Apply slim field-drop before returning
          const slimmed = slimMcpResultContent(result.content, toolName)
          return {
            content: [{ type: "text", text: slimmed }],
          }
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          errorLog(`tool call failed for ${toolName}: ${msg}`)
          return {
            content: [{ type: "text", text: `Atlassian MCP error: ${msg}` }],
            isError: true,
          }
        }
      },
    })
  }

  log(`registered ${tools.length} Atlassian tools`)
}

// ---------------------------------------------------------------------------
// Extension initialisation
// ---------------------------------------------------------------------------

/**
 * Ensure we have valid tokens, prompting OAuth login if needed.
 * Returns the tokens or null if auth is unavailable/declined.
 */
async function ensureTokens(pi: ExtensionAPI, ctx: { ui: { notify: (msg: string, type?: string) => void } }): Promise<AtlassianTokens | null> {
  // Try loading from disk first
  const stored = loadTokens()
  if (stored) {
    try {
      const { tokens } = await getValidAccessToken(stored)
      return tokens
    } catch {
      // Token refresh failed — will try fresh login
      warn("Stored token refresh failed, attempting fresh login")
    }
  }

  // Need fresh login
  warn("No valid Atlassian MCP OAuth tokens found — starting login flow")
  ctx.ui.notify("Atlassian MCP: No auth tokens. Use /login-atlassian to authenticate.", "warning")
  return null
}

async function initExtension(pi: ExtensionAPI, ctx: { ui: { notify: (msg: string, type?: string) => void } }): Promise<void> {
  // Get or refresh tokens
  const tokens = await ensureTokens(pi, ctx)
  if (!tokens) {
    // Non-blocking: extension fails gracefully, pi session continues
    return
  }

  _tokens = tokens

  // Connect to MCP server
  let tools: McpTool[]
  try {
    _session = await createMcpSession(tokens.accessToken)
    tools = await _session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`Failed to connect to mcp.atlassian.com: ${msg}`)
    ctx.ui.notify(`Atlassian MCP unavailable: ${msg}`, "error")
    _session = null
    return
  }

  // Register all tools
  registerTools(pi, tools)
}

// ---------------------------------------------------------------------------
// Login command
// ---------------------------------------------------------------------------

function registerLoginCommand(pi: ExtensionAPI): void {
  pi.registerCommand("login-atlassian", {
    description: "Log in to Atlassian MCP (OAuth PKCE flow)",
    async handler(_args, ctx) {
      // Check if already have valid tokens
      if (_tokens && Date.now() < _tokens.expiresAt) {
        ctx.ui.notify("Atlassian MCP: already authenticated", "info")
        return
      }

      ctx.ui.notify("Atlassian MCP: starting OAuth login flow...", "info")

      try {
        const tokens = await loginAtlassian({
          onAuthUrl(url, instructions) {
            ctx.ui.notify(`Atlassian OAuth: ${instructions}\n${url}`, "info")
          },
        })
        _tokens = tokens

        // Reconnect session with new token
        try {
          _session = await createMcpSession(tokens.accessToken)
          const tools = await _session.listTools()
          registerTools(pi, tools)
          ctx.ui.notify(`Atlassian MCP: authenticated and registered ${tools.length} tools`, "info")
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          errorLog(`Failed to reconnect after login: ${msg}`)
          ctx.ui.notify(`Atlassian MCP: login succeeded but connection failed: ${msg}`, "error")
        }
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        errorLog(`Login failed: ${msg}`)
        ctx.ui.notify(`Atlassian login failed: ${msg}`, "error")
      }
    },
  })
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

export default async function atlassianExtension(pi: ExtensionAPI): Promise<void> {
  registerLoginCommand(pi)

  pi.on("session_start", async (_event, ctx) => {
    // Non-blocking: errors here must not prevent pi from starting
    try {
      await initExtension(pi, ctx)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`Extension init failed: ${msg}`)
    }
  })
}
