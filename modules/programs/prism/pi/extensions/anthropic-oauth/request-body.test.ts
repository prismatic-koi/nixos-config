// Wire-form tests for buildRequestBody — the thinking-payload selection
// that fixes issue #2044 (opus-4-8 erratic behaviour).
//
// Mirrors the assertions from pi-ai's own smoke + force-adaptive tests:
//   - test/anthropic-opus-4-8-smoke.test.ts
//   - test/anthropic-force-adaptive-thinking.test.ts
//
// Run with: tsx --test request-body.test.ts  (Node 20+, zero new deps)

import { describe, it } from "node:test"
import assert from "node:assert/strict"
import type { Context, Model } from "@earendil-works/pi-ai"
import {
  buildRequestBody,
  mapThinkingLevelToEffort,
} from "./request-body.ts"
import { getModelBetas } from "./betas.ts"

// ---------------------------------------------------------------------------
// Helpers — minimal model + context fixtures.
//
// We do not use the live pi registry here so the tests stay hermetic and do
// not depend on a specific pi-ai version pinning a specific model id.
// ---------------------------------------------------------------------------

function makeAdaptiveModel(
  overrides: Partial<Model<"anthropic-messages">> = {},
): Model<"anthropic-messages"> {
  return {
    id: "claude-opus-4-8",
    name: "Claude Opus 4.8",
    api: "anthropic-messages",
    provider: "anthropic",
    baseUrl: "https://api.anthropic.com",
    reasoning: true,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 32000,
    compat: { forceAdaptiveThinking: true },
    thinkingLevelMap: { xhigh: "xhigh" },
    ...overrides,
  } as Model<"anthropic-messages">
}

function makeLegacyModel(
  overrides: Partial<Model<"anthropic-messages">> = {},
): Model<"anthropic-messages"> {
  // A non-adaptive anthropic-messages model — no forceAdaptiveThinking,
  // no thinkingLevelMap. Represents older Claude models (e.g. opus-4-5,
  // sonnet 3.x) and corporate-proxy custom ids that have not opted into
  // adaptive thinking.
  return {
    id: "claude-sonnet-3-5",
    name: "Claude Sonnet 3.5",
    api: "anthropic-messages",
    provider: "anthropic",
    baseUrl: "https://api.anthropic.com",
    reasoning: true,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: 200000,
    maxTokens: 8192,
    ...overrides,
  } as Model<"anthropic-messages">
}

function makeContext(): Context {
  return {
    systemPrompt: "You are a precise assistant.",
    messages: [
      { role: "user", content: "Hello", timestamp: Date.now() },
    ],
  }
}

