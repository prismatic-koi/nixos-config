// Tests for the unified rate-limit header capture (issue #2538, parent #2537).
// Run with: tsx --test ratelimit.test.ts  (Node 20+, zero new deps)
//
// The parser is exercised against fabricated header sets and the transport
// against a real node:http server on a Unix socket in a tmpdir. Live capture
// cannot be verified here: pi.nix mounts the extensions directory as a
// read-only nix-store symlink, so a change only takes effect after
// `nh switch`. Ben verifies live after merge (#2538 testing constraint).

import { describe, it, before, after } from "node:test"
import assert from "node:assert/strict"
import { createServer, type Server } from "node:http"
import { mkdtempSync, rmSync } from "node:fs"
import { readFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { initLogger, closeLogger } from "./logger.ts"
import {
  parseUnifiedRateLimitHeaders,
  buildSnapshotRequestOptions,
  sendSnapshot,
  captureRateLimitSnapshot,
  USAGE_SNAPSHOT_PATH,
  type HeaderReader,
  type RateLimitSnapshot,
} from "./ratelimit.ts"

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/** Builds a case-insensitive HeaderReader from a plain object, like Headers. */
function makeHeaders(entries: Record<string, string>): HeaderReader {
  const lower = new Map<string, string>()
  for (const [key, value] of Object.entries(entries)) {
    lower.set(key.toLowerCase(), value)
  }
  return { get: (name: string) => lower.get(name.toLowerCase()) ?? null }
}

/**
 * The full header set confirmed empirically against a live 200 response
 * (#2537). Kept verbatim so a change to the confirmed set fails here.
 */
const FULL_HEADERS: Record<string, string> = {
  "anthropic-ratelimit-unified-status": "allowed_warning",
  "anthropic-ratelimit-unified-5h-status": "allowed_warning",
  "anthropic-ratelimit-unified-5h-utilization": "0.94",
  "anthropic-ratelimit-unified-5h-reset": "1785634800",
  "anthropic-ratelimit-unified-5h-surpassed-threshold": "0.9",
  "anthropic-ratelimit-unified-7d-status": "allowed",
  "anthropic-ratelimit-unified-7d-utilization": "0.42",
  "anthropic-ratelimit-unified-7d-reset": "1786021200",
  "anthropic-ratelimit-unified-representative-claim": "five_hour",
  "anthropic-ratelimit-unified-reset": "1785634800",
  "anthropic-ratelimit-unified-fallback": "available",
  "anthropic-ratelimit-unified-fallback-percentage": "0.5",
  "anthropic-ratelimit-unified-overage-status": "rejected",
  "anthropic-ratelimit-unified-overage-disabled-reason": "out_of_credits",
}

// ---------------------------------------------------------------------------
// parseUnifiedRateLimitHeaders — the snapshot shape
// ---------------------------------------------------------------------------

describe("parseUnifiedRateLimitHeaders", () => {
  it("produces the exact snapshot documented in #2537", () => {
    const got = parseUnifiedRateLimitHeaders(makeHeaders(FULL_HEADERS))
    assert.deepEqual(got, {
      unified_status: "allowed_warning",
      representative_claim: "five_hour",
      unified_reset: 1785634800,
      windows: {
        five_hour: {
          status: "allowed_warning",
          utilization: 0.94,
          reset: 1785634800,
          surpassed_threshold: 0.9,
        },
        seven_day: {
          status: "allowed",
          utilization: 0.42,
          reset: 1786021200,
        },
      },
      fallback: { status: "available", percentage: 0.5 },
      overage: { status: "rejected", disabled_reason: "out_of_credits" },
    })
  })

  it("keeps utilization as the raw fraction — 0.94 stays 0.94", () => {
    const got = parseUnifiedRateLimitHeaders(makeHeaders(FULL_HEADERS))
    assert.equal(got?.windows?.five_hour?.utilization, 0.94)
    assert.equal(got?.windows?.seven_day?.utilization, 0.42)
  })

  it("serialises to a body with no undefined keys", () => {
    const got = parseUnifiedRateLimitHeaders(makeHeaders(FULL_HEADERS))
    const wire = JSON.parse(JSON.stringify(got)) as Record<string, unknown>
    assert.equal(Object.values(wire).includes(undefined), false)
  })

  it("returns null when no unified header is present", () => {
    const headers = makeHeaders({
      "content-type": "text/event-stream",
      "request-id": "req_abc123",
      // A non-unified rate-limit header must not be picked up.
      "anthropic-ratelimit-requests-limit": "40",
    })
    assert.equal(parseUnifiedRateLimitHeaders(headers), null)
  })

  it("returns null for a completely empty header set", () => {
    assert.equal(parseUnifiedRateLimitHeaders(makeHeaders({})), null)
  })

  it("returns null when every unified header present is unparseable", () => {
    const headers = makeHeaders({
      "anthropic-ratelimit-unified-5h-utilization": "not-a-number",
      "anthropic-ratelimit-unified-5h-reset": "",
    })
    assert.equal(parseUnifiedRateLimitHeaders(headers), null)
  })

  it("omits absent headers rather than zero-filling them", () => {
    // Only the 5h window reported; nothing else.
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        "anthropic-ratelimit-unified-5h-utilization": "0.31",
      }),
    )
    assert.deepEqual(got, { windows: { five_hour: { utilization: 0.31 } } })
    assert.equal("unified_status" in (got ?? {}), false)
    assert.equal("fallback" in (got ?? {}), false)
    assert.equal("overage" in (got ?? {}), false)
    assert.equal("seven_day" in (got?.windows ?? {}), false)
    assert.equal("reset" in (got?.windows?.five_hour ?? {}), false)
  })

  it("preserves an explicit zero — 0 is not the same as absent", () => {
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        "anthropic-ratelimit-unified-5h-utilization": "0",
        "anthropic-ratelimit-unified-5h-reset": "0",
      }),
    )
    assert.equal(got?.windows?.five_hour?.utilization, 0)
    assert.equal(got?.windows?.five_hour?.reset, 0)
  })

  it("omits a single unparseable field and keeps its siblings", () => {
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        ...FULL_HEADERS,
        "anthropic-ratelimit-unified-5h-utilization": "not-a-number",
      }),
    )
    assert.equal("utilization" in (got?.windows?.five_hour ?? {}), false)
    // Siblings survive.
    assert.equal(got?.windows?.five_hour?.reset, 1785634800)
    assert.equal(got?.windows?.five_hour?.status, "allowed_warning")
    assert.equal(got?.windows?.seven_day?.utilization, 0.42)
    // And nothing NaN-shaped reaches the wire.
    assert.equal(JSON.stringify(got).includes("null"), false)
    assert.equal(JSON.stringify(got).includes("NaN"), false)
  })

  it("treats an empty-string header as absent, never as zero", () => {
    // Number("") is 0 — this is the trap this assertion guards.
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        "anthropic-ratelimit-unified-status": "allowed",
        "anthropic-ratelimit-unified-5h-utilization": "",
        "anthropic-ratelimit-unified-5h-reset": "   ",
      }),
    )
    assert.deepEqual(got, { unified_status: "allowed" })
  })

  it("omits Infinity and NaN header values", () => {
    for (const value of ["Infinity", "-Infinity", "NaN"]) {
      const got = parseUnifiedRateLimitHeaders(
        makeHeaders({
          "anthropic-ratelimit-unified-status": "allowed",
          "anthropic-ratelimit-unified-5h-utilization": value,
        }),
      )
      assert.deepEqual(got, { unified_status: "allowed" }, `value=${value}`)
    }
  })

  it("truncates a fractional reset to integer unix seconds", () => {
    // The sidecar decodes reset into a Go int64 and 400s on a fractional
    // value, so the truncation here protects the whole snapshot.
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        "anthropic-ratelimit-unified-reset": "1785634800.75",
        "anthropic-ratelimit-unified-5h-reset": "1785634801.99",
      }),
    )
    assert.equal(got?.unified_reset, 1785634800)
    assert.equal(got?.windows?.five_hour?.reset, 1785634801)
    assert.equal(Number.isInteger(got?.unified_reset), true)
    assert.equal(Number.isInteger(got?.windows?.five_hour?.reset), true)
  })

  it("trims surrounding whitespace from enum values", () => {
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({ "anthropic-ratelimit-unified-status": "  allowed  " }),
    )
    assert.equal(got?.unified_status, "allowed")
  })

  it("reads surpassed_threshold for the 5h window only", () => {
    // The confirmed header set has no 7d surpassed-threshold. Reading one
    // would put a non-allowlisted field in the snapshot.
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        "anthropic-ratelimit-unified-5h-surpassed-threshold": "0.9",
        "anthropic-ratelimit-unified-7d-surpassed-threshold": "0.8",
        "anthropic-ratelimit-unified-7d-utilization": "0.42",
      }),
    )
    assert.equal(got?.windows?.five_hour?.surpassed_threshold, 0.9)
    assert.equal("surpassed_threshold" in (got?.windows?.seven_day ?? {}), false)
  })

  it("captures no header outside the #2537 allowlist", () => {
    // The token-leak guard. A naive Object.fromEntries(response.headers)
    // would sweep up authorization; the explicit allowlist must not.
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({
        ...FULL_HEADERS,
        authorization: "Bearer sk-ant-oat01-SECRET",
        "x-api-key": "sk-ant-api03-SECRET",
        cookie: "session=SECRET",
        "set-cookie": "session=SECRET",
      }),
    )
    const wire = JSON.stringify(got)
    for (const secret of [
      "SECRET",
      "Bearer",
      "authorization",
      "x-api-key",
      "cookie",
    ]) {
      assert.equal(
        wire.toLowerCase().includes(secret.toLowerCase()),
        false,
        `snapshot leaked ${secret}: ${wire}`,
      )
    }
  })

  it("reads headers case-insensitively", () => {
    const got = parseUnifiedRateLimitHeaders(
      makeHeaders({ "Anthropic-RateLimit-Unified-Status": "allowed" }),
    )
    assert.equal(got?.unified_status, "allowed")
  })
})

