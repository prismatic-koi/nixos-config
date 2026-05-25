# Session lifecycle invariants

This document is a falsifiability surface for the user to mark against intent.
Each statement describes a user-observable behaviour of prism session lifecycle
as it currently exists. It is **not** a tutorial, design rationale, or rollout
plan — read it once, mark any line that does not match intent, and the marked
lines feed follow-up tickets. Statements are deliberately phrased so they can
be verified by running prism and observing, without reading code.

Unless a statement names a specific session kind or isolation mode, it applies
to all of them. The valid isolation modes referenced below are `bwrap`,
`sandbox-exec`, and `host`; there is no `podman` mode in the current code.

## Spawn

- `prism spawn` creates a git worktree at `<bare-root>/<branch>/`, a tmux session named `<repo>@<branch>`, an `agent_status` row, a `sessions` row keyed by `instance_id`, a per-session run directory at `$XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/`, and allocates one harness port from the configured range.
- The `instance_id` for a new session is minted host-side as a UUID before the tmux session and sidecar start, and is written to both `agent_status.instance_id` and `sessions.instance_id`.
- The initial `agent_status.state` written at spawn time is `idle`; it transitions to `active` only after the harness handshake or the first `turn_start`.
- When a non-empty `--prompt` is supplied, the prompt body is written to the per-session run directory and delivered to the agent by file path — via `PRISM_INITIAL_PROMPT_FILE=<path>` on the agent tmux pane for every isolation mode (`host`, `bwrap`, `sandbox-exec`), and additionally via `--prompt "$(cat <path>)"` inside the agent command for the 3-window host-mode spawn layout. The prompt body is never inlined into the tmux launch argv.
- `prism spawn` rejects an empty `--prompt` for agent-bearing layouts, and rejects any spawn whose computed host-side launch command would exceed the safe size bound — in both cases before any tmux state or DB side effects are created.
- Two concurrent `prism spawn` calls for the same session name in the same prism process serialise through an in-process lock; the second caller sees the live row and exits non-zero with `"is already alive ... concurrent spawn rejected"`.
- `prism spawn --branch <name>` without `--reuse` fails when an active session already exists on that branch; with `--reuse` it returns the existing session's name, port, and agent and exits 0.
- `prism spawn --wait` blocks until the spawned session's first turn reaches `finished`, `error`, `interrupted`, or `deleted`, with a default timeout of `10m`.
- `prism spawn` enforces a per-isolator concurrency cap for `bwrap` and `sandbox-exec` (compiled-in default `20` each; configurable via `bwrap_concurrency_cap` / `sandbox_exec_concurrency_cap` in `~/.config/prism/config.json`) and refuses to start a new session over the cap unless `--ignore-concurrency-cap` is passed; `host` mode is uncapped.
- Spawning onto `main` in a bare+worktree repo (where `<repo>/main/` already exists as a worktree) fails with a git error rather than creating a second worktree on the same branch.
- The default isolation mode for a new session is the machine-level `default_isolation_mode` from `~/.config/prism/config.json` (default `host`), optionally overridden per-path by `project_isolation_overrides`.

## State transitions

