---
name: atlassian
description: Load this skill when reading from Jira Cloud or Confluence Cloud — searching issues with JQL, fetching a specific Jira issue or Confluence page, listing workflow transitions, or understanding the atlassian CLI read commands.
---

# Atlassian Skill — Jira and Confluence Read Access

## When to load

Load this skill when:

- Searching Jira issues with JQL (e.g. "find all open bugs in project FOO").
- Fetching a specific Jira issue by key (e.g. "get the details of FOO-123").
- Looking up available workflow transitions for a Jira issue.
- Searching Confluence pages with CQL.
- Fetching a specific Confluence page by ID.
- Deciding whether to use `--full` or the default slim output.

---

## Background: two surfaces, one future

**opencode currently uses the Atlassian MCP path** (`mcp-atlassian-slim-proxy.mjs` + `mcp-remote`) for its Atlassian integration. That surface is intentionally unchanged — opencode will migrate to `atlassian` CLI later, once it has been exercised in the pi harness.

**The `atlassian` binary is the cross-harness future.** It works in **both** the pi harness (which has no MCP support) and in opencode (today via Bash tool, eventually as the primary surface). Both surfaces apply identical slim-output drop-key sets, so outputs are comparable regardless of which path you use.

**If you are in the pi harness:** always use `atlassian` via the Bash tool.
**If you are in opencode today:** use the Atlassian MCP tools for native MCP calls, or `atlassian` via Bash when you need a command that MCP doesn't expose or when you want scripted pipelines with `jq`.

---

## Authentication

The binary reads three environment variables set by the NixOS/darwin module:

| Variable | Description |
|---|---|
| `ATLASSIAN_SITE` | e.g. `mycompany.atlassian.net` |
| `ATLASSIAN_EMAIL` | your Atlassian account email |
| `ATLASSIAN_API_TOKEN` | sourced at runtime from the sops-managed secret |

If any variable is missing the binary prints a single-line error and exits non-zero. No interactive prompt will appear.

---

## Command reference

### `atlassian whoami`

Print the current authenticated user as slim JSON.

```bash
atlassian whoami
```

### `atlassian resources`

List accessible Atlassian cloud resource IDs as slim JSON. `scopes` and `url` are dropped.

```bash
atlassian resources
```

### `atlassian jira search`

Search Jira with JQL. Returns `{"issues": [...], "total": N, ...}` — slim JSON, large body fields dropped.

```bash
# Recent open bugs in a project
atlassian jira search 'project = FOO AND issuetype = Bug AND status != Done ORDER BY created DESC' --limit 20

# Issues assigned to the current user
atlassian jira search 'assignee = currentUser() AND status != Done' --limit 50

# Sprint issues
atlassian jira search 'project = FOO AND sprint in openSprints()' --limit 100

# Empty result (limit 0)
atlassian jira search 'project = FOO' --limit 0
```

Flags:
- `--limit N` (default 50): max issues returned. 0 returns empty list.
- `--fields f1,f2`: comma-separated list of fields to include.

### `atlassian jira get`

Fetch a single Jira issue. By default returns slim JSON with `fields.description` replaced by `fields.description_markdown` (ADF rendered to Markdown).

```bash
atlassian jira get FOO-123
atlassian jira get FOO-123 | jq '.fields.description_markdown'
```

Use `--full` for the raw untouched API response (no slimming, raw ADF preserved):

```bash
atlassian jira get FOO-123 --full
```

**When to use `--full`:** when you need fields that the slim output drops (e.g. `renderedFields`, `operations`, raw ADF structure), or when debugging slim-output omissions.

### `atlassian jira transitions`

List available workflow transitions for an issue. Returns `{"transitions": [{"id": "...", "name": "..."}]}`.

```bash
atlassian jira transitions FOO-123
atlassian jira transitions FOO-123 | jq '.transitions[] | select(.name == "In Progress")'
```

### `atlassian confluence search`

Search Confluence pages with CQL. Returns slim JSON list of page results.

```bash
# Pages in a space matching a title
atlassian confluence search 'space = ENG AND title ~ "architecture"' --limit 10

# Recently modified pages
atlassian confluence search 'space = ENG AND lastModified > "2024-01-01"' --limit 25

# Pages with a specific label
atlassian confluence search 'label = "decision-record" AND space = ENG' --limit 20
```

Flags:
- `--limit N` (default 25): max pages returned. 0 returns empty list.

### `atlassian confluence get`

Fetch a single Confluence page by ID. By default returns slim JSON with `body_markdown` (ADF rendered to Markdown) instead of raw body.

```bash
atlassian confluence get 123456789
atlassian confluence get 123456789 | jq '.body_markdown'
```

Use `--full` for the raw untouched API response:

```bash
atlassian confluence get 123456789 --full
```

**When to use `--full`:** when you need the raw ADF/storage body, metadata fields that slim output drops, or need to inspect the response structure before building a pipeline.

---

## Slim output

All commands except `--full` apply these drop-key sets:

- **Universal** (all responses): `expand`, `self`, `iconUrl`, `avatarUrl`, `avatarUrls`, `avatarId`, `picture`, `schema`
- **Jira issues**: additionally drops `renderedFields`, `operations`, `permissions`, `transitions`, `watchers`, `worklog`, `attachments`, `properties`, `names`, `subtask`, `hierarchyLevel`, `editmeta`, `versionedRepresentations`, `colorName`
- **Confluence pages**: additionally drops `_links`, `status`, `lastModified`
- **Search/list responses**: additionally drops `body`, `description`, `content`, `comments`, `comment`, `changelog`, `history`, `adf`
- **`whoami`**: additionally drops `account_status`, `characteristics`, `last_updated`, `created_at`, `nickname`, `locale`, `extended_profile`, `account_type`, `email_verified`
- **`resources`**: additionally drops `scopes`, `url`

These sets match the MCP slim proxy exactly, so opencode and pi produce comparable outputs.

---

## Error handling

- **Missing env var**: single-line error naming the variable, exits non-zero.
- **HTTP 4xx/5xx**: single-line `<status> <method> <path>: <message>` on stderr, nothing on stdout, exits non-zero.
- **Invalid JQL/CQL**: clean non-zero exit with the Atlassian error message, no stack trace.

---

## Piping with jq

```bash
# Extract just the issue summary and status
atlassian jira search 'project = FOO AND status = "In Progress"' | \
  jq '.issues[] | {key, summary: .fields.summary, status: .fields.status.name}'

# Get page body as markdown
atlassian confluence get 123456789 | jq -r '.body_markdown'

# Get transition IDs
atlassian jira transitions FOO-123 | jq '.transitions[] | "\(.id): \(.name)"'
```
