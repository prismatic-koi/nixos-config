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

	"github.com/prismatic-koi/prism/internal/db"
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
		cmd.Flags().String("sort", "", "Sort columns by this axis value descending")
		cmd.Flags().Bool("include-inputs", false, "Prepend spawn_inputs block (default: on for 2-run, off for 3+)")
		cmd.Flags().Bool("include-rubric", false, "Include rubric_* columns (hidden by default)")
	}
	statsCmd.AddCommand(compareCmd)
	statsCmd.AddCommand(abtestCmd)
}

// ---------- runner functions ----------

func runStatsCompare(cmd *cobra.Command, args []string) error {
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

// ---------- comparison engine ----------

// compareRun holds per-run data for the comparison.
type compareRun struct {
	Label       string
	Session     *db.Session
	Outcome     *db.SpawnOutcome // may be nil if not yet computed
}

// axisRow is a single row in the comparison table: a label and one value per run.
type axisRow struct {
	Name   string
	Values []string // one per run
}

// runComparison is the shared engine for compare and abtest.
func runComparison(cmd *cobra.Command, d *db.DB, sessions []*db.Session) error {
	format, _ := cmd.Flags().GetString("format")
	diffOnly, _ := cmd.Flags().GetBool("diff-only")
	includeRubric, _ := cmd.Flags().GetBool("include-rubric")
	axesFlag, _ := cmd.Flags().GetStringSlice("axes")

	// includeInputs defaults to on for 2-run, off for 3+.
	includeInputsFlagSet := cmd.Flags().Changed("include-inputs")
	includeInputsVal, _ := cmd.Flags().GetBool("include-inputs")
	includeInputs := includeInputsVal
	if !includeInputsFlagSet {
		includeInputs = len(sessions) == 2
	}

	// Build run labels as run-A, run-B, run-C...
	runs := make([]compareRun, len(sessions))
	for i, sess := range sessions {
		label := "run-" + string(rune('A'+i))
		out, _ := d.SpawnOutcomeByInstanceID(sess.InstanceID) // nil is ok
		runs[i] = compareRun{Label: label, Session: sess, Outcome: out}
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
	_, ok := axisValueNumeric(axis, runs[idx].Outcome)
	if !ok {
		return ""
	}
	myVal, _ := axisValueNumeric(axis, runs[idx].Outcome)
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

	// Spawn inputs block (show session name differences).
	if includeInputs {
		fmt.Println()
		fmt.Println(styleHeader.Render("Spawn Inputs"))
		fmt.Println(styleDim.Render("  (spawn_inputs table not yet populated — run `prism spawn` with C.1 to capture inputs)"))
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
		line := fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.Name+":"))
		for i, v := range row.Values {
			_ = i
			line += fmt.Sprintf("  %-*s", wVal, truncateStr(v, wVal))
		}
		if isPairwise {
			delta := deltaStr(row.Name, runs[0], runs[1])
			if delta != "" {
				line += fmt.Sprintf("  %s", delta)
			} else {
				line += "  —"
			}
		} else {
			// 3+ runs: MIN/MAX annotation for the last run (cleaner than per-run).
			// Annotate each run's value.
			annotated := fmt.Sprintf("%-*s", wLabel, styleLabel.Render(row.Name+":"))
			for i, v := range row.Values {
				ann := minMaxAnnotation(row.Name, i, runs)
				cellStr := truncateStr(v, wVal)
				if ann != "" {
					cellStr = fmt.Sprintf("%s [%s]", truncateStr(v, wVal-len(ann)-3), ann)
				}
				annotated += fmt.Sprintf("  %-*s", wVal, cellStr)
			}
			line = annotated
		}
		fmt.Println(line)
	}
}

// ---------- JSON renderer ----------

// compareJSONOutput is the top-level JSON shape (C3 §6.3).
type compareJSONOutput struct {
	Runs  []compareJSONRun      `json:"runs"`
	Diffs compareJSONDiffs      `json:"diffs"`
}

type compareJSONRun struct {
	Label        string                 `json:"label"`
	SessionName  string                 `json:"session_name"`
	InstanceID   string                 `json:"instance_id"`
	SpawnInputs  map[string]interface{} `json:"spawn_inputs"` // always {} until C.1 writes rows
	SpawnOutcome map[string]interface{} `json:"spawn_outcome"`
}

type compareJSONDiffs struct {
	SpawnInputs  []string `json:"spawn_inputs"`  // always [] until C.1
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
			SpawnInputs: map[string]interface{}{},
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
	for _, axis := range effectiveAxes {
		if !allSame(axis, runs) {
			out.Diffs.SpawnOutcome = append(out.Diffs.SpawnOutcome, axis)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
