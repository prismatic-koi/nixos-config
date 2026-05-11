---
name: atlassian
description: Load this skill when reading from or writing to Jira Cloud or Confluence Cloud — searching issues, fetching a specific Jira issue or Confluence page, listing or performing workflow transitions, creating/updating issues, commenting on issues, publishing or updating Confluence pages, or understanding the atlassian CLI commands.
---

# Atlassian Skill — Jira and Confluence CLI

## When to load

Load this skill when:

- Searching Jira issues with JQL (e.g. "find all open bugs in project FOO").
- Fetching a specific Jira issue by key (e.g. "get the details of FOO-123").
- Creating or updating Jira issues from postmortem notes, investigation summaries, etc.
- Adding a comment to a Jira issue.
- Transitioning an issue through a workflow (e.g. "move FOO-123 to In Progress").
- Looking up available workflow transitions for a Jira issue.
- Searching Confluence pages with CQL.
- Fetching a specific Confluence page by ID.
- Publishing a new Confluence page from a markdown document.
- Updating an existing Confluence page (fetch-edit-push pattern).
- Deciding whether to use `--full`, `--body-file`, `--adf`, or `--storage` flags.

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

## Delivering bodies safely: `--body-file` and stdin

All body-bearing write commands take `--body-file=<path>` where `<path>` may be:
- A file path: `--body-file=description.md`
- `-` to read from stdin: `--body-file=-`

**There is no `--body` argument.** This is intentional — multi-line content on argv is fragile and hard to quote correctly. Always use `--body-file` or `--description-file`.

### stdin patterns

```bash
# Here-doc (good for multi-line structured content)
atlassian jira comment FOO-123 --body-file=- <<'EOF'
## Summary

Investigated the issue. Root cause is a nil pointer in the handler.

## Steps to reproduce

1. Call `/api/v1/foo` with empty body
2. Observe 500 response
EOF

# printf (good for short single-line content)
printf 'Quick note: confirmed fixed in v2.3.1' | \
  atlassian jira comment FOO-123 --body-file=-

# From a file
atlassian jira comment FOO-123 --body-file=investigation-notes.md

# Pipe from another command
cat postmortem.md | atlassian confluence create \
  --space=ENG --title="Postmortem: Outage 2024-05-10" --body-file=-
```

**Stdin is not consumed** unless `--body-file=-`, `--adf`, or `--storage` is passed. Write subcommands without these flags do not block on stdin.

---

## Supported markdown subset

The markdown→ADF builder supports these constructs. **Anything outside this list causes a non-zero exit** with a message naming the construct and line number.

| Construct | Markdown syntax |
|-----------|-----------------|
| Paragraph | Plain text |
| Heading h1–h6 | `# H1` through `###### H6` |
| Bullet list | `- item` or `* item` |
| Ordered list | `1. item` (with nesting) |
| Fenced code block | ` ```lang ` ... ` ``` ` |
| Inline code | `` `code` `` |
| Bold | `**text**` |
| Italic | `_text_` or `*text*` |
| Link | `[text](url)` |
| Hard line break | Two trailing spaces `  ` at end of line |
| Blockquote | `> text` |
| Table | GFM table syntax (`| col | col |`) |
| Horizontal rule | `---` |

### What fails and why

```
# This fails: raw HTML block
<div>some content</div>
# → error: unsupported markdown construct "raw HTML block" at line 2

# This fails: image
![diagram](architecture.png)
# → error: unsupported markdown construct "image" at line 1

# This fails: inline HTML
Text with <b>bold html</b> inline
# → error: unsupported markdown construct "raw HTML" at line 1
```

**Rationale:** Atlassian's ADF schema has no direct equivalent for these constructs. Silent dropping would produce incomplete documents. The strict rejection lets you fix the input rather than discover missing content after publishing.

**Workaround for unsupported constructs:** Use `--adf` to supply a raw ADF JSON document instead, or `--storage` (Confluence only) for storage-format XHTML.

