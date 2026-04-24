package cmd

// Tests for the per-incarnation stats rework (issue #999):
//   - prism stats (no args) → runStatsIncarnations
//   - prism stats <instance-id|session-name> → runStatsDetail / resolveSessionArg
//   - prism stats --repo / --since filtering
//   - prism stats --aggregate-by-name flag removed (produces unknown-flag error)
//   - prism stats --since with unparseable date

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// insertTestSession inserts a sessions row directly via InsertSession + UpdateSessionEnded.
// startedAt is required; endedAt may be zero (live session).
func insertTestSession(t *testing.T, d *db.DB, instanceID, sessionName, repo, worktree, harness string,
	startedAt time.Time, endedAt time.Time, endState string, archivePath string) {
	t.Helper()
	s := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    worktree,
		Harness:     harness,
		StartedAt:   startedAt,
	}
	agentRole := "coordinator"
	s.AgentRole = &agentRole
	rootAgent := "coordinator"
	s.RootAgentName = &rootAgent
	if err := d.InsertSession(s); err != nil {
		t.Fatalf("InsertSession(%s): %v", instanceID, err)
	}
	if !endedAt.IsZero() {
		// UpdateSessionEnded uses time.Now() internally; we need to test with custom times.
		// For tests we call the DB method directly which sets ended_at = now.
		// Instead we use a direct SQL approach via the QueryRow accessor.
		// The DB exposes QueryRow for tests — use it to set ended_at directly.
		d.QueryRow(`UPDATE sessions SET ended_at = ?, end_state = ?, archive_path = ? WHERE instance_id = ?`,
			endedAt.UnixMilli(), endState, archivePath, instanceID)
	}
}

// writeSessionEvent writes a msg_assistant event linked to the given instance_id.
func writeSessionEvent(t *testing.T, d *db.DB, instanceID, sessionName, model string,
	inputTokens, outputTokens int, ts time.Time) {
	t.Helper()
	payload := fmt.Sprintf(`{"messageId":"msg-%s","text":"reply","agent":"coordinator","model":%q,"inputTokens":%d,"outputTokens":%d,"durationMs":5000}`,
		uuid.New().String()[:8], model, inputTokens, outputTokens)
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "msg_assistant",
		Payload:     payload,
		CreatedAt:   ts,
		InstanceID:  &instanceID,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// openIncarnationTestDB opens a temp DB and registers cleanup. Also sets testDBPath.
func openIncarnationTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	return d
}

// --- prism stats (no args): per-incarnation table ---

// TestRunStatsIncarnations_EmptyTable verifies graceful output for empty sessions table.
func TestRunStatsIncarnations_EmptyTable(t *testing.T) {
	_ = openIncarnationTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	if !strings.Contains(out, "no sessions yet") {
		t.Errorf("expected 'no sessions yet' for empty table\ngot:\n%s", out)
	}
}

// TestRunStatsIncarnations_OneRow verifies a single incarnation row is shown.
func TestRunStatsIncarnations_OneRow(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	// Should show the short instance_id (first 8 chars).
	if !strings.Contains(out, iid[:8]) {
		t.Errorf("output missing short instance_id %q\ngot:\n%s", iid[:8], out)
	}
	// Should show session name.
	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("output missing session name 'testrepo@main'\ngot:\n%s", out)
	}
	// Should show column headers.
	if !strings.Contains(out, "INSTANCE") {
		t.Errorf("output missing 'INSTANCE' column header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "SESSION_NAME") {
		t.Errorf("output missing 'SESSION_NAME' column header\ngot:\n%s", out)
	}
	if !strings.Contains(out, "DURATION") {
		t.Errorf("output missing 'DURATION' column header\ngot:\n%s", out)
	}
}

// TestRunStatsIncarnations_ActiveSession verifies that live sessions (ended_at IS NULL)
// show STATE=active.
func TestRunStatsIncarnations_ActiveSession(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	// No end time — live session.
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-30*time.Minute), time.Time{}, "", "")

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	if !strings.Contains(out, "active") {
		t.Errorf("expected 'active' state for live session\ngot:\n%s", out)
	}
}

// TestRunStatsIncarnations_TokensAndCost verifies token/cost totals are shown
// when agent_events are linked to the instance_id.
func TestRunStatsIncarnations_TokensAndCost(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	// 100K input at $3/M = $0.30, 10K output at $15/M = $0.15 → ~$0.45
	writeSessionEvent(t, d, iid, "testrepo@main", "anthropic/claude-sonnet-4-6",
		100000, 10000, base.Add(-30*time.Minute))

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	// 110K total tokens.
	if !strings.Contains(out, "110K") {
		t.Errorf("expected '110K' total tokens\ngot:\n%s", out)
	}
}

