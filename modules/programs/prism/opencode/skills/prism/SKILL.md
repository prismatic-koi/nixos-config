---
name: prism
description: Spawn isolated agent sessions in their own git worktrees using the prism tool. Use when the user asks to spawn an agent, delegate work to another session, run something in parallel, or work on a PR or different repo.
---

> **Note:** The prism source code and this skill file live in the `nixos-config` repository under `modules/programs/prism/`. Changes to prism itself — the Go CLI, tmux config, opencode agents, and skills — are made there.

# Spawning Agents with prism

`prism spawn` creates a new git worktree, starts a tmux session in it, and launches opencode. Use it when work should be isolated, long-running, or in a different repo — rather than a subagent, which shares the current session context.

## When to use prism spawn vs a subagent

| Situation | Use |
|---|---|
| User says "spawn an agent" or "use prism" | `prism spawn` |
| Work is on a PR branch | `prism spawn --pr` or `prism pr` |
| Task is long-running / should outlive this session | `prism spawn` |
| Quick research or analysis within this repo | subagent (`@explore`, `@general`) |

## Converting a repo to bare+worktree layout

```bash
# Convert the current directory
prism convert

# Convert a specific repo
prism convert ~/code/nixos-config
```

This converts a regular git clone to the prism bare+worktree layout in-place. The working tree moves to `<repo>/<branch>/`, a bare clone is created at `<repo>/.bare`, and the index is populated immediately so `git status` is clean. This is the same operation as selecting `[convert to bare+worktree layout]` in the C-f picker.

## Basic usage

```bash
# Spawn in the current repo with a timestamped branch
prism spawn --prompt "go implement feature X and open a PR"

# Spawn on a named branch — use a short descriptive kebab-case name, not an issue number
prism spawn --branch update-plex-image --prompt "..."

# To work in a different repo, delegate via its coordinator (see "Delegating work to another repo")
prism prompt home-ops@main --prompt 'update the plex image to the latest tag and open a PR'

# Check out a PR branch and spawn a session on it (--repo is supported on prism pr)
prism pr 268 --prompt "review this PR"
prism pr 268 --repo nixos-config --prompt "review this PR"
```

## Passing prompts safely — shell escaping

The `--prompt` value passes through the caller's shell before prism receives it.
Shell metacharacters such as backticks, `$()`, `$VAR`, and double-quote contents
are interpreted by the shell and silently corrupted if you are not careful.

**Preferred approaches (safest first):**

1. **`--prompt-file <path>`** — Write the prompt to a temp file; the shell never
   touches its contents:
   ```bash
   printf 'run `gh pr view 42` and review the diff' > /tmp/prompt.txt
   prism spawn --prompt-file /tmp/prompt.txt
   ```

2. **`--prompt -` (read from stdin)** — The literal value `-` is a reserved
   sentinel that tells prism to read the prompt from stdin. Pipe or heredoc the
   prompt in. Use a quoted heredoc delimiter (`<<'EOF'`) to prevent expansion
   inside the body:
   ```bash
   prism spawn --prompt - <<'EOF'
   run `gh pr view 42` and review the diff
   EOF
   ```
   **Note:** because `-` is reserved, you cannot pass the literal string `-` as
   a prompt via `--prompt`. Use `--prompt-file` or single quotes with a
   different phrasing if your prompt content is literally a dash.

3. **Single quotes** — Wrap the value in single quotes. Single quotes prevent
   *all* shell interpolation in bash/zsh:
   ```bash
   prism spawn --prompt 'run `gh pr view 42` and review the diff'
   ```
   Single quotes cannot contain a literal `'`. For prompts with apostrophes,
   prefer option 1 or 2 instead.

**Do not** use double quotes around prompts containing backticks or `$`:
```bash
# BAD — the shell executes `gh pr view 42` and splices its output in
prism spawn --prompt "run `gh pr view 42` and summarise"
```

## All flags