---

## Escape hatches: `--adf` and `--storage`

### `--adf` (Jira + Confluence)

Reads raw ADF JSON from stdin, validates it parses as JSON, and uploads it unmodified. No markdown→ADF conversion occurs.

```bash
# Jira issue description
echo '{"version":1,"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"raw ADF"}]}]}' | \
  atlassian jira create --project=FOO --type=Task --summary="ADF test" --adf

# Confluence page
cat my-adf-document.json | \
  atlassian confluence create --space=ENG --title="ADF Page" --adf
```

### `--storage` (Confluence only)

Reads raw Confluence storage-format XHTML from stdin. Useful when you have an existing Confluence export or a template in XHTML format.

```bash
cat page-export.xhtml | \
  atlassian confluence create --space=ENG --title="Imported Page" --storage
```

Using `--storage` with any `jira` subcommand exits non-zero (unknown flag).

### Mutual exclusion

`--body-file`, `--adf`, and `--storage` are mutually exclusive. Passing more than one exits non-zero with a clear error.

---

## Read commands

### `atlassian whoami`

```bash
atlassian whoami
```

### `atlassian resources`

```bash
atlassian resources
```

### `atlassian jira search`

```bash
atlassian jira search 'project = FOO AND issuetype = Bug AND status != Done ORDER BY created DESC' --limit 20
atlassian jira search 'assignee = currentUser() AND status != Done' --limit 50
atlassian jira search 'project = FOO AND sprint in openSprints()' --limit 100
```

Flags: `--limit N` (default 50), `--fields f1,f2`.

### `atlassian jira get`

```bash
atlassian jira get FOO-123
atlassian jira get FOO-123 | jq '.fields.description_markdown'
atlassian jira get FOO-123 --full   # raw ADF, no slimming
```

### `atlassian jira transitions`

```bash
atlassian jira transitions FOO-123
atlassian jira transitions FOO-123 | jq '.transitions[] | "\(.id): \(.name)"'
```

### `atlassian confluence search`

```bash
atlassian confluence search 'space = ENG AND title ~ "architecture"' --limit 10
atlassian confluence search 'label = "decision-record" AND space = ENG' --limit 20
```

Flags: `--limit N` (default 25).

### `atlassian confluence get`

```bash
atlassian confluence get 123456789
atlassian confluence get 123456789 | jq -r '.body_markdown'
atlassian confluence get 123456789 --full
```

---

## Write commands

### `atlassian jira create` — File a Jira issue

Creates a new Jira issue. Prints the slim JSON of the created issue (including its key) to stdout on success.

```bash
atlassian jira create \
  --project=FOO \
  --type=Task \
  --summary="Investigate memory leak in worker pool" \
  --description-file=investigation.md

# With labels and assignee
atlassian jira create \
  --project=FOO \
  --type=Bug \
  --summary="Nil pointer in handler" \
  --labels=backend,critical \
  --assignee=<accountId> \
  --description-file=bug-report.md

# Capture the new issue key
KEY=$(atlassian jira create --project=FOO --type=Task --summary="Auto-filed" | jq -r '.key')
echo "Created: $KEY"
```

**Recipe — file an issue from a postmortem:**

```bash
# Write the description to a temp file
cat > /tmp/postmortem-desc.md <<'EOF'
## What happened

The deployment at 14:32 caused a 3-minute outage due to a misconfigured health check.

## Root cause

The new health check endpoint required authentication but the load balancer was not
configured with credentials.

## Action items

- [ ] Add health check endpoint to allowlist
- [ ] Update deployment runbook
EOF

atlassian jira create \
  --project=OPS \
  --type=Bug \
  --summary="Postmortem: deployment outage 2024-05-10" \
  --labels=postmortem,infra \
  --description-file=/tmp/postmortem-desc.md
```

