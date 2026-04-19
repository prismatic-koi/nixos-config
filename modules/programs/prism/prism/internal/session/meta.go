package session

// IsMetaSession reports whether a session name identifies a prism-internal
// meta-session (scratchpad, dashboard, etc.) rather than a user-spawned agent
// session. Meta-sessions must not appear in agent_status and are excluded from
// all session listings.
func IsMetaSession(name string) bool {
	switch name {
	case "scratchpad", "prism-dashboard":
		return true
	}
	return false
}
