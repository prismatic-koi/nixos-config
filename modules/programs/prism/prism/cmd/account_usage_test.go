// Tests for `prism account usage` (issue #2539, parent #2537).
package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/account"
	"github.com/prismatic-koi/prism/internal/usage"
)

// withUsageFixture redirects $XDG_STATE_HOME into a per-test tempdir and
// returns the usage directory (which does not yet exist — callers create it
// via usage.NewStore(dir).Write, or leave it absent to exercise the
// missing-directory path).
//
// It also pins $XDG_CONFIG_HOME and $PI_AUTH_JSON. That is not cosmetic
// isolation: `prism account usage` refreshes a missing or stale snapshot by
// default (#2541), and the refresh reads its bearer token from auth.json and
// its account name from $XDG_CONFIG_HOME/prism/accounts/. Left unpinned, both
// resolve to the DEVELOPER'S REAL FILES, and any test here whose snapshot is
// missing or stale posts a live, authenticated request to api.anthropic.com —
// spending real subscription quota, silently, on every `go test ./...`.
// Round 1 of the #2569 review measured exactly that: three hits from three
// tests, all still reporting PASS.
//
// Pinning here is the structural fix. It holds for every test in this file,
// present and future, regardless of which flags the test registers on its
// cobra command. TestAccountUsage_FixtureAloneBlocksTheNetwork pins the
// guarantee.
func withUsageFixture(t *testing.T) (dir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PI_AUTH_JSON", filepath.Join(root, "pi-agent", "auth.json"))
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

// ── Network-isolation regression (#2569 review round 1) ──────────────────────

// countingTransport records every outbound request and refuses to perform it.
// Reaching it at all is the failure the test is looking for.
type countingTransport struct{ hits atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return nil, errors.New("countingTransport: refusing to perform a real request")
}

// installCountingRefresher points the refresh path at a transport that never
// dials, and returns the hit counter.
func installCountingRefresher(t *testing.T) *countingTransport {
	t.Helper()
	tr := &countingTransport{}
	prev := newUsageRefresher
	t.Cleanup(func() { newUsageRefresher = prev })
	newUsageRefresher = func() *usage.Refresher {
		return &usage.Refresher{
			BaseURL:    "https://api.anthropic.example",
			HTTPClient: &http.Client{Transport: tr},
		}
	}
	return tr
}

// seedRefreshableAccount writes a valid, unexpired credential pair plus a
// stale snapshot under the fixture's tempdirs, so that a refresh is both
// WANTED (stale) and POSSIBLE (a token resolves). Any test using it that
// records zero outbound requests proves the block came from the code under
// test, not from an absent credential.
func seedRefreshableAccount(t *testing.T, usageDir string) {
	t.Helper()
	blob := `{"type":"oauth","access":"fixture-token","refresh":"r","expires":` +
		strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}`

	paths, err := account.ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := os.WriteFile(paths.AccountPath("work"), []byte(blob), 0o600); err != nil {
		t.Fatalf("write account blob: %v", err)
	}
	if err := os.WriteFile(paths.Current, []byte("work\n"), 0o600); err != nil {
		t.Fatalf("write accounts/current: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.AuthJSON), 0o700); err != nil {
		t.Fatalf("mkdir pi agent dir: %v", err)
	}
	if err := os.WriteFile(paths.AuthJSON, []byte(`{"anthropic":`+blob+`}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	reset := time.Now().Add(time.Hour).Unix()
	util := 0.5
	if err := usage.NewStore(usageDir).Write(usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now().Add(-time.Hour)), // stale
		Account:    "work",
		Windows:    &usage.Windows{FiveHour: &usage.Window{Utilization: &util, Reset: &reset}},
	}); err != nil {
		t.Fatalf("seed stale snapshot: %v", err)
	}
}

