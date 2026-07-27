// Unit tests for notion/scope.ts — the repo-scoping gate.
//
// Run with: tsx --test scope.test.ts (from this directory)
//
// Revert-and-watch-fail for the fail-closed contract: change the `catch {
// return false }` in isNotionEnabledForCwd to `catch { return true }` and
// "fails closed when the working directory cannot be resolved" fails.

import { describe, it, beforeEach, afterEach } from "node:test"
import assert from "node:assert/strict"
import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync } from "node:fs"
import { homedir, tmpdir } from "node:os"
import { join } from "node:path"

import {
  expandPath,
  isNotionEnabledForCwd,
  matchesAllowlist,
  parseAllowlist,
  resolveWorkingDirectory,
} from "./scope.ts"

let tempDir: string

beforeEach(() => {
  // realpathSync so macOS's /var -> /private/var symlink does not make every
  // comparison in here a false negative.
  tempDir = realpathSync(mkdtempSync(join(tmpdir(), "pi-notion-scope-test-")))
  delete process.env.NOTION_MCP_REPOS
})

afterEach(() => {
  delete process.env.NOTION_MCP_REPOS
  rmSync(tempDir, { recursive: true, force: true })
})

// ---------------------------------------------------------------------------
// expandPath
// ---------------------------------------------------------------------------

describe("expandPath", () => {
  it("expands a leading tilde", () => {
    assert.equal(expandPath("~/Documents/obsidian"), join(homedir(), "Documents/obsidian"))
    assert.equal(expandPath("~"), homedir())
  })

  it("expands $HOME and ${HOME}, which the Go isolators inject verbatim", () => {
    // internal/container/env.go sets AgentEnvVars with no shell in the loop,
    // so an unexpanded $HOME really does reach the extension inside a sandbox.
    assert.equal(expandPath("$HOME/code"), join(homedir(), "code"))
    assert.equal(expandPath("${HOME}/code"), join(homedir(), "code"))
  })

  it("leaves an absolute path alone but strips a trailing separator", () => {
    assert.equal(expandPath("/a/b"), "/a/b")
    assert.equal(expandPath("/a/b/"), "/a/b")
    assert.equal(expandPath("/"), "/")
  })

  it("trims surrounding whitespace", () => {
    assert.equal(expandPath("  /a/b  "), "/a/b")
  })

  it("returns empty for empty input", () => {
    assert.equal(expandPath("   "), "")
  })

  it("does not expand a tilde that is not a home reference", () => {
    assert.equal(expandPath("/tmp/~weird"), "/tmp/~weird")
  })
})

// ---------------------------------------------------------------------------
// parseAllowlist
// ---------------------------------------------------------------------------

describe("parseAllowlist", () => {
  it("returns an empty list for unset, empty or separator-only values", () => {
    assert.deepEqual(parseAllowlist(undefined), [])
    assert.deepEqual(parseAllowlist(""), [])
    assert.deepEqual(parseAllowlist(":::"), [])
    assert.deepEqual(parseAllowlist("  "), [])
  })

  it("splits on colons and expands each entry", () => {
    const parsed = parseAllowlist(`${tempDir}:~/code`)
    assert.deepEqual(parsed, [tempDir, join(homedir(), "code")])
  })

  it("keeps a non-existent entry lexically so it simply never matches", () => {
    const missing = join(tempDir, "does-not-exist")
    assert.deepEqual(parseAllowlist(missing), [missing])
  })
})

// ---------------------------------------------------------------------------
// matchesAllowlist
// ---------------------------------------------------------------------------

describe("matchesAllowlist", () => {
  it("matches the directory itself", () => {
    assert.equal(matchesAllowlist("/a/b", ["/a/b"]), true)
  })

  it("matches a descendant", () => {
    assert.equal(matchesAllowlist("/a/b/c/d", ["/a/b"]), true)
  })

  it("does not match a sibling with a shared textual prefix", () => {
    assert.equal(
      matchesAllowlist("/a/bcd", ["/a/b"]),
      false,
      "prefix matching must be separator-aware",
    )
  })

  it("does not match an ancestor", () => {
    assert.equal(matchesAllowlist("/a", ["/a/b"]), false)
  })

  it("matches when any one entry matches", () => {
    assert.equal(matchesAllowlist("/x/y", ["/a/b", "/x", "/q"]), true)
  })

  it("never matches against an empty list", () => {
    assert.equal(matchesAllowlist("/a/b", []), false)
  })
})