- The canonical agent states are `idle`, `active`, `waiting`, `compacting`, `reviewing`, `escalated`, `finished`, `interrupted`, `error`, and `deleted`.
- A session enters `active` from `idle` on the first `turn_start` frame, on `session.created` from the SSE stream, or on the first `server.heartbeat` when neither has fired yet.
- A session enters `waiting` on `permission.asked` or `question.asked`; it returns to `active` on `permission.replied` or on `question.replied`/`question.rejected`.
- A session enters `compacting` on a `session.updated` with `time.compacting` and returns to `active` on `session.compacted`; the compacting transition does not notify the coordinator.
- A session enters `reviewing` when the worker calls `prism review` (written before any review agents spawn). It leaves `reviewing` for `active` when the review-complete prompt is delivered with `source=review-complete`, and for `finished` when the worker produces a clean turn after that delivery.
- A session enters `escalated` when the worker calls `prism escalate`; any incoming `turn_start` from any source transitions it back to `active`.
- A session enters `finished` after a 2-second idle debounce that begins on `session.idle` (SSE path) or `state_change{finished}` (PI path), or immediately after a `session_shutdown` frame.
- A session enters `error` on `session.status{retry}`, on `session.error` with any name other than `MessageAbortedError`, or when the inactivity watchdog fires after `cfg.ActivityTimeout` of no inbound frames.
- A session enters `interrupted` on `session.error` with name `MessageAbortedError`, on a manual permission denial when the next idle debounce fires, or on a pane-died tmux hook with a non-zero exit (even from a previously-`finished` state).
- A session enters `deleted` on `session.deleted`; `deleted` is terminal with no outgoing transitions.
- A session can resume from `finished`, `interrupted`, or `error` back to `active` via `session.updated` (clearing `ended_at`), but a `session.updated` arriving within `ErrorResumeDebounce` of a `session.error` does not transition state — it only refreshes metadata.
- A session in `escalated` does **not** emit the `Agent <name> has finished its current task` notification when its turn ends; the `session.escalated` bus event has already informed the coordinator.
- A session in `reviewing` does **not** emit the `has finished` notification on `session.idle` / `state_change{finished}`, and suppresses the `turn_start`-driven `active` write so that incidental assistant turns do not clobber the `reviewing` state.

## `prism prompt` and the waiting guard

- `prism prompt <session> --prompt <text>` refuses to deliver when the target session is in `waiting` and exits non-zero with a message instructing the caller to switch to the session and respond directly.
- `prism prompt` against a session whose `ended_at` is non-NULL returns `"session ... has ended — escalate to user to restart if needed"`; against an unknown session it enumerates up to 10 active session names in the error.
- `prism prompt` delivery is idempotent on `delivery_id`: a repeat with the same UUID is dropped before reaching the harness pipe and the response carries `"replayed": true`.
- `prism prompt --deliver-as` accepts `steer`, `followUp`, or `nextTurn`; the CLI validates the value before any network call.

## Persistence & cleanup

- `prism cleanup --yes --session <name>` for a feature-branch session removes the worktree at `<bare-root>/<branch>/`, force-deletes the local branch, kills the tmux session, and redirects any attached clients to `scratchpad`.
- `prism cleanup --yes --session <name>` for a coordinator session (one whose `root_agent_name = 'coordinator'`, or whose branch matches the default branch when `root_agent_name` is NULL) kills the tmux session and marks the DB row ended, but **does not** remove the worktree or delete the branch.
- `prism cleanup --yes --session <name>` for a non-worktree session (no `@` in the name) kills the tmux session and marks the DB row ended; no git operations run.
- `prism cleanup --yes` always force-deletes the feature branch and does not check `git branch --merged` against `main`, because squash-merges produce a different SHA on `main` than the branch tip.
- `prism cleanup` invoked inside a sandbox (where `PRISM_HOST_API` is set) proxies the cleanup request to the host sidecar; per-resource progress lines are forwarded verbatim.
- `prism cleanup` releases the session's allocated `harness_port`, sets `agent_status.ended_at`, sets `sessions.ended_at` and `sessions.end_state = "finished"`, writes a `spawn_outcomes` row, purges queued bus messages for the session, and removes the per-session host-API socket, the `agent-run.log`, and their per-session run directory under `$XDG_STATE_HOME/prism/run/<sessionDirHash>/`.
- `prism cleanup` for `sandbox-exec` sessions also removes the per-instance staging `HOME` directory at `$XDG_STATE_HOME/prism/sessions/<instance_id>/home/`. `host` and `bwrap` modes have no staging-home equivalent.
- `prism cleanup` for a worker stops and removes every child review-agent container (30-second per-child budget) and then kills every `<worker>~review-N-<role>` session and releases each child's port and DB row.
- `prism cleanup` does **not** clean up investigator child sessions of the cleaned-up session — investigators must be cleaned up by their invoker with a separate `prism cleanup --yes --session <inv-session>` call.
- A session whose tmux process died without prism cleanup (a "zombie") leaves the `agent_status` row, the allocated port, and the worktree behind; `prism cleanup --yes --session <name>` is the supported recovery and removes them.
- The `agent_status` and `sessions` DB rows themselves are **not** deleted by cleanup — only their `ended_at` / `end_state` fields are updated; the rows remain queryable as audit/history.
- The OAuth token store, the user's profile config (`~/.config/prism/profiles.json`), and the `feedback.jsonl` log all live outside the per-session directory and survive cleanup unchanged.
- Branch deletion is best-effort — a failure deleting the branch does not abort cleanup; the worktree-removal and sidecar/tmux teardown still complete.
- `prism cleanup --yes --json` emits a single JSON object with `session`, `worktree_removed`, `branch_deleted`, and `session_killed` fields on stdout; per-step textual progress is suppressed and warnings still go to stderr.

