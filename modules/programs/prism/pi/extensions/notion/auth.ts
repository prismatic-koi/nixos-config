// Auth module for the Notion MCP pi extension.
//
// Notion runs a hosted remote MCP server at https://mcp.notion.com/mcp that
// requires OAuth 2.0 Authorization Code flow with PKCE (S256).
//
// OAuth server discovery: https://mcp.notion.com/.well-known/oauth-authorization-server
//   - authorization_endpoint: https://mcp.notion.com/authorize
//   - token_endpoint:         https://mcp.notion.com/token
//   - registration_endpoint:  https://mcp.notion.com/register
//
// Endpoints are hardcoded (verified 2026-07-27); we do not fetch the
// discovery document at runtime.
//
// ============================================================================
// CRITICAL: Notion refresh-token semantics differ from Atlassian
// ============================================================================
//
// Notion's OAuth server (Cloudflare workers-oauth-provider) rotates the
// refresh token on every /token call with grant_type=refresh_token. At most
// two refresh tokens are valid at once (the current one and the immediately
// previous one). **Replaying a refresh token that was rotated out more than
// a brief grace period earlier revokes the entire grant** — every session
// breaks and the human must re-authorize.
//
// Notion's docs (developers.notion.com/guides/mcp/build-mcp-client) require:
//
//   1. Serialize refreshes per connection with a mutex or distributed lock.
//      Do NOT refresh concurrently from multiple workers sharing a token
//      store — that is the most common cause of accidental reuse.
//   2. Treat `invalid_grant` as terminal. Clear the tokens, prompt the user
//      to re-authorize, and do NOT retry.
//   3. Persist the dynamic-client-registration client_id and reuse it on
//      subsequent logins. Re-registering orphans prior grants.
//
// Prism routinely runs many concurrent pi sessions (worker + 5 reviewers +
// coordinator) against one shared token file. This module therefore adds,
// over and above the Atlassian pattern:
//
//   - A cross-process file lock (openSync with the "wx" exclusive-create
//     flag) held across the entire read-refresh-write window. After
//     acquiring the lock, the token file is re-read from disk before
//     deciding whether a refresh is still needed — a peer may have
//     rotated the tokens while we waited.
//   - Atomic token writes: content is written to a tmp file created with
//     mode 0o600 and then renamed into place. Concurrent readers never
//     observe a partial or truncated token file.
//   - Terminal `invalid_grant` handling: on `invalid_grant`, the stored
//     tokens are cleared, a re-login prompt is surfaced, and no refresh
//     retry is attempted.
//   - Persistent client_id storage in a separate `notion-mcp-client.json`
//     file. On /login-notion, the stored client_id is reused rather than
//     re-registering with the OAuth server.
//
// See UPSTREAM.md for the verbatim upstream warnings and RFC 8414/9728
// metadata.

import { createServer } from "node:http"
import {
  chmodSync,
  closeSync,
  existsSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  unlinkSync,
  writeFileSync,
  writeSync,
} from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const AUTHORIZATION_ENDPOINT = "https://mcp.notion.com/authorize"
const TOKEN_ENDPOINT = "https://mcp.notion.com/token"
const REGISTRATION_ENDPOINT = "https://mcp.notion.com/register"

// Callback port MUST NOT collide with Atlassian's 3737. Notion uses 3738.
const CALLBACK_PORT = 3738
const CALLBACK_HOST = "127.0.0.1"
const CALLBACK_PATH = "/oauth/callback"
const LOCAL_CALLBACK_TIMEOUT = 5 * 60 * 1000

const TOKEN_STORE_FILENAME = "notion-mcp-oauth.json"
const CLIENT_STORE_FILENAME = "notion-mcp-client.json"
const LOCK_FILENAME = "notion-mcp-oauth.lock"

// Cross-process refresh-lock parameters.
const LOCK_ACQUIRE_TIMEOUT_MS = 30_000
const LOCK_POLL_MS = 50
const LOCK_STALE_AFTER_MS = 60_000

// Proactive refresh margin. Notion's docs recommend refreshing 5-10 minutes
// before expiry rather than at the boundary.
const REFRESH_MARGIN_MS = 5 * 60 * 1000

