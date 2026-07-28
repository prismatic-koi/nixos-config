// Integration-shaped tests for mcp-client.ts using a scripted fake child.
//
// The fake child is a tiny node --input-type=module snippet that reads
// newline-delimited JSON from stdin and echoes canned responses. This
// exercises the full StdioMcpSession framing loop (chunk buffering, line
// splitting, request/response correlation) without needing the real
// mcp-grafana binary.

import { test } from "node:test"
import assert from "node:assert/strict"
import { execPath } from "node:process"
import { createStdioMcpSession } from "./mcp-client.ts"

// A tiny stdio JSON-RPC echo server. Reads NDJSON on stdin and:
//   - for `initialize` → returns a well-formed serverInfo result
//   - for `tools/list` → returns two fake tools
//   - for `tools/call name=echo` → returns {content:[{type:text,text:<args>}]}
//   - for `notifications/*` → silently ignored (matches MCP notification shape)
//   - for anything else → returns error -32601 (method not found)
const FAKE_SERVER_SRC = `
let buf = ""
process.stdin.setEncoding("utf8")
process.stdin.on("data", (chunk) => {
  buf += chunk
  let idx
  while ((idx = buf.indexOf("\\n")) >= 0) {
    const line = buf.slice(0, idx)
    buf = buf.slice(idx + 1)
    if (line.trim() === "") continue
    let msg
    try { msg = JSON.parse(line) } catch { continue }
    if (msg.id === undefined) continue // notification
    let result, error
    if (msg.method === "initialize") {
      result = { protocolVersion: "2024-11-05", serverInfo: { name: "fake", version: "1" } }
    } else if (msg.method === "tools/list") {
      result = { tools: [
        { name: "t1", description: "d1", inputSchema: { type: "object" } },
        { name: "t2", description: "d2", inputSchema: { type: "object" } },
      ] }
    } else if (msg.method === "tools/call" && msg.params?.name === "echo") {
      result = { content: [{ type: "text", text: JSON.stringify(msg.params.arguments ?? {}) }] }
    } else {
      error = { code: -32601, message: "method not found: " + msg.method }
    }
    const resp = error ? { jsonrpc: "2.0", id: msg.id, error } : { jsonrpc: "2.0", id: msg.id, result }
    process.stdout.write(JSON.stringify(resp) + "\\n")
  }
})
`

// Spawn a fake server via node -e <source>. Uses process.execPath which is
// the current node/tsx binary — both accept -e.
async function withFakeServer<T>(
  fn: (binPath: string, args: string[]) => Promise<T>,
): Promise<T> {
  // We can't pass args to createStdioMcpSession directly (the API takes only
  // binPath), so instead we write the fake server to a tmp .mjs and point
  // binPath at a wrapper shell script that invokes node with the file.
  const { mkdtempSync, writeFileSync, chmodSync, rmSync } = await import("node:fs")
  const { tmpdir } = await import("node:os")
  const { join } = await import("node:path")
  const dir = mkdtempSync(join(tmpdir(), "grafana-mcp-fake-"))
  const serverFile = join(dir, "server.mjs")
  writeFileSync(serverFile, FAKE_SERVER_SRC)
  const wrapper = join(dir, "fake-mcp-grafana")
  writeFileSync(wrapper, `#!/bin/sh\nexec ${execPath} ${serverFile}\n`)
  chmodSync(wrapper, 0o755)
  try {
    return await fn(wrapper, [])
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

test("StdioMcpSession: initialize + listTools round-trip", async () => {
  await withFakeServer(async (binPath) => {
    const session = await createStdioMcpSession({
      binPath,
      grafanaUrl: "http://x",
      grafanaApiKey: "k",
    })
    try {
      const tools = await session.listTools()
      assert.equal(tools.length, 2)
      assert.equal(tools[0].name, "t1")
      assert.equal(tools[1].name, "t2")
    } finally {
      session.close()
    }
  })
})

test("StdioMcpSession: callTool forwards arguments verbatim", async () => {
  await withFakeServer(async (binPath) => {
    const session = await createStdioMcpSession({
      binPath,
      grafanaUrl: "http://x",
      grafanaApiKey: "k",
    })
    try {
      const result = await session.callTool("echo", { q: "hello", n: 42 })
      assert.equal(result.isError, undefined)
      const text = result.content?.[0]?.text ?? ""
      assert.deepEqual(JSON.parse(text), { q: "hello", n: 42 })
    } finally {
      session.close()
    }
  })
})

test("StdioMcpSession: unknown method returns MCP error", async () => {
  await withFakeServer(async (binPath) => {
    const session = await createStdioMcpSession({
      binPath,
      grafanaUrl: "http://x",
      grafanaApiKey: "k",
    })
    try {
      await assert.rejects(
        () => session.callTool("this-does-not-exist", {}),
        /method not found/,
      )
    } finally {
      session.close()
    }
  })
})

test("StdioMcpSession: close terminates the child and rejects further calls", async () => {
  await withFakeServer(async (binPath) => {
    const session = await createStdioMcpSession({
      binPath,
      grafanaUrl: "http://x",
      grafanaApiKey: "k",
    })
    session.close()
    // Subsequent callTool must fail fast, not hang.
    await assert.rejects(() => session.callTool("echo", {}))
  })
})

test("StdioMcpSession: handshake failure tears down the child (nonexistent binary)", async () => {
  await assert.rejects(() =>
    createStdioMcpSession({
      binPath: "/nonexistent/path/mcp-grafana",
      grafanaUrl: "http://x",
      grafanaApiKey: "k",
    }),
  )
})
