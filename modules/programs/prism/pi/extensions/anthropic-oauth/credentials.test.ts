// Unit tests for credentials.ts — nested auth.json shape.
// Run with: node --test credentials.test.ts  (Node 20+, zero new deps)
//
// credentials.ts respects PI_AUTH_JSON env var so we point it at a temp file
// in each test, avoiding any dependency on the real home directory.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  writeFileSync,
  readFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

// Import under test after env is set. Since the module caches nothing about
// the path at import time (it calls getAuthJsonPath() at call time), a single
// import suffices; individual tests just update PI_AUTH_JSON before calling.
import {
  readCredentials,
  writeCredentials,
  repairCredentials,
} from "./credentials.ts"

let tempDir: string
let authJson: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-creds-test-"))
  authJson = join(tempDir, "auth.json")
  process.env.PI_AUTH_JSON = authJson
})

afterEach(() => {
  delete process.env.PI_AUTH_JSON
  rmSync(tempDir, { recursive: true, force: true })
})

function writeAuthJson(data: unknown): void {
  writeFileSync(authJson, JSON.stringify(data, null, 2), "utf-8")
}

// ---------------------------------------------------------------------------
// readCredentials tests
// ---------------------------------------------------------------------------

describe("readCredentials", () => {
  it("returns null when auth.json is absent", () => {
    assert.equal(readCredentials(), null)
  })

  it("returns null when auth.json is malformed JSON", () => {
    writeFileSync(authJson, "not-json", "utf-8")
    assert.equal(readCredentials(), null)
  })

  it("returns null when auth.json has no 'anthropic' key", () => {
    writeAuthJson({ "github-copilot": { token: "abc" } })
    assert.equal(readCredentials(), null)
  })

  it("returns null when anthropic block is missing required fields", () => {
    writeAuthJson({ anthropic: { access: "tok" } }) // missing refresh, expires
    assert.equal(readCredentials(), null)
  })

  it("returns valid ClaudeCredentials from nested anthropic key", () => {
    const expires = Date.now() + 3_600_000
    writeAuthJson({
      anthropic: {
        access: "acc-token",
        refresh: "ref-token",
        expires,
      },
      "github-copilot": { token: "gh-token" },
    })
    const creds = readCredentials()
    assert.ok(creds !== null, "should return credentials")
    assert.equal(creds.accessToken, "acc-token")
    assert.equal(creds.refreshToken, "ref-token")
    assert.equal(creds.expiresAt, expires)
  })
})

// ---------------------------------------------------------------------------
// writeCredentials tests
// ---------------------------------------------------------------------------

describe("writeCredentials", () => {
  it("writes credentials under the anthropic key", () => {
    const expires = Date.now() + 3_600_000
    writeCredentials({ accessToken: "a", refreshToken: "r", expiresAt: expires })

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.ok(data.anthropic, "anthropic key should exist")
    assert.equal(data.anthropic.access, "a")
    assert.equal(data.anthropic.refresh, "r")
    assert.equal(data.anthropic.expires, expires)
  })

  it("creates the anthropic key when auth.json previously had none", () => {
    writeAuthJson({ "github-copilot": { token: "gh" } })
    writeCredentials({ accessToken: "x", refreshToken: "y", expiresAt: 123 })

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.equal(data.anthropic.access, "x")
    // Existing keys must be preserved
    assert.deepEqual(data["github-copilot"], { token: "gh" })
  })

  it("preserves other top-level keys when updating anthropic", () => {
    const expires = Date.now() + 1000
    writeAuthJson({
      anthropic: { access: "old-a", refresh: "old-r", expires: 0 },
      "github-copilot": { token: "gh" },
      someOtherKey: 42,
    })
    writeCredentials({ accessToken: "new-a", refreshToken: "new-r", expiresAt: expires })

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.equal(data.anthropic.access, "new-a")
    assert.deepEqual(data["github-copilot"], { token: "gh" })
    assert.equal(data.someOtherKey, 42)
  })

  it("creates parent directories if auth.json dir does not exist", () => {
    // Point to a nested path that doesn't exist yet
    const nested = join(tempDir, "deep", "nested", "auth.json")
    process.env.PI_AUTH_JSON = nested
    writeCredentials({ accessToken: "z", refreshToken: "w", expiresAt: 0 })
    const data = JSON.parse(readFileSync(nested, "utf-8"))
    assert.equal(data.anthropic.access, "z")
  })

  it("writes type: 'oauth' in the anthropic entry", () => {
    const expires = Date.now() + 3_600_000
    writeCredentials({ accessToken: "a", refreshToken: "r", expiresAt: expires })

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.equal(data.anthropic.type, "oauth")
  })
})

// ---------------------------------------------------------------------------
// readCredentials back-compat: missing type field
// ---------------------------------------------------------------------------

describe("readCredentials back-compat", () => {
  it("returns valid credentials when anthropic entry has no type field", () => {
    const expires = Date.now() + 3_600_000
    // Simulate a corrupted entry written by older versions of writeCredentials()
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: { access: "acc", refresh: "ref", expires },
      }),
      "utf-8",
    )
    const creds = readCredentials()
    assert.ok(creds !== null, "should return credentials even without type field")
    assert.equal(creds.accessToken, "acc")
    assert.equal(creds.refreshToken, "ref")
    assert.equal(creds.expiresAt, expires)
  })
})

// ---------------------------------------------------------------------------
// repairCredentials tests
// ---------------------------------------------------------------------------

describe("repairCredentials", () => {
  it("adds type: 'oauth' to an anthropic entry that is missing it", () => {
    const expires = Date.now() + 3_600_000
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: { access: "acc", refresh: "ref", expires },
        "github-copilot": { type: "oauth", refresh: "gh-ref" },
      }),
      "utf-8",
    )

    repairCredentials()

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.equal(data.anthropic.type, "oauth")
    // Original fields must be preserved
    assert.equal(data.anthropic.access, "acc")
    assert.equal(data.anthropic.refresh, "ref")
    assert.equal(data.anthropic.expires, expires)
    // Other top-level keys must be untouched
    assert.deepEqual(data["github-copilot"], { type: "oauth", refresh: "gh-ref" })
  })

  it("leaves an anthropic entry that already has type: 'oauth' unchanged", () => {
    const expires = Date.now() + 3_600_000
    const original = {
      anthropic: { type: "oauth", access: "acc", refresh: "ref", expires },
    }
    writeFileSync(authJson, JSON.stringify(original), "utf-8")

    repairCredentials()

    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.equal(data.anthropic.type, "oauth")
    assert.equal(data.anthropic.access, "acc")
    assert.equal(data.anthropic.refresh, "ref")
    assert.equal(data.anthropic.expires, expires)
  })

  it("is a no-op when auth.json is absent", () => {
    // File doesn't exist — should not throw
    assert.doesNotThrow(() => repairCredentials())
  })

  it("is a no-op when auth.json has no anthropic key", () => {
    writeFileSync(
      authJson,
      JSON.stringify({ "github-copilot": { type: "oauth" } }),
      "utf-8",
    )
    repairCredentials()
    // File should be unchanged
    const data = JSON.parse(readFileSync(authJson, "utf-8"))
    assert.ok(!("anthropic" in data))
  })
})
