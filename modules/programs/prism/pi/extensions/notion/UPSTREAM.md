# Upstream Sync State — pi Notion MCP Extension

This extension is original code (not vendored from an upstream); it was written
for issue #2448. It mirrors the structure of `../atlassian/` but **deliberately
diverges** in `auth.ts` and `retry.ts` — see
[Refresh-token rotation hazard](#refresh-token-rotation-hazard), which is the
most important section in this file.

## Auth method

**Chosen: OAuth 2.0 Authorization Code + PKCE (S256) against Notion's hosted
remote MCP server, with persisted RFC 7591 dynamic client registration.**

### Why not the API-token path?

Notion does offer bearer-token auth against `api.notion.com/v1/*`
(`Authorization: Bearer ntn_…` plus a `Notion-Version:` header), but MCP over
that path means running the open-source `notion-mcp-server` package, and
Notion's own documentation deprecates it:

> **Warning:** The open-source `notion-mcp-server` package is no longer
> actively maintained. We recommend using the remote Notion MCP server for the
> best experience. Issues and pull requests on the open-source repository are
> not actively monitored.
>
> — <https://developers.notion.com/guides/mcp/hosting-open-source-mcp.md>

Notion's client-integration guide repeats the warning against the
"pre-configured Notion connector … which uses the deprecated
`notion-mcp-server` package".

It is also a **stdio** server requiring `npx`/node at runtime, which is a poor
fit here — see the `~/.npm` mount rationale at
`prism/internal/container/mounts.go:186-207`; npx-fetched tooling was already a
friction point.

There is a second, security-shaped argument. `collectSecretsDAllowlistNames()`
(`sandbox_exec.go`) is the named re-allow inventory for the `secrets.d` deny
introduced in #2211, and its doc comment is explicit: *"Do NOT add a source
here merely because a secret exists — only because an in-sandbox consumer reads
it."* The OAuth path reads no sops secret, so **this change adds no entry to
that inventory and wires in no sops secret**. Choosing the token path would
have meant widening the `secrets.d` allowlist for the first time since #2245.

**Note for future maintainers:** this repo already holds a Notion API key in
sops — `notion_shopping_list_key`, read by
`modules/desktop/rofi/add-to-shopping-list.nix`. It is a narrow single-block
integration token, it is **not** suitable for a general MCP surface, and
reusing it would drag a secret into the sandbox for no benefit. Do not wire it
in here.

### Auth server endpoints

RFC 8414 metadata from `https://mcp.notion.com/.well-known/oauth-authorization-server`,
verified live on 2026-07-27:

| Endpoint | URL |
|---|---|
| Issuer | `https://mcp.notion.com` |
| Authorization | `https://mcp.notion.com/authorize` |
| Token | `https://mcp.notion.com/token` |
| Client registration | `https://mcp.notion.com/register` |
| Revocation | `https://mcp.notion.com/token` |
| Introspection | `https://mcp.notion.com/introspect` |

Other advertised metadata:

| Field | Value |
|---|---|
| `scopes_supported` | `["default"]` |
| `code_challenge_methods_supported` | `["plain", "S256"]` |
| `token_endpoint_auth_methods_supported` | `["client_secret_basic", "client_secret_post", "none"]` |
| `grant_types_supported` | `["authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"]` |
| `client_id_metadata_document_supported` | `true` |

`plain` is advertised but **must never be used** — `makeAuthorizeUrl()`
hardcodes `S256` and `auth.test.ts` asserts it.

There is a single `default` scope, so there is no scope-selection logic. There
is also no cloudId analogue: **the OAuth grant fixes the workspace**, which is
why every piece of Atlassian cloudId machinery was dropped from
`mcp-client.ts`.

### RFC 9728 protected-resource metadata

An unauthenticated `POST https://mcp.notion.com/mcp` answers:

```
HTTP/2 401
www-authenticate: Bearer realm="OAuth",
  resource_metadata="https://mcp.notion.com/.well-known/oauth-protected-resource/mcp",
  error="invalid_token"
```

and that document reads:

```json
{ "resource": "https://mcp.notion.com/mcp",
  "authorization_servers": ["https://mcp.notion.com"],
  "scopes_supported": ["default"],
  "bearer_methods_supported": ["header"],
  "resource_name": "Notion MCP (Beta)" }
```

Like the Atlassian extension, the endpoints above are **hardcoded** rather than
discovered at runtime. The discovery documents are recorded here so a future
maintainer can re-verify them cheaply.

### Dynamic client registration — verified probe

```
POST https://mcp.notion.com/register
  {"redirect_uris":["http://localhost:3738/oauth/callback"],
   "token_endpoint_auth_method":"none",
   "grant_types":["authorization_code","refresh_token"],
   "response_types":["code"], "client_name":"pi-notion-mcp-probe", …}
→ HTTP 201
  {"client_id":"egkQ8hkrnln1eacS", "token_endpoint_auth_method":"none",
   "registration_client_uri":"/register/egkQ8hkrnln1eacS",
   "client_id_issued_at":1785114930, …}
```

DCR works unauthenticated with `token_endpoint_auth_method: "none"`, so there
is no client secret to store.

## Refresh-token rotation hazard

**This is why `auth.ts` is not a copy of `atlassian/auth.ts`. Read this before
changing anything in the refresh path.**

Notion MCP is built on Cloudflare's `workers-oauth-provider`. From
<https://developers.notion.com/guides/mcp/build-mcp-client.md>:

- Access tokens last ~8 hours (subject to change — use `expires_in`).
- Refresh tokens expire at **180 days absolute** from first authorization
  (non-sliding), **or 30 consecutive days of inactivity**, whichever comes
  first. "Treat periodic reconnection as expected, not exceptional."
- **Every refresh rotates the refresh token.** At most two are valid at once
  (current + immediately previous — a one-step window).

And, verbatim:

> **Reusing a refresh token that has already been rotated away can revoke the
> entire connection.** … replaying a refresh token that was rotated out more
> than a brief grace period earlier is treated as a stolen-token signal: the
> server revokes the whole grant.
>
> * Serialize refreshes per connection with a mutex or distributed lock. Never
>   refresh the same connection from two workers or replicas concurrently —
>   distributed setups that share a connection without a consistent, atomic
>   token store are the most common cause of accidental reuse.
> * Treat `invalid_grant` as terminal … Do **not** retry a refresh that
>   returned `invalid_grant`.

Plus:

> Persist dynamic client registration credentials (`client_id` and
> `client_secret`) durably and reuse them, because re-registering orphans prior
> grants.

### Why this is acute for prism specifically

Prism routinely runs many concurrent pi sessions against **one shared token
file**: a worker, five review agents and a coordinator is an ordinary
afternoon. The Atlassian extension has:

- no locking of any kind,
- non-atomic `writeFileSync` token writes (a reader can observe a truncated
  file), and
- a 60-second pre-expiry margin, which makes simultaneous same-second refresh
  across sessions *likely*, not unlikely.

For Atlassian a lost race costs a transient 401 that `retry.ts` absorbs. For
Notion it costs **the whole grant** — every session breaks and a human must
re-authorize in a browser.

### The four mechanisms that make it safe

| # | Mechanism | Where | Test |
|---|---|---|---|
| 1 | Cross-process `mkdir(2)` lock held across the **entire** read-refresh-write window, with stale-lock breaking | `acquireTokenLock` / `withTokenLock`, used by `getValidAccessToken` and `loginNotion` | `auth.test.ts` → "cross-process refresh lock" |
| 2 | Mandatory cache-bypassing **re-read after acquiring the lock**, before deciding to refresh | `getValidAccessToken` step 3 | same block — "skips the refresh entirely when a peer rotated while we queued" |
| 3 | **Atomic writes**: temp file created `O_EXCL` + `fchmod 0600` *before* any content, `fsync`, then `rename(2)` | `writeJsonAtomic` | `auth.test.ts` → "atomic token writes" |
| 4 | **`invalid_grant` is terminal**: clear the store, prompt `/login-notion`, never retry | `refreshTokens` → `NotionAuthTerminalError`; `retry.ts` → `isTerminalAuthError` | `auth.test.ts` → "invalid_grant is terminal"; `retry.test.ts` → "terminal auth errors" |

Plus a fifth, supporting one: the DCR `client_id` is **persisted in a separate
file and reused** (`ensureClientRegistration`), and that file deliberately
survives `clearTokens()`.

Two further deviations from the Atlassian template, both deliberate:

- The pre-expiry margin is **5 minutes**, not 60 seconds, per Notion's
  "refresh 5-10 minutes before expiry" guidance.
- If the lock cannot be acquired, `getValidAccessToken` falls back to a
  **read-only** reload and otherwise fails the call. It never refreshes
  unserialised — that is the exact thing that revokes the grant.

Each of these has a revert-and-watch-fail pair recorded in the test file
headers, per the repo's AGENTS.md discipline.

### Lock file location and layout

`<token-store-path>.lock/` — a directory, with an `owner` file inside carrying
`{ id, pid, ts }`. `mkdir(2)` is the atomic primitive (the same one
`proper-lockfile` uses). A lock older than `PI_NOTION_LOCK_STALE_MS`
(default 30 s) is broken, but only if its owner id has not changed since the
age was observed. The acquire timeout (`PI_NOTION_LOCK_TIMEOUT_MS`,
default 45 s) is deliberately **greater than** the stale timeout, so a crashed
peer's lock is always broken before we give up on it.

Both env vars exist for tests. Do not set them in production config.

The lock lives in `~/.pi/agent/`, which both isolators already expose
read-write — bwrap dir-binds it (`pi_invocation.go`), sandbox-exec grants
`(subpath "<home>/.pi/agent")` (`sandbox_exec.go` §6a). Notably `mkdir` on that
parent is *already* required by `proper-lockfile`, which is why the bwrap bind
is RW rather than RO (`pi_invocation.go:566-586`). No sandbox change was
required for this extension.

## Token storage

| File | Contents |
|---|---|
| `notion-mcp-oauth.json` | `{ accessToken, refreshToken, expiresAt, clientId }` |
| `notion-mcp-client.json` | `{ clientId, redirectUri, registeredAt }` |

Both are mode 0600 in a 0700 directory. Path precedence (`getTokenStorePath`):

1. `$PI_NOTION_TOKENS` — explicit override (tests, manual escape hatch)
2. `$PI_CODING_AGENT_DIR/notion-mcp-oauth.json`
3. `~/.pi/agent/notion-mcp-oauth.json`

The middle entry is load-bearing. The bwrap dispatcher **dir**-binds the host's
`~/.pi/agent` at `$PI_CODING_AGENT_DIR`; reading through the dir-bind observes
dentry updates from peer writes (i.e. our `rename(2)`), whereas a host-path
**file**-bind would pin the pre-swap inode and we would never see a peer's
rotation. This is also why the optional host-path token bind in
`pi_invocation.go:630-660` (which the Atlassian extension has) was deliberately
**not** mirrored: the shared dir-bind already exposes the file, and adding a
file-bind would reintroduce exactly the inode-pinning hazard the precedence
order exists to avoid.

`home.persistence."/persist".directories = [ ".pi" ]` in `pi.nix` already covers
both files on impermanent NixOS hosts.

## Repo scoping

`scope.ts` gates tool registration on the session's working directory against
`NOTION_MCP_REPOS` (colon-separated paths, from
`nx.programs.prism.pi.notion.repos`). Unset or empty means unrestricted; any
error resolving the working directory or evaluating the allowlist **fails
closed**.

The allowlist is delivered through `nx.programs.prism.agent.envVars`, **not**
the zsh alias. The alias only reaches interactive shells;
`agent.envVars` is the channel that actually reaches prism-spawned agents (it
is serialised as `agent_env_vars` in profiles.json and applied by all three
isolators). `ATLASSIAN_DEFAULT_CLOUD_ID` goes through the alias and therefore
does *not* reach spawned agents — that is a pre-existing gap in the Atlassian
integration and a precedent this extension deliberately does not copy.

`process.cwd()` is used rather than `$PRISM_WORKTREE`: it says the same thing
in every mode we run in (bwrap passes `--chdir <worktree>`; sandbox-exec and
host mode both start in the worktree) but cannot be pointed elsewhere by an
environment variable.

This is a scoping and least-privilege control, not a security boundary against
a hostile agent.

## MCP transport

`mcp-client.ts` is a hand-rolled Streamable HTTP MCP client using Node.js
built-ins only. `@modelcontextprotocol/sdk` is not in pi's dependency tree
(verified against pi-coding-agent 0.82.1) so it cannot be imported, even though
Notion's own client guide recommends it.

| Transport | URL |
|---|---|
| Streamable HTTP (used) | `https://mcp.notion.com/mcp` |
| SSE (fallback, unused) | `https://mcp.notion.com/sse` |

Overridable via `NOTION_MCP_URL`. The transport implements `initialize`,
`notifications/initialized` (best-effort), `tools/list` and `tools/call`, and
replays the `Mcp-Session-Id` response header on subsequent requests for
server-side session affinity.

Everything Atlassian-specific was dropped: `getDefaultCloudId`,
`rewriteDescription`, `rewriteGetCloudIdDescription`, `injectDefaultCloudId`,
and the Jira-transitions REST fallback. There are no synthetic tools (no Notion
analogue of `transitionJiraIssueByName`).

## Tool surface

Tools are `notion-`-prefixed and kebab-case: `notion-search`, `notion-fetch`,
`notion-create-pages`, `notion-update-page`, `notion-move-pages`,
`notion-duplicate-page`, `notion-create-database`,
`notion-update-data-source`, `notion-query-data-sources`, and more. pi applies
no tool-name validation, so the hyphens are fine.

Two accepted postures worth stating out loud rather than letting them pass
silently:

- **Schema passthrough.** `Type.Unsafe` forwards the raw MCP JSON Schema to the
  model and arguments straight to Notion, unvalidated by TypeBox. Same posture
  as the Atlassian extension; the MCP server is the validating authority.
- **Destructive tools are registered knowingly.** A Notion grant is full
  workspace read/write, and `notion-update-page`, `notion-move-pages` and
  `notion-duplicate-page` are part of the surface. The repo-scoping gate is the
  mitigation.

## Known follow-ups

- **`slim.ts` is a passthrough stub.** The Atlassian drop-key sets are entirely
  Atlassian-shaped (`expand`, `avatarUrls`, `renderedFields`, `_links`) and none
  of them appear in Notion responses. Copying them across would be a no-op at
  best and silent data loss at worst. The Notion targets — `notion-fetch` page
  content, database schema blobs — need to be derived empirically against real
  responses. See the header of `slim.ts`.
- **`current_tool_access` filtering.** `notion-fetch` with the special id
  `self` returns `current_tool_access`, a per-tool map of
  `available` / `limited_free_trial` / `upgrade_required` / `not_enabled`.
  Notion advises consulting it because "Tools are listed on every plan" — i.e.
  `tools/list` over-reports, and some registered tools will only ever return
  upgrade prompts. Filtering registration by it is a cheap improvement, but it
  adds a second network call to `session_start` and so a second failure mode;
  it was left out of the initial change on scope grounds.
- **Ephemeral callback port.** The callback server binds a fixed
  `127.0.0.1:3738` (3737 is taken by the Atlassian extension). Moving to port 0
  would need the DCR call reordered after the bind, since the redirect URI must
  be registered before the server listens.

## Future updates

When Notion adds new tools to `mcp.notion.com`, no code change is needed —
tools are enumerated dynamically via `tools/list` at startup.

If the endpoints in this file ever change, re-verify against
`https://mcp.notion.com/.well-known/oauth-authorization-server` and update both
the constants in `auth.ts` and the table above.

There is no upstream SHA to track: this is original code, not vendored.
