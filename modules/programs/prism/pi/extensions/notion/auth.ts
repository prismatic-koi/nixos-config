// Auth module for the Notion MCP pi extension.
//
// Auth method: OAuth 2.0 Authorization Code + PKCE (S256) against Notion's
// hosted remote MCP server at https://mcp.notion.com, with RFC 7591 dynamic
// client registration.
//
// This file is deliberately NOT a copy of atlassian/auth.ts. Notion's
// refresh-token semantics are materially stricter and the failure mode is
// catastrophic rather than transient. See UPSTREAM.md §"Refresh-token
// rotation hazard" for the verbatim upstream warning; the short version:
//
//   * Every refresh ROTATES the refresh token. At most two are valid at
//     once (current + immediately previous).
//   * Replaying a refresh token that was rotated away is treated as a
//     stolen-token signal and Notion REVOKES THE ENTIRE GRANT.
//   * `invalid_grant` is terminal — it must never be retried.
//   * Dynamic client registrations must be persisted and reused, because
//     re-registering orphans prior grants.
//
// Prism routinely runs many concurrent pi sessions (a worker, five review
// agents and a coordinator is an ordinary afternoon) against ONE shared
// token file. For Atlassian a lost refresh race costs a transient 401 that
// retry.ts absorbs. For Notion it costs the whole grant and a human
// re-authorisation. Four mechanisms below exist solely to make that
// impossible:
//
//   1. `acquireTokenLock()` — a cross-process mkdir(2) lock held across the
//      ENTIRE read-refresh-write window, with stale-lock breaking. Every
//      token-endpoint request inside that window is bounded by
//      `TOKEN_REQUEST_TIMEOUT_MS`, which MUST stay well below the stale
//      threshold so a live holder can never be judged stale and preempted.
//   2. A mandatory re-read of the token file AFTER the lock is acquired and
//      BEFORE deciding whether to refresh. The peer that held the lock has
//      almost always already rotated for us.
//   3. `writeJsonAtomic()` — temp file created 0600 (before any content is
//      written), fsynced, then rename(2)d into place. A concurrent reader
//      never observes a partial or truncated token file, and the dentry
//      swap is exactly what the dir-bind reasoning below is designed around.
//   4. `NotionAuthTerminalError` — `invalid_grant` clears the store and is
//      surfaced as terminal. retry.ts refuses to retry it.
//
// Token storage: $PI_CODING_AGENT_DIR/notion-mcp-oauth.json, falling back to
// ~/.pi/agent/notion-mcp-oauth.json. Mode 0600.
//
// See UPSTREAM.md for endpoint provenance and the auth-method rationale.

import { createServer } from "node:http"
import {
  closeSync,
  existsSync,
  fchmodSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs"
import { homedir } from "node:os"
import { dirname, join } from "node:path"

// ---------------------------------------------------------------------------
// Endpoints (RFC 8414 metadata, verified live 2026-07-27 — see UPSTREAM.md)
// ---------------------------------------------------------------------------

const AUTHORIZATION_ENDPOINT = "https://mcp.notion.com/authorize"
const TOKEN_ENDPOINT = "https://mcp.notion.com/token"
const REGISTRATION_ENDPOINT = "https://mcp.notion.com/register"

// Port 3738, deliberately NOT 3737: the Atlassian extension hardcodes 3737 and
// a concurrent /login-atlassian + /login-notion would otherwise collide on
// listen(). Bound to 127.0.0.1 (never 0.0.0.0) because bwrap shares the host
// network namespace — a 0.0.0.0 listener inside a sandbox is a host-wide
// listener.
const CALLBACK_PORT = 3738
const CALLBACK_HOST = "127.0.0.1"
const CALLBACK_PATH = "/oauth/callback"
const LOCAL_CALLBACK_TIMEOUT = 5 * 60 * 1000

const TOKEN_STORE_FILENAME = "notion-mcp-oauth.json"
const CLIENT_STORE_FILENAME = "notion-mcp-client.json"

// Notion's guidance is to refresh 5-10 minutes before expiry rather than at
// the last second. Access tokens last ~8h, so a 5-minute margin is cheap.
// (The Atlassian extension uses 60s, which makes simultaneous same-second
// refresh across sessions likely — exactly the race we cannot afford here.)
const REFRESH_MARGIN_MS = 5 * 60 * 1000

// Lock timings. Two invariants, both asserted in auth.test.ts:
//
//   A. acquire timeout > stale timeout — a crashed peer's lock is always
//      broken before we give up waiting for it.
//   B. token request timeout << stale timeout — a LIVE holder always
//      finishes (or aborts) its network call well before a peer could
//      judge its lock stale and break it.
//
// Invariant B is not cosmetic. `breakStaleLock` only checks that the owner
// id has not changed, which is exactly what a live-but-hung holder looks
// like. Without a bound on the request, undici's default headers timeout
// (300 s) lets a wedged POST — sleep/wake mid-request, VPN flap, captive
// portal — hold the lock for minutes. A peer would break the lock, re-read
// the still-unchanged token file, and refresh with the SAME refresh token:
// precisely the unserialised double-refresh that costs the whole grant.
const DEFAULT_LOCK_STALE_MS = 30_000
const DEFAULT_LOCK_ACQUIRE_TIMEOUT_MS = 45_000
const DEFAULT_TOKEN_REQUEST_TIMEOUT_MS = 10_000
const LOCK_POLL_MS = 25

/** Exported for the invariant tests in auth.test.ts. */
export const LOCK_TIMINGS = {
  staleMs: DEFAULT_LOCK_STALE_MS,
  acquireTimeoutMs: DEFAULT_LOCK_ACQUIRE_TIMEOUT_MS,
  tokenRequestTimeoutMs: DEFAULT_TOKEN_REQUEST_TIMEOUT_MS,
} as const

export const NO_TOKENS_MESSAGE =
  "Notion MCP: no auth tokens — run /login-notion to authenticate."

export const LOST_LOCK_MESSAGE =
  "Notion MCP: the token lock was broken while a refresh was in flight, so the " +
  "refresh token on disk has been rotated away and cannot be reused safely. " +
  "Stored tokens have been cleared — run /login-notion to re-authenticate."

export const INVALID_GRANT_MESSAGE =
  "Notion MCP: the stored refresh token was rejected (invalid_grant). The " +
  "authorization grant has expired or been revoked and cannot be recovered " +
  "automatically. Stored tokens have been cleared — run /login-notion to " +
  "re-authenticate."

export interface NotionTokens {
  accessToken: string
  refreshToken: string
  /** Epoch ms at which the access token should be treated as expired
   *  (already includes REFRESH_MARGIN_MS). */
  expiresAt: number
  clientId: string
}

export interface NotionClientRegistration {
  clientId: string
  redirectUri: string
  registeredAt: number
}

/**
 * Terminal auth failure. The defining property is that RETRYING MAKES THINGS
 * WORSE (or at best cannot help): the grant is gone, or there is nothing to
 * refresh with. retry.ts detects this structurally via the `terminal` flag
 * (see `isTerminalAuthError`) so it never burns a second attempt — and, for
 * `invalid_grant` specifically, never replays a refresh token that Notion has
 * already told us is poison.
 */
export class NotionAuthTerminalError extends Error {
  readonly terminal = true as const
  constructor(message: string) {
    super(message)
    this.name = "NotionAuthTerminalError"
  }
}

/** Raised when the cross-process token lock could not be acquired in time. */
export class NotionLockTimeoutError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "NotionLockTimeoutError"
  }
}

