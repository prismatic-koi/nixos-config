// pi extension: Notion MCP client bridge.
//
// On session_start (when the working directory passes the repo-scoping
// gate), this extension:
//   1. Loads OAuth tokens from disk (or prompts the user to /login-notion).
//   2. Connects to mcp.notion.com via the Streamable HTTP MCP transport.
//   3. Calls tools/list to enumerate all available Notion tools.
//   4. Registers each tool via pi.registerTool().
//
// Auth: OAuth PKCE + persistent dynamic client registration. Tokens are
// stored at $PI_CODING_AGENT_DIR/notion-mcp-oauth.json (mode 0o600). See
// auth.ts for the concurrency-safe read-refresh-write path — that
// serialisation is not optional; Notion revokes the entire grant if two
// workers refresh the same connection concurrently.
//
// Repo-scoping gate: when NOTION_MCP_REPOS is set (colon-separated list
// of directory prefixes), the extension registers no tools and opens no
// MCP connection unless the current working directory is under one of
// those prefixes. When NOTION_MCP_REPOS is unset or empty, the extension
// is unrestricted. The gate fails closed on any error resolving the
// working directory or evaluating the allowlist.
//
// See UPSTREAM.md for auth method rationale, endpoint verification, and
// the concurrency hazard.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"

import {
  createMcpSession,
  type McpSession,
  type McpTool,
} from "./mcp-client.ts"
import {
  getValidAccessToken,
  invalidateCache,
  loadTokens,
  loginNotion,
  InvalidGrantError,
  type NotionTokens,
} from "./auth.ts"
import {
  callToolWithAuthRetry,
  type AuthCallbacks,
} from "./retry.ts"
import { notionEnabledForCwd } from "./gate.ts"

let _session: McpSession | null = null

// ---------------------------------------------------------------------------
// Auth callbacks
// ---------------------------------------------------------------------------

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
// Logging helpers
//
// Debug lines are gated on NOTION_MCP_DEBUG. Tokens, refresh tokens, and
// Authorization headers MUST NOT be passed to any of these functions.
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

// The repo-scoping gate lives in gate.ts so it can be unit-tested
// without pulling in the ExtensionAPI type dependency. See gate.ts for
// the semantics.

// ---------------------------------------------------------------------------
// JSON Schema passthrough
// ---------------------------------------------------------------------------

function mcpSchemaToTypebox(inputSchema: Record<string, unknown>) {
  return Type.Unsafe<Record<string, unknown>>(inputSchema as object)
}

// ---------------------------------------------------------------------------
// Register tools
// ---------------------------------------------------------------------------

function registerTools(pi: ExtensionAPI, tools: McpTool[]): void {
  if (tools.length === 0) {
    warn("tools/list returned 0 tools — nothing registered")
    return
  }

  log(`registering ${tools.length} tools`)

  for (const tool of tools) {
    const toolName = tool.name
    const schema = mcpSchemaToTypebox(
      tool.inputSchema ?? { type: "object", properties: {} },
    )

    pi.registerTool({
      name: toolName,
      label: toolName,
      description: tool.description ?? toolName,
      parameters: schema,
      async execute(_toolCallId, params, _signal) {
        if (!_session) {
          return {
            content: [
              { type: "text", text: "Notion MCP: no active session" },
            ],
            isError: true,
          }
        }

        const session = _session
        const auth = makeAuthCallbacks(session)

        try {
          log(`calling ${toolName}`)
          const callParams = params as Record<string, unknown>
          const result = await callToolWithAuthRetry(
            session,
            auth,
            toolName,
            callParams,
          )

          if (result.isError) {
            const raw =
              result.content?.map((c) => c.text ?? "").join("\n") ??
              "Unknown MCP tool error"
            return {
              content: [{ type: "text", text: raw }],
              isError: true,
            }
          }

          // Passthrough: Notion responses can be large (page content,
          // database schema blobs) and we do not yet have a Notion-
          // specific slim strategy. The Atlassian slim.ts drop-key sets
          // are not appropriate here and would be an unaudited port.
          // A follow-up can add slim.ts for Notion once we have empirical
          // response shapes to work from.
          const joined = result.content
            ?.map((c) => c.text ?? "")
            .join("\n") ?? ""
          return {
            content: [{ type: "text", text: joined }],
          }
        } catch (err) {
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

  log(`registered ${tools.length} Notion tools`)
}

// ---------------------------------------------------------------------------
// Session initialisation
// ---------------------------------------------------------------------------

async function ensureTokens(
  _pi: ExtensionAPI,
  ctx: { ui: { notify: (msg: string, type?: string) => void } },
): Promise<NotionTokens | null> {
  const stored = loadTokens()
  if (stored) {
    try {
      const { tokens } = await getValidAccessToken()
      return tokens
    } catch (err) {
      if (err instanceof InvalidGrantError) {
        warn("Stored refresh_token was rejected as invalid_grant — re-login required")
        ctx.ui.notify(
          "Notion MCP: OAuth grant revoked (invalid_grant). Use /login-notion to re-authenticate.",
          "error",
        )
        return null
      }
      warn("Stored token refresh failed, attempting fresh login")
    }
  }

  warn("No valid Notion MCP OAuth tokens found — starting login flow")
  ctx.ui.notify(
    "Notion MCP: No auth tokens. Use /login-notion to authenticate.",
    "warning",
  )
  return null
}

async function initExtension(
  pi: ExtensionAPI,
  ctx: { ui: { notify: (msg: string, type?: string) => void } },
): Promise<void> {
  const tokens = await ensureTokens(pi, ctx)
  if (!tokens) return

  let tools: McpTool[]
  try {
    _session = await createMcpSession(tokens.accessToken)
    tools = await _session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`Failed to connect to mcp.notion.com: ${msg}`)
    ctx.ui.notify(`Notion MCP unavailable: ${msg}`, "error")
    _session = null
    return
  }

  registerTools(pi, tools)
}

// ---------------------------------------------------------------------------
// /login-notion command
// ---------------------------------------------------------------------------

function registerLoginCommand(pi: ExtensionAPI): void {
  pi.registerCommand("login-notion", {
    description: "Log in to Notion MCP (OAuth PKCE flow)",
    async handler(_args, ctx) {
      const existing = loadTokens()
      if (existing && Date.now() < existing.expiresAt) {
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
          errorLog(`Failed to reconnect after login: ${msg}`)
          ctx.ui.notify(
            `Notion MCP: login succeeded but connection failed: ${msg}`,
            "error",
          )
        }
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        errorLog(`Login failed: ${msg}`)
        ctx.ui.notify(`Notion login failed: ${msg}`, "error")
      }
    },
  })
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

export default async function notionExtension(pi: ExtensionAPI): Promise<void> {
  // Login command is always registered — the user might want to /login-notion
  // even from a repo that is not in the allowlist (e.g. to seed the token
  // store).
  registerLoginCommand(pi)

  pi.on("session_start", async (_event, ctx) => {
    // Repo-scoping gate. When it returns false — either because the cwd is
    // outside the allowlist or because evaluation errored — we register
    // no tools and open no MCP connection.
    if (!notionEnabledForCwd()) {
      log("repo-scoping gate: not registering (cwd outside allowlist)")
      return
    }

    // Non-blocking: errors here must NOT prevent pi from starting.
    try {
      await initExtension(pi, ctx)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`Extension init failed: ${msg}`)
    }
  })
}
