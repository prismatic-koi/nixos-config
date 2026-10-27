// Pi-specific OAuth 2.0 + PKCE flow with local callback server.
// Primary source: leohenon/pi-anthropic-oauth src/auth.ts
// Mirror target: griffinmartin/opencode-claude-auth src/credentials.ts (OAuth helpers)
//
// pi-specific: This file implements pi's OAuthLoginCallbacks interface
// (loginAnthropic / refreshAnthropicToken). griffinmartin has no equivalent
// because opencode uses a different auth bootstrapping mechanism.

import { createServer } from "node:http"
import { log } from "./logger.ts"
import {
  invalidateCache,
  readCredentials,
  writeCredentials,
  type ClaudeCredentials,
} from "./credentials.ts"
import { acquireRefreshLock } from "./refresh-lock.ts"

const CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
const AUTHORIZE_URL = "https://claude.ai/oauth/authorize"
const TOKEN_URL = "https://claude.ai/v1/oauth/token"
const REDIRECT_URI = "https://platform.claude.com/oauth/code/callback"
const SCOPES = [
  "org:create_api_key",
  "user:profile",
  "user:inference",
  "user:sessions:claude_code",
  "user:mcp_servers",
  "user:file_upload",
].join(" ")
const USER_AGENT = "claude-code/2.1.97"
const CALLBACK_PORT = 53692
const CALLBACK_HOST = "127.0.0.1"
const LOCAL_CALLBACK_TIMEOUT = 5 * 60 * 1000
const MAX_TOKEN_RETRIES = 2
const INITIAL_RETRY_DELAY_MS = 5000
// pi calls refreshToken when the stored token expires within 5 minutes
// (pi-ai auth/resolve.js DEFAULT_OAUTH_MINIMUM_VALIDITY_MS). A token that a
// peer wrote is adopted only if it is valid past that margin.
const PI_MIN_VALIDITY_MS = 5 * 60 * 1000
// Bounds the refresh request that runs under the shared lock, the waits
// between its retries included.
const REFRESH_REQUEST_TIMEOUT_MS = 15_000

export { USER_AGENT, CLIENT_ID }

export interface OAuthCredentials {
  access: string
  refresh: string
  expires: number
}

// pi-specific: OAuthLoginCallbacks mirrors @earendil-works/pi-ai's interface
export interface OAuthLoginCallbacks {
  onAuth: (opts: { url: string; instructions: string }) => void
  onManualCodeInput?: () => Promise<string>
  onPrompt: (opts: { message: string }) => Promise<string>
  signal?: AbortSignal
}

type ParsedAuthInput = { code: string; state: string }
type LocalAuthorization = {
  redirectUri: string
  waitForCallback: () => Promise<string | null>
  cancel: () => void
}

export function isClaudeOAuthAccessToken(apiKey: string): boolean {
  return apiKey.includes("sk-ant-oat")
}

// pi-specific: loginAnthropic is called by pi.registerProvider oauth.login
export async function loginAnthropic(
  callbacks: OAuthLoginCallbacks,
): Promise<OAuthCredentials> {
  const { verifier, challenge } = await generatePKCE()
  const state = crypto.randomUUID().replace(/-/g, "")

  let authInput: string | null = null
  let redirectUri = REDIRECT_URI

  try {
    const localAuthorization = await createLocalAuthorization(state)
    redirectUri = localAuthorization.redirectUri

    callbacks.onAuth({
      url: makeAuthorizeUrl(challenge, state, redirectUri),
      instructions:
        "Complete login in your browser. If the browser is on another machine, paste the final redirect URL here.",
    })

    if (callbacks.onManualCodeInput) {
      let manualInput: string | undefined
      let manualError: Error | undefined
      const manualPromise = callbacks
        .onManualCodeInput()
        .then((input) => {
          manualInput = input
          localAuthorization.cancel()
        })
        .catch((err) => {
          manualError = err instanceof Error ? err : new Error(String(err))
          localAuthorization.cancel()
        })

      const callbackResult = await localAuthorization.waitForCallback()

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
      authInput = await localAuthorization.waitForCallback()
    }
  } catch (err) {
    log("login_local_auth_error", {
      error: err instanceof Error ? err.message : String(err),
    })
  }

  if (!authInput) {
    redirectUri = REDIRECT_URI
    callbacks.onAuth({
      url: makeAuthorizeUrl(challenge, state, redirectUri),
      instructions:
        "Sign in with Claude, then paste the full callback URL or the code#state value.",
    })
    authInput = await callbacks.onPrompt({
      message: "Paste the callback URL or code#state:",
    })
  }

  const parsed = parseAuthInput(authInput)
  if (!parsed) throw new Error("Could not parse authorization callback input.")
  if (parsed.state !== state) throw new Error("OAuth state mismatch.")

  log("login_token_exchange_start", {})
  const tokenResponse = await fetchWithRetry(
    TOKEN_URL,
    {
      method: "POST",
      headers: makeTokenHeaders(),
      body: new URLSearchParams({
        grant_type: "authorization_code",
        client_id: CLIENT_ID,
        code: parsed.code,
        state: parsed.state,
        redirect_uri: redirectUri,
        code_verifier: verifier,
      }).toString(),
      signal: callbacks.signal,
    },
    "Token exchange",
  )

  const data = (await tokenResponse.json()) as {
    access_token: string
    refresh_token: string
    expires_in: number
  }

  log("login_complete", {})
  return {
    access: data.access_token,
    refresh: data.refresh_token,
    expires: Date.now() + data.expires_in * 1000 - 5 * 60 * 1000,
  }
}

