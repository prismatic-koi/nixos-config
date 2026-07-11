// Regression tests for transformBody — specifically the
// "Relocate non-core system entries to user messages" block.
// Run with: node --test transforms.test.ts  (Node 20+, zero new deps)
//
// Covers the acceptance criteria from GitHub issue #1983:
//   - [functional] array-content first user message: relocated system text is
//     separated from the original first text block by exactly "\n\n" when the
//     resulting blocks are read in order.
//   - [functional] string-content first user message: existing
//     `prefix + "\n\n" + original` behaviour is preserved unchanged.
//   - [edge-case] multiple non-core system entries are joined to each other by
//     "\n\n" AND separated from the original user content by "\n\n".
//   - [edge-case] when no non-core system entries need relocating, the first
//     user message is not mutated.

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { transformBody } from "./transforms.ts"

const SYSTEM_IDENTITY =
  "You are Claude Code, Anthropic's official CLI for Claude."

type ParsedBody = {
  system?: Array<{ type?: string; text?: string }>
  messages?: Array<{
    role?: string
    content?: string | Array<{ type?: string; text?: string }>
  }>
}

// Helper: parse the JSON string returned by transformBody back into an object
// for structural assertions.
function parseResult(body: ReturnType<typeof transformBody>): ParsedBody {
  assert.equal(
    typeof body,
    "string",
    "transformBody should return a JSON string for a JSON input",
  )
  return JSON.parse(body as string) as ParsedBody
}

// Helper: concatenate the text of all text blocks in a content array in order.
function joinTextBlocks(
  content: string | Array<{ type?: string; text?: string }> | undefined,
): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content
    .filter((b) => b.type === "text" && typeof b.text === "string")
    .map((b) => b.text as string)
    .join("")
}

// ---------------------------------------------------------------------------
// [functional] array-content first user message — the regression case.
// ---------------------------------------------------------------------------
describe("transformBody — relocate non-core system entries (array content)", () => {
  it("separates the moved system text from the first user text block by \\n\\n", () => {
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [
        { type: "text", text: SYSTEM_IDENTITY },
        {
          type: "text",
          text: "Current working directory: /home/ben/code/nixos-config/main",
        },
      ],
      messages: [
        {
          role: "user",
          content: [{ type: "text", text: "hi" }],
        },
      ],
    })

    const result = parseResult(transformBody(input))

    // Only the billing header and identity should remain in system[].
    assert.ok(Array.isArray(result.system))
    const remainingTexts = (result.system ?? []).map((e) => e.text ?? "")
    assert.ok(
      remainingTexts.some((t) => t.startsWith("x-anthropic-billing-header")),
      "billing header should remain in system[]",
    )
    assert.ok(
      remainingTexts.some((t) => t === SYSTEM_IDENTITY),
      "identity prefix should remain in system[]",
    )
    assert.ok(
      !remainingTexts.some((t) => t.includes("nixos-config/main")),
      "non-core system entry should be moved out of system[]",
    )

    // The first user message should still have array content.
    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.ok(
      Array.isArray(firstUser.content),
      "first user content stays an array",
    )

    // Concatenation of text blocks must NOT glue "main" directly onto "hi".
    const joined = joinTextBlocks(firstUser.content)
    assert.ok(
      !joined.includes("mainhi"),
      `text blocks must not glue together; got: ${JSON.stringify(joined)}`,
    )
    assert.ok(
      joined.includes("nixos-config/main\n\nhi"),
      `moved system text must be separated from user text by \\n\\n; got: ${JSON.stringify(joined)}`,
    )
  })
})

// ---------------------------------------------------------------------------
// [functional] string-content first user message — existing behaviour preserved.
// ---------------------------------------------------------------------------
describe("transformBody — relocate non-core system entries (string content)", () => {
  it("preserves prefix + \\n\\n + original for string content", () => {
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [
        { type: "text", text: SYSTEM_IDENTITY },
        {
          type: "text",
          text: "Current working directory: /home/ben/code/nixos-config/main",
        },
      ],
      messages: [
        {
          role: "user",
          content: "hi",
        },
      ],
    })

    const result = parseResult(transformBody(input))

    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.equal(
      typeof firstUser.content,
      "string",
      "string content stays a string",
    )
    assert.equal(
      firstUser.content,
      "Current working directory: /home/ben/code/nixos-config/main\n\nhi",
    )
  })
})

