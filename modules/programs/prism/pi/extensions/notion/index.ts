// pi extension: Notion MCP client bridge.
//
// On session_start, this extension:
// 1. Checks the repo-scoping gate (scope.ts). If this session's working
//    directory is outside the configured allowlist it registers nothing and
//    opens no connection.
// 2. Loads OAuth tokens from disk (or notifies the user to run /login-notion).
// 3. Connects to https://mcp.notion.com/mcp via the Streamable HTTP MCP
//    transport.
// 4. Calls tools/list and registers each returned tool via pi.registerTool().
//
// Auth: OAuth 2.0 Authorization Code + PKCE (S256) with persisted dynamic
// client registration. See auth.ts — it is NOT a copy of the Atlassian one,
// because Notion revokes the entire grant if a rotated refresh token is
// replayed.
//
// Every failure path here is non-blocking: missing tokens, a dead grant, or
// an unreachable MCP server must degrade to "no Notion tools" and never stop
// the pi session from starting.
//
// See UPSTREAM.md for endpoint provenance and the auth-method rationale.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { createMcpSession, type McpSession, type McpTool } from "./mcp-client.ts"
import {
  getValidAccessToken,
  invalidateCache,
  loadTokens,
  loginNotion,
  needsRefresh,
  type NotionTokens,
} from "./auth.ts"
import {
  callToolWithAuthRetry,
  isTerminalAuthError,
  type AuthCallbacks,
} from "./retry.ts"
import { isNotionEnabledForCwd } from "./scope.ts"
import { slimMcpResultContent } from "./slim.ts"

let _session: McpSession | null = null
let _toolsRegistered = false

// ---------------------------------------------------------------------------
// Logging
//
// SECURITY: nothing here is ever handed a token. `log` is additionally gated
// on NOTION_MCP_DEBUG so a normal session stays quiet.
// ---------------------------------------------------------------------------

function log(msg: string, ...args: unknown[]): void {
  if (process.env.NOTION_MCP_DEBUG === "1") {
    console.error("[notion-mcp]", msg, ...args)
  }
}

function warn(msg: string, ...args: unknown[]): void {
  console.error("[notion-mcp] WARN:", msg, ...args)
}

function errorLog(msg: string, ...args: unknown[]): void {
  console.error("[notion-mcp] ERROR:", msg, ...args)
}

interface NotifyContext {
  ui: { notify: (msg: string, type?: string) => void }
}

// Auth callbacks handed to the retry shell. `refresh` pulls a valid token out
// of auth.ts (refreshing under the cross-process lock if needed) and pushes it
// onto the session; `invalidate` drops the in-memory cache so the next refresh
// observes whatever a peer wrote.
function makeAuthCallbacks(session: McpSession): AuthCallbacks {
  return {
    async refresh() {
      const { token } = await getValidAccessToken()
      session.updateToken(token)
    },
    invalidate() {
      invalidateCache()
    },
  }
}

// ---------------------------------------------------------------------------
// Convert an MCP JSON Schema to a TypeBox-compatible schema.
//
// Type.Unsafe() passes the raw JSON Schema straight through to the model with
// no TypeBox validation. Arguments are therefore forwarded to Notion
// unvalidated — the same accepted posture as the Atlassian extension. The MCP
// server is the validating authority; pi is a transport here, not a gate.
// ---------------------------------------------------------------------------

function mcpSchemaToTypebox(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

// ---------------------------------------------------------------------------
// Tool registration
// ---------------------------------------------------------------------------

function registerTools(pi: ExtensionAPI, tools: McpTool[]): void {
  if (tools.length === 0) {
    warn("tools/list returned 0 tools — nothing registered")
    return
  }

  if (_toolsRegistered) {
    // /login-notion after a successful session_start would otherwise
    // re-register every tool and trip pi's duplicate-registration guard.
    log("tools already registered for this session — skipping re-registration")
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
        if (!_session) {
          return {
            content: [
              {
                type: "text",
                text: "Notion MCP: no active session — run /login-notion to authenticate.",
              },
            ],
            isError: true,
          }
        }

        const session = _session
        const auth = makeAuthCallbacks(session)

        try {
          log(`calling ${toolName}`)
          const result = await callToolWithAuthRetry(
            session,
            auth,
            toolName,
            params as Record<string, unknown>,
          )

          if (result.isError) {
            const raw =
              result.content?.map((c) => c.text ?? "").join("\n") ?? "Unknown MCP tool error"
            return { content: [{ type: "text", text: raw }], isError: true }
          }

          return {
            content: [{ type: "text", text: slimMcpResultContent(result.content) }],
          }
        } catch (err) {
          // A tool failure must never kill the pi session — surface it to the
          // model as an error result instead.
          const msg = err instanceof Error ? err.message : String(err)
          errorLog(`tool call failed for ${toolName}: ${msg}`)
          return {
            content: [{ type: "text", text: `Notion MCP error: ${msg}` }],
            isError: true,
          }
        }
      },
    })
  }

  _toolsRegistered = true
  log(`registered ${tools.length} Notion tools`)
}

