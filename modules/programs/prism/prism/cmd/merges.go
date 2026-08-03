package cmd

// prism merges — inspect and manage the local serial merge queue (#783).
//
// Usage:
//
//	prism merges                         alias for `prism merges list`
//	prism merges list                    current watching queue (this coordinator)
//	prism merges list --failed           failed entries
//	prism merges list --abandoned        abandoned entries from previous coordinator
//	prism merges list --all              all non-abandoned rows from the last 7 days
//	prism merges cancel <pr>             cancel a watching entry
//
// Both verbs are coordinator-only, and each route carries its own
// enforcement point (#2608). The proxy route is gated at the host API:
// `/merges` and `/merges/cancel` both call requireCoordinator. The direct
// route is gated by requireMergesCoordinator below.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
)

var mergesCmd = &cobra.Command{
	Use:   "merges",
	Short: "Inspect and manage the merge queue",
	Long:  `Inspect and manage the local serial merge queue. Defaults to listing watching entries.`,
	Args:  cobra.NoArgs,
	RunE:  runMergesList, // default action is list
}

var mergesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List merge queue entries",
	Long: `List merge queue entries for the current coordinator session.

By default shows only 'watching' entries (active queue).
Use --failed, --abandoned, or --all to show other entries.`,
	Args: cobra.NoArgs,
	RunE: runMergesList,
}

var mergesCancelCmd = &cobra.Command{
	Use:   "cancel <pr>",
	Short: "Cancel a watching merge queue entry",
	Long: `Cancel a watching merge queue entry owned by the current coordinator session.

No-op (exit 0 with message) when:
  - The entry is already terminal (merged, failed, cancelled, abandoned)
  - The entry is owned by a different coordinator incarnation`,
	Args: cobra.ExactArgs(1),
	RunE: runMergesCancel,
}

func init() {
	mergesListCmd.Flags().Bool("failed", false, "Show failed entries")
	mergesListCmd.Flags().Bool("abandoned", false, "Show abandoned entries from previous coordinator incarnations")
	mergesListCmd.Flags().Bool("all", false, "Show all non-abandoned entries from the last 7 days")
	mergesListCmd.Flags().Bool("json", false, "Emit a JSON array of merge-queue entries to stdout instead of the human-readable table")

	// Also add the filter flags to the root mergesCmd so `prism merges --failed`
	// works as a shorthand for `prism merges list --failed`.
	mergesCmd.Flags().Bool("failed", false, "Show failed entries")
	mergesCmd.Flags().Bool("abandoned", false, "Show abandoned entries from previous coordinator incarnations")
	mergesCmd.Flags().Bool("all", false, "Show all non-abandoned entries from the last 7 days")
	mergesCmd.Flags().Bool("json", false, "Emit a JSON array of merge-queue entries to stdout instead of the human-readable table")

	mergesCmd.AddCommand(mergesListCmd)
	mergesCmd.AddCommand(mergesCancelCmd)
	rootCmd.AddCommand(mergesCmd)
}

func runMergesList(cmd *cobra.Command, _ []string) error {
	failed, _ := cmd.Flags().GetBool("failed")
	abandoned, _ := cmd.Flags().GetBool("abandoned")
	all, _ := cmd.Flags().GetBool("all")
	jsonMode, _ := cmd.Flags().GetBool("json")

	filter := ""
	switch {
	case failed:
		filter = "failed"
	case abandoned:
		filter = "abandoned"
	case all:
		filter = "all"
	}

	// Inside a bwrap sandbox: proxy the list to the host sidecar (#1043).
	// The host's prism.db is invisible to direct DB reads from inside the
	// sandbox, so falling through to the DB path would read from a shadow
	// tmpfs DB that has none of the host's queued rows. The sidecar reads its
	// own (host-side) DB using the same instance_id and session_name the
	// merge-queue watcher uses, so the response matches what `sqlite3
	// prism.db` shows on the host.
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		params := map[string]string{}
		if filter != "" {
			params["filter"] = filter
		}
		var merges []db.PendingMerge
		if proxyErr := proxyGetFromHostAPI(apiURL, "/merges", params, &merges); proxyErr != nil {
			return fmt.Errorf("prism merges list: %w", proxyErr)
		}
		if jsonMode {
			return renderMergesListJSON(merges)
		}
		return renderMergesList(merges, filter)
	}

	callerSession := review.LookupParentSession()
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism merges list: open db: %w", err)
	}
	defer d.Close()

	// Guard: coordinator-only (#2608). The read-only list path is guarded as
	// well as the cancel path — see requireMergesCoordinator for why a
	// read-only verb still needs it. The guard runs after the proxy branch
	// above returns, so the proxy route is untouched and keeps the host API's
	// HTTP 403 as its sole enforcement point.
	if guardErr := requireMergesCoordinator("prism merges list", "prism merges list", callerSession, d); guardErr != nil {
		return guardErr
	}

	// Look up instance_id and session_name.
	instanceID, sessionName, err := resolveCallerIdentity(callerSession, d)
	if err != nil {
		return fmt.Errorf("prism merges list: %w", err)
	}

	merges, err := d.MergeQueueForInstance(instanceID, sessionName, filter)
	if err != nil {
		return fmt.Errorf("prism merges list: %w", err)
	}

	if jsonMode {
		return renderMergesListJSON(merges)
	}
	return renderMergesList(merges, filter)
}

