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
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── sandbox detection ────────────────────────────────────────────────────────

// insideSandbox returns true when the test process is running inside an
// isolated sandbox environment where PTY-based tmux client attachment does not
// work reliably. Two environments are detected:
//
//  1. Nix build sandbox: detects via $NIX_BUILD_TOP being non-empty (always set
//     by nix during buildGoModule's checkPhase). In the nix sandbox, script(1)
//     runs but the PTY slave cannot become the controlling terminal for tmux's
//     client process, so the client never appears in list-clients.
//
//  2. opencode/prism bwrap sandbox: detects via /proc/1/comm == "bwrap". This
//     sandbox uses --unshare-pid so bwrap itself is PID 1. In this environment
//     a single script-attached client works, but a second concurrent attachment
//     causes both clients to exit immediately due to bwrap devpts namespace
//     constraints.
//
// Callers should only skip PTY-attach tests on this basis, not all tmux tests.
func insideSandbox() bool {
	// Nix build sandbox: NIX_BUILD_TOP is always exported during nix builds.
	if os.Getenv("NIX_BUILD_TOP") != "" {
		return true
	}
	// opencode/prism bwrap sandbox: PID 1 is the bwrap binary itself.
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == "bwrap"
}

// skipIfSandboxPTY calls t.Skip when the test requires script-based PTY
// attachment and the process is running in a sandbox that prevents it.
//
// In the nix build sandbox (detectable via $NIX_BUILD_TOP), script(1) runs
// but tmux's client process cannot acquire a controlling terminal, so the
// client never appears in list-clients. In the opencode bwrap sandbox
// (/proc/1/comm == "bwrap"), a second concurrent script-attached client causes
// both clients to exit immediately. Neither environment supports the full PTY
// attach lifecycle needed by multi-client tests.
//
// Tests needing only non-PTY tmux operations (session creation, window listing,
// option setting) do not need this guard and will run in both environments.
func skipIfSandboxPTY(t *testing.T) {
	t.Helper()
	if insideSandbox() {
		t.Skip("skipping PTY-attach integration test: running in a sandbox " +
			"(nix build or bwrap) where script-based tmux client attachment is " +
			"not supported — run from a host shell to exercise this path")
	}
}

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
// -f /dev/null suppresses the user's tmux.conf so no hooks (e.g.
// session-created → prism event tmux-session-start) fire against the live DB.
func (s *server) run(args ...string) error {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %w\n%s", args, err, out)
	}
	return nil
}

// output executes a tmux command and returns trimmed stdout.
// -f /dev/null suppresses the user's tmux.conf so no hooks fire.
func (s *server) output(args ...string) (string, error) {
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
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

// newWindow creates a new named window in a session.
func (s *server) newWindow(session string, idx int, name string) {
	_ = s.run("new-window", "-t", fmt.Sprintf("%s:%d", session, idx), "-n", name)
}

// setWindowOption sets a window option on a target window.
func (s *server) setWindowOption(target, option, value string) {
	_ = s.run("set-window-option", "-t", target, option, value)
}

// capturePane captures the visible pane contents for the given target.
func (s *server) capturePane(target string) string {
	out, _ := s.output("capture-pane", "-t", target, "-p")
	return out
}

// scriptArgs returns the argument list for the `script` command to run cmd in
// a pseudo-terminal without a real display. The syntax differs by platform:
//
//   - Linux:  script -q -c '<cmd>' /dev/null
//   - macOS:  script -q /dev/null <cmd args...>  (no -c flag; command is positional)
//
// On macOS, BSD script treats its command argument as a literal executable path,
// not a shell command string, so the command must be split into separate args.
// strings.Fields is safe here because tmux binary paths and all arguments
// (socket names, session names) are guaranteed to be whitespace-free.
func scriptArgs(cmd string) []string {
	if runtime.GOOS == "darwin" {
		return append([]string{"-q", "/dev/null"}, strings.Fields(cmd)...)
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

	var scriptStderr bytes.Buffer
	cmd := exec.Command("script", scriptArgs(s.bin+" -L "+s.socket+" -f /dev/null attach-session -t "+targetSession)...)
	cmd.Stderr = &scriptStderr
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
	var lastListOut string
	for time.Now().Before(deadline) {
		out, err := s.output("list-clients", "-F", "#{client_name}")
		if err == nil {
			lastListOut = out
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
		// Capture script process state for diagnosis.
		scriptAlive := "alive"
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			scriptAlive = fmt.Sprintf("dead (%v)", err)
		}
		t.Fatalf(
			"new client for session %q never appeared in list-clients (timeout)\n"+
				"  script process state:  %s\n"+
				"  script stderr:         %q\n"+
				"  list-clients after timeout: %q\n"+
				"  clients before attach: %q\n"+
				"  tip: if running inside bwrap or nix build sandbox, PTY-attached\n"+
				"  tmux clients may not work; call skipIfSandboxPTY(t) at the top\n"+
				"  of tests that need any script-attached tmux client",
			targetSession, scriptAlive, scriptStderr.String(), lastListOut, before,
		)
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
	// -f /dev/null suppresses the user's tmux.conf so no hooks fire against the
	// live DB when the package-level tmux.* functions create sessions on this
	// isolated server.
	script := "#!/bin/sh\nexec " + s.bin + " -L " + s.socket + " -f /dev/null \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}
