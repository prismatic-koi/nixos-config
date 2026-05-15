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
import { createMcpSession, type McpSession, type McpTool, getDefaultCloudId } from "./mcp-client.ts"
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
// Helper: augment getJiraIssue response with transitions (Issue #2)
// ---------------------------------------------------------------------------

/**
 * Fetch transitions for the issue and merge them into the getJiraIssue response
 * as a top-level `transitions` array. If fetching transitions fails, the original
 * result is returned unchanged (best-effort augmentation).
 */
async function augmentGetJiraIssueWithTransitions(
  session: McpSession,
  callParams: Record<string, unknown>,
  result: { content: Array<{ type: string; text?: string }>; isError?: boolean },
): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean }> {
  const issueIdOrKey = (callParams["issueIdOrKey"] ?? callParams["issueKey"]) as string | undefined
  if (!issueIdOrKey) return result

  const transitionArgs: Record<string, unknown> = { issueIdOrKey }
  const cloudId = callParams["cloudId"] as string | undefined
  if (cloudId) transitionArgs["cloudId"] = cloudId

  let transitions: Array<{ id: string; name: string }> = []
  try {
    const transitionsResult = await session.callTool("getTransitionsForJiraIssue", transitionArgs)
    if (!transitionsResult.isError) {
      const rawText = transitionsResult.content?.map((c) => c.text ?? "").join("").trim()
      const parsed = JSON.parse(rawText) as { transitions?: Array<{ id: string; name: string }> }
      transitions = parsed?.transitions ?? []
    }
  } catch {
    // Best-effort — if fetching transitions fails, return issue without them
  }

  if (transitions.length === 0) return result

  // Merge transitions into the issue JSON response
  const parts: Array<{ type: string; text?: string }> = []
  for (const block of result.content) {
    if (block.type === "text" && typeof block.text === "string") {
      try {
        const parsed = JSON.parse(block.text) as Record<string, unknown>
        parsed["transitions"] = transitions
        parts.push({ type: "text", text: JSON.stringify(parsed) })
        continue
      } catch {
        // Not JSON — leave as-is
      }
    }
    parts.push(block)
  }

  return { content: parts, isError: result.isError }
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
          const callParams = params as Record<string, unknown>
          let result = await _session.callTool(toolName, callParams)

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

          // Issue #2: extend getJiraIssue response with transitions array.
          // We make an additional call to getTransitionsForJiraIssue and merge
          // the transitions into the issue response so the agent sees them in
          // one round-trip. Documented behaviour: transitions are fetched via
          // an extra REST call and included as a top-level "transitions" array.
          if (toolName === "getJiraIssue" && !result.isError) {
            result = await augmentGetJiraIssueWithTransitions(
              _session,
              callParams,
              result,
            )
          }

          // Apply slim field-drop before returning (pass defaultCloudId for error nudge — Issue #5)
          const slimmed = slimMcpResultContent(result.content, toolName, getDefaultCloudId())
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

  // Issue #2: register synthetic transitionJiraIssueByName tool
  registerTransitionByNameTool(pi)
}

// ---------------------------------------------------------------------------
// Synthetic tool: transitionJiraIssueByName (Issue #2)
// ---------------------------------------------------------------------------

