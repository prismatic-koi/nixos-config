---
name: twg
description: Load this skill for Jira work with the `twg` CLI — sprints, boards, story points, JQL searches, ticket history (changelog), links, and bulk Jira updates. Covers the tested commands, output handling, forbidden commands, and gotchas.
---

# twg: Jira CLI for sprints, story points, and bulk updates

`twg` is the Atlassian CLI. The Atlassian MCP has no sprint or board tools, and `twg` has them. This skill covers `twg` for Jira.

For host-only login, the commands that you must not run (`login`, `setup`, `upgrade`, `upkeep enable`), and the environment variables, read the section "twg CLI (secondary surface)" in the `atlassian` skill. Do not repeat those rules here. The `atlassian` skill says to use the MCP tools unless your task tells you to use `twg`. Use `twg` when your task is in the list below.

## When to load

Load this skill for sprint work, story points, bulk Jira updates, ticket history, and JQL searches.

## Setup facts

- Config lives in `~/.config/twg`. Cache lives in `~/.cache/twg`. Both are writable in the sandbox.
- Login uses OAuth 2.1 and happens on the host. If `twg` reports no login or an expired login, stop and run `prism escalate`.
- Set `DO_NOT_TRACK=1` to turn off usage data, if the wrapper does not set it already.
- Do not copy the `twg` documentation. Use `twg help`, `twg help describe "<command>"`, and `twg help-map`.
- The ticket flag differs by command. Run `twg help describe "<command>"` before the first use.

## Forbidden without approval

CAUTION: Do not run any command in this list unless the owner gives explicit approval in the same session. Ask the owner and wait for the answer.

- Every `delete` command and every `archive` command (for example `jira workitem delete`, `jira board delete`, `jira sprint delete`, `jira space delete`).
- Every write command under `jira space`, including `update`.
- `jira board create`.
- Every write command under `jira field`, `jira filter`, and `jira dashboard` (including `dashboard bulk-edit`).
- `jira sprint start` and `jira sprint complete`.

The organisation setting "Allow all permissions by default" is on. The CLI can write and delete wherever the owner can. Approval from an earlier session does not carry over.

## Read commands

All commands start with `twg jira`.

| Job | Command |
|---|---|
| Find the board | `board query --project PLAT` |
| List sprints | `board sprints query --id 116 [--state future\|active\|closed]` |
| Search with JQL | `workitem query --jql '...' --fields a,b -n 100 --output json` |
| Read tickets | `workitem get KEY [KEY...] --full` or `--comments` |
| Read ticket history | `workitem changelog query --issue-id PLAT-N --first 20` |
| List valid transitions | `workitem transitions query --id PLAT-N` |
| List link types | `workitem link-types query` |
| List editable fields | `workitem field update-metadata --id PLAT-N` |

## Write commands

One `workitem update` call can set the sprint, points, priority, and labels together.

| Job | Command |
|---|---|
| Rename a sprint or change its dates | `sprint update --id 352 --board-id 116 --state future --name "..." --start-date ... --end-date ...` |
| Add a ticket to a sprint | `workitem update --id PLAT-N --sprint 352` |
| Remove a ticket from a sprint | `workitem update --id PLAT-N --sprint none` |
| Set story points | `workitem update --id PLAT-N --story-points 3` |
| Set priority, labels, due date | `workitem update --id PLAT-N --priority high --add-labels x --remove-labels x --due-date 2026-10-20` |
| Add a comment | `workitem update --id PLAT-N --comment "..." --comment-format plain` |
| Close a ticket with a comment | `workitem update --id PLAT-N --status "Not Doing" --transition-comment "..." --transition-comment-format plain` |
| Link tickets | `workitem link workitem --id A --link-type-id Duplicate --target-id B` |

## Reading output

- `--output json` does not print the data. It prints a YAML summary. The line `stdout:` under `output_files` names a temp file. Read the JSON from that file. A script that expects data on stdout fails.
- A query returns at most 100 tickets. If the summary shows `hasNextPage: true`, pass `nextCursor` back with `--after`.
- In query data and `get --full` data, ticket fields are at the top level of each issue, not under a `fields` key. A multi-key `get` wraps each ticket as `items[].data`.
- After a successful `update`, the output is `Updated KEY:` and a line for each field. A failure prints a JSON error with `error.code` and `error.message`. Read the message, because many failures share one code.

## Gotchas

1. `sprint update` fails with "Sprint state is required" unless you pass `--state`. Pass the current state.
2. Do not use `sprint workitems query` to check sprint membership. It returned `total: 0` for a sprint that held tickets. Use the JQL `sprint = N`.
3. `--story-points` fails with "Field 'storyPointEstimate' cannot be set" while Estimation is off on the board. Story points write to `customfield_10016`.
4. `--story-points none` is rejected. To clear the value, use `--fields-json '{"customfield_10016":null}'`.
5. Comment and description bodies are Atlassian Document Format (ADF). To append to a description, read the ADF, add nodes, and write it back with `--fields-json '{"description": <adf>}'`.
6. "Blocks" links are often stale. A blocker can be Done already, and real blockers can be only in comments. Read the comments before you trust a link.
7. `twg` cannot read Jira Automation rules. If an automation changed a ticket, the changelog shows the actor "Automation for Jira" with account type `app`.

## Verification rule

After any batch write, read the result back with JQL (for example `sprint = 352`). Compare the count with the plan. In one test, a batch of 33 updates left one ticket unchanged, and a second update fixed it. Update any ticket that is missing, then check again.

## Relationship to the atlassian skill

The `atlassian` skill still covers Confluence page edits and any work in a session without `twg`.
