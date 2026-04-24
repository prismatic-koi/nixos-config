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

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
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

	// Also add the filter flags to the root mergesCmd so `prism merges --failed`
	// works as a shorthand for `prism merges list --failed`.
	mergesCmd.Flags().Bool("failed", false, "Show failed entries")
	mergesCmd.Flags().Bool("abandoned", false, "Show abandoned entries from previous coordinator incarnations")
	mergesCmd.Flags().Bool("all", false, "Show all non-abandoned entries from the last 7 days")

	mergesCmd.AddCommand(mergesListCmd)
	mergesCmd.AddCommand(mergesCancelCmd)
	rootCmd.AddCommand(mergesCmd)
}

func runMergesList(cmd *cobra.Command, _ []string) error {
	failed, _ := cmd.Flags().GetBool("failed")
	abandoned, _ := cmd.Flags().GetBool("abandoned")
	all, _ := cmd.Flags().GetBool("all")

	filter := ""
	switch {
	case failed:
		filter = "failed"
	case abandoned:
		filter = "abandoned"
	case all:
		filter = "all"
	}

	callerSession := review.LookupParentSession()
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism merges list: open db: %w", err)
	}
	defer d.Close()

	// Look up instance_id and session_name.
	instanceID, sessionName, err := resolveCallerIdentity(callerSession, d)
	if err != nil {
		return fmt.Errorf("prism merges list: %w", err)
	}

	merges, err := d.MergeQueueForInstance(instanceID, sessionName, filter)
	if err != nil {
		return fmt.Errorf("prism merges list: %w", err)
	}

	return renderMergesList(merges, filter)
}

func runMergesCancel(cmd *cobra.Command, args []string) error {
	prArg := args[0]
	pr, err := strconv.Atoi(prArg)
	if err != nil || pr <= 0 {
		return fmt.Errorf("prism merges cancel: invalid PR number %q", prArg)
	}

	callerSession := review.LookupParentSession()
	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("prism merges cancel: open db: %w", dbErr)
	}
	defer d.Close()

	instanceID, _, err := resolveCallerIdentity(callerSession, d)
	if err != nil {
		return fmt.Errorf("prism merges cancel: %w", err)
	}

	cancelled, err := d.CancelMerge(pr, instanceID)
	if err != nil {
		return fmt.Errorf("prism merges cancel: %w", err)
	}
	if cancelled {
		fmt.Printf("PR #%d removed from merge queue.\n", pr)
	} else {
		// Look up the row to give a helpful message.
		row, _ := d.PendingMergeByPR(pr)
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

// resolveCallerIdentity returns the instance_id and session_name for the
// calling session. Falls back to empty strings gracefully when no session
// can be identified (e.g. running outside a tmux session).
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
		switch filter {
		case "failed":
			fmt.Println("no failed merge queue entries")
		case "abandoned":
			fmt.Println("no abandoned merge queue entries from previous coordinator sessions")
		case "all":
			fmt.Println("no merge queue entries in the last 7 days")
		default:
			fmt.Println("merge queue is empty")
		}
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	now := time.Now()

	const (
		wPos     = 5
		wPR      = 6
		wStatus  = 11
		wQueued  = 10
		wChecked = 10
		wError   = 40
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %s",
		wPos, "POS",
		wPR, "PR",
		wStatus, "STATUS",
		wQueued, "QUEUED",
		wChecked, "CHECKED",
		"ERROR",
	)
	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header)+5)))

	for _, m := range merges {
		posStr := fmt.Sprintf("%d", m.QueuePosition)
		if len(posStr) > wPos {
			posStr = posStr[:wPos]
		}
		prStr := fmt.Sprintf("#%d", m.PR)
		queuedStr := formatAge(now, m.QueuedAt)
		checkedStr := "—"
		if m.LastCheckedAt != nil {
			checkedStr = formatAge(now, *m.LastCheckedAt)
		}
		errStr := ""
		if m.Error != nil {
			errStr = *m.Error
		}
		if len(errStr) > wError {
			errStr = errStr[:wError-3] + "..."
		}

		statusStyled := mergeStatusStyle(m.Status).Render(fmt.Sprintf("%-*s", wStatus, truncateStr(m.Status, wStatus)))

		fmt.Printf("%-*s  %-*s  %s  %-*s  %-*s  %s\n",
			wPos, posStr,
			wPR, prStr,
			statusStyled,
			wQueued, queuedStr,
			wChecked, checkedStr,
			errStr,
		)
	}
	fmt.Fprintln(os.Stdout)
	return nil
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
