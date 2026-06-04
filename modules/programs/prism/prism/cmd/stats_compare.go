package cmd

// prism stats compare — side-by-side comparison of two or more spawn outcomes.
//
// Usage:
//
//	prism stats compare [flags] <run-A> <run-B> [<run-C> ...]
//	prism stats abtest <group_id>
//
// Each <run-X> is a session name, a 36-char instance_id, or an unambiguous
// instance_id prefix. prism stats abtest resolves all members of a group and
// calls the same comparison engine.
//
// Output formats: table (default), json, csv.
// Flags: --axes, --format, --diff-only, --sort, --include-inputs, --include-rubric.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// ---------- cobra command wiring ----------

var compareCmd = &cobra.Command{
	Use:   "compare <run-A> <run-B> [<run-C> ...]",
	Short: "Side-by-side comparison of spawn outcomes",
	Long: `Compare two or more spawn runs side-by-side across process-level,
agent-level, and per-axis aggregation metrics.

Each argument is a session name, a full 36-character instance_id, or an
unambiguous instance_id prefix. You may mix session names and instance IDs.

Output is a formatted table by default. Use --format json to emit the
machine-readable contract format or --format csv for spreadsheet import.

Examples:
  prism stats compare run-A run-B
  prism stats compare --diff-only run-A run-B run-C
  prism stats compare --format json run-A run-B | jq .`,
	Args: cobra.MinimumNArgs(2),
	RunE: runStatsCompare,
}

var abtestCmd = &cobra.Command{
	Use:   "abtest <group_id>",
	Short: "Compare all members of an abtest session group",
	Long: `Equivalent to prism stats compare but takes a session_groups.group_id and
resolves all group members automatically.

Example:
  prism stats abtest <group_id>`,
	Args: cobra.ExactArgs(1),
	RunE: runStatsAbtest,
}

func init() {
	for _, cmd := range []*cobra.Command{compareCmd, abtestCmd} {
		cmd.Flags().StringSlice("axes", nil, "Comma-separated axes to display (default: all). Names: end_state, duration_ms, interrupted_count, compaction_count, error_event_count, permission_ask_count, permission_denied_count, doom_loop_count, pr_number, pr_merged_at, review_verdict, review_pass_count, review_fail_count, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, cost_usd, tool_call, tool_error, msg_assistant, time_to_first_event, time_to_finished")
		cmd.Flags().String("format", "table", "Output format: table, json, csv")
		cmd.Flags().Bool("diff-only", false, "Hide rows where every run has the same value")
		cmd.Flags().String("sort", "", "Sort columns by this axis value descending (reserved; renderer details deferred per C3 §6.5)")
		cmd.Flags().Bool("include-inputs", false, "Prepend spawn_inputs block (default: on for 2-run, off for 3+)")
		cmd.Flags().Bool("include-rubric", false, "Include rubric_* columns (hidden by default)")
	}
	statsCmd.AddCommand(compareCmd)
	statsCmd.AddCommand(abtestCmd)
}

// ---------- runner functions ----------

func runStatsCompare(cmd *cobra.Command, args []string) error {
	// PRISM_HOST_API proxy dispatch: in-sandbox sessions cannot reach the
	// host DB directly, so forward the raw args verbatim to the host
	// sidecar's GET /stats?view=compare endpoint. The server resolves each
	// arg the same way `resolveSessionArg` does locally and returns the
	// per-run (session, outcome, inputs) triples; the renderer below is
	// unchanged so output is byte-identical to a host-direct invocation.
	// Issue #2098.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runStatsCompareProxy(cmd, args, apiURL)
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats compare: %w", err)
	}
	defer d.Close()

	// Resolve each arg to a sessions row.
	var sessions []*db.Session
	for _, arg := range args {
		sess, err := resolveSessionArg(d, arg, false)
		if err != nil {
			return fmt.Errorf("stats compare: %w", err)
		}
		sessions = append(sessions, sess)
	}

	return runComparison(cmd, d, sessions)
}

func runStatsAbtest(cmd *cobra.Command, args []string) error {
	groupID := args[0]

	// PRISM_HOST_API proxy dispatch (issue #2098). The server resolves the
	// group members and returns the same {"runs":[...]} envelope as
	// view=compare, sorted by session_name to match the direct-DB path's
	// deterministic ordering.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return runStatsAbtestProxy(cmd, groupID, apiURL)
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats abtest: %w", err)
	}
	defer d.Close()

	// Resolve group members.
	members, err := d.GroupResults(groupID)
	if err != nil {
		return fmt.Errorf("stats abtest: resolve group members: %w", err)
	}
	if len(members) == 0 {
		return fmt.Errorf("stats abtest: no members found for group %q", groupID)
	}

	// Get the sessions rows for each member by session name (most recent).
	var sessions []*db.Session
	for _, m := range members {
		sess, err := d.MostRecentSessionForName(m.SessionName)
		if err != nil {
			return fmt.Errorf("stats abtest: resolve member %q: %w", m.SessionName, err)
		}
		if sess == nil {
			return fmt.Errorf("stats abtest: member session %q not found in sessions table", m.SessionName)
		}
		sessions = append(sessions, sess)
	}
	// Sort by session_name for deterministic output.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionName < sessions[j].SessionName
	})

	return runComparison(cmd, d, sessions)
}

