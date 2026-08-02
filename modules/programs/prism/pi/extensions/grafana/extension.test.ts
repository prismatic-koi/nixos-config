// Unit tests for grafana/extension.ts — the pi-agnostic extension core.
//
// Run with: tsx --test extension.test.ts (from this directory)
//
// These cover the deferral contract from issue #2532. The expensive facts —
// "the sops bundle is not read" and "the mcp-grafana child does not start" —
// are asserted by counting calls to the injected `loadBundle` and `connect`
// dependencies, which is the only honest way to prove a side effect did NOT
// happen.
//
// Revert-and-watch-fail pairs:
//   * Move the loadBundle/connect calls back into `onSessionStart` →
//     "session_start reads no config bundle" and "session_start starts no
//     child process" fail.
//   * Delete the `isConfigured` guard in `onSessionStart` → "registers
//     nothing when GRAFANA_MCP_CONFIG_PATH is unset" fails.
//   * Delete the `eagerChecked` guard → "the eager check runs at most once
//     per session" fails.
//   * Make `isEagerRole` return true unconditionally → "a non-eager role does
//     not activate at before_agent_start" fails.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import {
  ACTIVATE_GRAFANA_DESCRIPTION,
  createGrafanaExtension,
  type GrafanaExtensionDeps,
  type GrafanaSessionLike,
  type NotifyContext,
} from "./extension.ts"
import type { GrafanaBundle } from "./config-loader.ts"
import type { GatewayHost, GatewayToolSpec } from "../mcp-activation/activation.ts"
import type { McpTool } from "./mcp-client.ts"

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

const SAMPLE_TOOLS: McpTool[] = [
  { name: "query_prometheus", description: "Query", inputSchema: { type: "object" } },
  { name: "search_dashboards", description: "Search", inputSchema: { type: "object" } },
  { name: "list_incidents", description: "List", inputSchema: { type: "object" } },
]

const CONFIGURED_ENV: Record<string, string | undefined> = {
  GRAFANA_MCP_CONFIG_PATH: "/run/secrets/grafana_config_home",
  PI_GRAFANA_MCP_BIN: "/nix/store/xxxx-mcp-grafana/bin/mcp-grafana",
}

// A live credential, so the security assertions have something to look for.
const FAKE_TOKEN = "glsa_TOTALLY_SECRET_TOKEN_0123456789"

function makeHost(): { host: GatewayHost; registered: GatewayToolSpec[] } {
  const registered: GatewayToolSpec[] = []
  return {
    registered,
    host: {
      registerTool(tool) {
        registered.push(tool)
      },
    },
  }
}

function makeCtx(): { ctx: NotifyContext; notices: Array<{ msg: string; type?: string }> } {
  const notices: Array<{ msg: string; type?: string }> = []
  return { notices, ctx: { ui: { notify: (msg, type) => notices.push({ msg, type }) } } }
}

interface DepRecorder {
  deps: GrafanaExtensionDeps
  loadCalls: number
  connectCalls: number
  closeCalls: number
  toolCalls: Array<{ name: string; args: Record<string, unknown> }>
}

function makeDeps(
  overrides: {
    env?: Record<string, string | undefined>
    tools?: McpTool[]
    loadThrows?: Error
    connectThrows?: Error
    listToolsThrows?: Error
  } = {},
): DepRecorder {
  const rec: DepRecorder = {
    loadCalls: 0,
    connectCalls: 0,
    closeCalls: 0,
    toolCalls: [],
    deps: {} as GrafanaExtensionDeps,
  }

  const session: GrafanaSessionLike = {
    async listTools() {
      if (overrides.listToolsThrows) throw overrides.listToolsThrows
      return overrides.tools ?? SAMPLE_TOOLS
    },
    async callTool(name, args) {
      rec.toolCalls.push({ name, args })
      return { content: [{ type: "text", text: `{"ok":"${name}"}` }] }
    },
    close() {
      rec.closeCalls++
    },
  }

  rec.deps = {
    wrapSchema: (schema) => schema,
    env: overrides.env ?? CONFIGURED_ENV,
    loadBundle(): GrafanaBundle {
      rec.loadCalls++
      if (overrides.loadThrows) throw overrides.loadThrows
      return { url: "https://grafana.example", apiKey: FAKE_TOKEN, extraEnv: {} }
    },
    async connect() {
      rec.connectCalls++
      if (overrides.connectThrows) throw overrides.connectThrows
      return session
    },
  }

  return rec
}