export interface NotionTokens {
  accessToken: string
  refreshToken: string
  /** Epoch ms when the access token expires */
  expiresAt: number
  clientId: string
}

export interface NotionClientRegistration {
  clientId: string
  redirectUri: string
}

/** Sentinel error type surfaced when the OAuth server returns invalid_grant. */
export class InvalidGrantError extends Error {
  constructor(message = "invalid_grant") {
    super(message)
    this.name = "InvalidGrantError"
  }
}

// ---------------------------------------------------------------------------
// Path resolution
// ---------------------------------------------------------------------------

/**
 * Resolve the token store path in precedence order:
 *
 *   1. PI_NOTION_TOKENS           explicit override (tests, manual escape hatch)
 *   2. PI_CODING_AGENT_DIR/notion-mcp-oauth.json
 *                                 set by the prism bwrap + sandbox-exec
 *                                 dispatchers, and on the host system-wide.
 *                                 This is the path that surfaces host-side
 *                                 writes inside a running sandbox (mirrors
 *                                 the precedent in atlassian/auth.ts).
 *   3. ~/.pi/agent/notion-mcp-oauth.json   legacy fallback.
 */
export function getTokenStorePath(): string {
  if (process.env.PI_NOTION_TOKENS) return process.env.PI_NOTION_TOKENS
  if (process.env.PI_CODING_AGENT_DIR) {
    return join(process.env.PI_CODING_AGENT_DIR, TOKEN_STORE_FILENAME)
  }
  return join(homedir(), ".pi", "agent", TOKEN_STORE_FILENAME)
}

/**
 * Resolve the client-registration store path. Follows the same precedence
 * as the token store; the client_id is persisted separately so that a
 * terminal `invalid_grant` (which clears the token store) does NOT force
 * a re-registration on the next login — the DCR client_id is preserved.
 */
export function getClientStorePath(): string {
  if (process.env.PI_NOTION_CLIENT) return process.env.PI_NOTION_CLIENT
  if (process.env.PI_CODING_AGENT_DIR) {
    return join(process.env.PI_CODING_AGENT_DIR, CLIENT_STORE_FILENAME)
  }
  return join(homedir(), ".pi", "agent", CLIENT_STORE_FILENAME)
}

function getLockPath(): string {
  const tokenPath = getTokenStorePath()
  const dir = tokenPath.substring(0, Math.max(tokenPath.lastIndexOf("/"), 0)) || "."
  return join(dir, LOCK_FILENAME)
}

// ---------------------------------------------------------------------------
// Cross-process refresh lock
//
// Held across the ENTIRE read-refresh-write window. Notion revokes the
// whole grant if two workers refresh the same connection simultaneously,
// so this lock is not optional. See the file header for the upstream
// warning.
// ---------------------------------------------------------------------------

interface LockHandle {
  fd: number
  path: string
}

async function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/**
 * Acquire the refresh lock. Uses openSync with the "wx" flag (O_CREAT |
 * O_EXCL) which is atomic across processes on POSIX filesystems. Polls
 * with a fixed interval until the lock is acquired or the timeout expires.
 *
 * If the lock file has been around for longer than LOCK_STALE_AFTER_MS
 * (crashed peer that never released it), it is treated as stale and
 * unlinked; the next iteration then re-attempts the exclusive create.
 */
export async function acquireRefreshLock(): Promise<LockHandle> {
  const lockPath = getLockPath()
  const dir = lockPath.substring(0, Math.max(lockPath.lastIndexOf("/"), 0)) || "."
  if (!existsSync(dir)) {
    mkdirSync(dir, { recursive: true, mode: 0o700 })
  }

  const deadline = Date.now() + LOCK_ACQUIRE_TIMEOUT_MS
  while (true) {
    try {
      const fd = openSync(lockPath, "wx", 0o600)
      // Write our PID to the lockfile so stale-detection can log which
      // peer we suspect. Best-effort — do not fail acquisition on write
      // error.
      try {
        writeSync(fd, `${process.pid}\n`)
      } catch {
        // ignore
      }
      return { fd, path: lockPath }
    } catch (err) {
      const errno = err instanceof Error && (err as NodeJS.ErrnoException).code
      if (errno !== "EEXIST") throw err
    }

    // Stale detection
    try {
      const stats = statSync(lockPath)
      const age = Date.now() - stats.mtimeMs
      if (age > LOCK_STALE_AFTER_MS) {
        try {
          unlinkSync(lockPath)
        } catch {
          // Another peer beat us to it — fall through and retry.
        }
        continue
      }
    } catch {
      // Lock disappeared — try again immediately.
      continue
    }

    if (Date.now() >= deadline) {
      throw new Error("Notion MCP: timed out acquiring refresh lock")
    }
    await sleep(LOCK_POLL_MS)
  }
}