// runStatsCompareProxy handles the PRISM_HOST_API path for `prism stats
// compare`. It forwards the raw args to the sidecar, unmarshals the per-run
// payloads, and hands them to the same renderer the direct-DB path uses.
func runStatsCompareProxy(cmd *cobra.Command, args []string, apiURL string) error {
	raw, err := proxyStatsCompare(apiURL, args)
	if err != nil {
		return fmt.Errorf("stats compare: %w", err)
	}
	var resp sidecar.StatsCompareResponseWire
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("stats compare: unmarshal response: %w", err)
	}
	runs, err := wireRunsToCompareRuns(resp.Runs)
	if err != nil {
		return fmt.Errorf("stats compare: %w", err)
	}
	return renderComparison(cmd, runs)
}

// runStatsAbtestProxy handles the PRISM_HOST_API path for `prism stats abtest
// <group_id>`.
func runStatsAbtestProxy(cmd *cobra.Command, groupID, apiURL string) error {
	raw, err := proxyStatsAbtest(apiURL, groupID)
	if err != nil {
		return fmt.Errorf("stats abtest: %w", err)
	}
	var resp sidecar.StatsCompareResponseWire
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("stats abtest: unmarshal response: %w", err)
	}
	runs, err := wireRunsToCompareRuns(resp.Runs)
	if err != nil {
		return fmt.Errorf("stats abtest: %w", err)
	}
	return renderComparison(cmd, runs)
}

// wireRunsToCompareRuns converts the server-side per-run payloads into the
// cmd-local compareRun shape consumed by the renderer. Returns an error if
// any payload is missing the required sessions row (defence-in-depth: the
// sidecar always populates this field, but a malformed response would
// otherwise NPE deep in axisValue).
func wireRunsToCompareRuns(wireRuns []sidecar.StatsCompareRunWire) ([]compareRun, error) {
	runs := make([]compareRun, 0, len(wireRuns))
	for i, w := range wireRuns {
		if w.Session == nil {
			return nil, fmt.Errorf("proxy response: run %d missing session", i)
		}
		label := w.Label
		if label == "" {
			// Defensive fallback so an empty server-side label does not
			// produce "run-" rendered cells. Matches the cmd-side
			// loadCompareRuns labelling.
			label = "run-" + string(rune('A'+i))
		}
		runs = append(runs, compareRun{
			Label:   label,
			Session: w.Session,
			Outcome: w.Outcome,
			Inputs:  w.Inputs,
		})
	}
	return runs, nil
}

// ---------- comparison engine ----------

// compareRun holds per-run data for the comparison.
type compareRun struct {
	Label   string
	Session *db.Session
	// Outcome may be nil when the session is still in-progress (live state
	// such as active/idle/reviewing). When a session has reached a terminal
	// state (finished/error/interrupted) but no spawn_outcome row exists yet
	// (the window between terminal transition and prism cleanup), Outcome is
	// populated by an on-the-fly call to db.ComputeSpawnOutcome — see
	// loadCompareRuns and issue #2102.
	Outcome *db.SpawnOutcome
	// Inputs is the spawn_inputs row for this session, or nil when no row
	// exists (pre-#2087 spawns, or a session created outside of the
	// SpawnSession chokepoint).
	Inputs *db.SpawnInputs
}

// axisRow is a single row in the comparison table: a label and one value per run.
type axisRow struct {
	Name   string
	Values []string // one per run
}

// runComparison is the shared engine for compare and abtest on the
// direct-DB path. It loads the per-run data via loadCompareRuns and hands
// off to renderComparison — the same renderer the proxy path uses, which
// is how byte-identity between the two paths is guaranteed (issue #2098).
func runComparison(cmd *cobra.Command, d *db.DB, sessions []*db.Session) error {
	runs := loadCompareRuns(d, sessions)
	return renderComparison(cmd, runs)
}

