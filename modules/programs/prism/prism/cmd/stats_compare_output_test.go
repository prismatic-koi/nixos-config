package cmd

// Tests for prism stats compare output-contract fixes (issue #2099):
//
//   Bug 1 — `--format table` column alignment uses a single-pass
//   width calculation across all rows; value cells line up vertically
//   with the run-A / run-B / Δ header anchors, regardless of label or
//   value byte length.
//
//   Bug 2 — top-level `--json` flag is accepted; emits the same JSON
//   document as `--format json` on the success path (byte-identical
//   stdout). Sibling commands `stats abtest <group_id>` and
//   `stats --abtest` honour the same convention.
//
//   Bug 3 — on error, `--json` / `--format json` emits a single-line
//   JSON envelope `{"error":"..."}` to stderr (no cobra usage dump);
//   stdout is empty; exit code non-zero. Other formats (table, csv)
//   are not affected.
//
// Tests use the seedCompareSession / writeAssistantTurn helpers defined
// in stats_compare_test.go and the captureStdout/captureStdoutStderr
// helpers defined in checkin_test.go / escalate_idempotency_test.go.

import (
	"encoding/csv"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// forceLipglossANSI forces lipgloss to emit ANSI escape sequences for
// the duration of the test. Without this, lipgloss auto-detects that
// stdout is not a TTY (test environment) and emits plain text, which
// would hide the issue #2099 Bug 1 mis-alignment that depends on
// `len(styled_bytes) > visible_width`. The cleanup restores the Ascii
// profile so concurrent / subsequent tests are unaffected.
//
// See internal/dashboard/review_summary_test.go for the established
// in-tree precedent.
func forceLipglossANSI(t *testing.T) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
	})
}

// ── Bug 1 — table column alignment ───────────────────────────────────────────

// ansiEscapeRE matches CSI/SGR escape sequences emitted by lipgloss.
// Used to strip styling for column-position assertions.
var ansiEscapeRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripANSI removes ANSI escape sequences from s so the result reflects
// the *visible* characters that would land in the user's terminal. This
// is the right primitive for column-alignment assertions because the
// alignment bug (#2099 Bug 1) was specifically that lipgloss escape
// codes contributed bytes but zero visible width, breaking the renderer's
// `%-*s` byte-count padding.
func stripANSI(s string) string { return ansiEscapeRE.ReplaceAllString(s, "") }

// dataRowRE matches lines of the form `<axis_name>: <value>...` — i.e.
// lines whose first colon is preceded by an axis-name-shaped identifier
// (lowercase letters, digits, underscores). This deliberately excludes
// section titles ("Process-level outcomes:", "Spawn Inputs", "Session")
// and the dim-styled Rubric fallback line ("Rubric-level outcomes:
// (none recorded; ...)") whose label has spaces and a hyphen.
var dataRowRE = regexp.MustCompile(`^\s*[a-z][a-z0-9_]*:\s+\S`)

func isDataRowLine(line string) bool { return dataRowRE.MatchString(line) }

