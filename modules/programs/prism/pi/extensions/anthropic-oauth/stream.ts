// Pi-specific SSE stream parsing and message conversion.
// Replaces leohenon's stream.ts which used @anthropic-ai/sdk.
// This file uses only host fetch() and manual SSE parsing (zero npm deps).
//
// Source patterns from:
//   leohenon/pi-anthropic-oauth src/stream.ts (structure, pi API types)
//   leohenon/pi-anthropic-oauth src/convert.ts (message conversion)
//   leohenon/pi-anthropic-oauth src/prompt.ts (system prompt building)

import type {
  AssistantMessage,
  AssistantMessageEventStream,
  Context,
  Model,
  Api,
  SimpleStreamOptions,
  TextContent,
  ThinkingContent,
  ToolCall,
  ImageContent,
  ToolResultMessage,
  Message,
  Tool,
} from "@mariozechner/pi-ai"

// ──────────────────────────────────────────────
// Type helpers
// ──────────────────────────────────────────────

type ContentBlockParam =
  | { type: "text"; text: string; cache_control?: { type: string } }
  | {
      type: "image"
      source: {
        type: "base64"
        media_type: "image/jpeg" | "image/png" | "image/gif" | "image/webp"
        data: string
      }
    }
  | {
      type: "tool_use"
      id: string
      name: string
      input: Record<string, unknown>
    }
  | {
      type: "tool_result"
      tool_use_id: string
      content: string | ToolResultContentBlock[]
      is_error?: boolean
    }

type ToolResultContentBlock =
  | { type: "text"; text: string }
  | {
      type: "image"
      source: {
        type: "base64"
        media_type: "image/jpeg" | "image/png" | "image/gif" | "image/webp"
        data: string
      }
    }

export type IndexedBlock =
  | (TextContent & { index: number })
  | (ThinkingContent & { index: number; thinkingSignature?: string })
  | (ToolCall & { index: number; partialJson: string })

// ──────────────────────────────────────────────
// System prompt utilities (from leohenon/prompt.ts)
// ──────────────────────────────────────────────

const CLAUDE_CODE_IDENTITY =
  "You are Claude Code, Anthropic's official CLI for Claude."
const PI_REMOVAL_ANCHORS = [
  "pi-coding-agent",
  "@mariozechner/pi-coding-agent",
  "badlogic/pi-mono",
] as const

export function sanitizeSurrogates(text: string): string {
  return text.replace(/[\uD800-\uDFFF]/g, "\uFFFD")
}

export function buildAnthropicSystemPrompt(
  systemPrompt: string | undefined,
  isOAuth: boolean,
): Array<{ type: string; text: string; cache_control?: { type: string } }> | undefined {
  const blocks: Array<{
    type: string
    text: string
    cache_control?: { type: string }
  }> = []

  if (isOAuth) {
    blocks.push({
      type: "text",
      text: CLAUDE_CODE_IDENTITY,
      cache_control: { type: "ephemeral" },
    })
  }

  const sanitized = systemPrompt ? sanitizeSystemText(systemPrompt) : ""
  if (sanitized) {
    blocks.push({
      type: "text",
      text: sanitized,
      cache_control: { type: "ephemeral" },
    })
  }

  return blocks.length > 0 ? blocks : undefined
}

function sanitizeSystemText(text: string): string {
  const paragraphs = text.split(/\n\n+/)
  const filtered = paragraphs.filter((paragraph) => {
    const lower = paragraph.toLowerCase()
    if (lower.includes("you are pi")) return false
    return !PI_REMOVAL_ANCHORS.some((anchor) => paragraph.includes(anchor))
  })

  return filtered
    .join("\n\n")
    .replace(/\bpi\b/g, "Claude Code")
    .replace(/\bPi\b/g, "Claude Code")
    .trim()
}

// ──────────────────────────────────────────────
// Message conversion (from leohenon/convert.ts)
// ──────────────────────────────────────────────

const claudeCodeTools = [
  "Read",
  "Write",
  "Edit",
  "Bash",
  "Grep",
  "Glob",
  "AskUserQuestion",
  "TodoWrite",
  "WebFetch",
  "WebSearch",
] as const

const claudeCodeToolLookup = new Map(
  claudeCodeTools.map((name) => [name.toLowerCase(), name]),
)

export function toClaudeCodeToolName(name: string): string {
  return claudeCodeToolLookup.get(name.toLowerCase()) ?? name
}

