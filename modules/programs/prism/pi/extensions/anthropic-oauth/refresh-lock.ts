// Cross-process lock around the Anthropic OAuth token refresh (#3289).
//
// A LOCAL ADDITION, not a port. It takes its idea from griffinmartin
// src/refresh-lock.ts (one refresher, the others adopt its token), which is
// not ported. See UPSTREAM.md divergence #21 for the differences.
//
// Why it exists. pi takes the proper-lockfile lock auth.json.lock in the pi
// agent dir around its own refresh. That dir is private in a bwrap sandbox
// (#3284), so that lock does not stop two sessions from sending the same
// refresh token at the same time. This lock is in a dir that every pi
// session on the host shares: <home>/.local/state/prism/pi-locks/. prism
// makes that dir, binds it into bwrap, and grants it in the sandbox-exec
// profile (internal/container/pi_shared_state.go). The path comes from
// HOME, because every isolator runs pi with the host HOME.
//
// The lock is a dir that mkdir makes (an atomic operation), with an "owner"
// file that holds the id of the holder. Rules:
//
//   - A lock dir whose mtime is older than LOCK_TIMINGS.staleMs is stale:
//     its holder is dead. A waiter removes it, but only when the owner id is
//     the same that it saw, so it cannot remove the lock of a new holder.
//   - An async holder touches the lock dir each LOCK_TIMINGS.heartbeatMs, so
//     a live holder never becomes stale while it waits on the network.
//   - A waiter gives up after a stated time. No session waits forever.
//   - If the lock dir cannot be used (no such dir, no permission), the
//     caller refreshes with no lock, as before this lock existed. A refresh
//     must not fail because of the lock.
//
// A sandboxed session can write the shared dir, and this code also runs in
// a pi that is not sandboxed. So it never follows a symlink when it writes
// or touches a path in the dir, and it never blocks on a read of one: a
// planted symlink or FIFO must not let a sandbox change a host file or stop
// a host pi.

import { randomBytes } from "node:crypto"
import {
  closeSync,
  constants,
  existsSync,
  fstatSync,
  lstatSync,
  lutimesSync,
  mkdirSync,
  openSync,
  readSync,
  rmSync,
  writeSync,
} from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { log } from "./logger.ts"

export const REFRESH_LOCK_NAME = "anthropic-oauth-refresh.lock"

export const LOCK_TIMINGS = {
  /** A lock dir not touched for this long belongs to a dead holder. */
  staleMs: 30_000,
  /** An async holder touches its lock dir this often. Well below staleMs. */
  heartbeatMs: 5_000,
  /** The longest wait of the async path. More than staleMs, so that a
   *  waiter removes the lock of a dead holder before it gives up. */
  asyncWaitMs: 45_000,
  /** The longest wait of the sync path, which blocks the event loop. More
   *  than the 15 s request timeout of refreshViaOAuth, so that a waiter
   *  outlasts a live sync holder. */
  syncWaitMs: 20_000,
  pollMs: 50,
} as const

export interface RefreshLock {
  /** False when the lock dir cannot be used and the caller runs with no lock. */
  readonly shared: boolean
  release(): void
}

export interface LockOptions {
  dir?: string
  staleMs?: number
  waitMs?: number
  heartbeatMs?: number
  pollMs?: number
}

const NO_LOCK: RefreshLock = { shared: false, release() {} }

// Locks that this process holds now. The sync path must not wait for one of
// them: it blocks the event loop, so the holder could never release.
let heldInProcess = 0

/** The shared lock dir: PI_ANTHROPIC_REFRESH_LOCK_DIR (tests), else the prism one. */
export function getRefreshLockDir(): string {
  return (
    process.env.PI_ANTHROPIC_REFRESH_LOCK_DIR ||
    join(homedir(), ".local", "state", "prism", "pi-locks")
  )
}

const OWNER_MAX_BYTES = 4096

// readOwnerId reads the owner id of the lock dir at path. It opens the
// owner file with O_NOFOLLOW and O_NONBLOCK and reads only a regular file,
// so a symlink or a FIFO at that name gives null and does not block.
function readOwnerId(path: string): string | null {
  let fd: number | undefined
  try {
    fd = openSync(
      join(path, "owner"),
      constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
    )
    if (!fstatSync(fd).isFile()) return null
    const buf = Buffer.alloc(OWNER_MAX_BYTES)
    const n = readSync(fd, buf, 0, OWNER_MAX_BYTES, 0)
    const owner = JSON.parse(buf.subarray(0, n).toString("utf8")) as {
      id?: unknown
    }
    return typeof owner?.id === "string" ? owner.id : null
  } catch {
    return null
  } finally {
    if (fd !== undefined) closeSync(fd)
  }
}

/**
 * Create the owner file of the lock dir at path. O_EXCL and O_NOFOLLOW: if
 * a name already exists there (a symlink that a sandbox put in a dir that
 * it swapped in after the mkdir), the open fails with EEXIST and no
 * existing file changes. Exported for the tests.
 */
export function writeOwnerFile(path: string, id: string): void {
  const fd = openSync(
    join(path, "owner"),
    constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW,
    0o600,
  )
  try {
    writeSync(fd, JSON.stringify({ id, pid: process.pid, ts: Date.now() }))
  } finally {
    closeSync(fd)
  }
}

