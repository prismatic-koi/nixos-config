// Shared deferred-registration gateway for the pi MCP extensions (issue #2532).
//
// WHY THIS EXISTS
//
// Every pi session used to register the full tool surface of every enabled MCP
// provider at `session_start`. Those schemas sit in the Anthropic `tools`
// array, which is the first segment of the cached prompt prefix, so every
// session paid for them whether or not it ever called a provider tool. On a
// grafana-enabled host that was measured at about 26400 cached tokens per
// session (issue #2531).
//
// The fix is to register ONE small tool per provider — `activate_<family>` —
// and to do the real work (read config, connect or spawn, `tools/list`,
// `registerTool` each result) only when that tool is called.
//
// MECHANISM (verified against pi 0.79.1, re-checked on 0.82.1)
//
//   - An unregistered tool costs nothing. The request `tools` array is built
//     from `agent.state.tools`, which only holds registered+active tools.
//   - `registerTool()` works after startup. It calls `runtime.refreshTools()`
//     -> `_refreshToolRegistry()`, which rebuilds the registry immediately.
//   - A tool registered mid-session AUTO-ACTIVATES: `_refreshToolRegistry`
//     pushes every name that was not in the previous registry onto the next
//     active set. There is no public API to suppress that, which is why
//     "defer registration entirely" is the correct shape and `setActiveTools`
//     is not used on this path.
//   - `emitBeforeAgentStart` is awaited BEFORE the request is built, so a
//     registration performed from a `before_agent_start` handler is visible on
//     that same turn. A registration performed from inside a tool call is
//     visible on the next turn.
//
// TWO RULES follow from the prompt-cache cost. Activation invalidates the
// Anthropic prompt cache once, because the tools array sits in front of every
// cache breakpoint:
//
//   1. Activate a whole family in one call. Never one tool at a time.
//   2. Never deactivate to tidy up. pi has no `unregisterTool`, so the absence
//      of an API enforces this.
//
// SECURITY: nothing in this module is ever handed a credential or a config
// bundle. It sees a family name, a label, a count, and an exception message
// produced by the caller. Callers are responsible for making sure the
// exception messages they let through carry no secrets — see
// grafana/extension.ts for the sanitising wrapper on the config-load path.

// ---------------------------------------------------------------------------
// Host-shaped types
//
// Structural subsets of pi's ExtensionAPI. Keeping them structural (rather
// than importing pi's types) is what lets the tests drive this module with
// plain object literals — `typebox` only resolves inside pi's own runtime.
// ---------------------------------------------------------------------------

export interface ToolResult {
  content: Array<{ type: string; text?: string }>
  isError?: boolean
}

