// Tests for the shared Anthropic OAuth refresh lock (refresh-lock.ts, #3289).
// Run with: tsx --test refresh-lock.test.ts
//
// The unit tests drive the lock in one process. The cross-process tests
// start two real pi-like processes that need a refresh at the same time,
// hold them at a barrier until both are ready, and count the refresh
// requests. They show that both refresh paths of the extension (the
// refreshToken handler and refreshIfNeeded) take the same lock.
//
// Each test uses its own temp dir for auth.json and for the lock dir, so no
// test reads or writes the real home dir.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { execFileSync, spawn } from "node:child_process"
import {
  existsSync,
  lstatSync,
  lutimesSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

import {
  acquireRefreshLock,
  acquireRefreshLockSync,
  getRefreshLockDir,
  LOCK_TIMINGS,
  REFRESH_LOCK_NAME,
  writeOwnerFile,
} from "./refresh-lock.ts"

const HERE = dirname(fileURLToPath(import.meta.url))

let tempDir: string
let lockDir: string
let lockPath: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-refresh-lock-test-"))
  lockDir = join(tempDir, "pi-locks")
  lockPath = join(lockDir, REFRESH_LOCK_NAME)
})

afterEach(() => {
  rmSync(tempDir, { recursive: true, force: true })
})

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// A lock dir of a dead holder: an owner file and an mtime ageMs ago.
function plantLock(ageMs: number, id = "dead-peer"): void {
  mkdirSync(lockPath, { recursive: true })
  writeFileSync(join(lockPath, "owner"), JSON.stringify({ id, pid: 1, ts: 0 }))
  const then = new Date(Date.now() - ageMs)
  utimesSync(lockPath, then, then)
}

describe("lock timings", () => {
  it("a waiter outlasts the stale time, and a holder touches its lock well within it", () => {
    assert.ok(LOCK_TIMINGS.asyncWaitMs > LOCK_TIMINGS.staleMs)
    assert.ok(LOCK_TIMINGS.heartbeatMs * 3 <= LOCK_TIMINGS.staleMs)
    // refreshViaOAuth runs its request with a 15 s timeout.
    assert.ok(LOCK_TIMINGS.syncWaitMs > 15_000)
    assert.ok(LOCK_TIMINGS.syncWaitMs < LOCK_TIMINGS.staleMs)
  })

  it("the default lock dir is the prism shared lock dir", () => {
    const saved = process.env.PI_ANTHROPIC_REFRESH_LOCK_DIR
    delete process.env.PI_ANTHROPIC_REFRESH_LOCK_DIR
    try {
      assert.match(getRefreshLockDir(), /\/\.local\/state\/prism\/pi-locks$/)
    } finally {
      if (saved !== undefined) process.env.PI_ANTHROPIC_REFRESH_LOCK_DIR = saved
    }
  })
})

