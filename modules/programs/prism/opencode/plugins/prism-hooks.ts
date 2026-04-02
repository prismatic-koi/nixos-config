import type { Plugin } from "@opencode-ai/plugin";
import { appendFileSync } from "fs";
import * as path from "path";
import * as fs from "fs";
import { Database } from "bun:sqlite";
import { homedir } from "os";

const PERMISSION_LOG =
  (process.env.XDG_DATA_HOME ?? `${process.env.HOME}/.local/share`) +
  "/opencode/permission-asks.jsonl";

function deriveSessionName(worktree: string): string | null {
  // Walk up to find .bare marker
  let dir = worktree;
  let bareRoot: string | null = null;
  while (true) {
    if (fs.existsSync(path.join(dir, ".bare"))) {
      bareRoot = dir;
      break;
    }
    const parent = path.dirname(dir);
    if (parent === dir) break; // filesystem root
    dir = parent;
  }
  if (!bareRoot) {
    console.warn("[prism-hooks] no .bare marker found — DB writes disabled");
    return null;
  }

  let repo = path.basename(bareRoot);
  if (repo.endsWith(".git")) repo = repo.slice(0, -4);
  // Match Go's strings.ReplaceAll(filepath.Base(projectRoot), ".", "_") so
  // both sides produce the same repo component in the session name.
  repo = repo.replaceAll(".", "_");

  // Derive branch component — mirrors worktreeBranchComponent() in cmd/switch.go.
  let branch: string;
  const symResult = Bun.spawnSync(["git", "-C", worktree, "symbolic-ref", "HEAD"]);
  if (symResult.exitCode === 0) {
    const ref = new TextDecoder().decode(symResult.stdout).trim();
    const stripped = ref.replace(/^refs\/heads\//, "");
    // The "--" substitution for "/" is for tmux session name compatibility only.
    // tmux does not allow "/" in session names. If tmux is ever removed in favour
    // of a custom TUI, restore full branch names with "/" here and in the plugin.
    branch = stripped.replaceAll("/", "--");
  } else {
    // Detached HEAD — fall back to short commit hash.
    const hashResult = Bun.spawnSync(["git", "-C", worktree, "rev-parse", "--short", "HEAD"]);
    if (hashResult.exitCode === 0) {
      branch = new TextDecoder().decode(hashResult.stdout).trim();
    } else {
      // Not a git repo or both commands failed — fall back to directory basename.
      // No dot substitution: dots are valid in tmux session names and branch
      // names may contain dots; the git-derived path above doesn't sanitise
      // dots, so keep both paths consistent.
      branch = path.basename(worktree);
    }
  }

  return `${repo}@${branch}`;
}

export const PrismHooks: Plugin = async ({ $, worktree: _worktree }) => {
  // worktree is defined in the PluginInput types but is NOT passed by opencode
  // at runtime — it arrives as undefined. Fall back to process.cwd() so that
  // deriveSessionName always receives a valid path.
  const worktree = _worktree ?? process.cwd();
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

  // Accumulate text parts by messageId so that msg_user / msg_assistant events
  // can include the actual text content. Text arrives via message.part.updated
  // (TextPart); the final write happens when message.updated fires.
  const textByMessageId = new Map<string, string>();

  // Set when a git push is detected; consumed on the next LLM turn to inject
  // a review reminder into the system prompt.
  let pendingReviewReminder = false;

  // Derive session name from worktree
  const sessionName = deriveSessionName(worktree);

  // Open DB
  const stateHome = process.env.XDG_STATE_HOME ?? path.join(homedir(), ".local/state");
  const dbPath = path.join(stateHome, "prism", "prism.db");

  let db: Database | null = null;
  if (sessionName) {
    try {
      if (fs.existsSync(dbPath)) {
        db = new Database(dbPath, { create: false });
      } else {
        console.warn("[prism-hooks] prism.db not found — DB writes disabled");
      }
    } catch (e) {
      console.error("[prism-hooks] failed to open prism.db:", e);
    }
  }

  // Prepared statements (only if db is open)
  const upsertStatus = db?.prepare(`
    INSERT INTO agent_status (session_name, repo, worktree, state, title, opencode_sid, last_seen)
    VALUES (?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(session_name) DO UPDATE SET
      state        = excluded.state,
      title        = COALESCE(excluded.title, title),
      opencode_sid = COALESCE(excluded.opencode_sid, opencode_sid),
      last_seen    = excluded.last_seen
  `);

  const updateTitleSid = db?.prepare(`
    UPDATE agent_status SET
      title        = COALESCE(?, title),
      opencode_sid = COALESCE(?, opencode_sid),
      last_seen    = ?
    WHERE session_name = ?
  `);

  const insertEvent = db?.prepare(`
    INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?)
  `);

  const insertBusMsg = db?.prepare(`
    INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at, delivered_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
  `);

  const getStatus = db?.prepare(`SELECT state FROM agent_status WHERE session_name = ?`);

  const activeCoord = db?.prepare(`
    SELECT session_name FROM agent_status
    WHERE session_name = ? AND ended_at IS NULL AND last_seen > ?
  `);

  const setEnded = db?.prepare(`
    UPDATE agent_status SET ended_at = ? WHERE session_name = ?
  `);

  // Helpers (closed over db, sessionName, worktree)
  const repo = sessionName ? sessionName.split("@")[0]! : "";

  function writeEvent(type: string, payload: unknown, opencodeSid?: string | null): void {
    if (!db || !sessionName || !insertEvent) return;
    try {
      insertEvent.run(
        crypto.randomUUID(), sessionName, repo, worktree,
        opencodeSid ?? null, type, JSON.stringify(payload), Date.now()
      );
    } catch (e) { console.error("[prism-hooks] writeEvent failed:", e); }
  }

  function upsertAgentStatus(state: string, title?: string | null, opencodeSid?: string | null): void {
    if (!db || !sessionName || !upsertStatus) return;
    try {
      upsertStatus.run(sessionName, repo, worktree, state, title ?? null, opencodeSid ?? null, Date.now());
    } catch (e) { console.error("[prism-hooks] upsertStatus failed:", e); }
  }

  function notifyCoordinator(message: string): void {
    if (!db || !sessionName || !activeCoord || !insertBusMsg) return;
    try {
      const coordName = `${repo}@main`;
      const fiveMinutesAgo = Date.now() - 5 * 60 * 1000;
      const coord = activeCoord.get(coordName, fiveMinutesAgo) as { session_name: string } | undefined;
      if (coord) {
        insertBusMsg.run(crypto.randomUUID(), sessionName, coordName, repo, message, "normal", Date.now());
      }
    } catch (e) { console.error("[prism-hooks] notifyCoordinator failed:", e); }
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.status":
          if (event.properties.status.type === "busy") {
            // Don't overwrite compacting state with active.
            if (!compacting) {
              await notify("set-active");
              // DB
              upsertAgentStatus("active");
              writeEvent("state_change", { state: "active" });
            }
          } else if (event.properties.status.type === "retry") {
            await notify("set-error");
            // DB
            upsertAgentStatus("error");
            writeEvent("state_change", { state: "error" });
          } else if (event.properties.status.type === "idle") {
            await notify("set-finished");
          }
          break;

        case "session.idle": {
          await notify("set-finished");
          // DB
          let prevState: string | null = null;
          if (db && sessionName && getStatus) {
            try {
              const row = getStatus.get(sessionName) as { state: string } | undefined;
              prevState = row?.state ?? null;
            } catch (e) { console.error("[prism-hooks] getStatus failed:", e); }
          }
          upsertAgentStatus("finished");
          writeEvent("state_change", { state: "finished" });
          if (prevState === "active") {
            notifyCoordinator(`Agent ${sessionName} has finished its current task`);
          }
          break;
        }

        case "session.created": {
          const info = event.properties.info;
          if (info.title) await setTitle(info.title);
          // DB
          upsertAgentStatus("active", info.title || null, info.id);
          writeEvent("state_change", { state: "active" }, info.id);
          break;
        }

        case "session.updated": {
          const info = event.properties.info;
          if (info.title) await setTitle(info.title);
          if (info.time?.compacting != null) {
            compacting = true;
            await notify("set-compacting");
            // DB
            writeEvent("compaction", { note: "compaction started" }, info.id);
            upsertAgentStatus("compacting", null, info.id);
          } else {
            // DB: only update metadata, don't overwrite state
            if (db && sessionName && updateTitleSid) {
              try {
                updateTitleSid.run(info.title || null, info.id, Date.now(), sessionName);
              } catch (e) { console.error("[prism-hooks] updateTitleSid failed:", e); }
            }
          }
          break;
        }

        case "session.deleted": {
          await notify("clear");
          await clearTitle();
          // DB
          const info = event.properties.info;
          if (db && sessionName && setEnded) {
            try {
              setEnded.run(Date.now(), sessionName);
            } catch (e) { console.error("[prism-hooks] setEnded failed:", e); }
          }
          writeEvent("state_change", { state: "deleted" }, info.id);
          break;
        }

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
          // DB
          upsertAgentStatus("waiting");
          writeEvent("permission_ask", { tool: props.tool, patterns: props.patterns });
          break;
        }

        case "permission.replied":
          await notify("set-active");
          // DB
          upsertAgentStatus("active");
          writeEvent("state_change", { state: "active" });
          break;

        // question.asked fires when the agent uses the question tool to ask
        // the user something — treat identically to a permission wait.
        case "question.asked":
          await notify("set-waiting");
          // DB
          upsertAgentStatus("waiting");
          writeEvent("state_change", { state: "waiting" });
          break;

        case "question.replied":
        case "question.rejected":
          await notify("set-active");
          // DB
          upsertAgentStatus("active");
          writeEvent("state_change", { state: "active" });
          break;

        case "session.error":
          await notify("set-error");
          // DB
          upsertAgentStatus("error");
          writeEvent("error", { note: "session error" });
          break;

        case "session.compacted": {
          // Compaction done — agent returns to idle.
          compacting = false;
          await notify("set-finished");
          // DB
          let prevStateCompact: string | null = null;
          if (db && sessionName && getStatus) {
            try {
              const row = getStatus.get(sessionName) as { state: string } | undefined;
              prevStateCompact = row?.state ?? null;
            } catch (e) { console.error("[prism-hooks] getStatus failed:", e); }
          }
          upsertAgentStatus("finished");
          writeEvent("compaction", { note: "compaction complete" });
          if (prevStateCompact === "active" || prevStateCompact === "compacting") {
            notifyCoordinator(`Agent ${sessionName} context was compacted — check in to verify current state`);
          }
          break;
        }

        case "message.updated": {
          const info = event.properties.info as any;
          if (info.role === "user") {
            // User messages are atomic — write on every update.
            const text = textByMessageId.get(info.id) ?? "";
            writeEvent("msg_user", { messageId: info.id, text });
            // Clean up to avoid unbounded growth of the map.
            textByMessageId.delete(info.id);
          } else if (info.role === "assistant" && info.time?.completed != null) {
            // Only write when the assistant message is fully complete to avoid
            // one row per streaming token.
            const text = textByMessageId.get(info.id) ?? "";
            writeEvent("msg_assistant", { messageId: info.id, text });
            // Clean up accumulated text now that the message is complete.
            textByMessageId.delete(info.id);
          }
          break;
        }

        case "message.part.updated": {
          const part = (event.properties as any).part;
          if (part.type === "text" && part.text) {
            // Accumulate text parts — the message.updated handler uses this
            // to write the full text into the msg_user / msg_assistant payload.
            textByMessageId.set(part.messageID, String(part.text));
          } else if (part.type === "tool" && part.state?.status === "completed") {
            const args = JSON.stringify(part.state.input ?? {}).slice(0, 500);
            const result = String(part.state.output ?? "").slice(0, 500);
            writeEvent("tool_call",   { tool: part.tool, args,   messageId: part.messageID });
            writeEvent("tool_result", { tool: part.tool, result, messageId: part.messageID });
          } else if (part.type === "reasoning" && part.time?.end != null) {
            // Only write when the reasoning block is complete to avoid one row per token.
            writeEvent("thinking", { text: String(part.text ?? "").slice(0, 500), messageId: part.messageID });
          }
          break;
        }
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
