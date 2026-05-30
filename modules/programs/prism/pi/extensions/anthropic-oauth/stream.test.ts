// Unit tests for sanitizeSystemText in stream.ts.
// Run with: node --test stream.test.ts  (Node 20+, zero new deps)
//
// Covers every acceptance criterion from GitHub issue #1444:
//   - Paths inside <available_skills> blocks are preserved verbatim
//   - "you are pi" paragraphs are still stripped
//   - Prose "pi" → "Claude Code" substitution is preserved
//   - PI_REMOVAL_ANCHORS paragraphs are still stripped
//   - Fenced code blocks are preserved verbatim
//   - Inline code spans are preserved verbatim
//   - Absolute Unix paths (including ~/) are preserved verbatim
//   - URLs are preserved verbatim
//   - Prose-only prompts behave identically to the prior implementation
//   - Two separate fenced code blocks are both preserved; prose between them
//     is still subject to substitution
//   - Real coordinator-shaped system prompt (skills + AGENTS.md frontmatter)
//     preserves /run/prism/pi-agent/skills/ paths

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { PassThrough } from "node:stream"
import { sanitizeSystemText, buildAnthropicSystemPrompt, parseSSEStream } from "./stream.ts"
import { initLogger, closeLogger } from "./logger.ts"

// ---------------------------------------------------------------------------
// Helper: reference implementation of the *prior* sanitizeSystemText behaviour
// (paragraph filter + unscoped \bpi\b replace) for regression comparison.
// ---------------------------------------------------------------------------
function legacySanitize(text: string): string {
  const PI_REMOVAL_ANCHORS = [
    "pi-coding-agent",
    "@earendil-works/pi-coding-agent",
    "badlogic/pi-mono",
  ]
  const paragraphs = text.split(/\n\n+/)
  const filtered = paragraphs.filter((paragraph) => {
    const lower = paragraph.toLowerCase()
    if (lower.includes("you are pi")) return false
    return !PI_REMOVAL_ANCHORS.some((anchor) => paragraph.includes(anchor))
  })
  return filtered
    .join("\n\n")
    .replace(/\bpi\b/g, "Claude Code")
    .replace(/\bPi\b/g, "Claude Code")
    .trim()
}

// ---------------------------------------------------------------------------
// [functional] <available_skills> block: paths preserved, "Claude Code-agent" absent
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — available_skills block", () => {
  it("preserves /run/prism/pi-agent/skills/ paths inside <available_skills>", () => {
    const input = [
      "Some prose introduction.",
      "<available_skills>\n  <skill>\n    <name>prism</name>\n    <description>Spawn agents.</description>\n    <location>/run/prism/pi-agent/skills/prism/SKILL.md</location>\n  </skill>\n  <skill>\n    <name>aws</name>\n    <description>AWS helpers.</description>\n    <location>/run/prism/pi-agent/skills/aws/SKILL.md</location>\n  </skill>\n</available_skills>",
      "Some trailing prose.",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      output.includes("/run/prism/pi-agent/skills/prism/SKILL.md"),
      "should contain original pi-agent path for prism skill",
    )
    assert.ok(
      output.includes("/run/prism/pi-agent/skills/aws/SKILL.md"),
      "should contain original pi-agent path for aws skill",
    )
    assert.ok(
      !output.includes("Claude Code-agent"),
      "should not contain mangled Claude Code-agent path",
    )
  })

  it("preserves the full <available_skills> block verbatim", () => {
    const skillsBlock =
      "<available_skills>\n  <skill>\n    <name>foo</name>\n    <description>Bar.</description>\n    <location>/run/prism/pi-agent/skills/foo/SKILL.md</location>\n  </skill>\n</available_skills>"
    const input = `Intro paragraph.\n\n${skillsBlock}\n\nTrailing paragraph.`

    const output = sanitizeSystemText(input)

    assert.ok(
      output.includes("/run/prism/pi-agent/skills/foo/SKILL.md"),
      "path inside skills block must survive verbatim",
    )
  })
})