function errorCode(err: unknown): string {
  return String((err as NodeJS.ErrnoException)?.code ?? err)
}

// "busy": a live holder has the lock. The caller waits and tries again.
type Attempt =
  | { kind: "held"; lock: RefreshLock }
  | { kind: "busy" }
  | { kind: "unavailable"; error: string }

function tryAcquire(
  dir: string,
  staleMs: number,
  heartbeatMs: number | null,
): Attempt {
  const path = join(dir, REFRESH_LOCK_NAME)
  try {
    // Only when it is absent: the sandbox-exec profile denies a create of
    // the dir node itself, which gives EPERM, not EEXIST.
    if (!existsSync(dir)) mkdirSync(dir, { recursive: true, mode: 0o700 })
    mkdirSync(path)
  } catch (err) {
    if (errorCode(err) !== "EEXIST") {
      return { kind: "unavailable", error: errorCode(err) }
    }
    const observedOwner = readOwnerId(path)
    let ageMs: number
    try {
      ageMs = Date.now() - lstatSync(path).mtimeMs
    } catch {
      return { kind: "busy" } // released between mkdir and stat
    }
    if (ageMs > staleMs && readOwnerId(path) === observedOwner) {
      log("refresh_lock_stale_removed", { ageMs })
      try {
        rmSync(path, { recursive: true, force: true })
      } catch {
        // Another waiter removed it first. The next mkdir decides.
      }
    }
    return { kind: "busy" }
  }

  const id = `${process.pid}-${randomBytes(6).toString("hex")}`
  try {
    writeOwnerFile(path, id)
  } catch (err) {
    // An owner file that exists right after our mkdir is not ours: another
    // process changed the dir. Leave it. It goes stale if nobody touches it.
    if (errorCode(err) === "EEXIST") return { kind: "busy" }
    try {
      rmSync(path, { recursive: true, force: true })
    } catch {}
    return { kind: "unavailable", error: errorCode(err) }
  }

  heldInProcess++
  let released = false
  const timer =
    heartbeatMs === null
      ? null
      : setInterval(() => {
          if (readOwnerId(path) !== id) return
          try {
            const now = new Date()
            lutimesSync(path, now, now)
          } catch {}
        }, heartbeatMs)
  timer?.unref()

  return {
    kind: "held",
    lock: {
      shared: true,
      release() {
        if (released) return
        released = true
        heldInProcess--
        if (timer) clearInterval(timer)
        // Do not remove the lock of a new holder, after a waiter judged
        // this one stale.
        if (readOwnerId(path) !== id) return
        try {
          rmSync(path, { recursive: true, force: true })
        } catch {}
      },
    },
  }
}

function sleepSync(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

function jitter(pollMs: number): number {
  return pollMs + Math.floor(Math.random() * pollMs)
}

/**
 * Take the shared refresh lock, and wait for a holder if one exists.
 * Returns null when the wait ends with the lock still held: the caller must
 * read auth.json again and must not refresh. Returns a lock with
 * shared === false when the lock dir cannot be used. The caller must call
 * release() in a finally block.
 */
export async function acquireRefreshLock(
  opts: LockOptions = {},
): Promise<RefreshLock | null> {
  const dir = opts.dir ?? getRefreshLockDir()
  const staleMs = opts.staleMs ?? LOCK_TIMINGS.staleMs
  const heartbeatMs = opts.heartbeatMs ?? LOCK_TIMINGS.heartbeatMs
  const pollMs = opts.pollMs ?? LOCK_TIMINGS.pollMs
  const deadline = Date.now() + (opts.waitMs ?? LOCK_TIMINGS.asyncWaitMs)
  for (;;) {
    const attempt = tryAcquire(dir, staleMs, heartbeatMs)
    if (attempt.kind === "held") return attempt.lock
    if (attempt.kind === "unavailable") {
      log("refresh_lock_unavailable", { error: attempt.error })
      return NO_LOCK
    }
    if (Date.now() >= deadline) {
      log("refresh_lock_timeout", {})
      return null
    }
    await new Promise((resolve) => setTimeout(resolve, jitter(pollMs)))
  }
}

/**
 * The sync form of acquireRefreshLock, for the sync credential path. It
 * blocks the event loop while it waits. If this process holds the lock on
 * the async path, it returns null at once.
 */
export function acquireRefreshLockSync(
  opts: LockOptions = {},
): RefreshLock | null {
  if (heldInProcess > 0) {
    log("refresh_lock_held_in_process", {})
    return null
  }
  const dir = opts.dir ?? getRefreshLockDir()
  const staleMs = opts.staleMs ?? LOCK_TIMINGS.staleMs
  const pollMs = opts.pollMs ?? LOCK_TIMINGS.pollMs
  const deadline = Date.now() + (opts.waitMs ?? LOCK_TIMINGS.syncWaitMs)
  for (;;) {
    const attempt = tryAcquire(dir, staleMs, null)
    if (attempt.kind === "held") return attempt.lock
    if (attempt.kind === "unavailable") {
      log("refresh_lock_unavailable", { error: attempt.error })
      return NO_LOCK
    }
    if (Date.now() >= deadline) {
      log("refresh_lock_timeout", {})
      return null
    }
    sleepSync(jitter(pollMs))
  }
}
