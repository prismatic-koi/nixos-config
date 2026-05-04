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
//   turn_end              → turn_end + state_change:finished + session_shutdown
//                            (stopReason=stop, idle, no pending, not reviewing)
//                         → turn_end + state_change:interrupted (stopReason=aborted)
//                         → turn_end + state_change:idle (other idle turns)
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

// ---------------------------------------------------------------------------
// Behavioural guards — ported from opencode's prism-hooks.ts
// ---------------------------------------------------------------------------
//
// Three guards are implemented here:
//   1. Doom-loop detector: 5 consecutive same-tool calls → steer message.
//   2. Review-cycle counter: 3 cycles without convergence → escalation message.
//   3. Git-push reminder: git push without prior review → system prompt reminder.
//
// All three guards can be disabled via env vars for debugging.
// ---------------------------------------------------------------------------

/** Number of consecutive matching tool calls that triggers the doom-loop. */
export const DOOM_LOOP_THRESHOLD = 5

/** Number of review cycles before the escalation warning fires. */
export const REVIEW_CYCLE_THRESHOLD = 3

/**
 * Tools excluded from doom-loop detection. Legitimate exploration revisits
 * the same paths often; treating them as loops generates false positives.
 * todowrite is excluded because task-list management is a housekeeping pattern.
 */
export const EXCLUDED_TOOLS = new Set(["read", "grep", "glob", "todowrite"])

/**
 * Compute a normalised similarity key for a tool call.
 * Returns null for excluded tools (no detection).
 *
 * Ported from opencode's prism-hooks.ts — see that file for the full
 * commentary on similarity rules per tool.
 */
export function similarityKey(tool: string, args: unknown): string | null {
  if (EXCLUDED_TOOLS.has(tool)) {
    return null
  }

  switch (tool) {
    case "bash": {
      const cmd: string =
        typeof args === "object" && args !== null
          ? (args as Record<string, unknown>).command as string ?? ""
          : String(args ?? "")
      // Strip leading `cd <path> &&` or `cd <path>;` prefixes so that
      // commands like `cd /worktree && git push origin main` are keyed on
      // `git push origin` rather than `cd`. Bare `cd /foo` (no following
      // command) is NOT stripped — it remains `bash:cd` as a legitimate op.
      const stripped = cmd.trim().replace(/^(cd\s+\S+\s*(?:&&|;)\s*)+/, "")
      const tokens = stripped.split(/\s+/)
      const meaningful = tokens.filter((t) => t.length > 0)
      let baseIdx = 0
      while (baseIdx < meaningful.length && meaningful[baseIdx].startsWith("-")) {
        baseIdx++
      }
      const base = meaningful[baseIdx] ?? ""

      const SUBCOMMAND_CLIS = new Set(["gh", "git", "kubectl", "helm", "docker", "podman"])
      if (SUBCOMMAND_CLIS.has(base)) {
        const positionals: string[] = []
        for (let i = baseIdx + 1; i < meaningful.length; i++) {
          if (!meaningful[i].startsWith("-")) {
            positionals.push(meaningful[i])
          }
        }
        const parts = [base, ...positionals.slice(0, 3)]
        return `bash:${parts.join(" ")}`.trimEnd()
      }

      let firstPos = ""
      for (let i = baseIdx + 1; i < meaningful.length; i++) {
        if (!meaningful[i].startsWith("-")) {
          firstPos = meaningful[i]
          break
        }
      }
      return `bash:${base} ${firstPos}`.trimEnd()
    }

    case "edit":
    case "write": {
      const filePath: string =
        typeof args === "object" && args !== null
          ? ((args as Record<string, unknown>).filePath as string)
            ?? ((args as Record<string, unknown>).path as string)
            ?? ""
          : String(args ?? "")
      return `${tool}:${filePath}`
    }

    case "webfetch": {
      const url: string =
        typeof args === "object" && args !== null
          ? (args as Record<string, unknown>).url as string ?? ""
          : String(args ?? "")
      return `webfetch:${url}`
    }

    default: {
      const raw =
        typeof args === "object" && args !== null
          ? JSON.stringify(args)
          : String(args ?? "")
      return `${tool}:${raw}`
    }
  }
}

