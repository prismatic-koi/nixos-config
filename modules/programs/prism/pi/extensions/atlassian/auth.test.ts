// Unit tests for auth.ts — mtime-aware token cache, refresh-fallback,
// and mode-0o600 persistence (#2389).
//
// Run with: tsx --test auth.test.ts (from this directory)
// Or:       cd modules/programs/prism/pi/extensions/atlassian && tsx --test auth.test.ts
//
// auth.ts respects PI_ATLASSIAN_TOKENS env var so we point it at a temp file
// in each test, avoiding any dependency on the real home directory.
//
// Mirrors the shape of anthropic-oauth/credentials.test.ts (the precedent
// fix for this class in #2283).

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  utimesSync,
  writeFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import {
  loadTokens,
  saveTokens,
  invalidateCache,
  getTokenStorePath,
  getValidAccessToken,
  type AtlassianTokens,
} from "./auth.ts"

let tempDir: string
let tokenFile: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-atlassian-tokens-test-"))
  tokenFile = join(tempDir, "atlassian-mcp-oauth.json")
  process.env.PI_ATLASSIAN_TOKENS = tokenFile
  // The developer host (and the bwrap dispatcher) set PI_CODING_AGENT_DIR
  // unconditionally. Clear it so the existing PI_ATLASSIAN_TOKENS-keyed
  // test fixtures continue to win precedence; the dedicated precedence
  // test below sets it back as needed.
  delete process.env.PI_CODING_AGENT_DIR
  invalidateCache()
})

afterEach(() => {
  delete process.env.PI_ATLASSIAN_TOKENS
  delete process.env.PI_CODING_AGENT_DIR
  rmSync(tempDir, { recursive: true, force: true })
  invalidateCache()
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function writeTokenFile(tokens: Partial<AtlassianTokens>): void {
  writeFileSync(tokenFile, JSON.stringify(tokens, null, 2), "utf-8")
}

function makeTokens(overrides: Partial<AtlassianTokens> = {}): AtlassianTokens {
  return {
    accessToken: "acc",
    refreshToken: "ref",
    expiresAt: Date.now() + 3_600_000,
    clientId: "client-abc",
    ...overrides,
  }
}

// ---------------------------------------------------------------------------
// getTokenStorePath precedence
// ---------------------------------------------------------------------------

describe("getTokenStorePath precedence", () => {
  it("prefers PI_ATLASSIAN_TOKENS when set", () => {
    process.env.PI_ATLASSIAN_TOKENS = "/tmp/explicit-atlassian.json"
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent-decoy"
    assert.equal(getTokenStorePath(), "/tmp/explicit-atlassian.json")
  })

  it("falls back to PI_CODING_AGENT_DIR/atlassian-mcp-oauth.json when PI_ATLASSIAN_TOKENS is unset", () => {
    delete process.env.PI_ATLASSIAN_TOKENS
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent"
    assert.equal(getTokenStorePath(), "/tmp/coding-agent/atlassian-mcp-oauth.json")
  })

  it("falls back to ~/.pi/agent/atlassian-mcp-oauth.json when neither is set", () => {
    delete process.env.PI_ATLASSIAN_TOKENS
    delete process.env.PI_CODING_AGENT_DIR
    const path = getTokenStorePath()
    assert.match(path, /\.pi\/agent\/atlassian-mcp-oauth\.json$/)
  })
})

// ---------------------------------------------------------------------------
// loadTokens
// ---------------------------------------------------------------------------

describe("loadTokens", () => {
  it("returns null when the token file is absent", () => {
    assert.equal(loadTokens(), null)
  })

  it("returns null when the token file is malformed JSON", () => {
    writeFileSync(tokenFile, "not-json", "utf-8")
    assert.equal(loadTokens(), null)
  })

  it("returns null when required fields are missing", () => {
    writeTokenFile({ accessToken: "acc" }) // missing refresh, clientId
    assert.equal(loadTokens(), null)
  })

  it("returns the tokens from disk when the file is well-formed", () => {
    const tokens = makeTokens({ accessToken: "abc-1" })
    writeTokenFile(tokens)
    const loaded = loadTokens()
    assert.ok(loaded)
    assert.equal(loaded.accessToken, "abc-1")
    assert.equal(loaded.refreshToken, "ref")
    assert.equal(loaded.clientId, "client-abc")
  })
})

// ---------------------------------------------------------------------------
// loadTokens — mtime-aware cache (#2389)
// ---------------------------------------------------------------------------

describe("loadTokens mtime cache", () => {
  it("returns the cached blob on a second call when the file mtime is unchanged", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))

    const first = loadTokens()
    assert.ok(first)
    assert.equal(first.accessToken, "A")

    // Rewrite the file with different content but force the mtime back
    // to the original value. The cache must still hit.
    const originalMtime = new Date(Date.now() - 60_000)
    utimesSync(tokenFile, originalMtime, originalMtime)
    writeTokenFile(makeTokens({ accessToken: "B" }))
    utimesSync(tokenFile, originalMtime, originalMtime)

    const second = loadTokens()
    assert.ok(second)
    assert.equal(
      second.accessToken,
      "A",
      "cache should return the originally-loaded tokens when mtime is unchanged",
    )
  })

  it("reloads from disk when the file mtime has advanced past the cached value", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))

    const first = loadTokens()
    assert.ok(first)
    assert.equal(first.accessToken, "A")

    // Simulate a peer pi process rewriting the file with fresh tokens.
    writeTokenFile(makeTokens({ accessToken: "B" }))
    const future = new Date(Date.now() + 10_000)
    utimesSync(tokenFile, future, future)

    const second = loadTokens()
    assert.ok(second)
    assert.equal(
      second.accessToken,
      "B",
      "cache must invalidate when mtime > cached mtime and re-read the blob",
    )
  })

  it("returns the cached copy when the file has been deleted after cache-populate", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))

    const first = loadTokens()
    assert.ok(first)

    rmSync(tokenFile)

    // statSync would throw — the cache is the last good copy, so the second
    // call returns "A" without throwing.
    const second = loadTokens()
    assert.ok(second)
    assert.equal(second.accessToken, "A")
  })

  it("returns null when file is absent and the cache is empty (post-invalidate)", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))
    loadTokens() // populates cache

    invalidateCache()
    rmSync(tokenFile)

    // Cache is empty AND disk is empty → null.
    assert.equal(loadTokens(), null)
  })
})

