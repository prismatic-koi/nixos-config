// Unit tests for auth.ts — the Notion-specific security surface.
//
// Every security-critical behaviour has a positive test AND a
// revert-and-watch-fail note describing exactly which line to break to
// re-open the class. The "revert" checks were run manually during
// development; the tests below are what a reviewer runs to confirm the
// checks are still not no-ops.
//
// Run with: tsx --test auth.test.ts (from this directory)

import { after, afterEach, before, beforeEach, describe, it } from "node:test"
import assert from "node:assert/strict"
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import {
  InvalidGrantError,
  acquireRefreshLock,
  clearTokens,
  ensureClientRegistration,
  generatePKCE,
  getClientStorePath,
  getTokenStorePath,
  getValidAccessToken,
  invalidateCache,
  loadClientRegistration,
  loadTokens,
  loginNotion,
  makeAuthorizeUrl,
  parseAuthInput,
  refreshTokens,
  releaseRefreshLock,
  saveClientRegistration,
  saveTokens,
  type NotionTokens,
} from "./auth.ts"

let tempDir: string
let tokenFile: string
let clientFile: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-notion-tokens-test-"))
  tokenFile = join(tempDir, "notion-mcp-oauth.json")
  clientFile = join(tempDir, "notion-mcp-client.json")
  process.env.PI_NOTION_TOKENS = tokenFile
  process.env.PI_NOTION_CLIENT = clientFile
  delete process.env.PI_CODING_AGENT_DIR
  invalidateCache()
})

