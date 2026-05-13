# Upstream Sync State — pi Atlassian MCP Extension

This extension is original code (not vendored from an upstream); it was written
for issue #1583. This file documents the auth flow rationale, token storage,
and the slim-field-drop provenance.

## Auth method

**Chosen: OAuth PKCE + dynamic client registration via the Atlassian MCP auth server.**

### Why not API token (ATLASSIAN_EMAIL + ATLASSIAN_API_TOKEN)?

The `mcp.atlassian.com` server accepts HTTP Basic auth (`base64(email:token)`) but
the API token path only exposes **2 Teamwork Graph tools**:
- `getTeamworkGraphContext`
- `getTeamworkGraphObject`

These are read-only graph traversal tools — not the Jira/Confluence CRUD surface
the extension needs (search, create, update, comment, transition, etc.).

Verified empirically on 2026-05-14 against `https://mcp.atlassian.com/v1/mcp` with
`ATLASSIAN_SITE=thankyoupayroll.atlassian.net` and valid API token credentials:

```
Total tools (Basic auth): 2
  - getTeamworkGraphContext
  - getTeamworkGraphObject
```

The Teamwork Graph tools also return `"You don't have permission to connect via API
token. Please ask your organization admin for access."` on actual invocation, so the
API token path is functionally unavailable for this site.

### OAuth path

The Atlassian MCP auth server uses standard OAuth 2.0 + PKCE with dynamic client
registration. Discovery endpoint:
`https://mcp.atlassian.com/.well-known/oauth-authorization-server`

With a valid OAuth Bearer token, `tools/list` returns **31 tools** including the
full Jira and Confluence CRUD surface:
- `atlassianUserInfo`, `getAccessibleAtlassianResources`
- `getConfluencePage`, `searchConfluenceUsingCql`, `getConfluenceSpaces`,
  `getPagesInConfluenceSpace`, `getConfluencePageFooterComments`,
  `getConfluencePageInlineComments`, `getConfluenceCommentChildren`,
  `getConfluencePageDescendants`, `createConfluencePage`, `updateConfluencePage`,
  `createConfluenceFooterComment`, `createConfluenceInlineComment`
- `getJiraIssue`, `editJiraIssue`, `createJiraIssue`, `getTransitionsForJiraIssue`,
  `getJiraIssueRemoteIssueLinks`, `getVisibleJiraProjects`,
  `getJiraProjectIssueTypesMetadata`, `getJiraIssueTypeMetaWithFields`,
  `addCommentToJiraIssue`, `transitionJiraIssue`, `searchJiraIssuesUsingJql`,
  `lookupJiraAccountId`, `addWorklogToJiraIssue`, `getIssueLinkTypes`,
  `createIssueLink`, `search`, `fetch`

### Auth server endpoints

| Endpoint | URL |
|---|---|
| Discovery | `https://mcp.atlassian.com/.well-known/oauth-authorization-server` |
| Authorization | `https://mcp.atlassian.com/v1/authorize` |
| Token | `https://cf.mcp.atlassian.com/v1/token` |
| Client registration | `https://cf.mcp.atlassian.com/v1/register` |

### Token storage

OAuth tokens are stored in `~/.pi/agent/atlassian-mcp-oauth.json` (mode 0o600).
The file contains:

```json
{
  "accessToken": "<bearer>",
  "refreshToken": "<refresh>",
  "expiresAt": 1234567890000,
  "clientId": "<dynamic-client-id>"
}
```

The extension reads this file on every `session_start`. If the token is expired, it
attempts a refresh using `refreshToken` before falling back to a fresh OAuth login.

A fresh login requires interactive user action (browser-based PKCE flow). The local
callback server listens on `localhost:3737/oauth/callback`.

To trigger a fresh login, run `/login-atlassian` from within a pi session.

## Slim field-drop provenance

The `slim.ts` file is a TypeScript port of the drop-key logic from:
`modules/programs/prism/opencode/mcp-atlassian-slim-proxy.mjs` lines 12–115.

The drop-key sets (`UNIVERSAL_DROP_KEYS`, `JIRA_ISSUE_DROP_KEYS`,
`CONFLUENCE_DROP_KEYS`, `USER_INFO_DROP_KEYS`, `RESOURCE_DROP_KEYS`) and the
`slimJson` recursive traversal function were ported verbatim with only the
JavaScript→TypeScript adaptation (explicit types, no process.env drops).

The opencode-specific debug-file writing (`fs.appendFileSync` to
`~/.local/state/opencode/mcp-*.jsonl`) was intentionally stripped; this extension
uses the same `ATLASSIAN_MCP_DEBUG=1` env-var gate for stderr logging only.

## MCP transport

The `mcp-client.ts` file is a hand-rolled Streamable HTTP MCP client using Node.js
built-ins only. The `@modelcontextprotocol/sdk` is not bundled in pi's dependency
tree (verified against pi-coding-agent 0.72.1), so it cannot be imported.

The transport implements:
- `initialize` → get session ID from `Mcp-Session-Id` response header
- `notifications/initialized` → best-effort notification
- `tools/list` → enumerate all tools
- `tools/call` → invoke a tool and return content array

Session IDs returned in `Mcp-Session-Id` headers are forwarded on subsequent
requests to enable server-side session affinity.

If the `@modelcontextprotocol/sdk` ever becomes available in pi's dependency tree,
the `McpSession` class could be replaced with the SDK's `StreamableHTTPClientTransport`.

## Future updates

When Atlassian adds new tools to `mcp.atlassian.com`:
- No code changes needed — tools are enumerated dynamically via `tools/list` at startup.
- Drop-key patterns in `optionsForTool` may need extension if the new tools have
  verbose fields not yet covered by the existing patterns.

When the slim-proxy drop keys are updated in `mcp-atlassian-slim-proxy.mjs`:
1. Port the change to `slim.ts`.
2. Update the tests in `slim.test.ts`.
3. No UPSTREAM.md SHA to update (this is original code, not vendored).
