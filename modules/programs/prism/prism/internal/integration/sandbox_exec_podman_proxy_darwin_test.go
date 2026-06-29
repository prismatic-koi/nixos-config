//go:build darwin

package integration_test

// sandbox_exec_podman_proxy_darwin_test.go — Darwin integration tests for
// the conditional SBPL allow that exposes the per-session filtering podman
// API socket inside the sandbox-exec sandbox (issue #2317 §3c / #2322,
// Step 5).
//
// Per the AGENTS.md sandbox-exec testing convention
// (modules/programs/prism/prism/docs/sandbox-exec-testing.md, issue #1192),
// every change to generateProfile / Manager.PrepareSandboxExec must be
// paired with:
//
//  1. A POSITIVE integration test that invokes /usr/bin/sandbox-exec against
//     a Nix-built test binary and exercises the rule end-to-end. Here: a
//     process inside the sandbox connect(2)s to <PodmanProxySockPath> and
//     receives a sentinel response from a stub Unix-socket server running
//     in the test process.
//
//  2. A NEGATIVE-MUTATION test that mutates the SBPL generator (via
//     withMutatedProfile) to OMIT the literal allow clause, runs the same
//     probe, and asserts it FAILS with the documented denial. This is what
//     proves the positive test is not a no-op — substring assertions on
//     profile content are necessary but not sufficient (see #1192 closure).
//
// The greppable security AC from #2322 ("the real upstream podman socket
// path does NOT appear in the rendered SBPL for ANY value of
// ContainersEnabled") is covered by the unit-test sibling under
// internal/container/sandbox_exec_podman_proxy_test.go — it does not need
// to invoke sandbox-exec.

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// podmanProxyProbeResponse is the sentinel byte sequence the stub server
// writes back to a successful connect. The probe asserts these exact bytes
// appear on stdout — anything else (empty output, partial bytes, sandbox
// denial text) is a failure.
const podmanProxyProbeResponse = "PRISM-PODMAN-PROXY-PROBE-OK-2322\n"

// startStubPodmanProxyListener starts a Unix-socket listener at sockPath
// in the test process, accepts a single connection in a goroutine, writes
// podmanProxyProbeResponse, and closes the connection. The listener is
// closed via t.Cleanup. The returned counter tracks the number of accepted
// connections so the negative-mutation test can assert the sandbox never
// reached the listener.
//
// This is a STUB — it does not implement the docker/podman HTTP API. The
// goal of the integration test is to prove the SBPL literal allow is
// load-bearing for the connect(2) syscall, not to exercise the full proxy
// (which has its own unit tests under internal/podmanproxy/).
func startStubPodmanProxyListener(t *testing.T, sockPath string) *atomic.Int32 {
	t.Helper()

	// Remove any stale socket from a previous run.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen(\"unix\", %q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})

	accepted := &atomic.Int32{}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // listener closed in cleanup
			}
			accepted.Add(1)
			// Best-effort write; the sandboxed reader is reading until the
			// listener half-closes, so we close immediately after writing.
			_, _ = conn.Write([]byte(podmanProxyProbeResponse))
			_ = conn.Close()
		}
	}()

	return accepted
}

// requireNixSocat resolves a Nix-built socat binary via PATH and returns
// its /nix/store/... absolute path. Skips the test when socat is not
// found in PATH or does not resolve to a Nix store path.
//
// We use socat (rather than /usr/bin/nc -U, /usr/bin/curl --unix-socket,
// or python3) because:
//
//   - /usr/bin/nc and /usr/bin/curl are Apple-signed binaries that SIGABRT
//     in dyld4::CacheFinder under a deny-default SBPL profile (issue #1190
//     — same reason the bash integration tests use Nix bash).
//   - Homebrew Python is not guaranteed to be present on every Darwin dev
//     machine; socat is a stable home-manager package on this codebase.
//   - socat's one-liner `UNIX-CONNECT:<path>` is the most direct probe of
//     the connect(2) syscall: it succeeds with exit 0 only if the kernel
//     permits the AF_UNIX connect, which is exactly what the SBPL literal
//     allow controls.
func requireNixSocat(t *testing.T) string {
	t.Helper()

	socatPath, err := exec.LookPath("socat")
	if err != nil {
		t.Skipf("socat not found in PATH: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(socatPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", socatPath, err)
	}

	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("socat resolves to %q which is not a /nix/store/ path — cannot use as test binary (Apple-signed binaries SIGABRT under deny-default sandbox, see #1190)", resolved)
	}

	return resolved
}