// renderComparison reads the comparison-engine flags (--format, --axes,
// --diff-only, --include-inputs, --include-rubric) and renders the
// provided runs in the chosen format. Shared between the direct-DB and
// proxy paths so that --format json / --format csv / table output is
// byte-identical between them.
func renderComparison(cmd *cobra.Command, runs []compareRun) error {
	format, _ := cmd.Flags().GetString("format")
	diffOnly, _ := cmd.Flags().GetBool("diff-only")
	includeRubric, _ := cmd.Flags().GetBool("include-rubric")
	axesFlag, _ := cmd.Flags().GetStringSlice("axes")

	// includeInputs defaults to on for 2-run, off for 3+.
	includeInputsFlagSet := cmd.Flags().Changed("include-inputs")
	includeInputsVal, _ := cmd.Flags().GetBool("include-inputs")
	includeInputs := includeInputsVal
	if !includeInputsFlagSet {
		includeInputs = len(runs) == 2
	}

	// Collect desired axes.
	wantAxes := defaultAxes()
	if len(axesFlag) > 0 {
		// Flatten (cobra's StringSlice may split on commas within a single flag value).
		var flat []string
		for _, a := range axesFlag {
			for _, part := range strings.Split(a, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					flat = append(flat, part)
				}
			}
		}
		wantAxes = flat
	}

	switch format {
	case "json":
		return renderCompareJSON(runs, wantAxes, includeInputs, includeRubric)
	case "csv":
		return renderCompareCSV(runs, wantAxes, includeInputs, includeRubric, diffOnly)
	default:
		return renderCompareTable(runs, wantAxes, includeInputs, includeRubric, diffOnly)
	}
}

// loadCompareRuns assembles the per-run data shown by `prism stats compare`.
//
// For each session it reads the spawn_outcome and spawn_inputs rows. When a
// session has reached a terminal state (finished / error / interrupted)
// but no spawn_outcome row exists yet — the window between the sidecar's
// terminal-state transition and `prism cleanup` — the outcome is computed
// on the fly from agent_events via db.ComputeSpawnOutcome. ComputeSpawnOutcome
// is the same code path WriteSpawnOutcome uses internally, so the values
// surfaced here agree byte-for-byte with the row the cleanup pass will
// later write (issue #2102).
//
// Live sessions (active / idle / reviewing / escalated) keep Outcome == nil
// so the renderer shows “—” — the aggregates are not yet meaningful.
func loadCompareRuns(d *db.DB, sessions []*db.Session) []compareRun {
	runs := make([]compareRun, len(sessions))
	for i, sess := range sessions {
		label := "run-" + string(rune('A'+i))
		run := compareRun{Label: label, Session: sess}

		// Read the persisted spawn_outcome row first. nil is ok — the
		// session may be in the gap between terminal-state transition and
		// cleanup, or still in-progress.
		if out, _ := d.SpawnOutcomeByInstanceID(sess.InstanceID); out != nil {
			run.Outcome = out
		} else if sessionIsTerminal(d, sess) {
			// Terminal state but no persisted row — compute on the fly
			// using the canonical aggregation. The row will later be
			// persisted by `prism cleanup`; the two paths must agree.
			if computed, err := d.ComputeSpawnOutcome(sess.InstanceID); err == nil && computed != nil {
				run.Outcome = computed
			}
		}

		// Load the spawn_inputs row. Best-effort: pre-#2087 sessions may
		// have no row, in which case Inputs stays nil and the renderer
		// surfaces what it can from the sessions row instead.
		if inputs, err := d.SpawnInputsByInstanceID(sess.InstanceID); err == nil {
			run.Inputs = inputs
		}

		runs[i] = run
	}
	return runs
}

// sessionIsTerminal reports whether the agent_status row for sess shows a
// terminal state (finished / error / interrupted / deleted) — the gate for
// computing spawn_outcome on the fly when no persisted row exists yet.
//
// Falls back to sess.EndState (sessions.end_state) for sessions whose
// agent_status row has already been cleaned away but whose sessions row
// still records a terminal end_state. Live sessions keep Outcome nil so the
// renderer shows “—” for aggregate axes (the over-broad-fix negative-test
// AC in #2102).
func sessionIsTerminal(d *db.DB, sess *db.Session) bool {
	if sess == nil {
		return false
	}
	// agent_status is the live source of truth while the row still exists.
	if st, err := d.CurrentStatus(sess.SessionName); err == nil && st != nil {
		return agent.IsTerminal(agent.AgentState(st.State))
	}
	// Fall back to sessions.end_state, written by the sidecar shutdown / by
	// `prism cleanup`. Any non-empty terminal-shaped value here counts as
	// terminal — "reset" (the SetEnded marker) is excluded because it can
	// be set before the more-specific UpdateSessionEnded call, and the
	// aggregates may not yet be stable at that point.
	if sess.EndState != nil {
		switch *sess.EndState {
		case "finished", "error", "interrupted", "deleted":
			return true
		}
	}
	return false
}

