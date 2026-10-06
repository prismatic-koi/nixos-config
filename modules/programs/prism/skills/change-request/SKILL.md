---
name: change-request
description: Load this skill when drafting or editing a Jira Change Request (CR) ticket in the CH project (Change Release) at thankyoupayroll.atlassian.net, or any change ticket whose primary reader is an executive approver rather than the implementing engineer. The skill covers audience framing, description structure, field-by-field guidance, and the CH project's known quirks (ADF formatting, character limits).
---

# Writing Change Requests for executive approval

Load alongside the `atlassian` skill when actually creating or editing the
ticket - this skill covers what to write; the `atlassian` skill covers how
to invoke the API.

## Audience model

The primary reader of a CR in the CH project is an executive approver who
is not deeply technical. They are slightly technical - they know what a
database is, they recognise "AWS" and "firewall", but they do not write
code and will not parse cryptic resource names. They care about:

1. What problem is being solved, and for whom.
2. Why it's safe.
3. What's NOT changing (blast radius).
4. How rollback works and how long it takes.

Write the description for that reader. The implementing engineer reads
the CLI commands in the custom fields; the approver reads the description.

The engineer-facing ticket (typically a PLAT-* engineering ticket) is where
the deep technical detail lives - link it under "Related tickets". Don't
duplicate that detail into the CR.

## Tone and language

- Lead with the human problem, not the technical mechanism. "Three admin
  staff working from home cannot use two pages" before "WAF rule
  CrossSiteScripting_BODY false-positive".
- Define jargon on first use, then keep using the term. "Web-application
  firewall (WAF)", then "WAF" or "the firewall" thereafter. "Load
  balancer" instead of "ALB" in prose.
- Explain mechanical-sounding details that are actually safety mechanisms.
  Example: "AWS returns a short-lived token (LockToken) that we must
  present when applying the update - a safety mechanism so two people
  cannot overwrite each other's changes."
- Do not oversimplify to the point of being patronising. Slightly
  technical means "don't insult the reader with a metaphor", not "don't
  use any technical words".
- British/NZ English spelling (colour, defence, behaviour, etc.).
- No smart punctuation. Em dashes (—), en dashes (–), curly quotes
  (‘ ’ “ ”), and ellipsis (…) are all AI-tell characters AND all fail
  in various downstream text pipelines (Jira ADF, IaC provider APIs,
  paste-into-shell). Use ASCII hyphen (-), straight quotes, literal
  three dots (...), or restructure.
- No bold formatting in body text.

## Description structure

A CR description must have these sections in roughly this order:

1. **What this change does** - one or two paragraphs in plain language.
   Start with who is affected and what they experience today. End with
   what changes for them after the CR lands.

2. **Why it's safe** - a bulleted list of the safety properties. Typical
   bullets include: scope is narrow (name it), other protections remain
   in place, the change has been rehearsed or soaked elsewhere, rollback
   is fast, the application/data is not touched.

3. **What's being changed** - numbered steps, in plain English. This is
   the description-level summary of the mechanism; the precise CLI lives
   in the Implementation Steps custom field.

4. **What's NOT changing** - explicit list of out-of-scope items.
   Executives reviewing changes want to know blast radius. State what is
   excluded: code, data, deployment, other systems, related cleanup
   work tracked elsewhere.

5. **Where it's being changed** - environment, account, region, the
   specific resource by name. This is the only place hard identifiers
   belong in the prose; everywhere else, use the readable name.

6. **Approval is contingent on** - the conditions that must be true
   before the approver signs off. This usually mirrors the Approval
   Conditions custom field (see field-by-field below).

7. **Related tickets** - link to the engineering ticket (PLAT-*),
   related cleanup tickets, and any other CRs in the same chain.

## CH project field-by-field guidance

The CH project has custom fields that must be filled. Each carries a
specific audience expectation.

> Field IDs below were verified against createmeta on 2026-07-14 for
> project CH, issue type Task (id `10008`). They are likely
> instance-specific to the thankyoupayroll.atlassian.net tenant; a
> different Jira tenant will almost certainly renumber them, so treat
> the IDs as a snapshot, not a specification. Pattern worth remembering:
> the ITSM-native fields (Change Risk, Approvers, Team, Start date) live
> in the `customfield_100xx` range, while the custom CH change-management
> fields live in the `customfield_111xx` and `customfield_112xx` ranges. Mixing the two ranges is
> the most common source of hard API failures when creating a CR.

### Change Category (`customfield_11130`)

One of: `Standard pre-approved change`, `Normal`, `Emergency`. Default to
`Normal` for engineering CRs unless there is an existing standing pre-
approval for the change pattern.