// isolatedSocketDir returns a freshly-allocated directory under $HOME
// suitable for hosting the test Unix-domain socket. The directory MUST be
// outside every other RW grant in the production SBPL profile so that the
// literal allow under test is the ONLY rule that can permit the in-sandbox
// connect(2). Specifically, the directory must NOT live under any of:
//
//   - /tmp or /private/tmp (section 3 RW grant).
//   - os.TempDir() / /var/folders/<...>/T (section 3b RW grant; this also
//     covers t.TempDir() on Darwin).
//   - <sessionDir> at <XDG_STATE_HOME>/prism/sessions/<instanceID> (section
//     6 RW grant for the per-session work dir).
//
// If we used a path under any of these, the file-read*/file-write* check
// during connect(2)'s namei() lookup would pass via the OTHER rule even
// with the literal allow under test removed, and the negative-mutation
// test would become a no-op — exactly the failure class the AGENTS.md
// sandbox-exec testing convention exists to prevent.
//
// $HOME is the safe choice: the production profile grants no broad RW on
// $HOME (only narrow per-subdir grants for ~/.cache/nix, ~/.aws/sso,
// ~/.config/claude, etc., none of which match the .prism-2322-* prefix
// below). The resulting path stays well under the Darwin sun_path limit
// (104 bytes) because $HOME on Darwin is /Users/<user>, which is short.
//
// The directory is removed via t.Cleanup.
func isolatedSocketDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home (needed for an isolated socket path): %v", err)
	}
	dir, err := os.MkdirTemp(home, ".prism-2322-podman-test-")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for isolated socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newPodmanProxyProfileManager creates a Manager configured for a session