// defaultAxes returns the default axis set (all non-rubric axes).
func defaultAxes() []string {
	return []string{
		// Process-level
		"end_state", "duration_ms", "interrupted_count", "compaction_count",
		"error_event_count", "permission_ask_count", "permission_denied_count",
		"doom_loop_count",
		// Agent-level
		"pr_number", "pr_merged_at", "review_verdict",
		"review_pass_count", "review_fail_count",
		// Per-axis aggregations
		"tokens_input", "tokens_output", "tokens_cache_read", "tokens_cache_write",
		"cost_usd", "tool_call", "tool_error", "msg_assistant",
		"time_to_first_event", "time_to_finished",
	}
}

// rubricAxes are shown only when --include-rubric is set.
var rubricAxes = []string{"rubric_verdict", "rubric_score", "rubric_breakdown", "rubric_grader"}

// axisValue returns the display string for a given axis name on a single run.
// Returns "" when the value is nil/zero and would render as "—".
func axisValue(axis string, run compareRun) string {
	sess := run.Session
	out := run.Outcome

	// Axes that come from the sessions table directly.
	switch axis {
	case "end_state":
		if out != nil && out.EndState != nil {
			return *out.EndState
		}
		if sess.EndState != nil {
			return *sess.EndState
		}
		return "—"
	}

	if out == nil {
		return "—"
	}

	switch axis {
	case "duration_ms":
		if out.DurationMs != nil {
			d := time.Duration(*out.DurationMs) * time.Millisecond
			return fmt.Sprintf("%d (%s)", *out.DurationMs, formatDurationLong(d))
		}
		return "—"
	case "interrupted_count":
		return strconv.Itoa(out.InterruptedCount)
	case "compaction_count":
		return strconv.Itoa(out.CompactionCount)
	case "error_event_count":
		return strconv.Itoa(out.ErrorEventCount)
	case "permission_ask_count":
		return strconv.Itoa(out.PermissionAskCount)
	case "permission_denied_count":
		return strconv.Itoa(out.PermissionDeniedCount)
	case "doom_loop_count":
		return strconv.Itoa(out.DoomLoopCount)
	case "pr_number":
		if out.PRNumber != nil {
			return strconv.Itoa(*out.PRNumber)
		}
		return "—"
	case "pr_merged_at":
		if out.PRMergedAt != nil {
			return time.UnixMilli(*out.PRMergedAt).UTC().Format(time.RFC3339)
		}
		return "(not merged)"
	case "review_verdict":
		if out.ReviewVerdict != nil {
			return *out.ReviewVerdict
		}
		return "—"
	case "review_pass_count":
		if out.ReviewPassCount != nil {
			return strconv.Itoa(*out.ReviewPassCount)
		}
		return "—"
	case "review_fail_count":
		if out.ReviewFailCount != nil {
			return strconv.Itoa(*out.ReviewFailCount)
		}
		return "—"
	case "rubric_verdict":
		if out.RubricVerdict != nil {
			return *out.RubricVerdict
		}
		return "—"
	case "rubric_score":
		if out.RubricScore != nil {
			return fmt.Sprintf("%.2f", *out.RubricScore)
		}
		return "—"
	case "rubric_breakdown":
		if out.RubricBreakdown != nil {
			return *out.RubricBreakdown
		}
		return "—"
	case "rubric_grader":
		if out.RubricGrader != nil {
			return *out.RubricGrader
		}
		return "—"
	case "tokens_input":
		if out.TokensInputTotal > 0 {
			return formatTokenCount(int(out.TokensInputTotal))
		}
		return "—"
	case "tokens_output":
		if out.TokensOutputTotal > 0 {
			return formatTokenCount(int(out.TokensOutputTotal))
		}
		return "—"
	case "tokens_cache_read":
		if out.TokensCacheReadTotal > 0 {
			return formatTokenCount(int(out.TokensCacheReadTotal))
		}
		return "—"
	case "tokens_cache_write":
		if out.TokensCacheWriteTotal > 0 {
			return formatTokenCount(int(out.TokensCacheWriteTotal))
		}
		return "—"
	case "cost_usd":
		if out.CostUSDTotal > 0 {
			return formatCost(out.CostUSDTotal)
		}
		return "—"
	case "tool_call":
		return strconv.Itoa(out.ToolCallCount)
	case "tool_error":
		return strconv.Itoa(out.ToolErrorCount)
	case "msg_assistant":
		return strconv.Itoa(out.MsgAssistantCount)
	case "time_to_first_event":
		if out.TimeToFirstEventMs != nil {
			return fmt.Sprintf("%d ms", *out.TimeToFirstEventMs)
		}
		return "—"
	case "time_to_finished":
		if out.TimeToFinishedMs != nil {
			d := time.Duration(*out.TimeToFinishedMs) * time.Millisecond
			return fmt.Sprintf("%d (%s)", *out.TimeToFinishedMs, formatDurationLong(d))
		}
		return "—"
	default:
		return "—"
	}
}

