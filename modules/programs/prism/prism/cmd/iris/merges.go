// merges.go — `iris merges` queue-inspector subcommand group (issue #1719).
//
// `iris merges` is the iris-side analogue of `prism merges`: a read/cancel
// surface over the shared `pending_merges` table (schema is shared per
// §10.4 of daemon-mode-design.md). The actual state-machine that drives
// rows from `watching` to a terminal state is the daemon's merge-queue
// watcher (internal/iris/mergequeue.go); this file only exposes the
// inspector + cancel surface that coordinators need.
//
// Surface:
//
//	iris merges                          alias for `iris merges list`
//	iris merges list                     watching queue for this coordinator
//	iris merges list --failed            failed entries
//	iris merges list --abandoned         abandoned rows from previous incarnations
//	iris merges list --all               all non-abandoned rows from the last 7 days
//	iris merges cancel <pr>              cancel a watching entry owned by this session
//
// # DB access pattern
//
// The CLI reads and writes the shared SQLite DB directly via the shared
// `*db.DB` surface, exactly as `prism merges` does and as `iris merge`
// already does. This is consistent with `iris checkin` and the `iris db`
// family: read-only / DB-mutating debug surfaces talk to the DB file, not
// the daemon's client socket. The all-surfaces-go-through-daemon rule
// (#1668) applies to operations that touch *in-memory* daemon state
// (spawn, prompt deliver, escalate, etc.) — not to operations that only
// touch the pending_merges table, which is process-agnostic durable state.
//
// The "daemon down" canonical error is still surfaced cleanly when the
// DB file does not exist: `ensureDBExists` returns the standard
// `systemctl --user start iris` hint that every other iris CLI uses
// (#1668 ACs).
//
// # Output parity with prism
//
// The human-readable table is rendered byte-for-byte to match
// `prism merges list` so that operators switching between the two CLIs
// during coexistence see the same column shapes, widths, headers, and
// empty-state messages. The rendering helpers are pure functions over
// `[]db.PendingMerge`, mirroring `cmd/merges.go` exactly.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// mergesCmd is the root of the `iris merges` subcommand group. With no
// subcommand it defaults to `list` (mirrors `prism merges`).
var mergesCmd = &cobra.Command{
	Use:   "merges",
	Short: "Inspect and manage the iris merge queue",
	Long: `Inspect and manage the iris merge queue.

Defaults to listing watching entries for the calling coordinator session.
Use the --failed, --abandoned, or --all flags (on either 'iris merges' or
'iris merges list') to see other entries.`,
	Args: cobra.NoArgs,
	RunE: runMergesList, // default action is list (matches prism merges)
}

// mergesListCmd is the explicit `iris merges list` subcommand. Required for
// shell tab-completion and for the discoverable surface mirror of
// `prism merges list`.
var mergesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iris merge queue entries",
	Long: `List iris merge queue entries for the current coordinator session.

By default shows only 'watching' entries (active queue).
Use --failed, --abandoned, or --all to show other entries.`,
	Args: cobra.NoArgs,
	RunE: runMergesList,
}

// mergesCancelCmd implements `iris merges cancel <pr>`.
var mergesCancelCmd = &cobra.Command{
	Use:   "cancel <pr>",
	Short: "Cancel a watching merge queue entry",
	Long: `Cancel a watching merge queue entry owned by the calling coordinator
session.

No-op (exit 0 with a clear message) when:

  - The entry is already terminal (merged, failed, cancelled, abandoned).
  - The entry is owned by a different coordinator incarnation (the
    instance_id on the row does not match this session's instance_id).
  - There is no row at all for the given PR.

These cases all exit 0 because they are "the row is not yours to cancel"
states rather than failures.`,
	Args: cobra.ExactArgs(1),
	RunE: runMergesCancel,
}

