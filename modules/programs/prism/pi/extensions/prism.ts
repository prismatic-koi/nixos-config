// prism.ts — PI extension that bridges PI's hook events to the prism sidecar
// over a JSONL framed Unix-or-TCP socket.
//
// Spec: see modules/programs/prism/prism/docs/pi-wire-protocol.md (PR #1220).
// Issue: #1210 (P2.EXTENSION).
//
// Activation guard
// ----------------
// The extension factory exits early if PRISM_SESSION_NAME is not set. This
// makes it a safe no-op when PI is invoked outside prism (e.g. directly from
// a shell). prism's agent-pane launcher always sets PRISM_SESSION_NAME and
// PRISM_HARNESS_PIPE before exec'ing PI.
//
// Connection lifecycle
// --------------------
// On `session_start`:
//   1. Parse PRISM_HARNESS_PIPE (unix:// or tcp:// per wire spec §2.4).
//   2. Open the connection via node:net.
//   3. Send `hello` frame (protocol_version=1).
//   4. Wait for `hello_ack`. Validate protocol_version. Disconnect on mismatch.
//   5. Set up the buffered line reader and the synchronous writer.
//
// Outbound hooks → frames
// -----------------------
//   before_agent_start    → state_change:active
//   tool_execution_start  → tool_call           (truncated args)
//   tool_execution_end    → tool_result         (truncated output)
//   turn_start            → turn_start
//   turn_end              → turn_end + state_change:idle (when no pending msgs)
//   message_update        → msg_assistant       (text_delta only, truncated)
//   after_provider_response (non-OK)
//                         → provider_error
//   auto_retry_start      → auto_retry_start
//   auto_retry_end        → auto_retry_end
//   session_shutdown      → state_change:finished + session_shutdown
//
// Inbound frames → PI runtime
// ---------------------------
//   prompt           → pi.sendUserMessage(text, {deliverAs, images})
//   set_model        → pi.setModel + pi.setThinkingLevel
//   register_provider→ pi.registerProvider
//   set_active_tools → pi.setActiveTools
//   abort            → ctx.abort()
//
// Errors during inbound dispatch are logged and emitted upstream as a
// non-blocking `error` frame; they do not tear down the connection.
//
// Truncation
// ----------
// Per the wire protocol (§5.3, §5.4, §5.5) tool args, tool output, and
// assistant message deltas are truncated at 8 KiB, with a literal
// "…[truncated]" sentinel suffix on truncated string fields. Truncation is
// applied only on the wire copy; PI retains the full data internally.

import * as net from "node:net"
import type {
  ExtensionAPI,
  ExtensionContext,
  ProviderConfig,
} from "@mariozechner/pi-coding-agent"

// ---------------------------------------------------------------------------
// Constants — kept at module scope so unit tests can import them.
// ---------------------------------------------------------------------------

/** Wire protocol version this extension speaks. */
export const PROTOCOL_VERSION = 1

/** Per-field byte cap for tool args, tool output, and assistant message deltas. */
export const TRUNCATION_LIMIT_BYTES = 8 * 1024 // 8 KiB

/** Sentinel appended to truncated string fields. */
export const TRUNCATION_SENTINEL = "…[truncated]"

/** Harness identifier sent in the `hello` frame. */
export const HARNESS_NAME = "pi"

// ---------------------------------------------------------------------------
// Truncation helpers — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Truncate a string to TRUNCATION_LIMIT_BYTES (UTF-8 encoded length), appending
 * the truncation sentinel if truncation occurred. Returns the input unchanged
 * if it already fits within the budget.
 *
 * Counting is by UTF-8 byte length, not by JS string length, so multi-byte
 * characters are accounted for correctly. The cut point is rounded back to a
 * UTF-8 boundary so the result is always valid UTF-8.
 */
