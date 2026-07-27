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

export interface ToolResult {
  content: Array<{ type: string; text?: string }>
  isError?: boolean
}

export interface ToolSpec {
  name: string
  label: string
  description: string
  parameters: unknown
  execute(
    toolCallId: string,
    params: unknown,
    signal: AbortSignal | undefined,
  ): Promise<ToolResult>
}

export interface ToolHost {
  registerTool(tool: ToolSpec): void
}

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
}

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

  let session: NotionSessionLike | null = null
  let toolsRegistered = false

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
  function registerTools(host: ToolHost, tools: McpTool[]): boolean {
    if (!isEnabledForCwd()) {
      log("working directory is outside NOTION_MCP_REPOS — no tools registered")
      return false
    }

    if (tools.length === 0) {
      warn("tools/list returned 0 tools — nothing registered")
      return false
    }

    if (toolsRegistered) {
      // A /login-notion after a successful session_start would otherwise
      // re-register every tool and trip pi's duplicate-registration guard.
      log("tools already registered for this session — skipping re-registration")
      return false
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
    return true
  }

  /**
   * Return usable tokens, or null (having notified the user) when there are
   * none. Never throws — a dead or absent grant is a normal startup state.
   */
  async function ensureTokens(ctx: NotifyContext): Promise<NotionTokens | null> {
    const stored = loadTokens()
    if (!stored) {
      warn("no Notion MCP OAuth tokens found")
      ctx.ui.notify(
        "Notion MCP: no auth tokens. Use /login-notion to authenticate.",
        "warning",
      )
      return null
    }

    if (!needsRefresh(stored)) return stored

    try {
      const { tokens } = await getValidAccessToken()
      return tokens
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      if (isTerminalAuthError(err)) {
        // invalid_grant, or a lock lost mid-refresh — auth.ts has already
        // cleared the store. Do NOT retry.
        warn("Notion grant is no longer usable")
        ctx.ui.notify(msg, "warning")
        return null
      }
      warn(`token refresh failed: ${msg}`)
      ctx.ui.notify(`Notion MCP: token refresh failed — ${msg}`, "warning")
      return null
    }
  }

  /** Open the MCP session and register its tools. Never throws. */
  async function connectAndRegister(
    host: ToolHost,
    ctx: NotifyContext,
    tokens: NotionTokens,
  ): Promise<number | null> {
    let tools: McpTool[]
    try {
      session = await deps.connect(tokens.accessToken)
      tools = await session.listTools()
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      errorLog(`failed to connect to mcp.notion.com: ${msg}`)
      ctx.ui.notify(`Notion MCP unavailable: ${msg}`, "error")
      session = null
      return null
    }

    registerTools(host, tools)
    return tools.length
  }

  return {
    /**
     * pi `session_start`. Non-blocking on every path: missing tokens, a dead
     * grant, or an unreachable MCP server must degrade to "no Notion tools"
     * and never stop the session from starting.
     */
    async onSessionStart(host: ToolHost, ctx: NotifyContext): Promise<void> {
      // Checked before ensureTokens so a non-allowlisted session performs no
      // token refresh and opens no connection at all.
      if (!isEnabledForCwd()) {
        log("working directory is outside NOTION_MCP_REPOS — Notion not initialised")
        return
      }

      const tokens = await ensureTokens(ctx)
      if (!tokens) return

      await connectAndRegister(host, ctx, tokens)
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

      let tokens: NotionTokens
      try {
        tokens = await deps.login({
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

      const count = await connectAndRegister(host, ctx, tokens)
      if (count !== null) {
        ctx.ui.notify(
          `Notion MCP: authenticated and registered ${count} tools`,
          "info",
        )
      }
    },
  }
}

export type NotionExtension = ReturnType<typeof createNotionExtension>