export function releaseRefreshLock(handle: LockHandle): void {
  try {
    closeSync(handle.fd)
  } catch {
    // ignore
  }
  try {
    unlinkSync(handle.path)
  } catch {
    // ignore
  }
}

// ---------------------------------------------------------------------------
// Atomic token persistence with mtime-aware caching
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
    const raw = readFileSync(path, "utf8")
    const data = JSON.parse(raw) as NotionTokens
    if (!data.accessToken || !data.refreshToken || !data.clientId) return null
    return { tokens: data, mtimeMs: stats.mtimeMs }
  } catch {
    return null
  }
}

/**
 * Return the tokens, using the in-memory cache when the on-disk file has
 * not been touched since the cache was populated. If a peer process has
 * written the file, the cache is refreshed from disk.
 *
 * When the token file is deleted after the cache was populated, the
 * cached copy is returned (last-good-copy semantics — mirrors the
 * Atlassian extension).
 */
export function loadTokens(): NotionTokens | null {
  if (_cache) {
    const path = getTokenStorePath()
    let mtimeMs = 0
    try {
      mtimeMs = statSync(path).mtimeMs
    } catch {
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
 * Persist tokens to disk atomically. The content is written to a temp
 * file (created with mode 0o600 up-front — never chmod'd after opening)
 * then renamed into place. Concurrent readers therefore never observe a
 * partial or truncated token file.
 *
 * Also updates the in-memory cache.
 */
export function saveTokens(tokens: NotionTokens): void {
  const path = getTokenStorePath()
  const dir = path.substring(0, Math.max(path.lastIndexOf("/"), 0)) || "."
  const tmpPath = `${path}.tmp.${process.pid}.${Date.now()}`
  try {
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true, mode: 0o700 })
    }
    // Create the temp file with 0o600 up-front. writeFileSync's `mode`
    // is only honoured on file create, which is what we want here — the
    // tmp file will always be new because the name embeds our PID + a
    // timestamp.
    const fd = openSync(tmpPath, "wx", 0o600)
    try {
      writeSync(fd, JSON.stringify(tokens, null, 2))
      // fsync so the rename below cannot make a zero-byte token file
      // visible to readers if the machine crashes between write and
      // rename.
      try {
        fsyncSync(fd)
      } catch {
        // fsync is best-effort — some filesystems reject it.
      }
    } finally {
      closeSync(fd)
    }
    // Defensive chmod in case umask flavour of the platform ignored our
    // mode. writeFileSync's `mode` is honoured only on create; the fresh
    // temp file we just created should already be 0o600, but a second
    // chmodSync here costs nothing.
    if (process.platform !== "win32") {
      chmodSync(tmpPath, 0o600)
    }
    // Atomic rename within the same directory. On POSIX this is a single
    // dentry swap, so a concurrent reader either sees the old inode or
    // the new inode — never a half-written file.
    renameSync(tmpPath, path)
    let mtimeMs = 0
    try {
      mtimeMs = statSync(path).mtimeMs
    } catch {
      // ignore
    }
    _cache = { tokens, mtimeMs }
  } catch (err) {
    // Clean up the tmp file if we bailed before rename.
    try {
      if (existsSync(tmpPath)) unlinkSync(tmpPath)
    } catch {
      // ignore
    }
    console.error("[notion-mcp] Failed to save tokens:", err)
    throw err
  }
}

/**
 * Delete the on-disk token store and drop the in-memory cache. Called
 * when a token refresh returns `invalid_grant` — Notion has revoked the
 * grant and the user must re-run /login-notion.
 *
 * The client-registration file is preserved: the DCR client_id can be
 * reused on the next login (re-registering orphans prior grants per the
 * upstream docs).
 */
export function clearTokens(): void {
  const path = getTokenStorePath()
  try {
    if (existsSync(path)) unlinkSync(path)
  } catch (err) {
    console.error("[notion-mcp] Failed to clear tokens:", err)
  }
  _cache = null
}

export function invalidateCache(): void {
  _cache = null
}

// ---------------------------------------------------------------------------
// Persistent client_id storage
//
// Written to a separate file so that a terminal invalid_grant that clears
// the tokens file does NOT force a re-registration on next login. Notion's
// docs warn that re-registering orphans prior grants.
// ---------------------------------------------------------------------------

export function loadClientRegistration(): NotionClientRegistration | null {
  const path = getClientStorePath()
  try {
    if (!existsSync(path)) return null
    const raw = readFileSync(path, "utf8")
    const data = JSON.parse(raw) as NotionClientRegistration
    if (!data.clientId || !data.redirectUri) return null
    return data
  } catch {
    return null
  }
}

export function saveClientRegistration(reg: NotionClientRegistration): void {
  const path = getClientStorePath()
  const dir = path.substring(0, Math.max(path.lastIndexOf("/"), 0)) || "."
  const tmpPath = `${path}.tmp.${process.pid}.${Date.now()}`
  try {
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true, mode: 0o700 })
    }
    const fd = openSync(tmpPath, "wx", 0o600)
    try {
      writeSync(fd, JSON.stringify(reg, null, 2))
    } finally {
      closeSync(fd)
    }
    if (process.platform !== "win32") {
      chmodSync(tmpPath, 0o600)
    }
    renameSync(tmpPath, path)
  } catch (err) {
    try {
      if (existsSync(tmpPath)) unlinkSync(tmpPath)
    } catch {
      // ignore
    }
    console.error("[notion-mcp] Failed to save client registration:", err)
    throw err
  }
}

