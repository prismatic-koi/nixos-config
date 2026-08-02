// Unit tests for atlassian/extension.ts — the pi-agnostic extension core.
//
// Run with: tsx --test extension.test.ts (from this directory)
//
// WHY THESE EXIST. Round-1 review of PR #2568 observed that atlassian is the
// ONE provider whose `eagerRoles` default is non-empty (`[ "coordinator" ]`),
// so its eager path is the one that actually runs in production — and it had
// no unit test at all, because every line of it sat behind index.ts's typebox
// import. AC 7, AC 9 and AC 10 of issue #2532 are all atlassian, and all three
// were previously backed only by a nix-eval statement, not a runtime
// observation. extension.ts exists so they are reachable.
//
// Revert-and-watch-fail pairs:
//   * Move the connect/register work back into `onSessionStart` →
//     "session_start registers only activate_atlassian" and "session_start
//     opens no connection" fail.
//   * Delete the `isEagerRole` check in `onBeforeAgentStart` → "a worker does
//     not activate at before_agent_start" fails.
//   * Delete the `eagerChecked` guard → "the eager check runs at most once per
//     session" fails.
//   * Make `activate` swallow its error instead of rethrowing → "an
//     unreachable mcp.atlassian.com returns an error result" fails.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import {
  ACTIVATE_ATLASSIAN_DESCRIPTION,
  createAtlassianExtension,
  type AtlassianSessionLike,
  type ExtensionDeps,
  type NotifyContext,
  type ToolHost,
  type ToolSpec,
} from "./extension.ts"
import type { AtlassianTokens } from "./auth.ts"
import type { McpTool } from "./mcp-client.ts"

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

const SAMPLE_TOOLS: McpTool[] = [
  { name: "searchJiraIssuesUsingJql", description: "Search", inputSchema: { type: "object" } },
  { name: "getJiraIssue", description: "Get", inputSchema: { type: "object" } },
  { name: "getConfluencePage", description: "Page", inputSchema: { type: "object" } },
]

const FAKE_ACCESS_TOKEN = "atl_TOTALLY_SECRET_ACCESS_TOKEN_0123456789"

function makeTokens(overrides: Partial<AtlassianTokens> = {}): AtlassianTokens {
  return {
    accessToken: FAKE_ACCESS_TOKEN,
    refreshToken: "ref",
    expiresAt: Date.now() + 3_600_000,
    clientId: "cid",
    ...overrides,
  }
}

function makeHost(): { host: ToolHost; registered: ToolSpec[] } {
  const registered: ToolSpec[] = []
  return { registered, host: { registerTool: (t) => void registered.push(t) } }
}

function makeCtx(): { ctx: NotifyContext; notices: Array<{ msg: string; type?: string }> } {
  const notices: Array<{ msg: string; type?: string }> = []
  return { notices, ctx: { ui: { notify: (msg, type) => notices.push({ msg, type }) } } }
}

interface DepRecorder {
  deps: ExtensionDeps
  connectCalls: number
  loginCalls: number
  loadTokenCalls: number
  toolCalls: Array<{ name: string; args: Record<string, unknown> }>
}

function makeDeps(
  overrides: {
    tokens?: AtlassianTokens | null
    tools?: McpTool[]
    connectFails?: boolean
    env?: Record<string, string | undefined>
    defaultCloudId?: string
  } = {},
): DepRecorder {
  const rec: DepRecorder = {
    connectCalls: 0,
    loginCalls: 0,
    loadTokenCalls: 0,
    toolCalls: [],
    deps: {} as ExtensionDeps,
  }

  let stored: AtlassianTokens | null =
    overrides.tokens === undefined ? makeTokens() : overrides.tokens

  const session: AtlassianSessionLike = {
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
    env: overrides.env ?? {},
    getDefaultCloudId: () => overrides.defaultCloudId ?? "",
    loadTokens: () => {
      rec.loadTokenCalls++
      return stored
    },
    async getValidAccessToken() {
      if (!stored) throw new Error("no tokens")
      return { token: stored.accessToken, tokens: stored }
    },
    invalidateCache() {},
    async connect() {
      rec.connectCalls++
      if (overrides.connectFails) throw new Error("fetch failed: ECONNREFUSED")
      return session
    },
    async login() {
      rec.loginCalls++
      stored = makeTokens({ accessToken: "post-login" })
      return stored
    },
  }

  return rec
}

async function activateVia(rec: { registered: ToolSpec[] }) {
  const gateway = rec.registered.find((t) => t.name === "activate_atlassian")
  assert.ok(gateway, "activate_atlassian must be registered before it can be called")
  return gateway.execute("activate-call", {}, undefined)
}

// ---------------------------------------------------------------------------
// AC 7: the atlassian extension defers its surface
// ---------------------------------------------------------------------------