// ---------------------------------------------------------------------------
// Debug logging
//
// SECURITY: nothing in this module ever passes an access token, a refresh
// token, an Authorization header, or a successful token-endpoint response
// body to `debug()`. Only counts, paths, endpoint names and error classes.
// notion/auth.test.ts asserts this with a stderr capture around a full
// refresh cycle.
// ---------------------------------------------------------------------------

function debug(msg: string, ...args: unknown[]): void {
  if (process.env.NOTION_MCP_DEBUG === "1") {
    console.error("[notion-mcp]", msg, ...args)
  }
}

// ---------------------------------------------------------------------------
// Store paths
// ---------------------------------------------------------------------------

/**
 * Resolve the token store path in precedence order:
 *
 *   1. PI_NOTION_TOKENS            explicit override (tests, manual escape hatch)
 *   2. PI_CODING_AGENT_DIR/notion-mcp-oauth.json
 *                                  set by the prism bwrap + sandbox-exec
 *                                  dispatchers, and on the host system-wide.
 *   3. ~/.pi/agent/notion-mcp-oauth.json   legacy fallback.
 *
 * The middle entry is load-bearing. The bwrap dispatcher DIR-binds the host's
 * ~/.pi/agent at $PI_CODING_AGENT_DIR. Reading through the dir-bind observes
 * dentry updates from peer writes (i.e. our rename(2)); a host-path FILE-bind
 * would pin the pre-swap inode and we would never see a peer's rotation.
 * This is the same reasoning as atlassian/auth.ts and
 * anthropic-oauth/credentials.ts, and it is why the optional host-path bind in
 * pi_invocation.go is deliberately NOT mirrored for this file.
 */
export function getTokenStorePath(): string {
  if (process.env.PI_NOTION_TOKENS) return process.env.PI_NOTION_TOKENS
  if (process.env.PI_CODING_AGENT_DIR) {
    return join(process.env.PI_CODING_AGENT_DIR, TOKEN_STORE_FILENAME)
  }
  return join(homedir(), ".pi", "agent", TOKEN_STORE_FILENAME)
}

/**
 * Resolve the dynamic-client-registration store path. Kept in a SEPARATE file
 * from the tokens on purpose: `clearTokens()` (invalid_grant) must destroy the
 * dead grant but must NOT destroy the registration, because re-registering
 * orphans prior grants (Notion's explicit guidance).
 */
export function getClientStorePath(): string {
  if (process.env.PI_NOTION_CLIENT) return process.env.PI_NOTION_CLIENT
  return join(dirname(getTokenStorePath()), CLIENT_STORE_FILENAME)
}

// ---------------------------------------------------------------------------
// Atomic, 0600 JSON persistence
// ---------------------------------------------------------------------------

