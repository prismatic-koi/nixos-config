import type { Plugin } from "@opencode-ai/plugin";

export const TmuxStatus: Plugin = async ({ $ }) => {
  const tmux = (action: string) =>
    $`echo '{}' | cli.tmux.setStatus ${action}`.quiet().nothrow();

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
            if (!compacting) await tmux("set-active");
          } else if (event.properties.status.type === "retry")
            await tmux("set-error");
          else if (event.properties.status.type === "idle")
            await tmux("set-finished");
          break;
        case "session.idle":
          await tmux("set-finished");
          break;
        case "session.created":
        case "session.updated":
          if (event.properties.info.title)
            await setTitle(event.properties.info.title);
          break;
        case "session.deleted":
          await tmux("clear");
          await clearTitle();
          break;
        case "permission.asked":
          await tmux("set-waiting");
          break;
        case "permission.replied":
          await tmux("set-active");
          break;
        case "session.error":
          await tmux("set-error");
          break;
        case "session.compacted":
          // Compaction done — agent returns to idle.
          compacting = false;
          await tmux("set-finished");
          break;
      }
    },
    // Fires before the LLM generates the compaction summary.
    // Set flag so concurrent busy events don't override the compacting state.
    "experimental.session.compacting": async (_input, output) => {
      compacting = true;
      await tmux("set-compacting");
      return output;
    },
  };
};