Flags:
- `--project KEY` (required): Jira project key.
- `--type NAME` (required): Issue type name (Task, Bug, Story, etc.).
- `--summary TEXT` (required): Issue summary/title.
- `--description-file PATH`: Markdown description file (- for stdin).
- `--adf`: Read raw ADF JSON from stdin (mutually exclusive with `--description-file`).
- `--labels a,b,c`: Comma-separated labels.
- `--assignee accountId`: Assignee's Atlassian account ID.

### `atlassian jira update` — Update an issue (partial)

Updates only the fields you explicitly pass. Omitting a flag leaves that field **unchanged** in Jira. Exits non-zero if no flags are provided (no-op prevention).

```bash
# Update just the summary
atlassian jira update FOO-123 --summary="New title"

# Update description only
printf '## Updated findings\n\nNew body.' | \
  atlassian jira update FOO-123 --description-file=-

# Update labels and assignee
atlassian jira update FOO-123 --labels=backend,high-priority --assignee=<accountId>
```

Prints the slim JSON of the updated issue to stdout on success.

Flags: `--summary`, `--description-file PATH` (- for stdin), `--adf`, `--labels`, `--assignee`.

### `atlassian jira comment` — Comment on an issue

```bash
# From stdin (here-doc)
atlassian jira comment FOO-123 --body-file=- <<'EOF'
Confirmed the fix works in staging. Deploying to prod at 16:00.
EOF

# From a file
atlassian jira comment FOO-123 --body-file=comment.md

# Raw ADF
echo '{"version":1,"type":"doc","content":[...]}' | \
  atlassian jira comment FOO-123 --adf
```

Prints the slim JSON of the created comment to stdout on success.

Flags: `--body-file PATH` (required, - for stdin), `--adf` (mutually exclusive with `--body-file`).

### `atlassian jira transition` — Transition an issue

Resolves a name or ID to a transition and performs it. If the name/ID is not found, exits non-zero listing all available transitions.

```bash
# By name (case-insensitive)
atlassian jira transition FOO-123 "In Progress"
atlassian jira transition FOO-123 Done

# By numeric ID
atlassian jira transition FOO-123 21

# List transitions first, then transition
atlassian jira transitions FOO-123
atlassian jira transition FOO-123 "Code Review"
```

Prints the slim JSON of the updated issue (including new status) to stdout on success.

**If the transition name is not found:**
```
unknown transition "Foo" for FOO-123
Available transitions:
  id=11 name=To Do
  id=21 name=In Progress
  id=31 name=Done
```

### `atlassian confluence create` — Publish a Confluence page

Creates a new page. Prints slim JSON of the created page (including its ID and version) to stdout.

```bash
# From a markdown file
atlassian confluence create \
  --space=ENG \
  --title="Architecture Decision: Use PostgreSQL" \
  --body-file=architecture-decision.md

# From stdin
cat investigation-notes.md | \
  atlassian confluence create \
    --space=ENG \
    --title="Investigation: Slow queries 2024-05-10" \
    --body-file=-

# Nested under a parent page
atlassian confluence create \
  --space=ENG \
  --title="2024-05-10 Runbook" \
  --parent=123456789 \
  --body-file=runbook.md
```

Flags:
- `--space KEY` (required): Confluence space key.
- `--title TEXT` (required): Page title.
- `--body-file PATH` (required unless `--adf` or `--storage`): Markdown body (- for stdin).
- `--parent pageId`: Parent page ID for nesting.
- `--adf`: Read raw ADF JSON from stdin.
- `--storage`: Read raw storage-format XHTML from stdin.

### `atlassian confluence update` — Update a Confluence page

Fetches the current version automatically and pushes the update. **You do not pass a version number.**