function ensureStoreDir(path: string): string {
  const dir = dirname(path)
  if (!existsSync(dir)) {
    mkdirSync(dir, { recursive: true, mode: 0o700 })
  }
  return dir
}

/**
 * Write `value` to `path` atomically with mode 0600.
 *
 * The temp file is created with O_CREAT|O_EXCL and mode 0600 and is
 * `fchmod`ed to 0600 through its own fd BEFORE a single byte of content is
 * written — so the secret is never momentarily readable at a wider mode, even
 * under a permissive umask. The subsequent rename(2) is atomic within the
 * directory, which is what guarantees a concurrent reader sees either the
 * whole old file or the whole new file and never a truncated one.
 */
export function writeJsonAtomic(path: string, value: unknown): void {
  const dir = ensureStoreDir(path)
  const unique = `${process.pid}.${Date.now().toString(36)}.${Math.random().toString(36).slice(2, 10)}`
  const tmp = join(dir, `.notion-mcp.tmp.${unique}`)

  let fd: number | undefined
  try {
    // "wx" => O_WRONLY|O_CREAT|O_EXCL. The mode argument applies at create
    // time; the fchmod below defeats a restrictive-but-not-0600 umask.
    fd = openSync(tmp, "wx", 0o600)
    if (process.platform !== "win32") {
      fchmodSync(fd, 0o600)
    }
    // writeFileSync against an fd loops until the whole payload is written
    // (a bare writeSync can short-write on large buffers).
    writeFileSync(fd, JSON.stringify(value, null, 2), { encoding: "utf-8" })
    fsyncSync(fd)
  } catch (err) {
    if (fd !== undefined) {
      try {
        closeSync(fd)
      } catch {
        /* already closed */
      }
      fd = undefined
    }
    try {
      unlinkSync(tmp)
    } catch {
      /* nothing to clean up */
    }
    throw err
  }
  closeSync(fd)

  try {
    renameSync(tmp, path)
  } catch (err) {
    try {
      unlinkSync(tmp)
    } catch {
      /* best effort */
    }
    throw err
  }
}

// ---------------------------------------------------------------------------
// Cross-process token lock
//
// mkdir(2) is atomic on every POSIX filesystem we run on and needs no
// dependency, which is why `proper-lockfile` uses it too. The lock directory
// lives beside the token file in ~/.pi/agent, which both isolators already
// expose read-write (bwrap dir-bind; sandbox-exec `(subpath ~/.pi/agent)`),
// so no sandbox change is required to make this work.
// ---------------------------------------------------------------------------

interface LockOwner {
  id: string
  pid: number
  ts: number
}

export interface LockHandle {
  dir: string
  id: string
}

function lockStaleMs(): number {
  const raw = Number(process.env.PI_NOTION_LOCK_STALE_MS)
  return Number.isFinite(raw) && raw >= 0 ? raw : DEFAULT_LOCK_STALE_MS
}

function lockAcquireTimeoutMs(): number {
  const raw = Number(process.env.PI_NOTION_LOCK_TIMEOUT_MS)
  return Number.isFinite(raw) && raw >= 0 ? raw : DEFAULT_LOCK_ACQUIRE_TIMEOUT_MS
}

function tokenRequestTimeoutMs(): number {
  const raw = Number(process.env.PI_NOTION_TOKEN_TIMEOUT_MS)
  return Number.isFinite(raw) && raw > 0 ? raw : DEFAULT_TOKEN_REQUEST_TIMEOUT_MS
}

export function getLockDirPath(): string {
  return `${getTokenStorePath()}.lock`
}

function readLockOwner(dir: string): LockOwner | null {
  try {
    const parsed = JSON.parse(readFileSync(join(dir, "owner"), "utf8")) as LockOwner
    if (typeof parsed?.id !== "string" || typeof parsed?.ts !== "number") return null
    return parsed
  } catch {
    return null
  }
}

/**
 * Describe a currently-held lock: its owner id (null if the owner file has
 * not been written yet, i.e. the holder crashed between mkdir and write) and
 * its age. Returns null when the lock directory has vanished.
 */
function inspectLock(dir: string): { ownerId: string | null; ageMs: number } | null {
  const owner = readLockOwner(dir)
  if (owner) return { ownerId: owner.id, ageMs: Date.now() - owner.ts }
  try {
    return { ownerId: null, ageMs: Date.now() - statSync(dir).mtimeMs }
  } catch {
    return null
  }
}

/**
 * Remove a lock believed to be stale, but only if it is still owned by the
 * same holder we observed. This closes the window where the holder released
 * and a third process re-took the lock between our age check and our removal.
 */