export function truncateString(s: string): {
  text: string
  truncated: boolean
} {
  const encoded = Buffer.from(s, "utf8")
  if (encoded.length <= TRUNCATION_LIMIT_BYTES) {
    return { text: s, truncated: false }
  }
  // Cut at TRUNCATION_LIMIT_BYTES and back off to the previous valid UTF-8
  // boundary. UTF-8 continuation bytes have the high two bits set to 10
  // (0b10xxxxxx), so we step backwards while we land on one.
  let cut = TRUNCATION_LIMIT_BYTES
  while (cut > 0 && (encoded[cut] & 0xc0) === 0x80) {
    cut--
  }
  const head = encoded.slice(0, cut).toString("utf8")
  return { text: head + TRUNCATION_SENTINEL, truncated: true }
}

/**
 * Truncate every string-valued field (top-level only) of a tool-args object.
 * Non-string values pass through unchanged. Returns a new object plus a
 * truncation flag indicating whether any field was truncated.
 *
 * This matches the wire protocol §5.3 convention: per-field cap, not whole-
 * object cap. Nested string fields are not recursively truncated — JSON
 * encoding the whole object is used as a fallback budget guard for the
 * envelope.
 */
export function truncateArgs(args: unknown): {
  args: unknown
  truncated: boolean
} {
  if (args === null || typeof args !== "object" || Array.isArray(args)) {
    // Non-object args: stringify and truncate as a single value.
    if (typeof args === "string") {
      const r = truncateString(args)
      return { args: r.text, truncated: r.truncated }
    }
    return { args, truncated: false }
  }
  const out: Record<string, unknown> = {}
  let anyTruncated = false
  for (const [k, v] of Object.entries(args as Record<string, unknown>)) {
    if (typeof v === "string") {
      const r = truncateString(v)
      out[k] = r.text
      if (r.truncated) anyTruncated = true
    } else {
      out[k] = v
    }
  }
  return { args: out, truncated: anyTruncated }
}

/**
 * Coerce a tool result `content` array (PI's MessageContent[]) into a single
 * string. Concatenates every text-typed block. Image and other non-text
 * blocks are skipped — they don't fit the wire protocol's `output: string`
 * shape and would not be useful to the sidecar's text-oriented persistence.
 */
export function coerceToolOutput(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  const parts: string[] = []
  for (const block of content) {
    if (
      block !== null &&
      typeof block === "object" &&
      (block as { type?: unknown }).type === "text" &&
      typeof (block as { text?: unknown }).text === "string"
    ) {
      parts.push((block as { text: string }).text)
    }
  }
  return parts.join("")
}

// ---------------------------------------------------------------------------
// Endpoint parsing — exported for unit testing.
// ---------------------------------------------------------------------------

export type ParsedEndpoint =
  | { kind: "unix"; path: string }
  | { kind: "tcp"; host: string; port: number }

/**
 * Parse the PRISM_HARNESS_PIPE env value into a connection target. Throws on
 * unrecognised schemes or malformed values.
 */
export function parseEndpoint(value: string): ParsedEndpoint {
  if (value.startsWith("unix://")) {
    const path = value.slice("unix://".length)
    if (path.length === 0) {
      throw new Error("PRISM_HARNESS_PIPE: empty unix path")
    }
    return { kind: "unix", path }
  }
  if (value.startsWith("tcp://")) {
    const rest = value.slice("tcp://".length)
    const lastColon = rest.lastIndexOf(":")
    if (lastColon === -1) {
      throw new Error(`PRISM_HARNESS_PIPE: tcp endpoint missing port: ${value}`)
    }
    const host = rest.slice(0, lastColon)
    const portStr = rest.slice(lastColon + 1)
    const port = Number.parseInt(portStr, 10)
    if (!Number.isFinite(port) || port <= 0 || port > 65535) {
      throw new Error(
        `PRISM_HARNESS_PIPE: tcp endpoint invalid port: ${portStr}`,
      )
    }
    if (host.length === 0) {
      throw new Error(`PRISM_HARNESS_PIPE: tcp endpoint missing host: ${value}`)
    }
    return { kind: "tcp", host, port }
  }
  throw new Error(
    `PRISM_HARNESS_PIPE: unsupported scheme (expected unix:// or tcp://): ${value}`,
  )
}

