// Unit tests for retry.ts — auth-error-reload-retry shell (#2389).
//
// Run with: tsx --test retry.test.ts (from this directory)
// Or:       cd modules/programs/prism/pi/extensions/atlassian && tsx --test retry.test.ts
//
// The retry shell is dependency-injected (see AuthCallbacks), so these tests
// stub the auth side entirely and drive the McpSessionLike interface with a
// small in-memory session that records call counts and returns scripted
// responses. No real tokens, no real fetch.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

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
// isAuthErrorMessage
// ---------------------------------------------------------------------------

describe("isAuthErrorMessage", () => {
  it("matches 'MCP HTTP 401: ...'", () => {
    assert.equal(isAuthErrorMessage("MCP HTTP 401: Unauthorized"), true)
  })

  it("matches '401 Unauthorized'", () => {
    assert.equal(isAuthErrorMessage("401 Unauthorized"), true)
  })

  it("matches 'Unauthorized' on its own (case-insensitive)", () => {
    assert.equal(isAuthErrorMessage("unauthorized"), true)
  })

  it("matches 'Authentication failed' (case-insensitive)", () => {
    assert.equal(isAuthErrorMessage("Authentication failed: token expired"), true)
    assert.equal(isAuthErrorMessage("authentication failed"), true)
  })

  it("does not match unrelated numeric strings containing '401' embedded in longer tokens", () => {
    // Word-boundary regex ensures issue key "PROJ-4010" or "id=54012" does not
    // match. \b treats digits as word chars — "4010" boundary is on '4' and
    // after '0', which does NOT match \b401\b because 401 is not a full word.
    assert.equal(isAuthErrorMessage("PROJ-4010"), false)
    assert.equal(isAuthErrorMessage("id=54012"), false)
  })

  it("returns false for empty or unrelated text", () => {
    assert.equal(isAuthErrorMessage(""), false)
    assert.equal(isAuthErrorMessage("MCP tool call succeeded"), false)
    assert.equal(isAuthErrorMessage("HTTP 500 Internal Server Error"), false)
    assert.equal(isAuthErrorMessage("HTTP 403 Forbidden"), false)
  })
})

// ---------------------------------------------------------------------------
// isAuthErrorResult
// ---------------------------------------------------------------------------

