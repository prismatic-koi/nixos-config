// Tests for the credential files of the value layer
// (nx.programs.prism.agentCredentials).
//
// prism gives some credentials to a session as a file. A path variable holds
// the path, no variable holds the value, and PRISM_CREDENTIAL_FILES lists each
// path variable with the marker name of its credential. The value layer reads
// each listed file, so controls 1, 2, and 3 mask the value. The end-to-end
// test through the pi runtime is in prism.tool-result-redaction.test.ts.
//
// Every name and every credential value in this file is synthetic.
//
// Run with: tsx --test prism.credential-file-redaction.test.ts

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import * as fs from "node:fs"
import * as net from "node:net"
import * as os from "node:os"
import * as path from "node:path"

import {
  CREDENTIAL_FILES_ENV_NAME,
  CREDENTIAL_FILE_MAX_BYTES,
  isCredentialEnvName,
  makeFrameWriter,
  makeSecretRedactor,
  parseCredentialFiles,
  readCredentialFile,
  redactToolResult,
  REDACTION_MIN_VALUE_LENGTH,
} from "./prism.ts"

const PASSWORD = "synthetic-credential-file-password-0123456789abcdef"
const PASSWORD_ENV = "EXAMPLE_PASSWORD_FILE"
const MARKER = "[redacted:EXAMPLE_PASSWORD]"
const REGISTRY = { [CREDENTIAL_FILES_ENV_NAME]: `${PASSWORD_ENV}=EXAMPLE_PASSWORD` }

// The Go suite runs the same file (redact_credential_file_test.go), so a pass
// on both sides means the two parse each value in the same way.
const CREDENTIAL_FILES_CASES_PATH = path.join(
  __dirname,
  "../../prism/internal/payload/testdata/credential-files-cases.json",
)

function tempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "prism-credential-file-"))
}

function passwordFile(dir: string, content: string, name = "example_password"): string {
  const p = path.join(dir, name)
  fs.writeFileSync(p, content, { mode: 0o400 })
  return p
}

describe("the credential file registry variable", () => {
  it("parses each value of the shared case file as the Go redactor does", () => {
    const file = JSON.parse(fs.readFileSync(CREDENTIAL_FILES_CASES_PATH, "utf8")) as {
      cases: Array<{ name: string; value: string; want: Record<string, string> }>
    }
    assert.ok(file.cases.length > 0, "the case file holds no cases")
    for (const c of file.cases) {
      assert.deepEqual(Object.fromEntries(parseCredentialFiles(c.value)), c.want, c.name)
    }
  })

  it("gives an empty registry when the variable is not set", () => {
    assert.equal(parseCredentialFiles(undefined).size, 0)
  })

  it("is not a credential name, and neither is a path variable", () => {
    assert.equal(isCredentialEnvName(CREDENTIAL_FILES_ENV_NAME), false)
    assert.equal(isCredentialEnvName(PASSWORD_ENV), false)
  })
})

