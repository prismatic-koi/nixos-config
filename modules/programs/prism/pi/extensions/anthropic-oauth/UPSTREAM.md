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
| `griffinmartin/opencode-claude-auth` main | `88b0f793` | 2026-07-08 (v2.0.0 merge commit for PR #240 — 1M-context opt-in removed, base betas regenerated) |
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

4. **~~`betas.ts` reads `PI_ANTHROPIC_ENABLE_1M_CONTEXT`~~** — REMOVED in v2.0.0
   port (issue #2382, PR to close #2382). Griffinmartin dropped the 1M-context
   opt-in entirely in v2.0.0 on the rationale that the API supports 1M
   context natively without the beta flag; we followed suit. `PI_ANTHROPIC_ENABLE_1M_CONTEXT`
   is now inert. Historical shape: `isEnable1mContext()` in `betas.ts` used to
   read the env var and, when combined with `supports1mContext(modelId)`,
   push `context-1m-2025-08-07` into `getModelBetas`. Both helpers are gone;
   `betas.test.ts` has a regression test that sets the env var and asserts the
   beta is NOT injected, guarding against accidental reintroduction. A user
   who still wants the beta can add it manually to `ANTHROPIC_BETA_FLAGS`.

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

   **pi 0.80.8 registry-construction change (issue #2428).** 0.80.8 removed
   `AuthStorage` from the `@earendil-works/pi-coding-agent` barrel
   (`dist/index.d.ts` — only `readStoredCredential` survives from
   `./core/auth-storage.ts`) and dropped the `ModelRegistry.create()` static
   factory. Construction is now:

   ```ts
   import { ModelRegistry, ModelRuntime } from "@earendil-works/pi-coding-agent"
   const runtime = await ModelRuntime.create()
   const registry = new ModelRegistry(runtime)
   await registry.refresh()   // load models.json before sync getAll()
   ```

   `ModelRegistry` is documented as a "synchronous compatibility facade
   exposed to extensions"; `refresh()` is `Promise<void>` and MUST be awaited
   before `getAll()` — skipping it yields an empty list and silently
   registers the anthropic provider with no models.

   Because `ModelRuntime.create()` is async, `getAnthropicModels()` and the
   `export default function (pi)` entry point are both `async`. This is safe:
   pi 0.80.8's extension loader awaits the factory
   (`dist/core/extensions/loader.js` — `await factory(api)`,
   `ExtensionFactory = (pi) => void | Promise<void>`), and
   `pi.registerProvider` calls made during the initial load phase are queued
   and applied once the runner binds its core context (see the JSDoc on
   `ExtensionAPI.registerProvider`).

   The 0.80.8 package `exports` map only exposes `.` and `./rpc-entry`, so a
   deep `./core/...` subpath import of the removed `AuthStorage` is NOT a
   sanctioned escape hatch — only symbols present in the 0.80.8 barrel may
   be imported. If registry construction rejects, surface a clear error
   rather than falling through to an empty-model registration.

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

10. **`betas.ts::getModelBetas` `forceAdaptiveThinking` suppression** —
    the pi-only twist that remains on `betas.ts` (issue #2044). When the
    caller passes `ctx.forceAdaptiveThinking = true` (driven by the model's
    `compat.forceAdaptiveThinking` flag in the pi-ai registry, currently
    `claude-opus-4-6/4-7/4-8`), `getModelBetas` filters out every occurrence
    of `interleaved-thinking-2025-05-14` before returning. Adaptive-thinking
    models have interleaved thinking built in, so the beta header is
    redundant — mirrors pi-ai's `anthropic.ts::createClient` (~line 789).

    The suppression is keyed off the model's compat flag at call time —
    it does NOT rely on substring matching, so any future adaptive model
    from the registry benefits automatically without a `model-config.ts`
    change. Implementation uses `.filter()` (not `indexOf`/`splice`) so
    both occurrences of the duplicate `interleaved-thinking-2025-05-14`
    entry in griffinmartin 2.0.0's regenerated `baseBetas` are removed.

    Historical note: the v1.5.1 port had a companion `4-8` entry in
    `model-config.ts::modelOverrides` that added `effort-2025-11-24` for
    the opus-4-8 substring. Griffinmartin 2.0.0 lifted `effort-2025-11-24`
    into `baseBetas`, so the per-model `4-6`/`4-7`/`4-8` overrides became
    no-ops. The v2.0.0 port drops all three overrides (upstream still
    carries `4-6`/`4-7` as dead entries; we don't). The haiku override
    stays and still excludes `interleaved-thinking-2025-05-14`.

11. **v1.5.1 auth parity (issue #2381) — ported**. griffinmartin PR #207
    (Claude Code 2.1.112 subscription-auth fingerprint) and PR #211
    (`isLongContextError` matches `"You're out of extra usage"`) landed
    upstream in v1.5.1 and were ported here in-place:

    - `model-config.ts`: `ccVersion` bumped to `2.1.112`;
      `advisor-tool-2026-03-01` appended to `baseBetas`.
    - `transforms.ts`: `CLAUDE_CODE_ENTRYPOINT` fallback changed from
      `"cli"` to `"sdk-cli"` (billing header + user-agent alignment).
    - `index.ts`: `getUserAgent()` returns `(external, sdk-cli)`;
      OAuth-mode requests set `anthropic-dangerous-direct-browser-access:
      true` and eight `x-stainless-*` headers (`getStainlessHeaders`
      mirror); `/v1/messages` URL gains `?beta=true` via
      `buildRequestUrl`, applied to both the initial fetch and the 401
      retry. Caller `options.headers` still override our defaults (mirror
      of griffinmartin's `!headers.has(key)` guard, implemented via merge
      order).
    - `betas.ts`: `isLongContextError` also matches
      `"You're out of extra usage"`.

    PR #211's `fetchWithRetry` retry-after cap is N/A here: our messages-
    path `streamSimple` has no retry loop (only a single 401 credential-
    refresh retry with no delay), and `auth.ts::fetchWithRetry` (used for
    token exchange, divergence #1) already caps at 30s. See
    `auth.ts::fetchWithRetry`'s existing cap logic.

    v1.5.2, v1.5.3, and v1.5.4 upstream were reviewed and skipped —
    they are keychain / multi-account fixes that do not apply here (see
    divergence #6, single-account only).

12. **v2.0.0 port (issue #2382) — ported**. griffinmartin PR #240 (v2.0.0)
    removed the 1M-context opt-in, regenerated `baseBetas` with new entries
    and a duplicate `interleaved-thinking-2025-05-14`, and switched the
    override-exclude implementation from `indexOf`/`splice` to `filter` so
    the duplicate is fully removed for excluded betas. Ported here in-place:

    - `betas.ts`: `isEnable1mContext()`, `supports1mContext()`, and their
      callsite in `getModelBetas` removed; override-exclude uses `filter`.
      The pi-specific `forceAdaptiveThinking` suppression block (divergence
      #10) is retained and was also switched to `filter` so the duplicate
      in `baseBetas` is fully suppressed for adaptive models.
    - `model-config.ts`: `ccVersion` bumped to `2.1.185`; `baseBetas`
      regenerated with `thinking-token-count-2026-05-13`,
      `extended-cache-ttl-2025-04-11`, `effort-2025-11-24`, and a duplicate
      `interleaved-thinking-2025-05-14` (all mirroring upstream verbatim);
      per-model `4-6`/`4-7`/`4-8` `effort-2025-11-24` overrides removed
      (redundant now that the beta is in the base list).
    - `betas.test.ts`: 1M-opt-in tests replaced with an inertness test
      that sets `PI_ANTHROPIC_ENABLE_1M_CONTEXT=true` and asserts
      `context-1m-2025-08-07` is NOT injected; new filter-fix regression
      tests cover the duplicate-in-baseBetas and duplicate-in-user-flags
      cases (revert-and-fail-verified: reverting the `filter` fix to
      `indexOf`/`splice` locally makes both tests fail).
    - `request-body.test.ts`: `"still adds effort-2025-11-24 for the 4-8
      substring match"` renamed to reflect the new mechanism (via
      `baseBetas`, not per-model override), and a companion test proves the
      beta also appears for non-4-8 models.

    Upstream still keeps `4-6`/`4-7` entries in `modelOverrides` as no-ops
    (they add a beta already in `baseBetas`); we intentionally dropped
    both because they add nothing. If a future upstream fix re-adds those
    entries with different `add` values, port that change here explicitly.

13. **Note on divergence #7 (MD5 vs PascalCase)** — verified accurate as of
    this port. Griffinmartin main uses PascalCase (per commit `9121ca47` in
    v1.4.10); we still use MD5 hash-based obfuscation. No changes to
    `transforms.ts` in this port.

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