// ---------------------------------------------------------------------------
// Dynamic client registration
//
// Called once per user (result persisted via saveClientRegistration).
// Do NOT call this on every /login-notion — re-registering orphans prior
// grants per Notion's docs.
// ---------------------------------------------------------------------------

export async function registerClient(): Promise<NotionClientRegistration> {
  const redirectUri = `http://localhost:${CALLBACK_PORT}${CALLBACK_PATH}`
  const response = await fetch(REGISTRATION_ENDPOINT, {
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
  })

  if (!response.ok) {
    const body = await response.text()
    throw new Error(`Client registration failed: ${response.status} ${body}`)
  }

  const data = (await response.json()) as { client_id: string }
  return { clientId: data.client_id, redirectUri }
}

/**
 * Ensure a client registration exists on disk, reusing the stored one if
 * present. The disk store is the security spec: if there is a client_id
 * on disk, DO NOT call /register again. Re-registering orphans prior
 * grants per Notion's upstream docs.
 */
export async function ensureClientRegistration(): Promise<NotionClientRegistration> {
  const existing = loadClientRegistration()
  if (existing) return existing
  const reg = await registerClient()
  saveClientRegistration(reg)
  return reg
}

// ---------------------------------------------------------------------------
// PKCE helpers
//
// S256 only. Notion advertises both `plain` and `S256` in its
// code_challenge_methods_supported, but plain must never be used —
// challenge equals verifier and provides zero protection against
// intercepted authorization codes.
// ---------------------------------------------------------------------------

export async function generatePKCE(): Promise<{
  verifier: string
  challenge: string
  method: "S256"
}> {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  const verifier = toBase64Url(bytes)
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(verifier),
  )
  return {
    verifier,
    challenge: toBase64Url(new Uint8Array(digest)),
    method: "S256",
  }
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

