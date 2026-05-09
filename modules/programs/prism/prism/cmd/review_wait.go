package cmd

// review_wait.go — `prism review --wait` blocking poll loop (#1500).
//
// After RunAsync spawns the review group and returns, --wait polls the prism
// DB for group completion. Termination is determined by db.GroupCompleted —
// the same condition the async monitor process uses — so the wait loop sees
// exactly the same terminal as the notification path. We do not duplicate
// the monitor's verdict-aggregation logic; we re-use db.GroupResults +
// review.ExtractAssistantText + review.AssessPassed to reach the same
// PASS/FAIL/NONE verdicts the notification message would carry.
//
// The wait loop is a thin observer: killing it does NOT cancel the review
// (the agents keep running, the monitor keeps watching). The user can re-run
// `prism review <pr>` (without --wait) to get the notification when it
// arrives, or read `prism reviews list` to find the group.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// reviewWaitJSON is the JSON shape emitted by `prism review --wait --json`.
// Stable schema — every key is always present.
type reviewWaitJSON struct {
	PR      string                 `json:"pr"`
	GroupID string                 `json:"group_id"`
	Verdict string                 `json:"verdict"` // "PASS" | "FAIL" | "TIMEOUT" | "NO_VERDICT"
	Agents  []reviewWaitAgentJSON  `json:"agents"`
	Status  string                 `json:"status"`  // mirrors Verdict for symmetry with merge --wait
}

type reviewWaitAgentJSON struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	State   string `json:"state"`   // finished / error / interrupted / deleted / unknown
	Verdict string `json:"verdict"` // PASS / FAIL / NONE
	Error   string `json:"error,omitempty"`
}

// waitForReviewTerminal polls the review group until db.GroupCompleted
// returns true, then aggregates per-agent verdicts and emits the result.
//
// Exit codes:
//
//   - all-PASS: 0
//   - any-FAIL or any-NONE / no-start: waitExitTerminalFail (2)
//   - timeout:                          waitExitTimeout (3)
//   - user interrupt:                   waitExitUserInterrupt (4)
func waitForReviewTerminal(prNumber, groupID string, jsonMode bool, timeout time.Duration) error {
	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("prism review --wait: open db: %w", dbErr)
	}
	defer d.Close()

	pollErr := pollWait(context.Background(), timeout,
		1*time.Second, 10*time.Second,
		func() (bool, error) {
			done, gErr := d.GroupCompleted(groupID)
			if gErr != nil {
				fmt.Fprintf(os.Stderr, "[prism review --wait] db error: %v (will retry)\n", gErr)
				return false, nil
			}
			return done, nil
		})

	switch exitCodeOf(pollErr) {
	case waitExitTimeout:
		_ = emitReviewWaitTimeout(prNumber, groupID, d, jsonMode, timeout)
		return newExitErr(waitExitTimeout, "")
	case waitExitUserInterrupt:
		return pollErr
	}
	if pollErr != nil {
		return pollErr
	}

	// Group is complete. Aggregate verdicts.
	return emitReviewWaitTerminal(prNumber, groupID, d, jsonMode)
}