## Archive

- `prism cleanup` archives the harness session storage into `~/.local/share/prism/archive/<repo>/<startedAtISO>_<instanceID>/` containing `raw/`, `manifest.json`, and (when present) `agent-run.log`.
- The archive copy is performed under a `.tmp-<instanceID>/` directory and renamed atomically on success; a partial archive is removed on any error. Archive directories are mode `0700`, files mode `0600`.
- When the final archive directory already exists, the archive step returns `archive.ErrAlreadyExists`; cleanup logs a warning and continues without re-archiving — worktree, branch, tmux, and DB teardown still complete.
- For `pi` harness sessions, `raw/session.jsonl` is the full PI JSONL transcript and `session.jsonl` (a near-identity export) sits next to it; sessions where PI never wrote any data archive an empty `raw/` directory and no `session.jsonl`.
- The PI JSONL transcript copied into the archive is **also retained** on disk after `prism cleanup` for `host` and `bwrap` sessions (the source lives under `~/.pi/agent/sessions/<encoded-cwd>/` which is not owned by prism); for `sandbox-exec` sessions the original transcript is removed when the per-instance staging `HOME` is deleted, and only the archive copy remains.
- `prism cleanup` writes the archive directory path to `sessions.archive_path` for the incarnation's row.
- `prism archive <instance-id|session-name>` prints the recorded `archive_path` and exits 0 when present; it exits non-zero when the row is unknown or `archive_path IS NULL`. `prism archive <session-name> --all --json` emits a JSON array of every archived incarnation for the name, newest first.

## Resume

- For PI sessions, `prism agent-run` appends `--session <HarnessSessionID>` to the `pi` invocation when the on-disk session JSONL exists under the mode-aware sessions root; pi then reopens the prior conversation.
- When the on-disk PI session JSONL for a known `HarnessSessionID` is missing, the agent starts a fresh conversation and a tagged warning is appended to the per-session `agent-run.log` — the spawn does not fail.
- The PI sessions root differs by isolation mode: `host` and `bwrap` both resolve to `~/.pi/agent/sessions/` (for `bwrap`, the directory is overlay-mounted into the sandbox at `$PI_CODING_AGENT_DIR/sessions/`); `sandbox-exec` resolves to `<staging-home>/.pi/agent/sessions/`. For `host` and `bwrap`, the `<encoded-cwd>` directory is shared across any prism session whose worktree matches the cwd; for `sandbox-exec`, the JSONL lives under the per-instance staging `HOME` and is not shared.
- A `session.updated` frame for a session whose current state is `finished` or `interrupted` clears `ended_at` and transitions the session back to `active`. Re-creating the tmux session for a previously-terminal session via `tmux-session-start` resets `agent_status.state` to `idle`.
- Once `prism cleanup` has run, the worktree, branch, tmux session, and port are gone and cannot be recovered; only the archive at `~/.local/share/prism/archive/...` remains.

## Session kinds

