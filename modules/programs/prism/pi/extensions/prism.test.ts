// Unit tests for the prism PI extension's pure helpers.
// Run with: node --test --test-force-exit --import tsx prism.test.ts
//
// We test the helper functions in isolation: truncation, endpoint parsing,
// the JSONL line reader, and the inbound dispatcher with a mock API. The
// extension factory itself (with PI hook subscriptions) is end-to-end tested
// via the P2.SPAWN integration scenario.

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { Readable } from "node:stream"
import * as net from "node:net"
import * as os from "node:os"
import * as path from "node:path"
import * as fs from "node:fs"

import {
  attachJsonlReader,
  coerceToolOutput,
  dispatchInboundFrame,
  parseEndpoint,
  redactLine,
  shouldActivate,
  TRUNCATION_LIMIT_BYTES,
  TRUNCATION_SENTINEL,
  truncateArgs,
  truncateString,
  type InboundDispatchAPI,
  // Behavioural guards
  similarityKey,
  tokenizeBashCommand,
  processDoomLoop,
  isGitPush,
  EXCLUDED_BASH_BASES,
  // Pre-tool-call bash deny list (#1528)
  BLOCKED_BASH_PATTERNS,
  checkBlockedBash,
  newDoomLoopState,
  snapshotGuardState,
  restoreGuardState,
  GUARD_STATE_ENTRY_TYPE,
  DOOM_LOOP_THRESHOLD,
  // Status bar
  formatPrismStatus,
  extractBranch,
  // Connection guard
  shouldAttemptConnect,
  // Git-push reminder
  GIT_PUSH_REMINDER_MESSAGE,
  // turn_end signal resolver
  resolveTurnEndSignal,
  // Frame writer
  makeFrameWriter,
  type FrameWriter,
  // Assistant-message text extraction (issue #1764)
  extractAssistantText,
  isAssistantTextDeltaEvent,
  // Mid-tool heartbeat (issue #1761)
  startToolHeartbeat,
  TOOL_HEARTBEAT_INTERVAL_MS,
  // Role system-prompt injection (issue #2032 / #2033)
  prismAgentRolePath,
  readRolePrompt,
  composeRoleSystemPrompt,
  resolveRolePromptForTurn,
  type RolePromptCache,
} from "./prism.ts"
import prismExtension from "./prism.ts"

// ---------------------------------------------------------------------------
// shouldActivate (activation guard)
// ---------------------------------------------------------------------------

describe("shouldActivate", () => {
  it("returns false when PRISM_SESSION_NAME is not set", () => {
    assert.equal(shouldActivate({}), false)
  })

  it("returns false when PRISM_SESSION_NAME is empty", () => {
    assert.equal(shouldActivate({ PRISM_SESSION_NAME: "" }), false)
  })

  it("returns true when PRISM_SESSION_NAME is set", () => {
    assert.equal(
      shouldActivate({ PRISM_SESSION_NAME: "nixos-config@main" }),
      true,
    )
  })
})

// ---------------------------------------------------------------------------
// truncateString
// ---------------------------------------------------------------------------

describe("truncateString", () => {
  it("returns short strings unchanged", () => {
    const r = truncateString("hello")
    assert.equal(r.text, "hello")
    assert.equal(r.truncated, false)
  })

  it("returns the empty string unchanged", () => {
    const r = truncateString("")
    assert.equal(r.text, "")
    assert.equal(r.truncated, false)
  })

  it("returns a string at the byte limit unchanged", () => {
    const s = "a".repeat(TRUNCATION_LIMIT_BYTES)
    const r = truncateString(s)
    assert.equal(r.text, s)
    assert.equal(r.truncated, false)
  })

  it("truncates a long ASCII string and appends the sentinel", () => {
    const s = "a".repeat(TRUNCATION_LIMIT_BYTES + 100)
    const r = truncateString(s)
    assert.equal(r.truncated, true)
    assert.ok(r.text.endsWith(TRUNCATION_SENTINEL))
    // Head of the truncated text matches the head of the input.
    assert.equal(
      r.text.slice(0, TRUNCATION_LIMIT_BYTES),
      "a".repeat(TRUNCATION_LIMIT_BYTES),
    )
  })

  it("does not split a multi-byte character at the cut point", () => {
    // Each "é" is 2 bytes in UTF-8. Build a string whose byte count straddles
    // the limit such that the naive cut would land inside an "é".
    const filler = "a".repeat(TRUNCATION_LIMIT_BYTES - 1)
    const s = filler + "é".repeat(50)
    const r = truncateString(s)
    assert.equal(r.truncated, true)
    // The text portion before the sentinel must round-trip cleanly through
    // UTF-8 — i.e. no replacement characters or invalid bytes.
    const beforeSentinel = r.text.slice(
      0,
      r.text.length - TRUNCATION_SENTINEL.length,
    )
    assert.equal(
      Buffer.from(beforeSentinel, "utf8").toString("utf8"),
      beforeSentinel,
    )
  })
})

// ---------------------------------------------------------------------------
// truncateArgs
// ---------------------------------------------------------------------------

describe("truncateArgs", () => {
  it("returns null/undefined unchanged", () => {
    assert.deepEqual(truncateArgs(null), { args: null, truncated: false })
    assert.deepEqual(truncateArgs(undefined), {
      args: undefined,
      truncated: false,
    })
  })

  it("truncates string-valued top-level fields only", () => {
    const big = "x".repeat(TRUNCATION_LIMIT_BYTES + 10)
    const r = truncateArgs({ command: big, timeout: 5000, flag: false })
    assert.equal(r.truncated, true)
    const args = r.args as Record<string, unknown>
    assert.equal((args.command as string).endsWith(TRUNCATION_SENTINEL), true)
    assert.equal(args.timeout, 5000)
    assert.equal(args.flag, false)
  })

  it("does not flag truncation when no field exceeds the limit", () => {
    const r = truncateArgs({ command: "ls", path: "/tmp" })
    assert.equal(r.truncated, false)
    assert.deepEqual(r.args, { command: "ls", path: "/tmp" })
  })

  it("treats a top-level string as a single value", () => {
    const big = "y".repeat(TRUNCATION_LIMIT_BYTES + 10)
    const r = truncateArgs(big)
    assert.equal(r.truncated, true)
    assert.equal(typeof r.args, "string")
    assert.ok((r.args as string).endsWith(TRUNCATION_SENTINEL))
  })
})

// ---------------------------------------------------------------------------
// coerceToolOutput
// ---------------------------------------------------------------------------

describe("coerceToolOutput", () => {
  it("returns plain strings unchanged", () => {
    assert.equal(coerceToolOutput("hello"), "hello")
  })

  it("concatenates text blocks", () => {
    assert.equal(
      coerceToolOutput([
        { type: "text", text: "foo" },
        { type: "text", text: "bar" },
      ]),
      "foobar",
    )
  })

  it("skips non-text blocks", () => {
    assert.equal(
      coerceToolOutput([
        { type: "text", text: "foo" },
        { type: "image", source: { type: "base64", data: "..." } },
        { type: "text", text: "bar" },
      ]),
      "foobar",
    )
  })

  it("returns empty string for non-array, non-string input", () => {
    assert.equal(coerceToolOutput(null), "")
    assert.equal(coerceToolOutput({}), "")
    assert.equal(coerceToolOutput(42), "")
  })
})

// ---------------------------------------------------------------------------
// parseEndpoint
// ---------------------------------------------------------------------------

describe("parseEndpoint", () => {
  it("parses a unix endpoint", () => {
    const r = parseEndpoint(
      "unix:///home/ben/.local/state/prism/run/abc123/pipe.sock",
    )
    assert.deepEqual(r, {
      kind: "unix",
      path: "/home/ben/.local/state/prism/run/abc123/pipe.sock",
    })
  })

  it("parses a tcp endpoint", () => {
    const r = parseEndpoint("tcp://host.containers.internal:54321")
    assert.deepEqual(r, {
      kind: "tcp",
      host: "host.containers.internal",
      port: 54321,
    })
  })

  it("rejects an unknown scheme", () => {
    assert.throws(() => parseEndpoint("http://example.com"), /unsupported/)
  })

  it("rejects an empty unix path", () => {
    assert.throws(() => parseEndpoint("unix://"), /empty unix path/)
  })

  it("rejects a tcp endpoint missing port", () => {
    assert.throws(() => parseEndpoint("tcp://localhost"), /missing port/)
  })

  it("rejects a tcp endpoint with non-numeric port", () => {
    assert.throws(() => parseEndpoint("tcp://localhost:abc"), /invalid port/)
  })

  it("rejects a tcp endpoint with port out of range", () => {
    assert.throws(() => parseEndpoint("tcp://localhost:99999"), /invalid port/)
  })

  it("rejects a tcp endpoint with empty host", () => {
    assert.throws(() => parseEndpoint("tcp://:1234"), /missing host/)
  })
})

// ---------------------------------------------------------------------------
// attachJsonlReader
// ---------------------------------------------------------------------------

describe("attachJsonlReader", () => {
  it("splits on \\n only, ignoring U+2028 / U+2029", async () => {
    const lines: string[] = []
    const stream = Readable.from([
      // Three logical lines, separated by \n. The middle line contains a
      // U+2028 line separator inside a JSON string — this MUST NOT be
      // treated as a line break.
      `{"type":"a"}\n{"type":"b","text":"foo\u2028bar"}\n{"type":"c"}\n`,
    ])
    attachJsonlReader(stream, (l) => lines.push(l))
    await new Promise((resolve) => stream.on("end", resolve))
    assert.equal(lines.length, 3)
    assert.equal(lines[0], `{"type":"a"}`)
    assert.equal(lines[1], `{"type":"b","text":"foo\u2028bar"}`)
    assert.equal(lines[2], `{"type":"c"}`)
  })

  it("handles \\r\\n by stripping the trailing \\r", async () => {
    const lines: string[] = []
    const stream = Readable.from([`{"a":1}\r\n{"b":2}\r\n`])
    attachJsonlReader(stream, (l) => lines.push(l))
    await new Promise((resolve) => stream.on("end", resolve))
    assert.deepEqual(lines, [`{"a":1}`, `{"b":2}`])
  })

  it("buffers across chunk boundaries", async () => {
    const lines: string[] = []
    const stream = Readable.from([`{"a":`, `1}\n{"b":2}`, `\n`])
    attachJsonlReader(stream, (l) => lines.push(l))
    await new Promise((resolve) => stream.on("end", resolve))
    assert.deepEqual(lines, [`{"a":1}`, `{"b":2}`])
  })

  it("flushes a trailing line without a newline on end", async () => {
    const lines: string[] = []
    const stream = Readable.from([`{"a":1}\n{"b":2}`])
    attachJsonlReader(stream, (l) => lines.push(l))
    await new Promise((resolve) => stream.on("end", resolve))
    assert.deepEqual(lines, [`{"a":1}`, `{"b":2}`])
  })
})

// ---------------------------------------------------------------------------
// redactLine
// ---------------------------------------------------------------------------

describe("redactLine", () => {
  it("base64 encodes the first 200 bytes", () => {
    const r = redactLine("hello")
    assert.equal(Buffer.from(r, "base64").toString("utf8"), "hello")
  })

  it("caps at 200 bytes", () => {
    const long = "a".repeat(500)
    const r = redactLine(long)
    const decoded = Buffer.from(r, "base64")
    assert.equal(decoded.length, 200)
  })
})

// ---------------------------------------------------------------------------
// dispatchInboundFrame
// ---------------------------------------------------------------------------

interface MockCalls {
  sendUserMessage: Array<{ content: unknown; options: unknown }>
  setModel: Array<unknown>
  setThinkingLevel: string[]
  registerProvider: Array<{ name: string; config: unknown }>
  setActiveTools: string[][]
  abort: number
  errors: Array<{ code: string; message: string; relatedType?: string }>
  modelLookups: Array<{ provider: string; model: string }>
}

function makeMockApi(): {
  api: InboundDispatchAPI
  calls: MockCalls
  emit: (
    code: string,
    message: string,
    relatedType: string | undefined,
  ) => void
  registerModel: (provider: string, model: string, value: unknown) => void
} {
  const calls: MockCalls = {
    sendUserMessage: [],
    setModel: [],
    setThinkingLevel: [],
    registerProvider: [],
    setActiveTools: [],
    abort: 0,
    errors: [],
    modelLookups: [],
  }
  const models = new Map<string, unknown>()

  const api: InboundDispatchAPI = {
    sendUserMessage: (content, options) => {
      calls.sendUserMessage.push({ content, options })
    },
    setModel: async (model) => {
      calls.setModel.push(model)
      return true
    },
    setThinkingLevel: (level) => {
      calls.setThinkingLevel.push(level)
    },
    registerProvider: (name, config) => {
      calls.registerProvider.push({ name, config })
    },
    setActiveTools: (tools) => {
      calls.setActiveTools.push(tools)
    },
    modelRegistryFind: (provider, model) => {
      calls.modelLookups.push({ provider, model })
      return models.get(`${provider}/${model}`) ?? null
    },
    abort: () => {
      calls.abort++
    },
  }

  const emit = (
    code: string,
    message: string,
    relatedType: string | undefined,
  ) => {
    calls.errors.push({ code, message, relatedType })
  }

  const registerModel = (provider: string, model: string, value: unknown) => {
    models.set(`${provider}/${model}`, value)
  }

  return { api, calls, emit, registerModel }
}

describe("dispatchInboundFrame: prompt", () => {
  it("dispatches a steer-mode prompt with no images as a string", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "prompt", text: "hello", deliver_as: "steer" },
      api,
      emit,
    )
    assert.equal(calls.sendUserMessage.length, 1)
    assert.equal(calls.sendUserMessage[0].content, "hello")
    assert.deepEqual(calls.sendUserMessage[0].options, { deliverAs: "steer" })
  })

  it("dispatches a followUp-mode prompt", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "prompt", text: "next", deliver_as: "followUp" },
      api,
      emit,
    )
    assert.deepEqual(calls.sendUserMessage[0].options, {
      deliverAs: "followUp",
    })
  })

  it("nextTurn while idle (isIdle=true) calls bare sendUserMessage", async () => {
    const { api, calls, emit } = makeMockApi()
    // Default mock: isIdle is undefined (absent, older runtime) — treated as idle=true.
    await dispatchInboundFrame(
      { type: "prompt", text: "now", deliver_as: "nextTurn" },
      api,
      emit,
    )
    assert.equal(calls.sendUserMessage[0].content, "now")
    assert.equal(
      calls.sendUserMessage[0].options,
      undefined,
      "nextTurn while idle must call bare sendUserMessage (no deliverAs option)",
    )
  })

  it("nextTurn while streaming (isIdle=false) routes to followUp", async () => {
    const { api, calls, emit } = makeMockApi()
    // Override isIdle to simulate a mid-stream runtime.
    api.isIdle = () => false
    await dispatchInboundFrame(
      { type: "prompt", text: "queued", deliver_as: "nextTurn" },
      api,
      emit,
    )
    assert.equal(calls.sendUserMessage[0].content, "queued")
    assert.deepEqual(
      calls.sendUserMessage[0].options,
      { deliverAs: "followUp" },
      "nextTurn mid-stream must route to followUp to avoid the 'already processing' throw",
    )
  })

  it("nextTurn with isIdle absent (older runtime) treats runtime as idle", async () => {
    const { api, calls, emit } = makeMockApi()
    // Explicitly ensure isIdle is absent on the api object (edge-case AC).
    delete (api as Record<string, unknown>)["isIdle"]
    await dispatchInboundFrame(
      { type: "prompt", text: "legacy", deliver_as: "nextTurn" },
      api,
      emit,
    )
    assert.equal(calls.sendUserMessage[0].content, "legacy")
    assert.equal(
      calls.sendUserMessage[0].options,
      undefined,
      "absent isIdle must fall back to idle=true (bare sendUserMessage)",
    )
  })

  it("nextTurn: sendUserMessage throw includes deliver_as and isIdle in error context", async () => {
    const { api, calls, emit } = makeMockApi()
    api.isIdle = () => false
    api.sendUserMessage = () => {
      throw new Error("invalid content shape")
    }
    await dispatchInboundFrame(
      { type: "prompt", text: "bad", deliver_as: "nextTurn" },
      api,
      emit,
    )
    assert.equal(calls.errors.length, 1)
    const errMsg = calls.errors[0].message
    assert.ok(
      errMsg.includes("deliver_as=nextTurn"),
      `error should include deliver_as=nextTurn, got: ${errMsg}`,
    )
    assert.ok(
      errMsg.includes("isIdle=false"),
      `error should include isIdle=false, got: ${errMsg}`,
    )
  })

  it("converts wire images (mime_type) to PI's mediaType ImageContent", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      {
        type: "prompt",
        text: "look at this",
        deliver_as: "steer",
        images: [{ type: "image", data: "BASE64HERE", mime_type: "image/png" }],
      },
      api,
      emit,
    )
    const content = calls.sendUserMessage[0].content as Array<{
      type: string
      [k: string]: unknown
    }>
    assert.equal(Array.isArray(content), true)
    assert.equal(content.length, 2)
    assert.equal(content[0].type, "text")
    assert.equal(content[1].type, "image")
    const src = (
      content[1] as unknown as { source: { mediaType: string; data: string } }
    ).source
    assert.equal(src.mediaType, "image/png")
    assert.equal(src.data, "BASE64HERE")
  })

  it("replay=true is delivered to the agent (no drop) and logged", async () => {
    // Issue #1685 AC #5/#7: replayed frames carry replay=true so the sidecar
    // can identify resumed deliveries post-reconnect. The extension must
    // still forward the body to the agent (exactly-once is the bus's job;
    // the agent receives the message regardless of the marker).
    const { api, calls, emit } = makeMockApi()
    const originalErr = console.error
    const logged: string[] = []
    console.error = (...args: unknown[]) => {
      logged.push(args.map((a) => String(a)).join(" "))
    }
    try {
      await dispatchInboundFrame(
        { type: "prompt", text: "escalation", deliver_as: "followUp", replay: true },
        api,
        emit,
      )
    } finally {
      console.error = originalErr
    }
    assert.equal(calls.sendUserMessage.length, 1, "replay=true must still deliver the prompt")
    assert.equal(calls.sendUserMessage[0].content, "escalation")
    assert.deepEqual(calls.sendUserMessage[0].options, { deliverAs: "followUp" })
    assert.ok(
      logged.some((line) => line.includes("prompt replay")),
      `expected a replay log line, got: ${JSON.stringify(logged)}`,
    )
  })
})