// mergeJSONRow is the snake_case JSON shape for a single merge-queue entry.
// Defined explicitly so the JSON contract is decoupled from db.PendingMerge
// (which has no struct tags). RFC3339 timestamps; null for absent fields.
type mergeJSONRow struct {
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

// renderMergesListJSON marshals merges to a JSON object with a `merges` array
// (snake_case keys, RFC3339 timestamps) and a `truncated` bool.
// An empty list renders with `"merges":[]` and `"truncated":false`.
func renderMergesListJSON(merges []db.PendingMerge) error {
	rows := make([]mergeJSONRow, 0, len(merges))
	for _, m := range merges {
		row := mergeJSONRow{
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
		Merges    []mergeJSONRow `json:"merges"`
		Truncated bool           `json:"truncated"`
	}{
		Merges:    rows,
		Truncated: false, // merge queue is never implicitly capped
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("prism merges list --json: marshal: %w", err)
	}
	return printJSON(data)
}

func runMergesCancel(cmd *cobra.Command, args []string) error {
	prArg := args[0]
	pr, err := strconv.Atoi(prArg)
	if err != nil || pr <= 0 {
		return fmt.Errorf("prism merges cancel: invalid PR number %q", prArg)
	}

	// Inside a bwrap sandbox: proxy the cancel to the host sidecar (#1043).
	// See merge.go and runMergesList for the reasoning. The sidecar uses its
	// own instance_id when calling CancelMerge, which is the same identity
	// the host watcher and `prism merges` use, so cancellation is visible to
	// both immediately.
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		var resp struct {
			Cancelled bool             `json:"cancelled"`
			Row       *db.PendingMerge `json:"row"`
		}
		if proxyErr := proxyToHostAPI(apiURL, "/merges/cancel", map[string]any{
			"pr": pr,
		}, &resp); proxyErr != nil {
			return fmt.Errorf("prism merges cancel: %w", proxyErr)
		}
		if resp.Cancelled {
			fmt.Printf("PR #%d removed from merge queue.\n", pr)
		} else if resp.Row == nil {
			fmt.Printf("PR #%d is not in the merge queue.\n", pr)
		} else {
			// Either the row is owned by a different incarnation, or it is
			// already in a terminal state. Match the host-path message
			// shapes so the user sees the same output regardless of mode.
			if resp.Row.Status == "watching" {
				fmt.Printf("PR #%d is owned by a different coordinator incarnation — not cancelled.\n", pr)
			} else {
				fmt.Printf("PR #%d is already in terminal state %q — no change.\n", pr, resp.Row.Status)
			}
		}
		return nil
	}

	callerSession := review.LookupParentSession()
	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("prism merges cancel: open db: %w", dbErr)
	}
	defer d.Close()

	// Guard: coordinator-only (#2608). Same placement as in runMergesList —
	// after the proxy branch, and before any DB read or write, so a
	// non-coordinator never reaches d.CancelMerge.
	if guardErr := requireMergesCoordinator("prism merges cancel", fmt.Sprintf("prism merges cancel %d", pr), callerSession, d); guardErr != nil {
		return guardErr
	}

	instanceID, sessionName, err := resolveCallerIdentity(callerSession, d)
	if err != nil {
		return fmt.Errorf("prism merges cancel: %w", err)
	}

	// Resolve the caller's repo authoritatively from agent_status. This is
	// required so CancelMerge cannot touch a same-numbered row belonging
	// to a different repo sharing this prism.db (issue #2354), and so the
	// helper messages below only surface rows from the caller's repo.
	status, statusErr := d.CurrentStatus(sessionName)
	if statusErr != nil || status == nil {
		return fmt.Errorf("prism merges cancel: cannot resolve repo for session %q", sessionName)
	}
	repo := status.Repo
	if repo == "" {
		return fmt.Errorf("prism merges cancel: session %q has no repo recorded in agent_status", sessionName)
	}

	cancelled, err := d.CancelMerge(pr, repo, instanceID)
	if err != nil {
		return fmt.Errorf("prism merges cancel: %w", err)
	}
	if cancelled {
		fmt.Printf("PR #%d removed from merge queue.\n", pr)
	} else {
		// Look up the row to give a helpful message. Scope by repo so we
		// only ever surface a row from the caller's own repo — a
		// same-numbered foreign row must not appear here.
		row, _ := d.PendingMergeByPR(pr, repo)
		if row == nil {
			fmt.Printf("PR #%d is not in the merge queue.\n", pr)
		} else if row.InstanceID != instanceID {
			fmt.Printf("PR #%d is owned by a different coordinator incarnation — not cancelled.\n", pr)
		} else {
			fmt.Printf("PR #%d is already in terminal state %q — no change.\n", pr, row.Status)
		}
	}
	return nil
}

