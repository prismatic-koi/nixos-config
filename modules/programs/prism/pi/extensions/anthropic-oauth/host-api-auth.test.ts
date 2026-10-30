// Tests for the host-API credential of the extension (host-api-auth.ts, #3379).
// Run with: tsx --test host-api-auth.test.ts

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import { HOST_API_SECRET_FILE, HOST_API_SECRET_HEADER, hostApiAuthHeaders } from "./host-api-auth.ts"

let dir: string

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "pi-host-api-auth-test-"))
})

afterEach(() => {
  rmSync(dir, { recursive: true, force: true })
})

describe("hostApiAuthHeaders", () => {
  it("reads the secret file in the dir of the socket", () => {
    writeFileSync(join(dir, HOST_API_SECRET_FILE), "0123abcd\n")
    assert.deepEqual(hostApiAuthHeaders(join(dir, "hostapi.sock")), { [HOST_API_SECRET_HEADER]: "0123abcd" })
  })

  it("reads the file at each call, so it gets the secret of a restarted sidecar", () => {
    const sock = join(dir, "hostapi.sock")
    writeFileSync(join(dir, HOST_API_SECRET_FILE), "first")
    assert.equal(hostApiAuthHeaders(sock)[HOST_API_SECRET_HEADER], "first")
    writeFileSync(join(dir, HOST_API_SECRET_FILE), "second")
    assert.equal(hostApiAuthHeaders(sock)[HOST_API_SECRET_HEADER], "second")
  })

  it("is empty, and does not throw, with no file or an empty file", () => {
    const sock = join(dir, "hostapi.sock")
    assert.deepEqual(hostApiAuthHeaders(sock), {})
    writeFileSync(join(dir, HOST_API_SECRET_FILE), " \n")
    assert.deepEqual(hostApiAuthHeaders(sock), {})
  })
})
