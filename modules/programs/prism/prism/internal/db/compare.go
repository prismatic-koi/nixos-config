package db

// compare.go — canonical data-assembly for `prism stats compare`,
// `prism stats abtest <group_id>`, and the resolution helpers they share.
//
// These functions are the single source of truth for the data shown by the
// comparison engine. Both the CLI direct-DB path (cmd/stats_compare.go) and
// the host-API proxy path (internal/sidecar host_api.go GET /stats?view=…)
// call them, so the bytes rendered are identical regardless of whether the
// query was served from the local DB or proxied to the host sidecar
// (issue #2098). Keeping resolution + aggregation here — rather than
// duplicating it on the sidecar side — is what guarantees the byte-identical
// output contract.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
)

// CompareRunData is the wire/render shape for one run in `prism stats compare`:
// a resolved sessions row plus its assembled spawn_outcome and the slim,
// non-sensitive spawn_inputs projection. It is the element type of the
// host-API /stats?view=compare and view=abtest responses; the CLI renderer
// consumes the same struct on both the direct and proxy paths. JSON tags are
// explicit so the proxy envelope is stable across refactors of the underlying
// struct field names.
//
// Inputs is CompareInputs, not the full *SpawnInputs, by design: /stats is the
// all-roles "aggregate counts" surface, and the full spawn_inputs row carries
// row-level conversation content (prompt_text, prompt_source, …) that must not
// cross that boundary. See CompareInputs.
type CompareRunData struct {
	Session *Session       `json:"session"`
	Outcome *SpawnOutcome  `json:"outcome"`
	Inputs  *CompareInputs `json:"inputs"`
}

// CompareInputs is the deliberately-slim projection of spawn_inputs that the
// comparison renderer consumes. It contains only the six non-sensitive
// provenance fields surfaced by `prism stats compare` (cmd/stats_compare.go
// inputsValue). The conversation-bearing columns of SpawnInputs —
// prompt_text, prompt_source, model_variant_overrides, extras — are
// intentionally excluded so they never cross the all-roles host-API /stats
// boundary.
//
// /stats is the "aggregate counts" surface; row-level conversation content
// stays behind the coordinator-only /db/query and /checkin endpoints (see the
// privilege boundary documented in internal/sidecar/host_api.go). This mirrors
// the view=detail precedent, which returns only *Session and never the spawn
// prompt (issue #2098, round-1 security review).
type CompareInputs struct {
	ProfileName *string `json:"profile_name"`
	HarnessFlag *string `json:"harness_flag"`
	// IsolationFlag is the raw --isolation CLI value (nil = flag omitted).
	// Preserved as the audit trail; consumers that want the actual mode the
	// session ran under should read IsolationMode instead. Issue #2105.
	IsolationFlag *string `json:"isolation_flag"`
	// IsolationMode is the resolved effective isolation mode the session
	// actually ran under (bwrap/sandbox-exec/host), captured at
	// spawn time post profile/config/Nix-default resolution. Always
	// populated for sessions spawned post-#2105; nil only on pre-#2105 rows
	// (the renderer falls back to IsolationFlag for those).
	IsolationMode *string `json:"isolation_mode"`
	AgentFlag     *string `json:"agent_flag"`
	BranchFlag    *string `json:"branch_flag"`
	AbtestPairID  *string `json:"abtest_pair_id"`
}

// ResolveSessionArg resolves a user-supplied argument to a sessions row.
// The argument may be a full 36-character instance_id, a session name, or an
// unambiguous instance_id prefix. When forceInstance is true the argument is
// treated as an instance_id regardless of length.
//
// This is the shared resolver behind both `prism stats <id>` and
// `prism stats compare <id>…`; the sidecar proxy path calls it directly so
// session-name / prefix resolution is byte-for-byte identical to the host
// path (issue #2098).
func (d *DB) ResolveSessionArg(arg string, forceInstance bool) (*Session, error) {
	// Step 1: full UUID (36 chars) or --instance flag.
	if forceInstance || len(arg) == 36 {
		sess, err := d.SessionByInstanceID(arg)
		if err != nil {
			return nil, fmt.Errorf("stats: lookup instance %q: %w", arg, err)
		}
		if sess != nil {
			return sess, nil
		}
		return nil, fmt.Errorf("stats: instance %q not found", arg)
	}

	// Step 2: try exact session_name match first.
	sess, err := d.MostRecentSessionForName(arg)
	if err != nil {
		return nil, fmt.Errorf("stats: lookup session name %q: %w", arg, err)
	}
	if sess != nil {
		return sess, nil
	}

	// Step 3: try UUID prefix match.
	matches, err := d.SessionsByInstanceIDPrefix(arg)
	if err != nil {
		return nil, fmt.Errorf("stats: lookup instance prefix %q: %w", arg, err)
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		var candidates []string
		for _, m := range matches {
			candidates = append(candidates, m.InstanceID)
		}
		return nil, fmt.Errorf("stats: %q is ambiguous — multiple incarnations match:\n  %s\nuse the full instance_id to disambiguate",
			arg, strings.Join(candidates, "\n  "))
	}

	return nil, fmt.Errorf("stats: %q is not a known instance_id or session_name", arg)
}

