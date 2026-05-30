// Regression test for issue #2064 — role system-prompt injection over the
// pi.registerFlag("agent") path.
//
// History. The pre-#2064 design sourced the agent role from the async
// hello_ack handshake (sidecar → extension). On a fresh bwrap session the
// handshake races BEHIND the agent-start hook: pi fires before_agent_start
// inline on the agent side, while the hello_ack frame travels over a Unix
// socket and arrives a moment later. The first turn therefore lost the role
// prompt every time. Multi-turn sessions resolved correctly on turn 2+, but
// the user-visible pi-session export — captured at the first turn — showed
// pi's default system prompt with no role append.
//
// The fix sources the role from pi.getFlag("agent") instead. pi binds
// extension-registered flags during applyExtensionFlagValues (see pi 0.77
// dist/core/agent-session-services.js) which runs after resourceLoader.reload()
// completes — i.e. after every extension factory has returned — and BEFORE
// the AgentSession is constructed. pi.getFlag("agent") therefore returns
// synchronously on the very first before_agent_start fire, with no
// dependency on any async sidecar handshake.
//
// This test exercises the end-to-end behaviour the user actually cares
// about: "a worker session's first-turn system prompt contains the contents
// of worker.md". It fails on main pre-#2064 because the handshake gate in
// resolveRolePromptForTurn forces undefined on the first call. It passes on
// the fix because the flag-sourced role is available synchronously.
//
// Run with: tsx --test prism.role-prompt.test.ts
//        or node --test --import tsx prism.role-prompt.test.ts

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"

import prismExtension from "./prism.ts"

// A minimal pi mock with the registerFlag / getFlag surface, plus an event
// dispatcher so the test can fire before_agent_start and inspect the result.
// Modelled on makeMockPI1554 in prism.test.ts but kept local here so this
// file does not collide with the in-flight handle-leak fixes there (#2060
// has landed but route-around discipline keeps the two test surfaces tidy).
interface MockPI {
  on: (event: string, handler: (...args: unknown[]) => unknown) => void
  registerFlag: (
    name: string,
    options: { description?: string; type: "boolean" | "string"; default?: boolean | string },
  ) => void
  getFlag: (name: string) => string | boolean | undefined
  sendUserMessage: (...args: unknown[]) => void
  setModel: (...args: unknown[]) => void
  setThinkingLevel: (...args: unknown[]) => void
  registerProvider: (...args: unknown[]) => void
  setActiveTools: (...args: unknown[]) => void
}

interface MockHarness {
  pi: MockPI
  trigger: (event: string, arg?: unknown, ctx?: unknown) => Promise<unknown>
  registeredFlags: Map<string, { type: "boolean" | "string"; description?: string }>
}

function makeMockPI(flagValues: Record<string, string> = {}): MockHarness {
  const handlers: Record<string, ((...args: unknown[]) => unknown)[]> = {}
  const registeredFlags = new Map<string, { type: "boolean" | "string"; description?: string }>()
  const bound = new Map<string, string | boolean>(Object.entries(flagValues))
  const pi: MockPI = {
    on: (event, handler) => {
      if (!handlers[event]) handlers[event] = []
      handlers[event].push(handler)
    },
    registerFlag: (name, options) => {
      registeredFlags.set(name, { type: options.type, description: options.description })
    },
    getFlag: (name) => bound.get(name),
    sendUserMessage: () => {},
    setModel: () => {},
    setThinkingLevel: () => {},
    registerProvider: () => {},
    setActiveTools: () => {},
  }
  // Returns the first handler's return value so before_agent_start callers
  // can inspect the { systemPrompt } override directly. Real pi chains
  // multiple handlers' results; for this regression test the prism extension
  // is the only handler registered so the single-return shape is correct.
  const trigger = async (event: string, arg: unknown = {}, ctx: unknown = {}): Promise<unknown> => {
    let last: unknown
    for (const h of handlers[event] ?? []) {
      last = await h(arg, ctx)
    }
    return last
  }
  return { pi, trigger, registeredFlags }
}

// Stage a temporary agents directory with a known-token role file. The
// extension resolves $XDG_CONFIG_HOME/prism/agents/<role>.md (see
// prismAgentRolePath in prism.ts), so pointing XDG_CONFIG_HOME at the
// scratch root gives us an isolated agents dir without touching the
// real ~/.config/prism/agents/.
function stageAgentsDir(): {
  dir: string
  workerToken: string
  coordinatorToken: string
  cleanup: () => void
} {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-2064-"))
  const agents = path.join(dir, "prism", "agents")
  fs.mkdirSync(agents, { recursive: true })
  const workerToken = "ROLE_PROMPT_TOKEN_WORKER_2064"
  const coordinatorToken = "ROLE_PROMPT_TOKEN_COORDINATOR_2064"
  fs.writeFileSync(
    path.join(agents, "worker.md"),
    "# Worker agent\n\n" + workerToken + "\n\nimplements features, fixes bugs, opens PRs.",
  )
  fs.writeFileSync(
    path.join(agents, "coordinator.md"),
    "# Coordinator agent\n\n" + coordinatorToken + "\n\nspawns workers, reviews PRs.",
  )
  return {
    dir,
    workerToken,
    coordinatorToken,
    cleanup: () => fs.rmSync(dir, { recursive: true, force: true }),
  }
}

