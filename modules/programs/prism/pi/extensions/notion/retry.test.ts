// Unit tests for notion/retry.ts — the auth-error-reload-retry shell.
//
// Run with: tsx --test retry.test.ts (from this directory)
//
// The shell is dependency-injected (see AuthCallbacks), so these tests stub
// the auth side entirely and drive McpSessionLike with a small in-memory
// session that records call counts and returns scripted responses. No real
// tokens, no real fetch, no filesystem.
//
// The Notion-specific behaviour under test — and the one that does not exist
// in atlassian/retry.ts — is that a TERMINAL auth error short-circuits
// everything. Revert-and-watch-fail: delete the two `isTerminalAuthError(err)`
// branches in callToolWithAuthRetry and the whole "terminal auth errors"
// describe block fails, because the shell falls through to the generic
// handling and (in the second case) makes another tool call.

import { describe, it } from "node:test"
import assert from "node:assert/strict"

import {
  callToolWithAuthRetry,
  isAuthErrorMessage,
  isAuthErrorResult,
  isTerminalAuthError,
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
  onRefresh: (attempt: number) => Promise<void>
}

function makeSession(responses: Array<McpCallResult | Error>): ScriptedSession {
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

function makeStubAuth(onRefresh: (attempt: number) => Promise<void> = async () => {}): StubAuth {
  const a: StubAuth = {
    refreshCalls: 0,
    invalidateCalls: 0,
    onRefresh,
    async refresh() {
      a.refreshCalls++
      await a.onRefresh(a.refreshCalls)
    },
    invalidate() {
      a.invalidateCalls++
    },
  }
  return a
}

/** A terminal error shaped exactly like auth.ts's NotionAuthTerminalError. */
function terminalError(message: string): Error {
  const err = new Error(message)
  ;(err as Error & { terminal: boolean }).terminal = true
  return err
}

const OK_RESULT: McpCallResult = { content: [{ type: "text", text: "ok" }] }

function authErrorResult(text: string): McpCallResult {
  return { content: [{ type: "text", text }], isError: true }
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

describe("isTerminalAuthError", () => {
  it("recognises an error carrying terminal = true", () => {
    assert.equal(isTerminalAuthError(terminalError("dead grant")), true)
  })

  it("does not treat a plain Error as terminal", () => {
    assert.equal(isTerminalAuthError(new Error("MCP HTTP 401: nope")), false)
  })

  it("is not fooled by truthy-but-not-true values or non-objects", () => {
    const almost = new Error("x")
    ;(almost as Error & { terminal: unknown }).terminal = "yes"
    assert.equal(isTerminalAuthError(almost), false)
    assert.equal(isTerminalAuthError(null), false)
    assert.equal(isTerminalAuthError(undefined), false)
    assert.equal(isTerminalAuthError("terminal"), false)
  })
})

describe("isAuthErrorMessage", () => {
  it("matches the three auth-error shapes", () => {
    assert.equal(isAuthErrorMessage("MCP HTTP 401: Unauthorized"), true)
    assert.equal(isAuthErrorMessage("unauthorized"), true)
    assert.equal(isAuthErrorMessage("Authentication failed for this request"), true)
  })

  it("does not match unrelated text containing 401 as a substring", () => {
    assert.equal(isAuthErrorMessage("page-4012 not found"), false)
    assert.equal(isAuthErrorMessage("MCP HTTP 500: boom"), false)
  })
})

describe("isAuthErrorResult", () => {
  it("only inspects isError results", () => {
    assert.equal(isAuthErrorResult({ content: [{ type: "text", text: "401" }] }), false)
    assert.equal(isAuthErrorResult(authErrorResult("401 Unauthorized")), true)
  })
})

// ---------------------------------------------------------------------------
// Terminal auth errors  (AC: security / invalid_grant is terminal, no retry)
// ---------------------------------------------------------------------------

describe("terminal auth errors", () => {
  it("makes no tool call at all when the initial refresh is terminal", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw terminalError("Notion MCP: invalid_grant — run /login-notion to re-authenticate.")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})

    assert.equal(session.callCount, 0, "a dead grant must not produce an upstream request")
    assert.equal(auth.refreshCalls, 1, "the refresh must not be retried")
    assert.equal(result.isError, true)
    assert.equal(
      result.content[0].text,
      "Notion MCP: invalid_grant — run /login-notion to re-authenticate.",
      "a terminal error is surfaced verbatim, not wrapped in the generic auth-error text",
    )
  })

  it("does not retry the tool call when the post-401 refresh turns out terminal", async () => {
    const session = makeSession([authErrorResult("401 Unauthorized"), OK_RESULT])
    const auth = makeStubAuth(async (attempt) => {
      if (attempt === 2) {
        throw terminalError("Notion MCP: invalid_grant — run /login-notion to re-authenticate.")
      }
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", { id: "self" })

    assert.equal(session.callCount, 1, "the second attempt must be abandoned, not made")
    assert.equal(auth.refreshCalls, 2)
    assert.equal(auth.invalidateCalls, 1)
    assert.equal(result.isError, true)
    assert.match(
      result.content[0].text ?? "",
      /login-notion/,
      "the terminal message must replace the vaguer 401",
    )
  })

  it("surfaces the terminal message verbatim rather than wrapping it", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw terminalError("EXACT-TERMINAL-TEXT")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})
    assert.equal(result.content[0].text, "EXACT-TERMINAL-TEXT")
  })
})