- A **coordinator** is identified by `root_agent_name = 'coordinator'`, with a fallback name heuristic of `<repo>@main` for pre-migration rows. Coordinators receive `has finished`/`has errored` notifications from workers in the same repo and own the merge-queue watcher for their session lifetime.
- A coordinator does **not** receive a `has finished` notification for its own session ending — the self-notification guard suppresses it. A coordinator cannot run `prism review` directly; the review verb is for worker / spawned sessions.
- Cleaning up a coordinator session kills the tmux session, ends the DB row, and writes the archive, but **keeps the worktree and branch intact**.
- A **worker** is any non-coordinator session created by `prism spawn` (typically with `root_agent_name = 'worker'`). Workers may run `prism review` and `prism escalate`; `prism investigate` and `prism merge` are both denied in their bash deny list.
- A worker's terminal `finished` / `error` transition delivers a body-bearing notification to the same-repo coordinator with text `Agent <name> has finished its current task` or `Agent <name> has errored its current task`, using delivery mode `followUp`. This notification is suppressed while the worker is in `escalated` or `reviewing`.
- An **investigator** is a session created by `prism investigate`, named `<invoker>~investigate-<slug>`, spawned with `WorktreeReadOnly: true` on the 2-window `LayoutAgentOnly` layout.
- An investigator does **not** emit the `has finished` notification to the coordinator; on terminal state (`finished`, `interrupted`, `error`) it delivers a body-bearing notification to its invoker carrying the last completed turn's text, prefixed with `From investigator session: <name>` and suffixed with `Reply with: prism prompt <name> --prompt '...'`. The notification is dropped silently if the invoker has ended.
- An investigator does not self-terminate; the invoker is responsible for cleaning it up with `prism cleanup --yes --session <inv-session>`. `prism cleanup` of the invoker does **not** transitively clean up its investigator children.
- A **review agent** is a session named `<parent>~review-<round>-<role>`, created by the worker's `prism review` invocation, spawned with `WorktreeReadOnly: true` on the 2-window `LayoutAgentOnly` layout, and does not run the `tmux-session-start` root-agent seeding hook.
- A review agent's terminal-state transition does **not** notify the parent worker's coordinator and does **not** notify the parent worker directly — the parent worker's `pollAgents` DB loop and the review monitor pick up the result.
- A review agent's **startup failure** (container readiness timeout, inactivity timeout, or sidecar startup error) is the one case where a review-agent session notifies its parent worker; the body is `review agent <name> failed to start: <err>`.
- A review agent is cleaned up by `prism cleanup` of its parent worker session — its child container, port, DB row, and tmux session are removed as part of the parent's cleanup.

## Merge queue

- `prism merge <pr>` is denied in the bash deny list for every non-coordinator role; only coordinators may enqueue.
- The merge-queue watcher is a goroutine started by the coordinator's sidecar at init; its lifetime equals the coordinator session's lifetime — there is no persistent daemon across coordinator sessions. It polls the queue head every 45 seconds and processes one PR at a time.
- Enqueued merge-queue rows carry the coordinator's `instance_id`. On coordinator-session shutdown, every `watching` row for that `instance_id` is transitioned to `abandoned` with `error = 'coordinator session ended'`. `abandoned` rows do not produce a live notification and surface only via `prism merges list --abandoned`.
- A new coordinator incarnation for the same session name starts with an empty `watching` view and can re-enqueue any abandoned PR with `prism merge <pr>`.
- Each terminal outcome (`merged`, `failed`, `cancelled`, `abandoned`) delivers a single bus notification to the coordinator session; the `merged` text includes the worker's archive path when known.
- `prism merges list` without flags shows only the rows for the current coordinator's `instance_id`; prior-incarnation rows are not shown unless `--abandoned` or `--all` is passed.
- `prism merge <pr>` is idempotent on `(pr_number, instance_id)` for non-terminal states; a repeat enqueue on a `watching` row returns without inserting a duplicate.

## Notifications

