// Unit tests for the round-2 concurrency hardening in auth.ts.
//
// Run with: tsx --test concurrency.test.ts (from this directory)
//
// Round-1 review found, independently from review-code and review-security,
// that the cross-process lock could be broken out from under a LIVE holder:
// `refreshTokens` issued an unbounded fetch (undici's default headers timeout
// is 300s) while the stale-lock threshold was 30s. A wedged POST — sleep/wake
// mid-request, VPN flap, captive portal — would be judged stale by a peer,
// which would then re-read the still-unchanged token file and refresh with the
// SAME refresh token. That is precisely the unserialised double-refresh that
// Notion punishes with whole-grant revocation, i.e. the one failure this
// module exists to prevent.
//
// Two fixes, tested here:
//
//   1. Every OAuth request is bounded by TOKEN_REQUEST_TIMEOUT_MS, which is
//      held well below the stale threshold by an asserted invariant. A live
//      holder can no longer outlive its own lock.
//   2. `getValidAccessToken` re-checks `ownsLock(handle)` after the refresh
//      and before the write. If the lock was lost anyway (SIGSTOP, machine
//      sleep, clock jump) it never writes: it defers to a peer's published
//      rotation, or clears the store when nobody published, because the
//      refresh token left on disk has been rotated away and would revoke the
//      grant on its next replay.
//
// Revert-and-watch-fail:
//   * Drop `signal: AbortSignal.timeout(...)` from `oauthFetch` →
//     "aborts a hung token request..." hits its per-test timeout and FAILS.
//   * Drop the `if (!ownsLock(handle))` branch in `getValidAccessToken` →
//     "defers to a peer's published rotation" and "clears the store when the
//     lock was lost and nobody published" both FAIL.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import {
  acquireTokenLock,
  getLockDirPath,
  getValidAccessToken,
  invalidateCache,
  LOCK_TIMINGS,
  NotionAuthTerminalError,
  ownsLock,
  refreshTokens,
  releaseTokenLock,
  type NotionTokens,
} from "./auth.ts"

let tempDir: string
let tokenFile: string
let originalFetch: typeof globalThis.fetch

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-notion-concurrency-test-"))
  tokenFile = join(tempDir, "notion-mcp-oauth.json")
  process.env.PI_NOTION_TOKENS = tokenFile
  process.env.PI_NOTION_CLIENT = join(tempDir, "notion-mcp-client.json")
  delete process.env.PI_CODING_AGENT_DIR
  delete process.env.NOTION_MCP_DEBUG
  originalFetch = globalThis.fetch
  invalidateCache()
})

