// Package nav implements directional session switching for `prism nav`.
//
// The package is intentionally split into pure resolver functions (no tmux,
// no DB) and thin wrappers that perform IO. The pure resolvers take a slice
// of dashboard.AgentSession (the same data structure the dashboard renders)
// plus the current session name and a direction, and return the target
// session name to switch to (or the empty string when the call is a no-op).
//
// This split keeps the navigation logic unit-testable without spinning up a
// tmux server or seeding a DB.
package nav

import (
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/review"
)

// Direction is one of the four cardinal directions.
type Direction string

const (
	DirUp    Direction = "up"
	DirDown  Direction = "down"
	DirLeft  Direction = "left"
	DirRight Direction = "right"
)

// ParseDirection parses the textual direction argument used on the command
// line. It returns an error for any unknown value.
func ParseDirection(s string) (Direction, error) {
	switch Direction(s) {
	case DirUp, DirDown, DirLeft, DirRight:
		return Direction(s), nil
	default:
		return "", fmt.Errorf("unknown direction %q: must be one of up, down, left, right", s)
	}
}

// IsSpineRow reports whether the session is a real switchable row in the
// dashboard's vertical spine. The spine includes top-level rows AND their
// depth-1 children (e.g. `nixos-config@dashboard-slim`). It excludes:
//
//   - virtual review-group rows (IsReviewGroup == true), and
//   - depth-2 review-agent children (matching dashboard.IsDepth2Session).
//
// This is broader than the original `IsTopLevel` predicate (which only
// admitted plain or `@main` names) and matches the actual spine the
// dashboard renders — see issue #1800.
func IsSpineRow(s dashboard.AgentSession) bool {
	if s.IsReviewGroup {
		return false
	}
	if dashboard.IsDepth2Session(s.Name) {
		return false
	}
	return true
}

// terminalStates is the set of agent states that exclude a session from the
// navigable vertical spine, per the issue spec. Sessions in any of these
// states are skipped even if `ended_at IS NULL` has not yet been written by
// the lifecycle layer.
var terminalStates = map[string]bool{
	"finished":    true,
	"deleted":     true,
	"interrupted": true,
}

// IsNavigableSpine reports whether s should be included in the up/down
// navigable spine. The session must be a spine row (per IsSpineRow), not in
// a terminal state, and must have a live tmux session (the latter checked
// by the caller via the liveCheck callback because tmux IO is impure).
func IsNavigableSpine(s dashboard.AgentSession, liveCheck func(string) bool) bool {
	if !IsSpineRow(s) {
		return false
	}
	if terminalStates[s.AgentState] {
		return false
	}
	if liveCheck != nil && !liveCheck(s.Name) {
		return false
	}
	return true
}

// VerticalTargets returns the ordered list of spine session names that
// participate in the up/down navigation, given a slice of all dashboard
// sessions (already sorted by dashboard.SortDisplayed) and a tmux liveness
// check. Both top-level rows and their depth-1 children are included; the
// dashboard ordering — `@main` first within each repo, then other branches
// alphabetically; repos ordered alphabetically — is preserved.
func VerticalTargets(sessions []dashboard.AgentSession, liveCheck func(string) bool) []string {
	var out []string
	for _, s := range sessions {
		if IsNavigableSpine(s, liveCheck) {
			out = append(out, s.Name)
		}
	}
	return out
}

// ResolveVertical returns the target session name for an up/down navigation
// from current within the given ordered list of top-level session names. The
// cycle wraps at both ends.
//
// Returns ("", false) when the list has fewer than two entries or when
// current is not in the list (in which case the navigation is a no-op).
func ResolveVertical(current string, dir Direction, targets []string) (string, bool) {
	if len(targets) < 2 {
		return "", false
	}
	idx := -1
	for i, n := range targets {
		if n == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false
	}
	switch dir {
	case DirUp:
		if idx == 0 {
			return targets[len(targets)-1], true
		}
		return targets[idx-1], true
	case DirDown:
		if idx == len(targets)-1 {
			return targets[0], true
		}
		return targets[idx+1], true
	default:
		return "", false
	}
}

// ReviewCycle holds the parent + agent-name ordering used for left/right
// navigation within a review round.
type ReviewCycle struct {
	// Parent is the parent session name (e.g. "nixos-config@feature").
	Parent string
	// AgentNames is the canonical ordering of review-agent names within a
	// round (e.g. "review-goal", "review-code", ...).
	AgentNames []string
	// Round is the review-round integer (e.g. 1).
	Round int
}

// CanonicalAgentNames returns the canonical ordered list of review-agent
// names. It is derived from internal/review.Agents() so it is never
// duplicated.
func CanonicalAgentNames() []string {
	agents := review.Agents()
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = a.Name
	}
	return out
}

// childName constructs the depth-2 session name for a given parent, round,
// and agent. Example: ("nixos-config@feature", 1, "review-goal") →
// "nixos-config@feature~review-1-review-goal".
func childName(parent string, round int, agent string) string {
	return fmt.Sprintf("%s~review-%d-%s", parent, round, agent)
}

