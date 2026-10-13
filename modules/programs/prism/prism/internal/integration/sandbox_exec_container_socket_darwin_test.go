//go:build darwin

package integration_test

// sandbox_exec_container_socket_darwin_test.go — the Unix-socket connect
// allowlist of the sandbox-exec profile (sections 17b and 17c of
// generateProfile, issue #3066).
//
// Section 17's (allow network*) admits a connect(2) to any Unix domain
// socket, so a sandboxed agent could reach the podman machine API socket
// under $TMPDIR/podman/ and drive the podman machine with no filter. It
// could also reach the tmux control socket, the user ssh-agent, and the
// AeroSpace window-manager socket under /private/tmp — each a host escape.
//
// Section 17b denies `network-outbound` for every Unix-socket path except an
// allowlist (the host-API socket dir, mDNSResponder, syslog, the nix daemon
// socket, and the scratch trees /tmp, /private/tmp, the session work dir, the
// worktree, and the bare repo). The leading (subpath "/") scopes the rule to
// path-bearing connects, so TCP is untouched. Section 17c then re-denies the
// three host-escape sockets that live inside the broad /private/tmp
// allowance.
//
// These tests invoke /usr/bin/sandbox-exec against the production profile and
// a Unix-socket connect probe, with paired negatives that mutate the profile
// to prove each rule is load-bearing. See docs/sandbox-exec-testing.md.
//
// The probe is a tiny Go program built with CGO disabled, so the macos-15 CI
// job needs no C compiler. It exits 0 when the connect succeeds and non-zero
// when the sandbox denies it.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// uxConnectProbeSrc is the source of the Unix-socket connect probe. It dials
// os.Args[1] and exits 0 on success, 1 on failure.
const uxConnectProbeSrc = `package main

import (
	"net"
	"os"
	"time"
)

func main() {
	c, err := net.DialTimeout("unix", os.Args[1], 3*time.Second)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	_ = c.Close()
	os.Stdout.WriteString("UNIX-CONNECTED\n")
}
`

// buildUnixConnectProbe compiles the probe into a t.TempDir() and returns its
// path. The build is CGO-disabled so it needs no C compiler on the bare
// macos-15 runner. The temp dir lives under os.TempDir() ($TMPDIR), which the
// profile's section 3b grants exec, so sandbox-exec can run the probe.
func buildUnixConnectProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "uxprobe.go")
	if err := os.WriteFile(src, []byte(uxConnectProbeSrc), 0o600); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	bin := filepath.Join(dir, "uxprobe")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the unix-connect probe (go unavailable?): %v\n%s", err, out)
	}
	return bin
}

