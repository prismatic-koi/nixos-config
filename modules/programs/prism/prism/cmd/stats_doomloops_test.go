package cmd

// Tests for `prism stats --doomloops` (runStatsDoomLoops).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// writeDoomLoopEvent writes a doom_loop_detected event to the given DB.
func writeDoomLoopEvent(t *testing.T, d *db.DB, session, tool, pattern string, count int, ts time.Time) {
	t.Helper()
	payload := fmt.Sprintf(`{"tool":%q,"pattern":%q,"count":%d}`, tool, pattern, count)
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "testrepo",
		Worktree:    "/code/testrepo/main",
		Type:        "doom_loop_detected",
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
}

// TestRunStatsDoomLoops_EmptyWindow verifies graceful output when no doom loop
// events exist in the window.
func TestRunStatsDoomLoops_EmptyWindow(t *testing.T) {
	_ = openStatsTestDB(t)

	out := captureStdout(t, func() {
		if err := runStatsDoomLoops("", 7); err != nil {
			t.Errorf("runStatsDoomLoops: %v", err)
		}
	})

	if !strings.Contains(out, "no doom_loop_detected events") {
		t.Errorf("expected 'no doom_loop_detected events', got:\n%s", out)
	}
}

// TestRunStatsDoomLoops_BasicOutput verifies that events within the window are
// rendered with the expected columns.
func TestRunStatsDoomLoops_BasicOutput(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "go test ./...", 5, base.Add(-1*time.Hour))
	writeDoomLoopEvent(t, d, "testrepo@feature", "edit", "/workspace/foo.go", 5, base.Add(-2*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsDoomLoops("", 7); err != nil {
			t.Errorf("runStatsDoomLoops: %v", err)
		}
	})

	// Headers must appear.
	for _, want := range []string{"SESSION", "TOOL", "ARG PATTERN", "COUNT", "TIMESTAMP"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing column %q\ngot:\n%s", want, out)
		}
	}
	// Both sessions should appear.
	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("output missing 'testrepo@main'\ngot:\n%s", out)
	}
	if !strings.Contains(out, "testrepo@feature") {
		t.Errorf("output missing 'testrepo@feature'\ngot:\n%s", out)
	}
	// Tools should appear.
	if !strings.Contains(out, "bash") {
		t.Errorf("output missing 'bash' tool\ngot:\n%s", out)
	}
	if !strings.Contains(out, "edit") {
		t.Errorf("output missing 'edit' tool\ngot:\n%s", out)
	}
}

// TestRunStatsDoomLoops_WindowFilter verifies that events older than the window
// are excluded.
func TestRunStatsDoomLoops_WindowFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Within 7-day window.
	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "go test ./...", 5, base.Add(-3*24*time.Hour))
	// Outside 7-day window (8 days ago).
	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "go build ./...", 5, base.Add(-8*24*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsDoomLoops("", 7); err != nil {
			t.Errorf("runStatsDoomLoops: %v", err)
		}
	})

	// Only the within-window event should appear.
	if !strings.Contains(out, "go test") {
		t.Errorf("output should contain 'go test' (within window)\ngot:\n%s", out)
	}
	// The old event should NOT appear.
	if strings.Contains(out, "go build") {
		t.Errorf("output must NOT contain 'go build' (outside window)\ngot:\n%s", out)
	}
}

// TestRunStatsDoomLoops_SessionFilter verifies that --doomloops with a session
// filter returns only that session's events.
func TestRunStatsDoomLoops_SessionFilter(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "go test ./...", 5, base.Add(-1*time.Hour))
	writeDoomLoopEvent(t, d, "testrepo@feature", "edit", "/workspace/foo.go", 5, base.Add(-2*time.Hour))

	out := captureStdout(t, func() {
		if err := runStatsDoomLoops("testrepo@main", 7); err != nil {
			t.Errorf("runStatsDoomLoops: %v", err)
		}
	})

	if !strings.Contains(out, "testrepo@main") {
		t.Errorf("output should contain 'testrepo@main'\ngot:\n%s", out)
	}
	if strings.Contains(out, "testrepo@feature") {
		t.Errorf("output must NOT contain 'testrepo@feature' (different session)\ngot:\n%s", out)
	}
}

// TestRunStatsDoomLoops_DaysFlag verifies that the window parameter limits
// results correctly.
func TestRunStatsDoomLoops_DaysFlag(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	// 3 days ago — within 30-day window, outside 2-day window.
	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "git log", 5, base.Add(-3*24*time.Hour))

	// Should appear with 30-day window.
	out30 := captureStdout(t, func() {
		if err := runStatsDoomLoops("", 30); err != nil {
			t.Errorf("runStatsDoomLoops(30): %v", err)
		}
	})
	if !strings.Contains(out30, "testrepo@main") {
		t.Errorf("30-day window should include event from 3 days ago\ngot:\n%s", out30)
	}

	// Should NOT appear with 2-day window.
	out2 := captureStdout(t, func() {
		if err := runStatsDoomLoops("", 2); err != nil {
			t.Errorf("runStatsDoomLoops(2): %v", err)
		}
	})
	if strings.Contains(out2, "testrepo@main") {
		t.Errorf("2-day window should exclude event from 3 days ago\ngot:\n%s", out2)
	}
}

// TestRunStats_DoomloopsFlag verifies that runStats routes to runStatsDoomLoops
// when --doomloops is set (default 7-day window, no session filter).
func TestRunStats_DoomloopsFlag(t *testing.T) {
	d := openStatsTestDB(t)
	base := time.Now().Truncate(time.Second)

	writeDoomLoopEvent(t, d, "testrepo@main", "bash", "git status", 5, base.Add(-1*time.Hour))

	statsCmd.Flags().Set("doomloops", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "Doom Loop Events") {
		t.Errorf("output missing 'Doom Loop Events' heading\ngot:\n%s", out)
	}
	if !strings.Contains(out, "git status") {
		t.Errorf("output missing event 'git status'\ngot:\n%s", out)
	}
}

// TestRunStats_DoomloopsDaysMutuallyAccepted verifies that --doomloops with
// --days does NOT return an error (unlike the --days + session name combination).
func TestRunStats_DoomloopsDaysMutuallyAccepted(t *testing.T) {
	_ = openStatsTestDB(t)

	statsCmd.Flags().Set("doomloops", "true") //nolint:errcheck
	statsCmd.Flags().Set("days", "30")        //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck
		statsCmd.Flags().Set("days", "0")          //nolint:errcheck
	}()

	if err := runStats(statsCmd, nil); err != nil {
		t.Errorf("runStats --doomloops --days 30 returned error: %v", err)
	}
}
