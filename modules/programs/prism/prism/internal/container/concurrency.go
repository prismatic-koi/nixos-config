package container

import "strings"

const (
	// DefaultConcurrencyCap is the maximum number of in-flight agent containers
	// before new spawns are refused. Chosen for a 32 GB host where
	// nixos-config@main (coordinator) and obsidian@main are typically always
	// running, leaving 4 slots for real workers.
	DefaultConcurrencyCap = 6
)

// InFlightSession describes a single in-flight agent session.
type InFlightSession struct {
	// Name is the prism session name (e.g. "nixos-config@feature").
	Name string
	// Role is the inferred role ("coordinator", "worker", or "unknown").
	// Derived from root_agent_name in the DB when available, or inferred
	// from the session name heuristic ("@main" → coordinator).
	Role string
}

// roleFor infers the role label for display. It uses rootAgentName from the DB
// when available; otherwise it falls back to a session-name heuristic.
func roleFor(sessionName string, rootAgentName *string) string {
	if rootAgentName != nil && *rootAgentName != "" {
		return *rootAgentName
	}
	// Heuristic: sessions on the main branch are coordinators.
	if strings.HasSuffix(sessionName, "@main") {
		return "coordinator"
	}
	return "unknown"
}
