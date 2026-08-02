package cmd

// prism account usage — print Claude subscription rate-limit usage
// snapshots (issue #2539, parent #2537).
//
// This subcommand is a READER of the snapshot format `internal/usage` owns
// (issue #2538). It never writes a snapshot. The sidecar is the only
// writer.
//
// Sandbox constraint: this command must identify the active account from
// current.json inside the usage directory, never from
// ~/.config/prism/accounts/, which is invisible inside an agent sandbox
// (internal/container/mounts.go). All active-account resolution happens in
// internal/usage.ReadAll.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/usage"
)

var accountUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show Claude subscription rate-limit usage for each account",
	Long: `Show Claude subscription rate-limit usage for each account with a
persisted snapshot.

Snapshots are captured passively from Anthropic's rate-limit response
headers and written by the prism sidecar to
~/.local/state/prism/usage/<account>.json. This command only reads that
directory — it never makes a live API call and never writes a snapshot.`,
	Args: cobra.NoArgs,
	RunE: runAccountUsage,
}

func init() {
	accountCmd.AddCommand(accountUsageCmd)
	accountUsageCmd.Flags().Bool("json", false, "Emit a JSON array of usage snapshots instead of the human-readable list")
}

// windowJSON is the snake_case --json shape for one rate-limit window.
// Percentage is an integer derived from the stored fraction; utilization
// itself is not re-exposed here because #2539's contract is percentage +
// reset, not the raw header shape (that's usage.Snapshot's job).
type windowJSON struct {
	PercentUsed int    `json:"percent_used"`
	Reset       string `json:"reset"`
}

// accountUsageJSON is the snake_case --json shape for one account's row.
type accountUsageJSON struct {
	Account    string      `json:"account"`
	Active     bool        `json:"active"`
	CapturedAt string      `json:"captured_at,omitempty"`
	Stale      bool        `json:"stale"`
	FiveHour   *windowJSON `json:"five_hour,omitempty"`
	SevenDay   *windowJSON `json:"seven_day,omitempty"`
	Error      string      `json:"error,omitempty"`
}

func runAccountUsage(cmd *cobra.Command, args []string) error {
	dir, err := usage.DefaultDir()
	if err != nil {
		return err
	}

	jsonMode, _ := cmd.Flags().GetBool("json")

	rows, err := usage.ReadAll(dir)
	if err != nil {
		var missing *usage.ErrUsageDirMissing
		if isUsageDirMissing(err, &missing) {
			if jsonMode {
				return printJSON([]byte("[]"))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "usage directory %s does not exist\n", missing.Dir)
			return nil
		}
		return err
	}

	now := time.Now()

	if jsonMode {
		return renderAccountUsageJSON(rows, now)
	}
	return renderAccountUsageText(cmd, rows, now)
}

// isUsageDirMissing extracts an *usage.ErrUsageDirMissing from err, if any.
func isUsageDirMissing(err error, out **usage.ErrUsageDirMissing) bool {
	if m, ok := err.(*usage.ErrUsageDirMissing); ok {
		*out = m
		return true
	}
	return false
}

func renderAccountUsageJSON(rows []usage.AccountSnapshot, now time.Time) error {
	out := make([]accountUsageJSON, 0, len(rows))
	for _, row := range rows {
		entry := accountUsageJSON{
			Account: row.Name,
			Active:  row.Active,
		}
		if row.ReadErr != nil {
			entry.Error = row.ReadErr.Error()
			out = append(out, entry)
			continue
		}
		entry.CapturedAt = row.Snapshot.CapturedAt
		entry.Stale = row.Stale(now)
		if w := row.Snapshot.Windows; w != nil {
			entry.FiveHour = windowToJSON(w.FiveHour, now)
			entry.SevenDay = windowToJSON(w.SevenDay, now)
		}
		out = append(out, entry)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("account usage --json: marshal: %w", err)
	}
	return printJSON(data)
}

func windowToJSON(w *usage.Window, now time.Time) *windowJSON {
	if w == nil || w.Utilization == nil {
		return nil
	}
	reset := ""
	if w.Reset != nil {
		reset = time.Unix(*w.Reset, 0).UTC().Format(time.RFC3339)
	}
	return &windowJSON{
		PercentUsed: int(*w.Utilization * 100),
		Reset:       reset,
	}
}

func renderAccountUsageText(cmd *cobra.Command, rows []usage.AccountSnapshot, now time.Time) error {
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	if len(rows) == 0 {
		fmt.Fprintln(w, "no usage snapshots found")
		return nil
	}

	for _, row := range rows {
		marker := "  "
		if row.Active {
			marker = "* "
		}
		if row.ReadErr != nil {
			fmt.Fprintf(errW, "error: %v\n", row.ReadErr)
			continue
		}

		staleSuffix := ""
		if row.Stale(now) {
			staleSuffix = " (stale)"
		}

		fmt.Fprintf(w, "%s%s%s\n", marker, row.Name, staleSuffix)

		var fiveHourWindow, sevenDayWindow *usage.Window
		if row.Snapshot.Windows != nil {
			fiveHourWindow = row.Snapshot.Windows.FiveHour
			sevenDayWindow = row.Snapshot.Windows.SevenDay
		}
		fiveHour := formatWindowText(fiveHourWindow, now)
		sevenDay := formatWindowText(sevenDayWindow, now)
		fmt.Fprintf(w, "    5h: %s\n", fiveHour)
		fmt.Fprintf(w, "    7d: %s\n", sevenDay)
	}
	return nil
}

// formatWindowText renders one window as "94% (reset in 1h56m)" or
// "94% (reset now)" when the reset time has already passed, or "no data"
// when the window is absent from the snapshot.
func formatWindowText(w *usage.Window, now time.Time) string {
	if w == nil || w.Utilization == nil {
		return "no data"
	}
	pct := int(*w.Utilization * 100)
	if w.Reset == nil {
		return fmt.Sprintf("%d%%", pct)
	}
	resetAt := time.Unix(*w.Reset, 0)
	if !resetAt.After(now) {
		return fmt.Sprintf("%d%% (now)", pct)
	}
	return fmt.Sprintf("%d%% (%s)", pct, formatCountdown(resetAt.Sub(now)))
}

// formatCountdown renders d as a compact "1h56m" / "4d13h" style string.
func formatCountdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalMinutes := int(d.Round(time.Minute).Minutes())
	days := totalMinutes / (24 * 60)
	totalMinutes -= days * 24 * 60
	hours := totalMinutes / 60
	minutes := totalMinutes % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
