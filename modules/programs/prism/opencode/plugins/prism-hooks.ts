import type { Plugin } from "@opencode-ai/plugin";

// ── Doom-loop detection ──────────────────────────────────────────────────────
//
// The detector tracks the last N=5 tool calls in hook-local memory per plugin
// instance (i.e. per opencode session — the plugin is re-initialised on each
// session start). It fires when all 5 are the same tool with "similar" arguments
// (see similarityKey below), emits a doom_loop_detected event to the prism DB
// via `prism event doom-loop-detected`, and injects a one-shot steering prompt
// into the next LLM turn via experimental.chat.messages.transform.
//
// Similarity rules (per tool):
//   bash      — for subcommand-driven CLIs (gh, git, kubectl, helm, docker,
//               podman), include the base command + subcommand + operand in the
//               key so that "gh issue view 1" and "gh issue view 2" are treated
//               as different calls. For all other commands, compare the base
//               command + first positional argument only (strips flags).
//               "git log -1" and "git log -3" match; "go test ./cmd/..." and
//               "go build ./..." do not.
//   edit/write — same file path (first argument), ignoring content.
//   webfetch  — same URL (first argument).
//   read/grep/glob — EXCLUDED. Legitimate exploration revisits the same paths
//               often; treating them as loops generates false positives.
//   default   — byte-exact full argument string.
//
// After the hook fires, further consecutive matching calls are suppressed (one
// nudge per loop). The suppression resets when the pattern breaks — a different
// tool or meaningfully different arguments.
//
// Detection state is per-session (per plugin instance). Two concurrent sessions
// each at 4 consecutive matching calls do NOT cross-contaminate.

const DOOM_LOOP_THRESHOLD = 5;

// excludedTools are never subject to doom-loop detection.
// read/grep/glob: legitimate exploration revisits the same paths often.
// todowrite: task-list management is a housekeeping pattern, not a loop.
const EXCLUDED_TOOLS = new Set(["read", "grep", "glob", "todowrite"]);

/**
 * Compute a normalised similarity key for a tool call.
 * Returns null for excluded tools (no detection).
 * Exported for unit testing.
 */
export function similarityKey(tool: string, args: any): string | null {
  if (EXCLUDED_TOOLS.has(tool)) {
    return null;
  }

  switch (tool) {
    case "bash": {
      // Extract the command string.
      const cmd: string =
        typeof args === "object" && args !== null
          ? (args as any)?.command ?? ""
          : String(args ?? "");
      // Split into tokens, strip leading flags/options (anything starting with
      // "-"), and keep the first two meaningful tokens (command + first positional).
      const tokens = cmd.trim().split(/\s+/);
      const meaningful = tokens.filter((t) => t.length > 0);
      // Find the base command (first non-flag token).
      let baseIdx = 0;
      while (baseIdx < meaningful.length && meaningful[baseIdx].startsWith("-")) {
        baseIdx++;
      }
      const base = meaningful[baseIdx] ?? "";

      // Subcommand-driven CLIs: include subcommand + operand in the key so
      // that calls with different operands are treated as distinct.
      // e.g. "gh issue view 1" → "bash:gh issue view 1"
      //      "git show abc:foo.go" → "bash:git show abc:foo.go"
      const SUBCOMMAND_CLIS = new Set([
        "gh",
        "git",
        "kubectl",
        "helm",
        "docker",
        "podman",
      ]);

      if (SUBCOMMAND_CLIS.has(base)) {
        // Collect all non-flag tokens after the base command.
        const positionals: string[] = [];
        for (let i = baseIdx + 1; i < meaningful.length; i++) {
          if (!meaningful[i].startsWith("-")) {
            positionals.push(meaningful[i]);
          }
        }
        // Include base + up to three positionals (subcommand + sub-subcommand + operand).
        // e.g. "gh issue view 1" → positionals=["issue","view","1"] → "bash:gh issue view 1"
        //      "git show abc:foo.go" → positionals=["show","abc:foo.go"] → "bash:git show abc:foo.go"
        // If fewer positionals exist, fall back gracefully.
        const parts = [base, ...positionals.slice(0, 3)];
        return `bash:${parts.join(" ")}`.trimEnd();
      }

      // Find the first positional argument after the base command (skip flags).
      let firstPos = "";
      for (let i = baseIdx + 1; i < meaningful.length; i++) {
        if (!meaningful[i].startsWith("-")) {
          firstPos = meaningful[i];
          break;
        }
      }
      return `bash:${base} ${firstPos}`.trimEnd();
    }

    case "edit":
    case "write": {
      // Same file path counts as similar, regardless of content.
      const filePath: string =
        typeof args === "object" && args !== null
          ? (args as any)?.filePath ?? (args as any)?.path ?? ""
          : String(args ?? "");
      return `${tool}:${filePath}`;
    }

    case "webfetch": {
      const url: string =
        typeof args === "object" && args !== null
          ? (args as any)?.url ?? ""
          : String(args ?? "");
      return `webfetch:${url}`;
    }

    default: {
      // Conservative default: byte-exact full args string.
      const raw =
        typeof args === "object" && args !== null
          ? JSON.stringify(args)
          : String(args ?? "");
      return `${tool}:${raw}`;
    }
  }
}

