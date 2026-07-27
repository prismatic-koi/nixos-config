// Repo-scoping gate for the Notion MCP extension.
//
// pi's extension list (~/.pi/agent/settings.json) is global — prism forces
// discovery to that one directory via PI_CODING_AGENT_DIR, and there is no
// per-project settings layer to hook. So the extension self-gates instead.
//
// Three things this buys, in descending order of importance:
//
//   1. Context-window budget. Notion exposes ~10 tool schemas. Carrying them
//      in the system prompt of every session in every repo that will never
//      call them is a real, recurring cost.
//   2. Blast radius. A Notion grant is full workspace read/write ("AI tools
//      can read and write to your Notion pages just like you can"), and the
//      tool surface includes notion-update-page, notion-move-pages and
//      notion-duplicate-page. A code-repo agent has no business holding it.
//   3. Fewer concurrent refreshers. Every session that skips the connection
//      is one fewer participant in the refresh-rotation race that auth.ts
//      exists to serialise. See auth.ts and UPSTREAM.md.
//
// Delivery: the NOTION_MCP_REPOS environment variable, a colon-separated list
// of directory paths, set from nx.programs.prism.pi.notion.repos. It is
// plumbed through nx.programs.prism.agent.envVars (NOT the zsh alias) so it
// reaches prism-spawned agents as well as interactive shells.
//
// SCOPE NOTE: this is a scoping and least-privilege control, not a security
// boundary against a hostile agent — an agent that can run arbitrary code in
// its own process can obviously read the token file directly. Its job is to
// keep the honest case narrow.

import { realpathSync } from "node:fs"
import { homedir } from "node:os"
import { isAbsolute, resolve, sep } from "node:path"

/**
 * Expand a leading `~`, `$HOME` or `${HOME}` and resolve to an absolute path
 * with any trailing separator removed.
 *
 * Both forms genuinely occur. The zsh alias performs tilde expansion itself,
 * but prism's Go isolators inject AgentEnvVars verbatim (see
 * internal/container/env.go — no shell, no expansion), so a `~/...` entry
 * arrives at the extension unexpanded inside a sandbox.
 */
export function expandPath(input: string): string {
  let path = input.trim()
  if (!path) return ""

  const home = homedir()
  if (path === "~" || path === "$HOME" || path === "${HOME}") {
    path = home
  } else if (path.startsWith("~/")) {
    path = resolve(home, path.slice(2))
  } else if (path.startsWith("$HOME/")) {
    path = resolve(home, path.slice(6))
  } else if (path.startsWith("${HOME}/")) {
    path = resolve(home, path.slice(8))
  }

  const absolute = isAbsolute(path) ? path : resolve(path)
  return absolute.length > 1 && absolute.endsWith(sep)
    ? absolute.slice(0, -sep.length)
    : absolute
}

/**
 * Canonicalise a path through realpathSync, falling back to the lexical form
 * when the path does not exist.
 *
 * Used for allowlist entries, where a typo'd or not-yet-created directory
 * should simply never match rather than throw. The working directory is
 * canonicalised strictly (see `resolveWorkingDirectory`).
 */
function canonicaliseLenient(path: string): string {
  try {
    return realpathSync(path)
  } catch {
    return path
  }
}

/** Split NOTION_MCP_REPOS into expanded, canonicalised entries. */
export function parseAllowlist(raw: string | undefined): string[] {
  if (!raw) return []
  return raw
    .split(":")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .map((entry) => canonicaliseLenient(expandPath(entry)))
    .filter(Boolean)
}

/**
 * Resolve the session's working directory, canonicalised through realpath so
 * a symlinked worktree still matches an allowlist entry naming its target
 * (and vice versa).
 *
 * `process.cwd()` is authoritative in every mode we run in: bwrap passes
 * `--chdir <worktree>`, sandbox-exec and host mode both start the agent in
 * the worktree, and an interactive `pi` inherits the shell's directory.
 * PRISM_WORKTREE is deliberately NOT consulted — it says the same thing but
 * is an environment variable, and preferring it would let the value be
 * pointed somewhere else without moving the process.
 *
 * Throws if the directory cannot be resolved; the caller fails closed.
 */
export function resolveWorkingDirectory(cwd?: string): string {
  return realpathSync(expandPath(cwd ?? process.cwd()))
}

/** True when `cwd` is an allowlisted directory or lives beneath one. */
export function matchesAllowlist(cwd: string, entries: string[]): boolean {
  return entries.some((entry) => cwd === entry || cwd.startsWith(entry + sep))
}

export interface ScopeOptions {
  /** Override the working directory (tests). */
  cwd?: string
  /** Override the raw allowlist string (tests). */
  allowlist?: string
}

/**
 * Decide whether the Notion tool surface should be exposed in this session.
 *
 * - Allowlist unset or empty  → unrestricted, returns true.
 * - Allowlist set             → true only when the working directory is, or
 *                               lives beneath, an allowlisted entry.
 * - Anything throws           → FAILS CLOSED, returns false. An unresolvable
 *                               working directory must not silently widen the
 *                               scope back to "everywhere".
 */
export function isNotionEnabledForCwd(opts: ScopeOptions = {}): boolean {
  try {
    const raw = opts.allowlist ?? process.env.NOTION_MCP_REPOS
    const entries = parseAllowlist(raw)
    // No allowlist configured — the option is opt-in, so an absent value
    // means "no scoping requested", not "deny everything".
    if (entries.length === 0) return true

    const cwd = resolveWorkingDirectory(opts.cwd)
    return matchesAllowlist(cwd, entries)
  } catch {
    return false
  }
}