// axisValueNumeric returns a float64 value for percentage-delta calculation.
// Returns (value, true) when numeric; ("", false) for non-numeric axes.
func axisValueNumeric(axis string, out *db.SpawnOutcome) (float64, bool) {
	if out == nil {
		return 0, false
	}
	switch axis {
	case "duration_ms":
		if out.DurationMs != nil {
			return float64(*out.DurationMs), true
		}
	case "interrupted_count":
		return float64(out.InterruptedCount), true
	case "compaction_count":
		return float64(out.CompactionCount), true
	case "error_event_count":
		return float64(out.ErrorEventCount), true
	case "permission_ask_count":
		return float64(out.PermissionAskCount), true
	case "permission_denied_count":
		return float64(out.PermissionDeniedCount), true
	case "doom_loop_count":
		return float64(out.DoomLoopCount), true
	case "review_pass_count":
		if out.ReviewPassCount != nil {
			return float64(*out.ReviewPassCount), true
		}
	case "review_fail_count":
		if out.ReviewFailCount != nil {
			return float64(*out.ReviewFailCount), true
		}
	case "tokens_input":
		return float64(out.TokensInputTotal), true
	case "tokens_output":
		return float64(out.TokensOutputTotal), true
	case "tokens_cache_read":
		return float64(out.TokensCacheReadTotal), true
	case "tokens_cache_write":
		return float64(out.TokensCacheWriteTotal), true
	case "cost_usd":
		return out.CostUSDTotal, true
	case "tool_call":
		return float64(out.ToolCallCount), true
	case "tool_error":
		return float64(out.ToolErrorCount), true
	case "msg_assistant":
		return float64(out.MsgAssistantCount), true
	case "time_to_first_event":
		if out.TimeToFirstEventMs != nil {
			return float64(*out.TimeToFirstEventMs), true
		}
	case "time_to_finished":
		if out.TimeToFinishedMs != nil {
			return float64(*out.TimeToFinishedMs), true
		}
	case "rubric_score":
		if out.RubricScore != nil {
			return *out.RubricScore, true
		}
	}
	return 0, false
}

// deltaStr computes the percentage delta of run-B relative to run-A.
// Used for 2-run comparisons.
func deltaStr(axis string, runA, runB compareRun) string {
	aVal, aOk := axisValueNumeric(axis, runA.Outcome)
	bVal, bOk := axisValueNumeric(axis, runB.Outcome)
	if !aOk || !bOk {
		return ""
	}
	if aVal == 0 && bVal == 0 {
		return "—"
	}
	if aVal == 0 {
		return "+∞"
	}
	pct := (bVal - aVal) / math.Abs(aVal) * 100
	if pct == 0 {
		return "—"
	}
	if pct > 0 {
		return fmt.Sprintf("+%.1f%%", pct)
	}
	return fmt.Sprintf("%.1f%%", pct)
}

// minMaxAnnotation returns "MIN" if this run has the minimum value, "MAX" if maximum,
// "" otherwise. Used for 3+ run comparisons.
func minMaxAnnotation(axis string, idx int, runs []compareRun) string {
	var vals []float64
	var valid []int
	for i, r := range runs {
		if v, ok := axisValueNumeric(axis, r.Outcome); ok {
			vals = append(vals, v)
			valid = append(valid, i)
		}
	}
	if len(vals) < 2 {
		return ""
	}
	myVal, ok := axisValueNumeric(axis, runs[idx].Outcome)
	if !ok {
		return ""
	}
	minVal, maxVal := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}
	if minVal == maxVal {
		return ""
	}
	if myVal == minVal {
		return "MIN"
	}
	if myVal == maxVal {
		return "MAX"
	}
	return ""
}

// allSame reports whether all runs have the same value for an axis.
func allSame(axis string, runs []compareRun) bool {
	if len(runs) == 0 {
		return true
	}
	first := axisValue(axis, runs[0])
	for _, r := range runs[1:] {
		if axisValue(axis, r) != first {
			return false
		}
	}
	return true
}

// buildAxisRows builds the ordered list of axisRows, applying --diff-only.
func buildAxisRows(axes []string, runs []compareRun, diffOnly bool) []axisRow {
	var rows []axisRow
	for _, axis := range axes {
		if diffOnly && allSame(axis, runs) {
			continue
		}
		vals := make([]string, len(runs))
		for i, r := range runs {
			vals[i] = axisValue(axis, r)
		}
		rows = append(rows, axisRow{Name: axis, Values: vals})
	}
	return rows
}

// ---------- table renderer ----------