// ---------------------------------------------------------------------------
// JSONL line reader — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Attach a JSONL reader to a Readable stream. Each \n-terminated line is
 * passed to onLine. Optional trailing \r before \n is stripped. The internal
 * buffer grows without an upper bound, per wire protocol §3 (line length is
 * unbounded — large tool outputs).
 *
 * NB: Node's built-in `readline` is non-compliant because it splits on
 * Unicode line separators (U+2028, U+2029) which can appear inside valid
 * JSON strings. This implementation splits on `\n` only, exactly as the
 * wire protocol prescribes.
 */
export function attachJsonlReader(
  stream: NodeJS.ReadableStream,
  onLine: (line: string) => void,
): void {
  let buffer = ""
  stream.setEncoding("utf8")
  stream.on("data", (chunk: string | Buffer) => {
    buffer += typeof chunk === "string" ? chunk : chunk.toString("utf8")
    while (true) {
      const idx = buffer.indexOf("\n")
      if (idx === -1) break
      let line = buffer.slice(0, idx)
      buffer = buffer.slice(idx + 1)
      if (line.endsWith("\r")) line = line.slice(0, -1)
      onLine(line)
    }
  })
  stream.on("end", () => {
    if (buffer.length > 0) {
      const line = buffer.endsWith("\r") ? buffer.slice(0, -1) : buffer
      buffer = ""
      onLine(line)
    }
  })
}

// ---------------------------------------------------------------------------
// Helper: redact a problematic line for logs (first 200 bytes, base64).
// Per wire spec §7.3 — keep the diagnostic but never let arbitrary bytes
// corrupt log streams.
// ---------------------------------------------------------------------------

export function redactLine(line: string): string {
  const head = Buffer.from(line, "utf8").slice(0, 200).toString("base64")
  return head
}

// ---------------------------------------------------------------------------
// Frame writer — synchronous (per wire spec §7.5).
// ---------------------------------------------------------------------------

interface FrameWriter {
  write(frame: Record<string, unknown>): void
  close(): void
}

function makeFrameWriter(socket: net.Socket): FrameWriter {
  let closed = false
  return {
    write(frame) {
      if (closed) return
      let line: string
      try {
        line = JSON.stringify(frame) + "\n"
      } catch (err) {
        // Should be unreachable for our own frames, but never throw out of a
        // hook callback — log and skip.
        console.error("[prism-extension] frame serialise failed:", err)
        return
      }
      try {
        socket.write(line)
      } catch (err) {
        console.error("[prism-extension] socket.write failed:", err)
      }
    },
    close() {
      if (closed) return
      closed = true
      try {
        socket.end()
      } catch {}
    },
  }
}

// ---------------------------------------------------------------------------
// Inbound frame dispatcher — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Minimal slice of ExtensionAPI / context that the inbound dispatcher needs.
 * Defined here so unit tests can supply a mock without constructing a full
 * ExtensionAPI object.
 */
export interface InboundDispatchAPI {
  sendUserMessage: (
    content:
      | string
      | Array<{ type: "text"; text: string } | Record<string, unknown>>,
    options?: { deliverAs?: "steer" | "followUp" | "nextTurn" },
  ) => void
  setModel: (model: unknown) => Promise<boolean>
  setThinkingLevel: (level: string) => void
  registerProvider: (name: string, config: ProviderConfig) => void
  setActiveTools: (tools: string[]) => void
  modelRegistryFind: (provider: string, modelId: string) => unknown
  abort: () => void
}

/**
 * Dispatch a single inbound frame. Errors are caught and reported via the
 * supplied error reporter; they are non-fatal per wire spec §6 (and the
 * sidecar handles dispatch errors as logged + non-blocking).
 */
