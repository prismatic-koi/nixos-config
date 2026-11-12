// Tests for the credential files of the value layer (issue #3472).
//
// A session of a repo in nx.programs.prism.unifi.repos gets the UniFi API key
// as a file. UNIFI_API_KEY_FILE holds the path, and no variable holds the key.
// The value layer reads the file, so controls 1, 2, and 3 mask the key. The
// end-to-end test through the pi runtime is in
// prism.tool-result-redaction.test.ts.
//
// Every credential value in this file is synthetic.
//
// Run with: tsx --test prism.credential-file-redaction.test.ts

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import * as fs from "node:fs"
import * as net from "node:net"
import * as os from "node:os"
import * as path from "node:path"

import {
  CREDENTIAL_FILE_ENV_NAMES,
  CREDENTIAL_FILE_MAX_BYTES,
  isCredentialEnvName,
  makeFrameWriter,
  makeSecretRedactor,
  readCredentialFile,
  redactToolResult,
  REDACTION_MIN_VALUE_LENGTH,
} from "./prism.ts"

const KEY = "synthetic-unifi-key-0123456789abcdef"
const MARKER = "[redacted:UNIFI_API_KEY]"

function tempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "prism-3472-"))
}

function keyFile(dir: string, content: string, name = "unifi_api_key"): string {
  const p = path.join(dir, name)
  fs.writeFileSync(p, content, { mode: 0o400 })
  return p
}

describe("#3472: the credential file registry", () => {
  it("maps UNIFI_API_KEY_FILE to the UNIFI_API_KEY marker", () => {
    assert.equal(CREDENTIAL_FILE_ENV_NAMES.UNIFI_API_KEY_FILE, "UNIFI_API_KEY")
  })

  it("does not treat the path variable as a credential", () => {
    assert.equal(isCredentialEnvName("UNIFI_API_KEY_FILE"), false)
  })
})

describe("#3472: readCredentialFile", () => {
  it("returns the content without surrounding whitespace", () => {
    const dir = tempDir()
    try {
      assert.equal(readCredentialFile(keyFile(dir, `${KEY}\n`)), KEY)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("refuses a missing path, a directory, and a file above the size limit", () => {
    const dir = tempDir()
    try {
      const big = keyFile(dir, "k".repeat(CREDENTIAL_FILE_MAX_BYTES + 1), "big")
      const atLimit = keyFile(dir, "k".repeat(CREDENTIAL_FILE_MAX_BYTES), "at-limit")
      assert.equal(readCredentialFile(""), undefined)
      assert.equal(readCredentialFile(path.join(dir, "missing")), undefined)
      assert.equal(readCredentialFile(dir), undefined)
      assert.equal(readCredentialFile(big), undefined)
      assert.equal(readCredentialFile(atLimit)?.length, CREDENTIAL_FILE_MAX_BYTES)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })
})

describe("#3472: the value layer reads the file that UNIFI_API_KEY_FILE names", () => {
  it("masks the key in the output of cat, env, and printenv", () => {
    const dir = tempDir()
    try {
      const file = keyFile(dir, `${KEY}\n`)
      const r = makeSecretRedactor({ UNIFI_API_KEY_FILE: file, PATH: "/bin" })
      assert.equal(r.valueCount, 1)
      // cat "$UNIFI_API_KEY_FILE"
      assert.equal(r.redact(`${KEY}\n`), `${MARKER}\n`)
      // env, and printenv, after the agent copies the key into a variable.
      assert.equal(
        r.redact(`PATH=/bin\nK=${KEY}\nUNIFI_API_KEY_FILE=${file}\n`),
        `PATH=/bin\nK=${MARKER}\nUNIFI_API_KEY_FILE=${file}\n`,
      )
      assert.equal(r.redact(`${KEY}\n`), `${MARKER}\n`)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("masks the key in the tool result that pi keeps (control 3)", () => {
    const dir = tempDir()
    try {
      const r = makeSecretRedactor({ UNIFI_API_KEY_FILE: keyFile(dir, KEY) })
      assert.deepEqual(
        redactToolResult(
          {
            content: [{ type: "text", text: `${KEY}\n` }],
            structuredContent: { output: `${KEY}\n`, exit_code: 0 },
          },
          r,
        ),
        {
          content: [{ type: "text", text: `${MARKER}\n` }],
          structuredContent: { output: `${MARKER}\n`, exit_code: 0 },
        },
      )
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("masks the key in the frame on the prism socket (control 1)", () => {
    const dir = tempDir()
    try {
      const written: string[] = []
      const socket = {
        write: (data: string) => {
          written.push(data)
          return true
        },
        end: () => {},
      } as unknown as net.Socket
      const writer = makeFrameWriter(
        socket,
        makeSecretRedactor({ UNIFI_API_KEY_FILE: keyFile(dir, KEY) }),
      )
      writer.write({ type: "tool_result", id: "call_1", output: `X-API-KEY: ${KEY}` })
      const line = written.join("")
      assert.ok(!line.includes(KEY), "the key reached the socket")
      assert.equal(JSON.parse(line.trimEnd()).output, `X-API-KEY: ${MARKER}`)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("reads the file through the readFile argument", () => {
    const seen: string[] = []
    const r = makeSecretRedactor({ UNIFI_API_KEY_FILE: "/run/secrets/unifi_api_key" }, (p) => {
      seen.push(p)
      return KEY
    })
    assert.deepEqual(seen, ["/run/secrets/unifi_api_key"])
    assert.equal(r.redact(KEY), MARKER)
  })

  it("names the lexicographically first name when a variable holds the same value", () => {
    const r = makeSecretRedactor({ ZULU_TOKEN: KEY, UNIFI_API_KEY_FILE: "x" }, () => KEY)
    assert.equal(r.redact(KEY), MARKER)
    const s = makeSecretRedactor({ ALPHA_TOKEN: KEY, UNIFI_API_KEY_FILE: "x" }, () => KEY)
    assert.equal(s.redact(KEY), "[redacted:ALPHA_TOKEN]")
  })

  it("reads no file when the variable is not set", () => {
    let calls = 0
    const r = makeSecretRedactor({ PATH: "/bin" }, () => {
      calls++
      return KEY
    })
    assert.equal(calls, 0)
    assert.equal(r.valueCount, 0)
    assert.equal(r.redact(KEY), KEY)
  })
})

describe("#3472: an empty or short key file does not over-redact", () => {
  const ordinary = "abc1234 abc ok\nlisting a b c 1 2 3 4\n"

  for (const [name, content] of [
    ["empty", ""],
    ["whitespace only", " \n\t\n"],
    ["short", "a".repeat(REDACTION_MIN_VALUE_LENGTH - 1) + "\n"],
    ["short and common", "abc1234"],
  ] as const) {
    it(`leaves ordinary output unchanged for a key file that is ${name}`, () => {
      const dir = tempDir()
      try {
        const r = makeSecretRedactor({ UNIFI_API_KEY_FILE: keyFile(dir, content) })
        assert.equal(r.valueCount, 0)
        assert.equal(r.redact(ordinary), ordinary)
        assert.equal(redactToolResult({ content: [{ type: "text", text: ordinary }] }, r), undefined)
      } finally {
        fs.rmSync(dir, { recursive: true, force: true })
      }
    })
  }

  it("leaves ordinary output unchanged for a key file that is missing", () => {
    const r = makeSecretRedactor({ UNIFI_API_KEY_FILE: path.join(os.tmpdir(), "prism-3472-missing") })
    assert.equal(r.valueCount, 0)
    assert.equal(r.redact(ordinary), ordinary)
  })
})
