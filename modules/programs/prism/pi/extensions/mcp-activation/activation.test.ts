// Unit tests for the shared activate_<family> gateway (issue #2532).
//
// Run with: tsx --test activation.test.ts (from this directory)
//
// Revert-and-watch-fail pairs:
//   * Delete the `if (active)` short-circuit in `run()` → "a second call
//     registers nothing and says the family is already active" fails.
//   * Delete the `if (inFlight) return inFlight` guard → "concurrent calls
//     share one activation" fails.
//   * Delete the `if (registered) return` guard in `register()` → "the
//     gateway tool is registered at most once" fails.
//   * Make `perform()` rethrow instead of returning a failed outcome → "a
//     failing provider returns an error result rather than throwing" fails.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import {
  createActivationGateway,
  isEagerRole,
  parseEagerRoles,
  type GatewayHost,
  type GatewayToolSpec,
} from "./activation.ts"

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

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

function makeGateway(
  activate: () => Promise<number>,
  overrides: { family?: string; label?: string; description?: string } = {},
) {
  return createActivationGateway({
    family: overrides.family ?? "grafana",
    label: overrides.label ?? "Grafana",
    description: overrides.description ?? "Activate the Grafana MCP tool family.",
    wrapSchema: (schema) => schema,
    activate,
  })
}

// ---------------------------------------------------------------------------
// Eager-role parsing
// ---------------------------------------------------------------------------

describe("parseEagerRoles", () => {
  it("returns an empty list for undefined and empty input", () => {
    assert.deepEqual(parseEagerRoles(undefined), [])
    assert.deepEqual(parseEagerRoles(""), [])
    assert.deepEqual(parseEagerRoles("   "), [])
  })

  it("splits on colons, the shape the nix side emits", () => {
    assert.deepEqual(parseEagerRoles("coordinator:worker"), ["coordinator", "worker"])
  })

  it("splits on commas too, and trims each entry", () => {
    assert.deepEqual(parseEagerRoles(" coordinator , worker "), ["coordinator", "worker"])
  })

  it("drops empty segments from a trailing or doubled separator", () => {
    assert.deepEqual(parseEagerRoles("coordinator::"), ["coordinator"])
  })
})

describe("isEagerRole", () => {
  it("is true only for a listed role", () => {
    assert.equal(isEagerRole("coordinator", "coordinator"), true)
    assert.equal(isEagerRole("worker", "coordinator"), false)
  })

  it("is false when no role is bound (interactive pi with no --agent)", () => {
    assert.equal(isEagerRole(undefined, "coordinator"), false)
    assert.equal(isEagerRole("", "coordinator"), false)
  })

  it("is false when the option is unset — the cheap default", () => {
    assert.equal(isEagerRole("coordinator", undefined), false)
    assert.equal(isEagerRole("coordinator", ""), false)
  })

  it("matches exactly — no prefix or substring match", () => {
    assert.equal(isEagerRole("review-goal", "review"), false)
    assert.equal(isEagerRole("coordinator-x", "coordinator"), false)
  })
})

// ---------------------------------------------------------------------------
// Registration surface
// ---------------------------------------------------------------------------

describe("gateway registration", () => {
  it("registers exactly one tool, named activate_<family>", () => {
    const gw = makeGateway(async () => 65)
    const host = makeHost()

    gw.register(host.host)

    assert.deepEqual(host.registered.map((t) => t.name), ["activate_grafana"])
    assert.equal(gw.toolName, "activate_grafana")
  })

  it("the gateway tool is registered at most once", () => {
    const gw = makeGateway(async () => 65)
    const host = makeHost()

    gw.register(host.host)
    gw.register(host.host)

    assert.equal(
      host.registered.length,
      1,
      "a repeat registration costs a full prompt-cache write for no gain",
    )
  })

  it("registering the gateway does no provider work", async () => {
    let activateCalls = 0
    const gw = makeGateway(async () => {
      activateCalls++
      return 65
    })

    gw.register(makeHost().host)

    assert.equal(activateCalls, 0, "no config read, no connect, no child process")
    assert.equal(gw.isActive(), false)
    assert.equal(gw.count(), 0)
  })

  it("takes no arguments", () => {
    const gw = makeGateway(async () => 65)
    const host = makeHost()
    gw.register(host.host)

    assert.deepEqual(host.registered[0].parameters, {
      type: "object",
      properties: {},
      additionalProperties: false,
    })
  })
})

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

