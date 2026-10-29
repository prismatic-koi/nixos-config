//go:build darwin

package integration_test

// sandbox_exec_pipe_secret_darwin_test.go — the harness pipe secret of a
// sandbox-exec session (#3356).
//
// On Darwin the harness pipe is a 127.0.0.1 TCP port, and every sandbox-exec
// session can connect to it. The sidecar refuses a hello without the secret
// that it wrote to pipe.secret in the per-session run dir. The section-6
// run-dir rule of the profile lets only the session itself read that file.
//
// The tests run a real sidecar with a TCP pipe outside the sandbox, and a
// probe under the production profile of a session. The probe reads a secret
// file (or takes a literal secret), sends hello over the loopback port, and
// prints the first reply. The paired negative takes the run-dir rule out of
// the profile, and the read of the own secret then fails. The positive end
// of the cross-session test sends the right secret from the sandbox of the
// other session and gets hello_ack, so the TCP path is open and the secret
// is what refuses the other peers.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// pipeProbeSrc is the source of the pipe probe.
//
//	probe <host:port> <secret-file>           read the secret from the file
//	probe <host:port> - <secret>              use the literal secret
//
// The probe sends hello with the secret and prints "REPLY <first line>". A
// file read that fails prints READ-DENIED and exits 2. Every other failure
// exits 1.
const pipeProbeSrc = `package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"
)

func fail(err error, marker string, code int) {
	os.Stderr.WriteString(err.Error() + "\n")
	if marker != "" {
		os.Stdout.WriteString(marker + "\n")
	}
	os.Exit(code)
}

func main() {
	addr, file := os.Args[1], os.Args[2]
	secret := ""
	if file != "-" {
		b, err := os.ReadFile(file)
		if err != nil {
			fail(err, "READ-DENIED", 2)
		}
		secret = strings.TrimSpace(string(b))
	}
	if len(os.Args) > 3 {
		secret = os.Args[3]
	}
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fail(err, "", 1)
	}
	defer c.Close()
	hello, _ := json.Marshal(map[string]any{
		"type": "hello", "protocol_version": 2, "harness": "pi",
		"harness_version": "probe", "secret": secret,
	})
	if _, err := c.Write(append(hello, '\n')); err != nil {
		fail(err, "", 1)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		fail(err, "", 1)
	}
	os.Stdout.WriteString("REPLY " + line)
}
`

// buildPipeProbe compiles the pipe probe into a t.TempDir(), which section 2
// of the profile grants read and map-executable. See buildConnectProbe.
func buildPipeProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "pipeprobe.go")
	if err := os.WriteFile(src, []byte(pipeProbeSrc), 0o600); err != nil {
		t.Fatalf("write pipe probe source: %v", err)
	}
	bin := filepath.Join(dir, "pipeprobe")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the pipe probe (go unavailable?): %v\n%s", err, out)
	}
	return bin
}

// startPipeSidecar runs a real sidecar for sessionName with a loopback TCP
// pipe, the transport of a sandbox-exec session. It returns the pipe address
// and the path of the pipe secret file. Call it after XDG_STATE_HOME is set.
func startPipeSidecar(t *testing.T, sessionName string) (addr, secretPath string) {
	t.Helper()
	secretPath, err := session.SidecarHarnessPipeSecretPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHarnessPipeSecretPath: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	sc := sidecar.New(sidecar.Config{
		SessionName:           sessionName,
		Repo:                  "prism-test",
		Worktree:              t.TempDir(),
		DB:                    openTestDB(t),
		Clock:                 sidecar.RealClock(),
		AgentRole:             "worker",
		HarnessName:           "pi",
		Harness:               pih.New("", "", ""),
		HarnessPipeTCPPort:    port,
		HarnessPipeSecretPath: secretPath,
		StartupConnectTimeout: time.Minute,
		PipeReconnectTimeout:  time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sc.Run(ctx)
	}()
	t.Cleanup(func() {
		sc.Shutdown()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("the sidecar of %s did not stop within 5s", sessionName)
		}
	})

	// Wait for the listener. A line that is not JSON gets the refusal, which
	// does not change the session or hold the accept loop.
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_, _ = c.Write([]byte("ready?\n"))
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 512)
			for {
				if _, err := c.Read(buf); err != nil {
					break
				}
			}
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pipe of %s at %s did not listen: %v", sessionName, addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("the sidecar of %s wrote no pipe secret: %v", sessionName, err)
	}
	return addr, secretPath
}

// newPipeSessionManager returns a profile Manager for sessionName, with the
// production run dir of the session as the dir of HostAPISockPath, and
// BareRoot set (see newSocketProfileManager). Call it after XDG_STATE_HOME is
// set.
func newPipeSessionManager(t *testing.T, sessionName string) *container.Manager {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3356-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}
	hostAPI, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(hostAPI), 0o700); err != nil {
		t.Fatalf("MkdirAll run dir: %v", err)
	}
	return container.New(container.Config{
		SessionName:     sessionName,
		InstanceID:      "integ-3356-" + session.SessionDirName(sessionName+t.Name()),
		Worktree:        t.TempDir(),
		BareRoot:        bareRoot,
		HostAPISockPath: hostAPI,
		GitUserName:     "test-user",
		GitUserEmail:    "test@example.com",
	})
}