func init() {
	// Filter flags live on both the root `merges` command and the explicit
	// `list` subcommand. The root copy makes `iris merges --failed` work as
	// a shorthand for `iris merges list --failed` (matches prism's UX).
	for _, c := range []*cobra.Command{mergesCmd, mergesListCmd} {
		c.Flags().Bool("failed", false, "Show failed entries")
		c.Flags().Bool("abandoned", false, "Show abandoned entries from previous coordinator incarnations")
		c.Flags().Bool("all", false, "Show all non-abandoned entries from the last 7 days")
		c.Flags().Bool("json", false, "Emit a JSON array of merge-queue entries to stdout instead of the human-readable table")
		// The --session and --socket flags are accepted by both list and
		// cancel so scripts can override the env-var-derived session when
		// running outside an iris-managed pi child.
		c.Flags().String("session", "", "Calling session name (defaults to $IRIS_SESSION_NAME)")
	}
	mergesCancelCmd.Flags().String("session", "", "Calling session name (defaults to $IRIS_SESSION_NAME)")

	mergesCmd.AddCommand(mergesListCmd)
	mergesCmd.AddCommand(mergesCancelCmd)
	rootCmd.AddCommand(mergesCmd)
}

// runMergesList resolves the calling session, opens the iris DB read-only,
// and writes either the human-readable table or the JSON shape to stdout.
func runMergesList(cmd *cobra.Command, _ []string) error {
	failed, _ := cmd.Flags().GetBool("failed")
	abandoned, _ := cmd.Flags().GetBool("abandoned")
	all, _ := cmd.Flags().GetBool("all")
	jsonMode, _ := cmd.Flags().GetBool("json")
	sessionFlag, _ := cmd.Flags().GetString("session")

	filter := ""
	switch {
	case failed:
		filter = "failed"
	case abandoned:
		filter = "abandoned"
	case all:
		filter = "all"
	}

	dbPath := resolveDBPath()
	if err := ensureDBExists(dbPath); err != nil {
		return fmt.Errorf("iris merges list: %w", err)
	}
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("iris merges list: open db: %w", err)
	}
	defer database.Close()

	callerSession := resolveIrisMergesCaller(sessionFlag)
	instanceID, sessionName, err := resolveIrisMergesIdentity(callerSession, database)
	if err != nil {
		return fmt.Errorf("iris merges list: %w", err)
	}

	merges, err := database.MergeQueueForInstance(instanceID, sessionName, filter)
	if err != nil {
		return fmt.Errorf("iris merges list: %w", err)
	}

	if jsonMode {
		return renderIrisMergesListJSON(cmd.OutOrStdout(), merges)
	}
	return renderIrisMergesList(cmd.OutOrStdout(), merges, filter)
}

// runMergesCancel cancels a watching entry owned by the calling session.
// Errors (vs. no-ops) are reserved for malformed input and DB errors.
func runMergesCancel(cmd *cobra.Command, args []string) error {
	prArg := args[0]
	pr, err := strconv.Atoi(prArg)
	if err != nil || pr <= 0 {
		return fmt.Errorf("iris merges cancel: invalid PR number %q — must be a positive integer", prArg)
	}
	sessionFlag, _ := cmd.Flags().GetString("session")

	dbPath := resolveDBPath()
	if err := ensureDBExists(dbPath); err != nil {
		return fmt.Errorf("iris merges cancel: %w", err)
	}
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("iris merges cancel: open db: %w", err)
	}
	defer database.Close()

	callerSession := resolveIrisMergesCaller(sessionFlag)
	instanceID, _, err := resolveIrisMergesIdentity(callerSession, database)
	if err != nil {
		return fmt.Errorf("iris merges cancel: %w", err)
	}

	cancelled, err := database.CancelMerge(pr, instanceID)
	if err != nil {
		return fmt.Errorf("iris merges cancel: %w", err)
	}
	out := cmd.OutOrStdout()
	if cancelled {
		fmt.Fprintf(out, "PR #%d removed from merge queue.\n", pr)
		return nil
	}
	// Look up the row to give a helpful message — mirrors prism merges cancel.
	row, _ := database.PendingMergeByPR(pr)
	switch {
	case row == nil:
		fmt.Fprintf(out, "PR #%d is not in the merge queue.\n", pr)
	case row.InstanceID != instanceID:
		fmt.Fprintf(out, "PR #%d is owned by a different coordinator incarnation — not cancelled.\n", pr)
	default:
		fmt.Fprintf(out, "PR #%d is already in terminal state %q — no change.\n", pr, row.Status)
	}
	return nil
}