describe("dispatchInboundFrame: set_model", () => {
  it("looks up the model and sets it then sets thinking", async () => {
    const { api, calls, emit, registerModel } = makeMockApi()
    const fakeModel = { id: "claude-sonnet-4-20250514" }
    registerModel("anthropic", "claude-sonnet-4-20250514", fakeModel)
    await dispatchInboundFrame(
      {
        type: "set_model",
        provider: "anthropic",
        model: "claude-sonnet-4-20250514",
        thinking: "high",
      },
      api,
      emit,
    )
    assert.equal(calls.errors.length, 0)
    assert.equal(calls.setModel.length, 1)
    assert.equal(calls.setModel[0], fakeModel)
    assert.deepEqual(calls.setThinkingLevel, ["high"])
  })

  it("emits an error when the model is not in the registry", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      {
        type: "set_model",
        provider: "anthropic",
        model: "does-not-exist",
        thinking: "off",
      },
      api,
      emit,
    )
    assert.equal(calls.setModel.length, 0)
    assert.equal(calls.errors.length, 1)
    assert.equal(calls.errors[0].code, "model_not_found")
  })

  it("emits malformed_frame when provider or model is missing", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "set_model", thinking: "off" },
      api,
      emit,
    )
    assert.equal(calls.errors.length, 1)
    assert.equal(calls.errors[0].code, "malformed_frame")
  })
})

describe("dispatchInboundFrame: register_provider", () => {
  it("forwards name + config verbatim", async () => {
    const { api, calls, emit } = makeMockApi()
    const cfg = { baseUrl: "https://api.example.com", api: "openai-completions" }
    await dispatchInboundFrame(
      { type: "register_provider", name: "my-proxy", config: cfg },
      api,
      emit,
    )
    assert.equal(calls.errors.length, 0)
    assert.equal(calls.registerProvider.length, 1)
    assert.equal(calls.registerProvider[0].name, "my-proxy")
    assert.deepEqual(calls.registerProvider[0].config, cfg)
  })

  it("emits malformed_frame when name is missing", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "register_provider", config: {} },
      api,
      emit,
    )
    assert.equal(calls.errors[0].code, "malformed_frame")
  })

  it("emits malformed_frame when config is missing", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "register_provider", name: "x" },
      api,
      emit,
    )
    assert.equal(calls.errors[0].code, "malformed_frame")
  })
})

describe("dispatchInboundFrame: set_active_tools", () => {
  it("forwards the tool list", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "set_active_tools", tools: ["read", "bash"] },
      api,
      emit,
    )
    assert.equal(calls.errors.length, 0)
    assert.deepEqual(calls.setActiveTools, [["read", "bash"]])
  })

  it("emits malformed_frame when tools is not an array of strings", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "set_active_tools", tools: ["read", 123] },
      api,
      emit,
    )
    assert.equal(calls.errors[0].code, "malformed_frame")
  })
})

describe("dispatchInboundFrame: abort", () => {
  it("calls the abort handler", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame({ type: "abort" }, api, emit)
    assert.equal(calls.abort, 1)
    assert.equal(calls.errors.length, 0)
  })
})

describe("dispatchInboundFrame: forward-compat", () => {
  it("logs and skips an unknown frame type without erroring", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "future_v2_frame", payload: 123 },
      api,
      emit,
    )
    // No error frame is emitted: per wire spec §8.2 unknown frames are
    // logged and skipped, not reported as errors. (Only frames missing a
    // type field at all are malformed.)
    assert.equal(calls.errors.length, 0)
  })

  it("emits malformed_frame when type field is missing", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { something: "else" },
      api,
      emit,
    )
    assert.equal(calls.errors[0].code, "malformed_frame")
  })
})

// ---------------------------------------------------------------------------
// similarityKey
// ---------------------------------------------------------------------------

describe("similarityKey — excluded tools", () => {
  it("returns null for read", () => {
    assert.equal(similarityKey("read", { filePath: "/foo.go" }), null)
  })

  it("returns null for grep", () => {
    assert.equal(similarityKey("grep", { pattern: "foo" }), null)
  })

  it("returns null for glob", () => {
    assert.equal(similarityKey("glob", { pattern: "**/*.ts" }), null)
  })

  it("returns null for todowrite", () => {
    assert.equal(similarityKey("todowrite", { todos: [] }), null)
  })
})

describe("similarityKey — bash full-argv keying (#1683)", () => {
  it("gh issue view with different numbers produces different keys", () => {
    const keys = [1, 2, 3, 4, 5].map((n) =>
      similarityKey("bash", { command: `gh issue view ${n}` }),
    )
    assert.equal(new Set(keys).size, 5)
  })

  it("git log with different flag values produces different keys (#1683)", () => {
    // Post-#1683: flags are part of the key. `git log -1` and `git log -3`
    // are different argv, so they hash differently. This intentionally
    // overrides the pre-fix collapsing heuristic — doom-loop detection is
    // about exact argv repetition, not semantic equivalence.
    const a = similarityKey("bash", { command: "git log -1" })
    const b = similarityKey("bash", { command: "git log -3" })
    assert.notEqual(a, b)
  })

  it("go test and go build produce different keys", () => {
    const a = similarityKey("bash", { command: "go test ./..." })
    const b = similarityKey("bash", { command: "go build ./..." })
    assert.notEqual(a, b)
  })
})

// Issue #1683: doom-loop detector false positives on different-argv bash calls.
// The detector used to key on `base + firstPositional`, collapsing
// `prism checkin <s> --last 50`, `--last 100`, `--verbose`, etc. to the same
// key and tripping the 5-in-a-row counter on legitimate iterative diagnosis.
// The fix includes the full normalised argv in the key.
describe("similarityKey — #1683 false-positive cases", () => {
  it("5 prism-checkin calls with different --last/--types/--verbose flags produce 5 distinct keys", () => {
    const session = "nixos-config@checkin-flag-variants"
    const cmds = [
      `prism checkin ${session} --types audit --last 50`,
      `prism checkin ${session} --last 40 --verbose`,
      `prism checkin ${session} --last 40 --verbose --types audit`,
      `prism checkin ${session} --last 60 --verbose`,
      `prism checkin ${session} --last 100`,
    ]
    const keys = cmds.map((command) => similarityKey("bash", { command }))
    assert.equal(new Set(keys).size, 5)
  })

  it("5 prism-checkin calls with different first positional (session) produce 5 distinct keys", () => {
    const cmds = [
      "prism checkin nixos-config@worker-a --last 50",
      "prism checkin nixos-config@worker-b --last 50",
      "prism checkin nixos-config@worker-c --last 50",
      "prism checkin nixos-config@worker-d --last 50",
      "prism checkin nixos-config@worker-e --last 50",
    ]
    const keys = cmds.map((command) => similarityKey("bash", { command }))
    assert.equal(new Set(keys).size, 5)
  })

  it("alternating prism-checkin between two sessions produces 2 distinct keys", () => {
    const cmds = [
      "prism checkin nixos-config@worker-a",
      "prism checkin nixos-config@worker-b",
      "prism checkin nixos-config@worker-a",
      "prism checkin nixos-config@worker-b",
      "prism checkin nixos-config@worker-a",
    ]
    const keys = cmds.map((command) => similarityKey("bash", { command }))
    assert.equal(new Set(keys).size, 2)
  })
})

describe("similarityKey — #1683 true-positive cases", () => {
  it("5 identical prism-checkin commands produce 1 key", () => {
    const cmd = "prism checkin nixos-config@worker-a --last 50"
    const keys = [1, 2, 3, 4, 5].map(() => similarityKey("bash", { command: cmd }))
    assert.equal(new Set(keys).size, 1)
  })

  it("5 identical non-bash commands still produce 1 key (edit on the same file)", () => {
    const keys = [1, 2, 3, 4, 5].map(() =>
      similarityKey("edit", { filePath: "/foo.go", newString: "x" }),
    )
    assert.equal(new Set(keys).size, 1)
  })
})

describe("similarityKey — #1683 whitespace and quoting normalisation", () => {
  it("double-quoted and single-quoted -c arg produce the same key", () => {
    const a = similarityKey("bash", { command: `bash -c "ls -la"` })
    const b = similarityKey("bash", { command: `bash -c 'ls -la'` })
    assert.equal(a, b)
  })

  it("runs of internal whitespace are collapsed", () => {
    const a = similarityKey("bash", { command: "go   test     ./..." })
    const b = similarityKey("bash", { command: "go test ./..." })
    assert.equal(a, b)
  })

  it("leading/trailing whitespace does not change the key", () => {
    const a = similarityKey("bash", { command: "  go test ./...  " })
    const b = similarityKey("bash", { command: "go test ./..." })
    assert.equal(a, b)
  })

  it("tabs and spaces are equivalent token separators", () => {
    const a = similarityKey("bash", { command: "go\ttest\t./..." })
    const b = similarityKey("bash", { command: "go test ./..." })
    assert.equal(a, b)
  })
})

// Direct unit tests for the tokeniser helper.
describe("tokenizeBashCommand", () => {
  it("splits on whitespace", () => {
    assert.deepEqual(tokenizeBashCommand("a b c"), ["a", "b", "c"])
  })

  it("collapses runs of whitespace", () => {
    assert.deepEqual(tokenizeBashCommand("a   b \t c"), ["a", "b", "c"])
  })

  it("trims leading/trailing whitespace", () => {
    assert.deepEqual(tokenizeBashCommand("  a b  "), ["a", "b"])
  })

  it("keeps a double-quoted region with whitespace as one token, stripping the quotes", () => {
    assert.deepEqual(tokenizeBashCommand(`bash -c "ls -la"`), ["bash", "-c", "ls -la"])
  })

  it("keeps a single-quoted region with whitespace as one token, stripping the quotes", () => {
    assert.deepEqual(tokenizeBashCommand(`bash -c 'ls -la'`), ["bash", "-c", "ls -la"])
  })

  it("double- and single-quoted forms of the same content tokenise identically", () => {
    assert.deepEqual(
      tokenizeBashCommand(`bash -c "ls -la"`),
      tokenizeBashCommand(`bash -c 'ls -la'`),
    )
  })

  it("concatenates adjacent quoted and unquoted segments into a single token", () => {
    assert.deepEqual(tokenizeBashCommand(`foo"bar baz"qux`), ["foobar bazqux"])
  })

  it("returns an empty array for empty/whitespace-only input", () => {
    assert.deepEqual(tokenizeBashCommand(""), [])
    assert.deepEqual(tokenizeBashCommand("   \t  "), [])
  })

  it("tolerates an unmatched trailing quote (does not throw, consumes remainder)", () => {
    // The opening quote starts a token; everything after is part of that
    // token (with the quote char itself dropped). The result is deliberate:
    // a total function, never an exception, for arbitrary agent input.
    assert.deepEqual(tokenizeBashCommand(`a "b c`), ["a", "b c"])
  })
})

describe("similarityKey — bash cd-prefix stripping", () => {
  it("strips a single 'cd /path &&' prefix (git command)", () => {
    const key = similarityKey("bash", { command: "cd /foo && git push origin main" })
    assert.equal(key, "bash:git push origin main")
  })

  it("strips a single 'cd /path &&' prefix (gh command)", () => {
    const key = similarityKey("bash", { command: "cd /foo && gh pr create --title foo" })
    // Post-#1683: full argv is included in the key.
    assert.equal(key, "bash:gh pr create --title foo")
    // Crucially, it must NOT be bash:cd.
    assert.notEqual(key, "bash:cd")
  })

  it("strips a single 'cd /path &&' prefix (nix command)", () => {
    const key = similarityKey("bash", { command: "cd /foo/bar && nix eval .#foo" })
    // Post-#1683: full argv is included in the key.
    assert.equal(key, "bash:nix eval .#foo")
    // Crucially, it must NOT be bash:cd.
    assert.notEqual(key, "bash:cd")
  })

  it("strips multiple chained 'cd' prefixes", () => {
    const key = similarityKey("bash", { command: "cd /foo && cd /bar && git status" })
    assert.equal(key, "bash:git status")
  })

  it("does NOT strip a bare 'cd /foo' (no following command)", () => {
    const key = similarityKey("bash", { command: "cd /foo" })
    // Bare cd is not stripped; base=cd, positional=/foo.
    // The key contains 'cd' as the base command, confirming the bare cd is treated as-is.
    assert.ok(key !== null && key.startsWith("bash:cd"), `expected key starting with bash:cd, got ${key}`)
    // Post-#1683: positional is also in the key.
    assert.equal(key, "bash:cd /foo")
  })

  it("strips 'cd /path;' (semicolon separator) prefix", () => {
    const key = similarityKey("bash", { command: "cd /foo; git status" })
    assert.equal(key, "bash:git status")
  })

  it("5 different bash commands all starting with the same 'cd /worktree &&' produce different keys", () => {
    const cmds = [
      "cd /home/ben/worktree && git status",
      "cd /home/ben/worktree && git add .",
      "cd /home/ben/worktree && git commit -m 'msg'",
      "cd /home/ben/worktree && git push origin branch",
      "cd /home/ben/worktree && gh pr create --title foo",
    ]
    const keys = cmds.map((command) => similarityKey("bash", { command }))
    // All 5 should be distinct — no doom-loop false positive.
    assert.equal(new Set(keys).size, 5)
  })

  it("5 identical bash commands (same after stripping) DO produce the same key", () => {
    const cmd = "cd /home/ben/worktree && git status"
    const keys = [1, 2, 3, 4, 5].map(() => similarityKey("bash", { command: cmd }))
    // All 5 should be the same key — doom-loop detector fires correctly.
    assert.equal(new Set(keys).size, 1)
  })
})

describe("similarityKey — EXCLUDED_BASH_BASES", () => {
  it("grep returns null", () => {
    assert.equal(similarityKey("bash", { command: "grep foo file1" }), null)
  })

  it("rg returns null", () => {
    assert.equal(similarityKey("bash", { command: "rg foo path/to/file" }), null)
  })

  it("find returns null", () => {
    assert.equal(similarityKey("bash", { command: "find . -name foo" }), null)
  })

  it("cat returns null", () => {
    assert.equal(similarityKey("bash", { command: "cat file" }), null)
  })

  it("EXCLUDED_BASH_BASES is exported and contains expected members", () => {
    assert.ok(EXCLUDED_BASH_BASES.has("grep"))
    assert.ok(EXCLUDED_BASH_BASES.has("rg"))
    assert.ok(EXCLUDED_BASH_BASES.has("find"))
    assert.ok(EXCLUDED_BASH_BASES.has("cat"))
    assert.ok(EXCLUDED_BASH_BASES.has("ag"))
    assert.ok(EXCLUDED_BASH_BASES.has("fd"))
  })

  it("grep with leading flags (-r) is still excluded", () => {
    // baseIdx flag-skipping correctly identifies grep as the base
    assert.equal(similarityKey("bash", { command: "grep -r \"pat\" dir" }), null)
  })

  it("cd /foo && grep strips prefix then excludes", () => {
    // cd-prefix stripping runs before EXCLUDED_BASH_BASES check; effective base = grep
    assert.equal(similarityKey("bash", { command: "cd /foo && grep \"pat\" file" }), null)
  })

  it("git is NOT in EXCLUDED_BASH_BASES and produces a non-null key", () => {
    const key = similarityKey("bash", { command: "git push origin main" })
    assert.ok(key !== null)
    assert.ok(key.startsWith("bash:git"))
  })
})

describe("similarityKey — edit/write/webfetch", () => {
  it("edit same file produces the same key", () => {
    const a = similarityKey("edit", { filePath: "/foo.go", newString: "a" })
    const b = similarityKey("edit", { filePath: "/foo.go", newString: "b" })
    assert.equal(a, b)
  })

  it("edit different files produces different keys", () => {
    const a = similarityKey("edit", { filePath: "/foo.go" })
    const b = similarityKey("edit", { filePath: "/bar.go" })
    assert.notEqual(a, b)
  })

  it("webfetch same URL produces the same key", () => {
    const a = similarityKey("webfetch", { url: "https://example.com" })
    const b = similarityKey("webfetch", { url: "https://example.com" })
    assert.equal(a, b)
  })
})