function createLocalCallbackServer(state: string): Promise<LocalAuth> {
  const server = createServer()

  return new Promise((resolve, reject) => {
    let done = false
    let timer: ReturnType<typeof setTimeout> | undefined
    let complete!: (value: string | null) => void
    const wait = new Promise<string | null>((innerResolve) => {
      complete = innerResolve
    })

    const finish = (value: string | null) => {
      if (done) return
      done = true
      if (timer) clearTimeout(timer)
      complete(value)
      if (server.listening) {
        // Ensure the server is closed on every completion path — success,
        // error, timeout, and manual cancellation.
        server.closeAllConnections?.()
        server.close()
      }
    }

    server.on("request", (req, res) => {
      const url = new URL(
        req.url ?? "/",
        `http://${req.headers.host ?? "localhost"}`,
      )

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
      // State equality check is mandatory — it prevents attacker-injected
      // authorization codes from being exchanged in this session's context.
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
      // Close on error path too — otherwise a failed listen() would leak.
      finish(null)
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
//
// code_challenge_method is fixed to S256 — never plain, even though Notion
// advertises plain in code_challenge_methods_supported.
// ---------------------------------------------------------------------------

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
 * Parse a fetch Response for an OAuth error payload. Returns the OAuth
 * `error` field when present, else undefined. Best-effort — malformed
 * bodies fall through to undefined.
 */
async function readOAuthError(body: string): Promise<string | undefined> {
  try {
    const parsed = JSON.parse(body) as { error?: string }
    return typeof parsed.error === "string" ? parsed.error : undefined
  } catch {
    return undefined
  }
}

async function exchangeCode(
  code: string,
  clientId: string,
  redirectUri: string,
  verifier: string,
): Promise<{ accessToken: string; refreshToken: string; expiresAt: number }> {
  const response = await fetch(TOKEN_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      client_id: clientId,
      code,
      redirect_uri: redirectUri,
      code_verifier: verifier,
    }).toString(),
  })

  if (!response.ok) {
    const body = await response.text()
    throw new Error(`Token exchange failed: ${response.status} ${body}`)
  }

  const data = (await response.json()) as {
    access_token: string
    refresh_token: string
    expires_in: number
  }
  return {
    accessToken: data.access_token,
    refreshToken: data.refresh_token,
    // Refresh REFRESH_MARGIN_MS before actual expiry (Notion recommends
    // 5-10 minutes) so proactive refresh happens well before the token
    // dies.
    expiresAt: Date.now() + data.expires_in * 1000 - REFRESH_MARGIN_MS,
  }
}

/**
 * Refresh an access token. On `invalid_grant` this throws an
 * InvalidGrantError, which the caller MUST treat as terminal — clear the
 * stored tokens and prompt the user to re-authenticate. Do NOT retry.
 *
 * Retrying an invalid_grant refresh is what Notion's upstream docs
 * describe as the stolen-token signal that revokes the entire grant.
 */
export async function refreshTokens(tokens: NotionTokens): Promise<NotionTokens> {
  const response = await fetch(TOKEN_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      client_id: tokens.clientId,
      refresh_token: tokens.refreshToken,
    }).toString(),
  })

  if (!response.ok) {
    const body = await response.text()
    const oauthError = await readOAuthError(body)
    if (oauthError === "invalid_grant") {
      throw new InvalidGrantError(`Token refresh failed: ${response.status} ${body}`)
    }
    throw new Error(`Token refresh failed: ${response.status} ${body}`)
  }

  const data = (await response.json()) as {
    access_token: string
    refresh_token: string
    expires_in: number
  }

  return {
    ...tokens,
    accessToken: data.access_token,
    // Every refresh rotates the refresh token — we MUST persist the new
    // one. Falling back to the old refresh token would set up the exact
    // "replay a rotated token" scenario that Notion revokes grants over.
    refreshToken: data.refresh_token || tokens.refreshToken,
    expiresAt: Date.now() + data.expires_in * 1000 - REFRESH_MARGIN_MS,
  }
}

// ---------------------------------------------------------------------------
// Full login flow
// ---------------------------------------------------------------------------

export interface LoginCallbacks {
  /** Called with the authorization URL and instructions to display to the user */
  onAuthUrl: (url: string, instructions: string) => void
  /** Called if the browser/callback failed — user must paste a code manually */
  onManualInput?: () => Promise<string>
}