### Change Risk (`customfield_10006`)

Select list. Allowed values and their option IDs:

- Critical - id `10007`
- High - id `10008`
- Medium - id `10009`
- Low - id `10010`

Accepted payload shapes are `{"value": "Medium"}` or `{"id": "10009"}`
(both work). Sending a plain string (e.g. `"Medium"`) fails.

Bias toward honesty over optimism. A change with a fast tested rollback
and no application impact is `Low`. A change that touches production
data, has no rehearsal, or has ambiguous rollback is at least `Medium`.

### Systems/Service Affected (`customfield_11132`)

Labels-type field: send a JSON array of strings. Label values cannot
contain spaces - Jira rejects a plain string with `Specify the value
for Systems/Service Affected in an array.` and rejects a whitespace-
containing label outright.

Array of hyphenated labels - no spaces. Name the system in human terms:
`["production-web-apps", "production-ALB", "thankyoupayroll.co.nz"]`.
Not ARNs.

### Business impact during change (`customfield_11133`)

Rich text (markdown). One short paragraph. State whether users see anything, whether
there is a service interruption, whether there are dropped requests, and
the expected propagation window. Be specific about durations.

### Communication plan (`customfield_11134`)

Rich text (markdown), numbered list. Cover: who is notified before the change, who
is notified at apply time, who is notified after, and whether customer
communication is required. Include the channel (Slack, email, etc.).

### Pre-implementation Steps (`customfield_11135`)

Rich text (markdown), numbered list. The engineer's prep work. Include the exact
CLI commands as code blocks (`bash` language). Cover: dependencies on
other tickets, downloading the current state for rollback, building the
new state, attaching artefacts for peer review.

### Implementation steps (`customfield_11136`)

Rich text (markdown), numbered list. The actual apply. Include the exact CLI
commands as code blocks. Capture any returned tokens or IDs needed for
rollback into the ticket as a step.

### Rollback Plan (`customfield_11137`)

Rich text (markdown). Lead with how long rollback takes and whether it has any
side effects. Include the exact CLI command. State explicitly what the
post-rollback state is and that it does not introduce new risk - it
returns the system to the current state, not to some untested third state.

### Pre-change testing (`customfield_11138`)

Rich text (markdown). Describe where and how the change has been rehearsed. If the
change has been soaked in staging, name the period and cite CloudWatch
or equivalent telemetry as evidence the change has been exercised, not
just deployed. If the change has not been rehearsed, say so plainly and
justify why a CR is still appropriate.

### Post-change validation (`customfield_11139`)

Rich text (markdown), numbered list. Each check must specify what is being
verified, where the evidence comes from, and a time window
(e.g. "within 5 minutes of apply", "for 30 minutes after apply").
Include at least one positive check (the thing we wanted to fix is
fixed), at least one negative check (we did not break adjacent
behaviour), and a monitoring check (no spike in errors).

### Approval conditions (`customfield_11140`)

**Plain string field, hard-capped at 255 characters.** Do not try to send
ADF here - the API rejects it with `Operation value must be a string`.
Do not try to send the full conditions - the API rejects with
`Approval conditions can't exceed 255 characters`.

Practical pattern: write the full approval conditions inside the
description ("Approval is contingent on" section), and put a one-line
pointer in this field. Example: `See description for full conditions:
PLAT-NNN prereqs recorded; peer reviewer signs off on diff; scope stays
Normal.`

### Security Impact Level (`customfield_11244`)

Select list. The CH approver requires a value on every CR. The API exposes
no allowed values, and it rejects `{"value": "Low"}` with `Specify a valid
value`.

Do not set this field. Ask the human to set it in the Jira UI. Keep this rule
until a Jira admin configures the options.

### Security Impact Description (`customfield_11245`)

Text field. The CH approver requires a value on every CR. Write one or two
sentences:

- If the change has a security impact, state the impact. Then state why it
  is acceptable, or which controls mitigate it.
- If the change has no security impact, state why.
- If the change widens access, do not write "none". Write "Minimal" and give
  the reason.

Example (CH-242): `Minimal. The change adds network access from cluster
nodes to two internal services only: DNS on port 53 and the KEDA operator on
port 9666. Nothing outside the cluster is opened.`

### Fields the agent fills

- Description
- Change Category (`customfield_11130`)
- Change Risk (`customfield_10006`)
- Systems/Service Affected (`customfield_11132`)
- Business impact during change (`customfield_11133`)
- Communication plan (`customfield_11134`)
- Pre-implementation Steps (`customfield_11135`)
- Implementation steps (`customfield_11136`)
- Rollback Plan (`customfield_11137`)
- Pre-change testing (`customfield_11138`)
- Post-change validation (`customfield_11139`)
- Approval conditions (`customfield_11140`)
- Security Impact Description (`customfield_11245`)
- Security Impact Level (`customfield_11244`) - do not set it. Ask the human
  to set it in the UI until a Jira admin configures the options.

