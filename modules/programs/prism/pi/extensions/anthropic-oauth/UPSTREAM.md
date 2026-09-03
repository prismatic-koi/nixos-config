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
| `griffinmartin/opencode-claude-auth` — `model-config.ts` + `betas.ts` ONLY | `09a13b4c` | 2026-09-01 (v2.2.0, PR #279 — Claude CLI 2.1.257 model config) |
| `griffinmartin/opencode-claude-auth` PR #193 | `9420fbef60567968bcd21a260db21be9f7dd475b` | 2026-04-14 (the MD5 hash obfuscation approach) |
| `leohenon/pi-anthropic-oauth` | `86d9d97829776a66aec58e3433900173ff7e184a` | 2026-04 (update readme) |

> **Note on the two SHAs**: the main-line SHA is the last commit at which the
> whole file set was reviewed. `model-config.ts` and `betas.ts` are ahead of it
> at `09a13b4c` because divergence #15 ported those two files alone. Everything
> upstream shipped between the two SHAs in `credentials.ts`, `transforms.ts`,
> `index.ts`, `keychain.ts`, `http.ts`, `refresh-lock.ts`, and
> `refresh-backoff.ts` is UNPORTED. Do not read the newer SHA as a whole-repo
> sync point. Before you port any of those files, diff from `88b0f793`, not
> from `09a13b4c`.

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
| `ratelimit.ts` | *(none — local addition, see divergence 14)* | *(no equivalent)* | *(no equivalent)* |
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

   **Regression guard.** `verify-extension-loads.mjs` (in this directory)
   loads this extension end-to-end against a real, installed
   pi-coding-agent build using pi's own extension-loading mechanism (jiti,
   aliased the same way `dist/core/extensions/loader.js` aliases it for
   Node/dev mode) and asserts the anthropic provider registers with a
   non-empty model list and all OAuth handlers wired. Run it after any
   pi-coding-agent version bump (overlay pin or nixpkgs bump) — it is what
   would have caught issue #2428 before it reached an interactive `pi`
   session:

   ```bash
   node modules/programs/prism/pi/extensions/anthropic-oauth/verify-extension-loads.mjs
   # or, to check a specific build not yet the default pkgs.pi-coding-agent:
   PI_INSTALL_ROOT=/nix/store/...-pi-coding-agent-X.Y.Z \
     node modules/programs/prism/pi/extensions/anthropic-oauth/verify-extension-loads.mjs
   ```

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