describe("activation", () => {
  it("registers the family and reports the count", async () => {
    const gw = makeGateway(async () => 65)
    const host = makeHost()
    gw.register(host.host)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, undefined)
    assert.match(result.content[0].text ?? "", /65 tools are now available/)
    assert.equal(gw.isActive(), true)
    assert.equal(gw.count(), 65)
  })

  it("a second call registers nothing and says the family is already active", async () => {
    let activateCalls = 0
    const gw = makeGateway(async () => {
      activateCalls++
      return 65
    })
    const host = makeHost()
    gw.register(host.host)

    await host.registered[0].execute("call-1", {}, undefined)
    const second = await host.registered[0].execute("call-2", {}, undefined)

    assert.equal(activateCalls, 1, "no duplicate registration of the family")
    assert.equal(second.isError, undefined)
    assert.match(second.content[0].text ?? "", /already active/)
    assert.match(second.content[0].text ?? "", /No tools were registered/)
  })

  it("concurrent calls share one activation", async () => {
    let activateCalls = 0
    let release: (() => void) | undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const gw = makeGateway(async () => {
      activateCalls++
      await gate
      return 31
    })
    const host = makeHost()
    gw.register(host.host)

    const a = host.registered[0].execute("call-1", {}, undefined)
    const b = host.registered[0].execute("call-2", {}, undefined)
    release!()
    const [ra, rb] = await Promise.all([a, b])

    assert.equal(activateCalls, 1, "one child process, not two")
    assert.match(ra.content[0].text ?? "", /31 tools are now available/)
    assert.match(rb.content[0].text ?? "", /31 tools are now available/)
  })

  it("run() and the tool call agree — the eager path cannot diverge", async () => {
    let activateCalls = 0
    const gw = makeGateway(async () => {
      activateCalls++
      return 65
    })
    const host = makeHost()
    gw.register(host.host)

    // The eager path: before_agent_start calls run() directly.
    const eager = await gw.run()
    assert.equal(eager.status, "activated")

    // A model that then calls the tool anyway gets the already-active answer.
    const viaTool = await host.registered[0].execute("call-1", {}, undefined)
    assert.equal(activateCalls, 1)
    assert.match(viaTool.content[0].text ?? "", /already active/)
  })
})

// ---------------------------------------------------------------------------
// Failure handling — the pi session must survive a broken provider
// ---------------------------------------------------------------------------

describe("failure handling", () => {
  it("a failing provider returns an error result rather than throwing", async () => {
    const gw = makeGateway(async () => {
      throw new Error("spawn mcp-grafana ENOENT")
    })
    const host = makeHost()
    gw.register(host.host)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /activation failed/)
    assert.match(result.content[0].text ?? "", /ENOENT/)
    assert.equal(gw.isActive(), false)
  })

  it("a non-Error rejection is still reported cleanly", async () => {
    const gw = makeGateway(async () => {
      throw "plain string blew up"
    })
    const host = makeHost()
    gw.register(host.host)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /plain string blew up/)
  })

  it("an empty tools/list is a failure, not a silent success", async () => {
    const gw = makeGateway(async () => 0)
    const host = makeHost()
    gw.register(host.host)

    const result = await host.registered[0].execute("call-1", {}, undefined)

    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /returned no tools/)
    assert.equal(gw.isActive(), false)
  })

  it("stays retryable after a failure", async () => {
    let attempt = 0
    const gw = makeGateway(async () => {
      attempt++
      if (attempt === 1) throw new Error("transient network fault")
      return 31
    })
    const host = makeHost()
    gw.register(host.host)

    const first = await host.registered[0].execute("call-1", {}, undefined)
    assert.equal(first.isError, true)

    const second = await host.registered[0].execute("call-2", {}, undefined)
    assert.equal(second.isError, undefined)
    assert.match(second.content[0].text ?? "", /31 tools are now available/)
    assert.equal(gw.isActive(), true)
  })

  it("gives each gateway instance independent state", async () => {
    const a = makeGateway(async () => 65)
    const b = makeGateway(async () => 31, { family: "atlassian", label: "Atlassian" })

    await a.run()

    assert.equal(a.isActive(), true)
    assert.equal(b.isActive(), false)
    assert.equal(b.toolName, "activate_atlassian")
  })
})