describe("session_start defers everything behind activate_atlassian", () => {
  it("registers exactly one tool, named activate_atlassian", () => {
    const rec = makeDeps()
    const host = makeHost()

    createAtlassianExtension(rec.deps).onSessionStart(host.host, makeCtx().ctx)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_atlassian"])
  })

  it("opens no connection and reads no token file", () => {
    const rec = makeDeps()
    createAtlassianExtension(rec.deps).onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(rec.connectCalls, 0, "no connection at session_start")
    assert.equal(rec.loadTokenCalls, 0, "no token read at session_start")
  })

  it("names the Atlassian capability areas and says calling it reveals the surface", () => {
    const host = makeHost()
    createAtlassianExtension(makeDeps().deps).onSessionStart(host.host, makeCtx().ctx)

    assert.equal(host.registered[0].description, ACTIVATE_ATLASSIAN_DESCRIPTION)
    for (const area of ["Jira", "Confluence", "JQL", "transition"]) {
      assert.ok(
        ACTIVATE_ATLASSIAN_DESCRIPTION.toLowerCase().includes(area.toLowerCase()),
        `description must name the "${area}" capability area`,
      )
    }
    assert.match(ACTIVATE_ATLASSIAN_DESCRIPTION, /registers the full/i)
    assert.ok(ACTIVATE_ATLASSIAN_DESCRIPTION.length < 900)
  })

  it("registers the full surface plus the synthetic tool when called", async () => {
    const rec = makeDeps()
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)

    assert.equal(rec.connectCalls, 1)
    assert.deepEqual(host.registered.map((t) => t.name), [
      "activate_atlassian",
      "searchJiraIssuesUsingJql",
      "getJiraIssue",
      "getConfluencePage",
      "transitionJiraIssueByName",
    ])
    assert.equal(result.isError, undefined)
    // 3 upstream tools + the synthetic transitionJiraIssueByName.
    assert.match(result.content[0].text ?? "", /4 tools are now available/)
    assert.equal(ext.isActive(), true)
  })

  it("a second call registers no duplicates and says it is already active", async () => {
    const rec = makeDeps()
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    await activateVia(host)
    const before = host.registered.length
    const second = await activateVia(host)

    assert.equal(host.registered.length, before)
    assert.equal(rec.connectCalls, 1)
    assert.match(second.content[0].text ?? "", /already active/)
  })

  it("registered tools forward args and return the slimmed payload", async () => {
    const rec = makeDeps()
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)
    await activateVia(host)

    const search = host.registered.find((t) => t.name === "searchJiraIssuesUsingJql")
    assert.ok(search)
    const result = await search.execute("call-1", { jql: "project = PLAT" }, undefined)

    assert.deepEqual(rec.toolCalls, [
      { name: "searchJiraIssuesUsingJql", args: { jql: "project = PLAT" } },
    ])
    assert.equal(result.isError, undefined)
  })
})

// ---------------------------------------------------------------------------
// AC 9 / AC 10: eager roles on m4mac
// ---------------------------------------------------------------------------

describe("eager roles", () => {
  // The nix default is [ "coordinator" ], delivered as
  // ATLASSIAN_MCP_EAGER_ROLES=coordinator via agent.envVars.
  const M4MAC_ENV = { ATLASSIAN_MCP_EAGER_ROLES: "coordinator" }

  it("a coordinator has the full surface without calling activate_atlassian", async () => {
    const rec = makeDeps({ env: M4MAC_ENV })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.deepEqual(host.registered.map((t) => t.name), [
      "activate_atlassian",
      "searchJiraIssuesUsingJql",
      "getJiraIssue",
      "getConfluencePage",
      "transitionJiraIssueByName",
    ])
    assert.equal(ext.isActive(), true)
  })

  it("a worker registers only activate_atlassian", async () => {
    const rec = makeDeps({ env: M4MAC_ENV })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "worker")

    assert.equal(rec.connectCalls, 0)
    assert.equal(rec.loadTokenCalls, 0)
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_atlassian"])
    assert.equal(ext.isActive(), false)
  })

  it("a review agent registers only activate_atlassian", async () => {
    const rec = makeDeps({ env: M4MAC_ENV })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "review-security")

    assert.equal(rec.connectCalls, 0)
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_atlassian"])
  })

  it("the eager check runs at most once per session", async () => {
    const rec = makeDeps({ env: M4MAC_ENV })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    // before_agent_start fires once per TURN.
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 5)
  })

  it("an unset eager-roles list means nobody is eager", async () => {
    const rec = makeDeps({ env: {} })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 0)
  })

  it("an interactive pi with no --agent is never eager", async () => {
    const rec = makeDeps({ env: M4MAC_ENV })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, undefined)

    assert.equal(rec.connectCalls, 0)
  })
})

// ---------------------------------------------------------------------------
// AC 14: a broken provider must not take the session down
// ---------------------------------------------------------------------------

