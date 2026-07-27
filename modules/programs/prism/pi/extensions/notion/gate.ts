// Repo-scoping gate for the Notion MCP extension.
//
// The gate reads NOTION_MCP_REPOS as a colon-separated list of allowlisted
// directory prefixes. It returns true if:
//
//   1. NOTION_MCP_REPOS is unset OR empty (unrestricted default).
//   2. The current working directory is equal to, or a subpath of, at
//      least one prefix.
//
// The gate fails CLOSED on any error resolving the current working
// directory or evaluating the allowlist — it returns false rather than
// leaking the extension into an unintended workspace.
//
// The cwd resolution reads PRISM_WORKTREE first (prism dispatchers inject
// this into every isolated shell) and falls back to process.cwd(). Both
// the cwd and the allowlist entries are canonicalised through realpath()
// where possible so a symlinked prefix cannot cause a false negative.

import { realpathSync } from "node:fs"
import { homedir } from "node:os"
import { isAbsolute, resolve as pathResolve } from "node:path"

function expandHome(p: string): string {
  if (p === "~") return homedir()
  if (p.startsWith("~/")) return pathResolve(homedir(), p.slice(2))
  return p
}

function tryRealpath(p: string): string {
  try {
    return realpathSync(p)
  } catch {
    return p
  }
}

/**
 * Determine whether the Notion extension should run in this working
 * directory. Fails closed on any exception.
 */
export function notionEnabledForCwd(
  env: NodeJS.ProcessEnv = process.env,
  cwd: () => string = process.cwd.bind(process),
): boolean {
  try {
    const raw = env.NOTION_MCP_REPOS
    if (!raw) return true

    const entries = raw
      .split(":")
      .map((s) => s.trim())
      .filter((s) => s.length > 0)

    if (entries.length === 0) return true

    const currentDir = env.PRISM_WORKTREE ?? cwd()
    if (!currentDir || !isAbsolute(currentDir)) return false

    const normalisedCwd = tryRealpath(currentDir)

    for (const entry of entries) {
      const expanded = expandHome(entry)
      const absolute = isAbsolute(expanded) ? expanded : pathResolve(expanded)
      const normalised = tryRealpath(absolute)
      if (normalisedCwd === normalised) return true
      if (normalisedCwd.startsWith(normalised + "/")) return true
    }

    return false
  } catch {
    // Fail closed on any error — better to register no tools than to
    // register them against an unintended workspace.
    return false
  }
}
