// Response flattening for Notion MCP tool results.
//
// STATUS: deliberate passthrough stub. This is the seam where a Notion
// equivalent of atlassian/slim.ts (recursive drop-key field removal, to keep
// verbose MCP responses from eating the context window) will live.
//
// Why a stub rather than a port. The Atlassian drop-key sets
// (UNIVERSAL_DROP_KEYS, JIRA_ISSUE_DROP_KEYS, CONFLUENCE_DROP_KEYS, ...) were
// derived empirically from real Jira/Confluence payloads and are entirely
// Atlassian-shaped — `expand`, `avatarUrls`, `renderedFields`, `_links`. None
// of them appear in Notion responses. The Notion targets are different in
// kind (notion-fetch returns full page content; database responses carry
// schema blobs) and picking drop keys without a live grant to probe would be
// guesswork that silently deletes fields the agent needs.
//
// Copying the Atlassian sets across would be worse than doing nothing: a
// no-op at best, and a silent data-loss bug the first time Notion ships a
// field that happens to collide with a Jira field name.
//
// Follow-up: derive the drop-key sets empirically against real responses once
// the extension is in day-to-day use, then replace this module and add the
// slim.test.ts fixtures. Tracked as a note in UPSTREAM.md.

/**
 * Flatten an MCP tool result's content array into the string pi returns to
 * the model.
 *
 * Current behaviour is lossless: text blocks are concatenated verbatim.
 * Non-text blocks are dropped, matching the Atlassian implementation's
 * contract (pi's tool-result shape is a single text block).
 */
export function slimMcpResultContent(content: unknown): string {
  if (!Array.isArray(content)) {
    return typeof content === "string" ? content : JSON.stringify(content)
  }

  const parts: string[] = []
  for (const block of content) {
    if (block !== null && typeof block === "object") {
      const record = block as Record<string, unknown>
      if (record.type === "text" && typeof record.text === "string") {
        parts.push(record.text)
      }
    }
  }
  return parts.join("\n")
}