// ---------------------------------------------------------------------------
// saveTokens
// ---------------------------------------------------------------------------

describe("saveTokens", () => {
  it("writes the JSON blob and refreshes the cache", () => {
    const tokens = makeTokens({ accessToken: "saved" })
    saveTokens(tokens)

    const onDisk = JSON.parse(readFileSync(tokenFile, "utf-8"))
    assert.equal(onDisk.accessToken, "saved")

    // Follow-up loadTokens() reads the cache — verify the accessToken
    // matches without re-reading disk.
    const loaded = loadTokens()
    assert.ok(loaded)
    assert.equal(loaded.accessToken, "saved")
  })

  it("persists with mode 0o600 (owner-only rw)", () => {
    // Skip on Windows where POSIX mode bits are not meaningful.
    if (process.platform === "win32") return

    saveTokens(makeTokens({ accessToken: "mode-test" }))
    const mode = statSync(tokenFile).mode & 0o777
    assert.equal(mode, 0o600, `expected 0o600, got 0o${mode.toString(8)}`)
  })

  it("re-applies mode 0o600 on overwrite of an existing more-permissive file", () => {
    if (process.platform === "win32") return

    // Create the file with a more-permissive mode first
    writeFileSync(tokenFile, "{}", { encoding: "utf-8", mode: 0o644 })
    assert.equal(statSync(tokenFile).mode & 0o777, 0o644)

    saveTokens(makeTokens())

    const mode = statSync(tokenFile).mode & 0o777
    assert.equal(
      mode,
      0o600,
      `saveTokens must chmod on overwrite; got 0o${mode.toString(8)}`,
    )
  })

  it("creates parent directories when needed", () => {
    const nested = join(tempDir, "deep", "nested", "atlassian-mcp-oauth.json")
    process.env.PI_ATLASSIAN_TOKENS = nested
    invalidateCache()

    saveTokens(makeTokens({ accessToken: "nested" }))
    const data = JSON.parse(readFileSync(nested, "utf-8"))
    assert.equal(data.accessToken, "nested")
  })
})

// ---------------------------------------------------------------------------
// invalidateCache
// ---------------------------------------------------------------------------

describe("invalidateCache", () => {
  it("clears the cache so the next loadTokens() re-reads from disk", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))

    const first = loadTokens()
    assert.ok(first)
    assert.equal(first.accessToken, "A")

    // Rewrite the file WITHOUT bumping mtime — without invalidate, cache
    // would hit and return "A".
    const originalMtime = new Date(Date.now() - 60_000)
    utimesSync(tokenFile, originalMtime, originalMtime)
    writeTokenFile(makeTokens({ accessToken: "B" }))
    utimesSync(tokenFile, originalMtime, originalMtime)

    invalidateCache()

    const second = loadTokens()
    assert.ok(second)
    assert.equal(
      second.accessToken,
      "B",
      "invalidateCache() must force a disk re-read on next load",
    )
  })
})

// ---------------------------------------------------------------------------
// getValidAccessToken — cross-process refresh-failure fallback (#2389)
// ---------------------------------------------------------------------------