// requireMergesCoordinator is the direct-CLI half of the coordinator-only
// gate on `prism merges list` and `prism merges cancel` (issue #2608).
//
// The host API gates both verbs: `/merges` and `/merges/cancel` each call
// requireCoordinator (internal/sidecar/host_api.go), so a sandboxed caller
// (bwrap, sandbox-exec) is refused with HTTP 403 — every sandboxed session
// carries the host-API socket, so every call from one routes through that
// gate. A session in `host` isolation mode has no socket, so
// sandboxenv.HostAPISocket() is empty, the proxy branch is skipped, and the
// call reaches the direct DB path. With no check there, any host-mode worker,
// review agent, or investigator reached d.CancelMerge with no role check.
//
// This mirrors the `// Guard: coordinator-only` branch in cmd/merge.go and
// requireInvestigateCoordinator in cmd/investigate.go — the same
// session.IsCoordinatorSession call, keyed on the resolved caller session.
// The three properties that made #2604 reject that shape for `prism spawn`
// all hold for these two verbs:
//
//  1. Both verbs already fail closed on an unresolvable caller:
//     resolveCallerIdentity errors when callerSession is empty. Neither verb
//     has a bare-shell bootstrap flow to preserve, unlike `prism spawn`,
//     which bootstraps the first coordinator of a repo and therefore cannot
//     refuse an unknown caller.
//  2. No keybind invokes either verb. modules/programs/prism/tmux.nix binds
//     `prefix + a` to `prism spawn --attach`, which is what makes a
//     caller-keyed guard unacceptable there; no tmux binding, nix module, or
//     script invokes `prism merges`.
//  3. Neither host-API handler spawns a host-side `prism merges` child — both
//     do their DB work in-process — so there is no child process whose
//     inherited PRISM_SESSION_NAME the guard would have to admit.
//
// The read-only `merges list` path is guarded too, not just `cancel`. The
// disclosure argument for leaving it open is real but weak:
// MergeQueueForInstance scopes every filter to the caller's own instance_id,
// so a non-coordinator already sees an empty table. Leaving it open would
// instead make the role boundary of one verb depend on the caller's isolation
// mode — a bwrap worker gets 403 from `/merges` while a host-mode worker gets
// a table. The host API is the contract for the verb; the direct path now
// matches it for both verbs.
//
// Behaviour on an unresolvable caller is explicit and differs from the role
// refusal by message only: an empty caller gets the identity error, because
// "run from inside a prism session" is the actionable message for an operator
// with no session, and it is the message both verbs already returned from
// resolveCallerIdentity before this guard existed. Both outcomes are a
// non-zero exit with no DB write, so the guard fails closed either way.
//
// For a caller that does resolve, IsCoordinatorSession fails closed as well:
// an unknown role — no agent_status row, a DB error, or a NULL
// root_agent_name — falls through to the "name ends in @main" heuristic
// alone, which is false for every worker, review-agent, and investigator
// name.
func requireMergesCoordinator(verb, suggestion, callerSession string, d *db.DB) error {
	if callerSession == "" {
		return fmt.Errorf("%s: cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME", verb)
	}
	if session.IsCoordinatorSession(callerSession, d) {
		return nil
	}
	return fmt.Errorf(`%s: this command is for coordinator sessions only (caller: %s).

The merge queue is arbitrated by the coordinator. Workers, review agents, and
investigators must not inspect or cancel its entries. Ask your coordinator to
run:

  %s

See: modules/programs/prism/agents/coordinator.md`, verb, callerSession, suggestion)
}