export interface GatewayToolSpec {
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

export interface GatewayHost {
  registerTool(tool: GatewayToolSpec): void
}

// ---------------------------------------------------------------------------
// Eager roles
// ---------------------------------------------------------------------------

/**
 * Split a `<PROVIDER>_MCP_EAGER_ROLES` value into role names.
 *
 * The nix side emits a colon-separated list (the same shape as
 * NOTION_MCP_REPOS, for consistency). Commas are accepted too so a
 * hand-exported value in a shell does not silently mean "no eager roles".
 *
 * Values are injected verbatim by prism's Go isolators — there is no shell in
 * the loop (internal/container/env.go) — so trimming here is not optional.
 */
export function parseEagerRoles(raw: string | undefined): string[] {
  if (!raw) return []
  return raw
    .split(/[:,]/)
    .map((entry) => entry.trim())
    .filter(Boolean)
}

/**
 * True when this session's agent role should receive the provider family
 * eagerly, with no `activate_<family>` call.
 *
 * `role` comes from `pi.getFlag("agent")`, which binds during
 * `applyExtensionFlagValues` — after every extension factory has returned, and
 * before the first `before_agent_start`. It is therefore NOT readable in a
 * factory prologue; see the call sites, which all check from
 * `before_agent_start`.
 *
 * A session with no `--agent` flag (an interactive `pi` launched by hand) has
 * no role, so it is never eager. That is the intended default: the cheapest
 * session is the one that registers one tool.
 */
export function isEagerRole(role: string | undefined, raw: string | undefined): boolean {
  if (!role) return false
  const roles = parseEagerRoles(raw)
  return roles.includes(role)
}

// ---------------------------------------------------------------------------
// Activation gateway
// ---------------------------------------------------------------------------

export type ActivationOutcome =
  | { status: "activated"; toolCount: number; message: string }
  | { status: "already-active"; toolCount: number; message: string }
  | { status: "failed"; message: string }

export interface ActivationGatewayOptions {
  /**
   * Provider family key. Names the tool `activate_<family>`, so it must be a
   * valid tool-name fragment: lowercase letters, digits and underscores.
   */
  family: string
  /** Human-readable provider name used in messages, e.g. "Grafana". */
  label: string
  /** Tool description shown to the model. See the AC on capability naming. */
  description: string
  /** Wrap the (empty) JSON schema for the host registry — `Type.Unsafe` in pi. */
  wrapSchema: (schema: Record<string, unknown>) => unknown
  /**
   * Do the real work: read config, connect or spawn, `tools/list`, and
   * register every returned tool. Resolve with the number of tools registered.
   * Reject to report a failure — the gateway converts the rejection into an
   * error tool result and stays retryable.
   */
  activate: () => Promise<number>
}

/**
 * A provider-agnostic `activate_<family>` gateway.
 *
 * State machine, deliberately small:
 *
 *   idle ──run()──▶ activating ──ok──▶ active   (terminal; further runs are
 *     ▲                  │                       no-ops that report the count)
 *     └────fail──────────┘
 *
 * Concurrent `run()` calls share one in-flight promise, so a model that emits
 * two `activate_grafana` calls in the same turn spawns one child process, not
 * two. A failure returns to `idle` so a later retry — after the user fixes the
 * config, or after a transient network fault clears — can succeed.
 */
export function createActivationGateway(opts: ActivationGatewayOptions) {
  const toolName = `activate_${opts.family}`

  let active = false
  let toolCount = 0
  let inFlight: Promise<ActivationOutcome> | null = null
  let registered = false

  async function perform(): Promise<ActivationOutcome> {
    let count: number
    try {
      count = await opts.activate()
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      return {
        status: "failed",
        message: `${opts.label} MCP activation failed: ${msg}`,
      }
    }

    if (count <= 0) {
      // A server that answers `tools/list` with an empty array is a
      // misconfiguration, not a success. Stay retryable.
      return {
        status: "failed",
        message: `${opts.label} MCP activation failed: the server returned no tools, so nothing was registered.`,
      }
    }

    active = true
    toolCount = count
    return {
      status: "activated",
      toolCount: count,
      message: `${opts.label} MCP activated: ${count} tools are now available. They are callable from your next tool call onward.`,
    }
  }

  /**
   * Activate the family if it is not active already. Never throws.
   *
   * Used by both the `activate_<family>` tool and the eager-role path, so the
   * two cannot diverge.
   */
  async function run(): Promise<ActivationOutcome> {
    if (active) {
      return {
        status: "already-active",
        toolCount,
        message: `${opts.label} MCP is already active for this session: ${toolCount} tools are available. No tools were registered.`,
      }
    }

    if (inFlight) return inFlight

    inFlight = perform().finally(() => {
      inFlight = null
    })
    return inFlight
  }

  return {
    toolName,

    /** True once the full family has been registered in this session. */
    isActive(): boolean {
      return active
    },

    /** Number of provider tools registered so far (0 until activation). */
    count(): number {
      return toolCount
    },

    run,

    /**
     * Register the gateway tool with the host. Idempotent.
     *
     * pi's `registerTool` overwrites by name rather than rejecting a duplicate
     * (`extension.tools.set(tool.name, ...)` in dist/core/extensions/
     * loader.js), so a repeat registration does not crash — but it does call
     * `runtime.refreshTools()`, which rebuilds the tools array and so costs a
     * full prompt-cache write. A provider whose `session_start` fires twice
     * (`/reload` re-emits it with `reason: "reload"`) must not pay that.
     */
    register(host: GatewayHost): void {
      if (registered) return
      registered = true

      host.registerTool({
        name: toolName,
        label: toolName,
        description: opts.description,
        parameters: opts.wrapSchema({
          type: "object",
          properties: {},
          additionalProperties: false,
        }),
        async execute(_toolCallId, _params, _signal): Promise<ToolResult> {
          const outcome = await run()
          return {
            content: [{ type: "text", text: outcome.message }],
            ...(outcome.status === "failed" ? { isError: true } : {}),
          }
        },
      })
    },
  }
}

export type ActivationGateway = ReturnType<typeof createActivationGateway>
