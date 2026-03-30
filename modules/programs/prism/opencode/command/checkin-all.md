---
description: Check in on all feature agents working in this repository and report their current status.
agent: "coordinator"
subtask: false
---

Run `prism list-sessions` and identify all sessions for the current repository. To determine the repo name, run `basename $(git rev-parse --show-toplevel)` from the repository root (not inside a worktree subdirectory — if `git rev-parse --show-toplevel` returns a path ending in a branch name, go one level up). Sessions for this repo will have names like `nixos-config@...`.

Exclude the session for the default branch (e.g. `@main` or `@master`) — that is the coordinator session, not a feature agent. The default branch can be identified by running `git remote show origin | grep 'HEAD branch'`, or by looking for the session whose branch matches the remote default.

For each feature agent session found:
1. Run `prism checkin <session>` to capture its current state
2. Note the session name, state (active/finished/idle/waiting), and what it appears to be working on

Report back with a summary table of all feature agents and their current status. If no feature agents are found, say so.
