// Unit tests for notion/extension.ts — the pi-agnostic extension core.
//
// Run with: tsx --test extension.test.ts (from this directory)
//
// These exist because round-1 review found that /login-notion bypassed the
// repo-scoping gate and registered the full Notion surface — including
// notion-update-page, notion-move-pages and notion-duplicate-page — in
// directories the allowlist excludes. That path was unreachable by tests
// while it lived in index.ts, which imports `typebox` (resolvable only inside
// pi's runtime). extension.ts exists so it is reachable.
//
// Revert-and-watch-fail pairs:
//   * Delete the `if (!isEnabledForCwd())` guard in `onLoginCommand` →
//     "does not register tools after login outside the allowlist" and
//     "opens no MCP connection after login outside the allowlist" fail.
//   * Delete the `if (!isEnabledForCwd())` guard at the top of
//     `registerTools` (the structural choke point) → "registerTools is itself
//     gated, so no call site can bypass the allowlist" fails.
//   * Delete the guard in `onSessionStart` → "opens no connection at
//     session_start outside the allowlist" fails.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import { invalidateCache, type NotionTokens } from "./auth.ts"
import {
  createNotionExtension,
  OUT_OF_SCOPE_MESSAGE,
  type ExtensionDeps,
  type NotifyContext,
  type NotionSessionLike,
  type ToolHost,
  type ToolSpec,
} from "./extension.ts"
import type { McpTool } from "./mcp-client.ts"

let tempDir: string
let tokenFile: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-notion-ext-test-"))
  tokenFile = join(tempDir, "notion-mcp-oauth.json")
  process.env.PI_NOTION_TOKENS = tokenFile
  process.env.PI_NOTION_CLIENT = join(tempDir, "notion-mcp-client.json")
  delete process.env.PI_CODING_AGENT_DIR
  delete process.env.NOTION_MCP_DEBUG
  invalidateCache()
})

afterEach(() => {
  delete process.env.PI_NOTION_TOKENS
  delete process.env.PI_NOTION_CLIENT
  delete process.env.NOTION_MCP_DEBUG
  rmSync(tempDir, { recursive: true, force: true })
  invalidateCache()
})

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

function makeTokens(overrides: Partial<NotionTokens> = {}): NotionTokens {
  return {
    accessToken: "acc",
    refreshToken: "ref",
    expiresAt: Date.now() + 3_600_000,
    clientId: "cid",
    ...overrides,
  }
}

function writeValidTokens(overrides: Partial<NotionTokens> = {}): NotionTokens {
  const tokens = makeTokens(overrides)
  writeFileSync(tokenFile, JSON.stringify(tokens), "utf-8")
  invalidateCache()
  return tokens
}

const SAMPLE_TOOLS: McpTool[] = [
  { name: "notion-search", description: "Search", inputSchema: { type: "object" } },
  { name: "notion-update-page", description: "Update", inputSchema: { type: "object" } },
  { name: "notion-move-pages", description: "Move", inputSchema: { type: "object" } },
]

interface Recorder {
  host: ToolHost
  registered: ToolSpec[]
}

function makeHost(): Recorder {
  const registered: ToolSpec[] = []
  return {
    registered,
    host: {
      registerTool(tool) {
        registered.push(tool)
      },
    },
  }
}

interface CtxRecorder {
  ctx: NotifyContext
  notices: Array<{ msg: string; type?: string }>
}

function makeCtx(): CtxRecorder {
  const notices: Array<{ msg: string; type?: string }> = []
  return {
    notices,
    ctx: { ui: { notify: (msg, type) => notices.push({ msg, type }) } },
  }
}

interface DepRecorder {
  deps: ExtensionDeps
  connectCalls: number
  loginCalls: number
  toolCalls: Array<{ name: string; args: Record<string, unknown> }>
}