func renderCompareTable(runs []compareRun, axes []string, includeInputs, includeRubric, diffOnly bool) error {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// Effective axes.
	effectiveAxes := axes
	if includeRubric {
		effectiveAxes = append(effectiveAxes, rubricAxes...)
	}

	isPairwise := len(runs) == 2

	// Column widths.
	const wLabel = 28
	wVal := 26

	// Header row: axis label + run labels.
	headerCols := fmt.Sprintf("%-*s", wLabel, "")
	for _, r := range runs {
		headerCols += fmt.Sprintf("  %-*s", wVal, r.Label)
	}
	if isPairwise {
		headerCols += fmt.Sprintf("  %s", "Δ")
	}
	fmt.Println(styleHeader.Render(headerCols))
	fmt.Println(styleDim.Render(fmt.Sprintf("%-*s", wLabel, "") +
		strings.Repeat("  "+strings.Repeat("─", wVal), len(runs))))

	// Session metadata block.
	fmt.Println()
	fmt.Println(styleHeader.Render("Session"))
	for _, row := range []struct {
		label string
		fn    func(compareRun) string
	}{
		{"instance_id", func(r compareRun) string { return r.Session.InstanceID }},
		{"session_name", func(r compareRun) string { return r.Session.SessionName }},
		{"started_at", func(r compareRun) string { return r.Session.StartedAt.Format("2006-01-02 15:04:05") }},
	} {
		line := fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.label+":"))
		for _, r := range runs {
			v := truncateStr(row.fn(r), wVal)
			line += fmt.Sprintf("  %-*s", wVal, v)
		}
		fmt.Println(line)
	}

	// Spawn inputs block: render the actual values written at spawn time by
	// SpawnSession (#2087) and the `prism pr` writer. The renderer reads
	// profile_name, isolation, harness, branch, agent_role, and the abtest
	// pair id. Partial data is rendered as-is — missing fields collapse to
	// “—” rather than treating the row as absent (issue #2102, Layer 2).
	if includeInputs {
		fmt.Println()
		fmt.Println(styleHeader.Render("Spawn Inputs"))
		if !anyInputsPresent(runs) {
			// Best-effort placeholder for sessions that pre-date the
			// spawn_inputs writer (#2087). The old “C.1” placeholder is
			// gone — the writer is live for every front door, so the only
			// reason a row is missing today is that the session is from
			// before #2087 merged.
			fmt.Println(styleDim.Render("  (no spawn_inputs rows for the runs being compared)"))
		} else {
			for _, row := range inputsAxisRows(runs, diffOnly) {
				line := fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.Name+":"))
				for _, v := range row.Values {
					line += fmt.Sprintf("  %-*s", wVal, truncateStr(v, wVal))
				}
				fmt.Println(line)
			}
		}
	}

	// Section: process-level.
	processAxes := []string{
		"end_state", "duration_ms", "interrupted_count", "compaction_count",
		"error_event_count", "permission_ask_count", "permission_denied_count", "doom_loop_count",
	}
	renderSection(styleHeader, styleLabel, styleDim, "Process-level outcomes", processAxes, effectiveAxes, runs, wLabel, wVal, isPairwise, diffOnly)

	// Section: agent-level.
	agentAxes := []string{
		"pr_number", "pr_merged_at", "review_verdict", "review_pass_count", "review_fail_count",
	}
	renderSection(styleHeader, styleLabel, styleDim, "Agent-level outcomes", agentAxes, effectiveAxes, runs, wLabel, wVal, isPairwise, diffOnly)

	// Section: per-axis aggregations.
	aggregateAxes := []string{
		"tokens_input", "tokens_output", "tokens_cache_read", "tokens_cache_write",
		"cost_usd", "tool_call", "tool_error", "msg_assistant",
		"time_to_first_event", "time_to_finished",
	}
	renderSection(styleHeader, styleLabel, styleDim, "Per-axis aggregations", aggregateAxes, effectiveAxes, runs, wLabel, wVal, isPairwise, diffOnly)

	// Section: rubric.
	if includeRubric {
		renderSection(styleHeader, styleLabel, styleDim, "Rubric-level outcomes", rubricAxes, effectiveAxes, runs, wLabel, wVal, isPairwise, diffOnly)
	} else {
		fmt.Println()
		fmt.Println(styleDim.Render("Rubric-level outcomes: (none recorded; pass --include-rubric to show NULLs)"))
	}

	return nil
}