// ---------------------------------------------------------------------------
// [functional] Adaptive wire form for forceAdaptiveThinking models
// ---------------------------------------------------------------------------
describe("buildRequestBody — adaptive thinking (forceAdaptiveThinking)", () => {
  it("sends {type:'adaptive', display:'summarized'} + output_config.effort='medium' for reasoning='medium'", () => {
    const body = buildRequestBody(
      makeAdaptiveModel(),
      makeContext(),
      { reasoning: "medium", maxTokens: 1024 },
      false,
    )

    assert.deepEqual(body.thinking, { type: "adaptive", display: "summarized" })
    assert.deepEqual(body.output_config, { effort: "medium" })
  })

  it("maps reasoning='high' to effort='high'", () => {
    const body = buildRequestBody(
      makeAdaptiveModel(),
      makeContext(),
      { reasoning: "high", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "high" })
  })

  it("maps reasoning='minimal' to effort='low'", () => {
    const body = buildRequestBody(
      makeAdaptiveModel(),
      makeContext(),
      { reasoning: "minimal", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "low" })
  })

  it("honours the model's thinkingLevelMap (xhigh → 'xhigh' for opus-4-8)", () => {
    // Issue #2053: the registry ships opus-4-8 with
    // thinkingLevelMap: {"xhigh":"xhigh"}. Before the projection in
    // index.ts::getAnthropicModels propagated thinkingLevelMap, this fell
    // through to the default mapping ("high"). After the fix the resolved
    // effort matches the registry.
    const body = buildRequestBody(
      makeAdaptiveModel(), // thinkingLevelMap: { xhigh: "xhigh" }
      makeContext(),
      { reasoning: "xhigh", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "xhigh" })
  })

  it("honours the model's thinkingLevelMap (xhigh → 'xhigh' for opus-4-7)", () => {
    // Mirrors the opus-4-8 case — the registry ships opus-4-7 with the
    // same {"xhigh":"xhigh"} entry. Explicit assertion per AC in #2053.
    const model = makeAdaptiveModel({
      id: "claude-opus-4-7",
      name: "Claude Opus 4.7",
      thinkingLevelMap: { xhigh: "xhigh" },
    } as Partial<Model<"anthropic-messages">>)
    const body = buildRequestBody(
      model,
      makeContext(),
      { reasoning: "xhigh", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "xhigh" })
  })

  it("honours the model's thinkingLevelMap (xhigh → 'max' for opus-4-6)", () => {
    // Registry ships opus-4-6 with thinkingLevelMap: {"xhigh":"max"} —
    // a distinct value from opus-4-7/4-8, so this assertion also proves the
    // map is being read per-model and not coincidentally matching a default.
    const model = makeAdaptiveModel({
      id: "claude-opus-4-6",
      thinkingLevelMap: { xhigh: "max" },
    } as Partial<Model<"anthropic-messages">>)
    const body = buildRequestBody(
      model,
      makeContext(),
      { reasoning: "xhigh", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "max" })
  })

  it("falls through to default mapping when thinkingLevelMap is absent (no regression)", () => {
    // Adaptive model with no thinkingLevelMap (hypothetical / legacy /
    // corporate-proxy registry entry that opts into adaptive thinking but
    // omits the map). Without thinkingLevelMap, mapThinkingLevelToEffort
    // falls back to its default switch — xhigh hits the `default` arm and
    // resolves to "high". This is the pre-fix behaviour for opus-4-6/4-7/4-8
    // (#2053) and the contract we preserve for models that never had a map.
    const model = makeAdaptiveModel({
      thinkingLevelMap: undefined,
    } as Partial<Model<"anthropic-messages">>)
    const body = buildRequestBody(
      model,
      makeContext(),
      { reasoning: "xhigh", maxTokens: 1024 },
      false,
    )
    assert.deepEqual(body.output_config, { effort: "high" })
  })

  it("does NOT set body.temperature when thinking is enabled (pi-ai parity)", () => {
    const body = buildRequestBody(
      makeAdaptiveModel(),
      makeContext(),
      { reasoning: "medium", maxTokens: 1024 },
      false,
    )
    assert.ok(!("temperature" in body), "temperature must not be set")
  })

  it("does NOT set body.thinking when reasoning is absent", () => {
    const body = buildRequestBody(
      makeAdaptiveModel(),
      makeContext(),
      { maxTokens: 1024 },
      false,
    )
    assert.equal(body.thinking, undefined)
    assert.equal(body.output_config, undefined)
  })
})

// ---------------------------------------------------------------------------
// [edge-case] Legacy wire form for non-adaptive models — no regression
// ---------------------------------------------------------------------------
describe("buildRequestBody — legacy budget thinking (no forceAdaptiveThinking)", () => {
  it("sends {type:'enabled', budget_tokens:N, display:'summarized'} for non-adaptive models", () => {
    const body = buildRequestBody(
      makeLegacyModel(),
      makeContext(),
      { reasoning: "medium", maxTokens: 8192 },
      false,
    )

    assert.ok(body.thinking, "body.thinking should be set")
    const thinking = body.thinking as {
      type: string
      budget_tokens: number
      display: string
    }
    assert.equal(thinking.type, "enabled")
    assert.equal(thinking.display, "summarized")
    assert.equal(typeof thinking.budget_tokens, "number")
    assert.ok(thinking.budget_tokens > 0)
    // Default budget for "medium" is 10240, clamped to maxTokens-1=8191.
    assert.equal(thinking.budget_tokens, 8191)
    assert.equal(body.output_config, undefined)
  })

  it("honours custom thinkingBudgets when provided", () => {
    const body = buildRequestBody(
      makeLegacyModel(),
      makeContext(),
      {
        reasoning: "low",
        maxTokens: 8192,
        thinkingBudgets: { low: 2000 },
      },
      false,
    )
    const thinking = body.thinking as { budget_tokens: number }
    assert.equal(thinking.budget_tokens, 2000)
  })

  it("treats compat.forceAdaptiveThinking=false as opt-out (legacy form)", () => {
    // Mirrors pi-ai's `anthropic-force-adaptive-thinking.test.ts`:
    //   "allows built-in adaptive models to opt out with
    //    compat.forceAdaptiveThinking false".
    const model = makeAdaptiveModel({
      compat: { forceAdaptiveThinking: false },
    })
    const body = buildRequestBody(
      model,
      makeContext(),
      { reasoning: "medium", maxTokens: 8192 },
      false,
    )
    const thinking = body.thinking as { type: string }
    assert.equal(thinking.type, "enabled")
    assert.equal(body.output_config, undefined)
  })

  it("does NOT set body.temperature for legacy thinking either", () => {
    const body = buildRequestBody(
      makeLegacyModel(),
      makeContext(),
      { reasoning: "medium", maxTokens: 8192 },
      false,
    )
    assert.ok(!("temperature" in body), "temperature must not be set")
  })
})

