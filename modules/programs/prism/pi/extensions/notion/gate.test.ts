// Unit tests for the repo-scoping gate — notionEnabledForCwd().
//
// The gate must:
//   - default to unrestricted when NOTION_MCP_REPOS is unset or empty;
//   - allow a cwd that is equal to (or a subpath of) an allowlist entry;
//   - reject a cwd that is outside the allowlist;
//   - fail closed on any exception during evaluation.
//
// Run with: tsx --test gate.test.ts (from this directory)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

import { notionEnabledForCwd } from "./gate.ts"

// ---------------------------------------------------------------------------
// Unset / empty allowlist
// ---------------------------------------------------------------------------

describe("notionEnabledForCwd — unrestricted defaults", () => {
  it("returns true when NOTION_MCP_REPOS is unset", () => {
    const result = notionEnabledForCwd({}, () => "/some/cwd")
    assert.equal(result, true)
  })

  it("returns true when NOTION_MCP_REPOS is the empty string", () => {
    const result = notionEnabledForCwd({ NOTION_MCP_REPOS: "" }, () => "/some/cwd")
    assert.equal(result, true)
  })

  it("returns true when NOTION_MCP_REPOS contains only whitespace-only entries", () => {
    const result = notionEnabledForCwd(
      { NOTION_MCP_REPOS: " : : " },
      () => "/some/cwd",
    )
    assert.equal(result, true)
  })
})

// ---------------------------------------------------------------------------
// Allowlist match / miss
// ---------------------------------------------------------------------------

describe("notionEnabledForCwd — allowlist matching", () => {
  it("matches a cwd equal to a single allowlist entry", () => {
    const tempDir = mkdtempSync(join(tmpdir(), "pi-notion-gate-"))
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: tempDir },
        () => tempDir,
      )
      assert.equal(result, true)
    } finally {
      rmSync(tempDir, { recursive: true, force: true })
    }
  })

  it("matches a cwd that is a subpath of an allowlist entry", () => {
    const tempDir = mkdtempSync(join(tmpdir(), "pi-notion-gate-"))
    const subdir = join(tempDir, "sub", "dir")
    mkdirSync(subdir, { recursive: true })
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: tempDir },
        () => subdir,
      )
      assert.equal(result, true)
    } finally {
      rmSync(tempDir, { recursive: true, force: true })
    }
  })

  it("rejects a cwd that shares a prefix but not a directory boundary", () => {
    // e.g. allowlist=/tmp/foo, cwd=/tmp/foobar should NOT match.
    const parent = mkdtempSync(join(tmpdir(), "pi-notion-gate-"))
    const foo = join(parent, "foo")
    const foobar = join(parent, "foobar")
    mkdirSync(foo)
    mkdirSync(foobar)
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: foo },
        () => foobar,
      )
      assert.equal(
        result,
        false,
        "prefix match without directory boundary must not count",
      )
    } finally {
      rmSync(parent, { recursive: true, force: true })
    }
  })

  it("rejects a cwd outside the allowlist entirely", () => {
    const tempDir = mkdtempSync(join(tmpdir(), "pi-notion-gate-"))
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: tempDir },
        () => "/some/other/absolute/path/that/does/not/exist",
      )
      assert.equal(result, false)
    } finally {
      rmSync(tempDir, { recursive: true, force: true })
    }
  })

  it("matches against ANY entry in a colon-separated allowlist", () => {
    const a = mkdtempSync(join(tmpdir(), "pi-notion-gate-a-"))
    const b = mkdtempSync(join(tmpdir(), "pi-notion-gate-b-"))
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: `${a}:${b}` },
        () => b,
      )
      assert.equal(result, true)
    } finally {
      rmSync(a, { recursive: true, force: true })
      rmSync(b, { recursive: true, force: true })
    }
  })

  it("resolves symlinked cwds through realpath", () => {
    const tempDir = mkdtempSync(join(tmpdir(), "pi-notion-gate-"))
    const target = join(tempDir, "real")
    const link = join(tempDir, "link")
    mkdirSync(target)
    symlinkSync(target, link)
    try {
      // Allowlist entry is the real path, cwd goes via the symlink.
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: target },
        () => link,
      )
      assert.equal(
        result,
        true,
        "symlinked cwd must be canonicalised before comparison",
      )
    } finally {
      rmSync(tempDir, { recursive: true, force: true })
    }
  })

  it("prefers PRISM_WORKTREE over process.cwd()", () => {
    const worktree = mkdtempSync(join(tmpdir(), "pi-notion-gate-wt-"))
    try {
      const result = notionEnabledForCwd(
        { NOTION_MCP_REPOS: worktree, PRISM_WORKTREE: worktree },
        () => "/something/completely/different",
      )
      assert.equal(result, true)
    } finally {
      rmSync(worktree, { recursive: true, force: true })
    }
  })
})

// ---------------------------------------------------------------------------
// Fail-closed on error
//
// Revert-and-watch-fail: if the outer try/catch is removed, a thrown cwd()
// propagates and this test fails.
// ---------------------------------------------------------------------------

describe("notionEnabledForCwd — fail closed", () => {
  it("returns false when cwd() throws", () => {
    const result = notionEnabledForCwd({ NOTION_MCP_REPOS: "/anywhere" }, () => {
      throw new Error("cwd unavailable")
    })
    assert.equal(result, false)
  })

  it("returns false when the resolved cwd is not absolute (e.g. empty string)", () => {
    const result = notionEnabledForCwd({ NOTION_MCP_REPOS: "/anywhere" }, () => "")
    assert.equal(result, false)
  })

  it("returns false when the resolved cwd is a relative path", () => {
    const result = notionEnabledForCwd(
      { NOTION_MCP_REPOS: "/anywhere" },
      () => "relative/path",
    )
    assert.equal(result, false)
  })
})