describe("acquireRefreshLock", () => {
  it("makes the lock dir with an owner file, and release removes it", async () => {
    const lock = await acquireRefreshLock({ dir: lockDir })
    assert.ok(lock)
    assert.equal(lock.shared, true)
    const owner = JSON.parse(readFileSync(join(lockPath, "owner"), "utf8"))
    assert.equal(owner.pid, process.pid)
    lock.release()
    assert.equal(existsSync(lockPath), false)
  })

  it("a second holder waits until the first releases", async () => {
    const first = await acquireRefreshLock({ dir: lockDir })
    assert.ok(first)
    let secondHeld = false
    const second = acquireRefreshLock({ dir: lockDir, pollMs: 5 }).then((l) => {
      secondHeld = true
      return l
    })
    await sleep(100)
    assert.equal(secondHeld, false, "the second holder took a held lock")
    first.release()
    const lock = await second
    assert.ok(lock)
    assert.equal(lock.shared, true)
    lock.release()
  })

  it("returns null when the wait ends with the lock still held", async () => {
    plantLock(0, "live-peer")
    const started = Date.now()
    const lock = await acquireRefreshLock({ dir: lockDir, waitMs: 150, pollMs: 5 })
    assert.equal(lock, null)
    assert.ok(Date.now() - started < 2_000, "the wait did not end")
    assert.equal(JSON.parse(readFileSync(join(lockPath, "owner"), "utf8")).id, "live-peer")
  })

  it("removes the lock dir of a dead holder after the stale time", async () => {
    plantLock(LOCK_TIMINGS.staleMs + 5_000)
    const lock = await acquireRefreshLock({ dir: lockDir, waitMs: 1_000, pollMs: 5 })
    assert.ok(lock)
    assert.equal(lock.shared, true)
    assert.equal(JSON.parse(readFileSync(join(lockPath, "owner"), "utf8")).pid, process.pid)
    lock.release()
  })

  it("a live holder touches its lock, so a waiter does not judge it stale", async () => {
    const holder = await acquireRefreshLock({ dir: lockDir, heartbeatMs: 20 })
    assert.ok(holder)
    const waiter = await acquireRefreshLock({ dir: lockDir, staleMs: 150, waitMs: 400, pollMs: 5 })
    assert.equal(waiter, null, "a waiter removed the lock of a live holder")
    holder.release()
  })

  it("release does not remove the lock of a new holder", async () => {
    const lock = await acquireRefreshLock({ dir: lockDir })
    assert.ok(lock)
    writeFileSync(join(lockPath, "owner"), JSON.stringify({ id: "new-holder", pid: 1, ts: 0 }))
    lock.release()
    assert.equal(existsSync(lockPath), true)
  })

  it("runs with no lock when the lock dir cannot be made", async () => {
    const notADir = join(tempDir, "file")
    writeFileSync(notADir, "")
    const lock = await acquireRefreshLock({ dir: join(notADir, "pi-locks") })
    assert.ok(lock)
    assert.equal(lock.shared, false)
    lock.release()
  })
})

// A sandbox can write the shared lock dir, and a pi that is not sandboxed
// takes the same lock. These tests plant what a sandbox can plant.
describe("a lock dir that a sandbox changed", () => {
  it("the owner write does not follow a symlink, so the target does not change", () => {
    // The race: a sandbox swaps in a dir with an "owner" symlink after the
    // mkdir of the holder. This is the state that the holder then sees.
    mkdirSync(lockPath, { recursive: true })
    const target = join(tempDir, "host-file")
    writeFileSync(target, "host content")
    symlinkSync(target, join(lockPath, "owner"))

    assert.throws(() => writeOwnerFile(lockPath, "holder"), { code: "EEXIST" })
    assert.equal(readFileSync(target, "utf8"), "host content")
  })

  it("an owner FIFO does not block the async or the sync path", async () => {
    mkdirSync(lockPath, { recursive: true })
    execFileSync("mkfifo", [join(lockPath, "owner")])
    const started = Date.now()
    assert.equal(await acquireRefreshLock({ dir: lockDir, waitMs: 100, pollMs: 5 }), null)
    assert.equal(acquireRefreshLockSync({ dir: lockDir, waitMs: 100, pollMs: 5 }), null)
    assert.ok(Date.now() - started < 5_000, "a read of the FIFO blocked")
  })

  it("a stale lock with an owner FIFO is removed", async () => {
    mkdirSync(lockPath, { recursive: true })
    execFileSync("mkfifo", [join(lockPath, "owner")])
    const then = new Date(Date.now() - LOCK_TIMINGS.staleMs - 5_000)
    utimesSync(lockPath, then, then)
    const lock = await acquireRefreshLock({ dir: lockDir, waitMs: 1_000, pollMs: 5 })
    assert.ok(lock)
    assert.equal(lock.shared, true)
    lock.release()
  })

  it("a stale lock that is a symlink is removed, and its target stays", async () => {
    const target = join(tempDir, "host-dir")
    mkdirSync(target)
    writeFileSync(join(target, "keep"), "x")
    mkdirSync(lockDir, { recursive: true })
    symlinkSync(target, lockPath)
    const then = new Date(Date.now() - LOCK_TIMINGS.staleMs - 5_000)
    lutimesSync(lockPath, then, then)

    const lock = await acquireRefreshLock({ dir: lockDir, waitMs: 1_000, pollMs: 5 })
    assert.ok(lock)
    assert.equal(lstatSync(lockPath).isDirectory(), true, "the lock is not a real dir")
    lock.release()
    assert.equal(readFileSync(join(target, "keep"), "utf8"), "x")
  })
})

