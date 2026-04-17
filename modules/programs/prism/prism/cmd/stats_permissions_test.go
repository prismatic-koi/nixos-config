package cmd

// Tests for `prism stats --denials` (runStatsDenials) and
// `prism stats --asks` (runStatsAsks).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// writeDenialEvent writes a permission_denied event to the given DB.
func writeDenialEvent(t *testing.T, d *db.DB, session, tool string, ts time.Time) {
	t.Helper()
	p := fmt.Sprintf(`{"tool":%q,"messageId":"msg-%s"}`, tool, uuid.New().String())
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "permission_denied",
		Payload:     p,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// writeAskEvent writes a permission_ask event to the given DB.
// patterns is the list of patterns to include in the payload.
func writeAskEvent(t *testing.T, d *db.DB, session, tool string, patterns []string, ts time.Time) {
	t.Helper()
	// Build patterns JSON array.
	patJSON := "[]"
	if len(patterns) > 0 {
		parts := make([]string, len(patterns))
		for i, pat := range patterns {
			parts[i] = fmt.Sprintf("%q", pat)
		}
		patJSON = "[" + strings.Join(parts, ",") + "]"
	}
	p := fmt.Sprintf(`{"tool":%q,"patterns":%s,"messageId":"msg-%s"}`, tool, patJSON, uuid.New().String())
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "permission_ask",
		Payload:     p,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// writeAskEventLegacyTool writes a permission_ask event where the tool field
// is a JSON object (legacy format), not a plain string.
func writeAskEventLegacyTool(t *testing.T, d *db.DB, session string, patterns []string, ts time.Time) {
	t.Helper()
	patJSON := "[]"
	if len(patterns) > 0 {
		parts := make([]string, len(patterns))
		for i, pat := range patterns {
			parts[i] = fmt.Sprintf("%q", pat)
		}
		patJSON = "[" + strings.Join(parts, ",") + "]"
	}
	// Legacy tool: JSON object instead of plain string.
	p := fmt.Sprintf(`{"tool":{"messageID":"msg-legacy","callID":"call-123"},"patterns":%s,"messageId":"msg-legacy"}`, patJSON)
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "permission_ask",
		Payload:     p,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// --- runStatsDenials tests ---

// TestRunStatsDenials_EmptyWindow verifies graceful output when no
// permission_denied events exist in the window.
func TestRunStatsDenials_EmptyWindow(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsDenials("", 7); err != nil {
			t.Errorf("runStatsDenials: %v", err)
		}
	})

	if !strings.Contains(out, "No permission denials") {
		t.Errorf("expected empty-state message, got:\n%s", out)
	}
}

// TestRunStatsDenials_AggregationCorrectness verifies that counts are summed
// correctly by (session, tool) and the table is sorted by count desc.
func TestRunStatsDenials_AggregationCorrectness(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// session A: bash denied 3 times, go-test denied 1 time
	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-5*time.Hour))
	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-4*time.Hour))
	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-3*time.Hour))
	writeDenialEvent(t, d, "repo@main", "go-test", base.Add(-2*time.Hour))

	// session B: kubectl denied 2 times
	writeDenialEvent(t, d, "repo@feature", "kubectl", base.Add(-1*time.Hour))
	writeDenialEvent(t, d, "repo@feature", "kubectl", base.Add(-30*time.Minute))

	out := captureStdout(t, func() {
		if err := runStatsDenials("", 7); err != nil {
			t.Errorf("runStatsDenials: %v", err)
		}
	})

	// Headers must appear.
	for _, want := range []string{"SESSION", "TOOL", "COUNT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing column %q\ngot:\n%s", want, out)
		}
	}

	// Both sessions should appear.
	if !strings.Contains(out, "repo@main") {
		t.Errorf("output missing 'repo@main'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "repo@feature") {
		t.Errorf("output missing 'repo@feature'\ngot:\n%s", out)
	}

	// Tools should appear.
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "kubectl") {
		t.Errorf("output missing 'kubectl'\ngot:\n%s", out)
	}

	// bash count=3 should appear before kubectl count=2.
	bashPos := strings.Index(out, "bash")
	kubectlPos := strings.Index(out, "kubectl")
	if bashPos == -1 || kubectlPos == -1 {
		t.Fatalf("could not find 'bash' or 'kubectl' in output:\n%s", out)
	}
	if bashPos > kubectlPos {
		t.Errorf("expected 'bash' (count=3) before 'kubectl' (count=2) in sorted output\ngot:\n%s", out)
	}

	// Count 3 for bash should appear.
	if !strings.Contains(out, "3") {
		t.Errorf("output missing count '3' for bash\ngot:\n%s", out)
	}
}