function toOAuthCredentials(creds: ClaudeCredentials): OAuthCredentials {
  return {
    access: creds.accessToken,
    refresh: creds.refreshToken,
    expires: creds.expiresAt,
  }
}

// pi-specific: refreshAnthropicToken is called by pi.registerProvider oauth.refreshToken
//
// The refresh runs under the shared lock of refresh-lock.ts (#3289). pi's
// own auth.json.lock is private to a bwrap sandbox, so it does not stop two
// sessions from sending the same refresh token. Under the shared lock this
// handler reads auth.json again. If a peer already wrote a new token that
// pi accepts, it returns that token and sends no request. Else it refreshes
// with the newest refresh token on disk, and writes the result before it
// releases the lock, so that the next holder reads it.
export async function refreshAnthropicToken(
  credentials: OAuthCredentials,
): Promise<OAuthCredentials> {
  // A token is adopted only when a peer wrote it: pi can ask for a refresh
  // of a token that is valid for more than 5 minutes, when it needs a
  // longer life.
  const peerToken = (stored: ClaudeCredentials | null): stored is ClaudeCredentials =>
    stored !== null &&
    stored.accessToken !== credentials.access &&
    stored.expiresAt > Date.now() + PI_MIN_VALIDITY_MS

  const lock = await acquireRefreshLock()
  if (!lock) {
    const stored = readCredentials()
    if (peerToken(stored)) return toOAuthCredentials(stored)
    // The same fallback as a failed request: keep a token that still works
    // for a short time. Do not refresh without the lock.
    const current = stored ? toOAuthCredentials(stored) : credentials
    if (current.expires > Date.now()) {
      log("refresh_lock_timeout_fallback", {})
      return { ...current, expires: Date.now() + 30_000 }
    }
    throw new Error(
      "Token refresh failed: another pi session holds the refresh lock and did not write a new token.",
    )
  }

  try {
    const stored = readCredentials()
    if (peerToken(stored)) {
      log("refresh_adopted", { expiresAt: stored.expiresAt })
      return toOAuthCredentials(stored)
    }
    const current = stored ? toOAuthCredentials(stored) : credentials
    const { result, refreshed } = await requestTokenRefresh(current)
    if (refreshed) {
      // A failed write must not lose the new token: pi writes the result
      // too, after this handler returns.
      try {
        writeCredentials({
          accessToken: result.access,
          refreshToken: result.refresh,
          expiresAt: result.expires,
        })
        invalidateCache()
      } catch (err) {
        log("credentials_write_error", {
          error: err instanceof Error ? err.message : String(err),
        })
      }
    }
    return result
  } finally {
    lock.release()
  }
}

// requestTokenRefresh sends the refresh request. refreshed is false when
// the request failed and the result is the old token with a short expiry.
// That result must not be written to auth.json.
async function requestTokenRefresh(
  credentials: OAuthCredentials,
): Promise<{ result: OAuthCredentials; refreshed: boolean }> {
  let response: Response
  try {
    response = await fetchWithRetry(
      TOKEN_URL,
      {
        method: "POST",
        headers: makeTokenHeaders(),
        body: new URLSearchParams({
          grant_type: "refresh_token",
          client_id: CLIENT_ID,
          refresh_token: credentials.refresh,
        }).toString(),
        signal: AbortSignal.timeout(REFRESH_REQUEST_TIMEOUT_MS),
      },
      "Token refresh",
    )
  } catch (err) {
    log("refresh_network_error", {
      error: err instanceof Error ? err.message : String(err),
    })
    if (credentials.expires > Date.now()) {
      return {
        result: { ...credentials, expires: Date.now() + 30_000 },
        refreshed: false,
      }
    }
    throw new Error("Token refresh failed and token has expired.")
  }

  const data = (await response.json()) as {
    access_token: string
    refresh_token: string
    expires_in: number
  }

  log("refresh_complete", {})
  return {
    result: {
      access: data.access_token,
      refresh: data.refresh_token || credentials.refresh,
      expires: Date.now() + data.expires_in * 1000 - 5 * 60 * 1000,
    },
    refreshed: true,
  }
}