// startUnixListener binds a stream Unix socket at path, drains and closes
// incoming connections, and registers cleanup. The test process runs OUTSIDE
// the sandbox, so it can bind anywhere it can write.
func startUnixListener(t *testing.T, path string) {
	t.Helper()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

// runProbe runs the probe under the given profile against sockPath and
// returns its combined output and error. A nil error means the connect
// succeeded.
func runProbe(t *testing.T, profilePath, probe, sockPath string) (string, error) {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, probe, sockPath)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// deniedPodmanSocketPath creates a listener under os.TempDir()/podman-ish/,
// which mirrors the real podman machine socket location ($TMPDIR/podman/) and
// is NOT in the section-17b allowlist. Returns the socket path.
func deniedPodmanSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "p3066pod")
	if err != nil {
		t.Fatalf("mkdir podman-ish dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	startUnixListener(t, sock)
	return sock
}

// TestSandboxExecContainerSocket_DeniedUnderProductionProfile is the positive
// test. Under the production profile, a connect to a Unix socket in the
// podman machine location ($TMPDIR, not allowlisted) fails, while a connect to
// a socket under the allowlisted /private/tmp scratch tree succeeds.
func TestSandboxExecContainerSocket_DeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildUnixConnectProbe(t)

	podmanSock := deniedPodmanSocketPath(t)

	allowedDir, err := os.MkdirTemp("/private/tmp", "prism-3066-allowed")
	if err != nil {
		t.Fatalf("mkdir allowed dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(allowedDir) })
	allowedSock := filepath.Join(allowedDir, "a.sock")
	startUnixListener(t, allowedSock)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// The podman-location socket must be denied by section 17b.
	if out, runErr := runProbe(t, profilePath, probe, podmanSock); runErr == nil {
		t.Errorf("connect to podman-location socket %s succeeded under the production profile — section 17b did not deny it.\nOutput: %s\nProfile: %s",
			podmanSock, out, profilePath)
	}

	// The /private/tmp scratch socket must be allowed (the go-test quality
	// gate binds its sockets there in-sandbox).
	if out, runErr := runProbe(t, profilePath, probe, allowedSock); runErr != nil || !strings.Contains(out, "UNIX-CONNECTED") {
		t.Errorf("connect to allowed /private/tmp socket %s failed under the production profile — section 17b is too strict.\nExit: %v\nOutput: %s\nProfile: %s",
			allowedSock, runErr, out, profilePath)
	}
}

// TestSandboxExecContainerSocket_PodmanConnectsWithoutAllowlistDeny is the
// paired negative for section 17b. It neutralises the rule's (subpath "/")
// scope term so the require-all never matches, then asserts the podman-
// location connect succeeds — proving the section-17b deny is what blocks it.
func TestSandboxExecContainerSocket_PodmanConnectsWithoutAllowlistDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildUnixConnectProbe(t)

	podmanSock := deniedPodmanSocketPath(t)

	m := newProfileManager(t)
	// The sequence below is unique to the section-17b deny: the secrets.d
	// require-all (section 3c) is followed by (require-any), and the only
	// other (subpath "/") lives in a plain allow, not a require-all.
	const scope = "(require-all\n    (subpath \"/\")\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, scope, "(require-all\n    (subpath \"/prism-3066-nonexistent\")\n", 1)
	})

	if out, runErr := runProbe(t, mutatedPath, probe, podmanSock); runErr != nil || !strings.Contains(out, "UNIX-CONNECTED") {
		t.Errorf("connect to %s still failed with the section-17b scope neutralised, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			podmanSock, runErr, out, mutatedPath)
	}
}

// TestSandboxExecContainerSocket_EscapeDeniedUnderProductionProfile is the
// positive test for section 17c. A socket under a tmux-shaped directory in
// /private/tmp is denied, even though section 17b allows /private/tmp,
// because section 17c re-denies it.
func TestSandboxExecContainerSocket_EscapeDeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildUnixConnectProbe(t)

	// A high, fixed uid-shaped number that no real tmux server uses, so the
	// dir cannot collide with the running user's tmux socket dir.
	tmuxDir := "/private/tmp/tmux-2147483646"
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatalf("mkdir tmux-shaped dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	escapeSock := filepath.Join(tmuxDir, "probe.sock")
	startUnixListener(t, escapeSock)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if out, runErr := runProbe(t, profilePath, probe, escapeSock); runErr == nil {
		t.Errorf("connect to tmux-shaped socket %s succeeded under the production profile — section 17c did not re-deny it inside the /private/tmp allowance.\nOutput: %s\nProfile: %s",
			escapeSock, out, profilePath)
	}
}

// TestSandboxExecContainerSocket_EscapeConnectsWithoutReDeny is the paired
// negative for section 17c. With the re-deny removed, the tmux-shaped socket
// connects (section 17b allows /private/tmp) — proving section 17c is what
// blocks the escape.
func TestSandboxExecContainerSocket_EscapeConnectsWithoutReDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildUnixConnectProbe(t)

	tmuxDir := "/private/tmp/tmux-2147483646"
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatalf("mkdir tmux-shaped dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	escapeSock := filepath.Join(tmuxDir, "probe.sock")
	startUnixListener(t, escapeSock)

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, escapeReDenyBlock, "", 1)
	})

	if out, runErr := runProbe(t, mutatedPath, probe, escapeSock); runErr != nil || !strings.Contains(out, "UNIX-CONNECTED") {
		t.Errorf("connect to %s still failed with the section-17c re-deny removed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			escapeSock, runErr, out, mutatedPath)
	}
}

// escapeReDenyBlock is the exact section-17c clause of generateProfile,
// duplicated here (the generator's containerRuntimeSocketEscapeReDeny constant
// is unexported). withMutatedProfile fails loudly if this text is not present
// verbatim, so a drift from the generator cannot pass silently.
const escapeReDenyBlock = "(deny network-outbound\n" +
	"  (regex #\"^/private/tmp/tmux-[0-9]+/\")\n" +
	"  (regex #\"^/tmp/tmux-[0-9]+/\")\n" +
	"  (regex #\"^/private/tmp/com\\.apple\\.launchd\\.\")\n" +
	"  (regex #\"^/tmp/com\\.apple\\.launchd\\.\")\n" +
	"  (regex #\"^/private/tmp/bobko\\.aerospace-\")\n" +
	"  (regex #\"^/tmp/bobko\\.aerospace-\"))\n"
