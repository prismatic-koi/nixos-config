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
//   * Move the connect/register work back into `onSessionStart` → "session_start
//     registers only activate_notion" and "session_start opens no connection"
//     fail (issue #2532).

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import { invalidateCache, NotionAuthTerminalError, type NotionTokens } from "./auth.ts"
import {
  ACTIVATE_NOTION_DESCRIPTION,
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

/**
 * Drive the deferred registration the way the model does: call the
 * `activate_notion` gateway tool. Fails loudly when it was never registered,
 * because that is a different bug from a failed activation.
 */
async function activateVia(rec: Recorder) {
  const gateway = rec.registered.find((t) => t.name === "activate_notion")
  assert.ok(gateway, "activate_notion must be registered before it can be called")
  return gateway.execute("activate-call", {}, undefined)
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
      "activate_notion",
      "notion-search",
      "notion-update-page",
      "notion-move-pages",
    ])
    assert.ok(ctx.notices.some((n) => /3 tools are now available/.test(n.msg)))
  })

  it("a repeat login re-registers nothing", async () => {
    // The login path now goes through the same gateway as the activate tool,
    // so it cannot trip pi's duplicate-tool-name guard. Historically this was
    // the bug: /login-notion after a successful startup re-registered the
    // whole surface.
    const rec = makeDeps({ isEnabledForCwd: () => true })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    // First login: no tokens on disk, so the OAuth flow runs and the gateway
    // registers the family behind it.
    await ext.onLoginCommand(host.host, makeCtx().ctx)
    const afterFirst = host.registered.length
    assert.equal(afterFirst, 4, "activate_notion plus the 3 sample tools")

    // Second login: the credential is valid now, so it short-circuits.
    const ctx = makeCtx()
    await ext.onLoginCommand(host.host, ctx.ctx)

    assert.equal(host.registered.length, afterFirst)
    assert.equal(rec.connectCalls, 1)
    assert.ok(ctx.notices.some((n) => /already authenticated/.test(n.msg)))
  })

  it("a gateway call after login reports already-active, not a duplicate", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => true })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onLoginCommand(host.host, makeCtx().ctx)
    const afterLogin = host.registered.length

    const result = await activateVia(host)

    assert.equal(host.registered.length, afterLogin)
    assert.equal(rec.connectCalls, 1)
    assert.match(result.content[0].text ?? "", /already active/)
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
  it("registers no family tools even when a call site forgets to check", async () => {
    // Drive activation with a gate that is open for the entry checks and shut
    // by the time registration happens. This models a call site that checked
    // once and then reached registration anyway — the class of bug round-1
    // review found in /login-notion. The choke point inside registerTools must
    // still hold.
    writeValidTokens()
    let calls = 0
    let open = true
    const rec = makeDeps({
      isEnabledForCwd: () => {
        calls++
        return open
      },
    })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    const callsAfterStart = calls
    // Shut the gate from inside the activation, after `activate`'s own
    // fail-fast check has already passed.
    rec.deps.connect = async () => {
      open = false
      rec.connectCalls++
      return {
        updateToken() {},
        async listTools() {
          return SAMPLE_TOOLS
        },
        async callTool() {
          return { content: [] }
        },
      }
    }

    const result = await activateVia(host)

    assert.ok(
      calls > callsAfterStart + 1,
      "registerTools must re-check the gate, not trust its caller",
    )
    assert.deepEqual(
      host.registered.map((t) => t.name),
      ["activate_notion"],
      "the choke point must refuse even when the caller already let it through",
    )
    assert.equal(result.isError, true, "a refused registration is not a success")
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

  it("registers only the gateway inside the allowlist", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
  })
})

// ---------------------------------------------------------------------------
// Deferred registration (issue #2532)
// ---------------------------------------------------------------------------