// ---------------------------------------------------------------------------
// AC 1 / 5 / 6: session_start registers one tool and does nothing expensive
// ---------------------------------------------------------------------------

describe("session_start defers everything behind activate_grafana", () => {
  it("registers exactly one tool, named activate_grafana", () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_grafana"])
  })

  it("reads no config bundle", () => {
    const rec = makeDeps()
    createGrafanaExtension(rec.deps).onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(rec.loadCalls, 0, "the sops bundle must not be read at session_start")
  })

  it("starts no mcp-grafana child process", () => {
    const rec = makeDeps()
    createGrafanaExtension(rec.deps).onSessionStart(makeHost().host, makeCtx().ctx)

    assert.equal(rec.connectCalls, 0, "the child must not spawn at session_start")
  })

  it("names the Grafana capability areas and says calling it reveals the surface", () => {
    const rec = makeDeps()
    const host = makeHost()
    createGrafanaExtension(rec.deps).onSessionStart(host.host, makeCtx().ctx)

    const description = host.registered[0].description
    assert.equal(description, ACTIVATE_GRAFANA_DESCRIPTION)

    // Capability areas the agent has to be able to recognise.
    for (const area of [
      "dashboard",
      "datasource",
      "Prometheus",
      "Loki",
      "Pyroscope",
      "alert",
      "OnCall",
      "incident",
      "Sift",
      "annotation",
    ]) {
      assert.ok(
        description.toLowerCase().includes(area.toLowerCase()),
        `description must name the "${area}" capability area`,
      )
    }

    assert.match(description, /registers the full/i)
    assert.match(description, /number of tools available/i)
  })

  it("stays small — it is paid for by every session", () => {
    // A rough budget guard, not a token count. 900 characters is about 220
    // tokens; the family it replaces was about 26400.
    assert.ok(
      ACTIVATE_GRAFANA_DESCRIPTION.length < 900,
      `description is ${ACTIVATE_GRAFANA_DESCRIPTION.length} characters — keep it under 900`,
    )
  })
})

// ---------------------------------------------------------------------------
// AC 15: an unconfigured provider registers no activate tool at all
// ---------------------------------------------------------------------------

describe("unconfigured provider", () => {
  it("registers nothing when GRAFANA_MCP_CONFIG_PATH is unset", () => {
    const rec = makeDeps({ env: { PI_GRAFANA_MCP_BIN: "/nix/store/x/bin/mcp-grafana" } })
    const host = makeHost()
    const ctx = makeCtx()

    createGrafanaExtension(rec.deps).onSessionStart(host.host, ctx.ctx)

    assert.deepEqual(host.registered, [], "not even activate_grafana")
    assert.deepEqual(ctx.notices, [], "a silent skip — grafana is simply not enabled here")
  })

  it("registers nothing when PI_GRAFANA_MCP_BIN is unset, and says why", () => {
    const rec = makeDeps({
      env: { GRAFANA_MCP_CONFIG_PATH: "/run/secrets/grafana_config_home" },
    })
    const host = makeHost()
    const ctx = makeCtx()

    createGrafanaExtension(rec.deps).onSessionStart(host.host, ctx.ctx)

    assert.deepEqual(host.registered, [])
    assert.ok(ctx.notices.some((n) => /PI_GRAFANA_MCP_BIN/.test(n.msg)))
  })

  it("does not activate eagerly when unconfigured, whatever the role", async () => {
    const rec = makeDeps({
      env: { GRAFANA_MCP_EAGER_ROLES: "coordinator" },
    })
    const host = makeHost()
    const ext = createGrafanaExtension(rec.deps)

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.deepEqual(host.registered, [])
    assert.equal(rec.loadCalls, 0)
    assert.equal(rec.connectCalls, 0)
  })
})

