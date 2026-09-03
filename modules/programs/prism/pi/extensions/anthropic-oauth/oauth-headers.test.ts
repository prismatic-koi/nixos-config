// Tests for the OAuth-mode request-shaping helpers ported in v1.5.1
// (griffinmartin PR #207 auth parity, issue #2381).
// Run with: tsx --test oauth-headers.test.ts  (Node 20+, zero new deps)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import {
  getUserAgent,
  getCliVersion,
  getStainlessHeaders,
  buildRequestUrl,
  buildOAuthHeaders,
} from "./oauth-headers.ts"
import { config } from "./model-config.ts"

// ---------------------------------------------------------------------------
// AC4: getUserAgent — sdk-cli fingerprint.
// ---------------------------------------------------------------------------
describe("getUserAgent", () => {
  it("returns claude-cli/<version> (external, sdk-cli) when env is unset", () => {
    const original = process.env.ANTHROPIC_USER_AGENT
    delete process.env.ANTHROPIC_USER_AGENT
    try {
      const ua = getUserAgent()
      assert.equal(ua, `claude-cli/${config.ccVersion} (external, sdk-cli)`)
      // Pinned literal, not just the interpolation: the Anthropic API
      // returns HTTP 400 `claude_code_version_too_old` for Fable 5.1 on
      // subscription auth below 2.1.251 (issue #2918).
      assert.equal(ua, "claude-cli/2.1.257 (external, sdk-cli)")
      assert.ok(ua.includes("sdk-cli"))
      assert.ok(!ua.includes("(external, cli)"))
    } finally {
      if (original !== undefined) {
        process.env.ANTHROPIC_USER_AGENT = original
      }
    }
  })

  it("honours ANTHROPIC_USER_AGENT override", () => {
    const original = process.env.ANTHROPIC_USER_AGENT
    process.env.ANTHROPIC_USER_AGENT = "my-custom-agent/9.9.9"
    try {
      assert.equal(getUserAgent(), "my-custom-agent/9.9.9")
    } finally {
      if (original === undefined) {
        delete process.env.ANTHROPIC_USER_AGENT
      } else {
        process.env.ANTHROPIC_USER_AGENT = original
      }
    }
  })

  // transforms.ts already read this env var for the billing header. The
  // user-agent must resolve the version the same way, or one request reports
  // two different CLI versions.
  it("honours ANTHROPIC_CLI_VERSION in the user-agent", () => {
    const savedUA = process.env.ANTHROPIC_USER_AGENT
    const savedVersion = process.env.ANTHROPIC_CLI_VERSION
    delete process.env.ANTHROPIC_USER_AGENT
    process.env.ANTHROPIC_CLI_VERSION = "9.9.9"
    try {
      assert.equal(getCliVersion(), "9.9.9")
      assert.equal(getUserAgent(), "claude-cli/9.9.9 (external, sdk-cli)")
    } finally {
      if (savedUA !== undefined) process.env.ANTHROPIC_USER_AGENT = savedUA
      if (savedVersion === undefined) delete process.env.ANTHROPIC_CLI_VERSION
      else process.env.ANTHROPIC_CLI_VERSION = savedVersion
    }
  })

  it("falls back to config.ccVersion when ANTHROPIC_CLI_VERSION is unset", () => {
    const saved = process.env.ANTHROPIC_CLI_VERSION
    delete process.env.ANTHROPIC_CLI_VERSION
    try {
      assert.equal(getCliVersion(), config.ccVersion)
    } finally {
      if (saved !== undefined) process.env.ANTHROPIC_CLI_VERSION = saved
    }
  })
})

