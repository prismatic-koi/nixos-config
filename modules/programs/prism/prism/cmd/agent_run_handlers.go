package cmd

// Per-session DB status cache for the AgentRun dispatch path. The runAgentRun
// command opens the prism DB once at the top of the
// dispatch, looks up the session status, and stashes it here so the
// registered AgentRun handlers (runAgentRunBwrapHandler,
// runAgentRunSandboxExecHandler) can read it without re-querying the DB.
//
// The cache is populated by runAgentRun before iso.AgentRun is invoked and
// cleared by the same function on return (deferred). It must therefore not
// be shared across invocations of `prism agent-run` — a single agent-run
// process services exactly one session.

import (
	"context"
	"sync"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

var (
	agentRunStatusMu sync.Mutex
	// agentRunStatusCache is keyed by session name. In practice the map
	// contains at most one entry at a time (runAgentRun is single-shot per
	// process); the map shape lets us key cleanly without a global pointer
	// race.
	agentRunStatusCache = map[string]*db.Status{}

	// agentRunOverridesCache caches the CLI override flag values
	// (--model / --variant) for the session being dispatched, so the
	// sandbox-exec handler can read them without re-parsing argv. Same
	// single-entry contract as agentRunStatusCache.
	agentRunOverridesCache = map[string]piOverrides{}
)

// storeAgentRunStatus stashes the looked-up DB status for sessionName so the
// registered AgentRun handler can read it via loadAgentRunStatus. Called
// once by runAgentRun before iso.AgentRun.
func storeAgentRunStatus(sessionName string, status *db.Status) {
	agentRunStatusMu.Lock()
	defer agentRunStatusMu.Unlock()
	agentRunStatusCache[sessionName] = status
}

// loadAgentRunStatus returns the cached DB status for sessionName, or nil
// when no entry has been stored. The handler treats nil as "DB lookup
// failed" and surfaces a clear error.
func loadAgentRunStatus(sessionName string) *db.Status {
	agentRunStatusMu.Lock()
	defer agentRunStatusMu.Unlock()
	return agentRunStatusCache[sessionName]
}

// clearAgentRunStatus removes the cached entry for sessionName. Called via
// defer from runAgentRun so the cache is cleared even when the handler
// returns an error.
func clearAgentRunStatus(sessionName string) {
	agentRunStatusMu.Lock()
	defer agentRunStatusMu.Unlock()
	delete(agentRunStatusCache, sessionName)
	delete(agentRunOverridesCache, sessionName)
}

// storeAgentRunOverrides stashes the CLI overrides parsed by runAgentRun so
// the registered per-mode handler can read them via loadAgentRunOverrides.
// Empty fields mean "no override" and the active profile slot's value is
// used unchanged.
func storeAgentRunOverrides(sessionName string, overrides piOverrides) {
	agentRunStatusMu.Lock()
	defer agentRunStatusMu.Unlock()
	agentRunOverridesCache[sessionName] = overrides
}

// loadAgentRunOverrides returns the cached CLI overrides for sessionName, or
// a zero piOverrides when no entry has been stored. Zero value means
// "no overrides" — the slot value is used unchanged.
func loadAgentRunOverrides(sessionName string) piOverrides {
	agentRunStatusMu.Lock()
	defer agentRunStatusMu.Unlock()
	return agentRunOverridesCache[sessionName]
}

// runAgentRunSandboxExecHandler is the registered AgentRun handler for the
// sandbox-exec isolation mode. It forwards to
// runAgentRunSandboxExec, which is implemented per-platform
// (agent_run_sandbox_exec_darwin.go on Darwin, _other.go elsewhere) and
// owns the kqueue parent-death watcher and the supervised-child lifecycle.
func runAgentRunSandboxExecHandler(ctx context.Context, opts container.AgentRunOpts) error {
	sessionName := opts.SessionName
	status := loadAgentRunStatus(sessionName)
	if status == nil {
		return errAgentRunNoStatus(sessionName)
	}
	return runAgentRunSandboxExec(sessionName, status, opts.StartTime, opts.LogFile)
}
