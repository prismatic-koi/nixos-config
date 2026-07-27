// Unit tests for retry.ts — auth-error-reload-retry shell with
// invalid_grant treated as terminal.
//
// Run with: tsx --test retry.test.ts (from this directory)

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import { InvalidGrantError } from "./auth.ts"
import {
  callToolWithAuthRetry,
  isAuthErrorMessage,
  isAuthErrorResult,
  type AuthCallbacks,
  type McpCallResult,
  type McpSessionLike,
} from "./retry.ts"

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

interface ScriptedSession extends McpSessionLike {
  callCount: number
  calls: Array<{ name: string; args: Record<string, unknown> }>
}

interface StubAuth extends AuthCallbacks {
  refreshCalls: number
  invalidateCalls: number
  onRefresh: () => Promise<void>
}

function makeSession(
  responses: Array<McpCallResult | Error>,
): ScriptedSession {
  const s: ScriptedSession = {
    callCount: 0,
    calls: [],
    async callTool(name, args) {
      s.calls.push({ name, args })
      const idx = s.callCount++
      const next = responses[idx]
      if (next === undefined) {
        throw new Error(`ScriptedSession: no response for call #${idx + 1}`)
      }
      if (next instanceof Error) throw next
      return next
    },
  }
  return s
}

function makeStubAuth(
  onRefresh: () => Promise<void> = async () => {},
): StubAuth {
  const a: StubAuth = {
    refreshCalls: 0,
    invalidateCalls: 0,
    onRefresh,
    async refresh() {
      a.refreshCalls++
      await a.onRefresh()
    },
    invalidate() {
      a.invalidateCalls++
    },
  }
  return a
}

const OK_RESULT: McpCallResult = {
  content: [{ type: "text", text: "ok" }],
}

function authErrorResult(text: string): McpCallResult {
  return {
    content: [{ type: "text", text }],
    isError: true,
  }
}

// ---------------------------------------------------------------------------
// isAuthErrorMessage / isAuthErrorResult
// ---------------------------------------------------------------------------

describe("isAuthErrorMessage", () => {
  it("matches HTTP 401 / Unauthorized / Authentication failed (case-insensitive)", () => {
    assert.equal(isAuthErrorMessage("MCP HTTP 401: Unauthorized"), true)
    assert.equal(isAuthErrorMessage("401 Unauthorized"), true)
    assert.equal(isAuthErrorMessage("unauthorized"), true)
    assert.equal(isAuthErrorMessage("Authentication failed"), true)
  })

  it("does not match embedded numeric substrings containing 401", () => {
    assert.equal(isAuthErrorMessage("PROJ-4010"), false)
    assert.equal(isAuthErrorMessage("id=54012"), false)
  })

  it("returns false for empty / unrelated text", () => {
    assert.equal(isAuthErrorMessage(""), false)
    assert.equal(isAuthErrorMessage("HTTP 500 Internal Server Error"), false)
    assert.equal(isAuthErrorMessage("HTTP 403 Forbidden"), false)
  })
})

describe("isAuthErrorResult", () => {
  it("is false when isError is undefined even if text mentions 401", () => {
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "The 401 error handler was updated" }],
      }),
      false,
    )
  })

  it("is true when isError is true AND text contains an auth marker", () => {
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "Authentication failed" }],
        isError: true,
      }),
      true,
    )
  })
})

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — happy path", () => {
  it("makes one tool call when the first attempt succeeds", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 1)
    assert.equal(auth.refreshCalls, 1)
    assert.equal(auth.invalidateCalls, 0)
  })

  it("does not retry on non-auth isError results", async () => {
    const session = makeSession([
      {
        content: [{ type: "text", text: "page not found" }],
        isError: true,
      },
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.isError, true)
    assert.equal(session.callCount, 1)
    assert.equal(auth.invalidateCalls, 0)
  })
})

