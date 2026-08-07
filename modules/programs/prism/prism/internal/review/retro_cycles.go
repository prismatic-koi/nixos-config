package review

// retro_cycles.go — per-cycle, per-agent review detail for `prism retro
// <train-session>` (issue #2584, part 3/4 of tracking issue #2529).
//
// This lives in internal/review, not internal/db, because it reuses the
// #2573 classifier (ClassifyRound, classifyMember, classifyAbsentMember) so a
// round `prism retro` renders as non-counting is the SAME round #2573's live
// review-complete path would have marked non-counting — internal/db cannot
// import internal/review (internal/review already imports internal/db), so
// the assembly has to sit on this side of that boundary.
//
// Data source: db.GroupResultsAll, NOT the live db.GroupResults. GroupResults
// filters `WHERE ended_at IS NULL`, which is correct for the in-flight
// monitor loop (see its doc comment — the #1495 cleanup escape hatch), but
// wrong for a historical read: by the time an operator runs `prism retro`,
// every review-agent agent_status row for a completed round has been closed
// (measured on the live DB: 3,290 of 3,290 review agent_status rows are
// closed — issue #2594). Reading GroupResults here would return an empty map
// for every historical round and mislabel every agent as having no verdict.
//
// Per-agent cost and turn counts come from agent_events directly
// (db.SessionEventAggregates), not from spawn_outcome: review-agent sessions
// almost never get a spawn_outcome row (measured coverage before #2591: 41 of
// 3,384, 1.2%), so relying on it would render "no data" for nearly every
// review agent.

