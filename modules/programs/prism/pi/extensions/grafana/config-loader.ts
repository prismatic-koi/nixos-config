// Loader for the sops-decrypted Grafana config bundle.
//
// The bundle is a dotenv-style KEY=VALUE file — see UPSTREAM.md ("Config
// bundle in sops"). This module reads the file, parses the two well-known
// keys (GRAFANA_URL, GRAFANA_API_KEY), and returns them alongside any
// unrecognised KEY=VALUE lines that a future bundle format might introduce.
//
// SECURITY: the returned api_key is a live credential. Never log
// `loadedConfig`, `apiKey`, or the raw file contents. `debug` is limited to
// the file path.

import { readFileSync } from "node:fs"

function debug(...args: unknown[]): void {
  if (process.env.GRAFANA_MCP_DEBUG === "1") {
    console.error("[grafana-mcp]", ...args)
  }
}

export interface GrafanaBundle {
  url: string
  apiKey: string
  /** Any additional KEY=VALUE lines beyond url/apiKey, in file order. */
  extraEnv: Record<string, string>
}

export class GrafanaConfigError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "GrafanaConfigError"
  }
}

/**
 * Parse a dotenv-style buffer into KEY=VALUE pairs.
 *
 *   - Blank lines and lines beginning with `#` are ignored.
 *   - Values are the raw text after the first `=` (no shell unquoting; sops
 *     preserves whatever the plaintext file contained).
 *   - Trailing "\r" is stripped so Windows-CRLF files still parse cleanly.
 *   - Duplicate keys keep the LAST value (dotenv convention); no warning is
 *     emitted because our own bundles are hand-authored and duplicates would
 *     be a caller bug we cannot fix here.
 *
 * Malformed lines (no `=` after the key) throw. The caller catches and
 * surfaces via ctx.ui.notify per the "fail-gracefully" AC.
 */
export function parseDotenv(contents: string): Record<string, string> {
  const out: Record<string, string> = {}
  const lines = contents.split("\n")
  for (let i = 0; i < lines.length; i++) {
    let line = lines[i]
    if (line.endsWith("\r")) line = line.slice(0, -1)
    const trimmed = line.trim()
    if (trimmed === "" || trimmed.startsWith("#")) continue
    const eq = line.indexOf("=")
    if (eq < 0) {
      throw new GrafanaConfigError(
        `line ${i + 1}: expected KEY=VALUE, got ${JSON.stringify(trimmed.slice(0, 40))}`,
      )
    }
    const key = line.slice(0, eq).trim()
    const value = line.slice(eq + 1)
    if (key === "") {
      throw new GrafanaConfigError(`line ${i + 1}: empty key`)
    }
    out[key] = value
  }
  return out
}

/**
 * Load the bundle at path and return the parsed structure. Never returns a
 * bundle with an empty url or apiKey — the caller can rely on both being
 * non-empty on success. Throws GrafanaConfigError on any failure so the
 * caller can distinguish config errors from MCP-runtime errors.
 */
export function loadGrafanaBundle(path: string): GrafanaBundle {
  debug("loading bundle from", path)
  let raw: string
  try {
    raw = readFileSync(path, "utf8")
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    throw new GrafanaConfigError(`cannot read config at ${path}: ${msg}`)
  }

  let kv: Record<string, string>
  try {
    kv = parseDotenv(raw)
  } catch (err) {
    // parseDotenv throws GrafanaConfigError already; rewrap only if not.
    if (err instanceof GrafanaConfigError) throw err
    const msg = err instanceof Error ? err.message : String(err)
    throw new GrafanaConfigError(`cannot parse config at ${path}: ${msg}`)
  }

  const url = kv["GRAFANA_URL"] ?? ""
  const apiKey = kv["GRAFANA_API_KEY"] ?? ""
  if (url === "") {
    throw new GrafanaConfigError(`config at ${path}: missing GRAFANA_URL`)
  }
  if (apiKey === "") {
    throw new GrafanaConfigError(`config at ${path}: missing GRAFANA_API_KEY`)
  }

  const extraEnv: Record<string, string> = {}
  for (const [k, v] of Object.entries(kv)) {
    if (k === "GRAFANA_URL" || k === "GRAFANA_API_KEY") continue
    extraEnv[k] = v
  }

  return { url, apiKey, extraEnv }
}