// TestRunStatsDenials_SessionFilter verifies that a session filter restricts
// results to the named session only.
func TestRunStatsDenials_SessionFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-1*time.Hour))
	writeDenialEvent(t, d, "repo@feature", "kubectl", base.Add(-2*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsDenials("repo@main", 7); err != nil {
			t.Errorf("runStatsDenials: %v", err)
		}
	})

	if !strings.Contains(out, "repo@main") {
		t.Errorf("output should contain 'repo@main'\ngot:\n%s", out)
	}
	if strings.Contains(out, "repo@feature") {
		t.Errorf("output must NOT contain 'repo@feature' (different session)\ngot:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("output should contain 'bash'\ngot:\n%s", out)
	}
}

// TestRunStatsDenials_WindowFilter verifies that events older than the window
// are excluded.
func TestRunStatsDenials_WindowFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Within window (3 days ago, window=7).
	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-3*24*time.Hour))
	// Outside window (8 days ago, window=7).
	writeDenialEvent(t, d, "repo@main", "kubectl", base.Add(-8*24*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsDenials("", 7); err != nil {
			t.Errorf("runStatsDenials: %v", err)
		}
	})

	if !strings.Contains(out, "bash") {
		t.Errorf("output should contain 'bash' (within window)\ngot:\n%s", out)
	}
	if strings.Contains(out, "kubectl") {
		t.Errorf("output must NOT contain 'kubectl' (outside window)\ngot:\n%s", out)
	}
}

// TestRunStatsDenials_MissingSession verifies that an unknown session returns
// a non-zero error referencing the session name.
func TestRunStatsDenials_MissingSession(t *testing.T) {
	_ = openStatsTestDB(t)

	err := runStatsDenials("nonexistent@main", 7)
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent@main") {
		t.Errorf("error should mention the session name, got: %v", err)
	}
}

// TestRunStats_DenialsFlag verifies that runStats routes to runStatsDenials
// when --denials is set.
func TestRunStats_DenialsFlag(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-1*time.Hour))

	statsCmd.Flags().Set("denials", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("denials", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "Permission Denials") {
		t.Errorf("output missing 'Permission Denials' heading\ngot:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash'\ngot:\n%s", out)
	}
}

// TestRunStats_DenialsDaysMutuallyAccepted verifies that --denials with --days
// does NOT return an error.
func TestRunStats_DenialsDaysMutuallyAccepted(t *testing.T) {
	_ = openStatsTestDB(t)

	statsCmd.Flags().Set("denials", "true") //nolint:errcheck
	statsCmd.Flags().Set("days", "30")      //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("denials", "false") //nolint:errcheck
		statsCmd.Flags().Set("days", "0")        //nolint:errcheck
	}()

	if err := runStats(statsCmd, nil); err != nil {
		t.Errorf("runStats --denials --days 30 returned error: %v", err)
	}
}

// --- runStatsAsks tests ---

// TestRunStatsAsks_EmptyWindow verifies graceful output when no
// permission_ask events exist in the window.
func TestRunStatsAsks_EmptyWindow(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	if !strings.Contains(out, "No permission asks") {
		t.Errorf("expected empty-state message, got:\n%s", out)
	}
}