function breakStaleLock(dir: string, observedOwnerId: string | null): void {
  const current = readLockOwner(dir)
  const currentId = current?.id ?? null
  if (currentId !== observedOwnerId) return
  debug("breaking stale token lock")
  try {
    rmSync(dir, { recursive: true, force: true })
  } catch {
    // Another process broke it first, or we lost a race. Either way the next
    // mkdir attempt is the arbiter.
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function randomLockId(): string {
  return `${process.pid}-${Math.random().toString(36).slice(2, 12)}`
}

/**
 * Acquire the cross-process token lock, waiting for a peer if necessary.
 * Throws `NotionLockTimeoutError` if the lock could not be taken in time.
 *
 * Callers MUST pass the handle to `releaseTokenLock` in a `finally`, or use
 * `withTokenLock` which does it for them.
 */
export async function acquireTokenLock(): Promise<LockHandle> {
  const dir = getLockDirPath()
  const id = randomLockId()
  const deadline = Date.now() + lockAcquireTimeoutMs()

  for (;;) {
    try {
      ensureStoreDir(dir)
      mkdirSync(dir) // atomic; throws EEXIST when a peer holds the lock
      try {
        writeFileSync(
          join(dir, "owner"),
          JSON.stringify({ id, pid: process.pid, ts: Date.now() } satisfies LockOwner),
          { encoding: "utf-8", mode: 0o600 },
        )
      } catch (ownerErr) {
        // We hold a lock nobody can identify. Drop it now rather than leaving
        // peers to wait out the stale timeout.
        try {
          rmSync(dir, { recursive: true, force: true })
        } catch {
          /* the stale-breaker will reclaim it */
        }
        throw ownerErr
      }
      return { dir, id }
    } catch (err) {
      if ((err as NodeJS.ErrnoException)?.code !== "EEXIST") throw err

      const held = inspectLock(dir)
      if (held && held.ageMs > lockStaleMs()) {
        breakStaleLock(dir, held.ownerId)
      }

      if (Date.now() >= deadline) {
        throw new NotionLockTimeoutError(
          `Notion MCP: timed out acquiring the token lock at ${dir}. ` +
            "Another process appears to be wedged mid-refresh; remove the " +
            "directory manually if it persists.",
        )
      }

      // Jittered backoff so a thundering herd of peers does not resynchronise.
      await sleep(LOCK_POLL_MS + Math.floor(Math.random() * LOCK_POLL_MS))
    }
  }
}

/**
 * Release a lock previously taken by `acquireTokenLock`. A no-op if the lock
 * directory is now owned by somebody else (i.e. ours was broken as stale
 * while we still held the handle) — deleting a peer's live lock would be
 * strictly worse than leaking ours, which the stale-breaker will clean up.
 */
export function releaseTokenLock(handle: LockHandle): void {
  const current = readLockOwner(handle.dir)
  if (current && current.id !== handle.id) return
  try {
    rmSync(handle.dir, { recursive: true, force: true })
  } catch {
    // Best effort — a leaked lock is reclaimed by the stale-breaker.
  }
}

/**
 * True when `handle` still owns its lock directory.
 *
 * Checked again after any network call made under the lock: if a peer judged
 * us stale and broke the lock mid-flight, we are no longer entitled to write.
 */
export function ownsLock(handle: LockHandle): boolean {
  return readLockOwner(handle.dir)?.id === handle.id
}

/** Run `fn` while holding the cross-process token lock. */
export async function withTokenLock<T>(fn: () => Promise<T>): Promise<T> {
  const handle = await acquireTokenLock()
  try {
    return await fn()
  } finally {
    releaseTokenLock(handle)
  }
}

// ---------------------------------------------------------------------------
// Token persistence with mtime-aware caching
// ---------------------------------------------------------------------------

interface CacheEntry {
  tokens: NotionTokens
  mtimeMs: number
}

let _cache: CacheEntry | null = null

function readTokensFromDisk(): CacheEntry | null {
  const path = getTokenStorePath()
  try {
    const stats = statSync(path)
    const data = JSON.parse(readFileSync(path, "utf8")) as NotionTokens
    if (!data.accessToken || !data.refreshToken || !data.clientId) return null
    return { tokens: data, mtimeMs: stats.mtimeMs }
  } catch {
    return null
  }
}

/**
 * Return the tokens, using the in-memory cache when the on-disk file has not
 * been touched since the cache was populated. If a peer process has rewritten
 * the file, the cache is refreshed from disk first.
 *
 * NOTE: this is an optimisation, not a correctness mechanism. Every decision
 * to actually refresh is taken from a post-lock, cache-bypassing re-read (see
 * `getValidAccessToken`).
 */
export function loadTokens(): NotionTokens | null {
  if (_cache) {
    const path = getTokenStorePath()
    let mtimeMs = 0
    try {
      mtimeMs = statSync(path).mtimeMs
    } catch {
      // File missing / unreadable — last-good-copy semantics, mirroring
      // atlassian/auth.ts and anthropic-oauth/credentials.ts.
      return _cache.tokens
    }
    if (mtimeMs > _cache.mtimeMs) {
      const fresh = readTokensFromDisk()
      if (fresh) {
        _cache = fresh
        return fresh.tokens
      }
      return _cache.tokens
    }
    return _cache.tokens
  }

  const fresh = readTokensFromDisk()
  if (!fresh) return null
  _cache = fresh
  return fresh.tokens
}

/**
 * Persist tokens atomically at mode 0600 and update the in-memory cache.
 *
 * Callers that are rotating a refresh token MUST already hold the
 * cross-process lock (see `getValidAccessToken` / `loginNotion`).
 */
export function saveTokens(tokens: NotionTokens): void {
  const path = getTokenStorePath()
  try {
    writeJsonAtomic(path, tokens)
    let mtimeMs = 0
    try {
      mtimeMs = statSync(path).mtimeMs
    } catch {
      // Can't stat immediately after writing — leave at 0 so the next
      // loadTokens() re-reads.
    }
    _cache = { tokens, mtimeMs }
  } catch (err) {
    // Never log the tokens themselves, only the failure.
    console.error(
      "[notion-mcp] Failed to save tokens:",
      err instanceof Error ? err.message : String(err),
    )
    throw err
  }
}

/**
 * Destroy the stored tokens. Called when Notion tells us the grant is dead
 * (`invalid_grant`) so no peer process can replay the poisoned refresh token
 * and compound the revocation.
 *
 * The client registration is deliberately left in place — re-registering
 * orphans prior grants, so the next /login-notion must reuse it.
 */
export function clearTokens(): void {
  _cache = null
  try {
    unlinkSync(getTokenStorePath())
    debug("cleared token store")
  } catch {
    // Already gone.
  }
}

/** Drop the in-memory token cache; the next loadTokens() re-reads from disk. */
export function invalidateCache(): void {
  _cache = null
}

/** True when the access token is at (or within the safety margin of) expiry. */
export function needsRefresh(tokens: NotionTokens): boolean {
  return Date.now() >= tokens.expiresAt
}

// ---------------------------------------------------------------------------
// Dynamic client registration (persisted and reused)
// ---------------------------------------------------------------------------

export function loadClientRegistration(): NotionClientRegistration | null {
  try {
    const data = JSON.parse(
      readFileSync(getClientStorePath(), "utf8"),
    ) as NotionClientRegistration
    if (!data.clientId || !data.redirectUri) return null
    return data
  } catch {
    return null
  }
}

export function saveClientRegistration(reg: NotionClientRegistration): void {
  writeJsonAtomic(getClientStorePath(), reg)
}

/**
 * `fetch` against an OAuth endpoint, bounded by `TOKEN_REQUEST_TIMEOUT_MS`.
 *
 * Invariant B (see the timing constants) depends on this: no request issued
 * under the token lock may outlive the stale threshold, or a peer will break
 * a live lock and double-refresh. An abort is surfaced as an ordinary
 * (non-terminal) Error so the retry shell treats it as transient — a timeout
 * says nothing about the validity of the grant.
 */
async function oauthFetch(url: string, init: RequestInit, what: string): Promise<Response> {
  const timeoutMs = tokenRequestTimeoutMs()
  try {
    return await fetch(url, { ...init, signal: AbortSignal.timeout(timeoutMs) })
  } catch (err) {
    const name = (err as { name?: unknown })?.name
    if (name === "TimeoutError" || name === "AbortError") {
      throw new Error(
        `${what} timed out after ${timeoutMs}ms and was aborted, so the token ` +
          "lock is released well before a peer could judge it stale.",
      )
    }
    throw err
  }
}

/** POST /register. Unauthenticated DCR, `token_endpoint_auth_method: "none"`. */
export async function registerClient(redirectUri: string): Promise<NotionClientRegistration> {
  debug("registering a new OAuth client")
  const response = await oauthFetch(REGISTRATION_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      redirect_uris: [redirectUri],
      token_endpoint_auth_method: "none",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      client_name: "pi-notion-mcp",
      client_uri: "https://github.com/prismatic-koi/nixos-config",
    }),
  }, "Client registration")

  if (!response.ok) {
    const body = await response.text()
    throw new Error(`Client registration failed: ${response.status} ${body}`)
  }

  const data = (await response.json()) as { client_id?: string }
  if (!data.client_id) {
    throw new Error("Client registration failed: response contained no client_id")
  }
  return { clientId: data.client_id, redirectUri, registeredAt: Date.now() }
}

