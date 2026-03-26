/**
 * OpenCode plugin — injects session tracking headers into Anthropic API requests
 * so the Meridian proxy can reliably map OpenCode sessions to Claude SDK sessions.
 *
 * Meridian proxy: https://github.com/rynfar/opencode-claude-max-proxy
 *
 * Usage: start Meridian separately, then run opencode with:
 *   ANTHROPIC_API_KEY=x ANTHROPIC_BASE_URL=http://127.0.0.1:3456 opencode
 */

type ChatHeadersHook = (
  incoming: {
    sessionID: string;
    agent: any;
    model: { providerID: string };
    provider: any;
    message: { id: string };
  },
  output: { headers: Record<string, string> }
) => Promise<void>;

type PluginHooks = {
  "chat.headers"?: ChatHeadersHook;
};

type PluginFn = (input: any) => Promise<PluginHooks>;

export const ClaudeMaxHeadersPlugin: PluginFn = async (_input) => {
  return {
    "chat.headers": async (incoming, output) => {
      // Only inject headers for Anthropic provider requests
      if (incoming.model.providerID !== "anthropic") return;

      output.headers["x-opencode-session"] = incoming.sessionID;
      output.headers["x-opencode-request"] = incoming.message.id;
    },
  };
};

export default ClaudeMaxHeadersPlugin;