// ---------------------------------------------------------------------------
// buildSnapshotRequestOptions — PRISM_HOST_API URL shapes
// ---------------------------------------------------------------------------

describe("buildSnapshotRequestOptions", () => {
  it("maps a unix:// URL to a socketPath request", () => {
    const options = buildSnapshotRequestOptions("unix:///run/prism/hostapi.sock")
    assert.equal(options?.socketPath, "/run/prism/hostapi.sock")
    assert.equal(options?.method, "POST")
    assert.equal(options?.path, USAGE_SNAPSHOT_PATH)
    assert.equal(options?.host, "prism-hostapi")
  })

  it("maps an http:// URL to a TCP request with the endpoint appended", () => {
    const options = buildSnapshotRequestOptions(
      "http://host.containers.internal:8123",
    )
    assert.equal(options?.hostname, "host.containers.internal")
    assert.equal(options?.port, "8123")
    assert.equal(options?.path, USAGE_SNAPSHOT_PATH)
    assert.equal(options?.socketPath, undefined)
  })

  it("returns null for an unsupported or malformed value", () => {
    for (const value of [
      "",
      "unix://",
      "https://example.com",
      "not-a-url",
      "/run/prism/hostapi.sock",
    ]) {
      assert.equal(buildSnapshotRequestOptions(value), null, `value=${value}`)
    }
  })
})

