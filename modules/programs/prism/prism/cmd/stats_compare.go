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
	"net/url"
	"os"
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

Output is a formatted table by default. Use --json (preferred) or the
equivalent --format json to emit the machine-readable contract format, or
--format csv for spreadsheet import. --json and --format json are
byte-identical aliases on the success path; on the error path both emit a
single-line JSON object {"error":"<message>"} to stderr and exit non-zero.

Examples:
  prism stats compare run-A run-B
  prism stats compare --diff-only run-A run-B run-C
  prism stats compare --json run-A run-B | jq .`,
	Args: cobra.MinimumNArgs(2),
	RunE: runStatsCompare,
	// SilenceUsage / SilenceErrors keep the JSON error contract clean:
	// on a bad ID we emit a single-line `{"error":"..."}` envelope to
	// stderr (see runStatsCompare) and let main.go drive the non-zero
	// exit code. Cobra's default behaviour would dump the usage block
	// and a duplicate "Error: ..." line, both of which violate the
	// documented `--json` output contract for `prism stats compare`
	// (issue #2099 Bug 3).
	SilenceUsage:  true,
	SilenceErrors: true,
}

var abtestCmd = &cobra.Command{
	Use:   "abtest <group_id>",
	Short: "Compare all members of an abtest session group",
	Long: `Equivalent to prism stats compare but takes a session_groups.group_id and
resolves all group members automatically.

