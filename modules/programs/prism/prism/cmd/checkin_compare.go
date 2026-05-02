package cmd

// checkin_compare.go — side-by-side narrative comparison of two A/B test sessions.
//
// Usage:
//
//	prism checkin --compare <session-a> <session-b>
//	prism checkin --compare --diff <session-a> <session-b>
//
// The two sessions must share a common abtest_pair_id in spawn_inputs.
// Passing two unrelated sessions (no shared pair_id) produces a clear error.
//
// --diff emits a unified text diff of the two narrative views suitable for
// piping into a viewer.
//
// One sibling missing (cleaned up): the surviving session is shown in full
// with a clear "sibling missing" indicator rather than erroring.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

// runCheckinCompare implements prism checkin --compare [--diff] <sessionA> <sessionB>.
func runCheckinCompare(sessionA, sessionB string, diffMode bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("checkin --compare: %w", err)
	}
	defer d.Close()

	// Resolve pair IDs for both sessions.
	pairIDA, err := abtestPairIDForSession(d, sessionA)
	if err != nil {
		return err
	}
	pairIDB, err := abtestPairIDForSession(d, sessionB)
	if err != nil {
		return err
	}

	// Both sessions must share a pair ID.
	if pairIDA == "" || pairIDB == "" || pairIDA != pairIDB {
		return fmt.Errorf(
			"checkin --compare: sessions %q and %q do not share an abtest_pair_id\n"+
				"  Use 'prism checkin <session>' to view a single session.",
			sessionA, sessionB,
		)
	}

	pairID := pairIDA

	// Load structured data for the pair.
	pair, err := archive.LoadAbtestPair(d, pairID)
	if err != nil {
		return fmt.Errorf("checkin --compare: %w", err)
	}

	if diffMode {
		return renderCheckinCompareDiff(d, pair, sessionA, sessionB)
	}
	return renderCheckinCompareSideBySide(d, pair, sessionA, sessionB)
}

// abtestPairIDForSession looks up the abtest_pair_id for a session by name,
// querying spawn_inputs via the sessions table. Returns "" when the session
// has no abtest_pair_id.
func abtestPairIDForSession(d *db.DB, sessionName string) (string, error) {
	// Look up the most recent sessions row for this name.
	sess, err := d.MostRecentSessionForName(sessionName)
	if err != nil {
		return "", fmt.Errorf("checkin --compare: lookup session %q: %w", sessionName, err)
	}
	if sess == nil {
		// Try all historical sessions — the session may be ended.
		sessions, err := d.SessionsByName(sessionName)
		if err != nil {
			return "", fmt.Errorf("checkin --compare: lookup session %q: %w", sessionName, err)
		}
		if len(sessions) == 0 {
			return "", fmt.Errorf("checkin --compare: session %q not found", sessionName)
		}
		sess = &sessions[0]
	}
	si, err := d.SpawnInputsByInstanceID(sess.InstanceID)
	if err != nil {
		return "", fmt.Errorf("checkin --compare: spawn_inputs for %q: %w", sessionName, err)
	}
	if si == nil || si.AbtestPairID == nil {
		return "", nil
	}
	return *si.AbtestPairID, nil
}

// extractAssistantText parses a msg_assistant payload and returns the text field.
func extractAssistantText(rawPayload string) string {
	var ap payload.MsgAssistant
	if err := json.Unmarshal([]byte(rawPayload), &ap); err == nil {
		return ap.Text
	}
	return ""
}