// ---------------------------------------------------------------------------
// sendSnapshot — the transport never rejects
// ---------------------------------------------------------------------------

describe("sendSnapshot", () => {
  let dir: string
  let sockPath: string
  let server: Server
  let received: { path?: string; body: string; contentType?: string }[] = []
  let respondWith = 200

  before(async () => {
    dir = mkdtempSync(join(tmpdir(), "prism-ratelimit-test-"))
    sockPath = join(dir, "hostapi.sock")
    server = createServer((req, res) => {
      let body = ""
      req.on("data", (chunk) => {
        body += String(chunk)
      })
      req.on("end", () => {
        received.push({
          path: req.url,
          body,
          contentType: req.headers["content-type"],
        })
        res.writeHead(respondWith, { "content-type": "application/json" })
        res.end("{}")
      })
    })
    await new Promise<void>((resolve) => server.listen(sockPath, resolve))
  })

  after(async () => {
    await new Promise<void>((resolve) => server.close(() => resolve()))
    rmSync(dir, { recursive: true, force: true })
  })

  it("POSTs the snapshot as JSON to /usage/snapshot over a unix socket", async () => {
    received = []
    respondWith = 200
    const snapshot: RateLimitSnapshot = {
      unified_status: "allowed_warning",
      windows: { five_hour: { utilization: 0.94 } },
    }
    await sendSnapshot(`unix://${sockPath}`, snapshot)

    assert.equal(received.length, 1)
    assert.equal(received[0].path, USAGE_SNAPSHOT_PATH)
    assert.equal(received[0].contentType, "application/json")
    assert.deepEqual(JSON.parse(received[0].body), snapshot)
  })

  it("sends no account or captured_at field — the sidecar owns both", async () => {
    received = []
    respondWith = 200
    await sendSnapshot(
      `unix://${sockPath}`,
      parseUnifiedRateLimitHeaders(makeHeaders(FULL_HEADERS))!,
    )
    const body = JSON.parse(received[0].body) as Record<string, unknown>
    assert.equal("account" in body, false)
    assert.equal("captured_at" in body, false)
  })

  it("resolves rather than rejecting on a non-2xx status", async () => {
    received = []
    respondWith = 400
    await sendSnapshot(`unix://${sockPath}`, { unified_status: "allowed" })
    assert.equal(received.length, 1)
  })

  it("resolves rather than rejecting when the socket does not exist", async () => {
    await sendSnapshot(`unix://${join(dir, "no-such.sock")}`, {
      unified_status: "allowed",
    })
  })

  it("survives a logger that throws from a detached handler", async () => {
    // The catch-all in captureRateLimitSnapshot cannot reach the 'error',
    // 'timeout', and 'end' callbacks — they run on later ticks, so a throw
    // there is an uncaught exception that kills the whole pi process rather
    // than one API call. Route the logger at a stream whose write() throws
    // and drive the failure path that logs.
    initLogger({
      stream: {
        write() {
          throw new Error("logger exploded")
        },
      } as unknown as Parameters<typeof initLogger>[0]["stream"],
    })
    try {
      // Connection refused → the 'error' handler → safeLog.
      await sendSnapshot(`unix://${join(dir, "no-such.sock")}`, {
        unified_status: "allowed",
      })
      // Non-2xx → the 'end' handler → safeLog.
      respondWith = 500
      await sendSnapshot(`unix://${sockPath}`, { unified_status: "allowed" })
      // Unsupported URL → the synchronous skip path → safeLog.
      await sendSnapshot("ftp://nope", { unified_status: "allowed" })
    } finally {
      closeLogger()
      respondWith = 200
    }
  })

  it("resolves rather than rejecting for an unsupported PRISM_HOST_API value", async () => {
    await sendSnapshot("ftp://nope", { unified_status: "allowed" })
  })

  it("resolves rather than rejecting when the server never answers", async () => {
    // A listener that accepts the connection and then stays silent forces the
    // timeout path.
    const silentSock = join(dir, "silent.sock")
    const silent = createServer(() => {
      /* never respond */
    })
    await new Promise<void>((resolve) => silent.listen(silentSock, resolve))
    try {
      const started = Date.now()
      await sendSnapshot(`unix://${silentSock}`, { unified_status: "allowed" }, 150)
      assert.ok(
        Date.now() - started < 5000,
        "the timeout must abandon the request promptly",
      )
    } finally {
      await new Promise<void>((resolve) => silent.close(() => resolve()))
    }
  })
})