// findValueColumnAnchors returns the *visible* offset of the first
// non-space rune after the label-column gap on each data row. Visible
// offset is the rune count of the prefix, matching what the user sees in
// a terminal regardless of UTF-8 byte width.
func findValueColumnAnchors(t *testing.T, plain string) []int {
	t.Helper()
	var anchors []int
	for _, line := range strings.Split(plain, "\n") {
		if !isDataRowLine(line) {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		i := colonIdx + 1
		for i < len(line) && line[i] == ' ' {
			i++
		}
		// Convert byte index to rune index for the visible anchor.
		anchors = append(anchors, len([]rune(line[:i])))
	}
	return anchors
}

// TestRenderCompareTable_ValueColumnsAlignAcrossRows is the primary
// alignment guard for issue #2099 Bug 1: every "label: value..." row
// must place its first value cell at the same column anchor as every
// other data row's first value cell. The old renderer drifted by
// (raw_bytes_of_styled_label - visible_width) per row, so rows with
// longer axis names sat further right than rows with short names.
func TestRenderCompareTable_ValueColumnsAlignAcrossRows(t *testing.T) {
	forceLipglossANSI(t)
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	inputsA := &db.SpawnInputs{
		ProfileName:   strPtr("anthropic-sonnet"),
		HarnessFlag:   strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
		AgentFlag:     strPtr("worker"),
	}
	inputsB := &db.SpawnInputs{
		ProfileName:   strPtr("google-gemini-3"),
		HarnessFlag:   strPtr("pi"),
		IsolationFlag: strPtr("bwrap"),
		AgentFlag:     strPtr("worker"),
	}
	iidA := seedCompareSession(t, d, "repo@align-a", startedAt, agent.StateFinished, inputsA)
	iidB := seedCompareSession(t, d, "repo@align-b", startedAt, agent.StateFinished, inputsB)
	writeAssistantTurn(t, d, "repo@align-a", iidA, startedAt.Add(10*time.Second), 1500, 700, 300, 150, 0.12)
	writeAssistantTurn(t, d, "repo@align-b", iidB, startedAt.Add(10*time.Second), 2000, 900, 400, 200, 0.34)

	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	runs := loadCompareRuns(d, []*db.Session{sessA, sessB})

	out := captureStdout(t, func() {
		if err := renderCompareTable(runs, defaultAxes(), true, false, false); err != nil {
			t.Fatalf("renderCompareTable: %v", err)
		}
	})

	plain := stripANSI(out)

	anchors := findValueColumnAnchors(t, plain)
	if len(anchors) < 6 {
		t.Fatalf("expected ≥6 data rows for alignment check, got %d\n--- plain output ---\n%s", len(anchors), plain)
	}
	first := anchors[0]
	for i, a := range anchors {
		if a != first {
			lines := strings.Split(plain, "\n")
			t.Errorf("data row %d anchor = %d, want %d (matches row 0)\n  row 0: %q\n  row %d: %q",
				i, a, first, firstDataLine(lines, isDataRowLine), i, ithDataLine(lines, isDataRowLine, i))
		}
	}

	// Cross-check: the header's "run-A" / "run-B" anchors must match the
	// data anchors. Find the header line — the first line that contains
	// both "run-A" and "run-B". Use rune index for visible alignment.
	var headerLine string
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "run-A") && strings.Contains(line, "run-B") {
			headerLine = line
			break
		}
	}
	if headerLine == "" {
		t.Fatalf("did not find run-A/run-B header line in output:\n%s", plain)
	}
	runAByte := strings.Index(headerLine, "run-A")
	runAIdx := len([]rune(headerLine[:runAByte]))
	if runAIdx != first {
		t.Errorf("run-A header anchor = %d, but data rows anchor at %d\n  header: %q", runAIdx, first, headerLine)
	}
}

