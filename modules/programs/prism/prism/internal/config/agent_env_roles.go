// Package config — agent_env_roles.go
//
// Role-aware filtering of the profile-level AgentEnvVars map (issue #2533).
//
// AgentEnvVars (profiles.json `agent_env_vars`) is a single flat map shared by
// every session role. Some entries are capability gates: the pi grafana MCP
// extension registers its 65-tool surface if — and only if — it finds
// GRAFANA_MCP_CONFIG_PATH and PI_GRAFANA_MCP_BIN in its environment. Review
// agents run on a read-only worktree and never call a grafana tool, so the
// surface costs about 26400 cached tokens per sandboxed review agent and buys
// nothing.
//
// The filter runs UPSTREAM of the isolator, by design:
//
//   - The isolator (internal/container) stays role-agnostic. It emits every
//     key present in the map it receives — the invariant that
//     internal/container/env_test.go pins since issue #2235.
//   - Role policy lives here, at the point where the map is built, so the
//     bwrap dispatch (cmd/agent_run.go), the sandbox-exec dispatch
//     (cmd/agent_run_sandbox_exec_darwin.go), the host agent-only layout
//     (internal/session/spawn.go), and the host full layout (cmd/spawn.go)
//     all resolve the same map for the same role.
//
// The filter removes keys by name. It never reads, logs, or copies a value
// into a diagnostic — the grafana entry points at a sops-decrypted bundle.

package config

// grafanaMCPEnvKeys are the AgentEnvVars keys that gate the pi grafana MCP
// extension (modules/programs/prism/pi/extensions/grafana/index.ts). The
// extension self-gates on these two vars alone: absent gives zero tools,
// present gives 65.
var grafanaMCPEnvKeys = []string{
	"GRAFANA_MCP_CONFIG_PATH",
	"PI_GRAFANA_MCP_BIN",
}

// reviewRoleEnvExclusions maps a session role to the AgentEnvVars keys that
// role must not receive.
//
// The five review roles are the canonical review set (internal/review
// Agents()). They are listed literally here because internal/review imports
// this package; an import in the other direction is a cycle.
//
// `investigate` is deliberately absent. It is a read-only role, but read-only
// is not the axis that matters: an investigator answering an observability
// question has a legitimate use for the grafana tools, so it is treated like
// `coordinator` (decision recorded on issue #2533).
var reviewRoleEnvExclusions = map[string][]string{
	"review-goal":     grafanaMCPEnvKeys,
	"review-code":     grafanaMCPEnvKeys,
	"review-context":  grafanaMCPEnvKeys,
	"review-qa":       grafanaMCPEnvKeys,
	"review-security": grafanaMCPEnvKeys,
}

// FilterAgentEnvVarsForRole returns a copy of vars with every key excluded for
// role removed.
//
// A role outside the known set gets an unfiltered copy, so an unrecognised or
// future role never silently loses capability. A nil map returns nil. The
// input map is never modified.
func FilterAgentEnvVarsForRole(role string, vars map[string]string) map[string]string {
	if vars == nil {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	for _, k := range reviewRoleEnvExclusions[role] {
		delete(out, k)
	}
	return out
}

// AgentEnvVarsForRole loads profiles.json and returns its AgentEnvVars map
// filtered for role.
//
// This is the single resolver for the sandboxed dispatch paths and the host
// agent-only layout, so both produce the same map for the same role. Env var
// injection is best-effort: a missing or malformed profiles.json returns a nil
// map rather than an error, which matches the pre-existing behaviour of every
// caller.
func AgentEnvVarsForRole(role string) map[string]string {
	pf, err := LoadProfiles()
	if err != nil || pf == nil {
		return nil
	}
	return FilterAgentEnvVarsForRole(role, pf.AgentEnvVars)
}