/**
 * Return a client registration, reusing the persisted one when possible.
 *
 * Notion: "Persist dynamic client registration credentials and reuse them,
 * because re-registering orphans prior grants." The Atlassian extension
 * re-registers on every login; doing that against Notion would silently
 * abandon the previous grant on each /login-notion.
 *
 * A stored registration is only reused when its redirect URI still matches
 * the one we are about to use — a mismatch would be rejected by the
 * authorization endpoint anyway.
 */
export async function ensureClientRegistration(): Promise<NotionClientRegistration> {
  const redirectUri = `http://localhost:${CALLBACK_PORT}${CALLBACK_PATH}`
  const existing = loadClientRegistration()
  if (existing && existing.redirectUri === redirectUri) {
    debug("reusing persisted client registration")
    return existing
  }
  if (existing) {
    debug("persisted client registration has a stale redirect URI; re-registering")
  }
  const registered = await registerClient(redirectUri)
  saveClientRegistration(registered)
  return registered
}

// ---------------------------------------------------------------------------
// PKCE helpers
// ---------------------------------------------------------------------------

export async function generatePKCE(): Promise<{ verifier: string; challenge: string }> {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  const verifier = toBase64Url(bytes)
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))
  return { verifier, challenge: toBase64Url(new Uint8Array(digest)) }
}

