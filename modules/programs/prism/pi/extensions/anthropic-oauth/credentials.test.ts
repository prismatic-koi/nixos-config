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
})