// ---------------------------------------------------------------------------
// [functional] "you are pi" paragraph is stripped (existing behaviour preserved)
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — you are pi paragraph removal", () => {
  it("removes the 'You are pi, an AI coding assistant.' paragraph", () => {
    const input = [
      "You are pi, an AI coding assistant.",
      "Some other prose.",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      !output.includes("You are pi, an AI coding assistant."),
      "identity paragraph should be stripped",
    )
    assert.ok(output.includes("Some other prose."), "other prose should remain")
  })

  it("strips paragraph containing 'you are pi' (case-insensitive)", () => {
    const input = "YOU ARE PI, the coding assistant.\n\nOther content here."
    const output = sanitizeSystemText(input)
    assert.ok(!output.includes("YOU ARE PI"), "case-insensitive match should strip")
  })
})

// ---------------------------------------------------------------------------
// [functional] Prose \bpi\b → "Claude Code" substitution preserved
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — prose pi → Claude Code substitution", () => {
  it("replaces 'pi' in prose with 'Claude Code'", () => {
    const input = "Use pi to invoke the agent."
    const output = sanitizeSystemText(input)
    assert.equal(output, "Use Claude Code to invoke the agent.")
  })

  it("replaces 'Pi' (capitalised) in prose with 'Claude Code'", () => {
    const input = "Pi is a coding assistant."
    const output = sanitizeSystemText(input)
    assert.equal(output, "Claude Code is a coding assistant.")
  })

  it("does not replace 'PI' (all-caps) — \\bpi\\b is case-sensitive", () => {
    const input = "The value of PI is 3.14."
    const output = sanitizeSystemText(input)
    // \bpi\b only matches lowercase and \bPi\b only matches title-case;
    // ALL-CAPS "PI" is unchanged.
    assert.ok(!output.includes("Claude Code"), "ALL-CAPS PI should not be replaced")
  })
})

// ---------------------------------------------------------------------------
// [functional] PI_REMOVAL_ANCHORS paragraph filtering preserved
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — PI_REMOVAL_ANCHORS paragraph filtering", () => {
  it("removes a paragraph anchored by '@earendil-works/pi-coding-agent'", () => {
    const input = [
      "Normal intro paragraph.",
      "This extension is @earendil-works/pi-coding-agent and does things.",
      "Normal trailing paragraph.",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      !output.includes("@earendil-works/pi-coding-agent"),
      "anchor paragraph should be stripped",
    )
    assert.ok(
      output.includes("Normal intro paragraph."),
      "non-anchor paragraphs should remain",
    )
    assert.ok(
      output.includes("Normal trailing paragraph."),
      "trailing paragraph should remain",
    )
  })

  it("removes a paragraph anchored by 'pi-coding-agent'", () => {
    const input = "Intro.\n\nSee pi-coding-agent for details.\n\nEnd."
    const output = sanitizeSystemText(input)
    assert.ok(!output.includes("pi-coding-agent"), "pi-coding-agent paragraph removed")
  })

  it("removes a paragraph anchored by 'badlogic/pi-mono'", () => {
    const input = "Intro.\n\nbadlogic/pi-mono is the monorepo.\n\nEnd."
    const output = sanitizeSystemText(input)
    assert.ok(!output.includes("badlogic/pi-mono"), "badlogic/pi-mono paragraph removed")
  })
})

// ---------------------------------------------------------------------------
// [edge-case] Fenced code blocks preserved verbatim
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — fenced code blocks preserved verbatim", () => {
  it("preserves code inside a ``` fenced block unchanged", () => {
    const input = [
      "Prose before the block.",
      "```typescript\npi.registerProvider(\"anthropic\", {...})\n```",
      "Prose after the block.",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      output.includes("pi.registerProvider(\"anthropic\", {...})"),
      "code inside fenced block should be preserved verbatim",
    )
  })

  it("two separate fenced code blocks are both preserved; prose between them is substituted", () => {
    const input = [
      "```js\nconst pi = require('pi-lib')\n```",
      "Use pi to do things.",
      "```sh\npi --version\n```",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      output.includes("const pi = require('pi-lib')"),
      "first fenced block preserved verbatim",
    )
    assert.ok(
      output.includes("pi --version"),
      "second fenced block preserved verbatim",
    )
    assert.ok(
      output.includes("Use Claude Code to do things."),
      "prose between blocks should have pi → Claude Code substitution applied",
    )
  })

  it("preserves ~~~ fenced code blocks", () => {
    const input = "Prose.\n\n~~~python\npi = 3.14\n~~~\n\nMore prose."
    const output = sanitizeSystemText(input)
    assert.ok(output.includes("pi = 3.14"), "~~~ fenced block preserved verbatim")
  })
})

