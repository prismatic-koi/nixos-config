package exporter_test

// Parity between the two consumers of the msg_assistant token/cost fields:
// the Grafana exporter's cost tailer (CostEventsTailSQL) and the
// spawn_outcome aggregation that `prism stats` and `prism stats compare`
// render (db.ComputeSpawnOutcome).
//
// Issue #2932 reported the two disagreeing over the same rows: the exporter
// populated prism_model_cost_usd_total while prism stats printed "no token
// data". The disagreement was on the read path around ComputeSpawnOutcome,
// not in its SQL — both statements read the same five JSON fields off the
// same event type. This test pins that agreement so the two cannot drift:
// the exporter is the reference for what a correct value looks like.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
)

// costTailTotals sums the token and cost columns the exporter's cost tailer
// projects, over every msg_assistant row in the database.
type costTailTotals struct {
	input, output, cacheRead, cacheWrite int64
	cost                                 float64
	rows                                 int
}

func (h *harness) costTailTotals() costTailTotals {
	h.t.Helper()
	rows, err := h.raw().Query(exporter.CostEventsTailSQL, 0, 1000)
	if err != nil {
		h.t.Fatalf("CostEventsTailSQL: %v", err)
	}
	defer rows.Close()

	var totals costTailTotals
	for rows.Next() {
		var (
			rowID                                int64
			model                                string
			input, output, cacheRead, cacheWrite int64
			cost                                 float64
			account, profile                     sql.NullString
		)
		if err := rows.Scan(&rowID, &model, &input, &output, &cacheRead, &cacheWrite, &cost, &account, &profile); err != nil {
			h.t.Fatalf("CostEventsTailSQL scan: %v", err)
		}
		totals.input += input
		totals.output += output
		totals.cacheRead += cacheRead
		totals.cacheWrite += cacheWrite
		totals.cost += cost
		totals.rows++
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("CostEventsTailSQL iterate: %v", err)
	}
	return totals
}

// TestCostTailAndSpawnOutcome_AgreeOnTheSameRows asserts that the numbers
// `prism stats` reports for a session equal the numbers the exporter derives
// from that session's msg_assistant rows.
func TestCostTailAndSpawnOutcome_AgreeOnTheSameRows(t *testing.T) {
	h := newHarness(t)

	const sess = "prism-test@2932-parity"
	iid := uuid.New().String()
	if err := h.writeDB.UpsertStatus(sess, "nixos-config", "/tmp/prism-test", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sess, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := h.writeDB.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sess,
		Repo:        "nixos-config",
		Worktree:    "/tmp/prism-test",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Three turns, including one with a zero cost (a subscription profile
	// reports no per-token billing) and one with fractional cost.
	h.insertAssistant(assistantOpts{instanceID: iid, model: knownModel, input: 1200, output: 9000, cacheRead: 40000, cacheWrite: 250, cost: 1.25})
	h.insertAssistant(assistantOpts{instanceID: iid, model: knownModel, input: 800, output: 8024, cacheRead: 1000, cacheWrite: 50, cost: 0.875})
	h.insertAssistant(assistantOpts{instanceID: iid, model: knownModel, input: 5, output: 7, cacheRead: 0, cacheWrite: 0, cost: 0})

	want := h.costTailTotals()
	if want.rows != 3 {
		t.Fatalf("cost tailer saw %d msg_assistant rows, want 3", want.rows)
	}

	got, err := h.writeDB.ComputeSpawnOutcome(iid)
	if err != nil || got == nil {
		t.Fatalf("ComputeSpawnOutcome = (%v, %v)", got, err)
	}

	if got.TokensInputTotal != want.input {
		t.Errorf("input tokens: stats %d, exporter %d", got.TokensInputTotal, want.input)
	}
	if got.TokensOutputTotal != want.output {
		t.Errorf("output tokens: stats %d, exporter %d", got.TokensOutputTotal, want.output)
	}
	if got.TokensCacheReadTotal != want.cacheRead {
		t.Errorf("cache-read tokens: stats %d, exporter %d", got.TokensCacheReadTotal, want.cacheRead)
	}
	if got.TokensCacheWriteTotal != want.cacheWrite {
		t.Errorf("cache-write tokens: stats %d, exporter %d", got.TokensCacheWriteTotal, want.cacheWrite)
	}
	if got.CostUSDTotal != want.cost {
		t.Errorf("cost: stats %v, exporter %v", got.CostUSDTotal, want.cost)
	}
	if got.MsgAssistantCount != want.rows {
		t.Errorf("msg_assistant count: stats %d, exporter %d", got.MsgAssistantCount, want.rows)
	}
}
