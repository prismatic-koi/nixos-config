// Mirror of griffinmartin/opencode-claude-auth src/credentials.ts
// adapted for pi's credential storage at ~/.pi/agent/auth.json.
// Source: https://github.com/griffinmartin/opencode-claude-auth
//
// pi-specific divergences:
//   - No keychain.ts (macOS only) — pi stores OAuth creds in auth.json
//   - Credential path is ~/.pi/agent/auth.json (not opencode's auth.json)
//   - refreshViaOAuth is the primary refresh path (no CLI fallback)
//   - No multi-account/account-switching support (pi is single-account)

import { execFileSync } from "node:child_process"
import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs"
import { homedir } from "node:os"
import { dirname, join } from "node:path"
import { log } from "./logger.ts"

export interface ClaudeCredentials {
  accessToken: string
  refreshToken: string
  expiresAt: number
}

const CREDENTIAL_CACHE_TTL_MS = 30_000

let cachedCreds: { creds: ClaudeCredentials; cachedAt: number } | null = null

export const OAUTH_TOKEN_URL = "https://claude.ai/v1/oauth/token"
export const OAUTH_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

/**
 * Resolve the path to pi's auth.json, in precedence order:
 *
 *   1. PI_AUTH_JSON      explicit override (tests, manual escape hatch)
 *   2. PI_CODING_AGENT_DIR/auth.json   set by the prism bwrap + sandbox-exec
 *                                       dispatchers, and on the host system-
 *                                       wide. This is the path that surfaces
 *                                       host-side os.Rename swaps inside a
 *                                       running sandbox (#2283).
 *   3. ~/.pi/agent/auth.json           legacy fallback.
 *
 * The middle entry is the critical piece for `prism account use`. The bwrap
 * dispatcher dir-binds the host's ~/.pi/agent at $PI_CODING_AGENT_DIR (a
 * directory bind), and ALSO file-binds auth.json at its host path for
 * back-compat. Linux `mount --bind` on a file pins the source inode at
 * mount time — so a host-side `os.Rename` (which creates a new inode) is
 * NOT visible at the file-bind path inside an already-running sandbox.
 * The dir-bind, by contrast, surfaces dentry updates (renames included),
 * so reading via $PI_CODING_AGENT_DIR/auth.json sees the post-swap blob
 * immediately. Combined with the mtime check below, this makes the swap
 * visible to a running pi without a restart.
 */
function getAuthJsonPath(): string {
  if (process.env.PI_AUTH_JSON) return process.env.PI_AUTH_JSON
  if (process.env.PI_CODING_AGENT_DIR) {
    return join(process.env.PI_CODING_AGENT_DIR, "auth.json")
  }
  return join(homedir(), ".pi", "agent", "auth.json")
}

/**
 * Read OAuth credentials from ~/.pi/agent/auth.json.
 * Returns null if the file is absent, malformed, or missing required fields.
 */
export function readCredentials(): ClaudeCredentials | null {
  const authPath = getAuthJsonPath()
  try {
    if (!existsSync(authPath)) return null
    const raw = readFileSync(authPath, "utf-8").trim()
    if (!raw) return null
    const data = JSON.parse(raw) as {
      anthropic?: {
        access?: string
        refresh?: string
        expires?: number
      }
    }
    const provider = data.anthropic
    if (
      !provider ||
      typeof provider.access !== "string" ||
      typeof provider.refresh !== "string" ||
      typeof provider.expires !== "number"
    ) {
      log("credentials_read_malformed", { path: authPath })
      return null
    }
    log("credentials_read_ok", { path: authPath })
    return {
      accessToken: provider.access,
      refreshToken: provider.refresh,
      expiresAt: provider.expires,
    }
  } catch (err) {
    log("credentials_read_error", {
      path: authPath,
      error: err instanceof Error ? err.message : String(err),
    })
    return null
  }
}

/**
 * Write updated credentials back to ~/.pi/agent/auth.json.
 * Preserves any extra fields already in the file (e.g. pi's own keys).
 */
export function writeCredentials(creds: ClaudeCredentials): void {
  const authPath = getAuthJsonPath()
  let existing: Record<string, unknown> = {}
  try {
    if (existsSync(authPath)) {
      const raw = readFileSync(authPath, "utf-8").trim()
      if (raw) existing = JSON.parse(raw) as Record<string, unknown>
    }
  } catch {
    // Start fresh if malformed
  }

  existing.anthropic = {
    type: "oauth",
    access: creds.accessToken,
    refresh: creds.refreshToken,
    expires: creds.expiresAt,
  }

  const dir = dirname(authPath)
  if (!existsSync(dir)) {
    mkdirSync(dir, { recursive: true, mode: 0o700 })
  }
  writeFileSync(authPath, JSON.stringify(existing, null, 2), {
    encoding: "utf-8",
    mode: 0o600,
  })
  if (process.platform !== "win32") {
    chmodSync(authPath, 0o600)
  }
  log("credentials_write_ok", { path: authPath })
}

/**
 * One-time data repair: if auth.json contains an anthropic entry without
 * type: "oauth", write it back with the field added. This self-heals existing
 * installations that were corrupted by older versions of writeCredentials().
 * Safe to call on every startup — a no-op when the entry is already correct
 * or absent.
 */