/** Doom-loop detector state (per session). */
export interface DoomLoopState {
  /** The similarity key of the current run. */
  currentKey: string | null
  /** How many consecutive matching calls we have seen. */
  consecutiveCount: number
  /** Whether we have already fired for this run (one nudge per loop). */
  fired: boolean
}

/** Review-cycle tracker state (per session). */
export interface ReviewCycleState {
  /** Cycle counts keyed by PR number (or "unknown"). */
  cycles: Map<string, number>
  /** The PR number most recently detected. */
  detectedPrNumber: string | null
  /**
   * Prevents counting 5 parallel review-agent invocations as 5 cycles.
   * Set to true when the first review agent fires; cleared on turn_start
   * so each full round counts as exactly one cycle.
   */
  pendingCycleCount: boolean
  /**
   * Tracks whether we've already emitted the review_cycle_exceeded wire
   * frame for the current PR. Prevents flooding the harness_frames table.
   * Reset when detectedPrNumber changes.
   */
  frameEmitted: boolean
}

/**
 * Create a fresh doom-loop state object.
 * Exported for unit testing.
 */
export function newDoomLoopState(): DoomLoopState {
  return { currentKey: null, consecutiveCount: 0, fired: false }
}

/**
 * Create a fresh review-cycle state object.
 * Exported for unit testing.
 */
export function newReviewCycleState(): ReviewCycleState {
  return { cycles: new Map(), detectedPrNumber: null, pendingCycleCount: false, frameEmitted: false }
}

/**
 * Process a single tool call against the doom-loop detector.
 *
 * Returns the steering message text when the detector fires, or null when
 * no action is needed. The caller is responsible for injecting the message
 * into the session.
 *
 * Exported for unit testing.
 */
export function processDoomLoop(
  state: DoomLoopState,
  toolName: string,
  toolArgs: unknown,
): string | null {
  if (EXCLUDED_TOOLS.has(toolName)) {
    // Break the current run without tracking.
    state.currentKey = null
    state.consecutiveCount = 0
    state.fired = false
    return null
  }

  const key = similarityKey(toolName, toolArgs)
  if (key === null) {
    return null
  }

  if (key === state.currentKey) {
    if (!state.fired) {
      state.consecutiveCount++
      if (state.consecutiveCount >= DOOM_LOOP_THRESHOLD) {
        state.fired = true
        return (
          `[PRISM DOOM-LOOP DETECTION] You've called \`${toolName}\` with the same arguments ` +
          `${state.consecutiveCount} times in a row. ` +
          `This usually means the current approach isn't working — stop and rethink. ` +
          `Consider: is there a different tool that would help? Is there a misunderstanding about the task? Should you escalate to the user?`
        )
      }
    }
    // already fired — suppress
    return null
  }

  // Pattern broke — reset and start fresh.
  state.currentKey = key
  state.consecutiveCount = 1
  state.fired = false
  return null
}

/**
 * Process a single tool call against the review-cycle tracker.
 *
 * This handles both `bash` calls to `prism review <N>` and `task` calls
 * to review subagents. Returns true when the review-cycle threshold is
 * exceeded (caller should inject escalation message).
 *
 * Exported for unit testing.
 */
