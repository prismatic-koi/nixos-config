// Pure helpers for the Anthropic /v1/messages request body.
//
// Lives in its own file (separate from index.ts) so unit tests can import
// it without pulling in `@earendil-works/pi-coding-agent` — the bundled
// extension runtime — which is not resolvable in a bare `tsx --test` run.
//
// Mirrors pi-ai's built-in `buildParams` in
// `@earendil-works/pi-ai/src/providers/anthropic.ts` (~lines 710-731 for
// the effort mapping, ~937-981 for the body assembly).

import type {
  AnthropicEffort,
  Context,
  Model,
  Message,
  SimpleStreamOptions,
  SystemMessage,
  TextContent,
  ThinkingLevel,
  Tool,
} from "@earendil-works/pi-ai"
import {
  buildAnthropicSystemPrompt,
  convertPiMessagesToAnthropic,
  convertPiToolsToAnthropic,
} from "./stream.ts"

/**
 * Fold pi-ai's transcript-shaped context back into the legacy
 * `{systemPrompt, tools, messages}` shape the rest of this extension reads.
 *
 * From pi 0.87, providers receive a `TranscriptContext`: the system prompt
 * and tool declarations travel as `role: "system"` messages (`content`,
 * `sections`, `toolsAdded`, `toolsRemoved`) instead of `context.systemPrompt`
 * and `context.tools`. Reading only the legacy fields sent every request with
 * no tools and no pi system prompt, so the model claimed it had no tools.
 *
 * Replays every system message the way pi-ai's `getCurrentSystemMessage` +
 * `getSystemMessageText` do (utils/transcript.ts, utils/text.ts), inlined so
 * this file keeps only type imports from pi-ai for `tsx --test`. Legacy
 * fields, when a caller still sets them, seed the replay as pi-ai's
 * `normalizeContext` would.
 */
export function flattenTranscriptContext(context: Context): Context {
  const contentText = (content: string | TextContent[]): string =>
    typeof content === "string"
      ? content
      : content
          .filter((block) => block.type === "text")
          .map((block) => block.text)
          .join("\n")

  const content: string[] = []
  if (context.systemPrompt) content.push(context.systemPrompt)
  const sections = new Map<string, string>()
  const tools = new Map<string, Tool>()
  for (const tool of context.tools ?? []) tools.set(tool.name, tool)

  const messages: Message[] = []
  for (const message of context.messages) {
    if ((message as { role: string }).role !== "system") {
      messages.push(message)
      continue
    }
    const system = message as unknown as SystemMessage
    const text = contentText(system.content)
    if (text.length > 0) content.push(text)
    for (const [name, value] of Object.entries(system.sections ?? {})) {
      if (value === null) sections.delete(name)
      else sections.set(name, value)
    }
    for (const tool of system.toolsRemoved ?? []) tools.delete(tool.name)
    for (const tool of system.toolsAdded ?? []) tools.set(tool.name, tool)
  }

  const systemPrompt = [content.join("\n\n"), ...sections.values()]
    .filter((part) => part.length > 0)
    .join("\n\n")

  return {
    ...(systemPrompt ? { systemPrompt } : {}),
    messages,
    ...(tools.size > 0 ? { tools: [...tools.values()] } : {}),
  }
}

/**
 * Map a pi reasoning level to an Anthropic effort string for the adaptive
 * thinking payload. Honours the model's `thinkingLevelMap` when present,
 * mirroring pi-ai's `mapThinkingLevelToEffort`
 * (`@earendil-works/pi-ai` src/providers/anthropic.ts ~710-731).
 */
export function mapThinkingLevelToEffort(
  model: Model<"anthropic-messages">,
  level: ThinkingLevel | undefined,
): AnthropicEffort {
  const thinkingLevelMap = (
    model as { thinkingLevelMap?: Record<string, string | null> }
  ).thinkingLevelMap
  const mapped = level ? thinkingLevelMap?.[level] : undefined
  if (typeof mapped === "string") return mapped as AnthropicEffort

  switch (level) {
    case "minimal":
    case "low":
      return "low"
    case "medium":
      return "medium"
    case "high":
      return "high"
    default:
      return "high"
  }
}

/**
 * Build the JSON request body sent to Anthropic's /v1/messages endpoint.
 *
 * Pure helper exported for unit testing. Mirrors pi-ai's built-in
 * `buildParams` in `src/providers/anthropic.ts` (~952-981) for the
 * thinking-payload selection:
 *
 *   - Models with `compat.forceAdaptiveThinking === true` get the adaptive
 *     payload: `thinking: {type:"adaptive", display:"summarized"}` plus
 *     `output_config: {effort}` (effort derived from the pi reasoning level
 *     via `mapThinkingLevelToEffort`, honouring the model's thinkingLevelMap).
 *   - All other anthropic models keep the legacy budget-based form:
 *     `thinking: {type:"enabled", budget_tokens:N, display:"summarized"}`.
 *     `display: "summarized"` matches the documented Anthropic default and
 *     pi-ai's legacy-path behaviour.
 *
 * Temperature is intentionally NOT set on the body when thinking is enabled
 * (this helper never sets temperature — Anthropic rejects requests that
 * combine extended thinking with a temperature override with a 400). Mirrors
 * pi-ai's guard at `anthropic.ts` ~937-940. If a future change adds a
 * temperature path, gate it on `!body.thinking`.
 */
export function buildRequestBody(
  model: Model<"anthropic-messages">,
  context: Context,
  options: SimpleStreamOptions | undefined,
  isOAuth: boolean,
): Record<string, unknown> {
  const maxTokens = options?.maxTokens || Math.floor(model.maxTokens / 3)

  const body: Record<string, unknown> = {
    model: model.id,
    max_tokens: maxTokens,
    stream: true,
  }

  body.messages = convertPiMessagesToAnthropic(context.messages, isOAuth)

  const system = buildAnthropicSystemPrompt(context.systemPrompt, isOAuth)
  if (system) body.system = system

  if (context.tools?.length) {
    body.tools = convertPiToolsToAnthropic(context.tools, isOAuth)
  }

  if (options?.reasoning && model.reasoning && maxTokens > 1) {
    const display = "summarized" as const
    if (model.compat?.forceAdaptiveThinking === true) {
      // Adaptive thinking (opus-4-6/4-7/4-8 and any other model that opts in
      // via compat). The legacy budget_tokens form produces a degraded /
      // erratic response from these models — see issue #2044.
      body.thinking = { type: "adaptive", display }
      const effort = mapThinkingLevelToEffort(model, options.reasoning)
      body.output_config = { effort }
    } else {
      // Legacy budget-based thinking for older / non-adaptive Claude models.
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
        display,
      }
    }
  }

  return body
}