export async function dispatchInboundFrame(
  frame: Record<string, unknown>,
  api: InboundDispatchAPI,
  emitError: (
    code: string,
    message: string,
    relatedType: string | undefined,
  ) => void,
): Promise<void> {
  const type = frame.type
  if (typeof type !== "string") {
    emitError("malformed_frame", "frame missing type field", undefined)
    return
  }

  try {
    switch (type) {
      case "prompt": {
        const text = typeof frame.text === "string" ? frame.text : ""
        const deliverAsRaw = frame.deliver_as
        const deliverAs =
          deliverAsRaw === "steer" ||
          deliverAsRaw === "followUp" ||
          deliverAsRaw === "nextTurn"
            ? deliverAsRaw
            : "steer"
        const images = Array.isArray(frame.images) ? frame.images : []

        // Translate wire `images` (snake_case mime_type) into PI's ImageContent
        // shape (camelCase mediaType) per wire spec §6.2.
        const piImages = images
          .filter((img): img is Record<string, unknown> =>
            typeof img === "object" && img !== null,
          )
          .map((img) => ({
            type: "image" as const,
            source: {
              type: "base64" as const,
              mediaType:
                typeof img.mime_type === "string" ? img.mime_type : "image/png",
              data: typeof img.data === "string" ? img.data : "",
            },
          }))

        let content:
          | string
          | Array<
              | { type: "text"; text: string }
              | { type: "image"; source: Record<string, unknown> }
            >
        if (piImages.length > 0) {
          content = [
            { type: "text", text },
            ...piImages,
          ]
        } else {
          content = text
        }

        // PI's sendUserMessage extension surface accepts steer | followUp; for
        // nextTurn we omit deliverAs so PI's internal logic decides based on
        // idle state (immediate when idle; queued when streaming). This is
        // the closest mapping to the RPC `prompt` command's behaviour.
        if (deliverAs === "nextTurn") {
          api.sendUserMessage(content)
        } else {
          api.sendUserMessage(content, { deliverAs })
        }
        break
      }

      case "set_model": {
        const provider =
          typeof frame.provider === "string" ? frame.provider : ""
        const modelId = typeof frame.model === "string" ? frame.model : ""
        const thinking =
          typeof frame.thinking === "string" ? frame.thinking : "off"
        if (!provider || !modelId) {
          emitError(
            "malformed_frame",
            "set_model missing provider or model",
            type,
          )
          return
        }
        const model = api.modelRegistryFind(provider, modelId)
        if (!model) {
          emitError(
            "model_not_found",
            `model ${provider}/${modelId} not found in registry`,
            type,
          )
          return
        }
        const ok = await api.setModel(model)
        if (!ok) {
          emitError(
            "model_set_failed",
            `setModel(${provider}/${modelId}) returned false (no API key?)`,
            type,
          )
          return
        }
        api.setThinkingLevel(thinking)
        break
      }

      case "register_provider": {
        const name = typeof frame.name === "string" ? frame.name : ""
        const config = frame.config
        if (!name) {
          emitError(
            "malformed_frame",
            "register_provider missing name",
            type,
          )
          return
        }
        if (typeof config !== "object" || config === null) {
          emitError(
            "malformed_frame",
            "register_provider missing config",
            type,
          )
          return
        }
        api.registerProvider(name, config as ProviderConfig)
        break
      }

      case "set_active_tools": {
        const tools = Array.isArray(frame.tools) ? frame.tools : null
        if (!tools || !tools.every((t) => typeof t === "string")) {
          emitError(
            "malformed_frame",
            "set_active_tools missing tools array",
            type,
          )
          return
        }
        api.setActiveTools(tools as string[])
        break
      }

      case "abort": {
        api.abort()
        break
      }

      case "error": {
        // Sidecar-side error; log and disconnect is handled by the caller.
        // Per wire spec §7.4 the extension does not retry the handshake.
        // Just log here; caller closes the socket.
        console.error("[prism-extension] sidecar error frame:", frame)
        break
      }

      default: {
        // Forward-compat: unknown frame types are logged and skipped, not
        // fatal. Per wire spec §8.2 this is the single most important
        // forward-compat guarantee.
        console.error(
          "[prism-extension] unknown inbound frame type, skipping:",
          type,
        )
        break
      }
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    emitError("dispatch_error", `${type}: ${msg}`, type)
  }
}

// ---------------------------------------------------------------------------
// Activation guard — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Returns true when the extension should activate (i.e., we are running
 * under prism). The sole signal is the presence of PRISM_SESSION_NAME in the
 * environment — prism's agent-pane launcher always sets this before exec'ing
 * PI inside the sandbox.
 *
 * Exposed as a function (not a captured boolean) so tests can manipulate
 * process.env between calls.
 */
