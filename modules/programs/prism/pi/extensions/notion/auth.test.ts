// Unit tests for notion/auth.ts.
//
// Run with: tsx --test auth.test.ts (from this directory)
// Or:       cd modules/programs/prism/pi/extensions/notion && tsx --test
//
// The four security-critical behaviours called out in issue #2448 each have a
// dedicated describe block, and each names the mutation that makes it fail
// (the revert-and-watch-fail pair required by AGENTS.md):
//
//   1. "cross-process refresh lock"   — delete the acquireTokenLock() call, or
//                                       the post-lock re-read, in
//                                       getValidAccessToken().
//   2. "atomic token writes"          — replace writeJsonAtomic()'s
//                                       open/fchmod/write/rename with a plain
//                                       writeFileSync(path, ...).
//   3. "invalid_grant is terminal"    — drop the isInvalidGrantBody() branch
//                                       in refreshTokens(), or the
//                                       clearTokens() call in
//                                       getValidAccessToken().
//   4. "client registration reuse"    — drop the `if (existing && ...) return
//                                       existing` branch in
//                                       ensureClientRegistration().
//
// auth.ts honours PI_NOTION_TOKENS / PI_NOTION_CLIENT so every test points at
// a temp directory and nothing touches the real home directory. (This matters
// beyond hygiene: the nix build runs with $HOME=/homeless-shelter.)

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import {
  closeSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  utimesSync,
  writeFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import {
  acquireTokenLock,
  clearTokens,
  createLocalCallbackServer,
  ensureClientRegistration,
  generatePKCE,
  getClientStorePath,
  getLockDirPath,
  getTokenStorePath,
  getValidAccessToken,
  invalidateCache,
  isInvalidGrantBody,
  loadClientRegistration,
  loadTokens,
  makeAuthorizeUrl,
  needsRefresh,
  NotionAuthTerminalError,
  parseAuthInput,
  refreshTokens,
  releaseTokenLock,
  saveTokens,
  toBase64Url,
  withTokenLock,
  writeJsonAtomic,
  type NotionTokens,
} from "./auth.ts"

let tempDir: string
let tokenFile: string
let clientFile: string
let originalFetch: typeof globalThis.fetch

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-notion-tokens-test-"))
  tokenFile = join(tempDir, "notion-mcp-oauth.json")
  clientFile = join(tempDir, "notion-mcp-client.json")
  process.env.PI_NOTION_TOKENS = tokenFile
  process.env.PI_NOTION_CLIENT = clientFile
  // The developer host (and both prism dispatchers) set PI_CODING_AGENT_DIR
  // unconditionally. Clear it so PI_NOTION_TOKENS wins precedence; the
  // dedicated precedence test sets it back as needed.
  delete process.env.PI_CODING_AGENT_DIR
  delete process.env.NOTION_MCP_DEBUG
  originalFetch = globalThis.fetch
  invalidateCache()
})

afterEach(() => {
  globalThis.fetch = originalFetch
  delete process.env.PI_NOTION_TOKENS
  delete process.env.PI_NOTION_CLIENT
  delete process.env.PI_CODING_AGENT_DIR
  delete process.env.PI_NOTION_LOCK_TIMEOUT_MS
  delete process.env.PI_NOTION_LOCK_STALE_MS
  delete process.env.NOTION_MCP_DEBUG
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

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
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

function textResponse(text: string, status: number): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => JSON.parse(text),
    text: async () => text,
    headers: new Headers(),
  } as Response
}

function mode(path: string): number {
  return statSync(path).mode & 0o777
}

// ---------------------------------------------------------------------------
// Store paths
// ---------------------------------------------------------------------------

describe("getTokenStorePath precedence", () => {
  it("prefers PI_NOTION_TOKENS when set", () => {
    process.env.PI_NOTION_TOKENS = "/tmp/explicit-notion.json"
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent-decoy"
    assert.equal(getTokenStorePath(), "/tmp/explicit-notion.json")
  })

  it("falls back to PI_CODING_AGENT_DIR/notion-mcp-oauth.json", () => {
    delete process.env.PI_NOTION_TOKENS
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent"
    assert.equal(getTokenStorePath(), "/tmp/coding-agent/notion-mcp-oauth.json")
  })

  it("falls back to ~/.pi/agent/notion-mcp-oauth.json when neither is set", () => {
    delete process.env.PI_NOTION_TOKENS
    delete process.env.PI_CODING_AGENT_DIR
    assert.match(getTokenStorePath(), /\.pi\/agent\/notion-mcp-oauth\.json$/)
  })

  it("keeps the client registration in a sibling file, not the token file", () => {
    delete process.env.PI_NOTION_CLIENT
    assert.equal(getClientStorePath(), join(tempDir, "notion-mcp-client.json"))
    assert.notEqual(getClientStorePath(), getTokenStorePath())
  })
})

