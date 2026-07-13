// Regression tests for betas.ts.
// Run with: tsx --test betas.test.ts  (Node 20+, zero new deps)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { getModelBetas, isLongContextError } from "./betas.ts"

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
// getModelBetas — griffinmartin v2.0.0 port (issue #2382).
//
// Two properties matter here:
//
//   1. The override-exclude path uses `filter`, not indexOf/splice, so a
//      duplicate beta in `baseBetas` is fully removed. Griffinmartin 2.0.0
//      regenerated their config with `interleaved-thinking-2025-05-14`
//      listed twice; the old indexOf/splice mechanism only removed one
//      occurrence and would silently leave the beta on the wire for haiku.
//
//   2. The v2.0.0 removal of the 1M-context opt-in stays removed here — we
//      previously read `PI_ANTHROPIC_ENABLE_1M_CONTEXT` (pi divergence #4);
//      that env var is now inert.
// ---------------------------------------------------------------------------
describe("getModelBetas — v2.0.0 override-exclude filter fix", () => {
  it("removes EVERY occurrence of an excluded beta from baseBetas (v2.0.0 duplicate)", () => {
    // Positive assertion: haiku overrides exclude `interleaved-thinking-2025-05-14`,
    // and v2.0.0 `baseBetas` lists it twice. Both occurrences must be gone.
    //
    // Revert-and-fail check (not automated): change the override-exclude
    // handling in betas.ts back to indexOf/splice locally and rerun this
    // test — it will fail because only one occurrence is removed.
    const betas = getModelBetas("claude-haiku-4-5")
    const occurrences = betas.filter(
      (b) => b === "interleaved-thinking-2025-05-14",
    ).length
    assert.equal(
      occurrences,
      0,
      "haiku override-exclude must remove every occurrence, not just the first",
    )
  })

  it("removes EVERY occurrence when a synthetic duplicate is provided via ANTHROPIC_BETA_FLAGS", () => {
    // Independent of `baseBetas` shape: user-provided flags can also contain
    // duplicates. The filter-fix must handle that too.
    const saved = process.env.ANTHROPIC_BETA_FLAGS
    process.env.ANTHROPIC_BETA_FLAGS =
      "interleaved-thinking-2025-05-14,custom-beta-1,interleaved-thinking-2025-05-14"
    try {
      const betas = getModelBetas("claude-haiku-4-5")
      assert.ok(
        !betas.includes("interleaved-thinking-2025-05-14"),
        "haiku should exclude every occurrence of interleaved-thinking",
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
