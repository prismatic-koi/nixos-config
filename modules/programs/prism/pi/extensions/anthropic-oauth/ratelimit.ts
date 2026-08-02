// Unified rate-limit header capture (issue #2538, parent #2537).
//
// LOCAL ADDITION — this file has no upstream counterpart in either
// griffinmartin/opencode-claude-auth or leohenon/pi-anthropic-oauth. See
// UPSTREAM.md divergence #14.
//
// Anthropic returns a set of `anthropic-ratelimit-unified-*` headers on every
// successful `/v1/messages` response on the Claude Code OAuth path. This
// module reads that allowlisted header set off the response, converts it to
// the snapshot shape documented in #2537, and POSTs it to the prism sidecar's
// host-API endpoint `/usage/snapshot`. The sidecar resolves the active
// account and owns the write; nothing here touches the filesystem.
//
// It lives beside index.ts rather than inside it so the parser is unit
// testable without pulling in the pi runtime packages
// (`@earendil-works/pi-ai`, `@earendil-works/pi-coding-agent`), following the
// precedent of oauth-headers.ts. Only Node builtins are imported.
//
// ## Two rules that outrank everything else in this file
//
// The capture point sits in the live OAuth request path. Therefore:
//
//  1. **It must never throw.** An unhandled exception here breaks every API
//     call for every session on the machine. captureRateLimitSnapshot wraps
//     its whole body in a catch-all, and sendSnapshot returns a promise that
//     never rejects.
//  2. **It must never block or consume the response.** The hook reads
//     headers only — never `response.body`, never `response.text()` — and it
//     does not await the POST. captureRateLimitSnapshot returns void
//     synchronously; the request continues while the POST is in flight.

import { request as httpRequest } from "node:http"
import type { ClientRequest, RequestOptions } from "node:http"
import { log } from "./logger.ts"

/**
 * log() that cannot throw.
 *
 * The catch-all in captureRateLimitSnapshot does NOT cover the callbacks
 * below — the 'error', 'timeout', and 'end' handlers in sendSnapshot run
 * detached on later ticks of the event loop, so a throw there is an uncaught
 * exception that takes down the whole pi process rather than one API call.
 *
 * The logger is not throw-free: it does `appendFileSync` when
 * PI_ANTHROPIC_OAUTH_DEBUG names a file, which fails on ENOSPC, EACCES, or a
 * log directory removed mid-session. That is a narrow window — the debug env
 * var is off by default and no config in this repo sets it — but the blast
 * radius is the entire process, so it is worth one wrapper.
 */
function safeLog(event: string, data?: Record<string, unknown>): void {
  try {
    log(event, data)
  } catch {
    // A failed debug log must never disturb the request path.
  }
}

/** The sidecar host-API path this module POSTs to. */
export const USAGE_SNAPSHOT_PATH = "/usage/snapshot"

/**
 * Default POST timeout. The sidecar write is two small local file renames, so
 * anything slower than this means the socket is wedged. The request is
 * abandoned at that point — the snapshot is a cache, and the next successful
 * API call refreshes it.
 */
export const DEFAULT_POST_TIMEOUT_MS = 2000

/**
 * The subset of `Headers` this module needs. Declaring the structural type
 * rather than importing `Headers` keeps the parser testable against a plain
 * object literal.
 */
export interface HeaderReader {
  get(name: string): string | null | undefined
}

/** One rate-limit window. `utilization` is the RAW fraction, not a percentage. */
export interface WindowSnapshot {
  status?: string
  utilization?: number
  reset?: number
  surpassed_threshold?: number
}

/**
 * The POST body. Field names are the wire form — `JSON.stringify` of this
 * object is exactly what the sidecar's `usageSnapshotRequest` schema accepts.
 *
 * Every field is optional and an absent header is OMITTED rather than
 * zero-filled, so a reader can tell "not present" from "zero". The sidecar
 * rejects any field not listed here.
 *
 * `captured_at` and `account` are deliberately absent: the sidecar sets both
 * host-side. A sandboxed session cannot read the accounts directory, and
 * resolving the account at write time is what keeps attribution correct after
 * a mid-session account switch (#2537).
 */
export interface RateLimitSnapshot {
  unified_status?: string
  representative_claim?: string
  unified_reset?: number
  windows?: {
    five_hour?: WindowSnapshot
    seven_day?: WindowSnapshot
  }
  fallback?: {
    status?: string
    percentage?: number
  }
  overage?: {
    status?: string
    disabled_reason?: string
  }
}

/** Injectable POST transport. Used by the tests; production uses sendSnapshot. */
export type SnapshotSender = (
  apiURL: string,
  snapshot: RateLimitSnapshot,
) => Promise<void>

// ---------------------------------------------------------------------------
// Header parsing
// ---------------------------------------------------------------------------

/**
 * Reads a header as a trimmed non-empty string, or undefined.
 *
 * A header present with an empty (or whitespace-only) value is treated as
 * absent — the field is omitted rather than persisted as "".
 */
function readString(
  headers: HeaderReader,
  name: string,
): string | undefined {
  const raw = headers.get(name)
  if (typeof raw !== "string") return undefined
  const trimmed = raw.trim()
  return trimmed === "" ? undefined : trimmed
}

