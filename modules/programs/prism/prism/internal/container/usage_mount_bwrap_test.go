//go:build linux

package container

// usage_mount_bwrap_test.go — Linux-only integration coverage for the
// read-only bind of the prism usage snapshot directory (issue #2572).
//
// The sibling unit tests in usage_mount_test.go assert the emitted argv.
// Argv assertions are necessary but not sufficient — the same lesson the
// sandbox-exec convention records in docs/sandbox-exec-testing.md, where a
// profile that could not launch any process kept every substring test green.
// These tests spawn a REAL bwrap sandbox through the production emitter
// (StandardSandboxMounts + AppendBwrapBind) and observe the behaviour the
// acceptance criteria actually name:
//
//   - a sandboxed process can read current.json through the mount;
//   - the mount is read-only: a write attempt fails and the host file is
//     unchanged;
//   - a snapshot written on the host AFTER the sandbox started is visible to
//     it without a restart;
//   - without the bind the same read fails — the no-op proof that the
//     positive tests are not green by accident.
//
// The sandbox is deliberately minimal and does NOT bind the host root: the
// usage bind is the only route to the directory, so a passing positive test
// can only be the rule under test.
//
// Skips (never fails) when bwrap is absent or cannot create user namespaces
// — GitHub Actions ubuntu runners (#1510) and the nix build sandbox both
// land there.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireUsableBwrapForUsageTest resolves bwrap and probes a live spawn.
// bwrap can be on PATH but unable to unshare a user namespace (GitHub
// Actions, the nix build sandbox, locked-down kernels); in that case these
// tests would fail for environmental reasons unrelated to the mount.
func requireUsableBwrapForUsageTest(t *testing.T) string {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Skip("skipping on GitHub Actions: unprivileged userns uid-map setup is disallowed — see #1510")
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not found in PATH")
	}
	probe := exec.Command(bin, "--ro-bind", "/nix", "/nix", "--ro-bind", "/bin", "/bin",
		"--proc", "/proc", "--dev", "/dev", "--unshare-pid", "/bin/sh", "-c", "true")
	if out, probeErr := probe.CombinedOutput(); probeErr != nil {
		t.Skipf("bwrap present but not usable in this environment: %v — %s", probeErr, out)
	}
	return bin
}

// nixCoreutilsBinDir returns the /nix/store bin directory holding coreutils,
// so the in-sandbox PATH can reach cat/sleep with only /nix bound. Skips
// when the host is not a Nix store deployment.
func nixCoreutilsBinDir(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat not found in PATH")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", p, err)
	}
	if !strings.HasPrefix(real, "/nix/store/") {
		t.Skipf("cat resolves to %q, not a /nix/store path — skipping", real)
	}
	return filepath.Dir(real)
}

// usageMountArgsForTest returns the bwrap bind triple the PRODUCTION emitter
// generates for the usage directory, and nothing else. Building it through
// StandardSandboxMounts + AppendBwrapBind (rather than hand-writing
// "--ro-bind src dst") is what makes these tests cover the shipped code.
//
// Fails the test when no usage entry is emitted — a silent empty slice would
// turn every positive test below into a false negative.
func usageMountArgsForTest(t *testing.T, hostHome string) []string {
	t.Helper()
	var args []string
	suffix := filepath.Join("prism", "usage")
	for _, spec := range StandardSandboxMounts(Config{}, hostHome, hostHome, isolationBwrap) {
		if strings.HasSuffix(spec.SandboxPath, suffix) {
			args = AppendBwrapBind(args, spec)
		}
	}
	if len(args) == 0 {
		t.Fatal("StandardSandboxMounts emitted no usage-dir bind — the mount under test is missing")
	}
	return args
}

// runInBwrap runs script under a minimal bwrap sandbox with the supplied
// extra mount arguments and returns the combined output and the run error.
func runInBwrap(t *testing.T, bwrapBin string, mountArgs []string, script string) (string, error) {
	t.Helper()
	cmd := exec.Command(bwrapBin, bwrapUsageTestArgs(t, mountArgs, script)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bwrapUsageTestArgs builds the minimal argv: /nix and /bin read-only (so
// /bin/sh and coreutils resolve), a private /proc and /dev, and the supplied
// mounts. The host root is NOT bound — the only route to the usage directory
// is the mount under test.
func bwrapUsageTestArgs(t *testing.T, mountArgs []string, script string) []string {
	t.Helper()
	args := []string{
		"--ro-bind", "/nix", "/nix",
		"--ro-bind", "/bin", "/bin",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid",
		"--die-with-parent",
		"--setenv", "PATH", nixCoreutilsBinDir(t),
	}
	args = append(args, mountArgs...)
	return append(args, "/bin/sh", "-c", script)
}

// usageFixture creates a fake host home with a populated usage directory and
// returns (hostHome, usageDir, snapshotPath). $XDG_STATE_HOME is cleared so
// the resolution takes the home-relative branch.
func usageFixture(t *testing.T) (hostHome, usageDir, snapshotPath string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", "")
	hostHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	usageDir = filepath.Join(hostHome, ".local", "state", "prism", "usage")
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll usage dir: %v", err)
	}
	snapshotPath = filepath.Join(usageDir, "current.json")
	return hostHome, usageDir, snapshotPath
}

// TestBwrapUsageStateDir_ReadableInsideSandbox is the functional AC: a
// sandboxed session can read $XDG_STATE_HOME/prism/usage/current.json. This
// is the read the bottom-bar usage segment performs
// (pi/extensions/prism.ts::readUsageSnapshot) and the read that returned
// nothing in every sandboxed session before this change.
func TestBwrapUsageStateDir_ReadableInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostHome, _, snapshotPath := usageFixture(t)

	const payload = `{"captured_at":"2026-08-03T00:00:00Z","account":"work"}`
	if err := os.WriteFile(snapshotPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}

	out, err := runInBwrap(t, bwrapBin, usageMountArgsForTest(t, hostHome),
		"cat "+shQuoteForTest(snapshotPath))
	if err != nil {
		t.Fatalf("reading current.json inside the sandbox failed: %v — %s", err, out)
	}
	if !strings.Contains(out, `"account":"work"`) {
		t.Errorf("current.json content not visible inside the sandbox.\nGot: %q\nWant it to contain: %q",
			out, `"account":"work"`)
	}
}

// TestBwrapUsageStateDir_UnreadableWithoutBind is the paired no-op proof:
// with the usage bind removed, the very same read must fail. Without this,
// a positive test could be green because some other mount happened to expose
// the path.
func TestBwrapUsageStateDir_UnreadableWithoutBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	_, _, snapshotPath := usageFixture(t)

	if err := os.WriteFile(snapshotPath, []byte(`{"account":"work"}`), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}

	// No mount args at all — the deliberate mutation.
	out, err := runInBwrap(t, bwrapBin, nil, "cat "+shQuoteForTest(snapshotPath))
	if err == nil {
		t.Errorf("read of %s succeeded WITHOUT the usage bind — some other mount exposes the path,\n"+
			"so the positive test is not isolating the mount under test.\nOutput: %s", snapshotPath, out)
	}
	if strings.Contains(out, `"account":"work"`) {
		t.Errorf("snapshot content leaked into a sandbox with no usage bind: %s", out)
	}
}