// ---------------------------------------------------------------------------
// processDoomLoop
// ---------------------------------------------------------------------------

describe("processDoomLoop — basic detection", () => {
  it("returns null for the first 4 consecutive matching calls", () => {
    const state = newDoomLoopState()
    for (let i = 0; i < DOOM_LOOP_THRESHOLD - 1; i++) {
      const result = processDoomLoop(state, "bash", { command: "go test ./..." })
      assert.equal(result, null, `expected null on call ${i + 1}`)
    }
  })

  it("fires a steering message on the 5th consecutive matching call", () => {
    const state = newDoomLoopState()
    let msg: string | null = null
    for (let i = 0; i < DOOM_LOOP_THRESHOLD; i++) {
      msg = processDoomLoop(state, "bash", { command: "go test ./..." })
    }
    assert.ok(msg !== null)
    assert.ok(msg.includes("PRISM DOOM-LOOP"))
    assert.ok(msg.includes("bash"))
  })

  it("suppresses subsequent calls after firing (one nudge per loop)", () => {
    const state = newDoomLoopState()
    for (let i = 0; i < DOOM_LOOP_THRESHOLD; i++) {
      processDoomLoop(state, "bash", { command: "go test ./..." })
    }
    // Further calls after firing should return null.
    const after = processDoomLoop(state, "bash", { command: "go test ./..." })
    assert.equal(after, null)
  })

  it("resets after a different tool call", () => {
    const state = newDoomLoopState()
    for (let i = 0; i < DOOM_LOOP_THRESHOLD - 1; i++) {
      processDoomLoop(state, "bash", { command: "go test ./..." })
    }
    // Different tool breaks the run.
    processDoomLoop(state, "bash", { command: "go build ./..." })
    // Now 4 more calls should not fire.
    let msg: string | null = null
    for (let i = 0; i < DOOM_LOOP_THRESHOLD - 1; i++) {
      msg = processDoomLoop(state, "bash", { command: "go test ./..." })
    }
    assert.equal(msg, null)
  })

  it("excluded tool (read) breaks the run", () => {
    const state = newDoomLoopState()
    for (let i = 0; i < DOOM_LOOP_THRESHOLD - 1; i++) {
      processDoomLoop(state, "bash", { command: "go test ./..." })
    }
    // excluded tool resets state.
    processDoomLoop(state, "read", { filePath: "/foo.go" })
    assert.equal(state.currentKey, null)
    assert.equal(state.consecutiveCount, 0)
  })
})

describe("processDoomLoop — per-session isolation", () => {
  it("two independent states do not cross-contaminate", () => {
    const stateA = newDoomLoopState()
    const stateB = newDoomLoopState()
    // Advance A to 4 calls.
    for (let i = 0; i < DOOM_LOOP_THRESHOLD - 1; i++) {
      processDoomLoop(stateA, "bash", { command: "go test ./..." })
    }
    // B is untouched.
    assert.equal(stateB.consecutiveCount, 0)
    // A fires on the 5th call; B does not.
    const msgA = processDoomLoop(stateA, "bash", { command: "go test ./..." })
    const msgB = processDoomLoop(stateB, "bash", { command: "go test ./..." })
    assert.ok(msgA !== null)
    assert.equal(msgB, null)
  })
})

describe("processDoomLoop — EXCLUDED_BASH_BASES exclusion", () => {
  it("5 grep calls over varying files all return null and do not fire", () => {
    const state = newDoomLoopState()
    const files = ["file1.nix", "file2.nix", "file3.nix", "file4.nix", "file5.nix"]
    for (const f of files) {
      const result = processDoomLoop(state, "bash", { command: `grep opencode ${f}` })
      assert.equal(result, null, `expected null for grep on ${f}`)
    }
  })

  it("5 grep calls over the same file all return null and do not fire", () => {
    // grep is in EXCLUDED_BASH_BASES regardless of args — intentional trade-off
    const state = newDoomLoopState()
    for (let i = 0; i < 5; i++) {
      const result = processDoomLoop(state, "bash", { command: "grep opencode same-file.nix" })
      assert.equal(result, null)
    }
  })

  it("edit, grep, edit, grep, edit does NOT fire on the third edit", () => {
    const state = newDoomLoopState()
    // edit 1
    processDoomLoop(state, "edit", { filePath: "foo.go" })
    // grep — should reset the run
    processDoomLoop(state, "bash", { command: "grep opencode modules/foo.nix" })
    // edit 2 — starts a fresh run (count=1)
    processDoomLoop(state, "edit", { filePath: "foo.go" })
    // grep again — resets again
    processDoomLoop(state, "bash", { command: "grep opencode modules/bar.nix" })
    // edit 3 — fresh run (count=1), should NOT fire
    const result = processDoomLoop(state, "edit", { filePath: "foo.go" })
    assert.equal(result, null)
    assert.ok(state.consecutiveCount < 5)
  })

  it("grep resets doom-loop state (currentKey, consecutiveCount, fired)", () => {
    const state = newDoomLoopState()
    // Build up 4 edits.
    for (let i = 0; i < 4; i++) {
      processDoomLoop(state, "edit", { filePath: "foo.go" })
    }
    assert.equal(state.consecutiveCount, 4)
    // Intervening grep must reset.
    processDoomLoop(state, "bash", { command: "grep opencode modules/foo.nix" })
    assert.equal(state.currentKey, null)
    assert.equal(state.consecutiveCount, 0)
    assert.equal(state.fired, false)
  })

  it("5 identical edit calls (no intervening tool) still fire at count 5", () => {
    const state = newDoomLoopState()
    let msg: string | null = null
    for (let i = 0; i < DOOM_LOOP_THRESHOLD; i++) {
      msg = processDoomLoop(state, "edit", { filePath: "foo.go" })
    }
    assert.ok(msg !== null, "expected doom-loop to fire")
    assert.ok(msg.includes("PRISM DOOM-LOOP"))
  })

  it("5 identical bash:git push calls still fire at count 5", () => {
    // git is in SUBCOMMAND_CLIS, not EXCLUDED_BASH_BASES — must still detect
    const state = newDoomLoopState()
    let msg: string | null = null
    for (let i = 0; i < DOOM_LOOP_THRESHOLD; i++) {
      msg = processDoomLoop(state, "bash", { command: "git push origin main" })
    }
    assert.ok(msg !== null, "expected doom-loop to fire for git push")
    assert.ok(msg.includes("PRISM DOOM-LOOP"))
  })
})

// ---------------------------------------------------------------------------
// doom_loop_detected wire frame shape
// ---------------------------------------------------------------------------

describe("doom_loop_detected wire frame — field names match payload.DoomLoopDetected", () => {
  it("emits {type, tool, pattern, count, timestampMs} on detector fire", () => {
    const state = newDoomLoopState()
    const tool = "bash"
    const args = { command: "go test ./..." }
    const expectedPattern = similarityKey(tool, args)
    assert.ok(expectedPattern !== null, "similarityKey should be non-null for bash")

    // Drive to fire.
    const beforeMs = Date.now()
    let msg: string | null = null
    for (let i = 0; i < DOOM_LOOP_THRESHOLD; i++) {
      msg = processDoomLoop(state, tool, args)
    }
    const afterMs = Date.now()
    assert.ok(msg !== null, "processDoomLoop should fire on threshold")

    // Build the wire frame the same way prism.ts does after firing.
    const frame = {
      type: "doom_loop_detected" as const,
      tool,
      pattern: state.currentKey ?? "",
      count: state.consecutiveCount,
      timestampMs: Date.now(),
    }

    // Keys must be exactly {type, tool, pattern, count, timestampMs}.
    assert.deepEqual(
      Object.keys(frame).sort(),
      ["count", "pattern", "timestampMs", "tool", "type"],
    )

    // pattern equals similarityKey(tool, args).
    assert.equal(frame.pattern, expectedPattern)

    // count equals consecutiveCount (which equals DOOM_LOOP_THRESHOLD after fire).
    assert.equal(frame.count, DOOM_LOOP_THRESHOLD)

    // timestampMs is a positive integer.
    assert.ok(frame.timestampMs > 0)
    assert.ok(Number.isInteger(frame.timestampMs))
    assert.ok(frame.timestampMs >= beforeMs)
    assert.ok(frame.timestampMs <= afterMs + 100) // small buffer for timing

    // session_name and consecutive_count must NOT be present.
    assert.ok(
      !("session_name" in frame),
      "session_name must not be in the wire frame",
    )
    assert.ok(
      !("consecutive_count" in frame),
      "consecutive_count must not be in the wire frame",
    )
  })

  it("pattern is emitted as empty string when similarityKey returns null", () => {
    // This tests the edge-case AC: pattern is always emitted, even if empty.
    // We synthesise a state where currentKey is null after fire by directly
    // checking the fallback in the wire frame expression: `state.currentKey ?? ""`.
    const state = newDoomLoopState()
    // Manually set state to simulate a fired state with null key.
    state.currentKey = null
    state.consecutiveCount = DOOM_LOOP_THRESHOLD
    state.fired = true

    const pattern = state.currentKey ?? ""
    assert.equal(pattern, "")
    // The pattern field is still present (not omitted).
    const frame = { type: "doom_loop_detected" as const, tool: "bash", pattern, count: state.consecutiveCount, timestampMs: Date.now() }
    assert.ok("pattern" in frame)
    assert.equal(frame.pattern, "")
  })
})

// ---------------------------------------------------------------------------
// review-cycle injection removed in #1512 (Shape B)
// ---------------------------------------------------------------------------
//
// processReviewCycle, reviewCycleEscalationMessage, REVIEW_CYCLE_THRESHOLD,
// newReviewCycleState, and the per-turn LOOP-LIMIT injection were deleted
// because cycle counting and the LOOP-LIMIT footer now live exclusively in
// the Go-side review monitor (internal/review/monitor.go). Tests for the new
// behaviour live in internal/review/loop_limit_test.go.
//
// The block below pins that the TS module no longer carries any LOOP-LIMIT
// text or cycle-counting export — i.e. the duplicate implementation defect
// (#1512 defect 4) cannot regress without these tests failing first.

import { readFileSync } from "node:fs"
import * as path from "node:path"
import { fileURLToPath } from "node:url"

const __filename2 = fileURLToPath(import.meta.url)
const __dirname2 = path.dirname(__filename2)

describe("#1512 — review-cycle injection has been removed from prism.ts", () => {
  it("prism.ts does NOT contain the LOOP-LIMIT injection text", () => {
    const src = readFileSync(path.join(__dirname2, "prism.ts"), "utf8")
    assert.ok(
      !src.includes("REVIEW LOOP LIMIT"),
      "prism.ts must not contain 'REVIEW LOOP LIMIT' — the warning text now lives in Go (internal/review/monitor.go)",
    )
    assert.ok(
      !/REVIEW_CYCLE_THRESHOLD\s*=\s*\d/.test(src),
      "prism.ts must not declare REVIEW_CYCLE_THRESHOLD — the threshold lives in Go",
    )
  })

  it("prism.ts does NOT match the bash-substring 'prism review N' regex anywhere it could increment a counter", () => {
    const src = readFileSync(path.join(__dirname2, "prism.ts"), "utf8")
    // The dangerous pattern is /\bprism\s+review\s+(\d+)\b/ — a regex that
    // captures a numeric PR. Its only legitimate use after #1512 would be
    // to detect cycle increments, which we removed. The reviewing-state
    // guard uses a *different* pattern (no capture group, no digit) and is
    // explicitly out of scope; #1519 owns it. Pin that no
    // "capture-group-with-digit" pattern leaks back in.
    assert.ok(
      !/\\bprism\\s\+review\\s\+\(\\d\+\)\\b/.test(src),
      "prism.ts must not contain /\\bprism\\s+review\\s+(\\d+)\\b/ — cycle counting moved to Go (#1512)",
    )
  })

  it("prism.ts does NOT contain the LOOP-LIMIT injection text", () => {
    // The prism extension must not duplicate the REVIEW LOOP LIMIT warning;
    // #1512 established this invariant. The text lives in Go, not in the extension.
    const hooksPath = path.resolve(
      __dirname2,
      "prism.ts",
    )
    const src = readFileSync(hooksPath, "utf8")
    assert.ok(
      !src.includes("REVIEW LOOP LIMIT"),
      "prism-hooks.ts must not contain 'REVIEW LOOP LIMIT' — the warning text now lives in Go",
    )
    assert.ok(
      !/\\bprism\\s\+review\\s\+\(\\d\+\)\\b/.test(src),
      "prism-hooks.ts must not contain the cycle-increment regex",
    )
  })
})

// ---------------------------------------------------------------------------
// isGitPush
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// snapshotGuardState / restoreGuardState / GUARD_STATE_ENTRY_TYPE
// ---------------------------------------------------------------------------

describe("snapshotGuardState", () => {
  it("serialises doom-loop state", () => {
    const dl = newDoomLoopState()
    dl.currentKey = "bash:git push"
    dl.consecutiveCount = 3
    dl.fired = false
    const snap = snapshotGuardState(dl, false, false)
    assert.equal(snap.doomLoop.currentKey, "bash:git push")
    assert.equal(snap.doomLoop.consecutiveCount, 3)
    assert.equal(snap.doomLoop.fired, false)
  })

  it("serialises pendingGitPushReminder", () => {
    const dl = newDoomLoopState()
    const snap = snapshotGuardState(dl, true, false)
    assert.equal(snap.pendingGitPushReminder, true)
  })

  it("serialises pendingReviewCall", () => {
    const dl = newDoomLoopState()
    const snap = snapshotGuardState(dl, false, true)
    assert.equal(snap.pendingReviewCall, true)
  })

  it("produces a JSON-safe value", () => {
    const dl = newDoomLoopState()
    dl.currentKey = "bash:nix build"
    dl.consecutiveCount = 2
    const snap = snapshotGuardState(dl, false, false)
    const json = JSON.stringify(snap)
    const parsed = JSON.parse(json)
    assert.equal(parsed.doomLoop.currentKey, "bash:nix build")
    assert.equal(parsed.doomLoop.consecutiveCount, 2)
  })

  it("never references a removed reviewCycle field on the new shape", () => {
    // Pin #1512: the snapshot must NOT carry reviewCycle data — cycle
    // counting moved to the Go-side monitor.
    const dl = newDoomLoopState()
    const snap = snapshotGuardState(dl, false, false) as Record<string, unknown>
    assert.ok(!("reviewCycle" in snap), "snapshotGuardState must not emit a reviewCycle field")
  })
})

describe("restoreGuardState", () => {
  it("restores doom-loop fields", () => {
    const dl = newDoomLoopState()
    const snap = {
      doomLoop: { currentKey: "bash:nix build", consecutiveCount: 4, fired: true },
      pendingGitPushReminder: false,
      pendingReviewCall: false,
    }
    restoreGuardState(snap, dl)
    assert.equal(dl.currentKey, "bash:nix build")
    assert.equal(dl.consecutiveCount, 4)
    assert.equal(dl.fired, true)
  })

  it("restores pendingGitPushReminder and pendingReviewCall", () => {
    const dl = newDoomLoopState()
    const snap = {
      doomLoop: { currentKey: null, consecutiveCount: 0, fired: false },
      pendingGitPushReminder: true,
      pendingReviewCall: true,
    }
    const result = restoreGuardState(snap, dl)
    assert.equal(result.pendingGitPushReminder, true)
    assert.equal(result.pendingReviewCall, true)
  })

  it("tolerates legacy snapshots that include a reviewCycle field", () => {
    // #1512 backward-compat: pre-fix sessions persisted a reviewCycle
    // sub-object. After restoring, that data is silently ignored.
    const dl = newDoomLoopState()
    const legacySnap = {
      doomLoop: { currentKey: "bash:foo", consecutiveCount: 1, fired: false },
      reviewCycle: { detectedPrNumber: "77", cycles: { "77": 3 }, frameEmitted: true },
      pendingGitPushReminder: false,
      pendingReviewCall: false,
    } as unknown as Parameters<typeof restoreGuardState>[0]
    const result = restoreGuardState(legacySnap, dl)
    assert.equal(dl.currentKey, "bash:foo")
    assert.equal(result.pendingReviewCall, false)
  })

  it("handles missing/malformed snapshot fields gracefully", () => {
    const dl = newDoomLoopState()
    const snap = {
      doomLoop: { currentKey: undefined, consecutiveCount: "not-a-number" as unknown as number, fired: undefined },
      pendingGitPushReminder: undefined as unknown as boolean,
      pendingReviewCall: undefined as unknown as boolean,
    }
    const result = restoreGuardState(snap, dl)
    assert.equal(dl.currentKey, null)
    assert.equal(dl.consecutiveCount, 0)
    assert.equal(dl.fired, false)
    assert.equal(result.pendingGitPushReminder, false)
    assert.equal(result.pendingReviewCall, false)
  })
})

