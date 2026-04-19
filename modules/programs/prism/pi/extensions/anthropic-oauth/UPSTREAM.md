# Upstream Sync State

This vendored extension is derived from two upstream repositories. This file
documents the current sync state and the procedure for porting future fixes.

## Source upstreams

| Upstream | URL | Role |
|---|---|---|
| `griffinmartin/opencode-claude-auth` | https://github.com/griffinmartin/opencode-claude-auth | Primary source for business logic: transforms, betas, signing, model-config, logger, credentials (OAuth helpers), anthropic-prompt.txt |
| `leohenon/pi-anthropic-oauth` | https://github.com/leohenon/pi-anthropic-oauth | Source for pi-specific wrapper: index.ts pattern, auth.ts (OAuth PKCE + callback server), stream.ts (message conversion) |

When the two disagree on an implementation detail, **griffinmartin takes priority** —
it is more actively maintained and is the source of the PR #193 fix.

## Current sync commit SHAs

| Upstream | SHA | Date |
|---|---|---|
| `griffinmartin/opencode-claude-auth` main | `df1b0cbc9e94ff9a8081ac98aa837893fd2be35e` | 2026-04 (chore(main): release 1.5.0 #204) |
| `griffinmartin/opencode-claude-auth` PR #193 | `9420fbef60567968bcd21a260db21be9f7dd475b` | 2026-04-14 (the MD5 hash obfuscation approach) |
| `leohenon/pi-anthropic-oauth` | `86d9d97829776a66aec58e3433900173ff7e184a` | 2026-04 (update readme) |

> **Note on PR #193**: At the time of vendoring, griffinmartin PR #193 was open
> but not yet merged into main. The MD5 hash-based tool name obfuscation from
> that PR was incorporated anyway (per the issue spec) because:
> 1. It is strictly superior to the PascalCase approach on griffinmartin's main.
> 2. The blacklisted names (`todowrite`, `background_output`, `background_cancel`)
>    are handled by the general hashing rather than a special-case list.
> When PR #193 eventually merges, bump the griffinmartin SHA to its merge commit.

## File-level mapping

| Our file | Primary upstream | griffinmartin file | leohenon file |
|---|---|---|---|
| `index.ts` | leohenon (pi-specific) | `src/index.ts` (different API, skip when porting) | `src/index.ts` |
| `auth.ts` | leohenon | `src/credentials.ts` (OAuth helpers only, no callback server) | `src/auth.ts` |
| `stream.ts` | leohenon | *(no equivalent — opencode uses different streaming)* | `src/stream.ts`, `src/convert.ts`, `src/prompt.ts` |
| `credentials.ts` | griffinmartin | `src/credentials.ts` | `src/auth.ts` (partial) |
| `transforms.ts` | griffinmartin PR #193 | `src/transforms.ts` | *(no equivalent)* |
| `betas.ts` | griffinmartin | `src/betas.ts` | *(no equivalent)* |
| `signing.ts` | griffinmartin | `src/signing.ts` | *(no equivalent)* |
| `model-config.ts` | griffinmartin | `src/model-config.ts` | *(no equivalent)* |
| `logger.ts` | griffinmartin | `src/logger.ts` | *(no equivalent)* |
| `anthropic-prompt.txt` | griffinmartin | `src/anthropic-prompt.txt` | *(no equivalent)* |

## Known divergences

1. **`auth.ts` token-exchange wire format tracks griffinmartin `credentials.ts::refreshViaOAuth`**.
   The OAuth token endpoint URL (`TOKEN_URL`), request body construction
   (`URLSearchParams` / `application/x-www-form-urlencoded`), and `User-Agent`
   header in `auth.ts` now mirror griffinmartin's `credentials.ts::refreshViaOAuth`
   rather than leohenon's original JSON-body approach. The local-callback-server
   PKCE flow, `loginAnthropic` / `refreshAnthropicToken` function signatures, and
   `pi.registerProvider` wrapper continue to mirror leohenon.
   Ported in: issue #885, griffinmartin SHA `df1b0cbc9e94ff9a8081ac98aa837893fd2be35e`.

2. **`index.ts` is pi-specific and will never match griffinmartin's `index.ts`**.
   griffinmartin's entry point uses opencode's `Plugin` type with `auth.loader`,
   `experimental.chat.system.transform`, and `config` hooks. Pi's extension API
   uses `pi.registerProvider(name, { oauth: {...}, streamSimple: ... })`. These
   are structurally different and cannot be reconciled.

3. **`stream.ts` has no griffinmartin equivalent**. griffinmartin delegates HTTP
   and SSE parsing to the opencode runtime; pi requires explicit streaming.
   Our `stream.ts` replaces leohenon's `@anthropic-ai/sdk`-based streaming with
   native `fetch()` + manual SSE parsing, matching the zero-npm-deps commitment.

4. **`betas.ts` reads `PI_ANTHROPIC_ENABLE_1M_CONTEXT`** instead of griffinmartin's
   `ANTHROPIC_ENABLE_1M_CONTEXT` (which goes through `plugin-config.ts`). Pi has
   no plugin config mechanism, so we read the env var directly.

5. **`logger.ts` default log path** is `~/.pi/agent/pi-anthropic-oauth-debug.log`
   instead of griffinmartin's `~/.local/share/opencode/claude-auth-debug.log`.
   The env var is `PI_ANTHROPIC_OAUTH_DEBUG` instead of `CLAUDE_AUTH_DEBUG`.

6. **`credentials.ts` is single-account only** (no keychain, no multi-account).
   griffinmartin's version supports macOS Keychain + multi-account. Pi stores
   credentials in `~/.pi/agent/auth.json` and is single-account.

7. **MD5 hash obfuscation vs PascalCase**: Our `transforms.ts` uses PR #193's
   MD5 approach (`t_<8hex>`) instead of griffinmartin main's `mcp_` PascalCase
   approach. This is intentional — see PR #193 for the rationale.

## Port procedure for future upstream fixes

When griffinmartin ships a fix you want to port:

1. **Identify what changed**:
   ```bash
   cd /tmp/upstream-griffinmartin
   git fetch origin
   git diff <recorded-sha> origin/main -- src/<filename>.ts
   ```

2. **Apply the equivalent change** to our file of the same name.
   - Skip any changes to `src/index.ts` — our `index.ts` is pi-specific.
   - Skip any changes to `src/keychain.ts` or `src/plugin-config.ts` —
     these have no equivalents here (see divergences above).
   - For `src/credentials.ts` changes: apply the OAuth-helper portions
     (refreshViaOAuth, parseOAuthResponse), skip the keychain/multi-account code.

3. **Update the SHA** in this file to the new griffinmartin HEAD (or the
   specific merge commit if the fix came via PR).

4. **Run the nix build** to verify:
   ```bash
   nix build .#nixosConfigurations.navi.config.system.build.toplevel
   ```

5. **Commit and PR** with `Closes #<issue>` in the body.

When leohenon ships a fix to the pi wrapper or auth flow:

1. `git diff <leohenon-sha> origin/master -- src/<filename>` in leohenon's repo.
2. Apply the equivalent change to `auth.ts` or `stream.ts` as appropriate.
3. Update the leohenon SHA in this file.
4. Note: leohenon fixes to `src/stream.ts` may need adaptation since our
   `stream.ts` does not use `@anthropic-ai/sdk`.