/**
 * Reads a header as a finite number, or undefined.
 *
 * The empty-string guard is load-bearing: `Number("")` and `Number("  ")` are
 * both 0, so without it an empty header would be persisted as a real zero and
 * a reader could not tell it from a genuine 0. `Number("abc")` is NaN and
 * `Number("Infinity")` is Infinity — both fail the finite check, so an
 * unparseable value omits that single field rather than persisting NaN or
 * null.
 *
 * No range check is applied. The headers are documented as fractions from 0
 * to 1, but clamping or rejecting an out-of-range value would silently drop
 * real data if Anthropic widens the range. Storing what the server said is
 * the safer contract.
 */
function readNumber(
  headers: HeaderReader,
  name: string,
): number | undefined {
  const raw = readString(headers, name)
  if (raw === undefined) return undefined
  const value = Number(raw)
  return Number.isFinite(value) ? value : undefined
}

/**
 * Reads a header as integer unix seconds, or undefined.
 *
 * The truncation matters: the sidecar decodes reset fields into a Go `int64`
 * and REJECTS the whole request if a value carries a fractional part. Sending
 * an integer keeps one odd header from discarding an otherwise good snapshot.
 */
function readUnixSeconds(
  headers: HeaderReader,
  name: string,
): number | undefined {
  const value = readNumber(headers, name)
  return value === undefined ? undefined : Math.trunc(value)
}

/**
 * Deletes every undefined-valued key from obj in place, then returns obj — or
 * undefined when nothing was left.
 *
 * Deleting rather than leaving the key with an undefined value matters. An
 * absent header must be OMITTED, and `{utilization: undefined}` reads as
 * present-with-no-value to `Object.keys`, `in`, and `deepEqual`, even though
 * `JSON.stringify` happens to drop it. Collapsing an all-absent group to
 * undefined is the same rule one level up: it keeps `{}` out of the body.
 */
function pruneUndefined<T extends object>(obj: T): T | undefined {
  const record = obj as Record<string, unknown>
  let hasValue = false
  for (const key of Object.keys(record)) {
    if (record[key] === undefined) {
      delete record[key]
    } else {
      hasValue = true
    }
  }
  return hasValue ? obj : undefined
}

/**
 * Parses one window's headers. `prefix` is "5h" or "7d".
 *
 * Only the 5-hour window has a `surpassed-threshold` header in the confirmed
 * set (#2537), so it is read for that prefix only. Adding a speculative 7d
 * read would put a non-allowlisted header in the snapshot.
 */
function parseWindow(
  headers: HeaderReader,
  prefix: "5h" | "7d",
): WindowSnapshot | undefined {
  const base = `anthropic-ratelimit-unified-${prefix}`
  const window: WindowSnapshot = {
    status: readString(headers, `${base}-status`),
    utilization: readNumber(headers, `${base}-utilization`),
    reset: readUnixSeconds(headers, `${base}-reset`),
  }
  if (prefix === "5h") {
    window.surpassed_threshold = readNumber(
      headers,
      `${base}-surpassed-threshold`,
    )
  }
  return pruneUndefined(window)
}

/**
 * Builds a snapshot from a response's headers, or returns null when the
 * response carried no usable `anthropic-ratelimit-unified-*` header.
 *
 * Returning null (rather than an empty object) is what implements "when the
 * response carries no rate-limit headers, the hook sends nothing and existing
 * snapshots are left byte-identical". A response whose only unified headers
 * are unparseable also yields null, for the same reason: an information-free
 * snapshot must not overwrite a good one.
 *
 * The header names read here are exactly the allowlist confirmed in #2537.
 * There is no bulk `Object.fromEntries(response.headers)` anywhere in this
 * module — that shape would sweep up `authorization` along with everything
 * else.
 */
export function parseUnifiedRateLimitHeaders(
  headers: HeaderReader,
): RateLimitSnapshot | null {
  const snapshot: RateLimitSnapshot = {
    unified_status: readString(headers, "anthropic-ratelimit-unified-status"),
    representative_claim: readString(
      headers,
      "anthropic-ratelimit-unified-representative-claim",
    ),
    unified_reset: readUnixSeconds(
      headers,
      "anthropic-ratelimit-unified-reset",
    ),
    windows: pruneUndefined({
      five_hour: parseWindow(headers, "5h"),
      seven_day: parseWindow(headers, "7d"),
    }),
    fallback: pruneUndefined({
      status: readString(headers, "anthropic-ratelimit-unified-fallback"),
      percentage: readNumber(
        headers,
        "anthropic-ratelimit-unified-fallback-percentage",
      ),
    }),
    overage: pruneUndefined({
      status: readString(
        headers,
        "anthropic-ratelimit-unified-overage-status",
      ),
      disabled_reason: readString(
        headers,
        "anthropic-ratelimit-unified-overage-disabled-reason",
      ),
    }),
  }
  return pruneUndefined(snapshot) ?? null
}

// ---------------------------------------------------------------------------
// Host-API transport
// ---------------------------------------------------------------------------

