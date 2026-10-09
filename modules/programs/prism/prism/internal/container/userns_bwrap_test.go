//go:build linux

package container

// userns_bwrap_test.go — a process in the bwrap sandbox cannot create a user
// namespace, so rootless podman cannot start a container from inside it
// (#3065). The tests run a real bwrap with the production namespace flags.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireNixUnshare returns the /nix/store path of util-linux unshare. The
// test sandbox binds only /nix.
func requireNixUnshare(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("unshare not found in PATH")
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil || !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("unshare resolves to %q (%v), not a /nix/store path", resolved, err)
	}
	return resolved
}

// runUnshareInBwrap runs `unshare --user` in a bwrap sandbox that has the
// given namespace flags and a read-only /nix. The command in the new
// namespace is `unshare --version`, because the sandbox has no PATH.
func runUnshareInBwrap(t *testing.T, bwrapBin, unshareBin string, baseline []string) (string, error) {
	t.Helper()
	args := append(append([]string{}, baseline...), "--ro-bind", "/nix", "/nix", unshareBin, "--user", unshareBin, "--version")
	out, err := exec.Command(bwrapBin, args...).CombinedOutput()
	return string(out), err
}

// TestBwrapBaseline_SandboxCannotCreateUserNamespace runs the production
// namespace flags under a real bwrap: a process in the sandbox cannot
// create a user namespace, which rootless podman needs for every container.
func TestBwrapBaseline_SandboxCannotCreateUserNamespace(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	unshareBin := requireNixUnshare(t)

	out, err := runUnshareInBwrap(t, bwrapBin, unshareBin, bwrapBaselineArgs())
	if err == nil {
		t.Fatalf("unshare --user succeeded in the sandbox, so rootless podman can start a container there.\nOutput: %s", out)
	}
	if !strings.Contains(out, "unshare failed") {
		t.Fatalf("the sandbox failed for a reason other than the unshare call.\nOutput: %s", out)
	}
}

// TestBwrapBaseline_UserNamespaceAllowedWithoutDisableUserns is the paired
// negative: without --disable-userns the same unshare call succeeds, so the
// flag is what blocks it.
func TestBwrapBaseline_UserNamespaceAllowedWithoutDisableUserns(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	unshareBin := requireNixUnshare(t)

	var baseline []string
	for _, a := range bwrapBaselineArgs() {
		if a != "--disable-userns" {
			baseline = append(baseline, a)
		}
	}
	if len(baseline) == len(bwrapBaselineArgs()) {
		t.Fatalf("bwrapBaselineArgs has no --disable-userns to remove: %q", bwrapBaselineArgs())
	}
	if out, err := runUnshareInBwrap(t, bwrapBin, unshareBin, baseline); err != nil {
		t.Fatalf("without --disable-userns, unshare --user still failed, so the positive test proves nothing.\nExit: %v\nOutput: %s", err, out)
	}
}