// buildNarrativeLines returns the checkin narrative for a session as a slice
// of lines (without ANSI codes) suitable for text comparison.
func buildNarrativeLines(d *db.DB, sessionName string) []string {
	assistantEvents, err := d.QueryEvents(sessionName, 100, nil, nil, []string{"msg_assistant"})
	if err != nil || len(assistantEvents) == 0 {
		return []string{fmt.Sprintf("[no events for session %s]", sessionName)}
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("=== %s ===", sessionName))

	status, _ := d.CurrentStatus(sessionName)
	state := "unknown"
	if status != nil {
		state = status.State
	}
	lines = append(lines, fmt.Sprintf("state: %s", state))
	lines = append(lines, "")

	for i, ae := range assistantEvents {
		ts := ae.CreatedAt.Local().Format("15:04:05")
		lines = append(lines, fmt.Sprintf("[%s] assistant turn %d", ts, i+1))
		text := extractAssistantText(ae.Payload)
		if text == "" {
			text = "(no text)"
		}
		// Wrap long lines for better diff output.
		for _, l := range strings.Split(text, "\n") {
			lines = append(lines, l)
		}
		lines = append(lines, "")
	}
	lines = append(lines, "── end of event log ──")
	return lines
}

// renderCheckinCompareSideBySide prints a side-by-side view of the two sessions.
func renderCheckinCompareSideBySide(d *db.DB, pair *archive.AbtestPair, sessionA, sessionB string) error {
	fmt.Printf("abtest compare: pair %s\n\n", pair.PairID)

	// Show sibling-missing warnings.
	if pair.MissingA {
		fmt.Printf("⚠  session A: sibling missing (cleaned up)\n\n")
	}
	if pair.MissingB {
		fmt.Printf("⚠  session B: sibling missing (cleaned up)\n\n")
	}

	// Header line showing the two sessions.
	labelA := sessionA
	labelB := sessionB
	if pair.SessionA != nil {
		labelA = pair.SessionA.SessionName
	}
	if pair.SessionB != nil {
		labelB = pair.SessionB.SessionName
	}

	// Print session A's narrative.
	fmt.Printf("──────────────────────────────────────────────────────────────────\n")
	fmt.Printf("SESSION A: %s\n", labelA)
	fmt.Printf("──────────────────────────────────────────────────────────────────\n")
	if pair.MissingA {
		fmt.Println("[sibling missing — session was cleaned up before comparison]")
	} else {
		linesA := buildNarrativeLines(d, labelA)
		for _, l := range linesA {
			fmt.Println(l)
		}
	}

	fmt.Println()

	// Print session B's narrative.
	fmt.Printf("──────────────────────────────────────────────────────────────────\n")
	fmt.Printf("SESSION B: %s\n", labelB)
	fmt.Printf("──────────────────────────────────────────────────────────────────\n")
	if pair.MissingB {
		fmt.Println("[sibling missing — session was cleaned up before comparison]")
	} else {
		linesB := buildNarrativeLines(d, labelB)
		for _, l := range linesB {
			fmt.Println(l)
		}
	}

	// Summary metrics footer.
	fmt.Printf("\n──────────────────────────────────────────────────────────────────\n")
	fmt.Println("SUMMARY METRICS")
	fmt.Printf("──────────────────────────────────────────────────────────────────\n")
	printAbtestSessionMetrics("A", labelA, pair.OutcomeA)
	printAbtestSessionMetrics("B", labelB, pair.OutcomeB)

	return nil
}

// renderCheckinCompareDiff emits a unified diff of the two session narratives.
func renderCheckinCompareDiff(d *db.DB, pair *archive.AbtestPair, sessionA, sessionB string) error {
	labelA := sessionA
	labelB := sessionB
	if pair.SessionA != nil {
		labelA = pair.SessionA.SessionName
	}
	if pair.SessionB != nil {
		labelB = pair.SessionB.SessionName
	}

	var linesA, linesB []string
	if pair.MissingA {
		linesA = []string{"[sibling missing — session was cleaned up before comparison]"}
	} else {
		linesA = buildNarrativeLines(d, labelA)
	}
	if pair.MissingB {
		linesB = []string{"[sibling missing — session was cleaned up before comparison]"}
	} else {
		linesB = buildNarrativeLines(d, labelB)
	}

	// Emit a simple unified diff.
	fmt.Printf("--- a/%s\n", labelA)
	fmt.Printf("+++ b/%s\n", labelB)
	fmt.Println(unifiedDiff(linesA, linesB, 3))
	return nil
}