// ---------------------------------------------------------------------------
// 2. Atomic token writes  (AC: security / atomic 0600 temp-file + rename)
//
// Revert-and-watch-fail: replace the body of writeJsonAtomic with
//   writeFileSync(path, JSON.stringify(value, null, 2), { mode: 0o600 })
// "replaces the destination via rename" and "a reader holding the old fd
// still sees complete content" both fail, because writeFileSync truncates
// the existing inode in place.
// ---------------------------------------------------------------------------

describe("atomic token writes", () => {
  it("persists with mode 0600", () => {
    saveTokens(makeTokens({ accessToken: "mode-test" }))
    assert.equal(mode(tokenFile), 0o600, `expected 0600, got 0o${mode(tokenFile).toString(8)}`)
  })

  it("creates the temp file at 0600 regardless of a permissive umask", () => {
    const previous = process.umask(0o000)
    try {
      saveTokens(makeTokens())
      assert.equal(
        mode(tokenFile),
        0o600,
        "the mode must come from the explicit open()/fchmod(), not the umask",
      )
    } finally {
      process.umask(previous)
    }
  })

  it("re-applies 0600 when the destination already existed more permissively", () => {
    writeFileSync(tokenFile, "{}", { encoding: "utf-8", mode: 0o644 })
    assert.equal(mode(tokenFile), 0o644)

    saveTokens(makeTokens())
    assert.equal(mode(tokenFile), 0o600)
  })

  it("replaces the destination via rename rather than truncating it", () => {
    saveTokens(makeTokens({ accessToken: "first" }))
    const firstIno = statSync(tokenFile).ino

    saveTokens(makeTokens({ accessToken: "second" }))
    const secondIno = statSync(tokenFile).ino

    assert.notEqual(
      firstIno,
      secondIno,
      "an atomic write swaps in a new inode; an in-place truncate keeps the old one",
    )
  })

  it("leaves a concurrent reader's already-open fd on complete content", () => {
    // This is the observable form of "a concurrent reader never sees a partial
    // or truncated file". A reader that opened the file before the write must
    // still read the whole previous document, because rename(2) only swaps the
    // dentry — it never touches the inode the reader is holding.
    const before = makeTokens({ accessToken: "before-swap", refreshToken: "r-before" })
    saveTokens(before)

    const readerFd = openSync(tokenFile, "r")
    try {
      saveTokens(makeTokens({ accessToken: "after-swap", refreshToken: "r-after" }))

      const seen = JSON.parse(readFileSync(readerFd, "utf-8")) as NotionTokens
      assert.equal(seen.accessToken, "before-swap")
      assert.equal(seen.refreshToken, "r-before")
    } finally {
      closeSync(readerFd)
    }

    // And a fresh reader sees the new document, whole.
    const now = JSON.parse(readFileSync(tokenFile, "utf-8")) as NotionTokens
    assert.equal(now.accessToken, "after-swap")
  })

  it("leaves no temp files behind", () => {
    for (let i = 0; i < 5; i++) saveTokens(makeTokens({ accessToken: `t${i}` }))
    const strays = readdirSync(tempDir).filter((f) => f.includes(".notion-mcp.tmp."))
    assert.deepEqual(strays, [], `temp files leaked: ${strays.join(", ")}`)
  })

  it("creates parent directories at 0700 when needed", () => {
    const nested = join(tempDir, "deep", "nested", "notion-mcp-oauth.json")
    process.env.PI_NOTION_TOKENS = nested
    invalidateCache()

    saveTokens(makeTokens({ accessToken: "nested" }))
    assert.equal(JSON.parse(readFileSync(nested, "utf-8")).accessToken, "nested")
    assert.equal(mode(join(tempDir, "deep", "nested")), 0o700)
  })

  it("writeJsonAtomic round-trips arbitrary JSON", () => {
    const target = join(tempDir, "blob.json")
    writeJsonAtomic(target, { a: 1, b: ["x", "y"] })
    assert.deepEqual(JSON.parse(readFileSync(target, "utf-8")), { a: 1, b: ["x", "y"] })
    assert.equal(mode(target), 0o600)
  })
})

