import type { Plugin } from "@opencode-ai/plugin";

export const TmuxStatus: Plugin = async ({ $ }) => {
  const tmux = (action: string) =>
    $`echo '{}' | cli.tmux.setStatus ${action}`.quiet().nothrow();

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.status":
          if (event.properties.status.type === "busy")
            await tmux("set-active");
          else if (event.properties.status.type === "retry")
            await tmux("set-error");
          else if (event.properties.status.type === "idle")
            await tmux("set-finished");
          break;
        case "session.idle":
          await tmux("set-finished");
          break;
        case "session.deleted":
          await tmux("clear");
          break;
        case "permission.asked":
          await tmux("set-waiting");
          break;
        case "permission.replied":
          // agent is resuming work after permission grant
          await tmux("set-active");
          break;
        case "session.error":
          await tmux("set-error");
          break;
        case "session.compacted":
          // compaction finished — transition back to active (agent resumes)
          await tmux("set-active");
          break;
      }
    },
    // fires before the LLM generates the compaction summary
    "experimental.session.compacting": async (_input, output) => {
      await tmux("set-compacting");
      return output;
    },
  };
};
