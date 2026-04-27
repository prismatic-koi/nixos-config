package cmd

// switch_dashboard.go — dashboard integration helper for prism switch.

import "github.com/prismatic-koi/prism/internal/tmux"

// ── ensure dashboard session ──────────────────────────────────────────────────

func ensureSwitchDashSession() {
	if !tmux.HasSession(dashSession) {
		// Best-effort; ignore errors.
		_ = tmux.NewSessionDetached(dashSession, "")
		// The session's command loop is set up by the tmux binding, not here.
	}
}
