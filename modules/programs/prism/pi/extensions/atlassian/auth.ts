// Auth module for the Atlassian MCP pi extension.
//
// Auth method: HTTP Basic auth using the existing ATLASSIAN_EMAIL + ATLASSIAN_API_TOKEN
// environment variables that already exist in this repo (set by
// modules/programs/atlassian/default.nix via sops-managed secrets).
//
// The mcp.atlassian.com server accepts Basic auth (base64 of email:token) for its
// API token path, which returns `initialize` and `tools/list` successfully,
// but the API token path only exposes 2 Teamwork Graph tools (getTeamworkGraphContext,
// getTeamworkGraphObject) — not the full Jira/Confluence CRUD surface.
//
// The OAuth PKCE path (used by mcp-remote) gives 31 tools including the full
// Jira and Confluence CRUD surface. We therefore use OAuth via the Atlassian
// MCP authorization server.
//
// OAuth server discovery: https://mcp.atlassian.com/.well-known/oauth-authorization-server
//   - authorization_endpoint: https://mcp.atlassian.com/v1/authorize
//   - token_endpoint: https://cf.mcp.atlassian.com/v1/token
//   - registration_endpoint: https://cf.mcp.atlassian.com/v1/register
//
// Token storage: ~/.pi/agent/atlassian-mcp-oauth.json (mirrors ~/.mcp-auth/mcp-remote-0.1.13/)
//
// See UPSTREAM.md for auth method rationale and provenance.

import { createServer } from "node:http"
import { readFileSync, writeFileSync, mkdirSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const AUTHORIZATION_ENDPOINT = "https://mcp.atlassian.com/v1/authorize"
const TOKEN_ENDPOINT = "https://cf.mcp.atlassian.com/v1/token"
const REGISTRATION_ENDPOINT = "https://cf.mcp.atlassian.com/v1/register"

const CALLBACK_PORT = 3737
const CALLBACK_HOST = "127.0.0.1"
const CALLBACK_PATH = "/oauth/callback"
const LOCAL_CALLBACK_TIMEOUT = 5 * 60 * 1000

const TOKEN_STORE_DIR = join(homedir(), ".pi", "agent")
const TOKEN_STORE_PATH = join(TOKEN_STORE_DIR, "atlassian-mcp-oauth.json")

export interface AtlassianTokens {
  accessToken: string
  refreshToken: string
  /** Epoch ms when the access token expires */
  expiresAt: number
  clientId: string
}

// ---------------------------------------------------------------------------
// Token persistence
// ---------------------------------------------------------------------------

export function loadTokens(): AtlassianTokens | null {
  try {
    const raw = readFileSync(TOKEN_STORE_PATH, "utf8")
    const data = JSON.parse(raw) as AtlassianTokens
    if (!data.accessToken || !data.refreshToken || !data.clientId) return null
    return data
  } catch {
    return null
  }
}

export function saveTokens(tokens: AtlassianTokens): void {
  try {
    mkdirSync(TOKEN_STORE_DIR, { recursive: true })
    writeFileSync(TOKEN_STORE_PATH, JSON.stringify(tokens, null, 2), { mode: 0o600 })
  } catch (err) {
    console.error("[atlassian-mcp] Failed to save tokens:", err)
  }
}

// ---------------------------------------------------------------------------
// Dynamic client registration
// ---------------------------------------------------------------------------

export interface ClientInfo {
  clientId: string
  redirectUri: string
}

export async function registerClient(): Promise<ClientInfo> {
  const redirectUri = `http://localhost:${CALLBACK_PORT}${CALLBACK_PATH}`
  const response = await fetch(REGISTRATION_ENDPOINT, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      redirect_uris: [redirectUri],
      token_endpoint_auth_method: "none",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      client_name: "pi-atlassian-mcp",
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

// ---------------------------------------------------------------------------
// PKCE helpers
// ---------------------------------------------------------------------------

export async function generatePKCE(): Promise<{
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
  <head><meta charset="utf-8"><title>Atlassian authorization complete</title></head>
  <body>
    <h1>Authorization complete</h1>
    <p>You can close this window and return to pi.</p>
  </body>
</html>`)
      finish(`${code}#${gotState}`)
    })

    server.once("error", reject)

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

function makeAuthorizeUrl(
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
    expiresAt: Date.now() + data.expires_in * 1000 - 60_000,
  }
}

export async function refreshTokens(tokens: AtlassianTokens): Promise<AtlassianTokens> {
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
    refreshToken: data.refresh_token || tokens.refreshToken,
    expiresAt: Date.now() + data.expires_in * 1000 - 60_000,
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

export async function loginAtlassian(callbacks: LoginCallbacks): Promise<AtlassianTokens> {
  // Register a new client (Atlassian's OAuth server requires dynamic registration)
  const { clientId, redirectUri } = await registerClient()

  const { verifier, challenge } = await generatePKCE()
  const state = crypto.randomUUID().replace(/-/g, "")

  // Start local callback server
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
    console.error("[atlassian-mcp] Local auth error:", err instanceof Error ? err.message : err)
  }

  if (!authInput) {
    // Fallback: user must paste the code
    if (!callbacks.onManualInput) {
      throw new Error("Authorization timed out or failed. No manual input callback provided.")
    }
    authInput = await callbacks.onManualInput()
  }

  // Parse the auth input: either "code#state" (from our local server) or a URL
  const parsed = parseAuthInput(authInput, state)
  if (!parsed) throw new Error("Could not parse authorization input.")

  const { accessToken, refreshToken, expiresAt } = await exchangeCode(
    parsed.code,
    clientId,
    effectiveRedirectUri,
    verifier,
  )

  const tokens: AtlassianTokens = { accessToken, refreshToken, expiresAt, clientId }
  saveTokens(tokens)
  return tokens
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function parseAuthInput(
  input: string,
  expectedState: string,
): { code: string } | null {
  const text = input.trim()

  // Try URL
  try {
    const url = new URL(text)
    const code = url.searchParams.get("code")
    const state = url.searchParams.get("state")
    if (code && state === expectedState) return { code }
  } catch {
    // not a URL
  }

  // Try "code#state" format (from our local server)
  const split = text.split("#")
  if (split.length === 2 && split[0] && split[1] === expectedState) {
    return { code: split[0] }
  }

  // Try just a code (user pasted only the code)
  if (text.length > 10 && !text.includes(" ")) {
    return { code: text }
  }

  return null
}

// ---------------------------------------------------------------------------
// Get a valid bearer token (refresh if needed)
// ---------------------------------------------------------------------------

/**
 * Returns a valid access token, refreshing if expired.
 * Saves refreshed tokens back to disk.
 * Throws if tokens are null or refresh fails.
 */
export async function getValidAccessToken(tokens: AtlassianTokens): Promise<{ token: string; tokens: AtlassianTokens }> {
  if (Date.now() < tokens.expiresAt) {
    return { token: tokens.accessToken, tokens }
  }
  // Refresh
  const refreshed = await refreshTokens(tokens)
  saveTokens(refreshed)
  return { token: refreshed.accessToken, tokens: refreshed }
}