export function repairCredentials(): void {
  const authPath = getAuthJsonPath()
  try {
    if (!existsSync(authPath)) return
    const raw = readFileSync(authPath, "utf-8").trim()
    if (!raw) return
    const data = JSON.parse(raw) as Record<string, unknown>
    const provider = data.anthropic as Record<string, unknown> | undefined
    if (!provider) return
    if (provider.type === "oauth") return // already correct, nothing to do

    // Entry exists but lacks type: "oauth" — repair it in place
    log("credentials_repair", { path: authPath })
    provider.type = "oauth"
    writeFileSync(authPath, JSON.stringify(data, null, 2), {
      encoding: "utf-8",
      mode: 0o600,
    })
    if (process.platform !== "win32") {
      chmodSync(authPath, 0o600)
    }
    log("credentials_repair_ok", { path: authPath })
  } catch (err) {
    log("credentials_repair_error", {
      path: authPath,
      error: err instanceof Error ? err.message : String(err),
    })
  }
}

/**
 * Parse a raw OAuth token response into ClaudeCredentials.
 * Returns null if the response is missing a valid access_token.
 * Defaults expires_in to 36000s (10h) to match observed Claude token lifetime.
 */
export function parseOAuthResponse(
  raw: string,
  currentRefreshToken: string,
  now: number = Date.now(),
): ClaudeCredentials | null {
  let data: {
    access_token?: string
    refresh_token?: string
    expires_in?: number
    error?: string
  }
  try {
    data = JSON.parse(raw)
  } catch {
    return null
  }

  if (!data.access_token) return null

  return {
    accessToken: data.access_token,
    refreshToken: data.refresh_token ?? currentRefreshToken,
    expiresAt: now + (data.expires_in ?? 36_000) * 1000,
  }
}

export function refreshViaOAuth(
  refreshToken: string,
): ClaudeCredentials | null {
  // Use a Node subprocess to perform the HTTP request synchronously.
  // The refresh token is passed via stdin to avoid exposure in process args.
  const script = `
    process.stdin.resume();
    let input = '';
    process.stdin.on('data', c => input += c);
    process.stdin.on('end', () => {
      const body = new URLSearchParams({
        grant_type: 'refresh_token',
        client_id: '${OAUTH_CLIENT_ID}',
        refresh_token: input.trim()
      });
      fetch('${OAUTH_TOKEN_URL}', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: body.toString()
      })
      .then(r => { if (!r.ok) throw new Error(String(r.status)); return r.json(); })
      .then(d => { process.stdout.write(JSON.stringify(d)); })
      .catch(e => { process.stdout.write(JSON.stringify({ error: String(e) })); process.exit(1); });
    });
  `

  try {
    log("refresh_started", { source: "oauth" })
    const result = execFileSync(process.execPath, ["-e", script], {
      input: refreshToken,
      timeout: 15_000,
      encoding: "utf-8",
      stdio: ["pipe", "pipe", "ignore"],
    })

    const creds = parseOAuthResponse(result, refreshToken)
    if (!creds) {
      log("refresh_failed", {
        source: "oauth",
        error: "no access_token in response",
      })
      return null
    }

    log("refresh_success", { source: "oauth" })
    return creds
  } catch (err) {
    log("refresh_failed", {
      source: "oauth",
      error: err instanceof Error ? err.message : String(err),
    })
    return null
  }
}

export function refreshIfNeeded(
  creds: ClaudeCredentials,
): ClaudeCredentials | null {
  if (creds.expiresAt > Date.now() + 60_000) return creds

  log("refresh_needed", {
    expiresAt: creds.expiresAt,
    expiresIn: creds.expiresAt - Date.now(),
  })

  if (creds.refreshToken) {
    const fresh = refreshViaOAuth(creds.refreshToken)
    if (fresh && fresh.expiresAt > Date.now() + 60_000) {
      writeCredentials(fresh)
      cachedCreds = null // invalidate cache so next call sees fresh creds
      return fresh
    }
  }

  log("refresh_exhausted", { expiresAt: creds.expiresAt })
  return null
}

export function getCachedCredentials(): ClaudeCredentials | null {
  const now = Date.now()

  if (
    cachedCreds &&
    now - cachedCreds.cachedAt < CREDENTIAL_CACHE_TTL_MS &&
    cachedCreds.creds.expiresAt > now + 60_000
  ) {
    // Bypass the cache if auth.json has been touched since we cached.
    // This is the `prism account use` hand-off path (#2283): the swap
    // renames a new auth.json over the old, bumping mtime; the next
    // outbound request sees mtime > cachedAt and re-reads the file
    // instead of returning the stale tokens.
    //
    // statSync may throw if the file was unlinked between writes; treat
    // that as a cache miss (fall through, re-read, log normally).
    let mtimeMs = 0
    try {
      mtimeMs = statSync(getAuthJsonPath()).mtimeMs
    } catch {
      mtimeMs = 0
    }
    if (mtimeMs > cachedCreds.cachedAt) {
      log("cache_invalidated_mtime", {
        mtimeMs,
        cachedAt: cachedCreds.cachedAt,
      })
    } else {
      log("cache_hit", {
        ttlRemaining: CREDENTIAL_CACHE_TTL_MS - (now - cachedCreds.cachedAt),
      })
      return cachedCreds.creds
    }
  }

  log("cache_miss", { reason: cachedCreds ? "stale or expiring" : "empty" })

  const stored = readCredentials()
  if (!stored) {
    log("credentials_unavailable", { reason: "not found in auth.json" })
    cachedCreds = null
    return null
  }

  const fresh = refreshIfNeeded(stored)
  if (!fresh) {
    log("credentials_unavailable", { reason: "refresh failed" })
    cachedCreds = null
    return null
  }

  cachedCreds = { creds: fresh, cachedAt: now }
  return fresh
}

export function invalidateCache(): void {
  cachedCreds = null
}