function makeDeps(
  overrides: Partial<ExtensionDeps> & { tools?: McpTool[]; connectFails?: boolean } = {},
): DepRecorder {
  const rec: DepRecorder = {
    connectCalls: 0,
    loginCalls: 0,
    toolCalls: [],
    deps: {} as ExtensionDeps,
  }

  const session: NotionSessionLike = {
    updateToken() {},
    async listTools() {
      return overrides.tools ?? SAMPLE_TOOLS
    },
    async callTool(name, args) {
      rec.toolCalls.push({ name, args })
      return { content: [{ type: "text", text: `{"ok":"${name}"}` }] }
    },
  }

  rec.deps = {
    wrapSchema: (schema) => schema,
    async connect() {
      rec.connectCalls++
      if (overrides.connectFails) throw new Error("connect refused")
      return session
    },
    async login() {
      rec.loginCalls++
      return writeValidTokens({ accessToken: "post-login" })
    },
    isEnabledForCwd: () => true,
    ...(overrides.wrapSchema ? { wrapSchema: overrides.wrapSchema } : {}),
    ...(overrides.isEnabledForCwd ? { isEnabledForCwd: overrides.isEnabledForCwd } : {}),
    ...(overrides.login ? { login: overrides.login } : {}),
  }

  return rec
}

// ---------------------------------------------------------------------------
// Repo-scoping gate — /login-notion  (round-1 review-code blocker)
// ---------------------------------------------------------------------------

describe("login command respects the repo-scoping gate", () => {
  it("does not register tools after login outside the allowlist", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onLoginCommand(host.host, ctx.ctx)

    assert.equal(rec.loginCalls, 1, "the login itself must still be allowed anywhere")
    assert.deepEqual(
      host.registered.map((t) => t.name),
      [],
      "a full workspace read/write surface must not appear in a non-allowlisted repo",
    )
  })

  it("opens no MCP connection after login outside the allowlist", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const ext = createNotionExtension(rec.deps)

    await ext.onLoginCommand(makeHost().host, makeCtx().ctx)

    assert.equal(rec.connectCalls, 0)
  })

  it("tells the user why nothing was registered", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const ext = createNotionExtension(rec.deps)
    const ctx = makeCtx()

    await ext.onLoginCommand(makeHost().host, ctx.ctx)

    const texts = ctx.notices.map((n) => n.msg)
    assert.ok(
      texts.includes(OUT_OF_SCOPE_MESSAGE),
      `expected the out-of-scope notice, got: ${JSON.stringify(texts)}`,
    )
    assert.ok(
      OUT_OF_SCOPE_MESSAGE.includes("NOTION_MCP_REPOS"),
      "the notice must name the setting so the user can act on it",
    )
  })

  it("does register after login inside the allowlist", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => true })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onLoginCommand(host.host, ctx.ctx)

    assert.equal(rec.connectCalls, 1)
    assert.deepEqual(host.registered.map((t) => t.name), [
      "notion-search",
      "notion-update-page",
      "notion-move-pages",
    ])
    assert.ok(ctx.notices.some((n) => /registered 3 tools/.test(n.msg)))
  })

  it("short-circuits when already authenticated, without connecting", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const ctx = makeCtx()

    await ext.onLoginCommand(makeHost().host, ctx.ctx)

    assert.equal(rec.loginCalls, 0)
    assert.equal(rec.connectCalls, 0)
    assert.ok(ctx.notices.some((n) => /already authenticated/.test(n.msg)))
  })

  it("reports a failed login without throwing", async () => {
    const rec = makeDeps({
      login: async () => {
        throw new Error("user cancelled")
      },
    })
    const ext = createNotionExtension(rec.deps)
    const ctx = makeCtx()

    await ext.onLoginCommand(makeHost().host, ctx.ctx)

    assert.equal(rec.connectCalls, 0)
    assert.ok(ctx.notices.some((n) => n.type === "error" && /user cancelled/.test(n.msg)))
  })
})

// ---------------------------------------------------------------------------
// Repo-scoping gate — structural choke point
// ---------------------------------------------------------------------------

