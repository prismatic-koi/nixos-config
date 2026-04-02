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

# Spawn on a named branch
prism spawn --branch update-plex --prompt "..."

# Check out a PR branch and spawn a session on it
prism spawn --pr 268 --prompt "review this PR"
# or shorthand:
prism pr 268 --prompt "review this PR"
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
| `--branch <name>` | Branch name for the new worktree. Defaults to a timestamp. |
| `--pr <number>` | Fetch and check out the branch for this PR number. |
| `--prompt <text>` | Instruction passed to opencode on launch. Wrap values containing shell metacharacters in **single quotes**. The value `-` is reserved and reads from stdin (cannot pass a literal `-`). |
| `--prompt-file <path>` | Read the prompt from a file instead of passing it as an argument. Mutually exclusive with `--prompt`. A single trailing newline is stripped. |
| `--agent <name>` | Opencode agent to use (`build` or `plan`). Defaults to `build`. |
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
  --branch update-plex \
  --prompt "find the plex container image in this repo and update it to the latest tag from dockerhub, then open a PR"
```

## Example: reviewing a PR

```bash
prism pr 268 --prompt "review this PR and summarise the changes"
```

## Example: create a ticket then spawn an agent to action it

When the user asks you to create a ticket (e.g. Jira) and then spawn an agent to work on it:

1. Create the ticket using the appropriate MCP tool and capture the ticket ID (e.g. `PROJ-123`)
2. Use the ticket ID as the branch name
3. Spawn the agent — it has the Atlassian MCP available and can read the ticket itself

```bash
# After creating ticket PROJ-123:
prism spawn \
  --branch PROJ-123 \
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

Use `prism checkin <session>` to read the recent conversation history for a session, sourced from the prism DB. Output is structured as message turns (user and assistant pairs) with tool calls, tool results, and permission events rendered inline under their parent assistant message:

```bash
prism checkin nixos-config@update-plex
```

```
checkin: nixos-config@update-plex

state: active

[2026-04-03 14:22:01] user
implement the feature and open a PR

[2026-04-03 14:22:05] assistant
I'll implement this now.
  → Bash: ls src/
  → result: main.go utils.go

── end of event log ──
```

With no argument, `prism checkin` lists available sessions and exits with a hint.

### `prism checkin` flags

| Flag | Description |
|---|---|
| `--last <N>` | Number of message turns to show (default 10) |
| `--from <id>` | Show N events forward from this event ID |
| `--before <id>` | Show N events backward from this event ID |
| `--types <list>` | Comma-separated event types to filter (default: `msg_user,msg_assistant,tool_call,tool_result,permission_ask,permission_denied`) |
| `--verbose` | Show full tool args/results without truncation |
| `--all` | (no-arg mode only) List all sessions across all repos |

These commands are useful when you need to know where a spawned agent is at without switching to its session.

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

# Urgent delivery — interrupt the agent within ~2 seconds
prism prompt nixos-config@update-plex --urgent --prompt 'stop what you are doing and open the PR now'
```

The same shell-escaping conventions that apply to `prism spawn` apply here —
see [Passing prompts safely](#passing-prompts-safely--shell-escaping) above.

### `prism prompt` flags

| Flag | Description |
|---|---|
| `--prompt <text>` | Prompt text to send. Supports `-` to read from stdin. |
| `--prompt-file <path>` | Read prompt from a file. Mutually exclusive with `--prompt`. |
| `--urgent` | Deliver within ~2 seconds via interrupt polling instead of waiting for the next idle event. |

The prompt is written to the message bus and delivered by the opencode plugin. The session must exist and have an agent window — use `prism list-sessions` to check first.

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
