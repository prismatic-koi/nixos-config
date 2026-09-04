package cmd

// Tests for the `prism stats` read path on a finished session that has not
// been cleaned up (issue #2932).
//
// Every worker that ran a review round has a spawn_outcome stub before
// cleanup: the review-complete handler writes review_verdict and its counts
// into a row whose aggregate columns are schema defaults. `prism stats
// <session>` and `prism stats compare` must not surface that stub as the
// session's usage. seedCompareSession (stats_compare_test.go) mirrors the
// cleanup-time UpdateSessionEnded write; the seeds here do not, because the
// scenario under test is the window before cleanup.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
)

// seedFinishedNoCleanup seeds a worker in the state it is in after its last
// turn and before cleanup: agent_status finished, sessions.end_state and
// ended_at NULL, and the sidecar's state_change{finished} event at
// finishedAt.
func seedFinishedNoCleanup(t *testing.T, d *db.DB, sessionName string, startedAt, finishedAt time.Time) string {
	t.Helper()
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	ev := db.Event{
		ID: uuid.New().String(), SessionName: sessionName, Repo: "repo",
		Worktree: "/wt/" + sessionName, InstanceID: &iid,
		Type: "state_change", Payload: `{"state":"finished"}`, CreatedAt: finishedAt,
	}
	if err := d.WriteEvent(ev); err != nil {
		t.Fatalf("WriteEvent (state_change finished): %v", err)
	}
	return iid
}

// TestLoadCompareRuns_ReviewStubBeforeCleanup_PopulatesEveryAxis is the
// compare-path regression test: two finished legs, each with a review-result
// stub and no cleanup, must report tokens, cost, end_state, and duration.
func TestLoadCompareRuns_ReviewStubBeforeCleanup_PopulatesEveryAxis(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-45 * time.Minute)

	const legA = "repo@abtest-leg-a"
	const legB = "repo@abtest-leg-b"
	iidA := seedFinishedNoCleanup(t, d, legA, startedAt, startedAt.Add(28*time.Minute+24*time.Second))
	iidB := seedFinishedNoCleanup(t, d, legB, startedAt, startedAt.Add(28*time.Minute+32*time.Second))
	writeAssistantTurn(t, d, legA, iidA, startedAt.Add(10*time.Second), 9000, 17024, 300, 150, 2.125)
	writeAssistantTurn(t, d, legB, iidB, startedAt.Add(10*time.Second), 5000, 9803, 200, 100, 1.061)
	for _, leg := range []struct{ name, iid string }{{legA, iidA}, {legB, iidB}} {
		if err := d.UpdateSpawnOutcomeReviewResult(leg.iid, "pass", 5, 0); err != nil {
			t.Fatalf("UpdateSpawnOutcomeReviewResult %q: %v", leg.name, err)
		}
	}

	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	runs := loadCompareRuns(d, []*db.Session{sessA, sessB})

	want := map[string][2]string{
		"tokens_output": {"17K", "9.8K"},
		"cost_usd":      {"~$2.13", "~$1.06"},
		"end_state":     {"finished", "finished"},
	}
	for axis, vals := range want {
		for i, run := range runs {
			if got := axisValue(axis, run); got != vals[i] {
				t.Errorf("axisValue(%q) run %d = %q, want %q", axis, i, got, vals[i])
			}
		}
	}
	for i, run := range runs {
		if got := axisValue("duration_ms", run); got == "—" {
			t.Errorf("axisValue(duration_ms) run %d = —, want the start→finished interval", i)
		}
		if got := axisValue("review_verdict", run); got != "pass" {
			t.Errorf("axisValue(review_verdict) run %d = %q, want pass (the stub's verdict must survive)", i, got)
		}
	}
	// The two legs finished 8 s apart; the durations must differ by exactly
	// that, which they cannot if either is measured to a cleanup stamp.
	if runs[0].Outcome == nil || runs[1].Outcome == nil ||
		runs[0].Outcome.DurationMs == nil || runs[1].Outcome.DurationMs == nil {
		t.Fatalf("DurationMs nil on a finished leg: %+v / %+v", runs[0].Outcome, runs[1].Outcome)
	}
	if diff := *runs[1].Outcome.DurationMs - *runs[0].Outcome.DurationMs; diff != 8000 {
		t.Errorf("duration_ms difference between legs = %d ms, want 8000", diff)
	}
}

// TestRenderIncarnationDetail_ReviewStubBeforeCleanup_ShowsTokenUsage covers
// `prism stats <session>` on the same state: the Token Usage block must
// carry the totals, not "no token data".
func TestRenderIncarnationDetail_ReviewStubBeforeCleanup_ShowsTokenUsage(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute)

	const sessionName = "repo@detail-stub"
	iid := seedFinishedNoCleanup(t, d, sessionName, startedAt, startedAt.Add(28*time.Minute))
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(10*time.Second), 9000, 17024, 0, 0, 2.125)
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	out := captureStdout(t, func() { renderIncarnationDetail(d, sess) })

	if strings.Contains(out, "no token data") {
		t.Errorf("detail view reports no token data for a finished session with token-bearing events:\n%s", out)
	}
	for _, want := range []string{"output:", "17K", "est. cost:", "$2.13"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail view missing %q:\n%s", want, out)
		}
	}
}