// resolveCallerIdentity returns the instance_id and session_name for the
// calling session. Falls back to empty strings gracefully when no session
// can be identified (e.g. running outside a tmux session).
//
// Since #2608, requireMergesCoordinator runs before this function on both
// call paths and returns the same unresolvable-caller error, so the empty
// check below is no longer the first line of defence. It is kept as defence
// in depth: a future caller that skips the guard must still not reach
// CurrentStatus with an empty session name.
func resolveCallerIdentity(callerSession string, d *db.DB) (instanceID, sessionName string, err error) {
	if callerSession == "" {
		return "", "", fmt.Errorf("cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}
	status, err := d.CurrentStatus(callerSession)
	if err != nil {
		return "", "", fmt.Errorf("look up session %q: %w", callerSession, err)
	}
	if status == nil {
		return "", "", fmt.Errorf("session %q not found in prism.db", callerSession)
	}
	if status.InstanceID == nil || *status.InstanceID == "" {
		return "", "", fmt.Errorf("session %q has no instance_id — coordinator may not have started correctly", callerSession)
	}
	return *status.InstanceID, callerSession, nil
}

// renderMergesList renders a tabular view of merge queue entries.
func renderMergesList(merges []db.PendingMerge, filter string) error {
	if len(merges) == 0 {
		fmt.Println(emptyMergesMessage(filter))
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	header, rows := formatMergesRows(merges, time.Now())

	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header)+5)))
	for _, row := range rows {
		fmt.Println(row)
	}
	fmt.Fprintln(os.Stdout)
	return nil
}

// emptyMergesMessage returns the empty-state message for the given filter.
func emptyMergesMessage(filter string) string {
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

// Column widths for the merges table.
const (
	mergesWPos     = 5
	mergesWPR      = 6
	mergesWTitle   = 40
	mergesWStatus  = 11
	mergesWQueued  = 10
	mergesWChecked = 10
	mergesWError   = 40
)

// formatMergesRows builds the header line and one formatted line per merge
// queue row. POS is the 1-based rank of the row in the (already FIFO-sorted)
// input slice — not the raw queue_position timestamp. The header and rows are
// returned as strings so tests can assert on them without capturing stdout.
func formatMergesRows(merges []db.PendingMerge, now time.Time) (header string, rows []string) {
	header = fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %s",
		mergesWPos, "POS",
		mergesWPR, "PR",
		mergesWTitle, "TITLE",
		mergesWStatus, "STATUS",
		mergesWQueued, "QUEUED",
		mergesWChecked, "CHECKED",
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
		queuedStr := formatAge(now, m.QueuedAt)
		checkedStr := "—"
		if m.LastCheckedAt != nil {
			checkedStr = formatAge(now, *m.LastCheckedAt)
		}
		errStr := ""
		if m.Error != nil {
			errStr = *m.Error
		}
		if len(errStr) > mergesWError {
			errStr = errStr[:mergesWError-3] + "..."
		}

		statusStyled := mergeStatusStyle(m.Status).Render(fmt.Sprintf("%-*s", mergesWStatus, truncateStr(m.Status, mergesWStatus)))

		rows = append(rows, fmt.Sprintf("%-*s  %-*s  %-*s  %s  %-*s  %-*s  %s",
			mergesWPos, posStr,
			mergesWPR, prStr,
			mergesWTitle, truncateStr(titleStr, mergesWTitle),
			statusStyled,
			mergesWQueued, queuedStr,
			mergesWChecked, checkedStr,
			errStr,
		))
	}
	return header, rows
}

// mergeStatusStyle returns a lipgloss style for a merge status value.
func mergeStatusStyle(status string) lipgloss.Style {
	switch status {
	case "watching":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")) // yellow
	case "merged":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("32")) // green
	case "failed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("31")) // red
	case "cancelled":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	case "abandoned":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("35")) // magenta
	default:
		return lipgloss.NewStyle()
	}
}

// formatAge returns a short human-readable age string like "2m", "1h", "3d".
func formatAge(now, t time.Time) string {
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