// resolveIrisMergesCaller resolves the calling session name. Explicit
// --session wins; otherwise we fall back to the shared
// lookupIrisParentSession() helper which reads $IRIS_SESSION_NAME (set
// by the iris supervisor on every pi child) and $PRISM_SESSION_NAME.
func resolveIrisMergesCaller(sessionFlag string) string {
	if sessionFlag != "" {
		return sessionFlag
	}
	return lookupIrisParentSession()
}

// resolveIrisMergesIdentity looks up the instance_id and session_name for
// the calling session. Returns a clear, user-facing error when no session
// can be identified or the agent_status row is missing.
func resolveIrisMergesIdentity(callerSession string, database *db.DB) (instanceID, sessionName string, err error) {
	if callerSession == "" {
		return "", "", fmt.Errorf(
			"cannot determine calling session — run from inside an iris-managed pi session (where $IRIS_SESSION_NAME is set), or pass --session <name>",
		)
	}
	status, err := database.CurrentStatus(callerSession)
	if err != nil {
		return "", "", fmt.Errorf("look up session %q: %w", callerSession, err)
	}
	if status == nil {
		return "", "", fmt.Errorf("session %q not found in iris.db (has the iris daemon ever seen this session?)", callerSession)
	}
	if status.InstanceID == nil || *status.InstanceID == "" {
		return "", "", fmt.Errorf("session %q has no instance_id — the daemon may not have finished registering it", callerSession)
	}
	return *status.InstanceID, callerSession, nil
}

// --- Rendering: human-readable table ---

// Column widths for the merges table. Match cmd/merges.go byte-for-byte so
// the two CLIs render identical column shapes during coexistence.
const (
	irisMergesWPos     = 5
	irisMergesWPR      = 6
	irisMergesWTitle   = 40
	irisMergesWStatus  = 11
	irisMergesWQueued  = 10
	irisMergesWChecked = 10
	irisMergesWError   = 40
)

// renderIrisMergesList writes the table to w. An empty queue prints a
// filter-specific message and returns.
func renderIrisMergesList(w io.Writer, merges []db.PendingMerge, filter string) error {
	if len(merges) == 0 {
		fmt.Fprintln(w, emptyIrisMergesMessage(filter))
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cliColSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cliColSecondary))

	header, rows := formatIrisMergesRows(merges, time.Now())
	fmt.Fprintln(w, styleHeader.Render(header))
	fmt.Fprintln(w, styleDim.Render(strings.Repeat("─", len(header)+5)))
	for _, row := range rows {
		fmt.Fprintln(w, row)
	}
	fmt.Fprintln(w)
	return nil
}

// emptyIrisMergesMessage returns the empty-state message for the given
// filter. Strings match cmd/merges.go byte-for-byte.
func emptyIrisMergesMessage(filter string) string {
	switch filter {
	case "failed":
		return "no failed merge queue entries"
	case "abandoned":
		return "no abandoned merge queue entries from previous coordinator sessions"
	case "all":
		return "no merge queue entries in the last 7 days"
	default:
		return "merge queue is empty"
	}
}

// formatIrisMergesRows builds the header line and one formatted line per
// merge queue row. POS is the 1-based rank of the row in the (already
// FIFO-sorted) input slice — not the raw queue_position timestamp. The
// shape mirrors cmd/merges.go::formatMergesRows for byte-for-byte parity.
func formatIrisMergesRows(merges []db.PendingMerge, now time.Time) (header string, rows []string) {
	header = fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s",
		irisMergesWPos, "POS",
		irisMergesWPR, "PR",
		irisMergesWTitle, "TITLE",
		irisMergesWStatus, "STATUS",
		irisMergesWQueued, "QUEUED",
		irisMergesWChecked, "CHECKED",
		"ERROR",
	)

	rows = make([]string, 0, len(merges))
	for i, m := range merges {
		posStr := fmt.Sprintf("%d", i+1)
		prStr := fmt.Sprintf("#%d", m.PR)
		titleStr := "—"
		if m.Title != nil && *m.Title != "" {
			titleStr = *m.Title
		}
		queuedStr := formatIrisMergeAge(now, m.QueuedAt)
		checkedStr := "—"
		if m.LastCheckedAt != nil {
			checkedStr = formatIrisMergeAge(now, *m.LastCheckedAt)
		}
		errStr := ""
		if m.Error != nil {
			errStr = *m.Error
		}
		if len(errStr) > irisMergesWError {
			errStr = errStr[:irisMergesWError-3] + "..."
		}

		statusStyled := irisMergeStatusStyle(m.Status).Render(
			fmt.Sprintf("%-*s", irisMergesWStatus, truncateIrisMergesStr(m.Status, irisMergesWStatus)),
		)

		rows = append(rows, fmt.Sprintf("%-*s  %-*s  %-*s  %s  %-*s  %-*s  %s",
			irisMergesWPos, posStr,
			irisMergesWPR, prStr,
			irisMergesWTitle, truncateIrisMergesStr(titleStr, irisMergesWTitle),
			statusStyled,
			irisMergesWQueued, queuedStr,
			irisMergesWChecked, checkedStr,
			errStr,
		))
	}
	return header, rows
}