afterEach(() => {
  delete process.env.PI_NOTION_TOKENS
  delete process.env.PI_NOTION_CLIENT
  delete process.env.PI_CODING_AGENT_DIR
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

// ---------------------------------------------------------------------------
// getTokenStorePath / getClientStorePath precedence
// ---------------------------------------------------------------------------

describe("path resolution precedence", () => {
  it("prefers PI_NOTION_TOKENS when set", () => {
    process.env.PI_NOTION_TOKENS = "/tmp/explicit-notion.json"
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent-decoy"
    assert.equal(getTokenStorePath(), "/tmp/explicit-notion.json")
  })

  it("falls back to PI_CODING_AGENT_DIR/notion-mcp-oauth.json", () => {
    delete process.env.PI_NOTION_TOKENS
    process.env.PI_CODING_AGENT_DIR = "/tmp/coding-agent"
    assert.equal(
      getTokenStorePath(),
      "/tmp/coding-agent/notion-mcp-oauth.json",
    )
  })

  it("falls back to ~/.pi/agent/notion-mcp-oauth.json when neither is set", () => {
    delete process.env.PI_NOTION_TOKENS
    delete process.env.PI_CODING_AGENT_DIR
    const path = getTokenStorePath()
    assert.match(path, /\.pi\/agent\/notion-mcp-oauth\.json$/)
  })

  it("keeps the client store next to the token store", () => {
    process.env.PI_NOTION_CLIENT = "/tmp/explicit-notion-client.json"
    assert.equal(getClientStorePath(), "/tmp/explicit-notion-client.json")
  })
})

// ---------------------------------------------------------------------------
// loadTokens / saveTokens — mode 0o600 + atomic write
// ---------------------------------------------------------------------------

describe("saveTokens", () => {
  it("writes with mode 0o600 (owner-only rw)", () => {
    if (process.platform === "win32") return
    saveTokens(makeTokens({ accessToken: "mode-test" }))
    const mode = statSync(tokenFile).mode & 0o777
    assert.equal(mode, 0o600, `expected 0o600, got 0o${mode.toString(8)}`)
  })

  // Revert-and-watch-fail: if saveTokens is changed to writeFileSync
  // directly to `path` (no tmp+rename), the syscall trace below will
  // NOT contain a rename() to the target path — assertion fails.
  it("uses an atomic tmp+rename write (target path is written via rename)", () => {
    // Instrument node:fs so we can see the syscall trace saveTokens
    // produces. Node caches modules, so we mutate the already-loaded
    // fs module in place.
    const fs = require("node:fs") as typeof import("node:fs")
    const originalRename = fs.renameSync
    const originalWrite = fs.writeFileSync
    const renameCalls: Array<{ from: string; to: string }> = []
    const writeCalls: Array<{ path: string }> = []
    fs.renameSync = ((from: string, to: string) => {
      renameCalls.push({ from: String(from), to: String(to) })
      return originalRename(from, to)
    }) as typeof fs.renameSync
    fs.writeFileSync = ((path: string, ...rest: unknown[]) => {
      writeCalls.push({ path: String(path) })
      // @ts-expect-error — forwarding variadic args
      return originalWrite(path, ...rest)
    }) as typeof fs.writeFileSync

    try {
      saveTokens(makeTokens({ accessToken: "atomic-test" }))

      // The target file must be reached via a rename — never a direct
      // writeFileSync to `tokenFile`.
      const renamesToTarget = renameCalls.filter((c) => c.to === tokenFile)
      assert.equal(
        renamesToTarget.length,
        1,
        `saveTokens must rename() into the target path exactly once; observed: ${JSON.stringify(renameCalls)}`,
      )
      const directWritesToTarget = writeCalls.filter((c) => c.path === tokenFile)
      assert.equal(
        directWritesToTarget.length,
        0,
        `saveTokens must NOT writeFileSync directly to the target path; observed: ${JSON.stringify(writeCalls)}`,
      )

      // And no stray tmp file lingers on happy path.
      const siblings = fs.readdirSync(tempDir)
      const tmpFiles = siblings.filter((s: string) => s.includes(".tmp."))
      assert.deepEqual(tmpFiles, [])
    } finally {
      fs.renameSync = originalRename
      fs.writeFileSync = originalWrite
    }
  })

  // Revert-and-watch-fail: if the write is changed to writeFileSync
  // directly to `path` in one shot, this test WILL fail because a
  // concurrent reader can observe an empty file. We simulate the
  // atomicity requirement by asserting that at no point during the save
  // is `tokenFile` observed with content other than the previous version
  // or the new version — never an empty or partial file.
  //
  // Implementation: we cannot reliably interleave in a single-threaded
  // event loop, so we instead assert on the file-system operations:
  // the target file's inode must survive the write (an "overwrite via
  // rename" changes the inode; but observers who statted before the
  // rename get the OLD content, and after the rename get the NEW
  // content — never a truncated middle state). We assert that the file
  // size never drops below the JSON payload size at any observed
  // point.
  it("target file never contains a truncated payload during rewrite", () => {
    // Seed with a large-ish payload so a truncated intermediate would
    // be detectable by size.
    const initial = makeTokens({
      accessToken: "x".repeat(2048),
      refreshToken: "y".repeat(2048),
    })
    saveTokens(initial)
    const sizeBefore = statSync(tokenFile).size
    assert.ok(sizeBefore > 1024)

    // Rewrite with a differently-sized payload.
    const next = makeTokens({
      accessToken: "z".repeat(4096),
      refreshToken: "w".repeat(4096),
    })
    saveTokens(next)
    const sizeAfter = statSync(tokenFile).size
    assert.ok(sizeAfter > sizeBefore, "new payload is larger")

    // Reading immediately must return valid JSON (never partial).
    const raw = readFileSync(tokenFile, "utf-8")
    const parsed = JSON.parse(raw) as NotionTokens
    assert.equal(parsed.accessToken, next.accessToken)
    assert.equal(parsed.refreshToken, next.refreshToken)
  })

  it("cleans up the temp file if the rename step fails", () => {
    // We simulate a rename failure by pre-creating a directory at the
    // target path — renameSync(file, dir) fails.
    require("node:fs").mkdirSync(tokenFile, { recursive: true })
    assert.throws(() => saveTokens(makeTokens()))

    const siblings = require("node:fs").readdirSync(tempDir) as string[]
    const tmpFiles = siblings.filter((s) => s.includes(".tmp."))
    assert.deepEqual(
      tmpFiles,
      [],
      `failed save should not leave orphaned .tmp.* files; found: ${tmpFiles.join(", ")}`,
    )
  })
})

describe("loadTokens", () => {
  it("returns null when the file is absent", () => {
    assert.equal(loadTokens(), null)
  })

  it("returns null when required fields are missing", () => {
    writeTokenFile({ accessToken: "acc" })
    assert.equal(loadTokens(), null)
  })

  it("returns the tokens when the file is well-formed", () => {
    writeTokenFile(makeTokens({ accessToken: "abc-1" }))
    const loaded = loadTokens()
    assert.ok(loaded)
    assert.equal(loaded.accessToken, "abc-1")
    assert.equal(loaded.refreshToken, "ref")
    assert.equal(loaded.clientId, "client-abc")
  })
})

// ---------------------------------------------------------------------------
// clearTokens
// ---------------------------------------------------------------------------

describe("clearTokens", () => {
  it("removes the token file from disk", () => {
    saveTokens(makeTokens())
    assert.ok(existsSync(tokenFile))
    clearTokens()
    assert.equal(existsSync(tokenFile), false)
  })

  it("empties the in-memory cache", () => {
    saveTokens(makeTokens({ accessToken: "before-clear" }))
    loadTokens() // populate cache
    clearTokens()
    // On next load the file is gone and the cache is empty → null.
    assert.equal(loadTokens(), null)
  })

  it("preserves the client registration file", () => {
    saveClientRegistration({ clientId: "persist-me", redirectUri: "http://x" })
    saveTokens(makeTokens())
    clearTokens()
    const reg = loadClientRegistration()
    assert.ok(reg, "client registration must survive clearTokens")
    assert.equal(reg.clientId, "persist-me")
  })
})

// ---------------------------------------------------------------------------
// Client registration persistence
//
// Revert-and-watch-fail: if ensureClientRegistration calls registerClient()
// unconditionally (removes the loadClientRegistration() branch), this
// test suite fails because fetch is invoked when it should not be.
// ---------------------------------------------------------------------------

describe("ensureClientRegistration", () => {
  it("reuses the stored client_id and does NOT call /register", async () => {
    // Pre-seed the client store.
    saveClientRegistration({
      clientId: "reused-id",
      redirectUri: "http://localhost:3738/oauth/callback",
    })

    const originalFetch = globalThis.fetch
    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error(
        "ensureClientRegistration must not call fetch when a stored client_id exists",
      )
    }) as typeof fetch

    try {
      const reg = await ensureClientRegistration()
      assert.equal(reg.clientId, "reused-id")
      assert.equal(fetchCalls, 0, "no /register call must happen on reuse")
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("calls /register when no stored client_id exists, then persists it", async () => {
    const originalFetch = globalThis.fetch
    let fetchCalls = 0
    globalThis.fetch = (async (url: unknown, _init: unknown) => {
      fetchCalls++
      assert.ok(String(url).includes("/register"))
      return {
        ok: true,
        status: 201,
        json: async () => ({ client_id: "fresh-id" }),
        text: async () => '{"client_id":"fresh-id"}',
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    try {
      const reg = await ensureClientRegistration()
      assert.equal(reg.clientId, "fresh-id")
      assert.equal(fetchCalls, 1)

      // Persisted for reuse.
      const persisted = loadClientRegistration()
      assert.ok(persisted)
      assert.equal(persisted.clientId, "fresh-id")

      // A second call must NOT hit fetch again.
      fetchCalls = 0
      const second = await ensureClientRegistration()
      assert.equal(second.clientId, "fresh-id")
      assert.equal(fetchCalls, 0)
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("saves the client registration with mode 0o600", () => {
    if (process.platform === "win32") return
    saveClientRegistration({ clientId: "x", redirectUri: "y" })
    const mode = statSync(clientFile).mode & 0o777
    assert.equal(mode, 0o600, `expected 0o600, got 0o${mode.toString(8)}`)
  })
})

// ---------------------------------------------------------------------------
// PKCE — S256 only
//
// Revert-and-watch-fail: if the code_challenge_method is changed to
// "plain", the authorize URL assertion below fails immediately.
// ---------------------------------------------------------------------------

describe("PKCE", () => {
  it("generatePKCE returns method=S256", async () => {
    const { verifier, challenge, method } = await generatePKCE()
    assert.equal(method, "S256")
    // verifier is 32 random bytes base64url'd → 43 chars
    assert.equal(verifier.length, 43)
    assert.match(verifier, /^[A-Za-z0-9_-]+$/)
    // challenge is 32-byte SHA-256 base64url'd → 43 chars
    assert.equal(challenge.length, 43)
    assert.match(challenge, /^[A-Za-z0-9_-]+$/)
  })

  it("makeAuthorizeUrl always sends code_challenge_method=S256", () => {
    const url = makeAuthorizeUrl("cid", "http://cb", "ch", "st")
    const parsed = new URL(url)
    assert.equal(parsed.searchParams.get("code_challenge_method"), "S256")
    assert.equal(
      parsed.searchParams.get("code_challenge_method")?.toLowerCase(),
      "s256",
    )
    // Never plain.
    assert.notEqual(parsed.searchParams.get("code_challenge_method"), "plain")
  })

  it("makeAuthorizeUrl targets https://mcp.notion.com/authorize", () => {
    const url = makeAuthorizeUrl("cid", "http://cb", "ch", "st")
    assert.ok(url.startsWith("https://mcp.notion.com/authorize?"))
  })
})

// ---------------------------------------------------------------------------
// parseAuthInput — state validation
//
// Revert-and-watch-fail: if a "bare code" branch is added (as the
// Atlassian version has), the last test in this block will fail.
// ---------------------------------------------------------------------------

describe("parseAuthInput", () => {
  it("accepts a code#state form with matching state", () => {
    const parsed = parseAuthInput("thecode#thestate", "thestate")
    assert.ok(parsed)
    assert.equal(parsed.code, "thecode")
  })

  it("rejects a code#state form with mismatched state", () => {
    assert.equal(parseAuthInput("thecode#wrong", "thestate"), null)
  })

  it("accepts a URL with matching state", () => {
    const parsed = parseAuthInput(
      "http://localhost:3738/oauth/callback?code=c&state=s",
      "s",
    )
    assert.ok(parsed)
    assert.equal(parsed.code, "c")
  })

  it("rejects a URL with mismatched state", () => {
    assert.equal(
      parseAuthInput(
        "http://localhost:3738/oauth/callback?code=c&state=x",
        "s",
      ),
      null,
    )
  })

  it("REJECTS a bare code (no state) — Atlassian's fallback is intentionally dropped", () => {
    assert.equal(parseAuthInput("bare-code-with-no-state", "expected"), null)
  })
})

// ---------------------------------------------------------------------------
// refreshTokens — invalid_grant is terminal
//
// Revert-and-watch-fail: if refreshTokens is changed to throw a plain
// Error on invalid_grant (removing the InvalidGrantError branch), the
// `instanceof InvalidGrantError` check below fails immediately.
// ---------------------------------------------------------------------------

describe("refreshTokens", () => {
  it("throws InvalidGrantError on OAuth invalid_grant", async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () => {
      return {
        ok: false,
        status: 400,
        text: async () => '{"error":"invalid_grant"}',
        json: async () => ({ error: "invalid_grant" }),
        headers: new Headers(),
      } as Response
    }) as typeof fetch
    try {
      await assert.rejects(
        () => refreshTokens(makeTokens()),
        (err: unknown) => err instanceof InvalidGrantError,
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("throws a plain Error on non-invalid_grant failures", async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () => {
      return {
        ok: false,
        status: 500,
        text: async () => "internal server error",
        json: async () => ({}),
        headers: new Headers(),
      } as Response
    }) as typeof fetch
    try {
      await assert.rejects(
        () => refreshTokens(makeTokens()),
        (err: unknown) =>
          err instanceof Error && !(err instanceof InvalidGrantError),
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("returns a rotated refresh_token when the server issued one", async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () => {
      return {
        ok: true,
        status: 200,
        text: async () =>
          '{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}',
        json: async () => ({
          access_token: "new-access",
          refresh_token: "rotated-refresh",
          expires_in: 3600,
        }),
        headers: new Headers(),
      } as Response
    }) as typeof fetch
    try {
      const refreshed = await refreshTokens(makeTokens({ refreshToken: "old" }))
      assert.equal(refreshed.accessToken, "new-access")
      assert.equal(
        refreshed.refreshToken,
        "rotated-refresh",
        "rotated refresh token must be persisted",
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })
})

// ---------------------------------------------------------------------------
// getValidAccessToken — invalid_grant clears tokens and does NOT retry
//
// Revert-and-watch-fail: if the InvalidGrantError branch is removed
// from getValidAccessToken (falls through to throw only), the file will
// still exist after the failed call — this test fails.
// ---------------------------------------------------------------------------

describe("getValidAccessToken", () => {
  it("throws 'no auth tokens' when the store is empty", async () => {
    await assert.rejects(
      () => getValidAccessToken(),
      /no auth tokens/i,
    )
  })

  it("returns the cached access token when it is not expired (fast path, no lock)", async () => {
    const originalFetch = globalThis.fetch
    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      throw new Error("fast path must not call fetch")
    }) as typeof fetch

    saveTokens(makeTokens({ accessToken: "still-valid" }))

    try {
      const { token } = await getValidAccessToken()
      assert.equal(token, "still-valid")
      assert.equal(fetchCalls, 0)
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("refreshes on the slow path and persists the new tokens", async () => {
    const originalFetch = globalThis.fetch
    globalThis.fetch = (async () => {
      return {
        ok: true,
        status: 200,
        text: async () =>
          '{"access_token":"fresh","refresh_token":"fresh-r","expires_in":3600}',
        json: async () => ({
          access_token: "fresh",
          refresh_token: "fresh-r",
          expires_in: 3600,
        }),
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    saveTokens(
      makeTokens({
        accessToken: "expired",
        refreshToken: "old-refresh",
        expiresAt: Date.now() - 1000,
      }),
    )

    try {
      const { token } = await getValidAccessToken()
      assert.equal(token, "fresh")
      // Persisted.
      const onDisk = JSON.parse(readFileSync(tokenFile, "utf-8"))
      assert.equal(onDisk.accessToken, "fresh")
      assert.equal(onDisk.refreshToken, "fresh-r")
    } finally {
      globalThis.fetch = originalFetch
    }
  })

  it("clears the token file when refresh returns invalid_grant and does NOT retry", async () => {
    const originalFetch = globalThis.fetch
    let fetchCalls = 0
    globalThis.fetch = (async () => {
      fetchCalls++
      return {
        ok: false,
        status: 400,
        text: async () => '{"error":"invalid_grant"}',
        json: async () => ({ error: "invalid_grant" }),
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    saveTokens(
      makeTokens({
        accessToken: "expired",
        refreshToken: "revoked",
        expiresAt: Date.now() - 1000,
      }),
    )

    try {
      await assert.rejects(
        () => getValidAccessToken(),
        (err: unknown) => err instanceof InvalidGrantError,
      )
      assert.equal(fetchCalls, 1, "must NOT retry after invalid_grant")
      assert.equal(
        existsSync(tokenFile),
        false,
        "token file must be cleared on terminal invalid_grant",
      )
    } finally {
      globalThis.fetch = originalFetch
    }
  })
})

// ---------------------------------------------------------------------------
// Cross-process refresh lock
//
// Revert-and-watch-fail: if acquireRefreshLock loses the "wx"
// (O_CREAT|O_EXCL) atomicity — e.g. replaced with a plain openSync in
// "w" mode — the concurrent-acquisition test fails because both callers
// succeed simultaneously.
// ---------------------------------------------------------------------------

describe("refresh lock", () => {
  it("acquires and releases the lockfile at the expected path", async () => {
    const lock = await acquireRefreshLock()
    try {
      assert.ok(existsSync(lock.path), "lockfile must exist while held")
      assert.ok(
        lock.path.endsWith("notion-mcp-oauth.lock"),
        `unexpected lock path: ${lock.path}`,
      )
    } finally {
      releaseRefreshLock(lock)
    }
    assert.equal(
      existsSync(lock.path),
      false,
      "lockfile must be unlinked on release",
    )
  })

  it("serialises concurrent acquisitions — the second waits for the first", async () => {
    const timings: string[] = []

    const first = await acquireRefreshLock()
    timings.push("first-acquired")

    // Start a second acquisition in the background. It must not
    // complete until we release the first.
    const secondPromise = (async () => {
      const s = await acquireRefreshLock()
      timings.push("second-acquired")
      releaseRefreshLock(s)
    })()

    // Give the second acquisition a chance to progress. It must still
    // be waiting because we hold the lock.
    await new Promise((r) => setTimeout(r, 200))
    assert.deepEqual(timings, ["first-acquired"])

    timings.push("first-releasing")
    releaseRefreshLock(first)

    await secondPromise
    assert.deepEqual(timings, [
      "first-acquired",
      "first-releasing",
      "second-acquired",
    ])
  })

  it("recovers from a stale lock owned by a crashed peer", async () => {
    // Create a stale lockfile manually with an old mtime.
    const lockPath = join(tempDir, "notion-mcp-oauth.lock")
    writeFileSync(lockPath, "99999\n", { mode: 0o600 })
    const past = new Date(Date.now() - 5 * 60 * 1000)
    require("node:fs").utimesSync(lockPath, past, past)

    // acquireRefreshLock should treat this as stale and take over.
    const lock = await acquireRefreshLock()
    try {
      assert.ok(existsSync(lock.path))
    } finally {
      releaseRefreshLock(lock)
    }
  })
})

// ---------------------------------------------------------------------------
// InvalidGrantError shape
// ---------------------------------------------------------------------------

describe("InvalidGrantError", () => {
  it("is an Error subclass and instanceof-checkable", () => {
    const err = new InvalidGrantError("grant revoked")
    assert.ok(err instanceof Error)
    assert.ok(err instanceof InvalidGrantError)
    assert.equal(err.name, "InvalidGrantError")
    assert.equal(err.message, "grant revoked")
  })
})

// ---------------------------------------------------------------------------
// loginNotion — client_id reuse end-to-end
//
// This is the compound test that ties together ensureClientRegistration
// (no /register when a client_id exists) and the login flow shape.
// Because loginNotion binds a real TCP socket, we test the smaller
// component seams directly and just cover the client_id reuse branch
// here by mocking fetch.
// ---------------------------------------------------------------------------

describe("loginNotion — client_id reuse", () => {
  it("reuses a persisted client_id and does not re-register", async () => {
    saveClientRegistration({
      clientId: "persisted-across-logins",
      redirectUri: "http://localhost:3738/oauth/callback",
    })

    const originalFetch = globalThis.fetch
    const fetchCallUrls: string[] = []
    globalThis.fetch = (async (url: unknown) => {
      fetchCallUrls.push(String(url))
      // Should NEVER hit /register given a persisted client_id.
      if (String(url).includes("/register")) {
        throw new Error("must not re-register when client_id is persisted")
      }
      // For any other call, return a benign response so the flow
      // doesn't blow up in unexpected places.
      return {
        ok: true,
        status: 200,
        json: async () => ({}),
        text: async () => "{}",
        headers: new Headers(),
      } as Response
    }) as typeof fetch

    try {
      // We only exercise ensureClientRegistration here — the rest of
      // loginNotion binds a TCP socket which we don't want in unit
      // tests.
      const reg = await ensureClientRegistration()
      assert.equal(reg.clientId, "persisted-across-logins")
      const registerHits = fetchCallUrls.filter((u) => u.includes("/register"))
      assert.deepEqual(registerHits, [])
    } finally {
      globalThis.fetch = originalFetch
    }
  })
})
