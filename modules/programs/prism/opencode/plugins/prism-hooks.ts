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
//   bash      — compare command + first positional argument only (strips flags).
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

export const PrismHooks: Plugin = async (pluginInput) => {
  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  // Track review cycles per PR for escalation enforcement.
  // A cycle is counted each time the worker invokes a review agent Task —
  // not on each git push, which would misfire for pre-review amendment pushes.
  // Since all 5 agents are invoked in parallel (same LLM response), the first
  // detected invocation per PR per cycle increments the counter; subsequent
  // parallel invocations in the same batch are deduplicated by the
  // pendingCycleCount flag below.
  // Key: PR number string (or "unknown"), Value: number of review cycles.
  const reviewCycles = new Map<string, number>();

  // The PR number most recently detected from a Task tool invocation of a
  // review agent. Used to scope cycle counts to the correct PR.
  let detectedPrNumber: string | null = null;

  // Prevents counting 5 parallel review-agent invocations as 5 cycles.
  // Set to true when the first review agent Task fires; cleared on the next
  // LLM turn (system.transform), so each full round of parallel invocations
  // counts as exactly one cycle.
  let pendingCycleCount = false;

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
        // The message ID is timestamp-based to ensure uniqueness.
        const syntheticMsg: any = {
          info: {
            id: `doom-loop-${Date.now()}`,
            role: "user",
            time: { created: Date.now() },
          },
          parts: [
            {
              id: `doom-loop-part-${Date.now()}`,
              type: "text",
              content: text,
              time: { created: Date.now() },
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
        // Match push commands: `git push`, `git push <remote> <branch>`, etc.
        // Also match `git -C <path> push` and `git --git-dir=... push` variants.
        const isPush = /\bgit(\s+-C\s+\S+|\s+--git-dir\S*)*\s+push\b/.test(
          command,
        );
        if (isPush) {
          pendingReviewReminder = true;
        }

        // Detect `prism review <pr-number>` Bash invocations.
        // This counts as one review cycle (same as a parallel batch of Task calls).
        const prismReviewMatch = command.match(
          /\bprism\s+review\s+(\d+)\b/,
        );
        if (prismReviewMatch) {
          const newPr = prismReviewMatch[1];
          if (newPr !== detectedPrNumber) {
            detectedPrNumber = newPr;
            reviewCycles.clear();
          }
          if (!pendingCycleCount) {
            pendingCycleCount = true;
            const prKey = detectedPrNumber ?? "unknown";
            reviewCycles.set(prKey, (reviewCycles.get(prKey) ?? 0) + 1);
          }
        }
      }

      // Detect review agent Task invocations (fallback path — direct @review-* calls).
      // Each invocation (or parallel batch of invocations) for a given PR
      // counts as one review cycle.
      if (input.tool === "task") {
        const prompt: string = (input.args as any)?.prompt ?? "";
        const subagentType: string = (input.args as any)?.subagent_type ?? "";

        const isReviewAgent =
          /^review(-goal|-code|-security|-qa|-context)?$/.test(subagentType);

        if (isReviewAgent) {
          // Try to extract a PR number from the prompt text.
          const prMatch =
            prompt.match(/\bPR\s*#(\d+)\b/i) ??
            prompt.match(/pull\s+request\s*#(\d+)/i) ??
            prompt.match(/\bpr[_\s-]?number[:\s]+(\d+)/i) ??
            prompt.match(/\b#(\d+)\b/);
          if (prMatch) {
            const newPr = prMatch[1];
            // If the PR number changed, reset cycle counts for the new PR.
            if (newPr !== detectedPrNumber) {
              detectedPrNumber = newPr;
              reviewCycles.clear();
            }
          }

          // Count this as a review cycle — but only once per batch of parallel
          // invocations. pendingCycleCount is set here and cleared on the next
          // system.transform, so 5 parallel Task calls count as exactly 1 cycle.
          if (!pendingCycleCount) {
            pendingCycleCount = true;
            const prKey = detectedPrNumber ?? "unknown";
            reviewCycles.set(prKey, (reviewCycles.get(prKey) ?? 0) + 1);
          }
        }
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
                const $ = (pluginInput as any).$;
                if ($) {
                  void $`prism event doom-loop-detected --session ${sessionName} --tool ${input.tool} --pattern ${key} --count ${String(doomLoop.consecutiveCount)}`.quiet().catch(() => {
                    // Ignore errors — event logging is best-effort.
                  });
                }
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

    // Inject messages into the system prompt on the next LLM turn:
    // 1. Escalation warning if the review cycle limit has been reached.
    // 2. Review reminder after a git push (one-shot, cleared after injection).
    // Note: doom-loop steering is handled by experimental.chat.messages.transform,
    // not here. This hook only manages the review-related system prompt injections.
    "experimental.chat.system.transform": async (_input, output) => {
      // Clear the per-turn cycle deduplication flag so the next batch of
      // review agent invocations counts as a fresh cycle.
      pendingCycleCount = false;

      // Inject escalation reminder if 3 or more review cycles have elapsed
      // for the current PR without all agents passing.
      const prKey = detectedPrNumber ?? "unknown";
      const cycles = reviewCycles.get(prKey) ?? 0;
      if (cycles >= 3) {
        const prLabel = detectedPrNumber
          ? `PR #${detectedPrNumber}`
          : "this PR";
        output.system.push(
          `⚠️ REVIEW LOOP LIMIT: You have run ${cycles} review cycles for ${prLabel} without all agents passing. You MUST stop and escalate to the user now. Do NOT run another review cycle. Instead, summarise: (1) what was originally requested, (2) what each review cycle found, and (3) why the fixes are not converging. Hand off to the coordinator.`,
        );
      }

      if (!pendingReviewReminder) return output;
      pendingReviewReminder = false;

      output.system.push(
        "You just ran git push. If this was in the context of an open PR, invoke @review-goal, @review-code, @review-security, @review-qa, and @review-context as parallel Task calls (all five in a single response) to review the updated changes before the PR is merged. ALL 5 agents must pass before the PR can be merged.",
      );
      return output;
    },
  };
};
