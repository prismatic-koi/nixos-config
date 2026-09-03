// OAuth-mode request-shaping helpers (headers + URL) extracted from index.ts
// so the tests in oauth-headers.test.ts can exercise them without pulling in
// pi runtime packages (`@earendil-works/pi-ai`,
// `@earendil-works/pi-coding-agent`).
//
// Mirrors the equivalent helpers in griffinmartin/opencode-claude-auth
// src/index.ts:
//   - getUserAgent
//   - getStainlessHeaders
//   - buildRequestUrl
//   - buildRequestHeaders (renamed here to buildOAuthHeaders because our
//     API-key branch is handled separately in index.ts's streamSimple closure)
//
// Ported in issue #2381 (v1.5.1 auth parity: griffinmartin PR #207).

import { getExcludedBetas, getModelBetas } from "./betas.ts"
import { config } from "./model-config.ts"

// One id per process, as upstream does. The API sees a run of requests from
// one CLI session; a fresh id per request would not.
const sessionId = crypto.randomUUID()

// transforms.ts reads $ANTHROPIC_CLI_VERSION for the billing header. Both
// callers must resolve the version the same way, or the two report different
// versions on one request.
export function getCliVersion(): string {
  return process.env.ANTHROPIC_CLI_VERSION ?? config.ccVersion
}

export function getUserAgent(): string {
  return (
    process.env.ANTHROPIC_USER_AGENT ??
    `claude-cli/${getCliVersion()} (external, sdk-cli)`
  )
}

// Mirror of griffinmartin/opencode-claude-auth src/index.ts::getStainlessHeaders.
// The Anthropic API's Cloudflare WAF fingerprints these fields on Claude Code
// subscription auth requests. Values are probed at request time via
// `process.arch` / `process.platform` / `process.version` — do NOT hoist to
// module-level constants, that would freeze the wire form at extension-load
// time.
export function getStainlessHeaders(): Record<string, string> {
  return {
    "x-stainless-arch": process.arch === "arm64" ? "arm64" : process.arch,
    "x-stainless-lang": "js",
    "x-stainless-os":
      process.platform === "darwin" ? "MacOS" : process.platform,
    "x-stainless-package-version": "0.81.0",
    "x-stainless-retry-count": "0",
    "x-stainless-runtime": "node",
    "x-stainless-runtime-version": process.version,
    "x-stainless-timeout": "600",
  }
}

// Mirror of griffinmartin/opencode-claude-auth src/index.ts::buildRequestUrl.
// The `?beta=true` query parameter is required on `/v1/messages` requests for
// Claude Code 2.1.112 subscription auth (PR #207). Restricted to `/v1/messages`
// so future non-messages endpoints (e.g. `/v1/messages/count_tokens`) are not
// silently rewritten.
export function buildRequestUrl(input: string): string {
  const url = new URL(input)
  if (url.pathname === "/v1/messages" && !url.searchParams.has("beta")) {
    url.searchParams.set("beta", "true")
  }
  return url.toString()
}

// Mirror of griffinmartin/opencode-claude-auth src/index.ts::buildRequestHeaders
// for the OAuth-mode branch. Extracted so tests can assert on the header block
// directly.
//
// Merge order: our OAuth-mode headers are set first, then `optionsHeaders` are
// merged in, so a caller-supplied `x-stainless-runtime` (or any other header)
// overrides our default — mirroring griffinmartin's `!headers.has(key)` guard
// semantics. `x-api-key` is filtered from `optionsHeaders` because bearer-token
// auth is authoritative on the OAuth path.
//
// `anthropic-beta` is the exception to that override: a caller's value is
// unioned with the model betas rather than replacing them, so a caller asking
// for one extra beta cannot drop the OAuth betas and break auth.
export function buildOAuthHeaders(
  model: {
    id: string
    compat?: { forceAdaptiveThinking?: boolean } & Record<string, unknown>
  },
  token: string,
  optionsHeaders?: Record<string, string>,
): Headers {
  const headers = new Headers()
  const excluded = getExcludedBetas(model.id)
  const betas = getModelBetas(model.id, excluded, {
    forceAdaptiveThinking: model.compat?.forceAdaptiveThinking === true,
  })
  const incomingBeta = optionsHeaders
    ? (Object.entries(optionsHeaders).find(
        ([key]) => key.toLowerCase() === "anthropic-beta",
      )?.[1] ?? "")
    : ""
  const mergedBetas = [
    ...new Set([
      ...betas,
      ...incomingBeta
        .split(",")
        .map((item) => item.trim())
        .filter(Boolean),
    ]),
  ]

  headers.set("authorization", `Bearer ${token}`)
  headers.set("anthropic-version", "2023-06-01")
  headers.set("anthropic-beta", mergedBetas.join(","))
  headers.set("anthropic-dangerous-direct-browser-access", "true")
  headers.set("x-app", "cli")
  headers.set("user-agent", getUserAgent())
  headers.set("x-client-request-id", crypto.randomUUID())
  headers.set("X-Claude-Code-Session-Id", sessionId)
  for (const [key, value] of Object.entries(getStainlessHeaders())) {
    headers.set(key, value)
  }
  if (optionsHeaders) {
    for (const [key, value] of Object.entries(optionsHeaders)) {
      const lower = key.toLowerCase()
      if (lower === "x-api-key" || lower === "anthropic-beta") continue
      headers.set(key, value)
    }
  }
  return headers
}