// ---------------------------------------------------------------------------
// [edge-case] Inline code spans preserved verbatim
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — inline code spans preserved verbatim", () => {
  it("preserves `pi-wire-protocol.md` inline span", () => {
    const input = "See `pi-wire-protocol.md` for details."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("`pi-wire-protocol.md`"),
      "inline code span should be preserved verbatim",
    )
  })

  it("preserves `pi.registerProvider` inline span", () => {
    const input = "Call `pi.registerProvider` to register."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("`pi.registerProvider`"),
      "inline code span with dot should be preserved verbatim",
    )
  })
})

// ---------------------------------------------------------------------------
// [edge-case] Absolute Unix paths and ~ paths preserved verbatim
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — absolute paths preserved verbatim", () => {
  it("preserves ~/.pi/agent/sessions unchanged", () => {
    const input = "Sessions are stored in ~/.pi/agent/sessions on disk."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("~/.pi/agent/sessions"),
      "~ path should be preserved verbatim",
    )
  })

  it("preserves /run/prism/pi-agent/... absolute path unchanged", () => {
    const input = "The skill file is at /run/prism/pi-agent/skills/foo/SKILL.md."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("/run/prism/pi-agent/skills/foo/SKILL.md"),
      "absolute path should be preserved verbatim",
    )
  })
})

// ---------------------------------------------------------------------------
// [edge-case] URLs preserved verbatim
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — URLs preserved verbatim", () => {
  it("preserves https://example.com/pi/docs unchanged", () => {
    const input = "Visit https://example.com/pi/docs for documentation."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("https://example.com/pi/docs"),
      "https URL should be preserved verbatim",
    )
  })

  it("preserves http:// URLs unchanged", () => {
    const input = "See http://internal/pi/api for the API."
    const output = sanitizeSystemText(input)
    assert.ok(
      output.includes("http://internal/pi/api"),
      "http URL should be preserved verbatim",
    )
  })
})

// ---------------------------------------------------------------------------
// [edge-case] Prose-only prompts: no regression vs prior implementation
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — prose-only no regression", () => {
  it("prose-only prompt without special regions is identical to legacy output", () => {
    const inputs = [
      "Use pi to write code.",
      "Pi is great for coding tasks.",
      "Normal paragraph.\n\nAnother normal paragraph.\n\nFinal paragraph.",
      "This is a sentence about pi and Pi together.",
    ]

    for (const input of inputs) {
      const newOutput = sanitizeSystemText(input)
      const oldOutput = legacySanitize(input)
      assert.equal(
        newOutput,
        oldOutput,
        `prose-only input should produce identical output.\nInput: ${JSON.stringify(input)}\nNew: ${JSON.stringify(newOutput)}\nOld: ${JSON.stringify(oldOutput)}`,
      )
    }
  })

  it("you-are-pi removal still works in prose-only prompts", () => {
    const input = "you are pi, an AI assistant.\n\nOther content."
    const newOutput = sanitizeSystemText(input)
    const oldOutput = legacySanitize(input)
    assert.equal(newOutput, oldOutput)
  })

  it("PI_REMOVAL_ANCHORS still work in prose-only prompts", () => {
    const input = "Intro.\n\n@earendil-works/pi-coding-agent extension.\n\nEnd."
    const newOutput = sanitizeSystemText(input)
    const oldOutput = legacySanitize(input)
    assert.equal(newOutput, oldOutput)
  })
})