describe("getValidAccessToken", () => {
  it("throws 'Atlassian MCP: no auth tokens' when the store is empty", async () => {
    await assert.rejects(
      () => getValidAccessToken(),
      /Atlassian MCP: no auth tokens/,
    )
  })

  it("returns the cached access token when it is not expired", async () => {
    const tokens = makeTokens({
      accessToken: "still-valid",
      expiresAt: Date.now() + 3_600_000,
    })
    writeTokenFile(tokens)

    const { token, tokens: returned } = await getValidAccessToken()
    assert.equal(token, "still-valid")
    assert.equal(returned.accessToken, "still-valid")
  })

  it("returns the refreshed token when the current one is expired", async () => {
    const originalFetch = globalThis.fetch
    const tokens = makeTokens({
      accessToken: "expired",
      refreshToken: "still-good-refresh",
      expiresAt: Date.now() - 1000, // already expired
    })
    writeTokenFile(tokens)

    // Mock fetch to return a fresh access + refresh token
    globalThis.fetch = (async (_url: unknown, _init: unknown) => {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          access_token: "fresh-access",
          refresh_token: "fresh-refresh",
          expires_in: 3600,
        }),
        text: async () =>
          '{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}',
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    try {
      const { token, tokens: returned } = await getValidAccessToken()
      assert.equal(token, "fresh-access")
      assert.equal(returned.accessToken, "fresh-access")
      assert.equal(returned.refreshToken, "fresh-refresh")

      // File on disk was updated with the refreshed tokens
      const onDisk = JSON.parse(readFileSync(tokenFile, "utf-8"))
      assert.equal(onDisk.accessToken, "fresh-access")
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("reloads from disk and returns the newer copy when refreshTokens throws", async () => {
    const originalFetch = globalThis.fetch

    // Initial tokens are expired — refresh will be attempted.
    const inMemory = makeTokens({
      accessToken: "our-stale-access",
      refreshToken: "our-stale-refresh",
      expiresAt: Date.now() - 1000,
    })
    writeTokenFile(inMemory)
    // Populate the cache with the expired copy AND freeze its recorded mtime
    // so the initial loadTokens() inside getValidAccessToken() returns from
    // cache (not from a peer-fresh disk read). Only after refresh throws
    // and invalidateCache() runs should we observe the peer's fresh tokens
    // — that's the specific path this test exercises.
    const initialLoad = loadTokens()
    assert.ok(initialLoad)
    assert.equal(initialLoad.accessToken, "our-stale-access")

    // Peer rewrites the token file with fresh, non-expired tokens WITHOUT
    // bumping mtime (utimesSync back to the original mtime). This models
    // the worst case: our mtime cache doesn't see the peer write, so we
    // proceed to refresh, refresh fails, and the ONLY way to recover is
    // the invalidateCache()+loadTokens() fallback.
    const originalMtime = new Date(statSync(tokenFile).mtimeMs - 1)
    const peerTokens = makeTokens({
      accessToken: "peer-fresh-access",
      refreshToken: "peer-fresh-refresh",
      expiresAt: Date.now() + 3_600_000,
    })
    writeTokenFile(peerTokens)
    utimesSync(tokenFile, originalMtime, originalMtime)

    // Sanity: a plain loadTokens() (mtime cache) still returns the stale copy
    // because mtime hasn't advanced.
    const staleView = loadTokens()
    assert.ok(staleView)
    assert.equal(
      staleView.accessToken,
      "our-stale-access",
      "pre-condition: mtime cache should still see the stale copy",
    )

    // Our refresh call throws invalid_grant because the peer already rotated.
    globalThis.fetch = (async () => {
      return {
        ok: false,
        status: 400,
        json: async () => ({ error: "invalid_grant" }),
        text: async () => '{"error":"invalid_grant"}',
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    try {
      const { token } = await getValidAccessToken()
      // We should observe the peer's fresh access token, NOT the refresh error.
      // This exercises the invalidateCache()+loadTokens() fallback — the
      // mtime-cache path was disabled above.
      assert.equal(
        token,
        "peer-fresh-access",
        "refresh-failure fallback should reload from disk and use the peer's fresh tokens",
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("surfaces the refresh error when refresh fails AND disk has no newer tokens", async () => {
    const originalFetch = globalThis.fetch

    const tokens = makeTokens({
      accessToken: "still-stale",
      refreshToken: "still-stale-refresh",
      expiresAt: Date.now() - 1000,
    })
    writeTokenFile(tokens)

    globalThis.fetch = (async () => {
      return {
        ok: false,
        status: 400,
        json: async () => ({ error: "invalid_grant" }),
        text: async () => '{"error":"invalid_grant"}',
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    try {
      await assert.rejects(
        () => getValidAccessToken(),
        /Token refresh failed/,
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })
})

// ---------------------------------------------------------------------------
// Cross-process visibility (AC: peer sees rotated tokens on next call)
// ---------------------------------------------------------------------------

describe("cross-process visibility after saveTokens", () => {
  it("a follow-up loadTokens() from an emptied cache sees the rotated fields", () => {
    // Process A refreshes and saves
    const rotated = makeTokens({
      accessToken: "rotated-access",
      refreshToken: "rotated-refresh",
      expiresAt: Date.now() + 3_600_000,
    })
    saveTokens(rotated)

    // Simulate Process B's fresh view: its cache is empty, so it reads
    // straight from disk.
    invalidateCache()
    const seen = loadTokens()
    assert.ok(seen)
    assert.equal(seen.accessToken, "rotated-access")
    assert.equal(seen.refreshToken, "rotated-refresh")
    assert.equal(seen.expiresAt, rotated.expiresAt)
  })
})