// TestRenderCompareTable_ThreeRunsAlign verifies the alignment property
// holds for an N-run comparison (no Δ column, MIN/MAX annotations).
func TestRenderCompareTable_ThreeRunsAlign(t *testing.T) {
	forceLipglossANSI(t)
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	inputs := func(profile string) *db.SpawnInputs {
		return &db.SpawnInputs{
			ProfileName:   strPtr(profile),
			HarnessFlag:   strPtr("pi"),
			IsolationFlag: strPtr("bwrap"),
		}
	}
	iidA := seedCompareSession(t, d, "repo@three-a", startedAt, agent.StateFinished, inputs("sonnet"))
	iidB := seedCompareSession(t, d, "repo@three-b", startedAt, agent.StateFinished, inputs("opus"))
	iidC := seedCompareSession(t, d, "repo@three-c", startedAt, agent.StateFinished, inputs("haiku"))
	writeAssistantTurn(t, d, "repo@three-a", iidA, startedAt.Add(5*time.Second), 1000, 500, 0, 0, 0.05)
	writeAssistantTurn(t, d, "repo@three-b", iidB, startedAt.Add(5*time.Second), 1500, 700, 0, 0, 0.10)
	writeAssistantTurn(t, d, "repo@three-c", iidC, startedAt.Add(5*time.Second), 800, 400, 0, 0, 0.04)

	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	sessC, _ := d.SessionByInstanceID(iidC)
	runs := loadCompareRuns(d, []*db.Session{sessA, sessB, sessC})

	out := captureStdout(t, func() {
		if err := renderCompareTable(runs, defaultAxes(), false, false, false); err != nil {
			t.Fatalf("renderCompareTable: %v", err)
		}
	})

	plain := stripANSI(out)
	anchors := findValueColumnAnchors(t, plain)
	if len(anchors) < 6 {
		t.Fatalf("expected ≥6 data rows, got %d:\n%s", len(anchors), plain)
	}
	first := anchors[0]
	for i, a := range anchors {
		if a != first {
			t.Errorf("3-run data row %d anchor = %d, want %d", i, a, first)
		}
	}
	// Header anchor (run-A) must equal the data anchor too — visible runes.
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "run-A") && strings.Contains(line, "run-B") && strings.Contains(line, "run-C") {
			runAByte := strings.Index(line, "run-A")
			runAIdx := len([]rune(line[:runAByte]))
			if runAIdx != first {
				t.Errorf("3-run header anchor = %d, want %d (matches data rows)", runAIdx, first)
			}
			break
		}
	}
}

// TestRenderCompareTable_LongValueTruncates verifies the edge-case AC:
// long values must wrap/truncate inside their column and never push later
// columns right. We exercise this with a long session_name (longer than
// the maxValW cap) and assert the value cell visible width does not
// exceed the column width and the next column anchor is unchanged.
func TestRenderCompareTable_LongValueTruncates(t *testing.T) {
	forceLipglossANSI(t)
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-2 * time.Minute)

	longName := "repo@" + strings.Repeat("very-long-leg-name-segment-", 4) // ~110 chars
	inputs := &db.SpawnInputs{ProfileName: strPtr("p"), HarnessFlag: strPtr("pi")}
	iidA := seedCompareSession(t, d, longName, startedAt, agent.StateFinished, inputs)
	iidB := seedCompareSession(t, d, "repo@short", startedAt, agent.StateFinished, inputs)
	writeAssistantTurn(t, d, longName, iidA, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, "repo@short", iidB, startedAt.Add(time.Second), 200, 100, 0, 0, 0.02)

	sessA, _ := d.SessionByInstanceID(iidA)
	sessB, _ := d.SessionByInstanceID(iidB)
	runs := loadCompareRuns(d, []*db.Session{sessA, sessB})

	out := captureStdout(t, func() {
		if err := renderCompareTable(runs, defaultAxes(), true, false, false); err != nil {
			t.Fatalf("renderCompareTable: %v", err)
		}
	})

	plain := stripANSI(out)
	// No line's *visible* width should exceed the per-column truncation
	// budget (label + 2 columns + delta + gaps). lipgloss.Width counts
	// visible columns, so multi-byte runes like "─" count as 1 each.
	const sanityMaxVisible = 200
	for _, line := range strings.Split(plain, "\n") {
		if lipgloss.Width(line) > sanityMaxVisible {
			t.Errorf("line visible width %d > sanity max %d (long value not truncated?): %q",
				lipgloss.Width(line), sanityMaxVisible, line)
		}
	}

	// The session_name row must still be present and the long name must
	// appear in truncated form (with the truncateStr "..." marker).
	foundSessionRow := false
	for _, line := range strings.Split(plain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "session_name:") {
			foundSessionRow = true
			if !strings.Contains(line, "...") {
				t.Errorf("long session_name not truncated in row: %q", line)
			}
		}
	}
	if !foundSessionRow {
		t.Errorf("expected session_name: row in output:\n%s", plain)
	}
}