// cycleSessionNames returns the full ordered cycle of session names for a
// review round, starting with the parent:
//
//	[parent, parent~review-N-<agent0>, ..., parent~review-N-<agentK>]
func cycleSessionNames(c ReviewCycle) []string {
	out := make([]string, 0, len(c.AgentNames)+1)
	out = append(out, c.Parent)
	for _, a := range c.AgentNames {
		out = append(out, childName(c.Parent, c.Round, a))
	}
	return out
}

// ResolveReviewContext determines which review cycle (parent + round) applies
// to the given current session, given the full slice of dashboard sessions.
//
// Three cases:
//
//  1. current is a depth-2 review agent (matches "<parent>~review-N-<agent>"):
//     the cycle is anchored on that agent's parent and round.
//
//  2. current is the parent of an active review round (it has at least one
//     live depth-2 review child): the cycle is anchored on the lowest-
//     numbered active round.
//
//  3. Otherwise: no review cycle applies and (zero ReviewCycle, false) is
//     returned. left/right become no-ops.
//
// "Live" here is determined by the liveCheck callback (so the resolver stays
// pure with respect to tmux IO).
func ResolveReviewContext(current string, sessions []dashboard.AgentSession, liveCheck func(string) bool) (ReviewCycle, bool) {
	agentNames := CanonicalAgentNames()

	// Case 1: current is itself a depth-2 review agent.
	if parent, round, ok := parseDepth2Agent(current); ok {
		return ReviewCycle{
			Parent:     parent,
			AgentNames: agentNames,
			Round:      round,
		}, true
	}

	// Case 2: current is the parent of an active round. Walk all sessions,
	// pick out depth-2 review children whose parsed parent matches current,
	// then choose the lowest round number that has any live child.
	live := func(name string) bool {
		if liveCheck == nil {
			return true
		}
		return liveCheck(name)
	}

	roundLive := map[int]bool{}
	for _, s := range sessions {
		if s.IsReviewGroup {
			continue
		}
		p, n, ok := parseDepth2Agent(s.Name)
		if !ok {
			continue
		}
		if p != current {
			continue
		}
		if live(s.Name) {
			roundLive[n] = true
		}
	}
	if len(roundLive) == 0 {
		return ReviewCycle{}, false
	}
	// Choose the lowest live round.
	lowest := -1
	for n := range roundLive {
		if lowest < 0 || n < lowest {
			lowest = n
		}
	}
	return ReviewCycle{
		Parent:     current,
		AgentNames: agentNames,
		Round:      lowest,
	}, true
}

// parseDepth2Agent parses a depth-2 review-agent session name into its parent
// session, round number, and agent name. Returns ok=false if the name does
// not match the per-agent pattern.
//
// Reuses dashboard.ReviewRoundKey for the parsing — it already validates the
// "~review-N-<agent>" shape and returns "<parent>~review-N".
func parseDepth2Agent(name string) (parent string, round int, ok bool) {
	rk := dashboard.ReviewRoundKey(name)
	if rk == "" {
		return "", 0, false
	}
	// rk is "<parent>~review-N". Split on the last "~review-".
	const sep = "~review-"
	idx := strings.LastIndex(rk, sep)
	if idx < 0 {
		return "", 0, false
	}
	parent = rk[:idx]
	nStr := rk[idx+len(sep):]
	n := 0
	for _, r := range nStr {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return "", 0, false
	}
	return parent, n, true
}

// ResolveLateral returns the target session name for a left/right navigation
// from current. cycle is the ReviewCycle returned by ResolveReviewContext.
// liveCheck filters out cycle entries whose tmux session is not live; the
// cycle contracts to only live entries plus the parent (the parent is always
// kept in the cycle when its tmux session is live; the parent is the anchor
// of the cycle).
//
// Returns ("", false) when:
//   - the post-live filter leaves zero or one entries, or
//   - current is not in the (filtered) cycle, or
//   - dir is not left or right.
func ResolveLateral(current string, dir Direction, cycle ReviewCycle, liveCheck func(string) bool) (string, bool) {
	full := cycleSessionNames(cycle)

	// Apply liveness filter. The parent is included on the same terms as the
	// agent children; if the parent itself has no live tmux session it is
	// dropped from the cycle too. (In practice the parent is always live for
	// case 1 — the user is navigating from a child — and is by definition
	// live for case 2 — the user is on the parent.)
	live := make([]string, 0, len(full))
	for _, name := range full {
		if liveCheck != nil && !liveCheck(name) {
			// Always retain `current` so the resolver can locate it; it will
			// be advanced past below.
			if name == current {
				live = append(live, name)
			}
			continue
		}
		live = append(live, name)
	}
	if len(live) < 2 {
		return "", false
	}
	idx := -1
	for i, n := range live {
		if n == current {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false
	}
	switch dir {
	case DirRight:
		next := (idx + 1) % len(live)
		return live[next], true
	case DirLeft:
		next := (idx - 1 + len(live)) % len(live)
		return live[next], true
	default:
		return "", false
	}
}