export function toBase64Url(bytes: Uint8Array): string {
  return Buffer.from(bytes)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "")
}

// ---------------------------------------------------------------------------
// Local callback server
// ---------------------------------------------------------------------------

interface LocalAuth {
  redirectUri: string
  waitForCallback: () => Promise<string | null>
  cancel: () => void
}

/**
 * Bind a one-shot callback listener on 127.0.0.1:3738.
 *
 * Every completion path — success, state mismatch, timeout, and the `error`
 * event — runs through `finish()`/`shutdown()`, so the listener is always
 * closed and the timer always cleared. A leaked listener would keep the port
 * bound for the life of the pi session and break the next /login-notion.
 */
export function createLocalCallbackServer(state: string): Promise<LocalAuth> {
  const server = createServer()

  return new Promise((resolve, reject) => {
    let done = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let complete!: (value: string | null) => void
    const wait = new Promise<string | null>((innerResolve) => {
      complete = innerResolve
    })

    const shutdown = () => {
      if (timer) clearTimeout(timer)
      timer = undefined
      try {
        server.closeAllConnections?.()
        server.close()
      } catch {
        // Never listened, or already closed.
      }
    }

    const finish = (value: string | null) => {
      if (done) return
      done = true
      complete(value)
      shutdown()
    }

    server.on("request", (req, res) => {
      const url = new URL(req.url ?? "/", `http://${req.headers.host ?? "localhost"}`)

      if (url.pathname !== CALLBACK_PATH) {
        res.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" })
        res.end("Not found")
        return
      }

      const code = url.searchParams.get("code")
      const gotState = url.searchParams.get("state")
      if (!code || !gotState) {
        res.writeHead(400, { "Content-Type": "text/plain; charset=utf-8" })
        res.end("Missing code or state")
        return
      }
      // CSRF guard: never hand a code to the exchange unless the state we
      // generated came back byte-for-byte.
      if (gotState !== state) {
        res.writeHead(400, { "Content-Type": "text/plain; charset=utf-8" })
        res.end("Invalid state")
        finish(null)
        return
      }

      res.writeHead(200, {
        "Content-Type": "text/html; charset=utf-8",
        Connection: "close",
      })
      res.end(`<!doctype html>
<html>
  <head><meta charset="utf-8"><title>Notion authorization complete</title></head>
  <body>
    <h1>Authorization complete</h1>
    <p>You can close this window and return to pi.</p>
  </body>
</html>`)
      finish(`${code}#${gotState}`)
    })

    server.once("error", (err) => {
      shutdown()
      reject(err)
    })

    server.listen(CALLBACK_PORT, CALLBACK_HOST, () => {
      timer = setTimeout(() => finish(null), LOCAL_CALLBACK_TIMEOUT)
      resolve({
        redirectUri: `http://localhost:${CALLBACK_PORT}${CALLBACK_PATH}`,
        waitForCallback: () => wait,
        cancel: () => finish(null),
      })
    })
  })
}

// ---------------------------------------------------------------------------
// Authorization URL
// ---------------------------------------------------------------------------

/**
 * Build the authorize URL.
 *
 * `code_challenge_method` is hardcoded to S256. Notion advertises `plain` in
 * its RFC 8414 metadata; it must never be selected.
 */
export function makeAuthorizeUrl(
  clientId: string,
  redirectUri: string,
  challenge: string,
  state: string,
): string {
  const params = new URLSearchParams({
    response_type: "code",
    client_id: clientId,
    redirect_uri: redirectUri,
    code_challenge: challenge,
    code_challenge_method: "S256",
    state,
  })
  return `${AUTHORIZATION_ENDPOINT}?${params.toString()}`
}

// ---------------------------------------------------------------------------
// Token exchange / refresh
// ---------------------------------------------------------------------------

/**
 * True when an OAuth error response body is an `invalid_grant`.
 *
 * A structured `error` field wins outright: a body that names some other
 * error is NOT an invalid_grant even if the phrase appears in its
 * description. The bare-text fallback only applies to non-JSON bodies.
 */
export function isInvalidGrantBody(body: string): boolean {
  try {
    const parsed = JSON.parse(body) as { error?: unknown }
    if (typeof parsed?.error === "string") return parsed.error === "invalid_grant"
  } catch {
    // Not JSON — fall through to the text probe.
  }
  return /\binvalid_grant\b/.test(body)
}

interface TokenResponse {
  access_token: string
  refresh_token?: string
  expires_in: number
}

function toTokens(
  data: TokenResponse,
  clientId: string,
  previousRefreshToken?: string,
): NotionTokens {
  const refreshToken = data.refresh_token || previousRefreshToken
  if (!data.access_token || !refreshToken) {
    throw new Error("Notion MCP: token response was missing access_token or refresh_token")
  }
  return {
    accessToken: data.access_token,
    refreshToken,
    expiresAt: Date.now() + data.expires_in * 1000 - REFRESH_MARGIN_MS,
    clientId,
  }
}

