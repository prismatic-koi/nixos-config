package dashboard

// review_summary.go — per-agent review-round status rendering.
//
// This file owns:
//   - the ReviewChildSummary type (one entry per canonical review agent on a
//     virtual review-group row),
//   - the verdict-parsing helper, which maps the shared verdict.Kind (the one
//     marker rule, in internal/verdict) onto the dashboard's Verdict* values,
//   - the canonical short-name mapping (derived from review.Agents()), and
//   - the single rendering helper RenderReviewSummary that produces the
//     per-agent verdict labels shown on the collapsed review-group row.
//
// The canonical five-agent set is read from review.Agents() in
// internal/review/agents.go — never duplicated here. The display order of
// the rendered labels is alphabetical by short label (see #1802); the
// canonical agent list is unchanged.

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/verdict"
)

// Verdict values produced by ParseVerdict and stored in ReviewChildSummary.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictRunning = "running" // active / waiting / compacting / reviewing
	VerdictPending = "pending" // idle / unknown / not yet started
	VerdictError   = "error"   // errored / startup failure / missing agent
	// VerdictPassWithDisagreement is review-goal's PASS_WITH_DISAGREEMENT
	// marker: the round passes but the reviewer records an unresolved concern
	// for the coordinator (agents/review-goal.md). Rendered distinctly from a
	// plain pass and from pending (#2862).
	VerdictPassWithDisagreement = "pass_with_disagreement"
)

// ReviewChildSummary holds the per-agent rollup used to render one cell of
// the collapsed review-group row's per-agent verdict labels. One entry per
// canonical review agent (the set comes from review.Agents()); the display
// order is alphabetical by AgentShortName.
type ReviewChildSummary struct {
	// AgentShortName is the canonical short label (e.g. "goal", "code",
	// "sec" — see ShortAgentName). It is rendered in the per-agent
	// verdict labels, e.g. "goal:P".
	AgentShortName string
	// State is the AgentState string ("active" | "waiting" | "finished" |
	// "compacting" | "error" | "idle" | ""). Used to decide running vs
	// pending vs error when no verdict marker is present.
	State string
	// Verdict is one of VerdictPass | VerdictFail | VerdictRunning |
	// VerdictPending | VerdictError. Derived from State + the last
	// assistant message.
	Verdict string
}

// canonicalReviewAgentNames returns the canonical list of review-agent full
// names (e.g. "review-goal", "review-code", ...) in the order defined by
// internal/review.Agents(). Centralised here so the dashboard never duplicates
// the agent list.
func canonicalReviewAgentNames() []string {
	agents := review.Agents()
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = a.Name
	}
	return out
}

// ShortAgentName maps a full review-agent name (e.g. "review-security") to its
// short display label (e.g. "sec"). Falls back to the part after the first "-"
// for any unknown name, and to the full name if there is no "-".
//
// The short labels are tuned for column-budget reasons: most agent names are
// short enough to use as-is, but "review-security" is collapsed to "sec" so
// the per-agent verdict labels line fits within the dashboard width budget at
// the common terminal sizes.
func ShortAgentName(fullName string) string {
	// Strip the "review-" prefix when present.
	stripped := strings.TrimPrefix(fullName, "review-")
	switch stripped {
	case "security":
		return "sec"
	default:
		return stripped
	}
}

// ParseVerdict returns VerdictPass when the last assistant message contains a
// case-insensitive `<verdict>pass</verdict>` marker, VerdictFail when it
// contains `<verdict>fail</verdict>`, and "" otherwise.
//
// The marker rule itself lives in internal/verdict — the single stdlib-only
// leaf shared with internal/db and internal/review (#2862). This function only
// maps verdict.Kind onto the dashboard's Verdict* value space.
func ParseVerdict(lastMessage string) string {
	switch verdict.Parse(lastMessage) {
	case verdict.Pass:
		return VerdictPass
	case verdict.Fail:
		return VerdictFail
	case verdict.PassWithDisagreement:
		return VerdictPassWithDisagreement
	default:
		return ""
	}
}

// classifyVerdict returns the final Verdict value for a child given its
// AgentState and the result of ParseVerdict on its LastMessage. The rules:
//
//   - error state                              → VerdictError
//   - parsed verdict pass                      → VerdictPass
//   - parsed verdict fail                      → VerdictFail
//   - active / waiting / compacting / reviewing → VerdictRunning
//   - anything else (idle, "", finished w/o verdict) → VerdictPending
//
// Note: a child whose AgentState is "finished" but whose last assistant
// message has no <verdict> marker is treated as VerdictPending — the rollup
// only commits to PASS/FAIL when the marker is present.
func classifyVerdict(state, lastMessage string) string {
	if state == "error" {
		return VerdictError
	}
	if v := ParseVerdict(lastMessage); v != "" {
		return v
	}
	switch state {
	case "active", "waiting", "compacting", "reviewing":
		return VerdictRunning
	default:
		return VerdictPending
	}
}