// DoomLoopState tracks consecutive matching calls for the current session.
interface DoomLoopState {
  // The similarity key of the current run.
  currentKey: string | null;
  // How many consecutive matching calls we have seen.
  consecutiveCount: number;
  // Whether we have already fired for this run (suppression flag).
  fired: boolean;
}

/**
 * Check whether a bash command string is a real `git push` invocation.
 *
 * The previous implementation matched the literal substring `git ... push`
 * anywhere in the raw command, which produced false positives for quoted
 * arguments, heredoc bodies, and grep / awk / sed patterns (issue #1519).
 *
 * Strategy:
 *   1. Strip heredoc bodies, single-quoted regions, and double-quoted
 *      regions from the command — these never contain a real invocation.
 *   2. Split what remains on shell separators (`;`, newline, `&&`, `||`,
 *      `|`, `&`, and command/process substitution boundaries) so each
 *      pipeline / sequence stage is examined independently.
 *   3. Tokenise each segment on whitespace and check whether the leading
 *      tokens form `git [-C <path>] [--git-dir=...|--git-dir <path>] push`.
 *
 * The implementation is intentionally duplicated in
 * `modules/programs/prism/pi/extensions/prism.ts` (`isGitPush`,
 * `stripQuotedAndHeredocRegions`, `splitShellSegments`) — the two files
 * live in different package trees and a shared-package refactor is out of
 * scope for #1519. Keep the two implementations in sync.
 *
 * Exported for unit testing.
 */
export function isGitPush(command: string): boolean {
  const stripped = stripQuotedAndHeredocRegions(command);
  for (const segment of splitShellSegments(stripped)) {
    if (segmentIsGitPush(segment)) return true;
  }
  return false;
}

/**
 * Remove heredoc bodies, single-quoted strings, and double-quoted strings
 * from a bash command. Heredoc start tokens (`<<EOF`, `<<-EOF`, `<<'EOF'`,
 * `<<"EOF"`) are also removed so they do not pollute the surrounding
 * segment, but the rest of the command line containing them is preserved.
 *
 * Exported for unit testing.
 */
export function stripQuotedAndHeredocRegions(command: string): string {
  // Pass 1: strip heredocs.
  const lines = command.split("\n");
  const out: string[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const heredocRe = /<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\1/;
    const m = line.match(heredocRe);
    if (m && m.index !== undefined) {
      const word = m[2];
      // Keep the line with the marker removed.
      out.push(line.slice(0, m.index) + line.slice(m.index + m[0].length));
      i++;
      while (i < lines.length && lines[i].trim() !== word) {
        i++;
      }
      if (i < lines.length) i++;
      continue;
    }
    out.push(line);
    i++;
  }

  // Pass 2: strip quoted regions.
  const text = out.join("\n");
  let result = "";
  let j = 0;
  while (j < text.length) {
    const ch = text[j];
    if (ch === "\\" && j + 1 < text.length) {
      result += " ";
      j += 2;
      continue;
    }
    if (ch === "'") {
      const end = text.indexOf("'", j + 1);
      if (end === -1) break;
      result += " ";
      j = end + 1;
      continue;
    }
    if (ch === '"') {
      let k = j + 1;
      while (k < text.length) {
        if (text[k] === "\\" && k + 1 < text.length) {
          k += 2;
          continue;
        }
        if (text[k] === '"') break;
        k++;
      }
      if (k >= text.length) break;
      result += " ";
      j = k + 1;
      continue;
    }
    result += ch;
    j++;
  }
  return result;
}

