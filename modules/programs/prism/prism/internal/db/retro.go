package db

// retro.go — canonical data-assembly for `prism retro` (issue #2583, part 2/4
// of the tracking issue #2529). AssembleRetro is the single source of truth
// for the window totals, the trains table, and the waste signals the command
// renders. Both the CLI direct-DB path (cmd/retro.go) and the host-API proxy
// path (internal/sidecar host_api.go GET /retro) call it, so the bytes the CLI
// renders are identical on the host path and the sandbox path — the same
// contract `prism stats compare` and `prism stats <session>` hold (issues
// #2098, #2582). Keeping the assembly here, not on the sidecar side, is what
// guarantees that contract.
//
// Data sources and the corrections that shaped them (see docs/retro.md and the
// CORRECTIONS block on issue #2583):
//
//   - Per-session token/cost/waste data comes from CompareRunOutcome, which
//     returns the persisted spawn_outcome row, or — for a terminal session
//     with no row yet — an on-the-fly ComputeSpawnOutcome aggregation over
//     agent_events. That fallback is what makes review-agent sessions countable
//     even before `prism cleanup` writes their rows and for historical rows
//     that predate the WriteSpawnOutcomeCascade change (#2591): ComputeSpawnOutcome
//     reads the same agent_events the corrections require, with COALESCE on
//     every token field, so a NULL cache-read/-write field counts as zero
//     rather than voiding the whole SUM.
//   - A live session (no terminal transition, no row) yields a nil outcome. It
//     is counted as a member of its train but contributes no tokens and no
//     waste signal — the difference between "not yet recorded" and "recorded
//     zero" is preserved, never collapsed.
//   - Train membership resolves through session_groups.parent_session, the
//     durable foreign key, via each session's own sessions.group_id — never
//     the ephemeral agent_status row. A name-parse fallback covers only the
//     case where the FK is absent (NULL group_id or a deleted group row).

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/sessionname"
)

// RetroReport is the assembled, render-ready shape for one `prism retro` run.
// It is the wire shape of the host-API GET /retro response and the value the
// CLI marshals for `--json`; the JSON tags are the snake_case contract the AC
// requires. Empty collections marshal as `[]`, not null (Trains is always a
// non-nil slice).
type RetroReport struct {
	Repo string `json:"repo"`
	// Since and Until bound the window as RFC 3339 timestamps (UTC). Since is
	// the sinceMs cut-off; Until is the assembly time.
	Since        string            `json:"since"`
	Until        string            `json:"until"`
	WindowTotals RetroWindowTotals `json:"window_totals"`
	Trains       []RetroTrain      `json:"trains"`
	WasteSignals RetroWasteSignals `json:"waste_signals"`
}

// RetroWindowTotals is section 1: the aggregate token volume across every
// contributing session in the window. Token volume is the primary axis; cost
// is secondary and is zero under subscription profiles, so it is never the
// only figure carried.
type RetroWindowTotals struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// CacheReadShare is cache-read tokens as a fraction (0..1) of TotalTokens —
	// the context-re-read share. Zero when TotalTokens is zero.
	CacheReadShare float64 `json:"cache_read_share"`
	CostUSD        float64 `json:"cost_usd"`
	// SessionCount is every session instance folded into the window (train
	// members included). LiveSessionCount is the subset with no spawn_outcome
	// row yet — counted, but contributing no tokens.
	SessionCount     int `json:"session_count"`
	LiveSessionCount int `json:"live_session_count"`
}

