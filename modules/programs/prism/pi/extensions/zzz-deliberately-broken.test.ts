import { test } from "node:test"
import assert from "node:assert"

// Scratch commit for issue #2561 AC4 — deliberately failing test to prove
// pr-gate goes red. Removed in a follow-up commit before merge.
test("deliberately broken for issue #2561 AC4 demonstration", () => {
  assert.strictEqual(1, 2)
})
