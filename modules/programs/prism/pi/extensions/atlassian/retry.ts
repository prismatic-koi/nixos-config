// Auth-error-reload-retry shell for MCP tool calls (#2389).
//
// The Atlassian MCP session holds an access token that can become invalid
// mid-session when a sibling pi process rotates the refresh token upstream
// (refresh-token rotation revokes the prior access token). We can't
// distinguish "stale" from "wrong" until we send the token and get back a
// 401 — so we react to the 401 by invalidating our in-memory cache,
// reloading tokens from disk (a peer may have just written fresher ones),
// and retrying the call exactly once.
//
// Two auth-error shapes exist:
//
//   1. HTTP-transport 401 — mcp-client.ts::send throws
//      `Error("MCP HTTP 401: ...")` when the underlying fetch response
//      has status 401.
//   2. Tool-payload auth error — a successful MCP response with
//      `isError: true` and a text block matching `401`, `Unauthorized`,
//      or `Authentication failed` (Atlassian's Jira/Confluence upstream
//      surfaces auth errors this way through the MCP proxy).
//
// Both are handled by the same shell: one retry, and only one retry, per
// tool invocation.
//
// Design: dependency-injected auth callbacks so the shell can be exercised
// in isolation (see retry.test.ts) without touching the real token store.

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
   * Ensure the session has a valid access token. Implementations typically
   * load-or-refresh tokens and push the current access token onto the
   * session (via updateToken). Throws on unrecoverable auth failure — e.g.
   * `Atlassian MCP: no auth tokens` when the store is empty.
   */
  refresh(): Promise<void>

  /**
   * Drop any in-memory token cache so the next refresh() reads fresh from
   * disk. Called between the first attempt and the retry when a 401 is
   * observed.
   */
  invalidate(): void
}

// ---------------------------------------------------------------------------
// Auth-error detection
// ---------------------------------------------------------------------------

/**
 * Return true when the text looks like an authentication error. Matches
 * (case-insensitive) any of `401`, `Unauthorized`, or `Authentication
 * failed`. The regex uses word boundaries around `401` to avoid matching
 * numeric strings that happen to contain "401" (e.g. issue keys).
 */
export function isAuthErrorMessage(text: string): boolean {
  return /\b401\b|\bUnauthorized\b|Authentication failed/i.test(text)
}

/**
 * Return true when a tool result payload looks like an authentication
 * error. Only isError results are inspected — a successful result is
 * never treated as an auth error even if its text happens to mention
 * "401".
 */
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
 * Call `session.callTool(name, args)` with the auth-error-reload-retry
 * behaviour described at the top of this file.
 *
 * Flow:
 *   1. `auth.refresh()` — ensure the session has a valid token (may
 *      refresh via OAuth). If this throws, an error-shaped result is
 *      returned rather than propagating.
 *   2. `session.callTool()` — first attempt.
 *   3. If the response indicates an auth error (HTTP 401 thrown OR
 *      tool-payload `isError` with a matching message):
 *      a. `auth.invalidate()` — drop the in-memory token cache.
 *      b. `auth.refresh()` — reload from disk (potentially picking up
 *         tokens rotated by a peer) and refresh if expired. If this
 *         throws, surface the original auth error unchanged.
 *      c. `session.callTool()` — second (and final) attempt.
 *   4. Return the final result. On a non-auth thrown error at any point,
 *      the error propagates.
 *
 * The retry is bounded — a second consecutive auth error is surfaced to
 * the caller unchanged. There is never a third attempt.
 */
export async function callToolWithAuthRetry(
  session: McpSessionLike,
  auth: AuthCallbacks,
  name: string,
  args: Record<string, unknown>,
): Promise<McpCallResult> {
  // Step 1: initial refresh. A failure here is fatal for this call —
  // convert to an error-shaped result so callers don't have to distinguish
  // between the auth path and the tool-call path in their error handlers.
  try {
    await auth.refresh()
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    return {
      content: [{ type: "text", text: `Atlassian auth error: ${msg}` }],
      isError: true,
    }
  }

  const first = await attemptCall(session, name, args)
  if (!first.isAuth) {
    if (first.thrown) throw first.thrown
    return first.result
  }

  // Step 3: auth error observed. Invalidate cache, refresh, retry ONCE.
  auth.invalidate()
  try {
    await auth.refresh()
  } catch {
    // Reload/refresh failed after the first auth error — surface the
    // original error unchanged. This is the "peer removed the token file
    // between our first call and the retry" edge case.
    if (first.thrown) throw first.thrown
    return first.result
  }

  const second = await attemptCall(session, name, args)
  if (second.thrown) throw second.thrown
  return second.result
}
