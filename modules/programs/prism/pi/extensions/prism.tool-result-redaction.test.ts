// Tests for the tool_result redaction handler (issue #3420).
//
// The unit tests drive redactToolResult with a redactor built from synthetic
// values. The end-to-end test runs a real agent session from the installed
// pi package, with pi's faux model provider. It checks what the model gets
// and what the session transcript holds. A mock cannot show either of these.
//
// Every credential value in this file is synthetic.
//
// Run with: tsx --test prism.tool-result-redaction.test.ts

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { execFileSync } from "node:child_process"
import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import { pathToFileURL } from "node:url"

import prismExtension, {
  isCredentialEnvName,
  makeSecretRedactor,
  redactToolResult,
  REDACTION_MIN_VALUE_LENGTH,
  resetDefaultSecretRedactor,
} from "./prism.ts"

const SYNTHETIC_VALUE = "SYNTHETIC-TOOL-RESULT-VALUE-0123456789"
const SYNTHETIC_NAME = "PRISM_TEST_SYNTHETIC_TOKEN"
const MARKER = `[redacted:${SYNTHETIC_NAME}]`
const SHAPE_TOKEN = `ghp_${"A".repeat(36)}`
const SHAPE_MARKER = "[redacted:github-token]"
// A valid 1x1 PNG. Its base64 holds no shape trigger.
const PNG_1X1 =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

type Item = { type: "text"; text: string } | { type: "image"; data: string; mimeType: string }

const text = (t: string): Item => ({ type: "text", text: t })
const image = (): Item => ({ type: "image", data: PNG_1X1, mimeType: "image/png" })
const redactor = () => makeSecretRedactor({ [SYNTHETIC_NAME]: SYNTHETIC_VALUE })