| Flag | Description |
|---|---|
| `--branch <name>` | Branch name for the new worktree. Defaults to a timestamp. Use a short, descriptive kebab-case name derived from the task (e.g. `update-plex-image`, `fix-login-redirect`) — never an issue number, PR number, or Jira ID. The branch name becomes the session name (e.g. `nixos-config@update-plex-image`), so it should be immediately readable in `prism list-sessions` and the tmux picker without looking anything up. |
| `--pr <number>` | Fetch and check out the branch for this PR number. |
| `--prompt <text>` | Instruction passed to opencode on launch. Wrap values containing shell metacharacters in **single quotes**. The value `-` is reserved and reads from stdin (cannot pass a literal `-`). |
| `--prompt-file <path>` | Read the prompt from a file instead of passing it as an argument. Mutually exclusive with `--prompt`. A single trailing newline is stripped. |
| `--agent <name>` | Opencode agent to use (`worker` or `plan`). Defaults to `worker`. |
| `--attach` | Switch the current tmux client to the new session instead of spawning headlessly. |

## Behaviour

- **Headless by default**: the session is created in the background. The user can switch to it via the prism dashboard (`C-w`) or picker (`C-f`).
- **`--attach`**: use when the user wants to be taken directly to the new session.
- The prompt is sent to opencode automatically after startup — no manual input needed.
- Each spawned session gets its own worktree, so changes are isolated from other branches.

## Delegating work to another repo

When you need to delegate work to a repo you are not the coordinator for, route it through that repo's `@main` coordinator session. The coordinator has full context about that repo's conventions, open work, and branch state.

**Flow:**

1. Run `prism list-sessions` and look for `<repo>@main`.
2. **Found, not in `waiting` state:** send the work request with `prism prompt <repo>@main --prompt '...'`.
3. **Found, in `waiting` state:** escalate to the user — the coordinator is blocked and expecting human input. The user needs to switch to that session and unblock it directly. Do not attempt to work around the waiting state guard.
4. **Not found:** there is no coordinator to delegate to. Escalate to the user and ask them to start a `<repo>@main` coordinator session. Note: you also cannot work around this by spawning onto `main` yourself — in the bare+worktree layout prism uses, `main` already has a worktree, so `prism spawn --branch main` will fail with a git error.

Spawning directly into a feature branch in another repo (bypassing the coordinator) should only happen when you **are** the coordinator for that repo, or when the user explicitly instructs you to.

```bash
# Check if the target repo has a coordinator session
prism list-sessions

# If home-ops@main exists and is not waiting:
prism prompt home-ops@main --prompt 'Please update the plex image to the latest tag and open a PR'

# If home-ops@main exists but IS in waiting state:
# escalate to the user — they need to switch to that session and unblock it

# If home-ops@main does not exist:
# escalate to the user — ask them to start a coordinator session for home-ops
```

### Example: spawning directly as coordinator

If you **are** the coordinator for the target repo (or the user has explicitly instructed you to spawn directly), use `prism spawn`:

```bash
prism spawn \
  --branch update-plex-image \
  --prompt "find the plex container image in this repo and update it to the latest tag from dockerhub, then open a PR"
```

## Running code review

Code review is done with `prism review <pr>`, which is **async**: it spawns 5
review agents, registers a group, and returns immediately with an
acknowledgement. Results are delivered to you via a follow-up `prism prompt`
when all agents complete.

```bash
prism review <pr-number>
```

**Do NOT commit, merge, or announce completion** until the review-complete
prompt arrives. When it does, handle PASS/FAIL per the worker agent instructions.

If no review-complete prompt arrives within 30 minutes, check progress with:

```bash
prism checkin <session>~review-<N>-review-goal
```

### Fallback: Task-call subagents

If `prism review` is unavailable, invoke the five subagents **in parallel** —
all five as Task tool calls in a single response:

1. `@review-goal` — pass the original issue/ACs and the PR number
2. `@review-code` — pass the PR number
3. `@review-security` — pass the PR number
4. `@review-qa` — pass the PR number
5. `@review-context` — pass the PR number

Wait for all 5 to complete. ALL must return `<verdict>PASS</verdict>` for the
review to pass. If ANY returns `<verdict>FAIL</verdict>`, fix all blocking
issues, push, and re-run all five. After 3 full cycles without convergence,
stop and escalate — do not run a 4th cycle.

## Merge queue (coordinators only)

> **Coordinators only.** Worker agents, container worker agents, bwrap worker agents, and review agents all have `prism merge` and `prism merge *` denied in their bash deny lists. If you are not a coordinator agent, skip this section.

