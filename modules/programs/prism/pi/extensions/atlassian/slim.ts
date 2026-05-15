// Slim field-drop logic ported from opencode/mcp-atlassian-slim-proxy.mjs lines 12–115.
// Strips verbose/useless fields from Atlassian MCP responses before returning
// results to the LLM, keeping token usage down.
//
// Ported by: issue #1583 (pi extension for Atlassian MCP).
// Original source: modules/programs/prism/opencode/mcp-atlassian-slim-proxy.mjs
// Strip-list source: verified from actual Jira/Confluence queries (see UPSTREAM.md).

// Fields that are proven safe to drop universally (only UI/API metadata)
export const UNIVERSAL_DROP_KEYS = new Set(
  "expand,self,iconUrl,avatarUrl,avatarUrls,avatarId,picture,schema"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean),
)

// Fields to drop from Jira issue responses.
// Note: "transitions" is intentionally NOT in this list because the getJiraIssue
// response is augmented with a transitions array (Issue #2) and we want it preserved.
export const JIRA_ISSUE_DROP_KEYS = new Set(
  "renderedFields,operations,permissions,watchers,worklog,attachments,properties,names,subtask,hierarchyLevel,editmeta,versionedRepresentations,colorName"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean),
)

// Fields to drop from Confluence responses
// Note: url is NOT in this list because it's needed in search results for page ID extraction
export const CONFLUENCE_DROP_KEYS = new Set(
  "_links,status,lastModified"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean),
)

// Fields to drop from user info responses
export const USER_INFO_DROP_KEYS = new Set(
  "account_status,characteristics,last_updated,created_at,nickname,locale,extended_profile,account_type,email_verified"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean),
)

// Fields to drop from resource listing
export const RESOURCE_DROP_KEYS = new Set(
  "scopes,url"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean),
)

export interface SlimOptions {
  allowKeys: Set<string>
  dropKeys: Set<string>
}

/**
 * Build the slim options for a given MCP tool name.
 * Mirrors the optionsForMethod function in mcp-atlassian-slim-proxy.mjs.
 */
export function optionsForTool(toolName: string): SlimOptions {
  const name = String(toolName ?? "")
  let dropKeys = new Set<string>([...UNIVERSAL_DROP_KEYS])

  // Add method-specific drops based on tool name patterns
  if (/jira|issue/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...JIRA_ISSUE_DROP_KEYS])
  }
  if (/confluence|page|space/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...CONFLUENCE_DROP_KEYS])
  }
  if (/user.*info/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...USER_INFO_DROP_KEYS])
  }
  if (/resource/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...RESOURCE_DROP_KEYS])
  }
  // For search/list endpoints, also drop body/description/comments/changelog
  if (/search|list/i.test(name)) {
    dropKeys = new Set([
      ...dropKeys,
      "body",
      "description",
      "content",
      "comments",
      "comment",
      "changelog",
      "history",
      "adf",
    ])
  }

  return { allowKeys: new Set<string>(), dropKeys }
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    (Object.getPrototypeOf(value) === Object.prototype ||
      Object.getPrototypeOf(value) === null)
  )
}

/**
 * Recursively drop keys from a JSON value according to slim options.
 * Mirrors slimJson in mcp-atlassian-slim-proxy.mjs.
 */
export function slimJson(value: unknown, options: SlimOptions, depth = 0): unknown {
  if (value === null || value === undefined) return value
  if (
    typeof value === "string" ||
    typeof value === "number" ||
    typeof value === "boolean"
  ) {
    return value
  }
  if (Array.isArray(value)) {
    return value.map((v) => slimJson(v, options, depth + 1))
  }
  if (isPlainObject(value)) {
    const out: Record<string, unknown> = {}
    for (const [key, v] of Object.entries(value)) {
      if (options.dropKeys.has(key)) continue

      // If allowlist is configured, keep allowKeys plus structural keys
      const keepBecauseStructural =
        key === "error" ||
        key === "message" ||
        key === "tool" ||
        key === "type"
      if (
        !keepBecauseStructural &&
        options.allowKeys.size > 0 &&
        !options.allowKeys.has(key)
      ) {
        if (key !== "data" && key !== "result") continue
      }

      out[key] = slimJson(v, options, depth + 1)
    }

    // If object got emptied by allowlist, fall back to a generic slim
    if (Object.keys(out).length === 0 && Object.keys(value).length > 0) {
      for (const [key, v] of Object.entries(value)) {
        if (options.dropKeys.has(key)) continue
        out[key] = slimJson(v, options, depth + 1)
      }
    }

    return out
  }

  // Dates, Buffers, etc.
  try {
    return JSON.parse(JSON.stringify(value))
  } catch {
    return String(value)
  }
}

/**
 * Deduplicate an array of resources (e.g. from getAccessibleAtlassianResources)
 * by their `id` field. Order is preserved; first occurrence wins.
 */
export function deduplicateById<T extends Record<string, unknown>>(items: T[]): T[] {
  const seen = new Set<unknown>()
  const result: T[] = []
  for (const item of items) {
    const id = item["id"]
    if (!seen.has(id)) {
      seen.add(id)
      result.push(item)
    }
  }
  return result
}

/**
 * Pattern that indicates a wrong/missing cloudId error from the upstream MCP.
 */
export const CLOUD_ID_ERROR_PATTERN = "Failed to fetch tenant info for cloud ID"

/**
 * Build the hint to append when a cloudId error is detected.
 * If defaultCloudId is set, the hint points to using the default.
 * Otherwise it points to getAccessibleAtlassianResources.
 */
export function buildCloudIdErrorHint(defaultCloudId: string | undefined): string {
  if (defaultCloudId) {
    return "\nHint: this site has a configured default cloud ID — omit the cloudId parameter to use it."
  }
  return "\nHint: call getAccessibleAtlassianResources to discover valid cloud IDs."
}

/**
 * Apply slim field-drop logic to the text content of an MCP tool result.
 * The MCP result content is an array of {type, text} blocks.
 * For each text block, parse as JSON (if possible) and apply slimJson.
 * Returns a string (the slimmed text content).
 *
 * Also applies post-processing:
 * - Deduplicates resources by id for getAccessibleAtlassianResources.
 * - Appends a hint when a cloudId error is detected.
 */
export function slimMcpResultContent(
  content: unknown,
  toolName: string,
  defaultCloudId?: string,
): string {
  if (!Array.isArray(content)) {
    return typeof content === "string" ? content : JSON.stringify(content)
  }

  const options = optionsForTool(toolName)
  const parts: string[] = []

  for (const block of content) {
    if (
      block !== null &&
      typeof block === "object" &&
      (block as Record<string, unknown>).type === "text"
    ) {
      const text = (block as Record<string, unknown>).text
      if (typeof text === "string") {
        // Try to parse as JSON and slim
        try {
          const parsed = JSON.parse(text)

          // Issue #4: deduplicate resources for getAccessibleAtlassianResources
          let processed: unknown = parsed
          if (
            toolName === "getAccessibleAtlassianResources" &&
            Array.isArray(parsed)
          ) {
            processed = deduplicateById(parsed as Record<string, unknown>[])
          }

          const slimmed = slimJson(processed, options)
          parts.push(JSON.stringify(slimmed))
        } catch {
          // Not JSON — pass through as-is
          parts.push(text)
        }
      }
    }
  }

  // Issue #5: also check if the joined result contains the error pattern (JSON-parsed path)
  const joined = parts.join("\n")
  if (joined.includes(CLOUD_ID_ERROR_PATTERN)) {
    return joined + buildCloudIdErrorHint(defaultCloudId)
  }
  return joined
}