export function fromClaudeCodeToolName(
  name: string,
  tools?: Tool[],
): string {
  const lower = name.toLowerCase()
  return tools?.find((tool) => tool.name.toLowerCase() === lower)?.name ?? name
}

export function convertPiMessagesToAnthropic(
  messages: Message[],
  isOAuth: boolean,
): Array<{ role: string; content: string | ContentBlockParam[] }> {
  const params: Array<{ role: string; content: string | ContentBlockParam[] }> =
    []
  const toolIdMap = new Map<string, string>()
  const usedToolIds = new Set<string>()

  const getAnthropicToolId = (id: string): string => {
    const existing = toolIdMap.get(id)
    if (existing) return existing

    let base = sanitizeSurrogates(id).replace(/[^a-zA-Z0-9_-]/g, "_")
    if (!base) base = "tool"
    let candidate = base
    let suffix = 1
    while (usedToolIds.has(candidate)) {
      candidate = `${base}_${suffix++}`
    }
    usedToolIds.add(candidate)
    toolIdMap.set(id, candidate)
    return candidate
  }

  for (let i = 0; i < messages.length; i++) {
    const message = messages[i]

    if (message.role === "user") {
      if (typeof message.content === "string") {
        if (message.content.trim())
          params.push({
            role: "user",
            content: sanitizeSurrogates(message.content),
          })
      } else {
        const blocks: ContentBlockParam[] = (
          message.content as (TextContent | ImageContent)[]
        ).map((item) =>
          item.type === "text"
            ? { type: "text", text: sanitizeSurrogates(item.text) }
            : {
                type: "image",
                source: {
                  type: "base64",
                  media_type: (item as ImageContent).mimeType as
                    | "image/jpeg"
                    | "image/png"
                    | "image/gif"
                    | "image/webp",
                  data: (item as ImageContent).data,
                },
              },
        )
        if (blocks.length > 0) params.push({ role: "user", content: blocks })
      }
      continue
    }

    if (message.role === "assistant") {
      const blocks: ContentBlockParam[] = []
      for (const block of message.content as (
        | TextContent
        | ThinkingContent
        | ToolCall
      )[]) {
        if (block.type === "text" && (block as TextContent).text.trim()) {
          blocks.push({
            type: "text",
            text: sanitizeSurrogates((block as TextContent).text),
          })
        } else if (block.type === "toolCall") {
          const tb = block as ToolCall
          blocks.push({
            type: "tool_use",
            id: getAnthropicToolId(tb.id),
            name: isOAuth ? toClaudeCodeToolName(tb.name) : tb.name,
            input: tb.arguments,
          })
        }
      }
      if (blocks.length > 0)
        params.push({ role: "assistant", content: blocks })
      continue
    }

    if (message.role === "toolResult") {
      const msg = message as ToolResultMessage
      const toolResults: ContentBlockParam[] = [
        {
          type: "tool_result",
          tool_use_id: getAnthropicToolId(msg.toolCallId),
          content: convertToolResultContentToAnthropic(msg.content),
          is_error: msg.isError,
        },
      ]

      let j = i + 1
      while (j < messages.length && messages[j]?.role === "toolResult") {
        const nextMessage = messages[j] as ToolResultMessage
        toolResults.push({
          type: "tool_result",
          tool_use_id: getAnthropicToolId(nextMessage.toolCallId),
          content: convertToolResultContentToAnthropic(nextMessage.content),
          is_error: nextMessage.isError,
        })
        j++
      }
      i = j - 1
      params.push({ role: "user", content: toolResults })
    }
  }

  // Add cache_control to last user message block (matches leohenon pattern)
  const last = params.at(-1)
  if (
    last?.role === "user" &&
    Array.isArray(last.content) &&
    last.content.length > 0
  ) {
    const lastBlock = last.content[last.content.length - 1] as {
      cache_control?: { type: string }
    }
    lastBlock.cache_control = { type: "ephemeral" }
  }

  return params
}

export function convertPiToolsToAnthropic(
  tools: Tool[],
  isOAuth: boolean,
): Array<{
  name: string
  description: string
  input_schema: { type: "object"; properties: Record<string, unknown>; required: string[] }
}> {
  return tools.map((tool) => ({
    name: isOAuth ? toClaudeCodeToolName(tool.name) : tool.name,
    description: tool.description,
    input_schema: {
      type: "object" as const,
      properties:
        (tool.parameters as { properties?: Record<string, unknown> })
          .properties ?? {},
      required:
        (tool.parameters as { required?: string[] }).required ?? [],
    },
  }))
}