describe("isAuthErrorResult", () => {
  it("is false for successful (isError undefined/false) results even if text mentions 401", () => {
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "The 401 error handler was updated" }],
      }),
      false,
    )
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "Unauthorized" }],
        isError: false,
      }),
      false,
    )
  })

  it("is true for isError results containing auth-error text", () => {
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "Authentication failed" }],
        isError: true,
      }),
      true,
    )
    assert.equal(
      isAuthErrorResult({
        content: [
          { type: "text", text: "line 1" },
          { type: "text", text: "code: 401 Unauthorized" },
        ],
        isError: true,
      }),
      true,
    )
  })

  it("is false for isError results without auth-error text", () => {
    assert.equal(
      isAuthErrorResult({
        content: [{ type: "text", text: "Rate limited (429)" }],
        isError: true,
      }),
      false,
    )
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — happy path
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — happy path", () => {
  it("makes one call and returns the result when the first attempt succeeds", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 1)
    assert.equal(auth.refreshCalls, 1)
    assert.equal(auth.invalidateCalls, 0)
  })

  it("does not retry on non-auth isError results", async () => {
    const session = makeSession([
      {
        content: [{ type: "text", text: "issue not found" }],
        isError: true,
      },
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.isError, true)
    assert.equal(session.callCount, 1)
    assert.equal(auth.invalidateCalls, 0)
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — HTTP-transport 401 (thrown)
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — HTTP-transport 401", () => {
  it("invalidates + refreshes + retries once on a thrown 'MCP HTTP 401'", async () => {
    const session = makeSession([
      new Error("MCP HTTP 401: Unauthorized"),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 2)
    assert.equal(auth.refreshCalls, 2, "refresh must be called before both attempts")
    assert.equal(
      auth.invalidateCalls,
      1,
      "cache must be invalidated between the first and second attempt",
    )
  })

  it("re-throws when a non-auth error is thrown on the first attempt", async () => {
    const session = makeSession([new Error("MCP HTTP 500: server error")])
    const auth = makeStubAuth()

    await assert.rejects(
      () => callToolWithAuthRetry(session, auth, "tool", {}),
      /HTTP 500/,
    )
    assert.equal(session.callCount, 1)
    assert.equal(auth.invalidateCalls, 0, "non-auth errors must not invalidate")
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — tool-payload auth error
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — tool-payload auth error", () => {
  it("invalidates + refreshes + retries once when the isError result mentions 401", async () => {
    const session = makeSession([
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 2)
    assert.equal(auth.invalidateCalls, 1)
  })

  it("invalidates + refreshes + retries once when the isError result mentions 'Authentication failed'", async () => {
    const session = makeSession([
      authErrorResult(
        'Authentication failed: {"code":401,"message":"Unauthorized"}',
      ),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.content[0].text, "ok")
    assert.equal(session.callCount, 2)
    assert.equal(auth.invalidateCalls, 1)
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — retry bounded to one
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — retry bounded to one", () => {
  it("surfaces the second auth-error result without a third call", async () => {
    const session = makeSession([
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /401|Unauthorized/i)
    assert.equal(session.callCount, 2, "must not call a third time")
    assert.equal(auth.invalidateCalls, 1, "invalidate only fires once between attempts")
  })

  it("re-throws the second thrown auth error without a third call", async () => {
    const session = makeSession([
      new Error("MCP HTTP 401: Unauthorized"),
      new Error("MCP HTTP 401: Unauthorized"),
    ])
    const auth = makeStubAuth()

    await assert.rejects(
      () => callToolWithAuthRetry(session, auth, "tool", {}),
      /MCP HTTP 401/,
    )
    assert.equal(session.callCount, 2, "must not call a third time")
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — refresh failure paths
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — refresh failure paths", () => {
  it("returns an error-shaped result when the initial refresh throws", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw new Error("Atlassian MCP: no auth tokens")
    })

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /no auth tokens/)
    assert.equal(
      session.callCount,
      0,
      "tool call must not be attempted when the initial refresh failed",
    )
  })

  it("surfaces the original auth error when the reload+refresh between attempts fails", async () => {
    // First refresh (initial) succeeds. Second refresh (post-invalidate) throws.
    let refreshInvocations = 0
    const session = makeSession([new Error("MCP HTTP 401: Unauthorized")])
    const auth = makeStubAuth(async () => {
      refreshInvocations++
      if (refreshInvocations === 2) {
        throw new Error("Atlassian MCP: no auth tokens")
      }
    })

    await assert.rejects(
      () => callToolWithAuthRetry(session, auth, "tool", {}),
      /MCP HTTP 401/,
    )
    assert.equal(session.callCount, 1, "no retry when reload/refresh failed")
    assert.equal(auth.invalidateCalls, 1)
    assert.equal(auth.refreshCalls, 2, "both refresh attempts happened")
  })

  it("surfaces the original isError payload when reload+refresh fails after a tool-payload auth error", async () => {
    let refreshInvocations = 0
    const session = makeSession([
      authErrorResult('{"code":401,"message":"Unauthorized"}'),
    ])
    const auth = makeStubAuth(async () => {
      refreshInvocations++
      if (refreshInvocations === 2) {
        throw new Error("Atlassian MCP: no auth tokens")
      }
    })

    const result = await callToolWithAuthRetry(session, auth, "tool", {})
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /401|Unauthorized/i)
    assert.equal(session.callCount, 1)
  })
})

// ---------------------------------------------------------------------------
// callToolWithAuthRetry — argument passthrough
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry — argument passthrough", () => {
  it("forwards the tool name and args unchanged on every attempt", async () => {
    const session = makeSession([
      new Error("MCP HTTP 401: Unauthorized"),
      OK_RESULT,
    ])
    const auth = makeStubAuth()

    await callToolWithAuthRetry(session, auth, "getJiraIssue", {
      issueIdOrKey: "PROJ-1",
      cloudId: "abc",
    })

    assert.equal(session.calls.length, 2)
    for (const call of session.calls) {
      assert.equal(call.name, "getJiraIssue")
      assert.deepEqual(call.args, { issueIdOrKey: "PROJ-1", cloudId: "abc" })
    }
  })
})
