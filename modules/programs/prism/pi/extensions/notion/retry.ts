// Auth-error-reload-retry shell for Notion MCP tool calls.
//
// Adapted from atlassian/retry.ts with one critical difference: Notion's
// InvalidGrantError is TERMINAL — it means the entire OAuth grant has been
// revoked by the upstream, typically because a rotated refresh_token was
// replayed. Retrying a call whose auth failure originated from
// invalid_grant is never useful — the tokens are gone and the user must
// re-run /login-notion.
//
// The retry shell distinguishes:
//
//   1. Plain 401 / Unauthorized       (retryable — refresh may have raced
//                                       and a peer's fresh token is on
//                                       disk). One retry, one refresh.
//   2. Tool-payload auth errors       (same treatment).
//   3. InvalidGrantError propagation  (TERMINAL — no retry; surface to
//                                       the caller as an error-shaped
//                                       result so the tool call fails
//                                       cleanly without another /token
//                                       hit).

import { InvalidGrantError } from "./auth.ts"

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface McpCallResult {
  content: Array<{ type: string; text?: string }>
  isError?: boolean
}

export interface McpSessionLike {
  callTool(
    name: string,
    args: Record<string, unknown>,
  ): Promise<McpCallResult>
}

export interface AuthCallbacks {
  /**
   * Ensure the session has a valid access token. Implementations
   * typically load-or-refresh tokens and push the current access token
   * onto the session (via updateToken).
   *
   * Throws InvalidGrantError when the OAuth grant has been terminally
   * revoked. Throws a plain Error for other unrecoverable auth failures
   * (e.g. `Notion MCP: no auth tokens` when the store is empty).
   */
  refresh(): Promise<void>

  /**
   * Drop any in-memory token cache so the next refresh() reads fresh
   * from disk. Called between the first attempt and the retry when a
   * 401 is observed.
   */
  invalidate(): void
}

// ---------------------------------------------------------------------------
// Auth-error detection
// ---------------------------------------------------------------------------

export function isAuthErrorMessage(text: string): boolean {
  return /\b401\b|\bUnauthorized\b|Authentication failed/i.test(text)
}

export function isAuthErrorResult(result: McpCallResult): boolean {
  if (!result.isError) return false
  const text = result.content?.map((c) => c.text ?? "").join("\n") ?? ""
  return isAuthErrorMessage(text)
}

// ---------------------------------------------------------------------------
// Retry shell
// ---------------------------------------------------------------------------

interface AttemptOutcome {
  result: McpCallResult
  isAuth: boolean
  thrown: unknown | null
}

async function attemptCall(
  session: McpSessionLike,
  name: string,
  args: Record<string, unknown>,
): Promise<AttemptOutcome> {
  try {
    const result = await session.callTool(name, args)
    return { result, isAuth: isAuthErrorResult(result), thrown: null }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    return {
      result: {
        content: [{ type: "text", text: msg }],
        isError: true,
      },
      isAuth: isAuthErrorMessage(msg),
      thrown: err,
    }
  }
}

/**
 * Call `session.callTool(name, args)` with auth-error-reload-retry
 * behaviour. Semantics:
 *
 *   1. Initial `auth.refresh()`. If this throws InvalidGrantError, return
 *      a terminal error-shaped result — do NOT attempt the tool call.
 *      This is the whole point of the invalid_grant branch: retrying
 *      would just hit /token again with the same rotated refresh_token
 *      and Notion may treat repeated replays as evidence of a stolen
 *      token.
 *   2. First tool call. If it succeeds (with or without an isError body
 *      that isn't an auth error), return the result.
 *   3. If the first attempt looked like an auth error, invalidate the
 *      cache and refresh again. If the second refresh throws
 *      InvalidGrantError, return the terminal message.
 *   4. Second tool call. Return whatever comes back.
 *
 * The retry is bounded to one. A second consecutive auth error is
 * surfaced unchanged.
 */
export async function callToolWithAuthRetry(
  session: McpSessionLike,
  auth: AuthCallbacks,
  name: string,
  args: Record<string, unknown>,
): Promise<McpCallResult> {
  try {
    await auth.refresh()
  } catch (err) {
    if (err instanceof InvalidGrantError) {
      return terminalGrantResult()
    }
    const msg = err instanceof Error ? err.message : String(err)
    return {
      content: [{ type: "text", text: `Notion auth error: ${msg}` }],
      isError: true,
    }
  }

  const first = await attemptCall(session, name, args)
  if (!first.isAuth) {
    if (first.thrown) throw first.thrown
    return first.result
  }

  // Auth error observed. Invalidate + refresh + retry once.
  auth.invalidate()
  try {
    await auth.refresh()
  } catch (err) {
    if (err instanceof InvalidGrantError) {
      // The refresh between attempts revealed a revoked grant. Surface
      // the terminal message and stop; do not attempt the second call.
      return terminalGrantResult()
    }
    // Reload/refresh failed for a non-terminal reason — surface the
    // original auth error unchanged.
    if (first.thrown) throw first.thrown
    return first.result
  }

  const second = await attemptCall(session, name, args)
  if (second.thrown) throw second.thrown
  return second.result
}

function terminalGrantResult(): McpCallResult {
  return {
    content: [
      {
        type: "text",
        text:
          "Notion MCP: the OAuth grant has been revoked (invalid_grant). " +
          "Run /login-notion to re-authenticate.",
      },
    ],
    isError: true,
  }
}
