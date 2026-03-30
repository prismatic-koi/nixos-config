---
description: Check in on all feature agents working in this repository and report their current status.
agent: "coordinator"
subtask: false
---

Run `NO_COLOR=1 prism list-sessions` (the `NO_COLOR` flag suppresses ANSI escape sequences so the output can be parsed as plain text). The output has a `SESSION  STATE  TITLE` header row — skip it when parsing. If the command returns an error, if the output is the single line `no agent sessions found`, or if there are no data rows after the header, report that no sessions are available and stop.

Determine the repo name by running `git remote get-url origin | xargs basename -s .git` (works correctly inside any worktree). Determine the default branch by running `git symbolic-ref refs/remotes/origin/HEAD --short 2>/dev/null | sed 's|origin/||'`; if that returns nothing, fall back to `git remote show origin | grep 'HEAD branch' | awk '{print $NF}'`.

Feature agent sessions are those whose name matches `^<repo>@` but is not exactly `<repo>@<default-branch>`. For example, for repo `nixos-config` with default branch `main`, keep sessions like `nixos-config@add-some-feature` and exclude `nixos-config@main` and any sessions from other repos.

For each feature agent session found:
1. Run `prism checkin <session>` to capture its current state
2. Note the session name, state (active/finished/idle/waiting), and what it appears to be working on

Report back with a summary table of all feature agents and their current status. If no feature agents are found, say so.