async function exchangeCode(
  code: string,
  clientId: string,
  redirectUri: string,
  verifier: string,
): Promise<NotionTokens> {
  const response = await oauthFetch(TOKEN_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      client_id: clientId,
      code,
      redirect_uri: redirectUri,
      code_verifier: verifier,
    }).toString(),
  }, "Token exchange")

  if (!response.ok) {
    const body = await response.text()
    throw new Error(`Token exchange failed: ${response.status} ${body}`)
  }

  return toTokens((await response.json()) as TokenResponse, clientId)
}

/**
 * Exchange the stored refresh token for a fresh pair.
 *
 * CALLERS MUST HOLD THE CROSS-PROCESS LOCK, and MUST re-check `ownsLock()`
 * before writing the result. Notion rotates the refresh token on every call
 * and treats a replay of a rotated token as theft, revoking the whole grant.
 *
 * The request is bounded by `TOKEN_REQUEST_TIMEOUT_MS` so it cannot outlive
 * the caller's lock (invariant B).
 *
 * An `invalid_grant` response is raised as `NotionAuthTerminalError` so no
 * layer above can retry it.
 */
export async function refreshTokens(tokens: NotionTokens): Promise<NotionTokens> {
  debug("refreshing access token")
  const response = await oauthFetch(TOKEN_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      client_id: tokens.clientId,
      refresh_token: tokens.refreshToken,
    }).toString(),
  }, "Token refresh")

  if (!response.ok) {
    const body = await response.text()
    if (isInvalidGrantBody(body)) {
      throw new NotionAuthTerminalError(INVALID_GRANT_MESSAGE)
    }
    throw new Error(`Token refresh failed: ${response.status} ${body}`)
  }

  return toTokens((await response.json()) as TokenResponse, tokens.clientId, tokens.refreshToken)
}

// ---------------------------------------------------------------------------
// Full login flow
// ---------------------------------------------------------------------------

export interface LoginCallbacks {
  /** Called with the authorization URL and instructions to display. */
  onAuthUrl: (url: string, instructions: string) => void
  /** Optional: called when the browser is on another machine and the user
   *  must paste the redirect URL back in. */
  onManualInput?: () => Promise<string>
}

