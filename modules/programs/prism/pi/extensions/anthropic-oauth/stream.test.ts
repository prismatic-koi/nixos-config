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
import { sanitizeSystemText, buildAnthropicSystemPrompt } from "./stream.ts"

// ---------------------------------------------------------------------------
// Helper: reference implementation of the *prior* sanitizeSystemText behaviour
// (paragraph filter + unscoped \bpi\b replace) for regression comparison.
// ---------------------------------------------------------------------------
function legacySanitize(text: string): string {
  const PI_REMOVAL_ANCHORS = [
    "pi-coding-agent",
    "@mariozechner/pi-coding-agent",
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
  it("removes a paragraph anchored by '@mariozechner/pi-coding-agent'", () => {
    const input = [
      "Normal intro paragraph.",
      "This extension is @mariozechner/pi-coding-agent and does things.",
      "Normal trailing paragraph.",
    ].join("\n\n")

    const output = sanitizeSystemText(input)

    assert.ok(
      !output.includes("@mariozechner/pi-coding-agent"),
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
    const input = "Intro.\n\n@mariozechner/pi-coding-agent extension.\n\nEnd."
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
