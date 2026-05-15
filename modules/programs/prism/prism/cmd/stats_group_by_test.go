package cmd

// Tests for stats --group-by.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// writeGroupBySession inserts a sessions row, a spawn_inputs row, and a
// spawn_outcome row for a synthetic session used in group-by tests.
func writeGroupBySession(t *testing.T, d *db.DB, harness, profile, variant, model string) string {
	t.Helper()

	iid := uuid.New().String()
	sessName := "repo@" + iid[:8]
	startedAt := time.Now().Add(-time.Hour)
	endedAt := startedAt.Add(30 * time.Minute)
	endState := "finished"

	sess := db.Session{
		InstanceID:  iid,
		SessionName: sessName,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Harness:     "pi",
		StartedAt:   startedAt,
		EndedAt:     &endedAt,
		EndState:    &endState,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	si := db.SpawnInputs{
		InstanceID: iid,
		CreatedAt:  startedAt.UnixMilli(),
	}
	if harness != "" {
		si.HarnessFlag = &harness
	}
	if profile != "" {
		si.ProfileName = &profile
	}
	if variant != "" {
		si.VariantFlag = &variant
	}
	if model != "" {
		si.ModelFlag = &model
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	// Write a couple of token-bearing events so spawn_outcome has non-zero values.
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessName,
		InstanceID:  &iid,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "msg_assistant",
		Payload:     `{"model":"anthropic/claude-sonnet-4-6","inputTokens":100,"outputTokens":50,"cacheReadTokens":10,"cacheWriteTokens":5,"cost":0.001}`,
		CreatedAt:   startedAt.Add(time.Minute),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	return iid
}

// TestRunStatsGroupBy_Harness verifies that --group-by harness renders a
// breakdown table showing each harness value as a row.
func TestRunStatsGroupBy_Harness(t *testing.T) {
	d := openStatsTestDB(t)

	// Create two sessions under "pi" harness and one under "pi".
	writeGroupBySession(t, d, "pi", "", "", "")
	writeGroupBySession(t, d, "pi", "", "", "")
	writeGroupBySession(t, d, "pi", "", "", "")

	out := captureStdout(t, func() {
		if err := runStatsGroupBy("harness", 0); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "pi") {
		t.Errorf("expected 'pi' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "pi") {
		t.Errorf("expected 'pi' in output; got:\n%s", out)
	}
}

// TestRunStatsGroupBy_Profile verifies grouping by profile_name.
func TestRunStatsGroupBy_Profile(t *testing.T) {
	d := openStatsTestDB(t)

	writeGroupBySession(t, d, "", "alpha", "", "")
	writeGroupBySession(t, d, "", "beta", "", "")
	writeGroupBySession(t, d, "", "alpha", "", "")

	out := captureStdout(t, func() {
		if err := runStatsGroupBy("profile", 0); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "alpha") {
		t.Errorf("expected 'alpha' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "beta") {
		t.Errorf("expected 'beta' in output; got:\n%s", out)
	}
}

// TestRunStatsGroupBy_Variant verifies grouping by variant_flag.
func TestRunStatsGroupBy_Variant(t *testing.T) {
	d := openStatsTestDB(t)

	writeGroupBySession(t, d, "", "", "v1", "")
	writeGroupBySession(t, d, "", "", "v2", "")

	out := captureStdout(t, func() {
		if err := runStatsGroupBy("variant", 0); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "v1") {
		t.Errorf("expected 'v1' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "v2") {
		t.Errorf("expected 'v2' in output; got:\n%s", out)
	}
}

// TestRunStatsGroupBy_Model verifies grouping by model_flag.
func TestRunStatsGroupBy_Model(t *testing.T) {
	d := openStatsTestDB(t)

	writeGroupBySession(t, d, "", "", "", "anthropic/claude-sonnet-4-6")
	writeGroupBySession(t, d, "", "", "", "anthropic/claude-opus-4-6")
	writeGroupBySession(t, d, "", "", "", "anthropic/claude-sonnet-4-6")

	out := captureStdout(t, func() {
		if err := runStatsGroupBy("model", 0); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "claude-son") {
		t.Errorf("expected claude-son in output; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-opus") {
		t.Errorf("expected claude-opus in output; got:\n%s", out)
	}
}

// TestRunStatsGroupBy_NullValues verifies that sessions with a NULL group-by
// column value are rendered as "(none)" rather than crashing or empty output.
func TestRunStatsGroupBy_NullValues(t *testing.T) {
	d := openStatsTestDB(t)

	// All sessions have NULL harness_flag (empty string → nil).
	writeGroupBySession(t, d, "", "", "", "")
	writeGroupBySession(t, d, "", "", "", "")

	out := captureStdout(t, func() {
		if err := runStatsGroupBy("harness", 0); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "(none)") {
		t.Errorf("expected '(none)' for NULL group values; got:\n%s", out)
	}
}

// TestRunStatsGroupBy_WithDaysFilter verifies that --days filtering is applied
// before grouping — sessions outside the window are excluded.
func TestRunStatsGroupBy_WithDaysFilter(t *testing.T) {
	d := openStatsTestDB(t)

	// Create one recent session (within 7 days) and one old session (14 days ago).
	recentIID := uuid.New().String()
	recentName := "repo@recent"
	now := time.Now()
	recentStart := now.Add(-1 * time.Hour)
	recentEnd := recentStart.Add(30 * time.Minute)
	recentEnd2 := recentEnd
	endState := "finished"
	profile := "new-profile"

	recentSess := db.Session{
		InstanceID:  recentIID,
		SessionName: recentName,
		Repo:        "testrepo",
		Worktree:    "/code",
		Harness:     "pi",
		StartedAt:   recentStart,
		EndedAt:     &recentEnd2,
		EndState:    &endState,
	}
	if err := d.InsertSession(recentSess); err != nil {
		t.Fatalf("InsertSession recent: %v", err)
	}
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:  recentIID,
		ProfileName: &profile,
		CreatedAt:   recentStart.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs recent: %v", err)
	}
	recentE := db.Event{
		ID:          uuid.New().String(),
		SessionName: recentName,
		InstanceID:  &recentIID,
		Repo:        "testrepo",
		Worktree:    "/code",
		Type:        "msg_assistant",
		Payload:     `{"model":"anthropic/claude-sonnet-4-6","inputTokens":50,"outputTokens":20,"cost":0.0005}`,
		CreatedAt:   recentStart.Add(time.Minute),
	}
	if err := d.WriteEvent(recentE); err != nil {
		t.Fatalf("WriteEvent recent: %v", err)
	}
	if err := d.WriteSpawnOutcome(recentIID); err != nil {
		t.Fatalf("WriteSpawnOutcome recent: %v", err)
	}

	// Old session: 14 days ago.
	oldIID := uuid.New().String()
	oldName := "repo@old"
	oldStart := now.Add(-14 * 24 * time.Hour)
	oldEnd := oldStart.Add(30 * time.Minute)
	oldProfile := "old-profile"

	oldSess := db.Session{
		InstanceID:  oldIID,
		SessionName: oldName,
		Repo:        "testrepo",
		Worktree:    "/code",
		Harness:     "pi",
		StartedAt:   oldStart,
		EndedAt:     &oldEnd,
		EndState:    &endState,
	}
	if err := d.InsertSession(oldSess); err != nil {
		t.Fatalf("InsertSession old: %v", err)
	}
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:  oldIID,
		ProfileName: &oldProfile,
		CreatedAt:   oldStart.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs old: %v", err)
	}
	oldE := db.Event{
		ID:          uuid.New().String(),
		SessionName: oldName,
		InstanceID:  &oldIID,
		Repo:        "testrepo",
		Worktree:    "/code",
		Type:        "msg_assistant",
		Payload:     `{"model":"anthropic/claude-sonnet-4-6","inputTokens":50,"outputTokens":20,"cost":0.0005}`,
		CreatedAt:   oldStart.Add(time.Minute),
	}
	if err := d.WriteEvent(oldE); err != nil {
		t.Fatalf("WriteEvent old: %v", err)
	}
	if err := d.WriteSpawnOutcome(oldIID); err != nil {
		t.Fatalf("WriteSpawnOutcome old: %v", err)
	}

	// Filter to last 7 days — only the recent session should appear.
	sinceMs := now.Add(-7 * 24 * time.Hour).UnixMilli()
	out := captureStdout(t, func() {
		if err := runStatsGroupBy("profile", sinceMs); err != nil {
			t.Errorf("runStatsGroupBy: %v", err)
		}
	})

	if !strings.Contains(out, "new-profile") {
		t.Errorf("expected 'new-profile' in filtered output; got:\n%s", out)
	}
	if strings.Contains(out, "old-profile") {
		t.Errorf("unexpected 'old-profile' in filtered output (should be excluded by sinceMs); got:\n%s", out)
	}
}

// TestRunStatsGroupBy_InvalidAxis verifies that unknown axis values produce a
// user-friendly error message listing valid axes and do not panic.
func TestRunStatsGroupBy_InvalidAxis(t *testing.T) {
	openStatsTestDB(t)

	err := runStatsGroupBy("invalid-axis", 0)
	if err == nil {
		t.Fatal("expected error for invalid axis, got nil")
	}

	msg := err.Error()
	for _, axis := range []string{"harness", "profile", "variant", "model"} {
		if !strings.Contains(msg, axis) {
			t.Errorf("error message missing valid axis %q; got: %s", axis, msg)
		}
	}
}
