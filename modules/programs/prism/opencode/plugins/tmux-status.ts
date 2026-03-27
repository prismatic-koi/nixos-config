import type { Plugin } from "@opencode-ai/plugin";

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
        case "permission.asked":
          await notify("set-waiting");
          break;
        case "permission.replied":
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
    // Fires before the LLM generates the compaction summary.
    // Set flag so concurrent busy events don't override the compacting state.
    "experimental.session.compacting": async (_input, output) => {
      compacting = true;
      await notify("set-compacting");
      return output;
    },
  };
};
