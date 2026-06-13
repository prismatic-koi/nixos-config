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
  utimesSync,
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
  getCachedCredentials,
  invalidateCache,
} from "./credentials.ts"

let tempDir: string
let authJson: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-creds-test-"))
  authJson = join(tempDir, "auth.json")
  process.env.PI_AUTH_JSON = authJson
  // The developer host (and the bwrap dispatcher) set PI_CODING_AGENT_DIR
  // unconditionally. Clear it so the existing PI_AUTH_JSON-keyed test
  // fixtures continue to win precedence; the dedicated precedence test
  // below sets it back as needed.
  delete process.env.PI_CODING_AGENT_DIR
})

afterEach(() => {
  delete process.env.PI_AUTH_JSON
  delete process.env.PI_CODING_AGENT_DIR
  rmSync(tempDir, { recursive: true, force: true })
})

function writeAuthJson(data: unknown): void {
  writeFileSync(authJson, JSON.stringify(data, null, 2), "utf-8")
}

// invalidate the module-level cache before each test so prior `getCached`
// calls cannot bleed across tests.
beforeEach(() => {
  invalidateCache()
})

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

// ---------------------------------------------------------------------------
// getAuthJsonPath — PI_CODING_AGENT_DIR precedence (#2283)
// ---------------------------------------------------------------------------
//
// The path resolution order is PI_AUTH_JSON > PI_CODING_AGENT_DIR/auth.json
// > homedir()/.pi/agent/auth.json. The middle entry is what makes the
// `prism account use` swap visible inside an already-running bwrap
// sandbox: the dispatcher sets PI_CODING_AGENT_DIR to the dir-bind path,
// which surfaces host-side renames (the file-bind at homedir() pins the
// pre-swap inode).
//
// We can't observe the bwrap behaviour from a unit test, but we CAN
// verify the precedence: when PI_AUTH_JSON is unset and
// PI_CODING_AGENT_DIR is set, readCredentials() must consult
// <PI_CODING_AGENT_DIR>/auth.json.

describe("getAuthJsonPath precedence", () => {
  it("prefers PI_CODING_AGENT_DIR/auth.json when PI_AUTH_JSON is unset", () => {
    delete process.env.PI_AUTH_JSON
    const agentDir = mkdtempSync(join(tmpdir(), "pi-agent-dir-"))
    process.env.PI_CODING_AGENT_DIR = agentDir
    const expires = Date.now() + 3_600_000
    writeFileSync(
      join(agentDir, "auth.json"),
      JSON.stringify({
        anthropic: {
          type: "oauth",
          access: "agent-dir-access",
          refresh: "agent-dir-refresh",
          expires,
        },
      }),
      "utf-8",
    )

    const creds = readCredentials()
    assert.ok(creds, "should resolve via PI_CODING_AGENT_DIR")
    assert.equal(creds.accessToken, "agent-dir-access")

    rmSync(agentDir, { recursive: true, force: true })
  })

  it("PI_AUTH_JSON wins over PI_CODING_AGENT_DIR when both are set", () => {
    // PI_AUTH_JSON is already pointing at `authJson` (the per-test temp).
    const decoyDir = mkdtempSync(join(tmpdir(), "pi-agent-decoy-"))
    process.env.PI_CODING_AGENT_DIR = decoyDir
    // Decoy file: if precedence were wrong we'd read this.
    writeFileSync(
      join(decoyDir, "auth.json"),
      JSON.stringify({
        anthropic: {
          type: "oauth",
          access: "DECOY",
          refresh: "DECOY",
          expires: Date.now() + 3_600_000,
        },
      }),
      "utf-8",
    )
    // The real file under PI_AUTH_JSON:
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: {
          type: "oauth",
          access: "WINNING",
          refresh: "WINNING",
          expires: Date.now() + 3_600_000,
        },
      }),
      "utf-8",
    )
    const creds = readCredentials()
    assert.ok(creds)
    assert.equal(creds.accessToken, "WINNING", "PI_AUTH_JSON must win")
    rmSync(decoyDir, { recursive: true, force: true })
  })
})

// ---------------------------------------------------------------------------
// getCachedCredentials — mtime-based cache invalidation (#2283)
// ---------------------------------------------------------------------------

describe("getCachedCredentials mtime invalidation", () => {
  function writeCreds(suffix: string, expires: number): void {
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: {
          type: "oauth",
          access: `access-${suffix}`,
          refresh: `refresh-${suffix}`,
          expires,
        },
      }),
      "utf-8",
    )
  }

  it("returns the cached blob on a second call when auth.json mtime is unchanged", () => {
    const expires = Date.now() + 3_600_000
    writeCreds("A", expires)

    const first = getCachedCredentials()
    assert.ok(first, "first call should populate the cache")
    assert.equal(first.accessToken, "access-A")

    // Rewrite the file with different content but force the mtime back
    // to its original value. The cache must still hit.
    const originalMtime = new Date(Date.now() - 60_000)
    utimesSync(authJson, originalMtime, originalMtime)
    writeCreds("B", expires)
    utimesSync(authJson, originalMtime, originalMtime)

    const second = getCachedCredentials()
    assert.ok(second, "second call should hit the cache")
    // The cache returns the originally-loaded "A" tokens, not the "B"
    // tokens we just wrote behind its back.
    assert.equal(
      second.accessToken,
      "access-A",
      "cache should return the originally-loaded creds when mtime is unchanged",
    )
  })

  it("bypasses the cache when auth.json mtime has advanced past cachedAt", () => {
    const expires = Date.now() + 3_600_000
    writeCreds("A", expires)

    const first = getCachedCredentials()
    assert.ok(first)
    assert.equal(first.accessToken, "access-A")

    // Simulate `prism account use other` mid-flight: the rename bumps
    // mtime to "now". We can't easily wait ≥1ms reliably in a unit test,
    // so explicitly set the mtime to a value larger than cachedAt.
    writeCreds("B", expires)
    const future = new Date(Date.now() + 10_000)
    utimesSync(authJson, future, future)

    const second = getCachedCredentials()
    assert.ok(second, "second call should re-read")
    assert.equal(
      second.accessToken,
      "access-B",
      "cache must invalidate when mtime > cachedAt and re-read the blob",
    )
  })

  it("treats a missing auth.json as a cache miss (does not throw)", () => {
    const expires = Date.now() + 3_600_000
    writeCreds("A", expires)

    const first = getCachedCredentials()
    assert.ok(first)

    // Delete the file behind the cache.
    rmSync(authJson)

    // statSync would throw — we expect the catch to fall through and
    // hit the cache (mtimeMs = 0, not > cachedAt), so the cache is
    // returned. This is intentional: the cache is the last good copy.
    const second = getCachedCredentials()
    assert.ok(second, "missing file should still return the cached creds")
    assert.equal(second.accessToken, "access-A")
  })
})

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