export function processReviewCycle(
  state: ReviewCycleState,
  toolName: string,
  toolArgs: unknown,
): boolean {
  if (toolName === "bash") {
    const command: string =
      typeof toolArgs === "object" && toolArgs !== null
        ? (toolArgs as Record<string, unknown>).command as string ?? ""
        : String(toolArgs ?? "")

    const prismReviewMatch = command.match(/\bprism\s+review\s+(\d+)\b/)
    if (prismReviewMatch) {
      const newPr = prismReviewMatch[1]
      if (newPr !== state.detectedPrNumber) {
        state.detectedPrNumber = newPr
        state.cycles.clear()
        state.frameEmitted = false
      }
      if (!state.pendingCycleCount) {
        state.pendingCycleCount = true
        const prKey = state.detectedPrNumber ?? "unknown"
        state.cycles.set(prKey, (state.cycles.get(prKey) ?? 0) + 1)
      }
    }
  }

  if (toolName === "task") {
    const prompt: string =
      typeof toolArgs === "object" && toolArgs !== null
        ? (toolArgs as Record<string, unknown>).prompt as string ?? ""
        : String(toolArgs ?? "")
    const subagentType: string =
      typeof toolArgs === "object" && toolArgs !== null
        ? (toolArgs as Record<string, unknown>).subagent_type as string ?? ""
        : ""

    const isReviewAgent = /^review(-goal|-code|-security|-qa|-context)?$/.test(subagentType)
    if (isReviewAgent) {
      const prMatch =
        prompt.match(/\bPR\s*#(\d+)\b/i) ??
        prompt.match(/pull\s+request\s*#(\d+)/i) ??
        prompt.match(/\bpr[_\s-]?number[:\s]+(\d+)/i) ??
        prompt.match(/\b#(\d+)\b/)
      if (prMatch) {
        const newPr = prMatch[1]
        if (newPr !== state.detectedPrNumber) {
          state.detectedPrNumber = newPr
          state.cycles.clear()
          state.frameEmitted = false
        }
      }
      if (!state.pendingCycleCount) {
        state.pendingCycleCount = true
        const prKey = state.detectedPrNumber ?? "unknown"
        state.cycles.set(prKey, (state.cycles.get(prKey) ?? 0) + 1)
      }
    }
  }

  const prKey = state.detectedPrNumber ?? "unknown"
  return (state.cycles.get(prKey) ?? 0) >= REVIEW_CYCLE_THRESHOLD
}

/**
 * Custom entry type used to persist guard state snapshots in the session file.
 *
 * Snapshots are written via `pi.appendEntry(GUARD_STATE_ENTRY_TYPE, snapshot)`
 * after each significant state change. On session resume (`session_switch` with
 * `reason: "resume"`), the extension scans session entries from newest to oldest,
 * finds the latest snapshot, and restores state from it.
 *
 * This is the "reconstruct from session entries on resume" pattern described in
 * issue #1219 and referenced in the wire protocol design doc.
 */
export const GUARD_STATE_ENTRY_TYPE = "prism-guard-state"

/** Shape persisted in the session file for state reconstruction. */
export interface GuardStateSnapshot {
  /** Doom-loop state: the current similarity key and consecutive count. */
  doomLoop: {
    currentKey: string | null
    consecutiveCount: number
    fired: boolean
  }
  /** Review-cycle state: PR number and cycle counts as a plain object (Map not JSON-safe). */
  reviewCycle: {
    detectedPrNumber: string | null
    cycles: Record<string, number>
    frameEmitted: boolean
  }
  /** Whether a git-push reminder is pending injection. */
  pendingGitPushReminder: boolean
  /**
   * Whether a `prism review` call is in-flight (set on detection, cleared on
   * prompt delivery). Must be persisted so that a session pause/resume during
   * the review-wait window does not reset the guard and allow turn_end to
   * emit state_change:idle and clobber the reviewing state.
   */
  pendingReviewCall: boolean
}

/**
 * Serialise the current guard state into a snapshot object for persistence.
 * Exported for unit testing.
 */
export function snapshotGuardState(
  doomLoop: DoomLoopState,
  reviewCycle: ReviewCycleState,
  pendingGitPushReminder: boolean,
  pendingReviewCall: boolean,
): GuardStateSnapshot {
  const cycles: Record<string, number> = {}
  for (const [k, v] of reviewCycle.cycles) {
    cycles[k] = v
  }
  return {
    doomLoop: {
      currentKey: doomLoop.currentKey,
      consecutiveCount: doomLoop.consecutiveCount,
      fired: doomLoop.fired,
    },
    reviewCycle: {
      detectedPrNumber: reviewCycle.detectedPrNumber,
      cycles,
      frameEmitted: reviewCycle.frameEmitted,
    },
    pendingGitPushReminder,
    pendingReviewCall,
  }
}

/**
 * Restore guard state from a snapshot. Mutates the provided state objects in
 * place. Exported for unit testing.
 */
export function restoreGuardState(
  snapshot: GuardStateSnapshot,
  doomLoop: DoomLoopState,
  reviewCycle: ReviewCycleState,
): { pendingGitPushReminder: boolean; pendingReviewCall: boolean } {
  doomLoop.currentKey = snapshot.doomLoop.currentKey ?? null
  doomLoop.consecutiveCount = typeof snapshot.doomLoop.consecutiveCount === "number"
    ? snapshot.doomLoop.consecutiveCount
    : 0
  doomLoop.fired = snapshot.doomLoop.fired === true

  reviewCycle.detectedPrNumber = snapshot.reviewCycle.detectedPrNumber ?? null
  reviewCycle.cycles.clear()
  if (snapshot.reviewCycle.cycles && typeof snapshot.reviewCycle.cycles === "object") {
    for (const [k, v] of Object.entries(snapshot.reviewCycle.cycles)) {
      if (typeof v === "number") {
        reviewCycle.cycles.set(k, v)
      }
    }
  }
  reviewCycle.frameEmitted = snapshot.reviewCycle.frameEmitted === true

  return {
    pendingGitPushReminder: snapshot.pendingGitPushReminder === true,
    pendingReviewCall: snapshot.pendingReviewCall === true,
  }
}
export function reviewCycleEscalationMessage(
  state: ReviewCycleState,
): string {
  const prKey = state.detectedPrNumber ?? "unknown"
  const cycles = state.cycles.get(prKey) ?? 0
  const prLabel = state.detectedPrNumber ? `PR #${state.detectedPrNumber}` : "this PR"
  return (
    `⚠️ REVIEW LOOP LIMIT: You have run ${cycles} review cycles for ${prLabel} without all agents passing. ` +
    `You MUST stop and escalate to the user now. Do NOT run another review cycle. ` +
    `Instead, summarise: (1) what was originally requested, (2) what each review cycle found, ` +
    `and (3) why the fixes are not converging. Hand off to the coordinator.`
  )
}

/**
 * Check whether a bash command string is a git push.
 * Exported for unit testing.
 */
export function isGitPush(command: string): boolean {
  return /\bgit(\s+-C\s+\S+|\s+--git-dir\S*)*\s+push\b/.test(command)
}

/** Per-field byte cap for tool args, tool output, and assistant message deltas. */
export const TRUNCATION_LIMIT_BYTES = 8 * 1024 // 8 KiB

/** Sentinel appended to truncated string fields. */
export const TRUNCATION_SENTINEL = "…[truncated]"

/** Harness identifier sent in the `hello` frame. */
export const HARNESS_NAME = "pi"

/**
 * The message injected as a steer after the agent runs `git push`.
 * Exported so unit tests can assert against the canonical text.
 */
export const GIT_PUSH_REMINDER_MESSAGE =
  "You just ran git push. If this was in the context of an open PR, first load the `prism` skill via the skill tool so you have the full async review workflow context, then run `prism review <pr-number>` to kick off the parallel review. Wait for the review-complete prompt before merging."

// ---------------------------------------------------------------------------
// Status bar helper — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Format the prism status bar text for PI's footer.
 *
 * Format:
 *   [coordinator] main
 *   [worker] fix-login-redirect
 *   [review] PR#42 · 2 cycles
 *
 * @param role         - session role from hello_ack (e.g. "coordinator", "worker", "review")
 * @param branch       - branch label extracted from session_name (part after "@", or full name)
 * @param prNumber     - detected PR number (null when unknown)
 * @param cycles       - current review cycle count
 */
export function formatPrismStatus(
  role: string,
  branch: string,
  prNumber: string | null,
  cycles: number,
): string {
  const roleLabel = role.length > 0 ? role : "unknown"
  const prefix = `[${roleLabel}] ${branch}`
  if (roleLabel === "review" && prNumber !== null) {
    const cycleLabel = cycles === 1 ? "1 cycle" : `${cycles} cycles`
    return `${prefix} · PR#${prNumber} · ${cycleLabel}`
  }
  return prefix
}

/**
 * Extract the branch label from a session_name.
 * If session_name contains "@", return the part after it.
 * Otherwise return the full session_name.
 */
export function extractBranch(sessionName: string): string {
  const atIdx = sessionName.indexOf("@")
  if (atIdx !== -1) {
    return sessionName.slice(atIdx + 1)
  }
  return sessionName
}

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
    // The dispatcher only ever passes "steer" or "followUp"; for the wire
    // protocol's `nextTurn` it omits options entirely so PI's idle-vs-
    // streaming logic decides. The narrower union here documents that.
    options?: { deliverAs?: "steer" | "followUp" },
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
// turn_end signal resolver — exported for unit testing.
// ---------------------------------------------------------------------------

/**
 * Valid stop reasons as defined by the PI agent.
 *
 * - "stop"      — model declared end_turn cleanly
 * - "toolUse"   — turn ended to execute tools; agent is not done
 * - "length"    — context limit hit; agent may continue
 * - "error"     — error during generation
 * - "aborted"   — user interrupted (e.g. Escape key)
 */
export type StopReason = "stop" | "toolUse" | "length" | "error" | "aborted"

/**
 * Signal that the turn_end handler should emit.
 *
 * - "finished"     — emit state_change:finished, session_shutdown, close writer
 * - "interrupted"  — emit state_change:interrupted
 * - "idle"         — emit state_change:idle (existing behaviour)
 * - "none"         — do not emit any state_change frame
 */
export type TurnEndSignal = "finished" | "interrupted" | "idle" | "none"

/**
 * Determines which state-change signal to emit at the end of a turn.
 *
 * Priority order:
 *  1. If stopReason is "aborted" → "interrupted" (always, regardless of idle)
 *  2. If stopReason is "stop" AND isIdle AND !hasPendingMessages AND
 *     !pendingReviewCall → "finished"
 *  3. If stopReason is "stop" (or any non-aborted, non-error reason) AND
 *     isIdle AND !hasPendingMessages AND !pendingReviewCall → "idle"
 *  4. Otherwise → "none"
 *
 * The "finished" path is only taken on a clean "stop" to avoid false-positives
 * from tool-use mid-turn or context-limit truncation.
 */
export function resolveTurnEndSignal(
  stopReason: StopReason | string | undefined,
  isIdle: boolean,
  hasPendingMessages: boolean,
  pendingReviewCall: boolean,
): TurnEndSignal {
  if (stopReason === "aborted") {
    return "interrupted"
  }
  if (
    stopReason === "stop" &&
    isIdle &&
    !hasPendingMessages &&
    !pendingReviewCall
  ) {
    return "finished"
  }
  if (
    stopReason !== "error" &&
    isIdle &&
    !hasPendingMessages &&
    !pendingReviewCall
  ) {
    return "idle"
  }
  return "none"
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
// Connection guard helper (exported for testing).
// ---------------------------------------------------------------------------

/**
 * Returns true if the extension should attempt to connect to the sidecar.
 *
 * The guard prevents a duplicate connection when PI fires `session_start`
 * while a previous connection is still live (e.g. the `/new` command
 * triggers `session_start` before the existing socket close event fires).
 * In that case the sidecar's reconnect loop will re-accept on its own once
 * the old connection drops — the extension must not race it with a second
 * dial attempt.
 */
export function shouldAttemptConnect(
  socket: unknown | null,
  connected: boolean,
): boolean {
  return socket === null && !connected
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

  // ── Behavioural guard state ───────────────────────────────────────────
  //
  // Disabled via env vars for debugging:
  //   PRISM_DOOM_LOOP_DISABLE=1        — disable doom-loop detector
  //   PRISM_REVIEW_CYCLE_DISABLE=1     — disable review-cycle escalation
  //   PRISM_GIT_PUSH_REMINDER_DISABLE=1 — disable git-push reminder
  //
  // Review agents have legitimate repeated tool patterns so all guards are
  // suppressed when the session_role from hello_ack is "review".
  const doomLoopEnabled = !process.env.PRISM_DOOM_LOOP_DISABLE
  const reviewCycleEnabled = !process.env.PRISM_REVIEW_CYCLE_DISABLE
  const gitPushReminderEnabled = !process.env.PRISM_GIT_PUSH_REMINDER_DISABLE

  const doomLoopState = newDoomLoopState()
  const reviewCycleState = newReviewCycleState()

  // Set to true when hello_ack reveals session_role is "review". All guards
  // are suppressed — review agents legitimately repeat tool patterns.
  let isReviewSession = false

  // Session identity captured from hello_ack. Used for status bar display.
  let sessionRole = ""
  let sessionBranch = extractBranch(process.env.PRISM_SESSION_NAME ?? "")

  // Flag: git push was detected; cleared after the next turn_start so the
  // reminder fires exactly once.
  let pendingGitPushReminder = false

  // Flag: `prism review` was called during this session. When true, the
  // turn_end idle emission is suppressed so that the `reviewing` state is
  // not clobbered while waiting for the review-complete prompt. Cleared
  // when a `prompt` inbound frame arrives (review-complete delivery) so
  // that subsequent turns can go idle normally.
  let pendingReviewCall = false

  // ── Connection state ──────────────────────────────────────────────────
  let socket: net.Socket | null = null
  let writer: FrameWriter | null = null
  let handshakeComplete = false
  let connected = false
  // Flag: session_shutdown has already been emitted from the turn_end handler
  // (clean-finish path). When true, the session_shutdown hook must not emit
  // a second finished+shutdown sequence or attempt to write to a closed writer.
  let sessionShutdownEmitted = false
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
      socket = null
      writer = null
      handshakeComplete = false
      // Per wire spec §7.6: if the sidecar dies, PI continues running
      // standalone. We don't try to reconnect here; the sidecar's reconnect
      // loop re-accepts on session_start.
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
        // Detect review-agent sessions: guards are suppressed so that
        // review agents (which legitimately repeat tool patterns) are not
        // falsely nudged.
        const role = typeof f.session_role === "string" ? f.session_role : ""
        if (role === "review") {
          isReviewSession = true
        }
        sessionRole = role
        // Override branch from hello_ack session_name if provided.
        if (typeof f.session_name === "string" && f.session_name.length > 0) {
          sessionBranch = extractBranch(f.session_name)
        }
        handshakeComplete = true

        // Set the status bar immediately after handshake.
        const prCycles = sessionRole === "review"
          ? (reviewCycleState.cycles.get(reviewCycleState.detectedPrNumber ?? "unknown") ?? 0)
          : 0
        const statusText = formatPrismStatus(sessionRole, sessionBranch, reviewCycleState.detectedPrNumber, prCycles)
        lastCtx?.ui?.setStatus("prism", statusText)
        if (writer) {
          writer.write({
            type: "session_status",
            role: sessionRole,
            branch: sessionBranch,
            review_cycles: prCycles,
            pr_number: reviewCycleState.detectedPrNumber ?? "",
          })
        }
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

      // A prompt frame signals that the sidecar is delivering a message
      // (e.g. review-complete). Clear the reviewing-state guard so subsequent
      // turns can emit state_change:idle normally.
      if (f.type === "prompt") {
        pendingReviewCall = false
      }

      void dispatchInboundFrame(f, apiAdapter, sendError)
    })
  }

  // ── Outbound hooks ────────────────────────────────────────────────────

  pi.on("session_start", async (_event, ctx) => {
    lastCtx = ctx
    // Connect on session_start. The wire spec requires the extension to dial
    // out; the sidecar has already bound the listener.
    //
    // Guard: if a socket connection is already active (e.g. after /new
    // triggers session_start while the previous connection is still live),
    // skip the reconnect — the sidecar's reconnect loop handles it.
    if (!shouldAttemptConnect(socket, connected)) return
    connect()
  })

  // ── State reconstruction on session resume ───────────────────────────
  //
  // When PI resumes a session (session_switch with reason="resume"), the
  // extension scans the session's branch entries for the latest
  // GUARD_STATE_ENTRY_TYPE snapshot and restores state from it. This
  // ensures the doom-loop consecutive count, review-cycle counts, and
  // git-push reminder flag survive a session pause/resume cycle.
  //
  // The entry is written whenever significant state changes occur (see
  // the tool_execution_start hook below). pendingCycleCount is not
  // persisted (it is cleared on turn_start anyway).
  pi.on("session_switch", async (event, ctx) => {
    lastCtx = ctx
    if ((event as { reason?: unknown }).reason !== "resume") return
    if (isReviewSession) return // guards are suppressed for review sessions

    const entries = ctx.sessionManager.getBranch()
    // Walk backward to find the most recent guard state snapshot.
    for (let i = entries.length - 1; i >= 0; i--) {
      const entry = entries[i]
      if (
        entry !== null &&
        typeof entry === "object" &&
        (entry as { type?: unknown }).type === "custom" &&
        (entry as { customType?: unknown }).customType === GUARD_STATE_ENTRY_TYPE
      ) {
        const snapshot = (entry as { data?: unknown }).data as GuardStateSnapshot | undefined
        if (snapshot && typeof snapshot === "object") {
          const restored = restoreGuardState(snapshot, doomLoopState, reviewCycleState)
          pendingGitPushReminder = restored.pendingGitPushReminder
          pendingReviewCall = restored.pendingReviewCall
        }
        break
      }
    }
  })

  pi.on("before_agent_start", async (_event, ctx) => {
    lastCtx = ctx
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
    // Clear per-turn review-cycle deduplication so the next batch of
    // review agent invocations counts as a fresh cycle.
    reviewCycleState.pendingCycleCount = false

    // Refresh status bar on each turn_start so review-cycle count is live.
    if (handshakeComplete) {
      const prCycles = reviewCycleState.cycles.get(reviewCycleState.detectedPrNumber ?? "unknown") ?? 0
      const statusText = formatPrismStatus(sessionRole, sessionBranch, reviewCycleState.detectedPrNumber, prCycles)
      lastCtx?.ui?.setStatus("prism", statusText)
      if (writer) {
        writer.write({
          type: "session_status",
          role: sessionRole,
          branch: sessionBranch,
          review_cycles: prCycles,
          pr_number: reviewCycleState.detectedPrNumber ?? "",
        })
      }
    }

    if (!isReviewSession) {
      // Inject review-cycle escalation warning when the threshold is exceeded.
      // This fires on every turn after the threshold so the agent cannot
      // simply ignore it and continue.
      if (reviewCycleEnabled) {
        const prKey = reviewCycleState.detectedPrNumber ?? "unknown"
        const cycles = reviewCycleState.cycles.get(prKey) ?? 0
        if (cycles >= REVIEW_CYCLE_THRESHOLD) {
          pi.sendUserMessage(reviewCycleEscalationMessage(reviewCycleState), {
            deliverAs: "steer",
          })
          // Emit the wire frame only once per PR (not on every turn).
          if (!reviewCycleState.frameEmitted && writer && handshakeComplete) {
            reviewCycleState.frameEmitted = true
            writer.write({
              type: "review_cycle_exceeded",
              session_name: process.env.PRISM_SESSION_NAME ?? "",
              pr_number: reviewCycleState.detectedPrNumber ?? "",
              cycle_count: cycles,
            })
          }
        }
      }

      // Inject git-push reminder if a push was detected on the previous turn.
      if (gitPushReminderEnabled && pendingGitPushReminder) {
        pendingGitPushReminder = false
        pi.sendUserMessage(
          GIT_PUSH_REMINDER_MESSAGE,
          { deliverAs: "steer" },
        )
      }
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

      // Determine which state-change signal to emit based on stopReason and
      // session context. resolveTurnEndSignal handles all cases:
      //   "finished"    → clean finish; emit finished + shutdown and close
      //   "interrupted" → user abort; emit interrupted
      //   "idle"        → normal idle; emit idle (existing behaviour)
      //   "none"        → do nothing
      try {
        const stopReason =
          message !== null && typeof message === "object"
            ? (message as { stopReason?: unknown }).stopReason
            : undefined
        const isIdle =
          typeof ctx.isIdle === "function" ? ctx.isIdle() : false
        const hasPending =
          typeof ctx.hasPendingMessages === "function"
            ? ctx.hasPendingMessages()
            : false
        const signal = resolveTurnEndSignal(
          stopReason as StopReason | undefined,
          isIdle,
          hasPending,
          pendingReviewCall,
        )
        if (signal === "finished") {
          sessionShutdownEmitted = true
          writer.write({ type: "state_change", state: "finished" })
          writer.write({ type: "session_shutdown" })
          writer.close()
        } else if (signal === "interrupted") {
          writer.write({ type: "state_change", state: "interrupted" })
        } else if (signal === "idle") {
          writer.write({ type: "state_change", state: "idle" })
        }
      } catch (err) {
        console.error("[prism-extension] turn_end signal resolution failed:", err)
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

    // ── Behavioural guards ────────────────────────────────────────────────
    // Guards are suppressed inside review-agent sessions.
    if (!isReviewSession) {
      // Doom-loop detection.
      if (doomLoopEnabled) {
        const steeringMsg = processDoomLoop(doomLoopState, name, rawArgs)
        if (steeringMsg !== null) {
          pi.sendUserMessage(steeringMsg, { deliverAs: "steer" })
          writer.write({
            type: "doom_loop_detected",
            session_name: process.env.PRISM_SESSION_NAME ?? "",
            tool: name,
            consecutive_count: doomLoopState.consecutiveCount,
          })
        }
      }

      // Review-cycle tracking + git-push detection.
      if (name === "bash") {
        const command: string =
          typeof rawArgs === "object" && rawArgs !== null
            ? (rawArgs as Record<string, unknown>).command as string ?? ""
            : String(rawArgs ?? "")

        // Git-push reminder flag.
        if (gitPushReminderEnabled && isGitPush(command)) {
          pendingGitPushReminder = true
        }

        // Reviewing-state guard: set flag when `prism review` is called so
        // that the turn_end idle emission is suppressed until the
        // review-complete prompt arrives.
        if (/\bprism\s+review\b/.test(command)) {
          pendingReviewCall = true
        }
      }

      if (reviewCycleEnabled) {
        processReviewCycle(reviewCycleState, name, rawArgs)
      }

      // Persist guard state after each tool call so session resume can
      // reconstruct the current doom-loop run and review-cycle counts.
      // pi.appendEntry is lightweight (appends to the session JSON file);
      // we do it unconditionally — the reviewer can deduplicate via the
      // "latest entry wins" pattern in session_switch above.
      pi.appendEntry(GUARD_STATE_ENTRY_TYPE,
        snapshotGuardState(doomLoopState, reviewCycleState, pendingGitPushReminder, pendingReviewCall))
    }
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
    if (writer && handshakeComplete) {
      // If the turn_end handler already emitted finished+shutdown (clean-finish
      // path), do not double-emit. The writer is already closed in that case.
      if (!sessionShutdownEmitted) {
        writer.write({ type: "state_change", state: "finished" })
        writer.write({ type: "session_shutdown" })
        writer.close()
      }
    } else if (writer) {
      writer.close()
    }
    if (socket && !connected) {
      try {
        socket.destroy()
      } catch {}
    }
  })
}
