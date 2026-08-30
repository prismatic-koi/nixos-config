// Package verdict holds the single source of truth for the review-agent
// verdict-marker rule: the case-insensitive substring test for the
// <verdict>…</verdict> tag in a review agent's assistant message.
//
// It is a stdlib-only leaf package on purpose. internal/review already imports
// internal/db, so the rule cannot live in internal/review without an import
// cycle back through internal/db. A leaf package lets all three consumers —
// internal/db, internal/review, and internal/dashboard — share one rule
// instead of separate copies that can drift. The rule must run on the decoded
// message text: in a raw, JSON-escaped payload '<' becomes \u003c and the
// marker never matches.
package verdict

import "strings"

// Kind is the classification of a review agent's assistant message by the
// verdict-marker rule.
type Kind int

const (
	// None means no recognised verdict marker was present.
	None Kind = iota
	// Pass means an explicit <verdict>PASS</verdict> marker was present.
	Pass
	// Fail means an explicit <verdict>FAIL</verdict> marker was present.
	Fail
	// PassWithDisagreement means an explicit
	// <verdict>PASS_WITH_DISAGREEMENT</verdict> marker was present. review-goal
	// issues this after a prior FAIL to pass the round while recording an
	// unresolved concern for the coordinator (agents/review-goal.md).
	PassWithDisagreement
)

// Parse classifies text by a case-insensitive substring test for the
// <verdict>…</verdict> marker. It is the only place this rule lives; each
// caller maps Kind onto its own value space.
//
// PASS_WITH_DISAGREEMENT is checked before PASS. The two do not overlap — the
// substring "<verdict>pass</verdict>" requires "pass" to be followed
// immediately by "</verdict>", which it is not inside
// "<verdict>pass_with_disagreement</verdict>" — but the explicit ordering
// keeps the intent obvious.
func Parse(text string) Kind {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "<verdict>pass_with_disagreement</verdict>"):
		return PassWithDisagreement
	case strings.Contains(lower, "<verdict>pass</verdict>"):
		return Pass
	case strings.Contains(lower, "<verdict>fail</verdict>"):
		return Fail
	default:
		return None
	}
}