// BuildReviewChildSummaries returns one ReviewChildSummary per child in
// `children` that matches a canonical review-agent name (via
// trailingReviewAgent), sorted alphabetically by short label.
//
// `children` is the set of per-agent AgentSessions that belong to the same
// review-round group. It is matched against the canonical agent list by the
// suffix of each child's session name: a child "...~review-1-review-goal"
// matches the canonical agent "review-goal".
//
// Canonical agents that have no matching child in `children` (e.g. the round
// was spawned with --only, or an agent was never started) are omitted from
// the returned slice — they produce no entry and no ✕ placeholder. Only
// children that were actually spawned appear in the output. A child whose
// AgentState is "error" (spawned but errored) still produces a
// ReviewChildSummary with Verdict == VerdictError (✕) — the errored-but-
// spawned path is unchanged.
//
// When `children` contains no canonical review agents, the returned slice is
// empty (length 0) and RenderReviewSummary returns ("", 0, summaryNone).
func BuildReviewChildSummaries(children []AgentSession) []ReviewChildSummary {
	names := canonicalReviewAgentNames()

	// Index children by the trailing review-agent name (e.g. "review-goal").
	byAgent := make(map[string]AgentSession, len(children))
	for _, ch := range children {
		// Find the trailing "-review-<name>" component.
		// The full session name is e.g. "repo@branch~review-N-review-goal".
		// We extract the substring after the last "~review-N-" boundary by
		// looking for "review-" anchored after a "-".
		// Simplest: split on "-" and look for "review-<rest>" suffix.
		if name := trailingReviewAgent(ch.Name); name != "" {
			byAgent[name] = ch
		}
	}

	out := make([]ReviewChildSummary, 0, len(names))
	for _, full := range names {
		ch, ok := byAgent[full]
		if !ok {
			continue
		}
		out = append(out, ReviewChildSummary{
			AgentShortName: ShortAgentName(full),
			State:          ch.AgentState,
			Verdict:        classifyVerdict(ch.AgentState, ch.LastMessage),
		})
	}

	// Sort alphabetically by short label for display. The canonical agent
	// set is unchanged (still review.Agents()); only the display order is
	// derived. See AC #1802.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AgentShortName < out[j].AgentShortName
	})
	return out
}

// trailingReviewAgent extracts the canonical review-agent name suffix from a
// per-agent session name. Given "repo@branch~review-3-review-security" it
// returns "review-security". Returns "" when the name has no such suffix.
func trailingReviewAgent(sessionName string) string {
	// ReviewRoundKey returns "" for non-per-agent sessions; we trust it as the
	// gate. The agent suffix is everything after the round key + "-".
	rk := ReviewRoundKey(sessionName)
	if rk == "" {
		return ""
	}
	// rk is e.g. "repo@branch~review-3"; the suffix is sessionName[len(rk)+1:].
	if len(sessionName) <= len(rk)+1 {
		return ""
	}
	return sessionName[len(rk)+1:]
}

// ── width budget ─────────────────────────────────────────────────────────────

// summaryMode is the rendering tier chosen by RenderReviewSummary for the
// trailing per-agent verdict segment on a collapsed review-group row. The
// caller (RenderReviewGroupRow) and the plain-text mirror used inside the
// selected-row bar must agree on the mode so their width footprints stay
// in sync. See #1812.
type summaryMode int

const (
	// summaryNone suppresses the trailing segment entirely — there isn't
	// enough horizontal budget even for the compact letter-only form.
	summaryNone summaryMode = iota
	// summaryCompact renders just the verdict letters separated by two
	// spaces, in alphabetical short-label order (e.g. "P  ·  P  ◌  F").
	summaryCompact
	// summaryFull renders the alphabetical labels form
	// ("code:P  context:·  goal:P  qa:◌  sec:F").
	summaryFull
)

// reviewSummaryLabelsWidth returns the rune width of the per-agent verdict
// labels segment for the given summaries (e.g.
// "code:P  context:·  goal:P  qa:◌  sec:F"). Used by RenderReviewSummary to
// decide whether the labels fit in the remaining width budget.
func reviewSummaryLabelsWidth(summaries []ReviewChildSummary) int {
	if len(summaries) == 0 {
		return 0
	}
	w := 0
	for i, s := range summaries {
		if i > 0 {
			w += 2 // two-space separator between labels
		}
		w += utf8.RuneCountInString(s.AgentShortName) + 1 + 1 // "name" + ":" + letter
	}
	return w
}

