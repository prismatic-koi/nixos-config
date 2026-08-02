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

The extension reads this file when the family is activated (see "Deferred
registration" below), not at `session_start`. If the token is expired, it
attempts a refresh using `refreshToken` before reporting that a fresh OAuth
login is needed.

A fresh login requires interactive user action (browser-based PKCE flow). The local
callback server listens on `localhost:3737/oauth/callback`.

To trigger a fresh login, run `/login-atlassian` from within a pi session.

## Deferred registration (issue #2532)

The extension registers exactly ONE tool at `session_start`:
`activate_atlassian`. It reads no token file and opens no connection.
Everything else — token resolution, the connection to `mcp.atlassian.com`,
`tools/list`, and the registration of all ~31 tools plus the synthetic
`transitionJiraIssueByName` — happens on the first call to
`activate_atlassian`.

Why: tool schemas sit in the Anthropic `tools` array, the first segment of the
cached prompt prefix, so every session on this machine paid for the Atlassian
surface whether or not it ever touched Jira. See issue #2531 for the
measurement and `../grafana/UPSTREAM.md` for the full mechanism note; the
shared state machine lives in `../mcp-activation/activation.ts`.

`nx.programs.prism.pi.atlassian.eagerRoles` names the agent roles that skip the
tool call and activate from their first `before_agent_start`. It defaults to
`[ "coordinator" ]`: the coordinator files Jira tickets in most sessions, so it
would pay the activation cost nearly every time. Workers and review agents get
one tool schema instead.

The role is read with `pi.getFlag("agent")`, which pi binds AFTER every
extension factory has returned, so it is not readable in a factory prologue.
This extension calls `pi.registerFlag("agent", ...)` itself, because pi scopes
`getFlag` to the registering extension.

`/login-atlassian` now registers through the SAME gateway. Before #2532 a login
after a successful startup re-registered every tool. pi tolerates that —
`registerTool` overwrites by name — but each re-registration calls
`refreshTools()` and so costs a full prompt-cache write. The gateway reports
"already active" instead.

NIX LAYOUT NOTE. `mcp-activation` is copied into this extension's derivation
and this extension's files move down one level, so the store tree is
`$out/atlassian/index.ts` next to `$out/mcp-activation/activation.ts`. That is
what makes the relative import `../mcp-activation/activation.ts` resolve
identically in the source tree and in the nix store.

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
