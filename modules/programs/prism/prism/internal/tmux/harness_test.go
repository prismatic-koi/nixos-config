// Package tmux_test provides a test harness for spinning up an isolated
// headless tmux server per test, allowing tests to exercise real tmux
// primitives without interfering with any live session.
//
// Parallelism design:
// The tmux package uses a package-level TmuxBin variable. Tests that call the
// package-level helpers directly cannot safely run in parallel because they
// would race on TmuxBin. Two strategies are used:
//
//  1. Tests that only need to verify tmux primitives call s.* helpers directly
//     on a per-test server — these are parallel-safe.
//
//  2. Tests that specifically test the tmux package API (CallerClient, etc.)
//     call withServer() to temporarily redirect TmuxBin, and do NOT call
//     t.Parallel() so they are always sequential.
package tmux_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── server harness ───────────────────────────────────────────────────────────

// server holds state for a test-scoped tmux server running on a unique socket.
type server struct {
	socket string // value passed to tmux -L
	bin    string // resolved path to the real tmux binary
}

// newServer starts a fresh headless tmux server on a unique socket and returns
// a handle. The server is automatically killed in t.Cleanup.
func newServer(t *testing.T) *server {
	t.Helper()

	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH — skipping integration test")
	}

	socket := fmt.Sprintf("prism-test-%d-%s", os.Getpid(), randHex(8))
	s := &server{socket: socket, bin: bin}

	// Bootstrap: create the first session so the server stays alive.
	if err := s.run("new-session", "-ds", "bootstrap", "-c", "/tmp"); err != nil {
		t.Fatalf("start test tmux server: %v", err)
	}

	t.Cleanup(func() {
		_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	})
	return s
}

// randHex returns n random hex bytes as a string (2*n characters), sourced
// from crypto/rand so socket names are unpredictable across runs.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// run executes a tmux command against this server's socket.
func (s *server) run(args ...string) error {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %w\n%s", args, err, out)
	}
	return nil
}

// output executes a tmux command and returns trimmed stdout.
func (s *server) output(args ...string) (string, error) {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// newSession creates a detached session on this server.
func (s *server) newSession(name string) {
	_ = s.run("new-session", "-ds", name, "-c", "/tmp")
}

// hasSession returns true if a session exists on this server.
func (s *server) hasSession(name string) bool {
	return s.run("has-session", "-t", name) == nil
}

// clientSession returns the session that the named client is viewing.
func (s *server) clientSession(client string) (string, error) {
	return s.output("display-message", "-t", client, "-p", "#{session_name}")
}

// listClients returns all client names on this server.
func (s *server) listClients() ([]string, error) {
	out, err := s.output("list-clients", "-F", "#{client_name}")
	if err != nil {
		return nil, err
	}
	var clients []string
	for _, c := range strings.Split(out, "\n") {
		c = strings.TrimSpace(c)
		if c != "" {
			clients = append(clients, c)
		}
	}
	return clients, nil
}

// switchClient switches the named client to the named session on this server.
func (s *server) switchClient(client, session string) error {
	return s.run("switch-client", "-c", client, "-t", session)
}

// setGlobal sets a global tmux option on this server.
func (s *server) setGlobal(option, value string) {
	_ = s.run("set-option", "-g", option, value)
}

// getGlobal gets a global tmux option value from this server.
func (s *server) getGlobal(option string) string {
	val, _ := s.output("show-option", "-gv", option)
	return val
}

// killSession kills a named session on this server.
func (s *server) killSession(name string) error {
	return s.run("kill-session", "-t", name)
}

// scriptArgs returns the argument list for the `script` command to run cmd in
// a pseudo-terminal without a real display. The syntax differs by platform:
//
//   - Linux:  script -q -c '<cmd>' /dev/null
//   - macOS:  script -q /dev/null <cmd>  (no -c flag; command is positional)
func scriptArgs(cmd string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"-q", "/dev/null", cmd}
	}
	return []string{"-q", "-c", cmd, "/dev/null"}
}

// attachClientToSession starts a real tmux client attached to the given session
// using `script` to allocate a pseudo-terminal without a real display.
// Returns the tmux client name (e.g. /dev/pts/N). The process is killed via
// t.Cleanup.
func (s *server) attachClientToSession(t *testing.T, targetSession string) string {
	t.Helper()

	// Snapshot existing clients on this session BEFORE starting the new one,
	// so we can identify the new client by exclusion.
	before, _ := s.output("list-clients", "-F", "#{client_name}")
	beforeSet := map[string]bool{}
	for _, c := range strings.Split(before, "\n") {
		if c = strings.TrimSpace(c); c != "" {
			beforeSet[c] = true
		}
	}

	cmd := exec.Command("script", scriptArgs(s.bin+" -L "+s.socket+" attach-session -t "+targetSession)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("attach client to %q: %v", targetSession, err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Poll list-clients (all clients, not just on this session) until a new
	// client appears that wasn't in beforeSet.
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

// withServer redirects tmux.TmuxBin to a wrapper script that injects
// -L <socket> for the duration of the test, then restores the original.
//
// Only call this from non-parallel tests — TmuxBin is a package-level global.
func withServer(t *testing.T, s *server) {
	t.Helper()

	orig := tmux.TmuxBin
	wrapperPath := t.TempDir() + "/tmux"
	script := "#!/bin/sh\nexec " + s.bin + " -L " + s.socket + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}
