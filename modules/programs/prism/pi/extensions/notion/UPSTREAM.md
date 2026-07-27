# Upstream Sync State — pi Notion MCP Extension

This extension is original code (not vendored from an upstream); it was
written for issue #2448. This file documents the auth flow rationale,
token storage, the concurrency-critical refresh path, and the verified
endpoint set.

## Auth method

**Chosen: OAuth PKCE + persisted dynamic client registration against the
hosted Notion MCP server.**

### Why not the token path (`notion-mcp-server` + `api.notion.com`)?

Notion also supports Bearer-token auth against `api.notion.com/v1/*`
(`Authorization: Bearer ntn_...` + `Notion-Version` header) — but the
MCP-over-token route is the open-source `notion-mcp-server` package, and
Notion's own docs
(`https://developers.notion.com/guides/mcp/hosting-open-source-mcp.md`)
carry a deprecation warning:

> **Warning:** The open-source `notion-mcp-server` package is no longer
> actively maintained. We recommend using the remote Notion MCP server
> for the best experience. Issues and pull requests on the open-source
> repository are not actively monitored.

The token path is also a stdio server requiring `npx` / node at runtime,
which is a poor fit for a vendored TypeScript extension loaded by pi's
jiti-based extension runtime.

Note: this repo already holds a Notion API key in sops
(`modules/desktop/rofi/secrets/notion.sops.yaml` →
`notion_shopping_list_key`), used by
`modules/desktop/rofi/add-to-shopping-list.nix` for a narrow single-block
integration. It is **not** suitable for a general MCP surface and must
not be reused here. This extension adds no sops secret; the entire OAuth
flow runs against Notion's hosted MCP server with no host-side secret
material.

### Endpoints (verified 2026-07-27)

RFC 8414 authorization-server metadata
(`https://mcp.notion.com/.well-known/oauth-authorization-server`):

| Field | Value |
|---|---|
| `issuer` | `https://mcp.notion.com` |
| `authorization_endpoint` | `https://mcp.notion.com/authorize` |
| `token_endpoint` | `https://mcp.notion.com/token` |
| `registration_endpoint` | `https://mcp.notion.com/register` |
| `revocation_endpoint` | `https://mcp.notion.com/token` |
| `introspection_endpoint` | `https://mcp.notion.com/introspect` |
| `scopes_supported` | `["default"]` |
| `code_challenge_methods_supported` | `["plain", "S256"]` |
| `token_endpoint_auth_methods_supported` | `["client_secret_basic","client_secret_post","none"]` |
| `grant_types_supported` | `["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:jwt-bearer"]` |

RFC 9728 protected-resource metadata
(`https://mcp.notion.com/.well-known/oauth-protected-resource/mcp`):

```json
{ "resource": "https://mcp.notion.com/mcp",
  "authorization_servers": ["https://mcp.notion.com"],
  "scopes_supported": ["default"],
  "bearer_methods_supported": ["header"],
  "resource_name": "Notion MCP (Beta)" }
```

Dynamic client registration works unauthenticated. Verified with:

```
POST https://mcp.notion.com/register
  {"redirect_uris":["http://localhost:3738/oauth/callback"],
   "token_endpoint_auth_method":"none",
   "grant_types":["authorization_code","refresh_token"],
   "response_types":["code"], "client_name":"pi-notion-mcp-probe"}
→ HTTP 201
  {"client_id":"...", "token_endpoint_auth_method":"none",
   "registration_client_uri":"/register/...", "client_id_issued_at":..., ...}
```

MCP transport:
- Streamable HTTP (recommended): `https://mcp.notion.com/mcp`
- SSE fallback: `https://mcp.notion.com/sse`

## CRITICAL: refresh-token semantics

Notion MCP is built on Cloudflare's `workers-oauth-provider`. From
`https://developers.notion.com/guides/mcp/build-mcp-client.md`:

- Access tokens last ~8 hours (`expires_in` is authoritative).
- Refresh tokens expire at 180 days absolute (non-sliding) OR after 30
  consecutive days of inactivity, whichever first. Then the OAuth server
  returns `invalid_grant` and the user must re-authorise.
- **Every refresh rotates the refresh token.** At most two are valid at
  once (current + immediately previous).
