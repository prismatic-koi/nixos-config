package cmd

// retro_render.go — the human-readable renderer for `prism retro`. It consumes
// the assembled db.RetroReport (the same value the --json path marshals and the
// host-API proxy path returns), so the table and the JSON never drift.
//
// Token counts render with thousands separators, never in scientific notation
// (issue #2583 correction 6): `prism db query` emits values such as
// 1.1297191e+07, which is unreadable at a glance.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/db"
)

// formatTokensGrouped renders a token count as a plain integer with comma
// thousands separators (e.g. 11297191 → "11,297,191"). This is the AC's
// required rendering: plain integers or thousands separators, never the
// K/M shorthand's lossy rounding and never scientific notation.
func formatTokensGrouped(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	// Leading group: the 1–3 digits before the first comma boundary.
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatRetroCost renders a dollar cost. A cost of exactly 0 is expected under
// subscription profiles (no per-token billing) and renders as "$0.00", not the
// "<$0.01" formatCost would produce — a subscription run is not "almost a
// cent", it is genuinely zero.
func formatRetroCost(cost float64) string {
	if cost <= 0 {
		return "$0.00"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// renderRetro prints the window totals, the trains table, and the waste
// signals for one report.
func renderRetro(r *db.RetroReport) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	repoLabel := r.Repo
	if repoLabel == "" {
		repoLabel = "(all repos)"
	}
	fmt.Printf("%s  %s\n", styleHeader.Render("prism retro"), styleDim.Render(fmt.Sprintf("repo %s  |  %s → %s", repoLabel, r.Since, r.Until)))

	// Empty window: explicit message, exit 0 (the caller returns nil).
	if r.WindowTotals.SessionCount == 0 {
		fmt.Println()
		fmt.Println(styleDim.Render("  no sessions in this window"))
		return
	}
	fmt.Println()

	renderRetroWindowTotals(styleHeader, styleLabel, styleDim, r.WindowTotals)
	fmt.Println()
	renderRetroTrains(styleHeader, styleDim, r.Trains)
	fmt.Println()
	renderRetroWaste(styleHeader, styleLabel, styleDim, r.WasteSignals)

	// NULL-token semantics, stated in the output per the AC.
	fmt.Println()
	fmt.Println(styleDim.Render("  A NULL token field is counted as zero. Cost is $0.00 under subscription profiles."))
}

func renderRetroWindowTotals(styleHeader, styleLabel, styleDim lipgloss.Style, wt db.RetroWindowTotals) {
	fmt.Println(styleHeader.Render("Window totals"))
	fmt.Printf("  %s %s\n", styleLabel.Render("output:     "), formatTokensGrouped(wt.OutputTokens))
	fmt.Printf("  %s %s\n", styleLabel.Render("input:      "), formatTokensGrouped(wt.InputTokens))
	fmt.Printf("  %s %s\n", styleLabel.Render("cache read: "), formatTokensGrouped(wt.CacheReadTokens))
	fmt.Printf("  %s %s\n", styleLabel.Render("cache write:"), formatTokensGrouped(wt.CacheWriteTokens))
	fmt.Printf("  %s %s\n", styleLabel.Render("total:      "), formatTokensGrouped(wt.TotalTokens))
	fmt.Printf("  %s %.1f%%\n", styleLabel.Render("cache-read share:"), wt.CacheReadShare*100)
	fmt.Printf("  %s %s\n", styleLabel.Render("est. cost:  "), formatRetroCost(wt.CostUSD))
	sessLine := fmt.Sprintf("%d session(s)", wt.SessionCount)
	if wt.LiveSessionCount > 0 {
		sessLine += fmt.Sprintf(", %d still live (no data yet)", wt.LiveSessionCount)
	}
	fmt.Printf("  %s\n", styleDim.Render(sessLine))
}

func renderRetroTrains(styleHeader, styleDim lipgloss.Style, trains []db.RetroTrain) {
	fmt.Println(styleHeader.Render("Trains"))
	if len(trains) == 0 {
		fmt.Println(styleDim.Render("  (none)"))
		return
	}
	// Column widths sized to the data.
	rootW := len("TRAIN")
	kindW := len("KIND")
	profW := len("PROFILE")
	for _, t := range trains {
		if len(t.Root) > rootW {
			rootW = len(t.Root)
		}
		if len(t.Kind) > kindW {
			kindW = len(t.Kind)
		}
		prof := t.Profile
		if prof == "" {
			prof = "—"
		}
		if len(prof) > profW {
			profW = len(prof)
		}
	}
	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %6s  %14s  %7s  %8s",
		rootW, "TRAIN", kindW, "KIND", profW, "PROFILE", "CYCLES", "TOTAL TOK", "SHARE", "COST")
	fmt.Println(styleDim.Render(header))
	for _, t := range trains {
		prof := t.Profile
		if prof == "" {
			prof = "—"
		}
		fmt.Printf("  %-*s  %-*s  %-*s  %6d  %14s  %6.1f%%  %8s\n",
			rootW, t.Root,
			kindW, t.Kind,
			profW, prof,
			t.ReviewCycles,
			formatTokensGrouped(t.TotalTokens),
			t.WindowShare*100,
			formatRetroCost(t.CostUSD),
		)
	}
}

func renderRetroWaste(styleHeader, styleLabel, styleDim lipgloss.Style, ws db.RetroWasteSignals) {
	fmt.Println(styleHeader.Render("Waste signals"))
	if !ws.Available {
		// The signal source (spawn_outcome rows) is absent for every session in
		// the window — render "unavailable", distinct from a recorded zero.
		fmt.Println(styleDim.Render("  unavailable (no spawn_outcome rows recorded for this window)"))
		return
	}
	fmt.Printf("  %s %d\n", styleLabel.Render("doom loops:        "), ws.DoomLoopCount)
	fmt.Printf("  %s %d\n", styleLabel.Render("tool errors:       "), ws.ToolErrorCount)
	fmt.Printf("  %s %d\n", styleLabel.Render("permission asks:   "), ws.PermissionAskCount)
	fmt.Printf("  %s %d\n", styleLabel.Render("permission denials:"), ws.PermissionDeniedCount)
}