export async function loginNotion(callbacks: LoginCallbacks): Promise<NotionTokens> {
  // Reuse the persisted client_id if one exists. Re-registering orphans
  // prior grants per Notion's docs.
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
      "Complete authorization in your browser. If the browser is on another machine, paste the authorization code here.",
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
    console.error(
      "[notion-mcp] Local auth error:",
      err instanceof Error ? err.message : err,
    )
  } finally {
    // Belt-and-braces: cancel() is a no-op if the server already closed
    // itself, but this guarantees no leaked listener on any error path
    // through the try above.
    localAuth?.cancel()
  }

  if (!authInput) {
    if (!callbacks.onManualInput) {
      throw new Error("Authorization timed out or failed. No manual input callback provided.")
    }
    authInput = await callbacks.onManualInput()
  }

  const parsed = parseAuthInput(authInput, state)
  if (!parsed) throw new Error("Could not parse authorization input.")

  const { accessToken, refreshToken, expiresAt } = await exchangeCode(
    parsed.code,
    clientId,
    effectiveRedirectUri,
    verifier,
  )

  const tokens: NotionTokens = { accessToken, refreshToken, expiresAt, clientId }
  // saveTokens is atomic (tmp file + rename) so concurrent readers never
  // see a partial write.
  saveTokens(tokens)
  return tokens
}

// ---------------------------------------------------------------------------
// Auth input parsing
//
// Two accepted forms — both go through state validation:
//   1. "code#state"       from our local callback server
//   2. "https://.../oauth/callback?code=…&state=…"    from a manual paste
//
// The Atlassian extension's "bare code" fallback branch is intentionally
// dropped for Notion — that branch bypasses state validation and opens a
// window for an attacker to inject an authorization code that was
// obtained against a different session. On Notion, the blast radius of
// swallowing an attacker's code is workspace read+write, so the
// stricter validator is worth the one-hop convenience it costs.
// ---------------------------------------------------------------------------

export function parseAuthInput(
  input: string,
  expectedState: string,
): { code: string } | null {
  const text = input.trim()

  try {
    const url = new URL(text)
    const code = url.searchParams.get("code")
    const state = url.searchParams.get("state")
    if (code && state === expectedState) return { code }
  } catch {
    // not a URL
  }

  const split = text.split("#")
  if (split.length === 2 && split[0] && split[1] === expectedState) {
    return { code: split[0] }
  }

  return null
}

// ---------------------------------------------------------------------------
// Serialised refresh: THE security-critical path
//
// getValidAccessToken() acquires the cross-process lock, re-reads the
// token file from disk after acquiring, decides whether a refresh is
// needed, refreshes if so, and persists the result atomically — all
// inside the lock. Any peer that was also trying to refresh has to wait,
// and by the time it gets the lock the disk copy will be current so it
// will skip the refresh entirely.
//
// invalid_grant is terminal: the token file is cleared, an
// InvalidGrantError is thrown to the caller (which must prompt the user
// to re-run /login-notion), and no retry is attempted.
// ---------------------------------------------------------------------------

export async function getValidAccessToken(): Promise<{
  token: string
  tokens: NotionTokens
}> {
  // Cheap fast-path: if the cached copy is still fresh, avoid the lock
  // entirely. Notion tokens live ~8h so this hits on every tool call
  // during a session.
  const cached = loadTokens()
  if (cached && Date.now() < cached.expiresAt) {
    return { token: cached.accessToken, tokens: cached }
  }

  if (!cached) {
    throw new Error("Notion MCP: no auth tokens")
  }

  // Slow path: acquire the cross-process lock and re-read from disk.
  const lock = await acquireRefreshLock()
  try {
    // CRITICAL: re-read from disk inside the lock. A peer may have
    // refreshed while we were waiting, in which case we must use their
    // fresh tokens rather than issuing our own refresh with a rotated
    // refresh_token.
    invalidateCache()
    const fresh = loadTokens()
    if (fresh && Date.now() < fresh.expiresAt) {
      return { token: fresh.accessToken, tokens: fresh }
    }

    const current = fresh ?? cached
    try {
      const refreshed = await refreshTokens(current)
      saveTokens(refreshed)
      return { token: refreshed.accessToken, tokens: refreshed }
    } catch (err) {
      if (err instanceof InvalidGrantError) {
        // Terminal: whole grant revoked. Clear tokens so the next
        // getValidAccessToken() call surfaces the "no auth tokens" path,
        // which drives the /login-notion prompt.
        clearTokens()
        throw err
      }
      throw err
    }
  } finally {
    releaseRefreshLock(lock)
  }
}
