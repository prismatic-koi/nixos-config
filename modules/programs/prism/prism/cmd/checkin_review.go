package cmd

// checkin_review.go — review-round summary paths:
//   - runCheckinReviewRoundsByGroup: DB-backed path (post-migration sessions)
//   - runCheckinReviewRounds: legacy name-prefix scan (pre-migration fallback)

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// runCheckinReviewRoundsByGroup is the DB-backed replacement for
// runCheckinReviewRounds. Instead of using a name-prefix scan, it queries
// session_groups.parent_session to find all review agent sessions belonging to
// groups owned by parentSession.
//
// This is the authoritative path for post-migration sessions where group_id is
// populated. Falls back to the name-prefix path when no group members are found.
func runCheckinReviewRoundsByGroup(parentSession string, verbose bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("checkin review (group): open db: %w", err)
	}
	defer d.Close()

	members, err := d.GroupMembersForParent(parentSession)
	if err != nil {
		return fmt.Errorf("checkin review (group): query members: %w", err)
	}

	if len(members) == 0 {
		// No group members — fall back to the legacy name-prefix scan.
		fmt.Fprintf(os.Stderr, "[deprecation] checkin: no group members found for %q via DB — falling back to name-prefix scan\n", parentSession)
		return runCheckinReviewRounds(parentSession+"~review", verbose)
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Printf("%s\n\n", styleBold.Render(fmt.Sprintf("review sessions for %s (%d agent(s))", parentSession, len(members))))

	// Sort by session name for deterministic output.
	for i := 1; i < len(members); i++ {
		key := members[i]
		j := i - 1
		for j >= 0 && members[j].SessionName > key.SessionName {
			members[j+1] = members[j]
			j--
		}
		members[j+1] = key
	}

	for _, m := range members {
		label := m.SessionName
		if m.RootAgentName != nil && *m.RootAgentName != "" {
			label = *m.RootAgentName
		}
		fmt.Printf("  %s\n", styleBold.Render(label))
		stateStyled := stateStyle(m.State).Render(m.State)
		fmt.Printf("  state: %s\n", stateStyled)

		if verbose {
			if err := runCheckinSession(m.SessionName, 20, nil, nil, nil, true); err != nil {
				fmt.Fprintf(os.Stderr, "  [error reading session %s: %v]\n", m.SessionName, err)
			}
		} else {
			events, qerr := d.QueryEvents(m.SessionName, 1, nil, nil, []string{"msg_assistant"})
			if qerr != nil || len(events) == 0 {
				fmt.Println(styleDim.Render("  (no output recorded)"))
			} else {
				e := events[len(events)-1]
				ts := e.CreatedAt.Local().Format("15:04:05")
				var ap struct {
					Text string `json:"text"`
				}
				text := e.Payload
				if jerr := json.Unmarshal([]byte(e.Payload), &ap); jerr == nil && ap.Text != "" {
					text = ap.Text
				}
				fmt.Printf("  [%s]\n  %s\n", ts, strings.ReplaceAll(text, "\n", "\n  "))
			}
		}
		fmt.Println()
	}

	return nil
}

