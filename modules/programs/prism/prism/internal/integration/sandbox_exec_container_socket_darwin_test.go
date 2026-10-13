//go:build darwin

package integration_test

// sandbox_exec_container_socket_darwin_test.go — the Unix-socket connect
// policy of the sandbox-exec profile (sections 17b and 17c of
// generateProfile, #3066).
//
// Section 17b denies `network-outbound` for every Unix-socket path except an
// allowlist. Its leading (subpath "/") scopes the deny to path-bearing
// connects, so TCP stays open. The podman machine socket is under $TMPDIR,
// outside the allowlist. Section 17c re-denies the host-escape sockets inside
// the /private/tmp allowance: tmux, the user ssh-agent, AeroSpace, and the
// .NET diagnostic IPC socket.
//
// Each positive test runs a connect probe under the production profile. Each
// paired negative mutates the profile and shows that the rule under test is
// what decides the result. See docs/sandbox-exec-testing.md.
//
// The probe is a small Go program built with CGO disabled, so the macos-15 CI
// job needs no C compiler. It exits 0 when the connect succeeds.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// connectProbeSrc is the source of the connect probe.
//
//	probe <path>                      connect to a Unix socket
//	probe tcp:<host:port>             connect over TCP
//	probe --move <src> <dst> <path>   os.Rename(src, dst), then connect to <path>
//
// The --move mode models the rename-bypass attack: move a protected socket
// (or its directory) to an allowed path, then connect. It prints
// RENAME-DENIED and exits 2 when the sandbox denies the rename.
const connectProbeSrc = `package main

import (
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 4 && args[0] == "--move" {
		if err := os.Rename(args[1], args[2]); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Stdout.WriteString("RENAME-DENIED\n")
			os.Exit(2)
		}
		args = args[3:]
	}
	network, addr := "unix", args[0]
	if strings.HasPrefix(addr, "tcp:") {
		network, addr = "tcp", strings.TrimPrefix(addr, "tcp:")
	}
	c, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	_ = c.Close()
	os.Stdout.WriteString("CONNECTED\n")
}
`

// connectedMarker is what the probe prints when its connect succeeds.
const connectedMarker = "CONNECTED"

// sectionNetworkScope is the opening of the section-17b deny. It is unique in
// the profile: the secrets.d require-all (section 3c) opens with
// (require-any), and the only other (subpath "/") is in a plain allow.
const sectionNetworkScope = "(require-all\n    (subpath \"/\")\n"

// buildConnectProbe compiles the probe into a t.TempDir(). That dir is under
// os.TempDir(), which section 3b of the profile grants, so sandbox-exec can
// run the probe.
func buildConnectProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(src, []byte(connectProbeSrc), 0o600); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	bin := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the connect probe (go unavailable?): %v\n%s", err, out)
	}
	return bin
}

// startUnixListener binds a stream Unix socket at path and closes each
// connection it accepts. The test process runs outside the sandbox.
func startUnixListener(t *testing.T, path string) {
	t.Helper()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)
}

// startTCPListener binds 127.0.0.1:0 and returns the probe address
// "tcp:127.0.0.1:<port>".
func startTCPListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAndClose(ln)
	return "tcp:127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// runProbe runs the probe under the profile with the given probe args. A nil
// error means the connect succeeded.
func runProbe(t *testing.T, profilePath, probe string, probeArgs ...string) (string, error) {
	t.Helper()
	args := append([]string{"-f", profilePath, probe}, probeArgs...)
	out, err := exec.Command(sandboxExecPath, args...).CombinedOutput()
	return string(out), err
}

