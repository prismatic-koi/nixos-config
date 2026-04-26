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

// WaitForReady polls the prism DB for the named session until either:
//
//   - The session has at least one event of type "state_change" (the sidecar
//     successfully connected to opencode's SSE stream and emitted a state
//     transition — see writeStateChange in internal/sidecar/sidecar.go), OR
//   - agent_status.harness_session_id is non-NULL (opencode emitted
//     session.created and the sidecar wrote the session ID), OR
//   - the timeout elapses.
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
	if d == nil {
		return fmt.Errorf("wait for ready: db handle is required")
	}
	if sessionName == "" {
		return fmt.Errorf("wait for ready: session name is required")
	}
	if timeout <= 0 {
		timeout = DefaultReadinessTimeout
	}

	deadline := time.Now().Add(timeout)
	for {
		// Primary signal: any state_change event written by the sidecar means
		// opencode produced at least one SSE event, which can only happen
		// after opencode bound its port and accepted the sidecar's TCP
		// connection. This is the cleanest and earliest readiness marker.
		evts, evtErr := d.QueryEvents(sessionName, 1, nil, nil, []string{"state_change"})
		if evtErr == nil && len(evts) > 0 {
			return nil
		}

		// Secondary signal: harness_session_id set on the agent_status row.
		// The sidecar writes this on session.created, which arrives shortly
		// after server.connected. Useful as a belt-and-braces check in case
		// the state_change query has a transient error or in case future
		// sidecar refactors order the writes differently.
		st, stErr := d.CurrentStatus(sessionName)
		if stErr == nil && st != nil && st.HarnessSessionID != nil && *st.HarnessSessionID != "" {
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