// printAbtestSessionMetrics prints a summary line for one session.
func printAbtestSessionMetrics(slot, sessionName string, outcome *db.SpawnOutcome) {
	if outcome == nil {
		fmt.Printf("  %s  %-50s  (no metrics)\n", slot, sessionName)
		return
	}
	endState := "—"
	if outcome.EndState != nil {
		endState = *outcome.EndState
	}
	durStr := "—"
	if outcome.DurationMs != nil {
		dur := *outcome.DurationMs
		mins := dur / 60000
		secs := (dur % 60000) / 1000
		if mins > 0 {
			durStr = fmt.Sprintf("%dm%ds", mins, secs)
		} else {
			durStr = fmt.Sprintf("%ds", secs)
		}
	}
	tokIn := outcome.TokensInputTotal
	tokOut := outcome.TokensOutputTotal
	turns := outcome.MsgAssistantCount

	fmt.Printf("  %s  %-40s  state=%-12s  turns=%-4d  in=%-8d  out=%-8d  dur=%s\n",
		slot, sessionName, endState, turns, tokIn, tokOut, durStr)
}

// diffEdit is a single edit operation in a diff: keep (' '), remove ('-'), or add ('+').
type diffEdit struct {
	op   rune
	line string
}

// unifiedDiff produces a simple unified-diff string from two slices of lines.
// context is the number of unchanged context lines to show around changes.
func unifiedDiff(a, b []string, context int) string {
	edits := myersDiff(a, b)

	var sb strings.Builder
	type hunk struct {
		aStart, aLen int
		bStart, bLen int
		lines        []diffEdit
	}

	// Group edits into hunks with context lines.
	var hunks []hunk
	var current *hunk
	aIdx, bIdx := 0, 0
	pendingCtx := 0

	for i, e := range edits {
		switch e.op {
		case ' ':
			if current != nil {
				if pendingCtx < context {
					current.lines = append(current.lines, e)
					current.aLen++
					current.bLen++
					pendingCtx++
				} else {
					// Check if there are more changes nearby.
					hasMoreChanges := false
					for j := i + 1; j < len(edits) && j < i+context+1; j++ {
						if edits[j].op != ' ' {
							hasMoreChanges = true
							break
						}
					}
					if !hasMoreChanges {
						hunks = append(hunks, *current)
						current = nil
						pendingCtx = 0
					} else {
						current.lines = append(current.lines, e)
						current.aLen++
						current.bLen++
					}
				}
			}
			aIdx++
			bIdx++
		case '-':
			if current == nil {
				current = &hunk{aStart: aIdx + 1, bStart: bIdx + 1}
			}
			current.lines = append(current.lines, e)
			current.aLen++
			pendingCtx = 0
			aIdx++
		case '+':
			if current == nil {
				current = &hunk{aStart: aIdx + 1, bStart: bIdx + 1}
			}
			current.lines = append(current.lines, e)
			current.bLen++
			pendingCtx = 0
			bIdx++
		}
	}
	if current != nil {
		hunks = append(hunks, *current)
	}

	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.aStart, h.aLen, h.bStart, h.bLen)
		for _, e := range h.lines {
			fmt.Fprintf(&sb, "%c%s\n", e.op, e.line)
		}
	}
	if sb.Len() == 0 {
		return "(no differences)"
	}
	return sb.String()
}

// myersDiff returns an edit script (sequence of keep/add/remove operations)
// that transforms slice a into slice b using a simple LCS-based approach.
func myersDiff(a, b []string) []diffEdit {
	// Build DP table for LCS.
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				if dp[i+1][j] > dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
	}
	// Traceback.
	var edits []diffEdit
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			edits = append(edits, diffEdit{' ', a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			edits = append(edits, diffEdit{'-', a[i]})
			i++
		} else {
			edits = append(edits, diffEdit{'+', b[j]})
			j++
		}
	}
	for ; i < m; i++ {
		edits = append(edits, diffEdit{'-', a[i]})
	}
	for ; j < n; j++ {
		edits = append(edits, diffEdit{'+', b[j]})
	}
	return edits
}