// ---------------------------------------------------------------------------
// Extension initialisation
// ---------------------------------------------------------------------------

/**
 * Return usable tokens, or null (having notified the user) when there are
 * none. Never throws — a dead or absent grant is a normal startup state.
 */
async function ensureTokens(ctx: NotifyContext): Promise<NotionTokens | null> {
  const stored = loadTokens()
  if (!stored) {
    warn("no Notion MCP OAuth tokens found")
    ctx.ui.notify("Notion MCP: no auth tokens. Use /login-notion to authenticate.", "warning")
    return null
  }

  if (!needsRefresh(stored)) return stored

  try {
    const { tokens } = await getValidAccessToken()
    return tokens
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    if (isTerminalAuthError(err)) {
      // invalid_grant — auth.ts has already cleared the store. Do NOT retry.
      warn("Notion grant is no longer valid")
      ctx.ui.notify(msg, "warning")
      return null
    }
    warn(`token refresh failed: ${msg}`)
    ctx.ui.notify(`Notion MCP: token refresh failed — ${msg}`, "warning")
    return null
  }
}

async function initExtension(pi: ExtensionAPI, ctx: NotifyContext): Promise<void> {
  const tokens = await ensureTokens(ctx)
  if (!tokens) return // Non-blocking: pi carries on without Notion tools.

  let tools: McpTool[]
  try {
    _session = await createMcpSession(tokens.accessToken)
    tools = await _session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`failed to connect to mcp.notion.com: ${msg}`)
    ctx.ui.notify(`Notion MCP unavailable: ${msg}`, "error")
    _session = null
    return
  }

  registerTools(pi, tools)
}

// ---------------------------------------------------------------------------
// Login command
// ---------------------------------------------------------------------------

function registerLoginCommand(pi: ExtensionAPI): void {
  // Registered unconditionally, including in non-allowlisted repos: logging in
  // is repo-agnostic, and a user who has just added a repo to the allowlist
  // should not have to find an allowlisted directory to authenticate from.
  pi.registerCommand("login-notion", {
    description: "Log in to Notion MCP (OAuth 2.0 authorization code + PKCE)",
    async handler(_args, ctx) {
      const existing = loadTokens()
      if (existing && !needsRefresh(existing)) {
        ctx.ui.notify("Notion MCP: already authenticated", "info")
        return
      }

      ctx.ui.notify("Notion MCP: starting OAuth login flow...", "info")

      try {
        const tokens = await loginNotion({
          onAuthUrl(url, instructions) {
            ctx.ui.notify(`Notion OAuth: ${instructions}\n${url}`, "info")
          },
        })

        try {
          _session = await createMcpSession(tokens.accessToken)
          const tools = await _session.listTools()
          registerTools(pi, tools)
          ctx.ui.notify(
            `Notion MCP: authenticated and registered ${tools.length} tools`,
            "info",
          )
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          errorLog(`failed to connect after login: ${msg}`)
          ctx.ui.notify(`Notion MCP: login succeeded but connection failed: ${msg}`, "error")
        }
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        errorLog(`login failed: ${msg}`)
        ctx.ui.notify(`Notion login failed: ${msg}`, "error")
      }
    },
  })
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

export default async function notionExtension(pi: ExtensionAPI): Promise<void> {
  registerLoginCommand(pi)

  pi.on("session_start", async (_event, ctx) => {
    // session_start (not before_agent_start): registration must happen exactly
    // once per session, and before_agent_start fires once per TURN.
    try {
      if (!isNotionEnabledForCwd()) {
        log("working directory is outside NOTION_MCP_REPOS — no tools registered")
        return
      }
      await initExtension(pi, ctx)
    } catch (err) {
      // Non-blocking: errors here must not prevent pi from starting.
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`extension init failed: ${msg}`)
    }
  })
}
