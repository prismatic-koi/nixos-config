// Mirror of griffinmartin/opencode-claude-auth src/betas.ts (v2.2.0).
// Source: https://github.com/griffinmartin/opencode-claude-auth
//
// pi divergence preserved here: the `ctx.forceAdaptiveThinking` block in
// `getModelBetas` suppresses the `interleaved-thinking-2025-05-14` beta for
// adaptive-thinking models (issue #2044). See UPSTREAM.md divergence #10.

import { config, getModelOverride } from "./model-config.ts"

// Beta flags to try removing in order when "long context" errors occur.
//
// Retained per griffinmartin v2.0.0, unchanged in v2.2.0: while the plugin no
// longer sends `context-1m-2025-08-07` by default, a user can still opt in
// manually via `ANTHROPIC_BETA_FLAGS=...,context-1m-2025-08-07,...`; if that 429s
// with a long-context error we peel these betas off one at a time.
export const LONG_CONTEXT_BETAS = config.longContextBetas

function getRequiredBetas(): string[] {
  return (process.env.ANTHROPIC_BETA_FLAGS ?? config.baseBetas.join(","))
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
}

// Session-level cache of excluded beta flags per model (resets on process restart)
const excludedBetas: Map<string, Set<string>> = new Map()

// Track the last-seen beta flags env var and model to detect changes
let lastBetaFlagsEnv: string | undefined = process.env.ANTHROPIC_BETA_FLAGS
let lastModelId: string | undefined

export function getExcludedBetas(modelId: string): Set<string> {
  // Reset exclusions if user changed ANTHROPIC_BETA_FLAGS
  const currentBetaFlags = process.env.ANTHROPIC_BETA_FLAGS
  if (currentBetaFlags !== lastBetaFlagsEnv) {
    excludedBetas.clear()
    lastBetaFlagsEnv = currentBetaFlags
  }

  // Reset exclusions if user switched models (new model may support different betas)
  if (lastModelId !== undefined && lastModelId !== modelId) {
    excludedBetas.clear()
  }
  lastModelId = modelId

  return excludedBetas.get(modelId) ?? new Set()
}

export function addExcludedBeta(modelId: string, beta: string): void {
  const existing = excludedBetas.get(modelId) ?? new Set()
  existing.add(beta)
  excludedBetas.set(modelId, existing)
}

export function resetExcludedBetas(): void {
  excludedBetas.clear()
  lastModelId = undefined
}

export function isLongContextError(responseBody: string): boolean {
  return (
    responseBody.includes(
      "Extra usage is required for long context requests",
    ) ||
    responseBody.includes("long context beta is not yet available") ||
    responseBody.includes("You're out of extra usage")
  )
}

export function getNextBetaToExclude(modelId: string): string | null {
  const excluded = getExcludedBetas(modelId)
  for (const beta of LONG_CONTEXT_BETAS) {
    if (!excluded.has(beta)) {
      return beta
    }
  }
  return null // All long-context betas already excluded
}

export interface GetModelBetasContext {
  /**
   * Whether the model uses adaptive thinking
   * (`compat.forceAdaptiveThinking === true`). When true,
   * `interleaved-thinking-2025-05-14` is suppressed because adaptive thinking
   * models have interleaved thinking built in — mirrors pi-ai's
   * `anthropic.ts::createClient` (~line 789):
   *   "Adaptive thinking models have interleaved thinking built in,
   *    so skip the beta header."
   */
  forceAdaptiveThinking?: boolean
}

export function getModelBetas(
  modelId: string,
  excluded?: Set<string>,
  ctx?: GetModelBetasContext,
): string[] {
  let betas = [...getRequiredBetas()]

  // The legacy context-1m-2025-08-07 beta is never sent — the API supports
  // 1M context natively without it. (griffinmartin v2.0.0.)

  // Apply per-model overrides (e.g. haiku excludes effort-2025-11-24)
  const override = getModelOverride(modelId)
  if (override) {
    const { exclude, add } = override
    if (exclude) {
      // Remove every occurrence — regenerated configs can contain duplicates,
      // and so can a user's `ANTHROPIC_BETA_FLAGS`. indexOf/splice would strip
      // only the first and leave the beta on the wire.
      betas = betas.filter((beta) => !exclude.includes(beta))
    }
    if (add) {
      for (const beta of add) {
        if (!betas.includes(beta)) betas.push(beta)
      }
    }
  }

  // pi divergence #10 (issue #2044): adaptive-thinking models have
  // interleaved thinking built in — drop the beta header. Mirrors pi-ai's
  // `anthropic.ts::createClient` ~789. Uses `filter` (not indexOf/splice)
  // so every occurrence goes, including a duplicate a user supplies via
  // `ANTHROPIC_BETA_FLAGS`.
  if (ctx?.forceAdaptiveThinking) {
    betas = betas.filter((beta) => beta !== "interleaved-thinking-2025-05-14")
  }

  // Filter out excluded betas (from previous failed requests due to long context errors)
  if (excluded && excluded.size > 0) {
    return betas.filter((beta) => !excluded.has(beta))
  }

  return betas
}