```bash
# Update body only (from stdin)
printf '# Updated\n\nNew content here.' | \
  atlassian confluence update 123456789 --body-file=-

# Update title only (body is preserved — fetched and re-sent automatically)
atlassian confluence update 123456789 --title="New Title"

# Update both title and body
atlassian confluence update 123456789 \
  --title="Revised Architecture Decision" \
  --body-file=updated-doc.md
```

**Version conflict (HTTP 409):** If the page was modified by someone else between when you fetched it and when you pushed, you get:
```
page was modified concurrently — refetch and retry
```
Fetch the page again, incorporate the changes, and retry.

Flags: `--title TEXT`, `--body-file PATH` (- for stdin), `--adf`, `--storage`.
At least one flag must be provided.

---

## Worked example: fetch-edit-push a Confluence page

```bash
# 1. Find the page
PAGE_ID=$(atlassian confluence search 'space = ENG AND title = "Architecture Overview"' \
  | jq -r '.results[0].id')

# 2. Fetch the current body as markdown
atlassian confluence get "$PAGE_ID" | jq -r '.body_markdown' > /tmp/page.md

# 3. Edit the markdown (add a new section, fix a typo, etc.)
echo -e "\n## Updated 2024-05-10\n\nAdded new service diagram." >> /tmp/page.md

# 4. Push the update (version is fetched automatically)
atlassian confluence update "$PAGE_ID" --body-file=/tmp/page.md

# 5. Confirm
atlassian confluence get "$PAGE_ID" | jq '{id, title, version}'
```

---

## Slim output

All commands except `--full` apply these drop-key sets:

- **Universal** (all responses): `expand`, `self`, `iconUrl`, `avatarUrl`, `avatarUrls`, `avatarId`, `picture`, `schema`
- **Jira issues**: additionally drops `renderedFields`, `operations`, `permissions`, `transitions`, `watchers`, `worklog`, `attachments`, `properties`, `names`, `subtask`, `hierarchyLevel`, `editmeta`, `versionedRepresentations`, `colorName`
- **Confluence pages**: additionally drops `_links`, `status`, `lastModified`
- **Search/list responses**: additionally drops `body`, `description`, `content`, `comments`, `comment`, `changelog`, `history`, `adf`
- **`whoami`**: additionally drops `account_status`, `characteristics`, `last_updated`, `created_at`, `nickname`, `locale`, `extended_profile`, `account_type`, `email_verified`
- **`resources`**: additionally drops `scopes`, `url`

---

## Error handling

- **Missing env var**: single-line error naming the variable, exits non-zero.
- **HTTP 4xx**: single-line `<status> <method> <path>: <message>` on stderr, exits non-zero. No retry.
- **HTTP 5xx**: one retry with 1s backoff, then the same `<status> <method> <path>: <message>` error, exits non-zero.
- **HTTP 409 (version conflict on Confluence update)**: special message "page was modified concurrently — refetch and retry".
- **Invalid `--parent` page**: Atlassian's error message is surfaced verbatim.
- **Unsupported markdown construct**: named construct + line number, exits non-zero before any network call.
- **Empty body**: "empty body: no content to send", exits non-zero before any network call.
- **Mutually exclusive flags**: clear error naming the conflicting flags, exits non-zero.
- **No fields to update** (`jira update`, `confluence update` with no flags): "no fields to update" error, exits non-zero.

**Security:** The request body is never printed to stdout or stderr at default verbosity. Bodies may contain confidential ticket content.

---

## Piping with jq

```bash
# Extract issue key from create output
atlassian jira create --project=FOO --type=Task --summary="Foo" | jq -r '.key'

# Extract page ID from confluence create
atlassian confluence create --space=ENG --title="Page" --body-file=page.md | jq -r '.id'

# Check new status after transition
atlassian jira transition FOO-123 Done | jq '.fields.status.name'

# Extract description as markdown
atlassian jira get FOO-123 | jq -r '.fields.description_markdown'

# Get transition IDs
atlassian jira transitions FOO-123 | jq '.transitions[] | "\(.id): \(.name)"'
```