// ---------------------------------------------------------------------------
// AC 3 / 4: calling activate_grafana registers the full surface
// ---------------------------------------------------------------------------

describe("activate_grafana", () => {
  it("registers the full surface and reports the count", async () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(rec.loadCalls, 1, "now the bundle is read")
    assert.equal(rec.connectCalls, 1, "now the child starts")
    assert.deepEqual(host.registered.map((t) => t.name), [
      "activate_grafana",
      "query_prometheus",
      "search_dashboards",
      "list_incidents",
    ])
    assert.equal(result.isError, undefined)
    assert.match(result.content[0].text ?? "", /3 tools are now available/)
    assert.equal(ext.isActive(), true)
  })

  it("passes the bundle through to the connector", async () => {
    const rec = makeDeps()
    let seen: Record<string, unknown> | undefined
    const ext = createGrafanaExtension({
      ...rec.deps,
      async connect(opts) {
        seen = opts as unknown as Record<string, unknown>
        return {
          async listTools() {
            return SAMPLE_TOOLS
          },
          async callTool() {
            return { content: [] }
          },
          close() {},
        }
      },
    })
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)
    await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(seen?.binPath, CONFIGURED_ENV.PI_GRAFANA_MCP_BIN)
    assert.equal(seen?.grafanaUrl, "https://grafana.example")
    assert.equal(seen?.grafanaApiKey, FAKE_TOKEN)
  })

  it("registered tools forward args and return the payload", async () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)
    await host.registered[0].execute("call-1", {}, undefined)

    const query = host.registered.find((t) => t.name === "query_prometheus")
    assert.ok(query)
    const result = await query.execute("call-2", { expr: "up" }, undefined)

    assert.deepEqual(rec.toolCalls, [{ name: "query_prometheus", args: { expr: "up" } }])
    assert.equal(result.content[0].text, '{"ok":"query_prometheus"}')
  })

  it("a second call registers no duplicates and says it is already active", async () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    await host.registered[0].execute("call-1", {}, undefined)
    const before = host.registered.length
    const second = await host.registered[0].execute("call-2", {}, undefined)

    assert.equal(host.registered.length, before, "no duplicate tool registration")
    assert.equal(rec.connectCalls, 1, "no second child process")
    assert.match(second.content[0].text ?? "", /already active/)
    assert.equal(second.isError, undefined)
  })
})

// ---------------------------------------------------------------------------
// AC 14: a broken provider must not take the session down
// ---------------------------------------------------------------------------

describe("failure inside activation", () => {
  it("a failed spawn returns an error result rather than throwing", async () => {
    const rec = makeDeps({ connectThrows: new Error("spawn ENOENT") })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /failed to start mcp-grafana/)
    assert.equal(host.registered.length, 1, "no partial surface")
    assert.equal(ext.isActive(), false)
  })

  it("a failed handshake / tools-list closes the child and reports the error", async () => {
    const rec = makeDeps({ listToolsThrows: new Error("handshake timed out") })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /tools\/list failed/)
    assert.equal(rec.closeCalls, 1, "the child must be torn down, not leaked")
  })

  it("an empty tools/list is reported and the child is closed", async () => {
    const rec = makeDeps({ tools: [] })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /returned no tools/)
    assert.equal(rec.closeCalls, 1)
  })

  it("a bad config bundle is reported without taking the session down", async () => {
    const { GrafanaConfigError } = await import("./config-loader.ts")
    const rec = makeDeps({
      loadThrows: new GrafanaConfigError(
        "config at /run/secrets/grafana_config_home: missing GRAFANA_URL",
      ),
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /missing GRAFANA_URL/)
    assert.equal(rec.connectCalls, 0, "a bad bundle must not reach the spawn")
  })
})

// ---------------------------------------------------------------------------
// AC 16: no credential on the activation path
// ---------------------------------------------------------------------------

