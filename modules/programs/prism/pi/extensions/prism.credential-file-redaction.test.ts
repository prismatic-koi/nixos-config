// Tests for the credential files of the value layer (issue #3472).
//
// A session of a repo in nx.programs.prism.unifi.repos gets the UniFi password
// as a file. UNIFI_PASSWORD_FILE holds the path, and no variable holds the
// password. The value layer reads the file, so controls 1, 2, and 3 mask the
// password. The
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

const PASSWORD = "synthetic-unifi-password-0123456789abcdef"
const MARKER = "[redacted:UNIFI_PASSWORD]"

function tempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "prism-3472-"))
}

function passwordFile(dir: string, content: string, name = "unifi_password"): string {
  const p = path.join(dir, name)
  fs.writeFileSync(p, content, { mode: 0o400 })
  return p
}

describe("#3472: the credential file registry", () => {
  it("maps UNIFI_PASSWORD_FILE to the UNIFI_PASSWORD marker", () => {
    assert.deepEqual({ ...CREDENTIAL_FILE_ENV_NAMES }, { UNIFI_PASSWORD_FILE: "UNIFI_PASSWORD" })
  })

  it("does not treat the path variable as a credential", () => {
    assert.equal(isCredentialEnvName("UNIFI_PASSWORD_FILE"), false)
  })
})

describe("#3472: readCredentialFile", () => {
  it("returns the content without surrounding whitespace", () => {
    const dir = tempDir()
    try {
      assert.equal(readCredentialFile(passwordFile(dir, `${PASSWORD}\n`)), PASSWORD)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("refuses a missing path, a directory, and a file above the size limit", () => {
    const dir = tempDir()
    try {
      const big = passwordFile(dir, "k".repeat(CREDENTIAL_FILE_MAX_BYTES + 1), "big")
      const atLimit = passwordFile(dir, "k".repeat(CREDENTIAL_FILE_MAX_BYTES), "at-limit")
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

describe("#3472: the value layer reads the file that UNIFI_PASSWORD_FILE names", () => {
  it("masks the password in the output of cat, env, and printenv", () => {
    const dir = tempDir()
    try {
      const file = passwordFile(dir, `${PASSWORD}\n`)
      const r = makeSecretRedactor({ UNIFI_PASSWORD_FILE: file, PATH: "/bin" })
      assert.equal(r.valueCount, 1)
      // cat "$UNIFI_PASSWORD_FILE"
      assert.equal(r.redact(`${PASSWORD}\n`), `${MARKER}\n`)
      // env, and printenv, after the agent copies the password into a variable.
      assert.equal(
        r.redact(`PATH=/bin\nK=${PASSWORD}\nUNIFI_PASSWORD_FILE=${file}\n`),
        `PATH=/bin\nK=${MARKER}\nUNIFI_PASSWORD_FILE=${file}\n`,
      )
      assert.equal(r.redact(`${PASSWORD}\n`), `${MARKER}\n`)
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("masks the password in the tool result that pi keeps (control 3)", () => {
    const dir = tempDir()
    try {
      const r = makeSecretRedactor({ UNIFI_PASSWORD_FILE: passwordFile(dir, PASSWORD) })
      assert.deepEqual(
        redactToolResult(
          {
            content: [{ type: "text", text: `${PASSWORD}\n` }],
            structuredContent: { output: `${PASSWORD}\n`, exit_code: 0 },
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

  it("masks the password in the frame on the prism socket (control 1)", () => {
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
        makeSecretRedactor({ UNIFI_PASSWORD_FILE: passwordFile(dir, PASSWORD) }),
      )
      const body = (p: string) => `{"username":"agent-readonly","password":"${p}","remember":false}`
      writer.write({ type: "tool_result", id: "call_1", output: body(PASSWORD) })
      const line = written.join("")
      assert.ok(!line.includes(PASSWORD), "the password reached the socket")
      assert.equal(JSON.parse(line.trimEnd()).output, body(MARKER))
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("reads the file through the readFile argument", () => {
    const seen: string[] = []
    const r = makeSecretRedactor({ UNIFI_PASSWORD_FILE: "/run/secrets/unifi_password" }, (p) => {
      seen.push(p)
      return PASSWORD
    })
    assert.deepEqual(seen, ["/run/secrets/unifi_password"])
    assert.equal(r.redact(PASSWORD), MARKER)
  })

  it("names the lexicographically first name when a variable holds the same value", () => {
    const r = makeSecretRedactor({ ZULU_TOKEN: PASSWORD, UNIFI_PASSWORD_FILE: "x" }, () => PASSWORD)
    assert.equal(r.redact(PASSWORD), MARKER)
    const s = makeSecretRedactor({ ALPHA_TOKEN: PASSWORD, UNIFI_PASSWORD_FILE: "x" }, () => PASSWORD)
    assert.equal(s.redact(PASSWORD), "[redacted:ALPHA_TOKEN]")
  })

  it("reads no file when the variable is not set", () => {
    let calls = 0
    const r = makeSecretRedactor({ PATH: "/bin" }, () => {
      calls++
      return PASSWORD
    })
    assert.equal(calls, 0)
    assert.equal(r.valueCount, 0)
    assert.equal(r.redact(PASSWORD), PASSWORD)
  })
})

describe("#3472: an empty or short password file does not over-redact", () => {
  const ordinary = "abc1234 abc ok\nlisting a b c 1 2 3 4\n"

  for (const [name, content] of [
    ["empty", ""],
    ["whitespace only", " \n\t\n"],
    ["short", "a".repeat(REDACTION_MIN_VALUE_LENGTH - 1) + "\n"],
    ["short and common", "abc1234"],
  ] as const) {
    it(`leaves ordinary output unchanged for a password file that is ${name}`, () => {
      const dir = tempDir()
      try {
        const r = makeSecretRedactor({ UNIFI_PASSWORD_FILE: passwordFile(dir, content) })
        assert.equal(r.valueCount, 0)
        assert.equal(r.redact(ordinary), ordinary)
        assert.equal(redactToolResult({ content: [{ type: "text", text: ordinary }] }, r), undefined)
      } finally {
        fs.rmSync(dir, { recursive: true, force: true })
      }
    })
  }

  it("leaves ordinary output unchanged for a password file that is missing", () => {
    const r = makeSecretRedactor({ UNIFI_PASSWORD_FILE: path.join(os.tmpdir(), "prism-3472-missing") })
    assert.equal(r.valueCount, 0)
    assert.equal(r.redact(ordinary), ordinary)
  })
})
