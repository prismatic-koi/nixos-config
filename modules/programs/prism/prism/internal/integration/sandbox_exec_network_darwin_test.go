//go:build darwin

package integration_test

// sandbox_exec_network_darwin_test.go — integration coverage for the
// network egress allow rule:
//
//   (allow network*)
//
// Approach: stand up a TCP listener on 127.0.0.1 in the test process,
// then have a Nix-bash inside the sandbox open a connection to it via
// bash's /dev/tcp/<host>/<port> redirection. The positive test asserts
// the connection succeeds; the negative test removes (allow network*)
// and asserts the connection fails.
//
// We use 127.0.0.1 rather than a routable address because:
//   - The test has no external dependencies (CI / offline-friendly).
//   - sandbox-exec's network rule is host-namespace shared on Darwin
//     (the sandbox does NOT have a separate network namespace), so
//     connecting to a loopback listener exercises the same kernel path
//     as a real outbound connection.

import (
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// startLoopbackListener starts a TCP listener on 127.0.0.1:0 (kernel-
// chosen port) and returns the listener and the bound port. The listener
// is registered for cleanup via t.Cleanup.
//
// We accept and immediately close any incoming connection — the test only
// cares about whether the connect succeeds, not the data exchange.
func startLoopbackListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen 127.0.0.1:0: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Drain incoming connections in a goroutine so the OS-level connect
	// completes (some kernels report connect failures more reliably when
	// the listener has actively accepted the SYN).
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // listener closed in cleanup
			}
			_ = conn.Close()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	return ln, port
}

// TestSandboxExecProfile_NetworkAllowsLoopbackConnect is the positive
// integration test for (allow network*). Asserts a Nix-bash inside the
// sandbox can connect to a loopback TCP listener.
func TestSandboxExecProfile_NetworkAllowsLoopbackConnect(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, port := startLoopbackListener(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// bash's /dev/tcp redirection: opening the descriptor performs a
	// connect(2). If the connect fails (sandbox blocks), bash exits non-
	// zero. If it succeeds, the immediate `:` builtin is a no-op and the
	// shell exits 0.
	probe := "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(port) + " && exec 3<&-"
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", probe)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("loopback connect failed under production profile.\n"+
			"This means (allow network*) is missing or ineffective.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			runErr, string(out), testProfilePath)
	}
}

// TestSandboxExecProfile_NetworkDeniedWithoutAllow is the paired negative
// test. It removes the (allow network*) line and asserts the same connect
// attempt fails — proving the production allow is what permits network
// egress, not some other rule.
func TestSandboxExecProfile_NetworkDeniedWithoutAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, port := startLoopbackListener(t)

	m := newProfileManager(t)

	// generateProfile emits the rule as "(allow network*)\n" on its own
	// line. Match the verbatim form so a future formatting change fails
	// withMutatedProfile's no-op detection rather than silently passing.
	const networkRule = "(allow network*)\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, networkRule, "")
	})

	probe := "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(port) + " && exec 3<&-"
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", probe)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("loopback connect succeeded (exit 0) WITHOUT (allow network*).\n"+
			"The negative test is not catching the regression — investigate why network is reachable without the allow.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — loopback connect correctly denied without (allow network*) (exit: %v)", runErr)
	}
}
