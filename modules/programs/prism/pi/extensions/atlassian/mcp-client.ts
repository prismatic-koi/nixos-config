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
   */
  async listTools(): Promise<McpTool[]> {
    const result = await this.send("tools/list", {}) as { tools?: McpTool[] }
    return result?.tools ?? []
  }

  /**
   * Call a tool and return the raw content array.
   */
  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean }> {
    const result = await this.send("tools/call", {
      name,
      arguments: args,
    }) as { content: Array<{ type: string; text?: string }>; isError?: boolean }
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
