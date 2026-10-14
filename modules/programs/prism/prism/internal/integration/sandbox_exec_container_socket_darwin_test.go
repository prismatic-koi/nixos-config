//go:build darwin

package integration_test

// sandbox_exec_container_socket_darwin_test.go — the Unix-socket connect
// policy of the sandbox-exec profile (section 17b of generateProfile; #3066,
// #3147, #3150).
//
// Section 17b denies `network-outbound` for every Unix-socket path except an
// allowlist. Its leading (subpath "/") scopes the deny to path-bearing
// connects, so TCP stays open. The allowlist holds the per-session TMPDIR
// and no shared temp tree: not /private/tmp, not /tmp, and not the user
// $TMPDIR. Thus a host socket in a shared temp tree is denied: the podman
// machine, tmux, the user ssh-agent, AeroSpace, a .NET diagnostic socket,
// nvim, or any other. The session has no write grant on a shared temp tree
// either, so it cannot move such a socket into its per-session TMPDIR.
//
// Each positive test runs a probe under the production profile. Each paired
// negative mutates the profile and shows that the rule under test is what
// decides the result. See docs/sandbox-exec-testing.md.
//
// The probe is a small Go program built with CGO disabled, so the macos-15 CI
// job needs no C compiler. The tests of a /tmp rule use the probe extras
// (augmentProfileForProbe), not the test-harness extras, because the
// test-harness extras grant write on /private/tmp.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// connectProbeSrc is the source of the probe.
//
//	probe <path>                      connect to a Unix socket
//	probe tcp:<host:port>             connect over TCP
//	probe --move <src> <dst> <path>   os.Rename(src, dst), then connect to <path>
//	probe --bind <path>               listen on <path>, then connect to it
//	probe --create <path>             write a new file
//	probe --read <path>               read a file
//	probe --remove <path>             remove a file or an empty dir
//	probe --rename <src> <dst>        os.Rename(src, dst)
//
// A connect that succeeds prints CONNECTED. A --create, --read, --remove, or
// --rename that succeeds prints OP-OK. A failed --move rename prints
// RENAME-DENIED and exits 2. Every failure exits non-zero.
const connectProbeSrc = `package main

import (
	"net"
	"os"
	"strings"
	"time"
)

func fail(marker string, err error, code int) {
	os.Stderr.WriteString(err.Error() + "\n")
	if marker != "" {
		os.Stdout.WriteString(marker + "\n")
	}
	os.Exit(code)
}

func op(err error) {
	if err != nil {
		fail("", err, 3)
	}
	os.Stdout.WriteString("OP-OK\n")
	os.Exit(0)
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) >= 2 && args[0] == "--create":
		op(os.WriteFile(args[1], []byte("x"), 0o600))
	case len(args) >= 2 && args[0] == "--read":
		_, err := os.ReadFile(args[1])
		op(err)
	case len(args) >= 2 && args[0] == "--remove":
		op(os.Remove(args[1]))
	case len(args) >= 3 && args[0] == "--rename":
		op(os.Rename(args[1], args[2]))
	case len(args) >= 2 && args[0] == "--bind":
		ln, err := net.Listen("unix", args[1])
		if err != nil {
			fail("", err, 4)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		args = args[1:]
	case len(args) >= 4 && args[0] == "--move":
		if err := os.Rename(args[1], args[2]); err != nil {
			fail("RENAME-DENIED", err, 2)
		}
		args = args[3:]
	}
	network, addr := "unix", args[0]
	if strings.HasPrefix(addr, "tcp:") {
		network, addr = "tcp", strings.TrimPrefix(addr, "tcp:")
	}
	c, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		fail("", err, 1)
	}
	_ = c.Close()
	os.Stdout.WriteString("CONNECTED\n")
}
`

// connectedMarker is what the probe prints when its connect succeeds.
const connectedMarker = "CONNECTED"

// opOKMarker is what the probe prints when a file operation succeeds.
const opOKMarker = "OP-OK"

// sectionNetworkScope is the opening of the section-17b deny. It is unique in
// the profile: the secrets.d require-all (section 3c) opens with
// (require-any), and the only other (subpath "/") is in a plain allow.
const sectionNetworkScope = "(require-all\n    (subpath \"/\")\n"

// buildConnectProbe compiles the probe into a t.TempDir(). That dir is under
// the user $TMPDIR, which section 2 of the profile grants read and
// map-executable, so sandbox-exec can run the probe.
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
// error means the probe succeeded.
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

