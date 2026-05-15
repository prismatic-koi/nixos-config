// Streamable HTTP MCP client for mcp.atlassian.com.
//
// This is a hand-rolled MCP client using Node.js built-ins (no @modelcontextprotocol/sdk).
// The MCP SDK is not bundled in pi's dependency tree so we cannot import it.
//
// Protocol: MCP streamable HTTP (https://spec.modelcontextprotocol.io/specification/2024-11-05/basic/transports/#streamable-http)
// - POST to the MCP endpoint with Content-Type: application/json
// - Accept: application/json, text/event-stream
// - Server responds with either JSON (single response) or SSE (streaming)
// - Session ID is returned in Mcp-Session-Id header and must be sent on subsequent requests

const MCP_URL = process.env.ATLASSIAN_MCP_URL ?? "https://mcp.atlassian.com/v1/mcp"
const DEBUG = process.env.ATLASSIAN_MCP_DEBUG === "1"

// ---------------------------------------------------------------------------
// Default cloudId (Issue #3)
// ---------------------------------------------------------------------------

// Set via ATLASSIAN_DEFAULT_CLOUD_ID (injected from Nix option nx.programs.prism.atlassian.defaultCloudId).
// When set, every tool call that omits cloudId has the default injected automatically.
export function getDefaultCloudId(): string | undefined {
  return process.env.ATLASSIAN_DEFAULT_CLOUD_ID || undefined
}

// Rewrite the description for a tool when a default cloudId is configured:
// - mark cloudId as optional and document the default.
function rewriteDescription(tool: McpTool, defaultCloudId: string): McpTool {
  const desc = tool.description ?? tool.name
  const paramCloudIdNote =
    `cloudId is optional when ATLASSIAN_DEFAULT_CLOUD_ID is configured; ` +
    `the default (${defaultCloudId}) is used if omitted.`

  // Only append the note if cloudId appears in the tool's schema
  const schema = tool.inputSchema as Record<string, unknown>
  const props = schema?.properties as Record<string, unknown> | undefined
  if (!props || !("cloudId" in props)) {
    return tool
  }

  return {
    ...tool,
    description: `${desc} [${paramCloudIdNote}]`,
    inputSchema: {
      ...schema,
      required: Array.isArray(schema.required)
        ? (schema.required as string[]).filter((k) => k !== "cloudId")
        : schema.required,
    },
  }
}

// Improve the description for getCloudId (Issue #6)
function rewriteGetCloudIdDescription(tool: McpTool): McpTool {
  const addition =
    " If you only have an issue key (e.g. PROJ-123) and don't know the site, call this tool first to discover the cloudId."
  return {
    ...tool,
    description: (tool.description ?? tool.name) + addition,
  }
}

function debug(...args: unknown[]): void {
  if (DEBUG) {
    console.error("[atlassian-mcp]", ...args)
  }
}

// ---------------------------------------------------------------------------
// SSE parser
// ---------------------------------------------------------------------------

/**
 * Parse a text/event-stream body and extract the data field from each event.
 * Returns all data payloads as strings.
 */
function parseSseBody(body: string): string[] {
  const results: string[] = []
  let currentData = ""
  for (const line of body.split("\n")) {
    const trimmed = line.trimEnd()
    if (trimmed.startsWith("data: ")) {
      currentData = trimmed.slice(6)
    } else if (trimmed === "" && currentData) {
      results.push(currentData)
      currentData = ""
    }
  }
  if (currentData) results.push(currentData)
  return results
}

// ---------------------------------------------------------------------------
// JSON-RPC framing
// ---------------------------------------------------------------------------

let _nextId = 1

function nextId(): number {
  return _nextId++
}

export interface McpTool {
  name: string
  description: string
  inputSchema: Record<string, unknown>
}

// ---------------------------------------------------------------------------
// MCP session
// ---------------------------------------------------------------------------

export class McpSession {
  private sessionId: string | null = null
  private accessToken: string

  constructor(accessToken: string) {
    this.accessToken = accessToken
  }

  updateToken(token: string): void {
    this.accessToken = token
  }

