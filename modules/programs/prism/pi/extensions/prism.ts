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
//   3. Send `hello` frame (protocol_version=2).
//   4. Wait for `hello_ack`. Validate protocol_version. Disconnect on mismatch.
//   5. Set up the buffered line reader and the synchronous writer.
//
// Outbound hooks → frames
// -----------------------
//   before_agent_start    → state_change:active
//   tool_execution_start  → tool_call           (truncated args)
//   tool_execution_end    → tool_result         (truncated output)
//   turn_start            → turn_start
//   turn_end              → turn_end + state_change:finished
//                            (stopReason=stop, no pending, not reviewing)
//                         → turn_end + state_change:interrupted (stopReason=aborted)
//                         → turn_end (no state_change, other cases)
//   message_update        → msg_assistant       (text_delta only, truncated)
//   after_provider_response (non-OK)
//                         → provider_error
//   auto_retry_start      → auto_retry_start
//   auto_retry_end        → auto_retry_end
//   session_shutdown      → writer.close() only (all session types)
//
// Note: agent_end no longer emits any state_change frames (removed in #1434).
// The unified turn_end → state_change{finished} path handles all session types.
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

/** Wire protocol version this extension speaks.
 * Bumped from 1→2 in #1434: turn_end now emits state_change{finished}
 * directly instead of state_change{idle}. */
export const PROTOCOL_VERSION = 2

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

// NOTE: review-cycle escalation is now driven entirely by Go-side code. The
// `prism review` monitor appends a LOOP-LIMIT footer to the review-complete
// prompt body when the parent's review-group history shows >= 3 verdict-
// producing cycles without convergence (#1512, Shape B). The TS extension
// no longer counts cycles and no longer injects per-turn warnings, which
// dissolves both the per-turn spam and the bash-substring false-match
// defects at the source.

/**
 * Tools excluded from doom-loop detection. Legitimate exploration revisits
 * the same paths often; treating them as loops generates false positives.
 * todowrite is excluded because task-list management is a housekeeping pattern.
 */
export const EXCLUDED_TOOLS = new Set(["read", "grep", "glob", "todowrite"])

/**
 * Bash command bases that are search/read/inspect operations. Excluded from
 * doom-loop detection for the same reason EXCLUDED_TOOLS excludes the native
 * read/grep/glob tools: legitimate exploration revisits the same pattern
 * across many files, and the bash similarity key collapses
 * `<base> "<pattern>" <file1>`, `<base> "<pattern>" <file2>`, … to one key
 * because positional #2+ is discarded by the default bash key formula.
 */
export const EXCLUDED_BASH_BASES = new Set([
  "grep", "rg", "ag",                              // search
  "find", "fd",                                    // file listing
  "cat", "head", "tail", "less", "more",           // read
  "ls", "tree", "stat", "file", "wc",              // inspect
  "awk", "sed", "cut", "sort", "uniq",             // text munging (read-only when piped)
  "jq", "yq",                                      // structured read
])

/**
 * Compute a normalised similarity key for a tool call.
 * Returns null for excluded tools (no detection).
 *
 * Ported from the prism-hooks.ts plugin — see that file for the full
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

      if (EXCLUDED_BASH_BASES.has(base)) {
        return null
      }

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

/**
 * Create a fresh doom-loop state object.
 * Exported for unit testing.
 */