### Fields the human must fill

The agent must not guess these. Flag them in the response to the human
so they can fill in:

- Assignee
- Approvers (`customfield_10003`)
- Peer Reviewer (`customfield_11141`)
- Target Implementation Date/Time (`customfield_11131`)
- Start date (`customfield_10015`)
- Team (`customfield_10001`)
- Security Impact Level (`customfield_11244`) - see the field section above.
- Related issues (`customfield_11142`) - optional. Link related Jira
  issues (typically the engineering PLAT-* ticket, or a parent CR)
  when the CR is part of a chain.

## Rich-text field format

By default, send the CH custom text fields as plain markdown strings with
`contentFormat: "markdown"` on `createJiraIssue` or `editJiraIssue`. This
worked for `customfield_11133` to `customfield_11139` on CH-235 to CH-238 and
CH-242. Code blocks and numbered lists render.

ADF is the fallback. If a field rejects markdown with `Operation value must
be an Atlassian Document`, send Atlassian Document Format (ADF) JSON. Wrap
content as:

```json
{
  "type": "doc",
  "version": 1,
  "content": [
    {"type": "paragraph", "content": [{"type": "text", "text": "..."}]},
    {"type": "orderedList", "attrs": {"order": 1}, "content": [
      {"type": "listItem", "content": [
        {"type": "paragraph", "content": [{"type": "text", "text": "..."}]}
      ]}
    ]},
    {"type": "codeBlock", "attrs": {"language": "bash"}, "content": [
      {"type": "text", "text": "aws ...\n  --flag value"}
    ]}
  ]
}
```

The ticket description itself takes Markdown via `contentFormat: markdown`
on the createJiraIssue / updateJiraIssue tools.

## Edits to an existing CR

CAUTION: An update to `description` replaces the whole field. It does not
merge with the current text.

If the ticket status is "under review", or a human is the assignee, do one
of these:

- Update only the custom fields. Do not send `description`.
- Tell the human what you want to change in the description before you
  change it.

This prevents an overwrite of edits that the human made.

## Done transition

To close a CH ticket, call `transitionJiraIssue` with transition id `41`
("Done"). Send no fields. The transition has a screen, but the call works
without fields.

```
transitionJiraIssue(issueIdOrKey: "CH-242", transition: {"id": "41"})
```

If `createJiraIssue` is not in your tool list, the Atlassian family is
deferred behind `activate_atlassian` (issue #2532) — call it once, with no
arguments, before drafting.

## Worked example

CH-5 is the canonical example as of this skill's first draft. Read it
end-to-end before drafting a similar CR. It demonstrates:

- Human problem framing in the first paragraph.
- Explicit "Why it's safe" section before "What's being changed".
- Plain-English mechanism summary with full CLI in the custom fields.
- "What's NOT changing" section calling out adjacent work tracked
  elsewhere (PLAT-328).
- Approval conditions truncated to 255 chars in the field, full text in
  the description.

CH-5 is a manual change with the AWS CLI. For a change that IaC or GitOps
applies, the Implementation steps field must show how CI or Flux applies the
change after merge. Do not show a local `tofu plan` or `tofu apply`. An agent
cannot run them (see the `infrastructure-as-code` skill).

OpenTofu repo on the shared workflow (current examples: CH-235, CH-236):

1. Merge the pull request to main in the `<repo>` repo.
2. CI starts the production OpenTofu apply job. Approve the production
   GitHub Environment deployment when it asks.
3. The apply job recomputes the plan and compares it with the reviewed plan
   comment. If the two differ, the job does not apply. Do not re-run a
   failed apply job. Open a new pull request.
4. Record the apply result on this ticket.

Flux path (current examples: CH-237, CH-238, CH-242):

1. Merge the pull request to main in the `<repo>` repo.
2. Flux applies the change. Make sure that the Flux Kustomization shows the
   merge commit and that the HelmRelease or resource reports Ready.
3. Record the result on this ticket.

## When NOT to load this skill

- Drafting a regular engineering ticket (PLAT-*, etc.) - use the
  engineering ticket conventions, not CR conventions. Engineering tickets
  are written for engineers, not approvers.
- Drafting a postmortem or RCA - different artefact, different audience.
- Drafting a change ticket in a different organisation's Jira whose
  Change project does not share the CH project's field shape.
