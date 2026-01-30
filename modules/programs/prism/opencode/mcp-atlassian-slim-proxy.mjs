#!/usr/bin/env node

// MCP stdio proxy that filters out verbose fields from Atlassian MCP responses.
// Wraps mcp-remote to reduce token usage while preserving functionality.

import process from "node:process";
import { spawn } from "node:child_process";
import fs from "node:fs";

const REMOTE_URL = process.env.ATLASSIAN_MCP_URL ?? "https://mcp.atlassian.com/v1/mcp";

// Fields that are PROVEN safe to drop universally (only UI/API metadata we've verified)
const UNIVERSAL_DROP_KEYS = new Set(
  "expand,self,iconUrl,avatarUrl,avatarUrls,avatarId,picture,schema"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
);

// Fields to drop from Jira issue responses (verified from actual Jira queries)
const JIRA_ISSUE_DROP_KEYS = new Set(
  "renderedFields,operations,permissions,transitions,watchers,worklog,attachments,properties,names,subtask,hierarchyLevel,editmeta,versionedRepresentations,colorName"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
);

// Fields to drop from Confluence responses (verified from actual Confluence queries)
// Note: url is NOT in this list because it's needed in search results for page ID extraction
const CONFLUENCE_DROP_KEYS = new Set(
  "_links,status,lastModified"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
);

// Fields to drop from user info responses (verified from actual userInfo query)
const USER_INFO_DROP_KEYS = new Set(
  "account_status,characteristics,last_updated,created_at,nickname,locale,extended_profile,account_type,email_verified"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
);

// Fields to drop from resource listing (verified from actual resources query)
const RESOURCE_DROP_KEYS = new Set(
  "scopes,url"
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
);

// Allow override via env var
const ENV_DROP_KEYS = (process.env.MCP_SLIM_DROP_KEYS ?? "")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

const DEFAULT_ALLOW_KEYS = (process.env.MCP_SLIM_ALLOW_KEYS ?? "")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

function optionsForMethod(method) {
  const name = String(method ?? "");
  
  let dropKeys = new Set([...UNIVERSAL_DROP_KEYS, ...ENV_DROP_KEYS]);

  // Add method-specific drops
  if (/jira|issue/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...JIRA_ISSUE_DROP_KEYS]);
  }
  
  if (/confluence|page|space/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...CONFLUENCE_DROP_KEYS]);
  }
  
  if (/user.*info/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...USER_INFO_DROP_KEYS]);
  }
  
  if (/resource/i.test(name)) {
    dropKeys = new Set([...dropKeys, ...RESOURCE_DROP_KEYS]);
  }

  // For search/list endpoints, also drop body/description/comments
  if (/search|list/i.test(name)) {
    dropKeys = new Set([...dropKeys, "body", "description", "content", "comments", "comment", "changelog", "history", "adf"]);
  }

  return {
    allowKeys: new Set(DEFAULT_ALLOW_KEYS),
    dropKeys,
  };
}

function isPlainObject(value) {
  return (
    value !== null &&
    typeof value === "object" &&
    (value.constructor === Object || Object.getPrototypeOf(value) === null)
  );
}

function slimJson(value, options, depth = 0) {
  if (value === null || value === undefined) return value;
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") return value;

  if (Array.isArray(value)) {
    return value.map((v) => slimJson(v, options, depth + 1));
  }

  if (isPlainObject(value)) {
    const out = {};
    for (const [key, v] of Object.entries(value)) {
      if (options.dropKeys.has(key)) continue;

      // If allowlist is configured, keep allowKeys plus structural keys
      const keepBecauseStructural = key === "error" || key === "message" || key === "tool" || key === "type";
      if (!keepBecauseStructural && options.allowKeys.size > 0 && !options.allowKeys.has(key)) {
        if (key !== "data" && key !== "result") continue;
      }

      out[key] = slimJson(v, options, depth + 1);
    }

    // If object got emptied by allowlist, fall back to a generic slim
    if (Object.keys(out).length === 0 && Object.keys(value).length > 0) {
      for (const [key, v] of Object.entries(value)) {
        if (options.dropKeys.has(key)) continue;
        out[key] = slimJson(v, options, depth + 1);
      }
    }

    return out;
  }

  // Dates, Buffers, etc.
  try {
    return JSON.parse(JSON.stringify(value));
  } catch {
    return String(value);
  }
}

function slimmingDisabled() {
  return process.env.MCP_SLIM_DISABLE === "1" || process.env.MCP_SLIM_DISABLE === "true";
}

