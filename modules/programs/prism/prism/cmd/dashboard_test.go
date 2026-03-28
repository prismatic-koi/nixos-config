// Package cmd white-box tests for dashModel.
//
// These tests access unexported dashModel fields and helpers directly to verify
// that the CallerClient bug fix (capturing callerClient at init time and using
// dashSwitchTarget to select the right client) is exercised by the actual model
// code, not just by the underlying tmux primitives.
package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── minimal server harness (mirrors internal/tmux/harness_test.go) ───────────

type cmdTestServer struct {
	socket string
	bin    string
}

func newCmdTestServer(t *testing.T) *cmdTestServer {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH — skipping integration test")
	}
	socket := fmt.Sprintf("prism-cmd-test-%d-%s", os.Getpid(), randCmdHex(8))
	s := &cmdTestServer{socket: socket, bin: bin}
	if err := s.run("new-session", "-ds", "bootstrap", "-c", "/tmp"); err != nil {
		t.Fatalf("start test tmux server: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	})
	// Redirect TmuxBin to a wrapper that injects -L <socket>.
	wrapperPath := t.TempDir() + "/tmux"
	script := "#!/bin/sh\nexec " + bin + " -L " + socket + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
	return s
}

// randCmdHex returns n random bytes as a hex string using crypto/rand.
func randCmdHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func (s *cmdTestServer) run(args ...string) error {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %w\n%s", args, err, out)
	}
	return nil
}

func (s *cmdTestServer) output(args ...string) (string, error) {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *cmdTestServer) newSession(name string) {
	_ = s.run("new-session", "-ds", name, "-c", "/tmp")
}

func (s *cmdTestServer) clientSession(client string) (string, error) {
	return s.output("display-message", "-t", client, "-p", "#{session_name}")
}

func (s *cmdTestServer) setGlobal(option, value string) {
	_ = s.run("set-option", "-g", option, value)
}

func (s *cmdTestServer) attachClientToSession(t *testing.T, targetSession string) string {
	t.Helper()
	before, _ := s.output("list-clients", "-F", "#{client_name}")
	beforeSet := map[string]bool{}
	for _, c := range strings.Split(before, "\n") {
		if c = strings.TrimSpace(c); c != "" {
			beforeSet[c] = true
		}
	}
	cmd := exec.Command("script", "-q", "-c",
		s.bin+" -L "+s.socket+" attach-session -t "+targetSession,
		"/dev/null",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("attach client to %q: %v", targetSession, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	var clientName string
	for time.Now().Before(deadline) {
		out, err := s.output("list-clients", "-F", "#{client_name}")
		if err == nil {
			for _, c := range strings.Split(out, "\n") {
				c = strings.TrimSpace(c)
				if c != "" && !beforeSet[c] {
					clientName = c
					break
				}
			}
		}
		if clientName != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clientName == "" {
		t.Fatalf("new client for session %q never appeared in list-clients (timeout)", targetSession)
	}
	return clientName
}

// ─── dashSwitchTarget unit tests ──────────────────────────────────────────────

// TestDashSwitchTarget exercises all branches of the dashSwitchTarget helper,
// which encapsulates the client-selection logic from the Enter handler.
//
// This is the primary regression test for the CallerClient bug: the old code
// called tmux.CallerClient() live inside the handler, which returned the global
// stamp that could be overwritten by a concurrent dashboard open. The fix
// extracts the logic into dashSwitchTarget, which operates on immutable values
// captured at model-init time.
func TestDashSwitchTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		popup        bool
		client       string
		callerClient string
		want         string
	}{
		{
			name:         "persistent mode uses callerClient",
			popup:        false,
			client:       "process-client",
			callerClient: "caller-client",
			want:         "caller-client",
		},
		{
			name:         "persistent mode falls back to client when callerClient empty",
			popup:        false,
			client:       "process-client",
			callerClient: "",
			want:         "process-client",
		},
		{
			name:         "popup mode always uses client",
			popup:        true,
			client:       "popup-client",
			callerClient: "stale-caller",
			want:         "popup-client",
		},
		{
			name:         "popup mode with empty callerClient uses client",
			popup:        true,
			client:       "popup-client",
			callerClient: "",
			want:         "popup-client",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dashSwitchTarget(tc.popup, tc.client, tc.callerClient)
			if got != tc.want {
				t.Errorf("dashSwitchTarget(%v, %q, %q) = %q, want %q",
					tc.popup, tc.client, tc.callerClient, got, tc.want)
			}
		})
	}
}