afterEach(() => {
  globalThis.fetch = originalFetch
  delete process.env.PI_NOTION_TOKENS
  delete process.env.PI_NOTION_CLIENT
  delete process.env.PI_NOTION_TOKEN_TIMEOUT_MS
  delete process.env.PI_NOTION_LOCK_TIMEOUT_MS
  delete process.env.PI_NOTION_LOCK_STALE_MS
  rmSync(tempDir, { recursive: true, force: true })
  invalidateCache()
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeTokens(overrides: Partial<NotionTokens> = {}): NotionTokens {
  return {
    accessToken: "acc",
    refreshToken: "ref",
    expiresAt: Date.now() + 3_600_000,
    clientId: "client-abc",
    ...overrides,
  }
}

function writeTokenFile(tokens: Partial<NotionTokens>): void {
  writeFileSync(tokenFile, JSON.stringify(tokens, null, 2), "utf-8")
}

function jsonResponse(body: unknown, status = 200): Response {
  const text = JSON.stringify(body)
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => JSON.parse(text),
    text: async () => text,
    headers: new Headers(),
  } as Response
}

// Every test that installs `hangingFetch` carries an explicit per-test
// `timeout`. node:test's default timeout is infinite, so without one the
// revert-and-watch-fail check for the AbortSignal fix would HANG instead of
// failing — which is a useless signal. With it, removing the signal from
// `oauthFetch` produces a clean, fast test failure.
const HANG_GUARD_MS = 5000

/** A fetch that never settles unless its abort signal fires. */
function hangingFetch(): typeof fetch {
  return ((_url: unknown, init: RequestInit) =>
    new Promise((_resolve, reject) => {
      init.signal?.addEventListener("abort", () => {
        const err = new Error("The operation was aborted due to timeout")
        err.name = "TimeoutError"
        reject(err)
      })
    })) as unknown as typeof fetch
}

// ---------------------------------------------------------------------------
// Timing invariants
// ---------------------------------------------------------------------------

describe("lock timing invariants", () => {
  it("bounds a token request well inside the stale-lock threshold", () => {
    assert.ok(
      LOCK_TIMINGS.tokenRequestTimeoutMs * 2 <= LOCK_TIMINGS.staleMs,
      `token timeout ${LOCK_TIMINGS.tokenRequestTimeoutMs}ms must leave ample headroom ` +
        `under the ${LOCK_TIMINGS.staleMs}ms stale threshold, or a live holder can be ` +
        "judged stale and preempted mid-refresh",
    )
  })

  it("lets a crashed peer's lock become breakable before we give up on it", () => {
    assert.ok(
      LOCK_TIMINGS.acquireTimeoutMs > LOCK_TIMINGS.staleMs,
      "acquire timeout must exceed the stale threshold",
    )
  })
})

// ---------------------------------------------------------------------------
// Bounded token requests
// ---------------------------------------------------------------------------

describe("bounded token requests", () => {
  it(
    "aborts a hung token request instead of holding the lock indefinitely",
    { timeout: HANG_GUARD_MS },
    async () => {
      process.env.PI_NOTION_TOKEN_TIMEOUT_MS = "100"
      writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
      invalidateCache()
      globalThis.fetch = hangingFetch()

      await assert.rejects(() => getValidAccessToken(), /timed out after 100ms/)

      assert.equal(
        existsSync(getLockDirPath()),
        false,
        "the lock must be released once the request is abandoned",
      )
      assert.equal(
        existsSync(tokenFile),
        true,
        "a timeout says nothing about the grant — the tokens must survive",
      )
    },
  )

  it("passes an abort signal on the refresh request", async () => {
    let sawSignal = false
    globalThis.fetch = ((_url: unknown, init: RequestInit) => {
      sawSignal = init.signal instanceof AbortSignal
      return Promise.resolve(
        jsonResponse({ access_token: "a", refresh_token: "b", expires_in: 3600 }),
      )
    }) as unknown as typeof fetch

    await refreshTokens(makeTokens())
    assert.equal(sawSignal, true, "refreshTokens must bound its request")
  })

  it(
    "surfaces a timeout as non-terminal so the grant is not discarded",
    { timeout: HANG_GUARD_MS },
    async () => {
      process.env.PI_NOTION_TOKEN_TIMEOUT_MS = "50"
      globalThis.fetch = hangingFetch()

      await assert.rejects(
        () => refreshTokens(makeTokens()),
        (err: unknown) => {
          assert.ok(
            !(err instanceof NotionAuthTerminalError),
            "a network timeout must not be mistaken for a dead grant",
          )
          return true
        },
      )
    },
  )
})

// ---------------------------------------------------------------------------
// Lock-ownership revalidation before the write
// ---------------------------------------------------------------------------

describe("lock-ownership revalidation before the write", () => {
  it("ownsLock reports loss once the lock directory is gone", async () => {
    const handle = await acquireTokenLock()
    assert.equal(ownsLock(handle), true)
    rmSync(handle.dir, { recursive: true, force: true })
    assert.equal(ownsLock(handle), false)
    releaseTokenLock(handle)
  })

  it("ownsLock reports loss once another owner holds the directory", async () => {
    const handle = await acquireTokenLock()
    writeFileSync(
      join(handle.dir, "owner"),
      JSON.stringify({ id: "someone-else", pid: 1, ts: Date.now() }),
    )
    assert.equal(ownsLock(handle), false)
    rmSync(handle.dir, { recursive: true, force: true })
  })

  it("defers to a peer's published rotation instead of clobbering it", async () => {
    writeTokenFile(
      makeTokens({
        accessToken: "stale",
        refreshToken: "rt-original",
        expiresAt: Date.now() - 1,
      }),
    )
    invalidateCache()

    globalThis.fetch = (async () => {
      // Mid-flight: a peer judges us stale, breaks our lock, refreshes, and
      // publishes its own rotation before we get our response back.
      rmSync(getLockDirPath(), { recursive: true, force: true })
      writeFileSync(
        tokenFile,
        JSON.stringify(
          makeTokens({
            accessToken: "peer-access",
            refreshToken: "rt-peer",
            expiresAt: Date.now() + 3_600_000,
          }),
        ),
      )
      return jsonResponse({
        access_token: "ours-access",
        refresh_token: "rt-ours",
        expires_in: 3600,
      })
    }) as typeof fetch

    const { token } = await getValidAccessToken()

    assert.equal(token, "peer-access", "the peer's write is the more recent one")
    assert.equal(
      JSON.parse(readFileSync(tokenFile, "utf-8")).refreshToken,
      "rt-peer",
      "our rotated pair must not overwrite the peer's — doing so strands a " +
        "rotated-away refresh token for the next refresh to replay",
    )
  })

  it("clears the store when the lock was lost and nobody published", async () => {
    writeTokenFile(
      makeTokens({
        accessToken: "stale",
        refreshToken: "rt-original",
        expiresAt: Date.now() - 1,
      }),
    )
    invalidateCache()

    globalThis.fetch = (async () => {
      rmSync(getLockDirPath(), { recursive: true, force: true })
      return jsonResponse({
        access_token: "ours-access",
        refresh_token: "rt-ours",
        expires_in: 3600,
      })
    }) as typeof fetch

    await assert.rejects(
      () => getValidAccessToken(),
      (err: unknown) => {
        assert.ok(err instanceof NotionAuthTerminalError)
        assert.match((err as Error).message, /login-notion/)
        return true
      },
    )

    assert.equal(
      existsSync(tokenFile),
      false,
      "the refresh token on disk was rotated away by our own call — leaving it " +
        "would let the next refresh replay it and revoke the whole grant",
    )
  })

  it("writes normally when the lock was held throughout", async () => {
    writeTokenFile(
      makeTokens({ accessToken: "stale", refreshToken: "rt-0", expiresAt: Date.now() - 1 }),
    )
    invalidateCache()

    globalThis.fetch = (async () =>
      jsonResponse({
        access_token: "ours-access",
        refresh_token: "rt-ours",
        expires_in: 3600,
      })) as typeof fetch

    const { token } = await getValidAccessToken()

    assert.equal(token, "ours-access")
    assert.equal(JSON.parse(readFileSync(tokenFile, "utf-8")).refreshToken, "rt-ours")
    assert.equal(existsSync(getLockDirPath()), false)
  })
})