describe("#3420: redactToolResult", () => {
  it("replaces a credential value in a text item with the variable marker", () => {
    const result = redactToolResult(
      { content: [text(`${SYNTHETIC_NAME}=${SYNTHETIC_VALUE}\n`)] },
      redactor(),
    )
    assert.deepEqual(result, { content: [text(`${SYNTHETIC_NAME}=${MARKER}\n`)] })
  })

  it("replaces a shape-layer token that is not in the environment", () => {
    const result = redactToolResult(
      { content: [text(`token ${SHAPE_TOKEN} end`)] },
      makeSecretRedactor({}),
    )
    assert.deepEqual(result, { content: [text(`token ${SHAPE_MARKER} end`)] })
  })

  it("redacts each string scalar of structuredContent and keeps every other field", () => {
    const event = {
      content: [text("ok")],
      structuredContent: {
        output: `t=${SYNTHETIC_VALUE}`,
        exit_code: 0,
        truncated: false,
        nested: { list: ["a", 1, null, true, `x ${SYNTHETIC_VALUE}`] },
      },
    }
    const result = redactToolResult(event, redactor())
    assert.deepEqual(result, {
      content: [text("ok")],
      structuredContent: {
        output: `t=${MARKER}`,
        exit_code: 0,
        truncated: false,
        nested: { list: ["a", 1, null, true, `x ${MARKER}`] },
      },
    })
  })

  it("redacts a structuredContent that is a bare string, as the read tool sets it", () => {
    const result = redactToolResult(
      { content: [text("clean")], structuredContent: `token=${SYNTHETIC_VALUE}\n` },
      redactor(),
    )
    assert.deepEqual(result, { content: [text("clean")], structuredContent: `token=${MARKER}\n` })
  })

  it("returns no result and changes nothing when no value matches", () => {
    const events = [
      { content: [text("PASS\nok\tgithub.com/prismatic-koi/prism\t0.4s\n")] },
      { content: [text("ok")], structuredContent: { output: "ok", exit_code: 0, list: [1, "two"] } },
      { content: [image()] },
      { content: [image(), text("caption")], structuredContent: "caption" },
      { content: [] },
      { content: [text(`already ${MARKER} and ${SHAPE_MARKER}`)] },
      { content: [text("ok")], details: { truncation: { content: "ok", truncated: true } } },
    ]
    for (const event of events) {
      const before = structuredClone(event)
      assert.equal(redactToolResult(event, redactor()), undefined, JSON.stringify(before))
      assert.deepEqual(event, before, "the handler mutated the event")
    }
  })

  it("returns the original structuredContent when only content changed", () => {
    // Pi drops structuredContent when a handler replaces content alone.
    const structured = { output: "clean", exit_code: 0 }
    const result = redactToolResult(
      { content: [text(`t=${SYNTHETIC_VALUE}`)], structuredContent: structured },
      redactor(),
    )
    assert.ok(result)
    assert.equal(result.structuredContent, structured)
    assert.deepEqual(result.structuredContent, { output: "clean", exit_code: 0 })
  })

  it("redacts details, where pi keeps the shown text of a truncated result", () => {
    const event = {
      content: [text("clean")],
      structuredContent: { exit_code: 0 },
      details: {
        truncation: { content: `line\n${SYNTHETIC_VALUE}\n`, truncated: true, totalLines: 3001 },
        fullOutputPath: "/tmp/pi-bash-1.log",
      },
    }
    assert.deepEqual(redactToolResult(event, redactor()), {
      content: [text("clean")],
      structuredContent: { exit_code: 0 },
      details: {
        truncation: { content: `line\n${MARKER}\n`, truncated: true, totalLines: 3001 },
        fullOutputPath: "/tmp/pi-bash-1.log",
      },
    })
  })

  it("returns no details when only content changed, so pi keeps the original", () => {
    const result = redactToolResult(
      { content: [text(`t=${SYNTHETIC_VALUE}`)], details: { diff: "clean" } },
      redactor(),
    )
    assert.deepEqual(result, { content: [text(`t=${MARKER}`)] })
  })

  it("passes an image item through unchanged", () => {
    const img = image()
    const result = redactToolResult({ content: [img, text(`t=${SYNTHETIC_VALUE}`)] }, redactor())
    assert.ok(result?.content)
    assert.equal(result.content[0], img)
    assert.deepEqual(result.content[0], image())
    assert.deepEqual(result.content[1], text(`t=${MARKER}`))
  })

  it("does not redact a value shorter than the minimum length", () => {
    const short = "s".repeat(REDACTION_MIN_VALUE_LENGTH - 1)
    const tooShort = makeSecretRedactor({ SHORT_TOKEN: short })
    assert.equal(
      redactToolResult({ content: [text(`v=${short}`)], structuredContent: { v: short } }, tooShort),
      undefined,
    )

    const edge = "s".repeat(REDACTION_MIN_VALUE_LENGTH)
    assert.deepEqual(
      redactToolResult({ content: [text(`v=${edge}`)] }, makeSecretRedactor({ EDGE_TOKEN: edge })),
      { content: [text("v=[redacted:EDGE_TOKEN]")] },
    )
  })

  it("redacts one scalar at a time, so a shape cannot span two items or two fields", () => {
    const event = {
      content: [text("a -----BEGIN RSA PRIVATE KEY-----"), text("-----END RSA PRIVATE KEY-----")],
      structuredContent: { a: "-----BEGIN A PRIVATE KEY-----", b: [1, "-----END A PRIVATE KEY-----"] },
    }
    assert.equal(redactToolResult(event, makeSecretRedactor({})), undefined)

    const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGlj\n-----END OPENSSH PRIVATE KEY-----"
    assert.deepEqual(
      redactToolResult({ content: [text(`k=${pem}`)] }, makeSecretRedactor({})),
      { content: [text("k=[redacted:private-key-block]")] },
    )
  })

  it("replaces a complete private-key block in each part of a tool result (#3437)", () => {
    const pem = "-----BEGIN RSA PRIVATE KEY-----\nc3ludGhldGlj\n-----END RSA PRIVATE KEY-----"
    const marker = "[redacted:private-key-block]"
    assert.deepEqual(
      redactToolResult(
        {
          content: [text(`cat id_rsa\n${pem}\n`)],
          structuredContent: { output: `${pem}\n`, exit_code: 0 },
          details: { truncation: { content: `${pem}\n`, truncated: true } },
        },
        makeSecretRedactor({}),
      ),
      {
        content: [text(`cat id_rsa\n${marker}\n`)],
        structuredContent: { output: `${marker}\n`, exit_code: 0 },
        details: { truncation: { content: `${marker}\n`, truncated: true } },
      },
    )
  })

  it("replaces a private-key block after the first 50 KB of a 1 MiB output (#3437)", () => {
    const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGlj\n-----END OPENSSH PRIVATE KEY-----\n"
    const head = "y".repeat(63) + "\n"
    const before = head.repeat(1024)
    const after = head.repeat(Math.floor(((1 << 20) - before.length - pem.length) / head.length))
    const result = redactToolResult(
      { content: [text("ok")], structuredContent: { output: before + pem + after } },
      makeSecretRedactor({}),
    )
    const output = (result?.structuredContent as { output?: string } | undefined)?.output
    assert.ok(before.length > 50_000)
    assert.ok(output === `${before}[redacted:private-key-block]\n${after}`, "the block was not redacted")
  })

  it("redacts a bash-shaped result of BEGIN lines with no END line in less than 100 ms (#3437)", () => {
    // Both line forms, at the sizes of the pi 1.5.0 bash tool: 50 KiB for
    // content and details, 1 MiB for structuredContent.output.
    const lines = "-----BEGIN RSA PRIVATE KEY-----\n-----BEGIN A PRIVATE KEY-----\n"
    const of = (size: number) => lines.repeat(Math.floor(size / lines.length))
    const event = {
      content: [text(of(50 << 10))],
      structuredContent: { output: of(1 << 20), exit_code: 0 },
      details: { truncation: { content: of(50 << 10), truncated: true } },
    }
    const r = redactor()
    assert.equal(redactToolResult(event, r), undefined)
    // The best of three runs, so a GC pause or a busy CI runner does not
    // fail the bound.
    let best = Infinity
    for (let i = 0; i < 3; i++) {
      const start = performance.now()
      redactToolResult(event, r)
      best = Math.min(best, performance.now() - start)
    }
    assert.ok(best < 100, `took ${best.toFixed(1)} ms`)
  })

  it("is idempotent: a second pass over a redacted result returns no result", () => {
    const first = redactToolResult(
      { content: [text(`t=${SYNTHETIC_VALUE}`)], structuredContent: { t: SYNTHETIC_VALUE } },
      redactor(),
    )
    assert.ok(first?.content)
    assert.equal(
      redactToolResult(
        { content: first.content, structuredContent: first.structuredContent },
        redactor(),
      ),
      undefined,
    )
  })

  it("leaves a missing content array as it is and still redacts structuredContent", () => {
    const result = redactToolResult(
      { content: undefined as never, structuredContent: { t: SYNTHETIC_VALUE } },
      redactor(),
    )
    assert.deepEqual(result, { structuredContent: { t: MARKER } })
  })
})

