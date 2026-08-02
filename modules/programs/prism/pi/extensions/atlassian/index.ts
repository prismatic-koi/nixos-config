// pi extension: Atlassian MCP client bridge.
//
// DEFERRED REGISTRATION (issue #2532)
//
// On session_start this extension registers exactly ONE tool,
// `activate_atlassian`. It opens no connection and reads no token file. The
// full ~31-tool surface used to sit in the prompt prefix of every session on
// this machine, whether or not the session ever touched Jira.
//
// When `activate_atlassian` is called — or, for a role named in
// ATLASSIAN_MCP_EAGER_ROLES, at the first `before_agent_start` — it:
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
import {
  createActivationGateway,
  isEagerRole,
  type GatewayHost,
  type GatewayToolSpec,
} from "../mcp-activation/activation.ts"
import { createMcpSession, type McpSession, type McpTool, getDefaultCloudId } from "./mcp-client.ts"
import {
  loadTokens,
  getValidAccessToken,
  loginAtlassian,
  invalidateCache,
  type AtlassianTokens,
} from "./auth.ts"
import {
  callToolWithAuthRetry,
  type AuthCallbacks,
  type McpCallResult,
} from "./retry.ts"
import { slimMcpResultContent } from "./slim.ts"

let _session: McpSession | null = null

// ---------------------------------------------------------------------------
// The description the model sees
//
// This is the ONLY Atlassian text in the prompt prefix of a non-eager session,
// so it has to name the capability areas well enough that the agent knows when
// to reach for the family — and it has to stay short, because every session
// pays for it.
// ---------------------------------------------------------------------------

export const ACTIVATE_ATLASSIAN_DESCRIPTION =
  "Reveal the Atlassian MCP tool family (about 31 tools): Jira issue search (JQL), " +
  "read, create, update, comment, and workflow transitions; Jira project and field " +
  "metadata; Confluence page and space search, read, create, and update. Only this " +
  "tool is registered until you call it; calling it registers the full surface and " +
  "returns the number of tools available. No arguments."