describe("acquireRefreshLockSync", () => {
  it("does not wait for a lock that this process holds on the async path", async () => {
    const held = await acquireRefreshLock({ dir: lockDir })
    assert.ok(held)
    const started = Date.now()
    assert.equal(acquireRefreshLockSync({ dir: lockDir }), null)
    assert.ok(Date.now() - started < 1_000, "the sync path blocked on its own process")
    held.release()
  })

  it("takes a free lock and removes the lock dir of a dead holder", () => {
    plantLock(LOCK_TIMINGS.staleMs + 5_000)
    const lock = acquireRefreshLockSync({ dir: lockDir, waitMs: 1_000, pollMs: 5 })
    assert.ok(lock)
    assert.equal(lock.shared, true)
    lock.release()
    assert.equal(existsSync(lockPath), false)
  })

  it("returns null when the wait ends with the lock still held", () => {
    plantLock(0, "live-peer")
    assert.equal(acquireRefreshLockSync({ dir: lockDir, waitMs: 100, pollMs: 5 }), null)
  })
})

// ---------------------------------------------------------------------------
// Cross-process tests
// ---------------------------------------------------------------------------

// The tsx loader flags of this test process, so that a child can import .ts.
function tsxLoaderArgs(): string[] {
  const out: string[] = []
  const argv = process.execArgv
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if ((a === "--import" || a === "--require") && argv[i + 1]?.includes("tsx")) {
      out.push(a, argv[i + 1])
      i++
    } else if ((a.startsWith("--import=") || a.startsWith("--require=")) && a.includes("tsx")) {
      out.push(a)
    }
  }
  return out
}

// The child: a pi session that needs a refresh. The fake token endpoint
// records each request in a shared file and answers after 300 ms, so that
// the two refreshes overlap if nothing serialises them.
const CHILD = `
import { appendFileSync, existsSync, writeFileSync } from "node:fs"
import { join } from "node:path"
const [mode, id, dir] = process.argv.slice(2)
const auth = await import(${JSON.stringify(pathToFileURL(join(HERE, "auth.ts")).href)})
const creds = await import(${JSON.stringify(pathToFileURL(join(HERE, "credentials.ts")).href)})
const requests = join(dir, "requests")
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
globalThis.fetch = async (_url, init) => {
  const rt = new URLSearchParams(String(init.body)).get("refresh_token")
  appendFileSync(requests, id + " " + rt + "\\n")
  await sleep(300)
  return new Response(JSON.stringify({ access_token: "access-" + id, refresh_token: "refresh-" + id, expires_in: 36000 }), { status: 200 })
}
const syncRefresh = (rt) => {
  appendFileSync(requests, id + " " + rt + "\\n")
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 300)
  return { accessToken: "access-" + id, refreshToken: "refresh-" + id, expiresAt: Date.now() + 36000000 }
}
writeFileSync(join(dir, "ready-" + id), "")
while (!(existsSync(join(dir, "ready-a")) && existsSync(join(dir, "ready-b")))) await sleep(5)
let access
if (mode === "async") {
  access = (await auth.refreshAnthropicToken({ access: "old-access", refresh: "old-refresh", expires: Date.now() + 20000 })).access
} else {
  access = creds.refreshIfNeeded({ accessToken: "old-access", refreshToken: "old-refresh", expiresAt: Date.now() + 20000 }, syncRefresh)?.accessToken
}
process.stdout.write(JSON.stringify({ access }))
`

interface ChildResult {
  access: string | undefined
}

function runChild(mode: "async" | "sync", id: "a" | "b", env: NodeJS.ProcessEnv): Promise<ChildResult> {
  return new Promise((resolve, reject) => {
    const script = join(tempDir, "child.mts")
    const child = spawn(process.execPath, [...tsxLoaderArgs(), script, mode, id, tempDir], {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    })
    let out = ""
    let err = ""
    child.stdout.on("data", (d) => (out += d))
    child.stderr.on("data", (d) => (err += d))
    child.on("error", reject)
    child.on("close", (code) => {
      if (code !== 0) return reject(new Error(`child ${id} exited ${code}: ${err}`))
      try {
        resolve(JSON.parse(out) as ChildResult)
      } catch {
        reject(new Error(`child ${id} wrote ${JSON.stringify(out)}; stderr: ${err}`))
      }
    })
  })
}

