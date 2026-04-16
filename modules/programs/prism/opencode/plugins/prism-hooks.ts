import type { Plugin } from "@opencode-ai/plugin";

export const PrismHooks: Plugin = async () => {
  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  // Read enhanced review mode once at plugin initialisation time.
  const enhancedReview = process.env.ENHANCED_REVIEW === "true";

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

  return {
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

        // Detect `prism review <pr-number>` Bash invocations in enhanced mode.
        // This counts as one review cycle (same as a parallel batch of Task calls).
        if (enhancedReview) {
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
      }

      // Detect review agent Task invocations (fallback path — direct @review-* calls).
      // Each invocation (or parallel batch of invocations) for a given PR
      // counts as one review cycle.
      if (input.tool === "task" && enhancedReview) {
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
    },

    // Inject messages into the system prompt on the next LLM turn:
    // 1. Escalation warning if the review cycle limit has been reached.
    // 2. Review reminder after a git push (one-shot, cleared after injection).
    "experimental.chat.system.transform": async (_input, output) => {
      // Clear the per-turn cycle deduplication flag so the next batch of
      // review agent invocations counts as a fresh cycle.
      pendingCycleCount = false;

      // Inject escalation reminder if 3 or more review cycles have elapsed
      // for the current PR without all agents passing.
      if (enhancedReview) {
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
      }

      if (!pendingReviewReminder) return output;
      pendingReviewReminder = false;

      if (enhancedReview) {
        output.system.push(
          "You just ran git push. If this was in the context of an open PR, run `prism review <pr-number>` to review the updated changes before the PR is merged (preferred). Alternatively, invoke @review-goal, @review-code, @review-security, @review-qa, and @review-context in parallel as a fallback. ALL 5 agents must pass before the PR can be merged.",
        );
      } else {
        output.system.push(
          "You just ran git push. If this was in the context of an open PR, invoke the @review subagent now so the updated changes are reviewed before the PR is merged.",
        );
      }
      return output;
    },
  };
};
