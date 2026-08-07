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
import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import type {
  ExtensionAPI,
  ExtensionContext,
  ProviderConfig,
} from "@earendil-works/pi-coding-agent"

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
 * Quote-aware tokeniser for a bash command string.
 *
 * Splits the input on whitespace, but treats matched single-quoted (`'...'`)
 * and double-quoted (`"..."`) regions as part of a single token. The quote
 * characters themselves are stripped from the resulting token. Adjacent
 * quoted and unquoted segments concatenate (e.g. `foo"bar baz"` → one token
 * `foobar baz`).
 *
 * This is intentionally simple — it does not handle backslash escapes,
 * `$'...'` ANSI-C quoting, or `$"..."` localised quoting. The goal is
 * normalisation for doom-loop similarity, not a full POSIX shell parser:
 * for that purpose, treating `bash -c "ls -la"` and `bash -c 'ls -la'` as
 * the same token sequence is the desired and sufficient outcome (#1683).
 *
 * Unmatched trailing quotes are tolerated: the rest of the string is
 * consumed as a single token with the opening quote stripped, which keeps
 * the function total (never throws) for arbitrary input.
 *
 * Exported for unit testing.
 */
export function tokenizeBashCommand(cmd: string): string[] {
  const tokens: string[] = []
  let current = ""
  let inToken = false
  let quote: '"' | "'" | null = null

  const flush = () => {
    if (inToken) {
      tokens.push(current)
      current = ""
      inToken = false
    }
  }

  for (let i = 0; i < cmd.length; i++) {
    const ch = cmd[i]
    if (quote !== null) {
      if (ch === quote) {
        // Closing quote — drop it, stay in the same token.
        quote = null
      } else {
        current += ch
      }
      continue
    }
    if (ch === '"' || ch === "'") {
      // Opening quote — start (or continue) a token, drop the quote char.
      quote = ch
      inToken = true
      continue
    }
    if (ch === " " || ch === "\t" || ch === "\n" || ch === "\r") {
      flush()
      continue
    }
    current += ch
    inToken = true
  }
  flush()
  return tokens
}

/**
 * Compute a normalised similarity key for a tool call.
 * Returns null for excluded tools (no detection).
 *
 * Ported from the prism-hooks.ts plugin — see that file for the full
 * commentary on similarity rules per tool.
 */
/**
 * Compute a short, stable, deterministic hex digest of a string.
 *
 * Not cryptographic — this exists only to keep similarity keys short while
 * still distinguishing distinct content. It never depends on JS object key
 * ordering because callers build the input string themselves from a fixed
 * field order (see the `edit`/`write` case in `similarityKey`) rather than
 * passing a JSON-serialised object through.
 *
 * This is the well-known cyrb53-family 64-bit string hash (two 32-bit
 * lanes combined), included inline rather than pulled in as a dependency
 * so the extension has no runtime deps beyond Node built-ins.
 *
 * Exported for unit testing.
 */
export function stableHash(input: string): string {
  let h1 = 0xdeadbeef ^ input.length
  let h2 = 0x41c6ce57 ^ input.length
  for (let i = 0; i < input.length; i++) {
    const ch = input.charCodeAt(i)
    h1 = Math.imul(h1 ^ ch, 2654435761)
    h2 = Math.imul(h2 ^ ch, 1597334677)
  }
  h1 =
    Math.imul(h1 ^ (h1 >>> 16), 2246822507) ^ Math.imul(h2 ^ (h2 >>> 13), 3266489909)
  h2 =
    Math.imul(h2 ^ (h2 >>> 16), 2246822507) ^ Math.imul(h1 ^ (h1 >>> 13), 3266489909)
  return (h1 >>> 0).toString(16).padStart(8, "0") + (h2 >>> 0).toString(16).padStart(8, "0")
}

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

      // Quote-aware tokenisation. Splits on whitespace outside matched single
      // or double quotes, then strips the surrounding quote characters from
      // each token. This normalises whitespace and quoting so that
      // `bash -c "ls -la"` and `bash -c 'ls -la'` produce the same token
      // sequence (`bash`, `-c`, `ls -la`) — see #1683.
      const meaningful = tokenizeBashCommand(stripped)

      let baseIdx = 0
      while (baseIdx < meaningful.length && meaningful[baseIdx].startsWith("-")) {
        baseIdx++
      }
      const base = meaningful[baseIdx] ?? ""

      if (EXCLUDED_BASH_BASES.has(base)) {
        return null
      }

      // Identity key is the full normalised argv (base + every following
      // token, preserving order). Including flags and trailing positionals
      // ensures different invocations of the same base command (e.g.
      // `prism checkin <s> --last 50` vs `--last 100`) produce different
      // keys — the previous "base + firstPositional" formula collapsed
      // legitimate diagnostic refinement into a doom-loop false positive
      // (#1683).
      const parts = meaningful.slice(baseIdx)
      return `bash:${parts.join(" ")}`.trimEnd()
    }

    case "edit":
    case "write": {
      const isObj = typeof args === "object" && args !== null
      const a: Record<string, unknown> = isObj ? (args as Record<string, unknown>) : {}
      const filePath: string =
        (a.filePath as string) ?? (a.path as string) ?? (isObj ? "" : String(args ?? ""))

      // Content is part of the identity key (#2599) — without it, five
      // *distinct* edits to one file collapse onto a single key and
      // false-positive the detector. Only truly identical calls (same
      // path AND same content) should accumulate toward a run.
      //
      // The `edit` tool's real schema is `{ path, edits: [{ oldText,
      // newText }] }` — hash the whole ordered set of pairs, not just the
      // first one, so changing any single pair in a multi-edit call
      // breaks the run (#2599 edge case). `write`'s real schema is
      // `{ path, content }`. A couple of legacy/alternate field-name
      // fallbacks are tolerated defensively.
      let contentKey: string
      if (Array.isArray(a.edits)) {
        contentKey = (a.edits as unknown[])
          .map((e) => {
            const pair = typeof e === "object" && e !== null ? (e as Record<string, unknown>) : {}
            const oldText = pair.oldText ?? pair.oldString ?? ""
            const newText = pair.newText ?? pair.newString ?? ""
            return `${String(oldText)}\u0000${String(newText)}`
          })
          .join("\u0001")
      } else if (typeof a.content === "string") {
        contentKey = a.content
      } else {
        const oldText = a.oldText ?? a.oldString ?? ""
        const newText = a.newText ?? a.newString ?? ""
        contentKey = `${String(oldText)}\u0000${String(newText)}`
      }

      return `${tool}:${filePath}:${stableHash(contentKey)}`
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
          `[PRISM DOOM-LOOP DETECTION] You've called \`${toolName}\` with the same target ` +
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
   * Whether the git-push reminder has already been delivered this session
   * (issue #2646). Once true, the reminder is never injected again,
   * regardless of further pushes. Must be persisted alongside
   * `pendingGitPushReminder` — an "already delivered" flag that is NOT
   * snapshotted resets on compaction/resume and the message returns
   * mid-session, which is the bug this field exists to prevent.
   */
  gitPushReminderDelivered: boolean
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
  gitPushReminderDelivered: boolean,
): GuardStateSnapshot {
  return {
    doomLoop: {
      currentKey: doomLoop.currentKey,
      consecutiveCount: doomLoop.consecutiveCount,
      fired: doomLoop.fired,
    },
    pendingGitPushReminder,
    gitPushReminderDelivered,
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
): { pendingGitPushReminder: boolean; pendingReviewCall: boolean; gitPushReminderDelivered: boolean } {
  doomLoop.currentKey = snapshot.doomLoop?.currentKey ?? null
  doomLoop.consecutiveCount = typeof snapshot.doomLoop?.consecutiveCount === "number"
    ? snapshot.doomLoop.consecutiveCount
    : 0
  doomLoop.fired = snapshot.doomLoop?.fired === true

  return {
    pendingGitPushReminder: snapshot.pendingGitPushReminder === true,
    gitPushReminderDelivered: snapshot.gitPushReminderDelivered === true,
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
 * Strip `$(...)` command-substitution and `` `...` `` backtick regions
 * from a (quote-stripped) command, replacing each region with a single
 * space so token boundaries are preserved but the substitution body is
 * gone. `$(...)` nesting is tracked with a depth counter; backticks do
 * not nest.
 *
 * Unlike `splitShellSegments` — which promotes substitution bodies to
 * their own segments so e.g. a nested `git push` is still considered a
 * real invocation — this helper is for patterns that need to match a
 * shape *spanning* a substitution. Example: `XDG_DATA_HOME=$(mktemp -d)
 * nix build` would otherwise be split into three disjoint segments
 * (`XDG_DATA_HOME=`, `mktemp -d`, `nix build .#prism`), none of which
 * matches the deny-list regex alone. After stripping, the outer command
 * collapses to a single segment `XDG_DATA_HOME=  nix build .#prism` that
 * a regex like `VAR=\S*\s+nix build` can detect.
 *
 * `checkBlockedBash` runs both the original-stripped and the
 * substitution-stripped command through `splitShellSegments` and dedupes
 * the results, so existing patterns that rely on the substitution body
 * being its own segment continue to fire.
 *
 * Exported for unit testing.
 */
export function stripCommandSubstitutions(command: string): string {
  let result = ""
  let i = 0
  while (i < command.length) {
    const c = command[i]
    const next = command[i + 1]
    if (c === "$" && next === "(") {
      // Skip past `$(` and the matching `)`, tracking nesting depth.
      i += 2
      let depth = 1
      while (i < command.length && depth > 0) {
        if (command[i] === "(") depth++
        else if (command[i] === ")") depth--
        if (depth > 0) i++
      }
      // Skip the closing `)` itself (if we found one).
      if (i < command.length) i++
      result += " "
      continue
    }
    if (c === "`") {
      // Backticks do not nest; consume to the next backtick.
      i++
      while (i < command.length && command[i] !== "`") i++
      // Skip the closing backtick (if we found one).
      if (i < command.length) i++
      result += " "
      continue
    }
    result += c
    i++
  }
  return result
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
//
// Scope note (#2588): no entry in this list matches a prism verb.
// `prism merge` and `prism investigate` are coordinator-only, and that is
// enforced at the host API by `requireCoordinator`
// (internal/sidecar/host_api.go), which answers a worker with HTTP 403. Do not
// audit a prism verb restriction from this file, and do not add a prism verb
// here expecting it to be the enforcement point.
//
// Two entries below DO guard the worker permission boundary: `gh-pr-merge`
// and `gh-pr-review-approve` (#2410) close the `gh` bypass of the same
// coordinator/worker separation the host-API gate enforces for `prism merge`.
// They are defence in depth on that boundary, alongside the host-API gate —
// not a substitute for it, and not the enforcement point for any prism verb.
//
// The other four entries guard a different class of hazard: damage whose
// blast radius reaches past the caller's own view. Sibling worktrees are not
// bind-mounted into the sandbox (`git-worktree-prune`, `git-worktree-remove`),
// the FD pool is host-wide (`nix-build-with-env-override`), and the stash
// stack lives in the shared bare repo, so it is repo-wide rather than
// per-worktree (`git-stash`, #2202). None of the four is about the role
// boundary.
//
// Role scoping is a separate axis from that split. THREE entries carry
// `appliesToRole: isWorkerClassRole` — `git-stash`, `gh-pr-merge`, and
// `gh-pr-review-approve`. `git-stash` is scoped because the coordinator is
// the deliberate exemption (it is then the sole prism writer to the shared
// stack); the two `gh` entries are scoped because the coordinator's manual
// merge and review paths must keep working. THREE more entries carry
// `appliesToRole: isReviewRole` — `git-checkout-review`, `git-apply-review`,
// and `git-merge-review` (#2648). That predicate is narrower still: only
// the five review roles are blocked, because ordinary workers use all three
// commands legitimately. The `git-worktree-*` and
// `nix-build-*` entries are unscoped: those hazards apply to coordinators too.

/**
 * One entry in the bash deny list. Each entry is matched against an
 * already-tokenised command segment (i.e. after `stripQuotedAndHeredocRegions`
 * + `splitShellSegments` have run on the raw command).
 *
 * The `match` predicate receives a single segment string and returns true
 * when the segment is a real invocation of the blocked command. The `reason`
 * surfaces verbatim to the agent via pi's tool-result channel and should
 * name the recommended alternative.
 *
 * `appliesToRole` optionally scopes a pattern to specific agent roles.
 * When present, the pattern fires only for sessions whose `--agent` role
 * satisfies the predicate; when absent, the pattern applies to every
 * session that loads the extension (the pre-existing convention — the
 * git-worktree-* and nix-build entries are unscoped because their hazards
 * apply to coordinators too).
 */
export interface BlockedBashPattern {
  id: string
  match: (segment: string) => boolean
  reason: string
  appliesToRole?: (agentRole: string) => boolean
}

/**
 * Worker-class role predicate for role-scoped deny-list entries (#2202).
 *
 * "Worker-class" means any prism-spawned agent role other than the
 * coordinator: worker, review-*, ac, investigate, retro, and any future
 * non-coordinator role. The coordinator is excluded because it is the
 * single session on the main worktree acting as the user's proxy — with
 * every worker-class session denied stash access, the coordinator is the
 * only prism writer to the shared stash stack, so the cross-session race
 * cannot occur from prism sessions.
 *
 * An empty role means pi was launched outside prism's spawn path (no
 * --agent flag) — e.g. a plain repo without the bare+worktree layout —
 * where the shared-stash hazard does not apply, so worker-scoped entries
 * do not fire. Prism always passes --agent for spawned sessions.
 *
 * Exported for unit testing.
 */
export function isWorkerClassRole(agentRole: string): boolean {
  return agentRole !== "" && agentRole !== "coordinator"
}

/**
 * Review-role predicate for the working-tree-mutation deny entries (#2648).
 *
 * NARROWER than `isWorkerClassRole`: it matches only the five canonical
 * review-agent roles, not every non-coordinator role. The distinction is
 * deliberate. `git checkout`, `git apply`, and `git merge` are legitimate,
 * everyday commands for an ordinary worker — a worker checks out a branch,
 * applies a WIP patch (the sanctioned `git stash` alternative in AGENTS.md
 * uses `git apply`), and can merge locally. Scoping these blocks to
 * `isWorkerClassRole` would break all of that. Review agents are the only
 * roles that must never touch the working tree: they inspect a PR from
 * `git show` / `git diff` and record a verdict; they share worktree state
 * with the worker under review, so a mutation there corrupts what the other
 * reviewers and the worker see.
 *
 * The five names are the canonical review-agent identities, matched exactly
 * against `internal/sidecar/host_api.go`'s review-agent set and the
 * guard-suppression list later in this file (search "review-goal"). A new
 * review role must be added here to inherit the block.
 *
 * Exported for unit testing.
 */
export function isReviewRole(agentRole: string): boolean {
  return (
    agentRole === "review-goal" ||
    agentRole === "review-code" ||
    agentRole === "review-context" ||
    agentRole === "review-qa" ||
    agentRole === "review-security"
  )
}

/**
 * Shared `reason` for the three review-scoped working-tree-mutation entries
 * (#2648). States the rule AND the alternative, per the deny-list
 * convention that a `reason` names the recommended path.
 */
const REVIEW_WORKING_TREE_REASON =
  "blocked by prism extension: review agents do not modify the working " +
  "tree or index. `git checkout`, `git apply`, and `git merge` all mutate " +
  "the worktree you share with the worker under review, which corrupts the " +
  "state the other review agents and the worker see. To read a file from " +
  "the PR branch use `git show origin/<branch>:<path>` (pipe it to a temp " +
  "file if you must run a command against it); to compare branches use " +
  "`git diff origin/main...origin/<branch>`. See issue #2648."

/**
 * Bash commands that the pi extension blocks before pi executes them.
 *
 * Add to this list when a specific incident motivates a new entry, OR when
 * moving an existing always-on prompt prohibition onto a cheaper, stronger
 * surface (#2648). The second class needs no incident: a prohibition the
 * agent never contemplates still costs prompt tokens on every spawn, and a
 * mechanical block is both zero standing cost and a stronger guarantee than
 * prose (prose can be skimmed). An entry of this class MUST be paired with
 * deletion of the prose it replaces, and scoped exactly as tightly as that
 * prose was — the `git-*-review` entries are the worked example: they
 * retired the working-tree-safety prose from the five review role files.
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
      "their worktrees are not bind-mounted into your view. A `prunable` " +
      "result here means the worktree is not mounted in this sandbox, not " +
      "that it is missing or damaged \u2014 check `prism sessions list` before " +
      "treating it as data loss. Use `prism cleanup --yes --session <name>` " +
      "instead, or escalate to the user if the residual state is from a " +
      "partial spawn.",
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
      "(they are not bind-mounted into your view). A `prunable` result " +
      "here means the worktree is not mounted in this sandbox, not that " +
      "it is missing or damaged \u2014 check `prism sessions list` before " +
      "treating it as data loss. Use `prism cleanup --yes --session <name>` " +
      "instead.",
  },
  {
    // #2180 — `nix build` with an inline override of any of XDG_DATA_HOME,
    // NIX_STORE_DIR, NIX_DATA_DIR, or HOME repoints nix's local profile /
    // trust DB / daemon-socket linkage at an empty tempdir, forcing it to
    // bootstrap a fresh single-user store. Inside sandbox-exec the
    // parallel evaluation leaks FDs that the next retry inherits; on
    // Darwin this exhausts `kern.maxfiles` and renders every process on
    // the host unable to call open() until a reboot. See AGENTS.md
    // § "When `nix build` fails inside a sandbox".
    //
    // The `\S*` (zero-or-more) in the value group is intentional — after
    // checkBlockedBash's substitution-stripping pass, the command
    // `XDG_DATA_HOME=$(mktemp -d) nix build` collapses to
    // `XDG_DATA_HOME=  nix build` (empty value) and must still match.
    id: "nix-build-with-env-override",
    match: (segment: string) =>
      /^\s*(?:env\s+)?(?:(?:XDG_DATA_HOME|NIX_STORE_DIR|NIX_DATA_DIR|HOME)=\S*\s+)+(?:env\s+)?nix\s+build\b/.test(
        segment,
      ),
    reason:
      "blocked by prism extension: `nix build` with an inline override " +
      "of XDG_DATA_HOME, NIX_STORE_DIR, NIX_DATA_DIR, or HOME has caused " +
      "system-wide FD exhaustion on Darwin (kern.maxfiles), requiring a " +
      "reboot to recover \u2014 see AGENTS.md § 'When `nix build` fails " +
      "inside a sandbox' for the incident writeup. If `nix build .#prism` " +
      "fails inside your sandbox, escalate via `prism escalate` rather " +
      "than attempting environment workarounds; CI runs the authoritative " +
      "build so a local failure is not a merge blocker.",
  },
  {
    // #2202 — `git stash` is not worktree-scoped. In the bare+worktree
    // layout the stash stack (refs/stash + its reflog) lives in the shared
    // bare repo, so every prism session in the repo races on a single LIFO
    // stack. On 2026-06-11 two concurrent workers ran `git stash -u` +
    // `git stash pop` within the same minute and the pops crossed,
    // silently swapping their WIP. All stash subcommands are blocked —
    // including read-only ones like `stash list` — because any stash use
    // normalises the stack as a workspace tool. Scoped to worker-class
    // roles: the coordinator (single session on the main worktree) is
    // exempt, consistent with the AC for #2202.
    id: "git-stash",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+stash\b/.test(
        segment,
      ),
    appliesToRole: isWorkerClassRole,
    reason:
      "blocked by prism extension: `git stash` is not worktree-scoped \u2014 " +
      "the stash stack (refs/stash) lives in the shared bare repo and is " +
      "shared by every prism session in this repo. Concurrent stash/pop " +
      "across sessions race on a single LIFO stack and can silently swap " +
      "WIP between workers (issue #2202). Set WIP aside with a temp commit " +
      "(`git commit -am wip`, restore with `git reset --soft HEAD~1`) or a " +
      "patch file (`git diff > /tmp/wip.patch` then `git restore .`, " +
      "restore with `git apply /tmp/wip.patch`) instead \u2014 see AGENTS.md " +
      "\u00a7 'Setting WIP aside \u2014 do not use git stash'.",
  },
  {
    // #2410 — `gh pr merge` from a worker-class agent bypasses the
    // coordinator/worker separation. Only the coordinator (via the merge
    // queue or the manual `gh pr merge --squash` fallback) is supposed to
    // land PRs. `prism merge` is already denied to workers by the host-API
    // role gate (`requireCoordinator` on /merge returns HTTP 403 — NOT this
    // deny list; see #2588), but `gh pr merge` had no equivalent block — the
    // guard was bypassable.
    // During the A/B calibration for #2406, the light-tier worker on
    // branch `align-workflow-matrix-keys-light` finished, ran
    // `prism review` (all 5 PASS), then ran `gh pr merge 2408 --squash`
    // and merged its own PR bypassing the coordinator hand-off. Scoped to
    // worker-class roles so the coordinator's manual-fallback merge path
    // is preserved. The regex tolerates the same normalisation shapes as
    // the pre-existing entries: leading env-var assignments (single or
    // multiple, with or without `env`), `gh`-level flags before `pr`,
    // and `pr`-level flags before `merge`, plus arbitrary whitespace
    // between tokens.
    id: "gh-pr-merge",
    match: (segment: string) =>
      /^\s*(?:env\s+)?(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*gh(?:\s+-\S+(?:\s+\S+)?)*\s+pr(?:\s+-\S+(?:\s+\S+)?)*\s+merge\b/.test(
        segment,
      ),
    appliesToRole: isWorkerClassRole,
    reason:
      "blocked by prism extension: `gh pr merge` from a worker-class agent " +
      "bypasses the coordinator/worker separation \u2014 only the coordinator " +
      "lands PRs (via the merge queue or its manual `gh pr merge --squash` " +
      "fallback). Hand off to the coordinator when your PR is open and " +
      "`prism review` passes; the coordinator enqueues via `prism merge` or " +
      "merges manually. See issue #2410.",
  },
  {
    // #2410 — same hardening class as `gh-pr-merge`. A worker
    // self-approving via `gh pr review --approve` games the required-review
    // gate the same way a self-merge games the merge gate. Review agents
    // use the `prism review` mechanism (which delivers verdicts through
    // prism, not through the GitHub review API), so blocking `--approve`
    // and `--request-changes` for worker-class roles breaks no legitimate
    // flow. Plain `gh pr review` (no verdict flag) and `--comment` remain
    // allowed — they map to informational paths, not to gating the merge.
    //
    // Both long-form (`--approve` / `--request-changes`) and short-form
    // (`-a` / `-r`) flags match — `gh pr review --help` lists them as
    // functionally identical aliases, so blocking one without the other
    // would leave a trivial bypass of the same class this deny closes.
    // The trailing `\b` prevents matching e.g. `-abc` or `--approver`;
    // the leading `\s` prevents matching mid-word (`--foo-a` etc.).
    id: "gh-pr-review-approve",
    match: (segment: string) =>
      /^\s*(?:env\s+)?(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*gh(?:\s+-\S+(?:\s+\S+)?)*\s+pr(?:\s+-\S+(?:\s+\S+)?)*\s+review\b[\s\S]*?\s(?:--approve|-a|--request-changes|-r)\b/.test(
        segment,
      ),
    appliesToRole: isWorkerClassRole,
    reason:
      "blocked by prism extension: `gh pr review --approve` / `-a` / " +
      "`--request-changes` / `-r` from a worker-class agent games the " +
      "required-review gate the same way `gh pr merge` games the merge " +
      "gate. Review agents use the `prism review` mechanism, not " +
      "`gh pr review`. Hand off to the coordinator instead. See issue #2410.",
  },
  {
    // #2648 — review agents must never mutate the working tree or index.
    // They inspect a PR from `git show` / `git diff` output and record a
    // verdict; they share worktree state with the worker under review, so
    // a `checkout` / `apply` / `merge` there corrupts what the other
    // reviewers and the worker see. This replaces the prose that used to
    // state the same prohibition in all five agents/review-*.md files
    // (the block is a stronger guarantee at zero standing prompt cost).
    //
    // Scoped to `isReviewRole`, NOT `isWorkerClassRole`: ordinary workers
    // use all three commands legitimately (checkout a branch, apply the
    // sanctioned WIP patch, merge locally), so a worker-class block would
    // break real flows. review-qa was checked as the most likely to need
    // a checkout for hands-on validation; its role file already directs it
    // to run against `git show` output piped to a temp file rather than
    // checking anything out, so no review role has a genuine need.
    //
    // The `(?!-)` after each subcommand keeps hyphenated cousins that do
    // NOT mutate the tree usable — notably `git merge-base` (used to find
    // a common ancestor for a diff) and `git checkout-index`.
    id: "git-checkout-review",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+checkout\b(?!-)/.test(
        segment,
      ),
    appliesToRole: isReviewRole,
    reason: REVIEW_WORKING_TREE_REASON,
  },
  {
    // #2648 — see git-checkout-review above.
    id: "git-apply-review",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+apply\b(?!-)/.test(
        segment,
      ),
    appliesToRole: isReviewRole,
    reason: REVIEW_WORKING_TREE_REASON,
  },
  {
    // #2648 — see git-checkout-review above. `(?!-)` preserves
    // `git merge-base`, which review agents legitimately use for diffs.
    id: "git-merge-review",
    match: (segment: string) =>
      /\bgit(\s+-C\s+\S+|\s+--git-dir\S*(\s+\S+)?)*\s+merge\b(?!-)/.test(
        segment,
      ),
    appliesToRole: isReviewRole,
    reason: REVIEW_WORKING_TREE_REASON,
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
 * `agentRole` is the session's `--agent` role (from pi.getFlag("agent")).
 * Patterns with an `appliesToRole` predicate are skipped when the role
 * does not satisfy it; unscoped patterns apply regardless of role. The
 * default of "" preserves the pre-#2202 behaviour for callers that do
 * not pass a role: unscoped patterns still fire, worker-scoped ones do not.
 *
 * Exported for unit testing.
 */
export function checkBlockedBash(
  command: string,
  agentRole: string = "",
): { id: string; reason: string } | null {
  const stripped = stripQuotedAndHeredocRegions(command)
  // Two passes:
  //   1. The plain quote-stripped command — preserves the existing
  //      behaviour where substitution bodies (e.g. `$(git worktree prune)`)
  //      become their own segments and can still trigger a block.
  //   2. The substitution-stripped command — collapses `$(...)` and
  //      backtick regions to a placeholder space so patterns that need to
  //      match a shape *spanning* a substitution (e.g. the
  //      `nix-build-with-env-override` entry on `XDG_DATA_HOME=$(mktemp -d)
  //      nix build`) fire on the resulting single segment.
  // The Set dedupes segments that survive unchanged in both passes so the
  // common case pays no extra cost.
  const segments = new Set<string>([
    ...splitShellSegments(stripped),
    ...splitShellSegments(stripCommandSubstitutions(stripped)),
  ])
  for (const segment of segments) {
    for (const pattern of BLOCKED_BASH_PATTERNS) {
      if (
        pattern.appliesToRole !== undefined &&
        !pattern.appliesToRole(agentRole)
      ) {
        continue
      }
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

// ---------------------------------------------------------------------------
// Mid-tool heartbeat (#1761)
// ---------------------------------------------------------------------------
//
// Long-running tool calls (e.g. `nix build .#prism`, `go test -count=20`) can
// exceed the sidecar's per-session inactivity watchdog window (#1728, default
// 15 min for review agents) when the call produces no intermediate frames on
// the wire. The sidecar interprets the silence as a stuck agent and force-
// transitions the session to `error`.
//
// Fix shape (a) from #1761: while a tool execution is in flight, emit a
// lightweight `tool_progress` frame on a fixed cadence. The sidecar's
// `touchActivity` runs on every inbound frame and resets the watchdog, so
// this drops in with no sidecar-side timer logic. The sidecar has an
// explicit case for the frame that resets activity *without* writing it to
// `agent_events`, so the heartbeat is invisible to downstream consumers
// (narrative, checkin, TUI) and never looks like a duplicate turn or tool
// call.
//
// Cadence: 30s by default, comfortably below the 15-minute watchdog window.
// The first heartbeat fires after one full cadence has elapsed — fast tools
// (completing in < TOOL_HEARTBEAT_INTERVAL_MS) never emit one.
//
// Genuine-stuck rescue path is preserved: if PI itself is hung (event loop
// blocked, process wedged), no heartbeats are emitted and the watchdog still
// fires within its configured window. The heartbeat only proves "PI's event
// loop is alive and a tool is in flight" — not "the tool is making progress".
// That weaker guarantee is sufficient: the existing watchdog target is
// session-level liveness, not per-tool progress.
//
// PRISM_TOOL_HEARTBEAT_INTERVAL_MS overrides the cadence for testing
// (small values make the timer test loop fast). Values <= 0 disable the
// heartbeat entirely.
export const TOOL_HEARTBEAT_INTERVAL_MS: number = (() => {
  const override = process.env.PRISM_TOOL_HEARTBEAT_INTERVAL_MS
  if (override !== undefined) {
    const parsed = Number(override)
    if (Number.isFinite(parsed)) {
      return parsed
    }
  }
  return 30_000
})()

/**
 * Start a periodic heartbeat for an in-flight tool call. Returns a
 * cancellation function the caller must invoke when the tool ends or the
 * connection drops.
 *
 * The heartbeat is a no-op (returns a noop cancel) when:
 *   - intervalMs <= 0 (disabled),
 *   - the writer is unavailable (handshake not complete).
 *
 * The first frame fires after `intervalMs` has elapsed — a tool that
 * completes before then emits nothing.
 *
 * Exported for unit testing.
 */
export function startToolHeartbeat(
  writer: FrameWriter | null,
  toolCallId: string,
  toolName: string,
  intervalMs: number = TOOL_HEARTBEAT_INTERVAL_MS,
  scheduler: {
    setInterval: (cb: () => void, ms: number) => unknown
    clearInterval: (handle: unknown) => void
  } = {
    setInterval: (cb, ms) => setInterval(cb, ms),
    clearInterval: (h) => clearInterval(h as ReturnType<typeof setInterval>),
  },
): () => void {
  if (intervalMs <= 0 || writer === null) {
    return () => {}
  }
  // Refresh the unref so the heartbeat interval doesn't pin the Node event
  // loop open (it's a best-effort liveness ping, not a load-bearing timer).
  const handle = scheduler.setInterval(() => {
    // The writer may have been closed between scheduling and firing. The
    // FrameWriter.write contract already silently drops post-close writes,
    // but check anyway to skip the JSON.stringify cost.
    writer.write({
      type: "tool_progress",
      id: toolCallId,
      name: toolName,
    })
  }, intervalMs)
  // Node's setInterval returns a Timeout with .unref(); duck-type to keep
  // this helper environment-agnostic (the unit-test scheduler returns a
  // plain object).
  const maybeUnref = (handle as { unref?: () => void }).unref
  if (typeof maybeUnref === "function") {
    maybeUnref.call(handle)
  }
  let cancelled = false
  return () => {
    if (cancelled) return
    cancelled = true
    scheduler.clearInterval(handle)
  }
}

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
 *   [coordinator] main · work · 5h 94% (1h56m) · 7d 42% (4d13h)
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
 * @param usageSegment  - pre-formatted account+usage segment (see formatUsageSegment), or
 *                        null when there is nothing to show (issue #2540). Appended verbatim
 *                        after the existing prefix/suffix so all pre-existing formatting is
 *                        unaffected when usageSegment is null.
 */
export function formatPrismStatus(
  role: string,
  branch: string,
  isolationMode: string,
  prNumber: string | null,
  cycles: number,
  usageSegment: string | null = null,
): string {
  const roleLabel = role.length > 0 ? role : "unknown"
  const prefix = `[${roleLabel}] ${branch}`
  // Append isolation mode suffix when host mode (or absent/unknown).
  // Sandboxed modes ("sandbox-exec", "bwrap") are the default — no suffix.
  const isHostMode = isolationMode !== "sandbox-exec" && isolationMode !== "bwrap"
  const isolationSuffix = isHostMode ? " (host)" : ""
  let base: string
  if (roleLabel === "review" && prNumber !== null) {
    const cycleLabel = cycles === 1 ? "1 cycle" : `${cycles} cycles`
    base = `${prefix}${isolationSuffix} · PR#${prNumber} · ${cycleLabel}`
  } else {
    base = `${prefix}${isolationSuffix}`
  }
  return usageSegment ? `${base} · ${usageSegment}` : base
}

// ---------------------------------------------------------------------------
// Usage status segment (issue #2540) — reads the passive-capture snapshot
// written by the sidecar (internal/usage/usage.go, issue #2538) and formats
// the "<account> · 5h <pct>% (<countdown>) · 7d <pct>% (<countdown>)" segment
// appended by formatPrismStatus above.
//
// This module is a READER of the snapshot format documented in issue #2537
// and internal/usage/usage.go — it must not redefine or write it.
// ---------------------------------------------------------------------------

/** One rate-limit window from the persisted snapshot. All fields optional —
 * an absent header is omitted from the JSON, never zero-filled (see
 * internal/usage/usage.go). */
export interface UsageWindow {
  status?: string
  utilization?: number
  reset?: number
  surpassed_threshold?: number
}

export interface UsageWindows {
  five_hour?: UsageWindow
  seven_day?: UsageWindow
}

/** The persisted per-account snapshot shape written by the sidecar
 * (internal/usage/usage.go Snapshot). `captured_at` and `account` are the
 * only fields not sourced from an optional header, so they are the minimum
 * bar for treating a parsed file as a valid snapshot. */
export interface UsageSnapshot {
  captured_at: string
  account: string
  unified_status?: string
  representative_claim?: string
  unified_reset?: number
  windows?: UsageWindows
  fallback?: { status?: string; percentage?: number }
  overage?: { status?: string; disabled_reason?: string }
}

/** A snapshot older than this is stale (issue #2537 "Agreed decisions"). The
 * countdown itself is unaffected by staleness — it is computed from the
 * absolute reset timestamp, which stays correct at any snapshot age. Only
 * the percentage is marked stale. */
export const USAGE_STALE_MS = 15 * 60 * 1000

// How often the status bar re-renders the usage segment on its own timer,
// independent of hello_ack/turn_start (issue #2540 AC: at least once every
// 60s). PRISM_USAGE_STATUS_REFRESH_INTERVAL_MS overrides the cadence for
// testing, mirroring TOOL_HEARTBEAT_INTERVAL_MS above.
export const USAGE_STATUS_REFRESH_INTERVAL_MS: number = (() => {
  const override = process.env.PRISM_USAGE_STATUS_REFRESH_INTERVAL_MS
  if (override !== undefined) {
    const parsed = Number(override)
    if (Number.isFinite(parsed) && parsed > 0) {
      return parsed
    }
  }
  return 30_000
})()

// ANSI SGR codes. sanitizeStatusText (footer.js:9-15) does not strip \x1b and
// truncateToWidth is ANSI-width-aware, so embedding raw escapes is the
// documented colour mechanism — there is no colour parameter on setStatus.
const ANSI_RESET = "\x1b[0m"
const ANSI_BOLD = "\x1b[1m"
const ANSI_DIM = "\x1b[2m"
const ANSI_GREEN = "\x1b[32m"
const ANSI_YELLOW = "\x1b[33m"
const ANSI_RED = "\x1b[31m"

/**
 * Resolve the absolute path of the active-account usage snapshot
 * (`~/.local/state/prism/usage/current.json`), honouring $XDG_STATE_HOME
 * first like internal/usage/usage.go's DefaultDir. Exported for unit
 * testing.
 */
export function usageSnapshotPath(): string {
  const stateHome = process.env.XDG_STATE_HOME
  const base =
    stateHome && stateHome.length > 0 ? stateHome : path.join(os.homedir(), ".local", "state")
  return path.join(base, "prism", "usage", "current.json")
}

/**
 * Read and parse the active-account usage snapshot. Returns null — never
 * throws — when the file does not exist, cannot be read, or does not parse
 * as a JSON object carrying at least `captured_at` and `account` strings.
 * One `readFileSync` call per invocation, no network access (issue #2540
 * performance AC).
 */
export function readUsageSnapshot(filePath: string = usageSnapshotPath()): UsageSnapshot | null {
  try {
    const raw = fs.readFileSync(filePath, "utf8")
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      return null
    }
    const obj = parsed as Record<string, unknown>
    if (typeof obj.captured_at !== "string" || typeof obj.account !== "string") {
      return null
    }
    return obj as unknown as UsageSnapshot
  } catch {
    return null
  }
}

/**
 * Format a remaining-time countdown from nowMs to resetEpochSeconds.
 *
 * At most two units, dropping zero-value LEADING units ("1h56m", never
 * "0d1h56m"). Under one minute remaining renders "<1m". A reset timestamp
 * at or before now renders "now". Exported for unit testing.
 */
export function formatCountdown(nowMs: number, resetEpochSeconds: number): string {
  const resetMs = resetEpochSeconds * 1000
  if (resetMs <= nowMs) {
    return "now"
  }
  const diffSec = Math.floor((resetMs - nowMs) / 1000)
  if (diffSec < 60) {
    return "<1m"
  }
  const days = Math.floor(diffSec / 86400)
  const hours = Math.floor((diffSec % 86400) / 3600)
  const minutes = Math.floor((diffSec % 3600) / 60)
  const units: Array<[number, string]> = [
    [days, "d"],
    [hours, "h"],
    [minutes, "m"],
  ]
  let start = 0
  while (start < units.length - 1 && units[start][0] === 0) {
    start++
  }
  return units
    .slice(start, start + 2)
    .map(([value, suffix]) => `${value}${suffix}`)
    .join("")
}

/**
 * Colour a percentage per the traffic-light thresholds: green below 60,
 * yellow 60-85 inclusive, red above 85. A stale percentage is additionally
 * dimmed (SGR 2) so staleness is visually distinguishable without changing
 * the width-affecting glyph set. Exported for unit testing.
 */
export function colorPercent(pct: number, stale: boolean): string {
  const color = pct > 85 ? ANSI_RED : pct >= 60 ? ANSI_YELLOW : ANSI_GREEN
  const prefix = stale ? `${ANSI_DIM}${color}` : color
  return `${prefix}${pct}%${ANSI_RESET}`
}

/**
 * Format one window's segment ("5h 94% (1h56m)"). Returns null when the
 * window is absent or missing the fields required to render (utilization,
 * reset) — this is how a snapshot missing one window renders only the
 * available window (issue #2540 edge-case AC).
 *
 * The governing window (named by `representative_claim`) is visually marked
 * with a bold label. The account name and countdown text are never
 * coloured — only the percentage carries colour/dim SGR codes.
 */
export function formatUsageWindow(
  labelText: string,
  window: UsageWindow | undefined,
  isGoverning: boolean,
  nowMs: number,
  ageStale: boolean,
): string | null {
  if (!window || typeof window.utilization !== "number" || typeof window.reset !== "number") {
    return null
  }
  const pct = Math.round(window.utilization * 100)
  const resetMs = window.reset * 1000
  const pastReset = resetMs <= nowMs
  const stale = ageStale || pastReset
  const countdown = formatCountdown(nowMs, window.reset)
  const label = isGoverning ? `${ANSI_BOLD}${labelText}${ANSI_RESET}` : labelText
  return `${label} ${colorPercent(pct, stale)} (${countdown})`
}

/**
 * Format the full "<account> · 5h ... · 7d ..." segment from a parsed
 * snapshot, or null when there is nothing renderable (no windows present).
 * Exported for unit testing.
 */
export function formatUsageSegment(snapshot: UsageSnapshot, nowMs: number): string | null {
  const capturedAtMs = Date.parse(snapshot.captured_at)
  const ageStale = !Number.isFinite(capturedAtMs) || nowMs - capturedAtMs > USAGE_STALE_MS
  const fiveHour = formatUsageWindow(
    "5h",
    snapshot.windows?.five_hour,
    snapshot.representative_claim === "five_hour",
    nowMs,
    ageStale,
  )
  const sevenDay = formatUsageWindow(
    "7d",
    snapshot.windows?.seven_day,
    snapshot.representative_claim === "seven_day",
    nowMs,
    ageStale,
  )
  const windowParts = [fiveHour, sevenDay].filter((p): p is string => p !== null)
  if (windowParts.length === 0) {
    return null
  }
  const account = snapshot.account.length > 0 ? snapshot.account : null
  return [account, ...windowParts].filter((p): p is string => p !== null).join(" · ")
}

/**
 * Read the active-account snapshot and format the usage segment for the
 * status bar in one call, or null when there is nothing to show (no
 * snapshot file, malformed snapshot, or no renderable windows). Reads at
 * most one file. Exported for unit testing.
 */
export function buildUsageStatusSegment(nowMs: number = Date.now()): string | null {
  const snapshot = readUsageSnapshot()
  if (!snapshot) {
    return null
  }
  return formatUsageSegment(snapshot, nowMs)
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

/**
 * Append a first-connect give-up diagnostic to the durable host-side
 * agent-run log (issue #2357).
 *
 * When the extension exhausts its first-connect retries the pane scrollback
 * is the only record of the failure — and scrollback dies with the pane. The
 * launcher (prism agent-run on bwrap/sandbox-exec, the host-mode pane env on
 * host) injects PRISM_AGENT_RUN_LOG with the host-side path of the
 * per-session agent-run log; this helper appends one timestamped line there
 * so the give-up survives for post-mortem debugging (`prism checkin`,
 * headless-session triage).
 *
 * Best-effort by design: when the env var is unset (PI invoked outside
 * prism, older launcher) or the write fails (missing grant, deleted dir),
 * the failure is logged to the pane and the extension continues — give-up
 * handling must never throw. Exported for unit testing.
 */
export function writeDurableGiveUpLog(endpointStr: string, retries: number): void {
  const logPath = process.env.PRISM_AGENT_RUN_LOG
  if (!logPath) return
  try {
    // Host mode has no agent-run process to pre-create the run dir; create
    // it on demand. In bwrap/sandbox-exec the dir already exists (agent-run
    // opened the log before exec'ing the sandbox) and this is a no-op.
    fs.mkdirSync(path.dirname(logPath), { recursive: true })
    fs.appendFileSync(
      logPath,
      `${new Date().toISOString()} [prism-extension] giving up: sidecar not accepting on ${endpointStr} after ${retries} retries — PI is running without a sidecar connection\n`,
    )
  } catch (err) {
    console.error("[prism-extension] failed to write give-up line to durable log:", err)
  }
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
  // Redact BEFORE the byte cut (issue #2589). Redacting only at the frame
  // writer would leave a secret that straddles the 8 KiB boundary split in
  // half, and the surviving half would be written to the database as a
  // partial credential that no literal match can find later.
  s = defaultSecretRedactor().redact(s)
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
// Assistant-message text extraction — issue #1764.
//
// pi 0.72.1's extension event model emits assistant content via two
// complementary paths:
//
//   1. `message_update` with `assistantMessageEvent.type === "text_delta"` —
//      fires once per streaming text chunk while the LLM response is in
//      flight. This is the existing path forwarded as one `msg_assistant`
//      frame per delta.
//
//   2. `message_end` with `message.role === "assistant"` — fires once at the
//      end of every assistant message and carries the *complete* content
//      array (`(TextContent | ThinkingContent | ToolCall)[]`). This is the
//      backstop for code paths where the streaming layer never produces
//      `text_delta` events: non-streaming providers, error paths that bypass
//      the delta loop, or future provider plugins.
//
// The helpers below let both handlers share the same content-walking logic
// and let the regression test in prism.test.ts pin pi 0.72.1's actual shape.
// ---------------------------------------------------------------------------

/**
 * Returns true when an extension `message_update` event's
 * `assistantMessageEvent` is a streaming text-delta carrying a non-empty
 * string delta. Used by the `message_update` handler in the extension and
 * by the per-message "have we seen a delta yet" tracker that suppresses
 * duplicate emission from `message_end`.
 *
 * The shape comes from pi 0.72.1's pi-ai package:
 * `node_modules/@earendil-works/pi-ai/dist/types.d.ts:185-225`.
 * The AssistantMessageEvent union includes:
 *   start, text_start, text_delta, text_end,
 *   thinking_start, thinking_delta, thinking_end,
 *   toolcall_start, toolcall_delta, toolcall_end,
 *   done, error
 * Of those, only `text_delta` carries assistant-visible text incrementally
 * (`thinking_delta` is internal reasoning, not user-visible response text).
 */
export function isAssistantTextDeltaEvent(
  ame: unknown,
): ame is { type: "text_delta"; delta: string } {
  if (ame === null || typeof ame !== "object") return false
  if ((ame as { type?: unknown }).type !== "text_delta") return false
  return typeof (ame as { delta?: unknown }).delta === "string"
}

/**
 * Extract every assistant-visible text block from a pi `AssistantMessage`'s
 * `content` array. Returns one string per `{type:"text", text}` block, in
 * order. Thinking blocks and toolCall blocks are skipped — thinking is
 * internal, toolCalls are surfaced via the separate `tool_call` wire frame.
 *
 * Empty strings are filtered out so the caller never emits an empty
 * `msg_assistant` frame. Returns `[]` for non-assistant messages, messages
 * with no text blocks, or malformed input.
 *
 * Source-of-truth for the shape:
 * `node_modules/@earendil-works/pi-ai/dist/types.d.ts:143` (`AssistantMessage`)
 * and the `TextContent`/`ThinkingContent`/`ToolCall` interfaces above it.
 */
export function extractAssistantText(message: unknown): string[] {
  if (message === null || typeof message !== "object") return []
  if ((message as { role?: unknown }).role !== "assistant") return []
  const content = (message as { content?: unknown }).content
  if (!Array.isArray(content)) return []
  const out: string[] = []
  for (const block of content) {
    if (
      block !== null &&
      typeof block === "object" &&
      (block as { type?: unknown }).type === "text" &&
      typeof (block as { text?: unknown }).text === "string"
    ) {
      const text = (block as { text: string }).text
      if (text.length > 0) out.push(text)
    }
  }
  return out
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
// Secret redaction — issue #2589.
//
// Why
// ---
// Every frame this extension writes to the socket is stored verbatim in
// prism.db. A command an agent runs can print a credential to stdout — `env`,
// `gh auth token`, `curl -v` with an Authorization header, a test that dumps
// an argv — and the value then lands in agent_events.payload and stays there
// until the prune job removes it. Nothing removed it before this block.
//
// Two layers
// ----------
//   1. VALUE matching (primary). This process holds the literal value of
//      every credential environment variable, so an exact match knows what a
//      regex could only guess. Near-zero false positives.
//   2. SHAPE matching (secondary). Well-known credential shapes, for a secret
//      that is not in this process's environment. Defence in depth; it never
//      replaces layer 1.
//
// Parity with Go
// --------------
// internal/payload/redact.go carries the same name registry, the same shape
// rules, the same minimum value length, and the same marker format, and runs
// as a second control at DB-write time. TestRedactorParityWithExtension in
// internal/payload keeps the two in step.
//
// Cost
// ----
// Two linear passes: one alternation of literal values, one alternation of
// shapes. Neither is quadratic in the size of a tool_result payload.
//
// SECURITY: nothing in this block logs, echoes, or returns a credential
// value. Every test value is synthetic.
// ---------------------------------------------------------------------------

/**
 * The shortest environment-variable value the value layer treats as a secret.
 *
 * The guard exists because an empty, whitespace-only, or one-character value
 * would otherwise match at nearly every position of ordinary output and shred
 * it. Every real credential is far longer: the shortest token prism forwards
 * is a GitHub PAT at 40 characters. A shorter value is left to the shape
 * layer.
 */
export const REDACTION_MIN_VALUE_LENGTH = 8

export const REDACTION_MARKER_PREFIX = "[redacted:"
export const REDACTION_MARKER_SUFFIX = "]"

/** The replacement text for a match attributed to `name`. */
export function redactionMarker(name: string): string {
  return REDACTION_MARKER_PREFIX + name + REDACTION_MARKER_SUFFIX
}

/**
 * Exact environment-variable names that carry a credential value.
 * Mirrors payload.CredentialEnvNames() in the Go tree.
 */
export const CREDENTIAL_ENV_NAMES: readonly string[] = [
  "ANTHROPIC_API_KEY",
  "ANTHROPIC_AUTH_TOKEN",
  "ATLASSIAN_API_TOKEN",
  "AWS_SECRET_ACCESS_KEY",
  "AWS_SESSION_TOKEN",
  "CACHIX_AUTH_TOKEN",
  "DEEPSEEK_API_KEY",
  "GEMINI_API_KEY",
  "GH_TOKEN",
  "GITHUB_TOKEN",
  "GITLAB_TOKEN",
  "GOOGLE_API_KEY",
  "GRAFANA_API_KEY",
  "NOTION_API_KEY",
  "NPM_TOKEN",
  "OPENAI_API_KEY",
  "OPENROUTER_API_KEY",
  "SLACK_TOKEN",
]

/**
 * Name prefixes that mark a whole family as credential-carrying.
 * Mirrors payload.CredentialEnvPrefixes().
 */
export const CREDENTIAL_ENV_PREFIXES: readonly string[] = [
  "PRISM_GITHUB_TOKEN_",
]

/**
 * Name-shape heuristic, so a credential nobody listed is still caught.
 * Every entry ends the name, so `GITHUB_TOKEN_PATH` and `SOPS_AGE_KEY_FILE`
 * do NOT match — they name a file, not a secret.
 * Mirrors payload.CredentialEnvNameSuffixes().
 */
export const CREDENTIAL_ENV_NAME_SUFFIXES: readonly string[] = [
  "_ACCESS_KEY",
  "_APIKEY",
  "_API_KEY",
  "_CREDENTIALS",
  "_PASSWD",
  "_PASSWORD",
  "_PRIVATE_KEY",
  "_SECRET",
  "_SECRET_KEY",
  "_TOKEN",
]

/**
 * Known credential shapes, in match order. Each is anchored on a distinctive
 * issuer prefix so ordinary output does not match. A shape with a generic
 * body (a bare base64 run, a JWT) is deliberately absent: the false-positive
 * rate would corrupt more output than the rule protects.
 *
 * The pattern strings are byte-identical to the Go rules in
 * internal/payload/redact.go so the parity test can compare them directly.
 *
 * `triggers` are literal substrings of which at least one MUST be present for
 * the pattern to have any chance of matching. They drive the prefilter, which
 * exists for cost, not correctness: running the combined regexp over a large
 * payload costs roughly ten times as much as the literal scans, and the
 * overwhelmingly common case is a payload with no credential shape at all.
 */
export const CREDENTIAL_SHAPES: ReadonlyArray<{
  readonly name: string
  readonly pattern: string
  readonly triggers: readonly string[]
}> = [
  {
    name: "private-key-block",
    pattern: String.raw`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
    triggers: ["-----BEGIN "],
  },
  {
    name: "github-fine-grained-pat",
    pattern: String.raw`github_pat_[A-Za-z0-9_]{40,255}`,
    triggers: ["github_pat_"],
  },
  {
    name: "github-token",
    pattern: String.raw`gh[pousr]_[A-Za-z0-9]{36,255}`,
    triggers: ["ghp_", "gho_", "ghu_", "ghs_", "ghr_"],
  },
  {
    name: "gitlab-pat",
    pattern: String.raw`glpat-[A-Za-z0-9_-]{20,255}`,
    triggers: ["glpat-"],
  },
  {
    name: "anthropic-api-key",
    pattern: String.raw`sk-ant-[A-Za-z0-9_-]{24,512}`,
    triggers: ["sk-ant-"],
  },
  {
    name: "openrouter-api-key",
    pattern: String.raw`sk-or-v1-[A-Za-z0-9]{32,512}`,
    triggers: ["sk-or-v1-"],
  },
  {
    name: "openai-api-key",
    pattern: String.raw`sk-proj-[A-Za-z0-9_-]{24,512}`,
    triggers: ["sk-proj-"],
  },
  {
    name: "slack-token",
    pattern: String.raw`xox[abprs]-[A-Za-z0-9-]{12,255}`,
    triggers: ["xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-"],
  },
  {
    name: "aws-access-key-id",
    pattern: String.raw`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`,
    triggers: ["AKIA", "ASIA"],
  },
  {
    name: "google-api-key",
    pattern: String.raw`\bAIza[0-9A-Za-z_-]{35}\b`,
    triggers: ["AIza"],
  },
  {
    name: "atlassian-api-token",
    pattern: String.raw`ATATT3[A-Za-z0-9_=.-]{50,512}`,
    triggers: ["ATATT3"],
  },
]

/** Reports whether an environment-variable name is expected to hold a secret. */
export function isCredentialEnvName(name: string): boolean {
  if (!name) return false
  if (CREDENTIAL_ENV_NAMES.includes(name)) return true
  for (const p of CREDENTIAL_ENV_PREFIXES) {
    if (name.startsWith(p) && name.length > p.length) return true
  }
  for (const s of CREDENTIAL_ENV_NAME_SUFFIXES) {
    if (name.endsWith(s) && name.length > s.length) return true
  }
  return false
}

/** Escape a literal so it can be embedded in a RegExp source. */
function escapeRegExpLiteral(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
}

/**
 * Reports whether a value is safe to feed to the value layer.
 *
 * An unexpanded `$(cat …)` value is a propagation bug, not a secret
 * (issue #2348); redacting it would hide the bug.
 */
function usableSecretValue(v: string): boolean {
  if (v.length < REDACTION_MIN_VALUE_LENGTH) return false
  if (v.trim().length < REDACTION_MIN_VALUE_LENGTH) return false
  if (v.includes("$(")) return false
  return true
}

const SHAPE_ATTRIBUTION: ReadonlyArray<{ name: string; anchored: RegExp }> =
  CREDENTIAL_SHAPES.map((s) => ({
    name: s.name,
    anchored: new RegExp("^(?:" + s.pattern + ")$"),
  }))

const COMBINED_SHAPE_SOURCE = CREDENTIAL_SHAPES.map(
  (s) => "(?:" + s.pattern + ")",
).join("|")

/** Flattened trigger set for the shape prefilter. */
const SHAPE_TRIGGERS: readonly string[] = CREDENTIAL_SHAPES.flatMap(
  (s) => s.triggers as string[],
)

/**
 * Reports whether any shape could possibly match `s`.
 *
 * Every trigger is a NECESSARY substring of its pattern, so a false negative
 * is impossible. The Go side pins that property with a fuzz target
 * (FuzzRedactShapePrefilter).
 */
export function shapeTriggerPresent(s: string): boolean {
  for (const t of SHAPE_TRIGGERS) {
    if (s.includes(t)) return true
  }
  return false
}

/** Attribute a combined-regexp match back to the shape rule that produced it. */
function shapeNameFor(match: string): string {
  for (const s of SHAPE_ATTRIBUTION) {
    if (s.anchored.test(match)) return s.name
  }
  return "credential"
}

/**
 * Maximum object depth redactFrame walks. Beyond it the value is passed
 * through unchanged, which bounds the walk on a pathological or cyclic frame.
 */
const REDACT_MAX_DEPTH = 64

export interface SecretRedactor {
  /** Replace every credential value and known shape in `text`. */
  redact(text: string): string
  /**
   * Return a copy of `frame` with every string value and every key redacted.
   *
   * Redacting the object, NOT the serialised line, is what makes the control
   * correct, for two independent reasons:
   *
   *   1. A secret containing a quote or a backslash appears in escaped form
   *      once JSON.stringify has run, and a literal match would miss it.
   *   2. The private-key-block shape has a `[\s\S]*?` body. Run over a
   *      serialised line, one match can start in one field and end in a
   *      later one, consuming the JSON structure between them — which either
   *      emits invalid JSON or silently deletes whole fields. Redacting each
   *      scalar on its own makes that structurally impossible.
   */
  redactFrame(frame: Record<string, unknown>): Record<string, unknown>
  /** Distinct credential values the value layer covers. Never the values. */
  readonly valueCount: number
}

/**
 * Build a redactor from an environment map.
 *
 * Values are filtered before they reach the value layer: see
 * usableSecretValue. When two names carry the same value, the marker names
 * the lexicographically first of them, so output is deterministic.
 */
export function makeSecretRedactor(
  env: NodeJS.ProcessEnv = process.env,
): SecretRedactor {
  const markerByValue = new Map<string, string>()
  const seenValue = new Set<string>()

  for (const name of Object.keys(env).sort()) {
    if (!isCredentialEnvName(name)) continue
    const value = env[name]
    if (typeof value !== "string" || !usableSecretValue(value)) continue
    if (seenValue.has(value)) continue
    seenValue.add(value)
    markerByValue.set(value, redactionMarker(name))
  }

  // Longest first, so a secret that is a prefix of another secret never wins
  // the match. RegExp alternation is leftmost-first in JS.
  const literals = [...markerByValue.keys()].sort((a, b) =>
    a.length !== b.length ? b.length - a.length : a < b ? -1 : 1,
  )
  const valueRe =
    literals.length > 0
      ? new RegExp(literals.map(escapeRegExpLiteral).join("|"), "g")
      : undefined
  const shapeRe = new RegExp(COMBINED_SHAPE_SOURCE, "g")

  const redact = (text: string): string => {
    if (!text) return text
    let out = text
    if (valueRe) {
      valueRe.lastIndex = 0
      out = out.replace(valueRe, (m) => markerByValue.get(m) ?? m)
    }
    if (shapeTriggerPresent(out)) {
      shapeRe.lastIndex = 0
      out = out.replace(shapeRe, (m) => redactionMarker(shapeNameFor(m)))
    }
    return out
  }

  const walk = (v: unknown, depth: number): unknown => {
    if (typeof v === "string") return redact(v)
    if (depth >= REDACT_MAX_DEPTH) return v
    if (Array.isArray(v)) return v.map((x) => walk(x, depth + 1))
    if (v !== null && typeof v === "object") {
      const proto = Object.getPrototypeOf(v)
      if (proto !== Object.prototype && proto !== null) {
        // A Buffer, Date, or class instance with a toJSON. Normalise it
        // through JSON first, so the walk covers exactly the strings
        // JSON.stringify is about to emit, then walk the plain result.
        // This is what makes the walk TOTAL, and it is why the writer does
        // not need a second pass over the serialised line — a second pass
        // would be able to span a JSON delimiter, which this cannot.
        try {
          return walk(JSON.parse(JSON.stringify(v)) as unknown, depth + 1)
        } catch {
          // Not serialisable (a cycle, a bigint). JSON.stringify in the
          // writer will throw on it too, so the frame is dropped there
          // rather than written unredacted.
          return v
        }
      }
      const out: Record<string, unknown> = {}
      for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
        out[redact(k)] = walk(val, depth + 1)
      }
      return out
    }
    return v
  }

  return {
    redact,
    redactFrame(frame) {
      return walk(frame, 0) as Record<string, unknown>
    },
    valueCount: markerByValue.size,
  }
}

let cachedDefaultRedactor: SecretRedactor | undefined

/**
 * The process-wide redactor, built from process.env on first use. The
 * environment does not change under a running extension, so it is built once.
 */
export function defaultSecretRedactor(): SecretRedactor {
  if (!cachedDefaultRedactor) {
    cachedDefaultRedactor = makeSecretRedactor(process.env)
  }
  return cachedDefaultRedactor
}

/** Test-only: drop the cached default so the next call re-reads process.env. */
export function resetDefaultSecretRedactor(): void {
  cachedDefaultRedactor = undefined
}

// ---------------------------------------------------------------------------
// Frame writer — synchronous (per wire spec §7.5).
// ---------------------------------------------------------------------------

export interface FrameWriter {
  write(frame: Record<string, unknown>): void
  close(): void
}

/**
 * Build the outbound frame writer.
 *
 * Every frame is redacted before it reaches the socket (issue #2589), so a
 * credential never gets as far as the sidecar or the database. `redactor`
 * defaults to the process-wide redactor; tests pass their own, built from
 * synthetic values.
 *
 * Redaction is applied to the frame OBJECT only. There is deliberately no
 * second pass over the serialised line: the private-key-block shape can span
 * a JSON delimiter, so a line-level pass can emit invalid JSON or silently
 * delete fields. redactFrame's walk is total over everything JSON.stringify
 * emits — non-plain objects included — so a line-level pass would add no
 * coverage and only that hazard.
 */
export function makeFrameWriter(
  socket: net.Socket,
  redactor: SecretRedactor = defaultSecretRedactor(),
): FrameWriter {
  let closed = false
  return {
    write(frame) {
      if (closed) return
      let line: string
      try {
        line = JSON.stringify(redactor.redactFrame(frame)) + "\n"
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
        // Replay marker (issue #1685 AC #5/#7): when the sidecar buffers a
        // /prompt that arrived during a PI disconnect, the replayed frame
        // sets replay=true so we can log it as a resumed (not fresh)
        // delivery. The sidecar guarantees exactly-once for a given
        // delivery_id, so the body is still delivered to the agent
        // unconditionally; the flag is informational.
        const isReplay = frame.replay === true
        if (isReplay) {
          console.error(
            `[prism-extension] prompt replay (deliver_as=${deliverAs}): post-reconnect resume of a buffered escalation/follow-up`,
          )
        }
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
        const rawModelId = typeof frame.model === "string" ? frame.model : ""
        const thinking =
          typeof frame.thinking === "string" ? frame.thinking : "off"
        if (!provider || !rawModelId) {
          emitError(
            "malformed_frame",
            "set_model missing provider or model",
            type,
          )
          return
        }
        // Defence-in-depth normalisation (issue #2252). The wire contract is
        // a bare model ID, and the sidecar now normalises before sending,
        // but older sidecar builds may still send a provider-prefixed model
        // (e.g. "anthropic/claude-fable-5"). Strip at most ONE leading
        // "<provider>/" segment, and only when it equals the frame's
        // provider, so nested model IDs (openrouter-style) are preserved.
        const prefix = provider + "/"
        const modelId = rawModelId.startsWith(prefix)
          ? rawModelId.slice(prefix.length)
          : rawModelId
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

      case "reviewing_state": {
        // Handled directly in the attachJsonlReader callback above (the
        // closure that owns pendingReviewCall). The case exists here so
        // the forward-compat "unknown inbound frame type" warning at the
        // bottom of the switch does not fire and clutter logs every time
        // the sidecar pushes a reviewing-state transition (issue #2050).
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
 * Returns true when the extension should activate. prism's agent-pane
 * launcher sets PRISM_SESSION_NAME before exec'ing PI inside the sandbox;
 * when that variable is set the wire-protocol producer (state_change,
 * tool_call observations, session_status, etc.) runs.
 *
 * Exposed as a function (not a captured boolean) so tests can manipulate
 * process.env between calls.
 */
export function shouldActivate(env: NodeJS.ProcessEnv = process.env): boolean {
  if (typeof env.PRISM_SESSION_NAME === "string" && env.PRISM_SESSION_NAME.length > 0) {
    return true
  }
  return false
}

// ---------------------------------------------------------------------------
// Role system-prompt injection (issue #2032 / design #2031).
// ---------------------------------------------------------------------------
//
// The agent role prompt (coordinator / worker / review-*) is read at
// `before_agent_start` from ~/.config/prism/agents/<role>.md and injected into
// the system prompt. This is the runtime counterpart to the per-session
// APPEND_SYSTEM.md staging file (StagePIAgentConfigDir, pi_invocation.go).
//
// Replace-vs-append semantics (verified against pi's agent-session.js):
//   - Returning { systemPrompt } from before_agent_start REPLACES the whole
//     prompt wholesale (`this.agent.state.systemPrompt = result.systemPrompt`).
//   - The event carries the fully assembled default in `event.systemPrompt`
//     (which the runtime passes as `this._baseSystemPrompt`).
//   - To match today's APPEND semantics we therefore concatenate
//     `event.systemPrompt + "\n\n" + rolePrompt` so the default prompt is
//     preserved in addition to the role prompt (not clobbered).
//
// Per-turn re-fire:
//   - before_agent_start fires once PER TURN, not once per session. Because the
//     handler recomputes from the constant `event.systemPrompt` (always the
//     base) each turn, returning `base + "\n\n" + rolePrompt` is idempotent —
//     the role prompt never accumulates across turns. The file read is memoised
//     so disk is touched at most once per session regardless.
//
// Single source of truth (PR2 of #2031):
//   - The per-session APPEND_SYSTEM.md staging file has been REMOVED
//     (StagePIAgentConfigDir no longer writes it). The extension is now the
//     SOLE source of the role prompt, so injection is unconditional — there is
//     no PRISM_INJECT_ROLE_PROMPT gate any more. With APPEND_SYSTEM.md gone
//     there is nothing for `event.systemPrompt` to double up against, so the
//     double-injection risk the PR1 gate guarded against no longer exists.

/**
 * Whitelist-validates a role name. Roles flow from pi.getFlag("agent") which
 * is bound from the CLI's --agent flag, ultimately set by prism Go-side at
 * spawn time. The host side is trusted, but a stray path-traversal or null
 * byte in cfg.AgentRole would otherwise concatenate into the role file path
 * and read an attacker-chosen file. The whitelist matches the host-side
 * validator at cmd/spawn.go (prism agent names: ASCII letters, digits,
 * hyphens, underscores).
 *
 * Exported for unit testing only.
 */
export function isValidRoleName(role: string): boolean {
  return typeof role === "string" && role.length > 0 && /^[A-Za-z0-9_-]+$/.test(role)
}

/**
 * Resolves the absolute path to the role prompt markdown file for `role`,
 * mirroring the host-side prismAgentRolePath (cmd/spawn.go): respect
 * XDG_CONFIG_HOME, else fall back to $HOME/.config. Returns "" when role is
 * empty or contains characters outside the role-name whitelist (defence in
 * depth against path traversal via a malicious flag value).
 */
export function prismAgentRolePath(role: string, env: NodeJS.ProcessEnv = process.env): string {
  if (!isValidRoleName(role)) {
    return ""
  }
  let base = env.XDG_CONFIG_HOME
  if (typeof base !== "string" || base.length === 0) {
    const home = env.HOME && env.HOME.length > 0 ? env.HOME : os.homedir()
    base = path.join(home, ".config")
  }
  return path.join(base, "prism", "agents", role + ".md")
}

/**
 * Reads the role prompt markdown for `role`, returning its contents or
 * undefined when the role is empty, the path cannot be resolved, or the file
 * does not exist / cannot be read. Trailing whitespace-only contents (e.g. an
 * empty file) yield undefined so an empty file is treated as "no role prompt".
 *
 * Mirrors today's edge case where a missing systemPromptPath simply omits
 * APPEND_SYSTEM.md (graceful no-op, no error).
 */
export function readRolePrompt(role: string, env: NodeJS.ProcessEnv = process.env): string | undefined {
  const p = prismAgentRolePath(role, env)
  if (p === "") {
    return undefined
  }
  let content: string
  try {
    content = fs.readFileSync(p, "utf8")
  } catch {
    // Missing file or unreadable → graceful no-op (matches APPEND_SYSTEM.md).
    return undefined
  }
  if (content.trim().length === 0) {
    return undefined
  }
  return content
}

/**
 * Composes the system prompt for a turn: preserves pi's default prompt
 * (`baseSystemPrompt`) and appends the role prompt, matching APPEND_SYSTEM.md
 * semantics. Returns undefined (caller returns no override, keeping the base)
 * when there is no role prompt to inject. Trailing newline on the base is
 * normalised so the separator is exactly one blank line.
 */
export function composeRoleSystemPrompt(
  baseSystemPrompt: string,
  rolePrompt: string | undefined,
): string | undefined {
  if (typeof rolePrompt !== "string" || rolePrompt.trim().length === 0) {
    return undefined
  }
  const base = typeof baseSystemPrompt === "string" ? baseSystemPrompt : ""
  if (base.length === 0) {
    return rolePrompt
  }
  return base.replace(/\s+$/, "") + "\n\n" + rolePrompt
}

/** Mutable cache the before_agent_start handler threads through
 * resolveRolePromptForTurn so the role file is read at most once per session. */
export interface RolePromptCache {
  /** true once the role file has been resolved (read or no-op). */
  resolved: boolean
  /** the role file contents, or undefined (no file / empty / read error). */
  cached: string | undefined
}

/**
 * Pure decision for one before_agent_start turn. Encapsulates the latch logic
 * so it is unit-testable independently of the PI runtime:
 *
 *   - The role identity is sourced from `agentRole`, which the caller reads
 *     synchronously from `pi.getFlag("agent")`. pi binds extension flags at
 *     `applyExtensionFlagValues` time (agent-session-services.js), which runs
 *     during `resourceLoader.reload()` BEFORE any `before_agent_start` event
 *     fires. There is therefore no handshake race — the role is available on
 *     the very first turn.
 *
 *     This replaces the pre-#2064 design which sourced the role from the
 *     async hello_ack handshake. That source was load-bearing on the wrong
 *     ordering: hello_ack arrived AFTER the first turn's before_agent_start
 *     fired (the handshake races over the sidecar's Unix socket; the agent
 *     hook fires inline on the pi side), so on a fresh bwrap session the
 *     first turn's role prompt was always missed. See PR for the full
 *     diagnosis chain.
 *
 *   - When `agentRole` is empty (host-mode pi launches that did not pass
 *     --agent, or a session running outside prism), the handler returns
 *     undefined and pi keeps its base system prompt unchanged.
 *
 *   - The file read is memoised (cache.resolved) so disk is touched at most
 *     once per session.
 *
 *   - The result is recomposed from `baseSystemPrompt` every turn, so it is
 *     idempotent and never accumulates the role prompt across turns even
 *     though before_agent_start fires per-turn (pi 0.77).
 *
 * Mutates `cache` in place (sets resolved/cached on first valid-role call)
 * and returns the systemPrompt override to send, or undefined to keep the base.
 */
export function resolveRolePromptForTurn(
  opts: {
    agentRole: string
    baseSystemPrompt: string
  },
  cache: RolePromptCache,
  readRole: (role: string) => string | undefined = readRolePrompt,
): string | undefined {
  if (!isValidRoleName(opts.agentRole)) {
    return undefined
  }
  if (!cache.resolved) {
    cache.cached = readRole(opts.agentRole)
    cache.resolved = true
  }
  return composeRoleSystemPrompt(opts.baseSystemPrompt, cache.cached)
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

  // Endpoint derivation: PRISM_HARNESS_PIPE points at the sidecar's listener.
  const endpointEnv = process.env.PRISM_HARNESS_PIPE
  if (!endpointEnv) {
    console.error(
      "[prism-extension] PRISM_HARNESS_PIPE is not set — extension is a no-op",
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

  // Role system-prompt injection cache (issue #2032 / #2064). The role file
  // is read once per session (load-once), then recomposed against
  // event.systemPrompt on every before_agent_start. The role identity is
  // sourced synchronously from pi.getFlag("agent") in the handler, not from
  // the async hello_ack handshake (which was the #2064 regression source).
  const roleSystemPromptCache: RolePromptCache = { resolved: false, cached: undefined }

  // Register --agent as an extension-owned CLI flag. pi has no native
  // concept of agents (prism owns the agent system), so pi parses --agent
  // into its `unknownFlags` map; pi.registerFlag binds that entry into
  // `runtime.flagValues` during applyExtensionFlagValues, which runs after
  // all extensions load and BEFORE any hook fires. pi.getFlag("agent") is
  // therefore synchronous and available from the first before_agent_start.
  //
  // Canonical example: pi 0.77 examples/extensions/plan-mode/index.ts which
  // registers --plan in the same shape.
  pi.registerFlag("agent", {
    description:
      "Primary agent identity (worker, coordinator, review-*, etc.) — selects the role system prompt appended to pi's base prompt at before_agent_start.",
    type: "string",
  })
  let sessionBranch = extractBranch(process.env.PRISM_SESSION_NAME ?? "")
  let sessionIsolationMode = ""

  // Flag: git push was detected; cleared after the next turn_start so the
  // reminder fires exactly once.
  let pendingGitPushReminder = false

  // Flag: the git-push reminder has already been delivered this session
  // (issue #2646). Once set, further pushes do not re-arm the reminder.
  // Persisted via the guard-state snapshot so compaction/resume cannot
  // reset it and cause a redelivery mid-session.
  let gitPushReminderDelivered = false

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

  // Mid-tool heartbeat cancellation handles, keyed by toolCallId (#1761).
  // tool_execution_start populates the map; tool_execution_end clears the
  // matching entry. The map (not a single handle) defends against PI
  // executing tools concurrently — each gets its own ticker.
  const toolHeartbeats = new Map<string, () => void>()
  const cancelAllToolHeartbeats = (): void => {
    for (const cancel of toolHeartbeats.values()) {
      try {
        cancel()
      } catch (err) {
        console.error("[prism-extension] cancel tool heartbeat failed:", err)
      }
    }
    toolHeartbeats.clear()
  }

  // ── Usage status bar refresh (issue #2540) ──────────────────────────
  //
  // Recomputes the status line — role/branch/isolation prefix plus the
  // account+usage segment — and pushes it via ctx.ui.setStatus. Shared by the
  // hello_ack handler, turn_start, and the timer below so all three stay in
  // sync. A no-op before the handshake completes, matching the existing
  // turn_start gate.
  const refreshStatusBar = (): void => {
    if (!handshakeComplete) return
    const usageSegment = buildUsageStatusSegment()
    const statusText = formatPrismStatus(
      sessionRole,
      sessionBranch,
      sessionIsolationMode,
      null,
      0,
      usageSegment,
    )
    lastCtx?.ui?.setStatus("prism", statusText)
  }

  // Refresh on a timer independent of hello_ack/turn_start (issue #2540 AC:
  // at least once every 60s) so a long-idle session's countdown and
  // percentage still advance/refresh between turns. unref'd — a
  // best-effort display refresh must never pin the event loop open.
  const usageStatusTimer = setInterval(() => {
    try {
      refreshStatusBar()
    } catch (err) {
      console.error("[prism-extension] usage status refresh failed:", err)
    }
  }, USAGE_STATUS_REFRESH_INTERVAL_MS)
  const maybeUnrefUsageTimer = (usageStatusTimer as { unref?: () => void }).unref
  if (typeof maybeUnrefUsageTimer === "function") {
    maybeUnrefUsageTimer.call(usageStatusTimer)
  }

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
        // Pane scrollback is ephemeral — also record the give-up in the
        // durable per-session agent-run log so a headless session can be
        // diagnosed after the fact (issue #2357).
        writeDurableGiveUpLog(endpointStr, retryAttempt)
      } else {
        console.error("[prism-extension] socket error:", err)
      }
    })
    socket.on("close", () => {
      connected = false
      socket = null
      writer = null
      handshakeComplete = false
      // Cancel any in-flight tool heartbeats so they don't keep ticking
      // against a torn-down writer (#1761). The writer's own post-close
      // guard would silently drop the writes, but cancelling here releases
      // the timer handles too.
      cancelAllToolHeartbeats()
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
        refreshStatusBar()
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
      // turns can emit state_change:idle normally. Belt-and-braces with the
      // reviewing_state frame handler below: either clearing path is
      // sufficient on its own; both are defence in depth.
      if (f.type === "prompt") {
        pendingReviewCall = false
      }

      // Authoritative reviewing-state signal from the sidecar (issue #2050).
      // The sidecar tracks reviewingInFlight against its session-ledger and
      // emits this frame on every transition plus immediately after
      // handshake. We mirror it directly into pendingReviewCall, which is
      // now driven solely from this signal — the previous bash-substring
      // set-trigger ("/\bprism\s+review\b/") was removed because it false-
      // matched on any bash command that incidentally contained the literal
      // "prism review" (e.g. a `gh pr comment` body, a grep, an echo of a
      // commit message). That re-latched the guard after the genuine
      // review-complete prompt had cleared it, and the worker's next
      // turn_end then suppressed state_change:finished forever — the exact
      // shape of the stuck-active incident in #2050.
      if (f.type === "reviewing_state") {
        pendingReviewCall = f.in_flight === true
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
          gitPushReminderDelivered = restored.gitPushReminderDelivered
          pendingReviewCall = restored.pendingReviewCall
        }
        break
      }
    }
  })

  pi.on("before_agent_start", async (event, ctx) => {
    lastCtx = ctx
    if (writer && handshakeComplete) {
      writer.write({ type: "state_change", state: "active" })
    }

    // Role system-prompt injection (issue #2032 / #2033 / #2064 fix).
    //
    // Source the role from pi.getFlag("agent") rather than the async
    // hello_ack-derived sessionRole. The flag is bound by pi BEFORE any
    // before_agent_start fires (see applyExtensionFlagValues in
    // pi's agent-session-services.js), so this is race-free — unlike the
    // pre-#2064 design which lost the first turn's role prompt because the
    // handshake socket-receive races behind the agent-start hook.
    //
    // getFlag returns `string | boolean | undefined`; we registered the flag
    // as type="string" so the runtime value is always a string when present.
    // Defensive narrowing keeps the handler safe if a future pi version
    // changes the contract or a missing-flag returns the registered default.
    const flagValue = pi.getFlag("agent")
    const agentRole = typeof flagValue === "string" ? flagValue : ""
    const composed = resolveRolePromptForTurn(
      {
        agentRole,
        baseSystemPrompt: event.systemPrompt,
      },
      roleSystemPromptCache,
    )
    if (composed !== undefined) {
      return { systemPrompt: composed }
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
      refreshStatusBar()
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
      // Delivered at most once per session (issue #2646): a repeat delivery
      // carries no new information and has been observed to prompt workers
      // into an unnecessary extra `prism review` round.
      if (gitPushReminderEnabled && pendingGitPushReminder && !gitPushReminderDelivered) {
        pendingGitPushReminder = false
        gitPushReminderDelivered = true
        pi.sendUserMessage(
          GIT_PUSH_REMINDER_MESSAGE,
          { deliverAs: "steer" },
        )
      } else if (gitPushReminderEnabled && pendingGitPushReminder) {
        pendingGitPushReminder = false
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
    // Role-scoped entries (#2202) need the session's agent role. Sourced
    // synchronously from pi.getFlag("agent") — bound before any hook fires
    // (see the before_agent_start handler) — so this is race-free even on
    // the first turn.
    const roleFlagValue = pi.getFlag("agent")
    const agentRole = typeof roleFlagValue === "string" ? roleFlagValue : ""
    const hit = checkBlockedBash(command, agentRole)
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
    // parentMessageId (#1787): the messageId of the in-flight assistant
    // message that issued this tool call. Tracked via the message_start
    // hook below — for the lifecycle pi guarantees, the assistant
    // message_start always fires before any tool_execution_start whose
    // tool was requested in that assistant turn, so this value is
    // populated for every tool call issued by a normal assistant turn.
    // An empty value (e.g. extension restart mid-turn before any
    // message_start was observed) is omitted so consumers that filter on
    // the field can detect orphans without confusing them with the
    // string "".
    if (currentAssistantMessageId !== "") {
      frame.parentMessageId = currentAssistantMessageId
    }
    if (truncated) frame.truncated = true
    writer.write(frame)

    // Arm the mid-tool heartbeat (#1761). The first frame fires only after
    // TOOL_HEARTBEAT_INTERVAL_MS has elapsed, so fast tools cost nothing.
    // Cancellation is keyed by tool-call id so concurrent tools don't
    // interfere with one another.
    if (id !== "" && !toolHeartbeats.has(id)) {
      toolHeartbeats.set(id, startToolHeartbeat(writer, id, name))
    }

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

        // Reviewing-state guard set-path REMOVED in #2050. The previous
        // implementation set pendingReviewCall = true whenever a bash
        // command matched /\bprism\s+review\b/, which false-positived on
        // any bash invocation that incidentally contained the literal
        // "prism review" (gh pr comment bodies, grep over docs, echo of
        // a commit message, etc.). A false-set after the genuine review-
        // complete prompt had cleared the flag re-latched the guard with
        // no release path, suppressing state_change:finished on the next
        // turn_end and stranding the session as `active` indefinitely.
        //
        // The guard is now driven authoritatively by the sidecar via the
        // `reviewing_state` inbound frame (see the attachJsonlReader
        // callback above). The sidecar emits one on every transition of
        // its ledger-backed `reviewingInFlight` flag plus once post-
        // handshake, so the extension's pendingReviewCall is a faithful
        // mirror of the sidecar's authoritative state.
      }

      // Persist guard state after each tool call so session resume can
      // reconstruct the current doom-loop run and review-wait flag.
      // pi.appendEntry is lightweight (appends to the session JSON file);
      // we do it unconditionally — the reviewer can deduplicate via the
      // "latest entry wins" pattern in session_switch above.
      pi.appendEntry(GUARD_STATE_ENTRY_TYPE,
        snapshotGuardState(doomLoopState, pendingGitPushReminder, pendingReviewCall, gitPushReminderDelivered))
    }
  })

  pi.on("tool_execution_end", async (event, ctx) => {
    lastCtx = ctx
    const id =
      typeof (event as { toolCallId?: unknown }).toolCallId === "string"
        ? (event as { toolCallId: string }).toolCallId
        : ""
    // Cancel the heartbeat for this tool call (#1761). Always run, even if
    // the writer was torn down in between — the map entry must not leak.
    if (id !== "") {
      const cancel = toolHeartbeats.get(id)
      if (cancel) {
        cancel()
        toolHeartbeats.delete(id)
      }
    }
    if (!writer || !handshakeComplete) return
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
    // parentMessageId (#1787): same value emitted on the matching
    // tool_call frame. Carried through to tool_result so the consumer's
    // secondary-query pushdown
    // (`db.QueryEventsByMessageIDs(..., ['tool_call','tool_result',...])`)
    // can locate both rows by the same assistant-turn id. The value is
    // taken from currentAssistantMessageId — pi's lifecycle guarantees
    // tool_execution_end fires inside the same assistant turn as the
    // matching tool_execution_start, so the id has not yet rotated.
    if (currentAssistantMessageId !== "") {
      frame.parentMessageId = currentAssistantMessageId
    }
    if (truncated) frame.truncated = true
    writer.write(frame)
  })

  // ── Assistant-message text forwarding (issue #1764) ──────────────────
  //
  // pi emits assistant text via two complementary code paths:
  //   (a) `message_update` with `assistantMessageEvent.type === "text_delta"`
  //       — streaming chunks while the LLM response is in flight;
  //   (b) `message_end` with `message.role === "assistant"` — fires once
  //       at the end of every assistant message with the complete content.
  //
  // Streaming providers (Anthropic, OpenAI, Google for normal responses)
  // exercise (a). Non-streaming code paths or providers that bypass the
  // delta loop only exercise (b). The extension forwards from whichever
  // path fires — and uses `currentAssistantSawDelta` to avoid emitting
  // the same text twice when both paths fire for the same message.
  //
  // The flag tracks the currently in-flight assistant message: set to
  // `false` on `message_start` for an assistant message, flipped to
  // `true` the first time a `text_delta` is forwarded, and consulted
  // (then cleared) by `message_end` to decide whether to emit a backstop.
  let currentAssistantSawDelta = false

  // The id of the most recently started assistant message. Captured on
  // message_start (role=assistant) and stamped onto every tool_call /
  // tool_result frame emitted during that assistant turn as
  // `parentMessageId` (#1787). This is what restores tool-call pairing
  // in `prism checkin --turns`: the consumer's
  // secondary query (`db.QueryEventsByMessageIDs`) joins child events
  // back to their assistant turn via this field. Empty string means
  // "no assistant message has started in this session yet" — tool
  // calls in that window are emitted without the field rather than with
  // an empty value, so consumers can distinguish "orphan" from "".
  let currentAssistantMessageId = ""

  pi.on("message_start", async (event, ctx) => {
    lastCtx = ctx
    const message = (event as { message?: unknown }).message
    if (
      message !== null &&
      typeof message === "object" &&
      (message as { role?: unknown }).role === "assistant"
    ) {
      currentAssistantSawDelta = false
      const mid = (message as { id?: unknown }).id
      currentAssistantMessageId = typeof mid === "string" ? mid : ""
    }
  })

  pi.on("message_update", async (event, ctx) => {
    lastCtx = ctx
    if (!writer || !handshakeComplete) return
    // Only forward streaming text deltas. Other update types (text_start,
    // text_end, thinking deltas, tool-call deltas) are noise on the wire —
    // the sidecar already gets the final tool call via tool_execution_*.
    const ame = (event as { assistantMessageEvent?: unknown })
      .assistantMessageEvent
    if (isAssistantTextDeltaEvent(ame)) {
      currentAssistantSawDelta = true
      const { text, truncated } = truncateString(ame.delta)
      const frame: Record<string, unknown> = {
        type: "msg_assistant",
        text,
      }
      if (truncated) frame.truncated = true
      writer.write(frame)
    }
  })

  pi.on("message_end", async (event, ctx) => {
    lastCtx = ctx
    // Backstop for the non-streaming path (issue #1764). If at least one
    // text_delta was forwarded for this assistant message, the deltas
    // already covered the content — emitting again would double-write.
    // If no delta was observed (non-streaming provider, error path, etc.)
    // walk the complete content array and emit each text block.
    const message = (event as { message?: unknown }).message
    const isAssistant =
      message !== null &&
      typeof message === "object" &&
      (message as { role?: unknown }).role === "assistant"
    const sawDelta = currentAssistantSawDelta
    // Reset the per-message flag regardless of role so the next assistant
    // message starts from a known state if message_start is missed.
    if (isAssistant) currentAssistantSawDelta = false
    if (!isAssistant) return
    if (sawDelta) return
    if (!writer || !handshakeComplete) return
    for (const text of extractAssistantText(message)) {
      const { text: out, truncated } = truncateString(text)
      const frame: Record<string, unknown> = {
        type: "msg_assistant",
        text: out,
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
    // Cancel any in-flight tool heartbeats on shutdown (#1761) so the
    // timers don't fire after the writer is closed.
    cancelAllToolHeartbeats()
    // Cancel the usage status refresh timer (issue #2540) so it does not
    // fire after the writer is closed.
    clearInterval(usageStatusTimer)
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
