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

import { existsSync, symlinkSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import {
  AuthStorage,
  ModelRegistry,
  type ExtensionAPI,
  type ProviderConfig,
} from "@mariozechner/pi-coding-agent"
import type { OAuthCredentials } from "@mariozechner/pi-ai"
import { initLogger, log } from "./logger.ts"
import {
  loginAnthropic,
  refreshAnthropicToken,
  isClaudeOAuthAccessToken,
} from "./auth.ts"
import { getCachedCredentials } from "./credentials.ts"
import { transformBody, transformResponseStream } from "./transforms.ts"
import { getModelBetas, getExcludedBetas } from "./betas.ts"
import { config } from "./model-config.ts"
import {
  buildAnthropicSystemPrompt,
  convertPiMessagesToAnthropic,
  convertPiToolsToAnthropic,
  fromClaudeCodeToolName,
  parseSSEStream,
} from "./stream.ts"

const DEFAULT_OPUS_4_7: NonNullable<ProviderConfig["models"]>[number] = {
  id: "claude-opus-4-7",
  name: "Claude Opus 4.7",
  api: "anthropic-messages",
  reasoning: true,
  input: ["text", "image"],
  cost: { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  contextWindow: 1000000,
  maxTokens: 128000,
  compat: undefined,
}

// pi-specific: create a ~/.Claude Code → ~/.pi symlink so that pi's jiti
// resolver can find modules in the expected Claude Code credential path.
function ensureClaudeCodeSymlink() {
  const target = join(homedir(), ".pi")
  const link = join(homedir(), ".Claude Code")
  if (existsSync(target) && !existsSync(link)) {
    try {
      symlinkSync(target, link)
    } catch {}
  }
}

function getAnthropicModels(): NonNullable<ProviderConfig["models"]> {
  const modelRegistry = ModelRegistry.create(AuthStorage.inMemory())
  const models: NonNullable<ProviderConfig["models"]> = modelRegistry
    .getAll()
    .filter((model) => model.provider === "anthropic")
    .map((model) => ({
      id: model.id,
      name: model.name,
      api: model.api ?? "anthropic-messages",
      reasoning: model.reasoning,
      input: model.input,
      cost: model.cost,
      contextWindow: model.contextWindow,
      maxTokens: model.maxTokens,
      compat: model.compat,
    }))

  if (!models.some((model) => model.id === DEFAULT_OPUS_4_7.id)) {
    models.push(DEFAULT_OPUS_4_7)
  }

  return models
}

function getUserAgent(): string {
  return (
    process.env.ANTHROPIC_USER_AGENT ??
    `claude-cli/${config.ccVersion} (external, cli)`
  )
}

// pi-specific: extension entry point — pi calls this with its ExtensionAPI
export default function (pi: ExtensionAPI) {
  initLogger()
  log("extension_init", { version: config.ccVersion })

  ensureClaudeCodeSymlink()
  const models = getAnthropicModels()

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
      // pi-specific: getApiKey extracts the bearer token from stored credentials
      getApiKey: (credentials: OAuthCredentials) => credentials.access,
    } as unknown as ProviderConfig["oauth"],

    // pi-specific: streamSimple wraps the Anthropic messages endpoint with:
    //   - Bearer token injection from ~/.pi/agent/auth.json
    //   - Beta header construction (mirrors griffinmartin's buildRequestHeaders)
    //   - Request body transformation (billing header + MD5 tool obfuscation)
    //   - Response stream deobfuscation (stripToolPrefix via transformResponseStream)
    //   - Automatic token refresh on 401
    //   - Manual SSE parsing (zero npm deps — no @anthropic-ai/sdk)
    streamSimple: (model, context, options) => {
      const { createAssistantMessageEventStream, calculateCost } =
        require("@mariozechner/pi-ai") as typeof import("@mariozechner/pi-ai")

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
            const headers = new Headers()

            if (isOAuth) {
              const creds = getCachedCredentials()
              const token = creds?.accessToken ?? apiKey
              const excluded = getExcludedBetas(model.id)
              const betas = getModelBetas(model.id, excluded)
              headers.set("authorization", `Bearer ${token}`)
              headers.set("anthropic-version", "2023-06-01")
              headers.set("anthropic-beta", betas.join(","))
              headers.set("x-app", "cli")
              headers.set("user-agent", getUserAgent())
              headers.set("x-client-request-id", crypto.randomUUID())
            }

            if (options?.headers) {
              for (const [key, value] of Object.entries(options.headers)) {
                const norm = key.toLowerCase()
                if (isOAuth && norm === "x-api-key") continue
                headers.set(key, value)
              }
            }

            return headers
          }

          const maxTokens =
            options?.maxTokens || Math.floor(model.maxTokens / 3)

          const body: Record<string, unknown> = {
            model: model.id,
            max_tokens: maxTokens,
            stream: true,
          }

          body.messages = convertPiMessagesToAnthropic(
            context.messages,
            isOAuth,
          )

          const system = buildAnthropicSystemPrompt(
            context.systemPrompt,
            isOAuth,
          )
          if (system) body.system = system

          if (context.tools?.length) {
            body.tools = convertPiToolsToAnthropic(context.tools, isOAuth)
          }

          if (options?.reasoning && model.reasoning && maxTokens > 1) {
            const defaultBudgets: Record<string, number> = {
              minimal: 1024,
              low: 4096,
              medium: 10240,
              high: 20480,
              xhigh: 32000,
            }
            const customBudget =
              options.thinkingBudgets?.[
                options.reasoning as keyof typeof options.thinkingBudgets
              ]
            const requestedBudget =
              customBudget ?? defaultBudgets[options.reasoning] ?? 10240
            body.thinking = {
              type: "enabled",
              budget_tokens: Math.min(requestedBudget, maxTokens - 1),
            }
          }

          const bodyStr = JSON.stringify(body)
          const transformedBody = isOAuth ? transformBody(bodyStr) : bodyStr
          const headers = buildHeaders()

          let response = await fetch(`${model.baseUrl}/v1/messages`, {
            method: "POST",
            headers,
            body: transformedBody as string,
            signal: options?.signal,
          })

          log("fetch_response", { status: response.status, modelId: model.id })

          // On 401, rebuild headers (may refresh credential) and retry once
          if (response.status === 401 && isOAuth) {
            log("fetch_401_retry", { modelId: model.id })
            const retryHeaders = buildHeaders()
            response = await fetch(`${model.baseUrl}/v1/messages`, {
              method: "POST",
              headers: retryHeaders,
              body: transformedBody as string,
              signal: options?.signal,
            })
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