function convertToolResultContentToAnthropic(
  content: (TextContent | ImageContent)[],
): string | ToolResultContentBlock[] {
  const hasImages = content.some((block) => block.type === "image")
  if (!hasImages) {
    return sanitizeSurrogates(
      content
        .filter((block): block is TextContent => block.type === "text")
        .map((block) => block.text)
        .join("\n"),
    )
  }

  const blocks: ToolResultContentBlock[] = content.map((block) => {
    if (block.type === "text")
      return { type: "text" as const, text: sanitizeSurrogates(block.text) }
    const img = block as ImageContent
    return {
      type: "image" as const,
      source: {
        type: "base64" as const,
        media_type: img.mimeType as
          | "image/jpeg"
          | "image/png"
          | "image/gif"
          | "image/webp",
        data: img.data,
      },
    }
  })

  if (!blocks.some((block) => block.type === "text")) {
    blocks.unshift({ type: "text", text: "(see attached image)" })
  }

  return blocks
}

// ──────────────────────────────────────────────
// SSE stream parsing (replaces @anthropic-ai/sdk streaming)
// ──────────────────────────────────────────────

function mapStopReason(reason: string | null | undefined): string {
  switch (reason) {
    case "end_turn":
    case "pause_turn":
    case "stop_sequence":
      return "stop"
    case "max_tokens":
      return "length"
    case "tool_use":
      return "toolUse"
    default:
      return "error"
  }
}

/**
 * Parse an SSE stream from the Anthropic API into pi's AssistantMessageEventStream.
 * Uses only ReadableStream / TextDecoder — zero npm deps.
 */