// ---------------------------------------------------------------------------
// Retry-once shell
// ---------------------------------------------------------------------------

describe("callToolWithAuthRetry", () => {
  it("passes a successful call straight through", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-search", { query: "x" })

    assert.equal(session.callCount, 1)
    assert.equal(auth.refreshCalls, 1)
    assert.equal(auth.invalidateCalls, 0)
    assert.deepEqual(result, OK_RESULT)
    assert.deepEqual(session.calls[0], { name: "notion-search", args: { query: "x" } })
  })

  it("invalidates, refreshes and retries exactly once on a payload 401", async () => {
    const session = makeSession([authErrorResult("401 Unauthorized"), OK_RESULT])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})

    assert.equal(session.callCount, 2)
    assert.equal(auth.invalidateCalls, 1)
    assert.equal(auth.refreshCalls, 2)
    assert.deepEqual(result, OK_RESULT)
  })

  it("retries once on a thrown HTTP 401", async () => {
    const session = makeSession([new Error("MCP HTTP 401: token expired"), OK_RESULT])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})

    assert.equal(session.callCount, 2)
    assert.deepEqual(result, OK_RESULT)
  })

  it("never makes a third attempt when the retry also 401s", async () => {
    const session = makeSession([
      authErrorResult("401 Unauthorized"),
      authErrorResult("401 Unauthorized"),
    ])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})

    assert.equal(session.callCount, 2)
    assert.equal(result.isError, true)
  })

  it("does not retry a non-auth tool error", async () => {
    const session = makeSession([authErrorResult("404 not found")])
    const auth = makeStubAuth()

    const result = await callToolWithAuthRetry(session, auth, "notion-fetch", {})

    assert.equal(session.callCount, 1)
    assert.equal(auth.invalidateCalls, 0)
    assert.equal(result.isError, true)
  })

  it("propagates a non-auth thrown error", async () => {
    const session = makeSession([new Error("MCP HTTP 500: upstream exploded")])
    const auth = makeStubAuth()

    await assert.rejects(
      () => callToolWithAuthRetry(session, auth, "notion-search", {}),
      /500/,
    )
  })

  it("returns an error-shaped result for a transient (non-terminal) initial refresh failure", async () => {
    const session = makeSession([OK_RESULT])
    const auth = makeStubAuth(async () => {
      throw new Error("network unreachable")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})

    assert.equal(session.callCount, 0)
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /Notion auth error: network unreachable/)
  })

  it("surfaces the original 401 when the post-401 refresh fails transiently", async () => {
    const session = makeSession([authErrorResult("401 Unauthorized"), OK_RESULT])
    const auth = makeStubAuth(async (attempt) => {
      if (attempt === 2) throw new Error("network unreachable")
    })

    const result = await callToolWithAuthRetry(session, auth, "notion-search", {})

    assert.equal(session.callCount, 1)
    assert.equal(result.isError, true)
    assert.match(result.content[0].text ?? "", /401/)
  })
})