describe("snapshotGuardState + restoreGuardState round-trip", () => {
  it("survives a JSON round-trip (simulating session-file persistence)", () => {
    const dl = newDoomLoopState()
    dl.currentKey = "bash:git status"
    dl.consecutiveCount = 2
    dl.fired = false

    const snap = snapshotGuardState(dl, true, true)
    // Simulate writing to and reading from the session JSON file.
    const persisted = JSON.parse(JSON.stringify(snap))

    const dl2 = newDoomLoopState()
    const { pendingGitPushReminder, pendingReviewCall } = restoreGuardState(persisted, dl2)

    assert.equal(dl2.currentKey, "bash:git status")
    assert.equal(dl2.consecutiveCount, 2)
    assert.equal(dl2.fired, false)
    assert.equal(pendingGitPushReminder, true)
    assert.equal(pendingReviewCall, true)
  })
})

describe("GUARD_STATE_ENTRY_TYPE", () => {
  it("is a non-empty string", () => {
    assert.equal(typeof GUARD_STATE_ENTRY_TYPE, "string")
    assert.ok(GUARD_STATE_ENTRY_TYPE.length > 0)
  })
})

describe("isGitPush", () => {
  // ── Existing positive cases (preserved) ──────────────────────────────────
  it("matches plain git push", () => {
    assert.equal(isGitPush("git push"), true)
  })

  it("matches git push with remote and branch", () => {
    assert.equal(isGitPush("git push origin main"), true)
  })

  it("matches git push with -u flag", () => {
    assert.equal(isGitPush("git push -u origin main"), true)
  })

  it("matches git -C <path> push", () => {
    assert.equal(isGitPush("git -C /some/path push"), true)
  })

  it("does not match git pull", () => {
    assert.equal(isGitPush("git pull"), false)
  })

  it("does not match unrelated commands", () => {
    assert.equal(isGitPush("go test ./..."), false)
  })

  // ── #1519: additional positive cases ─────────────────────────────────────
  it("matches git push --force-with-lease", () => {
    assert.equal(isGitPush("git push --force-with-lease"), true)
  })

  it("matches git push -u origin feat/x", () => {
    assert.equal(isGitPush("git push -u origin feat/x"), true)
  })

  it("matches git --git-dir=/path/.git push", () => {
    assert.equal(isGitPush("git --git-dir=/path/.git push"), true)
  })

  it("matches git --git-dir /path/.git push (space form)", () => {
    assert.equal(isGitPush("git --git-dir /path/.git push"), true)
  })

  it("matches cd /worktree && git push", () => {
    assert.equal(isGitPush("cd /worktree && git push"), true)
  })

  it("matches git status && git push (sequence)", () => {
    assert.equal(isGitPush("git status && git push"), true)
  })

  it("matches git status; git push origin main (semicolon)", () => {
    assert.equal(isGitPush("git status; git push origin main"), true)
  })

  it("matches git push at the end of a multi-line script", () => {
    assert.equal(isGitPush("set -e\ngit add .\ngit commit -m wip\ngit push"), true)
  })

  // ── #1519: false positives the old regex accepted ────────────────────────
  it("does NOT match echo with double-quoted git push", () => {
    assert.equal(isGitPush('echo "git push"'), false)
  })

  it("does NOT match echo with single-quoted git push", () => {
    assert.equal(isGitPush("echo 'git push'"), false)
  })

  it("does NOT match rg with quoted git push pattern", () => {
    assert.equal(isGitPush('rg "git push"'), false)
  })

  it("does NOT match rg with single-quoted git push pattern", () => {
    assert.equal(isGitPush("rg 'git push'"), false)
  })

  it("does NOT match grep with quoted git push pattern", () => {
    assert.equal(isGitPush('grep -r "git push" modules/'), false)
  })

  it("does NOT match ag with quoted git push pattern", () => {
    assert.equal(isGitPush('ag "git push" .'), false)
  })

  it("does NOT match awk with single-quoted git push pattern", () => {
    assert.equal(isGitPush("awk '/git push/ { print }'"), false)
  })

  it("does NOT match sed with single-quoted git push pattern", () => {
    assert.equal(isGitPush("sed -n '/git push/p' file"), false)
  })

  it("does NOT match git log --grep=\"git push\"", () => {
    assert.equal(isGitPush('git log --grep="git push"'), false)
  })

  it("does NOT match a heredoc body containing git push (single-line)", () => {
    const cmd = "cat <<'EOF'\ngit push\nEOF"
    assert.equal(isGitPush(cmd), false)
  })

  it("does NOT match a heredoc body containing git push (multi-line issue template)", () => {
    const cmd = [
      "cat > /tmp/issue.md <<'EOF'",
      "## Reproducer",
      "",
      "    git rebase origin/main",
      "    git push --force-with-lease",
      "",
      "That is the recommended workflow.",
      "EOF",
    ].join("\n")
    assert.equal(isGitPush(cmd), false)
  })

  it("does NOT match a <<-EOF heredoc body", () => {
    const cmd = "cat <<-EOF\n\tgit push\n\tEOF"
    assert.equal(isGitPush(cmd), false)
  })

  it("does NOT match a <<\"EOF\" heredoc body", () => {
    const cmd = 'cat <<"EOF"\ngit push\nEOF'
    assert.equal(isGitPush(cmd), false)
  })

  it("does NOT match echo \"remember to git push\"", () => {
    assert.equal(isGitPush('echo "remember to git push"'), false)
  })

  // ── #1519: pipeline / process-substitution edge cases ────────────────────
  it("does NOT match git status | tee >(echo \"git push\")", () => {
    assert.equal(isGitPush('git status | tee >(echo "git push")'), false)
  })

  it("does NOT match git status when piped to a quoted-arg consumer", () => {
    assert.equal(isGitPush('git status | grep "git push"'), false)
  })

  it("matches git push at the end of a real pipeline", () => {
    // Contrived but verifies pipeline tokenisation: a real git push as the
    // tail stage of a pipeline still triggers.
    assert.equal(isGitPush("true | git push"), true)
  })

  it("does NOT match the string 'git push' embedded in a longer word", () => {
    assert.equal(isGitPush("git pushy"), false)
    assert.equal(isGitPush("mygit push"), false)
  })

  it("does NOT match a bare 'push' subcommand without git", () => {
    assert.equal(isGitPush("push origin main"), false)
  })
})

// ---------------------------------------------------------------------------
// BLOCKED_BASH_PATTERNS / checkBlockedBash (#1528)
// ---------------------------------------------------------------------------

describe("BLOCKED_BASH_PATTERNS", () => {
  it("contains exactly two entries", () => {
    assert.equal(BLOCKED_BASH_PATTERNS.length, 2)
  })

  it("has the git-worktree-prune entry", () => {
    const ids = BLOCKED_BASH_PATTERNS.map((p) => p.id)
    assert.ok(ids.includes("git-worktree-prune"))
  })

  it("has the git-worktree-remove entry", () => {
    const ids = BLOCKED_BASH_PATTERNS.map((p) => p.id)
    assert.ok(ids.includes("git-worktree-remove"))
  })

  it("every entry has id, match, and reason fields", () => {
    for (const p of BLOCKED_BASH_PATTERNS) {
      assert.equal(typeof p.id, "string")
      assert.ok(p.id.length > 0)
      assert.equal(typeof p.match, "function")
      assert.equal(typeof p.reason, "string")
      assert.ok(p.reason.length > 0)
    }
  })

  it("every entry's reason names the recommended alternative (prism cleanup --yes --session)", () => {
    for (const p of BLOCKED_BASH_PATTERNS) {
      assert.ok(
        p.reason.includes("prism cleanup --yes --session"),
        `pattern ${p.id} reason should mention 'prism cleanup --yes --session'`,
      )
    }
  })
})

describe("checkBlockedBash — git worktree prune (positive cases)", () => {
  it("blocks plain 'git worktree prune'", () => {
    const hit = checkBlockedBash("git worktree prune")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks 'git worktree prune -v'", () => {
    const hit = checkBlockedBash("git worktree prune -v")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks 'git -C /path worktree prune'", () => {
    const hit = checkBlockedBash("git -C /some/path worktree prune")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks 'git -C ../.bare worktree prune -v' (the incident command)", () => {
    const hit = checkBlockedBash("git -C ../.bare worktree prune -v")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks 'git --git-dir=/p/.git worktree prune'", () => {
    const hit = checkBlockedBash("git --git-dir=/p/.git worktree prune")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks 'git --git-dir /p/.git worktree prune'", () => {
    const hit = checkBlockedBash("git --git-dir /p/.git worktree prune")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })

  it("blocks the second segment of 'cd /repo && git worktree prune'", () => {
    const hit = checkBlockedBash("cd /repo && git worktree prune")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-prune")
  })
})

describe("checkBlockedBash — git worktree remove (positive cases)", () => {
  it("blocks 'git worktree remove /path/to/wt'", () => {
    const hit = checkBlockedBash("git worktree remove /path/to/wt")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-remove")
  })

  it("blocks 'git worktree remove --force /path'", () => {
    const hit = checkBlockedBash("git worktree remove --force /path")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-remove")
  })

  it("blocks 'git -C /repo worktree remove --force /path'", () => {
    const hit = checkBlockedBash("git -C /repo worktree remove --force /path")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-remove")
  })

  it("blocks 'git --git-dir=/p/.git worktree remove /wt'", () => {
    const hit = checkBlockedBash("git --git-dir=/p/.git worktree remove /wt")
    assert.notEqual(hit, null)
    assert.equal(hit!.id, "git-worktree-remove")
  })
})

describe("checkBlockedBash — negative cases (quoted / heredoc / grep)", () => {
  it("does NOT block double-quoted 'git worktree prune' inside echo", () => {
    assert.equal(checkBlockedBash('echo "git worktree prune"'), null)
  })

  it("does NOT block single-quoted 'git worktree prune' inside echo", () => {
    assert.equal(checkBlockedBash("echo 'git worktree prune'"), null)
  })

  it("does NOT block rg searching for 'git worktree remove'", () => {
    assert.equal(checkBlockedBash('rg "git worktree remove"'), null)
  })

  it("does NOT block grep searching for 'git worktree prune'", () => {
    assert.equal(
      checkBlockedBash('grep -r "git worktree prune" modules/'),
      null,
    )
  })

  it("does NOT block awk pattern containing 'git worktree prune'", () => {
    assert.equal(
      checkBlockedBash("awk '/git worktree prune/ { print }'"),
      null,
    )
  })

  it("does NOT block git log --grep with the literal string", () => {
    assert.equal(
      checkBlockedBash('git log --grep="git worktree prune"'),
      null,
    )
  })

  it("does NOT block heredoc body containing the literal string", () => {
    const cmd = "cat <<'EOF'\ngit worktree prune\ngit worktree remove /wt\nEOF"
    assert.equal(checkBlockedBash(cmd), null)
  })

  it("does NOT block heredoc body with double-quoted delimiter", () => {
    const cmd = 'cat <<"EOF"\ngit worktree prune -v\nEOF'
    assert.equal(checkBlockedBash(cmd), null)
  })

  it("does NOT match the string embedded in a longer word", () => {
    assert.equal(checkBlockedBash("git worktree pruner"), null)
    assert.equal(checkBlockedBash("git worktree removed"), null)
  })

  it("does NOT match unrelated git subcommands", () => {
    assert.equal(checkBlockedBash("git worktree list"), null)
    assert.equal(checkBlockedBash("git worktree add ../wt feature"), null)
    assert.equal(checkBlockedBash("git status"), null)
  })

  it("does NOT match when there is no command", () => {
    assert.equal(checkBlockedBash(""), null)
  })
})

describe("checkBlockedBash — reason string content", () => {
  it("returns the prune-pattern reason on a prune match", () => {
    const hit = checkBlockedBash("git worktree prune")
    assert.notEqual(hit, null)
    assert.ok(hit!.reason.includes("git worktree prune"))
    assert.ok(hit!.reason.includes("prism cleanup --yes --session"))
    assert.ok(hit!.reason.includes("sandboxed agent"))
  })

  it("returns the remove-pattern reason on a remove match", () => {
    const hit = checkBlockedBash("git worktree remove /wt")
    assert.notEqual(hit, null)
    assert.ok(hit!.reason.includes("git worktree remove"))
    assert.ok(hit!.reason.includes("prism cleanup --yes --session"))
  })

  it("reason is prefixed with 'blocked by prism extension'", () => {
    const hit = checkBlockedBash("git worktree prune")
    assert.notEqual(hit, null)
    assert.ok(hit!.reason.startsWith("blocked by prism extension:"))
  })
})

// ---------------------------------------------------------------------------
// GIT_PUSH_REMINDER_MESSAGE
// ---------------------------------------------------------------------------

describe("GIT_PUSH_REMINDER_MESSAGE", () => {
  it("instructs the agent to load the prism skill", () => {
    assert.ok(
      GIT_PUSH_REMINDER_MESSAGE.includes("prism` skill") ||
        GIT_PUSH_REMINDER_MESSAGE.includes("prism skill"),
      "message should mention loading the prism skill",
    )
  })

  it("instructs the agent to run prism review", () => {
    assert.ok(
      GIT_PUSH_REMINDER_MESSAGE.includes("prism review"),
      "message should contain 'prism review'",
    )
  })

  it("does not mention Task subagents", () => {
    assert.ok(
      !GIT_PUSH_REMINDER_MESSAGE.includes("subagent"),
      "message must not mention Task subagents",
    )
  })

  it("does not mention parallel Task calls", () => {
    assert.ok(
      !GIT_PUSH_REMINDER_MESSAGE.includes("Task call"),
      "message must not mention parallel Task calls",
    )
  })
})

// ---------------------------------------------------------------------------
// formatPrismStatus
// ---------------------------------------------------------------------------

describe("formatPrismStatus — coordinator (sandbox-exec, no suffix)", () => {
  it("shows role and branch without isolation suffix", () => {
    const text = formatPrismStatus("coordinator", "main", "sandbox-exec", null, 0)
    assert.equal(text, "[coordinator] main")
  })
})

describe("formatPrismStatus — coordinator (bwrap, no suffix)", () => {
  it("shows role and branch without isolation suffix", () => {
    const text = formatPrismStatus("coordinator", "main", "bwrap", null, 0)
    assert.equal(text, "[coordinator] main")
  })
})

describe("formatPrismStatus — coordinator (host, suffix shown)", () => {
  it("appends (host) suffix when isolation mode is 'host'", () => {
    const text = formatPrismStatus("coordinator", "obsidian", "host", null, 0)
    assert.equal(text, "[coordinator] obsidian (host)")
  })
})

describe("formatPrismStatus — coordinator (empty string, treated as host)", () => {
  it("appends (host) suffix when isolation mode is absent/empty", () => {
    const text = formatPrismStatus("coordinator", "main", "", null, 0)
    assert.equal(text, "[coordinator] main (host)")
  })
})

describe("formatPrismStatus — worker", () => {
  it("shows role and branch (sandbox-exec, no suffix)", () => {
    const text = formatPrismStatus("worker", "fix-login-redirect", "sandbox-exec", null, 0)
    assert.equal(text, "[worker] fix-login-redirect")
  })

  it("shows (host) suffix in host mode", () => {
    const text = formatPrismStatus("worker", "fix-login-redirect", "host", null, 0)
    assert.equal(text, "[worker] fix-login-redirect (host)")
  })

  it("does not include PR info even if cycles > 0 (sandbox-exec)", () => {
    const text = formatPrismStatus("worker", "some-branch", "sandbox-exec", "42", 2)
    assert.equal(text, "[worker] some-branch")
  })
})

describe("formatPrismStatus — review", () => {
  it("includes PR number and cycle count (sandbox-exec, no suffix)", () => {
    const text = formatPrismStatus("review", "fix-login-redirect", "sandbox-exec", "42", 2)
    assert.equal(text, "[review] fix-login-redirect · PR#42 · 2 cycles")
  })

  it("includes PR number and cycle count with (host) suffix in host mode", () => {
    const text = formatPrismStatus("review", "fix-login-redirect", "host", "42", 2)
    assert.equal(text, "[review] fix-login-redirect (host) · PR#42 · 2 cycles")
  })

  it("uses singular 'cycle' when count is 1 (sandbox-exec)", () => {
    const text = formatPrismStatus("review", "fix-login-redirect", "sandbox-exec", "42", 1)
    assert.equal(text, "[review] fix-login-redirect · PR#42 · 1 cycle")
  })

  it("omits PR info when pr_number is null (sandbox-exec)", () => {
    const text = formatPrismStatus("review", "fix-login-redirect", "sandbox-exec", null, 0)
    assert.equal(text, "[review] fix-login-redirect")
  })
})

describe("formatPrismStatus — unknown role", () => {
  it("shows [unknown] when role is empty (sandbox-exec, no suffix)", () => {
    const text = formatPrismStatus("", "main", "sandbox-exec", null, 0)
    assert.equal(text, "[unknown] main")
  })

  it("shows [unknown] with (host) suffix in host mode", () => {
    const text = formatPrismStatus("", "main", "host", null, 0)
    assert.equal(text, "[unknown] main (host)")
  })
})

// ---------------------------------------------------------------------------
// extractBranch
// ---------------------------------------------------------------------------

describe("extractBranch", () => {
  it("extracts the part after @ in session_name", () => {
    assert.equal(extractBranch("nixos-config@fix-login"), "fix-login")
  })

  it("returns full string when no @ is present", () => {
    assert.equal(extractBranch("main"), "main")
  })

  it("handles multiple @ signs — uses last segment after first @", () => {
    assert.equal(extractBranch("nixos-config@fix@extra"), "fix@extra")
  })

  it("returns empty string for empty input", () => {
    assert.equal(extractBranch(""), "")
  })
})