function registerTransitionByNameTool(pi: ExtensionAPI): void {
  const defaultCloudIdNote = getDefaultCloudId()
    ? ` cloudId is optional (default configured).`
    : ""

  pi.registerTool({
    name: "transitionJiraIssueByName",
    label: "transitionJiraIssueByName",
    description:
      `Transition a Jira issue to a new status by specifying the transition name (e.g. "In Progress", "Done") ` +
      `instead of the numeric transition ID. The name is resolved case-insensitively against the issue's ` +
      `available transitions before calling transitionJiraIssue. ` +
      `If the name matches no transition, an error listing available names is returned. ` +
      `If the name matches multiple transitions, an error listing the colliding transitions is returned ` +
      `so the caller can disambiguate by ID. ` +
      `getJiraIssue's response is also augmented with a transitions array when available.` +
      defaultCloudIdNote,
    parameters: Type.Unsafe<Record<string, unknown>>({
      type: "object",
      properties: {
        cloudId: {
          type: "string",
          description: getDefaultCloudId()
            ? `Cloud ID of the Atlassian site. Optional — defaults to ${getDefaultCloudId()}.`
            : "Cloud ID of the Atlassian site (UUID from getAccessibleAtlassianResources).",
        },
        issueIdOrKey: {
          type: "string",
          description: "The Jira issue ID or key (e.g. PROJ-123).",
        },
        transitionName: {
          type: "string",
          description: "The name of the transition (e.g. \"In Progress\", \"Done\"). Case-insensitive.",
        },
      },
      required: getDefaultCloudId()
        ? ["issueIdOrKey", "transitionName"]
        : ["cloudId", "issueIdOrKey", "transitionName"],
    }),
    async execute(_toolCallId, params, _signal) {
      if (!_session) {
        return { content: [{ type: "text", text: "Atlassian MCP: no active session" }], isError: true }
      }
      if (!_tokens) {
        return { content: [{ type: "text", text: "Atlassian MCP: no auth tokens" }], isError: true }
      }

      // Refresh token
      try {
        const { token, tokens: updatedTokens } = await getValidAccessToken(_tokens)
        _tokens = updatedTokens
        _session.updateToken(token)
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        return { content: [{ type: "text", text: `Atlassian auth error: ${msg}` }], isError: true }
      }

      const p = params as Record<string, unknown>
      const issueIdOrKey = p["issueIdOrKey"] as string | undefined
      const transitionName = p["transitionName"] as string | undefined
      const cloudId = (p["cloudId"] as string | undefined) ?? getDefaultCloudId()

      if (!issueIdOrKey) {
        return { content: [{ type: "text", text: "transitionJiraIssueByName: issueIdOrKey is required" }], isError: true }
      }
      if (!transitionName) {
        return { content: [{ type: "text", text: "transitionJiraIssueByName: transitionName is required" }], isError: true }
      }

      // Step 1: look up available transitions
      log(`transitionJiraIssueByName: fetching transitions for ${issueIdOrKey}`)
      let transitionsResult: { content: Array<{ type: string; text?: string }>; isError?: boolean }
      try {
        const args: Record<string, unknown> = { issueIdOrKey }
        if (cloudId) args["cloudId"] = cloudId
        transitionsResult = await _session.callTool("getTransitionsForJiraIssue", args)
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        return { content: [{ type: "text", text: `transitionJiraIssueByName: failed to fetch transitions: ${msg}` }], isError: true }
      }

      if (transitionsResult.isError) {
        return transitionsResult
      }

      // Parse the transitions out of the result
      const rawText = transitionsResult.content?.map((c) => c.text ?? "").join("").trim()
      let transitions: Array<{ id: string; name: string }> = []
      try {
        const parsed = JSON.parse(rawText) as { transitions?: Array<{ id: string; name: string }> }
        transitions = parsed?.transitions ?? []
      } catch {
        return {
          content: [{ type: "text", text: `transitionJiraIssueByName: could not parse transitions response: ${rawText}` }],
          isError: true,
        }
      }

      if (transitions.length === 0) {
        return {
          content: [{ type: "text", text: `transitionJiraIssueByName: no transitions available for ${issueIdOrKey}` }],
          isError: true,
        }
      }

      // Step 2: resolve transition name (case-insensitive)
      const nameLower = transitionName.toLowerCase()
      const matches = transitions.filter((t) => t.name.toLowerCase() === nameLower)

      if (matches.length === 0) {
        const available = transitions.map((t) => `"${t.name}" (id: ${t.id})`).join(", ")
        return {
          content: [{
            type: "text",
            text: `transitionJiraIssueByName: no transition named "${transitionName}" found for ${issueIdOrKey}. Available transitions: ${available}`,
          }],
          isError: true,
        }
      }

      if (matches.length > 1) {
        const collisions = matches.map((t) => `"${t.name}" (id: ${t.id})`).join(", ")
        return {
          content: [{
            type: "text",
            text: `transitionJiraIssueByName: transition name "${transitionName}" is ambiguous for ${issueIdOrKey} — multiple transitions match: ${collisions}. Please call transitionJiraIssue with the specific transition ID.`,
          }],
          isError: true,
        }
      }

      const match = matches[0]
      log(`transitionJiraIssueByName: resolved "${transitionName}" to id=${match.id}`)

      // Step 3: call transitionJiraIssue with the resolved ID
      let transitionResult: { content: Array<{ type: string; text?: string }>; isError?: boolean }
      try {
        const args: Record<string, unknown> = { issueIdOrKey, transitionId: match.id }
        if (cloudId) args["cloudId"] = cloudId
        transitionResult = await _session.callTool("transitionJiraIssue", args)
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        return { content: [{ type: "text", text: `transitionJiraIssueByName: transition call failed: ${msg}` }], isError: true }
      }

      if (transitionResult.isError) {
        return transitionResult
      }

      const slimmed = slimMcpResultContent(transitionResult.content, "transitionJiraIssue", getDefaultCloudId())
      return { content: [{ type: "text", text: slimmed }] }
    },
  })
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