describe("activation path leaks no credential", () => {
  it("the success message names a count, not the bundle", async () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)
    const text = result.content.map((c) => c.text ?? "").join("\n")

    assert.equal(text.includes(FAKE_TOKEN), false, "the token must never reach a tool result")
  })

  it("a spawn failure message carries no credential", async () => {
    // The realistic shape: a child that dies and reports its own argv/env.
    // The extension must forward the exception message, and the exception
    // message must be the only thing it forwards.
    const rec = makeDeps({ connectThrows: new Error("mcp-grafana exited with code 1") })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    ext.onSessionStart(host.host, makeCtx().ctx)

    const result = await host.registered[0].execute("call-1", {}, undefined)
    const text = result.content.map((c) => c.text ?? "").join("\n")

    assert.equal(text.includes(FAKE_TOKEN), false)
  })
})

// ---------------------------------------------------------------------------
// AC 8: eager roles
// ---------------------------------------------------------------------------

describe("eager roles", () => {
  it("a listed role activates at before_agent_start with no tool call", async () => {
    const rec = makeDeps({
      env: { ...CONFIGURED_ENV, GRAFANA_MCP_EAGER_ROLES: "coordinator" },
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 4, "activate_grafana plus the 3 sample tools")
    assert.equal(ext.isActive(), true)
  })

  it("a non-eager role does not activate at before_agent_start", async () => {
    const rec = makeDeps({
      env: { ...CONFIGURED_ENV, GRAFANA_MCP_EAGER_ROLES: "coordinator" },
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "worker")

    assert.equal(rec.loadCalls, 0)
    assert.equal(rec.connectCalls, 0)
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_grafana"])
  })

  it("no eager roles configured means nobody is eager", async () => {
    const rec = makeDeps()
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 0)
  })

  it("the eager check runs at most once per session", async () => {
    const rec = makeDeps({
      env: { ...CONFIGURED_ENV, GRAFANA_MCP_EAGER_ROLES: "coordinator" },
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    // before_agent_start fires once per TURN.
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, "coordinator")

    assert.equal(rec.connectCalls, 1)
    assert.equal(host.registered.length, 4)
  })

  it("an eager activation failure notifies and leaves the session running", async () => {
    const rec = makeDeps({
      env: { ...CONFIGURED_ENV, GRAFANA_MCP_EAGER_ROLES: "coordinator" },
      connectThrows: new Error("spawn ENOENT"),
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()
    const ctx = makeCtx()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, ctx.ctx, "coordinator")

    assert.ok(ctx.notices.some((n) => n.type === "error" && /ENOENT/.test(n.msg)))
    assert.deepEqual(host.registered.map((t) => t.name), ["activate_grafana"])
  })

  it("a session with no --agent flag is never eager", async () => {
    const rec = makeDeps({
      env: { ...CONFIGURED_ENV, GRAFANA_MCP_EAGER_ROLES: "coordinator" },
    })
    const ext = createGrafanaExtension(rec.deps)
    const host = makeHost()

    ext.onSessionStart(host.host, makeCtx().ctx)
    await ext.onBeforeAgentStart(host.host, makeCtx().ctx, undefined)

    assert.equal(rec.connectCalls, 0)
  })
})

// ---------------------------------------------------------------------------
// Instance isolation
// ---------------------------------------------------------------------------

describe("createGrafanaExtension", () => {
  it("gives each instance independent state", async () => {
    const a = makeDeps()
    const b = makeDeps()
    const extA = createGrafanaExtension(a.deps)
    const extB = createGrafanaExtension(b.deps)
    const hostA = makeHost()
    const hostB = makeHost()

    extA.onSessionStart(hostA.host, makeCtx().ctx)
    extB.onSessionStart(hostB.host, makeCtx().ctx)
    await hostA.registered[0].execute("call-1", {}, undefined)

    assert.equal(extA.isActive(), true)
    assert.equal(extB.isActive(), false)
    assert.equal(b.connectCalls, 0)
  })
})
