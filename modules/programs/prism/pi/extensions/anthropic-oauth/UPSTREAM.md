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
| `request-body.ts` | pi-ai built-in | *(no equivalent — see divergence 9)* | *(no equivalent)* |
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
   (`URLSearchParams` / `application/x-www-form-urlencoded`), and absence of a
   `User-Agent` header in `makeTokenHeaders()` all mirror griffinmartin's
   `credentials.ts::refreshViaOAuth` exactly. No `User-Agent` is sent on
   token-exchange or token-refresh requests — Anthropic's Cloudflare WAF on
   `claude.ai/v1/oauth/token` blocks requests carrying `User-Agent: claude-code/*`
   with a 429. The `USER_AGENT` constant remains declared and exported for use on
   the API-call path (`index.ts`), but is not included in token-exchange headers.
   The local-callback-server PKCE flow, `loginAnthropic` / `refreshAnthropicToken`
   function signatures, and `pi.registerProvider` wrapper continue to mirror leohenon.
   Wire-format ported in: issue #885, griffinmartin SHA `df1b0cbc9e94ff9a8081ac98aa837893fd2be35e`.
   User-Agent removed in: issue #888.

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

8. **`getAnthropicModels` returns the runtime registry verbatim — no hardcoded
   fallback**. leohenon's `index.ts` carried a `DEFAULT_OPUS_4_7` literal that
   was pushed when the registry lacked opus-4-7. Since pi v0.77.0 the
   `@earendil-works/pi-ai` model catalog ships `claude-opus-4-7`/`-4-8`
   directly, so the fallback was dead code and was removed. Do **not**
   re-introduce a hardcoded model object — rely entirely on the registry.

9. **Adaptive thinking handling lives in `request-body.ts` and is ported from
   pi-ai — NOT from griffinmartin**. Issue #2044 fixed an erratic-behaviour
   bug on `claude-opus-4-8` (and a silent degradation on `-4-6` / `-4-7`)
   where our `streamSimple` was sending the legacy budget-based thinking
   payload (`thinking: {type:"enabled", budget_tokens:N}`) for all anthropic
   models. Models flagged with `compat.forceAdaptiveThinking === true` in the
   pi-ai registry (currently `claude-opus-4-6/4-7/4-8`) REQUIRE the adaptive
   form: `thinking: {type:"adaptive", display:"summarized"}` +
   `output_config: {effort}`.

   As of griffinmartin SHA `ffefe9d` (release 1.5.4, 2026-04), griffinmartin
   upstream has not implemented adaptive thinking — their `transforms.ts`
   still treats effort as a haiku-only strip target. So the port for this
   change came from pi-ai's built-in handler:
   `@earendil-works/pi-ai/src/providers/anthropic.ts`. Key references:

   - `mapThinkingLevelToEffort` (~710-731) — pi reasoning level → Anthropic
     effort, honouring the model's `thinkingLevelMap`.
   - `streamSimpleAnthropic` dispatch (~748-770) — selects adaptive vs
     budget-based based on `compat.forceAdaptiveThinking`.
   - `buildParams` body assembly (~937-981) — the wire-form details:
     `display: "summarized"` on BOTH branches; `output_config: {effort}` for
     adaptive; `budget_tokens` for legacy; and the temperature-suppression
     guard at ~937-940.
   - `createClient` interleaved-thinking gate (~789) — adaptive models have
     interleaved thinking built in, so the
     `interleaved-thinking-2025-05-14` beta header is suppressed. Mirrored in
     our `betas.ts::getModelBetas` via the new `ctx.forceAdaptiveThinking`
     argument.

   When griffinmartin eventually ports adaptive thinking, prefer their
   implementation if it lands in a logical home file (likely a new
   `request-body.ts` analogue or an addition to `transforms.ts`). If their
   shape diverges, keep the pi-ai-faithful behaviour and add a note here.

   Smoke-test references in pi-ai:
   - `test/anthropic-opus-4-8-smoke.test.ts` (live API smoke).
   - `test/anthropic-force-adaptive-thinking.test.ts` (offline wire-form
     capture, including the `forceAdaptiveThinking: false` opt-out case).
   Our offline parity lives in `request-body.test.ts`.

10. **`model-config.ts` `4-8` override and `betas.ts` adaptive
    suppression**: Both changes from issue #2044. The `4-8` entry mirrors
    the existing `4-6`/`4-7` pattern (substring match adds the
    `effort-2025-11-24` beta). The `forceAdaptiveThinking`-driven
    suppression of `interleaved-thinking-2025-05-14` in `getModelBetas` is
    keyed off the model's compat flag at call time — it does NOT rely on
    substring matching, so any future adaptive model from the registry
    benefits automatically without a `model-config.ts` change.

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
