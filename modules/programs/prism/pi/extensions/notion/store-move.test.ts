// Tests for the move of the Notion store out of the pi agent dir (#3290):
// the read of the old path until the move, the refusal to refresh from the
// old path, and migrateLegacyStore.
//
// Run with: tsx --test store-move.test.ts (from this directory)
//
// Each test sets HOME (the new store dir comes from it) and
// PI_CODING_AGENT_DIR (the old store dir) to temp dirs, and unsets
// PI_NOTION_TOKENS and PI_NOTION_CLIENT, so the default paths are in use
// and no test touches the real home dir.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import {
  acquireTokenLock,
  getLockDirPath,
  getStoreDir,
  getTokenStorePath,
  getValidAccessToken,
  invalidateCache,
  LEGACY_STORE_MESSAGE,
  loadClientRegistration,
  loadTokens,
  migrateLegacyStore,
  NotionAuthTerminalError,
  NotionLockTimeoutError,
  tokensFromLegacyStore,
  writeLockOwner,
  type NotionTokens,
} from "./auth.ts"

let tempDir: string
let legacyDir: string
let savedHome: string | undefined
let originalFetch: typeof globalThis.fetch

const tokens: NotionTokens = {
  accessToken: "legacy-access",
  refreshToken: "legacy-refresh",
  expiresAt: Date.now() + 60 * 60 * 1000,
  clientId: "client-1",
}
const client = { clientId: "client-1", redirectUri: "http://localhost:3738/oauth/callback", registeredAt: 1 }

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-notion-move-test-"))
  savedHome = process.env.HOME
  process.env.HOME = join(tempDir, "home")
  legacyDir = join(tempDir, "home", ".pi", "agent")
  mkdirSync(legacyDir, { recursive: true })
  process.env.PI_CODING_AGENT_DIR = legacyDir
  delete process.env.PI_NOTION_TOKENS
  delete process.env.PI_NOTION_CLIENT
  delete process.env.NOTION_MCP_DEBUG
  originalFetch = globalThis.fetch
  invalidateCache()
})

afterEach(() => {
  globalThis.fetch = originalFetch
  if (savedHome === undefined) delete process.env.HOME
  else process.env.HOME = savedHome
  delete process.env.PI_CODING_AGENT_DIR
  rmSync(tempDir, { recursive: true, force: true })
  invalidateCache()
})

function writeLegacy(t: NotionTokens = tokens): void {
  writeFileSync(join(legacyDir, "notion-mcp-oauth.json"), JSON.stringify(t), { mode: 0o600 })
  writeFileSync(join(legacyDir, "notion-mcp-client.json"), JSON.stringify(client), { mode: 0o600 })
}

describe("before the move", () => {
  it("the store dir comes from HOME", () => {
    assert.equal(getStoreDir(), join(tempDir, "home", ".local", "state", "prism", "pi-notion"))
    assert.equal(getTokenStorePath(), join(getStoreDir(), "notion-mcp-oauth.json"))
  })

  it("reads the tokens and the client registration at the old path", () => {
    writeLegacy()
    assert.deepEqual(loadTokens(), tokens)
    assert.equal(tokensFromLegacyStore(), true)
    assert.deepEqual(loadClientRegistration(), client)
  })

  it("does not refresh tokens from the old path, and sends no request", async () => {
    writeLegacy({ ...tokens, expiresAt: Date.now() - 1_000 })
    let calls = 0
    globalThis.fetch = (async () => {
      calls++
      throw new Error("no request expected")
    }) as typeof fetch

    await assert.rejects(getValidAccessToken(), (err: unknown) => {
      assert.ok(!(err instanceof NotionAuthTerminalError), "a store that has not moved is not a dead grant")
      assert.equal((err as Error).message, LEGACY_STORE_MESSAGE)
      return true
    })
    assert.equal(calls, 0)
    assert.equal(JSON.parse(readFileSync(join(legacyDir, "notion-mcp-oauth.json"), "utf8")).refreshToken, "legacy-refresh")
    assert.equal(existsSync(getTokenStorePath()), false)
  })

  it("prefers the store dir when both paths have a file", () => {
    writeLegacy()
    mkdirSync(getStoreDir(), { recursive: true })
    writeFileSync(getTokenStorePath(), JSON.stringify({ ...tokens, accessToken: "new-access" }))
    assert.equal(loadTokens()?.accessToken, "new-access")
    assert.equal(tokensFromLegacyStore(), false)
  })

  it("does not read the old path when PI_NOTION_TOKENS sets the store", () => {
    writeLegacy()
    process.env.PI_NOTION_TOKENS = join(tempDir, "explicit.json")
    try {
      assert.equal(loadTokens(), null)
      assert.equal(loadClientRegistration(), null)
    } finally {
      delete process.env.PI_NOTION_TOKENS
    }
  })
})

