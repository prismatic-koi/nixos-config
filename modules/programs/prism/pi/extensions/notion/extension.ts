// pi-agnostic core of the Notion MCP extension.
//
// index.ts is a thin wiring shell that supplies the two things this module
// cannot import for itself — `typebox`'s Type.Unsafe and the real MCP
// connector — and forwards pi's `session_start` event and `/login-notion`
// command in here.
//
// Why the split. `index.ts` imports `typebox`, which only resolves inside
// pi's own runtime, so anything reachable only through `index.ts` cannot be
// unit tested. The repo-scoping gate and the tool-registration choke point
// are exactly the logic that most needs coverage, so they live here behind a
// small injected-dependency surface — the same pattern retry.ts already uses
// for its auth callbacks. See extension.test.ts.

import {
  createActivationGateway,
  isEagerRole,
  type GatewayHost,
  type GatewayToolSpec,
  type ToolResult,
} from "../mcp-activation/activation.ts"
import {
  getValidAccessToken,
  invalidateCache,
  loadTokens,
  needsRefresh,
  type LoginCallbacks,
  type NotionTokens,
} from "./auth.ts"
import type { McpTool } from "./mcp-client.ts"
import {
  callToolWithAuthRetry,
  isTerminalAuthError,
  type AuthCallbacks,
  type McpCallResult,
} from "./retry.ts"
import { isNotionEnabledForCwd } from "./scope.ts"
import { slimMcpResultContent } from "./slim.ts"

// ---------------------------------------------------------------------------
// Host-shaped types
//
// Structural subsets of pi's ExtensionAPI / ExtensionContext. Keeping them
// structural (rather than importing pi's types) is what lets the tests drive
// this module with plain object literals.
// ---------------------------------------------------------------------------

export interface NotionSessionLike {
  updateToken(token: string): void
  listTools(): Promise<McpTool[]>
  callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult>
}

// Aliases, not copies. The shared activation gateway owns these shapes now, so
// a host built for one is accepted by the other by construction rather than by
// coincidence.
export type { ToolResult }
export type ToolSpec = GatewayToolSpec
export type ToolHost = GatewayHost

export interface NotifyContext {
  ui: { notify: (msg: string, type?: string) => void }
}

export interface ExtensionDeps {
  /** Wrap a raw MCP JSON Schema for the host's tool registry (typebox in prod). */
  wrapSchema: (schema: Record<string, unknown>) => unknown
  /** Open an MCP session against Notion. */
  connect: (accessToken: string) => Promise<NotionSessionLike>
  /** Run the interactive OAuth login. */
  login: (callbacks: LoginCallbacks) => Promise<NotionTokens>
  /** Repo-scoping gate. Defaults to the real cwd-based check. */
  isEnabledForCwd?: () => boolean
  /** Environment source. Defaults to process.env. */
  env?: Record<string, string | undefined>
  //
  // Auth seams. Injected so the session_start token-refresh path is directly
  // observable in tests — see "keeping the refresh-token clock alive" below.
  // Each defaults to the real auth.ts function, so production behaviour is
  // unchanged.
  //
  /** Token store reader. Defaults to auth.ts loadTokens. */
  loadTokens?: () => NotionTokens | null
  /** Refresh helper. Defaults to auth.ts getValidAccessToken. */
  getValidAccessToken?: () => Promise<{ token: string; tokens: NotionTokens }>
  /** Staleness predicate. Defaults to auth.ts needsRefresh. */
  needsRefresh?: (tokens: NotionTokens) => boolean
}

// ---------------------------------------------------------------------------
// The description the model sees
//
// This is the ONLY Notion text in the prompt prefix of a non-eager, in-scope
// session, so it has to name the capability areas well enough that the agent
// knows when to reach for the family — and it has to stay short.
// ---------------------------------------------------------------------------

export const ACTIVATE_NOTION_DESCRIPTION =
  "Reveal the Notion MCP tool family (about 10 tools): workspace search, page and " +
  "database read, create, update, move, and duplicate, comments, and user lookup. " +
  "Only this tool is registered until you call it; calling it registers the full " +
  "surface and returns the number of tools available. No arguments."

export const OUT_OF_SCOPE_MESSAGE =
  "Notion MCP: authenticated, but no tools were registered here — this " +
  "directory is outside NOTION_MCP_REPOS. Start pi inside an allowlisted " +
  "directory to use the Notion tools."

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

