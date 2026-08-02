// Tests for `prism account usage` (issue #2539, parent #2537).
package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/usage"
)

// withUsageFixture redirects $XDG_STATE_HOME into a per-test tempdir and
// returns the usage directory (which does not yet exist — callers create it
// via usage.NewStore(dir).Write, or leave it absent to exercise the
// missing-directory path).
func withUsageFixture(t *testing.T) (dir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	return filepath.Join(root, "prism", "usage")
}

func f64u(v float64) *float64 { return &v }
func i64u(v int64) *int64     { return &v }

func writeUsageSnapshot(t *testing.T, dir string, snap usage.Snapshot) {
	t.Helper()
	if err := usage.NewStore(dir).Write(snap); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}

func TestAccountUsage_TextRendersPercentagesAndActiveMarker(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.94), Reset: i64u(time.Now().Add(2 * time.Hour).Unix())},
			SevenDay: &usage.Window{Utilization: f64u(0.42), Reset: i64u(time.Now().Add(96 * time.Hour).Unix())},
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "* work") {
		t.Errorf("expected active marker on work, got:\n%s", out)
	}
	if !strings.Contains(out, "94%") {
		t.Errorf("expected 94%% rendered from 0.94, got:\n%s", out)
	}
	if !strings.Contains(out, "42%") {
		t.Errorf("expected 42%% rendered from 0.42, got:\n%s", out)
	}
}

func TestAccountUsage_ResetInPastRendersNow(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.1), Reset: i64u(time.Now().Add(-1 * time.Hour).Unix())},
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "(now)") {
		t.Errorf("expected a past reset to render as \"now\", got:\n%s", out)
	}
}

func TestAccountUsage_StaleSnapshotMarked(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now().Add(-20 * time.Minute)),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.5), Reset: i64u(time.Now().Add(time.Hour).Unix())},
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "(stale)") {
		t.Errorf("expected (stale) marker, got:\n%s", out)
	}
}

// TestAccountUsage_PercentageRoundsNotTruncates guards against a
// floating-point truncation bug: 0.29*100 evaluates to
// 28.999999999999996 in float64, so int(x*100) yields 28, not 29.
// Percentages must round, not truncate.
func TestAccountUsage_PercentageRoundsNotTruncates(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.29), Reset: i64u(time.Now().Add(time.Hour).Unix())},
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "29%") {
		t.Errorf("expected 0.29 to render as 29%%, got:\n%s", out)
	}
	if strings.Contains(out, "28%") {
		t.Errorf("0.29 must not truncate to 28%%, got:\n%s", out)
	}

	c := &cobra.Command{}
	c.Flags().Bool("json", true, "")
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	captured := captureStdout(t, func() {
		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage --json: %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(captured)), &rows); err != nil {
		t.Fatalf("parse json: %v (raw: %q)", err, captured)
	}
	fiveHour, _ := rows[0]["five_hour"].(map[string]any)
	if fiveHour["percent_used"] != float64(29) {
		t.Errorf("percent_used = %v, want 29", fiveHour["percent_used"])
	}
}

func TestAccountUsage_MissingWindowPrintsNoData(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.5), Reset: i64u(time.Now().Add(time.Hour).Unix())},
			// SevenDay omitted entirely.
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "no data") {
		t.Errorf("expected \"no data\" for the missing seven_day window, got:\n%s", out)
	}
}

func TestAccountUsage_MissingUsageDirPrintsMessageAndExitsZero(t *testing.T) {
	dir := withUsageFixture(t)

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage should exit 0 for a missing dir, got err: %v", err)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("expected the missing directory path %q named in output, got:\n%s", dir, out)
	}
}

func TestAccountUsage_MalformedSnapshotStillPrintsOtherAccounts(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.5), Reset: i64u(time.Now().Add(time.Hour).Unix())},
		},
	})
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken.json: %v", err)
	}

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if !strings.Contains(out, "broken.json") {
		t.Errorf("expected the malformed filename named in output, got:\n%s", out)
	}
	if !strings.Contains(out, "work") {
		t.Errorf("expected the remaining account still printed, got:\n%s", out)
	}
}

func TestAccountUsage_JSONEmitsSnakeCaseAndRFC3339(t *testing.T) {
	dir := withUsageFixture(t)
	resetTime := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.94), Reset: i64u(resetTime.Unix())},
		},
	})

	c := &cobra.Command{}
	c.Flags().Bool("json", true, "")
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}

	captured := captureStdout(t, func() {
		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage --json: %v", err)
		}
	})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(captured)), &rows); err != nil {
		t.Fatalf("parse json: %v (raw: %q)", err, captured)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row["account"] != "work" {
		t.Errorf("account = %v, want work", row["account"])
	}
	fiveHour, ok := row["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour missing or wrong shape: %+v", row)
	}
	if fiveHour["percent_used"] != float64(94) {
		t.Errorf("percent_used = %v, want 94", fiveHour["percent_used"])
	}
	gotReset, _ := fiveHour["reset"].(string)
	if _, err := time.Parse(time.RFC3339, gotReset); err != nil {
		t.Errorf("reset %q is not RFC3339: %v", gotReset, err)
	}
}

func TestAccountUsage_JSONEmptyArrayWhenNoSnapshots(t *testing.T) {
	_ = withUsageFixture(t)

	c := &cobra.Command{}
	c.Flags().Bool("json", true, "")
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}

	captured := captureStdout(t, func() {
		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage --json: %v", err)
		}
	})

	if strings.TrimSpace(captured) != "[]" {
		t.Errorf("got %q, want []", captured)
	}
}

func TestAccountUsage_NoTokenValuePrinted(t *testing.T) {
	dir := withUsageFixture(t)
	writeUsageSnapshot(t, dir, usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now()),
		Account:    "work",
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: f64u(0.5), Reset: i64u(time.Now().Add(time.Hour).Unix())},
		},
	})

	out, err := runSubcommand(t, runAccountUsage, nil)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	for _, forbidden := range []string{"access-", "refresh-", "sk-ant-"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output contains a token-shaped fragment %q:\n%s", forbidden, out)
		}
	}
}
