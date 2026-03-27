import type { Plugin } from "@opencode-ai/plugin";
import { appendFileSync } from "fs";

const PERMISSION_LOG =
  (process.env.XDG_DATA_HOME ?? `${process.env.HOME}/.local/share`) +
  "/opencode/permission-asks.jsonl";

export const TmuxStatus: Plugin = async ({ $ }) => {
  const notify = (state: string) =>
    $`echo '{}' | prism notify ${state}`.quiet().nothrow();

  const pane = process.env.TMUX_PANE ?? "";

  const setTitle = (title: string) =>
    $`tmux set-window-option -t ${pane} @agent_title ${title}`.quiet().nothrow();

  const clearTitle = () =>
    $`tmux set-window-option -t ${pane} -u @agent_title`.quiet().nothrow();

  // Flag to suppress busy→active transitions while compaction is in progress.
  let compacting = false;

  return {
    // Log every permission ask to a JSONL file for later analysis.
    // This gives accurate data on what commands are being gated, without
    // having to infer from tool call durations.
    "permission.ask": async (input, output) => {
      try {
        const entry = JSON.stringify({
          time: new Date().toISOString(),
          tool: input.type,
          pattern: input.pattern,
          title: input.title,
          sessionID: input.sessionID,
          callID: input.callID ?? null,
        });
        appendFileSync(PERMISSION_LOG, entry + "\n");
      } catch {
        // Never block the permission flow due to logging failure.
      }
      return output;
    },
    event: async ({ event }) => {
      switch (event.type) {
        case "session.status":
          if (event.properties.status.type === "busy") {
            // Don't overwrite compacting state with active.
            if (!compacting) notify("set-active");
          } else if (event.properties.status.type === "retry")
            notify("set-error");
          else if (event.properties.status.type === "idle")
            notify("set-finished");
          break;
        case "session.idle":
          notify("set-finished");
          break;
        case "session.created":
        case "session.updated": {
          const info = event.properties.info;
          if (info.title) setTitle(info.title);
          // session.updated fires with info.compacting set when compaction starts.
          if (info.compacting) {
            compacting = true;
            notify("set-compacting");
          }
          break;
        }
        case "session.deleted":
          notify("clear");
          clearTitle();
          break;
        case "permission.asked":
          notify("set-waiting");
          break;
        case "permission.replied":
          notify("set-active");
          break;
        case "session.error":
          notify("set-error");
          break;
        case "session.compacted":
          // Compaction done — agent returns to idle.
          compacting = false;
          notify("set-finished");
          break;
      }
    },
    // Fires before the LLM generates the compaction summary.
    // Set flag so concurrent busy events don't override the compacting state.
    "experimental.session.compacting": async (_input, output) => {
      compacting = true;
      notify("set-compacting");
      return output;
    },
  };
};