// ---------------------------------------------------------------------------
// [functional] mapThinkingLevelToEffort unit checks (mirrors pi-ai's helper)
// ---------------------------------------------------------------------------
describe("mapThinkingLevelToEffort", () => {
  it("returns 'low' for minimal and low", () => {
    const m = makeAdaptiveModel({
      thinkingLevelMap: undefined,
    } as Partial<Model<"anthropic-messages">>)
    assert.equal(mapThinkingLevelToEffort(m, "minimal"), "low")
    assert.equal(mapThinkingLevelToEffort(m, "low"), "low")
  })

  it("returns 'medium' for medium", () => {
    const m = makeAdaptiveModel({
      thinkingLevelMap: undefined,
    } as Partial<Model<"anthropic-messages">>)
    assert.equal(mapThinkingLevelToEffort(m, "medium"), "medium")
  })

  it("returns 'high' for high and as the default", () => {
    const m = makeAdaptiveModel({
      thinkingLevelMap: undefined,
    } as Partial<Model<"anthropic-messages">>)
    assert.equal(mapThinkingLevelToEffort(m, "high"), "high")
    assert.equal(mapThinkingLevelToEffort(m, undefined), "high")
  })

  it("prefers the model's thinkingLevelMap over the default mapping", () => {
    const m = makeAdaptiveModel({
      thinkingLevelMap: { high: "max" },
    } as Partial<Model<"anthropic-messages">>)
    assert.equal(mapThinkingLevelToEffort(m, "high"), "max")
  })
})

// ---------------------------------------------------------------------------
// [functional] getModelBetas — interleaved-thinking suppression for adaptive
// ---------------------------------------------------------------------------
describe("getModelBetas — interleaved-thinking suppression for adaptive models", () => {
  it("includes interleaved-thinking-2025-05-14 for non-adaptive models (no regression)", () => {
    const betas = getModelBetas("claude-sonnet-3-5", undefined, {
      forceAdaptiveThinking: false,
    })
    assert.ok(
      betas.includes("interleaved-thinking-2025-05-14"),
      "legacy models should still receive the interleaved-thinking beta",
    )
  })

  it("includes interleaved-thinking-2025-05-14 when no ctx is passed (backwards-compatible default)", () => {
    const betas = getModelBetas("claude-sonnet-3-5")
    assert.ok(
      betas.includes("interleaved-thinking-2025-05-14"),
      "backwards-compatible default should still include the beta",
    )
  })

  it("suppresses interleaved-thinking-2025-05-14 for forceAdaptiveThinking models (pi-ai parity)", () => {
    const betas = getModelBetas("claude-opus-4-8", undefined, {
      forceAdaptiveThinking: true,
    })
    assert.ok(
      !betas.includes("interleaved-thinking-2025-05-14"),
      "adaptive models have interleaved thinking built in; the beta header is redundant — pi-ai anthropic.ts ~789",
    )
  })

  it("suppresses interleaved-thinking-2025-05-14 for claude-fable-5-1 (adaptive)", () => {
    // Fable 5.1 is declared with compat.forceAdaptiveThinking in the
    // nix-managed ~/.pi/agent/models.json, so it takes the same path as the
    // opus adaptive models. The suppression is keyed off the compat flag at
    // call time, not a substring, so a new model needs no model-config edit.
    const betas = getModelBetas("claude-fable-5-1", undefined, {
      forceAdaptiveThinking: true,
    })
    assert.ok(
      !betas.includes("interleaved-thinking-2025-05-14"),
      "claude-fable-5-1 is adaptive; the interleaved-thinking beta is redundant",
    )
  })

  it("effort-2025-11-24 rides on per-model adds, not baseBetas (2.1.257)", () => {
    // Claude CLI 2.1.257 moved effort-2025-11-24 out of baseBetas into
    // per-model adds for opus-4-5 / 4-6 / 4-7. Models with no override entry
    // — including claude-opus-4-8 and claude-fable-5-1 — no longer send it.
    // This is upstream's shape, mirrored deliberately; see UPSTREAM.md #15.
    assert.ok(
      getModelBetas("claude-opus-4-7", undefined, {
        forceAdaptiveThinking: true,
      }).includes("effort-2025-11-24"),
      "opus-4-7 must include effort-2025-11-24 via its per-model add",
    )
    for (const model of [
      "claude-opus-4-8",
      "claude-sonnet-4-5",
      "claude-fable-5-1",
    ]) {
      assert.ok(
        !getModelBetas(model).includes("effort-2025-11-24"),
        `${model} has no override entry and must not send effort-2025-11-24`,
      )
    }
  })
})
