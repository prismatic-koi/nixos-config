// Ask the prism sidecar to sync the per-session auth.json (#3295).
//
// A LOCAL ADDITION, not a port. See UPSTREAM.md divergence #22.
//
// In a bwrap or sandbox-exec session, auth.json is the auth file of the
// session, not the host ~/.pi/agent/auth.json: pi runs an api_key value in
// auth.json that starts with "!" as a shell command, so a sandbox must not
// write the file that later sessions read. The sidecar of the session runs
// on the host and copies OAuth entries between the two files
// (prism/internal/container/pi_auth_session.go).
//
// The two refresh paths call the sync while they hold the shared refresh
// lock (refresh-lock.ts):
//
//   - before they read auth.json, so that they see a token that another
//     session wrote.
//   - after they write a new token, so that the next holder of the lock sees
//     it.
//
// So one refresh serves all sessions. The sidecar does not take the lock
// for this sync: the caller holds it.
//
// The sync does nothing unless PRISM_PI_AUTH_SYNC is "1" and PRISM_HOST_API
// is set. prism sets both in a sandbox that has a per-session auth.json. A
// failed sync is logged and does not stop the refresh: the refresh then
// runs as before #3295. No call sends or logs a credential: the request has
// no body, and the sidecar reads the files itself.

import { execFileSync } from "node:child_process"
import { request as httpRequest, type RequestOptions } from "node:http"
import { log } from "./logger.ts"

// Go copy: PIAuthSyncPath and PIAuthSyncEnvVar in
// prism/internal/container/pi_auth_session.go. Change both sides together.
// TestPIAuthSync_ExtensionUsesSameValues fails when they differ.
export const SESSION_SYNC_PATH = "/pi-auth/sync"
export const SESSION_SYNC_ENV = "PRISM_PI_AUTH_SYNC"

/** The longest wait for one sync. The holder of the lock waits for it. */
export const SESSION_SYNC_TIMEOUT_MS = 5_000

export interface SessionSyncOptions {
  /** PRISM_HOST_API override. Defaults to the environment, read at call time. */
  apiURL?: string
  timeoutMs?: number
}

/**
 * The PRISM_HOST_API value to sync through, or null when this session has
 * no per-session auth.json.
 */
export function sessionSyncURL(opts: SessionSyncOptions = {}): string | null {
  if (opts.apiURL !== undefined) return opts.apiURL === "" ? null : opts.apiURL
  if (process.env[SESSION_SYNC_ENV] !== "1") return null
  const api = process.env.PRISM_HOST_API ?? ""
  return api === "" ? null : api
}

/**
 * node:http options for POST /pi-auth/sync, or null for a PRISM_HOST_API
 * value of another shape. The same shapes as ratelimit.ts: unix:///path on
 * Linux and Darwin, and http://host:port.
 */
export function sessionSyncRequestOptions(apiURL: string): RequestOptions | null {
  if (apiURL.startsWith("unix://")) {
    const socketPath = apiURL.slice("unix://".length)
    if (socketPath === "") return null
    return { socketPath, host: "prism-hostapi", method: "POST", path: SESSION_SYNC_PATH }
  }
  if (apiURL.startsWith("http://")) {
    let url: URL
    try {
      url = new URL(apiURL + SESSION_SYNC_PATH)
    } catch {
      return null
    }
    return {
      protocol: url.protocol,
      hostname: url.hostname,
      port: url.port === "" ? undefined : url.port,
      method: "POST",
      path: url.pathname + url.search,
    }
  }
  return null
}

/**
 * Ask the sidecar for a sync, and wait for it. Returns true when the sync
 * completed. Never rejects. `reason` goes to the log only.
 */
export function syncSessionAuth(
  reason: "pull" | "push",
  opts: SessionSyncOptions = {},
): Promise<boolean> {
  const apiURL = sessionSyncURL(opts)
  if (apiURL === null) return Promise.resolve(false)
  const timeoutMs = opts.timeoutMs ?? SESSION_SYNC_TIMEOUT_MS
  return new Promise<boolean>((resolve) => {
    let settled = false
    const finish = (ok: boolean, detail: Record<string, unknown> = {}) => {
      if (settled) return
      settled = true
      log(ok ? "session_sync_ok" : "session_sync_failed", { reason, ...detail })
      resolve(ok)
    }
    try {
      const options = sessionSyncRequestOptions(apiURL)
      if (options === null) return finish(false, { error: "unsupported_host_api_url" })
      const req = httpRequest(options, (res) => {
        const status = res.statusCode ?? 0
        res.resume()
        res.on("end", () => finish(status >= 200 && status < 300, { status }))
        res.on("error", () => finish(false, { status }))
      })
      req.on("error", (err) => finish(false, { error: String(err) }))
      req.setTimeout(timeoutMs, () => {
        req.destroy()
        finish(false, { error: "timeout", timeoutMs })
      })
      req.end()
    } catch (err) {
      finish(false, { error: String(err) })
    }
  })
}

// The child of syncSessionAuthSync: one POST with the options on stdin.
// Exit 0 on a 2xx answer, else non-zero.
const SYNC_CHILD = `
const http = require("node:http");
let input = "";
process.stdin.on("data", (c) => (input += c));
process.stdin.on("end", () => {
  const { options, timeoutMs } = JSON.parse(input);
  const req = http.request(options, (res) => {
    res.resume();
    res.on("end", () => process.exit(res.statusCode >= 200 && res.statusCode < 300 ? 0 : 2));
  });
  req.on("error", () => process.exit(3));
  req.setTimeout(timeoutMs, () => { req.destroy(); process.exit(4); });
  req.end();
});
`

/**
 * The sync form of syncSessionAuth, for the sync refresh path. It blocks
 * the event loop for at most the timeout: the request runs in a child
 * process, as refreshViaOAuth does. Never throws.
 */
export function syncSessionAuthSync(
  reason: "pull" | "push",
  opts: SessionSyncOptions = {},
): boolean {
  const apiURL = sessionSyncURL(opts)
  if (apiURL === null) return false
  const timeoutMs = opts.timeoutMs ?? SESSION_SYNC_TIMEOUT_MS
  const options = sessionSyncRequestOptions(apiURL)
  if (options === null) {
    log("session_sync_failed", { reason, error: "unsupported_host_api_url" })
    return false
  }
  try {
    execFileSync(process.execPath, ["-e", SYNC_CHILD], {
      input: JSON.stringify({ options, timeoutMs }),
      timeout: timeoutMs + 1_000,
      stdio: ["pipe", "ignore", "ignore"],
    })
    log("session_sync_ok", { reason })
    return true
  } catch (err) {
    const status = (err as { status?: unknown })?.status
    log("session_sync_failed", { reason, exit: typeof status === "number" ? status : String(err) })
    return false
  }
}