// TestSandboxExecPipeSecret_OwnSessionHandshakes is AC3 on Darwin: under the
// production profile of a session, the agent reads the pipe secret of its own
// session and completes the handshake with the real sidecar over the
// loopback port.
func TestSandboxExecPipeSecret_OwnSessionHandshakes(t *testing.T) {
	requireSandboxExec(t)
	probe := buildPipeProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))

	const own = "prism-test@3356-own"
	addr, secretPath := startPipeSidecar(t, own)
	prepared, _ := preparePositiveProfile(t, newPipeSessionManager(t, own))
	profilePath := writeProbePositiveProfile(t, prepared)

	out, err := runProbe(t, profilePath, probe, addr, secretPath)
	if err != nil || !strings.Contains(out, `"type":"hello_ack"`) {
		t.Errorf("the agent of the session did not complete the handshake with its own secret.\nExit: %v\nOutput: %s\nProfile: %s", err, out, profilePath)
	}
}

// TestSandboxExecPipeSecret_OwnSecretDeniedWithoutRunDirGrant is the paired
// negative. It points the run-dir rule of the profile at a dir that does not
// exist, and the same probe then cannot read the secret.
func TestSandboxExecPipeSecret_OwnSecretDeniedWithoutRunDirGrant(t *testing.T) {
	requireSandboxExec(t)
	probe := buildPipeProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))

	const own = "prism-test@3356-own"
	addr, secretPath := startPipeSidecar(t, own)
	runDir := filepath.Dir(secretPath)
	mutatedPath := withMutatedProbeProfile(t, newPipeSessionManager(t, own), func(p string) string {
		return strings.ReplaceAll(p,
			"(subpath "+sbplQuoteForTest(runDir)+")",
			"(subpath "+sbplQuoteForTest(runDir+".prism-3356-disabled")+")")
	})

	out, err := runProbe(t, mutatedPath, probe, addr, secretPath)
	if err == nil || !strings.Contains(out, "READ-DENIED") {
		t.Errorf("the probe read the pipe secret without the run-dir rule, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s", err, out, mutatedPath)
	}
}

// TestSandboxExecPipeSecret_OtherSessionRefused is AC1 on Darwin. Under the
// production profile of session A, the probe cannot read the pipe secret of
// session B. The sidecar of B refuses a hello from the sandbox of A with A's
// own secret and with no secret. The probe gets no hello_ack, so it gets no
// held prompt of B and cannot send frames as B. The positive pair at the end
// sends B's secret from the sandbox of A and gets hello_ack: the loopback
// port of B is open to A, and only the secret stops A.
func TestSandboxExecPipeSecret_OtherSessionRefused(t *testing.T) {
	requireSandboxExec(t)
	probe := buildPipeProbe(t)
	fileProbe := buildConnectProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))

	const a, b = "prism-test@3356-a", "prism-test@3356-b"
	_, secretA := startPipeSidecar(t, a)
	addrB, secretB := startPipeSidecar(t, b)
	prepared, _ := preparePositiveProfile(t, newPipeSessionManager(t, a))
	profileA := writeProbePositiveProfile(t, prepared)

	expectOp(t, profileA, fileProbe, false, "session A can read the pipe secret of session B", "--read", secretB)
	expectOp(t, profileA, fileProbe, true, "session A cannot read its own pipe secret, so the deny of B's secret proves nothing", "--read", secretA)

	if out, err := runProbe(t, profileA, probe, addrB, secretB); err == nil || !strings.Contains(out, "READ-DENIED") {
		t.Errorf("the probe in session A read the pipe secret of session B.\nExit: %v\nOutput: %s", err, out)
	}
	for _, tc := range []struct{ name, secretFile string }{
		{"the secret of session A", secretA},
		{"no secret", "-"},
	} {
		out, err := runProbe(t, profileA, probe, addrB, tc.secretFile)
		if err != nil || !strings.Contains(out, `"code":"unauthorized"`) || strings.Contains(out, "hello_ack") {
			t.Errorf("hello from session A to session B with %s: want the unauthorized refusal.\nExit: %v\nOutput: %s", tc.name, err, out)
		}
	}

	raw, err := os.ReadFile(secretB)
	if err != nil {
		t.Fatalf("read the secret of session B outside the sandbox: %v", err)
	}
	if out, err := runProbe(t, profileA, probe, addrB, "-", string(raw)); err != nil || !strings.Contains(out, `"type":"hello_ack"`) {
		t.Errorf("hello from the sandbox of session A with the secret of session B got no hello_ack, so the refusals above can come from the network, not the secret.\nExit: %v\nOutput: %s", err, out)
	}
}
