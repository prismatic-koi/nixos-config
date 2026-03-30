import type { Plugin } from "@opencode-ai/plugin";
import { appendFileSync } from "fs";

const PERMISSION_LOG =
  (process.env.XDG_DATA_HOME ?? `${process.env.HOME}/.local/share`) +
  "/opencode/permission-asks.jsonl";

export const PrismHooks: Plugin = async ({ $ }) => {
  const notify = (state: string) =>
    $`echo '{}' | prism notify ${state}`.quiet().nothrow();

  const pane = process.env.TMUX_PANE ?? "";

  const setTitle = (title: string) =>
    $`tmux set-window-option -t ${pane} @agent_title ${title}`
      .quiet()
      .nothrow();

  const clearTitle = () =>
    $`tmux set-window-option -t ${pane} -u @agent_title`.quiet().nothrow();

  // Flag to suppress busy→active transitions while compaction is in progress.
  let compacting = false;

  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.status":
          if (event.properties.status.type === "busy") {
            // Don't overwrite compacting state with active.
            if (!compacting) await notify("set-active");
          } else if (event.properties.status.type === "retry")
            await notify("set-error");
          else if (event.properties.status.type === "idle")
            await notify("set-finished");
          break;
        case "session.idle":
          await notify("set-finished");
          break;
        case "session.created":
        case "session.updated": {
          const info = event.properties.info;
          if (info.title) await setTitle(info.title);
          // session.updated fires with info.compacting set when compaction starts.
          if (info.compacting) {
            compacting = true;
            await notify("set-compacting");
          }
          break;
        }
        case "session.deleted":
          await notify("clear");
          await clearTitle();
          break;
        case "permission.asked": {
          // prism notify returns immediately (display-message runs async in Go).
          await notify("set-waiting");
          // Log to JSONL for later analysis — permission.asked carries a
          // PermissionRequest object with the permission type, patterns, and
          // tool metadata.
          const props = event.properties as any;
          const entry = JSON.stringify({
            time: new Date().toISOString(),
            sessionID: props.sessionID,
            permission: props.permission,
            patterns: props.patterns,
            metadata: props.metadata,
            tool: props.tool,
          });
          try { appendFileSync(PERMISSION_LOG, entry + "\n"); } catch { }
          break;
        }
        case "permission.replied":
          await notify("set-active");
          break;
        // question.asked fires when the agent uses the question tool to ask
        // the user something — treat identically to a permission wait.
        case "question.asked":
          await notify("set-waiting");
          break;
        case "question.replied":
        case "question.rejected":
          await notify("set-active");
          break;
        case "session.error":
          await notify("set-error");
          break;
        case "session.compacted":
          // Compaction done — agent returns to idle.
          compacting = false;
          await notify("set-finished");
          break;
      }
    },

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

    // Fires before the LLM generates the compaction summary.
    // Set flag so concurrent busy events don't override the compacting state.
    "experimental.session.compacting": async (_input, output) => {
      compacting = true;
      await notify("set-compacting");
      return output;
    },
  };
};