// TestRunStatsIncarnations_RepoFilter verifies --repo filtering.
func TestRunStatsIncarnations_RepoFilter(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	insertTestSession(t, d, iid1, "repo-a@main", "repo-a", "/code/repo-a/main", "opencode",
		base.Add(-2*time.Hour), base, "finished", "")
	insertTestSession(t, d, iid2, "repo-b@main", "repo-b", "/code/repo-b/main", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("repo-a", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	if !strings.Contains(out, "repo-a@main") {
		t.Errorf("expected 'repo-a@main' in filtered output\ngot:\n%s", out)
	}
	if strings.Contains(out, "repo-b@main") {
		t.Errorf("expected 'repo-b@main' to be filtered out\ngot:\n%s", out)
	}
}

// TestRunStatsIncarnations_SinceFilter verifies --since filtering.
func TestRunStatsIncarnations_SinceFilter(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iidOld := uuid.New().String()
	iidNew := uuid.New().String()
	// Old session: started 30 days ago.
	insertTestSession(t, d, iidOld, "repo@old", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-30*24*time.Hour), base.Add(-29*24*time.Hour), "finished", "")
	// New session: started 1 hour ago.
	insertTestSession(t, d, iidNew, "repo@new", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	// Filter to the last 7 days.
	sinceMs := base.Add(-7 * 24 * time.Hour).UnixMilli()

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", sinceMs); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	if strings.Contains(out, "repo@old") {
		t.Errorf("expected old session to be filtered out\ngot:\n%s", out)
	}
	if !strings.Contains(out, "repo@new") {
		t.Errorf("expected new session to be shown\ngot:\n%s", out)
	}
}

// --- prism stats <arg>: detail view ---

// TestRunStatsDetail_ByFullUUID verifies detail view with a full 36-char UUID.
func TestRunStatsDetail_ByFullUUID(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code/testrepo/main", "opencode",
		base.Add(-1*time.Hour), base, "finished", "/archive/path")

	out := captureStdout(t, func() {
		if err := runStatsDetail(iid, false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})

	if !strings.Contains(out, iid) {
		t.Errorf("expected full instance_id %q in output\ngot:\n%s", iid, out)
	}
	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("expected session name 'testrepo@main'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "/archive/path") {
		t.Errorf("expected archive path '/archive/path'\ngot:\n%s", out)
	}
}

// TestRunStatsDetail_BySessionName verifies detail view with a session name.
func TestRunStatsDetail_BySessionName(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two incarnations of the same session name; most recent should be returned.
	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	insertTestSession(t, d, iid1, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-3*time.Hour), base.Add(-2*time.Hour), "finished", "")
	insertTestSession(t, d, iid2, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		if err := runStatsDetail("testrepo@main", false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})

	// Should show the most recent incarnation (iid2).
	if !strings.Contains(out, iid2) {
		t.Errorf("expected most recent instance_id %q\ngot:\n%s", iid2, out)
	}
}

// TestRunStatsDetail_ByUUIDPrefix verifies detail view with an unambiguous UUID prefix.
func TestRunStatsDetail_ByUUIDPrefix(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := "aaaabbbb-cccc-dddd-eeee-ffffffffffff"
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		// Use first 8 chars as prefix.
		if err := runStatsDetail("aaaabbbb", false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})

	if !strings.Contains(out, iid) {
		t.Errorf("expected instance_id %q in output from prefix lookup\ngot:\n%s", iid, out)
	}
}

