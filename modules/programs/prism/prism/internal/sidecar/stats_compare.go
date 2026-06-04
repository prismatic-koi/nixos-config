package sidecar

// Server-side helpers for the host-API GET /stats compare/abtest views
// (issue #2098).
//
// These helpers mirror the cmd-side `resolveSessionArg` and
// `loadCompareRuns` so that the proxy path produces byte-identical
// output to a host-direct `prism stats compare` / `prism stats abtest
// <group>` invocation:
//
//   - resolveStatsSessionArg replicates the disambiguation rules from
//     cmd/stats_render.go::resolveSessionArg (UUID > session_name >
//     UUID-prefix > error).
//   - statsSessionIsTerminal replicates cmd/stats_compare.go::
//     sessionIsTerminal so that the on-the-fly ComputeSpawnOutcome gate
//     matches between paths.
//   - buildStatsCompareRuns is the server-side analogue of
//     cmd/stats_compare.go::loadCompareRuns: for each session it reads
//     spawn_outcome, falls back to ComputeSpawnOutcome when the session
//     is terminal but no row has been persisted yet (issue #2102), and
//     reads spawn_inputs best-effort.
//
// We intentionally keep these helpers small and inlined rather than
// re-using the cmd-package implementations: cmd cannot be imported from
// internal/sidecar (it would form an import cycle), and lifting the
// logic into internal/db would force db to import internal/agent for
// IsTerminal. The duplication is mechanical and is exercised by the
// proxy-path tests in cmd/stats_proxy_test.go plus the sidecar
// view-handler tests in internal/sidecar/sidecar_stats_compare_test.go.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// statsLookupError carries an HTTP status code alongside an error
// message so the /stats handler can translate a missing-session
// condition into a 404 (matching the CLI's "instance %q not found")
// while still surfacing 500 for genuine DB failures.
type statsLookupError struct {
	status int
	msg    string
}

func (e *statsLookupError) Error() string { return e.msg }

// resolveStatsSessionArg resolves a single `prism stats compare` argument
// (session name, full 36-char UUID, or unambiguous UUID prefix) to a
// sessions row. Mirrors cmd/stats_render.go::resolveSessionArg with the
// same precedence order so the proxy path and the direct-DB path return
// the same row for the same input.
//
// Returns a *statsLookupError with status 404 when the arg cannot be
// resolved, status 409 when a prefix is ambiguous, and a wrapped DB error
// (statsLookupError with status 500 via the caller) for query failures.
func resolveStatsSessionArg(d *db.DB, arg string) (*db.Session, error) {
	// Step 1: full UUID (36 chars).
	if len(arg) == 36 {
		sess, err := d.SessionByInstanceID(arg)
		if err != nil {
			return nil, fmt.Errorf("lookup instance %q: %w", arg, err)
		}
		if sess != nil {
			return sess, nil
		}
		return nil, &statsLookupError{
			status: http.StatusNotFound,
			msg:    fmt.Sprintf("instance %q not found", arg),
		}
	}

	// Step 2: exact session_name match.
	sess, err := d.MostRecentSessionForName(arg)
	if err != nil {
		return nil, fmt.Errorf("lookup session name %q: %w", arg, err)
	}
	if sess != nil {
		return sess, nil
	}

	// Step 3: UUID prefix match.
	matches, err := d.SessionsByInstanceIDPrefix(arg)
	if err != nil {
		return nil, fmt.Errorf("lookup instance prefix %q: %w", arg, err)
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		candidates := make([]string, 0, len(matches))
		for _, m := range matches {
			candidates = append(candidates, m.InstanceID)
		}
		return nil, &statsLookupError{
			status: http.StatusConflict,
			msg: fmt.Sprintf("%q is ambiguous — multiple incarnations match:\n  %s\nuse the full instance_id to disambiguate",
				arg, strings.Join(candidates, "\n  ")),
		}
	}
	return nil, &statsLookupError{
		status: http.StatusNotFound,
		msg:    fmt.Sprintf("%q is not a known instance_id or session_name", arg),
	}
}

// statsSessionIsTerminal mirrors cmd/stats_compare.go::sessionIsTerminal:
// the agent_status row is the live source of truth while it exists, with
// a fallback to sess.EndState for sessions whose status row has already
// been cleaned up.
//
// Returning true gates the on-the-fly ComputeSpawnOutcome call so a
// session that has reached a terminal state but whose spawn_outcome row
// has not yet been written (the window between terminal-state transition
// and `prism cleanup`) still surfaces aggregates over the proxy.
func statsSessionIsTerminal(d *db.DB, sess *db.Session) bool {
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

// buildStatsCompareRuns is the server-side analogue of
// cmd/stats_compare.go::loadCompareRuns. For each session it reads the
// persisted spawn_outcome row first; when the session is terminal but
// has no persisted row yet it computes the outcome on the fly using the
// canonical aggregation (db.ComputeSpawnOutcome). spawn_inputs is read
// best-effort — pre-#2087 sessions have no row and the CLI renderer
// surfaces what it can from sessions instead.
//
// Labels are assigned in input order so the CLI does not need to
// re-derive ordering after unmarshaling. The caller is responsible for
// any ordering policy (input-order for view=compare, session_name-sorted
// for view=abtest).
func buildStatsCompareRuns(d *db.DB, sessions []*db.Session) []StatsCompareRunWire {
	runs := make([]StatsCompareRunWire, 0, len(sessions))
	for i, sess := range sessions {
		label := "run-" + string(rune('A'+i))
		run := StatsCompareRunWire{
			Label:   label,
			Session: sess,
		}
		if out, _ := d.SpawnOutcomeByInstanceID(sess.InstanceID); out != nil {
			run.Outcome = out
		} else if statsSessionIsTerminal(d, sess) {
			if computed, err := d.ComputeSpawnOutcome(sess.InstanceID); err == nil && computed != nil {
				run.Outcome = computed
			}
		}
		if inputs, err := d.SpawnInputsByInstanceID(sess.InstanceID); err == nil {
			run.Inputs = inputs
		}
		runs = append(runs, run)
	}
	return runs
}

// asStatsLookupError extracts a *statsLookupError from err, if present.
// Tiny helper to keep the dispatcher's switch readable.
func asStatsLookupError(err error) (*statsLookupError, bool) {
	var lookupErr *statsLookupError
	if errors.As(err, &lookupErr) {
		return lookupErr, true
	}
	return nil, false
}