// SessionIsTerminal reports whether sess is in a terminal state
// (finished / error / interrupted / deleted) — the gate for computing
// spawn_outcome on the fly when no persisted row exists yet.
//
// agent_status is the live source of truth while the row still exists; it
// falls back to sessions.end_state for sessions whose agent_status row has
// already been cleaned away but whose sessions row still records a terminal
// end_state. The "reset" marker is deliberately excluded — it can be set
// before the more-specific UpdateSessionEnded call, when the aggregates may
// not yet be stable (issue #2102).
func (d *DB) SessionIsTerminal(sess *Session) bool {
	if sess == nil {
		return false
	}
	if st, err := d.CurrentStatus(sess.SessionName); err == nil && st != nil {
		return agent.IsTerminal(agent.AgentState(st.State))
	}
	if sess.EndState != nil {
		switch *sess.EndState {
		case "finished", "error", "interrupted", "deleted":
			return true
		}
	}
	return false
}

// CompareRunOutcome returns the spawn_outcome to display for sess: the
// persisted spawn_outcome row when present, or — when the session has reached
// a terminal state but no row has been written yet (the window between the
// terminal transition and `prism cleanup`) — an on-the-fly computation via
// ComputeSpawnOutcome. ComputeSpawnOutcome is the same aggregation
// WriteSpawnOutcome uses internally, so the value agrees byte-for-byte with
// the row cleanup will later persist. Returns nil for live sessions (the
// renderer shows "—"). Issue #2102.
func (d *DB) CompareRunOutcome(sess *Session) *SpawnOutcome {
	if sess == nil {
		return nil
	}
	if out, _ := d.SpawnOutcomeByInstanceID(sess.InstanceID); out != nil {
		return out
	}
	if d.SessionIsTerminal(sess) {
		if computed, err := d.ComputeSpawnOutcome(sess.InstanceID); err == nil && computed != nil {
			return computed
		}
	}
	return nil
}

// AssembleCompareRun gathers the per-run data the comparison renderer needs
// for a single resolved session: the persisted-or-computed spawn_outcome and
// the slim spawn_inputs projection. spawn_inputs is best-effort — pre-#2087
// sessions may have no row, in which case Inputs stays nil and the renderer
// surfaces what it can from the sessions row instead. The full row is
// projected down to CompareInputs here so the sensitive conversation columns
// never leave the DB layer (see CompareInputs).
func (d *DB) AssembleCompareRun(sess *Session) CompareRunData {
	cr := CompareRunData{Session: sess}
	cr.Outcome = d.CompareRunOutcome(sess)
	if inputs, err := d.SpawnInputsByInstanceID(sess.InstanceID); err == nil && inputs != nil {
		cr.Inputs = &CompareInputs{
			ProfileName:   inputs.ProfileName,
			HarnessFlag:   inputs.HarnessFlag,
			IsolationFlag: inputs.IsolationFlag,
			IsolationMode: inputs.IsolationMode,
			AgentFlag:     inputs.AgentFlag,
			BranchFlag:    inputs.BranchFlag,
			AbtestPairID:  inputs.AbtestPairID,
		}
	}
	return cr
}

// AbtestGroupSessions resolves all members of a session group to their most
// recent sessions rows, sorted by session_name for deterministic output.
// Shared by `prism stats abtest <group_id>` on both the direct and proxy
// paths so the resolved member set and ordering are identical (issue #2098).
func (d *DB) AbtestGroupSessions(groupID string) ([]*Session, error) {
	members, err := d.GroupResults(groupID)
	if err != nil {
		return nil, fmt.Errorf("resolve group members: %w", err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("no members found for group %q", groupID)
	}

	var sessions []*Session
	for _, m := range members {
		sess, err := d.MostRecentSessionForName(m.SessionName)
		if err != nil {
			return nil, fmt.Errorf("resolve member %q: %w", m.SessionName, err)
		}
		if sess == nil {
			return nil, fmt.Errorf("member session %q not found in sessions table", m.SessionName)
		}
		sessions = append(sessions, sess)
	}
	// Sort by session_name for deterministic output.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionName < sessions[j].SessionName
	})
	return sessions, nil
}
