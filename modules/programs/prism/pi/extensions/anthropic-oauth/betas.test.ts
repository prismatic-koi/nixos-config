// Regression tests for betas.ts.
// Run with: tsx --test betas.test.ts  (Node 20+, zero new deps)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import { isLongContextError } from "./betas.ts"

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
