package cmd

// spawn_wait.go — `prism spawn --wait` blocking poll loop (#1500).
//
// Terminal definition (documented and verified by tests):
//
//   --wait blocks until the spawned agent reaches a terminal state for its
//   FIRST TURN — that is, the initial prompt has been processed to
//   completion (state == "finished") or has failed (state == "error" /
//   "deleted"). It does NOT wait for the entire session to end (which would
//   be hours / days for an interactive coordinator session).
//
// Concretely, the wait succeeds as soon as the session's state transitions
// from "active"/"idle"/"reviewing" to "finished" — the agent's signal that
// the initial prompt's turn loop has completed and the agent is awaiting
// further input. This matches the issue guidance ("wait for the spawned
// agent to finish its initial prompt").
//
// Falls back to "error" / "deleted" as the failure terminals, plus a
// readiness-style guard: if the session never transitions past "active"
// AND no msg_assistant / msg_user / turn_start event is ever recorded
// AND the timeout elapses, we report timeout (the agent is still running;
// the wait simply gave up).
//
// Killing this command does NOT cancel the spawned session — the agent
// continues in its tmux session and (eventually) reaches a terminal state
// of its own. The user can inspect via `prism checkin <session>` or
// `prism sessions list`.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// spawnWaitJSON is the JSON shape emitted by `prism spawn --wait --json`.
// Stable schema — every key is always present.
type spawnWaitJSON struct {
	Session string `json:"session"`
	State   string `json:"state"`  // finished / error / interrupted / deleted / timeout
	Status  string `json:"status"` // duplicate of State for symmetry with merge --wait
	Error   string `json:"error,omitempty"`
}

// spawnTerminalStates lists the agent_status.state values that --wait treats
// as terminal for the initial-prompt definition. "interrupted" is included
// because the user explicitly redirecting an agent counts as "the first turn
// is done" — pollAgents in review/poll.go intentionally excludes interrupted
// from its terminal set, but for `prism spawn --wait` we treat any halt
// (clean or interrupted) as a wait terminal so the command returns instead
// of hanging on a paused agent.
var spawnWaitTerminals = map[string]bool{
	"finished":    true,
	"error":       true,
	"deleted":     true,
	"interrupted": true,
}

// waitForSpawnTerminal polls the prism DB for the named session until its
// state field reaches a terminal value, the timeout elapses, or the user
// interrupts.
//
// Sandbox-aware via newWaitProbe(): in-sandbox callers route reads through
// the sidecar's /sessions/status endpoint so the host's agent_status table
// is visible. Without this, --wait inside a sandbox would poll a tmpfs
// shadow DB and never observe a terminal (issue #1500 review-code feedback).
func waitForSpawnTerminal(sessionName string, jsonMode bool, timeout time.Duration) error {
	probe, err := newWaitProbe()
	if err != nil {
		return fmt.Errorf("prism spawn --wait: %w", err)
	}
	defer probe.Close()

	var lastState string
	pollErr := pollWait(context.Background(), timeout,
		500*time.Millisecond, 5*time.Second,
		func() (bool, error) {
			st, qErr := probe.SessionStatus(sessionName)
			if qErr != nil {
				fmt.Fprintf(os.Stderr, "[prism spawn --wait] probe error: %v (will retry)\n", qErr)
				return false, nil
			}
			if st == nil {
				// Row not yet visible — keep polling. SpawnSession writes
				// the row before returning so this is a brief window at
				// most.
				return false, nil
			}
			lastState = st.State
			if spawnWaitTerminals[st.State] {
				return true, nil
			}
			return false, nil
		})

	switch exitCodeOf(pollErr) {
	case waitExitTimeout:
		_ = emitSpawnWaitTimeout(sessionName, lastState, jsonMode, timeout)
		return newExitErr(waitExitTimeout, "")
	case waitExitUserInterrupt:
		return pollErr
	}
	if pollErr != nil {
		return pollErr
	}
	// Final state lookup uses the same probe so the path stays sandbox-correct.
	finalState, _ := probe.SessionStatus(sessionName)
	return emitSpawnWaitTerminalRow(sessionName, finalState, jsonMode)
}

// emitSpawnWaitTerminal is kept as a thin wrapper for tests that pre-date
// the probe abstraction. Production code calls emitSpawnWaitTerminalRow
// with a pre-fetched *db.Status.
func emitSpawnWaitTerminal(sessionName string, d *db.DB, jsonMode bool) error {
	st, _ := d.CurrentStatus(sessionName)
	return emitSpawnWaitTerminalRow(sessionName, st, jsonMode)
}

// emitSpawnWaitTerminalRow emits the terminal status from an already-fetched
// *db.Status. Returns nil on "finished" (exit 0) or waitExitTerminalFail (2)
// on any other terminal.
func emitSpawnWaitTerminalRow(sessionName string, st *db.Status, jsonMode bool) error {
	if st == nil {
		// Should not happen — we just polled it. Treat as error.
		if jsonMode {
			payload := spawnWaitJSON{Session: sessionName, State: "error", Status: "error", Error: "session row vanished after terminal-state observation"}
			data, _ := json.Marshal(payload)
			_ = printJSON(data)
		} else {
			fmt.Fprintf(os.Stderr, "prism spawn --wait: session %q vanished after terminal-state observation\n", sessionName)
		}
		return newExitErr(waitExitTerminalFail, "")
	}

	state := st.State
	errMsg := ""
	if state == "error" || state == "deleted" {
		errMsg = "agent did not finish cleanly"
	}

	if jsonMode {
		payload := spawnWaitJSON{Session: sessionName, State: state, Status: state, Error: errMsg}
		data, mErr := json.Marshal(payload)
		if mErr != nil {
			return fmt.Errorf("prism spawn --wait: marshal JSON: %w", mErr)
		}
		if pErr := printJSON(data); pErr != nil {
			return pErr
		}
	} else {
		switch state {
		case "finished":
			fmt.Printf("session %q finished.\n", sessionName)
		case "interrupted":
			fmt.Printf("session %q interrupted (the agent paused; resume with `prism prompt %s`).\n", sessionName, sessionName)
		case "error":
			fmt.Printf("session %q ended with state %q.\n", sessionName, state)
		case "deleted":
			fmt.Printf("session %q was cleaned up before reaching a normal terminal.\n", sessionName)
		default:
			fmt.Printf("session %q reached state %q.\n", sessionName, state)
		}
	}
	if state == "finished" {
		return nil
	}
	// "interrupted" is non-zero too — the agent did NOT finish its first
	// turn cleanly. Callers can still recover by sending a prompt.
	return newExitErr(waitExitTerminalFail, "")
}

func emitSpawnWaitTimeout(sessionName, lastState string, jsonMode bool, timeout time.Duration) error {
	if jsonMode {
		payload := spawnWaitJSON{
			Session: sessionName,
			State:   "timeout",
			Status:  "timeout",
			Error:   fmt.Sprintf("waited %s; session is still %q", timeout, lastState),
		}
		data, mErr := json.Marshal(payload)
		if mErr != nil {
			return fmt.Errorf("prism spawn --wait timeout: marshal: %w", mErr)
		}
		return printJSON(data)
	}
	fmt.Fprintf(os.Stderr, "prism spawn --wait: timed out after %s; session %q remains %q.\n",
		formatDurationShort(timeout), sessionName, lastState)
	fmt.Fprintf(os.Stderr, "  Inspect with: prism checkin %s\n", sessionName)
	return nil
}