describe("readCredentialFile", () => {
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

describe("the value layer reads the file of each listed path variable", () => {
  it("masks the value in the output of cat, env, and printenv", () => {
    const dir = tempDir()
    try {
      const file = passwordFile(dir, `${PASSWORD}\n`)
      const r = makeSecretRedactor({ ...REGISTRY, [PASSWORD_ENV]: file, PATH: "/bin" })
      assert.equal(r.valueCount, 1)
      // cat "$EXAMPLE_PASSWORD_FILE"
      assert.equal(r.redact(`${PASSWORD}\n`), `${MARKER}\n`)
      // env, and printenv, after the agent copies the value into a variable.
      assert.equal(
        r.redact(`PATH=/bin\nK=${PASSWORD}\n${PASSWORD_ENV}=${file}\n`),
        `PATH=/bin\nK=${MARKER}\n${PASSWORD_ENV}=${file}\n`,
      )
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("gives each listed file its own marker", () => {
    const second = "synthetic-second-credential-token-fedcba9876"
    const r = makeSecretRedactor(
      {
        [CREDENTIAL_FILES_ENV_NAME]: `${PASSWORD_ENV}=EXAMPLE_PASSWORD,SECOND_TOKEN_FILE=SECOND_TOKEN`,
        [PASSWORD_ENV]: "/first",
        SECOND_TOKEN_FILE: "/second",
      },
      (p) => (p === "/first" ? PASSWORD : p === "/second" ? second : undefined),
    )
    assert.equal(r.redact(`${PASSWORD} ${second}`), `${MARKER} [redacted:SECOND_TOKEN]`)
  })

  it("masks the value in the tool result that pi keeps (control 3)", () => {
    const dir = tempDir()
    try {
      const r = makeSecretRedactor({ ...REGISTRY, [PASSWORD_ENV]: passwordFile(dir, PASSWORD) })
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

  it("masks the value in the frame on the prism socket (control 1)", () => {
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
        makeSecretRedactor({ ...REGISTRY, [PASSWORD_ENV]: passwordFile(dir, PASSWORD) }),
      )
      const body = (p: string) => `{"username":"agent","password":"${p}","remember":false}`
      writer.write({ type: "tool_result", id: "call_1", output: body(PASSWORD) })
      const line = written.join("")
      assert.ok(!line.includes(PASSWORD), "the value reached the socket")
      assert.equal(JSON.parse(line.trimEnd()).output, body(MARKER))
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })

  it("reads the file through the readFile argument", () => {
    const seen: string[] = []
    const r = makeSecretRedactor({ ...REGISTRY, [PASSWORD_ENV]: "/run/secrets/example_password" }, (p) => {
      seen.push(p)
      return PASSWORD
    })
    assert.deepEqual(seen, ["/run/secrets/example_password"])
    assert.equal(r.redact(PASSWORD), MARKER)
  })

  it("names the lexicographically first name when a variable holds the same value", () => {
    const r = makeSecretRedactor({ ...REGISTRY, ZULU_TOKEN: PASSWORD, [PASSWORD_ENV]: "x" }, () => PASSWORD)
    assert.equal(r.redact(PASSWORD), MARKER)
    const s = makeSecretRedactor({ ...REGISTRY, ALPHA_TOKEN: PASSWORD, [PASSWORD_ENV]: "x" }, () => PASSWORD)
    assert.equal(s.redact(PASSWORD), "[redacted:ALPHA_TOKEN]")
  })

  for (const [name, env] of [
    ["the registry is not set", { [PASSWORD_ENV]: "/x", PATH: "/bin" }],
    ["the registry names another variable", { [CREDENTIAL_FILES_ENV_NAME]: "OTHER_FILE=OTHER", [PASSWORD_ENV]: "/x" }],
    ["the registry entry is not a name", { [CREDENTIAL_FILES_ENV_NAME]: `${PASSWORD_ENV}=bad marker`, [PASSWORD_ENV]: "/x" }],
    ["the listed variable is not set", { ...REGISTRY }],
  ] as const) {
    it(`reads no file when ${name}`, () => {
      let calls = 0
      const r = makeSecretRedactor({ ...env }, () => {
        calls++
        return PASSWORD
      })
      assert.equal(calls, 0)
      assert.equal(r.valueCount, 0)
      assert.equal(r.redact(PASSWORD), PASSWORD)
    })
  }
})

describe("an empty or short credential file does not over-redact", () => {
  const ordinary = "abc1234 abc ok\nlisting a b c 1 2 3 4\n"

  for (const [name, content] of [
    ["empty", ""],
    ["whitespace only", " \n\t\n"],
    ["short", "a".repeat(REDACTION_MIN_VALUE_LENGTH - 1) + "\n"],
    ["short and common", "abc1234"],
  ] as const) {
    it(`leaves ordinary output unchanged for a credential file that is ${name}`, () => {
      const dir = tempDir()
      try {
        const r = makeSecretRedactor({ ...REGISTRY, [PASSWORD_ENV]: passwordFile(dir, content) })
        assert.equal(r.valueCount, 0)
        assert.equal(r.redact(ordinary), ordinary)
        assert.equal(redactToolResult({ content: [{ type: "text", text: ordinary }] }, r), undefined)
      } finally {
        fs.rmSync(dir, { recursive: true, force: true })
      }
    })
  }

  it("leaves ordinary output unchanged for a credential file that is missing", () => {
    const r = makeSecretRedactor({
      ...REGISTRY,
      [PASSWORD_ENV]: path.join(os.tmpdir(), "prism-credential-file-missing"),
    })
    assert.equal(r.valueCount, 0)
    assert.equal(r.redact(ordinary), ordinary)
  })
})