// TestCompareRunOutcome_AgreesWithExporterCostRows pins the values `prism
// stats` reports to the values the Grafana exporter derives from the same
// msg_assistant rows. The exporter's CostEventsTailSQL is run as-is against
// the test database and its per-row values summed; the sums must equal the
// aggregate the stats path returns.
func TestCompareRunOutcome_AgreesWithExporterCostRows(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute)

	const sessionName = "repo@exporter-parity"
	iid := seedFinishedNoCleanup(t, d, sessionName, startedAt, startedAt.Add(20*time.Minute))
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeAssistantTurn(t, d, sessionName, iid, startedAt.Add(40*time.Second), 2000, 900, 400, 200, 0.34)
	// A row with no token fields at all, as pi writes for a text-only flush.
	writeStatsEventForInstance(t, d, sessionName, iid, "msg_assistant", `{"text":"flush"}`, startedAt.Add(50*time.Second))
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess)
	if out == nil {
		t.Fatal("CompareRunOutcome: nil")
	}

	ro, err := db.OpenReadOnly(d.Path())
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	rows, err := ro.QueryContext(context.Background(), exporter.CostEventsTailSQL, 0, 1000)
	if err != nil {
		t.Fatalf("CostEventsTailSQL: %v", err)
	}
	defer rows.Close()
	var (
		in, outTok, cacheRead, cacheWrite int64
		cost                              float64
		n                                 int
	)
	for rows.Next() {
		var (
			rowid                    int64
			model                    string
			i, o, cr, cw             int64
			c                        float64
			accountName, profileName *string
		)
		if err := rows.Scan(&rowid, &model, &i, &o, &cr, &cw, &c, &accountName, &profileName); err != nil {
			t.Fatalf("scan: %v", err)
		}
		in += i
		outTok += o
		cacheRead += cr
		cacheWrite += cw
		cost += c
		n++
	}
	if n != 5 {
		t.Fatalf("exporter tailed %d msg_assistant rows, want 5", n)
	}
	if out.TokensInputTotal != in || out.TokensOutputTotal != outTok ||
		out.TokensCacheReadTotal != cacheRead || out.TokensCacheWriteTotal != cacheWrite {
		t.Errorf("token totals disagree with the exporter\n  stats:    in=%d out=%d cr=%d cw=%d\n  exporter: in=%d out=%d cr=%d cw=%d",
			out.TokensInputTotal, out.TokensOutputTotal, out.TokensCacheReadTotal, out.TokensCacheWriteTotal,
			in, outTok, cacheRead, cacheWrite)
	}
	if absDiff(out.CostUSDTotal, cost) > 1e-9 {
		t.Errorf("cost disagrees with the exporter: stats=%.6f exporter=%.6f", out.CostUSDTotal, cost)
	}
	if out.MsgAssistantCount != n {
		t.Errorf("MsgAssistantCount = %d, exporter tailed %d rows", out.MsgAssistantCount, n)
	}
}

// TestRenderIncarnationDetail_NoTokenFields_ReportsNoData is the edge case:
// a finished session whose events carry no token fields renders the explicit
// no-data marker and does not error.
func TestRenderIncarnationDetail_NoTokenFields_ReportsNoData(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-10 * time.Minute)

	const sessionName = "repo@no-token-fields"
	iid := seedFinishedNoCleanup(t, d, sessionName, startedAt, startedAt.Add(5*time.Minute))
	writeStatsEventForInstance(t, d, sessionName, iid, "msg_assistant", `{"text":"only text"}`, startedAt.Add(time.Second))
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	sess, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess)
	if out == nil {
		t.Fatal("CompareRunOutcome: nil for a finished session")
	}
	if out.TokensInputTotal != 0 || out.TokensOutputTotal != 0 || out.CostUSDTotal != 0 {
		t.Errorf("token-less events must aggregate to zero, got in=%d out=%d cost=%f",
			out.TokensInputTotal, out.TokensOutputTotal, out.CostUSDTotal)
	}
	if out.MsgAssistantCount != 1 {
		t.Errorf("MsgAssistantCount = %d, want 1", out.MsgAssistantCount)
	}

	rendered := captureStdout(t, func() { renderIncarnationDetail(d, sess) })
	if !strings.Contains(rendered, "no token data") {
		t.Errorf("detail view must report the no-data marker for token-less events:\n%s", rendered)
	}
	runs := loadCompareRuns(d, []*db.Session{sess})
	for _, axis := range []string{"tokens_input", "tokens_output", "cost_usd"} {
		if got := axisValue(axis, runs[0]); got != "—" {
			t.Errorf("axisValue(%q) = %q, want — for token-less events", axis, got)
		}
	}
	if got := axisValue("msg_assistant", runs[0]); got != "1" {
		t.Errorf("axisValue(msg_assistant) = %q, want 1", got)
	}
}

// writeStatsEventForInstance writes an event with an explicit instance_id.
func writeStatsEventForInstance(t *testing.T, d *db.DB, sessionName, iid, typ, payload string, ts time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID: uuid.New().String(), SessionName: sessionName, Repo: "repo",
		Worktree: "/wt/" + sessionName, InstanceID: &iid,
		Type: typ, Payload: payload, CreatedAt: ts,
	}); err != nil {
		t.Fatalf("WriteEvent (%s): %v", typ, err)
	}
}
