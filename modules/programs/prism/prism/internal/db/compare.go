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
// a resolved sessions row plus its assembled spawn_outcome and spawn_inputs.
// It is the element type of the host-API /stats?view=compare and
// view=abtest responses; the CLI renderer consumes the same struct on both
// the direct and proxy paths. JSON tags are explicit so the proxy envelope is
// stable across refactors of the underlying struct field names.
type CompareRunData struct {
	Session *Session      `json:"session"`
	Outcome *SpawnOutcome `json:"outcome"`
	Inputs  *SpawnInputs  `json:"inputs"`
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
// the spawn_inputs row. spawn_inputs is best-effort — pre-#2087 sessions may
// have no row, in which case Inputs stays nil and the renderer surfaces what
// it can from the sessions row instead.
func (d *DB) AssembleCompareRun(sess *Session) CompareRunData {
	cr := CompareRunData{Session: sess}
	cr.Outcome = d.CompareRunOutcome(sess)
	if inputs, err := d.SpawnInputsByInstanceID(sess.InstanceID); err == nil {
		cr.Inputs = inputs
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
