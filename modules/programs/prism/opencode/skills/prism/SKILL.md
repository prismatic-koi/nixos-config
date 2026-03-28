---
name: prism
description: Spawn isolated agent sessions in their own git worktrees using the prism tool. Use when the user asks to spawn an agent, delegate work to another session, run something in parallel, or work on a PR or different repo.
---

# Spawning Agents with prism

`prism spawn` creates a new git worktree, starts a tmux session in it, and launches opencode. Use it when work should be isolated, long-running, or in a different repo — rather than a subagent, which shares the current session context.

## When to use prism spawn vs a subagent

| Situation | Use |
|---|---|
| User says "spawn an agent" or "use prism" | `prism spawn` |
| Work is in a different repo | `prism spawn --repo` |
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

# Spawn in a different repo (shorthand name under ~/code)
prism spawn --repo home-ops --prompt "update the plex image to the latest tag and open a PR"

# Spawn on a named branch
prism spawn --repo home-ops --branch update-plex --prompt "..."

# Check out a PR branch and spawn a session on it
prism spawn --repo nixos-config --pr 268 --prompt "review this PR"
# or shorthand:
prism pr 268 --repo nixos-config --prompt "review this PR"
```

## All flags

| Flag | Description |
|---|---|
| `--repo <name>` | Repo folder name under `~/code`, or full path. Errors with a `prism clone` hint if not found. |
| `--branch <name>` | Branch name for the new worktree. Defaults to a timestamp. |
| `--pr <number>` | Fetch and check out the branch for this PR number. |
| `--prompt <text>` | Instruction passed to opencode on launch. |
| `--agent <name>` | Opencode agent to use (`build` or `plan`). Defaults to `build`. |
| `--attach` | Switch the current tmux client to the new session instead of spawning headlessly. |

## Behaviour

- **Headless by default**: the session is created in the background. The user can switch to it via the prism dashboard (`C-w`) or picker (`C-f`).
- **`--attach`**: use when the user wants to be taken directly to the new session.
- The prompt is sent to opencode automatically after startup — no manual input needed.
- Each spawned session gets its own worktree, so changes are isolated from other branches.

## Example: delegating work to another repo

```bash
prism spawn \
  --repo home-ops \
  --branch update-plex \
  --prompt "find the plex container image in this repo and update it to the latest tag from dockerhub, then open a PR"
```

## Example: reviewing a PR

```bash
prism pr 268 --repo nixos-config --prompt "review this PR and summarise the changes"
```

## Example: create a ticket then spawn an agent to action it

When the user asks you to create a ticket (e.g. Jira) and then spawn an agent to work on it:

1. Create the ticket using the appropriate MCP tool and capture the ticket ID (e.g. `PROJ-123`)
2. Use the ticket ID as the branch name
3. Spawn the agent — it has the Atlassian MCP available and can read the ticket itself

```bash
# After creating ticket PROJ-123:
prism spawn \
  --repo home-ops \
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

Use `prism checkin <session>` to capture the live screen of a session's agent window. The output is cleaned up (borders and chrome stripped) and is suitable for reading directly:

```bash
prism checkin nixos-config@update-plex
```

With no argument, `prism checkin` lists available sessions and exits with a hint.

These commands are useful when you need to know where a spawned agent is at without switching to its session.

## Sending a follow-up prompt to a running session

Use `prism prompt <session> --prompt <text>` to send a follow-up message to the opencode agent in a session that is already running (or has finished and is waiting for input):

```bash
prism prompt nixos-config@update-plex --prompt "looks good, go ahead and open a PR"
```

The prompt is delivered after a short delay (500 ms) to allow opencode to finish any in-flight operation before accepting the new input. The session must exist and have an agent window — use `prism list-sessions` to check first.