// renderSection prints a named section of the comparison table.
// Only axes that are in both sectionAxes and wantAxes are shown.
func renderSection(
	styleHeader, styleLabel, styleDim lipgloss.Style,
	title string,
	sectionAxes, wantAxes []string,
	runs []compareRun,
	wLabel, wVal int,
	isPairwise, diffOnly bool,
) {
	// Intersect: only axes requested by the user.
	wantSet := make(map[string]bool, len(wantAxes))
	for _, a := range wantAxes {
		wantSet[a] = true
	}

	var filtered []string
	for _, a := range sectionAxes {
		if wantSet[a] {
			filtered = append(filtered, a)
		}
	}
	if len(filtered) == 0 {
		return
	}

	rows := buildAxisRows(filtered, runs, diffOnly)
	if len(rows) == 0 {
		return
	}

	fmt.Println()
	fmt.Println(styleHeader.Render(title + ":"))
	for _, row := range rows {
		var line string
		if isPairwise {
			// 2 runs: plain values + Δ column.
			line = fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.Name+":"))
			for _, v := range row.Values {
				line += fmt.Sprintf("  %-*s", wVal, truncateStr(v, wVal))
			}
			delta := deltaStr(row.Name, runs[0], runs[1])
			if delta != "" {
				line += fmt.Sprintf("  %s", delta)
			} else {
				line += "  —"
			}
		} else {
			// 3+ runs: MIN/MAX annotation per cell; no Δ column.
			line = fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.Name+":"))
			for i, v := range row.Values {
				ann := minMaxAnnotation(row.Name, i, runs)
				cellStr := truncateStr(v, wVal)
				if ann != "" {
					cellStr = fmt.Sprintf("%s [%s]", truncateStr(v, wVal-len(ann)-3), ann)
				}
				line += fmt.Sprintf("  %-*s", wVal, cellStr)
			}
		}
		fmt.Println(line)
	}
}

// ---------- spawn_inputs renderer helpers ----------

// inputsAxes is the canonical ordered list of spawn_inputs columns surfaced
// by `prism stats compare`. The set must match the AC for issue #2102 Layer 2:
// profile_name, isolation_mode, branch, agent_role, harness, plus the
// abtest pair id (the data point that ties two sibling sessions together).
var inputsAxes = []string{
	"profile_name",
	"harness",
	"isolation_mode",
	"agent_role",
	"branch",
	"abtest_pair_id",
}

// inputsValue returns the display string for a single spawn_inputs axis on
// one run. Falls back from spawn_inputs columns to the sessions row for
// harness and agent_role so that runs with a partial spawn_inputs row (or
// no row at all) still surface what we know. Returns "—" for missing data.
func inputsValue(axis string, run compareRun) string {
	in := run.Inputs
	switch axis {
	case "profile_name":
		if in != nil && in.ProfileName != nil && *in.ProfileName != "" {
			return *in.ProfileName
		}
	case "harness":
		if in != nil && in.HarnessFlag != nil && *in.HarnessFlag != "" {
			return *in.HarnessFlag
		}
		if run.Session != nil && run.Session.Harness != "" {
			return run.Session.Harness
		}
	case "isolation_mode":
		if in != nil && in.IsolationFlag != nil && *in.IsolationFlag != "" {
			return *in.IsolationFlag
		}
	case "agent_role":
		if in != nil && in.AgentFlag != nil && *in.AgentFlag != "" {
			return *in.AgentFlag
		}
		if run.Session != nil && run.Session.AgentRole != nil && *run.Session.AgentRole != "" {
			return *run.Session.AgentRole
		}
	case "branch":
		if in != nil && in.BranchFlag != nil && *in.BranchFlag != "" {
			return *in.BranchFlag
		}
	case "abtest_pair_id":
		if in != nil && in.AbtestPairID != nil && *in.AbtestPairID != "" {
			return *in.AbtestPairID
		}
	}
	return "—"
}

// anyInputsPresent reports whether any inputs axis has a non-“—” value on
// at least one run — used to decide whether to render the Spawn Inputs
// block at all, or fall back to the “no spawn_inputs rows” note.
func anyInputsPresent(runs []compareRun) bool {
	for _, axis := range inputsAxes {
		for _, r := range runs {
			if inputsValue(axis, r) != "—" {
				return true
			}
		}
	}
	return false
}

// inputsAxisRows builds the per-axis rows for the spawn_inputs block,
// applying --diff-only consistently with the outcome sections.
func inputsAxisRows(runs []compareRun, diffOnly bool) []axisRow {
	var rows []axisRow
	for _, axis := range inputsAxes {
		vals := make([]string, len(runs))
		allSame := true
		for i, r := range runs {
			vals[i] = inputsValue(axis, r)
			if i > 0 && vals[i] != vals[0] {
				allSame = false
			}
		}
		if diffOnly && allSame {
			continue
		}
		// Skip rows that are entirely missing across all runs — keeps the
		// rendering compact when (e.g.) no leg in the pair set --branch.
		nonEmpty := false
		for _, v := range vals {
			if v != "—" {
				nonEmpty = true
				break
			}
		}
		if !nonEmpty {
			continue
		}
		rows = append(rows, axisRow{Name: axis, Values: vals})
	}
	return rows
}