// ---------------------------------------------------------------------------
// HTTP-transport 401 and tool-payload auth error retry
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — retryable auth errors", () => {
  it("invalidates + refreshes + retries once on a thrown 'MCP HTTP 401'", async () => {
    const session = makeSession([
      new Error("MCP HTTP 401: Unauthorized"),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 2)
    assert.equal(auth.refreshCalls, 2)
    assert.equal(auth.invalidateCalls, 1)
  })

  it("retries once on a tool-payload 401 isError result", async () => {
    const session = makeSession([
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 2)
    assert.equal(auth.invalidateCalls, 1)
  })

  it("surfaces the second auth error without a third call", async () => {
    const session = makeSession([
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.isError, true)
    assert.equal(session.callCount, 2, "must not call a third time")
  })
})

// ---------------------------------------------------------------------------
// invalid_grant is TERMINAL — no retry, no fetch
//
// Revert-and-watch-fail: if the InvalidGrantError branch is removed
// from callToolWithAuthRetry, the "no tool call" and "no retry"
// assertions in this block fail.
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — invalid_grant is terminal", () => {
  it("initial refresh throwing InvalidGrantError short-circuits — no tool call is made", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw new InvalidGrantError("grant revoked upstream")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.isError, true)
    assert.match(
      result.content[0].text ?? "",
      /invalid_grant|revoked|login-notion/i,
    )
    assert.equal(
      session.callCount,
      0,
      "tool call must not be attempted after InvalidGrantError on initial refresh",
    )
    assert.equal(auth.refreshCalls, 1)
  })

  it("retry-time refresh throwing InvalidGrantError does NOT trigger a second tool call", async () => {
    let refreshInvocations = 0
    const session = makeSession([
      // First tool call returns an auth-error → triggers retry path.
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
      // If we (incorrectly) issue a second call, the harness will hand
      // out OK_RESULT — the assertion below catches that and fails.
      OK_RESULT,
    ])
    const auth = makeStubAuth(async () => {
      refreshInvocations++
      // Second refresh (post-invalidate) throws InvalidGrantError.
      if (refreshInvocations === 2) {
        throw new InvalidGrantError("grant revoked during retry-refresh")
      }
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.isError, true)
    assert.match(
      result.content[0].text ?? "",
      /invalid_grant|revoked|login-notion/i,
    )
    assert.equal(
      session.callCount,
      1,
      "second tool call must not happen when refresh returned InvalidGrantError",
    )
    assert.equal(auth.invalidateCalls, 1)
    assert.equal(auth.refreshCalls, 2)
  })
})

// ---------------------------------------------------------------------------
// Refresh failure paths (non-terminal)
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — non-terminal refresh failure", () => {
  it("returns an error result when the initial refresh throws a plain Error", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw new Error("Notion MCP: no auth tokens")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /no auth tokens/)
    assert.equal(session.callCount, 0)
  })

  it("surfaces original auth error when retry-time refresh throws non-terminally", async () => {
    let refreshInvocations = 0
    const session = makeSession([new Error("MCP HTTP 401: Unauthorized")])
    const auth = makeStubAuth(async () => {
      refreshInvocations++
      if (refreshInvocations === 2) {
        throw new Error("network temporarily down")
      }
    })

    await assert.rejects(
      () => callToolWithAuthRetry(session, auth, "notion-fetch", {}),
      /MCP HTTP 401/,
    )
    assert.equal(session.callCount, 1)
  })
})

// ---------------------------------------------------------------------------
// Argument passthrough
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — argument passthrough", () => {
  it("forwards the tool name and args unchanged on every attempt", async () => {
    const session = makeSession([
      new Error("MCP HTTP 401: Unauthorized"),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    await callToolWithAuthRetry(session, auth, "notion-search", {
      query: "test",
      limit: 10,
    })

    assert.equal(session.calls.length, 2)
    for (const call of session.calls) {
      assert.equal(call.name, "notion-search")
      assert.deepEqual(call.args, { query: "test", limit: 10 })
    }
  })
})
