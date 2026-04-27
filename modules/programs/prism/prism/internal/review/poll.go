package review

// poll.go — agent polling and result construction.
//
// pollAgents polls the DB until all agents reach a terminal state or the
// timeout expires. buildResults constructs AgentResult entries from polling
// outcomes. Both are called from Run() in run.go; BuildResults is the exported
// entry point for tests.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// pollAgents polls the DB until all agents reach a terminal state or the
// timeout expires. Uses db.GroupCompleted(groupID) as the primary termination
// signal — this replaces the prior name-prefix-based approach (#860, Issue E).
//
// Per-agent progress tracking (onProgress callbacks for "finished" and "timed
// out" events) still checks individual session states so that progress lines
// are emitted at the right moment.
//
// spawnTimes[i] is the time agent i was spawned; used to compute durations.
// onProgress may be nil.
func pollAgents(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string), groupID string) ([]AgentResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	finished := make([]bool, len(agents))
	timedOut := make([]bool, len(agents))

	const pollInterval = 2 * time.Second

	for {
		// Primary termination check: ask the DB whether all group members
		// have reached a terminal state. This is the group-id-based path
		// introduced by #860 (Issue E).
		groupDone, groupErr := d.GroupCompleted(groupID)
		if groupErr != nil {
			// DB error — log and fall through to per-agent checks below
			// so progress tracking still works.
			fmt.Fprintf(os.Stderr, "[prism review] warning: GroupCompleted(%s): %v\n", groupID, groupErr)
		}

		// Even when groupDone is true, scan agents once more to emit any
		// remaining progress lines for agents whose terminal transition
		// hasn't been reported yet.
		for i, agentSession := range agentSessions {
			if finished[i] || timedOut[i] {
				continue
			}
			status, err := d.CurrentStatus(agentSession)
			if err != nil {
				continue
			}
			if status != nil && isTerminalState(status.State) {
				finished[i] = true
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s finished in %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			} else if time.Now().After(deadline) {
				timedOut[i] = true
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s timed out after %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			}
		}

		// Check context cancellation.
		select {
		case <-ctx.Done():
			for i := range agents {
				if !finished[i] && !timedOut[i] {
					timedOut[i] = true
				}
			}
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true, groupID), ctx.Err()
		default:
		}

		// If the group is completed (all members terminal), we're done.
		if groupDone && groupErr == nil {
			break
		}

		if time.Now().After(deadline) {
			// Mark all remaining as timed out and emit progress lines.
			for i := range agents {
				if !finished[i] && !timedOut[i] {
					timedOut[i] = true
					if onProgress != nil {
						elapsed := time.Since(spawnTimes[i])
						onProgress(fmt.Sprintf("%s timed out after %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
					}
				}
			}
			break
		}

		// Sleep while remaining responsive to context cancellation.
		select {
		case <-ctx.Done():
			for i := range agents {
				if !finished[i] {
					timedOut[i] = true
				}
			}
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true, groupID), ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, false, groupID), nil
}

// isTerminalState returns true if the state is considered terminal (finished or error).
func isTerminalState(state string) bool {
	switch state {
	case "finished", "interrupted", "error":
		return true
	}
	return false
}

// BuildResults is the exported entry point for tests. See buildResults.
// Production code calls buildResults directly (via pollAgents).
func BuildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool, groupID string) []AgentResult {
	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, cancelled, groupID)
}

// buildResults constructs AgentResult entries from polling outcomes.
// finished[i] is true when agent i reached a terminal DB state; timedOut[i] is
// true when it did not finish before the deadline; cancelled is true on context
// cancellation (e.g. SIGINT).
//
// When groupID is non-empty, result data (terminal state and last assistant
// message) is fetched via db.GroupResults(groupID) in a single batch query.
// When groupID is empty (tests that pre-date the group wiring), per-session
// individual queries are used as a fallback.
//
// Layer 3 (fail-safe): when cancelled is true, no result may have Passed=true.
// Every result is either IsError=true or annotated as incomplete. This prevents
// a false PASS even if layers 1 or 2 develop regressions later.
//
// Layer 2: agents whose DB terminal state is "interrupted" or "error" always
// produce an error result, regardless of any msg_assistant events they may have.
// Only agents that reached the "finished" state cleanly proceed to AssessPassed.
func buildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool, groupID string) []AgentResult {
	// Pre-fetch group member data when a group_id is available. This replaces
	// individual per-session CurrentStatus + QueryEvents calls with a single
	// batch query via GroupResults (#860, Issue E).
	var groupData map[string]db.GroupMemberResult
	if groupID != "" {
		var grErr error
		groupData, grErr = d.GroupResults(groupID)
		if grErr != nil {
			// Non-fatal: fall back to per-session queries below.
			fmt.Fprintf(os.Stderr, "[prism review] warning: GroupResults(%s): %v — falling back to per-session queries\n", groupID, grErr)
			groupData = nil
		}
	}

	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := agentSessions[i]

		// Layer 3 fail-safe: when the whole review was cancelled (SIGINT),
		// never produce a Passed=true result for any agent. Agents that did not
		// finish are timed-out/cancelled; agents that did "finish" before the
		// signal arrived are annotated as incomplete rather than trusted.
		if cancelled {
			if !finished[i] {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: review cancelled before agent completed (waited %s)", formatDuration(timeout)),
					IsError: true,
				}
			} else {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: review cancelled mid-run — result may be incomplete",
					IsError: true,
				}
			}
			continue
		}

		// Agent did not finish before the timeout deadline.
		if timedOut[i] {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: timed out after %s", formatDuration(timeout)),
				IsError: true,
			}
			continue
		}

		// Resolve state and last message: prefer GroupResults batch data when
		// available, fall back to individual DB queries.
		var agentState string
		var lastPayload string
		if mr, ok := groupData[agentSession]; ok {
			agentState = mr.State
			lastPayload = mr.LastMessage
		} else {
			// Fallback: individual per-session queries.
			status, stErr := d.CurrentStatus(agentSession)
			if stErr == nil && status != nil {
				agentState = status.State
			}
			events, evtErr := d.QueryEvents(agentSession, 1, nil, nil, []string{"msg_assistant"})
			if evtErr == nil && len(events) > 0 {
				lastPayload = events[len(events)-1].Payload
			}
		}

		// Layer 2: check the agent's actual DB terminal state. Only "finished"
		// is considered a clean completion; "interrupted" and "error" are errors
		// regardless of what msg_assistant events may exist.
		if agentState == "interrupted" || agentState == "error" {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent did not complete cleanly (state: %s)", agentState),
				IsError: true,
			}
			continue
		}

		// Read last msg_assistant event.
		if lastPayload == "" {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: no output produced",
				IsError: true,
			}
			continue
		}

		// Extract text from the last msg_assistant event.
		// Layer 1 applies here: AssessPassed requires an explicit positive marker.
		text := extractAssistantText(lastPayload)
		passed, kind := AssessPassed(text)

		if !passed && kind == VerdictNone {
			// Agent finished cleanly but emitted no recognisable verdict.
			// Surface the output as an error so a human can judge.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: no verdict found in agent output — review output:\n" + text,
				IsError: true,
			}
			continue
		}

		results[i] = AgentResult{
			Agent:  ag,
			Passed: passed,
			Output: text,
		}
	}
	return results
}

// isTerminalAgentState mirrors the terminalStates set in internal/db/db.go
// (finished, interrupted, error, deleted). Duplicated here so the review
// package does not have to widen the db package's exported surface for a
// single check.
func isTerminalAgentState(state string) bool {
	switch state {
	case "finished", "interrupted", "error", "deleted":
		return true
	}
	return false
}
