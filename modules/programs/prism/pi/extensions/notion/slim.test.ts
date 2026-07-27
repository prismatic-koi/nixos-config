// Unit tests for notion/slim.ts.
//
// Run with: tsx --test slim.test.ts (from this directory)
//
// slim.ts is currently a deliberate passthrough stub (see its header for why
// the Atlassian drop-key sets must not be copied across). These tests pin the
// contract that matters today: the transformation is LOSSLESS. If a future PR
// introduces real field dropping, these tests should be replaced with fixtures
// derived from real Notion responses — not deleted.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import { slimMcpResultContent } from "./slim.ts"

describe("slimMcpResultContent", () => {
  it("returns a single text block verbatim", () => {
    const json = JSON.stringify({ object: "page", id: "abc", properties: { Name: "Kōrero" } })
    assert.equal(slimMcpResultContent([{ type: "text", text: json }]), json)
  })

  it("does not drop any fields (the stub is lossless)", () => {
    const payload = {
      object: "page",
      // Field names that the Atlassian drop-key sets would have removed. A
      // careless port of atlassian/slim.ts would silently delete these.
      self: "keep-me",
      status: "keep-me-too",
      properties: { schema: "and-me" },
    }
    const out = JSON.parse(slimMcpResultContent([{ type: "text", text: JSON.stringify(payload) }]))
    assert.deepEqual(out, payload)
  })

  it("joins multiple text blocks with newlines", () => {
    assert.equal(
      slimMcpResultContent([
        { type: "text", text: "one" },
        { type: "text", text: "two" },
      ]),
      "one\ntwo",
    )
  })

  it("ignores non-text blocks", () => {
    assert.equal(
      slimMcpResultContent([
        { type: "image", data: "..." },
        { type: "text", text: "kept" },
      ]),
      "kept",
    )
  })

  it("passes non-JSON text through unchanged", () => {
    assert.equal(slimMcpResultContent([{ type: "text", text: "not json at all" }]), "not json at all")
  })

  it("handles an empty content array", () => {
    assert.equal(slimMcpResultContent([]), "")
  })

  it("stringifies a non-array payload rather than throwing", () => {
    assert.equal(slimMcpResultContent("bare string"), "bare string")
    assert.equal(slimMcpResultContent({ a: 1 }), '{"a":1}')
    assert.equal(slimMcpResultContent(null), "null")
  })

  it("tolerates malformed blocks", () => {
    assert.equal(
      slimMcpResultContent([null, undefined, { type: "text" }, { type: "text", text: "ok" }]),
      "ok",
    )
  })
})