// ---------------------------------------------------------------------------
// [functional] Real coordinator-shaped system prompt
// ---------------------------------------------------------------------------
describe("sanitizeSystemText — real coordinator-shaped system prompt", () => {
  it("preserves /run/prism/pi-agent/skills/ and does not produce /run/prism/Claude Code-agent/skills/", () => {
    // Simulates what buildAnthropicSystemPrompt receives: a real-world system
    // prompt with agent frontmatter, an AGENTS.md excerpt, and a skills index.
    const systemPrompt = [
      // Agent frontmatter (would be stripped by PI_REMOVAL_ANCHORS)
      // We omit those deliberately here so we can check the skills block.

      // Skills index (emitted by formatSkillsForPrompt)
      "<available_skills>\n  <skill>\n    <name>prism</name>\n    <description>Spawn isolated agent sessions in their own git worktrees using the prism tool. Use when the user asks to spawn an agent, delegate work to another session, run something in parallel, or work on a PR or different repo.</description>\n    <location>/run/prism/pi-agent/skills/prism/SKILL.md</location>\n  </skill>\n  <skill>\n    <name>aws</name>\n    <description>Use AWS CLI correctly.</description>\n    <location>/run/prism/pi-agent/skills/aws/SKILL.md</location>\n  </skill>\n  <skill>\n    <name>playwright-cli</name>\n    <description>Automates browser interactions.</description>\n    <location>/run/prism/pi-agent/skills/playwright-cli/SKILL.md</location>\n  </skill>\n</available_skills>",

      // AGENTS.md excerpt with prose
      "## Project Overview\n\nThis project is managed with Nix Flakes. Use pi to spawn agents.",

      // Tool description prose
      "The pi tool supports read, write, bash, and edit operations.",
    ].join("\n\n")

    const output = sanitizeSystemText(systemPrompt)

    // Must contain at least one pi-agent path
    assert.ok(
      output.includes("/run/prism/pi-agent/skills/"),
      "output should contain /run/prism/pi-agent/skills/ at least once",
    )

    // Must NOT contain the corrupted path
    assert.ok(
      !output.includes("/run/prism/Claude Code-agent/skills/"),
      "output must not contain /run/prism/Claude Code-agent/skills/",
    )
  })

  it("transformBody scenario: buildAnthropicSystemPrompt output for oauth=true preserves pi-agent paths", () => {
    const systemPrompt =
      "<available_skills>\n  <skill>\n    <name>prism</name>\n    <description>...</description>\n    <location>/run/prism/pi-agent/skills/prism/SKILL.md</location>\n  </skill>\n</available_skills>\n\nUse pi to invoke the agent."

    const blocks = buildAnthropicSystemPrompt(systemPrompt, true)

    assert.ok(blocks && blocks.length >= 2, "should have at least 2 blocks (identity + content)")

    // The second block (index 1) is the sanitized system prompt content
    const contentBlock = blocks[1]
    assert.ok(contentBlock, "content block should exist")
    assert.ok(
      contentBlock.text.includes("/run/prism/pi-agent/skills/prism/SKILL.md"),
      "content block should contain original pi-agent skill path",
    )
    assert.ok(
      !contentBlock.text.includes("/run/prism/Claude Code-agent/skills/"),
      "content block must not contain corrupted Claude Code-agent path",
    )

    // Prose substitution still works — "Use pi to invoke the agent." → "Use Claude Code to invoke the agent."
    assert.ok(
      contentBlock.text.includes("Use Claude Code to invoke the agent."),
      "prose pi → Claude Code substitution should still work",
    )
  })
})

// ---------------------------------------------------------------------------
// [functional] parseSSEStream diagnostic instrumentation (issue #2048)
//
// Asserts:
//   - With PI_ANTHROPIC_OAUTH_DEBUG unset / logger disabled, parseSSEStream
//     emits no log lines and parses the stream identically.
//   - With the logger enabled (via initLogger({ stream }), the same mechanism
//     the runtime uses), per-event logs, the event:error log, and the
//     terminal summary all appear with the expected compact field shapes.
//   - No request/response bodies, headers, tokens, or message content appear
//     in the captured log output — frame metadata only.
// ---------------------------------------------------------------------------

// Build a Response whose body is a ReadableStream emitting `chunks` as raw
// SSE bytes. Each chunk is delivered as a separate Uint8Array, which is
// closer to how the network actually fragments frames.
function makeSSEResponse(chunks: string[]): Response {
  const encoder = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(encoder.encode(c))
      controller.close()
    },
  })
  return new Response(body, {
    status: 200,
    headers: { "content-type": "text/event-stream" },
  })
}

// A no-op AssistantMessageEventStream that just collects pushed events.
// Typed loosely — we don't assert on stream events, only on log output.
function makeFakeStream(): { push: (e: unknown) => void; events: unknown[] } {
  const events: unknown[] = []
  return { push: (e) => events.push(e), events }
}

