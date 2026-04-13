import type { Plugin } from "@opencode-ai/plugin";

export const PrismHooks: Plugin = async (input) => {
  const client = input?.client;

  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  // Set when gh pr create is blocked by incomplete todos; consumed on the next
  // LLM turn to inject an explanation into the system prompt.
  let pendingPrBlockMessage: string | null = null;

  return {
    // Before a bash tool executes, intercept `gh pr create` and check the
    // session's todo list. If any todos are pending or in_progress, rewrite the
    // command to an explanatory echo instead of allowing the PR to be created.
    "tool.execute.before": async (hookInput, output) => {
      if (hookInput.tool !== "bash") return;

      const command: string = (output.args as any)?.command ?? "";

      // Match `gh pr create` with or without preceding chained commands or
      // trailing flags. The lookahead (?=\s|$) ensures we don't match a
      // hypothetical `gh pr create-something` subcommand.
      // Intentionally does NOT match: gh pr view, gh pr list, gh pr merge,
      // gh pr close, gh pr checkout.
      const isPrCreate = /\bgh\s+pr\s+create(?=\s|$)/.test(command);
      if (!isPrCreate) return;

      // Fail-open guards: if client or session is unavailable, allow through.
      if (!client || !client.session) return;

      let todos: Array<{ content: string; status: string; id: string; priority: string }>;
      try {
        const result = await client.session.todo({
          path: { id: hookInput.sessionID },
        });
        // The SDK returns a response object; unwrap the data.
        const data = (result as any)?.data ?? result;
        if (!Array.isArray(data)) return;
        todos = data;
      } catch {
        // Fail open on any error fetching todos.
        return;
      }

      // If there are no todos at all, allow the PR through — don't block
      // agents on trivial tasks that have no structured AC.
      if (todos.length === 0) return;

      // Find todos that are not yet resolved.
      const incomplete = todos.filter(
        (t) => t.status === "pending" || t.status === "in_progress",
      );

      if (incomplete.length === 0) return;

      // Rewrite the command to a safe echo that explains the block.
      const itemList = incomplete
        .map((t) => `  - [${t.status}] ${t.content}`)
        .join("\n");
      const message =
        `PR creation blocked: ${incomplete.length} todo item(s) are still incomplete.\n` +
        `Mark all todos as completed or cancelled before opening a PR.\n\n` +
        `Incomplete items:\n${itemList}`;

      // Use printf with single-quote escaping so real newlines in the message
      // are preserved in the shell output. JSON.stringify would encode them as
      // \n (two chars) which echo prints literally rather than as newlines.
      const shellEscaped = message.replace(/'/g, "'\\''");
      output.args = {
        ...(output.args as any),
        command: `printf '%s\n' '${shellEscaped}'`,
      };

      // Store the block message so the next system transform can inject it.
      pendingPrBlockMessage =
        `gh pr create was blocked because ${incomplete.length} todo item(s) are still ` +
        `incomplete. Do not open the PR until all todos are marked completed or cancelled.\n\n` +
        `Incomplete todos:\n${itemList}\n\n` +
        `Mark each item completed (with evidence) or cancelled (with a reason) using TodoWrite, ` +
        `then retry gh pr create.`;
    },

    // After a bash tool executes a git push, set a flag so the next LLM turn
    // gets a review reminder injected into the system prompt.
    "tool.execute.after": async (hookInput) => {
      if (hookInput.tool !== "bash") return;

      const command: string = (hookInput.args as any)?.command ?? "";
      // Match push commands: `git push`, `git push <remote> <branch>`, etc.
      // Also match `git -C <path> push` and `git --git-dir=... push` variants.
      const isPush = /\bgit(\s+-C\s+\S+|\s+--git-dir\S*)*\s+push\b/.test(
        command,
      );
      if (isPush) pendingReviewReminder = true;
    },

    // Inject system prompts on the next LLM turn:
    //   • PR block explanation (when gh pr create was blocked by incomplete todos)
    //   • Review reminder (after a git push)
    // Each flag is cleared after firing so it only fires once.
    "experimental.chat.system.transform": async (_hookInput, output) => {
      if (pendingPrBlockMessage) {
        output.system.push(pendingPrBlockMessage);
        pendingPrBlockMessage = null;
      }
      if (pendingReviewReminder) {
        pendingReviewReminder = false;
        output.system.push(
          "You just ran git push. If this was in the context of an open PR, invoke the @review subagent now so the updated changes are reviewed before the PR is merged.",
        );
      }
      return output;
    },
  };
};