// Sets `vars` for the duration of `fn` and removes every other
// credential-named variable, so the default redactor sees only synthetic
// values. Restores the environment and the cached redactor after.
async function withEnv<T>(
  vars: Record<string, string | undefined>,
  fn: () => Promise<T>,
): Promise<T> {
  const saved = { ...process.env }
  for (const name of Object.keys(process.env)) {
    if (isCredentialEnvName(name)) delete process.env[name]
  }
  for (const [name, value] of Object.entries(vars)) {
    if (value === undefined) delete process.env[name]
    else process.env[name] = value
  }
  resetDefaultSecretRedactor()
  try {
    return await fn()
  } finally {
    for (const name of Object.keys(process.env)) {
      if (!(name in saved)) delete process.env[name]
    }
    Object.assign(process.env, saved)
    resetDefaultSecretRedactor()
  }
}

// A prism session with no harness pipe. The pipe variables are cleared so the
// extension cannot dial the sidecar of the session that runs the tests.
const PRISM_SESSION_ENV = {
  PRISM_SESSION_NAME: "prism-test@tool-result-redaction",
  PRISM_HARNESS_PIPE: undefined,
  PRISM_HARNESS_PIPE_SECRET_FILE: undefined,
  [SYNTHETIC_NAME]: SYNTHETIC_VALUE,
}

type Handler = (event: unknown, ctx: unknown) => unknown

function registeredHandlers(): Map<string, Handler[]> {
  const handlers = new Map<string, Handler[]>()
  const pi = new Proxy(
    {},
    {
      get: (_target, prop) =>
        prop === "on"
          ? (event: string, handler: Handler) => {
              handlers.set(event, [...(handlers.get(event) ?? []), handler])
            }
          : () => undefined,
    },
  )
  prismExtension(pi as never)
  return handlers
}