// Minimal Model / Context shaped to satisfy parseSSEStream. We never read
// network or invoke a tool; these are only assigned onto `output`.
function makeModelAndContext() {
  const model = {
    id: "claude-test",
    api: "anthropic",
    provider: "anthropic",
  } as unknown as Parameters<typeof parseSSEStream>[1]
  const context = { tools: [] } as unknown as Parameters<typeof parseSSEStream>[2]
  return { model, context }
}

// Canonical, well-formed SSE transcript covering each event arm we log.
const HEALTHY_TRANSCRIPT = [
  // message_start
  'event: message_start\ndata: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}\n\n',
  // content_block_start (text)
  'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n',
  // content_block_delta (text)
  'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n\n',
  // content_block_stop
  'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n',
  // content_block_start (tool_use)
  'event: content_block_start\ndata: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}\n\n',
  // content_block_delta (input_json)
  'event: content_block_delta\ndata: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}\n\n',
  // content_block_stop
  'event: content_block_stop\ndata: {"type":"content_block_stop","index":1}\n\n',
  // message_delta with stop_reason
  'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}\n\n',
  // message_stop
  'event: message_stop\ndata: {"type":"message_stop"}\n\n',
]

function parseLogLines(buf: string): Array<Record<string, unknown>> {
  return buf
    .split("\n")
    .filter((l) => l.trim().length > 0)
    .map((l) => JSON.parse(l) as Record<string, unknown>)
}

