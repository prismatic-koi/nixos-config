package cmd

// prism retro — a retrospective surface for coordinators (issue #2583, part
// 2/4 of tracking issue #2529).
//
// Usage:
//
//	prism retro                     last 24h, current repo, all trains
//	prism retro --since 2026-08-02  explicit window (ISO 8601 or YYYY-MM-DD)
//	prism retro --days 7            relative window
//	prism retro --repo <name>       cross-repo scoping
//	prism retro --json              scriptable, same data
//
// The command reports the window totals (section 1), the trains table
// (section 2), and the waste signals (section 5). Per-train review-cycle
// detail (section 3) and fixed-overhead accounting (section 4) are separate
// child issues and are out of scope here.
//
// Inside a sandbox (PRISM_HOST_API set) the read is proxied to the host
// sidecar's GET /retro endpoint, the same pattern `prism db` and
// `prism stats compare` use, so a sandboxed session reads the host DB rather
// than the empty shadow DB. The data assembly (db.AssembleRetro) is shared by
// both paths, so the rendered bytes are identical on the host and sandbox
// paths.

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var retroCmd = &cobra.Command{
	Use:   "retro",
	Short: "Retrospective over the trains of work in a window",
	Long: `Report a retrospective over the trains of work started in a window.

A train is a worker session plus its ~review-N-<agent> children rolled up as
one unit of work; a coordinator plus the investigators it spawned; a solo
investigator; or one leg of an A/B pair. Each is one row in the trains table.

With no arguments, reports the last 24 hours for the current repo. Use
--since or --days to change the window, --repo to scope to another repo, and
--json for the machine-readable form (snake_case keys, RFC 3339 timestamps,
empty collections as []).`,
	Args: cobra.NoArgs,
	RunE: runRetro,
}

func init() {
	retroCmd.Flags().String("since", "", "Report trains started on or after this date (ISO 8601 or YYYY-MM-DD)")
	retroCmd.Flags().Int("days", 0, "Report trains started in the last N days (default 1)")
	retroCmd.Flags().String("repo", "", "Scope to this repo (default: the current repo)")
	retroCmd.Flags().Bool("json", false, "Emit structured JSON to stdout instead of the human-readable tables")
	rootCmd.AddCommand(retroCmd)
}

// retroWindowStart computes the sinceMs cut-off from the --since / --days
// flags. --since wins when both are set. With neither, the window is the last
// 24 hours.
func retroWindowStart(sinceStr string, days int) (int64, error) {
	if sinceStr != "" {
		return parseSinceFlag(sinceStr)
	}
	window := 24 * time.Hour
	if days > 0 {
		window = time.Duration(days) * 24 * time.Hour
	}
	return time.Now().Add(-window).UnixMilli(), nil
}

func runRetro(cmd *cobra.Command, _ []string) error {
	sinceStr, _ := cmd.Flags().GetString("since")
	days, _ := cmd.Flags().GetInt("days")
	repoFilter, _ := cmd.Flags().GetString("repo")
	jsonMode, _ := cmd.Flags().GetBool("json")

	sinceMs, err := retroWindowStart(sinceStr, days)
	if err != nil {
		return err
	}

	// Default to the current repo when --repo is not given.
	if repoFilter == "" {
		if cwd, cwErr := os.Getwd(); cwErr == nil {
			repoFilter = deriveRepo(cwd)
		}
	}

	// Sandbox proxy path: route the read to the host sidecar so a sandboxed
	// session reads the host DB, not the empty shadow DB (AC: no host-DB error
	// inside a sandbox).
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		raw, perr := proxyRetro(apiURL, repoFilter, sinceMs)
		if perr != nil {
			return perr
		}
		if jsonMode {
			return printJSON(raw)
		}
		var report db.RetroReport
		if uerr := json.Unmarshal(raw, &report); uerr != nil {
			return fmt.Errorf("retro proxy: unmarshal response: %w", uerr)
		}
		renderRetro(&report)
		return nil
	}

	// Direct DB path (host / host-isolation session).
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("retro: %w", err)
	}
	defer d.Close()

	report, err := d.AssembleRetro(repoFilter, sinceMs)
	if err != nil {
		return fmt.Errorf("retro: %w", err)
	}

	if jsonMode {
		data, merr := json.Marshal(report)
		if merr != nil {
			return fmt.Errorf("retro --json: marshal report: %w", merr)
		}
		return printJSON(data)
	}

	renderRetro(report)
	return nil
}
