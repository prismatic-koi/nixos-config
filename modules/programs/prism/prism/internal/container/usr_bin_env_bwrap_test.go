//go:build linux

package container

// usr_bin_env_bwrap_test.go — live bwrap coverage for the /usr/bin/env
// mounts (#3174). Each test runs a real bwrap with the mounts that
// bwrapUsrBinEnvArgs emits for BuildArgs. The sandbox binds only /nix and
// /bin, so the mounts under test are the only source of /usr.
//
// The source is a symlink into /nix/store, which is the shape of
// /usr/bin/env on NixOS. The tests do not use the host /usr/bin/env: on a
// non-NixOS CI runner it needs the host /lib64 loader, which is not bound.
//
// The tests skip when bwrap cannot start (see requireUsableBwrapForUsageTest).
// The go-tests-bwrap-live CI job runs them and fails on a skip.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nixEnvSymlinkForTest returns a symlink to the env binary in /nix/store.
func nixEnvSymlinkForTest(t *testing.T) string {
	t.Helper()
	env := filepath.Join(nixCoreutilsBinDir(t), "env")
	if _, err := os.Stat(env); err != nil {
		t.Fatalf("coreutils has no env: %v", err)
	}
	link := filepath.Join(t.TempDir(), "env")
	if err := os.Symlink(env, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	return link
}

// runInUsrBinEnvSandbox runs script with mountArgs and a PATH that holds
// coreutils and /bin, so that env can find sh.
func runInUsrBinEnvSandbox(t *testing.T, bwrapBin string, mountArgs []string, script string) (string, error) {
	t.Helper()
	pathDirs := []string{nixCoreutilsBinDir(t), "/bin"}
	return runInBwrapWithPath(t, bwrapBin, t.TempDir(), pathDirs, mountArgs, script)
}

// TestBwrapUsrBinEnv_ShebangRuns: /usr/bin/env runs a command, and a script
// with a "#!/usr/bin/env sh" shebang runs.
func TestBwrapUsrBinEnv_ShebangRuns(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	mounts := bwrapUsrBinEnvArgs(nixEnvSymlinkForTest(t))
	script := `/usr/bin/env sh -c 'echo env-ok' && ` +
		`printf '#!/usr/bin/env sh\necho shebang-ok\n' > /tmp/hello && ` +
		`chmod +x /tmp/hello && /tmp/hello`

	out, err := runInUsrBinEnvSandbox(t, bwrapBin, mounts, script)
	if err != nil {
		t.Fatalf("/usr/bin/env failed in the sandbox: %v\nOutput: %s", err, out)
	}
	for _, want := range []string{"env-ok", "shebang-ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q.\nOutput: %s", want, out)
		}
	}
}

// TestBwrapUsrBinEnv_ReadOnly: the sandbox cannot write /usr/bin/env, and
// cannot create, change, or remove anything under /usr.
func TestBwrapUsrBinEnv_ReadOnly(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	mounts := bwrapUsrBinEnvArgs(nixEnvSymlinkForTest(t))
	for _, script := range []string{
		"echo changed > /usr/bin/env",
		"rm -f /usr/bin/env",
		"touch /usr/bin/new",
		"mkdir /usr/new",
		"chmod 0777 /usr/bin",
		"chmod 0777 /usr",
	} {
		out, err := runInUsrBinEnvSandbox(t, bwrapBin, mounts, script)
		if err == nil {
			t.Errorf("%q succeeded in the sandbox.\nOutput: %s", script, out)
			continue
		}
		if !strings.Contains(out, "Read-only file system") {
			t.Errorf("%q failed for a reason other than the read-only mount: %v\nOutput: %s", script, err, out)
		}
	}
}

// TestBwrapUsrBinEnv_WritableWithoutRemount is the paired negative of
// TestBwrapUsrBinEnv_ReadOnly. A bare read-only bind of /usr/bin/env leaves
// /usr and /usr/bin writable, so the --tmpfs and --remount-ro pair is
// necessary.
func TestBwrapUsrBinEnv_WritableWithoutRemount(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	mounts := []string{"--ro-bind", nixEnvSymlinkForTest(t), "/usr/bin/env"}
	out, err := runInUsrBinEnvSandbox(t, bwrapBin, mounts, "touch /usr/bin/new && mkdir /usr/new")
	if err != nil {
		t.Fatalf("a write under /usr failed without the remount: %v\nOutput: %s", err, out)
	}
}

// TestBwrapUsrBinEnv_MissingSourceStarts: when the host has no
// /usr/bin/env, the sandbox starts with no /usr/bin/env and a read-only
// /usr.
func TestBwrapUsrBinEnv_MissingSourceStarts(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	missing := filepath.Join(t.TempDir(), "no-env")
	mounts := bwrapUsrBinEnvArgs(missing)

	out, err := runInUsrBinEnvSandbox(t, bwrapBin, mounts, "test ! -e /usr/bin/env && echo started")
	if err != nil || !strings.Contains(out, "started") {
		t.Fatalf("the sandbox did not start without a /usr/bin/env source: %v\nOutput: %s", err, out)
	}
	out, err = runInUsrBinEnvSandbox(t, bwrapBin, mounts, "mkdir /usr/bin")
	if err == nil || !strings.Contains(out, "Read-only file system") {
		t.Errorf("mkdir /usr/bin = %v, want a read-only failure.\nOutput: %s", err, out)
	}
}

// TestBwrapUsrBinEnv_MissingSourceAbortsWithoutTry is the paired negative of
// TestBwrapUsrBinEnv_MissingSourceStarts. A --ro-bind of a missing source
// stops bwrap, so --ro-bind-try is necessary.
func TestBwrapUsrBinEnv_MissingSourceAbortsWithoutTry(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	missing := filepath.Join(t.TempDir(), "no-env")
	mounts := []string{"--ro-bind", missing, "/usr/bin/env"}
	if out, err := runInUsrBinEnvSandbox(t, bwrapBin, mounts, "echo started"); err == nil {
		t.Fatalf("bwrap started with a --ro-bind of a missing source.\nOutput: %s", out)
	}
}