/**
 * Split a (quote-stripped) command string into individual command segments
 * along shell separators. Process substitutions `<(...)` and `>(...)`,
 * command substitutions `$(...)`, and backtick `` `...` `` regions are
 * promoted to their own segments.
 *
 * Exported for unit testing.
 */
export function splitShellSegments(command: string): string[] {
  const SEP = "\x00";
  let s = "";
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    const next = command[i + 1];
    if ((c === "&" && next === "&") || (c === "|" && next === "|")) {
      s += SEP;
      i++;
      continue;
    }
    if ((c === "<" || c === ">" || c === "$") && next === "(") {
      s += SEP;
      i++;
      continue;
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
      s += SEP;
      continue;
    }
    s += c;
  }
  return s
    .split(SEP)
    .map((seg) => seg.trim())
    .filter((seg) => seg.length > 0);
}

/**
 * Decide whether a single tokenised command segment is a `git push`
 * invocation. Accepts the leading-flag forms the prior regex accepted:
 *   git push ...
 *   git -C <path> push ...
 *   git --git-dir <path> push ...
 *   git --git-dir=<path> push ...
 */
function segmentIsGitPush(segment: string): boolean {
  const tokens = segment.split(/\s+/).filter((t) => t.length > 0);
  if (tokens.length < 2) return false;
  if (tokens[0] !== "git") return false;
  let i = 1;
  while (i < tokens.length) {
    const t = tokens[i];
    if (t === "-C" || t === "--git-dir") {
      if (i + 1 >= tokens.length) return false;
      i += 2;
      continue;
    }
    if (t.startsWith("--git-dir=")) {
      i += 1;
      continue;
    }
    break;
  }
  return tokens[i] === "push";
}

