// Auth-error-reload-retry shell for Notion MCP tool calls.
//
// Ported from atlassian/retry.ts (#2389) with one substantive addition: a
// TERMINAL auth-error class that must never be retried.
//
// Why the addition matters. The Atlassian shell reacts to any auth error by
// invalidating the cache, refreshing, and retrying once. Against Notion that
// is dangerous: a refresh whose response was `invalid_grant` means the grant
// is revoked, and hammering the token endpoint with the same (now poisoned)
// refresh token is exactly the "replayed a rotated refresh token" signal
// Notion punishes by revoking the whole connection. Notion's own guidance is
// blunt: "Treat invalid_grant as terminal ... Do not retry a refresh that
// returned invalid_grant."
//
// So the shell distinguishes:
//
//   * TERMINAL (auth.ts throws NotionAuthTerminalError — no tokens at all, or
//     invalid_grant): surface immediately with a /login-notion prompt. No
//     tool call, no retry, no second refresh.
//   * Plain 401 / Unauthorized: the access token is merely stale. Invalidate,
//     reload from disk (a peer may have rotated), retry exactly once.
//
// Design: dependency-injected auth callbacks so the shell can be exercised in
// isolation (see retry.test.ts) without touching the real token store.

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface McpCallResult {
  content: Array<{ type: string; text?: string }>
  isError?: boolean
}

export interface McpSessionLike {
  callTool(name: string, args: Record<string, unknown>): Promise<McpCallResult>
}

export interface AuthCallbacks {
  /**
   * Ensure the session has a valid access token. Implementations load-or-
   * refresh tokens and push the current access token onto the session.
   * Throws on unrecoverable auth failure.
   */
  refresh(): Promise<void>

  /**
   * Drop any in-memory token cache so the next refresh() reads fresh from
   * disk. Called between the first attempt and the retry.
   */
  invalidate(): void
}

// ---------------------------------------------------------------------------
// Auth-error detection
// ---------------------------------------------------------------------------

/**
 * True when `err` is a terminal auth failure.
 *
 * Detection is STRUCTURAL — a truthy `terminal` property — rather than an
 * `instanceof` check, so this module stays free of any dependency on auth.ts
 * and remains testable with plain object literals. `NotionAuthTerminalError`
 * carries `terminal = true`.
 */
export function isTerminalAuthError(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    (err as { terminal?: unknown }).terminal === true
  )
}

/**
 * True when the text looks like an authentication error. Matches (case-
 * insensitively) `401`, `Unauthorized`, or `Authentication failed`. Word
 * boundaries around `401` avoid matching identifiers that merely contain it.
 */
export function isAuthErrorMessage(text: string): boolean {
  return /\b401\b|\bUnauthorized\b|Authentication failed/i.test(text)
}

/**
 * True when a tool result payload looks like an authentication error. Only
 * `isError` results are inspected — a successful result is never treated as
 * an auth error even if its text happens to mention "401".
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
      result: { content: [{ type: "text", text: msg }], isError: true },
      isAuth: isAuthErrorMessage(msg),
      thrown: err,
    }
  }
}

function terminalResult(err: unknown): McpCallResult {
  const msg = err instanceof Error ? err.message : String(err)
  return { content: [{ type: "text", text: msg }], isError: true }
}

/**
 * Call `session.callTool(name, args)` with auth-error-reload-retry.
 *
 * Flow:
 *   1. `auth.refresh()`. A TERMINAL failure returns immediately with the
 *      re-login prompt — no tool call is attempted. A non-terminal failure
 *      returns an error-shaped result rather than propagating.
 *   2. `session.callTool()` — first attempt.
 *   3. On a non-terminal auth error (HTTP 401 thrown OR a tool-payload
 *      `isError` with a matching message):
 *      a. `auth.invalidate()`.
 *      b. `auth.refresh()`. If this throws — terminal or otherwise — the
 *         result is surfaced without a second tool call.
 *      c. `session.callTool()` — second and final attempt.
 *   4. Return the final result. A non-auth thrown error propagates.
 *
 * There is never a third attempt, and never a retry past a terminal error.
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
    if (isTerminalAuthError(err)) {
      // Terminal: the grant is gone or absent. Retrying the refresh is the
      // specific thing Notion tells us not to do.
      return terminalResult(err)
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

  // Non-terminal auth error: invalidate, reload, retry ONCE.
  auth.invalidate()
  try {
    await auth.refresh()
  } catch (err) {
    if (isTerminalAuthError(err)) {
      // The reload told us the grant is dead. Surface that rather than the
      // vaguer 401, and do NOT make a second tool call.
      return terminalResult(err)
    }
    // Transient reload failure — surface the original auth error unchanged.
    if (first.thrown) throw first.thrown
    return first.result
  }

  const second = await attemptCall(session, name, args)
  if (second.thrown) throw second.thrown
  return second.result
}
