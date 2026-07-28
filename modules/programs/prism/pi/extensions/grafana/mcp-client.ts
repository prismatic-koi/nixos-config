// Stdio MCP client bridging pi to the nixpkgs `mcp-grafana` Go binary.
//
// Hand-rolled using Node.js built-ins. `@modelcontextprotocol/sdk` is not in
// pi's dependency tree (verified against pi-coding-agent 0.82.1) so it cannot
// be imported.
//
// Unlike the sibling Streamable-HTTP clients in `../atlassian/mcp-client.ts`
// and `../notion/mcp-client.ts`, this transport is stdio: JSON-RPC messages
// are exchanged as newline-delimited JSON on the child process's stdin /
// stdout. The child is spawned by createStdioMcpSession(); its stderr is
// forwarded to console.error prefixed with "[grafana-mcp/child]" for
// operator diagnosis.
//
// Protocol (MCP stdio transport, spec 2024-11-05):
//
//   - Each JSON-RPC request/response is a single line of JSON terminated by
//     "\n". No Content-Length framing (that is the LSP-style variant, not
//     what MCP stdio uses).
//   - The client sends `initialize`, then `notifications/initialized` (best
//     effort), then any number of `tools/list` / `tools/call` requests.
//   - Session lifecycle is bound to the child process. When the child
//     exits, in-flight requests reject and subsequent calls fail fast.
//
// SECURITY: `debug` must never log the child's env or the parsed contents
// of GRAFANA_API_KEY. The child's stdout carries MCP JSON-RPC (tool results,
// not credentials) so logging response bodies is acceptable; the child's
// stderr is passed through to console.error so operators see server-side
// errors verbatim.

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"