describe("#3420: the extension registers the tool_result handler", () => {
  it("registers it in a prism session with no harness pipe, for every tool", async () => {
    await withEnv(PRISM_SESSION_ENV, async () => {
      const handlers = registeredHandlers().get("tool_result") ?? []
      assert.equal(handlers.length, 1)
      for (const toolName of ["bash", "read", "grep", "mcp_custom_tool"]) {
        const result = await handlers[0](
          { type: "tool_result", toolName, content: [text(`t=${SYNTHETIC_VALUE}`)] },
          {},
        )
        assert.deepEqual(result, { content: [text(`t=${MARKER}`)] }, toolName)
      }
    })
  })

  it("does not register it outside prism", async () => {
    await withEnv({ ...PRISM_SESSION_ENV, PRISM_SESSION_NAME: undefined }, async () => {
      assert.equal(registeredHandlers().has("tool_result"), false)
    })
  })
})

// ---------------------------------------------------------------------------
// End to end, through the installed pi runtime.
// ---------------------------------------------------------------------------

// The pi install root, found the same way as verify-extension-loads.mjs.
function piInstallRoot(): string {
  if (process.env.PI_INSTALL_ROOT) return process.env.PI_INSTALL_ROOT
  const piBin = execFileSync("/bin/sh", ["-c", "command -v pi"]).toString().trim()
  return path.dirname(path.dirname(fs.realpathSync(piBin)))
}

interface TranscriptMessage {
  role: string
  toolCallId?: string
  content?: unknown
  isError?: boolean
}

interface ToolExecutionEnd {
  type: string
  toolCallId: string
  result: { content: unknown; structuredContent?: unknown; details?: unknown }
}