// Auth callbacks passed to the retry shell. `refresh` pulls a valid token
// out of auth.ts's mtime-aware cache (or refreshes it) and pushes it onto
// the session. `invalidate` drops the cache so the next refresh sees whatever
// a peer process has written (#2389).
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
 *
 * The MCP call underneath uses the same auth-retry shell as the primary tool
 * call, so a mid-session token rotation doesn't strand this call with a stale
 * bearer (#2389).
 */
async function augmentGetJiraIssueWithTransitions(
  session: McpSession,
  auth: AuthCallbacks,
  callParams: Record<string, unknown>,
  result: McpCallResult,
): Promise<McpCallResult> {
  const issueIdOrKey = (callParams["issueIdOrKey"] ?? callParams["issueKey"]) as string | undefined
  if (!issueIdOrKey) return result

  const transitionArgs: Record<string, unknown> = { issueIdOrKey }
  const cloudId = callParams["cloudId"] as string | undefined
  if (cloudId) transitionArgs["cloudId"] = cloudId

  let transitions: Array<{ id: string; name: string }> = []
  try {
    const transitionsResult = await callToolWithAuthRetry(
      session,
      auth,
      "getTransitionsForJiraIssue",
      transitionArgs,
    )
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

function registerTools(pi: ExtensionAPI, tools: McpTool[]): number {
  if (tools.length === 0) {
    warn("tools/list returned 0 tools — nothing registered")
    return 0
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
        // Ensure we have a live session
        if (!_session) {
          return {
            content: [{ type: "text", text: "Atlassian MCP: no active session" }],
            isError: true,
          }
        }

        const session = _session
        const auth = makeAuthCallbacks(session)

        try {
          log(`calling ${toolName}`)
          const callParams = params as Record<string, unknown>
          let result = await callToolWithAuthRetry(session, auth, toolName, callParams)

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
              session,
              auth,
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

  // +1 for the synthetic tool, so the count the activation reports matches
  // what the agent can actually call.
  return tools.length + 1
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

      const session = _session
      const auth = makeAuthCallbacks(session)

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

      // Step 1: look up available transitions (auth-retry wrapped — #2389)
      log(`transitionJiraIssueByName: fetching transitions for ${issueIdOrKey}`)
      let transitionsResult: McpCallResult
      try {
        const args: Record<string, unknown> = { issueIdOrKey }
        if (cloudId) args["cloudId"] = cloudId
        transitionsResult = await callToolWithAuthRetry(session, auth, "getTransitionsForJiraIssue", args)
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

      // Step 3: call transitionJiraIssue with the resolved ID (auth-retry wrapped — #2389)
      let transitionResult: McpCallResult
      try {
        const args: Record<string, unknown> = { issueIdOrKey, transitionId: match.id }
        if (cloudId) args["cloudId"] = cloudId
        transitionResult = await callToolWithAuthRetry(session, auth, "transitionJiraIssue", args)
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
 * Return usable tokens, or throw with an actionable message.
 *
 * This used to notify and return null, because it ran at session_start where
 * there was no caller to report to. It now runs inside the activation path, so
 * the failure has somewhere to go: the gateway turns the rejection into an
 * error tool result the agent can read and act on (issue #2532).
 */
async function resolveTokens(): Promise<AtlassianTokens> {
  const stored = loadTokens()
  if (stored) {
    try {
      const { tokens } = await getValidAccessToken()
      return tokens
    } catch {
      // Token refresh failed — a fresh login is the only way forward.
      // SECURITY: the underlying error can quote a token in a request body,
      // so it is deliberately NOT forwarded.
      warn("Stored token refresh failed")
    }
  }

  warn("No valid Atlassian MCP OAuth tokens found")
  throw new Error("no valid OAuth tokens. Run /login-atlassian to authenticate, then try again.")
}

/**
 * The deferred work: resolve tokens, connect, tools/list, register.
 * Rejects on failure; the gateway converts the rejection into an error tool
 * result and stays retryable.
 *
 * NOTHING in here runs at session_start. That is the whole point of #2532.
 */
async function activateAtlassian(pi: ExtensionAPI): Promise<number> {
  const tokens = await resolveTokens()

  let tools: McpTool[]
  try {
    _session = await createMcpSession(tokens.accessToken)
    tools = await _session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`Failed to connect to mcp.atlassian.com: ${msg}`)
    _session = null
    throw new Error(`could not reach mcp.atlassian.com: ${msg}`)
  }

  return registerTools(pi, tools)
}

// ---------------------------------------------------------------------------
// Login command
// ---------------------------------------------------------------------------

function registerLoginCommand(pi: ExtensionAPI, gateway: AtlassianGateway): void {
  pi.registerCommand("login-atlassian", {
    description: "Log in to Atlassian MCP (OAuth PKCE flow)",
    async handler(_args, ctx) {
      // Check if already have valid tokens (respecting mtime cache invalidation)
      const existing = loadTokens()
      if (existing && Date.now() < existing.expiresAt) {
        ctx.ui.notify("Atlassian MCP: already authenticated", "info")
        return
      }

      ctx.ui.notify("Atlassian MCP: starting OAuth login flow...", "info")

      try {
        await loginAtlassian({
          onAuthUrl(url, instructions) {
            ctx.ui.notify(`Atlassian OAuth: ${instructions}\n${url}`, "info")
          },
        })
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        errorLog(`Login failed: ${msg}`)
        ctx.ui.notify(`Atlassian login failed: ${msg}`, "error")
        return
      }

      // Connect and register through the SAME gateway the activate tool uses,
      // so a login after a successful activation reports "already active"
      // instead of re-registering every tool (which pi tolerates, but which
      // costs a full prompt-cache write). The credential is on disk now, so
      // the gateway's own token lookup picks it up.
      const outcome = await gateway.run()
      ctx.ui.notify(
        outcome.status === "failed"
          ? `Atlassian MCP: login succeeded but ${outcome.message}`
          : `Atlassian MCP: authenticated. ${outcome.message}`,
        outcome.status === "failed" ? "error" : "info",
      )
    },
  })
}

// ---------------------------------------------------------------------------
// Extension entry point
// ---------------------------------------------------------------------------

type AtlassianGateway = ReturnType<typeof createActivationGateway>

export default async function atlassianExtension(pi: ExtensionAPI): Promise<void> {
  // pi's registerTool is generic over the TypeBox schema type; the gateway
  // hands back an already-wrapped schema, so the host adapter is a cast.
  const host: GatewayHost = {
    registerTool(tool: GatewayToolSpec) {
      pi.registerTool(tool as unknown as Parameters<ExtensionAPI["registerTool"]>[0])
    },
  }

  const gateway = createActivationGateway({
    family: "atlassian",
    label: "Atlassian",
    description: ACTIVATE_ATLASSIAN_DESCRIPTION,
    wrapSchema: (schema) => mcpSchemaToTypebox(schema),
    activate: () => activateAtlassian(pi),
  })

  registerLoginCommand(pi, gateway)

  // Register --agent so pi.getFlag("agent") resolves for THIS extension. pi
  // scopes getFlag to the registering extension (dist/core/extensions/
  // loader.js: `if (!extension.flags.has(name)) return undefined`), so the
  // prism extension's own registration is not visible here. Duplicate
  // registration across extensions is safe: applyExtensionFlagValues builds a
  // flat name->flag map (dist/core/agent-session-services.js) and both
  // registrations declare the same string type, so the flag resolves
  // identically whichever extension is consulted.
  pi.registerFlag("agent", {
    description:
      "Primary agent identity (worker, coordinator, review-*, etc.) — selects the role system prompt appended to pi's base prompt at before_agent_start.",
    type: "string",
  })

  pi.on("session_start", async (_event, _ctx) => {
    // Registers activate_atlassian and nothing else: no token read, no
    // connection. Non-blocking — errors here must not prevent pi from
    // starting.
    try {
      gateway.register(host)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`Extension init failed: ${msg}`)
    }
  })

  // The earliest hook at which pi.getFlag("agent") is bound: pi binds
  // extension flags during applyExtensionFlagValues, after every extension
  // factory has returned. before_agent_start fires once per TURN, so the
  // eager check is guarded to run at most once per session.
  let eagerChecked = false
  pi.on("before_agent_start", async (_event, ctx) => {
    if (eagerChecked) return
    eagerChecked = true
    try {
      const flag = pi.getFlag("agent")
      const role = typeof flag === "string" ? flag : undefined
      if (!isEagerRole(role, process.env.ATLASSIAN_MCP_EAGER_ROLES)) return

      log(`role "${role}" is in ATLASSIAN_MCP_EAGER_ROLES — activating eagerly`)
      const outcome = await gateway.run()
      if (outcome.status === "failed") {
        ctx.ui.notify(outcome.message, "error")
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`Eager activation failed: ${msg}`)
    }
  })
}