// ---------------------------------------------------------------------------
// loadTokens / cache
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
    writeTokenFile({ accessToken: "acc" })
    assert.equal(loadTokens(), null)
  })

  it("reloads from disk when the file mtime advances", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))
    assert.equal(loadTokens()?.accessToken, "A")

    writeTokenFile(makeTokens({ accessToken: "B" }))
    const future = new Date(Date.now() + 10_000)
    utimesSync(tokenFile, future, future)

    assert.equal(loadTokens()?.accessToken, "B")
  })

  it("returns the cached copy when the file is deleted after cache-populate", () => {
    writeTokenFile(makeTokens({ accessToken: "A" }))
    assert.ok(loadTokens())
    rmSync(tokenFile)
    assert.equal(loadTokens()?.accessToken, "A")
  })
})

describe("clearTokens", () => {
  it("removes the token file and empties the cache", () => {
    saveTokens(makeTokens({ accessToken: "doomed" }))
    assert.ok(existsSync(tokenFile))

    clearTokens()

    assert.equal(existsSync(tokenFile), false)
    assert.equal(loadTokens(), null)
  })

  it("leaves the client registration intact (re-registering orphans grants)", () => {
    saveTokens(makeTokens())
    writeJsonAtomic(clientFile, {
      clientId: "keep-me",
      redirectUri: "http://localhost:3738/oauth/callback",
      registeredAt: 1,
    })

    clearTokens()

    assert.equal(loadClientRegistration()?.clientId, "keep-me")
  })
})

describe("needsRefresh", () => {
  it("is false for a token comfortably inside its lifetime", () => {
    assert.equal(needsRefresh(makeTokens({ expiresAt: Date.now() + 60_000 })), false)
  })

  it("is true once expiresAt has passed", () => {
    assert.equal(needsRefresh(makeTokens({ expiresAt: Date.now() - 1 })), true)
  })
})

// ---------------------------------------------------------------------------
// 1. Cross-process refresh lock  (AC: security / serialised read-refresh-write)
//
// Revert-and-watch-fail:
//   * Delete the `acquireTokenLock()` call in getValidAccessToken (refresh
//     unguarded) → "serialises concurrent refreshes" sees 3 network refreshes.
//   * Keep the lock but delete the post-lock `invalidateCache(); loadTokens()`
//     re-read → "serialises concurrent refreshes" and "skips the refresh
//     entirely when a peer rotated while we queued" both see extra refreshes,
//     which is precisely the rotated-token replay that revokes the grant.
// ---------------------------------------------------------------------------