// RetroTrain is one row of section 2: a worker session plus its review
// children rolled up as one unit of work, or a coordinator plus its
// investigators, or a solo investigator, or one A/B leg.
type RetroTrain struct {
	// Root is the friendly session name the operator already types — the
	// worker name, the coordinator name, or the investigator name. group_id
	// never surfaces here.
	Root string `json:"root"`
	// Kind is "worker", "coordinator", or "investigator".
	Kind string `json:"kind"`
	// Profile is the spawn --profile tier of the root session, empty when no
	// spawn_inputs row records one.
	Profile string `json:"profile"`
	// StartedAt is the RFC 3339 start time of the earliest member.
	StartedAt string `json:"started_at"`
	// MemberCount is every session instance in the train; LiveMemberCount is
	// the subset with no spawn_outcome row yet.
	MemberCount     int `json:"member_count"`
	LiveMemberCount int `json:"live_member_count"`
	// ReviewCycles is the number of review rounds (session_groups rows) whose
	// parent_session is Root. Zero for investigators and un-reviewed workers.
	ReviewCycles     int     `json:"review_cycles"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// WindowShare is this train's TotalTokens as a fraction (0..1) of the
	// window's TotalTokens.
	WindowShare float64 `json:"window_share"`
}

// RetroWasteSignals is section 5: the waste-signal counts summed over every
// contributing spawn_outcome row in the window. Available is false only when
// no session in the window had an outcome row (persisted or computable) — the
// signal source is absent, which must render distinctly from a recorded zero.
// When Available is true the four counts are authoritative, and a zero is a
// real "no occurrences", stated explicitly rather than omitted.
type RetroWasteSignals struct {
	Available             bool  `json:"available"`
	DoomLoopCount         int64 `json:"doom_loop_count"`
	ToolErrorCount        int64 `json:"tool_error_count"`
	PermissionAskCount    int64 `json:"permission_ask_count"`
	PermissionDeniedCount int64 `json:"permission_denied_count"`
}

// AssembleRetro builds the retrospective for the window [sinceMs, now] scoped
// to repo (empty repo = all repos). It is the single assembly used by both the
// CLI direct path and the host-API proxy path.
func (d *DB) AssembleRetro(repo string, sinceMs int64) (*RetroReport, error) {
	var sessions []Session
	var err error
	if repo != "" {
		sessions, err = d.SessionsForRepoSince(repo, sinceMs)
	} else {
		sessions, err = d.SessionsSince(sinceMs)
	}
	if err != nil {
		return nil, fmt.Errorf("db: assemble retro: list sessions: %w", err)
	}

	// group_id → parent_session for every registered review group. This is the
	// durable train-membership link; sessions.group_id joins into it.
	groupParents, err := d.AllGroupParents()
	if err != nil {
		return nil, fmt.Errorf("db: assemble retro: group parents: %w", err)
	}
	// parent_session → set of its group_ids, so a train's review-cycle count is
	// the number of distinct rounds (session_groups rows) it owns.
	groupsByParent := map[string]map[string]struct{}{}
	for gid, parent := range groupParents {
		if groupsByParent[parent] == nil {
			groupsByParent[parent] = map[string]struct{}{}
		}
		groupsByParent[parent][gid] = struct{}{}
	}

	report := &RetroReport{
		Repo:   repo,
		Since:  time.UnixMilli(sinceMs).UTC().Format(time.RFC3339),
		Until:  time.Now().UTC().Format(time.RFC3339),
		Trains: []RetroTrain{},
	}

	type trainAcc struct {
		train        RetroTrain
		earliest     time.Time
		rootInstance string
	}
	accs := map[string]*trainAcc{}
	var order []string

	var anyOutcome bool
	for i := range sessions {
		s := &sessions[i]
		root, kind, isRoot := d.retroClassify(s, groupParents)

		a := accs[root]
		if a == nil {
			a = &trainAcc{train: RetroTrain{Root: root, Kind: kind}, earliest: s.StartedAt}
			accs[root] = a
			order = append(order, root)
		}
		// The root's own session carries the authoritative kind and the
		// instance whose spawn_inputs holds the profile tier.
		if isRoot {
			a.train.Kind = kind
			a.rootInstance = s.InstanceID
		}
		if !s.StartedAt.IsZero() && s.StartedAt.Before(a.earliest) {
			a.earliest = s.StartedAt
		}
		a.train.MemberCount++
		report.WindowTotals.SessionCount++

		out := d.CompareRunOutcome(s)
		if out == nil {
			// Live session: counted, but no token or waste contribution. Keep
			// "not yet recorded" distinct from "recorded zero".
			a.train.LiveMemberCount++
			report.WindowTotals.LiveSessionCount++
			continue
		}
		anyOutcome = true

		a.train.InputTokens += out.TokensInputTotal
		a.train.OutputTokens += out.TokensOutputTotal
		a.train.CacheReadTokens += out.TokensCacheReadTotal
		a.train.CacheWriteTokens += out.TokensCacheWriteTotal
		a.train.CostUSD += out.CostUSDTotal

		wt := &report.WindowTotals
		wt.InputTokens += out.TokensInputTotal
		wt.OutputTokens += out.TokensOutputTotal
		wt.CacheReadTokens += out.TokensCacheReadTotal
		wt.CacheWriteTokens += out.TokensCacheWriteTotal
		wt.CostUSD += out.CostUSDTotal

		ws := &report.WasteSignals
		ws.DoomLoopCount += int64(out.DoomLoopCount)
		ws.ToolErrorCount += int64(out.ToolErrorCount)
		ws.PermissionAskCount += int64(out.PermissionAskCount)
		ws.PermissionDeniedCount += int64(out.PermissionDeniedCount)
	}

	// Waste signals are available only when at least one outcome row backed
	// them. No rows → source absent → render "unavailable", not "0".
	report.WasteSignals.Available = anyOutcome

	wt := &report.WindowTotals
	wt.TotalTokens = wt.InputTokens + wt.OutputTokens + wt.CacheReadTokens + wt.CacheWriteTokens
	if wt.TotalTokens > 0 {
		wt.CacheReadShare = float64(wt.CacheReadTokens) / float64(wt.TotalTokens)
	}

	for _, root := range order {
		a := accs[root]
		t := a.train
		t.TotalTokens = t.InputTokens + t.OutputTokens + t.CacheReadTokens + t.CacheWriteTokens
		if wt.TotalTokens > 0 {
			t.WindowShare = float64(t.TotalTokens) / float64(wt.TotalTokens)
		}
		t.ReviewCycles = len(groupsByParent[root])
		if !a.earliest.IsZero() {
			t.StartedAt = a.earliest.UTC().Format(time.RFC3339)
		}
		if a.rootInstance != "" {
			if si, siErr := d.SpawnInputsByInstanceID(a.rootInstance); siErr == nil && si != nil && si.ProfileName != nil {
				t.Profile = *si.ProfileName
			}
		}
		report.Trains = append(report.Trains, t)
	}

	// Deterministic order: heaviest train first, ties broken by name so the
	// table and the JSON agree run to run.
	sort.SliceStable(report.Trains, func(i, j int) bool {
		if report.Trains[i].TotalTokens != report.Trains[j].TotalTokens {
			return report.Trains[i].TotalTokens > report.Trains[j].TotalTokens
		}
		return report.Trains[i].Root < report.Trains[j].Root
	})

	return report, nil
}

// retroClassify maps a session to its train root and the train kind, and
// reports whether the session is the train's own root (as opposed to a member
// that rolls up into another session's train).
//
//   - A review agent (`<worker>~review-N-<agent>`) rolls into its worker train,
//     resolved through session_groups.parent_session via sessions.group_id. The
//     name is only the identifier and the FK-absent fallback, never the primary
//     link.
//   - An investigator (`<invoker>~investigate-<slug>`) rolls into its invoker's
//     train when the invoker is a coordinator (the coordinator + its
//     investigators are one train); otherwise it is a solo train of one, never
//     attributed to a worker train.
//   - A coordinator session (`<repo>@main`, or root_agent_name = coordinator)
//     is its own train root.
//   - Any other session is a worker train root; each A/B leg has its own name
//     and so is its own row.
func (d *DB) retroClassify(s *Session, groupParents map[string]string) (root, kind string, isRoot bool) {
	name := s.SessionName

	if idx := strings.Index(name, "~review-"); idx >= 0 {
		parent := ""
		if s.GroupID != nil {
			parent = groupParents[*s.GroupID]
		}
		if parent == "" {
			// FK absent (NULL group_id or a deleted group row): fall back to the
			// naming convention so the child still rolls into its worker.
			parent = name[:idx]
		}
		return parent, "worker", false
	}

	if idx := strings.Index(name, "~investigate-"); idx >= 0 {
		invoker := name[:idx]
		if s.ParentSession != nil && *s.ParentSession != "" {
			invoker = *s.ParentSession
		}
		if d.retroIsCoordinator(invoker) {
			return invoker, "coordinator", false
		}
		return name, "investigator", true
	}

	if d.retroIsCoordinator(name) {
		return name, "coordinator", true
	}

	return name, "worker", true
}

// retroIsCoordinator reports whether name is a coordinator session. It mirrors
// authz.IsCoordinatorSession's rule — a descendant name is never a
// coordinator, then the "@main" name suffix, or a root_agent_name of
// "coordinator" — without the logger dependency, since a retro read needs no
// diagnostics on the classification.
func (d *DB) retroIsCoordinator(name string) bool {
	if sessionname.IsDescendant(name) {
		return false
	}
	if sessionname.HasCoordinatorSuffix(name) {
		return true
	}
	if r, rowExists, err := d.RootAgentName(name); err == nil && rowExists && r == "coordinator" {
		return true
	}
	return false
}
