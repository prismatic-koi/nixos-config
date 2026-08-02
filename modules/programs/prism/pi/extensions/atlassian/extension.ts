// pi-agnostic core of the Atlassian MCP extension.
//
// index.ts is a thin wiring shell that supplies the dependencies this module
// cannot import for itself — `typebox`'s Type.Unsafe, the real MCP connector,
// and the real auth functions — and forwards pi's `session_start`,
// `before_agent_start` and `/login-atlassian` events in here.
//
// WHY THE SPLIT. `index.ts` imports `typebox`, which only resolves inside pi's
// own runtime, so anything reachable *only* through `index.ts` cannot be unit
// tested. `notion/UPSTREAM.md` already states the rule: "Keep logic out of
// `index.ts`. Anything added there is, by construction, untestable."
//
// That rule bit here. Atlassian is the ONE provider whose `eagerRoles` default
// is non-empty (`[ "coordinator" ]`), so its eager path is the one that
// actually runs in production — and before this split it had no unit test at
// all, because every line of it sat behind the typebox import. Round-1 review
// of PR #2568 caught that. See atlassian/extension.test.ts.
//
// DEFERRED REGISTRATION (issue #2532). `activateAtlassian` performs the work
// that used to happen at `session_start`: resolve tokens, connect to
// mcp.atlassian.com, `tools/list`, and register every tool plus the synthetic
// `transitionJiraIssueByName`. Nothing here touches the network or the token
// store until then.

import {
  createActivationGateway,
  isEagerRole,
  type GatewayHost,
  type GatewayToolSpec,
  type ToolResult,
} from "../mcp-activation/activation.ts"
import type { McpTool } from "./mcp-client.ts"
import type { AtlassianTokens, LoginCallbacks } from "./auth.ts"
import {
  callToolWithAuthRetry,
  type AuthCallbacks,
  type McpCallResult,
} from "./retry.ts"
import { slimMcpResultContent } from "./slim.ts"

// ---------------------------------------------------------------------------
// Host-shaped types
//
// Structural subsets of pi's ExtensionAPI, so the tests can drive this module
// with plain object literals. Aliased to the shared gateway's shapes so a host
// built for one is accepted by the other by construction.
// ---------------------------------------------------------------------------

export interface AtlassianSessionLike {
  updateToken(token: string): void
  listTools(): Promise<McpTool[]>
  callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult>
}

export type { ToolResult }
export type ToolSpec = GatewayToolSpec
export type ToolHost = GatewayHost

export interface NotifyContext {
  ui: { notify: (msg: string, type?: string) => void }
}

export interface ExtensionDeps {
  /** Wrap a raw MCP JSON Schema for the host's tool registry (typebox in pi). */
  wrapSchema: (schema: Record<string, unknown>) => unknown
  /** Open an MCP session against Atlassian. */
  connect: (accessToken: string) => Promise<AtlassianSessionLike>
  /** Run the interactive OAuth login. */
  login: (callbacks: LoginCallbacks) => Promise<AtlassianTokens>
  /** Read the token store. */
  loadTokens: () => AtlassianTokens | null
  /** Resolve (refreshing if needed) a valid access token. */
  getValidAccessToken: () => Promise<{ token: string; tokens: AtlassianTokens }>
  /** Drop the mtime-aware token cache so a peer's rotation is observed (#2389). */
  invalidateCache: () => void
  /** Configured default cloud ID, or "" when unset. */
  getDefaultCloudId: () => string
  /** Environment source. Defaults to process.env. */
  env?: Record<string, string | undefined>
}