// TestRunStatsDetail_AmbiguousPrefix verifies that an ambiguous UUID prefix returns an error.
func TestRunStatsDetail_AmbiguousPrefix(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two instance IDs sharing the same first 8 chars.
	iid1 := "aaaabbbb-0000-0000-0000-000000000001"
	iid2 := "aaaabbbb-0000-0000-0000-000000000002"
	insertTestSession(t, d, iid1, "s1@main", "r1", "/code/r1", "opencode",
		base.Add(-2*time.Hour), base, "finished", "")
	insertTestSession(t, d, iid2, "s2@main", "r2", "/code/r2", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	err := runStatsDetail("aaaabbbb", false)
	if err == nil {
		t.Fatal("expected error for ambiguous prefix, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error\ngot: %v", err)
	}
}

// TestRunStatsDetail_Unknown verifies that an unknown arg returns a clear error.
func TestRunStatsDetail_Unknown(t *testing.T) {
	_ = openIncarnationTestDB(t)

	err := runStatsDetail("totally-unknown-session", false)
	if err == nil {
		t.Fatal("expected error for unknown arg, got nil")
	}
}

// TestRunStatsDetail_NotYetArchived verifies "(not yet archived)" output when
// archive_path IS NULL.
func TestRunStatsDetail_NotYetArchived(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	// No archive path.
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	out := captureStdout(t, func() {
		if err := runStatsDetail(iid, false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})

	if !strings.Contains(out, "not yet archived") {
		t.Errorf("expected 'not yet archived' when archive_path IS NULL\ngot:\n%s", out)
	}
}

// --- --aggregate-by-name flag: must be removed ---

// TestRunStats_AggregateByNameFlagRemoved verifies that --aggregate-by-name is NOT
// registered on statsCmd (flag is explicitly removed per issue #999).
func TestRunStats_AggregateByNameFlagRemoved(t *testing.T) {
	f := statsCmd.Flags().Lookup("aggregate-by-name")
	if f != nil {
		t.Error("statsCmd should NOT have an --aggregate-by-name flag (it was removed), but it is still registered")
	}
}

// TestRunStats_AggregateByNameProducesError verifies that passing --aggregate-by-name
// to the cobra root command produces an error (unknown flag).
func TestRunStats_AggregateByNameProducesError(t *testing.T) {
	_ = openIncarnationTestDB(t)

	// Execute with the flag through cobra — should return an unknown flag error.
	rootCmd.SetArgs([]string{"stats", "--aggregate-by-name"})
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil) // reset
	if err == nil {
		t.Fatal("expected error for unknown flag --aggregate-by-name, got nil")
	}
}

// --- --since flag parsing ---

// TestParseSinceFlag_ValidDate verifies ISO 8601 date parsing.
func TestParseSinceFlag_ValidDate(t *testing.T) {
	cases := []struct {
		input string
		valid bool
	}{
		{"2026-04-01", true},
		{"2026-04-01T00:00:00Z", true},
		{"2026-04-01T15:04:05Z", true},
		{"", true},                          // empty is valid (means no filter)
		{"not-a-date", false},
		{"2026/04/01", false},
	}

	for _, tc := range cases {
		ms, err := parseSinceFlag(tc.input)
		if tc.valid {
			if err != nil {
				t.Errorf("parseSinceFlag(%q) returned unexpected error: %v", tc.input, err)
			}
			if tc.input == "" && ms != 0 {
				t.Errorf("parseSinceFlag(\"\") should return 0, got %d", ms)
			}
		} else {
			if err == nil {
				t.Errorf("parseSinceFlag(%q) should have returned an error", tc.input)
			}
		}
	}
}

// TestRunStats_SinceUnparseable verifies that a bad --since value produces an error.
func TestRunStats_SinceUnparseable(t *testing.T) {
	_ = openIncarnationTestDB(t)

	statsCmd.Flags().Set("since", "not-a-date") //nolint:errcheck
	defer statsCmd.Flags().Set("since", "")     //nolint:errcheck

	err := runStats(statsCmd, nil)
	if err == nil {
		t.Fatal("expected error for unparseable --since, got nil")
	}
	if !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("expected 'cannot parse' in error\ngot: %v", err)
	}
}

// --- token/cost aggregation: NULL instance_id excluded ---

// TestRunStatsIncarnations_NullInstanceIDExcluded verifies that events with
// NULL instance_id (pre-migration) are NOT counted in incarnation totals.
func TestRunStatsIncarnations_NullInstanceIDExcluded(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	// Write an event with NULL instance_id (legacy — should NOT be counted).
	legacyPayload := `{"messageId":"msg-legacy","text":"old reply","agent":"coordinator","model":"anthropic/claude-sonnet-4-6","inputTokens":500000,"outputTokens":100000,"durationMs":5000}`
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: "testrepo@main",
		Repo:        "testrepo",
		Worktree:    "/code",
		Type:        "msg_assistant",
		Payload:     legacyPayload,
		CreatedAt:   base.Add(-45 * time.Minute),
		InstanceID:  nil, // NULL — should NOT be counted
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsIncarnations("", 0); err != nil {
			t.Errorf("runStatsIncarnations: %v", err)
		}
	})

	// 500K+100K = 600K tokens — should NOT appear (NULL instance_id excluded).
	if strings.Contains(out, "600K") {
		t.Errorf("NULL instance_id events should be excluded from incarnation totals\ngot:\n%s", out)
	}
	// Token column should show "—" (no events with this instance_id).
	if !strings.Contains(out, "—") {
		t.Errorf("expected '—' for tokens when no instance-linked events\ngot:\n%s", out)
	}
}
