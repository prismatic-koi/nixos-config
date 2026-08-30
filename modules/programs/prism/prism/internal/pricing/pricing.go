// Package pricing holds the shared per-million-token pricing table for known
// models, along with the unknown-model fallback logic used when computing
// cost from token counts.
//
// It is the single source of pricing for all consumers, so that cost is
// computed identically everywhere instead of from separate copies that can
// quietly diverge.
package pricing

// ModelCost is per-million-token pricing for a single model. Cost is in USD.
type ModelCost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// ModelCosts contains per-million-token pricing for known models.
// Keys are "providerID/modelID" exactly as stored in payloads — these must
// match the model IDs emitted by the pi plugin verbatim.
// Add new entries when new models are configured.
var ModelCosts = map[string]ModelCost{
	// Anthropic direct models (hyphens as version separators).
	"anthropic/claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
	"anthropic/claude-opus-4-6":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
	"anthropic/claude-haiku-4-5":  {Input: 0.80, Output: 4.0, CacheRead: 0.08, CacheWrite: 1.00},
	// GitHub Copilot models (dots as version separators — different from Anthropic direct).
	"github-copilot/claude-sonnet-4.6": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
	"github-copilot/claude-opus-4.6":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
	"github-copilot/claude-haiku-4.5":  {Input: 0.80, Output: 4.0, CacheRead: 0.08, CacheWrite: 1.00},
	// Google Gemini models.
	"google/gemini-3-flash-preview":        {Input: 0.15, Output: 0.60, CacheRead: 0.0375, CacheWrite: 0},
	"google/gemini-3.1-flash-lite-preview": {Input: 0.075, Output: 0.30, CacheRead: 0.01875, CacheWrite: 0},
}

// Lookup returns the pricing entry for the given "providerID/modelID" key,
// and whether it was found. Callers use ok=false to trigger the
// event-reported-cost fallback for unknown models (e.g. openrouter/*).
func Lookup(key string) (ModelCost, bool) {
	c, ok := ModelCosts[key]
	return c, ok
}

// Cost computes the USD cost for the given token counts against the known
// pricing table entry for key. When key is not present in the pricing
// table, it falls back to eventCost — the cost reported directly in the
// event payload — for unknown models (e.g. openrouter/*).
func Cost(key string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens float64, eventCost float64) float64 {
	c, ok := ModelCosts[key]
	if !ok {
		return eventCost
	}
	return (inputTokens*c.Input +
		outputTokens*c.Output +
		cacheReadTokens*c.CacheRead +
		cacheWriteTokens*c.CacheWrite) / 1_000_000
}