// irisMergeStatusStyle returns a lipgloss style for a merge status value.
// Colour codes are intentionally identical to cmd/merges.go::mergeStatusStyle
// so the styled output is visually identical between the two CLIs.
func irisMergeStatusStyle(status string) lipgloss.Style {
	switch status {
	case "watching":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")) // yellow
	case "merged":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("32")) // green
	case "failed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("31")) // red
	case "cancelled":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(cliColSecondary))
	case "abandoned":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("35")) // magenta
	default:
		return lipgloss.NewStyle()
	}
}

// formatIrisMergeAge returns a short human-readable age string ("2m", "1h",
// "3d"). Matches cmd/merges.go::formatAge byte-for-byte.
func formatIrisMergeAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncateIrisMergesStr truncates a string to maxLen, adding "..." when the
// truncation actually drops characters. Local copy of cmd/stats_format.go's
// truncateStr (which lives in the prism `cmd` package and is not importable
// here).
func truncateIrisMergesStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// --- Rendering: JSON ---

// irisMergeJSONRow is the snake_case JSON shape for a single merge-queue
// entry. Defined explicitly so the JSON contract is decoupled from
// db.PendingMerge (which carries no struct tags). RFC3339 timestamps; null
// for absent fields. Shape matches cmd/merges.go::mergeJSONRow.
type irisMergeJSONRow struct {
	QueuePosition      int64   `json:"queue_position"`
	PR                 int     `json:"pr"`
	Title              *string `json:"title"`
	Status             string  `json:"status"`
	Error              *string `json:"error"`
	EnqueuedAt         string  `json:"enqueued_at"`
	LastCheckedAt      *string `json:"last_checked_at"`
	MergedAt           *string `json:"merged_at"`
	EndedAt            *string `json:"ended_at"`
	CoordinatorSession string  `json:"coordinator_session"`
	InstanceID         string  `json:"instance_id"`
}

// renderIrisMergesListJSON marshals merges to a JSON object with a `merges`
// array (snake_case keys, RFC3339 timestamps) and a `truncated` bool. An
// empty list renders with `"merges":[]` and `"truncated":false`.
func renderIrisMergesListJSON(w io.Writer, merges []db.PendingMerge) error {
	rows := make([]irisMergeJSONRow, 0, len(merges))
	for _, m := range merges {
		row := irisMergeJSONRow{
			QueuePosition:      m.QueuePosition,
			PR:                 m.PR,
			Title:              m.Title,
			Status:             m.Status,
			Error:              m.Error,
			EnqueuedAt:         m.QueuedAt.UTC().Format(time.RFC3339),
			CoordinatorSession: m.SessionName,
			InstanceID:         m.InstanceID,
		}
		if m.LastCheckedAt != nil {
			s := m.LastCheckedAt.UTC().Format(time.RFC3339)
			row.LastCheckedAt = &s
		}
		if m.MergedAt != nil {
			s := m.MergedAt.UTC().Format(time.RFC3339)
			row.MergedAt = &s
		}
		if m.EndedAt != nil {
			s := m.EndedAt.UTC().Format(time.RFC3339)
			row.EndedAt = &s
		}
		rows = append(rows, row)
	}
	out := struct {
		Merges    []irisMergeJSONRow `json:"merges"`
		Truncated bool               `json:"truncated"`
	}{
		Merges:    rows,
		Truncated: false, // merge queue is never implicitly capped
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("iris merges list --json: marshal: %w", err)
	}
	_, werr := fmt.Fprintln(w, string(data))
	return werr
}
