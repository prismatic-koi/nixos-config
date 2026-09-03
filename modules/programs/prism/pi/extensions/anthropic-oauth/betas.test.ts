// Regression tests for betas.ts.
// Run with: tsx --test betas.test.ts  (Node 20+, zero new deps)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { getModelBetas, isLongContextError } from "./betas.ts"
import { config, getModelOverride } from "./model-config.ts"

// ---------------------------------------------------------------------------
// isLongContextError — griffinmartin v1.5.1 PR #211 parity (issue #2381).
// ---------------------------------------------------------------------------
describe("isLongContextError", () => {
  it('matches "Extra usage is required for long context requests"', () => {
    assert.ok(
      isLongContextError(
        "Extra usage is required for long context requests to model X",
      ),
    )
  })

  it('matches "long context beta is not yet available"', () => {
    assert.ok(
      isLongContextError(
        "the long context beta is not yet available for this account",
      ),
    )
  })

  // Added in v1.5.1 port (PR #211). AC8.
  it("matches \"You're out of extra usage\" (Max subscription quota)", () => {
    assert.ok(
      isLongContextError("You're out of extra usage"),
      "should detect bare out-of-extra-usage error",
    )
    assert.ok(
      isLongContextError(
        "You're out of extra usage. Add more at claude.ai/settings/usage and keep going.",
      ),
      "should detect full out-of-extra-usage message",
    )
    assert.ok(
      isLongContextError(
        `{"error": {"message": "You're out of extra usage. Add more at claude.ai/settings/usage and keep going."}}`,
      ),
      "should detect out-of-extra-usage error inside JSON envelope",
    )
  })

  it("does not match unrelated error strings", () => {
    assert.ok(!isLongContextError("rate limited"))
    assert.ok(!isLongContextError("invalid api key"))
    assert.ok(!isLongContextError(""))
  })
})

// ---------------------------------------------------------------------------
// model-config — griffinmartin v2.2.0 port, Claude CLI 2.1.257 (issue #2918).
//
// The Anthropic API rejects Fable 5.1 on subscription auth below Claude Code
// 2.1.251 with HTTP 400 `claude_code_version_too_old`, so `ccVersion` is
// load-bearing, not cosmetic. The betas that ride with it were regenerated
// from 2.1.257 intercept traffic; a wrong list breaks auth for every session
// on this machine, so pin the exact shape rather than a substring of it.
// ---------------------------------------------------------------------------
describe("model-config — Claude CLI 2.1.257 (griffinmartin v2.2.0)", () => {
  it("pins ccVersion at 2.1.257", () => {
    assert.equal(config.ccVersion, "2.1.257")
  })

  it("declares baseBetas exactly as upstream: eight entries, no duplicate, no effort", () => {
    assert.deepEqual(config.baseBetas, [
      "claude-code-20250219",
      "oauth-2025-04-20",
      "interleaved-thinking-2025-05-14",
      "prompt-caching-scope-2026-01-05",
      "context-management-2025-06-27",
      "advisor-tool-2026-03-01",
      "thinking-token-count-2026-05-13",
      "extended-cache-ttl-2025-04-11",
    ])
    assert.equal(
      new Set(config.baseBetas).size,
      config.baseBetas.length,
      "v2.2.0 removed the deliberate interleaved-thinking duplicate",
    )
  })

  it("orders modelOverrides haiku-first so haiku never reaches an effort add", () => {
    // getModelOverride is first-match-wins over insertion order, and
    // "claude-haiku-4-5" contains neither "opus-4-5" nor "4-6"/"4-7" — but
    // key order is still the upstream-pinned contract, so assert it.
    assert.deepEqual(Object.keys(config.modelOverrides), [
      "haiku",
      "opus-4-5",
      "4-6",
      "4-7",
    ])
    assert.deepEqual(getModelOverride("claude-haiku-4-5"), {
      exclude: ["effort-2025-11-24"],
      disableEffort: true,
    })
    for (const model of ["claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7"]) {
      assert.deepEqual(
        getModelOverride(model),
        { add: ["effort-2025-11-24"] },
        `${model} must add the effort beta`,
      )
    }
  })

  it("sends the effort beta only for the models 2.1.257 sends it for", () => {
    // Upstream's "effort beta" test, mirrored. effort-2025-11-24 left
    // baseBetas in v2.2.0, so it now rides on per-model adds only.
    for (const model of [
      "claude-opus-4-5",
      "claude-opus-4-6",
      "claude-sonnet-4-6",
      "claude-opus-4-7",
    ]) {
      assert.ok(
        getModelBetas(model).includes("effort-2025-11-24"),
        `${model} must include the effort beta`,
      )
    }
    for (const model of ["claude-sonnet-4-5", "claude-haiku-4-5"]) {
      assert.ok(
        !getModelBetas(model).includes("effort-2025-11-24"),
        `${model} must not include the effort beta`,
      )
    }
  })
})