export function shouldActivate(env: NodeJS.ProcessEnv = process.env): boolean {
  return typeof env.PRISM_SESSION_NAME === "string" && env.PRISM_SESSION_NAME.length > 0
}

// ---------------------------------------------------------------------------
// Extension entry point.
// ---------------------------------------------------------------------------

export default function prismExtension(pi: ExtensionAPI): void {
  // Activation guard. When PI runs outside prism, leave it untouched.
  if (!shouldActivate()) {
    return
  }

  const endpointEnv = process.env.PRISM_HARNESS_PIPE
  if (!endpointEnv) {
    console.error(
      "[prism-extension] PRISM_SESSION_NAME is set but PRISM_HARNESS_PIPE is not — extension is a no-op",
    )
    return
  }

  let endpoint: ParsedEndpoint
  try {
    endpoint = parseEndpoint(endpointEnv)
  } catch (err) {
    console.error("[prism-extension] parseEndpoint failed:", err)
    return
  }

  // ── Connection state ──────────────────────────────────────────────────
  let socket: net.Socket | null = null
  let writer: FrameWriter | null = null
  let handshakeComplete = false
  let connected = false
  // Track the latest active turn's abort handle. ctx.abort() requires an
  // ExtensionContext, so we capture one whenever a turn-active hook fires.
  let lastCtx: ExtensionContext | null = null

  const sendError = (
    code: string,
    message: string,
    relatedType: string | undefined,
  ): void => {
    if (!writer) return
    const frame: Record<string, unknown> = {
      type: "error",
      code,
      message,
    }
    if (relatedType) frame.related_type = relatedType
    writer.write(frame)
  }

  // ── Connect to sidecar ────────────────────────────────────────────────
  const connect = (): void => {
    try {
      socket =
        endpoint.kind === "unix"
          ? net.createConnection(endpoint.path)
          : net.createConnection(endpoint.port, endpoint.host)
    } catch (err) {
      console.error("[prism-extension] createConnection failed:", err)
      return
    }

    socket.on("error", (err) => {
      console.error("[prism-extension] socket error:", err)
    })
    socket.on("close", () => {
      connected = false
      writer = null
      // Per wire spec §7.6: if the sidecar dies, PI continues running
      // standalone. We don't try to reconnect.
    })
    socket.on("connect", () => {
      connected = true
      writer = makeFrameWriter(socket!)

      // Handshake (§4.1): hello frame, first thing on the wire.
      writer.write({
        type: "hello",
        protocol_version: PROTOCOL_VERSION,
        harness: HARNESS_NAME,
        // harness_version: best-effort; PI does not expose it on the API,
        // so we send the package version from env if available, else a
        // sentinel string. Sidecar does not branch on this in v1.
        harness_version: process.env.PI_VERSION ?? "unknown",
      })
    })

    attachJsonlReader(socket, (line) => {
      if (line.length === 0) return
      let frame: unknown
      try {
        frame = JSON.parse(line)
      } catch (err) {
        console.error(
          "[prism-extension] inbound parse error (line redacted):",
          redactLine(line),
        )
        return
      }
      if (typeof frame !== "object" || frame === null || Array.isArray(frame)) {
        console.error(
          "[prism-extension] inbound non-object frame (line redacted):",
          redactLine(line),
        )
        return
      }
      const f = frame as Record<string, unknown>

      if (!handshakeComplete) {
        // Expect hello_ack as the first frame.
        if (f.type !== "hello_ack") {
          console.error(
            "[prism-extension] expected hello_ack, got:",
            f.type,
          )
          // Per wire spec §7.4: log and disconnect, no retry.
          if (socket) socket.end()
          return
        }
        const ackVersion = f.protocol_version
        if (typeof ackVersion !== "number" || ackVersion !== PROTOCOL_VERSION) {
          console.error(
            "[prism-extension] hello_ack protocol_version mismatch (extension speaks " +
              PROTOCOL_VERSION +
              ", sidecar said " +
              String(ackVersion) +
              ") — disconnecting",
          )
          if (socket) socket.end()
          return
        }
        handshakeComplete = true
        return
      }

      // Post-handshake: dispatch.
      const apiAdapter: InboundDispatchAPI = {
        sendUserMessage: (content, options) =>
          pi.sendUserMessage(
            content as Parameters<ExtensionAPI["sendUserMessage"]>[0],
            options as Parameters<ExtensionAPI["sendUserMessage"]>[1],
          ),
        setModel: (model) =>
          pi.setModel(model as Parameters<ExtensionAPI["setModel"]>[0]),
        setThinkingLevel: (level) =>
          pi.setThinkingLevel(
            level as Parameters<ExtensionAPI["setThinkingLevel"]>[0],
          ),
        registerProvider: (name, config) => pi.registerProvider(name, config),
        setActiveTools: (tools) => pi.setActiveTools(tools),
        modelRegistryFind: (provider, modelId) => {
          // ctx.modelRegistry is the canonical lookup, but only available
          // from a handler context. Fall back to lastCtx if we have one.
          if (lastCtx?.modelRegistry) {
            return lastCtx.modelRegistry.find(provider, modelId)
          }
          return null
        },
        abort: () => {
          if (lastCtx?.abort) {
            lastCtx.abort()
          } else {
            console.error(
              "[prism-extension] abort received but no active context",
            )
          }
        },
      }

      void dispatchInboundFrame(f, apiAdapter, sendError)
    })
  }

  // ── Outbound hooks ────────────────────────────────────────────────────
  // Track turn state so we can emit state_change:idle on turn_end when
  // there's no pending message.
  let agentRunning = false

  pi.on("session_start", async (_event, ctx) => {
    lastCtx = ctx
    // Connect on session_start. The wire spec requires the extension to dial
    // out; the sidecar has already bound the listener.
    connect()
  })

  pi.on("before_agent_start", async (_event, ctx) => {
    lastCtx = ctx
    agentRunning = true
    if (writer && handshakeComplete) {
      writer.write({ type: "state_change", state: "active" })
    }
    return undefined
  })

  pi.on("turn_start", async (_event, ctx) => {
    lastCtx = ctx
    if (writer && handshakeComplete) {
      writer.write({ type: "turn_start" })
    }
  })

  pi.on("turn_end", async (event, ctx) => {
    lastCtx = ctx
    if (writer && handshakeComplete) {
      // Best-effort token/cost extraction from the message's usage block, if
      // PI populates it. We deliberately tolerate missing fields per wire
      // spec §5.7.
      const message = (event as { message?: unknown }).message
      const usage =
        message !== null && typeof message === "object"
          ? (message as { usage?: Record<string, unknown> }).usage
          : undefined
      const turnEndFrame: Record<string, unknown> = { type: "turn_end" }
      if (usage && typeof usage === "object") {
        const u: Record<string, unknown> = {}
        if (typeof usage.input === "number") u.input = usage.input
        if (typeof usage.output === "number") u.output = usage.output
        if (typeof usage.cacheRead === "number") u.cache_read = usage.cacheRead
        if (typeof usage.cacheWrite === "number")
          u.cache_write = usage.cacheWrite
        const cost = (usage as { cost?: unknown }).cost
        if (typeof cost === "number") {
          u.cost = cost
        } else if (
          cost !== null &&
          typeof cost === "object" &&
          typeof (cost as { total?: unknown }).total === "number"
        ) {
          u.cost = (cost as { total: number }).total
        }
        if (Object.keys(u).length > 0) turnEndFrame.usage = u
      }
      writer.write(turnEndFrame)

      // Idle detection: if PI reports idle and no pending messages, emit
      // state_change:idle. Sidecar applies its own debounce window.
      try {
        if (
          typeof ctx.isIdle === "function" &&
          ctx.isIdle() &&
          (typeof ctx.hasPendingMessages !== "function" ||
            !ctx.hasPendingMessages())
        ) {
          writer.write({ type: "state_change", state: "idle" })
        }
      } catch (err) {
        console.error("[prism-extension] idle check failed:", err)
      }
    }
  })

  pi.on("tool_execution_start", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    const id =
      typeof (event as { toolCallId?: unknown }).toolCallId === "string"
        ? (event as { toolCallId: string }).toolCallId
        : ""
    const name =
      typeof (event as { toolName?: unknown }).toolName === "string"
        ? (event as { toolName: string }).toolName
        : ""
    const rawArgs = (event as { args?: unknown }).args
    const { args, truncated } = truncateArgs(rawArgs)
    const frame: Record<string, unknown> = {
      type: "tool_call",
      id,
      name,
      args,
    }
    if (truncated) frame.truncated = true
    writer.write(frame)
  })

  pi.on("tool_execution_end", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    const id =
      typeof (event as { toolCallId?: unknown }).toolCallId === "string"
        ? (event as { toolCallId: string }).toolCallId
        : ""
    const isError =
      (event as { isError?: unknown }).isError === true
    const result = (event as { result?: unknown }).result
    const rawOutput =
      result !== null && typeof result === "object"
        ? coerceToolOutput((result as { content?: unknown }).content)
        : ""
    const { text: output, truncated } = truncateString(rawOutput)
    const frame: Record<string, unknown> = {
      type: "tool_result",
      id,
      success: !isError,
      output,
    }
    if (truncated) frame.truncated = true
    writer.write(frame)
  })

  pi.on("message_update", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    // Only forward streaming text deltas. Other update types (text_start,
    // text_end, thinking deltas, tool-call deltas) are noise on the wire —
    // the sidecar already gets the final tool call via tool_execution_*.
    const ame = (event as { assistantMessageEvent?: unknown })
      .assistantMessageEvent
    if (
      ame !== null &&
      typeof ame === "object" &&
      (ame as { type?: unknown }).type === "text_delta" &&
      typeof (ame as { delta?: unknown }).delta === "string"
    ) {
      const delta = (ame as { delta: string }).delta
      const { text, truncated } = truncateString(delta)
      const frame: Record<string, unknown> = {
        type: "msg_assistant",
        text,
      }
      if (truncated) frame.truncated = true
      writer.write(frame)
    }
  })

  pi.on("after_provider_response", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    const status = (event as { status?: unknown }).status
    if (typeof status !== "number") return
    // Per wire spec §5.8 only non-OK responses are emitted; successful
    // responses are noise (one per turn, redundant with turn_end).
    if (status >= 200 && status < 300) return
    const provider =
      typeof (event as { provider?: unknown }).provider === "string"
        ? (event as { provider: string }).provider
        : ""
    // PI's after_provider_response doesn't include a message body in the
    // event payload; surface what we can. The sidecar persists what arrives.
    const message =
      typeof (event as { message?: unknown }).message === "string"
        ? (event as { message: string }).message
        : `HTTP ${status}`
    writer.write({
      type: "provider_error",
      provider,
      status_code: status,
      message,
    })
  })

  pi.on("auto_retry_start", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    const e = event as Record<string, unknown>
    writer.write({
      type: "auto_retry_start",
      attempt: typeof e.attempt === "number" ? e.attempt : 0,
      max_attempts: typeof e.maxAttempts === "number" ? e.maxAttempts : 0,
      delay_ms: typeof e.delayMs === "number" ? e.delayMs : 0,
      error_message:
        typeof e.errorMessage === "string" ? e.errorMessage : "",
    })
  })

  pi.on("auto_retry_end", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    const e = event as Record<string, unknown>
    const success = e.success === true
    const frame: Record<string, unknown> = {
      type: "auto_retry_end",
      success,
      attempt: typeof e.attempt === "number" ? e.attempt : 0,
    }
    if (!success && typeof e.errorMessage === "string") {
      frame.final_error = e.errorMessage
    } else if (!success && typeof e.finalError === "string") {
      frame.final_error = e.finalError
    }
    writer.write(frame)
  })

  pi.on("session_shutdown", async (_event, ctx) => {
    lastCtx = ctx
    agentRunning = false
    if (writer && handshakeComplete) {
      writer.write({ type: "state_change", state: "finished" })
      writer.write({ type: "session_shutdown" })
      writer.close()
    } else if (writer) {
      writer.close()
    }
    if (socket && !connected) {
      try {
        socket.destroy()
      } catch {}
    }
  })

  // Suppress the unused-variable warning for agentRunning — the variable is
  // tracked for future use (e.g. waiting state) but currently only consulted
  // implicitly via PI's own state.
  void agentRunning
}