/**
 * Translates a PRISM_HOST_API value into node:http request options for
 * POST /usage/snapshot, or null when the value is not a shape we understand.
 *
 * Mirrors `cmd/hostapi.go::proxyToHostAPI`: `unix:///path/to/sock` on Linux
 * (dialled through `socketPath`, with a placeholder Host header) and
 * `http://host:port` on the Darwin TCP path (where the endpoint path is
 * appended to the base URL).
 */
export function buildSnapshotRequestOptions(
  apiURL: string,
): RequestOptions | null {
  const headers = { "content-type": "application/json" }

  if (apiURL.startsWith("unix://")) {
    const socketPath = apiURL.slice("unix://".length)
    if (socketPath === "") return null
    return {
      socketPath,
      // Irrelevant for a Unix socket but required for a valid HTTP/1.1
      // request; matches the Go client's placeholder.
      host: "prism-hostapi",
      method: "POST",
      path: USAGE_SNAPSHOT_PATH,
      headers,
    }
  }

  if (apiURL.startsWith("http://")) {
    let url: URL
    try {
      url = new URL(apiURL + USAGE_SNAPSHOT_PATH)
    } catch {
      return null
    }
    return {
      protocol: url.protocol,
      hostname: url.hostname,
      port: url.port === "" ? undefined : url.port,
      method: "POST",
      path: url.pathname + url.search,
      headers,
    }
  }

  return null
}

/**
 * POSTs the snapshot to the sidecar. The returned promise NEVER rejects — a
 * connection failure, a timeout, or a non-2xx status all resolve normally,
 * because a failed usage capture must not disturb the request that produced
 * it.
 *
 * The body carries only the parsed header fields. No credential material is
 * read, sent, or logged: the failure logs record a status code or an error
 * message, never the request headers.
 */
export function sendSnapshot(
  apiURL: string,
  snapshot: RateLimitSnapshot,
  timeoutMs: number = DEFAULT_POST_TIMEOUT_MS,
): Promise<void> {
  return new Promise<void>((resolve) => {
    let settled = false
    const finish = (): void => {
      if (settled) return
      settled = true
      resolve()
    }

    let req: ClientRequest
    try {
      const options = buildSnapshotRequestOptions(apiURL)
      if (options === null) {
        safeLog("ratelimit_capture_skipped", {
          reason: "unsupported_host_api_url",
        })
        finish()
        return
      }
      const body = JSON.stringify(snapshot)
      req = httpRequest(options, (res) => {
        const status = res.statusCode ?? 0
        // Drain and discard: an unread body holds the socket open.
        res.resume()
        res.on("end", () => {
          if (status < 200 || status >= 300) {
            safeLog("ratelimit_capture_rejected", { status })
          }
          finish()
        })
        res.on("error", () => finish())
      })
      req.on("error", (error) => {
        safeLog("ratelimit_capture_failed", { error: String(error) })
        finish()
      })
      req.setTimeout(timeoutMs, () => {
        safeLog("ratelimit_capture_failed", { error: "timeout", timeoutMs })
        req.destroy()
        finish()
      })
      req.end(body)
    } catch (error) {
      // Covers a synchronous throw from httpRequest / JSON.stringify.
      safeLog("ratelimit_capture_failed", { error: String(error) })
      finish()
    }
  })
}

// ---------------------------------------------------------------------------
// Capture entry point
// ---------------------------------------------------------------------------

export interface CaptureOptions {
  /**
   * PRISM_HOST_API override. Defaults to the environment variable, read at
   * call time so a test can set and unset it per case.
   */
  apiURL?: string
  /** POST transport override. Defaults to sendSnapshot. */
  send?: SnapshotSender
}

/**
 * Captures the rate-limit headers off a completed response and schedules a
 * POST to the sidecar. Returns void SYNCHRONOUSLY and never throws.
 *
 * No capture happens when any of these hold:
 *
 *   - the status is not 200 (an error response carries no usable header set,
 *     and a WAF rejection carries none at all);
 *   - PRISM_HOST_API is unset or empty (no sidecar to POST to, so no network
 *     call is made at all);
 *   - the response carried no parseable unified rate-limit header.
 *
 * The response object is never read beyond its headers, so the body stays
 * untouched for the SSE parser downstream.
 */
export function captureRateLimitSnapshot(
  status: number,
  headers: HeaderReader,
  options: CaptureOptions = {},
): void {
  try {
    if (status !== 200) return

    const apiURL = options.apiURL ?? process.env.PRISM_HOST_API ?? ""
    if (apiURL === "") return

    const snapshot = parseUnifiedRateLimitHeaders(headers)
    if (snapshot === null) return

    const send = options.send ?? sendSnapshot
    // Fire and forget. The response is already on its way back to pi; the
    // POST completes (or fails) on its own. The .catch is belt-and-braces —
    // sendSnapshot never rejects, but an injected sender might.
    void Promise.resolve(send(apiURL, snapshot)).catch(() => {})
  } catch (error) {
    // Nothing in the capture path may escape into the request path.
    safeLog("ratelimit_capture_failed", { error: String(error) })
  }
}