function makeAuthorizeUrl(
  challenge: string,
  state: string,
  redirectUri: string,
): string {
  const authParams = new URLSearchParams({
    code: "true",
    client_id: CLIENT_ID,
    response_type: "code",
    redirect_uri: redirectUri,
    scope: SCOPES,
    code_challenge: challenge,
    code_challenge_method: "S256",
    state,
  })

  return `${AUTHORIZE_URL}?${authParams.toString()}`
}

async function fetchWithRetry(
  url: string,
  init: RequestInit,
  label: string,
): Promise<Response> {
  let lastError: Error | undefined

  for (let attempt = 0; attempt <= MAX_TOKEN_RETRIES; attempt++) {
    const response = await fetch(url, init)

    if (response.ok) return response

    const bodyText = await response.text()

    const shouldRetry = response.headers.get("x-should-retry")
    if (shouldRetry === "false") {
      throw new Error(`${label} failed: ${response.status} ${bodyText}`)
    }

    if (
      attempt < MAX_TOKEN_RETRIES &&
      (response.status === 429 || response.status >= 500)
    ) {
      const retryAfter = response.headers.get("retry-after")
      const delayMs = retryAfter
        ? Math.min(Number(retryAfter) * 1000, 30_000)
        : INITIAL_RETRY_DELAY_MS * 2 ** attempt

      await abortableDelay(delayMs, init.signal ?? undefined)
      lastError = new Error(`${label} failed: ${response.status} ${bodyText}`)
      continue
    }

    throw new Error(`${label} failed: ${response.status} ${bodyText}`)
  }

  throw lastError ?? new Error(`${label} failed after retries`)
}

// abortableDelay waits ms, and rejects when signal aborts first. The
// refresh request passes a timeout signal, so its retries stay inside that
// time.
function abortableDelay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason)
    const onAbort = () => {
      clearTimeout(timer)
      reject(signal?.reason)
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort)
      resolve()
    }, ms)
    signal?.addEventListener("abort", onAbort, { once: true })
  })
}

function makeTokenHeaders(): HeadersInit {
  return {
    "Content-Type": "application/x-www-form-urlencoded",
  }
}

async function createLocalAuthorization(
  state: string,
): Promise<LocalAuthorization> {
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
        server.closeAllConnections?.()
        server.close()
      }
    }

    server.on("request", (req, res) => {
      const url = new URL(
        req.url ?? "/",
        `http://${req.headers.host ?? "localhost"}`,
      )

      if (url.pathname !== "/callback") {
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
      res.end(makeCallbackPage())
      finish(url.toString())
    })

    server.once("error", reject)

    server.listen(CALLBACK_PORT, CALLBACK_HOST, () => {
      timer = setTimeout(() => finish(null), LOCAL_CALLBACK_TIMEOUT)
      resolve({
        redirectUri: `http://localhost:${CALLBACK_PORT}/callback`,
        waitForCallback: () => wait,
        cancel: () => finish(null),
      })
    })
  })
}

function makeCallbackPage(): string {
  return `<!doctype html>
<html>
  <head><meta charset="utf-8" /><title>Authorization complete</title></head>
  <body>
    <h1>Authorization complete</h1>
    <p>You can close this window and return to pi.</p>
  </body>
</html>`
}

async function generatePKCE(): Promise<{
  verifier: string
  challenge: string
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
  }
}

function toBase64Url(bytes: Uint8Array): string {
  return Buffer.from(bytes)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/g, "")
}

function parseAuthInput(input: string): ParsedAuthInput | null {
  const text = input.trim()

  try {
    const url = new URL(text)
    const code = url.searchParams.get("code")
    const state = url.searchParams.get("state")
    if (code && state) return { code, state }
  } catch {}

  const split = text.split("#")
  if (split.length === 2 && split[0] && split[1]) {
    return { code: split[0], state: split[1] }
  }

  const params = new URLSearchParams(text)
  const code = params.get("code")
  const state = params.get("state")
  return code && state ? { code, state } : null
}