describe("#3420: end to end through the installed pi runtime", () => {
  it("gives the model and the transcript the marker, not the value", async () => {
    const piRoot = path.join(piInstallRoot(), "lib/node_modules/pi-monorepo")
    // The bundle is the code the pi binary runs. It does not export the faux
    // provider, so that comes from the pi-ai package.
    const sdk = await import(pathToFileURL(path.join(piRoot, "dist/bundle/index.js")).href)
    const faux = await import(
      pathToFileURL(
        path.join(piRoot, "node_modules/@earendil-works/pi-ai/dist/providers/faux.js"),
      ).href
    )

    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-3420-"))
    const ends: ToolExecutionEnd[] = []
    try {
      const credFile = path.join(dir, "creds.txt")
      fs.writeFileSync(credFile, `token=${SYNTHETIC_VALUE}\n`)
      const shapeFile = path.join(dir, "shape.txt")
      fs.writeFileSync(shapeFile, `shape=${SHAPE_TOKEN}\n`)

      // No tool-call argument holds a secret. The assistant message is not
      // in the scope of the handler, so the transcript checks below can then
      // cover the whole file.
      let modelSaw: TranscriptMessage[] = []
      const provider = faux.fauxProvider({ models: [{ id: "faux-1", input: ["text", "image"] }] })
      provider.setResponses([
        faux.fauxAssistantMessage(
          [
            faux.fauxToolCall(
              "bash",
              { command: `printf '%s\\n' "$${SYNTHETIC_NAME}"` },
              { id: "bash-ok" },
            ),
            faux.fauxToolCall(
              "bash",
              { command: `printf '%s\\n' "$${SYNTHETIC_NAME}"; exit 3` },
              { id: "bash-failed" },
            ),
            // More than 2000 lines, so pi truncates the result and puts the
            // shown output in details.truncation.content.
            faux.fauxToolCall(
              "bash",
              { command: `seq 1 3000; printf '%s\\n' "$${SYNTHETIC_NAME}"` },
              { id: "bash-truncated" },
            ),
            faux.fauxToolCall("read", { path: credFile }, { id: "read-cred" }),
            faux.fauxToolCall("read", { path: shapeFile }, { id: "read-shape" }),
            faux.fauxToolCall("fixture_structured", { variant: "secret" }, { id: "fixture-secret" }),
            faux.fauxToolCall("fixture_structured", { variant: "plain" }, { id: "fixture-plain" }),
          ],
          { stopReason: "toolUse" },
        ),
        (context: { messages: TranscriptMessage[] }) => {
          modelSaw = structuredClone(context.messages)
          return faux.fauxAssistantMessage("done")
        },
      ])

      const fixtureTool = (pi: { registerTool: (tool: unknown) => void }) => {
        pi.registerTool({
          name: "fixture_structured",
          label: "fixture",
          description: "Returns an image, a text item, and structured content.",
          parameters: {
            type: "object",
            properties: { variant: { type: "string" } },
            required: ["variant"],
          },
          async execute(_id: string, params: { variant: string }) {
            const v = params.variant === "secret" ? `t=${SYNTHETIC_VALUE}` : "plain"
            return {
              content: [image(), text(v)],
              structuredContent: { value: v, count: 3, list: ["a", { b: v }] },
              details: {},
            }
          },
        })
      }

      let sessionFile = ""
      await withEnv(PRISM_SESSION_ENV, async () => {
        const modelRuntime = await sdk.ModelRuntime.create({
          authPath: path.join(dir, "auth.json"),
          modelsPath: null,
          refreshOnCreate: false,
        })
        modelRuntime.registerNativeProvider(provider.provider)
        const settingsManager = sdk.SettingsManager.inMemory({
          compaction: { enabled: false },
          retry: { enabled: false },
        })
        const resourceLoader = new sdk.DefaultResourceLoader({
          cwd: dir,
          agentDir: dir,
          settingsManager,
          extensionFactories: [prismExtension, fixtureTool],
          noSkills: true,
          noPromptTemplates: true,
          noThemes: true,
          noContextFiles: true,
          systemPrompt: "test",
        })
        await resourceLoader.reload()
        const sessionManager = sdk.SessionManager.create(dir, path.join(dir, "sessions"))
        const { session } = await sdk.createAgentSession({
          cwd: dir,
          agentDir: dir,
          model: provider.getModel(),
          thinkingLevel: "off",
          modelRuntime,
          resourceLoader,
          tools: ["bash", "read", "fixture_structured"],
          sessionManager,
          settingsManager,
        })
        session.subscribe((e: ToolExecutionEnd) => {
          if (e.type === "tool_execution_end") ends.push(structuredClone(e))
        })
        try {
          await session.prompt("go")
        } finally {
          session.dispose()
        }
        sessionFile = sessionManager.getSessionFile() ?? ""
      })

      // What the model got.
      const results = modelSaw.filter((m) => m.role === "toolResult")
      assert.equal(results.length, 7, JSON.stringify(modelSaw.map((m) => m.role)))
      const sawResult = (id: string) => {
        const m = results.find((r) => r.toolCallId === id)
        assert.ok(m, `the model got no result for ${id}`)
        return m
      }
      assert.deepEqual(sawResult("bash-ok").content, [text(`${MARKER}\n`)])
      // A failed command reaches the model as an error result. It goes
      // through the tool_result event too.
      const bashFailed = sawResult("bash-failed")
      assert.equal(bashFailed.isError, true)
      assert.ok(JSON.stringify(bashFailed.content).includes(MARKER), JSON.stringify(bashFailed))
      assert.ok(JSON.stringify(sawResult("bash-truncated").content).includes(MARKER))
      assert.deepEqual(sawResult("read-cred").content, [text(`token=${MARKER}\n`)])
      assert.deepEqual(sawResult("read-shape").content, [text(`shape=${SHAPE_MARKER}\n`)])
      assert.deepEqual(sawResult("fixture-secret").content, [image(), text(`t=${MARKER}`)])
      assert.deepEqual(sawResult("fixture-plain").content, [image(), text("plain")])
      const sent = JSON.stringify(modelSaw)
      // The context messages also carry `details`, so this covers it too.
      assert.ok(!sent.includes(SYNTHETIC_VALUE), "the credential value is in the model context")
      assert.ok(!sent.includes(SHAPE_TOKEN), "the shape token is in the model context")

      // What the transcript holds.
      assert.ok(sessionFile, "the session wrote no transcript file")
      const transcript = fs.readFileSync(sessionFile, "utf8")
      assert.ok(!transcript.includes(SYNTHETIC_VALUE), "the credential value is in the transcript")
      assert.ok(!transcript.includes(SHAPE_TOKEN), "the shape token is in the transcript")
      const entries = transcript
        .split("\n")
        .filter((line) => line.trim() !== "")
        .map((line) => JSON.parse(line) as { message?: TranscriptMessage & { details?: unknown } })
      const entryFor = (id: string) =>
        entries.find((e) => e.message?.role === "toolResult" && e.message.toolCallId === id)
      assert.deepEqual(entryFor("bash-ok")?.message?.content, [text(`${MARKER}\n`)])
      const truncation = (
        entryFor("bash-truncated")?.message?.details as { truncation?: { content?: unknown } }
      )?.truncation
      assert.equal(typeof truncation?.content, "string", "pi did not truncate the bash result")
      assert.ok(String(truncation?.content).includes(MARKER), "details lost the marker")

      // structuredContent, which pi gives to programmatic callers. Pi can end
      // parallel tool calls in any order, so look each one up by its id.
      const structuredOf = (id: string) =>
        ends.find((e) => e.toolCallId === id)?.result.structuredContent
      assert.deepEqual(structuredOf("fixture-secret"), {
        value: `t=${MARKER}`,
        count: 3,
        list: ["a", { b: `t=${MARKER}` }],
      })
      assert.deepEqual(structuredOf("fixture-plain"), {
        value: "plain",
        count: 3,
        list: ["a", { b: "plain" }],
      })
      assert.equal(
        (structuredOf("bash-ok") as { output?: unknown } | undefined)?.output,
        `${MARKER}\n`,
      )
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
      // The bash tool saves the full output of a truncated command to a temp
      // file, which holds the synthetic value.
      for (const e of ends) {
        const p = (e.result.details as { fullOutputPath?: unknown } | undefined)?.fullOutputPath
        if (typeof p === "string") fs.rmSync(p, { force: true })
      }
    }
  })
})