// reviewSummaryCompactWidth returns the rune width of the compact letter-only
// segment for the given summaries (e.g. "P  ·  P  ◌  F"). Each verdict
// letter is one rune wide (P / F / ◌ / · / ✕) and adjacent letters are
// separated by two spaces, matching the wide form's separator. Peer of
// reviewSummaryLabelsWidth. See #1812.
func reviewSummaryCompactWidth(summaries []ReviewChildSummary) int {
	n := len(summaries)
	if n == 0 {
		return 0
	}
	// n single-rune letters + (n-1) two-space separators.
	return n + 2*(n-1)
}

// reviewChildVerdictLabel returns the full-word verdict label shown in the
// title column of an expanded per-agent review child row (#2862). Unlike the
// single-letter form on the collapsed group row, this is spelled out because
// the title column has the width for it and the row has no other verdict cue.
func reviewChildVerdictLabel(v string) string {
	switch v {
	case VerdictPass:
		return "PASS"
	case VerdictFail:
		return "FAIL"
	case VerdictPassWithDisagreement:
		return "PASS (disagreement)"
	case VerdictRunning:
		return "running"
	case VerdictError:
		return "error"
	default:
		return "pending"
	}
}

// letterForVerdict returns the per-agent label letter for a verdict.
func letterForVerdict(v string) string {
	switch v {
	case VerdictPass:
		return "P"
	case VerdictFail:
		return "F"
	case VerdictPassWithDisagreement:
		return "D"
	case VerdictRunning:
		return "◌"
	case VerdictError:
		return "✕"
	default:
		return "·"
	}
}

// colorForVerdict returns the lipgloss colour name for a verdict, used for
// the per-agent label letters.
func colorForVerdict(v string) string {
	switch v {
	case VerdictPass:
		return ColorGreen
	case VerdictFail:
		return ColorRed
	case VerdictPassWithDisagreement:
		return ColorYellow
	case VerdictRunning:
		return ColorPrimary
	case VerdictError:
		return ColorRed
	default:
		return ColorSecondary
	}
}

// RenderReviewSummary builds the per-agent trailing segment for a collapsed
// review-group row. It returns the ANSI-coloured fragment, its plain rune
// width, and the rendering mode chosen for the given budget so the caller
// can keep its selected-row mirror in sync.
//
// Width-budget tiers (see #1812):
//
//   - `budget >= labelsW` → summaryFull: the alphabetical labels form
//     ("code:P  context:·  goal:P  qa:◌  sec:F").
//   - `labelsW > budget >= compactW` → summaryCompact: letter-only form
//     in alphabetical short-label order ("P  ·  P  ◌  F").
//   - `budget < compactW` → summaryNone: the trailing segment is suppressed
//     entirely and the caller falls back to session + state only.
//
// The caller is responsible for any inter-fragment gap characters between
// the preceding state column and the returned fragment.
func RenderReviewSummary(summaries []ReviewChildSummary, budget int) (rendered string, plainWidth int, mode summaryMode) {
	if len(summaries) == 0 {
		return "", 0, summaryNone
	}
	labelsW := reviewSummaryLabelsWidth(summaries)
	if budget >= labelsW {
		return renderLabels(summaries), labelsW, summaryFull
	}
	compactW := reviewSummaryCompactWidth(summaries)
	if budget >= compactW {
		return renderCompact(summaries), compactW, summaryCompact
	}
	return "", 0, summaryNone
}

// renderLabels renders the per-agent verdict labels segment as a single
// ANSI-coloured string. Format: "code:P  context:·  goal:P  qa:◌  sec:F" —
// the agent short name in dim, ":" in dim, the verdict letter in the verdict
// colour.
func renderLabels(summaries []ReviewChildSummary) string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	var b strings.Builder
	for i, s := range summaries {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(styleDim.Render(s.AgentShortName + ":"))
		letter := lipgloss.NewStyle().Foreground(lipgloss.Color(colorForVerdict(s.Verdict)))
		b.WriteString(letter.Render(letterForVerdict(s.Verdict)))
	}
	return b.String()
}

// renderCompact renders the letter-only fallback form as a single
// ANSI-coloured string. Format: "P  ·  P  ◌  F" — each verdict letter is
// rendered in its colorForVerdict colour (matching the wide form's palette),
// with two-space separators between letters. The alphabetical short-label
// order of `summaries` (produced by BuildReviewChildSummaries) is preserved.
// See #1812.
func renderCompact(summaries []ReviewChildSummary) string {
	var b strings.Builder
	for i, s := range summaries {
		if i > 0 {
			b.WriteString("  ")
		}
		letter := lipgloss.NewStyle().Foreground(lipgloss.Color(colorForVerdict(s.Verdict)))
		b.WriteString(letter.Render(letterForVerdict(s.Verdict)))
	}
	return b.String()
}
