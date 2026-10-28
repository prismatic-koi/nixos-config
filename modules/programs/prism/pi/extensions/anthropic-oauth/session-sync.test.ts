// Tests for the sync of the per-session auth.json (session-sync.ts, #3295).
// Run with: tsx --test session-sync.test.ts
//
// The unit tests drive syncSessionAuth and syncSessionAuthSync against a
// fake sidecar on a Unix socket. The cross-process tests start two real
// pi-like processes, each with its own auth.json, that need a refresh at
// the same time. A fake sidecar per session copies the anthropic entry
// between the session file and a shared host file, as the prism sidecar
// does. The tests count the refresh requests.
//
// Each test uses its own temp dir, so no test reads or writes the real home
// dir.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { spawn, type ChildProcess } from "node:child_process"
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http"
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"

import {
  SESSION_SYNC_ENV,
  SESSION_SYNC_PATH,
  sessionSyncRequestOptions,
  sessionSyncURL,
  syncSessionAuth,
  syncSessionAuthSync,
} from "./session-sync.ts"

const HERE = dirname(fileURLToPath(import.meta.url))

let tempDir: string

beforeEach(() => {
  tempDir = mkdtempSync(join(tmpdir(), "pi-session-sync-test-"))
})

afterEach(() => {
  rmSync(tempDir, { recursive: true, force: true })
})

interface FakeSidecar {
  server: Server
  socket: string
  requests: { method: string; url: string; body: string }[]
  close(): Promise<void>
}

// A fake sidecar on a Unix socket. handle runs for each request.
function fakeSidecar(name: string, handle: (res: ServerResponse) => void): Promise<FakeSidecar> {
  const socket = join(tempDir, `${name}.sock`)
  const requests: FakeSidecar["requests"] = []
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    let body = ""
    req.on("data", (c) => (body += c))
    req.on("end", () => {
      requests.push({ method: req.method ?? "", url: req.url ?? "", body })
      handle(res)
    })
  })
  return new Promise((resolve) => {
    server.listen(socket, () =>
      resolve({
        server,
        socket,
        requests,
        close: () => new Promise((r) => server.close(() => r())),
      }),
    )
  })
}

describe("sessionSyncURL", () => {
  it("is null unless PRISM_PI_AUTH_SYNC is 1 and PRISM_HOST_API is set", () => {
    const saved = { sync: process.env[SESSION_SYNC_ENV], api: process.env.PRISM_HOST_API }
    try {
      delete process.env[SESSION_SYNC_ENV]
      process.env.PRISM_HOST_API = "unix:///x.sock"
      assert.equal(sessionSyncURL(), null)
      process.env[SESSION_SYNC_ENV] = "1"
      assert.equal(sessionSyncURL(), "unix:///x.sock")
      delete process.env.PRISM_HOST_API
      assert.equal(sessionSyncURL(), null)
    } finally {
      if (saved.sync === undefined) delete process.env[SESSION_SYNC_ENV]
      else process.env[SESSION_SYNC_ENV] = saved.sync
      if (saved.api === undefined) delete process.env.PRISM_HOST_API
      else process.env.PRISM_HOST_API = saved.api
    }
  })

  it("builds the request for a Unix socket and for TCP, and refuses other shapes", () => {
    assert.deepEqual(sessionSyncRequestOptions("unix:///run/x/hostapi.sock"), {
      socketPath: "/run/x/hostapi.sock",
      host: "prism-hostapi",
      method: "POST",
      path: SESSION_SYNC_PATH,
    })
    const tcp = sessionSyncRequestOptions("http://127.0.0.1:4321")
    assert.equal(tcp?.hostname, "127.0.0.1")
    assert.equal(tcp?.port, "4321")
    assert.equal(tcp?.path, SESSION_SYNC_PATH)
    assert.equal(sessionSyncRequestOptions("unix://"), null)
    assert.equal(sessionSyncRequestOptions("tcp://127.0.0.1:1"), null)
  })
})

describe("syncSessionAuth", () => {
  it("POSTs to /pi-auth/sync with no body, and is true on a 2xx answer", async () => {
    const sidecar = await fakeSidecar("ok", (res) => res.end('{"ok":true}'))
    try {
      assert.equal(await syncSessionAuth("pull", { apiURL: `unix://${sidecar.socket}` }), true)
      assert.deepEqual(sidecar.requests, [{ method: "POST", url: SESSION_SYNC_PATH, body: "" }])
    } finally {
      await sidecar.close()
    }
  })

  it("is false, and does not throw, on an error answer, no sidecar, or a timeout", async () => {
    const failing = await fakeSidecar("fail", (res) => {
      res.statusCode = 503
      res.end('{"error":"busy"}')
    })
    const silent = await fakeSidecar("silent", () => {})
    try {
      assert.equal(await syncSessionAuth("push", { apiURL: `unix://${failing.socket}` }), false)
      assert.equal(await syncSessionAuth("push", { apiURL: `unix://${join(tempDir, "none.sock")}` }), false)
      const started = Date.now()
      assert.equal(await syncSessionAuth("push", { apiURL: `unix://${silent.socket}`, timeoutMs: 100 }), false)
      assert.ok(Date.now() - started < 2_000, "the timeout did not stop the wait")
      assert.equal(await syncSessionAuth("push", { apiURL: "" }), false)
    } finally {
      silent.server.closeAllConnections()
      await failing.close()
      await silent.close()
    }
  })
})