14. **`ratelimit.ts` is a LOCAL ADDITION, not an upstream port** (issue
    #2538, parent #2537). Neither griffinmartin nor leohenon captures the
    `anthropic-ratelimit-unified-*` response headers. Do NOT look for an
    upstream counterpart, and do NOT delete this file during a future port
    because it is absent upstream.

    **What it does.** Anthropic returns a unified rate-limit header set on
    every successful `/v1/messages` response on the Claude Code OAuth path.
    `ratelimit.ts` reads that allowlisted set, converts it to the snapshot
    shape documented in #2537, and POSTs it to the prism sidecar's host-API
    endpoint `/usage/snapshot`. The sidecar resolves the active account and
    persists `~/.local/state/prism/usage/<account>.json` plus `current.json`.
    Prism reads those files back to show subscription usage.

    **Where it hooks.** One call in `index.ts::streamSimple`, immediately
    after the 401-retry block and before `finalResponse` is computed. That
    is the only point where the headers are fully populated and the body is
    not yet consumed. `stream.ts` and `transforms.ts` are deliberately NOT
    hooked — `parseSSEStream` sees only the body, and `transforms.ts` runs
    on the error branch too.

    **Two invariants a future port must not break.** The hook sits in the
    live OAuth request path, so an exception or a stall there breaks every
    API call for every session on the machine:

    - `captureRateLimitSnapshot` returns void SYNCHRONOUSLY and never
      throws. It does not await the POST.
    - It reads response HEADERS only. It never touches `response.body`,
      `.text()`, or `.json()`.

    **Token-leak guard.** The parser reads an explicit header allowlist. A
    bulk `Object.fromEntries(response.headers)` would sweep up
    `authorization`; `ratelimit.test.ts` has a regression test that feeds in
    `authorization`, `x-api-key`, and `cookie` and asserts none reaches the
    POST body.

    **When PRISM_HOST_API is unset** (a session running outside a prism
    sandbox) the hook makes no network call at all.

    Tests: `ratelimit.test.ts` (parser, transport, and request-path
    guarantees). Live capture cannot be verified from a worktree — `pi.nix`
    mounts this directory as a read-only nix-store symlink, so a change
    needs `nh switch` first.

15. **v2.2.0 model-config port (issue #2918) — ported, `model-config.ts` and
    `betas.ts` only**. griffinmartin PR #279 (released as v2.2.0, commit
    `09a13b4c`) regenerated the model config from Claude CLI 2.1.257
    intercept traffic. The bump is not cosmetic: the Anthropic API rejects
    `claude-fable-5-1` on subscription (OAuth) auth below Claude Code
    2.1.251 with HTTP 400 `claude_code_version_too_old`, so this port is
    what makes Fable 5.1 selectable at all.

    Ported here in-place:

    - `model-config.ts`: `ccVersion` `2.1.185` → `2.1.257`. `baseBetas`
      regenerated to eight entries — `effort-2025-11-24` left the base list,
      and the deliberate duplicate `interleaved-thinking-2025-05-14` that
      v2.0.0 carried is gone. `modelOverrides` gained `opus-4-5`, `4-6`, and
      `4-7` (each adding `effort-2025-11-24`), and the `haiku` entry now
      excludes `effort-2025-11-24` instead of `interleaved-thinking-2025-05-14`.
      The file is now byte-identical to upstream apart from our three-line
      provenance header.
    - `betas.ts`: comment-only. The logic already matched upstream; the
      stale comments that named the removed `baseBetas` duplicate and the
      wrong haiku exclude target were corrected.
    - `betas.test.ts`, `request-body.test.ts`, `oauth-headers.test.ts`:
      expectations regenerated — see "Test-visible behaviour changes" below.

    **The `4-6`/`4-7` overrides come back.** Divergence #12 dropped them as
    no-ops, correctly: v2.0.0 had `effort-2025-11-24` in `baseBetas`, so a
    per-model add did nothing. v2.2.0 took the beta out of the base list, so
    the overrides are load-bearing again and are restored verbatim.

    **Test-visible behaviour changes.** Two models that used to receive
    `effort-2025-11-24` from `baseBetas` no longer receive it, because
    neither matches an override key: `claude-opus-4-8` and
    `claude-sonnet-4-5`. `claude-fable-5-1` does not match one either. This
    is upstream's shape, mirrored deliberately — upstream pins it with an
    "effort beta" test taken from 2.1.257 intercept traffic, and
    `4-8`-shaped names post-date the CLI build that traffic came from. Do
    not add a local `4-8` or `fable` override to "restore" the beta without
    intercept evidence that the real client sends it.

    **Divergence #10 is retained** (AC of #2918). `getModelBetas` still
    filters every occurrence of `interleaved-thinking-2025-05-14` when the
    caller passes `ctx.forceAdaptiveThinking`. The `filter`-over-splice
    choice outlives the duplicate that motivated it: `ANTHROPIC_BETA_FLAGS`
    is user-supplied and can list a beta twice, so `betas.test.ts` now feeds
    the duplicate in through the env var on both filter paths (the
    override-exclude path and the divergence-#10 path). Both tests were
    revert-and-fail verified against an indexOf/splice implementation.

    **Scope.** Only the two files above were ported. Upstream shipped a
    large amount of other work between `88b0f793` and this release —
    external credential rotation (`credentials.ts`, `keychain.ts`, new
    `refresh-lock.ts` / `refresh-backoff.ts` / `http.ts`) and a
    ~290-line `transforms.ts` change. None of it is ported. The keychain and
    multi-account parts are out of scope permanently (divergence #6); the
    rest is unreviewed here. See the note under "Current sync commit SHAs".

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