function slimResponseLine(line) {
  if (slimmingDisabled()) return line;

  try {
    const msg = JSON.parse(line);
    
    // Debug mode: write full response to file for analysis (only tool invocations, not initialization)
    // Tool invocation responses have result.content (array of tool results)
    if (process.env.MCP_SLIM_DEBUG === "true" && msg && typeof msg === "object" && "result" in msg) {
      const isToolResponse = Array.isArray(msg.result?.content);
      if (isToolResponse) {
        // Write raw response - use append mode to capture multiple calls
        const rawDebugFile = process.env.HOME + "/.local/state/opencode/mcp-raw-responses.jsonl";
        try {
          // Unpack the stringified JSON in result.content[0].text if it exists
          const unpackedMsg = JSON.parse(JSON.stringify(msg));
          if (unpackedMsg.result?.content?.[0]?.text) {
            try {
              const parsedText = JSON.parse(unpackedMsg.result.content[0].text);
              unpackedMsg.result.content[0].parsedText = parsedText;
            } catch {
              // If text is not JSON, leave it as is
            }
          }
          // Append as JSONL (one JSON object per line)
          fs.appendFileSync(rawDebugFile, JSON.stringify(unpackedMsg) + "\n");
          process.stderr.write(`[mcp-slim] Raw response appended to ${rawDebugFile}\n`);
        } catch (err) {
          process.stderr.write(`[mcp-slim] Failed to write raw debug file: ${err.message}\n`);
        }
      }
    }
    
    if (msg && typeof msg === "object" && "result" in msg) {
      const method = msg?.result?._meta?.method ?? msg?.method;
      
      // Special handling: parse and filter stringified JSON in result.content[0].text
      if (Array.isArray(msg.result?.content)) {
        for (const item of msg.result.content) {
          if (item.type === "text" && typeof item.text === "string") {
            try {
              const parsed = JSON.parse(item.text);
              const slimmed = slimJson(parsed, optionsForMethod(method));
              item.text = JSON.stringify(slimmed);
            } catch {
              // If text is not JSON, leave it as is
            }
          }
        }
      }
      
      // Also filter the MCP wrapper structure
      msg.result = slimJson(msg.result, optionsForMethod(method));
      
      // Debug mode: write filtered response after slimming
      if (process.env.MCP_SLIM_DEBUG === "true") {
        const isToolResponse = Array.isArray(msg.result?.content);
        if (isToolResponse) {
          const filteredDebugFile = process.env.HOME + "/.local/state/opencode/mcp-filtered-responses.jsonl";
          try {
            // Unpack the stringified JSON in result.content[0].text if it exists
            const unpackedMsg = JSON.parse(JSON.stringify(msg));
            if (unpackedMsg.result?.content?.[0]?.text) {
              try {
                const parsedText = JSON.parse(unpackedMsg.result.content[0].text);
                unpackedMsg.result.content[0].parsedText = parsedText;
              } catch {
                // If text is not JSON, leave it as is
              }
            }
            // Append as JSONL (one JSON object per line)
            fs.appendFileSync(filteredDebugFile, JSON.stringify(unpackedMsg) + "\n");
            process.stderr.write(`[mcp-slim] Filtered response appended to ${filteredDebugFile}\n`);
          } catch (err) {
            process.stderr.write(`[mcp-slim] Failed to write filtered debug file: ${err.message}\n`);
          }
        }
      }
      
      return JSON.stringify(msg);
    }
    return line;
  } catch {
    return line;
  }
}

async function main() {
  // Clear debug logs at start of new session
  if (process.env.MCP_SLIM_DEBUG === "true") {
    const debugDir = process.env.HOME + "/.local/state/opencode";
    const rawLog = debugDir + "/mcp-raw-responses.jsonl";
    const filteredLog = debugDir + "/mcp-filtered-responses.jsonl";
    try {
      if (fs.existsSync(rawLog)) fs.unlinkSync(rawLog);
      if (fs.existsSync(filteredLog)) fs.unlinkSync(filteredLog);
      process.stderr.write("[mcp-slim] Debug logs cleared\n");
    } catch (err) {
      process.stderr.write(`[mcp-slim] Failed to clear debug logs: ${err.message}\n`);
    }
  }

  const child = spawn("npx", ["-y", "mcp-remote@0.1.13", REMOTE_URL], {
    stdio: ["pipe", "pipe", "pipe"],
    env: process.env,
  });

  child.on("exit", (code) => process.exit(code ?? 1));
  child.on("error", (err) => {
    process.stderr.write(`Failed to spawn mcp-remote: ${err.message}\n`);
    process.exit(1);
  });

  // Forward stdin/stderr unchanged
  process.stdin.pipe(child.stdin);
  child.stderr.pipe(process.stderr);

  // Process stdout line-by-line, filtering verbose fields
  child.stdout.setEncoding("utf8");
  let buffer = "";
  for await (const chunk of child.stdout) {
    buffer += chunk;
    let idx;
    while ((idx = buffer.indexOf("\n")) !== -1) {
      const line = buffer.slice(0, idx).trim();
      buffer = buffer.slice(idx + 1);
      
      if (!line) {
        process.stdout.write("\n");
        continue;
      }

      process.stdout.write(slimResponseLine(line) + "\n");
    }
  }
}

main().catch((e) => {
  process.stderr.write(String(e?.stack ?? e) + "\n");
  process.exit(1);
});