export const PrismHooks: Plugin = async (pluginInput) => {
  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  // Review-cycle escalation moved to the Go-side review monitor in #1512
  // (Shape B). The monitor appends a LOOP-LIMIT footer to the
  // review-complete prompt body when the parent's review-group history
  // shows >= 3 verdict-producing cycles without convergence — see
  // internal/review/monitor.go: buildLoopLimitFooter. The TS plugin no
  // longer counts cycles or injects per-turn warnings, which dissolves
  // both the per-turn spam and the bash-substring false-match defects at
  // the source. The duplicate implementation that previously lived here
  // is gone.

  // ── Doom-loop state (per session / per plugin instance) ──────────────────
  const doomLoop: DoomLoopState = {
    currentKey: null,
    consecutiveCount: 0,
    fired: false,
  };

  // Steering prompt queued for injection on the next LLM turn.
  // Contains the message text, or null when nothing is pending.
  let pendingSteeringPrompt: string | null = null;

  // Session name from the environment — needed for the prism event CLI call.
  const sessionName = process.env.PRISM_SESSION_NAME ?? "";

  return {
    // Inject doom-loop steering as a user-role message into the messages array.
    // This fires on each LLM turn; the pending flag is one-shot (cleared after
    // injection). The message is structurally distinguishable from real user
    // input via the [PRISM-HOOKS DOOM-LOOP DETECTION] prefix.
    "experimental.chat.messages.transform": async (_input, output) => {
      if (pendingSteeringPrompt !== null) {
        const text = pendingSteeringPrompt;
        pendingSteeringPrompt = null;
        // Inject a synthetic user-role message at the end of the history.
        // IDs use the msg_/prt_ prefixes required by opencode's MessageID/PartID
        // schemas. The text field (not content) is what TextPart expects.
        const now = Date.now();
        const syntheticMsg: any = {
          info: {
            id: `msg_doom-loop-${now}`,
            role: "user",
            time: { created: now },
          },
          parts: [
            {
              id: `prt_doom-loop-${now}`,
              type: "text",
              text: text,
              time: { created: now },
            },
          ],
        };
        output.messages.push(syntheticMsg);
      }
      return output;
    },

    // After a bash tool executes a git push, set a flag so the next LLM turn
    // gets a review reminder injected into the system prompt.
    "tool.execute.after": async (input) => {
      if (input.tool === "bash") {
        const command: string = (input.args as any)?.command ?? "";
        // Match real `git push` invocations only — see isGitPush() below for
        // the tokenised check that ignores quoted arguments and heredoc
        // bodies (issue #1519). The previous raw-string regex fired on
        // commands such as `echo "git push"` and `rg "git push"`.
        if (isGitPush(command)) {
          pendingReviewReminder = true;
        }

        // Cycle counting for `prism review` was deleted in #1512 (Shape B).
        // The Go-side review monitor now owns the LOOP-LIMIT decision and
        // appends a footer to the review-complete prompt body when
        // appropriate. No bash-substring detection happens here — which is
        // exactly what dissolves the false-match defect class for this hook.
      }

      // ── Doom-loop detection ──────────────────────────────────────────────
      // Excluded tools (read, grep, glob, todowrite) do not contribute to loop
      // counts but DO break an active run — a different tool being invoked
      // resets the state so a subsequent same-tool loop can fire fresh.
      if (EXCLUDED_TOOLS.has(input.tool)) {
        // Break the current run without tracking the excluded call.
        doomLoop.currentKey = null;
        doomLoop.consecutiveCount = 0;
        doomLoop.fired = false;
      } else {
        const key = similarityKey(input.tool, input.args);

        if (key === null) {
          // Should not happen for non-excluded tools, but be defensive.
        } else if (key === doomLoop.currentKey) {
          // Same pattern — extend the run.
          if (!doomLoop.fired) {
            doomLoop.consecutiveCount++;

            if (doomLoop.consecutiveCount >= DOOM_LOOP_THRESHOLD) {
              // Fire!
              doomLoop.fired = true;

              const steeringText =
                `[PRISM-HOOKS DOOM-LOOP DETECTION] You've called \`${input.tool}\` with the same arguments ${doomLoop.consecutiveCount} times in a row. ` +
                `This usually means the current approach isn't working — stop and rethink. ` +
                `Consider: is there a different tool that would help? Is there a misunderstanding about the task? Should you escalate to the user?`;
              pendingSteeringPrompt = steeringText;

              // Log the event to the prism DB asynchronously (fire-and-forget).
              // We do not await this so the tool execution path is not blocked.
              if (sessionName) {
                Bun.spawn(
                  [
                    "prism",
                    "event",
                    "doom-loop-detected",
                    "--session",
                    sessionName,
                    "--tool",
                    input.tool,
                    "--pattern",
                    key,
                    "--count",
                    String(doomLoop.consecutiveCount),
                  ],
                  { stderr: "ignore", stdout: "ignore" },
                );
              }
            }
          }
          // If already fired: suppress — do nothing for further matching calls.
        } else {
          // Pattern broke (different tool or different key) — reset state and
          // start tracking the new pattern.
          doomLoop.currentKey = key;
          doomLoop.consecutiveCount = 1;
          doomLoop.fired = false;
        }
      }
    },

    // Inject the post-push review reminder into the system prompt on the
    // next LLM turn. Doom-loop steering is handled by
    // experimental.chat.messages.transform, not here.
    //
    // The review-cycle escalation message that previously lived in this
    // hook was removed in #1512 (Shape B): the Go-side monitor now appends
    // the LOOP-LIMIT footer to the review-complete prompt body, so the
    // warning is delivered exactly once — embedded in the prompt the
    // worker is already going to act on — instead of being re-injected on
    // every turn. This eliminates the per-turn spam and the bash-substring
    // false-match miscount in one structural move.
    "experimental.chat.system.transform": async (_input, output) => {
      if (!pendingReviewReminder) return output;
      pendingReviewReminder = false;

      output.system.push(
        "You just ran git push. If this was in the context of an open PR, invoke @review-goal-subagent, @review-code-subagent, @review-security-subagent, @review-qa-subagent, and @review-context-subagent as parallel Task calls (all five in a single response) to review the updated changes before the PR is merged. ALL 5 agents must pass before the PR can be merged.",
      );
      return output;
    },
  };
};