describe("cross-process refresh lock", () => {
  it("serialises concurrent refreshes so only one reaches the network", async () => {
    writeTokenFile(makeTokens({ accessToken: "stale", refreshToken: "r0", expiresAt: Date.now() - 1 }))
    invalidateCache()

    let refreshCalls = 0
    globalThis.fetch = (async () => {
      refreshCalls++
      // Hold the lock long enough that the peers are guaranteed to queue.
      await sleep(80)
      return jsonResponse({
        access_token: `fresh-${refreshCalls}`,
        refresh_token: `r-fresh-${refreshCalls}`,
        expires_in: 3600,
      })
    }) as typeof fetch

    const results = await Promise.all([
      getValidAccessToken(),
      getValidAccessToken(),
      getValidAccessToken(),
    ])

    assert.equal(
      refreshCalls,
      1,
      "every concurrent caller past the first must observe the peer's rotation, not refresh again",
    )
    assert.equal(results[0].token, "fresh-1")
    assert.equal(results[1].token, "fresh-1")
    assert.equal(results[2].token, "fresh-1")
  })

  it("holds the lock across the whole read-refresh-write window", async () => {
    writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
    invalidateCache()

    let sawLockDuringNetworkCall = false
    globalThis.fetch = (async () => {
      sawLockDuringNetworkCall = existsSync(getLockDirPath())
      return jsonResponse({
        access_token: "fresh",
        refresh_token: "r-fresh",
        expires_in: 3600,
      })
    }) as typeof fetch

    await getValidAccessToken()

    assert.equal(sawLockDuringNetworkCall, true, "the lock must be held during the refresh")
    assert.equal(
      existsSync(getLockDirPath()),
      false,
      "the lock must be released once the write completes",
    )
    // The write happened inside the lock, so the rotated token is on disk.
    assert.equal(JSON.parse(readFileSync(tokenFile, "utf-8")).refreshToken, "r-fresh")
  })

  it("skips the refresh entirely when a peer rotated while we queued", async () => {
    writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
    invalidateCache()

    // A peer process takes the lock before we do.
    const lockDir = getLockDirPath()
    mkdirSync(lockDir, { recursive: true })
    writeFileSync(
      join(lockDir, "owner"),
      JSON.stringify({ id: "peer", pid: 999999, ts: Date.now() }),
    )

    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error("refresh must not be attempted — the peer already rotated")
    }) as typeof fetch

    // The peer finishes its rotation and drops the lock.
    setTimeout(() => {
      writeFileSync(
        tokenFile,
        JSON.stringify(
          makeTokens({ accessToken: "peer-fresh", refreshToken: "r-peer", expiresAt: Date.now() + 3_600_000 }),
        ),
      )
      rmSync(lockDir, { recursive: true, force: true })
    }, 80)

    const { token } = await getValidAccessToken()

    assert.equal(token, "peer-fresh")
    assert.equal(fetchCalls, 0, "the post-lock re-read must make the refresh unnecessary")
  })

  it("breaks a stale lock left behind by a crashed peer", async () => {
    process.env.PI_NOTION_LOCK_STALE_MS = "50"
    process.env.PI_NOTION_LOCK_TIMEOUT_MS = "5000"

    const lockDir = getLockDirPath()
    mkdirSync(lockDir, { recursive: true })
    writeFileSync(
      join(lockDir, "owner"),
      JSON.stringify({ id: "crashed", pid: 999999, ts: Date.now() - 10_000 }),
    )

    const handle = await acquireTokenLock()
    try {
      assert.notEqual(handle.id, "crashed")
      assert.ok(existsSync(lockDir))
    } finally {
      releaseTokenLock(handle)
    }
    assert.equal(existsSync(lockDir), false)
  })

  it("never refreshes unserialised — a lock timeout falls back to a read-only reload", async () => {
    // Zero timeout + huge stale window: the lock can never be taken.
    process.env.PI_NOTION_LOCK_TIMEOUT_MS = "0"
    process.env.PI_NOTION_LOCK_STALE_MS = "3600000"

    writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
    invalidateCache()
    loadTokens() // populate the cache with the expired copy

    const lockDir = getLockDirPath()
    mkdirSync(lockDir, { recursive: true })
    writeFileSync(join(lockDir, "owner"), JSON.stringify({ id: "peer", pid: 1, ts: Date.now() }))

    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error("must never refresh without the lock")
    }) as typeof fetch

    // A peer has already published fresh tokens; the read-only fallback finds
    // them without ever touching the token endpoint.
    writeFileSync(
      tokenFile,
      JSON.stringify(makeTokens({ accessToken: "peer-fresh", expiresAt: Date.now() + 3_600_000 })),
    )

    const { token } = await getValidAccessToken()
    assert.equal(token, "peer-fresh")
    assert.equal(fetchCalls, 0)

    rmSync(lockDir, { recursive: true, force: true })
  })

  it("propagates the lock timeout when there is no fresher copy to fall back to", async () => {
    process.env.PI_NOTION_LOCK_TIMEOUT_MS = "0"
    process.env.PI_NOTION_LOCK_STALE_MS = "3600000"

    writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
    invalidateCache()

    const lockDir = getLockDirPath()
    mkdirSync(lockDir, { recursive: true })
    writeFileSync(join(lockDir, "owner"), JSON.stringify({ id: "peer", pid: 1, ts: Date.now() }))

    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error("must never refresh without the lock")
    }) as typeof fetch

    await assert.rejects(() => getValidAccessToken(), /timed out acquiring the token lock/)
    assert.equal(fetchCalls, 0)

    rmSync(lockDir, { recursive: true, force: true })
  })

  it("withTokenLock releases the lock even when the body throws", async () => {
    await assert.rejects(
      () =>
        withTokenLock(async () => {
          assert.ok(existsSync(getLockDirPath()))
          throw new Error("boom")
        }),
      /boom/,
    )
    assert.equal(existsSync(getLockDirPath()), false)
  })
})

// ---------------------------------------------------------------------------
// 3. invalid_grant is terminal  (AC: security / clear + prompt + no retry)
//
// Revert-and-watch-fail:
//   * Remove the isInvalidGrantBody() branch in refreshTokens (so it throws a
//     plain Error) → "raises a terminal error" fails.
//   * Remove the clearTokens() call in getValidAccessToken's catch → "clears
//     the stored tokens" fails and the poisoned refresh token stays on disk
//     for every peer to replay.
// ---------------------------------------------------------------------------