// that has opted in to the filtering podman proxy. ContainersEnabled=true
// and PodmanProxySockPath is set to the supplied socket path so generateProfile
// emits the literal RW allow under test.
//
// BareRoot is set so the section-6b ancestor block fires, granting
// file-test-existence / file-read-metadata along the $HOME → / chain.
// Without it the in-sandbox namei() traversal of the proxy path
// (e.g. /Users/<user>/.prism-2322-XYZ/podman.sock) would EPERM at
// /Users/<user>/ before ever reaching the literal allow under test, and
// the positive case would fail for an unrelated reason. The BareRoot
// itself is unused by the test — only its ancestor-traversal side effect
// matters.
func newPodmanProxyProfileManager(t *testing.T, proxyPath string) *container.Manager {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	// Two-level dir structure under HOME (same pattern as
	// newProfileManagerWithBareRoot in sandbox_exec_helpers_darwin_test.go):
	// without the wrap level, BareRoot lives directly under HOME and the
	// ancestor loop in generateProfile finds zero ancestors, skipping the
	// section-6b block entirely.
	wrap, err := os.MkdirTemp(home, ".prism-2322-bareroot-wrap-")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot, err := os.MkdirTemp(wrap, "bareroot-")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}

	instanceID := "integ-sbx-podman-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName:         "integ-sandbox-exec-podman-proxy-test",
		InstanceID:          instanceID,
		Worktree:            t.TempDir(),
		BareRoot:            bareRoot,
		ContainersEnabled:   true,
		PodmanProxySockPath: proxyPath,
		// Required since #1960 — see newProfileManager
		// (sandbox_exec_helpers_darwin_test.go).
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// TestSandboxExecPodmanProxy_ConnectSucceedsWhenLiteralAllowed is the
// positive integration test. It stands up a stub Unix-socket server in the
// test process at <runDir>/podman.sock and verifies that a Nix-socat
// invocation inside the sandbox can connect(2), exchange bytes, and read
// the sentinel response.
//
// The sentinel response on stdout proves end-to-end success:
//   - sandbox-exec applied the profile (no policy error).
//   - The literal RW allow on PodmanProxySockPath permitted the connect(2).
//   - The stub server accepted, wrote the sentinel, and closed.
//   - socat wrote the bytes through to stdout.
//
// Any other outcome — empty stdout, EPERM, exit non-zero — fails the test.
func TestSandboxExecPodmanProxy_ConnectSucceedsWhenLiteralAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixSocat := requireNixSocat(t)

	runDir := isolatedSocketDir(t)
	proxyPath := filepath.Join(runDir, "podman.sock")
	accepted := startStubPodmanProxyListener(t, proxyPath)

	m := newPodmanProxyProfileManager(t, proxyPath)
	prepared, _ := preparePositiveProfile(t, m)

	// Confirm the profile contains the literal allow under test. This is
	// a belt-and-braces check — the negative-mutation test below proves
	// the rule is load-bearing, but if the rule isn't present at all the
	// positive test would also fail (in a confusing way).
	wantClause := "(literal " + sbplQuoteForTest(proxyPath) + ")"
	if !strings.Contains(prepared.content, wantClause) {
		t.Fatalf("generated profile does not contain literal allow %q — the SBPL clause under test was not emitted.\nProfile:\n%s",
			wantClause, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// socat exit 0 + sentinel on stdout = pass. The `-t 2` gives the read
	// side 2s after EOF to drain any buffered bytes (default is 0.5s).
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixSocat, "-t", "2", "-", "UNIX-CONNECT:"+proxyPath)
	cmd.Stdin = bytes.NewReader(nil) // close stdin so socat exits after EOF
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("socat UNIX-CONNECT %s failed under production profile.\n"+
			"This means the literal allow for the podman proxy socket is not\n"+
			"load-bearing — connect(2) was denied even with ContainersEnabled=true.\n"+
			"Exit: %v\nOutput: %q\nProfile: %s",
			proxyPath, runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), strings.TrimSpace(podmanProxyProbeResponse)) {
		t.Errorf("stdout does not contain probe sentinel — connection may have been silently dropped.\n"+
			"want substring: %q\ngot stdout: %q\nProfile: %s",
			podmanProxyProbeResponse, string(out), testProfilePath)
	}
	if accepted.Load() == 0 {
		t.Errorf("stub server recorded zero accepts — the sandboxed socat never reached the listener (sandbox redirected the connect?)\nOutput: %q", string(out))
	}
}

// TestSandboxExecPodmanProxy_ConnectDeniedWithoutLiteralAllow is the
// paired negative-mutation test. It mutates the SBPL profile to OMIT the
// literal allow for PodmanProxySockPath, runs the same probe as the positive
// test, and asserts the connect FAILS. This is the AGENTS.md-mandated
// proof that the positive test is not a no-op: the SBPL clause is the
// specific mechanism that permits the connect(2).
//
// Mutation strategy: replace the literal allow line emitted by
// generateProfile with an empty string. If the substitution fails to
// match anything in the profile (e.g. because the rule text drifted),
// withMutatedProfile fails the test loudly — a silent no-op mutation is
// a common source of bogus passes.
func TestSandboxExecPodmanProxy_ConnectDeniedWithoutLiteralAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixSocat := requireNixSocat(t)

	runDir := isolatedSocketDir(t)
	proxyPath := filepath.Join(runDir, "podman.sock")
	accepted := startStubPodmanProxyListener(t, proxyPath)

	m := newPodmanProxyProfileManager(t, proxyPath)

	// The production generator emits the rule as:
	//
	//   (allow file-read* file-write*
	//     (literal "<proxyPath>"))
	//
	// Match the verbatim shape so a future formatting change fails
	// withMutatedProfile's no-op detection rather than silently passing.
	productionRule := "(allow file-read* file-write*\n  (literal " + sbplQuoteForTest(proxyPath) + "))\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, productionRule, "")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixSocat, "-t", "2", "-", "UNIX-CONNECT:"+proxyPath)
	cmd.Stdin = bytes.NewReader(nil)
	// Bound the runtime — a denied connect should fail fast, but if for
	// some reason socat hangs we want a deterministic test failure rather
	// than a CI timeout.
	cmd.WaitDelay = 10 * time.Second
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("socat UNIX-CONNECT %s succeeded (exit 0) WITHOUT the literal allow.\n"+
			"The negative-mutation test is not catching the regression — investigate.\n"+
			"Output: %q\nMutated profile: %s", proxyPath, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — UNIX-CONNECT correctly denied without literal allow (exit: %v, output: %q)", runErr, string(out))
	}

	// Sanity: the stub listener must not have received any successful
	// connections. A non-zero accept count would mean the sandbox allowed
	// the connect by some other rule, undermining the negative test.
	if accepted.Load() > 0 {
		t.Errorf("stub server recorded %d accept(s) despite the literal allow being removed — some other SBPL rule is permitting the connect", accepted.Load())
	}
}