// emitReviewWaitTerminal aggregates per-agent verdicts from db.GroupResults
// and emits a JSON object or a textual summary, then returns 0 on all-PASS
// or waitExitTerminalFail (2) on any FAIL / NONE / missing.
func emitReviewWaitTerminal(prNumber, groupID string, d *db.DB, jsonMode bool) error {
	members, mErr := d.GroupResults(groupID)
	if mErr != nil {
		return fmt.Errorf("prism review --wait: aggregate group results: %w", mErr)
	}
	// We also need the full member list (including ended_at-set rows that
	// GroupResults intentionally drops) so the JSON output reports every
	// agent that was spawned, even when one was cleaned up mid-run.
	allMembers, allErr := d.GroupMembersForGroup(groupID)
	if allErr != nil {
		return fmt.Errorf("prism review --wait: list group members: %w", allErr)
	}

	allPass := true
	rows := make([]reviewWaitAgentJSON, 0, len(allMembers))
	for _, m := range allMembers {
		row := reviewWaitAgentJSON{
			Agent:   agentNameFromSession(m.SessionName),
			Session: m.SessionName,
			State:   m.State,
		}
		if mr, ok := members[m.SessionName]; ok {
			switch mr.State {
			case "finished":
				if mr.LastMessage == "" {
					row.Verdict = "NONE"
					row.Error = "no output produced"
					allPass = false
				} else {
					text := review.ExtractAssistantText(mr.LastMessage)
					passed, kind := review.AssessPassed(text)
					switch {
					case passed:
						row.Verdict = "PASS"
					case kind == review.VerdictFail:
						row.Verdict = "FAIL"
						allPass = false
					default:
						row.Verdict = "NONE"
						row.Error = "no recognised verdict in agent output"
						allPass = false
					}
				}
			case "error":
				row.Verdict = "FAIL"
				row.Error = "agent reached error state"
				if mr.StartupError != "" {
					row.Error = "startup error: " + mr.StartupError
				}
				allPass = false
			default:
				row.Verdict = "NONE"
				allPass = false
			}
		} else {
			// Member was cleaned up before result aggregation; treat as failure.
			row.Verdict = "NONE"
			row.Error = "session not present in group results (likely cleaned up mid-run)"
			allPass = false
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		// No members were ever spawned — definitely a fail.
		allPass = false
	}

	verdict := "PASS"
	if !allPass {
		verdict = "FAIL"
	}

	if jsonMode {
		payload := reviewWaitJSON{
			PR:      prNumber,
			GroupID: groupID,
			Verdict: verdict,
			Agents:  rows,
			Status:  verdict,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("prism review --wait: marshal JSON: %w", err)
		}
		if pErr := printJSON(data); pErr != nil {
			return pErr
		}
	} else {
		if allPass {
			fmt.Printf("\nReview PR #%s — verdict: PASS (%d/%d agents passed)\n", prNumber, len(rows), len(rows))
		} else {
			passN := 0
			for _, r := range rows {
				if r.Verdict == "PASS" {
					passN++
				}
			}
			fmt.Printf("\nReview PR #%s — verdict: FAIL (%d/%d agents passed)\n", prNumber, passN, len(rows))
		}
		for _, r := range rows {
			if r.Error != "" {
				fmt.Printf("  • %s — %s [%s]: %s\n", r.Agent, r.Verdict, r.State, r.Error)
			} else {
				fmt.Printf("  • %s — %s [%s]\n", r.Agent, r.Verdict, r.State)
			}
		}
	}

	if allPass {
		return nil
	}
	return newExitErr(waitExitTerminalFail, "")
}

func emitReviewWaitTimeout(prNumber, groupID string, d *db.DB, jsonMode bool, timeout time.Duration) error {
	allMembers, _ := d.GroupMembersForGroup(groupID)
	rows := make([]reviewWaitAgentJSON, 0, len(allMembers))
	for _, m := range allMembers {
		rows = append(rows, reviewWaitAgentJSON{
			Agent:   agentNameFromSession(m.SessionName),
			Session: m.SessionName,
			State:   m.State,
			Verdict: "PENDING",
		})
	}
	if jsonMode {
		payload := reviewWaitJSON{
			PR:      prNumber,
			GroupID: groupID,
			Verdict: "TIMEOUT",
			Agents:  rows,
			Status:  "timeout",
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("prism review --wait timeout: marshal: %w", err)
		}
		return printJSON(data)
	}
	fmt.Fprintf(os.Stderr, "prism review --wait: timed out after %s; review continues running.\n", formatDurationShort(timeout))
	fmt.Fprintf(os.Stderr, "  Track progress with: prism reviews list\n")
	return nil
}

// agentNameFromSession recovers the per-agent role from a review session name
// of the form "<parent>~review-<N>-<agent>". Falls back to the full session
// name when no match is found.
func agentNameFromSession(sessionName string) string {
	m := reviewSessionPattern.FindStringSubmatch(sessionName)
	if len(m) == 4 {
		return m[3]
	}
	return sessionName
}