describe("session_start defers everything behind activate_notion", () => {
  it("registers exactly one tool, named activate_notion", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const host = makeHost()

    await createNotionExtension(rec.deps).onSessionStart(host.host, makeCtx().ctx)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
  })

  it("opens no connection at session_start", async () => {
    writeValidTokens()
    const rec = makeDeps()

    await createNotionExtension(rec.deps).onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(rec.connectCalls, 0)
  })

  it("names the Notion capability areas and says calling it reveals the surface", async () => {
    writeValidTokens()
    const host = makeHost()
    await createNotionExtension(makeDeps().deps).onSessionStart(host.host, makeCtx().ctx)

    assert.equal(host.registered[0].description, ACTIVATE_NOTION_DESCRIPTION)
    for (const area of ["search", "page", "database", "comment"]) {
      assert.ok(
        ACTIVATE_NOTION_DESCRIPTION.toLowerCase().includes(area),
        `description must name the "${area}" capability area`,
      )
    }
    assert.match(ACTIVATE_NOTION_DESCRIPTION, /registers the full/i)
    assert.ok(ACTIVATE_NOTION_DESCRIPTION.length < 900)
  })

  it("registers the full surface when the gateway is called", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    await ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)

    assert.equal(rec.connectCalls, 1)
    assert.deepEqual(host.registered.map((t) => t.name), [
      "activate_notion",
      "notion-search",
      "notion-update-page",
      "notion-move-pages",
    ])
    assert.equal(result.isError, undefined)
    assert.match(result.content[0].text ?? "", /3 tools are now available/)
    assert.equal(ext.isActive(), true)
  })

  it("a second gateway call registers no duplicates", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()
    await ext.onSessionStart(host.host, makeCtx().ctx)

    await activateVia(host)
    const second = await activateVia(host)

    assert.equal(host.registered.length, 4)
    assert.equal(rec.connectCalls, 1)
    assert.match(second.content[0].text ?? "", /already active/)
  })

  it("registers neither the family nor activate_notion outside the allowlist", async () => {
    writeValidTokens()
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.deepEqual(host.registered, [])
    assert.equal(rec.connectCalls, 0)
  })
})

describe("eager roles", () => {
  it("a listed role activates at before_agent_start with no tool call", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension({
      ...rec.deps,
      env: { NOTION_MCP_EAGER_ROLES: "coordinator" },
    })
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 4)
  })

  it("a non-eager role does not activate", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension({
      ...rec.deps,
      env: { NOTION_MCP_EAGER_ROLES: "coordinator" },
    })
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "worker")

    assert.equal(rec.connectCalls, 0)
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
  })

  it("the eager check runs at most once per session", async () => {
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension({
      ...rec.deps,
      env: { NOTION_MCP_EAGER_ROLES: "coordinator" },
    })
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 4)
  })
})

// ---------------------------------------------------------------------------
// Non-blocking startup  (AC: edge-case)
// ---------------------------------------------------------------------------

describe("a broken provider never blocks pi", () => {
  it("reports the missing-token case as an error result, not an exception", async () => {
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    const result = await activateVia(host)

    assert.equal(rec.connectCalls, 0)
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /login-notion/)
  })

  it("reports a failed MCP connection as an error result", async () => {
    writeValidTokens()
    const rec = makeDeps({ connectFails: true })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    const result = await activateVia(host)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /could not reach mcp\.notion\.com/)
  })

  it("reports an empty tools/list and registers nothing", async () => {
    writeValidTokens()
    const rec = makeDeps({ tools: [] })
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    const result = await activateVia(host)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /returned no tools/)
  })

  it("notifies rather than throwing when an eager activation fails", async () => {
    const rec = makeDeps({ connectFails: true })
    writeValidTokens()
    const ext = createNotionExtension({
      ...rec.deps,
      env: { NOTION_MCP_EAGER_ROLES: "coordinator" },
    })
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, ctx.ctx, "coordinator")

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
    assert.ok(ctx.notices.some((n) => n.type === "error"))
  })
})

// ---------------------------------------------------------------------------
// The 30-day refresh-token inactivity clock (round-3 review of PR #2568)
//
// Notion refresh tokens die after 30 consecutive days of inactivity. Before
// #2532, onSessionStart called ensureTokens on every vault session, so ordinary
// session starts kept the rotation alive. Deferring everything behind
// activate_notion moved that clock: with eagerRoles = [ ], a refresh would only
// happen when a Notion tool was called, and 30 quiet days would kill the grant.
// Recovery is /login-notion — a browser flow a headless worker cannot complete.
//
// These tests exist because the regression is INVISIBLE FOR 30 DAYS. Nothing
// else stands between us and a repeat.
//
// Revert-and-watch-fail: delete the `await keepTokensAlive(ctx)` call from
// onSessionStart → "refreshes a stale token at session_start" and "keeps the
// clock alive without registering the surface" both fail.
// ---------------------------------------------------------------------------