// ---------------------------------------------------------------------------
// AC6: getStainlessHeaders — all 8 keys present, values probed at call time.
// ---------------------------------------------------------------------------
describe("getStainlessHeaders", () => {
  it("returns all eight x-stainless-* keys", () => {
    const headers = getStainlessHeaders()
    const expectedKeys = [
      "x-stainless-arch",
      "x-stainless-lang",
      "x-stainless-os",
      "x-stainless-package-version",
      "x-stainless-retry-count",
      "x-stainless-runtime",
      "x-stainless-runtime-version",
      "x-stainless-timeout",
    ]
    for (const key of expectedKeys) {
      assert.ok(key in headers, `missing key ${key}`)
      assert.equal(typeof headers[key], "string")
      assert.ok(headers[key].length > 0, `empty value for ${key}`)
    }
    assert.equal(
      Object.keys(headers).length,
      expectedKeys.length,
      "unexpected extra stainless keys",
    )
  })

  it("sets fixed literal values from the griffinmartin mirror", () => {
    const headers = getStainlessHeaders()
    assert.equal(headers["x-stainless-lang"], "js")
    assert.equal(headers["x-stainless-package-version"], "0.81.0")
    assert.equal(headers["x-stainless-retry-count"], "0")
    assert.equal(headers["x-stainless-runtime"], "node")
    assert.equal(headers["x-stainless-timeout"], "600")
  })

  it("probes runtime values at call time (process.version)", () => {
    // The runtime-version value must reflect the live process.version at
    // call time — freezing it to a module-load constant would drift over
    // node upgrades.
    const headers = getStainlessHeaders()
    assert.equal(headers["x-stainless-runtime-version"], process.version)
  })
})

// ---------------------------------------------------------------------------
// AC7: buildRequestUrl — appends ?beta=true to /v1/messages only.
// ---------------------------------------------------------------------------
describe("buildRequestUrl", () => {
  it("appends ?beta=true to /v1/messages", () => {
    const url = buildRequestUrl("https://api.anthropic.com/v1/messages")
    assert.equal(url, "https://api.anthropic.com/v1/messages?beta=true")
  })

  it("preserves an existing beta query parameter (no duplicate)", () => {
    const url = buildRequestUrl(
      "https://api.anthropic.com/v1/messages?beta=false",
    )
    // Existing beta=false is preserved (not overwritten) — mirrors
    // griffinmartin's `!url.searchParams.has("beta")` guard.
    assert.equal(url, "https://api.anthropic.com/v1/messages?beta=false")
  })

  it("does NOT modify non-/v1/messages URLs", () => {
    const url = buildRequestUrl("https://api.anthropic.com/v1/models")
    assert.equal(url, "https://api.anthropic.com/v1/models")
    const complete = buildRequestUrl(
      "https://api.anthropic.com/v1/messages/count_tokens",
    )
    assert.equal(
      complete,
      "https://api.anthropic.com/v1/messages/count_tokens",
    )
  })

  it("preserves other query parameters when appending beta", () => {
    const url = buildRequestUrl(
      "https://api.anthropic.com/v1/messages?stream=true",
    )
    const parsed = new URL(url)
    assert.equal(parsed.searchParams.get("stream"), "true")
    assert.equal(parsed.searchParams.get("beta"), "true")
  })
})

