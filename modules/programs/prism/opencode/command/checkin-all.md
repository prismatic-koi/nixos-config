---
description: Check in on all feature agents working in this repository and report their current status.
agent: "coordinator"
subtask: false
---

Run `prism list-sessions` and identify all sessions for the current repository (sessions whose name starts with the repo name, e.g. `nixos-config@...`). Exclude the `@main` session — that is the coordinator, not a feature agent.

For each feature agent session found:
1. Run `prism checkin <session>` to capture its current state
2. Note the session name, state (active/finished/idle/waiting), and what it appears to be working on

Report back with a summary table of all feature agents and their current status. If no feature agents are found, say so.