describe("invalid_grant is terminal", () => {
  it("recognises a structured invalid_grant body", () => {
    assert.equal(isInvalidGrantBody('{"error":"invalid_grant"}'), true)
    assert.equal(
      isInvalidGrantBody('{"error":"invalid_grant","error_description":"expired"}'),
      true,
    )
    assert.equal(isInvalidGrantBody("invalid_grant"), true)
  })

  it("does not mistake another error that merely mentions the phrase", () => {
    assert.equal(
      isInvalidGrantBody('{"error":"invalid_request","error_description":"not invalid_grant"}'),
      false,
      "a structured error field is authoritative",
    )
    assert.equal(isInvalidGrantBody('{"error":"invalid_client"}'), false)
    assert.equal(isInvalidGrantBody('{"error":"server_error"}'), false)
  })

  it("raises a terminal error rather than a plain one", async () => {
    globalThis.fetch = (async () =>
      textResponse('{"error":"invalid_grant"}', 400)) as typeof fetch

    await assert.rejects(
      () => refreshTokens(makeTokens()),
      (err: unknown) => {
        assert.ok(err instanceof NotionAuthTerminalError)
        assert.equal((err as NotionAuthTerminalError).terminal, true)
        assert.match((err as Error).message, /login-notion/)
        return true
      },
    )
  })

  it("clears the stored tokens and does not retry the refresh", async () => {
    writeTokenFile(makeTokens({ accessToken: "doomed", expiresAt: Date.now() - 1 }))
    invalidateCache()

    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      return textResponse('{"error":"invalid_grant"}', 400)
    }) as typeof fetch

    await assert.rejects(
      () => getValidAccessToken(),
      (err: unknown) => err instanceof NotionAuthTerminalError,
    )

    assert.equal(fetchCalls, 1, "invalid_grant must never be retried")
    assert.equal(
      existsSync(tokenFile),
      false,
      "the poisoned refresh token must be destroyed so no peer replays it",
    )
    assert.equal(loadTokens(), null)
    assert.equal(existsSync(getLockDirPath()), false, "the lock must still be released")
  })

  it("keeps the tokens on a transient (non-invalid_grant) refresh failure", async () => {
    writeTokenFile(makeTokens({ accessToken: "stale", expiresAt: Date.now() - 1 }))
    invalidateCache()

    globalThis.fetch = (async () => textResponse("upstream exploded", 503)) as typeof fetch

    await assert.rejects(() => getValidAccessToken(), /Token refresh failed: 503/)
    assert.equal(
      existsSync(tokenFile),
      true,
      "a 503 says nothing about the grant — the tokens must survive",
    )
  })

  it("treats a missing token store as terminal too", async () => {
    await assert.rejects(
      () => getValidAccessToken(),
      (err: unknown) => {
        assert.ok(err instanceof NotionAuthTerminalError)
        assert.match((err as Error).message, /login-notion/)
        return true
      },
    )
  })
})

// ---------------------------------------------------------------------------
// 4. Client registration reuse  (AC: security / persisted, reused client_id)
//
// Revert-and-watch-fail: delete the `if (existing && existing.redirectUri ===
// redirectUri) return existing` branch in ensureClientRegistration() →
// "registers once and reuses the persisted client_id" sees 2 registrations
// and two different client ids, which is how prior grants get orphaned.
// ---------------------------------------------------------------------------