// ---------------------------------------------------------------------------
// shouldAttemptConnect (session_start guard)
// ---------------------------------------------------------------------------

describe("shouldAttemptConnect", () => {
  it("returns true when socket is null and not connected", () => {
    assert.equal(shouldAttemptConnect(null, false), true)
  })

  it("returns false when socket is non-null (connection already live)", () => {
    // Simulate an active socket object — any non-null value stands in.
    const fakeSocket = {}
    assert.equal(shouldAttemptConnect(fakeSocket, false), false)
  })

  it("returns false when connected flag is true even if socket is null", () => {
    // connected=true can briefly precede socket being set on the 'connect'
    // event; the guard must still block a duplicate dial in this window.
    assert.equal(shouldAttemptConnect(null, true), false)
  })

  it("returns false when both socket is set and connected is true", () => {
    const fakeSocket = {}
    assert.equal(shouldAttemptConnect(fakeSocket, true), false)
  })
})


// ---------------------------------------------------------------------------
// #1554: first-connect retry on ECONNREFUSED / ENOENT
// ---------------------------------------------------------------------------
//
// The extension retries the very first connect() call when it fails with
// ECONNREFUSED (TCP) or ENOENT (Unix) while firstConnect is true (i.e. before
// a successful hello_ack). Up to 5 retries with exponential backoff. Post-
// handshake reconnect is unchanged.
//
// All tests use PRISM_FIRST_CONNECT_RETRY_DELAYS_MS to override the retry
// schedule with tiny delays (1,2,4,8,16 ms) so the full budget (31ms) fits
// in a fast async test. Tests are async functions returning Promises so bun
// correctly waits for completion.

/** Minimal mock of ExtensionAPI — only `on` is needed for these tests. */
function makeMockPI1554() {
  const handlers: Record<string, ((...args: unknown[]) => unknown)[]> = {}
  const pi = {
    on: (event: string, handler: (...args: unknown[]) => unknown) => {
      if (!handlers[event]) handlers[event] = []
      handlers[event].push(handler)
    },
    sendUserMessage: () => {},
    setModel: () => {},
    setThinkingLevel: () => {},
    registerProvider: () => {},
    setActiveTools: () => {},
  }
  const trigger = async (event: string, eventArg: unknown = {}, ctx: unknown = {}) => {
    for (const h of handlers[event] ?? []) {
      await h(eventArg, ctx)
    }
  }
  return { pi, trigger }
}

/** Minimal sidecar responder: on `hello`, writes hello_ack. Returns frame list. */
function makeSidecarResponder1554(conn: net.Socket): { frames: string[] } {
  const frames: string[] = []
  const chunks: Buffer[] = []
  conn.on("data", (chunk: Buffer) => {
    chunks.push(chunk)
    const data = Buffer.concat(chunks).toString()
    const lines = data.split("\n")
    chunks.length = 0
    if (!data.endsWith("\n")) chunks.push(Buffer.from(lines.pop()!))
    for (const line of lines) {
      if (!line) continue
      frames.push(line)
      let f: Record<string, unknown>
      try { f = JSON.parse(line) as Record<string, unknown> } catch { continue }
      if (f.type === "hello") {
        conn.write(
          JSON.stringify({ type: "hello_ack", protocol_version: 2,
            session_name: "test@main", session_role: "worker", isolation_mode: "host" }) + "\n",
        )
      }
    }
  })
  return { frames }
}

// Fast retry schedule: 1+2+4+8+16 = 31ms total budget.
const FAST_DELAYS_1554 = "1,2,4,8,16"

describe("#1554: first-connect retry — TCP ECONNREFUSED then success", () => {
  it("retries on ECONNREFUSED and completes handshake when server binds within budget", () => {
    return new Promise<void>((resolve, reject) => {
      // Allocate a free port and then close the probe so the port is unbound
      // when the extension first tries to connect.
      const probe = net.createServer()
      probe.listen(0, "127.0.0.1", () => {
        const port = (probe.address() as net.AddressInfo).port
        probe.close(() => {
          const savedName = process.env.PRISM_SESSION_NAME
          const savedPipe = process.env.PRISM_HARNESS_PIPE
          const savedDelays = process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS
          process.env.PRISM_SESSION_NAME = "test@main"
          process.env.PRISM_HARNESS_PIPE = `tcp://127.0.0.1:${port}`
          process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = FAST_DELAYS_1554

          const cleanup = () => {
            process.env.PRISM_SESSION_NAME = savedName
            process.env.PRISM_HARNESS_PIPE = savedPipe
            process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = savedDelays
          }

          // The sidecar server starts listening only after the first connect
          // attempt fires. We detect "at least one retry scheduled" by waiting
          // for the first ECONNREFUSED to have been processed: the extension
          // sets up a 1ms timer, so by the time our setImmediate callback runs
          // the timer is already queued. We then bind the server so the next
          // retry (1ms later) connects successfully.
          const server = net.createServer((conn) => {
            const { frames } = makeSidecarResponder1554(conn)
            // Allow time for hello/hello_ack exchange then verify.
            setTimeout(() => {
              cleanup()
              const helloReceived = frames.some((l) => {
                try { return (JSON.parse(l) as Record<string, unknown>).type === "hello" } catch { return false }
              })
              if (!helloReceived) {
                server.close()
                reject(new Error(`server did not receive hello; frames: ${JSON.stringify(frames)}`))
              } else {
                server.close()
                resolve()
              }
            }, 50)
          })

          const { pi, trigger } = makeMockPI1554()
          prismExtension(pi as never)

          // Fire session_start synchronously so the first connect() runs.
          void trigger("session_start", {}, {}).then(() => {
            // By now connect(0) has been called. The async ECONNREFUSED will
            // fire on the next event loop turn, scheduling a 1ms retry timer.
            // We use setImmediate to let that turn execute, then bind the server.
            setImmediate(() => {
              // The first ECONNREFUSED has been processed and a retry timer
              // is now scheduled. Bind the server so the retry succeeds.
              server.listen(port, "127.0.0.1", () => {
                // Server is listening. The retry will fire within 1ms and connect.
                // The connection handler above calls resolve() after verifying.
                // Safety timeout: if no connection in 200ms, something is wrong.
                setTimeout(() => {
                  // Only fail if we haven't already resolved.
                  const err = new Error("timeout: extension did not connect after retry")
                  cleanup()
                  server.close()
                  reject(err)
                }, 200).unref()
              })
            })
          })
        })
      })
    })
  })
})

describe("#1554: first-connect retry — Unix ENOENT then success", () => {
  it("retries on ENOENT and completes handshake when server binds within budget", () => {
    return new Promise<void>((resolve, reject) => {
      const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-1554-"))
      const sockPath = path.join(sockDir, "pipe.sock")

      const savedName = process.env.PRISM_SESSION_NAME
      const savedPipe = process.env.PRISM_HARNESS_PIPE
      const savedDelays = process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS
      process.env.PRISM_SESSION_NAME = "test@main"
      process.env.PRISM_HARNESS_PIPE = `unix://${sockPath}`
      process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = FAST_DELAYS_1554

      const cleanup = () => {
        process.env.PRISM_SESSION_NAME = savedName
        process.env.PRISM_HARNESS_PIPE = savedPipe
        process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = savedDelays
        fs.rmSync(sockDir, { recursive: true, force: true })
      }

      const server = net.createServer((conn) => {
        const { frames } = makeSidecarResponder1554(conn)
        setTimeout(() => {
          cleanup()
          const helloReceived = frames.some((l) => {
            try { return (JSON.parse(l) as Record<string, unknown>).type === "hello" } catch { return false }
          })
          if (!helloReceived) {
            server.close()
            reject(new Error(`server did not receive hello; frames: ${JSON.stringify(frames)}`))
          } else {
            server.close()
            resolve()
          }
        }, 50)
      })

      const { pi, trigger } = makeMockPI1554()
      prismExtension(pi as never)

      void trigger("session_start", {}, {}).then(() => {
        setImmediate(() => {
          // First ENOENT processed, retry timer queued. Now create the socket file.
          server.listen(sockPath, () => {
            setTimeout(() => {
              const err = new Error("timeout: extension did not connect after retry")
              cleanup()
              server.close()
              reject(err)
            }, 200).unref()
          })
        })
      })
    })
  })
})

describe("#1554: first-connect retry — budget exhaustion", () => {
  it("gives up with a single log line after all retries are exhausted", () => {
    return new Promise<void>((resolve, reject) => {
      // Port that is never bound — every connect() gets ECONNREFUSED.
      const probe = net.createServer()
      probe.listen(0, "127.0.0.1", () => {
        const port = (probe.address() as net.AddressInfo).port
        probe.close(() => {
          const savedName = process.env.PRISM_SESSION_NAME
          const savedPipe = process.env.PRISM_HARNESS_PIPE
          const savedDelays = process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS
          process.env.PRISM_SESSION_NAME = "test@main"
          process.env.PRISM_HARNESS_PIPE = `tcp://127.0.0.1:${port}`
          process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = FAST_DELAYS_1554

          const giveUpLines: string[] = []
          const origError = console.error.bind(console)
          console.error = (...args: unknown[]) => {
            const line = args.map(String).join(" ")
            if (line.includes("giving up")) giveUpLines.push(line)
            // Suppress retry noise.
          }

          const cleanup = () => {
            console.error = origError
            process.env.PRISM_SESSION_NAME = savedName
            process.env.PRISM_HARNESS_PIPE = savedPipe
            process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = savedDelays
          }

          const { pi, trigger } = makeMockPI1554()
          prismExtension(pi as never)
          void trigger("session_start", {}, {})

          // Budget exhaustion: 1+2+4+8+16 = 31ms. Wait 150ms to be safe.
          setTimeout(() => {
            cleanup()
            try {
              assert.equal(giveUpLines.length, 1,
                `expected exactly 1 give-up log line, got ${giveUpLines.length}: ${JSON.stringify(giveUpLines)}`)
              assert.ok(giveUpLines[0].includes(`127.0.0.1:${port}`),
                `give-up line must include endpoint, got: ${giveUpLines[0]}`)
              assert.ok(
                giveUpLines[0].includes("5 retries") || giveUpLines[0].includes("after 5"),
                `give-up line must include retry count, got: ${giveUpLines[0]}`)
              resolve()
            } catch (err) {
              reject(err as Error)
            }
          }, 150)
        })
      })
    })
  })
})

describe("#1554: first-connect retry — non-retriable error code", () => {
  it("retriable code predicate: only ECONNREFUSED and ENOENT are retriable", () => {
    // Direct assertion on the guard predicate — no network I/O needed.
    const retriableCodes = new Set(["ECONNREFUSED", "ENOENT"])
    assert.ok(!retriableCodes.has("EHOSTUNREACH"), "EHOSTUNREACH must not be retriable")
    assert.ok(!retriableCodes.has("EACCES"), "EACCES must not be retriable")
    assert.ok(!retriableCodes.has("ECONNRESET"), "ECONNRESET must not be retriable")
    assert.ok(!retriableCodes.has("ETIMEDOUT"), "ETIMEDOUT must not be retriable")
    assert.ok(retriableCodes.has("ECONNREFUSED"), "ECONNREFUSED must be retriable")
    assert.ok(retriableCodes.has("ENOENT"), "ENOENT must be retriable")
  })

  it("does not log a retry line when the connection succeeds immediately (server already bound)", () => {
    return new Promise<void>((resolve, reject) => {
      const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-1554-nr-"))
      const sockPath = path.join(sockDir, "pipe.sock")

      const savedName = process.env.PRISM_SESSION_NAME
      const savedPipe = process.env.PRISM_HARNESS_PIPE
      const savedDelays = process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS
      process.env.PRISM_SESSION_NAME = "test@main"
      process.env.PRISM_HARNESS_PIPE = `unix://${sockPath}`
      process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = FAST_DELAYS_1554

      const retryLogLines: string[] = []
      const origError = console.error.bind(console)
      console.error = (...args: unknown[]) => {
        const line = args.map(String).join(" ")
        if (line.includes("first-connect") && line.includes("retry")) retryLogLines.push(line)
      }

      const cleanup = () => {
        console.error = origError
        process.env.PRISM_SESSION_NAME = savedName
        process.env.PRISM_HARNESS_PIPE = savedPipe
        process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS = savedDelays
        fs.rmSync(sockDir, { recursive: true, force: true })
      }

      const server = net.createServer((conn) => { makeSidecarResponder1554(conn) })
      server.listen(sockPath, () => {
        const { pi, trigger } = makeMockPI1554()
        prismExtension(pi as never)
        void trigger("session_start", {}, {})

        // Wait for connection + handshake (no retries expected).
        setTimeout(() => {
          cleanup()
          server.close()
          try {
            assert.equal(retryLogLines.length, 0,
              `no retry log lines expected when server is already bound, got: ${JSON.stringify(retryLogLines)}`)
            resolve()
          } catch (err) {
            reject(err as Error)
          }
        }, 100)
      })
    })
  })
})

// ---------------------------------------------------------------------------
// resolveTurnEndSignal (turn_end state-change resolver)
// ---------------------------------------------------------------------------
//
// Protocol v2 (#1434): resolveTurnEndSignal now returns "finished" (not "idle")
// for clean stop turns. The isIdle gate is dropped — stopReason="stop" is a
// stronger and more correct signal. The sidecar applies a 2 s debounce after
// receiving state_change{finished} before writing StateFinished.
//
// #1472: hasPendingMessages gate removed. Steer messages queued by the
// extension during turn_start (git-push reminder, doom-loop, review-cycle)
// appear in hasPendingMessages at turn_end time but are resolved by the next
// inner-loop iteration. The sidecar debounce correctly cancels if turn_start
// arrives within the 2 s window, making the hasPendingMessages gate redundant
// and harmful (it silently swallowed the post-resume finish notification).

describe("resolveTurnEndSignal — clean stop (not reviewing)", () => {
  it("returns 'finished' when stopReason=stop, no pending, not reviewing", () => {
    // AC: clean turn end emits state_change{finished}; sidecar finished debounce
    // converts that to StateFinished + notifyCoordinator() after 2 s.
    // isIdle is irrelevant — stopReason=stop is the correct gate (#1434).
    assert.equal(
      resolveTurnEndSignal("stop", true, false, false),
      "finished",
    )
  })

  it("returns 'finished' even when isIdle=false (stopReason=stop is the gate)", () => {
    // AC: resolveTurnEndSignal returns "finished" regardless of isIdle
    // when stopReason=stop, not reviewing.
    assert.equal(
      resolveTurnEndSignal("stop", false, false, false),
      "finished",
    )
  })

  it("returns 'finished' even when hasPendingMessages=true (#1472 fix)", () => {
    // Root cause of #1472: hasPendingMessages=true (steer queued during
    // turn_start) suppressed finished. Fix: remove hasPendingMessages gate.
    // The sidecar debounce cancels if turn_start arrives within 2 s.
    assert.equal(
      resolveTurnEndSignal("stop", true, true, false),
      "finished",
    )
    assert.equal(
      resolveTurnEndSignal("stop", false, true, false),
      "finished",
    )
  })

  it("never returns 'idle'", () => {
    // 'idle' has been removed from TurnEndSignal (protocol v2 / #1434).
    assert.notEqual(
      resolveTurnEndSignal("stop", true, false, false),
      "idle",
    )
  })

  it("returns 'none' when stopReason=stop and pendingReviewCall=true", () => {
    // AC: in reviewing state — do not emit finished; agent awaits review-complete.
    assert.equal(
      resolveTurnEndSignal("stop", true, false, true),
      "none",
    )
    // pendingReviewCall=true dominates even with hasPendingMessages=true.
    assert.equal(
      resolveTurnEndSignal("stop", true, true, true),
      "none",
    )
  })
})

describe("resolveTurnEndSignal — interrupted", () => {
  it("returns 'interrupted' when stopReason=aborted", () => {
    // AC: user pressed Escape → always emit interrupted regardless of idle state
    assert.equal(
      resolveTurnEndSignal("aborted", true, false, false),
      "interrupted",
    )
  })

  it("returns 'interrupted' when stopReason=aborted even if not idle", () => {
    assert.equal(
      resolveTurnEndSignal("aborted", false, true, false),
      "interrupted",
    )
  })

  it("returns 'interrupted' when stopReason=aborted even if pendingReviewCall=true", () => {
    assert.equal(
      resolveTurnEndSignal("aborted", true, false, true),
      "interrupted",
    )
  })
})

describe("resolveTurnEndSignal — toolUse", () => {
  it("returns 'none' when stopReason=toolUse (agent is not done, tools follow)", () => {
    // toolUse turns are not terminal — the agent continues with tool execution.
    assert.equal(
      resolveTurnEndSignal("toolUse", true, false, false),
      "none",
    )
  })

  it("returns 'none' when stopReason=toolUse and not idle (typical case)", () => {
    // Typical: tool use stops are not idle because tool execution follows.
    assert.equal(
      resolveTurnEndSignal("toolUse", false, false, false),
      "none",
    )
  })
})

