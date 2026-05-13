// Unit tests for slim.ts — field-drop logic for Atlassian MCP responses.
// Run with: tsx --test slim.test.ts (from this directory)
// Or: cd modules/programs/prism/pi/extensions/atlassian && tsx --test slim.test.ts

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import {
  slimJson,
  slimMcpResultContent,
  optionsForTool,
  UNIVERSAL_DROP_KEYS,
  JIRA_ISSUE_DROP_KEYS,
  CONFLUENCE_DROP_KEYS,
} from "./slim.ts"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function universalPayload(): Record<string, unknown> {
  return {
    expand: "should be dropped",
    self: "should be dropped",
    iconUrl: "should be dropped",
    avatarUrl: "should be dropped",
    avatarUrls: { "48x48": "http://..." },
    avatarId: 12345,
    picture: "url",
    schema: { type: "object" },
    id: "preserved",
    key: "preserved",
    summary: "preserved",
    nested: {
      expand: "should be dropped inside nested",
      displayName: "preserved",
    },
  }
}

// ---------------------------------------------------------------------------
// UNIVERSAL_DROP_KEYS tests
// ---------------------------------------------------------------------------

describe("slimJson — UNIVERSAL_DROP_KEYS", () => {
  it("drops all UNIVERSAL_DROP_KEYS from a flat object", () => {
    const opts = optionsForTool("anyTool")
    const input = universalPayload()
    const result = slimJson(input, opts) as Record<string, unknown>

    for (const key of UNIVERSAL_DROP_KEYS) {
      assert.ok(!(key in result), `Expected key "${key}" to be dropped`)
    }
  })

  it("preserves non-drop keys", () => {
    const opts = optionsForTool("anyTool")
    const input = universalPayload()
    const result = slimJson(input, opts) as Record<string, unknown>

    assert.equal(result.id, "preserved")
    assert.equal(result.key, "preserved")
    assert.equal(result.summary, "preserved")
  })

  it("drops universal keys recursively inside nested objects", () => {
    const opts = optionsForTool("anyTool")
    const input = universalPayload()
    const result = slimJson(input, opts) as Record<string, unknown>
    const nested = result.nested as Record<string, unknown>

    assert.ok(!("expand" in nested), "expand should be dropped inside nested object")
    assert.equal(nested.displayName, "preserved")
  })

  it("handles arrays correctly", () => {
    const opts = optionsForTool("anyTool")
    const input = {
      items: [
        { expand: "drop", id: "keep1" },
        { self: "drop", id: "keep2" },
      ],
    }
    const result = slimJson(input, opts) as { items: Record<string, unknown>[] }

    assert.equal(result.items.length, 2)
    assert.ok(!("expand" in result.items[0]))
    assert.equal(result.items[0].id, "keep1")
    assert.ok(!("self" in result.items[1]))
    assert.equal(result.items[1].id, "keep2")
  })
})

// ---------------------------------------------------------------------------
// JIRA_ISSUE_DROP_KEYS tests
// ---------------------------------------------------------------------------

describe("slimJson — JIRA_ISSUE_DROP_KEYS", () => {
  it("drops JIRA_ISSUE_DROP_KEYS for jira tool names", () => {
    const opts = optionsForTool("getJiraIssue")
    const input: Record<string, unknown> = {}
    for (const key of JIRA_ISSUE_DROP_KEYS) {
      input[key] = `value of ${key}`
    }
    input.id = "preserved"
    input.summary = "preserved"

    const result = slimJson(input, opts) as Record<string, unknown>

    for (const key of JIRA_ISSUE_DROP_KEYS) {
      assert.ok(!(key in result), `Expected Jira key "${key}" to be dropped`)
    }
    assert.equal(result.id, "preserved")
    assert.equal(result.summary, "preserved")
  })

  it("also drops JIRA_ISSUE_DROP_KEYS for tools matching /issue/ pattern", () => {
    const opts = optionsForTool("createJiraIssue")
    const input = { renderedFields: "drop this", id: "keep" }
    const result = slimJson(input, opts) as Record<string, unknown>

    assert.ok(!("renderedFields" in result))
    assert.equal(result.id, "keep")
  })

  it("does NOT drop JIRA_ISSUE_DROP_KEYS for unrelated tool names", () => {
    // A tool that doesn't match jira/issue patterns should not have Jira-specific drops
    const opts = optionsForTool("atlassianUserInfo")
    const input = { renderedFields: "keep this", id: "keep" }
    const result = slimJson(input, opts) as Record<string, unknown>

    // renderedFields should be kept for non-jira tools
    assert.equal((result as Record<string, unknown>).renderedFields, "keep this")
  })
})

// ---------------------------------------------------------------------------
// CONFLUENCE_DROP_KEYS tests
// ---------------------------------------------------------------------------

describe("slimJson — CONFLUENCE_DROP_KEYS", () => {
  it("drops CONFLUENCE_DROP_KEYS for confluence tool names", () => {
    const opts = optionsForTool("getConfluencePage")
    const input: Record<string, unknown> = {}
    for (const key of CONFLUENCE_DROP_KEYS) {
      input[key] = `value of ${key}`
    }
    input.id = "preserved"
    input.title = "preserved"

    const result = slimJson(input, opts) as Record<string, unknown>

    for (const key of CONFLUENCE_DROP_KEYS) {
      assert.ok(!(key in result), `Expected Confluence key "${key}" to be dropped`)
    }
    assert.equal(result.id, "preserved")
    assert.equal(result.title, "preserved")
  })
})