// ---------------------------------------------------------------------------
// captureRateLimitSnapshot — the request-path guarantees
// ---------------------------------------------------------------------------

describe("captureRateLimitSnapshot", () => {
  /** Records calls and reports whether the sender was invoked. */
  function recordingSender(): {
    calls: { apiURL: string; snapshot: RateLimitSnapshot }[]
    send: (apiURL: string, snapshot: RateLimitSnapshot) => Promise<void>
  } {
    const calls: { apiURL: string; snapshot: RateLimitSnapshot }[] = []
    return {
      calls,
      send: async (apiURL, snapshot) => {
        calls.push({ apiURL, snapshot })
      },
    }
  }

  it("POSTs the parsed fields after a 200 response", () => {
    const sender = recordingSender()
    captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
      apiURL: "unix:///tmp/fake.sock",
      send: sender.send,
    })
    assert.equal(sender.calls.length, 1)
    assert.equal(sender.calls[0].apiURL, "unix:///tmp/fake.sock")
    assert.equal(sender.calls[0].snapshot.unified_status, "allowed_warning")
    assert.equal(sender.calls[0].snapshot.windows?.five_hour?.utilization, 0.94)
  })

  it("makes no call for a non-200 status", () => {
    for (const status of [201, 204, 301, 400, 401, 429, 500, 503]) {
      const sender = recordingSender()
      captureRateLimitSnapshot(status, makeHeaders(FULL_HEADERS), {
        apiURL: "unix:///tmp/fake.sock",
        send: sender.send,
      })
      assert.equal(sender.calls.length, 0, `status=${status}`)
    }
  })

  it("makes no network call when PRISM_HOST_API is unset", () => {
    const original = process.env.PRISM_HOST_API
    delete process.env.PRISM_HOST_API
    try {
      const sender = recordingSender()
      captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
        send: sender.send,
      })
      assert.equal(sender.calls.length, 0)
    } finally {
      if (original !== undefined) process.env.PRISM_HOST_API = original
    }
  })

  it("makes no network call when PRISM_HOST_API is empty", () => {
    const original = process.env.PRISM_HOST_API
    process.env.PRISM_HOST_API = ""
    try {
      const sender = recordingSender()
      captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
        send: sender.send,
      })
      assert.equal(sender.calls.length, 0)
    } finally {
      if (original === undefined) {
        delete process.env.PRISM_HOST_API
      } else {
        process.env.PRISM_HOST_API = original
      }
    }
  })

  it("falls back to PRISM_HOST_API when no override is given", () => {
    const original = process.env.PRISM_HOST_API
    process.env.PRISM_HOST_API = "unix:///tmp/from-env.sock"
    try {
      const sender = recordingSender()
      captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
        send: sender.send,
      })
      assert.equal(sender.calls.length, 1)
      assert.equal(sender.calls[0].apiURL, "unix:///tmp/from-env.sock")
    } finally {
      if (original === undefined) {
        delete process.env.PRISM_HOST_API
      } else {
        process.env.PRISM_HOST_API = original
      }
    }
  })

  it("sends nothing when the response carries no unified headers", () => {
    const sender = recordingSender()
    captureRateLimitSnapshot(
      200,
      makeHeaders({ "content-type": "text/event-stream" }),
      { apiURL: "unix:///tmp/fake.sock", send: sender.send },
    )
    assert.equal(sender.calls.length, 0)
  })

  it("does not throw when the header reader throws", () => {
    const hostile: HeaderReader = {
      get() {
        throw new Error("header access exploded")
      },
    }
    assert.doesNotThrow(() =>
      captureRateLimitSnapshot(200, hostile, {
        apiURL: "unix:///tmp/fake.sock",
      }),
    )
  })

  it("does not throw when the sender throws synchronously", () => {
    assert.doesNotThrow(() =>
      captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
        apiURL: "unix:///tmp/fake.sock",
        send: () => {
          throw new Error("sender exploded")
        },
      }),
    )
  })

  it("does not throw and produces no unhandled rejection when the sender rejects", async () => {
    let unhandled: unknown = null
    const onUnhandled = (reason: unknown): void => {
      unhandled = reason
    }
    process.on("unhandledRejection", onUnhandled)
    try {
      assert.doesNotThrow(() =>
        captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
          apiURL: "unix:///tmp/fake.sock",
          send: () => Promise.reject(new Error("sender rejected")),
        }),
      )
      // Let the microtask queue and one macrotask tick drain so an
      // unhandled rejection would have surfaced by now.
      await new Promise((resolve) => setTimeout(resolve, 20))
      assert.equal(unhandled, null, `unhandled rejection: ${String(unhandled)}`)
    } finally {
      process.off("unhandledRejection", onUnhandled)
    }
  })

  it("returns undefined, so no call site can await the POST", () => {
    // The load-bearing assertion for the performance AC. A hook that returned
    // the send promise would let index.ts await it with a one-word change,
    // putting a network round trip in front of every API response. Returning
    // undefined makes that impossible rather than merely discouraged.
    const returned = captureRateLimitSnapshot(
      200,
      makeHeaders(FULL_HEADERS),
      {
        apiURL: "unix:///tmp/fake.sock",
        send: () => new Promise<void>(() => {}), // never settles
      },
    ) as unknown
    assert.equal(returned, undefined)
    assert.equal(
      typeof (returned as { then?: unknown })?.then,
      "undefined",
      "the hook must not hand back a thenable",
    )
  })

  it("returns before the POST completes — it does not await the send", async () => {
    let released: (() => void) | undefined
    let completed = false
    const gate = new Promise<void>((resolve) => {
      released = resolve
    })

    captureRateLimitSnapshot(200, makeHeaders(FULL_HEADERS), {
      apiURL: "unix:///tmp/fake.sock",
      send: async () => {
        await gate
        completed = true
      },
    })

    // Control is already back here while the send is still parked on the
    // gate. If the hook awaited, this line would not run until released.
    assert.equal(completed, false)
    released!()
    await gate
  })

  it("the index.ts call site does not await the hook", async () => {
    // Source-level guard for the same AC one layer up. The hook returning
    // undefined already makes an await useless, but an `await` keyword there
    // would still add a microtask hop to every response and would signal the
    // wrong intent to the next reader.
    const source = await readFile(
      join(import.meta.dirname, "index.ts"),
      "utf-8",
    )
    assert.ok(
      source.includes("captureRateLimitSnapshot("),
      "index.ts must call captureRateLimitSnapshot",
    )
    assert.equal(
      /await\s+captureRateLimitSnapshot\s*\(/.test(source),
      false,
      "index.ts must not await captureRateLimitSnapshot",
    )
  })

  it("never reads the response body", () => {
    // A stand-in for the fetch Response: touching body/text/json is a
    // regression, so those accessors throw.
    let bodyTouched = false
    const response = {
      status: 200,
      headers: makeHeaders(FULL_HEADERS),
      get body() {
        bodyTouched = true
        throw new Error("the capture hook must not consume the response body")
      },
      text() {
        bodyTouched = true
        throw new Error("the capture hook must not consume the response body")
      },
    }
    const sender = recordingSender()
    captureRateLimitSnapshot(response.status, response.headers, {
      apiURL: "unix:///tmp/fake.sock",
      send: sender.send,
    })
    assert.equal(bodyTouched, false)
    assert.equal(sender.calls.length, 1)
  })
})