// ---------------------------------------------------------------------------
// getModelBetas — griffinmartin v2.0.0 port (issue #2382).
//
// Two properties matter here:
//
//   1. The override-exclude path uses `filter`, not indexOf/splice, so every
//      occurrence of an excluded beta is removed. v2.2.0 dropped the
//      duplicate that `baseBetas` used to carry, so the remaining live
//      source of a duplicate is the user's own `ANTHROPIC_BETA_FLAGS` —
//      which is what these tests feed in. indexOf/splice strips only the
//      first and leaves the beta on the wire for haiku.
//
//   2. The v2.0.0 removal of the 1M-context opt-in stays removed here — we
//      previously read `PI_ANTHROPIC_ENABLE_1M_CONTEXT` (pi divergence #4);
//      that env var is now inert.
// ---------------------------------------------------------------------------
describe("getModelBetas — v2.0.0 override-exclude filter fix", () => {
  it("removes EVERY occurrence of an excluded beta, not just the first", () => {
    // Revert-and-fail check (not automated): change the override-exclude
    // handling in betas.ts back to indexOf/splice locally and rerun this
    // test — it will fail because only one occurrence is removed.
    const saved = process.env.ANTHROPIC_BETA_FLAGS
    process.env.ANTHROPIC_BETA_FLAGS =
      "effort-2025-11-24,custom-beta-1,effort-2025-11-24"
    try {
      const betas = getModelBetas("claude-haiku-4-5")
      const occurrences = betas.filter((b) => b === "effort-2025-11-24").length
      assert.equal(
        occurrences,
        0,
        "haiku override-exclude must remove every occurrence, not just the first",
      )
      assert.ok(
        betas.includes("custom-beta-1"),
        "unrelated user-provided beta should remain",
      )
    } finally {
      if (saved === undefined) delete process.env.ANTHROPIC_BETA_FLAGS
      else process.env.ANTHROPIC_BETA_FLAGS = saved
    }
  })

  it("removes EVERY occurrence on the forceAdaptiveThinking path too (pi divergence #10)", () => {
    // Divergence #10 filters interleaved-thinking for adaptive models. It has
    // its own filter call, so it needs its own duplicate coverage: a user can
    // list the beta twice in ANTHROPIC_BETA_FLAGS.
    const saved = process.env.ANTHROPIC_BETA_FLAGS
    process.env.ANTHROPIC_BETA_FLAGS =
      "interleaved-thinking-2025-05-14,custom-beta-1,interleaved-thinking-2025-05-14"
    try {
      const betas = getModelBetas("claude-fable-5-1", undefined, {
        forceAdaptiveThinking: true,
      })
      const occurrences = betas.filter(
        (b) => b === "interleaved-thinking-2025-05-14",
      ).length
      assert.equal(
        occurrences,
        0,
        "adaptive suppression must remove every occurrence, not just the first",
      )
      assert.ok(
        betas.includes("custom-beta-1"),
        "unrelated user-provided beta should remain",
      )
    } finally {
      if (saved === undefined) delete process.env.ANTHROPIC_BETA_FLAGS
      else process.env.ANTHROPIC_BETA_FLAGS = saved
    }
  })
})

describe("getModelBetas — 1M context opt-in removed (v2.0.0)", () => {
  it("does NOT add context-1m-2025-08-07 even with PI_ANTHROPIC_ENABLE_1M_CONTEXT=true", () => {
    // Pre-2.0.0, PI_ANTHROPIC_ENABLE_1M_CONTEXT=true auto-injected
    // context-1m-2025-08-07 for 4.6+ opus/sonnet models. Griffinmartin
    // 2.0.0 removed the opt-in (the API supports 1M natively). Verify the
    // env var is inert now.
    const saved = process.env.PI_ANTHROPIC_ENABLE_1M_CONTEXT
    process.env.PI_ANTHROPIC_ENABLE_1M_CONTEXT = "true"
    try {
      for (const model of [
        "claude-opus-4-6",
        "claude-opus-4-7",
        "claude-opus-4-8",
        "claude-sonnet-4-6",
      ]) {
        const betas = getModelBetas(model)
        assert.ok(
          !betas.includes("context-1m-2025-08-07"),
          `${model} must not receive context-1m even with the pi opt-in env var set`,
        )
      }
    } finally {
      if (saved === undefined) delete process.env.PI_ANTHROPIC_ENABLE_1M_CONTEXT
      else process.env.PI_ANTHROPIC_ENABLE_1M_CONTEXT = saved
    }
  })

  it("still allows a user to opt back in via ANTHROPIC_BETA_FLAGS manually", () => {
    // The long-context retry machinery (LONG_CONTEXT_BETAS, getNextBetaToExclude)
    // is intentionally retained per the AC \u2014 a user who *knows* they want
    // context-1m can still add it via ANTHROPIC_BETA_FLAGS and get the
    // graceful-degradation retry loop.
    const saved = process.env.ANTHROPIC_BETA_FLAGS
    process.env.ANTHROPIC_BETA_FLAGS = "context-1m-2025-08-07,claude-code-20250219"
    try {
      const betas = getModelBetas("claude-opus-4-8")
      assert.ok(
        betas.includes("context-1m-2025-08-07"),
        "manual opt-in via ANTHROPIC_BETA_FLAGS should still work",
      )
    } finally {
      if (saved === undefined) delete process.env.ANTHROPIC_BETA_FLAGS
      else process.env.ANTHROPIC_BETA_FLAGS = saved
    }
  })
})