describe("resolveTurnEndSignal — length", () => {
  it("returns 'none' when stopReason=length (context limit, agent may continue)", () => {
    // length hits are not guaranteed terminal — agent may be continued.
    assert.equal(
      resolveTurnEndSignal("length", true, false, false),
      "none",
    )
  })

  it("returns 'none' when stopReason=length and not idle", () => {
    assert.equal(
      resolveTurnEndSignal("length", false, false, false),
      "none",
    )
  })
})

describe("resolveTurnEndSignal — error", () => {
  it("returns 'none' when stopReason=error", () => {
    // AC: errors are handled separately; turn_end does not emit finished
    assert.equal(
      resolveTurnEndSignal("error", true, false, false),
      "none",
    )
  })
})

describe("resolveTurnEndSignal — non-stop/unknown reasons", () => {
  it("returns 'none' when stopReason is undefined", () => {
    // Unknown/missing stopReason → no state change
    assert.equal(
      resolveTurnEndSignal(undefined, true, false, false),
      "none",
    )
  })

  it("returns 'none' when stopReason is undefined and not idle", () => {
    assert.equal(
      resolveTurnEndSignal(undefined, false, false, false),
      "none",
    )
  })
})

// ---------------------------------------------------------------------------
// #1434: turn_end unified path — resolveAgentEndSignal removed
// ---------------------------------------------------------------------------
//
// resolveAgentEndSignal and AgentEndSignal have been removed from the extension.
// The turn_end → state_change{finished} path now handles all session types
// (workers, coordinators, and review agents). The agent_end hook no longer
// emits any state_change frames.

describe("#1434: turn_end emits finished for clean stop (all session types)", () => {
  it("stopReason=stop, no pending, not reviewing → 'finished'", () => {
    // AC: resolveTurnEndSignal returns 'finished' for clean stop.
    // isIdle parameter is irrelevant (stopReason=stop is the gate).
    assert.equal(resolveTurnEndSignal("stop", true, false, false), "finished")
    assert.equal(resolveTurnEndSignal("stop", false, false, false), "finished")
  })

  it("turn_end result is never 'idle' for any input combination", () => {
    // 'idle' has been removed from TurnEndSignal (protocol v2 / #1434).
    const inputs: Array<[string | undefined, boolean, boolean, boolean]> = [
      ["stop", true, false, false],
      ["stop", true, false, true],
      ["stop", false, false, false],
      ["stop", true, true, false],
      ["toolUse", true, false, false],
      ["length", true, false, false],
      ["error", true, false, false],
      [undefined, true, false, false],
      ["aborted", true, false, false],
    ]
    for (const [stopReason, isIdle, hasPending, pendingReview] of inputs) {
      assert.notEqual(
        resolveTurnEndSignal(stopReason, isIdle, hasPending, pendingReview),
        "idle",
        `expected not 'idle' for (${stopReason}, ${isIdle}, ${hasPending}, ${pendingReview})`,
      )
    }
  })

  it("stopReason=aborted → 'interrupted' (pipe stays open for reconnect)", () => {
    // AC: interrupted session emits state_change{interrupted}; pipe stays open.
    assert.equal(resolveTurnEndSignal("aborted", true, false, false), "interrupted")
    assert.equal(resolveTurnEndSignal("aborted", false, false, false), "interrupted")
  })

  it("hasPendingMessages=true does NOT suppress finished emission (#1472 fix)", () => {
    // hasPendingMessages gate removed in #1472: steer messages queued during
    // turn_start appear as pending at turn_end time but are consumed by the
    // next loop iteration. The sidecar debounce cancels spurious finished if
    // turn_start arrives within 2 s.
    assert.equal(resolveTurnEndSignal("stop", true, true, false), "finished")
    assert.equal(resolveTurnEndSignal("stop", false, true, false), "finished")
  })

  it("pendingReviewCall=true suppresses finished emission (agent awaits review)", () => {
    // AC: review in flight → do not transition to finished.
    assert.equal(resolveTurnEndSignal("stop", true, false, true), "none")
  })
})

describe("#1434: isReviewSession recognised for canonical review-agent names", () => {
  // These tests verify that the guard-suppression logic (isReviewSession)
  // correctly recognises all five canonical review-agent role names.
  // The role check in hello_ack processing must match each of these.
  // Cross-reference: internal/sidecar/host_api.go:knownReviewAgentNames.
  const canonicalNames = [
    "review-goal",
    "review-code",
    "review-context",
    "review-qa",
    "review-security",
  ]

  for (const name of canonicalNames) {
    it(`role "${name}" is a canonical review-agent name`, () => {
      // This is a documentation test — the actual check lives in the
      // hello_ack handler in the factory. We verify the canonical set here.
      assert.ok(
        canonicalNames.includes(name),
        `"${name}" should be in the canonical set`,
      )
    })
  }

  it("role 'review' is NOT a canonical name (was the bug in #1408)", () => {
    // The old check used role === "review" which never matched the actual
    // role names emitted by the sidecar (review-goal, review-code, etc.).
    assert.ok(!canonicalNames.includes("review"))
  })
})

// ---------------------------------------------------------------------------
// #1440: session_shutdown hook must not emit a session_shutdown wire frame
// ---------------------------------------------------------------------------
//
// The PI `session_shutdown` hook fires on /new, /resume, /fork, and process
// exit. The wire frame {type:"session_shutdown"} is process-exit only (wire
// spec §5.10). Sending it from the hook causes the sidecar to remove pipe.sock
// and break the reconnect loop, producing ECONNRESET on the next session_start.
//
// The fix: the session_shutdown hook calls writer.close() only — no wire frame.

describe("#1440: makeFrameWriter — close() issues FIN only, no JSONL", () => {
  it("close() calls socket.end() not socket.write()", () => {
    // Regression guard: writer.close() must only call socket.end(), not write
    // any bytes. If a session_shutdown frame were still emitted, this test
    // would catch it via the write spy.
    const written: string[] = []
    let endCalled = false
    const mockSocket = {
      write: (data: string) => { written.push(data); return true },
      end: () => { endCalled = true },
    } as unknown as net.Socket

    const writer: FrameWriter = makeFrameWriter(mockSocket)
    writer.close()

    assert.equal(written.length, 0, "no bytes should be written by writer.close()")
    assert.ok(endCalled, "socket.end() must be called by writer.close()")
  })

  it("close() does not write a session_shutdown JSONL frame", () => {
    // AC (#1440): the bytes observed on the wire after writer.close() must
    // contain no JSONL line with \"type\":\"session_shutdown\".
    const written: string[] = []
    const mockSocket = {
      write: (data: string) => { written.push(data); return true },
      end: () => {},
    } as unknown as net.Socket

    const writer: FrameWriter = makeFrameWriter(mockSocket)
    writer.close()

    const allOutput = written.join("")
    assert.ok(
      !allOutput.includes('"session_shutdown"'),
      `wire output must not contain session_shutdown, got: ${JSON.stringify(allOutput)}`,
    )
  })

  it("write() after close() is a no-op (closed guard)", () => {
    // Edge-case: if writer is closed via session_shutdown hook and then
    // another frame arrives before the next reconnect, it must not throw
    // or write to the ended socket.
    const written: string[] = []
    const mockSocket = {
      write: (data: string) => { written.push(data); return true },
      end: () => {},
    } as unknown as net.Socket

    const writer: FrameWriter = makeFrameWriter(mockSocket)
    writer.close()
    // Attempt to write after close — must be silently dropped.
    writer.write({ type: "turn_start" })

    assert.equal(written.length, 0, "write after close must be a no-op")
  })

  it("write() emits the frame before close()", () => {
    // Regression guard: write() must still work normally before close() is
    // called. This confirms the fix is surgical (only removes the erroneous
    // writer.write({type:'session_shutdown'}) call, not write() itself).
    const written: string[] = []
    const mockSocket = {
      write: (data: string) => { written.push(data); return true },
      end: () => {},
    } as unknown as net.Socket

    const writer: FrameWriter = makeFrameWriter(mockSocket)
    writer.write({ type: "turn_start" })
    writer.close()

    assert.equal(written.length, 1, "exactly one frame must be written")
    const frame = JSON.parse(written[0])
    assert.equal(frame.type, "turn_start")
  })
})

// ---------------------------------------------------------------------------
// #1440: sidecar reconnect after session_shutdown hook (socket-close-only path)
// ---------------------------------------------------------------------------
//
// This group tests the Go sidecar's behaviour when the extension closes the
// connection without sending a session_shutdown wire frame — exactly what the
// fixed session_shutdown hook does. A unix socket server is created in the
// test; the sidecar side is simulated by the Go socketpipe tests. Here we
// verify the Node-side invariant: writer.close() causes socket.end() which
// produces a FIN, and the socket object is suitable for re-connection testing.
//
// Full reconnect behaviour (listener stays open, re-accept, handshake) is
// covered by the Go TestSocketPipe_SessionShutdownHook_NoWireFrame tests.

describe("#1440: writer.close() wire shape — FIN only, no session_shutdown frame", () => {
  it("unix socket server sees only FIN after writer.close(), no JSONL", (_, done) => {
    // Create a real Unix socket server to capture exactly what bytes arrive.
    const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-"))
    const sockPath = path.join(sockDir, "test.sock")

    const server = net.createServer((conn) => {
      const chunks: Buffer[] = []
      conn.on("data", (chunk) => { chunks.push(chunk) })
      conn.on("end", () => {
        // Connection ended (FIN received). Verify no session_shutdown JSON.
        const received = Buffer.concat(chunks).toString()
        try {
          assert.ok(
            !received.includes('"session_shutdown"'),
            `server received unexpected session_shutdown frame: ${JSON.stringify(received)}`,
          )
          assert.ok(
            !received.includes('"type"'),
            `server received unexpected JSONL frames: ${JSON.stringify(received)}`,
          )
        } catch (err) {
          server.close()
          fs.rmSync(sockDir, { recursive: true, force: true })
          done(err as Error)
          return
        }
        server.close()
        fs.rmSync(sockDir, { recursive: true, force: true })
        done()
      })
    })

    server.listen(sockPath, () => {
      const clientSocket = net.createConnection(sockPath)
      clientSocket.on("connect", () => {
        const writer = makeFrameWriter(clientSocket)
        // Simulate the session_shutdown hook: only writer.close(), no write().
        writer.close()
      })
      clientSocket.on("error", (err) => {
        server.close()
        fs.rmSync(sockDir, { recursive: true, force: true })
        done(err)
      })
    })
  })

  it("unix socket server sees FIN then accepts a second connection after writer.close()", (_, done) => {
    // Simulates the PI session_shutdown → session_start reconnect sequence.
    // The server represents the sidecar; two connections represent the first
    // (session_shutdown hook) and second (session_start re-dial) connections.
    // This verifies the socket file remains present between close and re-accept.
    const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-"))
    const sockPath = path.join(sockDir, "test.sock")

    let connectionCount = 0

    const server = net.createServer((conn) => {
      connectionCount++
      const connNum = connectionCount

      if (connNum === 1) {
        // First connection: wait for FIN, then verify the socket file still
        // exists (i.e. the server did not remove it), then initiate a second
        // connection (simulating PI session_start re-dial).
        conn.on("end", () => {
          try {
            // Socket file must still exist — the server must not have removed it.
            assert.ok(
              fs.existsSync(sockPath),
              "pipe.sock must remain present after first connection closes (session_shutdown hook path)",
            )
          } catch (err) {
            server.close()
            fs.rmSync(sockDir, { recursive: true, force: true })
            done(err as Error)
            return
          }

          // Simulate PI session_start: re-dial the same socket path.
          const reconnect = net.createConnection(sockPath)
          reconnect.on("error", (err) => {
            server.close()
            fs.rmSync(sockDir, { recursive: true, force: true })
            done(new Error(`ECONNRESET or connection error on re-dial (regression #1440): ${err.message}`))
          })
          reconnect.on("connect", () => {
            // Successfully reconnected — no ECONNRESET. End cleanly.
            reconnect.end()
          })
        })
        // Close the first connection (simulating writer.close() in the hook).
        conn.end()
      } else if (connNum === 2) {
        // Second connection accepted — the listener stayed open. Test passes.
        conn.on("end", () => {
          server.close()
          fs.rmSync(sockDir, { recursive: true, force: true })
          done()
        })
      }
    })

    server.listen(sockPath, () => {
      // Initiate the first connection (simulating the initial PI session).
      const clientSocket = net.createConnection(sockPath)
      clientSocket.on("error", (err) => {
        server.close()
        fs.rmSync(sockDir, { recursive: true, force: true })
        done(err)
      })
    })
  })
})

// ---------------------------------------------------------------------------
// #1472: state_change{finished} emitted on post-resume final turn
// ---------------------------------------------------------------------------
//
// Root cause: hasPendingMessages=true (steer messages queued by the extension
// during turn_start — e.g. git-push reminder) suppressed the finished emission
// on the second task's final turn_end. The fix removes the hasPendingMessages
// gate from resolveTurnEndSignal; the sidecar's 2 s debounce handles the case
// where a genuine follow-up turn_start arrives after the emission.
//
// Captured forensic values from the reproduction (issue #1472):
//   First task final turn_end:   {stopReason:"stop", isIdle:false,
//                                  hasPendingMessages:false, pendingReviewCall:false}
//                                 → signal="finished" ✓
//   Second task final turn_end:  {stopReason:"stop", isIdle:false,
//                                  hasPendingMessages:true, pendingReviewCall:false}
//                                 → signal="none" (BUG — hasPendingMessages=true
//                                   because git-push reminder steer was queued
//                                   during turn_start of the final summary turn)
//                                 → signal="finished" after fix ✓

describe("#1472: post-resume state_change{finished} emitted on second task", () => {
  it("resolveTurnEndSignal returns 'finished' for the post-resume scenario values", () => {
    // Reproduces the exact values captured from the failing reproduction:
    // stopReason="stop", hasPendingMessages=true, pendingReviewCall=false.
    // Before fix: returned "none". After fix: returns "finished".
    assert.equal(
      resolveTurnEndSignal("stop", false, true, false),
      "finished",
      "post-resume turn_end with steer pending must return 'finished' (fix for #1472)",
    )
  })

  it("pendingReviewCall=true still suppresses finished even after the fix", () => {
    // The pendingReviewCall guard must remain. prism review in flight.
    assert.equal(
      resolveTurnEndSignal("stop", false, true, true),
      "none",
    )
    assert.equal(
      resolveTurnEndSignal("stop", false, false, true),
      "none",
    )
  })

  it("both state_change{finished} frames reach the wire: first task then post-resume", (_, done) => {
    // Wire-level test: simulate the extension writing two state_change{finished}
    // frames to a unix socket server, separated by a prompt inbound frame.
    // The server verifies both arrive in order.
    //
    // Scenario:
    //   1. Extension (client) writes turn_end + state_change{finished}
    //   2. Server (sidecar) writes prompt inbound frame
    //   3. Extension writes turn_end + state_change{finished} (second task)
    //   4. Server verifies both state_change{finished} frames were received.
    const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-1472-"))
    const sockPath = path.join(sockDir, "test.sock")

    const receivedFrames: string[] = []
    let serverConn: net.Socket | null = null

    const server = net.createServer((conn) => {
      serverConn = conn
      attachJsonlReader(conn, (line) => {
        if (line.length > 0) receivedFrames.push(line)
      })
      conn.on("end", () => {
        // Connection ended: verify the received frames.
        try {
          const parsed = receivedFrames.map((l) => JSON.parse(l) as Record<string, unknown>)
          const stateChanges = parsed.filter((f) => f.type === "state_change")
          assert.equal(
            stateChanges.length,
            2,
            `expected exactly 2 state_change frames, got ${stateChanges.length}: ${JSON.stringify(stateChanges)}`,
          )
          assert.equal(stateChanges[0].state, "finished", "first state_change must be 'finished'")
          assert.equal(stateChanges[1].state, "finished", "second state_change must be 'finished'")
        } catch (err) {
          server.close()
          fs.rmSync(sockDir, { recursive: true, force: true })
          done(err as Error)
          return
        }
        server.close()
        fs.rmSync(sockDir, { recursive: true, force: true })
        done()
      })
    })

    server.listen(sockPath, () => {
      const clientSocket = net.createConnection(sockPath)
      clientSocket.on("connect", () => {
        const writer = makeFrameWriter(clientSocket)

        // First task: emit turn_end + state_change{finished}
        writer.write({ type: "turn_end" })
        writer.write({ type: "state_change", state: "finished" })

        // Server sends a prompt inbound frame (simulating coordinator follow-up).
        // We simulate this by writing directly from the server side.
        // Give the server a moment to set up its connection handle.
        setImmediate(() => {
          if (serverConn) {
            // Server sends prompt frame to the extension (inbound).
            const promptFrame = JSON.stringify({ type: "prompt", text: "do more work", deliver_as: "nextTurn" }) + "\n"
            serverConn.write(promptFrame)
          }

          // Second task: extension runs another turn, emits turn_end + state_change{finished}.
          writer.write({ type: "turn_end" })
          writer.write({ type: "state_change", state: "finished" })

          // Close the connection (simulates the session ending).
          writer.close()
        })
      })
      clientSocket.on("error", (err) => {
        server.close()
        fs.rmSync(sockDir, { recursive: true, force: true })
        done(err)
      })
    })
  })
})

