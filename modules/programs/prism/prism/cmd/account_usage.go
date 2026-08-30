package cmd

// prism account usage — print Claude subscription rate-limit usage snapshots.
//
// This subcommand is a READER of the snapshot format `internal/usage` owns.
// It never writes a snapshot itself. The sidecar is the only writer — the
// active refresh persists by POSTing to the sidecar's /usage/snapshot
// endpoint (see account_usage_refresh.go).
//
// Sandbox constraint: the DISPLAY path must identify the active account from
// current.json inside the usage directory, never from
// ~/.config/prism/accounts/, which is invisible inside an agent sandbox
// (internal/container/mounts.go). All active-account resolution for display
// happens in internal/usage.ReadAll. The refresh path does need the accounts
// directory, and therefore runs host-side only; account_usage_refresh.go
// handles the sandboxed case explicitly.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
~/.local/state/prism/usage/<account>.json.

When the active account's snapshot is missing or more than 15 minutes old,
one live request is made to refresh it, and the result is persisted by the
sidecar. Pass --no-refresh to read stored data only.

The refresh must read ~/.config/prism/accounts/ to learn which account it is
refreshing. That directory is not visible inside an agent sandbox, so a
sandboxed invocation always reads stored data and says so.`,
	Args: cobra.NoArgs,
	RunE: runAccountUsage,
}

func init() {
	accountCmd.AddCommand(accountUsageCmd)
	addAccountUsageFlags(accountUsageCmd)
}

// addAccountUsageFlags registers the subcommand's flags.
//
// Factored out of init so the refresh tests can build a command carrying the
// PRODUCTION defaults (refresh enabled) rather than asserting against a
// hand-built flag set that could drift from the real one.
func addAccountUsageFlags(c *cobra.Command) {
	c.Flags().Bool("json", false, "Emit a JSON array of usage snapshots instead of the human-readable list")
	c.Flags().Bool("no-refresh", false, "Print stored snapshots only; make no live API request")
}

// windowJSON is the snake_case --json shape for one rate-limit window.
// Percentage is an integer derived from the stored fraction; utilization
// itself is not re-exposed here because the contract is percentage + reset,
// not the raw header shape (that's usage.Snapshot's job).
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
	// OrganizationID and WorkspaceID mirror usage.Snapshot's fields of the
	// same name — the round-trip requires them to reach `prism account usage
	// --json`, not just the on-disk file.
	OrganizationID string `json:"organization_id,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
}

func runAccountUsage(cmd *cobra.Command, args []string) error {
	dir, err := usage.DefaultDir()
	if err != nil {
		return err
	}

	jsonMode, _ := cmd.Flags().GetBool("json")

	// Fail CLOSED on the refresh flag. pflag returns (false, err) when the
	// flag is not registered, and false means "refresh", so discarding the
	// error would make an unregistered flag spend real quota with a real
	// credential. Production always registers it via addAccountUsageFlags, so
	// the error branch only fires for a caller that built the command by hand
	// — which is exactly the case that must not reach the network.
	noRefresh, flagErr := cmd.Flags().GetBool("no-refresh")
	if flagErr != nil {
		noRefresh = true
	}

	rows, err := usage.ReadAll(dir)
	missingDir := ""
	if err != nil {
		var missing *usage.ErrUsageDirMissing
		if !isUsageDirMissing(err, &missing) {
			return err
		}
		// A missing directory is not fatal: it is exactly the cold-account
		// case a refresh exists to fill. Carry on with no rows.
		missingDir = missing.Dir
		rows = nil
	}

	now := time.Now()

	// AC: --no-refresh makes NO network request. The whole refresh path is
	// skipped, not merely short-circuited inside it.
	if !noRefresh {
		// cobra's Command.Context() returns nil unless Execute set it, and a
		// nil parent panics context.WithTimeout. Unit tests call RunE
		// directly, so the guard is load-bearing, not defensive padding.
		parent := cmd.Context()
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, usage.DefaultTimeout)
		defer cancel()
		outcome := maybeRefresh(ctx, rows, now)
		writeRefreshWarning(cmd.ErrOrStderr(), outcome)
		if outcome.Snapshot != nil {
			rows = mergeRefreshed(rows, outcome.Snapshot, outcome.Account)
			missingDir = ""
		}
	}

	if missingDir != "" && len(rows) == 0 {
		if jsonMode {
			return printJSON([]byte("[]"))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "usage directory %s does not exist\n", missingDir)
		return nil
	}

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
		entry.OrganizationID = row.Snapshot.OrganizationID
		entry.WorkspaceID = row.Snapshot.WorkspaceID
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
		PercentUsed: int(math.Round(*w.Utilization * 100)),
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
	pct := int(math.Round(*w.Utilization * 100))
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