- The worker → coordinator `has finished`/`has errored` notification is delivered as a `prompt_async`-shaped frame via the coordinator's host-API Unix socket at `$XDG_STATE_HOME/prism/run/<sessionDirHash>/hostapi.sock`, with delivery mode `followUp`.
- Each notification carries a `delivery_id` (UUID). The receiving sidecar dedups repeats by ID before they reach the harness pipe; a repeated delivery receives `{"replayed": true}` in the HTTP response. The dedup set is in-memory per sidecar (LRU, capacity 256) and does not survive a sidecar restart.
- A notification that arrives while the recipient's PI extension is disconnected is buffered (capacity 16, FIFO drop) and flushed on the next successful handshake; replayed entries carry `replay: true` on the prompt frame.
- The `notifyCoordinator` path writes a `bus_messages` audit row on success and on failure, and is suppressed for the coordinator itself, for review-agent sessions, for investigator sessions, and for any session currently in `escalated`.
- `prism escalate` emits a `session.escalated` bus event (distinct from `session.finished`) and writes the same payload to the calling session's own event log as type `escalation`, with fields `source`, `target`, `prompt`, `pr_numbers`, `branch`, `head_sha`, `verdicts`, and `occurred_at`.
- `prism escalate` without `--to` requires exactly one same-repo coordinator candidate; with multiple candidates it exits non-zero with the list and **no state transition**. With `--to <unknown>` it exits non-zero with **no state transition**. With zero candidates it still transitions to `escalated` and writes `"no coordinator found, please wait for a human to come check on you"` into the worker's own log.
- A `session_shutdown` PI frame writes `StateFinished` directly (no idle debounce) and calls `notifyCoordinator` exactly once.
- The inactivity watchdog (`cfg.ActivityTimeout > 0`) transitions an otherwise-non-terminal session to `error` with `note="inactivity timeout"` after the configured window of zero inbound frames; for review-agent sessions it additionally fires the parent-worker startup-failure notification.

## Concurrency

- No two active sessions may share an `agent_status.harness_port` — the partial-unique index on `(harness_port) WHERE harness_port IS NOT NULL` enforces this at the DB level.
- No two active sessions may share a tmux session name; the in-process spawn lock serialises same-name races, and tmux rejects a second `new-session -s <name>` against a live name.
- Two sessions on the same branch are rejected by the git worktree machinery: only one worktree may be checked out for a branch at a time.
- The per-session run directory under `$XDG_STATE_HOME/prism/run/<sessionDirHash>/` (12-hex SHA-256 prefix of the session name) is unique per session; the host-API socket, the harness pipe socket, and the `agent-run.log` are not shared between sessions.
- The merge queue is serial: only one PR is in flight at a time per coordinator, regardless of how many `prism merge` invocations have been made.
- `prism review` is gated behind a one-shot `git fetch origin main` + strict `git merge-base --is-ancestor origin/main HEAD` check before any review agents are spawned; on refusal no agents spawn and the cycle counter does not increment. `prism review --rebase` performs fetch + rebase + force-push inline and on conflict aborts the rebase and exits non-zero, never leaving the worktree mid-rebase.

## Transports / isolation modes

- The valid isolation modes are `bwrap` (Linux only), `sandbox-exec` (Darwin only), and `host` (no isolation). There is no `podman` mode in the current code.
- The per-session run directory layout, the host-API socket path, the harness pipe socket path, and the `agent-run.log` path are **the same across all isolation modes** — only the access path to those files from inside the sandbox differs. For `bwrap` and `sandbox-exec`, the per-session run directory is bind-mounted (or SBPL-subpath-allowed) into the sandbox so the agent reaches the sockets at the same host path used outside.
- For `host` sessions, the agent runs in the tmux pane with full host filesystem access; no sandbox setup is performed.
- PI session storage location differs by mode: `host` and `bwrap` both write to the conventional `~/.pi/agent/sessions/` (for `bwrap`, via an overlay mount into the sandbox), shared across any PI invocation on the same `cwd` and **not owned by prism** — survives `prism cleanup`; `sandbox-exec` writes to the per-instance staging `HOME` and is removed by `prism cleanup`.
- The notification-delivery contract (exactly-once on `delivery_id`, `followUp` mode, dedup capacity 256, replay buffer capacity 16) is **identical across all isolation modes**.
- `prism cleanup`'s sidecar-teardown, port-release, DB-end, archive, and bus-message-purge steps are **identical across all isolation modes**; only the isolator-specific `EnsureRemoved` step differs (and is a no-op for `host`).
- The concurrency cap is per-isolator: `bwrap` and `sandbox-exec` each enforce their own cap independently via `Isolator.Cap`; `host` is uncapped (`Isolator.Cap` returns `Limit: 0`).
- A coordinator running inside a sandbox proxies CLI calls (`prism cleanup`, `prism logs`, `prism merges`, etc.) through `PRISM_HOST_API` to the host sidecar; the host-side prism DB and merge queue remain the source of truth regardless of which side the CLI is invoked from.