// ---------------------------------------------------------------------------
// resolveWorkingDirectory
// ---------------------------------------------------------------------------

describe("resolveWorkingDirectory", () => {
  it("canonicalises through realpath so a symlinked worktree still matches", () => {
    const real = join(tempDir, "real")
    const link = join(tempDir, "link")
    mkdirSync(real)
    symlinkSync(real, link)

    assert.equal(resolveWorkingDirectory(link), real)
  })

  it("throws for a directory that does not exist", () => {
    assert.throws(() => resolveWorkingDirectory(join(tempDir, "nope")))
  })
})

// ---------------------------------------------------------------------------
// isNotionEnabledForCwd  (AC: edge-case / scoping + fail-closed)
// ---------------------------------------------------------------------------

describe("isNotionEnabledForCwd", () => {
  it("is unrestricted when the allowlist is unset", () => {
    assert.equal(isNotionEnabledForCwd({ cwd: tempDir }), true)
  })

  it("is unrestricted when the allowlist is empty or separator-only", () => {
    assert.equal(isNotionEnabledForCwd({ cwd: tempDir, allowlist: "" }), true)
    assert.equal(isNotionEnabledForCwd({ cwd: tempDir, allowlist: "::" }), true)
  })

  it("enables inside an allowlisted directory", () => {
    assert.equal(isNotionEnabledForCwd({ cwd: tempDir, allowlist: tempDir }), true)
  })

  it("enables in a subdirectory of an allowlisted directory", () => {
    const sub = join(tempDir, "nested", "deeper")
    mkdirSync(sub, { recursive: true })
    assert.equal(isNotionEnabledForCwd({ cwd: sub, allowlist: tempDir }), true)
  })

  it("disables outside the allowlist — no tools, no connection", () => {
    const other = realpathSync(mkdtempSync(join(tmpdir(), "pi-notion-scope-other-")))
    try {
      assert.equal(isNotionEnabledForCwd({ cwd: other, allowlist: tempDir }), false)
    } finally {
      rmSync(other, { recursive: true, force: true })
    }
  })

  it("does not let a sibling with a shared prefix slip through", () => {
    const allowed = join(tempDir, "vault")
    const sibling = join(tempDir, "vault-notes")
    mkdirSync(allowed)
    mkdirSync(sibling)

    assert.equal(isNotionEnabledForCwd({ cwd: sibling, allowlist: allowed }), false)
    assert.equal(isNotionEnabledForCwd({ cwd: allowed, allowlist: allowed }), true)
  })

  it("resolves a symlinked working directory to its target before matching", () => {
    const real = join(tempDir, "real-vault")
    const link = join(tempDir, "link-vault")
    mkdirSync(real)
    symlinkSync(real, link)

    assert.equal(isNotionEnabledForCwd({ cwd: link, allowlist: real }), true)
  })

  it("fails closed when the working directory cannot be resolved", () => {
    assert.equal(
      isNotionEnabledForCwd({ cwd: join(tempDir, "vanished"), allowlist: tempDir }),
      false,
      "an unresolvable cwd must not silently widen the scope back to everywhere",
    )
  })

  it("fails closed rather than matching a non-existent allowlist entry", () => {
    assert.equal(
      isNotionEnabledForCwd({ cwd: tempDir, allowlist: join(tempDir, "typo") }),
      false,
    )
  })

  it("reads NOTION_MCP_REPOS from the environment when no override is given", () => {
    process.env.NOTION_MCP_REPOS = tempDir
    assert.equal(isNotionEnabledForCwd({ cwd: tempDir }), true)

    const other = realpathSync(mkdtempSync(join(tmpdir(), "pi-notion-scope-env-")))
    try {
      assert.equal(isNotionEnabledForCwd({ cwd: other }), false)
    } finally {
      rmSync(other, { recursive: true, force: true })
    }
  })

  it("honours a multi-entry allowlist", () => {
    const a = join(tempDir, "a")
    const b = join(tempDir, "b")
    const c = join(tempDir, "c")
    mkdirSync(a)
    mkdirSync(b)
    mkdirSync(c)

    const allowlist = `${a}:${b}`
    assert.equal(isNotionEnabledForCwd({ cwd: a, allowlist }), true)
    assert.equal(isNotionEnabledForCwd({ cwd: b, allowlist }), true)
    assert.equal(isNotionEnabledForCwd({ cwd: c, allowlist }), false)
  })
})
