import type { Plugin } from "@opencode-ai/plugin";
import { appendFileSync } from "fs";
import * as path from "path";
import * as fs from "fs";
import { Database } from "bun:sqlite";
import { homedir } from "os";

const PERMISSION_LOG =
  (process.env.XDG_DATA_HOME ?? `${process.env.HOME}/.local/share`) +
  "/opencode/permission-asks.jsonl";

// Agent state constants — must match the Go AgentState constants in
// internal/agent/agent.go. Any change here must be mirrored there.
const STATE_ACTIVE = "active";
const STATE_WAITING = "waiting";
const STATE_FINISHED = "finished";
const STATE_COMPACTING = "compacting";
const STATE_ERROR = "error";
// STATE_IDLE is declared for parity with Go constants; the plugin never writes
// idle directly (tmux-session-start handles idle on the Go side).
const STATE_IDLE = "idle"; // eslint-disable-line @typescript-eslint/no-unused-vars
const STATE_INTERRUPTED = "interrupted";
const STATE_DELETED = "deleted";

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

// Accumulate text parts by messageId so that msg_user / msg_assistant events
// can include the actual text content. Text arrives via message.part.updated
// (TextPart); the final write happens when message.updated fires.
const textByMessageId = new Map<string, string>();

// Track which messageIds have already been written to avoid duplicate events.
// message.updated fires multiple times per message (once per partial update)
// so without this guard each message would produce several DB rows.
//
// This Set grows with the number of messages in a session but is never
// pruned — entries need to persist for the plugin lifetime to guard against
// late re-fires of message.updated after the text map has been cleared.
// Sessions are short-lived (restarted regularly), so unbounded growth within
// a session is acceptable; the tradeoff is intentional.
const writtenMessageIds = new Set<string>();

// Track the last state_change value written so we only emit an event when
// the state actually transitions.  session.status busy fires multiple times
// per turn; without this guard each turn produces 4+ state_change rows.
let lastWrittenState: string | null = null;

// Debounce timer for session.idle: only mark finished and notify the
// coordinator after 2 seconds of inactivity.  If session.status busy fires
// within the window (mid-task turn boundary) the timer is cancelled so no
// spurious finished state or coordinator notification is written.
// Module-level (same as writtenMessageIds) because opencode instantiates the
// plugin twice and both instances share this timer.  This assumes one active
// session per plugin process — which matches the single-user prism design
// where each tmux window runs an independent opencode process.
let idleTimer: ReturnType<typeof setTimeout> | null = null;

// Map from permissionID → { tool, messageID } so that permission.replied can
// correlate a denial back to the tool that was asked about.
const pendingPermissions = new Map<string, { tool: string; messageID: string }>();

// Track the current opencode session ID so bus delivery can call session.prompt.
let currentOpencodeSID: string | null = null;

