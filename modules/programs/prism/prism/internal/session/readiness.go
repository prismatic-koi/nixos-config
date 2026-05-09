package session

// Agent readiness gate (#1051 Piece A).
//
// SpawnSession returns success as soon as it has created the tmux session and
// kicked off the sidecar. That is "spawned", not "ready": opencode itself runs
// inside the bwrap/podman/host process the tmux pane launches, several steps
// further along, and may take seconds to bind its TCP port — or never bind at
// all if startup fails silently.
//
// WaitForReady polls the prism DB for an explicit readiness signal: the
// sidecar writes a `state_change` event to agent_events as soon as it has
// successfully connected to opencode's SSE stream and processed the first
// event (see internal/sidecar/sidecar.go HandleEvent → writeStateChange). The
// presence of any such event for the session means "opencode bound its port,
// the sidecar connected, and we have a live SSE stream".
//
// Equivalent earlier signals fall through too: when opencode emits a
// session.created event the sidecar calls UpdateHarnessSessionID, which sets
// agent_status.harness_session_id to a non-NULL value. Either signal is
// sufficient evidence that opencode is up.
//
// The default readiness window is 30 seconds — comfortably longer than the
// healthy-startup latency observed on real-world hardware (sub-second to ~9s,
// see #1051 widening comment) but short enough that a never-coming-up agent
// is surfaced quickly instead of after the 20-minute review monitor timeout.
//
// This primitive is shared between single-worker `prism spawn` (via
// SpawnOpts.ReadinessTimeout) and the parallel review fan-out
// (internal/review/review.go calls WaitForReady directly, in goroutines, so
// per-agent gates run concurrently and one slow agent does not delay the
// others).

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// DefaultReadinessTimeout is the default deadline for WaitForReady. It is
// exported so callers (and tests) can reference the same constant when they
// want the documented default.
//
// 30 seconds covers the observed slow-but-eventually-successful startup
// (~8.5s in the worst case captured in #1051) with a comfortable margin
// while still surfacing never-coming-up agents quickly relative to the
// 20-minute review-monitor timeout.
const DefaultReadinessTimeout = 30 * time.Second

// readinessPollInterval is how often WaitForReady samples the DB. 250ms is
// fast enough that a healthy startup adds tens of ms of overhead at most,
// and slow enough that the DB read load is negligible across a 5-agent
// fan-out (≈20 reads/agent over the median healthy startup).
const readinessPollInterval = 250 * time.Millisecond

// ReadinessTimeoutError indicates that WaitForReady's deadline expired before
// any readiness signal was observed for the session. Callers should treat this
// as a "failed to start" outcome, not a transient error: the spawned sidecar
// is still running and reporting `connection refused` to its own log, but
// opencode itself never came up. The right response is to clean up the half-
// alive session (KillSidecar + cleanupAgentSession + tmux KillSession) and
// surface the failure to the operator.
type ReadinessTimeoutError struct {
	SessionName string
	Timeout     time.Duration
	// Hint, when non-empty, is appended to the error message after the
	// standard "not ready within <timeout>" prefix. SpawnSession sets this
	// in #1064's enrichment path (host mode + unusually large launch
	// command) so the operator sees the prompt-size suspicion alongside
	// the bare timeout message instead of having to grep through logs to
	// discover it. Other call sites leave Hint empty and the message stays
	// unchanged.
	Hint string
}

func (e *ReadinessTimeoutError) Error() string {
	base := fmt.Sprintf("not ready within %s", formatTimeout(e.Timeout))
	if e.Hint == "" {
		return base
	}
	return base + " — " + e.Hint
}

// IsReadinessTimeout reports whether err is (or wraps) a ReadinessTimeoutError.
// Useful for callers that want to render a different message for readiness
// failures versus other spawn errors.
func IsReadinessTimeout(err error) bool {
	var rte *ReadinessTimeoutError
	return errors.As(err, &rte)
}