function debug(...args: unknown[]): void {
  if (process.env.GRAFANA_MCP_DEBUG === "1") {
    console.error("[grafana-mcp]", ...args)
  }
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface McpTool {
  name: string
  description: string
  inputSchema: Record<string, unknown>
}

export interface McpCallResult {
  content: Array<{ type: string; text?: string }>
  isError?: boolean
}

export interface StdioMcpSessionOptions {
  /** Absolute path to the mcp-grafana binary (from PI_GRAFANA_MCP_BIN). */
  binPath: string
  /** GRAFANA_URL for the child's env. */
  grafanaUrl: string
  /** GRAFANA_API_KEY for the child's env. */
  grafanaApiKey: string
  /**
   * Additional env vars to pass to the child. Merged over a minimal base
   * (PATH, HOME, plus GRAFANA_URL / GRAFANA_API_KEY). The bundle's KEY=VALUE
   * entries beyond the two well-known keys flow through here so a future
   * bundle format extension (e.g. GRAFANA_ORG_ID) reaches the child without
   * a code change in this client.
   */
  extraEnv?: Record<string, string>
}

// ---------------------------------------------------------------------------
// JSON-RPC framing
// ---------------------------------------------------------------------------

let _nextId = 1
function nextId(): number {
  return _nextId++
}

interface PendingRequest {
  resolve: (value: unknown) => void
  reject: (err: Error) => void
}

interface JsonRpcResponse {
  jsonrpc: string
  id: number
  result?: unknown
  error?: { code: number; message: string; data?: unknown }
}

// ---------------------------------------------------------------------------
// StdioMcpSession
// ---------------------------------------------------------------------------

export class StdioMcpSession {
  private child: ChildProcessWithoutNullStreams
  private pending = new Map<number, PendingRequest>()
  private stdoutBuf = ""
  private closed = false
  private closeErr: Error | null = null

  private constructor(child: ChildProcessWithoutNullStreams) {
    this.child = child

    this.child.stdout.setEncoding("utf8")
    this.child.stdout.on("data", (chunk: string) => this.onStdoutData(chunk))

    this.child.stderr.setEncoding("utf8")
    this.child.stderr.on("data", (chunk: string) => {
      // Forward child stderr line-by-line. mcp-grafana emits structured
      // logs there; keeping the prefix makes them findable in the pi
      // agent log.
      for (const line of chunk.split("\n")) {
        if (line.trim() !== "") console.error("[grafana-mcp/child]", line)
      }
    })

    this.child.on("exit", (code, signal) => {
      const msg = `mcp-grafana child exited (code=${code}, signal=${signal ?? "null"})`
      this.markClosed(new Error(msg))
    })
    this.child.on("error", (err) => {
      this.markClosed(new Error(`mcp-grafana child error: ${err.message}`))
    })
  }

  static spawn(opts: StdioMcpSessionOptions): StdioMcpSession {
    const env: Record<string, string> = {
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      HOME: process.env.HOME ?? "/tmp",
      GRAFANA_URL: opts.grafanaUrl,
      GRAFANA_API_KEY: opts.grafanaApiKey,
      ...(opts.extraEnv ?? {}),
    }
    // TERM is helpful for the child's structured logger to skip colour codes;
    // absent TERM is fine (mcp-grafana falls back to plain output).
    if (process.env.TERM) env.TERM = process.env.TERM

    // Explicitly pass no transport argument — mcp-grafana defaults to stdio
    // when invoked with no `-transport` flag. See `mcp-grafana --help`:
    // "-transport string  Transport type (stdio, sse or streamable-http)
    //  (default \"stdio\")".
    debug("spawning", opts.binPath)
    const child = spawn(opts.binPath, [], {
      env,
      stdio: ["pipe", "pipe", "pipe"],
    })
    return new StdioMcpSession(child)
  }

  /**
   * Reject all in-flight requests and mark the session closed. Any subsequent
   * send() call fails fast with the recorded error.
   */
  private markClosed(err: Error): void {
    if (this.closed) return
    this.closed = true
    this.closeErr = err
    for (const [, pending] of this.pending) {
      pending.reject(err)
    }
    this.pending.clear()
  }

  /**
   * Newline-delimited JSON on stdout. Buffer partial lines; parse each
   * complete line as a JSON-RPC response frame.
   */
  private onStdoutData(chunk: string): void {
    this.stdoutBuf += chunk
    while (true) {
      const idx = this.stdoutBuf.indexOf("\n")
      if (idx < 0) break
      const line = this.stdoutBuf.slice(0, idx)
      this.stdoutBuf = this.stdoutBuf.slice(idx + 1)
      if (line.trim() === "") continue
      this.handleLine(line)
    }
  }

  private handleLine(line: string): void {
    let parsed: JsonRpcResponse
    try {
      parsed = JSON.parse(line) as JsonRpcResponse
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      console.error(`[grafana-mcp] failed to parse JSON-RPC line: ${msg}: ${line.slice(0, 200)}`)
      return
    }

    // Ignore server-initiated notifications and progress messages: MCP allows
    // the server to push `logging/message`, `notifications/progress`, etc.
    // without an `id`. We do not act on them yet — the tool surface is fully
    // request/response driven — so drop them silently rather than warn.
    if (parsed.id === undefined || parsed.id === null) return

    const pending = this.pending.get(parsed.id)
    if (!pending) {
      debug("received response for unknown id", parsed.id)
      return
    }
    this.pending.delete(parsed.id)

    if (parsed.error) {
      pending.reject(
        new Error(`MCP error ${parsed.error.code}: ${parsed.error.message}`),
      )
      return
    }
    pending.resolve(parsed.result)
  }

  /**
   * Send a JSON-RPC request and await its response. Fails fast if the child
   * is already closed.
   */
  private send(
    method: string,
    params: Record<string, unknown> = {},
  ): Promise<unknown> {
    if (this.closed) {
      return Promise.reject(this.closeErr ?? new Error("mcp-grafana session closed"))
    }
    const id = nextId()
    const frame = JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n"
    debug("send", method, JSON.stringify(params).slice(0, 200))

    return new Promise<unknown>((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.child.stdin.write(frame, (err) => {
        if (err) {
          this.pending.delete(id)
          reject(new Error(`mcp-grafana stdin write failed: ${err.message}`))
        }
      })
    })
  }

  /**
   * Send a JSON-RPC notification (no `id`, no response awaited). Best-effort:
   * a write error is logged but not thrown, matching the atlassian/notion
   * `sendInitializedNotification` shape.
   */
  private notify(method: string, params: Record<string, unknown> = {}): void {
    if (this.closed) return
    const frame = JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n"
    this.child.stdin.write(frame, (err) => {
      if (err) debug(`notify ${method} write failed: ${err.message}`)
    })
  }

  async initialize(): Promise<{
    protocolVersion: string
    serverInfo: { name: string; version: string }
  }> {
    const result = await this.send("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "pi-grafana-mcp", version: "1.0.0" },
    })
    return result as { protocolVersion: string; serverInfo: { name: string; version: string } }
  }

  /** Best-effort `notifications/initialized`; errors are dropped. */
  sendInitializedNotification(): void {
    this.notify("notifications/initialized", {})
  }

  async listTools(): Promise<McpTool[]> {
    const result = (await this.send("tools/list", {})) as { tools?: McpTool[] }
    return result?.tools ?? []
  }

  async callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult> {
    return (await this.send("tools/call", { name, arguments: args })) as McpCallResult
  }

  /**
   * Terminate the child process and reject any in-flight requests. Idempotent.
   */
  close(): void {
    if (this.closed) return
    try {
      this.child.stdin.end()
    } catch {
      // best-effort
    }
    try {
      this.child.kill("SIGTERM")
    } catch {
      // best-effort
    }
    this.markClosed(new Error("mcp-grafana session closed by client"))
  }
}

/**
 * Spawn mcp-grafana and complete the MCP initialize handshake. Throws if the
 * handshake fails or the child dies before completing it.
 */
export async function createStdioMcpSession(
  opts: StdioMcpSessionOptions,
): Promise<StdioMcpSession> {
  const session = StdioMcpSession.spawn(opts)
  try {
    const info = await session.initialize()
    debug("connected to", info.serverInfo?.name, info.serverInfo?.version)
    session.sendInitializedNotification()
    return session
  } catch (err) {
    // If handshake fails we own the child — tear it down before rethrowing.
    session.close()
    throw err
  }
}