export const PrismHooks: Plugin = async ({ $, worktree: _worktree, client }) => {
  // worktree is defined in the PluginInput types but is NOT passed by opencode
  // at runtime — it arrives as undefined. Fall back to process.cwd() so that
  // deriveSessionName always receives a valid path.
  const worktree = _worktree ?? process.cwd();
  const pane = process.env.TMUX_PANE ?? "";

  const setTitle = (title: string) =>
    $`tmux set-window-option -t ${pane} @agent_title ${title}`
      .quiet()
      .nothrow();

  const clearTitle = () =>
    $`tmux set-window-option -t ${pane} -u @agent_title`.quiet().nothrow();

  // Flag to suppress busy→active transitions while compaction is in progress.
  let compacting = false;

  // Set when the user manually denies a permission request (permission.replied
  // with response === "reject" after a permission.asked event put the session
  // into the waiting state).  Cleared on the next session.status busy event so
  // that if the agent recovers and issues more tool calls, a subsequent idle
  // correctly becomes finished rather than interrupted.
  let manualDenial = false;

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
        db = new Database(dbPath);
        db.run("PRAGMA busy_timeout = 5000");
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

  // Two-step helpers for session.updated (resumed sessions).
  //
  // Step 1: INSERT OR IGNORE — inserts with state='active' only if no row
  // exists yet.  Statement.run() returns { changes, lastInsertRowid };
  // changes === 1 means a genuine insert (i.e. the session was resumed with
  // no prior row).
  const insertResumedSession = db?.prepare(`
    INSERT OR IGNORE INTO agent_status (session_name, repo, worktree, state, title, opencode_sid, last_seen)
    VALUES (?, ?, ?, ?, ?, ?, ?)
  `);
  // Step 2: UPDATE metadata only — does not touch state so an already-active
  // session keeps its current state.  Also clears ended_at so a previously-
  // closed session that is resumed becomes visible again to dashboard queries
  // (which filter WHERE ended_at IS NULL).
  const updateResumedMetadata = db?.prepare(`
    UPDATE agent_status SET
      title        = COALESCE(?, title),
      opencode_sid = COALESCE(?, opencode_sid),
      last_seen    = ?,
      ended_at     = NULL
    WHERE session_name = ?
  `);

  const insertEvent = db?.prepare(`
    INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?)
  `);

  const setRootAgentModelIfAbsent = db?.prepare(`
    UPDATE agent_status SET
      agent_name      = ?,
      model_id        = ?,
      root_agent_name = COALESCE(root_agent_name, ?),
      root_model_id   = COALESCE(root_model_id, ?)
    WHERE session_name = ?
  `);

  const insertBusMsg = db?.prepare(`
    INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at, delivered_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
  `);

  const getStatus = db?.prepare(`SELECT state FROM agent_status WHERE session_name = ?`);

  const getAgentModel = db?.prepare(`SELECT agent_name, model_id, root_agent_name, root_model_id FROM agent_status WHERE session_name = ?`);

  const pendingMsgs = db?.prepare(`
    SELECT id, from_session, to_session, repo, text, urgency, sent_at, delivered_at
    FROM bus_messages
    WHERE to_session = ? AND urgency = ? AND delivered_at IS NULL
    ORDER BY sent_at ASC
  `);

  const markDelivered = db?.prepare(`
    UPDATE bus_messages SET delivered_at = ? WHERE id = ?
  `);

  // activeCoord: look up the coordinator for a given repo by session name.
  // We only filter on ended_at IS NULL — not on last_seen. A coordinator is
  // considered active as long as its tmux session has not ended (tmux-session-end
  // writes ended_at). Filtering on last_seen would incorrectly exclude an idle
  // coordinator (one waiting for a message) because idle coordinators don't
  // update last_seen. This was the original bug in issue #390.
  const activeCoord = db?.prepare(`
    SELECT session_name FROM agent_status
    WHERE session_name = ? AND ended_at IS NULL
  `);

  const setEnded = db?.prepare(`
    UPDATE agent_status SET ended_at = ? WHERE session_name = ?
  `);

  // Helpers (closed over db, sessionName, worktree)
  const repo = sessionName ? sessionName.split("@")[0]! : "";

  // Register signal handlers so that a Ctrl-C (SIGINT) or termination
  // (SIGTERM) immediately writes "interrupted" and cancels the idle debounce
  // timer.  Without this, opencode catches the signal first and fires
  // session.idle, which starts the 2-second debounce.  The debounce fires
  // "finished" before the tmux pane-died hook can write "interrupted", leaving
  // the session recorded as finished even though it was interrupted.
  //
  // The Bun SQLite run() call is synchronous at the C level, so the write
  // completes before control returns to opencode's own signal handler.  Layer 2
  // (pane-died with --exit-code) is the safety net for cases where the write
  // cannot complete in time (SIGKILL, crash).
  //
  // The pane-died hook and the idle timer callback both already guard against
  // clobbering "interrupted", so it is safe for multiple paths to write it.
  function handleShutdownSignal(): void {
    if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
    // Write interrupted unless the session is already in a terminal state.
    // Notably, we also write when lastWrittenState === null (plugin never saw
    // a session event — interrupted before session.created fired): the
    // pane-died fallback relies on a DB row existing, but the plugin can still
    // write one here via upsertAgentStatus.
    if (
      lastWrittenState !== STATE_FINISHED &&
      lastWrittenState !== STATE_DELETED &&
      lastWrittenState !== STATE_INTERRUPTED
    ) {
      upsertAgentStatus(STATE_INTERRUPTED);
      writeStateChange(STATE_INTERRUPTED);
    }
    // Do not call process.exit() here — let opencode's own signal handling
    // proceed normally.  We only need to ensure the DB write happens first.
  }
  process.on("SIGINT", handleShutdownSignal);
  process.on("SIGTERM", handleShutdownSignal);

  function writeEvent(type: string, payload: unknown, opencodeSid?: string | null): void {
    if (!db || !sessionName || !insertEvent) return;
    try {
      insertEvent.run(
        crypto.randomUUID(), sessionName, repo, worktree,
        opencodeSid ?? null, type, JSON.stringify(payload), Date.now()
      );
    } catch (e) { console.error("[prism-hooks] writeEvent failed:", e); }
  }

  function touchDashboardSentinel(): void {
    const sentinelPath = path.join(stateHome, "prism", "bus", ".dashboard.signal");
    try {
      fs.mkdirSync(path.dirname(sentinelPath), { recursive: true });
      const now = new Date();
      try {
        fs.utimesSync(sentinelPath, now, now);
      } catch {
        // File doesn't exist yet — create it.
        fs.closeSync(fs.openSync(sentinelPath, "a"));
      }
    } catch { /* best-effort */ }
  }

  function writeStateChange(state: string, opencodeSid?: string | null): void {
    if (state === lastWrittenState) return; // deduplicate same-state transitions
    writeEvent("state_change", { state }, opencodeSid);
    lastWrittenState = state;
    touchDashboardSentinel();
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
      if (sessionName === coordName) return;
      const coord = activeCoord.get(coordName) as { session_name: string } | undefined;
      if (coord) {
        insertBusMsg.run(crypto.randomUUID(), sessionName, coordName, repo, message, "normal", Date.now());
      }
    } catch (e) { console.error("[prism-hooks] notifyCoordinator failed:", e); }
  }

  // Interrupt-urgency delivery: poll every 2 seconds and deliver any pending
  // interrupt messages via client.session.promptAsync. This fires regardless of
  // agent state, delivering within ~2 seconds of the message being written.
  //
  // Normal-urgency delivery when already idle: if the coordinator is already in
  // the "finished" state (i.e. it went idle before the bus message arrived and
  // session.idle will never fire again), also deliver pending normal-urgency
  // messages here. This ensures they are delivered within ~2 seconds rather
  // than waiting for a session.idle that may never come.
  setInterval(async () => {
    if (!db || !sessionName || !currentOpencodeSID || !pendingMsgs || !markDelivered) return;

    // Read root_agent_name and root_model_id from agent_status for this session.
    // These are set at session creation and never overwritten by subagent messages,
    // so using them ensures bus delivery always prompts under the root agent's
    // permission context (fixes the permission bug from issue #404).
    // Fall back to agent_name/model_id for sessions created before this migration
    // (where root_agent_name/root_model_id will be NULL).
    let agentName: string | null = null;
    let modelId: string | null = null;
    if (getAgentModel) {
      try {
        const row = getAgentModel.get(sessionName) as { agent_name: string | null; model_id: string | null; root_agent_name: string | null; root_model_id: string | null } | undefined;
        agentName = row?.root_agent_name ?? row?.agent_name ?? null;
        modelId = row?.root_model_id ?? row?.model_id ?? null;
      } catch (e) { /* best-effort */ }
    }

    // Build the promptAsync body. When agent_name and model_id are known,
    // pass them explicitly so the session keeps using its current agent/model.
    // When unknown (pre-migration or no messages yet), call without those fields
    // (current fallback behaviour, acceptable for new sessions).
    function buildPromptBody(text: string): Parameters<typeof client.session.promptAsync>[0]["body"] {
      const base = { parts: [{ type: "text" as const, text }] };
      if (agentName && modelId) {
        // Split model_id on the first "/" to get providerID and modelID.
        const slashIdx = modelId.indexOf("/");
        const providerID = slashIdx >= 0 ? modelId.slice(0, slashIdx) : modelId;
        const modelID = slashIdx >= 0 ? modelId.slice(slashIdx + 1) : "";
        return { ...base, agent: agentName, model: { providerID, modelID } };
      }
      return base;
    }

    try {
      const urgent = pendingMsgs.all(sessionName, "interrupt") as Array<{ id: string; text: string; from_session: string }>;
      for (const msg of urgent) {
        try {
          await client.tui.showToast({
            body: {
              message: `Urgent from ${msg.from_session}: ${msg.text.slice(0, 100)}`,
              variant: "warning",
            },
          });
        } catch { /* toast is best-effort */ }
        try {
          await client.session.promptAsync({
            path: { id: currentOpencodeSID },
            body: buildPromptBody(msg.text),
          });
          markDelivered.run(Date.now(), msg.id);
        } catch (e) { console.error("[prism-hooks] interrupt promptAsync failed:", e); }
      }
    } catch (e) { console.error("[prism-hooks] interrupt delivery failed:", e); }

    // Normal-urgency: deliver pending messages if the coordinator is already idle.
    if (lastWrittenState === STATE_FINISHED) {
      try {
        const normal = pendingMsgs.all(sessionName, "normal") as Array<{ id: string; text: string }>;
        for (const msg of normal) {
          try {
            await client.session.promptAsync({
              path: { id: currentOpencodeSID! },
              body: buildPromptBody(msg.text),
            });
            markDelivered.run(Date.now(), msg.id);
          } catch (e) { console.error("[prism-hooks] normal (idle-poll) promptAsync failed:", e); }
        }
      } catch (e) { console.error("[prism-hooks] normal idle-poll delivery failed:", e); }
    }
  }, 2000);

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.status":
          if (event.properties.status.type === "busy") {
            // Cancel any pending idle→finished debounce: the agent is still
            // active (mid-task turn boundary).
            if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
            // Clear manual denial flag — the agent is issuing new tool calls,
            // so a previous denial is no longer the reason it will stop.
            manualDenial = false;
            // Don't overwrite compacting state with active.
            if (!compacting) {
              // DB
              upsertAgentStatus(STATE_ACTIVE);
              writeStateChange(STATE_ACTIVE);
            }
          } else if (event.properties.status.type === "retry") {
            // DB
            upsertAgentStatus(STATE_ERROR);
            writeStateChange(STATE_ERROR);
          }
          // session.status idle is intentionally not handled here.
          // session.idle fires shortly after and starts the debounce timer;
          // handling idle status here would fire notify("set-finished")
          // immediately on every turn boundary, defeating the debounce entirely.
          break;

        case "session.idle": {
          // Read prevState immediately (before the timer fires) so we capture
          // the state at the point the agent went idle, not 2 seconds later.
          let prevState: string | null = null;
          if (db && sessionName && getStatus) {
            try {
              const row = getStatus.get(sessionName) as { state: string } | undefined;
              prevState = row?.state ?? null;
            } catch (e) { console.error("[prism-hooks] getStatus failed:", e); }
          }
          // Debounce: wait 2 seconds before committing finished state and
          // notifying the coordinator.  If session.status busy fires within
          // that window (mid-task turn boundary) the timer is cancelled and no
          // finished state, UI notification, or coordinator message is written.
          if (idleTimer) clearTimeout(idleTimer);
          idleTimer = setTimeout(() => {
            idleTimer = null;
            // If the user manually denied a permission request, transition to
            // interrupted rather than finished — the agent paused for user input,
            // the user said no, and the agent stopped. This is a user interrupt,
            // not a successful task completion. Skip coordinator notification.
            if (manualDenial) {
              manualDenial = false;
              upsertAgentStatus(STATE_INTERRUPTED);
              writeStateChange(STATE_INTERRUPTED);
              return;
            }
            // Check current DB state: if the session was already marked
            // interrupted by the pane-died hook (Ctrl+C or unexpected exit),
            // do not overwrite that state with finished and do not notify the
            // coordinator — the task was not completed cleanly.
            let currentState: string | null = null;
            if (db && sessionName && getStatus) {
              try {
                const row = getStatus.get(sessionName) as { state: string } | undefined;
                currentState = row?.state ?? null;
              } catch (e) { console.error("[prism-hooks] getStatus (timer) failed:", e); }
            }
            if (currentState === STATE_INTERRUPTED || currentState === STATE_ERROR) {
              // Session was interrupted or errored — leave the state intact
              // and skip coordinator notification.
              return;
            }
            upsertAgentStatus(STATE_FINISHED);
            writeStateChange(STATE_FINISHED);
            if (prevState === STATE_ACTIVE) {
              notifyCoordinator(`Agent ${sessionName} has finished its current task`);
            }
            // Normal-urgency bus delivery is intentionally NOT done here.
            // The setInterval polling loop delivers pending normal-urgency
            // messages whenever lastWrittenState === "finished" (set above via
            // writeStateChange), so delivery happens within ~2 seconds of this
            // timer firing without any additional code here.  Delivering in both
            // places would require cross-path coordination to avoid double-
            // delivery, since both run on independent async timers.
          }, 2000);
          break;
        }

        case "session.created": {
          const info = event.properties.info;
          if (info.title) await setTitle(info.title);
          // Track current opencode session ID for bus delivery.
          currentOpencodeSID = info.id;
          // DB
          upsertAgentStatus(STATE_ACTIVE, info.title || null, info.id);
          writeStateChange(STATE_ACTIVE, info.id);
          break;
        }

        case "session.updated": {
          const info = event.properties.info;
          if (info.title) await setTitle(info.title);
          if (info.time?.compacting != null) {
            compacting = true;
            // DB
            writeEvent("compaction", { note: "compaction started" }, info.id);
            upsertAgentStatus(STATE_COMPACTING, null, info.id);
          } else {
            // Track current opencode session ID for bus delivery.
            currentOpencodeSID = info.id;
            // Two-step so we can detect a genuine resume (new row inserted).
            // Step 1: insert with state='active' if no row exists yet.
            let wasInserted = false;
            if (db && sessionName && insertResumedSession) {
              try {
                const result = insertResumedSession.run(sessionName, repo, worktree, STATE_ACTIVE, info.title || null, info.id, Date.now());
                wasInserted = result.changes === 1;
              } catch (e) { console.error("[prism-hooks] insertResumedSession failed:", e); }
            }
            // Step 2: update metadata (title, opencode_sid, last_seen, ended_at)
            // on the existing row — no-op if we just inserted it.
            if (db && sessionName && updateResumedMetadata) {
              try {
                updateResumedMetadata.run(info.title || null, info.id, Date.now(), sessionName);
              } catch (e) { console.error("[prism-hooks] updateResumedMetadata failed:", e); }
            }
            // Only emit a state_change event when a new row was inserted —
            // i.e. a genuine resume of a session with no prior DB entry.
            if (wasInserted) {
              writeStateChange(STATE_ACTIVE, info.id);
            }
          }
          break;
        }

        case "session.deleted": {
          await clearTitle();
          // DB
          const info = event.properties.info;
          if (db && sessionName && setEnded) {
            try {
              setEnded.run(Date.now(), sessionName);
            } catch (e) { console.error("[prism-hooks] setEnded failed:", e); }
          }
          writeStateChange(STATE_DELETED, info.id);
          break;
        }

        case "permission.asked": {
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
          upsertAgentStatus(STATE_WAITING);
          writeStateChange(STATE_WAITING);
          writeEvent("permission_ask", { tool: props.permission ?? "unknown", patterns: props.patterns, messageId: props.tool?.messageID });
          // Track this permission so we can correlate a denial in permission.replied.
          // props is a PermissionRequest; props.id is the framework request ID that
          // matches repliedProps.requestID in the permission.replied handler.
          // props.permission is the permission-type string (e.g. "bash"), not an object.
          if (props.id) {
            pendingPermissions.set(props.id, {
              tool: props.permission ?? "unknown",
              messageID: props.tool?.messageID ?? "",
            });
          }
          break;
        }

        case "permission.replied": {
          const repliedProps = event.properties as any;
          const permID: string = repliedProps.requestID ?? "";
          const response: string = repliedProps.reply ?? "";
          if (response === "reject") {
            // Permission was denied — record it so checkin can surface it.
            const pending = pendingPermissions.get(permID);
            writeEvent("permission_denied", {
              tool: pending?.tool ?? "unknown",
              messageId: pending?.messageID ?? "",
            });
            pendingPermissions.delete(permID);
            // Mark that this was a manual denial (triggered by permission.asked
            // putting the session into the waiting state).  When the agent
            // subsequently goes idle, session.idle will write interrupted
            // instead of finished and will not notify the coordinator.
            manualDenial = true;
          } else {
            // Approved — clean up tracking entry.
            pendingPermissions.delete(permID);
          }
          // DB
          upsertAgentStatus(STATE_ACTIVE);
          writeStateChange(STATE_ACTIVE);
          break;
        }

        // question.asked fires when the agent uses the question tool to ask
        // the user something — treat identically to a permission wait.
        case "question.asked":
          // DB
          upsertAgentStatus(STATE_WAITING);
          writeStateChange(STATE_WAITING);
          break;

        case "question.replied":
        case "question.rejected":
          // DB
          upsertAgentStatus(STATE_ACTIVE);
          writeStateChange(STATE_ACTIVE);
          break;

        case "session.error": {
          // Read the error name from the event payload (may be absent).
          const errProps = event.properties as any;
          const errorName: string | null = errProps?.error?.name ?? null;
          if (errorName === "MessageAbortedError") {
            // User pressed Escape or Ctrl-C — record as interrupted, not error.
            // Cancel any pending idle debounce so it cannot fire "finished"
            // after this write — eliminates the race where session.idle starts
            // the timer before session.error fires.
            if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
            upsertAgentStatus(STATE_INTERRUPTED);
            writeStateChange(STATE_INTERRUPTED);
          } else {
            // All other error types (AuthError, APIError, OutputLengthError, …).
            upsertAgentStatus(STATE_ERROR);
            writeStateChange(STATE_ERROR);
            writeEvent("error", { name: errorName });
          }
          break;
        }

        case "session.compacted": {
          // Compaction done — agent returns to idle.
          compacting = false;
          // Cancel any pending idle debounce: compaction handles finished state
          // and coordinator notification directly below, so the timer would
          // double-fire upsert if allowed to run.
          if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
          // DB
          let prevStateCompact: string | null = null;
          if (db && sessionName && getStatus) {
            try {
              const row = getStatus.get(sessionName) as { state: string } | undefined;
              prevStateCompact = row?.state ?? null;
            } catch (e) { console.error("[prism-hooks] getStatus failed:", e); }
          }
          // If the session was already marked interrupted (pane-died hook
          // fired concurrently), leave the interrupted state intact and skip
          // coordinator notification.
          if (prevStateCompact === STATE_INTERRUPTED) {
            break;
          }
          await notify("set-finished");
          upsertAgentStatus(STATE_FINISHED);
          writeStateChange(STATE_FINISHED);
          writeEvent("compaction", { note: "compaction complete" });
          if (prevStateCompact === STATE_ACTIVE || prevStateCompact === STATE_COMPACTING) {
            notifyCoordinator(`Agent ${sessionName} context was compacted — check in to verify current state`);
          }
          break;
        }

        case "message.updated": {
          const info = event.properties.info as any;
          if (info.role === "user") {
            const text = textByMessageId.get(info.id) ?? "";
            // Skip if already written for this messageId.
            if (writtenMessageIds.has(info.id)) break;
            // Skip empty user messages — these are tool result messages sent by
            // opencode internally, or message.updated fires before text has
            // accumulated.  Real human messages always have text content.
            if (!text) break;
            // Extract agent and model from the message.updated event info.
            // For user messages: info.agent (string) and info.model.providerID + "/" + info.model.modelID.
            const agent: string = info.agent ?? "";
            const model: string = info.model
              ? `${info.model.providerID ?? ""}/${info.model.modelID ?? ""}`
              : "";
            writeEvent("msg_user", { messageId: info.id, text, agent, model });
            writtenMessageIds.add(info.id);
            textByMessageId.delete(info.id);
            // Persist agent and model into agent_status. For user messages,
            // also set root_agent_name/root_model_id if not yet present —
            // the first user message establishes the root agent context used
            // by bus delivery (fixes #404). Subsequent subagent messages
            // update agent_name/model_id but leave root fields intact.
            if (agent && model && db && sessionName && setRootAgentModelIfAbsent) {
              try {
                setRootAgentModelIfAbsent.run(agent, model, agent, model, sessionName);
              } catch (e) { console.error("[prism-hooks] setRootAgentModelIfAbsent failed:", e); }
            }
          } else if (info.role === "assistant" && info.time?.completed != null) {
            // Only write when the assistant message is fully complete to avoid
            // one row per streaming token.
            if (writtenMessageIds.has(info.id)) break;
            const text = textByMessageId.get(info.id) ?? "";
            // Skip messages with no text content — these occur when opencode
            // fires message.updated with completed set but no text parts have
            // accumulated (e.g. tool-only turns). Mirrors the msg_user guard.
            if (!text) break;
            // Extract agent and model from the message.updated event info.
            // For assistant messages: info.agent and info.providerID + "/" + info.modelID.
            const asst_agent: string = info.agent ?? "";
            const asst_model: string = (info.providerID && info.modelID)
              ? `${info.providerID}/${info.modelID}`
              : "";
            writeEvent("msg_assistant", { messageId: info.id, text, agent: asst_agent, model: asst_model });
            writtenMessageIds.add(info.id);
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
      return output;
    },
  };
};