// TestRunStatsAsks_AggregationCorrectness verifies that counts are aggregated
// correctly by (session, tool, pattern) and sorted by count desc.
func TestRunStatsAsks_AggregationCorrectness(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// bash asked 3 times with pattern "atlasian_create*"
	writeAskEvent(t, d, "repo@main", "bash", []string{"atlasian_create*"}, base.Add(-5*time.Hour))
	writeAskEvent(t, d, "repo@main", "bash", []string{"atlasian_create*"}, base.Add(-4*time.Hour))
	writeAskEvent(t, d, "repo@main", "bash", []string{"atlasian_create*"}, base.Add(-3*time.Hour))

	// webfetch asked 2 times with no pattern
	writeAskEvent(t, d, "repo@feature", "webfetch", []string{}, base.Add(-2*time.Hour))
	writeAskEvent(t, d, "repo@feature", "webfetch", []string{}, base.Add(-1*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	// Headers must appear.
	for _, want := range []string{"SESSION", "TOOL", "PATTERN", "COUNT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing column %q\ngot:\n%s", want, out)
		}
	}

	// Both sessions should appear.
	if !strings.Contains(out, "repo@main") {
		t.Errorf("output missing 'repo@main'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "repo@feature") {
		t.Errorf("output missing 'repo@feature'\ngot:\n%s", out)
	}

	// Tools should appear.
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "webfetch") {
		t.Errorf("output missing 'webfetch'\ngot:\n%s", out)
	}

	// Pattern should appear.
	if !strings.Contains(out, "atlasian_create*") {
		t.Errorf("output missing 'atlasian_create*'\ngot:\n%s", out)
	}

	// Empty-patterns should show <no pattern>.
	if !strings.Contains(out, "<no pattern>") {
		t.Errorf("output missing '<no pattern>' for empty patterns\ngot:\n%s", out)
	}

	// bash (count=3) should appear before webfetch (count=2).
	bashPos := strings.Index(out, "bash")
	webfetchPos := strings.Index(out, "webfetch")
	if bashPos == -1 || webfetchPos == -1 {
		t.Fatalf("could not find 'bash' or 'webfetch' in output:\n%s", out)
	}
	if bashPos > webfetchPos {
		t.Errorf("expected 'bash' (count=3) before 'webfetch' (count=2)\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_MultiplePatternsSplitRows verifies that a single event with
// multiple patterns produces one row per pattern (not one combined row).
func TestRunStatsAsks_MultiplePatternsSplitRows(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// One event with two patterns → two separate aggregation rows.
	writeAskEvent(t, d, "repo@main", "bash", []string{"git push*", "gh pr*"}, base)

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	if !strings.Contains(out, "git push*") {
		t.Errorf("output missing 'git push*'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "gh pr*") {
		t.Errorf("output missing 'gh pr*'\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_NullPatterns verifies that events with an empty patterns
// slice render as <no pattern>, not as a blank or panic.
func TestRunStatsAsks_NullPatterns(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Null/empty patterns.
	writeAskEvent(t, d, "repo@main", "webfetch", []string{}, base)

	// Also test explicit null JSON (no patterns key).
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: "repo@main",
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "permission_ask",
		Payload:     `{"tool":"bash","messageId":"msg-null"}`, // no patterns key → nil slice
		CreatedAt:   base.Add(time.Second),
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	if !strings.Contains(out, "<no pattern>") {
		t.Errorf("output missing '<no pattern>' for empty/null patterns\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_LegacyToolObject verifies that events with a JSON object in
// the tool field (legacy format) render as <unknown> without error.
func TestRunStatsAsks_LegacyToolObject(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeAskEventLegacyTool(t, d, "repo@main", []string{"some-pattern"}, base)

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	// Should not crash and should show <unknown> for the tool.
	if !strings.Contains(out, "<unknown>") {
		t.Errorf("output missing '<unknown>' for legacy tool object\ngot:\n%s", out)
	}
	// The pattern should still appear.
	if !strings.Contains(out, "some-pattern") {
		t.Errorf("output missing 'some-pattern'\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_SessionFilter verifies that a session filter restricts
// results to the named session only.
func TestRunStatsAsks_SessionFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	if err := d.UpsertStatus("repo@main", "repo", "/code/repo/main", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeAskEvent(t, d, "repo@main", "bash", []string{"git*"}, base.Add(-1*time.Hour))
	writeAskEvent(t, d, "repo@feature", "kubectl", []string{"apply*"}, base.Add(-2*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsAsks("repo@main", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	if !strings.Contains(out, "repo@main") {
		t.Errorf("output should contain 'repo@main'\ngot:\n%s", out)
	}
	if strings.Contains(out, "repo@feature") {
		t.Errorf("output must NOT contain 'repo@feature' (different session)\ngot:\n%s", out)
	}
	if !strings.Contains(out, "git*") {
		t.Errorf("output should contain 'git*'\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_WindowFilter verifies that events older than the window
// are excluded.
func TestRunStatsAsks_WindowFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Within window (3 days ago, window=7).
	writeAskEvent(t, d, "repo@main", "bash", []string{"inside-window"}, base.Add(-3*24*time.Hour))
	// Outside window (8 days ago, window=7).
	writeAskEvent(t, d, "repo@main", "bash", []string{"outside-window"}, base.Add(-8*24*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsAsks("", 7); err != nil {
			t.Errorf("runStatsAsks: %v", err)
		}
	})

	if !strings.Contains(out, "inside-window") {
		t.Errorf("output should contain 'inside-window' (within window)\ngot:\n%s", out)
	}
	if strings.Contains(out, "outside-window") {
		t.Errorf("output must NOT contain 'outside-window' (outside window)\ngot:\n%s", out)
	}
}

// TestRunStatsAsks_MissingSession verifies that an unknown session returns a
// non-zero error referencing the session name.
func TestRunStatsAsks_MissingSession(t *testing.T) {
	_ = openStatsTestDB(t)

	err := runStatsAsks("nonexistent@main", 7)
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent@main") {
		t.Errorf("error should mention the session name, got: %v", err)
	}
}

// TestRunStats_AsksFlag verifies that runStats routes to runStatsAsks when
// --asks is set.
func TestRunStats_AsksFlag(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeAskEvent(t, d, "repo@main", "bash", []string{"git*"}, base.Add(-1*time.Hour))

	statsCmd.Flags().Set("asks", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("asks", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "Permission Asks") {
		t.Errorf("output missing 'Permission Asks' heading\ngot:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash'\ngot:\n%s", out)
	}
}

// TestRunStats_AsksDaysMutuallyAccepted verifies that --asks with --days does
// NOT return an error.
func TestRunStats_AsksDaysMutuallyAccepted(t *testing.T) {
	_ = openStatsTestDB(t)

	statsCmd.Flags().Set("asks", "true") //nolint:errcheck
	statsCmd.Flags().Set("days", "30")   //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("asks", "false") //nolint:errcheck
		statsCmd.Flags().Set("days", "0")     //nolint:errcheck
	}()

	if err := runStats(statsCmd, nil); err != nil {
		t.Errorf("runStats --asks --days 30 returned error: %v", err)
	}
}

// TestRunStats_DenialsAndAsks verifies that --denials and --asks together
// produce both datasets without error.
func TestRunStats_DenialsAndAsks(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeDenialEvent(t, d, "repo@main", "bash", base.Add(-1*time.Hour))
	writeAskEvent(t, d, "repo@main", "webfetch", []string{"https://*"}, base.Add(-2*time.Hour))

	statsCmd.Flags().Set("denials", "true") //nolint:errcheck
	statsCmd.Flags().Set("asks", "true")    //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("denials", "false") //nolint:errcheck
		statsCmd.Flags().Set("asks", "false")    //nolint:errcheck
	}()

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --denials --asks: %v", err)
		}
	})

	// Both sections should appear.
	if !strings.Contains(out, "Permission Denials") {
		t.Errorf("output missing 'Permission Denials' heading\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Permission Asks") {
		t.Errorf("output missing 'Permission Asks' heading\ngot:\n%s", out)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash' (from denials)\ngot:\n%s", out)
	}
	if !strings.Contains(out, "webfetch") {
		t.Errorf("output missing 'webfetch' (from asks)\ngot:\n%s", out)
	}
}

// TestRunStatsDenials_FlagRegistered verifies that --denials is a registered
// Bool flag on statsCmd and appears in --help output.
func TestRunStatsDenials_FlagRegistered(t *testing.T) {
	f := statsCmd.Flags().Lookup("denials")
	if f == nil {
		t.Fatal("statsCmd should have a --denials flag but it is not registered")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--denials flag should be Bool, got %q", f.Value.Type())
	}
}

// TestRunStatsAsks_FlagRegistered verifies that --asks is a registered Bool
// flag on statsCmd.
func TestRunStatsAsks_FlagRegistered(t *testing.T) {
	f := statsCmd.Flags().Lookup("asks")
	if f == nil {
		t.Fatal("statsCmd should have an --asks flag but it is not registered")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--asks flag should be Bool, got %q", f.Value.Type())
	}
}
