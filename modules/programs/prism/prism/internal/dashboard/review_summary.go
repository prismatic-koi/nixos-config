package dashboard

// review_summary.go — per-agent review-round status rendering.
//
// This file owns:
//   - the ReviewChildSummary type (one entry per canonical review agent on a
//     virtual review-group row),
//   - the verdict-parsing helper (case-insensitive substring match against the
//     last assistant message, mirroring internal/db/sessions.go::ReviewVerdict),
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
)

// Verdict values produced by ParseVerdict and stored in ReviewChildSummary.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictRunning = "running" // active / waiting / compacting / reviewing
	VerdictPending = "pending" // idle / unknown / not yet started
	VerdictError   = "error"   // errored / startup failure / missing agent
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
// This mirrors the rule used by internal/db/sessions.go::ReviewVerdict
// rollup — no regex, no XML parsing, just a substring check.
func ParseVerdict(lastMessage string) string {
	if lastMessage == "" {
		return ""
	}
	lower := strings.ToLower(lastMessage)
	if strings.Contains(lower, "<verdict>pass</verdict>") {
		return VerdictPass
	}
	if strings.Contains(lower, "<verdict>fail</verdict>") {
		return VerdictFail
	}
	return ""
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

// BuildReviewChildSummaries returns one ReviewChildSummary per canonical
// review agent (the set defined by review.Agents()), sorted alphabetically
// by short label so the rendered labels line is "code … context … goal …
// qa … sec".
//
// `children` is the set of per-agent AgentSessions that belong to the same
// review-round group. It is matched against the canonical agent list by the
// suffix of each child's session name: a child "...~review-1-review-goal"
// matches the canonical agent "review-goal".
//
// Agents that have no matching child (e.g. the round was spawned with --only,
// or an agent failed to start) are rendered as VerdictError so the user can
// see at a glance that the slot is missing — this matches the AC's intent
// ("missing agents as ✕").
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

	out := make([]ReviewChildSummary, len(names))
	for i, full := range names {
		ch, ok := byAgent[full]
		if !ok {
			out[i] = ReviewChildSummary{
				AgentShortName: ShortAgentName(full),
				State:          "",
				Verdict:        VerdictError,
			}
			continue
		}
		out[i] = ReviewChildSummary{
			AgentShortName: ShortAgentName(full),
			State:          ch.AgentState,
			Verdict:        classifyVerdict(ch.AgentState, ch.LastMessage),
		}
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

// letterForVerdict returns the per-agent label letter for a verdict.
func letterForVerdict(v string) string {
	switch v {
	case VerdictPass:
		return "P"
	case VerdictFail:
		return "F"
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
	case VerdictRunning:
		return ColorPrimary
	case VerdictError:
		return ColorRed
	default:
		return ColorSecondary
	}
}

// RenderReviewSummary builds the per-agent labels segment for a collapsed
// review-group row. It returns the ANSI-coloured label fragment and its plain
// rune width.
//
// The width budget is honoured as a single decision:
//
//   - If `budget` >= labels width, the labels fragment is returned non-empty.
//   - Otherwise the labels fragment is suppressed entirely and the caller
//     falls back to rendering session + state only (see #1802).
//
// The caller is responsible for any inter-fragment gap characters between
// the preceding state column and the returned labels string.
func RenderReviewSummary(summaries []ReviewChildSummary, budget int) (labels string, plainWidth int) {
	if len(summaries) == 0 {
		return "", 0
	}
	labelsW := reviewSummaryLabelsWidth(summaries)
	if budget < labelsW {
		return "", 0
	}
	return renderLabels(summaries), labelsW
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
