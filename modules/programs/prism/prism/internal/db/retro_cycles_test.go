package db_test

// retro_cycles_test.go — tests for the DB primitives behind section 3 of
// `prism retro <train-session>`: GroupsForParent,
// GroupResultsAll, and SessionEventAggregates.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestGroupsForParent_OrdersByRoundColumn verifies groups are grouped and
// ordered by the native `round` column, not by registration order or a
// session-name parse.
func TestGroupsForParent_OrdersByRoundColumn(t *testing.T) {
	d := openTestDB(t)
	const parent = "nixos-config@retro-cycles"

	if _, err := d.RegisterGroupWithPR(parent, "10", 3); err != nil {
		t.Fatalf("RegisterGroupWithPR round3: %v", err)
	}
	if _, err := d.RegisterGroupWithPR(parent, "10", 1); err != nil {
		t.Fatalf("RegisterGroupWithPR round1: %v", err)
	}
	if _, err := d.RegisterGroupWithPR(parent, "10", 2); err != nil {
		t.Fatalf("RegisterGroupWithPR round2: %v", err)
	}

	groups, err := d.GroupsForParent(parent)
	if err != nil {
		t.Fatalf("GroupsForParent: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("len(groups) = %d, want 3", len(groups))
	}
	for i, want := range []int{1, 2, 3} {
		if groups[i].Round != want {
			t.Errorf("groups[%d].Round = %d, want %d", i, groups[i].Round, want)
		}
	}
}

// TestGroupsForParent_NoGroups verifies the empty-collection contract: a
// parent with no session_groups rows returns an empty, non-nil slice.
func TestGroupsForParent_NoGroups(t *testing.T) {
	d := openTestDB(t)
	groups, err := d.GroupsForParent("nixos-config@no-groups-at-all")
	if err != nil {
		t.Fatalf("GroupsForParent: %v", err)
	}
	if groups == nil {
		t.Fatal("GroupsForParent returned nil; want non-nil empty slice")
	}
	if len(groups) != 0 {
		t.Fatalf("len(groups) = %d, want 0", len(groups))
	}
}

// TestGetGroup_CarriesDeliveredAt verifies GetGroup surfaces delivered_at,
// which section 3 needs to state whether a round was delivered.
func TestGetGroup_CarriesDeliveredAt(t *testing.T) {
	d := openTestDB(t)
	groupID, err := d.RegisterGroupWithPR("nixos-config@delivered", "11", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}

	gi, err := d.GetGroup(groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if gi.DeliveredAt != nil {
		t.Errorf("DeliveredAt = %v, want nil before delivery", gi.DeliveredAt)
	}

	if err := d.SetGroupDeliveredAt(groupID); err != nil {
		t.Fatalf("SetGroupDeliveredAt: %v", err)
	}

	gi, err = d.GetGroup(groupID)
	if err != nil {
		t.Fatalf("GetGroup after delivery: %v", err)
	}
	if gi.DeliveredAt == nil {
		t.Error("DeliveredAt = nil, want non-nil after SetGroupDeliveredAt")
	}
}

// TestGroupResultsAll_IncludesEndedRows verifies the core fix:
// unlike GroupResults, GroupResultsAll does NOT drop rows whose ended_at is
// set — which is every historical review row (measured: 100% of review
// agent_status rows are closed by the time an operator looks at history).
func TestGroupResultsAll_IncludesEndedRows(t *testing.T) {
	d := openTestDB(t)

	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	const sessEnded = "nixos-config@feature~review-1-goal"
	const sessLive = "nixos-config@feature~review-1-code"

	for _, name := range []string{sessEnded, sessLive} {
		if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", name, err)
		}
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", name, err)
		}
	}
	if err := d.SetEnded(sessEnded); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	results, err := d.GroupResultsAll(groupID)
	if err != nil {
		t.Fatalf("GroupResultsAll: %v", err)
	}
	if _, present := results[sessEnded]; !present {
		t.Errorf("GroupResultsAll excluded the ended row %q; want it included", sessEnded)
	}
	if _, present := results[sessLive]; !present {
		t.Errorf("GroupResultsAll missing the live row %q", sessLive)
	}

	// GroupResults (the live path) must still exclude the ended row —
	// GroupResultsAll must not have changed that behaviour.
	liveResults, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	if _, present := liveResults[sessEnded]; present {
		t.Errorf("GroupResults (live) includes the ended row %q; want it excluded", sessEnded)
	}
}

// TestSessionEventAggregates_SumsWithCoalesce verifies turns and token/cost
// totals are computed per session_name with COALESCE, so a NULL field on one
// event does not void the whole sum, and a session with no msg_assistant
// events is simply absent from the returned map.
func TestSessionEventAggregates_SumsWithCoalesce(t *testing.T) {
	d := openTestDB(t)

	const sess = "nixos-config@feature~review-1-goal"
	const noData = "nixos-config@feature~review-1-code"

	events := []struct {
		payload string
	}{
		{`{"outputTokens":100,"cacheReadTokens":10,"cacheWriteTokens":5,"cost":1.5}`},
		// A NULL cost field (omitted key) must count as zero, not void the sum.
		{`{"outputTokens":50,"cacheReadTokens":0,"cacheWriteTokens":0}`},
	}
	for i, e := range events {
		if err := d.WriteEvent(db.Event{
			ID:          "agg-evt-" + string(rune('a'+i)),
			SessionName: sess,
			Repo:        "nixos-config",
			Worktree:    "/wt",
			Type:        "msg_assistant",
			Payload:     e.payload,
		}); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}

	agg, err := d.SessionEventAggregates([]string{sess, noData})
	if err != nil {
		t.Fatalf("SessionEventAggregates: %v", err)
	}

	got, ok := agg[sess]
	if !ok {
		t.Fatalf("no aggregate for %q", sess)
	}
	if got.Turns != 2 {
		t.Errorf("Turns = %d, want 2", got.Turns)
	}
	if got.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150", got.OutputTokens)
	}
	if got.CostUSD != 1.5 {
		t.Errorf("CostUSD = %v, want 1.5 (NULL cost on the second event must count as zero)", got.CostUSD)
	}

	if _, present := agg[noData]; present {
		t.Errorf("SessionEventAggregates has an entry for %q, want absent (no msg_assistant events)", noData)
	}
}
