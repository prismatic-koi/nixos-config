package review

// Per-agent readiness gate for the review fan-out (#1051 Piece A).
//
// The single-worker `prism spawn` path lets session.SpawnSession run its own
// readiness gate inline, because a single spawn does not benefit from
// concurrency. The review fan-out is different: five agents are spawned
// nearly simultaneously, and a 30-second per-agent gate run sequentially
// would stretch a healthy startup to 30s+ in the worst case.
//
// gateReviewAgents runs session.WaitForReady in one goroutine per
// successfully-spawned agent. The gate completes when every agent has either
// signalled readiness or timed out. Per-agent outcomes are written back into
// the caller-provided spawnErr slice (timeouts produce a *session.ReadinessTimeoutError),
// and the standard cleanup path (KillSidecar + cleanupAgentSession +
// tmux.KillSession) fires for any agent whose gate trips so a subsequent
// spawn with the same name does not see stale state.
//
// Progress callback contract:
//
//   - "<role> started" is emitted when an agent passes its readiness gate.
//     This replaces the old emission point (immediately after SpawnSession
//     returns), making the line a truthful "this agent is up and reachable"
//     signal rather than the optimistic "tmux session was created" signal
//     it used to be.
//
//   - "<role> failed to start: not ready within <timeout>" is emitted when
//     the readiness gate trips on timeout. Other "failed to start: …" forms
//     are emitted by the spawn loop itself (config errors, SpawnSession
//     errors) — the gate only owns the readiness branch.

import (
	"fmt"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// DefaultReviewReadinessTimeout is the per-agent readiness window used by
// gateReviewAgents when the caller does not pass an explicit value. 30s
// matches session.DefaultReadinessTimeout — see that constant for the
// rationale.
const DefaultReviewReadinessTimeout = 30 * time.Second

// gateReviewAgents runs session.WaitForReady concurrently for every agent
// whose spawnErr[i] is nil. agentSessions[i] is the session name to gate;
// spawnTimes[i] is updated on successful gate so the polling phase reports
// elapsed times relative to the moment the agent became ready (matching the
// behaviour of the pre-#1051 code, where spawnTimes was set at SpawnSession-
// return time).
//
// Outcomes are written back into spawnErr — timeouts populate a
// *session.ReadinessTimeoutError, surface via onProgress as "failed to start:
// not ready within …", and trigger the standard cleanup. The caller still
// owns the post-gate logic (which agents to poll, whether allFailed → return
// error, etc.).
//
// timeout ≤ 0 falls back to DefaultReviewReadinessTimeout.
//
// onProgress may be nil. d must not be nil.
func gateReviewAgents(
	d *db.DB,
	agents []Agent,
	agentSessions []string,
	spawnErr []error,
	spawnTimes []time.Time,
	timeout time.Duration,
	onProgress func(string),
) {
	if timeout <= 0 {
		timeout = DefaultReviewReadinessTimeout
	}

	// Mutex protects concurrent writes to spawnErr / spawnTimes /
	// onProgress. The slices are indexed by agent and each goroutine writes
	// only its own slot, but Go's memory model still requires synchronisation
	// to make those writes visible to the reader after wg.Wait(). The
	// onProgress callback is also serialised so output lines do not interleave.
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range agents {
		if spawnErr[i] != nil {
			// Pre-existing spawn failure (config error, SpawnSession error)
			// — the spawn loop has already emitted a "failed to start" line
			// for this agent. Skip the gate.
			continue
		}

		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ag := agents[i]
			agentSession := agentSessions[i]

			readyErr := session.WaitForReady(d, agentSession, timeout)

			mu.Lock()
			defer mu.Unlock()
			if readyErr != nil {
				spawnErr[i] = fmt.Errorf("readiness gate for %s: %w", ag.Name, readyErr)
				if onProgress != nil {
					if session.IsReadinessTimeout(readyErr) {
						onProgress(fmt.Sprintf("%s failed to start: not ready within %s",
							FormatAgentDisplayName(ag.Name), formatReadinessTimeout(timeout)))
					} else {
						onProgress(fmt.Sprintf("%s failed to start: %v",
							FormatAgentDisplayName(ag.Name), readyErr))
					}
				}
				// Clean up the half-alive session so a subsequent spawn with
				// the same name does not see stale state. Mirror the cleanup
				// the spawn loop already performs for SpawnSession failures.
				session.KillSidecar(agentSession)
				cleanupAgentSession(d, agentSession)
				_ = tmux.KillSession(agentSession)
				return
			}
			// Reset spawnTimes[i] to "ready" time so the polling phase
			// reports elapsed durations relative to the moment opencode
			// became reachable (consistent with the pre-#1051 behaviour
			// where the time was captured immediately after SpawnSession
			// returned, which was effectively the same moment).
			spawnTimes[i] = time.Now()
			if onProgress != nil {
				onProgress(fmt.Sprintf("%s started", FormatAgentDisplayName(ag.Name)))
			}
		}(i)
	}

	wg.Wait()
}

// formatReadinessTimeout renders a duration the way the AC text says it
// should appear in the "failed to start: not ready within X" line.
// Mirrors session.formatTimeout — duplicated here because that function is
// unexported in the session package and we don't want to widen its surface
// just for a string format.
func formatReadinessTimeout(d time.Duration) string {
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

// GateReviewAgentsForTest is the test-only export of gateReviewAgents.
// External tests use this to exercise the per-agent readiness gate without
// going through the full Run / RunAsync spawn path. Production code calls
// gateReviewAgents directly.
func GateReviewAgentsForTest(
	d *db.DB,
	agents []Agent,
	agentSessions []string,
	spawnErr []error,
	spawnTimes []time.Time,
	timeout time.Duration,
	onProgress func(string),
) {
	gateReviewAgents(d, agents, agentSessions, spawnErr, spawnTimes, timeout, onProgress)
}
