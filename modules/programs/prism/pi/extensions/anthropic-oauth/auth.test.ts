// Unit tests for auth.ts OAuth wire format.
// Run with: node --test auth.test.ts  (Node 20+, zero new deps)
//
// These tests verify that loginAnthropic and refreshAnthropicToken send
// requests to the correct URL with the correct headers and form-encoded body.
// fetch is mocked via globalThis so no network calls are made.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

// ---------------------------------------------------------------------------
// We need to import after mocking fetch. We'll use a dynamic import inside
// each test block, but since the module is cached after the first import we
// mock fetch BEFORE any import of auth.ts.
// ---------------------------------------------------------------------------

// Helper: build a minimal successful OAuth token response
function makeOkResponse(body: object): Response {
  return {
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
    headers: new Headers(),
  } as unknown as Response
}

// Helper: parse URLSearchParams from a string body
function parseBody(body: unknown): Record<string, string> {
  assert.equal(typeof body, "string", "body should be a string")
  const params = new URLSearchParams(body as string)
  const result: Record<string, string> = {}
  for (const [k, v] of params) {
    result[k] = v
  }
  return result
}

// ---------------------------------------------------------------------------
// Tests for refreshAnthropicToken
// ---------------------------------------------------------------------------
describe("refreshAnthropicToken", () => {
  it("POSTs to the correct TOKEN_URL with form-urlencoded body and correct headers", async () => {
    const capturedCalls: { url: string; init: RequestInit }[] = []

    // Mock fetch before importing the module under test
    const fakeFetch = async (url: string, init: RequestInit) => {
      capturedCalls.push({ url, init })
      return makeOkResponse({
        access_token: "new-access-token",
        refresh_token: "new-refresh-token",
        expires_in: 3600,
      })
    }
    globalThis.fetch = fakeFetch as unknown as typeof fetch

    // Dynamic import to pick up the mock (module will be cached after first call)
    const { refreshAnthropicToken, USER_AGENT } = await import("./auth.ts")

    const creds = {
      access: "old-access",
      refresh: "old-refresh",
      expires: Date.now() + 60_000,
    }

    await refreshAnthropicToken(creds)

    assert.equal(capturedCalls.length, 1, "fetch should be called exactly once")

    const { url, init } = capturedCalls[0]

    // AC: TOKEN_URL
    assert.equal(url, "https://claude.ai/v1/oauth/token")

    // AC: Content-Type header
    const headers = new Headers(init.headers as HeadersInit)
    assert.equal(
      headers.get("content-type"),
      "application/x-www-form-urlencoded",
    )

    // AC: User-Agent header equals USER_AGENT constant
    assert.equal(headers.get("user-agent"), USER_AGENT)
    assert.equal(USER_AGENT, "claude-code/2.1.97")

    // AC: form-encoded body with correct fields
    const body = parseBody(init.body)
    assert.equal(body.grant_type, "refresh_token")
    assert.ok("client_id" in body, "body should contain client_id")
    assert.equal(body.refresh_token, "old-refresh")
    // Verify it is NOT JSON
    assert.doesNotMatch(
      init.body as string,
      /^\s*\{/,
      "body must not be JSON",
    )
  })
})

// ---------------------------------------------------------------------------
// Tests for loginAnthropic (token exchange)
// ---------------------------------------------------------------------------
describe("loginAnthropic", () => {
  it("POSTs to the correct TOKEN_URL with form-urlencoded body and correct headers", async () => {
    const capturedCalls: { url: string; init: RequestInit }[] = []

    const fakeFetch = async (url: string, init: RequestInit) => {
      capturedCalls.push({ url, init })
      return makeOkResponse({
        access_token: "new-access-token",
        refresh_token: "new-refresh-token",
        expires_in: 3600,
      })
    }
    globalThis.fetch = fakeFetch as unknown as typeof fetch

    const { loginAnthropic, USER_AGENT } = await import("./auth.ts")

    // Build callbacks that drive the manual-input fallback path.
    // We do NOT provide onManualCodeInput, so the flow goes:
    //   createLocalAuthorization → (will fail or timeout but we provide onPrompt)
    //   → onPrompt returns "mycode#mystate"
    // However, createLocalAuthorization tries to bind a real TCP port, which
    // can succeed or fail. We want a reliable test, so we use the fact that
    // loginAnthropic catches any error from createLocalAuthorization and falls
    // through to onPrompt. We trigger this by providing a signal that is already
    // aborted — but that only affects the fetch call, not the server.
    //
    // Simplest reliable approach: provide onManualCodeInput that immediately
    // returns a code#state string. The local server and manual input race;
    // whichever arrives first wins. We make manualInput win by resolving
    // immediately.

    let capturedState: string | undefined

    const callbacks = {
      onAuth: (opts: { url: string }) => {
        // Extract state from the authorize URL so we can echo it back
        const u = new URL(opts.url)
        capturedState = u.searchParams.get("state") ?? undefined
      },
      onManualCodeInput: async () => {
        // Wait a tick to let the local server start, then return immediately
        await new Promise((r) => setTimeout(r, 10))
        return `fakecode#${capturedState ?? "fakestate"}`
      },
      onPrompt: async (_opts: { message: string }) => {
        return `fakecode#${capturedState ?? "fakestate"}`
      },
    }

    await loginAnthropic(callbacks)

    // Find the token-exchange fetch call (url matches TOKEN_URL)
    const tokenCall = capturedCalls.find(
      (c) => c.url === "https://claude.ai/v1/oauth/token",
    )
    assert.ok(tokenCall, "fetch should have been called with the TOKEN_URL")

    const { init } = tokenCall

    // AC: TOKEN_URL
    assert.equal(tokenCall.url, "https://claude.ai/v1/oauth/token")

    // AC: Content-Type
    const headers = new Headers(init.headers as HeadersInit)
    assert.equal(
      headers.get("content-type"),
      "application/x-www-form-urlencoded",
    )

    // AC: User-Agent
    assert.equal(headers.get("user-agent"), USER_AGENT)
    assert.equal(USER_AGENT, "claude-code/2.1.97")

    // AC: form-encoded body with correct fields
    const body = parseBody(init.body)
    assert.equal(body.grant_type, "authorization_code")
    assert.ok("client_id" in body, "body should contain client_id")
    assert.ok("code" in body, "body should contain code")
    assert.ok("state" in body, "body should contain state")
    assert.ok("redirect_uri" in body, "body should contain redirect_uri")
    assert.ok("code_verifier" in body, "body should contain code_verifier")

    // Verify it is NOT JSON
    assert.doesNotMatch(
      init.body as string,
      /^\s*\{/,
      "body must not be JSON",
    )
  })
})