// TestBwrapUsageStateDir_WriteDeniedInsideSandbox is the functional AC that
// the mount is read-only, and the security consequence: a compromised
// session must not be able to forge usage figures on the host. Every
// legitimate writer goes through the sidecar endpoint POST /usage/snapshot
// (issue #2538), so nothing in-sandbox needs write access.
func TestBwrapUsageStateDir_WriteDeniedInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostHome, usageDir, snapshotPath := usageFixture(t)

	const payload = `{"captured_at":"2026-08-03T00:00:00Z","account":"work"}`
	if err := os.WriteFile(snapshotPath, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile current.json: %v", err)
	}
	mountArgs := usageMountArgsForTest(t, hostHome)

	// Overwrite an existing file.
	out, err := runInBwrap(t, bwrapBin, mountArgs,
		"echo forged > "+shQuoteForTest(snapshotPath))
	if err == nil {
		t.Errorf("overwriting current.json succeeded inside the sandbox — the mount is not read-only.\nOutput: %s", out)
	}

	// Create a new file in the directory.
	forged := filepath.Join(usageDir, "forged.json")
	out, err = runInBwrap(t, bwrapBin, mountArgs,
		"echo forged > "+shQuoteForTest(forged))
	if err == nil {
		t.Errorf("creating %s succeeded inside the sandbox — the mount is not read-only.\nOutput: %s", forged, out)
	}
	if _, statErr := os.Stat(forged); statErr == nil {
		t.Errorf("%s exists on the host — the sandbox wrote through a read-only mount", forged)
	}

	// The host snapshot must be byte-identical.
	got, readErr := os.ReadFile(snapshotPath)
	if readErr != nil {
		t.Fatalf("ReadFile current.json after the write probes: %v", readErr)
	}
	if string(got) != payload {
		t.Errorf("current.json changed across the write probes: got %q, want %q", string(got), payload)
	}
}

// TestBwrapUsageStateDir_LateSnapshotVisibleWithoutRestart is the edge-case
// AC: a snapshot written on the host after the session started becomes
// visible to that session without a restart.
//
// This is why the bind is the DIRECTORY and not current.json itself. A
// file-level bind pins the original inode, and the writer replaces the file
// by atomic rename, so a session would be stuck with whatever existed at
// spawn time — or with nothing at all.
func TestBwrapUsageStateDir_LateSnapshotVisibleWithoutRestart(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostHome, _, snapshotPath := usageFixture(t)

	// No snapshot exists yet — the directory is empty at spawn time.
	quoted := shQuoteForTest(snapshotPath)
	script := "i=0; while [ $i -lt 100 ]; do " +
		"if [ -f " + quoted + " ]; then cat " + quoted + "; exit 0; fi; " +
		"sleep 0.1; i=$((i+1)); done; echo TIMEOUT; exit 1"

	cmd := exec.Command(bwrapBin, bwrapUsageTestArgs(t, usageMountArgsForTest(t, hostHome), script)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the sandbox: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Write the snapshot host-side while the sandbox is already running.
	const payload = `{"captured_at":"2026-08-03T00:00:00Z","account":"late"}`
	time.Sleep(200 * time.Millisecond)
	tmp := snapshotPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile temp snapshot: %v", err)
	}
	// Rename, matching the writer's atomic-replace pattern — the exact
	// operation a file-level bind would hide from the sandbox.
	if err := os.Rename(tmp, snapshotPath); err != nil {
		t.Fatalf("Rename temp snapshot: %v", err)
	}

	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	for {
		n, readErr := stdout.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if readErr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the running sandbox never saw the late snapshot: %v — %s", err, string(buf))
	}
	if !strings.Contains(string(buf), `"account":"late"`) {
		t.Errorf("late snapshot content not visible to the running sandbox.\nGot: %q", string(buf))
	}
}

// shQuoteForTest single-quotes s for /bin/sh. Test paths are t.TempDir()
// derivatives with no quotes in them, but quoting keeps the scripts robust.
func shQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