// socketInNewDir creates a temp dir under parent with the name prefix
// pattern, binds a listener at <dir>/a.sock, and returns the socket path.
func socketInNewDir(t *testing.T, parent, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		t.Fatalf("mkdir %s/%s*: %v", parent, pattern, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	startUnixListener(t, sock)
	return sock
}

// podmanLocationSocket returns a socket under os.TempDir(), the location of
// the real podman machine socket ($TMPDIR/podman/). The CI job sets TMPDIR to
// the /var/folders user temp dir, which is outside the section-17b allowlist.
func podmanLocationSocket(t *testing.T) string {
	return socketInNewDir(t, os.TempDir(), "p3066pod")
}

func expectDenied(t *testing.T, profilePath, probe, target, why string) {
	t.Helper()
	if out, err := runProbe(t, profilePath, probe, target); err == nil {
		t.Errorf("connect to %s succeeded: %s.\nOutput: %s\nProfile: %s", target, why, out, profilePath)
	}
}

func expectConnected(t *testing.T, profilePath, probe, target, why string) {
	t.Helper()
	if out, err := runProbe(t, profilePath, probe, target); err != nil || !strings.Contains(out, connectedMarker) {
		t.Errorf("connect to %s failed: %s.\nExit: %v\nOutput: %s\nProfile: %s", target, why, err, out, profilePath)
	}
}

// TestSandboxExecContainerSocket_DeniedUnderProductionProfile: under the
// production profile, a connect to the podman machine location fails. A
// connect to a socket under the allowlisted /private/tmp and a loopback TCP
// connect both succeed.
func TestSandboxExecContainerSocket_DeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	podmanSock := podmanLocationSocket(t)
	allowedSock := socketInNewDir(t, "/private/tmp", "prism-3066-allowed")
	tcpAddr := startTCPListener(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	expectDenied(t, profilePath, probe, podmanSock,
		"section 17b did not deny the podman machine location")
	expectConnected(t, profilePath, probe, allowedSock,
		"section 17b denies the /private/tmp scratch tree, where go test binds its sockets")
	expectConnected(t, profilePath, probe, tcpAddr,
		"section 17b denies TCP; its (subpath \"/\") scope must match Unix-socket connects only")
}

// TestSandboxExecContainerSocket_PodmanConnectsWithoutAllowlistDeny is the
// paired negative for the podman case. It points the section-17b scope at a
// path that does not exist, so the deny never matches, and the podman
// location connect succeeds.
func TestSandboxExecContainerSocket_PodmanConnectsWithoutAllowlistDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	podmanSock := podmanLocationSocket(t)

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, sectionNetworkScope, "(require-all\n    (subpath \"/prism-3066-nonexistent\")\n", 1)
	})

	expectConnected(t, mutatedPath, probe, podmanSock,
		"the connect still fails with the section-17b scope neutralised, so the positive test proves nothing")
}

// TestSandboxExecContainerSocket_TCPDeniedWithoutScope is the paired negative
// for the TCP case. It removes the (subpath "/") scope from section 17b, and
// the loopback TCP connect then fails. The scope is what keeps TCP open.
func TestSandboxExecContainerSocket_TCPDeniedWithoutScope(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	tcpAddr := startTCPListener(t)

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, sectionNetworkScope, "(require-all\n", 1)
	})

	expectDenied(t, mutatedPath, probe, tcpAddr,
		"TCP still connects without the (subpath \"/\") scope, so the TCP check in the positive test proves nothing")
}

// TestSandboxExecContainerSocket_EscapeDeniedUnderProductionProfile: under the
// production profile, a socket under a tmux-shaped dir and a socket under a
// .NET-diagnostic-shaped name in /private/tmp are denied. Section 17b allows
// /private/tmp, so section 17c is what denies them.
func TestSandboxExecContainerSocket_EscapeDeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	// os.MkdirTemp appends digits, so the dir names match the 17c regexes
	// and do not collide with a real tmux dir or a concurrent test run.
	tmuxSock := socketInNewDir(t, "/private/tmp", "tmux-")
	dotnetSock := socketInNewDir(t, "/private/tmp", "dotnet-diagnostic-")

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	expectDenied(t, profilePath, probe, tmuxSock,
		"section 17c did not re-deny the tmux-shaped socket")
	expectDenied(t, profilePath, probe, dotnetSock,
		"section 17c did not re-deny the .NET-diagnostic-shaped socket")
}

// TestSandboxExecContainerSocket_EscapeConnectsWithoutReDeny is the paired
// negative for section 17c. With the re-deny removed, both sockets connect.
func TestSandboxExecContainerSocket_EscapeConnectsWithoutReDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	tmuxSock := socketInNewDir(t, "/private/tmp", "tmux-")
	dotnetSock := socketInNewDir(t, "/private/tmp", "dotnet-diagnostic-")

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, escapeReDenyBlock, "", 1)
	})

	for _, sock := range []string{tmuxSock, dotnetSock} {
		expectConnected(t, mutatedPath, probe, sock,
			"the connect still fails with section 17c removed, so the positive test proves nothing")
	}
}