// ---------------------------------------------------------------------------
// [edge-case] multiple non-core system entries joined by \n\n to each other
// AND separated from the original user content by \n\n.
// ---------------------------------------------------------------------------
describe("transformBody — multiple non-core system entries", () => {
  it("joins moved entries to each other and separates them from user content (array)", () => {
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [
        { type: "text", text: SYSTEM_IDENTITY },
        { type: "text", text: "FIRST_NON_CORE" },
        { type: "text", text: "SECOND_NON_CORE" },
      ],
      messages: [
        {
          role: "user",
          content: [{ type: "text", text: "USER_TEXT" }],
        },
      ],
    })

    const result = parseResult(transformBody(input))
    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.ok(Array.isArray(firstUser.content))

    const joined = joinTextBlocks(firstUser.content)
    assert.ok(
      joined.includes("FIRST_NON_CORE\n\nSECOND_NON_CORE"),
      `moved entries should be joined by \\n\\n; got: ${JSON.stringify(joined)}`,
    )
    assert.ok(
      joined.includes("SECOND_NON_CORE\n\nUSER_TEXT"),
      `last moved entry should be separated from user text by \\n\\n; got: ${JSON.stringify(joined)}`,
    )
  })

  it("joins moved entries to each other and separates them from user content (string)", () => {
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [
        { type: "text", text: SYSTEM_IDENTITY },
        { type: "text", text: "FIRST_NON_CORE" },
        { type: "text", text: "SECOND_NON_CORE" },
      ],
      messages: [
        {
          role: "user",
          content: "USER_TEXT",
        },
      ],
    })

    const result = parseResult(transformBody(input))
    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.equal(typeof firstUser.content, "string")
    assert.equal(
      firstUser.content,
      "FIRST_NON_CORE\n\nSECOND_NON_CORE\n\nUSER_TEXT",
    )
  })
})

// ---------------------------------------------------------------------------
// [functional] CLAUDE_CODE_ENTRYPOINT fallback — v1.5.1 auth parity (#2381).
// The billing header injected as system[0] must encode `cc_entrypoint=sdk-cli`
// when the env var is unset, not `cc_entrypoint=cli`. AC3/AC10.
// ---------------------------------------------------------------------------
describe("transformBody — CLAUDE_CODE_ENTRYPOINT fallback", () => {
  it("encodes cc_entrypoint=sdk-cli in the billing header when env is unset", () => {
    const original = process.env.CLAUDE_CODE_ENTRYPOINT
    delete process.env.CLAUDE_CODE_ENTRYPOINT
    try {
      const input = JSON.stringify({
        model: "claude-sonnet-4-5",
        system: [{ type: "text", text: SYSTEM_IDENTITY }],
        messages: [{ role: "user", content: "hi" }],
      })
      const result = parseResult(transformBody(input))
      const billing = (result.system ?? []).find((e) =>
        (e.text ?? "").startsWith("x-anthropic-billing-header"),
      )
      assert.ok(billing, "billing header should be system[0]")
      assert.ok(
        (billing.text ?? "").includes("cc_entrypoint=sdk-cli"),
        `billing header should include cc_entrypoint=sdk-cli; got: ${billing.text}`,
      )
      assert.ok(
        !(billing.text ?? "").includes("cc_entrypoint=cli;"),
        `billing header should not include the legacy cc_entrypoint=cli; got: ${billing.text}`,
      )
    } finally {
      if (original !== undefined) {
        process.env.CLAUDE_CODE_ENTRYPOINT = original
      }
    }
  })

  it("honours CLAUDE_CODE_ENTRYPOINT when set", () => {
    const original = process.env.CLAUDE_CODE_ENTRYPOINT
    process.env.CLAUDE_CODE_ENTRYPOINT = "my-entrypoint"
    try {
      const input = JSON.stringify({
        model: "claude-sonnet-4-5",
        system: [{ type: "text", text: SYSTEM_IDENTITY }],
        messages: [{ role: "user", content: "hi" }],
      })
      const result = parseResult(transformBody(input))
      const billing = (result.system ?? []).find((e) =>
        (e.text ?? "").startsWith("x-anthropic-billing-header"),
      )
      assert.ok(billing)
      assert.ok((billing.text ?? "").includes("cc_entrypoint=my-entrypoint"))
    } finally {
      if (original === undefined) {
        delete process.env.CLAUDE_CODE_ENTRYPOINT
      } else {
        process.env.CLAUDE_CODE_ENTRYPOINT = original
      }
    }
  })
})

// ---------------------------------------------------------------------------
// [edge-case] no non-core system entries → first user message is not mutated.
// ---------------------------------------------------------------------------
describe("transformBody — no non-core system entries", () => {
  it("does not mutate the first user message when movedTexts is empty (array)", () => {
    const originalContent = [{ type: "text", text: "USER_TEXT" }]
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [{ type: "text", text: SYSTEM_IDENTITY }],
      messages: [
        {
          role: "user",
          content: originalContent,
        },
      ],
    })

    const result = parseResult(transformBody(input))
    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.deepEqual(
      firstUser.content,
      originalContent,
      "first user content should be unchanged when no entries are moved",
    )
  })

  it("does not mutate the first user message when movedTexts is empty (string)", () => {
    const input = JSON.stringify({
      model: "claude-3-5-sonnet-20241022",
      system: [{ type: "text", text: SYSTEM_IDENTITY }],
      messages: [
        {
          role: "user",
          content: "USER_TEXT",
        },
      ],
    })

    const result = parseResult(transformBody(input))
    const firstUser = result.messages?.[0]
    assert.ok(firstUser, "first user message present")
    assert.equal(firstUser.content, "USER_TEXT")
  })
})