// TestSandboxExecPodmanProxy_NoProxyMentionWhenDisabled is a defence-in-depth
// integration check: a session with ContainersEnabled=false has no literal
// allow rule for the proxy path, and the same socat probe inside the
// sandbox fails. This pairs with the unit-test default-off assertion
// (TestGenerateProfile_PodmanProxy_NoMentionWhenDisabled) at the
// integration layer — proving the gate is honoured end-to-end, not just
// at the SBPL-rendering layer.
func TestSandboxExecPodmanProxy_NoProxyMentionWhenDisabled(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixSocat := requireNixSocat(t)

	runDir := isolatedSocketDir(t)
	proxyPath := filepath.Join(runDir, "podman.sock")
	accepted := startStubPodmanProxyListener(t, proxyPath)

	// Construct a manager WITHOUT ContainersEnabled but otherwise shaped
	// identically to the positive test (BareRoot set so the section-6b
	// ancestor block fires — the test is otherwise gated by namei rather
	// than the rule under test).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-2322-disabled-wrap-")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot, err := os.MkdirTemp(wrap, "bareroot-")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-podman-disabled-test",
		InstanceID:  "integ-sbx-podman-disabled-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:    t.TempDir(),
		BareRoot:    bareRoot,
		// ContainersEnabled deliberately false.
		// PodmanProxySockPath deliberately populated to assert the gate
		// short-circuits before the rule is emitted.
		PodmanProxySockPath: proxyPath,
		GitUserName:         "test-user",
		GitUserEmail:        "test@example.com",
	}
	m := container.New(cfg)
	prepared, _ := preparePositiveProfile(t, m)

	if strings.Contains(prepared.content, proxyPath) {
		t.Fatalf("default-off profile mentions %q despite ContainersEnabled=false — gate is broken.\nProfile:\n%s",
			proxyPath, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixSocat, "-t", "2", "-", "UNIX-CONNECT:"+proxyPath)
	cmd.Stdin = bytes.NewReader(nil)
	cmd.WaitDelay = 10 * time.Second
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("UNIX-CONNECT succeeded on a ContainersEnabled=false session.\n"+
			"Output: %q\nProfile: %s", string(out), testProfilePath)
	}
	if accepted.Load() > 0 {
		t.Errorf("stub server recorded %d accept(s) on a ContainersEnabled=false session — gate failure", accepted.Load())
	}

	// Bonus sanity: net.Dial from the test process (NOT inside the sandbox)
	// must still succeed — this confirms the stub server is up and the only
	// thing preventing the in-sandbox connect is the SBPL policy itself,
	// not some unrelated listener misconfiguration. errors.Is os.ErrNotExist
	// is treated as "fine" because the listener may have been torn down by
	// cleanup race; the test is best-effort here.
	c, dialErr := net.DialTimeout("unix", proxyPath, time.Second)
	if dialErr == nil {
		_ = c.Close()
	} else if !errors.Is(dialErr, os.ErrNotExist) {
		t.Logf("out-of-sandbox dial probe: %v (informational — not a failure)", dialErr)
	}
}
