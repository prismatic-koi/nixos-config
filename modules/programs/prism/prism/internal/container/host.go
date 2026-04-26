// Package container manages the podman container lifecycle for prism sidecar.
// This file defines hostIsolator, a no-op implementation of the Isolator
// interface for IsolationHost ("host") mode, where opencode runs directly in
// the tmux pane with no sandbox layer.
//
// Option A from A.1 §4.4: hostIsolator is a registered no-op so that every
// caller goes through registry.For(mode) uniformly — no special-case for host.
package container

import (
	"context"

	"github.com/prismatic-koi/prism/internal/config"
)

// hostIsolator implements Isolator for the "host" isolation mode. The agent
// runs directly in the tmux pane with no sandboxing; most lifecycle operations
// are no-ops. AgentRun returns an error because host mode reaches opencode
// directly via the tmux pane command, not via prism agent-run.
type hostIsolator struct {
	// name is the stable session identifier (used for log messages).
	name string
}

// newHostIsolator returns an Isolator that represents the host (no-op) mode
// for the given session name.
func newHostIsolator(name string) Isolator {
	return &hostIsolator{name: name}
}

// Name returns config.IsolationHost — the registry key for this isolator.
func (h *hostIsolator) Name() config.IsolationMode {
	return config.IsolationHost
}

// Capabilities returns the host feature flags: all flags are false because
// host mode runs opencode directly with no sandboxing, no container lifecycle,
// no config-blob injection, and no special sidecar behaviours.
func (h *hostIsolator) Capabilities() Capabilities {
	return Capabilities{
		IsContainer:                false,
		OwnsContainerLifecycle:     false,
		NeedsConfigBlob:            false,
		NeedsHostAPISocket:         false,
		UsesContainerHarness:       false,
		RestartOnExit:              false,
		NeedsStartupConnectTimeout: false,
		NeedsReadinessWait:         false,
		EmitsTmuxStatusColumns:     false,
	}
}

// BuildRunArgs is a no-op for host mode: opencode is launched directly by the
// tmux pane command (BuildOpencodeCmd), not via a Run call.
func (h *hostIsolator) BuildRunArgs() []string {
	return nil
}

// Run is a no-op stub for host mode: opencode is launched directly in the
// tmux pane, so this Isolator's Run path is never reached in the production
// flow.
func (h *hostIsolator) Run(_ context.Context, _ []string) error {
	return nil
}

// Shutdown is a no-op for host mode: the opencode process is owned by the
// tmux pane's process tree and terminates when the pane closes.
func (h *hostIsolator) Shutdown() {}

// HasExited always returns (false, 0) for host mode: the Manager-level
// Isolator never observes the opencode process in the production flow.
func (h *hostIsolator) HasExited() (bool, int) {
	return false, 0
}

// DumpLogs is a no-op for host mode: opencode writes directly to the tmux
// pane terminal; there is no separate log capture path.
func (h *hostIsolator) DumpLogs() {}

func init() {
	MustRegister(config.IsolationHost, func(opts ConstructorOpts) Isolator {
		return newHostIsolator(opts.Name)
	})
}
