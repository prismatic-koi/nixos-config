// Streamable HTTP MCP client for mcp.notion.com.
//
// Hand-rolled MCP client using Node.js built-ins (no @modelcontextprotocol/sdk;
// the SDK is not bundled in pi's dependency tree).
//
// Protocol: MCP streamable HTTP
//   - POST to https://mcp.notion.com/mcp with Content-Type: application/json
//   - Accept: application/json, text/event-stream
//   - Server responds with either JSON (single response) or SSE (streaming)
//   - Session ID is returned in Mcp-Session-Id header and must be sent on
//     subsequent requests
//
// This is a pared-down port of atlassian/mcp-client.ts. Notion has:
//   - No cloudId concept — the OAuth grant fixes the workspace.
//   - No Jira transitions REST fallback (all machinery removed).
//   - A single "default" scope.

const MCP_URL = process.env.NOTION_MCP_URL ?? "https://mcp.notion.com/mcp"
const DEBUG = process.env.NOTION_MCP_DEBUG === "1"

/**
 * Debug logger that MUST NOT expose credentials.
 *
 * Tokens (access_token, refresh_token) and Authorization headers are
 * never passed as arguments to this function. If a future change adds a
 * new debug callsite, the value must be sanitised at the callsite — this
 * function is deliberately not smart enough to detect and redact secrets
 * itself.
 */
function debug(...args: unknown[]): void {
  if (DEBUG) {
    console.error("[notion-mcp]", ...args)
  }
}

// ---------------------------------------------------------------------------
// SSE parser
// ---------------------------------------------------------------------------

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

    // Only the method + params are logged; the Authorization header and
    // access token are never passed to debug(). If you add a new debug
    // line here, keep it that way.
    debug(`send ${method}`, params)

    const response = await fetch(MCP_URL, {
      method: "POST",
      headers,
      body,
    })

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

    // Response body may contain tokens (e.g. an initialize response echoes
    // nothing sensitive, but tool responses may include workspace data).
    // We only log a short prefix to keep the noise floor low; do not
    // extend this to a full-body dump.
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

  async initialize(): Promise<{ protocolVersion: string; serverInfo: { name: string; version: string } }> {
    const result = await this.send("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "pi-notion-mcp", version: "1.0.0" },
    })
    return result as { protocolVersion: string; serverInfo: { name: string; version: string } }
  }

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

  async listTools(): Promise<McpTool[]> {
    const result = (await this.send("tools/list", {})) as { tools?: McpTool[] }
    return result?.tools ?? []
  }

  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean }> {
    const result = (await this.send("tools/call", {
      name,
      arguments: args,
    })) as { content: Array<{ type: string; text?: string }>; isError?: boolean }
    return result
  }
}

// ---------------------------------------------------------------------------
// Session factory
// ---------------------------------------------------------------------------

export async function createMcpSession(accessToken: string): Promise<McpSession> {
  const session = new McpSession(accessToken)
  const serverInfo = await session.initialize()
  debug("connected to", serverInfo.serverInfo?.name, serverInfo.serverInfo?.version)
  await session.sendInitializedNotification()
  return session
}