describe("client registration reuse", () => {
  it("registers once and reuses the persisted client_id", async () => {
    let registrations = 0
    globalThis.fetch = (async () => {
      registrations++
      return jsonResponse({ client_id: `cid-${registrations}` }, 201)
    }) as typeof fetch

    const first = await ensureClientRegistration()
    const second = await ensureClientRegistration()

    assert.equal(registrations, 1, "re-registering orphans the prior grant")
    assert.equal(first.clientId, "cid-1")
    assert.equal(second.clientId, "cid-1")
  })

  it("persists the registration at mode 0600", async () => {
    globalThis.fetch = (async () => jsonResponse({ client_id: "cid-x" }, 201)) as typeof fetch

    await ensureClientRegistration()

    assert.ok(existsSync(clientFile))
    assert.equal(mode(clientFile), 0o600)
    assert.equal(loadClientRegistration()?.clientId, "cid-x")
  })

  it("survives a token clear, so re-login reuses the same registration", async () => {
    let registrations = 0
    globalThis.fetch = (async () => {
      registrations++
      return jsonResponse({ client_id: `cid-${registrations}` }, 201)
    }) as typeof fetch

    await ensureClientRegistration()
    saveTokens(makeTokens())
    clearTokens()
    const afterClear = await ensureClientRegistration()

    assert.equal(registrations, 1)
    assert.equal(afterClear.clientId, "cid-1")
  })

  it("re-registers when the persisted redirect URI no longer matches", async () => {
    writeJsonAtomic(clientFile, {
      clientId: "old-cid",
      redirectUri: "http://localhost:9999/oauth/callback",
      registeredAt: 1,
    })

    let registrations = 0
    globalThis.fetch = (async () => {
      registrations++
      return jsonResponse({ client_id: "new-cid" }, 201)
    }) as typeof fetch

    const reg = await ensureClientRegistration()

    assert.equal(registrations, 1)
    assert.equal(reg.clientId, "new-cid")
    assert.match(reg.redirectUri, /:3738\/oauth\/callback$/)
  })

  it("uses a callback port other than 3737 to avoid colliding with /login-atlassian", async () => {
    globalThis.fetch = (async () => jsonResponse({ client_id: "cid" }, 201)) as typeof fetch
    const reg = await ensureClientRegistration()
    assert.doesNotMatch(reg.redirectUri, /:3737\b/)
    assert.match(reg.redirectUri, /^http:\/\/localhost:3738\/oauth\/callback$/)
  })

  it("rejects a registration response with no client_id", async () => {
    globalThis.fetch = (async () => jsonResponse({}, 201)) as typeof fetch
    await assert.rejects(() => ensureClientRegistration(), /no client_id/)
  })
})

// ---------------------------------------------------------------------------
// PKCE + state  (AC: security / S256 only, state validated before exchange)
// ---------------------------------------------------------------------------

describe("PKCE", () => {
  it("derives the challenge as base64url(SHA-256(verifier))", async () => {
    const { verifier, challenge } = await generatePKCE()
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))
    assert.equal(challenge, toBase64Url(new Uint8Array(digest)))
    assert.doesNotMatch(challenge, /[+/=]/, "challenge must be base64url, not base64")
  })

  it("produces a fresh verifier each time", async () => {
    const a = await generatePKCE()
    const b = await generatePKCE()
    assert.notEqual(a.verifier, b.verifier)
  })
})

describe("makeAuthorizeUrl", () => {
  it("only ever requests the S256 challenge method", () => {
    const url = new URL(makeAuthorizeUrl("cid", "http://localhost:3738/oauth/callback", "chal", "st8"))
    assert.equal(url.origin + url.pathname, "https://mcp.notion.com/authorize")
    assert.equal(url.searchParams.get("code_challenge_method"), "S256")
    assert.notEqual(url.searchParams.get("code_challenge_method"), "plain")
    assert.equal(url.searchParams.get("response_type"), "code")
    assert.equal(url.searchParams.get("state"), "st8")
    assert.equal(url.searchParams.get("code_challenge"), "chal")
  })
})

describe("parseAuthInput state validation", () => {
  it("accepts a redirect URL whose state matches", () => {
    const input = "http://localhost:3738/oauth/callback?code=abc123&state=expected"
    assert.deepEqual(parseAuthInput(input, "expected"), { code: "abc123" })
  })

  it("rejects a redirect URL whose state does not match", () => {
    const input = "http://localhost:3738/oauth/callback?code=abc123&state=attacker"
    assert.equal(parseAuthInput(input, "expected"), null)
  })

  it("rejects a redirect URL with no state at all", () => {
    assert.equal(parseAuthInput("http://localhost:3738/oauth/callback?code=abc123", "expected"), null)
  })

  it("accepts the code#state form our callback server emits", () => {
    assert.deepEqual(parseAuthInput("abc123#expected", "expected"), { code: "abc123" })
  })

  it("rejects code#state when the state differs", () => {
    assert.equal(parseAuthInput("abc123#attacker", "expected"), null)
  })

  it("rejects a bare code — every accepted path must carry state", () => {
    // atlassian/auth.ts accepts this shape and thereby skips CSRF validation.
    assert.equal(parseAuthInput("a-long-authorization-code-value", "expected"), null)
  })

  it("rejects empty input", () => {
    assert.equal(parseAuthInput("   ", "expected"), null)
  })
})