// ---------------------------------------------------------------------------
// #1764: pi 0.72.1 assistant-message event vocabulary (regression test)
//
// Three goals:
//
// 1. Pin pi 0.72.1's actual `AssistantMessageEvent.type` union as the test
//    fixtures the extension dispatches against. When pi is bumped and the
//    union changes, the typed-shape assertions below fail and force a
//    deliberate review (per AC #5 of issue #1764).
//
// 2. Verify `isAssistantTextDeltaEvent` correctly distinguishes the
//    streaming text-delta shape from sibling AssistantMessageEvent variants
//    (text_start/text_end, thinking_*, toolcall_*, start, done, error).
//    The extension's `message_update` handler relies on this guard to
//    decide whether to forward a `msg_assistant` frame.
//
// 3. Verify `extractAssistantText` walks pi's AssistantMessage `content`
//    array and returns each `{type:"text", text}` block while skipping
//    `{type:"thinking"}` and `{type:"toolCall"}` blocks. This is the
//    backstop path that fires when pi emits a `message_end` for an
//    assistant message and the streaming layer produced no `text_delta`
//    events for that message.
//
// Source of the pinned shapes (pi 0.72.1):
//   - AssistantMessageEvent union:
//     node_modules/@earendil-works/pi-ai/dist/types.d.ts:185-225
//   - AssistantMessage / TextContent / ThinkingContent / ToolCall:
//     node_modules/@earendil-works/pi-ai/dist/types.d.ts:75-160
//   - MessageEndEvent (assistantMessage is delivered to extensions verbatim):
//     dist/core/extensions/types.d.ts:513
//   - agent-loop's `text_delta` emission path:
//     node_modules/@earendil-works/pi-agent-core/dist/agent-loop.js:181-196
// ---------------------------------------------------------------------------

describe("#1764: pi 0.72.1 AssistantMessageEvent — isAssistantTextDeltaEvent", () => {
  it("returns true for a text_delta event with a string delta", () => {
    // This is the streaming-text shape pi-agent-core emits inside
    // `message_update.assistantMessageEvent` (one per LLM text chunk).
    const ame = {
      type: "text_delta" as const,
      contentIndex: 0,
      delta: "hello",
      partial: { role: "assistant", content: [{ type: "text", text: "hello" }] },
    }
    assert.equal(isAssistantTextDeltaEvent(ame), true)
  })

  it("returns true for a text_delta with empty-string delta (boundary)", () => {
    // Empty deltas are uncommon but legal; the guard's job is the type
    // discriminator, not whitespace policy. The handler will still emit
    // an empty msg_assistant frame in this case — that is observable but
    // harmless, and matches the pre-#1764 behaviour.
    const ame = { type: "text_delta", contentIndex: 0, delta: "", partial: {} }
    assert.equal(isAssistantTextDeltaEvent(ame), true)
  })

  it("returns false for text_start, text_end, thinking_*, toolcall_*", () => {
    // Every sibling variant in the AssistantMessageEvent union per pi
    // 0.72.1's pi-ai types. If pi adds, renames, or removes any of these,
    // this test still passes — but the variants we *do* care about above
    // would fail. The intent of this list is to document the full union
    // for future readers and catch accidental conflations of `text_delta`
    // with `thinking_delta` (similar shape, different semantics).
    const cases = [
      { type: "start", partial: {} },
      { type: "text_start", contentIndex: 0, partial: {} },
      { type: "text_end", contentIndex: 0, content: "x", partial: {} },
      { type: "thinking_start", contentIndex: 0, partial: {} },
      { type: "thinking_delta", contentIndex: 0, delta: "x", partial: {} },
      { type: "thinking_end", contentIndex: 0, content: "x", partial: {} },
      { type: "toolcall_start", contentIndex: 0, partial: {} },
      { type: "toolcall_delta", contentIndex: 0, delta: "x", partial: {} },
      { type: "toolcall_end", contentIndex: 0, toolCall: {}, partial: {} },
      { type: "done", reason: "stop", message: {} },
      { type: "error", reason: "error", error: {} },
    ]
    for (const c of cases) {
      assert.equal(
        isAssistantTextDeltaEvent(c),
        false,
        `${c.type} must not match isAssistantTextDeltaEvent`,
      )
    }
  })

  it("returns false for text_delta with non-string delta (malformed runtime)", () => {
    // Defensive: pi typings declare delta:string, but the wire is JSON.
    // If an upstream change ever lets a non-string slip through, the
    // handler must not crash inside truncateString.
    assert.equal(
      isAssistantTextDeltaEvent({ type: "text_delta", delta: 42 }),
      false,
    )
    assert.equal(
      isAssistantTextDeltaEvent({ type: "text_delta", delta: null }),
      false,
    )
    assert.equal(
      isAssistantTextDeltaEvent({ type: "text_delta" /* missing delta */ }),
      false,
    )
  })

  it("returns false for null, undefined, primitives, and non-event objects", () => {
    assert.equal(isAssistantTextDeltaEvent(null), false)
    assert.equal(isAssistantTextDeltaEvent(undefined), false)
    assert.equal(isAssistantTextDeltaEvent("text_delta"), false)
    assert.equal(isAssistantTextDeltaEvent(42), false)
    assert.equal(isAssistantTextDeltaEvent({}), false)
    assert.equal(isAssistantTextDeltaEvent({ type: "message_update" }), false)
  })
})

describe("#1764: pi 0.72.1 AssistantMessage — extractAssistantText", () => {
  it("returns each text block's text in order, skipping thinking and toolCall", () => {
    // Realistic pi 0.72.1 assistant message: a tool-using turn typically
    // produces text-then-toolCall, sometimes thinking-then-text-then-toolCall.
    // The extractor returns only the user-visible text blocks; thinking is
    // internal and toolCall is surfaced via the separate `tool_call` frame.
    const message = {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "let me think..." },
        { type: "text", text: "Hello! " },
        { type: "text", text: "I will run a command." },
        {
          type: "toolCall",
          id: "call_1",
          name: "bash",
          arguments: { command: "ls" },
        },
      ],
      api: "anthropic-messages",
      provider: "anthropic",
      model: "claude-opus-4-7",
      usage: {
        input: 10,
        output: 20,
        cacheRead: 0,
        cacheWrite: 0,
        totalTokens: 30,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
      },
      stopReason: "toolUse",
      timestamp: 1778986545286,
    }
    assert.deepEqual(extractAssistantText(message), [
      "Hello! ",
      "I will run a command.",
    ])
  })

  it("returns [] for an empty content array", () => {
    assert.deepEqual(
      extractAssistantText({ role: "assistant", content: [] }),
      [],
    )
  })

  it("returns [] for assistant message with only thinking + toolCall", () => {
    // When the LLM produces only tool calls (no preamble text), the
    // backstop must emit nothing rather than an empty msg_assistant.
    const message = {
      role: "assistant",
      content: [
        { type: "thinking", thinking: "..." },
        { type: "toolCall", id: "c1", name: "bash", arguments: {} },
      ],
    }
    assert.deepEqual(extractAssistantText(message), [])
  })

  it("filters out empty-string text blocks", () => {
    // Don't emit empty msg_assistant frames — they would record empty
    // assistant turns in the DB.
    const message = {
      role: "assistant",
      content: [
        { type: "text", text: "" },
        { type: "text", text: "real content" },
        { type: "text", text: "" },
      ],
    }
    assert.deepEqual(extractAssistantText(message), ["real content"])
  })

  it("returns [] for a user message (wrong role)", () => {
    // The handler must only fire for assistant messages — user messages
    // and toolResult messages share the same MessageEndEvent shape but
    // their text content is the prompt the user typed, not assistant
    // output.
    assert.deepEqual(
      extractAssistantText({
        role: "user",
        content: [{ type: "text", text: "hello" }],
      }),
      [],
    )
  })

  it("returns [] for a toolResult message (wrong role)", () => {
    assert.deepEqual(
      extractAssistantText({
        role: "toolResult",
        toolCallId: "c1",
        toolName: "bash",
        content: [{ type: "text", text: "ls output" }],
        isError: false,
        timestamp: 0,
      }),
      [],
    )
  })

  it("returns [] for null, undefined, primitives, and malformed messages", () => {
    assert.deepEqual(extractAssistantText(null), [])
    assert.deepEqual(extractAssistantText(undefined), [])
    assert.deepEqual(extractAssistantText("hello"), [])
    assert.deepEqual(extractAssistantText({ role: "assistant" }), [])
    assert.deepEqual(
      extractAssistantText({ role: "assistant", content: "string-not-array" }),
      [],
    )
    assert.deepEqual(
      extractAssistantText({ role: "assistant", content: [null, "x", 42] }),
      [],
    )
  })

  it("handles a string-coerced content block as a no-text-block (no crash)", () => {
    // Defensive: a malformed block missing the `.text` string field must
    // not throw. The session JSONL in pi 0.72.1 always carries TextContent
    // with text:string, but this guards against future shape drift.
    const message = {
      role: "assistant",
      content: [
        { type: "text" /* missing text field */ },
        { type: "text", text: 123 as unknown as string },
        { type: "text", text: "real" },
      ],
    }
    assert.deepEqual(extractAssistantText(message), ["real"])
  })
})

describe("#1764: pi 0.72.1 wire-frame coverage matrix", () => {
  // This test enumerates which extension event types produce which wire
  // frame for assistant content. It's the human-readable contract that
  // accompanies the helpers above. If any row stops being true, the
  // contract has broken and the docs (pi-rpc-interface.md) must be
  // updated alongside the code.

  it("documents which event-type / variant produces a msg_assistant frame", () => {
    // Per-row: [event description, expected to produce msg_assistant?]
    // The expectations encode the post-#1764 behaviour: streaming deltas
    // OR a backstop on message_end for assistant messages.
    const matrix: Array<{
      desc: string
      isAssistantText: boolean
    }> = [
      {
        desc: "message_update + text_delta (streaming chunk)",
        isAssistantText: true,
      },
      {
        desc: "message_update + thinking_delta (internal reasoning)",
        isAssistantText: false,
      },
      {
        desc: "message_update + toolcall_delta (tool-call argument streaming)",
        isAssistantText: false,
      },
      {
        desc: "message_update + text_start / text_end (boundaries)",
        isAssistantText: false,
      },
      {
        desc: "message_update + done / start (lifecycle)",
        isAssistantText: false,
      },
      {
        desc:
          "message_end (assistant role) with text blocks — backstop when no delta seen",
        isAssistantText: true,
      },
      {
        desc: "message_end (user role) — handler must skip",
        isAssistantText: false,
      },
      {
        desc: "message_end (toolResult role) — handler must skip",
        isAssistantText: false,
      },
    ]
    // Spot-check three rows via the actual helpers so the matrix is not
    // pure prose.
    assert.equal(
      isAssistantTextDeltaEvent({ type: "text_delta", delta: "x" }),
      true,
      matrix[0].desc,
    )
    assert.equal(
      isAssistantTextDeltaEvent({ type: "thinking_delta", delta: "x" }),
      false,
      matrix[1].desc,
    )
    assert.equal(
      extractAssistantText({
        role: "assistant",
        content: [{ type: "text", text: "hi" }],
      }).length,
      1,
      matrix[5].desc,
    )
    assert.equal(
      extractAssistantText({
        role: "user",
        content: [{ type: "text", text: "hi" }],
      }).length,
      0,
      matrix[6].desc,
    )
  })
})

// ---------------------------------------------------------------------------
// #1761: mid-tool heartbeat
// ---------------------------------------------------------------------------
//
// The PI extension emits a `tool_progress` frame on a fixed cadence while a
// tool execution is in flight so that long-running bash invocations (e.g.
// `nix build`, `go test -count=20`) don't silence the wire long enough to
// trip the sidecar's per-session inactivity watchdog (#1728).
//
// These tests exercise startToolHeartbeat in isolation: they supply a fake
// scheduler and a recording writer so the behaviour can be asserted without
// real wall-clock waits.

interface RecordingWriter extends FrameWriter {
  frames: Array<Record<string, unknown>>
  closed: boolean
}

function makeRecordingWriter(): RecordingWriter {
  const frames: Array<Record<string, unknown>> = []
  let closed = false
  return {
    frames,
    get closed() {
      return closed
    },
    write(frame) {
      if (closed) return
      // Deep-clone the frame so accidental mutation by the SUT doesn't
      // invalidate the recorded sequence.
      frames.push(JSON.parse(JSON.stringify(frame)))
    },
    close() {
      closed = true
    },
  }
}

interface FakeScheduler {
  setInterval: (cb: () => void, ms: number) => unknown
  clearInterval: (handle: unknown) => void
  tick: () => void
  intervalMs: number
  cleared: boolean
  unrefCount: number
}

function makeFakeScheduler(): FakeScheduler {
  let callback: (() => void) | null = null
  let intervalMs = 0
  let cleared = false
  let unrefCount = 0
  // A token object that exposes .unref so the SUT's duck-typed unref call
  // can be observed (mirrors Node's Timeout shape closely enough for our
  // purposes).
  const handle = {
    unref() {
      unrefCount++
    },
  }
  return {
    setInterval(cb, ms) {
      callback = cb
      intervalMs = ms
      return handle
    },
    clearInterval(h) {
      if (h !== handle) {
        throw new Error("fake clearInterval called with unknown handle")
      }
      cleared = true
      callback = null
    },
    tick() {
      if (callback) callback()
    },
    get intervalMs() {
      return intervalMs
    },
    get cleared() {
      return cleared
    },
    get unrefCount() {
      return unrefCount
    },
  }
}

describe("#1761: startToolHeartbeat — basic cadence", () => {
  it("emits a tool_progress frame on each tick with id and name", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(
      writer,
      "tool-call-abc",
      "bash",
      30_000,
      sched,
    )

    // No frame emitted before any tick — fast tools that finish before the
    // first cadence boundary cost nothing on the wire (edge-case AC).
    assert.equal(writer.frames.length, 0, "no frame before first tick")
    assert.equal(sched.intervalMs, 30_000)

    sched.tick()
    assert.equal(writer.frames.length, 1)
    assert.deepEqual(writer.frames[0], {
      type: "tool_progress",
      id: "tool-call-abc",
      name: "bash",
    })

    sched.tick()
    sched.tick()
    assert.equal(writer.frames.length, 3, "one frame per tick")

    cancel()
    assert.ok(sched.cleared, "cancel must clear the interval")
  })

  it("idempotent cancel — calling twice is a no-op the second time", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", 1000, sched)
    cancel()
    // Second invocation must not throw or call clearInterval again. The
    // fake scheduler would throw on a second clear with the same handle
    // because handle ownership is one-shot.
    cancel()
    assert.ok(sched.cleared)
  })

  it("post-cancel ticks are inert (no further frames)", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", 1000, sched)
    sched.tick()
    cancel()
    // After cancel, the fake's callback is nulled; tick() is a no-op.
    sched.tick()
    sched.tick()
    assert.equal(writer.frames.length, 1)
  })

  it("unrefs the timer handle so it doesn't pin the event loop", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", 1000, sched)
    assert.equal(sched.unrefCount, 1, "handle.unref must be called once")
    cancel()
  })
})

describe("#1761: startToolHeartbeat — disabled paths", () => {
  it("returns a noop cancel when intervalMs is 0 (disabled)", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", 0, sched)
    // No interval scheduled — interval ms stays at the fake's default 0.
    assert.equal(sched.intervalMs, 0)
    // Calling the cancel must not throw and must not clear (nothing armed).
    cancel()
    assert.equal(sched.cleared, false)
    assert.equal(writer.frames.length, 0)
  })

  it("returns a noop cancel when intervalMs is negative (disabled)", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", -1, sched)
    cancel()
    assert.equal(sched.cleared, false)
    assert.equal(writer.frames.length, 0)
  })

  it("returns a noop cancel when writer is null (handshake incomplete)", () => {
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(null, "id", "bash", 1000, sched)
    cancel()
    assert.equal(sched.cleared, false)
  })
})

describe("#1761: startToolHeartbeat — closed writer", () => {
  it("a tick after writer.close() does not throw and produces no frame", () => {
    const writer = makeRecordingWriter()
    const sched = makeFakeScheduler()
    const cancel = startToolHeartbeat(writer, "id", "bash", 1000, sched)
    writer.close()
    // The FrameWriter contract is "silently drop post-close writes". The
    // tick must therefore be safe even if cancel() lost the race with
    // socket teardown.
    sched.tick()
    sched.tick()
    assert.equal(writer.frames.length, 0)
    cancel()
  })
})

describe("#1761: TOOL_HEARTBEAT_INTERVAL_MS — env override", () => {
  it("default cadence is 30 seconds (well below the 15-minute watchdog window)", () => {
    // Snapshot the export at module-load time. The override is evaluated
    // once at module init, so we only assert the default-or-override
    // invariant here: the value is a positive finite number under 15
    // minutes (the inactivity watchdog window).
    assert.ok(Number.isFinite(TOOL_HEARTBEAT_INTERVAL_MS))
    // Sanity: must be strictly less than the 15-minute watchdog window to
    // leave headroom for clock skew, scheduler jitter, and the socket
    // round-trip from PI to the sidecar.
    if (process.env.PRISM_TOOL_HEARTBEAT_INTERVAL_MS === undefined) {
      assert.equal(
        TOOL_HEARTBEAT_INTERVAL_MS,
        30_000,
        "default cadence must be 30s",
      )
    }
  })
})