Example:
  prism stats abtest <group_id>`,
	Args:          cobra.ExactArgs(1),
	RunE:          runStatsAbtest,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	for _, cmd := range []*cobra.Command{compareCmd, abtestCmd} {
		cmd.Flags().StringSlice("axes", nil, "Comma-separated axes to display (default: all). Names: end_state, duration_ms, interrupted_count, compaction_count, error_event_count, permission_ask_count, permission_denied_count, doom_loop_count, pr_number, pr_merged_at, review_verdict, review_pass_count, review_fail_count, tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, cost_usd, tool_call, tool_error, msg_assistant, time_to_first_event, time_to_finished")
		cmd.Flags().String("format", "table", "Output format: table, json, csv. --format json is equivalent to the top-level --json flag.")
		cmd.Flags().Bool("diff-only", false, "Hide rows where every run has the same value")
		cmd.Flags().String("sort", "", "Sort columns by this axis value descending (reserved; renderer details deferred per C3 §6.5)")
		cmd.Flags().Bool("include-inputs", false, "Prepend spawn_inputs block (default: on for 2-run, off for 3+)")
		cmd.Flags().Bool("include-rubric", false, "Include rubric_* columns (hidden by default)")
		// Top-level `--json` flag honours the prism-wide convention that
		// every list/lookup subcommand accepts `--json` (issue #2099 Bug 2).
		// It is byte-equivalent to `--format json` on stdout for the success
		// path; on the error path both surfaces emit `{"error":"..."}` on
		// stderr per the prism `--json` contract (issue #2099 Bug 3).
		cmd.Flags().Bool("json", false, "Emit a single JSON document to stdout (alias for --format json). On error, emits {\"error\":\"...\"} to stderr.")
	}
	statsCmd.AddCommand(compareCmd)
	statsCmd.AddCommand(abtestCmd)
}

// jsonOutputRequested reports whether the caller asked for the JSON
// output contract via either the top-level `--json` flag (issue #2099
// Bug 2) or the legacy `--format json`. The two surfaces are equivalent
// — see compareCmd.Long for the documented contract.
func jsonOutputRequested(cmd *cobra.Command) bool {
	if j, _ := cmd.Flags().GetBool("json"); j {
		return true
	}
	if f, _ := cmd.Flags().GetString("format"); f == "json" {
		return true
	}
	return false
}

// reportCompareError implements the JSON-or-passthrough error contract
// shared by runStatsCompare and runStatsAbtest. When the caller asked for
// JSON output (via --json or --format json), the error is rendered as a
// single-line JSON envelope on stderr and a quietExitErr is returned so
// main does not double-print. Otherwise the original error is returned
// unchanged.
func reportCompareError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	if jsonOutputRequested(cmd) {
		return emitJSONErrorEnvelope(err)
	}
	return err
}

// ---------- runner functions ----------

func runStatsCompare(cmd *cobra.Command, args []string) error {
	return reportCompareError(cmd, runStatsCompareInner(cmd, args))
}

func runStatsCompareInner(cmd *cobra.Command, args []string) error {
	// PRISM_HOST_API proxy dispatch: inside a sandbox the local shadow DB does
	// not carry the host's data, so the resolution + aggregation must run on the
	// host. The sidecar returns the assembled per-run data and the rendering
	// (Δ column, table/json/csv, all flags) stays on the CLI side — byte for
	// byte identical to the host-direct path (issue #2098).
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		runs, err := proxyStatsCompare(apiURL, args)
		if err != nil {
			return fmt.Errorf("stats compare: %w", err)
		}
		return renderComparison(cmd, runs)
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
	return reportCompareError(cmd, runStatsAbtestInner(cmd, args))
}

func runStatsAbtestInner(cmd *cobra.Command, args []string) error {
	groupID := args[0]

	// PRISM_HOST_API proxy dispatch (issue #2098): same contract as compare —
	// the host resolves the group members and assembles per-run data; the CLI
	// renders.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		runs, err := proxyStatsAbtest(apiURL, groupID)
		if err != nil {
			return fmt.Errorf("stats abtest: %w", err)
		}
		return renderComparison(cmd, runs)
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats abtest: %w", err)
	}
	defer d.Close()

	// Resolve group members to their most recent sessions rows (sorted by
	// session_name) — shared with the proxy path via db.AbtestGroupSessions.
	sessions, err := d.AbtestGroupSessions(groupID)
	if err != nil {
		return fmt.Errorf("stats abtest: %w", err)
	}

	return runComparison(cmd, d, sessions)
}

// proxyStatsCompare proxies `prism stats compare <ids…>` to the host sidecar's
// GET /stats?view=compare endpoint. The instance IDs / session names are sent
// as repeated id= query params (preserving order, which drives run-A/run-B…
// labelling). The host resolves each arg and returns the assembled per-run
// data; resolution is atomic — if any arg fails to resolve the host returns a
// 404 and no runs, matching the host-direct path which aborts on the first
// unresolvable arg.
func proxyStatsCompare(apiURL string, args []string) ([]compareRun, error) {
	q := url.Values{}
	q.Set("view", "compare")
	for _, a := range args {
		q.Add("id", a)
	}
	var resp struct {
		Runs []db.CompareRunData `json:"runs"`
	}
	if err := proxyGetValuesFromHostAPI(apiURL, "/stats", q, &resp); err != nil {
		return nil, err
	}
	return compareRunsFromData(resp.Runs), nil
}

// proxyStatsAbtest proxies `prism stats abtest <group_id>` to the host
// sidecar's GET /stats?view=abtest endpoint. The host resolves the group
// members, sorts them, and returns the assembled per-run data.
func proxyStatsAbtest(apiURL, groupID string) ([]compareRun, error) {
	q := url.Values{}
	q.Set("view", "abtest")
	q.Set("group", groupID)
	var resp struct {
		Runs []db.CompareRunData `json:"runs"`
	}
	if err := proxyGetValuesFromHostAPI(apiURL, "/stats", q, &resp); err != nil {
		return nil, err
	}
	return compareRunsFromData(resp.Runs), nil
}

// compareRunsFromData converts the host-API per-run DTOs into the renderer's
// compareRun values, assigning run-A / run-B / … labels in order. This is the
// proxy-path analogue of loadCompareRuns: identical label assignment, fed the
// data the host already assembled via db.AssembleCompareRun.
func compareRunsFromData(data []db.CompareRunData) []compareRun {
	runs := make([]compareRun, len(data))
	for i, cr := range data {
		runs[i] = compareRun{
			Label:   "run-" + string(rune('A'+i)),
			Session: cr.Session,
			Outcome: cr.Outcome,
			Inputs:  cr.Inputs,
		}
	}
	return runs
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
	// Inputs is the slim, non-sensitive spawn_inputs projection for this
	// session, or nil when no row exists (pre-#2087 spawns, or a session
	// created outside of the SpawnSession chokepoint). It is db.CompareInputs
	// rather than the full db.SpawnInputs so the conversation-bearing columns
	// (prompt_text, …) never cross the all-roles host-API /stats boundary on
	// the proxy path (issue #2098 security review).
	Inputs *db.CompareInputs
}

// axisRow is a single row in the comparison table: a label and one value per run.
type axisRow struct {
	Name   string
	Values []string // one per run
}

// runComparison is the shared engine for the direct-DB compare and abtest
// paths. It assembles the per-run data from the local DB and hands off to
// renderComparison, which is also used directly by the proxy path once the
// host has assembled the runs.
func runComparison(cmd *cobra.Command, d *db.DB, sessions []*db.Session) error {
	return renderComparison(cmd, loadCompareRuns(d, sessions))
}

// renderComparison reads the rendering flags and dispatches to the table,
// JSON, or CSV renderer. It takes fully-assembled runs so the direct-DB and
// host-API proxy paths render byte-identically from the same code (issue
// #2098): every rendering flag (--format, --diff-only, --axes,
// --include-inputs, --include-rubric) is applied here, on the CLI side, so
// none of them can silently degrade on the proxy path.
func renderComparison(cmd *cobra.Command, runs []compareRun) error {
	format, _ := cmd.Flags().GetString("format")
	// The top-level `--json` flag (issue #2099 Bug 2) is an alias for
	// `--format json`. Both surfaces must produce byte-identical stdout,
	// so collapse them to the same renderer dispatch here. If both are
	// set with conflicting values (e.g. `--json --format csv`), --json
	// wins — it is the documented prism-wide convention and the more
	// explicit signal.
	if j, _ := cmd.Flags().GetBool("json"); j {
		format = "json"
	}
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
	data := make([]db.CompareRunData, len(sessions))
	for i, sess := range sessions {
		// db.AssembleCompareRun is the canonical assembly shared with the
		// host-API proxy path (db.AssembleCompareRun → CompareRunOutcome →
		// SessionIsTerminal): persisted spawn_outcome, or an on-the-fly
		// ComputeSpawnOutcome when the session is terminal but not yet
		// cleaned up, plus the best-effort spawn_inputs row.
		data[i] = d.AssembleCompareRun(sess)
	}
	return compareRunsFromData(data)
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
//
// Issue #2099 Bug 1 — the previous renderer used `fmt.Sprintf("%-*s", wLabel,
// styleLabel.Render(row.Name+":"))` which mixes byte-count padding with ANSI
// escape sequences from lipgloss. Escape codes contribute bytes but zero
// visible width, so the visible column anchor drifted by (raw_bytes - visible)
// on every row, breaking vertical alignment between value cells and the
// header row's run-A / run-B / Δ positions.
//
// The new renderer:
//   - assembles every section (Session, Spawn Inputs, Process-level, ...)
//     into a flat list of (section-title, rows[]) entries up front,
//   - performs a single pass over the full dataset to compute the label
//     column width and per-run value column widths from the actual data
//     (rather than guessing fixed constants),
//   - pads with `padRightVisible`, which measures visible width via
//     `lipgloss.Width` so styled labels and unstyled values share the
//     same column anchors,
//   - applies `truncateStr` per column using the column's measured width,
//     so a long value (e.g. a multi-line PR title or a full instance_id)
//     stays inside its own column instead of pushing later columns right.

// compareTableSection groups one rendered block of the compare table. The
// header is fully-rendered text ("Session", "Process-level outcomes:", "").
// emptyMsg, when non-empty, is printed in place of rows for the Spawn Inputs
// no-data fallback. showDelta is true only for the outcome / aggregation
// sections in a 2-run comparison.
type compareTableSection struct {
	title    string
	suffix   string // ":" for outcome sections, empty for the Session block
	rows     []axisRow
	emptyMsg string
	showDelta bool
}

func renderCompareTable(runs []compareRun, axes []string, includeInputs, includeRubric, diffOnly bool) error {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	effectiveAxes := axes
	if includeRubric {
		effectiveAxes = append(effectiveAxes, rubricAxes...)
	}

	isPairwise := len(runs) == 2

	// --- Build sections ------------------------------------------------------
	var sections []compareTableSection

	// Session block — always shown, no Δ column.
	sections = append(sections, compareTableSection{
		title: "Session",
		rows: []axisRow{
			{Name: "instance_id", Values: valsFromRuns(runs, func(r compareRun) string { return r.Session.InstanceID })},
			{Name: "session_name", Values: valsFromRuns(runs, func(r compareRun) string { return r.Session.SessionName })},
			{Name: "started_at", Values: valsFromRuns(runs, func(r compareRun) string { return r.Session.StartedAt.Format("2006-01-02 15:04:05") })},
		},
	})

	// Spawn Inputs block — optional, no Δ column.
	if includeInputs {
		inputsSection := compareTableSection{title: "Spawn Inputs"}
		if !anyInputsPresent(runs) {
			inputsSection.emptyMsg = "  (no spawn_inputs rows for the runs being compared)"
		} else {
			inputsSection.rows = inputsAxisRows(runs, diffOnly)
		}
		sections = append(sections, inputsSection)
	}

	// Outcome sections — deltas only meaningful for pairwise.
	processAxes := []string{
		"end_state", "duration_ms", "interrupted_count", "compaction_count",
		"error_event_count", "permission_ask_count", "permission_denied_count", "doom_loop_count",
	}
	agentAxes := []string{
		"pr_number", "pr_merged_at", "review_verdict", "review_pass_count", "review_fail_count",
	}
	aggregateAxes := []string{
		"tokens_input", "tokens_output", "tokens_cache_read", "tokens_cache_write",
		"cost_usd", "tool_call", "tool_error", "msg_assistant",
		"time_to_first_event", "time_to_finished",
	}
	wantSet := make(map[string]bool, len(effectiveAxes))
	for _, a := range effectiveAxes {
		wantSet[a] = true
	}
	filterRequested := func(axes []string) []string {
		var out []string
		for _, a := range axes {
			if wantSet[a] {
				out = append(out, a)
			}
		}
		return out
	}

	appendOutcomeSection := func(title string, axes []string) {
		filtered := filterRequested(axes)
		if len(filtered) == 0 {
			return
		}
		rows := buildAxisRows(filtered, runs, diffOnly)
		if len(rows) == 0 {
			return
		}
		sections = append(sections, compareTableSection{
			title:     title,
			suffix:    ":",
			rows:      rows,
			showDelta: isPairwise,
		})
	}
	appendOutcomeSection("Process-level outcomes", processAxes)
	appendOutcomeSection("Agent-level outcomes", agentAxes)
	appendOutcomeSection("Per-axis aggregations", aggregateAxes)
	if includeRubric {
		appendOutcomeSection("Rubric-level outcomes", rubricAxes)
	}

	// --- Single-pass width calculation across ALL rows ----------------------
	//
	// labelW is the max axis-name length plus the trailing ":".
	// valW[i] is the max visible width of any value in column i, including
	// run header labels and any "[MIN]" / "[MAX]" annotation that will be
	// appended in the 3+ run case. We use lipgloss.Width so that styled
	// strings contribute their *visible* width, not their raw byte length.
	labelW := 0
	for _, s := range sections {
		for _, r := range s.rows {
			if w := lipgloss.Width(r.Name) + 1; w > labelW { // +1 for ":"
				labelW = w
			}
		}
	}
	if labelW < 12 {
		labelW = 12
	}

	valW := make([]int, len(runs))
	for i, r := range runs {
		if w := lipgloss.Width(r.Label); w > valW[i] {
			valW[i] = w
		}
	}
	for _, s := range sections {
		for _, row := range s.rows {
			for i, v := range row.Values {
				if i >= len(valW) {
					break
				}
				candidate := v
				if !isPairwise {
					if ann := minMaxAnnotation(row.Name, i, runs); ann != "" {
						candidate = candidate + " [" + ann + "]"
					}
				}
				if w := lipgloss.Width(candidate); w > valW[i] {
					valW[i] = w
				}
			}
		}
	}
	const maxValW = 40
	for i := range valW {
		if valW[i] > maxValW {
			valW[i] = maxValW
		}
		if valW[i] < len("run-A") {
			valW[i] = len("run-A")
		}
	}

	const colGap = "  "

	// --- Render: header + separator ----------------------------------------
	var sb strings.Builder
	sb.WriteString(strings.Repeat(" ", labelW))
	for i, r := range runs {
		sb.WriteString(colGap)
		sb.WriteString(padRightVisible(r.Label, valW[i]))
	}
	if isPairwise {
		sb.WriteString(colGap)
		sb.WriteString("Δ")
	}
	fmt.Println(styleHeader.Render(sb.String()))

	sb.Reset()
	sb.WriteString(strings.Repeat(" ", labelW))
	for i := range runs {
		sb.WriteString(colGap)
		sb.WriteString(strings.Repeat("─", valW[i]))
	}
	fmt.Println(styleDim.Render(sb.String()))

	// --- Render: per-section bodies ----------------------------------------
	renderRow := func(name string, values []string, showDelta bool, deltaVal string) {
		var sb strings.Builder
		labelStr := name + ":"
		sb.WriteString(styleLabel.Render(labelStr))
		// Pad the label column to labelW using *visible* width — escape
		// codes from styleLabel.Render must not push the value column right.
		if pad := labelW - lipgloss.Width(labelStr); pad > 0 {
			sb.WriteString(strings.Repeat(" ", pad))
		}
		for i, v := range values {
			sb.WriteString(colGap)
			cell := truncateStr(v, valW[i])
			sb.WriteString(padRightVisible(cell, valW[i]))
		}
		if showDelta {
			sb.WriteString(colGap)
			sb.WriteString(deltaVal)
		}
		fmt.Println(sb.String())
	}

	for _, sec := range sections {
		fmt.Println()
		fmt.Println(styleHeader.Render(sec.title + sec.suffix))
		if sec.emptyMsg != "" {
			fmt.Println(styleDim.Render(sec.emptyMsg))
			continue
		}
		for _, row := range sec.rows {
			// Build the per-cell display values — for 3+ runs, append
			// the MIN/MAX annotation BEFORE truncation/padding so the
			// width pass above already accounted for it.
			vals := make([]string, len(row.Values))
			for i, v := range row.Values {
				if !sec.showDelta && !isPairwise {
					if ann := minMaxAnnotation(row.Name, i, runs); ann != "" {
						vals[i] = v + " [" + ann + "]"
						continue
					}
				}
				vals[i] = v
			}

			deltaVal := ""
			if sec.showDelta {
				d := deltaStr(row.Name, runs[0], runs[1])
				if d == "" {
					d = "—"
				}
				deltaVal = d
			}
			renderRow(row.Name, vals, sec.showDelta, deltaVal)
		}
	}

	if !includeRubric {
		fmt.Println()
		fmt.Println(styleDim.Render("Rubric-level outcomes: (none recorded; pass --include-rubric to show NULLs)"))
	}

	return nil
}

// valsFromRuns is a small helper that materialises a per-run value slice
// from a projection function. Used to build the Session block rows without
// repeating the boilerplate `make + for` loop at each call site.
func valsFromRuns(runs []compareRun, fn func(compareRun) string) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = fn(r)
	}
	return out
}

// padRightVisible right-pads s with spaces so its visible width is at
// least width. Visible width is measured by lipgloss.Width so ANSI escape
// sequences (from lipgloss.Style.Render) contribute zero to the width
// budget. If s is already >= width visibly, it is returned unchanged.
func padRightVisible(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
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
