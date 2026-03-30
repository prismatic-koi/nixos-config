---
description: Check in on all feature agents working in this repository and report their current status.
agent: "coordinator"
subtask: false
---

Run `prism list-sessions` and identify all sessions for the current repository. To determine the repo name, run `git remote get-url origin | xargs basename -s .git` — this works correctly regardless of whether you are inside a worktree. Sessions for this repo will have names like `nixos-config@...`.

Exclude the session for the default branch (e.g. `@main` or `@master`) — that is the coordinator session, not a feature agent. The default branch can be identified by running `git remote show origin | grep 'HEAD branch'`, or by looking for the session whose branch matches the remote default.

For each feature agent session found:
1. Run `prism checkin <session>` to capture its current state
2. Note the session name, state (active/finished/idle/waiting), and what it appears to be working on

Report back with a summary table of all feature agents and their current status. If no feature agents are found, say so.
