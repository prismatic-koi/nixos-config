// Unit tests for the prism PI extension's pure helpers.
// Run with: node --test --import tsx prism.test.ts
//
// We test the helper functions in isolation: truncation, endpoint parsing,
// the JSONL line reader, and the inbound dispatcher with a mock API. The
// extension factory itself (with PI hook subscriptions) is end-to-end tested
// via the P2.SPAWN integration scenario.

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { Readable } from "node:stream"

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
} from "./prism.ts"

// ---------------------------------------------------------------------------
// shouldActivate (activation guard)
// ---------------------------------------------------------------------------

describe("shouldActivate", () => {
  it("returns false when PRISM_SESSION_NAME is missing", () => {
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

  it("translates nextTurn into an option-less call (PI decides)", async () => {
    const { api, calls, emit } = makeMockApi()
    await dispatchInboundFrame(
      { type: "prompt", text: "now", deliver_as: "nextTurn" },
      api,
      emit,
    )
    assert.equal(calls.sendUserMessage[0].content, "now")
    assert.equal(calls.sendUserMessage[0].options, undefined)
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