/** Per-instance mutable state, passed explicitly so nothing is module-global. */
interface State {
  session: AtlassianSessionLike | null
  toolHost: ToolHost | null
  eagerChecked: boolean
}

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
function makeAuthCallbacks(deps: ExtensionDeps, session: AtlassianSessionLike): AuthCallbacks {
  return {
    async refresh() {
      const { token } = await deps.getValidAccessToken()
      session.updateToken(token)
    },
    invalidate() {
      deps.invalidateCache()
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
  session: AtlassianSessionLike,
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

function registerTools(deps: ExtensionDeps, state: State, host: ToolHost, tools: McpTool[]): number {
  if (tools.length === 0) {
    warn("tools/list returned 0 tools — nothing registered")
    return 0
  }

  log(`registering ${tools.length} tools`)

  for (const tool of tools) {
    const toolName = tool.name
    const schema = deps.wrapSchema(tool.inputSchema ?? { type: "object", properties: {} })

    host.registerTool({
      name: toolName,
      label: toolName,
      description: tool.description ?? toolName,
      parameters: schema,
      async execute(_toolCallId, params, _signal) {
        // Ensure we have a live session
        if (!state.session) {
          return {
            content: [{ type: "text", text: "Atlassian MCP: no active session" }],
            isError: true,
          }
        }

        const session = state.session
        const auth = makeAuthCallbacks(deps, session)

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
          const slimmed = slimMcpResultContent(result.content, toolName, deps.getDefaultCloudId())
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
  registerTransitionByNameTool(deps, state, host)

  // +1 for the synthetic tool, so the count the activation reports matches
  // what the agent can actually call.
  return tools.length + 1
}

// ---------------------------------------------------------------------------
// Synthetic tool: transitionJiraIssueByName (Issue #2)
// ---------------------------------------------------------------------------

function registerTransitionByNameTool(deps: ExtensionDeps, state: State, host: ToolHost): void {
  const defaultCloudIdNote = deps.getDefaultCloudId()
    ? ` cloudId is optional (default configured).`
    : ""

  host.registerTool({
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
    parameters: deps.wrapSchema({
      type: "object",
      properties: {
        cloudId: {
          type: "string",
          description: deps.getDefaultCloudId()
            ? `Cloud ID of the Atlassian site. Optional — defaults to ${deps.getDefaultCloudId()}.`
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
      required: deps.getDefaultCloudId()
        ? ["issueIdOrKey", "transitionName"]
        : ["cloudId", "issueIdOrKey", "transitionName"],
    }),
    async execute(_toolCallId, params, _signal) {
      if (!state.session) {
        return { content: [{ type: "text", text: "Atlassian MCP: no active session" }], isError: true }
      }

      const session = state.session
      const auth = makeAuthCallbacks(deps, session)

      const p = params as Record<string, unknown>
      const issueIdOrKey = p["issueIdOrKey"] as string | undefined
      const transitionName = p["transitionName"] as string | undefined
      const cloudId = (p["cloudId"] as string | undefined) ?? deps.getDefaultCloudId()

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

      const slimmed = slimMcpResultContent(transitionResult.content, "transitionJiraIssue", deps.getDefaultCloudId())
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
async function resolveTokens(deps: ExtensionDeps): Promise<AtlassianTokens> {
  const stored = deps.loadTokens()
  if (stored) {
    try {
      const { tokens } = await deps.getValidAccessToken()
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
async function activateAtlassian(deps: ExtensionDeps, state: State, host: ToolHost): Promise<number> {
  const tokens = await resolveTokens(deps)

  let tools: McpTool[]
  try {
    state.session = await deps.connect(tokens.accessToken)
    tools = await state.session.listTools()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    errorLog(`Failed to connect to mcp.atlassian.com: ${msg}`)
    state.session = null
    throw new Error(`could not reach mcp.atlassian.com: ${msg}`)
  }

  return registerTools(deps, state, host, tools)
}

// ---------------------------------------------------------------------------
// Extension instance
// ---------------------------------------------------------------------------

/**
 * Build an Atlassian extension instance.
 *
 * A factory rather than module-level mutable state so each pi session (and
 * each test) gets its own session / gateway pair with no cross-contamination.
 */
export function createAtlassianExtension(deps: ExtensionDeps) {
  const env = deps.env ?? process.env
  const state: State = { session: null, toolHost: null, eagerChecked: false }

  const gateway = createActivationGateway({
    family: "atlassian",
    label: "Atlassian",
    description: ACTIVATE_ATLASSIAN_DESCRIPTION,
    wrapSchema: deps.wrapSchema,
    activate: () => {
      const host = state.toolHost
      if (!host) {
        // Unreachable in pi: the gateway tool cannot be called before
        // onSessionStart bound the host.
        throw new Error("internal error: no tool host bound")
      }
      return activateAtlassian(deps, state, host)
    },
  })

  return {
    gatewayToolName: gateway.toolName,

    /** True once the full Atlassian surface has been registered. */
    isActive(): boolean {
      return gateway.isActive()
    },

    /**
     * pi `session_start`. Registers `activate_atlassian` and nothing else: no
     * token read, no connection, no tool schemas in the prompt prefix.
     */
    onSessionStart(host: ToolHost, _ctx: NotifyContext): void {
      state.toolHost = host
      gateway.register(host)
    },

    /**
     * pi `before_agent_start`. Fires once per TURN, so the eager check is
     * guarded to run at most once per session.
     *
     * `role` is read from argv by the caller, NOT from `pi.getFlag("agent")`.
     * Registering that flag in a second extension is a FATAL startup conflict
     * — see readAgentRoleFromArgv in ../mcp-activation/activation.ts.
     */
    async onBeforeAgentStart(
      host: ToolHost,
      ctx: NotifyContext,
      role: string | undefined,
    ): Promise<void> {
      if (state.eagerChecked) return
      state.eagerChecked = true

      if (!isEagerRole(role, env.ATLASSIAN_MCP_EAGER_ROLES)) return

      state.toolHost ??= host
      gateway.register(host)

      log(`role "${role}" is in ATLASSIAN_MCP_EAGER_ROLES — activating eagerly`)
      const outcome = await gateway.run()
      if (outcome.status === "failed") {
        ctx.ui.notify(outcome.message, "error")
      }
    },

    /**
     * `/login-atlassian`. Registers through the SAME gateway the activate tool
     * uses, so a login after a successful activation reports "already active"
     * instead of re-registering every tool (which pi tolerates, but which
     * costs a full prompt-cache write).
     */
    async onLoginCommand(host: ToolHost, ctx: NotifyContext): Promise<void> {
      const existing = deps.loadTokens()
      if (existing && Date.now() < existing.expiresAt) {
        ctx.ui.notify("Atlassian MCP: already authenticated", "info")
        return
      }

      ctx.ui.notify("Atlassian MCP: starting OAuth login flow...", "info")

      try {
        await deps.login({
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

      state.toolHost ??= host
      gateway.register(host)
      const outcome = await gateway.run()
      ctx.ui.notify(
        outcome.status === "failed"
          ? `Atlassian MCP: login succeeded but ${outcome.message}`
          : `Atlassian MCP: authenticated. ${outcome.message}`,
        outcome.status === "failed" ? "error" : "info",
      )
    },
  }
}

export type AtlassianExtension = ReturnType<typeof createAtlassianExtension>