// runCheckinReviewRounds handles `prism checkin <parent>~review` — a prefix
// match that lists all review agent sessions for the parent, grouped by round.
//
// Supports both the new per-agent session shape (PR-C):
//
//	<parent>~review-<N>-<agent>
//
// and old-shape round sessions (pre-PR-C):
//
//	<parent>~review-<N>          (pure integer suffix — old round session)
//	<parent>~review-<N>~<agent>  (old agent sub-session)
//
// Default mode: one summary entry per agent session (last msg_assistant output).
// Verbose mode (--verbose): shows the full per-agent conversation.
func runCheckinReviewRounds(reviewPrefix string, verbose bool) error {
	// reviewPrefix is e.g. "nixos-config@feature~review" — agent sessions
	// are "nixos-config@feature~review-1-review-goal", etc.
	roundPrefix := reviewPrefix + "-"

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("checkin review: open db: %w", err)
	}
	defer d.Close()

	// Find all sessions (active and ended) whose name starts with roundPrefix.
	all, err := d.AllStatusesWithPrefix(roundPrefix)
	if err != nil {
		return fmt.Errorf("checkin review: query db: %w", err)
	}

	if len(all) == 0 {
		fmt.Printf("no review sessions found matching %q\n", reviewPrefix)
		return nil
	}

	// Classify sessions and group by round number.
	// New shape: <prefix>N-<agent>  → roundN = N, label = ~review-N-<agent>
	// Old round: <prefix>N          → roundN = N, label = ~review-N
	// Old agent: <prefix>N~<agent>  → roundN = N, label = ~review-N~<agent>
	type agentEntry struct {
		sessionName string
		label       string
		state       string
	}
	roundAgents := make(map[int][]agentEntry) // round number → agent sessions
	var roundNums []int
	seenRounds := make(map[int]bool)

	for _, s := range all {
		suffix := strings.TrimPrefix(s.SessionName, roundPrefix)
		// suffix examples:
		//   new shape:  "1-review-goal"
		//   old round:  "1"
		//   old agent:  "1~review-goal"

		var roundN int
		var agentLabel string

		if dashIdx := strings.Index(suffix, "-"); dashIdx > 0 {
			// New shape: N-<agent-name> (no ~ in agent part).
			nStr := suffix[:dashIdx]
			n, convErr := strconv.Atoi(nStr)
			if convErr != nil {
				continue // not a recognised shape
			}
			agentPart := suffix[dashIdx+1:]
			if strings.Contains(agentPart, "~") {
				continue // old agent sub-session (N-something with ~)
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1-review-goal
		} else if tildeIdx := strings.Index(suffix, "~"); tildeIdx > 0 {
			// Old agent sub-session shape: N~<agent>.
			nStr := suffix[:tildeIdx]
			n, convErr := strconv.Atoi(nStr)
			if convErr != nil {
				continue
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1~review-goal
		} else {
			// Old round session shape: pure integer N.
			n, convErr := strconv.Atoi(suffix)
			if convErr != nil {
				continue
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1
		}

		if !seenRounds[roundN] {
			seenRounds[roundN] = true
			roundNums = append(roundNums, roundN)
		}
		roundAgents[roundN] = append(roundAgents[roundN], agentEntry{
			sessionName: s.SessionName,
			label:       agentLabel,
			state:       s.State,
		})
	}

	// Sort round numbers ascending.
	for i := 1; i < len(roundNums); i++ {
		key := roundNums[i]
		j := i - 1
		for j >= 0 && roundNums[j] > key {
			roundNums[j+1] = roundNums[j]
			j--
		}
		roundNums[j+1] = key
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	for _, roundN := range roundNums {
		agents := roundAgents[roundN]

		// Sort agents within the round alphabetically by label.
		for i := 1; i < len(agents); i++ {
			key := agents[i]
			j := i - 1
			for j >= 0 && agents[j].label > key.label {
				agents[j+1] = agents[j]
				j--
			}
			agents[j+1] = key
		}

		fmt.Printf("%s\n\n", styleBold.Render(fmt.Sprintf("round %d (%d session(s))", roundN, len(agents))))

		for _, ag := range agents {
			fmt.Printf("  %s\n", styleBold.Render(ag.label))
			if ag.state != "" {
				stateStyled := stateStyle(ag.state).Render(ag.state)
				fmt.Printf("  state: %s\n", stateStyled)
			}

			if verbose {
				// Show full conversation for this agent session.
				if err := runCheckinSession(ag.sessionName, 20, nil, nil, nil, true); err != nil {
					fmt.Fprintf(os.Stderr, "  [error reading session %s: %v]\n", ag.label, err)
				}
			} else {
				// Show summary: last msg_assistant event.
				events, qerr := d.QueryEvents(ag.sessionName, 1, nil, nil, []string{"msg_assistant"})
				if qerr != nil || len(events) == 0 {
					fmt.Println(styleDim.Render("  (no output recorded)"))
				} else {
					e := events[len(events)-1]
					ts := e.CreatedAt.Local().Format("15:04:05")
					var ap struct {
						Text string `json:"text"`
					}
					text := e.Payload
					if jerr := json.Unmarshal([]byte(e.Payload), &ap); jerr == nil && ap.Text != "" {
						text = ap.Text
					}
					fmt.Printf("  [%s]\n  %s\n", ts, strings.ReplaceAll(text, "\n", "\n  "))
				}
			}
			fmt.Println()
		}

		fmt.Println(styleDim.Render("── end of round ──"))
		fmt.Println()
	}

	return nil
}