describe("session_start keeps the refresh-token clock alive", () => {
  function makeAuthSpy(opts: { stale?: boolean; tokens?: NotionTokens | null; refreshThrows?: Error } = {}) {
    const spy = { loadCalls: 0, refreshCalls: 0 }
    const stored =
      opts.tokens === undefined
        ? makeTokens({ accessToken: "stored" })
        : opts.tokens
    return {
      spy,
      overrides: {
        loadTokens: () => {
          spy.loadCalls++
          return stored
        },
        needsRefresh: () => opts.stale ?? false,
        getValidAccessToken: async () => {
          spy.refreshCalls++
          if (opts.refreshThrows) throw opts.refreshThrows
          const t = makeTokens({ accessToken: "refreshed" })
          return { token: t.accessToken, tokens: t }
        },
      } satisfies Partial<ExtensionDeps>,
    }
  }

  it("refreshes a stale token at session_start", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({ stale: true })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })

    await ext.onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(auth.spy.refreshCalls, 1, "a stale grant must be refreshed at session_start")
  })

  it("keeps the clock alive without registering the surface or connecting", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({ stale: true })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    // The whole point: the token stays alive and the schemas stay out.
    assert.equal(auth.spy.refreshCalls, 1)
    assert.equal(rec.connectCalls, 0, "no MCP connection at session_start")
    assert.deepEqual(
      host.registered.map((t) => t.name),
      ["activate_notion"],
      "no tool schemas may enter the prompt prefix",
    )
    assert.equal(ext.isActive(), false)
  })

  it("performs no network refresh when the token is still fresh", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({ stale: false })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })

    await ext.onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(auth.spy.loadCalls >= 1, true, "the store is still read (it is cheap)")
    assert.equal(auth.spy.refreshCalls, 0, "most session starts must cost no network")
  })

  it("reads no token file and refreshes nothing outside the vault scope (AC 11)", async () => {
    const rec = makeDeps({ isEnabledForCwd: () => false })
    const auth = makeAuthSpy({ stale: true })
    const ext = createNotionExtension({
      ...rec.deps,
      ...auth.overrides,
      isEnabledForCwd: () => false,
    })
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.equal(auth.spy.loadCalls, 0, "an out-of-scope session must touch nothing")
    assert.equal(auth.spy.refreshCalls, 0)
    assert.deepEqual(host.registered, [])
  })

  it("a terminal auth failure notifies and does not throw", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({
      stale: true,
      refreshThrows: new NotionAuthTerminalError(
        "Notion MCP: the grant was revoked. Run /login-notion.",
      ),
    })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onSessionStart(host.host, ctx.ctx)

    assert.ok(ctx.notices.some((n) => /login-notion/.test(n.msg)))
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
  })

  it("a transient refresh failure does not stop the session or nag", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({ stale: true, refreshThrows: new Error("fetch failed") })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onSessionStart(host.host, ctx.ctx)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_notion"])
    assert.deepEqual(ctx.notices, [], "a transient fault is logged, not surfaced")
  })

  it("notifies when there are no tokens at all", async () => {
    const rec = makeDeps()
    const auth = makeAuthSpy({ tokens: null })
    const ext = createNotionExtension({ ...rec.deps, ...auth.overrides })
    const ctx = makeCtx()

    await ext.onSessionStart(makeHost().host, ctx.ctx)

    assert.equal(auth.spy.refreshCalls, 0)
    assert.ok(ctx.notices.some((n) => /login-notion/.test(n.msg)))
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
    await activateVia(host)

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
    await activateVia(host)

    const search = host.registered.find((t) => t.name === "notion-search")
    assert.ok(search)

    // A throwing transport must surface as an isError result, never as an
    // exception that would tear down the pi session.
    const result = await search.execute("call-2", {}, undefined)
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /500/)
  })

  it("does not re-register on a second session_start", async () => {
    // /reload re-emits session_start with reason: "reload".
    writeValidTokens()
    const rec = makeDeps()
    const ext = createNotionExtension(rec.deps)
    const host = makeHost()

    await ext.onSessionStart(host.host, makeCtx().ctx)
    await activateVia(host)
    await ext.onSessionStart(host.host, makeCtx().ctx)

    assert.equal(host.registered.length, 4, "pi rejects duplicate tool registration")
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
    await activateVia(hostA)

    assert.equal(hostA.registered.length, 4, "A activated: gateway plus 3 tools")
    assert.deepEqual(
      hostB.registered.map((t) => t.name),
      ["activate_notion"],
      "B must not inherit A's activation",
    )
    assert.equal(a.isActive(), true)
    assert.equal(b.isActive(), false)
  })
})
