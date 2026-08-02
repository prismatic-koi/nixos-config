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
import { execFileSync, spawnSync } from "node:child_process"
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

import {
  createActivationGateway,
  isEagerRole,
  parseEagerRoles,
  readAgentRoleFromArgv,
  type GatewayHost,
  type GatewayToolSpec,
} from "./activation.ts"

const HERE = dirname(fileURLToPath(import.meta.url))
const EXTENSIONS_DIR = join(HERE, "..")

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

// ---------------------------------------------------------------------------
// Reading the agent role
//
// REGRESSION GUARD (round-1 review of PR #2568). An earlier revision read the
// role via pi.registerFlag("agent") + pi.getFlag("agent"). pi scopes getFlag to
// the registering extension, so each MCP extension had to register the flag —
// and pi treats the same flag name owned by two different extension PATHS as a
// FATAL conflict, exiting 1 before the session starts. prism always loads
// prism.ts, which owns --agent, so that revision stopped every session on every
// machine from starting. Reading argv has no such coupling.
// ---------------------------------------------------------------------------

describe("readAgentRoleFromArgv", () => {
  const base = ["/usr/bin/node", "/nix/store/x/pi"]

  it("reads the space-separated form prism emits", () => {
    // internal/container/pi_invocation.go appends: "--agent", cfg.AgentRole
    assert.equal(readAgentRoleFromArgv([...base, "--agent", "coordinator"]), "coordinator")
  })

  it("reads the --agent=<role> form too", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--agent=review-goal"]), "review-goal")
  })

  it("finds the flag among other arguments", () => {
    const argv = [...base, "--extension", "/x/prism.ts", "--agent", "worker", "--print", "hi"]
    assert.equal(readAgentRoleFromArgv(argv), "worker")
  })

  it("returns undefined when the flag is absent", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--print", "hi"]), undefined)
  })

  it("does not read the next flag as a role", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--agent", "--print"]), undefined)
  })

  it("returns undefined for a bare trailing --agent", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--agent"]), undefined)
  })

  it("returns undefined for an empty value", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--agent", "   "]), undefined)
    assert.equal(readAgentRoleFromArgv([...base, "--agent="]), undefined)
  })

  it("trims surrounding whitespace", () => {
    assert.equal(readAgentRoleFromArgv([...base, "--agent", " worker "]), "worker")
  })
})

/**
 * Strip `//` line comments and block comments so the source guard below tests
 * CODE, not prose. Each shell carries a comment explaining why it must not
 * call registerFlag, and that comment necessarily names the function.
 */
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[ \t]*\/\/.*$/gm, "")
}

describe("no MCP extension registers the --agent flag", () => {
  // A source-level guard. The failure it prevents is fatal but invisible to
  // ordinary unit tests: pi only detects the conflict when two extension paths
  // are loaded together, which no unit test does. Grepping the shells is cheap
  // and pins the exact line that can regress.
  for (const provider of ["grafana", "notion", "atlassian"]) {
    it(`${provider}/index.ts does not call registerFlag`, () => {
      const path = join(EXTENSIONS_DIR, provider, "index.ts")
      if (!existsSync(path)) return // provider dir absent (nix store layout)
      const src = stripComments(readFileSync(path, "utf8"))
      assert.equal(
        /\bregisterFlag\s*\(/.test(src),
        false,
        `${provider}/index.ts calls registerFlag — prism.ts already owns --agent, ` +
          `and pi exits 1 when two extension paths register the same flag name`,
      )
    })
  }

  it("prism.ts is still the sole owner of --agent", () => {
    // If prism.ts ever stops registering it, readAgentRoleFromArgv keeps
    // working (argv is unchanged) — but this documents the assumption.
    const path = join(EXTENSIONS_DIR, "prism.ts")
    if (!existsSync(path)) return
    const src = stripComments(readFileSync(path, "utf8"))
    assert.match(src, /registerFlag\("agent"/)
  })
})

// The test round-1 review asked for by name: load prism.ts together with one
// MCP extension and assert pi starts. It needs the real pi binary, so it
// self-skips when pi is not on PATH rather than failing a runner without pi.
describe("pi starts with prism.ts and an MCP extension loaded together", () => {
  const piPath = (() => {
    try {
      return execFileSync("sh", ["-c", "command -v pi"], { encoding: "utf8" }).trim()
    } catch {
      return ""
    }
  })()

  it("reports no extension flag conflict", { skip: piPath === "" }, () => {
    const prismTs = join(EXTENSIONS_DIR, "prism.ts")
    const grafanaTs = join(EXTENSIONS_DIR, "grafana", "index.ts")
    if (!existsSync(prismTs) || !existsSync(grafanaTs)) return

    const dir = mkdtempSync(join(tmpdir(), "pi-flag-conflict-"))
    try {
      writeFileSync(
        join(dir, "settings.json"),
        JSON.stringify({
          defaultProjectTrust: "always",
          quietStartup: true,
          extensions: [prismTs, grafanaTs],
        }),
      )

      // pi evaluates extension-conflict diagnostics and exits 1 BEFORE it
      // creates an agent session, so this never reaches the network. A later
      // failure (no auth, offline) is irrelevant: the assertion is only about
      // the conflict diagnostic.
      const res = spawnSync(piPath, ["--print", "ok"], {
        encoding: "utf8",
        timeout: 90_000,
        env: {
          ...process.env,
          PI_CODING_AGENT_DIR: dir,
          PI_OFFLINE: "1",
          PI_SKIP_VERSION_CHECK: "1",
        },
      })
      const output = `${res.stdout ?? ""}${res.stderr ?? ""}`
      assert.equal(
        /conflicts with/.test(output),
        false,
        `pi reported an extension conflict:\n${output.slice(0, 600)}`,
      )
      assert.equal(
        /Failed to load extension/.test(output),
        false,
        `pi failed to load an extension:\n${output.slice(0, 600)}`,
      )
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
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