// TestAccountUsage_FixtureAloneBlocksTheNetwork is the regression test for the
// round-1 review-qa finding on PR #2569.
//
// Before the fix, `withUsageFixture` pinned only $XDG_STATE_HOME. Three tests
// in this file build a bare cobra command that registers only `json`, so
// `GetBool("no-refresh")` returned (false, err), the discarded error left
// noRefresh false, and the refresh ran against the DEVELOPER'S REAL
// ~/.config/prism/accounts/ and ~/.pi/agent/auth.json. The measured result was
// three live authenticated POSTs to api.anthropic.com per `go test ./cmd/`,
// with every test still reporting PASS. CI never saw it — the runner has no
// accounts directory.
//
// Two independent defences now hold, and each sub-test below isolates ONE of
// them. Testing them together would let either mask the other, which is how a
// mutation probe caught an earlier version of this very test.
func TestAccountUsage_FixtureAloneBlocksTheNetwork(t *testing.T) {
	// Defence 1, asserted DIRECTLY rather than by observing an effect.
	//
	// An effect-based assertion ("no request was made") passes vacuously on
	// any machine that happens to have no ~/.config/prism/accounts/ — which
	// includes every CI runner and every agent sandbox. That is precisely why
	// the original defect survived a full review round. Asserting the
	// resolved paths instead fails deterministically everywhere.
	t.Run("fixture pins the credential paths inside the tempdir", func(t *testing.T) {
		usageDir := withUsageFixture(t)
		root := filepath.Dir(filepath.Dir(usageDir)) // <root>/prism/usage → <root>

		paths, err := account.ResolvePaths()
		if err != nil {
			t.Fatalf("ResolvePaths: %v", err)
		}
		if !strings.HasPrefix(paths.Dir, root) {
			t.Errorf("accounts dir resolved to %q, outside the test tempdir %q — "+
				"a refresh would read the developer's real credentials", paths.Dir, root)
		}
		if !strings.HasPrefix(paths.AuthJSON, root) {
			t.Errorf("auth.json resolved to %q, outside the test tempdir %q — "+
				"a refresh would read the developer's real bearer token", paths.AuthJSON, root)
		}
	})

	// Defence 2, isolated from defence 1: credentials ARE present and the
	// snapshot IS stale, so the only thing that can stop the request is the
	// fail-closed read of the unregistered `no-refresh` flag. This is the
	// exact command shape the three offending tests build.
	t.Run("bare command with no no-refresh flag fails closed", func(t *testing.T) {
		usageDir := withUsageFixture(t)
		t.Setenv("PRISM_HOST_API", "")
		seedRefreshableAccount(t, usageDir)
		tr := installCountingRefresher(t)

		c := &cobra.Command{}
		c.Flags().Bool("json", false, "")
		var out, errOut strings.Builder
		c.SetOut(&out)
		c.SetErr(&errOut)

		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage: %v", err)
		}
		if n := tr.hits.Load(); n != 0 {
			t.Fatalf("outbound request count = %d, want 0 — an unregistered "+
				"no-refresh flag must fail closed", n)
		}
	})

	// Control: the same fixture with the flag registered and refresh ENABLED
	// does reach the transport. Without this, the sub-test above could pass
	// because the refresh path is broken rather than because it fails closed.
	t.Run("control: registered flag with refresh enabled does reach the transport", func(t *testing.T) {
		usageDir := withUsageFixture(t)
		t.Setenv("PRISM_HOST_API", "")
		seedRefreshableAccount(t, usageDir)
		tr := installCountingRefresher(t)

		c := &cobra.Command{Use: "usage"}
		addAccountUsageFlags(c) // no-refresh defaults to false: refresh ON
		var out, errOut strings.Builder
		c.SetOut(&out)
		c.SetErr(&errOut)

		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage: %v", err)
		}
		if n := tr.hits.Load(); n != 1 {
			t.Fatalf("outbound request count = %d, want 1 — the fail-closed "+
				"sub-test above is vacuous unless this path genuinely fires "+
				"(stderr: %s)", n, errOut.String())
		}
	})
}