// ---------------------------------------------------------------------------
// AC5 + AC6: buildOAuthHeaders — the OAuth-mode header block that gets sent
// on the outgoing /v1/messages request. Mirrors griffinmartin's
// buildRequestHeaders behaviour.
// ---------------------------------------------------------------------------
describe("buildOAuthHeaders", () => {
  const makeModel = () => ({
    id: "claude-sonnet-4-5",
    compat: { forceAdaptiveThinking: false },
  })

  it("sets anthropic-dangerous-direct-browser-access: true (AC5)", () => {
    const headers = buildOAuthHeaders(makeModel(), "token-xyz")
    assert.equal(
      headers.get("anthropic-dangerous-direct-browser-access"),
      "true",
    )
  })

  it("sets all eight x-stainless-* headers on the request (AC6)", () => {
    const headers = buildOAuthHeaders(makeModel(), "token-xyz")
    const stainlessKeys = [
      "x-stainless-arch",
      "x-stainless-lang",
      "x-stainless-os",
      "x-stainless-package-version",
      "x-stainless-retry-count",
      "x-stainless-runtime",
      "x-stainless-runtime-version",
      "x-stainless-timeout",
    ]
    for (const key of stainlessKeys) {
      const value = headers.get(key)
      assert.ok(value !== null, `missing header ${key}`)
      assert.ok((value ?? "").length > 0, `empty header ${key}`)
    }
  })

  it("sets bearer token, anthropic-version, anthropic-beta, x-app, user-agent, x-client-request-id", () => {
    const headers = buildOAuthHeaders(makeModel(), "token-xyz")
    assert.equal(headers.get("authorization"), "Bearer token-xyz")
    assert.equal(headers.get("anthropic-version"), "2023-06-01")
    assert.ok((headers.get("anthropic-beta") ?? "").length > 0)
    assert.equal(headers.get("x-app"), "cli")
    assert.ok((headers.get("user-agent") ?? "").includes("sdk-cli"))
    assert.ok((headers.get("x-client-request-id") ?? "").length > 0)
  })

  it("lets options.headers override our stainless defaults (AC6 override behaviour)", () => {
    const headers = buildOAuthHeaders(makeModel(), "token-xyz", {
      "x-stainless-runtime": "custom-runtime",
    })
    assert.equal(headers.get("x-stainless-runtime"), "custom-runtime")
    // Other stainless headers remain at the default
    assert.equal(headers.get("x-stainless-lang"), "js")
  })

  it("filters x-api-key from options.headers on the OAuth path", () => {
    const headers = buildOAuthHeaders(makeModel(), "token-xyz", {
      "x-api-key": "should-not-appear",
      "x-custom": "keep-me",
    })
    assert.equal(headers.get("x-api-key"), null)
    assert.equal(headers.get("x-custom"), "keep-me")
  })

  it("sets X-Claude-Code-Session-Id, stable across requests", () => {
    const first = buildOAuthHeaders(makeModel(), "token-xyz")
    const second = buildOAuthHeaders(makeModel(), "token-xyz")
    const id = first.get("x-claude-code-session-id")
    assert.ok(id !== null && id.length > 0, "session id must be sent")
    assert.equal(
      second.get("x-claude-code-session-id"),
      id,
      "one id per process, not per request",
    )
  })

  it("keeps x-client-request-id per-request, unlike the session id", () => {
    const first = buildOAuthHeaders(makeModel(), "token-xyz")
    const second = buildOAuthHeaders(makeModel(), "token-xyz")
    assert.notEqual(
      first.get("x-client-request-id"),
      second.get("x-client-request-id"),
    )
  })

  it("unions a caller's anthropic-beta with the model betas", () => {
    const plain = buildOAuthHeaders(makeModel(), "token-xyz")
    const ours = (plain.get("anthropic-beta") ?? "").split(",")
    const merged = buildOAuthHeaders(makeModel(), "token-xyz", {
      "anthropic-beta": "caller-beta-1",
    })
    const got = (merged.get("anthropic-beta") ?? "").split(",")
    assert.ok(got.includes("caller-beta-1"), "caller beta must survive")
    for (const beta of ours) {
      assert.ok(got.includes(beta), `OAuth beta ${beta} must not be dropped`)
    }
  })

  it("unions on a differently-cased anthropic-beta key and de-duplicates", () => {
    const merged = buildOAuthHeaders(makeModel(), "token-xyz", {
      "Anthropic-Beta": "oauth-2025-04-20, caller-beta-2",
    })
    const got = (merged.get("anthropic-beta") ?? "").split(",")
    assert.ok(got.includes("caller-beta-2"))
    assert.equal(
      got.filter((b) => b === "oauth-2025-04-20").length,
      1,
      "a beta the caller repeats must appear once",
    )
  })
})
