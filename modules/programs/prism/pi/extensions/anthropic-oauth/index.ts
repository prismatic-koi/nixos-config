// pi-specific: pi.registerProvider wrapper for the Anthropic OAuth extension.
// This is the ONE pi-specific file — it will NEVER match griffinmartin's index.ts
// exactly because opencode and pi use different plugin/extension APIs.
//
// griffinmartin/opencode-claude-auth uses plugin.auth.loader + hook APIs.
// pi uses pi.registerProvider(name, { oauth: { login, refreshToken, getApiKey } }).
//
// When porting a future upstream fix that touches griffinmartin's index.ts,
// skip it — that file corresponds to nothing here. All business logic changes
// in other griffinmartin files DO have 1:1 mirrors here and should be ported.
//
// See UPSTREAM.md for the port procedure.

import {
  ModelRegistry,
  ModelRuntime,
  type ExtensionAPI,
  type ProviderConfig,
} from "@earendil-works/pi-coding-agent"
import type { OAuthCredentials } from "@earendil-works/pi-ai"
import { initLogger, log } from "./logger.ts"
import {
  loginAnthropic,
  refreshAnthropicToken,
  isClaudeOAuthAccessToken,
} from "./auth.ts"
import { getCachedCredentials, repairCredentials } from "./credentials.ts"
import { transformBody, transformResponseStream } from "./transforms.ts"
import { streamSimpleAnthropic } from "@earendil-works/pi-ai"
import { config } from "./model-config.ts"
import { fromClaudeCodeToolName, parseSSEStream } from "./stream.ts"
import { buildRequestBody } from "./request-body.ts"
import { buildOAuthHeaders, buildRequestUrl } from "./oauth-headers.ts"
import { captureRateLimitSnapshot } from "./ratelimit.ts"

// pi 0.80.8 removed `AuthStorage` from the barrel and dropped the
// `ModelRegistry.create()` static factory. Construction is now
// `new ModelRegistry(await ModelRuntime.create())`, and `refresh()` must be
// awaited before the synchronous `getAll()` read (the facade loads
// models.json lazily). See UPSTREAM.md divergence #8.
async function getAnthropicModels(): Promise<
  NonNullable<ProviderConfig["models"]>
> {
  const runtime = await ModelRuntime.create()
  const registry = new ModelRegistry(runtime)
  await registry.refresh()
  const models: NonNullable<ProviderConfig["models"]> = registry
    .getAll()
    .filter((model) => model.provider === "anthropic")
    .map((model) => ({
      id: model.id,
      name: model.name,
      api: model.api ?? "anthropic-messages",
      reasoning: model.reasoning,
      // thinkingLevelMap drives request-body.ts::mapThinkingLevelToEffort —
      // without it, opus-4-6/4-7/4-8 silently fall through to the default
      // mapping (e.g. xhigh → "high" instead of "max"/"xhigh"). Issue #2053;
      // same bug shape as leohenon/pi-anthropic-oauth#7. Pi's
      // ProviderModelConfig schema explicitly accepts this field
      // (`dist/core/model-registry.js` ~line 114 / 505).
      thinkingLevelMap: model.thinkingLevelMap,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      compat: model.compat,
      // Deliberately NOT propagated:
      //   - provider: fixed at "anthropic" by registerProvider() below.
      //   - baseUrl: set at the provider level in registerProvider(); no
      //     anthropic-provider registry entry overrides it per-model.
      //   - headers: this extension builds the Anthropic request headers
      //     itself in streamSimple (Bearer token, anthropic-version,
      //     anthropic-beta, x-app, user-agent, x-client-request-id) — pi's
      //     per-model headers would be additive and risk colliding with the
      //     OAuth-mode auth header. No anthropic registry model currently
      //     declares per-model headers, so this is also a no-op today.
    }))

  return models
}