// formatTimeout renders a duration the same way the AC text says it should
// appear: "30s", "1m30s", etc. We never round to nanoseconds in user-facing
// strings.
func formatTimeout(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

// ReadinessOpts tunes WaitForReadyWithOpts. The zero value reproduces the
// pre-#1507 behaviour (any state_change or harness_session_id signal
// satisfies the gate).
type ReadinessOpts struct {
	// Timeout is the deadline for the gate. ≤ 0 falls back to
	// DefaultReadinessTimeout.
	Timeout time.Duration

	// RequirePromptDelivered, when true, raises the bar for the gate:
	// a bare "state_change → active" event is not sufficient evidence
	// that the spawn succeeded, because the agent may transition to
	// active on harness-handshake completion and then sit idle if the
	// initial prompt was lost between the spawn driver and the agent's
	// input queue (issue #1507 Symptom 2 — the "silently broken" mode
	// where the spawn returns success but the prompt never arrives).
	//
	// When true, the gate additionally requires evidence that the
	// agent is actually processing the prompt:
	//
	//   - a turn_start, msg_user, or msg_assistant event has been
	//     written, OR
	//   - harness_session_id is set (opencode session.created — implies
	//     opencode received the prompt via --prompt CLI flag), OR
	//   - state_change observed a non-"active" terminal transition
	//     ("finished", "interrupted", "error") — the agent ran to
	//     completion or failed in a way that is now visible.
	//
	// Set this when SpawnOpts.Prompt is non-empty.
	RequirePromptDelivered bool
}

// WaitForReady is the legacy entry point for the readiness gate. It is
// equivalent to WaitForReadyWithOpts(d, sessionName, ReadinessOpts{Timeout: timeout})
// — the pre-#1507 "any state_change satisfies the gate" semantics.
//
// New call sites that have an initial prompt should call WaitForReadyWithOpts
// with RequirePromptDelivered: true so the lost-prompt failure mode
// (#1507 Symptom 2) surfaces as a *ReadinessTimeoutError instead of a
// silently-broken "session created" success.
//
// On success returns nil. On timeout returns *ReadinessTimeoutError; check
// with IsReadinessTimeout. DB-error returns from CurrentStatus / QueryEvents
// are treated as transient (the sidecar may still be writing) and the loop
// continues; only the deadline ends the wait.
//
// timeout ≤ 0 falls back to DefaultReadinessTimeout.
//
// This function does not perform any cleanup on timeout — callers own the
// "what to do on failed-to-ready" policy. Single-worker spawns might leave
// the session alive for the operator to debug; review fan-outs typically
// kill it. Both code paths handle the cleanup explicitly via
// KillSidecar + cleanupAgentSession + tmux.KillSession.
func WaitForReady(d *db.DB, sessionName string, timeout time.Duration) error {
	return WaitForReadyWithOpts(d, sessionName, ReadinessOpts{Timeout: timeout})
}

// WaitForReadyWithOpts polls the prism DB for the named session until either:
//
//   - the gate's success condition (depends on opts.RequirePromptDelivered)
//     is met, OR
//   - the timeout elapses.
//
// Default condition (RequirePromptDelivered=false):
//   - any "state_change" event written by the sidecar (the harness
//     handshake/SSE-connection signal), OR
//   - agent_status.harness_session_id is non-NULL.
//
// Strict condition (RequirePromptDelivered=true):
//   - a turn_start / msg_user / msg_assistant event (proves the agent has
//     started processing the prompt), OR
//   - a state_change to a non-"active" terminal state, OR
//   - agent_status.harness_session_id is non-NULL — this fires when opencode
//     emits session.created, which (for opencode in CLI-prompt mode) means
//     opencode parsed --prompt and accepted the message.
//
// On timeout returns *ReadinessTimeoutError. DB-error returns are transient.
func WaitForReadyWithOpts(d *db.DB, sessionName string, opts ReadinessOpts) error {
	if d == nil {
		return fmt.Errorf("wait for ready: db handle is required")
	}
	if sessionName == "" {
		return fmt.Errorf("wait for ready: session name is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultReadinessTimeout
	}

	deadline := time.Now().Add(timeout)
	for {
		if promptReadinessSatisfied(d, sessionName, opts.RequirePromptDelivered) {
			return nil
		}

		if time.Now().After(deadline) {
			return &ReadinessTimeoutError{
				SessionName: sessionName,
				Timeout:     timeout,
			}
		}

		// Sleep for the poll interval, but never overshoot the deadline by
		// more than one tick. (The deadline check above is the only place we
		// return ReadinessTimeoutError, so a tight sleep here keeps the
		// reported wait time honest.)
		remaining := time.Until(deadline)
		sleep := readinessPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// promptReadinessSatisfied returns true when the gate's success condition has
// been met, given whether the strict (prompt-delivered) condition is required.
// Pulled out as a helper so the polling loop in WaitForReadyWithOpts stays
// linear and easy to read.
func promptReadinessSatisfied(d *db.DB, sessionName string, requirePromptDelivered bool) bool {
	// Secondary (and strict-condition) signal: harness_session_id non-NULL.
	// opencode writes session.created → sidecar updates harness_session_id
	// when it parses --prompt and accepts the message; for the strict path
	// this is enough proof the prompt landed. For the loose path it is
	// belt-and-braces alongside state_change.
	st, stErr := d.CurrentStatus(sessionName)
	if stErr == nil && st != nil && st.HarnessSessionID != nil && *st.HarnessSessionID != "" {
		return true
	}

	if !requirePromptDelivered {
		// Loose path: any state_change is sufficient. This is the legacy
		// behaviour and the path used by callers that have no prompt to
		// deliver (or that defer the prompt-delivery question to a later
		// stage, e.g. the review-monitor's per-agent state poll).
		evts, evtErr := d.QueryEvents(sessionName, 1, nil, nil, []string{"state_change"})
		if evtErr == nil && len(evts) > 0 {
			return true
		}
		return false
	}

	// Strict path: require evidence the agent is actually processing.
	//
	// Primary: turn_start / msg_user / msg_assistant event. Any of these
	// only fires after the agent has consumed the prompt and entered the
	// turn loop, so they are unambiguous "prompt delivered" markers.
	procEvts, procErr := d.QueryEvents(sessionName, 1, nil, nil, []string{"turn_start", "msg_user", "msg_assistant"})
	if procErr == nil && len(procEvts) > 0 {
		return true
	}

	// Secondary strict-path signal: a state_change to a NON-"active" state.
	// "active" is written on harness handshake — even when the prompt was
	// lost — so it is not on its own evidence of prompt delivery. Any
	// transition past "active" ("finished" on completion, "interrupted" /
	// "error" on failure) is unambiguous.
	stateEvts, stateErr := d.QueryEvents(sessionName, 50, nil, nil, []string{"state_change"})
	if stateErr == nil {
		for _, e := range stateEvts {
			if !payloadIsBareActive(e.Payload) {
				return true
			}
		}
	}
	return false
}

// payloadIsBareActive reports whether a state_change payload is the bare
// "agent transitioned to active" event. Returns true only when the payload
// parses as {"state":"active"} — anything else (idle, finished, error,
// interrupted, parse failure) is treated as "not bare active" and counts as
// progress evidence. Conservative on parse failure: an unparseable payload
// is assumed to be progress (rather than risk the gate hanging).
func payloadIsBareActive(payload string) bool {
	var p struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return p.State == "active"
}