describe("migrateLegacyStore", () => {
  it("moves both files to the store dir with mode 0600 and removes them from the agent dir", async () => {
    writeLegacy()
    await migrateLegacyStore()

    for (const [name, want] of [
      ["notion-mcp-oauth.json", tokens],
      ["notion-mcp-client.json", client],
    ] as const) {
      const path = join(getStoreDir(), name)
      assert.deepEqual(JSON.parse(readFileSync(path, "utf8")), want)
      assert.equal(statSync(path).mode & 0o777, 0o600)
    }
    assert.deepEqual(readdirSync(legacyDir), [], "the agent dir keeps a store file or a lock dir")
    assert.deepEqual(readdirSync(getStoreDir()).sort(), ["notion-mcp-client.json", "notion-mcp-oauth.json"])
    assert.deepEqual(loadTokens(), tokens)
    assert.equal(tokensFromLegacyStore(), false)
  })

  it("removes the old file and keeps the store file when both exist", async () => {
    writeLegacy()
    mkdirSync(getStoreDir(), { recursive: true })
    const newer = { ...tokens, refreshToken: "newer-refresh" }
    writeFileSync(getTokenStorePath(), JSON.stringify(newer))
    await migrateLegacyStore()

    assert.deepEqual(JSON.parse(readFileSync(getTokenStorePath(), "utf8")), newer)
    assert.equal(existsSync(join(legacyDir, "notion-mcp-oauth.json")), false)
    assert.deepEqual(loadClientRegistration(), client)
  })

  it("does nothing when the agent dir has no store file", async () => {
    await migrateLegacyStore()
    assert.deepEqual(readdirSync(legacyDir), [])
    assert.equal(existsSync(getStoreDir()), false)
  })

  it("waits for the old lock, and moves nothing while a peer holds it", { timeout: 10_000 }, async () => {
    writeLegacy()
    const oldLock = join(legacyDir, "notion-mcp-oauth.json.lock")
    mkdirSync(oldLock)
    writeFileSync(join(oldLock, "owner"), JSON.stringify({ id: "live-peer", pid: 1, ts: Date.now() }))

    await assert.rejects(migrateLegacyStore(), NotionLockTimeoutError)
    assert.equal(existsSync(getTokenStorePath()), false)
    assert.equal(existsSync(join(legacyDir, "notion-mcp-oauth.json")), true)
    assert.equal(JSON.parse(readFileSync(join(oldLock, "owner"), "utf8")).id, "live-peer")
  })

  it("moves nothing when it cannot write the agent dir, as in a sandbox", async (t) => {
    if (process.getuid?.() === 0) return t.skip("root ignores the dir mode")
    writeLegacy()
    const { chmodSync } = await import("node:fs")
    chmodSync(legacyDir, 0o500)
    try {
      await assert.rejects(migrateLegacyStore())
    } finally {
      chmodSync(legacyDir, 0o700)
    }
    assert.equal(existsSync(getTokenStorePath()), false)
    assert.deepEqual(readdirSync(legacyDir).sort(), ["notion-mcp-client.json", "notion-mcp-oauth.json"])
  })

  it("does nothing when PI_NOTION_TOKENS sets the store", async () => {
    writeLegacy()
    process.env.PI_NOTION_TOKENS = join(tempDir, "explicit.json")
    try {
      await migrateLegacyStore()
    } finally {
      delete process.env.PI_NOTION_TOKENS
    }
    assert.equal(existsSync(join(legacyDir, "notion-mcp-oauth.json")), true)
    assert.equal(existsSync(getStoreDir()), false)
  })
})

// Every sandbox can write the store dir, and a pi that is not sandboxed
// reads and writes it too. These tests plant what a sandbox can plant.
describe("a store dir that a sandbox changed", () => {
  it("the lock owner write does not follow a symlink, so the target does not change", () => {
    const lock = getLockDirPath()
    mkdirSync(lock, { recursive: true })
    const target = join(tempDir, "host-file")
    writeFileSync(target, "host content")
    symlinkSync(target, join(lock, "owner"))

    assert.throws(() => writeLockOwner(lock, "holder"), { code: "EEXIST" })
    assert.equal(readFileSync(target, "utf8"), "host content")
  })

  it("a lock owner FIFO does not block the lock wait", { timeout: 10_000 }, async () => {
    const lock = getLockDirPath()
    mkdirSync(lock, { recursive: true })
    execFileSync("mkfifo", [join(lock, "owner")])
    await assert.rejects(acquireTokenLock(lock, 200), NotionLockTimeoutError)
  })

  it("a token file FIFO does not block a token read", { timeout: 10_000 }, () => {
    mkdirSync(getStoreDir(), { recursive: true })
    execFileSync("mkfifo", [getTokenStorePath()])
    assert.equal(loadTokens(), null)
    execFileSync("mkfifo", [join(getStoreDir(), "notion-mcp-client.json")])
    assert.equal(loadClientRegistration(), null)
  })
})