// pi-specific: extension entry point — pi calls this with its ExtensionAPI.
// The loader awaits async default exports (see pi 0.80.8
// `dist/core/extensions/loader.js` — `await factory(api)`), and
// `pi.registerProvider` calls made before the runner binds its core context
// are queued and applied on bind. This is the sanctioned pattern for
// providers whose model list requires async construction.
export default async function (pi: ExtensionAPI) {
  initLogger()
  log("extension_init", { version: config.ccVersion })

  repairCredentials()

  let models: NonNullable<ProviderConfig["models"]>
  try {
    models = await getAnthropicModels()
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    log("model_registry_error", { error: message })
    // Surface a clear failure rather than silently registering the anthropic
    // provider with an empty model list (AC: edge-case).
    throw new Error(
      `anthropic-oauth: failed to load Anthropic model registry: ${message}`,
    )
  }

  if (models.length === 0) {
    throw new Error(
      "anthropic-oauth: Anthropic model registry returned no models after refresh()",
    )
  }

  pi.registerProvider("anthropic", {
    baseUrl: "https://api.anthropic.com",
    api: "anthropic-messages",
    models,
    oauth: {
      name: "Claude Pro/Max",
      usesCallbackServer: true,
      // pi-specific: login triggers the full OAuth PKCE flow (auth.ts)
      login: loginAnthropic,
      // pi-specific: refreshToken is called by pi when tokens are near expiry
      refreshToken: refreshAnthropicToken,
      // pi-specific: getApiKey extracts the bearer token from stored credentials.
      // Fall back to auth.json when the per-session credential store is empty
      // (e.g. new bwrap sessions that have never run /login anthropic).
      getApiKey: (credentials: OAuthCredentials) =>
        credentials.access || getCachedCredentials()?.accessToken || null,
    } as unknown as ProviderConfig["oauth"],

    // pi-specific: streamSimple wraps the Anthropic messages endpoint with:
    //   - Bearer token injection from ~/.pi/agent/auth.json
    //   - Beta header construction (mirrors griffinmartin's buildRequestHeaders)
    //   - Request body transformation (billing header + MD5 tool obfuscation)
    //   - Response stream deobfuscation (stripToolPrefix via transformResponseStream)
    //   - Automatic token refresh on 401
    //   - Manual SSE parsing (zero npm deps — no @anthropic-ai/sdk)
    streamSimple: (model, context, options) => {
      // Only intercept requests for the anthropic provider (Claude OAuth subscriptions).
      // For all other providers (github-copilot, openrouter, etc.) that also use the
      // anthropic-messages API type, delegate to pi's built-in handler which has
      // provider-specific logic (e.g. Copilot dynamic headers).
      if (model.provider !== "anthropic") {
        return streamSimpleAnthropic(model, context, options)
      }

      const { createAssistantMessageEventStream, calculateCost } =
        require("@earendil-works/pi-ai") as typeof import("@earendil-works/pi-ai")

      const stream = createAssistantMessageEventStream()

      void (async () => {
        try {
          const apiKey = options?.apiKey
          if (!apiKey) {
            throw new Error(
              "No Anthropic auth available. Run /login anthropic.",
            )
          }

          const isOAuth = isClaudeOAuthAccessToken(apiKey)

          const buildHeaders = (): Headers => {
            if (isOAuth) {
              const creds = getCachedCredentials()
              const token = creds?.accessToken ?? apiKey
              return buildOAuthHeaders(model, token, options?.headers)
            }

            // API-key mode: pass through caller headers unchanged.
            const headers = new Headers()
            if (options?.headers) {
              for (const [key, value] of Object.entries(options.headers)) {
                headers.set(key, value)
              }
            }
            return headers
          }

          const body = buildRequestBody(model, context, options, isOAuth)
          // NOTE: temperature is intentionally not set anywhere in body —
          // Anthropic rejects extended-thinking requests that also pass
          // temperature with a 400. Mirrors pi-ai's guard
          // (`anthropic.ts` ~937-940). If a future change adds a
          // temperature path, gate it on `!body.thinking`.
          const bodyStr = JSON.stringify(body)
          const transformedBody = isOAuth ? transformBody(bodyStr) : bodyStr
          const headers = buildHeaders()

          const requestUrl = buildRequestUrl(`${model.baseUrl}/v1/messages`)

          let response = await fetch(requestUrl, {
            method: "POST",
            headers,
            body: transformedBody as string,
            signal: options?.signal,
          })

          log("fetch_response", { status: response.status, modelId: model.id })

          // On 401, rebuild headers (may refresh credential) and retry once.
          // Reuse the beta-appended URL — the retry must hit the same endpoint
          // as the original request (issue #2381, mirrors griffinmartin PR #207).
          if (response.status === 401 && isOAuth) {
            log("fetch_401_retry", { modelId: model.id })
            const retryHeaders = buildHeaders()
            response = await fetch(requestUrl, {
              method: "POST",
              headers: retryHeaders,
              body: transformedBody as string,
              signal: options?.signal,
            })
          }

          // Rate-limit usage capture (issue #2538, parent #2537). A LOCAL
          // ADDITION with no upstream counterpart — see UPSTREAM.md
          // divergence #14.
          //
          // This is the one point in the request path where the headers are
          // fully populated and the body is not yet consumed. The
          // log("fetch_response", …) call above fires BEFORE the 401 retry,
          // so it would report the pre-retry response; parseSSEStream only
          // ever sees the body; transforms.ts runs on the error branch too.
          //
          // Reads `response`, not `finalResponse`: the raw fetch Response is
          // the authoritative source of the headers, so the capture does not
          // depend on transformResponseStream continuing to propagate them.
          //
          // captureRateLimitSnapshot returns void synchronously and never
          // throws. It touches headers only, never the body, and it does not
          // await the POST — an exception or a stall here would break every
          // API call on this machine.
          if (isOAuth) {
            captureRateLimitSnapshot(response.status, response.headers)
          }

          const finalResponse = isOAuth
            ? transformResponseStream(response)
            : response

          if (!finalResponse.ok || !finalResponse.body) {
            const errorText = await finalResponse.text()
            throw new Error(
              `Anthropic API error ${finalResponse.status}: ${errorText}`,
            )
          }

          const output = await parseSSEStream(
            finalResponse,
            model,
            context,
            isOAuth,
            stream,
            options,
          )

          calculateCost(model, output.usage)

          stream.push({
            type: "done",
            reason: output.stopReason as "stop" | "length" | "toolUse",
            message: output,
          })
          stream.end()
        } catch (error) {
          const stopReason = options?.signal?.aborted ? "aborted" : "error"
          const errorMessage =
            error instanceof Error ? error.message : String(error)
          log("stream_error", { error: errorMessage })
          stream.push({
            type: "error",
            reason: stopReason,
            error: {
              role: "assistant",
              content: [],
              api: model.api,
              provider: model.provider,
              model: model.id,
              usage: {
                input: 0,
                output: 0,
                cacheRead: 0,
                cacheWrite: 0,
                totalTokens: 0,
                cost: {
                  input: 0,
                  output: 0,
                  cacheRead: 0,
                  cacheWrite: 0,
                  total: 0,
                },
              },
              stopReason,
              errorMessage,
              timestamp: Date.now(),
            },
          })
          stream.end()
        }
      })()

      return stream
    },
  })
}
