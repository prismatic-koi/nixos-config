package review

// agents.go — review agent catalogue and configuration helpers.
//
// This file defines the Agent type, the canonical five-agent review set, and
// the functions that filter, validate, and resolve per-agent configuration.
// None of the code here has any dependency on DB, tmux, or spawn machinery.

import (
	"fmt"
	"strings"
)

// Agent describes a single review agent to run.
type Agent struct {
	// Name is the agent identifier, e.g. "review-goal". Used for session
	// names (~review-N-<Name>), root_agent_name in the DB, progress
	// display, as the AgentRole passed to the sidecar, and as the
	// on-disk filename (<Name>.md) looked up by CheckAgentAvailability.
	Name string
}

// Agents returns the five-agent review set.
func Agents() []Agent {
	return []Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
		{Name: "review-security"},
		{Name: "review-qa"},
		{Name: "review-context"},
	}
}

// RoleValidator is a function that reports whether a given agent role is
// valid for the active harness. Returns nil when valid; an error with a
// descriptive message when invalid. This matches the signature of
// harness.Harness.ValidateAgentRole — callers pass h.ValidateAgentRole
// directly.
type RoleValidator func(role string) error

// CheckAgentAvailability verifies that all given agents are valid for the
// active harness. The validator function should be h.ValidateAgentRole from
// the active harness adapter. Returns a descriptive error listing any
// invalid agents; returns nil when all are valid.
//
// Validation is performed against Agent.Name, which matches the on-disk
// filename (<Name>.md) directly.
//
// This is intentionally skipped in container mode because the check cannot
// reliably inspect the container filesystem.
func CheckAgentAvailability(agents []Agent, validate RoleValidator) error {
	var invalid []string
	var firstErr error
	for _, ag := range agents {
		if err := validate(ag.Name); err != nil {
			invalid = append(invalid, ag.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(invalid) > 0 {
		return fmt.Errorf(
			"review agents not available: %s\n%v",
			strings.Join(invalid, ", "), firstErr,
		)
	}
	return nil
}

// AgentsByName filters the agents slice to only those whose Name is in the
// allowedNames set. Returns an error if any name in allowedNames does not exist
// in agents.
func AgentsByName(agents []Agent, allowedNames []string) ([]Agent, error) {
	available := make(map[string]Agent, len(agents))
	for _, a := range agents {
		available[a.Name] = a
	}
	var result []Agent
	var unknown []string
	for _, name := range allowedNames {
		if a, ok := available[name]; ok {
			result = append(result, a)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--only must be one of: %s (got: %q)",
			strings.Join(agentNames(agents), ", "),
			strings.Join(unknown, ", "),
		)
	}
	return result, nil
}

func agentNames(agents []Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// FormatAgentDisplayName converts an agent name like "review-goal" to a
// display name like "Review-Goal" for progress output lines.
func FormatAgentDisplayName(name string) string {
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// Note (#2854): ResolveAgentConfigContent used to render each reviewer's
// profile slot into a harness-config JSON blob for the retired
// PI_CONFIG_CONTENT / harness-config-file transports. pi read neither. Each
// reviewer's model, provider, and thinking level now reach pi only over argv,
// resolved at agent-run time by populatePIConfig from the profile slot that
// the RequireSlot gate in Run / RunAsync already validates.