// privateTmpSocket binds a listener at a unique path directly in
// /private/tmp and returns it. It models a user socket there that the
// profile does not name, such as macron-type.sock on m4mac.
func privateTmpSocket(t *testing.T) string {
	t.Helper()
	sock := filepath.Join("/private/tmp", "prism-3150-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".sock")
	startUnixListener(t, sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	return sock
}

// sessionTmpSocket binds a listener in the per-session TMPDIR of m and
// returns the socket path in the /tmp form, the form that TMPDIR holds.
// Prepare must have run, so the dir exists.
func sessionTmpSocket(t *testing.T, m *container.Manager, name string) string {
	t.Helper()
	sock := filepath.Join(sessionTmpDirOf(t, m), name)
	startUnixListener(t, sock)
	return sock
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
// production profile, a connect to a socket under the user $TMPDIR (the
// podman machine location, and an nvim-shaped location) fails. A connect to
// a socket in the per-session TMPDIR and a loopback TCP connect both
// succeed.
func TestSandboxExecContainerSocket_DeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	podmanSock := podmanLocationSocket(t)
	nvimSock, _ := nvimShapedSocket(t)
	tcpAddr := startTCPListener(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	ownSock := sessionTmpSocket(t, m, "a.sock")

	expectDenied(t, profilePath, probe, podmanSock,
		"section 17b did not deny the podman machine location under the user $TMPDIR")
	expectDenied(t, profilePath, probe, nvimSock,
		"section 17b did not deny the nvim RPC location under the user $TMPDIR")
	expectConnected(t, profilePath, probe, ownSock,
		"section 17b denies the per-session TMPDIR, where go test binds its sockets")
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

// sharedTmpSockets binds the host-socket shapes of /private/tmp: a socket
// directly in /private/tmp that the profile does not name, a tmux-shaped
// socket, a .NET-diagnostic-shaped socket, and the podman and nvim sockets
// of the TMPDIR-unset location. The last entry is the generic socket by its
// /tmp path.
func sharedTmpSockets(t *testing.T) []string {
	t.Helper()
	generic := privateTmpSocket(t)
	return []string{
		generic,
		// os.MkdirTemp appends digits, so the dir names do not collide with
		// a real tmux dir or a concurrent test run.
		socketInNewDir(t, "/private/tmp", "tmux-"),
		socketInNewDir(t, "/private/tmp", "dotnet-diagnostic-"),
		podmanTmpSocket(t),
		nvimTmpSocket(t),
		strings.TrimPrefix(generic, "/private"),
	}
}

// TestSandboxExecContainerSocket_SharedTmpDeniedUnderProductionProfile:
// under the production profile, a connect to each host-socket shape in
// /private/tmp fails. No profile rule names them: section 17b has no
// /private/tmp or /tmp allowance, so the class is closed (#3150).
func TestSandboxExecContainerSocket_SharedTmpDeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	socks := sharedTmpSockets(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)

	for _, sock := range socks {
		expectDenied(t, profilePath, probe, sock,
			"section 17b did not deny a socket in the shared /tmp tree")
	}
}

// TestSandboxExecContainerSocket_SharedTmpConnectsWithAllowance is the paired
// negative: with a /private/tmp and /tmp allowance added to section 17b, every
// socket of the positive test connects. This proves that the missing
// allowance is what denies them.
func TestSandboxExecContainerSocket_SharedTmpConnectsWithAllowance(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	socks := sharedTmpSockets(t)

	m := newProfileManager(t)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return strings.Replace(p, sectionNetworkScope, sectionNetworkScope+
			"    (require-not (subpath \"/private/tmp\"))\n    (require-not (subpath \"/tmp\"))\n", 1)
	})

	for _, sock := range socks {
		expectConnected(t, mutatedPath, probe, sock,
			"the connect still fails with a /private/tmp allowance, so the positive test proves nothing")
	}
}

// podmanShapedSocket creates <$TMPDIR>/<rand>/podman/api.sock with a live
// listener and returns the socket path and the podman directory. The
// `podman` component is nested under a MkdirTemp parent, so the test never
// touches the real podman machine directory at <$TMPDIR>/podman.
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

// nvimShapedSocket creates <$TMPDIR>/<rand>/nvim.<user>/<rand2>/nvim.<pid>.0
// with a live listener and returns the socket path and the nvim.<user>
// directory: the location class of the real host nvim RPC socket. The
// nvim.<user> component sits under a MkdirTemp parent, so the test never
// touches the real host nvim directory. A plain listener stands in for nvim:
// the deny is path-based, and the macos-15 CI job (which fails on any SKIP)
// has no nvim to depend on.
func nvimShapedSocket(t *testing.T) (sock, nvimDir string) {
	t.Helper()
	// Short names keep <$TMPDIR>/<base>/nvim.u/r/nvim.1.0 under the Darwin
	// sun_path limit (~104 bytes).
	base, err := os.MkdirTemp(os.TempDir(), "p3147n")
	if err != nil {
		t.Fatalf("mkdir temp base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	nvimDir = filepath.Join(base, "nvim.u")
	inner := filepath.Join(nvimDir, "r")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("mkdir nvim dir: %v", err)
	}
	sock = filepath.Join(inner, "nvim.1.0")
	startUnixListener(t, sock)
	return sock, nvimDir
}

// podmanTmpSocket creates /private/tmp/podman/<unique>-api.sock with a live
// listener and returns the socket path. This models the podman machine API
// socket location when podman runs with TMPDIR unset. The socket name is
// unique, and the directory is removed only when this helper created it, so
// the helper never clobbers a real podman runtime dir.
func podmanTmpSocket(t *testing.T) string {
	t.Helper()
	dir := "/private/tmp/podman"
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		t.Cleanup(func() { _ = os.Remove(dir) })
	}
	sock := filepath.Join(dir, "prism-3147-"+strconv.Itoa(os.Getpid())+"-api.sock")
	startUnixListener(t, sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	return sock
}

// nvimTmpSocket creates /private/tmp/nvim.<unique>/<rand>/nvim.1.0 with a live
// listener and returns the socket path. This models the nvim RPC socket
// location when nvim runs with TMPDIR unset. The nvim.<x> name is unique, so
// the helper never collides with a real host nvim dir.
func nvimTmpSocket(t *testing.T) string {
	t.Helper()
	nvimDir := filepath.Join("/private/tmp", "nvim.p3147-"+strconv.Itoa(os.Getpid()))
	inner := filepath.Join(nvimDir, "r")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", inner, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nvimDir) })
	sock := filepath.Join(inner, "nvim.1.0")
	startUnixListener(t, sock)
	return sock
}

// renameBypassCase is one move of a host socket, or of the dir that holds
// it, into the per-session TMPDIR, where section 17b allows the connect.
type renameBypassCase struct {
	name    string
	src     string // the node to move
	dstName string // the new name in the per-session TMPDIR
	connect string // the socket path below the moved node, "" for the node itself
}

// renameBypassCases builds a host socket in each shared temp tree and
// returns the moves that would reach it from the per-session TMPDIR.
func renameBypassCases(t *testing.T) []renameBypassCase {
	t.Helper()
	podmanSock, podmanDir := podmanShapedSocket(t)
	_, nvimDir := nvimShapedSocket(t)
	tmpSock := privateTmpSocket(t)
	tmuxSock := socketInNewDir(t, "/private/tmp", "tmux-")
	return []renameBypassCase{
		{"podman socket file in $TMPDIR", podmanSock, "mv-podman.sock", ""},
		{"podman dir in $TMPDIR", podmanDir, "mv-podman", "api.sock"},
		{"nvim dir in $TMPDIR", nvimDir, "mv-nvim", filepath.Join("r", "nvim.1.0")},
		{"socket file in /private/tmp", tmpSock, "mv-tmp.sock", ""},
		{"tmux dir in /private/tmp", filepath.Dir(tmuxSock), "mv-tmux", "a.sock"},
	}
}

func (c renameBypassCase) probeArgs(tmpDir string) []string {
	dst := filepath.Join(tmpDir, c.dstName)
	target := dst
	if c.connect != "" {
		target = filepath.Join(dst, c.connect)
	}
	return []string{"--move", c.src, dst, target}
}

// TestSandboxExecContainerSocket_RenameBypassDeniedUnderProductionProfile:
// under the production profile, the session cannot move a host socket, or
// the dir that holds it, from a shared temp tree into its per-session
// TMPDIR, where a connect is allowed. It has no write grant on the source,
// so the unlink of the source fails.
func TestSandboxExecContainerSocket_RenameBypassDeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	cases := renameBypassCases(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	tmpDir := sessionTmpDirOf(t, m)

	for _, c := range cases {
		out, runErr := runProbe(t, profilePath, probe, c.probeArgs(tmpDir)...)
		if runErr == nil || strings.Contains(out, connectedMarker) {
			t.Errorf("%s: moved into the per-session TMPDIR and connected — the shared temp tree is writable.\nOutput: %s\nProfile: %s", c.name, out, profilePath)
		}
		if !strings.Contains(out, "RENAME-DENIED") {
			t.Errorf("%s: the move was not the step that failed.\nExit: %v\nOutput: %s", c.name, runErr, out)
		}
	}
}

// sharedTmpWriteGrant is the write grant on the shared temp trees that the
// profile does not have. The paired negatives append it.
func sharedTmpWriteGrant() string {
	g := "(allow file-read* file-write*\n  (subpath \"/private/tmp\")\n  (subpath \"/tmp\")"
	for _, p := range []string{os.TempDir(), resolvedTempDir()} {
		g += "\n  (subpath " + sbplQuoteForTest(filepath.Clean(p)) + ")"
	}
	return g + ")\n"
}

// resolvedTempDir returns os.TempDir() with its symlinks resolved, or
// os.TempDir() when that fails.
func resolvedTempDir() string {
	if p, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		return p
	}
	return os.TempDir()
}

// TestSandboxExecContainerSocket_RenameBypassWorksWithSharedTmpWrite is the
// paired negative: with a write grant on /private/tmp, /tmp, and the user
// $TMPDIR added, each move succeeds and the connect to the moved socket
// succeeds. This proves that the missing write grant is what stops the
// bypass.
func TestSandboxExecContainerSocket_RenameBypassWorksWithSharedTmpWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	cases := renameBypassCases(t)

	m := newProfileManager(t)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return p + "\n" + sharedTmpWriteGrant()
	})
	tmpDir := sessionTmpDirOf(t, m)

	for _, c := range cases {
		out, runErr := runProbe(t, mutatedPath, probe, c.probeArgs(tmpDir)...)
		if runErr != nil || !strings.Contains(out, connectedMarker) {
			t.Errorf("%s: the move+connect still failed with a shared-tmp write grant, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s", c.name, runErr, out, mutatedPath)
		}
	}
}