describe("registerTools is itself gated", () => {
  it("registers nothing even when a call site forgets to check", async () => {
    // Drive session_start with a gate that is open at entry and shut by the
    // time registration happens. This models a call site that checked once and
    // then reached registration anyway — the class of bug round-1 review found
    // in /login-notion. The choke point inside registerTools must still hold.
    writeValidTokens()
    let calls = 0
    const rec = makeDeps({
      isEnabledForCwd: () => {
        calls++
        return calls === 1 // open for the entry check only
      },
    })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.ok(calls >= 2, "registerTools must re-check the gate, not trust its caller")
    assert.deepEqual(
      host.registered.map((t) => t.name),
      [],
      "the choke point must refuse even when the caller already let it through",
    )
  })
})

// ---------------------------------------------------------------------------
// Repo-scoping gate — session_start
// ---------------------------------------------------------------------------

describe("session_start respects the repo-scoping gate", () => {
  it("opens no connection at session_start outside the allowlist", async () => {
    writeValidTokens()
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onSessionStart(host.host, ctx.ctx)

    assert.equal(rec.connectCalls, 0)
    assert.deepEqual(host.registered, [])
    assert.deepEqual(ctx.notices, [], "a silent skip — no nagging in unrelated repos")
  })

  it("registers the tool surface inside the allowlist", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 3)
  })
})

// ---------------------------------------------------------------------------
// Non-blocking startup  (AC: edge-case)
// ---------------------------------------------------------------------------

describe("session_start never blocks pi from starting", () => {
  it("notifies and returns when there are no tokens", async () => {
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const ctx = makeCtx()

    await ext.onSessionStart(makeHost().host, ctx.ctx)

    assert.equal(rec.connectCalls, 0)
    assert.ok(ctx.notices.some((n) => /login-notion/.test(n.msg)))
  })

  it("notifies and returns when the MCP connection fails", async () => {
    writeValidTokens()
    const rec = makeDeps({ connectFails: true })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onSessionStart(host.host, ctx.ctx)

    assert.deepEqual(host.registered, [])
    assert.ok(ctx.notices.some((n) => n.type === "error" && /unavailable/.test(n.msg)))
  })

  it("warns and registers nothing when tools/list returns empty", async () => {
    writeValidTokens()
    const rec = makeDeps({ tools: [] })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.deepEqual(host.registered, [])
  })
})

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

describe("registered tool execution", () => {
  it("forwards args and returns the flattened payload", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    await ext.onSessionStart(host.host, makeCtx().ctx)

    const search = host.registered.find((t) => t.name === "notion-search")
    assert.ok(search)
    const result = await search.execute("call-1", { query: "kōrero" }, undefined)

    assert.deepEqual(rec.toolCalls, [
      { name: "notion-search", args: { query: "kōrero" } },
    ])
    assert.equal(result.isError, undefined)
    assert.equal(result.content[0].text, '{"ok":"notion-search"}')
  })

  it("returns an error result rather than throwing when the call blows up", async () => {
    writeValidTokens()
    const base = makeDeps()
    const ext = createNotionExtension({
      ...base.deps,
      connect: async () => ({
        updateToken() {},
        async listTools() {
          return SAMPLE_TOOLS
        },
        async callTool() {
          throw new Error("MCP HTTP 500: upstream exploded")
        },
      }),
    })
    const host = makeHost()
    await ext.onSessionStart(host.host, makeCtx().ctx)

    const search = host.registered.find((t) => t.name === "notion-search")
    assert.ok(search)

    // A throwing transport must surface as an isError result, never as an
    // exception that would tear down the pi session.
    const result = await search.execute("call-2", {}, undefined)
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /500/)
  })

  it("does not re-register tools on a second pass", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.equal(host.registered.length, 3, "pi rejects duplicate tool registration")
  })
})

// ---------------------------------------------------------------------------
// Instance isolation
// ---------------------------------------------------------------------------

describe("createNotionExtension", () => {
  it("gives each instance independent registration state", async () => {
    writeValidTokens()
    const a = createNotionExtension(makeDeps().deps)
    const b = createNotionExtension(makeDeps().deps)
    const hostA = makeHost()
    const hostB = makeHost()

    await a.onSessionStart(hostA.host, makeCtx().ctx)
    await b.onSessionStart(hostB.host, makeCtx().ctx)

    assert.equal(hostA.registered.length, 3)
    assert.equal(hostB.registered.length, 3)
  })
})
