---
name: atlassian
description: Load this skill when reading from or writing to Jira Cloud or Confluence Cloud — searching issues, fetching a specific Jira issue or Confluence page, listing or performing workflow transitions, creating/updating issues, commenting on issues, publishing or updating Confluence pages, or understanding the atlassian MCP tools.
---

# Atlassian Skill — Jira and Confluence via pi atlassian-mcp

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

---

## Integration: pi atlassian-mcp extension

Atlassian operations are performed exclusively via the **pi atlassian-mcp extension** — a TypeScript extension bundled with pi that connects to `mcp.atlassian.com` using OAuth PKCE.

**There is no standalone `atlassian` CLI binary.** The old CLI has been removed. Do not attempt to call `atlassian` from a Bash tool.

### Authentication

Authentication is via OAuth PKCE (not API tokens). Tokens are stored in
`~/.pi/agent/atlassian-mcp-oauth.json` and refreshed automatically on expiry.

To log in or re-authenticate, run `/login-atlassian` inside a pi session.
This opens a browser-based authorization flow.

The `ATLASSIAN_SITE`, `ATLASSIAN_EMAIL`, and `ATLASSIAN_API_TOKEN` environment
variables are **not used** by the pi MCP extension and are not forwarded into
agent sandboxes.

### Available tools (31 tools via OAuth)

The extension exposes the full Jira and Confluence CRUD surface:

**Identity / discovery**
- `atlassianUserInfo` — who am I
- `getAccessibleAtlassianResources` — list sites

**Jira**
- `getJiraIssue` — fetch an issue by key
- `searchJiraIssuesUsingJql` — JQL search
- `createJiraIssue` — file a new issue
- `editJiraIssue` — update issue fields
- `addCommentToJiraIssue` — comment on an issue
- `getTransitionsForJiraIssue` — list available transitions
- `transitionJiraIssue` — move an issue to a new status
- `getJiraIssueRemoteIssueLinks` — remote links
- `getVisibleJiraProjects` — list projects
- `getJiraProjectIssueTypesMetadata` — issue type metadata for a project
- `getJiraIssueTypeMetaWithFields` — field metadata for a specific issue type
- `lookupJiraAccountId` — resolve a user to an account ID
- `addWorklogToJiraIssue` — log time
- `getIssueLinkTypes` — list issue link types
- `createIssueLink` — link two issues

**Confluence**
- `getConfluencePage` — fetch a page by ID
- `searchConfluenceUsingCql` — CQL search
- `getConfluenceSpaces` — list spaces
- `getPagesInConfluenceSpace` — list pages in a space
- `getConfluencePageFooterComments` — footer comments
- `getConfluencePageInlineComments` — inline comments
- `getConfluenceCommentChildren` — comment threads
- `getConfluencePageDescendants` — child pages
- `createConfluencePage` — publish a new page
- `updateConfluencePage` — update an existing page
- `createConfluenceFooterComment` — add a footer comment
- `createConfluenceInlineComment` — add an inline comment

**General**
- `search` — cross-product search
- `fetch` — fetch an arbitrary Atlassian URL

---

## Common patterns

### Jira: search and fetch

```
# Search for open issues in a project
searchJiraIssuesUsingJql(jql: "project = FOO AND status != Done ORDER BY created DESC", maxResults: 20)

# Fetch a specific issue
getJiraIssue(issueKey: "FOO-123")
```

### Jira: create an issue

```
createJiraIssue(
  projectKey: "FOO",
  issueType: "Task",
  summary: "Investigate memory leak in worker pool",
  description: "## What happened\n\nSaw OOM errors in production...",
)
```

### Jira: comment and transition

```
# Comment
addCommentToJiraIssue(issueKey: "FOO-123", comment: "Confirmed fixed in staging.")

# List transitions, then transition
getTransitionsForJiraIssue(issueKey: "FOO-123")
transitionJiraIssue(issueKey: "FOO-123", transitionId: "31")
```

### Confluence: fetch-edit-push

```
# 1. Find the page
searchConfluenceUsingCql(cql: 'space = ENG AND title = "Architecture Overview"', limit: 1)

# 2. Fetch its body
getConfluencePage(pageId: "123456789")

# 3. Push an update
updateConfluencePage(
  pageId: "123456789",
  title: "Architecture Overview",
  body: "<updated markdown content>",
)
```

---

## Output slimming

The pi atlassian-mcp extension applies the same field-drop sets as the
prism MCP proxy, removing verbose metadata (`expand`, `self`, `iconUrl`,
`avatarUrls`, `renderedFields`, `operations`, etc.) so tool outputs are
compact enough to fit in context.

---

## Troubleshooting

- **"Not logged in" / auth error**: run `/login-atlassian` in your pi session
  to complete the OAuth flow.
- **Token expired**: the extension refreshes tokens automatically on each
  `session_start`. If refresh fails, `/login-atlassian` again.
- **Permission error on a specific tool**: some Atlassian tools require admin
  scopes. Check whether your account has the necessary Jira/Confluence
  permissions.
- **Debug logging**: set `ATLASSIAN_MCP_DEBUG=1` in the environment to get
  MCP request/response logs on stderr.