// ---------------------------------------------------------------------------
// Debug logging  (AC: security / never logs tokens or Authorization headers)
// ---------------------------------------------------------------------------

describe("debug logging", () => {
  it("never writes access tokens, refresh tokens or bearer headers to stderr", async () => {
    process.env.NOTION_MCP_DEBUG = "1"

    const secretRefresh = "SUPER-SECRET-REFRESH-TOKEN-a1b2c3"
    const secretAccessOld = "SUPER-SECRET-ACCESS-OLD-d4e5f6"
    const secretAccessNew = "SUPER-SECRET-ACCESS-NEW-g7h8i9"
    const secretRefreshNew = "SUPER-SECRET-REFRESH-NEW-j0k1l2"

    writeTokenFile(
      makeTokens({
        accessToken: secretAccessOld,
        refreshToken: secretRefresh,
        expiresAt: Date.now() - 1,
      }),
    )
    invalidateCache()

    globalThis.fetch = (async () =>
      jsonResponse({
        access_token: secretAccessNew,
        refresh_token: secretRefreshNew,
        expires_in: 3600,
      })) as typeof fetch

    const captured: string[] = []
    const realError = console.error
    console.error = (...args: unknown[]) => {
      captured.push(args.map((a) => (typeof a === "string" ? a : JSON.stringify(a))).join(" "))
    }
    try {
      const { token } = await getValidAccessToken()
      assert.equal(token, secretAccessNew)
    } finally {
      console.error = realError
    }

    const all = captured.join("\n")
    assert.ok(captured.length > 0, "the debug gate should have produced some output")
    for (const secret of [secretRefresh, secretAccessOld, secretAccessNew, secretRefreshNew]) {
      assert.ok(!all.includes(secret), `debug output leaked a token: ${all}`)
    }
    assert.ok(!/Bearer\s/i.test(all), `debug output leaked an Authorization header: ${all}`)
  })

  it("stays silent when NOTION_MCP_DEBUG is unset", async () => {
    writeTokenFile(makeTokens({ accessToken: "a", expiresAt: Date.now() - 1 }))
    invalidateCache()
    globalThis.fetch = (async () =>
      jsonResponse({ access_token: "b", refresh_token: "c", expires_in: 3600 })) as typeof fetch

    const captured: string[] = []
    const realError = console.error
    console.error = (...args: unknown[]) => {
      captured.push(args.join(" "))
    }
    try {
      await getValidAccessToken()
    } finally {
      console.error = realError
    }

    assert.deepEqual(captured, [])
  })
})

// ---------------------------------------------------------------------------
// Refresh mechanics
// ---------------------------------------------------------------------------

describe("refreshTokens", () => {
  it("carries the previous refresh token forward when the server omits it", async () => {
    globalThis.fetch = (async () =>
      jsonResponse({ access_token: "new-access", expires_in: 3600 })) as typeof fetch

    const refreshed = await refreshTokens(makeTokens({ refreshToken: "keep-me" }))
    assert.equal(refreshed.accessToken, "new-access")
    assert.equal(refreshed.refreshToken, "keep-me")
  })

  it("stores the rotated refresh token when the server sends one", async () => {
    globalThis.fetch = (async () =>
      jsonResponse({
        access_token: "new-access",
        refresh_token: "rotated",
        expires_in: 3600,
      })) as typeof fetch

    const refreshed = await refreshTokens(makeTokens({ refreshToken: "old" }))
    assert.equal(refreshed.refreshToken, "rotated")
  })

  it("applies a refresh margin well ahead of the real expiry", async () => {
    globalThis.fetch = (async () =>
      jsonResponse({ access_token: "a", refresh_token: "b", expires_in: 28800 })) as typeof fetch

    const before = Date.now()
    const refreshed = await refreshTokens(makeTokens())
    const realExpiry = before + 28800 * 1000

    assert.ok(refreshed.expiresAt < realExpiry, "expiresAt must precede the true expiry")
    assert.ok(
      realExpiry - refreshed.expiresAt >= 5 * 60 * 1000,
      "the margin must be at least the 5 minutes Notion recommends",
    )
  })

  it("sends grant_type=refresh_token with the stored client_id", async () => {
    let seenBody = ""
    globalThis.fetch = (async (_url: unknown, init: RequestInit) => {
      seenBody = String(init.body)
      return jsonResponse({ access_token: "a", refresh_token: "b", expires_in: 3600 })
    }) as unknown as typeof fetch

    await refreshTokens(makeTokens({ clientId: "cid-9", refreshToken: "rt-9" }))

    const params = new URLSearchParams(seenBody)
    assert.equal(params.get("grant_type"), "refresh_token")
    assert.equal(params.get("client_id"), "cid-9")
    assert.equal(params.get("refresh_token"), "rt-9")
  })
})