The merge queue is a local serial FIFO queue running in the coordinator's sidecar process. The sidecar polls the head of the queue every 45 seconds; only one PR is in flight at a time. The watcher's lifetime equals the coordinator session's lifetime — there is no persistent daemon.

A queued PR moves through states keyed off GitHub's `mergeStateStatus`: `watching` → `merged` / `failed` / `cancelled` / `abandoned`. See issue #783 for the full state machine.

### Command surface

| Command | Description |
|---|---|
| `prism merge <pr>` | Enqueue a PR. Returns within ~2 seconds. Idempotent — safe to call on an already-queued PR. |
| `prism merges` | Show the queue scoped to the current coordinator session (alias for `prism merges list`). |
| `prism merges list` | Same as above. |
| `prism merges list --failed` | Show only failed entries. |
| `prism merges list --abandoned` | Show entries left behind by a previous coordinator incarnation. |
| `prism merges list --all` | Include terminal-state history (last 7 days). |
| `prism merges cancel <pr>` | Remove a `watching` entry from the queue. |

### Notification contract

When a queued PR reaches a terminal outcome, the watcher delivers a bus notification to the coordinator session. The text is:

| Outcome | Notification text |
|---|---|
| Merged (with archive path) | ``PR #N merged. Archive: <archive_path>. Run `git pull` in @main and `prism cleanup` the worker session.`` |
| Merged (no archive path yet) | ``PR #N merged. Run `git pull` in @main and `prism cleanup` the worker session.`` |
| Merge conflicts | `PR #N has merge conflicts — worker rebase needed` |
| CI failure | `PR #N CI failed — needs worker fix` |
| Closed without merging | `PR #N was closed without merging — removed from queue` |
| Other merge failure | `PR #N merge failed: <error>` |
| Coordinator session ended while watching | Row transitions to `abandoned` — surfaces via `prism merges list --abandoned` only; no live notification. |

### Action table

When a merge-queue notification arrives, treat it as high-priority (same as a worker finished-notification):

| Notification | Action |
|---|---|
| `merged` | `git pull` in @main, then `prism cleanup --yes --session <worker-session>` |
| `merge conflicts` | `prism prompt <worker-session>` asking it to rebase onto main and push; re-enqueue with `prism merge <pr>` after the worker finishes |
| `CI failed` | `prism prompt <worker-session>` asking it to investigate the failed check, fix, and push; re-enqueue with `prism merge <pr>` |
| `closed without merging` | Usually nothing — the PR was closed deliberately. Investigate if unexpected. |
| `merge failed: <error>` | Read the error, decide whether to retry (`prism merge <pr>`) or escalate to the user. |
| `abandoned` (via `--abandoned` listing) | A new coordinator decides whether to re-enqueue with `prism merge <pr>`. |

### Why workers cannot invoke it

Worker agents, container worker agents, bwrap worker agents, and review agents all have `prism merge` and `prism merge *` in their bash deny lists. Only coordinator agents have it allowed. This is by security design: only coordinators arbitrate merge order. Do not attempt to work around the deny list.

## Example: reviewing a PR (manual spawn)

```bash
# PR in the current repo
prism pr 268 --prompt "review this PR and summarise the changes"

# PR in a different repo (--repo is supported on prism pr)
prism pr 268 --repo nixos-config --prompt "review this PR and summarise the changes"
```

## Example: create a ticket then spawn an agent to action it

When the user asks you to create a ticket (e.g. Jira) and then spawn an agent to work on it:

1. Create the ticket using the appropriate MCP tool and capture the ticket ID (e.g. `PROJ-123`)
2. Derive a short, descriptive kebab-case branch name from the ticket title — not the ticket ID
3. Spawn the agent — pass the ticket ID in the prompt so the agent can look it up; the Atlassian MCP is available in the spawned session

```bash
# After creating ticket PROJ-123 ("Update plex image to latest tag"):
prism spawn \
  --branch update-plex-image \
  --prompt "Please take a look at PROJ-123, cover off the work required, and open a pull request."
```

## Lifecycle: cleaning up after a merge