- Verbatim upstream warning:

  > **Reusing a refresh token that has already been rotated away can
  > revoke the entire connection.** … replaying a refresh token that was
  > rotated out more than a brief grace period earlier is treated as a
  > stolen-token signal: the server revokes the whole grant.
  >
  > * Serialize refreshes per connection with a mutex or distributed lock.
  >   Never refresh the same connection from two workers or replicas
  >   concurrently — distributed setups that share a connection without
  >   a consistent, atomic token store are the most common cause of
  >   accidental reuse.
  > * Treat `invalid_grant` as terminal … Do **not** retry a refresh
  >   that returned `invalid_grant`.
- Also: "Persist dynamic client registration credentials (`client_id`
  and `client_secret`) durably and reuse them, because re-registering
  orphans prior grants."

Prism routinely runs many concurrent pi sessions (worker + 5 reviewers +
coordinator) against a single token file. `auth.ts` in this extension
therefore adds, over and above the Atlassian pattern:

1. **Cross-process lock** held across the entire read-refresh-write
   window. After acquiring, the token file is re-read from disk so a
   peer refresh that raced with us is observed and we skip the refresh
   entirely. (`acquireRefreshLock` / `releaseRefreshLock`.)
2. **Atomic token writes** — content is written to a temp file created
   with mode 0o600, then `rename()`d into place. Concurrent readers
   never observe a partial file. (`saveTokens`.)
3. **Terminal `invalid_grant`** — the OAuth error is surfaced as
   `InvalidGrantError`, the stored tokens are cleared, and no retry is
   attempted. `retry.ts` also short-circuits on `InvalidGrantError` so
   the tool call fails cleanly. (`refreshTokens` +
   `getValidAccessToken` + `callToolWithAuthRetry`.)
4. **Persisted DCR `client_id`** in a separate `notion-mcp-client.json`
   file. The token file is cleared on `invalid_grant`; the client file
   is not. `ensureClientRegistration` reuses the stored ID rather than
   re-registering. (`loadClientRegistration` /
   `saveClientRegistration` / `ensureClientRegistration`.)
5. **Proactive refresh margin** of 5 minutes (`REFRESH_MARGIN_MS`) —
   `expiresAt` is set to `issue_time + expires_in - 5min` so the
   refresh happens well before the token dies rather than at the wire.

## Token storage

| File | Content | Mode |
|---|---|---|
| `notion-mcp-oauth.json` | `{accessToken, refreshToken, expiresAt, clientId}` | 0o600 |
| `notion-mcp-client.json` | `{clientId, redirectUri}` | 0o600 |
| `notion-mcp-oauth.lock` | Ephemeral cross-process lock (PID content) | 0o600 |

All three live in `$PI_CODING_AGENT_DIR` (bwrap dir-bind of the host
`~/.pi/agent`, which is also the resolution when the env var is unset on
the host).

Path resolution precedence:
1. `PI_NOTION_TOKENS` / `PI_NOTION_CLIENT` — test/escape-hatch overrides.
2. `$PI_CODING_AGENT_DIR/notion-mcp-oauth.json` — the path prism's
   dispatchers expose to the sandbox.
3. `~/.pi/agent/notion-mcp-oauth.json` — legacy fallback.

The middle entry matters: bwrap dir-binds host `~/.pi/agent` at
`$PI_CODING_AGENT_DIR`, and reading through the dir-bind sees dentry
updates from peer processes. A file-bind at `homedir()` would pin the
pre-swap inode. See the comment on `saveTokens` for the atomic-rename
reasoning.

## Callback server

- Bound to `127.0.0.1:3738/oauth/callback` (Atlassian uses 3737 — the
  Notion port MUST be different or concurrent `/login-*` flows collide
  on `listen()`).
- 5-minute timeout.
- Closed on every completion path (success, error, state mismatch,
  timeout, cancellation) — `finish()` in
  `createLocalCallbackServer` is the single close-once gate.

## PKCE

- `S256` only. Notion advertises `plain` in
  `code_challenge_methods_supported` but the extension never sends it —
  `makeAuthorizeUrl` hardcodes `code_challenge_method=S256`.