// ---------------------------------------------------------------------------
// Local callback server (AC: functional / 127.0.0.1, port != 3737, always closed)
// ---------------------------------------------------------------------------

/**
 * Probe whether 127.0.0.1:3738 is free. These tests bind a real port, so on a
 * host that already has something there they skip rather than fail.
 */
async function callbackPortAvailable(): Promise<boolean> {
  try {
    const auth = await createLocalCallbackServer("probe")
    auth.cancel()
    await auth.waitForCallback()
    await sleep(10)
    return true
  } catch {
    return false
  }
}

describe("local callback server", () => {
  it("binds 127.0.0.1 on port 3738 and resolves code#state on success", async (t) => {
    if (!(await callbackPortAvailable())) return t.skip("127.0.0.1:3738 is busy")

    const auth = await createLocalCallbackServer("the-state")
    assert.equal(auth.redirectUri, "http://localhost:3738/oauth/callback")

    // Reachable on the loopback address, and only there.
    const res = await originalFetch(
      "http://127.0.0.1:3738/oauth/callback?code=the-code&state=the-state",
    )
    assert.equal(res.status, 200)

    assert.equal(await auth.waitForCallback(), "the-code#the-state")

    // Closed on the success path — the port is immediately re-bindable.
    await sleep(20)
    assert.equal(await callbackPortAvailable(), true, "server must close after success")
  })

  it("rejects a mismatched state and closes without yielding a code", async (t) => {
    if (!(await callbackPortAvailable())) return t.skip("127.0.0.1:3738 is busy")

    const auth = await createLocalCallbackServer("expected-state")
    const res = await originalFetch(
      "http://127.0.0.1:3738/oauth/callback?code=the-code&state=attacker-state",
    )
    assert.equal(res.status, 400)
    assert.equal(await res.text(), "Invalid state")

    assert.equal(
      await auth.waitForCallback(),
      null,
      "a state mismatch must never surface a code to the exchange",
    )

    await sleep(20)
    assert.equal(await callbackPortAvailable(), true, "server must close after a state mismatch")
  })

  it("closes on cancel — the same completion path the timeout uses", async (t) => {
    if (!(await callbackPortAvailable())) return t.skip("127.0.0.1:3738 is busy")

    const auth = await createLocalCallbackServer("st8")
    auth.cancel()
    assert.equal(await auth.waitForCallback(), null)

    await sleep(20)
    assert.equal(await callbackPortAvailable(), true, "server must close on cancel/timeout")
  })

  it("404s an unrelated path without completing the flow", async (t) => {
    if (!(await callbackPortAvailable())) return t.skip("127.0.0.1:3738 is busy")

    const auth = await createLocalCallbackServer("st8")
    const res = await originalFetch("http://127.0.0.1:3738/not-the-callback")
    assert.equal(res.status, 404)

    auth.cancel()
    await auth.waitForCallback()
    await sleep(20)
  })
})

describe("getValidAccessToken happy paths", () => {
  it("returns the current token without any I/O when it is still valid", async () => {
    writeTokenFile(makeTokens({ accessToken: "still-valid", expiresAt: Date.now() + 3_600_000 }))
    invalidateCache()

    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error("should not refresh")
    }) as typeof fetch

    const { token } = await getValidAccessToken()
    assert.equal(token, "still-valid")
    assert.equal(fetchCalls, 0)
    assert.equal(existsSync(getLockDirPath()), false, "no lock is taken on the fast path")
  })

  it("persists the rotated pair so peers observe it", async () => {
    writeTokenFile(makeTokens({ accessToken: "old", refreshToken: "r-old", expiresAt: Date.now() - 1 }))
    invalidateCache()

    globalThis.fetch = (async () =>
      jsonResponse({
        access_token: "rotated-access",
        refresh_token: "rotated-refresh",
        expires_in: 3600,
      })) as typeof fetch

    await getValidAccessToken()

    invalidateCache()
    const onDisk = loadTokens()
    assert.equal(onDisk?.accessToken, "rotated-access")
    assert.equal(onDisk?.refreshToken, "rotated-refresh")
    assert.equal(mode(tokenFile), 0o600)
  })
})
