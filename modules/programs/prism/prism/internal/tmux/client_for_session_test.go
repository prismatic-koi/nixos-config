package tmux_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// TestClientForSession_API verifies the deterministic client resolution that
// replaces display-message in the persistent dashboard (issue #2522, defect 2).
//
// It reproduces the exact failure the probe found: with a client attached to a
// DIFFERENT session and none on the dashboard session, display-message can leak
// the other-session client, whereas ClientForSession returns "". Then, with a
// client attached to the dashboard session, ClientForSession returns exactly
// that client.
//
// Uses withServer (not t.Parallel) because it mutates the package-level
// TmuxBin. Skips inside sandboxes/CI where PTY-based client attachment does not
// work.
func TestClientForSession_API(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newServer(t)
	withServer(t, s)

	s.newSession("prism-dashboard")
	s.newSession("other-a")

	// Attach a client to a different session only.
	otherClient := s.attachClientToSession(t, "other-a")

	// No client is attached to the dashboard session: ClientForSession must
	// return "" and must NOT leak the client on other-a.
	got, err := tmux.ClientForSession("prism-dashboard")
	if err != nil {
		t.Fatalf("ClientForSession(prism-dashboard): %v", err)
	}
	if got != "" {
		t.Errorf("with no client on the dashboard, ClientForSession = %q, want \"\" (leaked other-session client %q?)", got, otherClient)
	}

	// Attach a client to the dashboard session: ClientForSession must return it.
	dashClient := s.attachClientToSession(t, "prism-dashboard")
	got, err = tmux.ClientForSession("prism-dashboard")
	if err != nil {
		t.Fatalf("ClientForSession(prism-dashboard) after attach: %v", err)
	}
	if got != dashClient {
		t.Errorf("ClientForSession(prism-dashboard) = %q, want %q", got, dashClient)
	}
}