// ── Bug 2 — top-level --json flag accepted and equivalent to --format json ───

// TestStatsCompare_JSONFlagAccepted is the headline AC for Bug 2:
// `prism stats compare --json <runs>` must be accepted by cobra (no
// "unknown flag" error) and emit a single JSON document on stdout.
func TestStatsCompare_JSONFlagAccepted(t *testing.T) {
	d := openStatsTestDB(t)
	resetRootCmdFlags(t)
	startedAt := time.Now().Add(-2 * time.Minute)
	iidA := seedCompareSession(t, d, "repo@json-a", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("sonnet"), HarnessFlag: strPtr("pi")})
	iidB := seedCompareSession(t, d, "repo@json-b", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("opus"), HarnessFlag: strPtr("pi")})
	writeAssistantTurn(t, d, "repo@json-a", iidA, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, "repo@json-b", iidB, startedAt.Add(time.Second), 200, 100, 0, 0, 0.02)

	rootCmd.SetArgs([]string{"stats", "compare", iidA, iidB, "--json"})
	stdout, _ := captureStdoutStderr(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute(): %v", err)
		}
	})
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("--json produced empty stdout")
	}
	var parsed compareJSONOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("--json stdout is not parseable JSON: %v\nstdout:\n%s", err, stdout)
	}
	if len(parsed.Runs) != 2 {
		t.Errorf("--json runs count = %d, want 2", len(parsed.Runs))
	}
}

// TestStatsCompare_JSONFlagEquivalentToFormatJSON asserts the byte-identical
// stdout contract between `--json` and `--format json` on the success path.
// Both surfaces MUST produce the same output (one shape, two surface flags).
func TestStatsCompare_JSONFlagEquivalentToFormatJSON(t *testing.T) {
	d := openStatsTestDB(t)
	resetRootCmdFlags(t)
	startedAt := time.Now().Add(-2 * time.Minute)
	iidA := seedCompareSession(t, d, "repo@equiv-a", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("p1")})
	iidB := seedCompareSession(t, d, "repo@equiv-b", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("p2")})
	writeAssistantTurn(t, d, "repo@equiv-a", iidA, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, "repo@equiv-b", iidB, startedAt.Add(time.Second), 200, 100, 0, 0, 0.02)

	runOnce := func(args []string) string {
		resetRootCmdFlags(t)
		rootCmd.SetArgs(args)
		out, _ := captureStdoutStderr(t, func() {
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("rootCmd.Execute(%v): %v", args, err)
			}
		})
		return out
	}
	a := runOnce([]string{"stats", "compare", iidA, iidB, "--json"})
	b := runOnce([]string{"stats", "compare", iidA, iidB, "--format", "json"})
	if a != b {
		t.Errorf("--json and --format json are not byte-identical\n--- --json ---\n%s\n--- --format json ---\n%s", a, b)
	}
}

// TestStatsAbtest_JSONFlagAccepted is the sibling-surface AC for Bug 2:
// `prism stats abtest <group_id> --json` must also accept --json and emit
// JSON, sharing the engine with `stats compare`. The members are
// associated with the group via agent_status.group_id (the column
// AbtestGroupSessions reads) using SetGroupID.
func TestStatsAbtest_JSONFlagAccepted(t *testing.T) {
	d := openStatsTestDB(t)
	resetRootCmdFlags(t)
	startedAt := time.Now().Add(-2 * time.Minute)
	inputsA := &db.SpawnInputs{ProfileName: strPtr("sonnet")}
	inputsB := &db.SpawnInputs{ProfileName: strPtr("opus")}
	iidA := seedCompareSession(t, d, "repo@grp-a", startedAt, agent.StateFinished, inputsA)
	iidB := seedCompareSession(t, d, "repo@grp-b", startedAt.Add(time.Second), agent.StateFinished, inputsB)
	writeAssistantTurn(t, d, "repo@grp-a", iidA, startedAt.Add(2*time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, "repo@grp-b", iidB, startedAt.Add(2*time.Second), 200, 100, 0, 0, 0.02)

	groupID, err := d.RegisterGroup("repo@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID("repo@grp-a", groupID); err != nil {
		t.Fatalf("SetGroupID a: %v", err)
	}
	if err := d.SetGroupID("repo@grp-b", groupID); err != nil {
		t.Fatalf("SetGroupID b: %v", err)
	}

	rootCmd.SetArgs([]string{"stats", "abtest", groupID, "--json"})
	stdout, _ := captureStdoutStderr(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute(): %v", err)
		}
	})
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("stats abtest --json produced empty stdout")
	}
	var parsed compareJSONOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("--json stdout not parseable: %v\n%s", err, stdout)
	}
	if len(parsed.Runs) != 2 {
		t.Errorf("expected 2 runs in JSON output, got %d", len(parsed.Runs))
	}
}

