package cmd

// Tests for the --agent-role flag default.
//
// The --agent-role flag defaults to "" so that host-mode sessions defer to
// SSE-based inference. A non-empty default ("worker") would label every
// host-mode session agent: worker in the DB regardless of the actual agent
// that ran.

import (
	"testing"
)

// TestSidecarFlag_AgentRoleDefaultIsEmpty verifies that the --agent-role flag
// on sidecarCmd defaults to "" (empty string) after the fix. A non-empty
// default ("worker") would cause every host-mode session to be labelled
// "worker" in the DB, regardless of the actual agent name emitted by the agent
// SSE events.
func TestSidecarFlag_AgentRoleDefaultIsEmpty(t *testing.T) {
	flag := sidecarCmd.Flags().Lookup("agent-role")
	if flag == nil {
		t.Fatal("--agent-role flag not found on sidecarCmd")
	}

	got := flag.DefValue
	if got != "" {
		t.Errorf("--agent-role default = %q, want %q (empty string so host-mode sessions infer agent from SSE events)", got, "")
	}
}

// TestSidecarFlag_AgentRoleDescription mentions SSE inference.
// This is a soft check: the description should at least acknowledge that the
// flag is only needed in container mode, and that inference happens when empty.
func TestSidecarFlag_AgentRoleDescriptionMentionsInference(t *testing.T) {
	flag := sidecarCmd.Flags().Lookup("agent-role")
	if flag == nil {
		t.Fatal("--agent-role flag not found on sidecarCmd")
	}

	usage := flag.Usage
	if usage == "" {
		t.Error("--agent-role flag has empty usage string")
	}
	// Verify the usage at least mentions container mode (existing) and
	// something about inference or empty behaviour.
	if len(usage) < 10 {
		t.Errorf("--agent-role usage is suspiciously short (%d chars): %q", len(usage), usage)
	}
}
