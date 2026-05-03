package container

import (
	"testing"
)

// ── hostIsolator.SidecarFlags ─────────────────────────────────────────────────

func TestHostIsolator_SidecarFlags_WithAgentRole(t *testing.T) {
	h := &hostIsolator{}
	opts := SidecarFlagOpts{
		Port:      9000,
		AgentRole: "worker",
	}
	flags := h.SidecarFlags(opts)

	// Must include --agent-role worker
	found := false
	for i := 0; i+1 < len(flags); i++ {
		if flags[i] == "--agent-role" && flags[i+1] == "worker" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --agent-role worker in SidecarFlags output, got %v", flags)
	}
}

func TestHostIsolator_SidecarFlags_EmptyAgentRole(t *testing.T) {
	h := &hostIsolator{}
	opts := SidecarFlagOpts{
		Port:      9000,
		AgentRole: "",
	}
	flags := h.SidecarFlags(opts)

	for _, f := range flags {
		if f == "--agent-role" {
			t.Errorf("expected --agent-role to be absent when AgentRole is empty, got flags: %v", flags)
		}
	}
}