- `state` is a 128-bit UUID (hex-encoded, no dashes) and is compared for
  equality before the code exchange in every accepted path:
  - The local callback server rejects mismatched state with a 400 and
    resolves the wait with `null`.
  - `parseAuthInput` accepts a URL form and a `code#state` form; both
    check state equality. The Atlassian extension's "bare code"
    branch is intentionally dropped for Notion because it bypasses
    state validation.

## Debug logging

Debug output is gated on `NOTION_MCP_DEBUG=1` and writes to stderr only.
No callsite in this extension passes an access token, refresh token, or
Authorization header value into `debug(...)`, `log(...)`, `warn(...)`,
or `errorLog(...)`. The `mcp-client.ts::send()` debug lines log the
method name + params + a truncated 200-char response prefix; the
Authorization header value is never logged.

## Explicitly NOT changed (out-of-scope confirmations)

- No egress allowlist change — none exists. Darwin's SBPL emits
  `(allow network*)` and Linux bwrap shares the host netns. Notion is
  reachable the moment the extension loads.
- No `sandbox_exec.go` change — the `(subpath ~/.pi/agent)` grant
  already covers the new token file and lock file.
- No `mounts.go` change — `~/.mcp-auth` / `~/.npm` are legacy
  `mcp-remote` / npx surfaces.
- No `collectSecretsDAllowlistNames` change — the OAuth path reads no
  sops secret.
- No reuse of `notion_shopping_list_key`.
- No `pi_invocation.go` host-path bind for `notion-mcp-oauth.json` —
  the shared `~/.pi/agent` dir-bind already exposes the file. Skipping
  this bind avoids the Go CI gate for a feature that would be a
  functional no-op.

## Slim field-drop

Not implemented yet. Notion responses can be large (`notion-fetch` page
content, database schema blobs), but the Atlassian drop-key sets are not
appropriate and porting them without empirical response data would be an
unaudited guess. A follow-up can add `slim.ts` once real response
shapes are available. Until then, the extension returns responses
unchanged — this is safe (never drops needed fields) but leaves context
tokens on the table.

## MCP transport

`mcp-client.ts` is a hand-rolled Streamable HTTP MCP client using Node
built-ins only. `@modelcontextprotocol/sdk` is not bundled in pi's
dependency tree. If the SDK ever becomes available, `McpSession` could
be replaced with the SDK's `StreamableHTTPClientTransport`.

The transport implements:
- `initialize` → get session ID from `Mcp-Session-Id` response header
- `notifications/initialized` → best-effort notification
- `tools/list` → enumerate all tools
- `tools/call` → invoke a tool and return content array

Session IDs from `Mcp-Session-Id` are forwarded on subsequent requests
to enable server-side session affinity.

## Repo-scoping gate

`notionEnabledForCwd()` reads `NOTION_MCP_REPOS` (colon-separated list
of prefixes). Semantics:

- Unset or empty → unrestricted (the extension always registers).
- Non-empty → the current working directory must equal, or be a
  subpath of, at least one prefix (after `~/` expansion and symlink
  resolution).
- Any error resolving cwd or evaluating the allowlist → fail closed
  (no tools registered).

The gate reads `PRISM_WORKTREE` first (prism dispatchers inject this
into every isolated shell) and falls back to `process.cwd()`. Login
command registration is repo-agnostic — `/login-notion` is always
available even outside the allowlist so tokens can be seeded from any
context.

## Tool surface (as of issue #2448)

Per `https://developers.notion.com/guides/mcp/mcp-supported-tools.md`,
tools are `notion-` prefixed and kebab-case (`notion-search`,
`notion-fetch`, `notion-create-pages`, `notion-update-page`,
`notion-move-pages`, `notion-duplicate-page`, `notion-create-database`,
`notion-update-data-source`, `notion-query-data-sources`, …).

The full list is enumerated dynamically via `tools/list` at session
start. Filtering by `current_tool_access` (via a `notion-fetch id=self`
probe) is a plausible next optimisation — Notion advertises tools on
every plan even when the account lacks them — but is not implemented
here.

## Future updates

When Notion adds new tools to `mcp.notion.com`:
- No code changes needed — tools are enumerated dynamically at startup.
- If a Notion-specific slim strategy is added later, the drop-key sets
  will need extending.

When Notion changes the auth endpoints:
- Update the three constants at the top of `auth.ts`.
- Refresh this document's endpoint table with the new discovery output.