// The sync form blocks the event loop, so its sidecar runs in a child.
const SIDECAR_CHILD = `
const http = require("node:http")
const [socket, status, ready] = process.argv.slice(1)
http.createServer((req, res) => { res.statusCode = Number(status); res.end("{}") })
  .listen(socket, () => require("node:fs").writeFileSync(ready, ""))
`

async function childSidecar(status: number): Promise<{ socket: string; child: ChildProcess }> {
  const socket = join(tempDir, `child-${status}.sock`)
  const ready = join(tempDir, `child-${status}.ready`)
  const child = spawn(process.execPath, ["-e", SIDECAR_CHILD, socket, String(status), ready], { stdio: "ignore" })
  const deadline = Date.now() + 10_000
  while (!existsSync(ready)) {
    if (Date.now() > deadline) throw new Error("the child sidecar did not start")
    await new Promise((r) => setTimeout(r, 10))
  }
  return { socket, child }
}

describe("syncSessionAuthSync", () => {
  it("is true on a 2xx answer, and false on an error answer or no sidecar", async () => {
    const ok = await childSidecar(200)
    const failing = await childSidecar(503)
    try {
      assert.equal(syncSessionAuthSync("pull", { apiURL: `unix://${ok.socket}` }), true)
      assert.equal(syncSessionAuthSync("pull", { apiURL: `unix://${failing.socket}` }), false)
      assert.equal(syncSessionAuthSync("pull", { apiURL: `unix://${join(tempDir, "none.sock")}` }), false)
      assert.equal(syncSessionAuthSync("pull", { apiURL: "tcp://x" }), false)
    } finally {
      ok.child.kill()
      failing.child.kill()
    }
  })
})

// ---------------------------------------------------------------------------
// Cross-process tests
// ---------------------------------------------------------------------------

// The tsx loader flags of this test process, so that a child can import .ts.
function tsxLoaderArgs(): string[] {
  const out: string[] = []
  const argv = process.execArgv
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if ((a === "--import" || a === "--require") && argv[i + 1]?.includes("tsx")) {
      out.push(a, argv[i + 1])
      i++
    } else if ((a.startsWith("--import=") || a.startsWith("--require=")) && a.includes("tsx")) {
      out.push(a)
    }
  }
  return out
}

// The child: a sandboxed pi session that needs a refresh, with its own
// auth.json (PI_AUTH_JSON). The fake token endpoint records each request in
// a shared file and answers after 300 ms, so that the two refreshes overlap
// if nothing serialises them.
const CHILD = `
import { appendFileSync, existsSync, writeFileSync } from "node:fs"
import { join } from "node:path"
const [mode, id, dir] = process.argv.slice(2)
const auth = await import(${JSON.stringify(pathToFileURL(join(HERE, "auth.ts")).href)})
const creds = await import(${JSON.stringify(pathToFileURL(join(HERE, "credentials.ts")).href)})
const requests = join(dir, "requests")
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
globalThis.fetch = async (_url, init) => {
  const rt = new URLSearchParams(String(init.body)).get("refresh_token")
  appendFileSync(requests, id + " " + rt + "\\n")
  await sleep(300)
  return new Response(JSON.stringify({ access_token: "access-" + id, refresh_token: "refresh-" + id, expires_in: 36000 }), { status: 200 })
}
const syncRefresh = (rt) => {
  appendFileSync(requests, id + " " + rt + "\\n")
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 300)
  return { accessToken: "access-" + id, refreshToken: "refresh-" + id, expiresAt: Date.now() + 36000000 }
}
writeFileSync(join(dir, "ready-" + id), "")
while (!(existsSync(join(dir, "ready-a")) && existsSync(join(dir, "ready-b")))) await sleep(5)
let access
if (mode === "async") {
  access = (await auth.refreshAnthropicToken({ access: "old-access", refresh: "old-refresh", expires: Date.now() + 20000 })).access
} else {
  access = creds.refreshIfNeeded({ accessToken: "old-access", refreshToken: "old-refresh", expiresAt: Date.now() + 20000 }, syncRefresh)?.accessToken
}
process.stdout.write(JSON.stringify({ access }))
`

const OLD = { type: "oauth", access: "old-access", refresh: "old-refresh" }

function readJSON(path: string): Record<string, any> {
  return JSON.parse(readFileSync(path, "utf8"))
}