When you spawn a session and later merge its PR yourself, you are responsible for cleaning up the worktree and session. The spawned agent cannot do this — it would be tearing down its own environment.

`prism spawn` prints the session name when running headlessly:

```
session "nixos-config@update-plex" created
```

Note down the session name from that output. Once you have merged the PR, clean up:

```bash
prism cleanup --yes --session "nixos-config@update-plex"
```

`prism cleanup --yes --session <name>` will:
- Remove the git worktree
- Delete the branch if it is already merged (skips deletion if not, safe default)
- Kill the tmux session, redirecting any attached client to `scratchpad`

Only call this after you have confirmed the PR is merged. If the branch is not yet merged, branch deletion is skipped automatically.

## Checking in on a running session

Use `prism list-sessions` to see all active agent sessions with their state and current task title:

```bash
prism list-sessions
```

Use `prism checkin <session>` to read the recent conversation history for a session, sourced from the prism DB. The default output is a rich narrative view: assistant messages, state changes, and tool call one-liners interleaved chronologically.

```bash
prism checkin nixos-config@update-plex
```

```
checkin: nixos-config@update-plex

state: finished

[18:48:35] assistant
I'll fix both issues from the review.
  → edit: container.go ✓
  → edit: sidecar.go ✓
  → bash: go build ./... ✓
  → bash: go test ./internal/... ok (9.2s)

[18:49:16] assistant
Tests pass. Committing and pushing.
  → bash: git commit -m "fix: capture elapsed..." ✓
  → bash: git push ✓

[18:50:25] ● finished

── end of event log ──
```

**Tool one-liner format**: `→ <tool>: <key_arg> <result_summary>`

- **bash** — key arg is the command (first ~80 chars); result is first meaningful output line, `✓` for empty stdout, `✗ <message>` on error
- **read** — key arg is the file path; result is `✓ (N lines)`
- **edit / write** — key arg is the file path; result is `✓` or `✗`
- **task** — key arg is the description; result is `✓` or `✗`
- **glob / grep** — key arg is the pattern; result is `N matches` or `no matches`
- **todowrite** — result is `✓` (key arg omitted)

**State changes** appear inline as `● <state>` (e.g. `● finished`) with a timestamp.

**User messages** (`▶ user`) are visually distinct from assistant turns so coordinator prompts and bus notifications are easy to spot.

With no argument, `prism checkin` lists available sessions and exits with a hint.