// podmanShapedSocket creates <$TMPDIR>/<rand>/podman/api.sock with a live
// listener and returns the socket path and the podman directory. The path
// matches the section-3d regex `^/var/folders/.*/podman(/|$)`. The `podman`
// component is nested under a MkdirTemp parent, so the test never touches the
// real podman machine directory at <$TMPDIR>/podman.
func podmanShapedSocket(t *testing.T) (sock, podmanDir string) {
	t.Helper()
	base, err := os.MkdirTemp(os.TempDir(), "prism-3066-")
	if err != nil {
		t.Fatalf("mkdir temp base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	podmanDir = filepath.Join(base, "podman")
	if err := os.Mkdir(podmanDir, 0o700); err != nil {
		t.Fatalf("mkdir podman dir: %v", err)
	}
	sock = filepath.Join(podmanDir, "api.sock")
	startUnixListener(t, sock)
	return sock, podmanDir
}

// TestSandboxExecContainerSocket_RenameBypassDeniedUnderProductionProfile:
// under the production profile, a sandboxed process cannot move a protected
// socket, or the directory that holds it, to an allowed path. The connect
// deny (sections 17b/17c) filters by path, so a successful rename would reach
// the socket at its new path. Section 3d denies the rename.
//
// The test moves from the podman location under $TMPDIR. The source unlink is
// denied there, so the destination does not matter. (A /private/tmp source,
// for the tmux family, cannot be exercised here: writeAugmentedPositiveProfile
// appends a broad `(allow file-write* (subpath "/private/tmp"))` for the test
// binary, which outranks section 3d inside /private/tmp. The production
// profile has no such allow; the content test
// TestGenerateProfile_HostSocketWriteDeny asserts the tmux/launchd/aerospace/
// dotnet entries are present.)
func TestSandboxExecContainerSocket_RenameBypassDeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	sock, podmanDir := podmanShapedSocket(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Move the socket FILE out of the podman dir into /private/tmp.
	movedFile := "/private/tmp/prism-3066-mv-file.sock"
	t.Cleanup(func() { _ = os.Remove(movedFile) })
	if out, runErr := runProbe(t, profilePath, probe, "--move", sock, movedFile, movedFile); runErr == nil || strings.Contains(out, connectedMarker) {
		t.Errorf("moved the podman socket file and connected — section 3d did not deny the file rename.\nOutput: %s\nProfile: %s", out, profilePath)
	}
	// Move the podman DIRECTORY (with the socket inside) into /private/tmp.
	movedDir := "/private/tmp/prism-3066-mv-dir"
	t.Cleanup(func() { _ = os.RemoveAll(movedDir) })
	if out, runErr := runProbe(t, profilePath, probe, "--move", podmanDir, movedDir, filepath.Join(movedDir, "api.sock")); runErr == nil || strings.Contains(out, connectedMarker) {
		t.Errorf("moved the podman directory and connected — section 3d did not deny the directory rename.\nOutput: %s\nProfile: %s", out, profilePath)
	}
}

// TestSandboxExecContainerSocket_RenameBypassWorksWithoutWriteDeny is the
// paired negative for section 3d. With the write deny removed, the rename
// succeeds and the connect to the moved socket succeeds — which is the bypass
// section 3d closes.
func TestSandboxExecContainerSocket_RenameBypassWorksWithoutWriteDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	sock, _ := podmanShapedSocket(t)
	moved := "/private/tmp/prism-3066-mv-neg.sock"
	t.Cleanup(func() { _ = os.Remove(moved) })

	m := newProfileManager(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, hostSocketWriteDenyBlock, "", 1)
	})

	if out, runErr := runProbe(t, mutatedPath, probe, "--move", sock, moved, moved); runErr != nil || !strings.Contains(out, connectedMarker) {
		t.Errorf("the move+connect still failed with section 3d removed, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s", runErr, out, mutatedPath)
	}
}

// escapeReDenyBlock is the section-17c clause of generateProfile
// (hostEscapeSocketReDeny, unexported). withMutatedProfile fails when the
// text is not in the profile, so a copy that drifts from the generator
// fails the negative test.
const escapeReDenyBlock = "(deny network-outbound\n" +
	"  (regex #\"^/private/tmp/tmux-[0-9]+/\")\n" +
	"  (regex #\"^/tmp/tmux-[0-9]+/\")\n" +
	"  (regex #\"^/private/tmp/com\\.apple\\.launchd\\.\")\n" +
	"  (regex #\"^/tmp/com\\.apple\\.launchd\\.\")\n" +
	"  (regex #\"^/private/tmp/bobko\\.aerospace-\")\n" +
	"  (regex #\"^/tmp/bobko\\.aerospace-\")\n" +
	"  (regex #\"^/private/tmp/dotnet-diagnostic-\")\n" +
	"  (regex #\"^/tmp/dotnet-diagnostic-\"))\n"

// hostSocketWriteDenyBlock is the section-3d clause of generateProfile
// (hostSocketWriteDeny, unexported). Kept in sync by the same
// withMutatedProfile no-op guard as escapeReDenyBlock.
const hostSocketWriteDenyBlock = "(deny file-write*\n" +
	"  (regex #\"^/var/folders/.*/podman(/|$)\")\n" +
	"  (regex #\"^/private/var/folders/.*/podman(/|$)\")\n" +
	"  (regex #\"^/private/tmp/tmux-[0-9]+(/|$)\")\n" +
	"  (regex #\"^/tmp/tmux-[0-9]+(/|$)\")\n" +
	"  (regex #\"^/private/tmp/com\\.apple\\.launchd\\.[^/]+(/|$)\")\n" +
	"  (regex #\"^/tmp/com\\.apple\\.launchd\\.[^/]+(/|$)\")\n" +
	"  (regex #\"^/private/tmp/bobko\\.aerospace-\")\n" +
	"  (regex #\"^/tmp/bobko\\.aerospace-\")\n" +
	"  (regex #\"^/private/tmp/dotnet-diagnostic-\")\n" +
	"  (regex #\"^/tmp/dotnet-diagnostic-\"))\n"