// Save/restore the env vars the extension factory reads. Each test that
// wires the extension MUST call savedEnv() before and restoreEnv() in a
// finally — otherwise concurrent tests in the same process see leaks.
function savedEnv() {
  return {
    PRISM_SESSION_NAME: process.env.PRISM_SESSION_NAME,
    PRISM_HARNESS_PIPE: process.env.PRISM_HARNESS_PIPE,
    XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME,
  }
}
function restoreEnv(saved: ReturnType<typeof savedEnv>) {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = v
  }
}

// shouldActivate requires PRISM_SESSION_NAME. PRISM_HARNESS_PIPE must be
// set to something parseable but unreachable so the connect attempt fails
// silently — we never wait for the handshake, so it does not matter that
// no sidecar is listening.
function setExtensionEnv(xdgConfigHome: string): void {
  process.env.PRISM_SESSION_NAME = "test@regression-2064"
  // tcp:// to a free port that nothing is listening on: connect() schedules
  // its first ECONNREFUSED retry and we tear down before the budget is hit.
  process.env.PRISM_HARNESS_PIPE = "tcp://127.0.0.1:1"
  process.env.XDG_CONFIG_HOME = xdgConfigHome
}

describe("issue #2064: first-turn role prompt is injected when --agent is set", () => {
  it(
    "worker session: before_agent_start returns { systemPrompt } containing worker.md on the very first call",
    async () => {
      const stage = stageAgentsDir()
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        // Simulate `pi --agent worker --extension prism.ts`: pi's CLI parses
        // --agent into unknownFlags, then applyExtensionFlagValues binds it
        // into runtime.flagValues once the extension's pi.registerFlag has
        // run. We mirror that by pre-populating the mock pi's bound flags.
        const { pi, trigger, registeredFlags } = makeMockPI({ agent: "worker" })

        // Factory entry: runs pi.registerFlag("agent", ...) and binds the
        // before_agent_start handler.
        prismExtension(pi as never)

        // The extension MUST register --agent so pi can resolve the unknown
        // flag against an extension-owned one (without this, the same flag
        // becomes pi's "Unknown option" diagnostic).
        assert.ok(
          registeredFlags.has("agent"),
          "prism extension must register --agent via pi.registerFlag",
        )
        assert.equal(registeredFlags.get("agent")?.type, "string")

        // Fire before_agent_start with a representative base system prompt.
        const baseSystemPrompt =
          "You are an expert coding assistant. <project_context>repo notes</project_context>"
        const result = (await trigger(
          "before_agent_start",
          { systemPrompt: baseSystemPrompt },
          {},
        )) as { systemPrompt?: string } | undefined

        // The fix: the handler MUST return a non-undefined { systemPrompt }
        // on the first turn (pre-fix returned undefined because the
        // handshake gate forced it).
        assert.ok(result, "handler must return an override on the first turn")
        assert.ok(typeof result.systemPrompt === "string", "override must carry systemPrompt")

        // The composed prompt MUST contain both the base AND the role token
        // — with exactly one blank line between them (composeRoleSystemPrompt
        // contract).
        assert.ok(
          result.systemPrompt!.includes(baseSystemPrompt),
          "composed prompt must preserve the base system prompt",
        )
        assert.ok(
          result.systemPrompt!.includes(stage.workerToken),
          `composed prompt must contain worker.md token; got: ${result.systemPrompt!.slice(0, 200)}...`,
        )
        // Exactly one blank line as separator.
        assert.ok(
          result.systemPrompt!.includes("</project_context>\n\n# Worker agent"),
          "separator between base and role must be exactly one blank line",
        )
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )

  it(
    "coordinator session: before_agent_start returns coordinator.md on the very first call",
    async () => {
      const stage = stageAgentsDir()
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        const { pi, trigger } = makeMockPI({ agent: "coordinator" })
        prismExtension(pi as never)
        const result = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE" },
          {},
        )) as { systemPrompt?: string } | undefined
        assert.ok(result?.systemPrompt, "coordinator must get an override on first turn")
        assert.ok(
          result!.systemPrompt!.includes(stage.coordinatorToken),
          "override must contain coordinator.md token",
        )
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )

  it("review-* session: before_agent_start composes the matching review-<n>.md", async () => {
    const stage = stageAgentsDir()
    // Add a review-goal.md so the matching review case has a file to read.
    const reviewToken = "ROLE_PROMPT_TOKEN_REVIEW_GOAL_2064"
    fs.writeFileSync(
      path.join(stage.dir, "prism", "agents", "review-goal.md"),
      "# Review goal\n\n" + reviewToken + "\n\nReviews PRs against their stated goal.",
    )
    const saved = savedEnv()
    setExtensionEnv(stage.dir)
    try {
      const { pi, trigger } = makeMockPI({ agent: "review-goal" })
      prismExtension(pi as never)
      const result = (await trigger(
        "before_agent_start",
        { systemPrompt: "BASE" },
        {},
      )) as { systemPrompt?: string } | undefined
      assert.ok(result?.systemPrompt, "review-goal must get an override on first turn")
      assert.ok(
        result!.systemPrompt!.includes(reviewToken),
        "override must contain review-goal.md token",
      )
    } finally {
      restoreEnv(saved)
      stage.cleanup()
    }
  })

  it(
    "empty --agent: before_agent_start returns undefined (host-mode launch without --agent — graceful no-op)",
    async () => {
      const stage = stageAgentsDir()
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        // No agent flag bound — pi.getFlag("agent") returns undefined.
        const { pi, trigger } = makeMockPI({})
        prismExtension(pi as never)
        const result = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE" },
          {},
        )) as { systemPrompt?: string } | undefined
        // No override: pi keeps its base system prompt unchanged. This is
        // the missing-role edge-case AC from the issue body.
        assert.equal(result, undefined)
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )

  it(
    "unknown role (no matching .md file): before_agent_start returns undefined (graceful no-op)",
    async () => {
      const stage = stageAgentsDir()
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        // A role whose file does not exist in the staged dir.
        const { pi, trigger } = makeMockPI({ agent: "unknown-role-xyz" })
        prismExtension(pi as never)
        const result = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE" },
          {},
        )) as { systemPrompt?: string } | undefined
        assert.equal(result, undefined, "missing role file must be a graceful no-op")
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )

  it(
    "path-traversal in --agent: before_agent_start returns undefined (security AC, defence in depth)",
    async () => {
      const stage = stageAgentsDir()
      // Put a sentinel file outside the agents dir that the traversal
      // attempt would land on if isValidRoleName did not reject it.
      const sentinel = path.join(stage.dir, "secret.md")
      fs.writeFileSync(sentinel, "SECRET_TOKEN_MUST_NOT_LEAK")
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        // Construct a relative path that, joined onto
        // <stage>/prism/agents/<role>.md, would resolve to <stage>/secret.md.
        // isValidRoleName must reject this BEFORE the path is constructed.
        const { pi, trigger } = makeMockPI({ agent: "../../secret" })
        prismExtension(pi as never)
        const result = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE" },
          {},
        )) as { systemPrompt?: string } | undefined
        assert.equal(result, undefined, "path-traversal role must be rejected")
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )

  it(
    "idempotency across turns: before_agent_start fires twice, both turns get a non-undefined override and the role prompt does not accumulate",
    async () => {
      const stage = stageAgentsDir()
      const saved = savedEnv()
      setExtensionEnv(stage.dir)
      try {
        const { pi, trigger } = makeMockPI({ agent: "worker" })
        prismExtension(pi as never)

        // Turn 1: standard base.
        const turn1 = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE_TURN_1" },
          {},
        )) as { systemPrompt?: string } | undefined
        assert.ok(turn1?.systemPrompt)
        assert.ok(turn1!.systemPrompt!.startsWith("BASE_TURN_1"))
        assert.ok(turn1!.systemPrompt!.includes(stage.workerToken))

        // Turn 2: pi sends a fresh _baseSystemPrompt (event.systemPrompt
        // always reflects the agent-session base, NOT the previous turn's
        // override — confirmed at pi 0.77 dist/core/agent-session.js line
        // 795 where _baseSystemPrompt is the third arg to
        // emitBeforeAgentStart). The composed result must therefore be
        // BASE_TURN_2 + role, not BASE_TURN_1 + role + role.
        const turn2 = (await trigger(
          "before_agent_start",
          { systemPrompt: "BASE_TURN_2" },
          {},
        )) as { systemPrompt?: string } | undefined
        assert.ok(turn2?.systemPrompt)
        assert.ok(turn2!.systemPrompt!.startsWith("BASE_TURN_2"))
        // The role token must appear EXACTLY ONCE — not accumulated.
        const tokenCount = turn2!.systemPrompt!.split(stage.workerToken).length - 1
        assert.equal(tokenCount, 1, "role prompt must appear exactly once per turn (no accumulation)")
        // And there must be NO leftover BASE_TURN_1 substring.
        assert.equal(
          turn2!.systemPrompt!.includes("BASE_TURN_1"),
          false,
          "turn 2 must not carry turn 1's base",
        )
      } finally {
        restoreEnv(saved)
        stage.cleanup()
      }
    },
  )
})