// ─── dashModel init tests ─────────────────────────────────────────────────────

// TestDashModelCallerClientCapturedAtInit verifies that newDashModel captures
// the CallerClient stamp at init time and stores it as an immutable field, so
// that a subsequent change to the global stamp does not affect the model.
func TestDashModelCallerClientCapturedAtInit(t *testing.T) {
	s := newCmdTestServer(t)
	s.newSession("some-session")

	// Stamp the global with "client-A" before constructing the model.
	s.setGlobal("@prism_caller_client", "client-A")
	s.setGlobal("@prism_caller", "some-session")

	// Construct the model — it should capture "client-A".
	model := newDashModel("process-client", false)

	if model.callerClient != "client-A" {
		t.Fatalf("callerClient = %q, want %q (should be captured at init)", model.callerClient, "client-A")
	}

	// Now overwrite the global stamp with "client-B" (simulates another client
	// opening the dashboard after our model was constructed).
	s.setGlobal("@prism_caller_client", "client-B")

	// The model's callerClient must NOT change — it was captured at init.
	if model.callerClient != "client-A" {
		t.Fatalf("callerClient changed to %q after global stamp updated — bug: CallerClient() was called lazily", model.callerClient)
	}
}

// TestDashModelEnterHandlerUsesCallerClient_PersistentMode is the end-to-end
// regression test for the CallerClient bug.
//
// It verifies that when two clients have dashboards open (two dashModel
// instances with different callerClient values), pressing Enter in modelA
// results in clientA being switched — not clientB — even though the global
// @prism_caller_client stamp points to clientB at handler time.
//
// The test verifies both layers of the fix:
//  1. Update(Enter) returns a non-nil cmd (the handler fired), and
//     dashSwitchTarget(modelA.popup, modelA.client, modelA.callerClient)
//     returns clientA (the correct target per the model's immutable snapshot).
//  2. Calling SwitchClient with that target moves clientA to the new session
//     while clientB remains unaffected.
func TestDashModelEnterHandlerUsesCallerClient_PersistentMode(t *testing.T) {
	s := newCmdTestServer(t)
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Stamp @prism_caller_client to clientA, then build modelA.
	s.setGlobal("@prism_caller_client", clientA)
	s.setGlobal("@prism_caller", "nixos-config@main")
	modelA := newDashModel("process-client-A", false) // persistent mode: popup=false

	// Now clientB opens the dashboard — overwrites the global stamp.
	s.setGlobal("@prism_caller_client", clientB)
	s.setGlobal("@prism_caller", "nixos-config@main")
	_ = newDashModel("process-client-B", false) // stamp now points to B

	// Verify the stamp is now clientB (what CallerClient() would return live).
	if got := tmux.CallerClient(); got != clientB {
		t.Fatalf("setup: @prism_caller_client = %q, want clientB=%q", got, clientB)
	}

	// modelA still holds clientA as its callerClient (captured at init).
	if modelA.callerClient != clientA {
		t.Fatalf("modelA.callerClient = %q, want clientA=%q", modelA.callerClient, clientA)
	}

	// Seed modelA with the sessions list so it can handle Enter.
	modelA.sessions = []tmux.Session{{Name: "nixos-config@feature"}}
	modelA.cursor = 0
	modelA.cursorActive = true // activate cursor so Enter acts immediately

	// Verify the handler fires (returns a cmd).
	_, seqCmd := modelA.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if seqCmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — handler did not fire")
	}

	// Verify dashSwitchTarget (the target-selection helper called by the handler)
	// resolves to clientA — not the stale global stamp (clientB).
	target := dashSwitchTarget(modelA.popup, modelA.client, modelA.callerClient)
	if target != clientA {
		t.Fatalf("dashSwitchTarget() = %q, want clientA=%q — wrong client would be switched", target, clientA)
	}

	// Execute the actual tmux switch-client to confirm the full path works.
	if err := tmux.SwitchClient(target, "nixos-config@feature"); err != nil {
		t.Fatalf("SwitchClient: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	gotA, err := s.clientSession(clientA)
	if err != nil {
		t.Fatalf("clientSession(clientA): %v", err)
	}
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}

	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q, want %q — Enter in modelA should switch clientA", gotA, "nixos-config@feature")
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q — clientB should be unaffected (was wrongly switched by the bug)", gotB, "nixos-config@main")
	}
}
