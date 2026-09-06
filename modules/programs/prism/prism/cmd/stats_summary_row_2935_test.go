package cmd

// Issue #2935: the sandbox proxy path of the incarnations table
// (renderStatsIncarnationsFromSessions) used to read sessions.end_state and
// sessions.ended_at directly — both `prism cleanup` writes — so a finished,
// not-yet-cleaned-up session rendered "active" with a still-growing duration
// on the proxy path while the host-direct path already reported the real
// state (issue #2932). TOKENS and COST also always showed "—" on the proxy
// path because it had no DB to aggregate from.
//
// db.AssembleIncarnationSummary now resolves state, duration, tokens, and
// cost once on the host; both renderIncarnationsWithDB (host-direct) and
// renderStatsIncarnationsFromSessions (proxy) render from the resulting
// db.IncarnationSummaryRow, so this file verifies the two renderers produce
// byte-identical output for the same underlying row.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestAssembleIncarnationSummary_ParityBeforeCleanup verifies that the
// host-direct and proxy renderers agree on STATE, DURATION, TOKENS, and COST
// for a session that has reached a terminal state but has not yet been
// cleaned up (sessions.end_state / ended_at still NULL).
func TestAssembleIncarnationSummary_ParityBeforeCleanup(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-40 * time.Minute)

	const sessionName = "repo@2935-parity"
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(time.Minute), 1000, 500, 0, 0, 0.02)
	writeStateChange(t, d, sessionName, iid, "finished", startedAt.Add(12*time.Minute))

	sessions, err := d.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}

	// Host-direct path.
	directOut := captureStdout(t, func() {
		if rerr := renderIncarnationsWithDB(d, sessions); rerr != nil {
			t.Fatalf("renderIncarnationsWithDB: %v", rerr)
		}
	})

	// Proxy path: assemble the row exactly as the host-API handler does, then
	// render with no DB access, mirroring the sandbox path.
	sess := sessions[0]
	row := d.AssembleIncarnationSummary(&sess)
	proxyOut := captureStdout(t, func() {
		if rerr := renderStatsIncarnationsFromSessions([]db.IncarnationSummaryRow{row}); rerr != nil {
			t.Fatalf("renderStatsIncarnationsFromSessions: %v", rerr)
		}
	})

	if directOut != proxyOut {
		t.Errorf("host-direct and proxy output diverge:\ndirect:\n%s\nproxy:\n%s", directOut, proxyOut)
	}
	if !strings.Contains(proxyOut, "finished") {
		t.Errorf("proxy STATE must report the terminal state before cleanup:\n%s", proxyOut)
	}
	if !strings.Contains(proxyOut, "12m 0s") {
		t.Errorf("proxy DURATION must measure to the terminal transition:\n%s", proxyOut)
	}
	if !strings.Contains(proxyOut, "1.5K") || !strings.Contains(proxyOut, "$0.01") {
		t.Errorf("proxy TOKENS/COST must carry real values, not '—':\n%s", proxyOut)
	}
}

// TestAssembleIncarnationSummary_LiveSession verifies a live session renders
// "active" and a to-now duration via the row-assembly helper, matching the
// pre-#2935 live-session behaviour.
func TestAssembleIncarnationSummary_LiveSession(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-5 * time.Minute)

	const sessionName = "repo@2935-live"
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateActive, nil)

	sess, err := d.SessionByInstanceID(iid)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID = (%v, %v)", sess, err)
	}

	row := d.AssembleIncarnationSummary(sess)
	if row.State != "active" {
		t.Errorf("live session state = %q, want active", row.State)
	}
	gotDur := time.Duration(row.DurationMs) * time.Millisecond
	if gotDur < 4*time.Minute || gotDur > 6*time.Minute {
		t.Errorf("live session duration = %s, want ~5m (to-now)", gotDur)
	}
}

// TestAssembleIncarnationSummary_NoOutcomeNoEvents is the edge-case AC: a
// session with no spawn_outcome row and no agent_events must not error, and
// must render with zeroed TOKENS/COST rather than fabricated data.
func TestAssembleIncarnationSummary_NoOutcomeNoEvents(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-1 * time.Minute)

	const sessionName = "repo@2935-empty"
	iid := uuid.New().String()
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sess, err := d.SessionByInstanceID(iid)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID = (%v, %v)", sess, err)
	}

	row := d.AssembleIncarnationSummary(sess)
	if row.TotalTokens != 0 || row.TotalCost != 0 {
		t.Errorf("no-events session should have zero tokens/cost, got %+v", row)
	}

	out := captureStdout(t, func() {
		if rerr := renderStatsIncarnationsFromSessions([]db.IncarnationSummaryRow{row}); rerr != nil {
			t.Fatalf("renderStatsIncarnationsFromSessions: %v", rerr)
		}
	})
	if !strings.Contains(out, sessionName) {
		t.Errorf("output missing session name:\n%s", out)
	}
}
