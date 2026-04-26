package review

// agents.go — review agent catalogue and configuration helpers.
//
// This file defines the Agent type, the canonical five-agent review set, and
// the functions that filter, validate, and resolve per-agent configuration.
// None of the code here has any dependency on DB, tmux, or spawn machinery.

import (
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
)

// Agent describes a single review agent to run.
type Agent struct {
	// Name is the agent identifier, e.g. "review-goal".
	Name string
	// OpencodeName is the opencode --agent flag value, e.g. "review-goal".
	OpencodeName string
}

// Agents returns the five-agent review set.
// Each agent corresponds to a specialised opencode agent definition under
// modules/programs/prism/opencode/agents/.
func Agents() []Agent {
	return []Agent{
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
		{Name: "review-security", OpencodeName: "review-security"},
		{Name: "review-qa", OpencodeName: "review-qa"},
		{Name: "review-context", OpencodeName: "review-context"},
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
		return nil, fmt.Errorf("unknown agent name(s): %s\navailable: %s",
			strings.Join(unknown, ", "),
			strings.Join(agentNames(agents), ", "),
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

// ResolveAgentConfigContent resolves the per-agent opencode.json config blob
// for a single agent in sandboxed mode (podman, bwrap, or sandbox-exec). It is
// factored out of Run so that it can be unit-tested independently of the
// tmux/DB machinery.
//
// Returns ("", nil) in host mode (isolationMode == "host" or ""), because no
// config injection is needed — opencode is launched directly on the host.
//
// In sandboxed mode (isolationMode == "podman", "bwrap", or "sandbox-exec"):
//   - Returns an error if pf is nil (missing profiles file).
//   - Returns an error if ContainerConfigForRole returns an error.
//   - Returns an error if the resolved blob is empty (stale profiles.json).
//   - Returns the non-empty blob when resolution succeeds.
//
// Exported so that cmd/review_test.go (and integration tests) can exercise the
// config-resolution path without needing a live DB or tmux session.
func ResolveAgentConfigContent(isolationMode string, pf *config.ProfilesFile, agentName string) (string, error) {
	needsConfig := isolationMode == string(config.IsolationPodman) || isolationMode == string(config.IsolationBwrap) || isolationMode == string(config.IsolationSandboxExec)
	if !needsConfig {
		return "", nil
	}
	if pf == nil {
		return "", fmt.Errorf("review: %s mode requires a profiles file to resolve per-agent config for %q; got nil ProfilesFile", isolationMode, agentName)
	}
	blob, cfgErr := config.ContainerConfigForRole(pf, agentName)
	if cfgErr != nil {
		return "", fmt.Errorf("review: ContainerConfigForRole(%q): %w", agentName, cfgErr)
	}
	if blob == "" {
		return "", fmt.Errorf("review: no container config blob for agent %q — profiles.json appears to be stale (missing container_review_*_config fields)\nhint: rebuild the system with the prism NixOS module to regenerate profiles.json", agentName)
	}
	return blob, nil
}