import (
	"fmt"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// ReviewCycleAgent is one review agent's contribution to one review cycle:
// its cost, turn count, and verdict (issue #2584).
//
// Verdict is "PASS", "FAIL", or "" when the agent produced no parseable
// verdict. NoVerdictClass and NoVerdictReason are populated only when Verdict
// is "" — NoVerdictClass is the #2573 classification label (e.g. "stalled
// mid-run"), and NoVerdictReason is the detail recorded for it.
//
// DataRecorded distinguishes "no review data was ever recorded for this
// agent" (turns == 0, no msg_assistant events at all) from "the agent ran and
// recorded a genuine zero" — these must never render the same (issue #2584
// correction 2).
type ReviewCycleAgent struct {
	Agent            string  `json:"agent"`
	Session          string  `json:"session"`
	DataRecorded     bool    `json:"data_recorded"`
	Turns            int64   `json:"turns"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Verdict          string  `json:"verdict"`
	NoVerdictClass   string  `json:"no_verdict_class,omitempty"`
	NoVerdictReason  string  `json:"no_verdict_reason,omitempty"`
}

// ReviewCycle is one review round (one session_groups row) for a train, with
// its per-agent detail (issue #2584).
type ReviewCycle struct {
	// Round is the native session_groups.round column — never inferred from a
	// session name.
	Round int `json:"round"`
	// GroupID is exposed for --json consumers only; it never surfaces in the
	// human-readable table (docs/retro.md section 3: the operator types a
	// session name, never a group_id).
	GroupID string `json:"group_id"`
	// PRNumber is empty when the group was registered via the legacy
	// RegisterGroup path with no PR metadata.
	PRNumber string `json:"pr_number"`
	// CreatedAt is the RFC 3339 time the round was registered.
	CreatedAt string `json:"created_at"`
	// DeliveredAt is the RFC 3339 time the review-complete prompt was
	// delivered for this round, or "" when the round was never delivered
	// (#2259 — the authoritative end-of-life signal).
	DeliveredAt string `json:"delivered_at,omitempty"`
	// CountsAsCycle reports whether this round counted toward the worker's
	// 3-cycle limit, per #2573's classification (RoundStatus.CountsAsCycle).
	CountsAsCycle bool `json:"counts_as_cycle"`
	// NonCountingLabel names why the round did not count, using the same
	// label #2573's RoundStatus.NonCountingLabel assigns. Empty when
	// CountsAsCycle is true.
	NonCountingLabel string `json:"non_counting_label,omitempty"`
	// Agents is the per-agent detail, in the order the group's members were
	// registered (session_name ASC).
	Agents []ReviewCycleAgent `json:"agents"`
}

// RetroReportWithCycles is the wire shape of `prism retro <train-session>`:
// the same db.RetroReport the base command marshals, plus the review-cycle
// detail for the requested train. Train is the resolved session name the
// review cycles were assembled for. ReviewCycles is always a non-nil slice
// (marshals as `[]` when the train has no review groups), matching the
// empty-collection contract the base command's --json output already keeps.
//
// Defined here, not in cmd or internal/sidecar, so the CLI direct-DB path and
// the host-API proxy path serialise byte-identical JSON for the same
// underlying data — the same contract db.AssembleRetro holds for the base
// command (internal/db/retro.go's package doc comment).
type RetroReportWithCycles struct {
	*db.RetroReport
	Train        string        `json:"train"`
	ReviewCycles []ReviewCycle `json:"review_cycles"`
}

// AssembleReviewCycles builds the review-cycle detail for `prism retro
// <train-session>` (issue #2584): every session_groups row whose
// parent_session is root, each with its per-agent cost, turn count, and
// verdict, and a non-counting label per #2573's classification.
//
// Returns an empty, non-nil slice when root has no review groups (no
// session_groups rows) — callers must render "no review cycles ran" for that
// case rather than an empty table (issue #2584 edge case AC).
func AssembleReviewCycles(d *db.DB, root string) ([]ReviewCycle, error) {
	groups, err := d.GroupsForParent(root)
	if err != nil {
		return nil, fmt.Errorf("review: assemble review cycles: %w", err)
	}
	cycles := make([]ReviewCycle, 0, len(groups))
	for _, g := range groups {
		cyc, err := assembleOneReviewCycle(d, g)
		if err != nil {
			return nil, err
		}
		cycles = append(cycles, cyc)
	}
	return cycles, nil
}

// assembleOneReviewCycle builds the ReviewCycle for one session_groups row.
func assembleOneReviewCycle(d *db.DB, g db.GroupInfo) (ReviewCycle, error) {
	cyc := ReviewCycle{
		Round:    g.Round,
		GroupID:  g.GroupID,
		PRNumber: g.PRNumber,
		Agents:   []ReviewCycleAgent{},
	}
	if !g.CreatedAt.IsZero() {
		cyc.CreatedAt = g.CreatedAt.UTC().Format(time.RFC3339)
	}
	if g.DeliveredAt != nil {
		cyc.DeliveredAt = g.DeliveredAt.UTC().Format(time.RFC3339)
	}

	members, err := d.GroupMembersForGroup(g.GroupID)
	if err != nil {
		return cyc, fmt.Errorf("review: assemble review cycles: group %s: members: %w", g.GroupID, err)
	}
	if len(members) == 0 {
		// No agent_status rows were ever written for this group — "no review
		// data recorded", not "review cost was zero" (issue #2584 correction
		// 2). CountsAsCycle stays false with no label: there is nothing to
		// classify.
		return cyc, nil
	}

	sessionNames := make([]string, len(members))
	for i, m := range members {
		sessionNames[i] = m.SessionName
	}

	// Historical read: GroupResultsAll, NOT the live GroupResults (see file
	// doc comment).
	groupData, err := d.GroupResultsAll(g.GroupID)
	if err != nil {
		return cyc, fmt.Errorf("review: assemble review cycles: group %s: results: %w", g.GroupID, err)
	}

	agg, err := d.SessionEventAggregates(sessionNames)
	if err != nil {
		return cyc, fmt.Errorf("review: assemble review cycles: group %s: aggregates: %w", g.GroupID, err)
	}

	// The expected agent list for ClassifyRound is exactly the members the
	// group registered — every review agent gets an agent_status row at
	// spawn time, so there is no historical case where a member is expected
	// but was never registered at all.
	agents := make([]Agent, len(sessionNames))
	rs := ClassifyRound(agents, sessionNames, groupData, nil)
	cyc.CountsAsCycle = rs.CountsAsCycle()
	if !cyc.CountsAsCycle {
		cyc.NonCountingLabel = rs.NonCountingLabel()
	}

	missingBySession := make(map[string]MissingVerdict, len(rs.Missing))
	for _, mv := range rs.Missing {
		missingBySession[mv.Session] = mv
	}

	for _, sn := range sessionNames {
		a := ReviewCycleAgent{
			Session: sn,
			Agent:   agentNameFromSession(sn),
		}
		if av, ok := agg[sn]; ok {
			a.DataRecorded = true
			a.Turns = av.Turns
			a.OutputTokens = av.OutputTokens
			a.CacheReadTokens = av.CacheReadTokens
			a.CacheWriteTokens = av.CacheWriteTokens
			a.CostUSD = av.CostUSD
		}
		if mv, missing := missingBySession[sn]; missing {
			a.NoVerdictClass = string(mv.Class)
			a.NoVerdictReason = mv.Reason
		} else if mr, present := groupData[sn]; present {
			_, _, kind := classifyMember(mr)
			switch kind {
			case VerdictPass:
				a.Verdict = "PASS"
			case VerdictFail:
				a.Verdict = "FAIL"
			}
		} else {
			// Defensive: should be impossible — every session in
			// sessionNames came from the same group_id GroupResultsAll
			// queried, with no filter to drop rows. Treat as unclassified
			// rather than crash.
			a.NoVerdictClass = string(NoVerdictSessionUnknown)
			a.NoVerdictReason = "no group results row could be read for this session"
		}
		cyc.Agents = append(cyc.Agents, a)
	}

	return cyc, nil
}