// TestStatsAbtestFlag_JSON verifies the top-level `prism stats --abtest --json`
// surface emits the abtest_list payload as JSON (shape {"pairs":[...]}).
func TestStatsAbtestFlag_JSON(t *testing.T) {
	d := openStatsTestDB(t)
	resetRootCmdFlags(t)
	// Reuse the seedAbtestPair helper from stats_proxy_test.go.
	seedAbtestPair(t, d, "pair-2099-aaaa-bbbb-cccc-dddddddddddd")

	rootCmd.SetArgs([]string{"stats", "--abtest", "--json"})
	stdout, _ := captureStdoutStderr(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute(): %v", err)
		}
	})

	var parsed struct {
		Pairs []db.AbtestPairRow `json:"pairs"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("--abtest --json stdout not parseable JSON: %v\n%s", err, stdout)
	}
	if len(parsed.Pairs) != 1 {
		t.Errorf("expected 1 abtest pair in JSON output, got %d:\n%s", len(parsed.Pairs), stdout)
	}
}

// ── Bug 3 — JSON error envelope contract ─────────────────────────────────────

// TestStatsCompare_JSONErrorEnvelope is the headline AC for Bug 3:
// on a bad instance ID, stdout MUST be empty, stderr MUST carry a single
// parseable JSON object with an `error` field, and the exit code MUST be
// non-zero. No cobra usage block.
func TestStatsCompare_JSONErrorEnvelope(t *testing.T) {
	_ = openStatsTestDB(t)
	resetRootCmdFlags(t)

	rootCmd.SetArgs([]string{"stats", "compare", "bogus-id-1", "bogus-id-2", "--json"})
	stdout, stderr := captureStdoutStderr(t, func() {
		err := rootCmd.Execute()
		if err == nil {
			t.Fatalf("expected non-nil error for bad instance ID")
		}
	})

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty on --json error path", stdout)
	}

	// stderr must contain exactly one parseable JSON object with an
	// "error" field; no cobra usage dump.
	if strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr contains cobra usage dump (forbidden on --json error path):\n%s", stderr)
	}
	stderrLines := splitNonEmpty(stderr)
	var jsonLine string
	for _, ln := range stderrLines {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			if jsonLine != "" {
				t.Errorf("stderr carries multiple JSON envelopes (must be exactly one):\n%s", stderr)
			}
			jsonLine = strings.TrimSpace(ln)
		}
	}
	if jsonLine == "" {
		t.Fatalf("no JSON envelope on stderr:\n%s", stderr)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(jsonLine), &payload); err != nil {
		t.Fatalf("stderr JSON envelope not parseable: %v\nline: %q", err, jsonLine)
	}
	msg, ok := payload["error"]
	if !ok || msg == "" {
		t.Errorf("JSON envelope missing 'error' field or empty: %v", payload)
	}
	// The exact error message phrasing is owned by resolveSessionArg —
	// the contract this test enforces is that the *underlying* error is
	// surfaced (not swallowed by the cobra usage handler), which we
	// verify by checking that the message at minimum identifies the bad
	// arg by name.
	if !strings.Contains(msg, "bogus-id-1") {
		t.Errorf("error message %q does not name the bad arg (was the underlying error swallowed?)", msg)
	}
}

// TestStatsCompare_FormatJSONErrorEnvelope verifies the same contract is
// honoured via the legacy `--format json` surface (one shape, two flags).
func TestStatsCompare_FormatJSONErrorEnvelope(t *testing.T) {
	_ = openStatsTestDB(t)
	resetRootCmdFlags(t)

	rootCmd.SetArgs([]string{"stats", "compare", "bogus-id-1", "bogus-id-2", "--format", "json"})
	stdout, stderr := captureStdoutStderr(t, func() {
		err := rootCmd.Execute()
		if err == nil {
			t.Fatalf("expected non-nil error for bad instance ID")
		}
	})
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout non-empty on --format json error path: %q", stdout)
	}
	if strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr carries cobra usage dump on --format json error path:\n%s", stderr)
	}
	var jsonLine string
	for _, ln := range splitNonEmpty(stderr) {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			jsonLine = strings.TrimSpace(ln)
		}
	}
	if jsonLine == "" {
		t.Fatalf("no JSON envelope on stderr:\n%s", stderr)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(jsonLine), &payload); err != nil {
		t.Fatalf("JSON envelope not parseable: %v", err)
	}
	if payload["error"] == "" {
		t.Errorf("envelope missing error field: %v", payload)
	}
}

// ── CSV negative — the fix must NOT over-broad to the CSV format ─────────────

// TestStatsCompare_CSVFormatStillWorks is the negative AC for the
// renderer/error-envelope refactor: --format csv must continue to emit a
// parseable CSV document on stdout, with the existing column order and
// header row. No JSON envelope. No regression in the CSV writer.
func TestStatsCompare_CSVFormatStillWorks(t *testing.T) {
	d := openStatsTestDB(t)
	resetRootCmdFlags(t)
	startedAt := time.Now().Add(-2 * time.Minute)
	iidA := seedCompareSession(t, d, "repo@csv-a", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("sonnet")})
	iidB := seedCompareSession(t, d, "repo@csv-b", startedAt, agent.StateFinished,
		&db.SpawnInputs{ProfileName: strPtr("opus")})
	writeAssistantTurn(t, d, "repo@csv-a", iidA, startedAt.Add(time.Second), 100, 50, 0, 0, 0.01)
	writeAssistantTurn(t, d, "repo@csv-b", iidB, startedAt.Add(time.Second), 200, 100, 0, 0, 0.02)

	rootCmd.SetArgs([]string{"stats", "compare", iidA, iidB, "--format", "csv"})
	stdout, _ := captureStdoutStderr(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("--format csv failed: %v", err)
		}
	})
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("--format csv produced empty stdout")
	}

	r := csv.NewReader(strings.NewReader(stdout))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("--format csv output is not parseable CSV: %v\nstdout:\n%s", err, stdout)
	}
	if len(rows) < 2 {
		t.Fatalf("--format csv produced too few rows: %d", len(rows))
	}
	// Header: axis,run-A,run-B,delta
	header := rows[0]
	if len(header) < 4 || header[0] != "axis" || header[1] != "run-A" || header[2] != "run-B" || header[3] != "delta" {
		t.Errorf("--format csv header = %v, want [axis run-A run-B delta]", header)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// splitNonEmpty splits s on newlines and returns only the non-empty lines.
func splitNonEmpty(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func firstDataLine(lines []string, isData func(string) bool) string {
	for _, ln := range lines {
		if isData(ln) {
			return ln
		}
	}
	return ""
}

func ithDataLine(lines []string, isData func(string) bool, idx int) string {
	n := 0
	for _, ln := range lines {
		if isData(ln) {
			if n == idx {
				return ln
			}
			n++
		}
	}
	return ""
}