// ---------------------------------------------------------------------------
// Extension state
// ---------------------------------------------------------------------------

/**
 * Build a Notion extension instance.
 *
 * Returned as a factory rather than module-level mutable state so each pi
 * session (and each test) gets its own `session` / `toolsRegistered` pair
 * with no cross-contamination.
 */
export function createNotionExtension(deps: ExtensionDeps) {
  const isEnabledForCwd = deps.isEnabledForCwd ?? (() => isNotionEnabledForCwd())
  const env = deps.env ?? process.env
  const readTokens = deps.loadTokens ?? loadTokens
  const refreshTokens = deps.getValidAccessToken ?? getValidAccessToken
  const isStale = deps.needsRefresh ?? needsRefresh

  let session: NotionSessionLike | null = null
  let toolsRegistered = false
  let toolHost: ToolHost | null = null
  let eagerChecked = false

  // Auth callbacks for the retry shell. `refresh` pulls a valid token out of
  // auth.ts (refreshing under the cross-process lock if needed) and pushes it
  // onto the session; `invalidate` drops the in-memory cache so the next
  // refresh observes whatever a peer wrote.
  function makeAuthCallbacks(target: NotionSessionLike): AuthCallbacks {
    return {
      async refresh() {
        const { token } = await getValidAccessToken()
        target.updateToken(token)
      },
      invalidate() {
        invalidateCache()
      },
    }
  }

  /**
   * Register the MCP tool surface.
   *
   * This is the SINGLE choke point through which tools reach pi, and the
   * repo-scoping gate is re-checked here rather than only at the call sites.
   * That makes the `pi.notion.repos` contract structural: a future call site
   * cannot register the Notion surface in a non-allowlisted directory by
   * forgetting to check first. (Round-1 review caught exactly that bug in the
   * /login-notion path.)
   */
  function registerTools(host: ToolHost, tools: McpTool[]): number {
    if (!isEnabledForCwd()) {
      log("working directory is outside NOTION_MCP_REPOS — no tools registered")
      return 0
    }

    if (tools.length === 0) {
      warn("tools/list returned 0 tools — nothing registered")
      return 0
    }

    if (toolsRegistered) {
      // A /login-notion after a successful activation would otherwise
      // re-register every tool and trip pi's duplicate-registration guard.
      log("tools already registered for this session — skipping re-registration")
      return 0
    }

    log(`registering ${tools.length} tools`)

    for (const tool of tools) {
      const toolName = tool.name
      const schema = deps.wrapSchema(
        tool.inputSchema ?? { type: "object", properties: {} },
      )

      host.registerTool({
        name: toolName,
        label: toolName,
        description: tool.description ?? toolName,
        parameters: schema,
        async execute(_toolCallId, params, _signal) {
          const active = session
          if (!active) {
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

          const auth = makeAuthCallbacks(active)

          try {
            log(`calling ${toolName}`)
            const result = await callToolWithAuthRetry(
              active,
              auth,
              toolName,
              (params ?? {}) as Record<string, unknown>,
            )

            if (result.isError) {
              const raw =
                result.content?.map((c) => c.text ?? "").join("\n") ??
                "Unknown MCP tool error"
              return { content: [{ type: "text", text: raw }], isError: true }
            }

            return {
              content: [{ type: "text", text: slimMcpResultContent(result.content) }],
            }
          } catch (err) {
            // A tool failure must never kill the pi session — surface it to
            // the model as an error result instead.
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

    toolsRegistered = true
    log(`registered ${tools.length} Notion tools`)
    return tools.length
  }

  /**
   * Return usable tokens, or throw with an actionable message.
   *
   * This used to notify and return null, because it ran at session_start where
   * there was no caller to report to. It now runs inside the activation path,
   * so the failure has somewhere to go: the gateway turns the rejection into
   * an error tool result the agent can read and act on (issue #2532).
   *
   * SECURITY: `isTerminalAuthError` messages are auth.ts's own prose, not the
   * provider's response body, so no token text travels with them.
   */
  async function resolveTokens(): Promise<NotionTokens> {
    const stored = readTokens()
    if (!stored) {
      warn("no Notion MCP OAuth tokens found")
      throw new Error("no auth tokens. Run /login-notion to authenticate, then try again.")
    }

    if (!isStale(stored)) return stored

    try {
      const { tokens } = await refreshTokens()
      return tokens
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      if (isTerminalAuthError(err)) {
        // invalid_grant, or a lock lost mid-refresh — auth.ts has already
        // cleared the store. Do NOT retry.
        warn("Notion grant is no longer usable")
        throw new Error(msg)
      }
      warn(`token refresh failed: ${msg}`)
      throw new Error(`token refresh failed — ${msg}`)
    }
  }

  /**
   * Refresh the OAuth tokens if they are stale, WITHOUT registering any tools
   * and WITHOUT opening an MCP connection.
   *
   * WHY THIS EXISTS — the 30-day inactivity clock (round-3 review of #2568).
   *
   * Notion refresh tokens die after 30 consecutive days of inactivity (see
   * UPSTREAM.md "Token lifetimes"). Before #2532, `onSessionStart` called
   * `ensureTokens` on every session inside NOTION_MCP_REPOS, so ordinary
   * session starts kept the rotation alive as a side effect. Deferring the
   * whole of that behind `activate_notion` moved the clock: with
   * `notion.eagerRoles = [ ]`, a refresh would happen only when a Notion tool
   * was actually called. Thirty quiet days in the vault would kill the grant,
   * and the only recovery is `/login-notion` — an interactive browser flow a
   * headless worker CANNOT complete.
   *
   * Token refresh and tool registration are separate concerns that merely
   * shared a call site. This PR defers the SURFACE; tokens are not surface.
   * Refreshing here costs one token-file read, and at most one refresh request
   * per access-token lifetime (~8h) — no `tools/list`, no tool schemas, so the
   * cached-prefix saving is preserved in full.
   *
   * Never throws. A dead or absent grant must not stop pi from starting.
   */
  async function keepTokensAlive(ctx: NotifyContext): Promise<void> {
    const stored = readTokens()
    if (!stored) {
      // Same signal the pre-#2532 session_start gave. Actionable, and not new
      // noise: this fired on every tokenless vault session before too.
      warn("no Notion MCP OAuth tokens found")
      ctx.ui.notify(
        "Notion MCP: no auth tokens. Use /login-notion to authenticate.",
        "warning",
      )
      return
    }

    // Fresh enough: no network at all. Most session starts land here.
    if (!isStale(stored)) return

    try {
      await refreshTokens()
      log("refreshed Notion tokens at session_start (keeps the 30-day clock alive)")
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      if (isTerminalAuthError(err)) {
        // invalid_grant, or a lock lost mid-refresh — auth.ts has already
        // cleared the store. Do NOT retry; tell the user, who must re-login.
        warn("Notion grant is no longer usable")
        ctx.ui.notify(msg, "warning")
        return
      }
      // Transient (offline, 5xx, lock contention). Log and carry on —
      // activate_notion reports it as a tool result if this session actually
      // needs Notion.
      log(`token refresh at session_start did not succeed: ${msg}`)
    }
  }

  /**
   * The deferred work: resolve tokens, connect, tools/list, register.
   * Rejects on failure; the gateway converts the rejection into an error tool
   * result and stays retryable.
   *
   * NOTHING in here runs at session_start. That is the whole point of #2532.
   */
  async function activate(): Promise<number> {
    const host = toolHost
    if (!host) {
      // Unreachable in pi: the gateway tool cannot be called before
      // onSessionStart registered it, and onSessionStart binds the host first.
      throw new Error("internal error: no tool host bound")
    }

    // Fail fast outside the allowlist so no token refresh and no connection
    // happen. registerTools re-checks the same gate as the structural choke
    // point; this check is the cheap one, not the authoritative one.
    if (!isEnabledForCwd()) throw new Error(OUT_OF_SCOPE_MESSAGE)

    const tokens = await resolveTokens()

    let tools: McpTool[]
    try {
      session = await deps.connect(tokens.accessToken)
      tools = await session.listTools()
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`failed to connect to mcp.notion.com: ${msg}`)
      session = null
      throw new Error(`could not reach mcp.notion.com: ${msg}`)
    }

    return registerTools(host, tools)
  }

  const gateway = createActivationGateway({
    family: "notion",
    label: "Notion",
    description: ACTIVATE_NOTION_DESCRIPTION,
    wrapSchema: deps.wrapSchema,
    activate,
  })

  return {
    gatewayToolName: gateway.toolName,

    /** True once the full Notion surface has been registered. */
    isActive(): boolean {
      return gateway.isActive()
    },

    /**
     * pi `session_start`. Registers `activate_notion` and nothing else — no
     * token read, no connection, no tool schemas in the prompt prefix.
     *
     * The repo-scoping gate still runs first, so a session outside
     * NOTION_MCP_REPOS registers neither the family NOR the activate tool.
     */
    async onSessionStart(host: ToolHost, ctx: NotifyContext): Promise<void> {
      // The cwd gate runs FIRST and covers the refresh too: a session outside
      // the vault scope reads no token file, performs no refresh, opens no
      // connection, and registers nothing at all. AC 11 is unchanged.
      if (!isEnabledForCwd()) {
        log("working directory is outside NOTION_MCP_REPOS — Notion not initialised")
        return
      }

      toolHost = host
      gateway.register(host)

      // Keep the refresh-token rotation alive without registering the surface.
      // See keepTokensAlive for why this is separate from activation.
      await keepTokensAlive(ctx)
    },

    /**
     * pi `before_agent_start`. Fires once per TURN, so the eager check is
     * guarded to run at most once per session.
     *
     * `role` is read from argv by the caller (`readAgentRoleFromArgv`), NOT
     * from `pi.getFlag("agent")` — registering that flag in a second extension
     * is a FATAL startup conflict. See ../mcp-activation/activation.ts.
     *
     * argv is readable in a factory prologue, so this hook is not forced by
     * flag binding. It is still the right one: registration from here takes
     * effect on the CURRENT turn, and it keeps the eager check on the same
     * path the gateway tool uses.
     */
    async onBeforeAgentStart(
      host: ToolHost,
      ctx: NotifyContext,
      role: string | undefined,
    ): Promise<void> {
      if (eagerChecked) return
      eagerChecked = true

      if (!isEnabledForCwd()) return
      if (!isEagerRole(role, env.NOTION_MCP_EAGER_ROLES)) return

      toolHost ??= host
      gateway.register(host)

      log(`role "${role}" is in NOTION_MCP_EAGER_ROLES — activating eagerly`)
      const outcome = await gateway.run()
      if (outcome.status === "failed") {
        ctx.ui.notify(outcome.message, "error")
      }
    },

    /**
     * `/login-notion`. The command itself is available everywhere — logging in
     * is repo-agnostic and a user who has just edited the allowlist should not
     * have to hunt for an allowlisted directory to authenticate from — but the
     * tool surface and the MCP connection stay gated.
     */
    async onLoginCommand(host: ToolHost, ctx: NotifyContext): Promise<void> {
      const existing = loadTokens()
      if (existing && !needsRefresh(existing)) {
        ctx.ui.notify("Notion MCP: already authenticated", "info")
        return
      }

      ctx.ui.notify("Notion MCP: starting OAuth login flow...", "info")

      try {
        await deps.login({
          onAuthUrl(url, instructions) {
            ctx.ui.notify(`Notion OAuth: ${instructions}\n${url}`, "info")
          },
        })
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        errorLog(`login failed: ${msg}`)
        ctx.ui.notify(`Notion login failed: ${msg}`, "error")
        return
      }

      // The credential is now stored, but this directory may not be entitled
      // to the tool surface. Registering it here would hand a full workspace
      // read/write grant — including notion-update-page, notion-move-pages and
      // notion-duplicate-page — to a session the allowlist excludes.
      if (!isEnabledForCwd()) {
        log("login succeeded but the working directory is outside NOTION_MCP_REPOS")
        ctx.ui.notify(OUT_OF_SCOPE_MESSAGE, "info")
        return
      }

      // Connect and register through the SAME gateway the activate tool uses,
      // so a login after a successful activation reports "already active"
      // instead of re-registering every tool. The credential is on disk now,
      // so the gateway's own token lookup picks it up.
      toolHost ??= host
      gateway.register(host)
      const outcome = await gateway.run()
      ctx.ui.notify(
        outcome.status === "failed"
          ? `Notion MCP: login succeeded but ${outcome.message}`
          : `Notion MCP: authenticated. ${outcome.message}`,
        outcome.status === "failed" ? "error" : "info",
      )
    },
  }
}

export type NotionExtension = ReturnType<typeof createNotionExtension>
