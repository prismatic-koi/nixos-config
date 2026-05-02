package cmd

// stats_abtest.go — prism stats --abtest listing of all abtest pairs.
//
// Shows one row per abtest pair with summary metrics:
//   - Profile A vs Profile B (from spawn_inputs)
//   - Token usage per session
//   - Wall time per session
//   - Turn count per session
//   - Outcome (both finished, one errored, one still running, etc.)

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/db"
)

// runStatsAbtestFlag implements prism stats --abtest (the --abtest top-level flag,
// distinct from `prism stats abtest <group_id>` which is runStatsAbtest in stats_compare.go).
func runStatsAbtestFlag() error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats --abtest: %w", err)
	}
	defer d.Close()

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		return fmt.Errorf("stats --abtest: %w", err)
	}

	if len(pairs) == 0 {
		fmt.Println("no abtest pairs recorded")
		fmt.Println("  Use 'prism spawn --abtest <profileA> <profileB> --prompt \"...\"' to create a pair.")
		return nil
	}

	renderAbtestPairsTable(pairs)
	return nil
}

// renderAbtestPairsTable renders the abtest pairs table to stdout.
func renderAbtestPairsTable(pairs []db.AbtestPairRow) {
	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleBold := lipgloss.NewStyle().Bold(true)

	fmt.Println(styleBold.Render("A/B Test Pairs"))
	fmt.Println()

	const (
		wPairID   = 12
		wSession  = 36
		wProfile  = 16
		wTurns    = 6
		wTokIn    = 9
		wTokOut   = 9
		wDur      = 9
		wState    = 12
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s  %-*s",
		wPairID, "PAIR",
		wSession, "SESSION",
		wProfile, "PROFILE",
		wTurns, "TURNS",
		wTokIn, "TOK-IN",
		wTokOut, "TOK-OUT",
		wDur, "DURATION",
		wState, "STATE",
	)
	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, p := range pairs {
		pairShort := p.PairID
		if len(pairShort) > wPairID {
			pairShort = pairShort[:wPairID]
		}

		// Print two rows per pair (A then B).
		printAbtestPairRow(pairShort, p.SessionNameA, p.ProfileA, p.TurnsA, p.TokensInputA, p.TokensOutputA, p.DurationMsA, p.EndStateA, wPairID, wSession, wProfile, wTurns, wTokIn, wTokOut, wDur, wState)
		if p.SessionNameB != "" {
			printAbtestPairRow("↳", p.SessionNameB, p.ProfileB, p.TurnsB, p.TokensInputB, p.TokensOutputB, p.DurationMsB, p.EndStateB, wPairID, wSession, wProfile, wTurns, wTokIn, wTokOut, wDur, wState)
		} else {
			fmt.Printf("%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s  %-*s\n",
				wPairID, "↳",
				wSession, "(sibling missing)",
				wProfile, "—",
				wTurns, "—",
				wTokIn, "—",
				wTokOut, "—",
				wDur, "—",
				wState, "—",
			)
		}
		fmt.Println()
	}
}

func printAbtestPairRow(label, sessionName, profile string, turns *int, tokIn, tokOut, durMs *int64, endState *string, wPairID, wSession, wProfile, wTurns, wTokIn, wTokOut, wDur, wState int) {
	sessName := sessionName
	if len(sessName) > wSession {
		sessName = sessName[:wSession-3] + "..."
	}

	prof := profile
	if prof == "" {
		prof = "(none)"
	}
	if len(prof) > wProfile {
		prof = prof[:wProfile-3] + "..."
	}

	turnsStr := "—"
	if turns != nil {
		turnsStr = fmt.Sprintf("%d", *turns)
	}

	tokInStr := "—"
	if tokIn != nil && *tokIn > 0 {
		tokInStr = formatTokenCount(int(*tokIn))
	}
	tokOutStr := "—"
	if tokOut != nil && *tokOut > 0 {
		tokOutStr = formatTokenCount(int(*tokOut))
	}

	durStr := "—"
	if durMs != nil && *durMs > 0 {
		d := time.Duration(*durMs) * time.Millisecond
		durStr = formatDurationLong(d)
	}

	stateStr := "—"
	if endState != nil && *endState != "" {
		stateStr = *endState
	}

	fmt.Printf("%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s  %-*s\n",
		wPairID, label,
		wSession, sessName,
		wProfile, prof,
		wTurns, turnsStr,
		wTokIn, tokInStr,
		wTokOut, tokOutStr,
		wDur, durStr,
		wState, stateStr,
	)
}