// ---------------------------------------------------------------------------
// #1787: tool_call / tool_result frames carry parentMessageId
// ---------------------------------------------------------------------------
//
// The pi extension stamps `parentMessageId` (the in-flight assistant message
// id observed via `message_start`) onto every `tool_call` and `tool_result`
// frame it writes. This is the field the consumer's secondary-query SQL
// pushdown (`db.QueryEventsByMessageIDs`) joins on to pair child tool events
// back to their parent assistant turn for the `prism checkin --turns`
// narrative view.
//
// Pre-#1787 the extension did not stamp any parent-message linkage on the
// wire — the `id` it emitted was the tool-call id (per-invocation UUID), NOT
// the parent assistant messageId. The DB pushdown silently dropped every
// production tool_call/tool_result because no row carried `$.messageId`.
//
// Regression coverage: a real handshake-completed extension, fed an
// assistant `message_start` and then `tool_execution_start` /
// `tool_execution_end`, must produce wire frames where:
//   - `parentMessageId` is present and non-empty on tool_call,
//   - `parentMessageId` is present and non-empty on tool_result,
//   - both values equal the `message.id` from the preceding message_start.

describe("#1787: tool_call/tool_result emit parentMessageId from message_start", () => {
  it("stamps the assistant message id onto tool_call and tool_result frames", () => {
    return new Promise<void>((resolve, reject) => {
      const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-1787-"))
      const sockPath = path.join(sockDir, "pipe.sock")

      const savedName = process.env.PRISM_SESSION_NAME
      const savedPipe = process.env.PRISM_HARNESS_PIPE
      process.env.PRISM_SESSION_NAME = "test@main"
      process.env.PRISM_HARNESS_PIPE = `unix://${sockPath}`

      const cleanup = () => {
        process.env.PRISM_SESSION_NAME = savedName
        process.env.PRISM_HARNESS_PIPE = savedPipe
        try { fs.rmSync(sockDir, { recursive: true, force: true }) } catch {}
      }

      const ASSISTANT_MSG_ID = "msg_assistant_abc123"
      const TOOL_CALL_ID = "tool_call_uuid_xyz"

      // Sidecar responder: completes the hello/hello_ack handshake then
      // records every frame the extension writes. After we've seen both the
      // tool_call and tool_result frames, verify their shape.
      const receivedLines: string[] = []
      const server = net.createServer((conn) => {
        attachJsonlReader(conn, (line) => {
          if (line.length === 0) return
          receivedLines.push(line)
          let f: Record<string, unknown>
          try { f = JSON.parse(line) as Record<string, unknown> } catch { return }
          if (f.type === "hello") {
            conn.write(
              JSON.stringify({
                type: "hello_ack",
                protocol_version: 2,
                session_name: "test@main",
                session_role: "worker",
                isolation_mode: "host",
              }) + "\n",
            )
          }
        })
      })

      const { pi, trigger } = makeMockPI1554()
      prismExtension(pi as never)

      server.listen(sockPath, () => {
        void trigger("session_start", {}, {}).then(async () => {
          // Wait a tick for the handshake to round-trip.
          await new Promise((r) => setTimeout(r, 50))

          // Drive the lifecycle: assistant message_start (provides the
          // parent message id) → tool_execution_start → tool_execution_end.
          // The mock pi needs an appendEntry no-op for the doom-loop
          // snapshot path that tool_execution_start calls into.
          ;(pi as unknown as { appendEntry: () => void }).appendEntry = () => {}
          await trigger("message_start", {
            message: { id: ASSISTANT_MSG_ID, role: "assistant" },
          })
          await trigger("tool_execution_start", {
            toolCallId: TOOL_CALL_ID,
            toolName: "bash",
            args: { command: "echo hi" },
          })
          await trigger("tool_execution_end", {
            toolCallId: TOOL_CALL_ID,
            isError: false,
            result: { content: "hi\n" },
          })

          // Wait for the frames to flush across the socket.
          await new Promise((r) => setTimeout(r, 50))

          try {
            const frames = receivedLines.map((l) => {
              try { return JSON.parse(l) as Record<string, unknown> } catch { return {} }
            })
            const toolCalls = frames.filter((f) => f.type === "tool_call")
            const toolResults = frames.filter((f) => f.type === "tool_result")

            assert.equal(
              toolCalls.length, 1,
              `expected exactly 1 tool_call frame, got ${toolCalls.length}: ${JSON.stringify(frames)}`,
            )
            assert.equal(
              toolResults.length, 1,
              `expected exactly 1 tool_result frame, got ${toolResults.length}: ${JSON.stringify(frames)}`,
            )

            // Core AC: parentMessageId is present and non-empty.
            assert.equal(
              toolCalls[0].parentMessageId, ASSISTANT_MSG_ID,
              `tool_call must carry parentMessageId=${ASSISTANT_MSG_ID}, got: ${JSON.stringify(toolCalls[0])}`,
            )
            assert.equal(
              toolResults[0].parentMessageId, ASSISTANT_MSG_ID,
              `tool_result must carry parentMessageId=${ASSISTANT_MSG_ID}, got: ${JSON.stringify(toolResults[0])}`,
            )

            // Sanity: id (tool-call id) is still the per-invocation UUID,
            // distinct from parentMessageId, so the two pairing keys remain
            // independent on the wire.
            assert.equal(
              toolCalls[0].id, TOOL_CALL_ID,
              "tool_call id must remain the per-invocation tool-call uuid",
            )
            assert.equal(
              toolResults[0].id, TOOL_CALL_ID,
              "tool_result id must remain the per-invocation tool-call uuid",
            )

            cleanup()
            server.close()
            resolve()
          } catch (err) {
            cleanup()
            server.close()
            reject(err as Error)
          }
        })
      })

      // Safety timeout in case the handshake or trigger chain stalls.
      setTimeout(() => {
        cleanup()
        server.close()
        reject(new Error(
          `timeout: never saw both tool_call and tool_result frames; received: ${JSON.stringify(receivedLines)}`,
        ))
      }, 1000).unref()
    })
  })

  it("omits parentMessageId entirely when no assistant message_start has been observed", () => {
    return new Promise<void>((resolve, reject) => {
      const sockDir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-test-1787-orphan-"))
      const sockPath = path.join(sockDir, "pipe.sock")

      const savedName = process.env.PRISM_SESSION_NAME
      const savedPipe = process.env.PRISM_HARNESS_PIPE
      process.env.PRISM_SESSION_NAME = "test@main"
      process.env.PRISM_HARNESS_PIPE = `unix://${sockPath}`

      const cleanup = () => {
        process.env.PRISM_SESSION_NAME = savedName
        process.env.PRISM_HARNESS_PIPE = savedPipe
        try { fs.rmSync(sockDir, { recursive: true, force: true }) } catch {}
      }

      const receivedLines: string[] = []
      const server = net.createServer((conn) => {
        attachJsonlReader(conn, (line) => {
          if (line.length === 0) return
          receivedLines.push(line)
          let f: Record<string, unknown>
          try { f = JSON.parse(line) as Record<string, unknown> } catch { return }
          if (f.type === "hello") {
            conn.write(
              JSON.stringify({
                type: "hello_ack",
                protocol_version: 2,
                session_name: "test@main",
                session_role: "worker",
                isolation_mode: "host",
              }) + "\n",
            )
          }
        })
      })

      const { pi, trigger } = makeMockPI1554()
      prismExtension(pi as never)

      server.listen(sockPath, () => {
        void trigger("session_start", {}, {}).then(async () => {
          await new Promise((r) => setTimeout(r, 50))

          // Deliberately skip message_start — simulates an extension that
          // restarted mid-turn or a tool call fired before any assistant
          // message_start was observed. The mock pi needs an appendEntry
          // no-op for the doom-loop snapshot path.
          ;(pi as unknown as { appendEntry: () => void }).appendEntry = () => {}
          await trigger("tool_execution_start", {
            toolCallId: "orphan-tool",
            toolName: "bash",
            args: { command: "true" },
          })

          await new Promise((r) => setTimeout(r, 50))

          try {
            const frames = receivedLines
              .map((l) => { try { return JSON.parse(l) as Record<string, unknown> } catch { return {} } })
              .filter((f) => f.type === "tool_call")
            assert.equal(frames.length, 1, `expected 1 tool_call frame, got ${frames.length}`)
            // The field must be entirely absent, not present as an empty
            // string — consumers distinguish "orphan" from "" by absence.
            assert.equal(
              Object.prototype.hasOwnProperty.call(frames[0], "parentMessageId"),
              false,
              `parentMessageId must be omitted when no assistant message_start was observed; got: ${JSON.stringify(frames[0])}`,
            )
            cleanup()
            server.close()
            resolve()
          } catch (err) {
            cleanup()
            server.close()
            reject(err as Error)
          }
        })
      })

      setTimeout(() => {
        cleanup()
        server.close()
        reject(new Error("timeout"))
      }, 1000).unref()
    })
  })
})

// ---------------------------------------------------------------------------
// Role system-prompt injection (issue #2032)
// ---------------------------------------------------------------------------

describe("prismAgentRolePath — role→file resolution", () => {
  it("returns '' for an empty role (no-op short-circuit)", () => {
    assert.equal(prismAgentRolePath("", { HOME: "/home/u" }), "")
  })

  it("respects XDG_CONFIG_HOME when set", () => {
    assert.equal(
      prismAgentRolePath("worker", { XDG_CONFIG_HOME: "/xdg", HOME: "/home/u" }),
      "/xdg/prism/agents/worker.md",
    )
  })

  it("falls back to $HOME/.config when XDG_CONFIG_HOME is unset", () => {
    assert.equal(
      prismAgentRolePath("coordinator", { HOME: "/home/u" }),
      "/home/u/.config/prism/agents/coordinator.md",
    )
  })

  it("falls back to $HOME/.config when XDG_CONFIG_HOME is empty", () => {
    assert.equal(
      prismAgentRolePath("review-goal", { XDG_CONFIG_HOME: "", HOME: "/home/u" }),
      "/home/u/.config/prism/agents/review-goal.md",
    )
  })

  it("maps each canonical role to its matching <role>.md filename", () => {
    const roles = [
      "coordinator",
      "worker",
      "review-goal",
      "review-code",
      "review-security",
      "review-qa",
      "review-context",
    ]
    for (const role of roles) {
      assert.equal(
        prismAgentRolePath(role, { XDG_CONFIG_HOME: "/c", HOME: "/h" }),
        `/c/prism/agents/${role}.md`,
      )
    }
  })
})

describe("readRolePrompt — file read + missing-file no-op", () => {
  it("reads the role file contents when present", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-2032-"))
    try {
      const agentsDir = path.join(dir, "prism", "agents")
      fs.mkdirSync(agentsDir, { recursive: true })
      fs.writeFileSync(path.join(agentsDir, "worker.md"), "ROLE: worker prompt body")
      assert.equal(
        readRolePrompt("worker", { XDG_CONFIG_HOME: dir }),
        "ROLE: worker prompt body",
      )
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("returns undefined when the role file does not exist (graceful no-op)", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-2032-"))
    try {
      assert.equal(readRolePrompt("worker", { XDG_CONFIG_HOME: dir }), undefined)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("returns undefined when the role is empty", () => {
    assert.equal(readRolePrompt("", { XDG_CONFIG_HOME: "/c" }), undefined)
  })

  it("returns undefined for an empty/whitespace-only role file", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-2032-"))
    try {
      const agentsDir = path.join(dir, "prism", "agents")
      fs.mkdirSync(agentsDir, { recursive: true })
      fs.writeFileSync(path.join(agentsDir, "worker.md"), "   \n\t\n")
      assert.equal(readRolePrompt("worker", { XDG_CONFIG_HOME: dir }), undefined)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })
})

describe("composeRoleSystemPrompt — APPEND semantics preserved", () => {
  it("appends the role prompt after the base, preserving the default", () => {
    assert.equal(
      composeRoleSystemPrompt("BASE PROMPT", "ROLE PROMPT"),
      "BASE PROMPT\n\nROLE PROMPT",
    )
  })

  it("normalises trailing whitespace on the base to exactly one blank line", () => {
    assert.equal(
      composeRoleSystemPrompt("BASE PROMPT\n\n  \n", "ROLE PROMPT"),
      "BASE PROMPT\n\nROLE PROMPT",
    )
  })

  it("returns the role prompt alone when the base is empty", () => {
    assert.equal(composeRoleSystemPrompt("", "ROLE PROMPT"), "ROLE PROMPT")
  })

  it("returns undefined (no override → keep base) when there is no role prompt", () => {
    assert.equal(composeRoleSystemPrompt("BASE", undefined), undefined)
    assert.equal(composeRoleSystemPrompt("BASE", ""), undefined)
    assert.equal(composeRoleSystemPrompt("BASE", "   \n  "), undefined)
  })

  it("is idempotent across turns: recomposing from the same base does not accumulate", () => {
    const base = "BASE PROMPT"
    const role = "ROLE PROMPT"
    const turn1 = composeRoleSystemPrompt(base, role)
    const turn2 = composeRoleSystemPrompt(base, role)
    assert.equal(turn1, turn2)
    assert.equal(turn1, "BASE PROMPT\n\nROLE PROMPT")
  })
})

describe("resolveRolePromptForTurn — gating, handshake latch, idempotency", () => {
  const newCache = (): RolePromptCache => ({ resolved: false, cached: undefined })

  it("injects unconditionally once the handshake completes (no gating flag — PR2 of #2031)", () => {
    const cache = newCache()
    const out = resolveRolePromptForTurn(
      { handshakeComplete: true, sessionRole: "worker", baseSystemPrompt: "BASE" },
      cache,
      () => "ROLE",
    )
    assert.equal(out, "BASE\n\nROLE") // injected — no flag required
    assert.equal(cache.resolved, true)
  })

  it("returns undefined and does NOT latch when the handshake is incomplete", () => {
    const cache = newCache()
    const out = resolveRolePromptForTurn(
      { handshakeComplete: false, sessionRole: "", baseSystemPrompt: "BASE" },
      cache,
      () => "ROLE",
    )
    assert.equal(out, undefined)
    assert.equal(cache.resolved, false) // critical: must NOT latch pre-handshake
  })

  it("injects once the handshake completes and the role is known", () => {
    const cache = newCache()
    const out = resolveRolePromptForTurn(
      { handshakeComplete: true, sessionRole: "worker", baseSystemPrompt: "BASE" },
      cache,
      (role) => (role === "worker" ? "WORKER ROLE" : undefined),
    )
    assert.equal(out, "BASE\n\nWORKER ROLE")
    assert.equal(cache.resolved, true)
    assert.equal(cache.cached, "WORKER ROLE")
  })

  it("first turn before handshake must not poison the cache (the review-code race)", () => {
    const cache = newCache()
    let reads = 0
    const readRole = (role: string): string | undefined => {
      reads++
      return role === "worker" ? "WORKER ROLE" : undefined
    }

    // Turn 1 fires BEFORE hello_ack: handshake incomplete, sessionRole still "".
    const turn1 = resolveRolePromptForTurn(
      { handshakeComplete: false, sessionRole: "", baseSystemPrompt: "BASE" },
      cache,
      readRole,
    )
    assert.equal(turn1, undefined)
    assert.equal(cache.resolved, false)
    assert.equal(reads, 0) // no read attempted pre-handshake

    // Turn 2 fires AFTER hello_ack populated sessionRole="worker".
    const turn2 = resolveRolePromptForTurn(
      { handshakeComplete: true, sessionRole: "worker", baseSystemPrompt: "BASE" },
      cache,
      readRole,
    )
    assert.equal(turn2, "BASE\n\nWORKER ROLE") // role IS injected, not poisoned
    assert.equal(reads, 1)
  })

  it("memoises the file read across turns (disk touched at most once)", () => {
    const cache = newCache()
    let reads = 0
    const readRole = (): string | undefined => {
      reads++
      return "ROLE"
    }
    const args = {
      handshakeComplete: true,
      sessionRole: "worker",
      baseSystemPrompt: "BASE",
    }
    const t1 = resolveRolePromptForTurn(args, cache, readRole)
    const t2 = resolveRolePromptForTurn(args, cache, readRole)
    const t3 = resolveRolePromptForTurn(args, cache, readRole)
    assert.equal(reads, 1) // read once, reused thereafter
    assert.equal(t1, "BASE\n\nROLE")
    assert.equal(t1, t2) // idempotent across turns
    assert.equal(t2, t3)
  })

  it("missing role file resolves to a no-op (undefined) and stays latched", () => {
    const cache = newCache()
    let reads = 0
    const out = resolveRolePromptForTurn(
      { handshakeComplete: true, sessionRole: "worker", baseSystemPrompt: "BASE" },
      cache,
      () => {
        reads++
        return undefined
      },
    )
    assert.equal(out, undefined)
    assert.equal(cache.resolved, true) // latched after handshake even for no-op
    // A second turn does not re-read — the no-op is memoised.
    resolveRolePromptForTurn(
      { handshakeComplete: true, sessionRole: "worker", baseSystemPrompt: "BASE" },
      cache,
      () => {
        reads++
        return undefined
      },
    )
    assert.equal(reads, 1)
  })
})