  /**
   * Send a single JSON-RPC request and return the result.
   * Handles both JSON and SSE responses.
   */
  private async send(
    method: string,
    params: Record<string, unknown> = {},
  ): Promise<unknown> {
    const id = nextId()
    const body = JSON.stringify({
      jsonrpc: "2.0",
      id,
      method,
      params,
    })

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
      Authorization: `Bearer ${this.accessToken}`,
    }
    if (this.sessionId) {
      headers["Mcp-Session-Id"] = this.sessionId
    }

    debug(`send ${method}`, params)

    const response = await fetch(MCP_URL, {
      method: "POST",
      headers,
      body,
    })

    // Capture session ID from response headers
    const newSessionId = response.headers.get("mcp-session-id")
    if (newSessionId) {
      this.sessionId = newSessionId
      debug("session id:", this.sessionId)
    }

    if (!response.ok) {
      const text = await response.text()
      throw new Error(`MCP HTTP ${response.status}: ${text}`)
    }

    const contentType = response.headers.get("content-type") ?? ""
    const text = await response.text()

    let jsonText: string
    if (contentType.includes("text/event-stream")) {
      const dataLines = parseSseBody(text)
      if (dataLines.length === 0) {
        throw new Error(`MCP SSE response had no data lines for ${method}`)
      }
      jsonText = dataLines[0]
    } else {
      jsonText = text
    }

    debug(`response ${method}`, jsonText.slice(0, 200))

    const rpc = JSON.parse(jsonText) as {
      jsonrpc: string
      id: number
      result?: unknown
      error?: { code: number; message: string }
    }

    if (rpc.error) {
      throw new Error(`MCP error ${rpc.error.code}: ${rpc.error.message}`)
    }

