package dashboard

import "testing"

// TestAgentTypeLabel verifies that agentTypeLabel is a pass-through:
// every agent_name value renders as itself, including agent types beyond the
// old hard-coded coordinator/worker allowlist. See #844 / #849.
func TestAgentTypeLabel(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		// [functional] Previously-supported types still render as themselves.
		{"coordinator passes through", "coordinator", "coordinator"},
		{"worker passes through", "worker", "worker"},

		// [functional] Review subagents — the central bug from #844.
		{"review-goal passes through", "review-goal", "review-goal"},
		{"review-code passes through", "review-code", "review-code"},
		{"review-security passes through", "review-security", "review-security"},
		{"review-qa passes through", "review-qa", "review-qa"},
		{"review-context passes through", "review-context", "review-context"},

		// [functional] Other known agent types.
		{"ac passes through", "ac", "ac"},
		{"retro passes through", "retro", "retro"},

		// [functional] Future / unknown agent types must not be silently dropped.
		{"unknown agent type passes through", "some-future-agent", "some-future-agent"},

		// [edge-case] Defensive: empty input yields empty output.
		{"empty string yields empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentTypeLabel(tc.agentName)
			if got != tc.want {
				t.Errorf("agentTypeLabel(%q) = %q, want %q", tc.agentName, got, tc.want)
			}
		})
	}
}