// ---------------------------------------------------------------------------
// Search/list extra drops
// ---------------------------------------------------------------------------

describe("slimJson — search/list extra drops", () => {
  it("drops body/description/comments/changelog for search tools", () => {
    const opts = optionsForTool("searchJiraIssuesUsingJql")
    const input = {
      id: "keep",
      body: "drop",
      description: "drop",
      content: "drop",
      comments: ["drop"],
      comment: "drop",
      changelog: "drop",
      history: "drop",
      adf: "drop",
      summary: "keep",
    }
    const result = slimJson(input, opts) as Record<string, unknown>

    assert.equal(result.id, "keep")
    assert.equal(result.summary, "keep")
    assert.ok(!("body" in result))
    assert.ok(!("description" in result))
    assert.ok(!("content" in result))
    assert.ok(!("comments" in result))
    assert.ok(!("comment" in result))
    assert.ok(!("changelog" in result))
    assert.ok(!("history" in result))
    assert.ok(!("adf" in result))
  })

  it("drops extra fields for list tools", () => {
    // listJiraProjects matches /list/ pattern
    const opts = optionsForTool("listJiraProjects")
    const input = {
      id: "keep",
      body: "drop",
      title: "keep",
    }
    const result = slimJson(input, opts) as Record<string, unknown>

    assert.equal(result.id, "keep")
    assert.equal(result.title, "keep")
    assert.ok(!("body" in result))
  })
})

// ---------------------------------------------------------------------------
// slimMcpResultContent tests
// ---------------------------------------------------------------------------

describe("slimMcpResultContent", () => {
  it("slims JSON content blocks", () => {
    const content = [
      {
        type: "text",
        text: JSON.stringify({
          id: "TYP-1",
          expand: "drop",
          self: "drop",
          summary: "keep",
          fields: {
            avatarUrl: "drop",
            summary: "keep",
          },
        }),
      },
    ]
    const result = slimMcpResultContent(content, "getJiraIssue")
    const parsed = JSON.parse(result) as Record<string, unknown>

    assert.equal(parsed.id, "TYP-1")
    assert.equal(parsed.summary, "keep")
    assert.ok(!("expand" in parsed))
    assert.ok(!("self" in parsed))
    const fields = parsed.fields as Record<string, unknown>
    assert.ok(!("avatarUrl" in fields))
    assert.equal(fields.summary, "keep")
  })

  it("passes through non-JSON text blocks unchanged", () => {
    const content = [{ type: "text", text: "plain text response" }]
    const result = slimMcpResultContent(content, "anyTool")
    assert.equal(result, "plain text response")
  })

  it("handles empty content array", () => {
    const result = slimMcpResultContent([], "anyTool")
    assert.equal(result, "")
  })

  it("handles non-text blocks (ignores them)", () => {
    const content = [
      { type: "image", data: "base64..." },
      { type: "text", text: JSON.stringify({ id: "keep", expand: "drop" }) },
    ]
    const result = slimMcpResultContent(content, "anyTool")
    const parsed = JSON.parse(result) as Record<string, unknown>
    assert.equal(parsed.id, "keep")
    assert.ok(!("expand" in parsed))
  })

  it("handles null/undefined content", () => {
    const result = slimMcpResultContent(null, "anyTool")
    assert.equal(result, "null")
  })
})

// ---------------------------------------------------------------------------
// optionsForTool — tool-name pattern matching
// ---------------------------------------------------------------------------

describe("optionsForTool", () => {
  it("matches jira tools for JIRA_ISSUE_DROP_KEYS", () => {
    const opts = optionsForTool("editJiraIssue")
    assert.ok(opts.dropKeys.has("renderedFields"))
    assert.ok(opts.dropKeys.has("operations"))
  })

  it("matches confluence tools for CONFLUENCE_DROP_KEYS", () => {
    const opts = optionsForTool("createConfluencePage")
    assert.ok(opts.dropKeys.has("_links"))
    assert.ok(opts.dropKeys.has("status"))
  })

  it("includes UNIVERSAL_DROP_KEYS for all tools", () => {
    for (const toolName of [
      "anyTool",
      "getJiraIssue",
      "searchConfluenceUsingCql",
      "atlassianUserInfo",
    ]) {
      const opts = optionsForTool(toolName)
      for (const key of UNIVERSAL_DROP_KEYS) {
        assert.ok(opts.dropKeys.has(key), `Expected universal key "${key}" in drop set for tool "${toolName}"`)
      }
    }
  })

  it("applies search extra drops for searchJiraIssuesUsingJql", () => {
    const opts = optionsForTool("searchJiraIssuesUsingJql")
    assert.ok(opts.dropKeys.has("body"))
    assert.ok(opts.dropKeys.has("description"))
    assert.ok(opts.dropKeys.has("changelog"))
  })

  it("applies search extra drops for searchConfluenceUsingCql", () => {
    const opts = optionsForTool("searchConfluenceUsingCql")
    assert.ok(opts.dropKeys.has("body"))
    assert.ok(opts.dropKeys.has("content"))
  })
})
