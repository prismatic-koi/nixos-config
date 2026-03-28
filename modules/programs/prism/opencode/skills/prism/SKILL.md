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
2. Use the ticket ID as the branch name and include it in the prompt so the spawned agent has full context
3. Spawn the agent with enough detail that it can work autonomously without referring back

```bash
# After creating ticket PROJ-123:
prism spawn \
  --repo home-ops \
  --branch PROJ-123 \
  --prompt "PROJ-123: <paste ticket title and description here so the agent has full context without needing to look it up>"
```

The spawned agent won't have access to your current session context, so the `--prompt` must be self-contained — include the ticket summary, acceptance criteria, and any relevant details rather than just the ticket ID.