describe("a broken provider never blocks pi", () => {
  it("an unreachable mcp.atlassian.com returns an error result", async () => {
    const rec = makeDeps({ connectFails: true })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /could not reach mcp\.atlassian\.com/)
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_atlassian"])
    assert.equal(ext.isActive(), false)
  })

  it("missing tokens are reported with the action the user must take", async () => {
    const rec = makeDeps({ tokens: null })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /login-atlassian/)
    assert.equal(rec.connectCalls, 0)
  })

  it("an empty tools/list is reported rather than silently succeeding", async () => {
    const rec = makeDeps({ tools: [] })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /returned no tools/)
  })

  it("an eager activation failure notifies and leaves the session running", async () => {
    const rec = makeDeps({
      connectFails: true,
      env: { ATLASSIAN_MCP_EAGER_ROLES: "coordinator" },
    })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, ctx.ctx, "coordinator")

    assert.ok(ctx.notices.some((n) => n.type === "error"))
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_atlassian"])
  })

  it("stays retryable after a failure", async () => {
    let attempt = 0
    const base = makeDeps()
    const ext = createAtlassianExtension({
      ...base.deps,
      async connect(token) {
        attempt++
        if (attempt === 1) throw new Error("transient 503")
        return base.deps.connect(token)
      },
    })
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    assert.equal((await activateVia(host)).isError, true)
    const second = await activateVia(host)

    assert.equal(second.isError, undefined)
    assert.equal(ext.isActive(), true)
  })
})

// ---------------------------------------------------------------------------
// AC 16: no credential on the activation path
// ---------------------------------------------------------------------------

describe("activation path leaks no credential", () => {
  it("the success message names a count, not the token", async () => {
    const rec = makeDeps()
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)
    const text = result.content.map((c) => c.text ?? "").join("\n")

    assert.equal(text.includes(FAKE_ACCESS_TOKEN), false)
  })

  it("a connection failure message carries no token", async () => {
    const rec = makeDeps({ connectFails: true })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await activateVia(host)
    const text = result.content.map((c) => c.text ?? "").join("\n")

    assert.equal(text.includes(FAKE_ACCESS_TOKEN), false)
  })
})

// ---------------------------------------------------------------------------
// /login-atlassian
// ---------------------------------------------------------------------------

describe("login command", () => {
  it("short-circuits when already authenticated, without connecting", async () => {
    const rec = makeDeps()
    const ext = createAtlassianExtension(rec.deps)
    const ctx = makeCtx()

    await ext.onLoginCommand(makeHost().host, ctx.ctx)

    assert.equal(rec.loginCalls, 0)
    assert.equal(rec.connectCalls, 0)
    assert.ok(ctx.notices.some((n) => /already authenticated/.test(n.msg)))
  })

  it("registers through the gateway after a successful login", async () => {
    const rec = makeDeps({ tokens: null })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    await ext.onLoginCommand(host.host, ctx.ctx)

    assert.equal(rec.loginCalls, 1)
    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 5, "gateway + 3 tools + synthetic")
    assert.ok(ctx.notices.some((n) => /4 tools are now available/.test(n.msg)))
  })

  it("a gateway call after login reports already-active, not a duplicate", async () => {
    const rec = makeDeps({ tokens: null })
    const ext = createAtlassianExtension(rec.deps)
    const host = makeHost()

    await ext.onLoginCommand(host.host, makeCtx().ctx)
    const afterLogin = host.registered.length
    const result = await activateVia(host)

    assert.equal(host.registered.length, afterLogin)
    assert.equal(rec.connectCalls, 1)
    assert.match(result.content[0].text ?? "", /already active/)
  })

  it("reports a failed login without throwing", async () => {
    const base = makeDeps({ tokens: null })
    const ext = createAtlassianExtension({
      ...base.deps,
      async login() {
        throw new Error("user cancelled")
      },
    })
    const ctx = makeCtx()

    await ext.onLoginCommand(makeHost().host, ctx.ctx)

    assert.equal(base.connectCalls, 0)
    assert.ok(ctx.notices.some((n) => n.type === "error" && /user cancelled/.test(n.msg)))
  })
})

// ---------------------------------------------------------------------------
// Instance isolation
// ---------------------------------------------------------------------------

describe("createAtlassianExtension", () => {
  it("gives each instance independent state", async () => {
    const a = makeDeps()
    const b = makeDeps()
    const extA = createAtlassianExtension(a.deps)
    const extB = createAtlassianExtension(b.deps)
    const hostA = makeHost()
    const hostB = makeHost()

    extA.onSessionStart(hostA.host, makeCtx().ctx)
    extB.onSessionStart(hostB.host, makeCtx().ctx)
    await activateVia(hostA)

    assert.equal(extA.isActive(), true)
    assert.equal(extB.isActive(), false)
    assert.equal(b.connectCalls, 0)
  })
})