    return rpc.result
  }

  /**
   * Send the MCP initialize request and return server capabilities.
   */
  async initialize(): Promise<{ protocolVersion: string; serverInfo: { name: string; version: string } }> {
    const result = await this.send("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "pi-atlassian-mcp", version: "1.0.0" },
    })
    return result as { protocolVersion: string; serverInfo: { name: string; version: string } }
  }

  /**
   * Send the initialized notification (no response expected).
   * Best-effort — ignore errors.
   */
  async sendInitializedNotification(): Promise<void> {
    try {
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: `Bearer ${this.accessToken}`,
      }
      if (this.sessionId) {
        headers["Mcp-Session-Id"] = this.sessionId
      }
      await fetch(MCP_URL, {
        method: "POST",
        headers,
        body: JSON.stringify({
          jsonrpc: "2.0",
          method: "notifications/initialized",
          params: {},
        }),
      })
    } catch {
      // Best-effort
    }
  }

  /**
   * List all available tools from the MCP server.
   * Applies description rewrites for default-cloudId (Issue #3),
   * getCloudId wording (Issue #6), and registers the synthetic
   * transitionJiraIssueByName tool (Issue #2).
   */
  async listTools(): Promise<McpTool[]> {
    const result = await this.send("tools/list", {}) as { tools?: McpTool[] }
    let tools = result?.tools ?? []

    const defaultCloudId = getDefaultCloudId()

    tools = tools.map((tool) => {
      // Issue #6: improve getCloudId description
      if (tool.name === "getCloudId") {
        tool = rewriteGetCloudIdDescription(tool)
      }
      // Issue #3: when a default cloudId is configured, rewrite descriptions
      if (defaultCloudId) {
        tool = rewriteDescription(tool, defaultCloudId)
      }
      return tool
    })

    return tools
  }

  /**
   * Inject the default cloudId into args if configured and the caller omitted it.
   * Issue #3.
   */
  private injectDefaultCloudId(args: Record<string, unknown>): Record<string, unknown> {
    const defaultCloudId = getDefaultCloudId()
    if (defaultCloudId && !("cloudId" in args)) {
      debug("injecting default cloudId:", defaultCloudId)
      return { cloudId: defaultCloudId, ...args }
    }
    return args
  }

  /**
   * Fetch transitions for a Jira issue via the Jira REST API as a fallback
   * when the upstream MCP tool returns an empty object.
   * Issue #1.
   *
   * Returns a result-shaped object with content array, or null if the cloudId
   * is not available (cannot build the REST URL).
   */
  private async fetchTransitionsFallback(
    args: Record<string, unknown>,
  ): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean } | null> {
    const issueIdOrKey = args["issueIdOrKey"] as string | undefined
    if (!issueIdOrKey) {
      return {
        content: [{ type: "text", text: "getTransitionsForJiraIssue: issueIdOrKey is required" }],
        isError: true,
      }
    }

    // Determine the Jira site base URL from cloudId using the Atlassian REST
    // resource listing. For the REST fallback we need the actual site URL.
    // The accessible resources endpoint returns [{id, url, name, ...}].
    // We use the Atlassian REST API directly via the known pattern:
    //   https://api.atlassian.com/ex/jira/{cloudId}/rest/api/3/issue/{key}/transitions
    const effectiveCloudId = (args["cloudId"] as string | undefined) ?? getDefaultCloudId()
    if (!effectiveCloudId) {
      debug("transitions fallback: no cloudId available")
      return null
    }

    const restUrl = `https://api.atlassian.com/ex/jira/${effectiveCloudId}/rest/api/3/issue/${issueIdOrKey}/transitions`
    debug("transitions REST fallback:", restUrl)

    const response = await fetch(restUrl, {
      method: "GET",
      headers: {
        Authorization: `Bearer ${this.accessToken}`,
        Accept: "application/json",
      },
    })

    if (response.status === 404) {
      return {
        content: [{ type: "text", text: `getTransitionsForJiraIssue: issue not found: ${issueIdOrKey}` }],
        isError: true,
      }
    }

    if (response.status === 403 || response.status === 401) {
      return {
        content: [{
          type: "text",
          text: `getTransitionsForJiraIssue: permission denied — the authenticated account lacks permission to view transitions for ${issueIdOrKey}`,
        }],
        isError: true,
      }
    }

    if (!response.ok) {
      const text = await response.text()
      return {
        content: [{ type: "text", text: `getTransitionsForJiraIssue: REST fallback failed (${response.status}): ${text}` }],
        isError: true,
      }
    }

    const data = await response.json() as { transitions?: Array<{ id: string; name: string; [k: string]: unknown }> }
    const transitions = data.transitions ?? []
    const simplified = transitions.map(({ id, name }) => ({ id, name }))

    return {
      content: [{ type: "text", text: JSON.stringify({ transitions: simplified }) }],
      isError: false,
    }
  }

  /**
   * Call a tool and return the raw content array.
   * Injects default cloudId when configured (Issue #3).
   * For getTransitionsForJiraIssue, falls back to REST API when upstream
   * returns an empty object (Issue #1).
   */
  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean }> {
    // Issue #3: inject default cloudId
    const effectiveArgs = this.injectDefaultCloudId(args)

    const result = await this.send("tools/call", {
      name,
      arguments: effectiveArgs,
    }) as { content: Array<{ type: string; text?: string }>; isError?: boolean }

    // Issue #1: if getTransitionsForJiraIssue returned {} (empty), use REST fallback
    if (name === "getTransitionsForJiraIssue" && !result.isError) {
      const text = result.content?.map((c) => c.text ?? "").join("").trim()
      if (text === "{}" || text === "") {
        debug("getTransitionsForJiraIssue returned {}, trying REST fallback")
        const fallback = await this.fetchTransitionsFallback(effectiveArgs)
        if (fallback !== null) {
          return fallback
        }
      }
    }

    return result
  }
}

// ---------------------------------------------------------------------------
// Session factory — creates and initializes a session
// ---------------------------------------------------------------------------

export async function createMcpSession(accessToken: string): Promise<McpSession> {
  const session = new McpSession(accessToken)
  const serverInfo = await session.initialize()
  debug("connected to", serverInfo.serverInfo?.name, serverInfo.serverInfo?.version)
  await session.sendInitializedNotification()
  return session
}
