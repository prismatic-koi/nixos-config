import type { Plugin } from "@opencode-ai/plugin";

export const PrismHooks: Plugin = async () => {
  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  return {
    // After a bash tool executes a git push, set a flag so the next LLM turn
    // gets a review reminder injected into the system prompt.
    "tool.execute.after": async (input) => {
      if (input.tool !== "bash") return;

      const command: string = (input.args as any)?.command ?? "";
      // Match push commands: `git push`, `git push <remote> <branch>`, etc.
      // Also match `git -C <path> push` and `git --git-dir=... push` variants.
      const isPush = /\bgit(\s+-C\s+\S+|\s+--git-dir\S*)*\s+push\b/.test(
        command,
      );
      if (isPush) pendingReviewReminder = true;
    },

    // Inject the review reminder into the system prompt on the next LLM turn
    // after a git push. Cleared immediately so it only fires once.
    "experimental.chat.system.transform": async (_input, output) => {
      if (!pendingReviewReminder) return output;
      pendingReviewReminder = false;
      output.system.push(
        "You just ran git push. If this was in the context of an open PR, invoke the @review subagent now so the updated changes are reviewed before the PR is merged.",
      );
      return output;
    },
  };
};
