// Streamable HTTP MCP client for mcp.notion.com.
//
// Hand-rolled using Node.js built-ins. `@modelcontextprotocol/sdk` is not in
// pi's dependency tree (verified against pi-coding-agent 0.82.1) so it cannot
// be imported, even though Notion's own client guide recommends it.
//
// Ported from atlassian/mcp-client.ts with every Atlassian-specific concern
// removed: there is no cloudId (a Notion OAuth grant fixes the workspace), no
// description rewriting, and no REST fallback.
//
// Protocol: MCP streamable HTTP.
// - POST to the MCP endpoint with Content-Type: application/json
// - Accept: application/json, text/event-stream
// - Server responds with either JSON (single response) or SSE (streaming)
// - Session ID is returned in Mcp-Session-Id and replayed on later requests

const DEFAULT_MCP_URL = "https://mcp.notion.com/mcp"

function mcpUrl(): string {
  return process.env.NOTION_MCP_URL || DEFAULT_MCP_URL
}

// SECURITY: `debug` must never receive the access token or an Authorization
// header. Request bodies (JSON-RPC params) and truncated response payloads
// are fine — they carry Notion content, not credentials.
function debug(...args: unknown[]): void {
  if (process.env.NOTION_MCP_DEBUG === "1") {
    console.error("[notion-mcp]", ...args)
  }
}

// ---------------------------------------------------------------------------
// SSE parser
// ---------------------------------------------------------------------------

/** Parse a text/event-stream body and extract each event's `data` payload. */
export function parseSseBody(body: string): string[] {
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
    const body = JSON.stringify({ jsonrpc: "2.0", id, method, params })

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
      Authorization: `Bearer ${this.accessToken}`,
    }
    if (this.sessionId) {
      headers["Mcp-Session-Id"] = this.sessionId
    }

    // NOTE: `headers` is deliberately never logged — it carries the bearer.
    debug(`send ${method}`, params)

    const response = await fetch(mcpUrl(), { method: "POST", headers, body })

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

  async initialize(): Promise<{
    protocolVersion: string
    serverInfo: { name: string; version: string }
  }> {
    const result = await this.send("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "pi-notion-mcp", version: "1.0.0" },
    })
    return result as { protocolVersion: string; serverInfo: { name: string; version: string } }
  }

  /** Best-effort `notifications/initialized`; errors are ignored. */
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
      await fetch(mcpUrl(), {
        method: "POST",
        headers,
        body: JSON.stringify({
          jsonrpc: "2.0",
          method: "notifications/initialized",
          params: {},
        }),
      })
    } catch {
      // Best-effort.
    }
  }

  /** Enumerate the tools the grant exposes. */
  async listTools(): Promise<McpTool[]> {
    const result = (await this.send("tools/list", {})) as { tools?: McpTool[] }
    return result?.tools ?? []
  }

  async callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<{ content: Array<{ type: string; text?: string }>; isError?: boolean }> {
    return (await this.send("tools/call", { name, arguments: args })) as {
      content: Array<{ type: string; text?: string }>
      isError?: boolean
    }
  }
}

export async function createMcpSession(accessToken: string): Promise<McpSession> {
  const session = new McpSession(accessToken)
  const serverInfo = await session.initialize()
  debug("connected to", serverInfo.serverInfo?.name, serverInfo.serverInfo?.version)
  await session.sendInitializedNotification()
  return session
}