describe("parseSSEStream — diagnostic instrumentation (#2048)", () => {
  it("emits zero log lines when the logger is disabled (env-flag unset default)", async () => {
    // Ensure no leaked state from a prior test re-enables the logger.
    closeLogger()

    // Capture writes to the logger's stream sink: install one, then close it
    // again to flip mode back to "disabled". After this, log() must be a no-op.
    const sink = new PassThrough()
    let captured = ""
    sink.on("data", (chunk) => {
      captured += chunk.toString()
    })

    // (Sanity: with no initLogger call, logger is disabled by default.)
    const { model, context } = makeModelAndContext()
    const fakeStream = makeFakeStream()
    const response = makeSSEResponse(HEALTHY_TRANSCRIPT)

    const out = await parseSSEStream(
      response,
      model,
      context,
      false,
      fakeStream as unknown as Parameters<typeof parseSSEStream>[4],
    )

    assert.equal(captured, "", "no log bytes should be written when logger is disabled")
    // Behavioural sanity: parse still works.
    assert.equal(out.role, "assistant")
    assert.equal(out.content.length, 2, "text + toolCall block parsed")
    assert.equal(out.stopReason, "toolUse")
  })

  it("emits per-event, error, and terminal-summary log lines with compact field shapes when enabled", async () => {
    const sink = new PassThrough()
    let captured = ""
    sink.on("data", (chunk) => {
      captured += chunk.toString()
    })
    initLogger({ stream: sink })

    try {
      const { model, context } = makeModelAndContext()
      const fakeStream = makeFakeStream()

      // Include a mid-stream `event: error` frame to assert the error-arm log.
      // The parser's silent-fall-through behaviour for errors is preserved
      // (no error arm in the switch); we only assert that the raw payload was
      // logged BEFORE that fall-through.
      const transcriptWithError = [
        ...HEALTHY_TRANSCRIPT.slice(0, 3),
        'event: error\ndata: {"type":"error","error":{"type":"overloaded_error","message":"x"}}\n\n',
        ...HEALTHY_TRANSCRIPT.slice(3),
      ]
      const response = makeSSEResponse(transcriptWithError)

      const out = await parseSSEStream(
        response,
        model,
        context,
        false,
        fakeStream as unknown as Parameters<typeof parseSSEStream>[4],
      )

      const lines = parseLogLines(captured)
      assert.ok(lines.length > 0, "some log lines should have been emitted")

      // Every log entry has the logger's standard envelope: ts + event + payload.
      for (const entry of lines) {
        assert.equal(typeof entry.ts, "string")
        assert.equal(typeof entry.event, "string")
      }

      // Per-event logs: one sse_event per parsed SSE frame.
      const perEvent = lines.filter((l) => l.event === "sse_event")
      const types = perEvent.map((l) => l.t)
      assert.deepEqual(
        types,
        [
          "message_start",
          "content_block_start",
          "content_block_delta",
          // NOTE: event:error frame is logged separately as sse_event_error;
          // it ALSO produces a per-event log because the parser still JSON-
          // parses the data and routes through the switch with type "error".
          "error",
          "content_block_stop",
          "content_block_start",
          "content_block_delta",
          "content_block_stop",
          "message_delta",
          "message_stop",
        ],
        "per-event log should cover every parsed frame in order",
      )

      // content_block_start carries the cb (content_block type) field.
      const blockStarts = perEvent.filter((l) => l.t === "content_block_start")
      assert.deepEqual(
        blockStarts.map((l) => l.cb),
        ["text", "tool_use"],
        "content_block_start logs include cb field",
      )
      // and an index.
      for (const bs of blockStarts) assert.equal(typeof bs.i, "number")

      // content_block_delta carries the d (delta type) field.
      const blockDeltas = perEvent.filter((l) => l.t === "content_block_delta")
      assert.deepEqual(
        blockDeltas.map((l) => l.d),
        ["text_delta", "input_json_delta"],
        "content_block_delta logs include d field",
      )

      // message_delta carries the sr (stop_reason) field.
      const msgDeltas = perEvent.filter((l) => l.t === "message_delta")
      assert.equal(msgDeltas.length, 1)
      assert.equal(msgDeltas[0].sr, "tool_use", "message_delta log includes sr field")

      // event:error frame produces a dedicated sse_event_error log with raw data.
      const errLogs = lines.filter((l) => l.event === "sse_event_error")
      assert.equal(errLogs.length, 1, "exactly one sse_event_error log expected")
      assert.equal(typeof errLogs[0].data, "string")
      assert.ok(
        (errLogs[0].data as string).includes("overloaded_error"),
        "raw error payload should be captured",
      )

      // Terminal summary: exactly one, emitted last.
      const ends = lines.filter((l) => l.event === "sse_stream_end")
      assert.equal(ends.length, 1, "exactly one sse_stream_end summary expected")
      assert.equal(lines[lines.length - 1].event, "sse_stream_end", "summary is last")
      const summary = ends[0]
      assert.equal(summary.contentLen, 2, "text + toolCall content blocks")
      assert.equal(summary.toolCalls, 1, "one toolCall block")
      assert.equal(summary.stopReason, "toolUse")
      assert.equal(summary.sawMessageStop, true)

      // Security: no message content or token strings leaked into the log.
      // The transcript contains the text "hi" inside a text_delta. The
      // per-event logger MUST NOT include it (we only log frame metadata).
      assert.ok(!captured.includes('"hi"'), "message text must not appear in logs")
      // No usage tokens fields either.
      assert.ok(
        !captured.includes("input_tokens"),
        "raw usage fields should not appear in per-event logs",
      )

      // Behavioural sanity: parse still returns the same shape as the
      // disabled-logger case.
      assert.equal(out.content.length, 2)
      assert.equal(out.stopReason, "toolUse")
    } finally {
      closeLogger()
    }
  })

  it("terminal summary reports sawMessageStop=false when the stream ends without one", async () => {
    const sink = new PassThrough()
    let captured = ""
    sink.on("data", (chunk) => {
      captured += chunk.toString()
    })
    initLogger({ stream: sink })

    try {
      // Same transcript but drop the trailing message_stop frame.
      const truncated = HEALTHY_TRANSCRIPT.slice(0, HEALTHY_TRANSCRIPT.length - 1)
      const { model, context } = makeModelAndContext()
      const fakeStream = makeFakeStream()
      const response = makeSSEResponse(truncated)

      await parseSSEStream(
        response,
        model,
        context,
        false,
        fakeStream as unknown as Parameters<typeof parseSSEStream>[4],
      )

      const lines = parseLogLines(captured)
      const ends = lines.filter((l) => l.event === "sse_stream_end")
      assert.equal(ends.length, 1)
      assert.equal(
        ends[0].sawMessageStop,
        false,
        "missing message_stop must be observable in the summary",
      )
    } finally {
      closeLogger()
    }
  })
})