export async function loginNotion(callbacks: LoginCallbacks): Promise<NotionTokens> {
  const { clientId, redirectUri } = await ensureClientRegistration()

  const { verifier, challenge } = await generatePKCE()
  const state = crypto.randomUUID().replace(/-/g, "")

  let localAuth: LocalAuth | null = null
  let authInput: string | null = null
  let effectiveRedirectUri = redirectUri

  try {
    localAuth = await createLocalCallbackServer(state)
    effectiveRedirectUri = localAuth.redirectUri

    callbacks.onAuthUrl(
      makeAuthorizeUrl(clientId, effectiveRedirectUri, challenge, state),
      "Complete authorization in your browser. If the browser is on another machine, paste the full redirect URL here.",
    )

    if (callbacks.onManualInput) {
      let manualInput: string | undefined
      let manualError: Error | undefined
      const manualPromise = callbacks
        .onManualInput()
        .then((input) => {
          manualInput = input
          localAuth?.cancel()
        })
        .catch((err) => {
          manualError = err instanceof Error ? err : new Error(String(err))
          localAuth?.cancel()
        })

      const callbackResult = await localAuth.waitForCallback()
      if (manualError) throw manualError

      if (callbackResult) {
        authInput = callbackResult
      } else if (manualInput) {
        authInput = manualInput
      }

      if (!authInput) {
        await manualPromise
        if (manualError) throw manualError
        if (manualInput) authInput = manualInput
      }
    } else {
      authInput = await localAuth.waitForCallback()
    }
  } catch (err) {
    // Never interpolate the error object wholesale — keep it to a message.
    debug("local auth error:", err instanceof Error ? err.message : String(err))
    localAuth?.cancel()
  }

  if (!authInput) {
    if (!callbacks.onManualInput) {
      throw new Error(
        "Notion MCP: authorization timed out or failed, and no manual input callback was provided.",
      )
    }
    authInput = await callbacks.onManualInput()
  }

  const parsed = parseAuthInput(authInput, state)
  if (!parsed) {
    throw new Error(
      "Notion MCP: could not parse the authorization response, or its state parameter did not match. Run /login-notion again.",
    )
  }

  const tokens = await exchangeCode(parsed.code, clientId, effectiveRedirectUri, verifier)

  // Serialise the write with any peer that is mid-refresh so we cannot be
  // clobbered by (or clobber) a concurrent rotation.
  await withTokenLock(async () => {
    saveTokens(tokens)
  })
  return tokens
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Parse the authorization response into a code.
 *
 * EVERY accepted branch requires the `state` we generated to be present and
 * byte-equal. There is deliberately no "user pasted just the code" branch:
 * atlassian/auth.ts has one and it silently bypasses state validation, which
 * is precisely the CSRF hole the state parameter exists to close.
 */
export function parseAuthInput(
  input: string,
  expectedState: string,
): { code: string } | null {
  const text = input.trim()
  if (!text || !expectedState) return null

  // Full redirect URL, e.g. http://localhost:3738/oauth/callback?code=..&state=..
  try {
    const url = new URL(text)
    const code = url.searchParams.get("code")
    const state = url.searchParams.get("state")
    if (code && state === expectedState) return { code }
    return null
  } catch {
    // Not a URL — fall through.
  }

  // "code#state", the shape our local callback server resolves with.
  const split = text.split("#")
  if (split.length === 2 && split[0] && split[1] === expectedState) {
    return { code: split[0] }
  }

  return null
}

// ---------------------------------------------------------------------------
// Get a valid bearer token
// ---------------------------------------------------------------------------

/**
 * Return a valid access token, refreshing under the cross-process lock when
 * the current one is at or past its safety margin.
 *
 * The ordering here is the whole point of this module:
 *
 *   1. Cheap cached check — if the token is still good, no lock, no I/O.
 *   2. Acquire the cross-process lock. Nothing below this line may run
 *      concurrently in another pi session.
 *   3. RE-READ the token file, bypassing the cache. In the common contention
 *      case a peer refreshed while we queued, so there is nothing to do and
 *      we never touch the network.
 *   4. Only now refresh, then RE-CHECK that we still own the lock, then
 *      write atomically.
 *
 * If step 2 times out we fall back to a READ-ONLY reload. We would rather
 * fail the call than refresh unserialised — an unserialised refresh is how
 * the whole grant gets revoked.
 *
 * Step 4's re-check is the belt to invariant B's braces: the bounded request
 * timeout means a live holder should never be judged stale, but if it happens
 * anyway (SIGSTOP, machine sleep, a clock jump) we must not write. See
 * `recoverFromLostLock`.
 *
 * Throws:
 *   - `NotionAuthTerminalError` when there are no tokens, when Notion answers
 *     `invalid_grant`, or when the lock was lost mid-refresh (the store is
 *     cleared first in the latter two cases).
 *   - The underlying refresh error for transient failures.
 */
export async function getValidAccessToken(): Promise<{
  token: string
  tokens: NotionTokens
}> {
  const initial = loadTokens()
  if (!initial) throw new NotionAuthTerminalError(NO_TOKENS_MESSAGE)
  if (!needsRefresh(initial)) {
    return { token: initial.accessToken, tokens: initial }
  }

  let handle: LockHandle
  try {
    handle = await acquireTokenLock()
  } catch (lockErr) {
    // Read-only fallback. A peer may have finished refreshing while we waited.
    debug("token lock unavailable; falling back to a read-only reload")
    invalidateCache()
    const fresh = loadTokens()
    if (fresh && !needsRefresh(fresh)) {
      return { token: fresh.accessToken, tokens: fresh }
    }
    throw lockErr
  }

  try {
    // Step 3 — authoritative re-read under the lock.
    invalidateCache()
    const current = loadTokens()
    if (!current) throw new NotionAuthTerminalError(NO_TOKENS_MESSAGE)
    if (!needsRefresh(current)) {
      debug("a peer refreshed while we waited for the lock; reusing its tokens")
      return { token: current.accessToken, tokens: current }
    }

    let refreshed: NotionTokens
    try {
      refreshed = await refreshTokens(current)
    } catch (err) {
      if (err instanceof NotionAuthTerminalError) {
        // The grant is dead. Destroy the poisoned refresh token so no peer
        // replays it and compounds the revocation. Terminal — no retry.
        clearTokens()
      }
      throw err
    }

    // We hold a freshly rotated pair — but are we still entitled to publish it?
    if (!ownsLock(handle)) {
      return recoverFromLostLock(current)
    }

    saveTokens(refreshed)
    return { token: refreshed.accessToken, tokens: refreshed }
  } finally {
    releaseTokenLock(handle)
  }
}

/**
 * Salvage the least-bad outcome after discovering our lock was broken while a
 * refresh was in flight.
 *
 * We must NOT write our own rotated pair: a peer took the lock and may already
 * have published its own rotation, and clobbering that is the documented
 * hazard — it strands a rotated-away refresh token on disk for the next
 * refresh to replay.
 *
 * Two cases:
 *
 *   1. A peer published (the on-disk refresh token differs from the one we
 *      refreshed with). Its write is more recent than ours. Defer to it.
 *   2. Nobody published. The refresh token still on disk is the one we just
 *      rotated AWAY, so it is poison — the next refresh would replay it and
 *      Notion would revoke the grant. Destroy it and force a clean re-auth.
 *      That costs one /login-notion instead of the whole workspace grant.
 */
function recoverFromLostLock(preRefresh: NotionTokens): {
  token: string
  tokens: NotionTokens
} {
  invalidateCache()
  const onDisk = loadTokens()

  if (onDisk && onDisk.refreshToken !== preRefresh.refreshToken) {
    debug("lock broken mid-refresh; deferring to the peer's published rotation")
    if (!needsRefresh(onDisk)) {
      return { token: onDisk.accessToken, tokens: onDisk }
    }
    throw new Error(
      "Notion MCP: the token lock was broken mid-refresh and the peer's " +
        "published tokens are already expired. Retry.",
    )
  }

  clearTokens()
  throw new NotionAuthTerminalError(LOST_LOCK_MESSAGE)
}