// inputsMapForRun returns the per-run spawn_inputs map used by the JSON
// renderer. Missing fields are emitted as JSON null rather than the “—”
// glyph used by the table renderer.
func inputsMapForRun(run compareRun) map[string]interface{} {
	m := make(map[string]interface{}, len(inputsAxes))
	for _, axis := range inputsAxes {
		v := inputsValue(axis, run)
		if v == "—" {
			m[axis] = nil
		} else {
			m[axis] = v
		}
	}
	return m
}

// ---------- JSON renderer ----------

// compareJSONOutput is the top-level JSON shape (C3 §6.3).
type compareJSONOutput struct {
	Runs  []compareJSONRun `json:"runs"`
	Diffs compareJSONDiffs `json:"diffs"`
}

type compareJSONRun struct {
	Label        string                 `json:"label"`
	SessionName  string                 `json:"session_name"`
	InstanceID   string                 `json:"instance_id"`
	SpawnInputs  map[string]interface{} `json:"spawn_inputs"`
	SpawnOutcome map[string]interface{} `json:"spawn_outcome"`
}

type compareJSONDiffs struct {
	SpawnInputs  []string `json:"spawn_inputs"`
	SpawnOutcome []string `json:"spawn_outcome"`
}

func renderCompareJSON(runs []compareRun, axes []string, includeInputs, includeRubric bool) error {
	effectiveAxes := axes
	if includeRubric {
		effectiveAxes = append(effectiveAxes, rubricAxes...)
	}

	out := compareJSONOutput{
		Diffs: compareJSONDiffs{
			SpawnInputs:  []string{},
			SpawnOutcome: []string{},
		},
	}

	for _, r := range runs {
		jRun := compareJSONRun{
			Label:       r.Label,
			SessionName: r.Session.SessionName,
			InstanceID:  r.Session.InstanceID,
			SpawnInputs: inputsMapForRun(r),
		}
		outcomeMap := make(map[string]interface{})
		for _, axis := range effectiveAxes {
			v := axisValue(axis, r)
			if v == "—" {
				outcomeMap[axis] = nil
			} else {
				outcomeMap[axis] = v
			}
		}
		jRun.SpawnOutcome = outcomeMap
		out.Runs = append(out.Runs, jRun)
	}

	// Compute diffs: axes where runs differ.
	for _, axis := range inputsAxes {
		if !inputsAxisAllSame(axis, runs) {
			out.Diffs.SpawnInputs = append(out.Diffs.SpawnInputs, axis)
		}
	}
	for _, axis := range effectiveAxes {
		if !allSame(axis, runs) {
			out.Diffs.SpawnOutcome = append(out.Diffs.SpawnOutcome, axis)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// inputsAxisAllSame mirrors allSame() but for a single spawn_inputs axis.
func inputsAxisAllSame(axis string, runs []compareRun) bool {
	if len(runs) == 0 {
		return true
	}
	first := inputsValue(axis, runs[0])
	for _, r := range runs[1:] {
		if inputsValue(axis, r) != first {
			return false
		}
	}
	return true
}

// ---------- CSV renderer ----------

func renderCompareCSV(runs []compareRun, axes []string, includeInputs, includeRubric, diffOnly bool) error {
	effectiveAxes := axes
	if includeRubric {
		effectiveAxes = append(effectiveAxes, rubricAxes...)
	}

	rows := buildAxisRows(effectiveAxes, runs, diffOnly)

	w := csv.NewWriter(os.Stdout)

	// Header.
	header := []string{"axis"}
	for _, r := range runs {
		header = append(header, r.Label)
	}
	if len(runs) == 2 {
		header = append(header, "delta")
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("csv: write header: %w", err)
	}

	// Inputs rows (CSV equivalent of the table renderer’s Spawn Inputs
	// block). Emitted before outcome axes when --include-inputs is on so
	// downstream consumers see the per-axis ordering: inputs, then outcome.
	if includeInputs {
		for _, row := range inputsAxisRows(runs, diffOnly) {
			record := []string{row.Name}
			record = append(record, row.Values...)
			if len(runs) == 2 {
				// Delta is meaningful only for numeric axes; spawn_inputs
				// columns are all strings (profile_name, isolation, etc).
				record = append(record, "")
			}
			if err := w.Write(record); err != nil {
				return fmt.Errorf("csv: write inputs row: %w", err)
			}
		}
	}

	// Data rows.
	for _, row := range rows {
		record := []string{row.Name}
		record = append(record, row.Values...)
		if len(runs) == 2 {
			record = append(record, deltaStr(row.Name, runs[0], runs[1]))
		}
		if err := w.Write(record); err != nil {
			return fmt.Errorf("csv: write row: %w", err)
		}
	}

	w.Flush()
	return w.Error()
}
