package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestFormatMergesRows_PosIsOneBasedRank verifies that the POS column shows
// the row's 1-based rank in the input slice — not the raw queue_position
// timestamp. This is the core fix for the "POS shows truncated Unix-ms
// timestamp like 17771" bug.
func TestFormatMergesRows_PosIsOneBasedRank(t *testing.T) {
	now := time.Date(2025, 4, 26, 12, 0, 0, 0, time.UTC)
	queuedAt := now.Add(-3 * time.Minute)

	// Three rows whose queue_position values are realistic Unix-ms timestamps.
	// Pre-fix, the POS column would render the leading 5 digits of each
	// timestamp ("17771", "17771", "17771"). Post-fix it must render
	// "1", "2", "3" — the rank in the FIFO-sorted slice.
	merges := []db.PendingMerge{
		{PR: 1046, Status: "watching", QueuePosition: 1777154720435, Title: strPtr("first"), QueuedAt: queuedAt},
		{PR: 1047, Status: "watching", QueuePosition: 1777154720436, Title: strPtr("second"), QueuedAt: queuedAt},
		{PR: 1048, Status: "watching", QueuePosition: 1777154720437, Title: strPtr("third"), QueuedAt: queuedAt},
	}

	_, rows := formatMergesRows(merges, now)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	// Each row must start with its 1-based rank, padded to mergesWPos columns.
	wantPrefixes := []string{"1    ", "2    ", "3    "}
	for i, row := range rows {
		if !strings.HasPrefix(row, wantPrefixes[i]) {
			t.Errorf("row %d: expected POS prefix %q, got row %q", i, wantPrefixes[i], row)
		}
		// Sanity: the raw timestamp must NOT appear anywhere in the row.
		if strings.Contains(row, "17771") {
			t.Errorf("row %d: raw queue_position timestamp leaked into output: %q", i, row)
		}
	}
}

// TestFormatMergesRows_RankPerFilteredView verifies that the rank reflects
// display order within a filtered view, not the storage ordering key. AC-2:
// `prism merges list --failed`, `--abandoned`, `--all` all use the same
// renderer, so a single test on the renderer covers all of them.
func TestFormatMergesRows_RankPerFilteredView(t *testing.T) {
	now := time.Date(2025, 4, 26, 12, 0, 0, 0, time.UTC)
	queuedAt := now.Add(-1 * time.Hour)

	// Simulate a `--failed` filter result: two failed rows with non-contiguous
	// queue_position timestamps. Display rank should still be 1, 2.
	merges := []db.PendingMerge{
		{PR: 900, Status: "failed", QueuePosition: 1777000000000, Title: strPtr("a"), QueuedAt: queuedAt, Error: strPtr("boom")},
		{PR: 950, Status: "failed", QueuePosition: 1777200000000, Title: strPtr("b"), QueuedAt: queuedAt, Error: strPtr("kaboom")},
	}

	_, rows := formatMergesRows(merges, now)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if !strings.HasPrefix(rows[0], "1    ") {
		t.Errorf("filtered row 0: expected POS=1, got %q", rows[0])
	}
	if !strings.HasPrefix(rows[1], "2    ") {
		t.Errorf("filtered row 1: expected POS=2, got %q", rows[1])
	}
}

// TestFormatMergesRows_HeaderIncludesPos sanity-checks that the header row
// still labels the column "POS".
func TestFormatMergesRows_HeaderIncludesPos(t *testing.T) {
	header, _ := formatMergesRows(nil, time.Now())
	if !strings.HasPrefix(header, "POS") {
		t.Errorf("expected header to start with POS, got %q", header)
	}
}

// TestEmptyMergesMessage covers the empty-state messages for each filter so
// the refactor (extracting emptyMergesMessage from renderMergesList) is
// observably equivalent to the previous inline switch.
func TestEmptyMergesMessage(t *testing.T) {
	cases := map[string]string{
		"":          "merge queue is empty",
		"failed":    "no failed merge queue entries",
		"abandoned": "no abandoned merge queue entries from previous coordinator sessions",
		"all":       "no merge queue entries in the last 7 days",
	}
	for filter, want := range cases {
		got := emptyMergesMessage(filter)
		if got != want {
			t.Errorf("emptyMergesMessage(%q) = %q, want %q", filter, got, want)
		}
	}
}