async function runPair(modeA: "async" | "sync", modeB: "async" | "sync", lockDirForChildren: string) {
  writeFileSync(join(tempDir, "child.mts"), CHILD)
  const authJson = join(tempDir, "auth.json")
  writeFileSync(
    authJson,
    JSON.stringify({
      anthropic: { type: "oauth", access: "old-access", refresh: "old-refresh", expires: Date.now() + 20_000 },
    }),
  )
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    PI_AUTH_JSON: authJson,
    PI_ANTHROPIC_REFRESH_LOCK_DIR: lockDirForChildren,
  }
  delete env.PI_ANTHROPIC_OAUTH_DEBUG
  const [a, b] = await Promise.all([runChild(modeA, "a", env), runChild(modeB, "b", env)])
  const requests = readFileSync(join(tempDir, "requests"), "utf8").trim().split("\n")
  const stored = JSON.parse(readFileSync(authJson, "utf8")).anthropic
  return { a, b, requests, stored }
}

describe("two sessions that need a refresh at the same time", { timeout: 60_000 }, () => {
  for (const [modeA, modeB] of [
    ["async", "async"],
    ["sync", "sync"],
    ["async", "sync"],
  ] as const) {
    it(`send one refresh request (${modeA} path and ${modeB} path), and the second uses the token that the first wrote`, async () => {
      const { a, b, requests, stored } = await runPair(modeA, modeB, lockDir)

      assert.equal(requests.length, 1, `refresh requests: ${JSON.stringify(requests)}`)
      const [winner, sentToken] = requests[0].split(" ")
      assert.equal(sentToken, "old-refresh")
      assert.equal(a.access, `access-${winner}`)
      assert.equal(b.access, `access-${winner}`)
      assert.equal(stored.access, `access-${winner}`)
      assert.equal(stored.refresh, `refresh-${winner}`)
      assert.equal(existsSync(lockPath), false, "the lock dir was not released")
    })
  }

  // The no-op proof: with no shared lock dir (each session its own dir, as
  // pi's own auth.json.lock is in a bwrap sandbox) both sessions send the
  // old refresh token. So the one-request assertions above can fail.
  it("send two refresh requests with the same token when the lock dir is not shared", async () => {
    writeFileSync(join(tempDir, "child.mts"), CHILD)
    const authJson = join(tempDir, "auth.json")
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: { type: "oauth", access: "old-access", refresh: "old-refresh", expires: Date.now() + 20_000 },
      }),
    )
    const base = { ...process.env, PI_AUTH_JSON: authJson }
    const [, , ] = await Promise.all([
      runChild("async", "a", { ...base, PI_ANTHROPIC_REFRESH_LOCK_DIR: join(tempDir, "locks-a") }),
      runChild("async", "b", { ...base, PI_ANTHROPIC_REFRESH_LOCK_DIR: join(tempDir, "locks-b") }),
    ])
    const requests = readFileSync(join(tempDir, "requests"), "utf8").trim().split("\n")
    assert.equal(requests.length, 2, `refresh requests: ${JSON.stringify(requests)}`)
    assert.deepEqual(requests.map((r) => r.split(" ")[1]), ["old-refresh", "old-refresh"])
  })
})

describe("a dead holder", { timeout: 60_000 }, () => {
  it("does not stop the refresh: its lock dir is removed after the stale time", async () => {
    plantLock(LOCK_TIMINGS.staleMs + 5_000)
    writeFileSync(join(tempDir, "ready-b"), "")
    writeFileSync(join(tempDir, "child.mts"), CHILD)
    const authJson = join(tempDir, "auth.json")
    writeFileSync(
      authJson,
      JSON.stringify({
        anthropic: { type: "oauth", access: "old-access", refresh: "old-refresh", expires: Date.now() + 20_000 },
      }),
    )
    const started = Date.now()
    const a = await runChild("async", "a", {
      ...process.env,
      PI_AUTH_JSON: authJson,
      PI_ANTHROPIC_REFRESH_LOCK_DIR: lockDir,
    })
    assert.equal(a.access, "access-a")
    assert.ok(Date.now() - started < LOCK_TIMINGS.asyncWaitMs, "the refresh waited for the full wait time")
    assert.equal(readFileSync(join(tempDir, "requests"), "utf8").trim().split("\n").length, 1)
  })
})