export async function parseSSEStream(
  response: Response,
  model: Model<Api>,
  context: Context,
  isOAuth: boolean,
  stream: AssistantMessageEventStream,
  options?: SimpleStreamOptions,
): Promise<AssistantMessage> {
  const output: AssistantMessage = {
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
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
    },
    stopReason: "stop",
    timestamp: Date.now(),
  }

  stream.push({ type: "start", partial: output })

  const reader = response.body!.getReader()
  const decoder = new TextDecoder()
  let buffer = ""

  const blocks = output.content as IndexedBlock[]

  const processEvent = (eventText: string) => {
    const lines = eventText.split("\n")
    let eventType = ""
    let dataStr = ""

    for (const line of lines) {
      if (line.startsWith("event: ")) {
        eventType = line.slice(7).trim()
      } else if (line.startsWith("data: ")) {
        dataStr = line.slice(6).trim()
      }
    }

    if (!dataStr || dataStr === "[DONE]") return

    let event: Record<string, unknown>
    try {
      event = JSON.parse(dataStr) as Record<string, unknown>
    } catch {
      return
    }

    const type = (event.type as string) ?? eventType

    if (type === "message_start") {
      const msg = event.message as {
        usage?: {
          input_tokens?: number
          output_tokens?: number
          cache_read_input_tokens?: number
          cache_creation_input_tokens?: number
        }
      }
      const usage = msg?.usage ?? {}
      output.usage.input = usage.input_tokens ?? 0
      output.usage.output = usage.output_tokens ?? 0
      output.usage.cacheRead = usage.cache_read_input_tokens ?? 0
      output.usage.cacheWrite = usage.cache_creation_input_tokens ?? 0
      output.usage.totalTokens =
        output.usage.input +
        output.usage.output +
        output.usage.cacheRead +
        output.usage.cacheWrite
      return
    }

    if (type === "content_block_start") {
      const idx = event.index as number
      const cb = event.content_block as { type: string; id?: string; name?: string }
      if (cb.type === "text") {
        output.content.push({ type: "text", text: "", index: idx } as IndexedBlock)
        stream.push({
          type: "text_start",
          contentIndex: output.content.length - 1,
          partial: output,
        })
      } else if (cb.type === "thinking") {
        output.content.push({
          type: "thinking",
          thinking: "",
          thinkingSignature: "",
          index: idx,
        } as IndexedBlock)
        stream.push({
          type: "thinking_start",
          contentIndex: output.content.length - 1,
          partial: output,
        })
      } else if (cb.type === "tool_use") {
        const toolName = isOAuth
          ? fromClaudeCodeToolName(cb.name ?? "", context.tools)
          : (cb.name ?? "")
        output.content.push({
          type: "toolCall",
          id: cb.id ?? "",
          name: toolName,
          arguments: {},
          partialJson: "",
          index: idx,
        } as IndexedBlock)
        stream.push({
          type: "toolcall_start",
          contentIndex: output.content.length - 1,
          partial: output,
        })
      }
      return
    }

    if (type === "content_block_delta") {
      const idx = event.index as number
      const contentIndex = blocks.findIndex((b) => b.index === idx)
      const block = blocks[contentIndex]
      if (!block) return

      const delta = event.delta as { type: string; text?: string; thinking?: string; signature?: string; partial_json?: string }

      if (delta.type === "text_delta" && block.type === "text") {
        (block as TextContent & { index: number }).text += delta.text ?? ""
        stream.push({
          type: "text_delta",
          contentIndex,
          delta: delta.text ?? "",
          partial: output,
        })
      } else if (delta.type === "thinking_delta" && block.type === "thinking") {
        (block as ThinkingContent & { index: number }).thinking +=
          delta.thinking ?? ""
        stream.push({
          type: "thinking_delta",
          contentIndex,
          delta: delta.thinking ?? "",
          partial: output,
        })
      } else if (delta.type === "signature_delta" && block.type === "thinking") {
        const tb = block as ThinkingContent & {
          index: number
          thinkingSignature?: string
        }
        tb.thinkingSignature = (tb.thinkingSignature ?? "") + (delta.signature ?? "")
      } else if (delta.type === "input_json_delta" && block.type === "toolCall") {
        const tb = block as ToolCall & { index: number; partialJson: string }
        tb.partialJson += delta.partial_json ?? ""
        try {
          tb.arguments = JSON.parse(tb.partialJson) as Record<string, unknown>
        } catch {}
        stream.push({
          type: "toolcall_delta",
          contentIndex,
          delta: delta.partial_json ?? "",
          partial: output,
        })
      }
      return
    }

    if (type === "content_block_stop") {
      const idx = event.index as number
      const contentIndex = blocks.findIndex((b) => b.index === idx)
      const block = blocks[contentIndex]
      if (!block) return

      delete (block as { index?: number }).index
      if (block.type === "text") {
        stream.push({
          type: "text_end",
          contentIndex,
          content: (block as TextContent).text,
          partial: output,
        })
      } else if (block.type === "thinking") {
        stream.push({
          type: "thinking_end",
          contentIndex,
          content: (block as ThinkingContent).thinking,
          partial: output,
        })
      } else if (block.type === "toolCall") {
        const tb = block as ToolCall & { index?: number; partialJson?: string }
        try {
          tb.arguments = JSON.parse(tb.partialJson ?? "") as Record<
            string,
            unknown
          >
        } catch {}
        delete tb.partialJson
        stream.push({
          type: "toolcall_end",
          contentIndex,
          toolCall: block as ToolCall,
          partial: output,
        })
      }
      return
    }

    if (type === "message_delta") {
      const delta = event.delta as { stop_reason?: string }
      const usage = event.usage as {
        input_tokens?: number
        output_tokens?: number
        cache_read_input_tokens?: number
        cache_creation_input_tokens?: number
      }
      output.stopReason = mapStopReason(delta.stop_reason)
      if (usage) {
        output.usage.input = usage.input_tokens ?? output.usage.input
        output.usage.output = usage.output_tokens ?? output.usage.output
        output.usage.cacheRead = usage.cache_read_input_tokens ?? 0
        output.usage.cacheWrite = usage.cache_creation_input_tokens ?? 0
        output.usage.totalTokens =
          output.usage.input +
          output.usage.output +
          output.usage.cacheRead +
          output.usage.cacheWrite
      }
    }
  }

  // Read the SSE stream chunk by chunk
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    // Process complete SSE events (separated by \n\n)
    for (;;) {
      const boundary = buffer.indexOf("\n\n")
      if (boundary === -1) break
      const event = buffer.slice(0, boundary)
      buffer = buffer.slice(boundary + 2)
      if (event.trim()) processEvent(event)
    }
  }

  // Process any remaining buffer
  if (buffer.trim()) processEvent(buffer)

  return output
}