// An agent credential gives the session a secret as a file. No variable holds
// the value, so the value layer must read the file that PRISM_CREDENTIAL_FILES
// lists. Issue #3476: the context shapes mask the session values of a UniFi OS
// login. The credential names here are synthetic.
describe("end to end, a credential file and the UniFi OS session values", () => {
  it("gives the model and the transcript the marker for cat, env, and printenv", async () => {
    const FILE_SECRET = "synthetic-credential-file-e2e-0123456789abcdef"
    const FILE_MARKER = "[redacted:EXAMPLE_PASSWORD]"
    const SESSION_JWT = "eyJhbGciOiJIUzI1NiJ9.eyJzeW50aGV0aWMiOnRydWV9.c3ludGhldGljLTM0NzY"
    const CSRF_TOKEN = "00000000-0000-4000-8000-000000003476"
    const piRoot = path.join(piInstallRoot(), "lib/node_modules/pi-monorepo")
    const sdk = await import(pathToFileURL(path.join(piRoot, "dist/bundle/index.js")).href)
    const faux = await import(
      pathToFileURL(
        path.join(piRoot, "node_modules/@earendil-works/pi-ai/dist/providers/faux.js"),
      ).href
    )

    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "prism-credential-file-"))
    try {
      const passwordFile = path.join(dir, "example_password")
      fs.writeFileSync(passwordFile, `${FILE_SECRET}\n`, { mode: 0o400 })
      const loginFile = path.join(dir, "login-output")
      fs.writeFileSync(
        loginFile,
        `set-cookie: TOKEN=${SESSION_JWT}; path=/; secure; httponly\r\n` +
          `x-csrf-token: ${CSRF_TOKEN}\r\n` +
          `#HttpOnly_host.example.invalid\tFALSE\t/\tTRUE\t0\tTOKEN\t${SESSION_JWT}\n`,
      )

      let modelSaw: TranscriptMessage[] = []
      const provider = faux.fauxProvider({ models: [{ id: "faux-1", input: ["text"] }] })
      provider.setResponses([
        faux.fauxAssistantMessage(
          [
            faux.fauxToolCall("bash", { command: 'cat "$EXAMPLE_PASSWORD_FILE"' }, { id: "cat" }),
            faux.fauxToolCall("bash", { command: "env" }, { id: "env" }),
            faux.fauxToolCall("bash", { command: "printenv" }, { id: "printenv" }),
            faux.fauxToolCall(
              "bash",
              { command: 'K="$(cat "$EXAMPLE_PASSWORD_FILE")" env' },
              { id: "env-copy" },
            ),
            faux.fauxToolCall(
              "bash",
              { command: 'K="$(cat "$EXAMPLE_PASSWORD_FILE")" printenv K' },
              { id: "printenv-copy" },
            ),
            faux.fauxToolCall(
              "bash",
              { command: `cat ${JSON.stringify(loginFile)}` },
              { id: "session" },
            ),
          ],
          { stopReason: "toolUse" },
        ),
        (context: { messages: TranscriptMessage[] }) => {
          modelSaw = structuredClone(context.messages)
          return faux.fauxAssistantMessage("done")
        },
      ])

      let sessionFile = ""
      const sessionEnv = {
        ...PRISM_SESSION_ENV,
        PRISM_CREDENTIAL_FILES: "EXAMPLE_PASSWORD_FILE=EXAMPLE_PASSWORD",
        EXAMPLE_PASSWORD_FILE: passwordFile,
      }
      await withEnv(sessionEnv, async () => {
        const modelRuntime = await sdk.ModelRuntime.create({
          authPath: path.join(dir, "auth.json"),
          modelsPath: null,
          refreshOnCreate: false,
        })
        modelRuntime.registerNativeProvider(provider.provider)
        const settingsManager = sdk.SettingsManager.inMemory({
          compaction: { enabled: false },
          retry: { enabled: false },
        })
        const resourceLoader = new sdk.DefaultResourceLoader({
          cwd: dir,
          agentDir: dir,
          settingsManager,
          extensionFactories: [prismExtension],
          noSkills: true,
          noPromptTemplates: true,
          noThemes: true,
          noContextFiles: true,
          systemPrompt: "test",
        })
        await resourceLoader.reload()
        const sessionManager = sdk.SessionManager.create(dir, path.join(dir, "sessions"))
        const { session } = await sdk.createAgentSession({
          cwd: dir,
          agentDir: dir,
          model: provider.getModel(),
          thinkingLevel: "off",
          modelRuntime,
          resourceLoader,
          tools: ["bash"],
          sessionManager,
          settingsManager,
        })
        try {
          await session.prompt("go")
        } finally {
          session.dispose()
        }
        sessionFile = sessionManager.getSessionFile() ?? ""
      })

      const results = modelSaw.filter((m) => m.role === "toolResult")
      assert.equal(results.length, 6, JSON.stringify(modelSaw.map((m) => m.role)))
      const saw = (id: string) => JSON.stringify(results.find((r) => r.toolCallId === id)?.content)
      assert.deepEqual(results.find((r) => r.toolCallId === "cat")?.content, [
        text(`${FILE_MARKER}\n`),
      ])
      assert.ok(saw("env").includes(`EXAMPLE_PASSWORD_FILE=${passwordFile}`), saw("env"))
      assert.ok(saw("printenv").includes(`EXAMPLE_PASSWORD_FILE=${passwordFile}`), saw("printenv"))
      assert.ok(saw("env-copy").includes(`K=${FILE_MARKER}`), saw("env-copy"))
      assert.deepEqual(results.find((r) => r.toolCallId === "printenv-copy")?.content, [
        text(`${FILE_MARKER}\n`),
      ])
      assert.deepEqual(results.find((r) => r.toolCallId === "session")?.content, [
        text(
          "set-cookie: TOKEN=[redacted:unifi-token-cookie]; path=/; secure; httponly\r\n" +
            "x-csrf-token: [redacted:csrf-token-header]\r\n" +
            "#HttpOnly_host.example.invalid\tFALSE\t/\tTRUE\t0\tTOKEN\t[redacted:unifi-token-cookie-jar]\n",
        ),
      ])
      for (const secret of [FILE_SECRET, SESSION_JWT, CSRF_TOKEN]) {
        assert.ok(!JSON.stringify(modelSaw).includes(secret), "a secret is in the model context")
      }

      assert.ok(sessionFile, "the session wrote no transcript file")
      const transcript = fs.readFileSync(sessionFile, "utf8")
      for (const secret of [FILE_SECRET, SESSION_JWT, CSRF_TOKEN]) {
        assert.ok(!transcript.includes(secret), "a secret is in the transcript")
      }
    } finally {
      fs.rmSync(dir, { recursive: true, force: true })
    }
  })
})