See [Debugging a running or stuck session](#debugging-a-running-or-stuck-session) for `prism checkin` flag reference and a full decision tree for diagnosing issues.

## Sending a follow-up prompt to a running session

Use `prism prompt <session> --prompt <text>` to send a follow-up message to the opencode agent in a session that is already running (or has finished and is waiting for input):

```bash
# Simple prompt — single quotes prevent shell interpolation
prism prompt nixos-config@update-plex --prompt 'looks good, go ahead and open a PR'

# Prompt with backticks or complex content — use --prompt-file
printf 'run `make test` and fix any failures' > /tmp/p.txt
prism prompt nixos-config@update-plex --prompt-file /tmp/p.txt

# Or via stdin
prism prompt nixos-config@update-plex --prompt - <<'EOF'
run `make test` and fix any failures
EOF
```

The same shell-escaping conventions that apply to `prism spawn` apply here —
see [Passing prompts safely](#passing-prompts-safely--shell-escaping) above.

### `prism prompt` flags

| Flag | Description |
|---|---|
| `--prompt <text>` | Prompt text to send. Supports `-` to read from stdin. |
| `--prompt-file <path>` | Read prompt from a file. Mutually exclusive with `--prompt`. |

The prompt is delivered directly via HTTP to the opencode session. The session must exist and have an active opencode port — use `prism list-sessions` to check first.

### Waiting state guard

`prism prompt` will **refuse** to send a prompt if the target session is in `waiting` state. A `waiting` agent has paused and is expecting direct input from the user — injecting a programmatic prompt would corrupt the input field.

If you encounter this error, **escalate to the user**. Do not attempt to work around the guard. The user must switch to the session themselves (via `C-f` or `C-w`) and respond directly.

```
session "nixos-config@update-plex" is waiting for user input

The agent has paused and is expecting a direct response from the user.
Please switch to that session and respond there, or escalate to the user
so they can address it directly.

  prism checkin nixos-config@update-plex   — inspect the current state
  (C-f or C-w)                             — switch to the session in tmux
```

## Debugging a running or stuck session

Use this decision tree when a session appears stuck, produces no output, or fails unexpectedly.

**Step 1 — Session state check:**

```bash
prism list-sessions
```

Examine the `state` column (`active`, `waiting`, `idle`, `finished`, `error`), the port, and the `last_seen` timestamp. If a session has a DB row but no live tmux session, it may be a zombie (DB row without a live process). Proceed to step 2.

**Step 2 — Recent activity:**

```bash
prism checkin <session>
```

Reads the last 10 turns from the prism DB as a rich narrative view. Use `--verbose` for full tool args/results when something looks off. See [`prism checkin` flags](#prism-checkin-flags) below for all options.

**Step 3 — Sidecar logs:**

```bash
prism logs <session>
```

The raw sidecar log is where container startup errors, timing traces, and stderr from failed `podman run` commands land. This is the most informative diagnostic for infrastructure failures. See [`prism logs`](#prism-logs) below for full flag documentation.

**Step 4 — Source cross-reference:**

When a log or event message includes a `file:line` reference, read that location directly in the Go source under `modules/programs/prism/prism/` to understand the exact code path.

### `prism checkin` flags

| Flag | Description |
|---|---|
| `--last <N>` | Number of message turns to show (default 10) |
| `--from <id>` | Show N events forward from this event ID |
| `--before <id>` | Show N events backward from this event ID |
| `--verbose` / `-v` | Full forensic output: full tool args and full results with no truncation |
| `--types <list>` | Orthogonal event-type filter — routes to the raw-event path (e.g. `--types audit`, `--types state_change`). Not needed for the narrative view; tool calls are included by default. |
| `--all` | (no-arg mode only) List all sessions across all repos |

### `prism logs`

The `prism logs` command streams the raw sidecar log for a session to stdout.

```bash
prism logs <session>              # full sidecar log to stdout
prism logs <session> --tail N     # last N lines only
prism logs <session> --follow     # stream new lines as they arrive (ends ~5s after terminal state)
prism logs <session> -f           # alias for --follow
```

Works identically from a host shell and from inside a coordinator container. When `PRISM_HOST_API` is set (container mode), `prism logs` proxies through the host-API Unix socket — no special handling required. The output is the raw log and can be piped to `grep` / `rg`.

### Common failure signatures

A lookup table of log patterns, their causes, and remediation hints:

- **`statfs <path>: no such file or directory`** — podman was told to bind-mount a path that does not exist on the host. Common cause: a container-internal path (e.g. `/workspace`) leaked into a host-side `podman run` invocation. Check the preceding log line for the full `podman run` command. See incident #751 for a historical example.

- **`exit status 125`** — the `podman container create` (or `podman run`) command failed at the OS level. The actual cause is on the preceding line(s) in the log — read upward from this line to find it.

- **`container did not become ready within 120s`** — the container started but the sidecar never reached the ready state. Usual causes: a misconfigured `opencode.json` (agent not declared, malformed JSON), a missing bind-mount, or a missing `--agent` flag value. Check the sidecar log for the `podman run` command line and any JSON parse errors.

- **Session rows present in `prism list-sessions` but no events in `prism checkin`** — the container either never started or died immediately after creation. Run `prism logs <session>` to see the full podman command line and its stderr output.

- **Session name doesn't match expected shape** (e.g. `~review` where `~review-1-review-code` is expected) — the agent-list construction produced the wrong agent names, or the `--agent` flag value passed to opencode is incorrect. Check the container's `opencode.json` for the `agent` block contents and the sidecar log for the `--agent` flag value used in the command line.

- **Zombie DB rows (session in `prism list-sessions` but no live tmux session)** — a previous session's process died without cleaning up DB state. Use `prism cleanup --yes --session <name>` to remove the stale row and any dangling port allocation.

### Escalation

If two diagnostic cycles (`prism checkin` + `prism logs`) do not clarify the issue, **escalate to the user** rather than continuing to probe in circles. Document what you observed in each cycle and what remains unclear. Do not run a third diagnostic cycle on your own.