// A fake prism sidecar for one session: it copies a new anthropic entry
// from the session file to the host file when the host entry did not
// change, then writes the host file to the session file. The rules of
// prism/internal/container/pi_auth_session.go, for one provider.
function syncingSidecar(name: string, sessionFile: string, hostFile: string): Promise<FakeSidecar> {
  let current = JSON.stringify(readJSON(hostFile).anthropic)
  const seen = new Set([current])
  return fakeSidecar(name, (res) => {
    const host = readJSON(hostFile)
    const session = JSON.stringify(readJSON(sessionFile).anthropic)
    if (!seen.has(session) && JSON.stringify(host.anthropic) === current) {
      host.anthropic = JSON.parse(session)
      writeFileSync(hostFile, JSON.stringify(host))
    }
    seen.add(session)
    writeFileSync(sessionFile, JSON.stringify(host))
    current = JSON.stringify(host.anthropic)
    seen.add(current)
    res.end('{"ok":true}')
  })
}

function runChild(mode: "async" | "sync", id: "a" | "b", env: NodeJS.ProcessEnv): Promise<{ access: string | undefined }> {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [...tsxLoaderArgs(), join(tempDir, "child.mts"), mode, id, tempDir], {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    })
    let out = ""
    let err = ""
    child.stdout.on("data", (d) => (out += d))
    child.stderr.on("data", (d) => (err += d))
    child.on("error", reject)
    child.on("close", (code) => {
      if (code !== 0) return reject(new Error(`child ${id} exited ${code}: ${err}`))
      try {
        resolve(JSON.parse(out))
      } catch {
        reject(new Error(`child ${id} wrote ${JSON.stringify(out)}; stderr: ${err}`))
      }
    })
  })
}

async function runSandboxedPair(modeA: "async" | "sync", modeB: "async" | "sync", sync: boolean) {
  writeFileSync(join(tempDir, "child.mts"), CHILD)
  const old = JSON.stringify({ anthropic: { ...OLD, expires: Date.now() + 20_000 } })
  const host = join(tempDir, "host-auth.json")
  const files = { a: join(tempDir, "auth-a.json"), b: join(tempDir, "auth-b.json") }
  for (const f of [host, files.a, files.b]) writeFileSync(f, old)
  const sidecars = {
    a: await syncingSidecar("sidecar-a", files.a, host),
    b: await syncingSidecar("sidecar-b", files.b, host),
  }
  const envFor = (id: "a" | "b"): NodeJS.ProcessEnv => {
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      PI_AUTH_JSON: files[id],
      PI_ANTHROPIC_REFRESH_LOCK_DIR: join(tempDir, "pi-locks"),
      PRISM_HOST_API: `unix://${sidecars[id].socket}`,
    }
    delete env.PI_ANTHROPIC_OAUTH_DEBUG
    if (sync) env[SESSION_SYNC_ENV] = "1"
    else delete env[SESSION_SYNC_ENV]
    return env
  }
  try {
    const [a, b] = await Promise.all([runChild(modeA, "a", envFor("a")), runChild(modeB, "b", envFor("b"))])
    const requests = readFileSync(join(tempDir, "requests"), "utf8").trim().split("\n")
    return { a, b, requests, host: readJSON(host).anthropic, syncCalls: sidecars.a.requests.length + sidecars.b.requests.length }
  } finally {
    await sidecars.a.close()
    await sidecars.b.close()
  }
}

describe("two sandboxed sessions with their own auth.json that need a refresh at the same time", { timeout: 60_000 }, () => {
  for (const [modeA, modeB] of [
    ["async", "async"],
    ["sync", "sync"],
    ["async", "sync"],
  ] as const) {
    it(`send one refresh request (${modeA} path and ${modeB} path), and both use the new token, which reaches the host`, async () => {
      const { a, b, requests, host, syncCalls } = await runSandboxedPair(modeA, modeB, true)

      assert.equal(requests.length, 1, `refresh requests: ${JSON.stringify(requests)}`)
      const [winner, sentToken] = requests[0].split(" ")
      assert.equal(sentToken, "old-refresh")
      assert.equal(a.access, `access-${winner}`)
      assert.equal(b.access, `access-${winner}`)
      assert.equal(host.access, `access-${winner}`)
      assert.equal(host.refresh, `refresh-${winner}`)
      assert.ok(syncCalls >= 3, `sync calls: ${syncCalls}`)
    })
  }

  // The no-op proof: with no sync, each session reads only its own file,
  // and both send the old refresh token, although they share the lock. So
  // the one-request assertions above can fail.
  it("send two refresh requests with the same token when the sessions do not sync", async () => {
    const { requests, host, syncCalls } = await runSandboxedPair("async", "async", false)
    assert.equal(requests.length, 2, `refresh requests: ${JSON.stringify(requests)}`)
    assert.deepEqual(requests.map((r) => r.split(" ")[1]), ["old-refresh", "old-refresh"])
    assert.equal(host.access, "old-access")
    assert.equal(syncCalls, 0)
  })
})