export function newDoomLoopState(): DoomLoopState {
  return { currentKey: null, consecutiveCount: 0, fired: false }
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
  const key = similarityKey(toolName, toolArgs)
  if (key === null) {
    // Excluded by similarityKey (either an excluded tool or an excluded bash
    // base). Treat as a run-breaker for symmetry with the EXCLUDED_TOOLS path.
    state.currentKey = null
    state.consecutiveCount = 0
    state.fired = false
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
 *
 * NOTE: review-cycle counting was removed in #1512 (Shape B). Old snapshots
 * with a `reviewCycle` field still parse correctly via restoreGuardState
 * — the field is simply ignored.
 */
export function snapshotGuardState(
  doomLoop: DoomLoopState,
  pendingGitPushReminder: boolean,
  pendingReviewCall: boolean,
): GuardStateSnapshot {
  return {
    doomLoop: {
      currentKey: doomLoop.currentKey,
      consecutiveCount: doomLoop.consecutiveCount,
      fired: doomLoop.fired,
    },
    pendingGitPushReminder,
    pendingReviewCall,
  }
}

/**
 * Restore guard state from a snapshot. Mutates the provided state objects in
 * place. Exported for unit testing.
 *
 * Tolerates legacy snapshots that include a `reviewCycle` field (pre-#1512);
 * such fields are silently ignored.
 */
export function restoreGuardState(
  snapshot: GuardStateSnapshot,
  doomLoop: DoomLoopState,
): { pendingGitPushReminder: boolean; pendingReviewCall: boolean } {
  doomLoop.currentKey = snapshot.doomLoop?.currentKey ?? null
  doomLoop.consecutiveCount = typeof snapshot.doomLoop?.consecutiveCount === "number"
    ? snapshot.doomLoop.consecutiveCount
    : 0
  doomLoop.fired = snapshot.doomLoop?.fired === true

  return {
    pendingGitPushReminder: snapshot.pendingGitPushReminder === true,
    pendingReviewCall: snapshot.pendingReviewCall === true,
  }
}

/**
 * Check whether a bash command string is a real `git push` invocation.
 *
 * The previous implementation matched the literal substring `git ... push`
 * anywhere in the raw command, which produced false positives for:
 *   - quoted arguments:  echo "git push", rg "git push"
 *   - heredoc bodies:    cat <<'EOF' ... git push ... EOF
 *   - log filters:       git log --grep="git push"
 *   - awk/sed patterns:  awk '/git push/ { ... }'
 *
 * The principled fix (issue #1519) is to:
 *   1. Strip heredoc bodies, single-quoted regions, and double-quoted
 *      regions from the command — these never contain a real invocation.
 *   2. Split what remains on shell separators (`;`, newline, `&&`, `||`,
 *      `|`, `&`, and command/process substitution boundaries) so each
 *      pipeline / sequence stage is examined independently.
 *   3. Tokenise each segment on whitespace and check whether the leading
 *      tokens form `git [-C <path>] [--git-dir=...|--git-dir <path>] push`.
 *
 * Exported for unit testing.
 */
export function isGitPush(command: string): boolean {
  const stripped = stripQuotedAndHeredocRegions(command)
  for (const segment of splitShellSegments(stripped)) {
    if (segmentIsGitPush(segment)) return true
  }
  return false
}

/**
 * Remove heredoc bodies, single-quoted strings, and double-quoted strings
 * from a bash command. Heredoc start tokens (`<<EOF`, `<<-EOF`, `<<'EOF'`,
 * `<<"EOF"`) are also removed so they do not pollute the surrounding
 * segment, but the rest of the command line containing them is preserved.
 *
 * This is a deliberately small lexer — it does not implement full POSIX
 * shell quoting (no `$'...'`, no parameter expansion). It targets the
 * dominant false-match cases described in #1519.
 *
 * Exported for unit testing.
 */
export function stripQuotedAndHeredocRegions(command: string): string {
  // Pass 1: strip heredocs.
  // Find each `<<[-]?(['"]?)WORD\1` marker on a line, drop the marker, then
  // drop subsequent lines until a line whose trimmed content equals WORD.
  const lines = command.split("\n")
  const out: string[] = []
  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    // Match the first heredoc opener on this line. We do not try to handle
    // multiple heredocs on a single line — that is a vanishingly rare shape
    // in agent-issued commands and stripping the first marker is sufficient
    // to avoid a false match on its body.
    const heredocRe = /<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\1/
    const m = line.match(heredocRe)
    if (m && m.index !== undefined) {
      const word = m[2]
      // Keep the line with the marker removed.
      out.push(line.slice(0, m.index) + line.slice(m.index + m[0].length))
      i++
      // Skip body lines until we see the terminator. For `<<-WORD` the
      // terminator may be indented; for `<<WORD` it must be flush. We treat
      // both the same because being lenient errs on the side of stripping.
      while (i < lines.length && lines[i].trim() !== word) {
        i++
      }
      // Skip the terminator line itself if present.
      if (i < lines.length) i++
      continue
    }
    out.push(line)
    i++
  }

  // Pass 2: strip quoted regions on the surviving text.
  const text = out.join("\n")
  let result = ""
  let j = 0
  while (j < text.length) {
    const ch = text[j]
    if (ch === "\\" && j + 1 < text.length) {
      // Preserve escaped characters as-is so e.g. `\"` does not open a
      // double-quoted region. We replace with a space so token boundaries
      // are not accidentally created/removed.
      result += " "
      j += 2
      continue
    }
    if (ch === "'") {
      // Single quotes: no escapes inside; consume to next `'`.
      const end = text.indexOf("'", j + 1)
      if (end === -1) {
        // Unterminated — drop the rest.
        break
      }
      result += " " // placeholder to preserve token boundaries
      j = end + 1
      continue
    }
    if (ch === '"') {
      // Double quotes: skip to next unescaped `"`.
      let k = j + 1
      while (k < text.length) {
        if (text[k] === "\\" && k + 1 < text.length) {
          k += 2
          continue
        }
        if (text[k] === '"') break
        k++
      }
      if (k >= text.length) break
      result += " "
      j = k + 1
      continue
    }
    result += ch
    j++
  }
  return result
}

/**
 * Split a (quote-stripped) command string into individual command segments
 * along shell separators. Process substitutions `<(...)` and `>(...)`,
 * command substitutions `$(...)`, and backtick `` `...` `` regions are
 * promoted to their own segments so a `git push` nested inside one is
 * still considered a real invocation, while the surrounding command is
 * also examined.
 */
export function splitShellSegments(command: string): string[] {
  // Replace top-level shell separators with a sentinel \x00, while also
  // splitting out substitution bodies as their own segments by replacing
  // their delimiters with sentinels too.
  const SEP = "\x00"
  let s = ""
  for (let i = 0; i < command.length; i++) {
    const c = command[i]
    const next = command[i + 1]
    // Two-char operators first.
    if ((c === "&" && next === "&") || (c === "|" && next === "|")) {
      s += SEP
      i++
      continue
    }
    // Process / command substitution openers: `<(`, `>(`, `$(`.
    if ((c === "<" || c === ">" || c === "$") && next === "(") {
      s += SEP
      i++
      continue
    }
    if (
      c === "|" ||
      c === "&" ||
      c === ";" ||
      c === "\n" ||
      c === "(" ||
      c === ")" ||
      c === "`"
    ) {
      s += SEP
      continue
    }
    s += c
  }
  return s
    .split(SEP)
    .map((seg) => seg.trim())
    .filter((seg) => seg.length > 0)
}

/**
 * Decide whether a single tokenised command segment is a `git push`
 * invocation. Accepts the leading-flag forms the prior regex accepted:
 *   git push ...
 *   git -C <path> push ...
 *   git --git-dir <path> push ...
 *   git --git-dir=<path> push ...
 * and any combination/repetition of -C and --git-dir before `push`.
 */
function segmentIsGitPush(segment: string): boolean {
  const tokens = segment.split(/\s+/).filter((t) => t.length > 0)
  if (tokens.length < 2) return false
  if (tokens[0] !== "git") return false
  let i = 1
  while (i < tokens.length) {
    const t = tokens[i]
    if (t === "-C" || t === "--git-dir") {
      // Two-token form: skip the option and its argument.
      if (i + 1 >= tokens.length) return false
      i += 2
      continue
    }
    if (t.startsWith("--git-dir=")) {
      i += 1
      continue
    }
    break
  }
  return tokens[i] === "push"
}

// ---------------------------------------------------------------------------
// Pre-tool-call bash deny list (#1528)
// ---------------------------------------------------------------------------

/**
 * One entry in the bash deny list. Each entry is matched against an
 * already-tokenised command segment (i.e. after `stripQuotedAndHeredocRegions`
 * + `splitShellSegments` have run on the raw command).
 *
 * The `match` predicate receives a single segment string and returns true
 * when the segment is a real invocation of the blocked command. The `reason`
 * surfaces verbatim to the agent via pi's tool-result channel and should
 * name the recommended alternative.
 */
export interface BlockedBashPattern {
  id: string
  match: (segment: string) => boolean
  reason: string
}

/**
 * Bash commands that the pi extension blocks before pi executes them.
 *
 * Add to this list only when a specific incident motivates a new entry.
 * The permission block has ~50 entries because it is the primary
 * enforcement surface for agents; the pi extension grows
 * incrementally as agent behaviour demands. Keep entries tightly scoped
 * — they are mandatory blocks with no per-session opt-out.
 *
 * Patterns match "real" git invocations after stripping quoted and
 * heredoc regions and splitting on shell separators — same approach as
 * `isGitPush`. The leading-flag form mirrors `segmentIsGitPush`: each
 * regex accepts an optional `git -C <path>` and/or `git --git-dir[=]<path>`
 * prefix before the subcommand.
 */
export const BLOCKED_BASH_PATTERNS: readonly BlockedBashPattern[] = [
  {
    id: "git-worktree-prune",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+worktree\s+prune\b/.test(
        segment,
      ),
    reason:
      "blocked by prism extension: `git worktree prune` from inside a " +
      "sandboxed agent will sever sibling sessions' git trees because " +
      "their worktrees are not bind-mounted into your view. Use " +
      "`prism cleanup --yes --session <name>` instead, or escalate " +
      "to the user if the residual state is from a partial spawn.",
  },
  {
    id: "git-worktree-remove",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+worktree\s+remove\b/.test(
        segment,
      ),
    reason:
      "blocked by prism extension: `git worktree remove` from inside " +
      "a sandboxed agent risks removing sibling sessions' worktrees " +
      "(they are not bind-mounted into your view). Use " +
      "`prism cleanup --yes --session <name>` instead.",
  },
]

/**
 * Check a raw bash command against the deny list.
 *
 * Returns the matching pattern's `{ id, reason }` on the first hit, or
 * `null` when no pattern matches. The check strips quoted/heredoc regions
 * and splits on shell separators first, so e.g. `echo "git worktree prune"`
 * does not fire a block but `cd /repo && git worktree prune` does
 * (the second segment).
 *
 * Exported for unit testing.
 */
export function checkBlockedBash(
  command: string,
): { id: string; reason: string } | null {
  const stripped = stripQuotedAndHeredocRegions(command)
  for (const segment of splitShellSegments(stripped)) {
    for (const pattern of BLOCKED_BASH_PATTERNS) {
      if (pattern.match(segment)) {
        return { id: pattern.id, reason: pattern.reason }
      }
    }
  }
  return null
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
 *   [coordinator] obsidian (host)
 *
 * The isolation mode suffix is appended only when the session is running in
 * "host" isolation mode (or when isolation_mode is absent/unknown, treated
 * the same as "host"). Sandboxed modes ("sandbox-exec", "bwrap") produce no
 * suffix — they are the normal/expected case.
 *
 * @param role          - session role from hello_ack (e.g. "coordinator", "worker", "review")
 * @param branch        - branch label extracted from session_name (part after "@", or full name)
 * @param isolationMode - isolation mode from hello_ack ("sandbox-exec", "bwrap", "host", or "" for absent)
 * @param prNumber      - detected PR number (null when unknown)
 * @param cycles        - current review cycle count
 */
export function formatPrismStatus(
  role: string,
  branch: string,
  isolationMode: string,
  prNumber: string | null,
  cycles: number,
): string {
  const roleLabel = role.length > 0 ? role : "unknown"
  const prefix = `[${roleLabel}] ${branch}`
  // Append isolation mode suffix when host mode (or absent/unknown).
  // Sandboxed modes ("sandbox-exec", "bwrap") are the default — no suffix.
  const isHostMode = isolationMode !== "sandbox-exec" && isolationMode !== "bwrap"
  const isolationSuffix = isHostMode ? " (host)" : ""
  if (roleLabel === "review" && prNumber !== null) {
    const cycleLabel = cycles === 1 ? "1 cycle" : `${cycles} cycles`
    return `${prefix}${isolationSuffix} · PR#${prNumber} · ${cycleLabel}`
  }
  return `${prefix}${isolationSuffix}`
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

export interface FrameWriter {
  write(frame: Record<string, unknown>): void
  close(): void
}

export function makeFrameWriter(socket: net.Socket): FrameWriter {
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
    // For "steer" and "followUp" the option is forwarded verbatim.
    // For "nextTurn", the dispatcher queries isIdle() and routes to
    // "followUp" mid-stream (when isIdle returns false) or calls bare
    // sendUserMessage (when idle). The full union is kept here so the
    // adapter in the factory can forward the value PI actually supports.
    options?: { deliverAs?: "steer" | "followUp" | "nextTurn" },
  ) => void
  // isIdle returns true when the PI runtime is not currently streaming a
  // turn. Used by the dispatcher to choose between bare sendUserMessage
  // (idle) and sendUserMessage({ deliverAs: "followUp" }) (mid-stream)
  // when the wire frame carries deliver_as="nextTurn". Implementations on
  // older PI runtimes that do not expose isIdle may omit the method; the
  // dispatcher treats its absence as idle=true (pre-streaming behaviour
  // preserved).
  isIdle?: () => boolean
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

        // PI's sendUserMessage requires an explicit deliverAs whenever the
        // runtime is streaming. For the wire's "nextTurn" value, query
        // isIdle and route to "followUp" mid-stream so the message queues
        // until the current turn settles. Calling sendUserMessage without
        // deliverAs while streaming throws "Agent is already processing",
        // which would silently drop the frame in this dispatcher's outer
        // try/catch. When isIdle is unavailable (older PI runtime), treat
        // the runtime as idle and call bare sendUserMessage (pre-streaming
        // behaviour preserved).
        let isIdleAtDecision: boolean | undefined
        try {
          if (deliverAs === "nextTurn") {
            isIdleAtDecision =
              typeof api.isIdle === "function" ? api.isIdle() : true
            if (isIdleAtDecision) {
              api.sendUserMessage(content)
            } else {
              api.sendUserMessage(content, { deliverAs: "followUp" })
            }
          } else {
            api.sendUserMessage(content, { deliverAs })
          }
        } catch (sendErr) {
          // Re-emit with enough context to diagnose the failure from logs alone:
          // frame type, the wire deliver_as value, and the isIdle state at the
          // moment the delivery decision was made.
          const sendMsg =
            sendErr instanceof Error ? sendErr.message : String(sendErr)
          const idleCtx =
            isIdleAtDecision === undefined
              ? "(not evaluated)"
              : String(isIdleAtDecision)
          emitError(
            "dispatch_error",
            `prompt: ${sendMsg} (deliver_as=${deliverAs}, isIdle=${idleCtx})`,
            "prompt",
          )
          break
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
 * - "interrupted"  — emit state_change:interrupted
 * - "finished"     — emit state_change:finished (protocol v2, issue #1434)
 * - "none"         — do not emit any state_change frame
 *
 * The sidecar applies a 2 s debounce after receiving state_change{finished}
 * before writing StateFinished and calling notifyCoordinator(), so a
 * follow-up turn_start can cancel the transition.
 */
export type TurnEndSignal = "interrupted" | "finished" | "none"

/**
 * Determines which state-change signal to emit at the end of a turn.
 *
 * Priority order:
 *  1. If stopReason is "aborted" → "interrupted" (always)
 *  2. If stopReason is "stop" AND !pendingReviewCall → "finished" (protocol
 *     v2: drop the isIdle gate; stopReason="stop" is a stronger signal than
 *     the streaming flag). hasPendingMessages is intentionally NOT gated here
 *     (#1472): steer messages queued by the extension during turn_start
 *     (git-push reminder, doom-loop, review-cycle) show up as pending at
 *     turn_end time but are consumed in the next inner-loop iteration; the
 *     sidecar's 2 s finished-debounce correctly cancels if turn_start arrives
 *     within the window, so this guard was over-broad and silently swallowed
 *     the post-resume finish notification.
 *  3. Otherwise → "none"
 *
 * hasPendingMessages is kept as a parameter so callers that already
 * compute it do not need to change (and tests can continue to pass the
 * value in). It is not used in the decision.
 *
 * This applies uniformly to all session types (worker, coordinator,
 * review agents). The sidecar's 2 s debounce handles the cancellation
 * window if a follow-up turn_start arrives.
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
  if (stopReason === "stop" && !pendingReviewCall) {
    return "finished"
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

// ---------------------------------------------------------------------------
// Iris daemon-mode integration (§3.3.2, §3.4, §3.5 of daemon-mode-design.md)
// ---------------------------------------------------------------------------
//
// The seven canonical pi built-in tool names. Any deviation from this set
// detected by the tool surface check (§3.5) aborts the session.
export const IRIS_CANONICAL_TOOLS = [
  "read",
  "bash",
  "edit",
  "write",
  "grep",
  "find",
  "ls",
] as const

// The iris extension allowlist (§3.5). Extension-registered tools from
// extensions NOT in this list abort the session before any LLM turn runs.
export const IRIS_EXTENSION_ALLOWLIST = ["prism", "atlassian", "anthropic-oauth"]

/**
 * registerIrisOverrides — called from session_start when IRIS_DAEMON_SOCK is
 * set. Performs three steps:
 *
 * 1. Derives override ToolDefinitions from pi.getAllTools() filtered to
 *    sourceInfo.source === "builtin" for the seven canonical tools.
 * 2. Calls pi.registerTool() for each override, swapping execute() with a
 *    function that sends tool_exec to the daemon over IRIS_DAEMON_SOCK and
 *    returns the tool_exec_result.
 * 3. Runs the §3.5 tool surface check: asserts canonical seven are now
 *    iris-owned (source !== "builtin"), no unknown builtins exist, no
 *    unauthorised extension tools exist. Fatal on any violation.
 */
async function registerIrisOverrides(
  pi: ExtensionAPI,
  sockPath: string,
): Promise<void> {
  // Step 1: capture the canonical built-in ToolDefinitions.
  const allToolsBefore = pi.getAllTools()
  const builtins = allToolsBefore.filter(
    (t) => t.sourceInfo.source === "builtin",
  )

  // For each canonical tool, register an override that forwards to the daemon.
  for (const toolInfo of builtins) {
    const name = toolInfo.name

    // Build the override ToolDefinition, copying all fields verbatim and
    // replacing only execute() (§3.3.2 of the design doc).
    const override = {
      // Verbatim copies from the built-in ToolInfo.
      name: toolInfo.name,
      label: (toolInfo as Record<string, unknown>).label as string ?? toolInfo.name,
      description: toolInfo.description ?? "",
      parameters: toolInfo.parameters,
      // Optional fields: copy if present on the original.
      ...(toolInfo as Record<string, unknown>).promptSnippet !== undefined
        ? { promptSnippet: (toolInfo as Record<string, unknown>).promptSnippet as string }
        : {},
      ...(toolInfo as Record<string, unknown>).promptGuidelines !== undefined
        ? { promptGuidelines: (toolInfo as Record<string, unknown>).promptGuidelines as string[] }
        : {},
      ...(toolInfo as Record<string, unknown>).renderShell !== undefined
        ? { renderShell: (toolInfo as Record<string, unknown>).renderShell as "default" | "self" }
        : {},
      ...(toolInfo as Record<string, unknown>).prepareArguments !== undefined
        ? { prepareArguments: (toolInfo as Record<string, unknown>).prepareArguments as (args: unknown) => unknown }
        : {},
      // Use parallel execution mode so the daemon can dispatch concurrent calls.
      executionMode: "parallel" as const,

      // Replaced execute(): forward to the iris daemon.
      execute: async (
        toolCallId: string,
        params: unknown,
        signal: AbortSignal | undefined,
        onUpdate: ((partial: unknown) => void) | undefined,
        _ctx: unknown,
      ): Promise<{ content: Array<{ type: string; text: string }>; details: unknown }> => {
        return irisExecute(sockPath, name, toolCallId, params, signal, onUpdate)
      },
    }

    // Register the override (replaces the builtin for this session).
    pi.registerTool(override as Parameters<ExtensionAPI["registerTool"]>[0])
  }

  // Step 3: tool surface check (§3.5). Run AFTER registerTool calls.
  runIrisSurfaceCheck(pi)
}

/**
 * runIrisSurfaceCheck — §3.5 surface assertion.
 *
 * Fatal conditions (any one throws):
 *  1. Unknown built-in: a tool with sourceInfo.source === "builtin" whose
 *     name is not in the canonical seven.
 *  2. Unauthorised extension tool: a tool with sourceInfo.source === "extension"
 *     whose sourceInfo.path resolves to an extension not on the allowlist.
 *  3. Failed override: a canonical tool still resolves to "builtin" (the
 *     registerTool call was silently ignored).
 */
export function runIrisSurfaceCheck(pi: ExtensionAPI): void {
  const tools = pi.getAllTools()
  const canonicalSet = new Set<string>(IRIS_CANONICAL_TOOLS)
  // Track canonical tools that are confirmed overridden (source !== "builtin").
  const overriddenCanonicals = new Set<string>()

  for (const t of tools) {
    const source = t.sourceInfo.source
    if (source === "builtin") {
      if (!canonicalSet.has(t.name)) {
        // Condition 1: unknown built-in — pi has added a tool iris has not reviewed.
        const msg =
          `[iris-extension] fatal: unknown built-in tool "${t.name}" ` +
          `(not in canonical seven). Update iris's tool allowlist or ` +
          `upgrade iris to support this new tool. ` +
          `Unset IRIS_DAEMON_SOCK to use vanilla pi while the issue is resolved.`
        console.error(msg)
        throw new Error(msg)
      } else {
        // Condition 3: canonical built-in that was NOT overridden — registerTool
        // was called but the tool still resolves to the original built-in.
        const msg =
          `[iris-extension] fatal: canonical built-in "${t.name}" was not ` +
          `overridden by iris (still resolves to "builtin"). This indicates ` +
          `an iris bug or a pi API change. ` +
          `Unset IRIS_DAEMON_SOCK to use vanilla pi while the issue is resolved.`
        console.error(msg)
        throw new Error(msg)
      }
    } else if (source === "extension") {
      // Condition 2: unauthorised extension tool.
      const extName = extractExtensionName(t.sourceInfo.path)
      if (!IRIS_EXTENSION_ALLOWLIST.includes(extName)) {
        const msg =
          `[iris-extension] fatal: tool "${t.name}" is registered by ` +
          `extension "${extName}" which is not on the iris allowlist ` +
          `(${IRIS_EXTENSION_ALLOWLIST.join(", ")}). ` +
          `Add the extension to the iris allowlist or remove it.`
        console.error(msg)
        throw new Error(msg)
      }
      // Track canonical tools that have been successfully overridden.
      if (canonicalSet.has(t.name)) {
        overriddenCanonicals.add(t.name)
      }
    }
  }

  // Condition 3 (absence variant): a canonical tool whose registerTool() call
  // was silently dropped entirely — not present in getAllTools() at all, or
  // present only as "builtin" (caught above). Any canonical name absent from
  // overriddenCanonicals after the loop means the iris shim is not in effect.
  for (const name of IRIS_CANONICAL_TOOLS) {
    if (!overriddenCanonicals.has(name)) {
      const msg =
        `[iris-extension] fatal: canonical built-in "${name}" was not ` +
        `overridden by iris (missing from overridden tool registry). This ` +
        `indicates an iris bug or a pi API change. ` +
        `Unset IRIS_DAEMON_SOCK to use vanilla pi while the issue is resolved.`
      console.error(msg)
      throw new Error(msg)
    }
  }
}

/**
 * extractExtensionName extracts the extension identifier from a sourceInfo
 * path. The path is typically the absolute path to the extension .ts/.js file.
 * We use the basename without extension as the name (e.g. "prism" from
 * "/etc/prism/pi-extensions/prism.ts").
 *
 * Exported for testing.
 */
export function extractExtensionName(path: string): string {
  // Normalise separators then split into components, filtering empty segments
  // (e.g. from leading slashes or double-slashes).
  const parts = path.replace(/\\/g, "/").split("/").filter((p) => p.length > 0)
  if (parts.length === 0) return path

  // Strip the file extension from the last component.
  const basename = (parts[parts.length - 1] ?? "").replace(/\.(ts|js|mjs|cjs)$/, "")

  // When the filename is a generic entry-point name ("index", "main", "mod"),
  // the meaningful extension identity is the parent directory name — e.g.
  // "anthropic-oauth/index.ts" → "anthropic-oauth".  Fall back to the
  // basename when there is no parent segment.
  const GENERIC_FILENAMES = new Set(["index", "main", "mod"])
  if (GENERIC_FILENAMES.has(basename) && parts.length >= 2) {
    return parts[parts.length - 2] ?? basename
  }

  return basename
}

/**
 * irisExecute — the replacement execute() for overridden built-in tools.
 *
 * Opens a fresh Unix socket connection to the iris daemon, sends a tool_exec
 * frame, streams tool_exec_update frames as onUpdate callbacks, awaits the
 * matching tool_exec_result, then closes the connection.
 *
 * On AbortSignal fire: sends tool_abort and closes the connection.
 * Returns a tool_exec_result with success=false, isError=true, output="aborted".
 */
async function irisExecute(
  sockPath: string,
  toolName: string,
  toolCallId: string,
  params: unknown,
  signal: AbortSignal | undefined,
  onUpdate: ((partial: unknown) => void) | undefined,
): Promise<{ content: Array<{ type: string; text: string }>; details: unknown }> {
  return new Promise((resolve, reject) => {
    const sock = require("node:net").createConnection(sockPath) as import("node:net").Socket
    let settled = false
    let buffer = ""

    const settle = (result: { content: Array<{ type: string; text: string }>; details: unknown }) => {
      if (settled) return
      settled = true
      sock.destroy()
      resolve(result)
    }
    const fail = (err: Error) => {
      if (settled) return
      settled = true
      sock.destroy()
      reject(err)
    }

    // Handle abort signal.
    const abortHandler = () => {
      if (settled) return
      try {
        const abortFrame = JSON.stringify({ type: "tool_abort", id: toolCallId }) + "\n"
        sock.write(abortFrame)
      } catch {}
      settle({
        content: [{ type: "text", text: "aborted" }],
        details: { isError: true, success: false },
      })
    }
    if (signal) {
      signal.addEventListener("abort", abortHandler, { once: true })
    }

    sock.on("error", (err: Error) => fail(err))

    sock.on("connect", () => {
      // Send tool_exec frame.
      const frame = JSON.stringify({
        type: "tool_exec",
        id: toolCallId,
        name: toolName,
        args: params,
      }) + "\n"
      sock.write(frame)
    })

    sock.on("data", (chunk: Buffer) => {
      buffer += chunk.toString()
      let nl: number
      while ((nl = buffer.indexOf("\n")) !== -1) {
        const line = buffer.slice(0, nl)
        buffer = buffer.slice(nl + 1)
        if (!line.trim()) continue
        let parsed: Record<string, unknown>
        try {
          parsed = JSON.parse(line) as Record<string, unknown>
        } catch {
          continue
        }
        if (parsed.id !== toolCallId) continue

        switch (parsed.type) {
          case "tool_exec_update": {
            const content = typeof parsed.content === "string" ? parsed.content : ""
            if (onUpdate && content) {
              try {
                onUpdate({
                  content: [{ type: "text", text: content }],
                  details: {},
                })
              } catch {}
            }
            break
          }
          case "tool_exec_result": {
            const success = parsed.success === true
            const isError = parsed.is_error === true || !success
            const output = typeof parsed.output === "string" ? parsed.output : ""
            settle({
              content: [{ type: "text", text: output }],
              details: {
                success,
                isError,
                ...(parsed.details && typeof parsed.details === "object" ? parsed.details : {}),
              },
            })
            break
          }
        }
      }
    })

    sock.on("close", () => {
      if (!settled) {
        fail(new Error(`[iris-extension] connection closed before tool_exec_result for id=${toolCallId}`))
      }
    })
  })
}

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
  //   PRISM_GIT_PUSH_REMINDER_DISABLE=1 — disable git-push reminder
  //
  // Review-cycle escalation moved to the Go-side review monitor in #1512;
  // see internal/review/monitor.go: the LOOP-LIMIT footer is appended to the
  // review-complete prompt body, so no per-turn injection lives here anymore.
  //
  // Review agents have legitimate repeated tool patterns so all guards are
  // suppressed when session_role from hello_ack is a canonical review-agent name.
  const doomLoopEnabled = !process.env.PRISM_DOOM_LOOP_DISABLE
  const gitPushReminderEnabled = !process.env.PRISM_GIT_PUSH_REMINDER_DISABLE

  const doomLoopState = newDoomLoopState()

  // Set to true when hello_ack.session_role matches a canonical review-agent name
  // (review-goal, review-code, review-context, review-qa, review-security).
  // All behavioural guards are suppressed for review agents.
  let isReviewSession = false

  // Session identity captured from hello_ack. Used for status bar display.
  let sessionRole = ""
  let sessionBranch = extractBranch(process.env.PRISM_SESSION_NAME ?? "")
  let sessionIsolationMode = ""

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
  // Track the latest active turn's abort handle. ctx.abort() requires an
  // ExtensionContext, so we capture one whenever a turn-active hook fires.
  let lastCtx: ExtensionContext | null = null

  // ── First-connect retry state ─────────────────────────────────────────
  //
  // firstConnect starts true and is set to false only after a successful
  // hello_ack. While true, ECONNREFUSED (TCP) and ENOENT (Unix) errors on
  // the connect() path are retried with exponential backoff to close the
  // startup race between sidecar bind and extension dial (issue #1554).
  //
  // Retry schedule: 200ms, 400ms, 800ms, 1600ms, 3200ms (5 retries,
  // ≈6.2s total wall time, well within the 8s budget).
  // All other error codes are not retried — they fall through to today's
  // log-and-give-up path regardless of firstConnect.
  //
  // PRISM_FIRST_CONNECT_RETRY_DELAYS_MS overrides the schedule for testing
  // (comma-separated ms values, e.g. "1,2,4,8,16"). Not documented as a
  // user-facing feature; it exists solely to make the exhaustion test fast.
  const FIRST_CONNECT_RETRY_DELAYS_MS: readonly number[] = (() => {
    const override = process.env.PRISM_FIRST_CONNECT_RETRY_DELAYS_MS
    if (override) {
      const parsed = override.split(",").map(Number)
      if (parsed.length > 0 && parsed.every((n) => Number.isFinite(n) && n >= 0)) {
        return parsed
      }
    }
    return [200, 400, 800, 1600, 3200]
  })()
  let firstConnect = true
  let pendingRetryTimer: ReturnType<typeof setTimeout> | null = null

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
  //
  // retryAttempt tracks which retry we are on (0 = initial attempt, 1..5 =
  // the bounded retry attempts). It is passed through the retry chain so the
  // error handler can schedule the next attempt or give up.
  const connect = (retryAttempt: number = 0): void => {
    // Cancel any pending retry timer — no two connect() calls in flight.
    if (pendingRetryTimer !== null) {
      clearTimeout(pendingRetryTimer)
      pendingRetryTimer = null
    }

    // Create the socket object first, attach handlers, then call connect()
    // so the error handler is registered before any synchronous error event
    // can fire (relevant for ENOENT on Unix sockets in some runtimes).
    try {
      socket = new net.Socket()
    } catch (err) {
      console.error("[prism-extension] Socket creation failed:", err)
      return
    }

    socket.on("error", (err) => {
      const code = (err as NodeJS.ErrnoException).code
      const isRetriable = code === "ECONNREFUSED" || code === "ENOENT"

      if (firstConnect && isRetriable && retryAttempt < FIRST_CONNECT_RETRY_DELAYS_MS.length) {
        // First-connect retry: sidecar may not have bound yet. Schedule the
        // next attempt with exponential backoff.
        const delayMs = FIRST_CONNECT_RETRY_DELAYS_MS[retryAttempt]
        const endpointStr =
          endpoint.kind === "unix"
            ? endpoint.path
            : `${endpoint.host}:${endpoint.port}`
        console.error(
          `[prism-extension] first-connect ${code} on ${endpointStr} — retry ${
            retryAttempt + 1
          }/${FIRST_CONNECT_RETRY_DELAYS_MS.length} in ${delayMs}ms`,
        )
        pendingRetryTimer = setTimeout(() => {
          pendingRetryTimer = null
          connect(retryAttempt + 1)
        }, delayMs)
        return
      }

      // Budget exhausted or non-retriable error — give up.
      if (firstConnect && isRetriable) {
        const endpointStr =
          endpoint.kind === "unix"
            ? endpoint.path
            : `${endpoint.host}:${endpoint.port}`
        console.error(
          `[prism-extension] giving up: sidecar not accepting on ${
            endpointStr
          } after ${retryAttempt} retries`,
        )
      } else {
        console.error("[prism-extension] socket error:", err)
      }
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
      // Cancel any pending retry timer now that we are connected.
      if (pendingRetryTimer !== null) {
        clearTimeout(pendingRetryTimer)
        pendingRetryTimer = null
      }
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
        //
        // Canonical review-agent names match internal/sidecar/host_api.go
        // (knownReviewAgentNames). The sidecar populates session_role from
        // Config.AgentRole, which is set to ag.Name (e.g. "review-goal") at
        // spawn time (internal/review/run.go). A prefix match covers all five
        // known names and future additions under the same naming scheme.
        const role = typeof f.session_role === "string" ? f.session_role : ""
        if (
          role === "review-goal" ||
          role === "review-code" ||
          role === "review-context" ||
          role === "review-qa" ||
          role === "review-security"
        ) {
          isReviewSession = true
        }
        sessionRole = role
        // Override branch from hello_ack session_name if provided.
        if (typeof f.session_name === "string" && f.session_name.length > 0) {
          sessionBranch = extractBranch(f.session_name)
        }
        // Capture isolation_mode from hello_ack. Absent field is treated as
        // empty string, which formatPrismStatus maps to the (host) suffix.
        sessionIsolationMode = typeof f.isolation_mode === "string" ? f.isolation_mode : ""
        handshakeComplete = true
        // Successful hello_ack: exit the first-connect retry window. Cancel
        // any lingering retry timer (defensive — should already be null).
        firstConnect = false
        if (pendingRetryTimer !== null) {
          clearTimeout(pendingRetryTimer)
          pendingRetryTimer = null
        }

        // Set the status bar immediately after handshake.
        // Review-cycle counting now lives in the Go-side monitor (#1512), so
        // the status bar's review_cycles field is fixed at 0 and pr_number is
        // empty — the worker reads cycle/PR context from the LOOP-LIMIT footer
        // on the review-complete prompt instead.
        const statusText = formatPrismStatus(sessionRole, sessionBranch, sessionIsolationMode, null, 0)
        lastCtx?.ui?.setStatus("prism", statusText)
        if (writer) {
          writer.write({
            type: "session_status",
            role: sessionRole,
            branch: sessionBranch,
            review_cycles: 0,
            pr_number: "",
            // session_id: the PI session ID used to locate the session
            // directory on disk for archiving (bug #1538 fix). Populated
            // from lastCtx when available; "" when no turn has started yet
            // (the turn_start session_status will carry the real ID).
            session_id: lastCtx?.sessionManager?.getSessionId() ?? "",
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
        isIdle: () => {
          // ctx.isIdle is available on PI runtimes that expose it (the same
          // guard used in the turn_end handler at ~line 1509). Fall back to
          // true (treat as idle) for older runtimes without the method, which
          // preserves the pre-streaming sendUserMessage behaviour.
          const ctx = lastCtx
          return typeof ctx?.isIdle === "function" ? ctx.isIdle() : true
        },
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

    // Initiate the connection now that all event handlers are registered.
    // Doing this after handler registration ensures the error listener is
    // in place before any synchronous error event fires (e.g. ENOENT on a
    // Unix socket path that does not exist yet in some runtimes).
    try {
      if (endpoint.kind === "unix") {
        socket.connect(endpoint.path)
      } else {
        socket.connect(endpoint.port, endpoint.host)
      }
    } catch (err) {
      console.error("[prism-extension] socket.connect failed:", err)
      socket = null
    }
  }

  // ── Outbound hooks ────────────────────────────────────────────────────

  pi.on("session_start", async (_event, ctx) => {
    lastCtx = ctx

    // ── Iris daemon-mode override registration (§3.4 escape hatch) ────────
    //
    // When IRIS_DAEMON_SOCK is set, the iris daemon has spawned this pi child.
    // Override the seven canonical built-in tools with iris dispatch shims.
    // When IRIS_DAEMON_SOCK is unset, skip entirely — vanilla pi behaviour.
    const irisSockPath = process.env.IRIS_DAEMON_SOCK
    if (irisSockPath) {
      try {
        await registerIrisOverrides(pi, irisSockPath)
      } catch (err) {
        // Fatal: abort before any LLM turn runs (§3.5 — no degraded mode).
        console.error("[iris-extension] fatal: override registration failed:", err)
        throw err
      }
    }
    // ── End iris override registration ────────────────────────────────────

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
          const restored = restoreGuardState(snapshot, doomLoopState)
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

    // Refresh status bar on each turn_start. Review-cycle counting is owned
    // by the Go-side monitor (#1512); the TS extension no longer reads or
    // tracks cycle counts here, so we always emit pr_number="" and
    // review_cycles=0. The LOOP-LIMIT footer on the review-complete prompt
    // is the canonical signal that the worker should escalate.
    if (handshakeComplete) {
      const statusText = formatPrismStatus(sessionRole, sessionBranch, sessionIsolationMode, null, 0)
      lastCtx?.ui?.setStatus("prism", statusText)
      if (writer) {
        writer.write({
          type: "session_status",
          role: sessionRole,
          branch: sessionBranch,
          review_cycles: 0,
          pr_number: "",
          // session_id: the PI session ID used by prism cleanup to locate
          // the session directory for archiving (bug #1538 fix). Sourced
          // from ctx.sessionManager.getSessionId() which is guaranteed
          // non-empty on turn_start.
          session_id: ctx.sessionManager.getSessionId(),
        })
      }
    }

    if (!isReviewSession) {
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
      //   "interrupted" → user abort; emit state_change{interrupted}
      //   "finished"    → clean stop (stopReason=stop, no pending, not reviewing);
      //                   emit state_change{finished} — sidecar applies 2 s debounce
      //   "none"        → do nothing (toolUse, error, length, pending msgs, reviewing)
      //
      // This path applies uniformly to all session types (workers, coordinators,
      // and review agents). The agent_end hook no longer emits state_change frames
      // (#1434 — unified turn_end path).
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
        if (signal === "interrupted") {
          writer.write({ type: "state_change", state: "interrupted" })
        } else if (signal === "finished") {
          writer.write({ type: "state_change", state: "finished" })
        }
      } catch (err) {
        console.error("[prism-extension] turn_end signal resolution failed:", err)
      }
    }
  })

  // Pre-tool-call bash deny list (#1528). Runs on the `tool_call` event
  // (fires before `tool_execution_start`) so the block is enforced *before*
  // pi hands the command to the bash tool. Returning `{ block: true,
  // reason }` causes pi to refuse execution and surface the reason string
  // to the agent via the tool-result channel.
  pi.on("tool_call", async (event) => {
    const toolName =
      typeof (event as { toolName?: unknown }).toolName === "string"
        ? (event as { toolName: string }).toolName
        : ""
    if (toolName !== "bash") return
    const input = (event as { input?: unknown }).input
    const command =
      typeof input === "object" && input !== null
        ? (input as Record<string, unknown>).command
        : undefined
    if (typeof command !== "string") return
    const hit = checkBlockedBash(command)
    if (hit !== null) {
      return { block: true, reason: hit.reason }
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
            tool: name,
            pattern: doomLoopState.currentKey ?? "",
            count: doomLoopState.consecutiveCount,
            timestampMs: Date.now(),
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
        // review-complete prompt arrives. NOTE: this is intentionally a
        // separate flag from cycle counting (the latter moved to the Go
        // monitor in #1512). The reviewing-state flag's substring-detection
        // class is tracked in #1519 and is out of scope here.
        if (/\bprism\s+review\b/.test(command)) {
          pendingReviewCall = true
        }
      }

      // Persist guard state after each tool call so session resume can
      // reconstruct the current doom-loop run and review-wait flag.
      // pi.appendEntry is lightweight (appends to the session JSON file);
      // we do it unconditionally — the reviewer can deduplicate via the
      // "latest entry wins" pattern in session_switch above.
      pi.appendEntry(GUARD_STATE_ENTRY_TYPE,
        snapshotGuardState(doomLoopState, pendingGitPushReminder, pendingReviewCall))
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
    // Do NOT send a session_shutdown wire frame here. The PI `session_shutdown`
    // hook fires on /new, /resume, /fork, and process exit — not only on
    // process exit. The wire frame (per §5.10) is process-exit only: the
    // sidecar treats it as terminal, breaks the reconnect loop, and removes
    // pipe.sock. Sending it here would cause the sidecar to delete pipe.sock
    // before PI fires `session_start` for the new/resumed session, resulting
    // in ECONNRESET when the extension re-dials (#1440 / regression of #1432).
    //
    // Only close the writer (issues a TCP FIN / half-close). The sidecar reads
    // EOF, leaves the listener open, and waits for a reconnect. Process-exit
    // cleanup of pipe.sock is handled by the sidecar's own Shutdown() path and
    // by the reconnect-timeout path.
    //
    // Cancel any pending first-connect retry so it does not race the
    // session_start-triggered connect() on the next session.
    if (pendingRetryTimer !== null) {
      clearTimeout(pendingRetryTimer)
      pendingRetryTimer = null
    }
    if (writer) {
      writer.close()
    }
    if (socket && !connected) {
      try {
        socket.destroy()
      } catch {}
    }
  })
}
