//go:build linux

package iris_test

// bwrap_test_linux_test.go — bwrap skip guard for tests that exercise the
// Linux bwrap file-tool sandbox.
//
// Tests that call into the live bwrap path (TestReadTool, TestEditToolUniqueness)
// must call requireBwrap(t) at the start.  This skips the test when:
//
//   - bwrap is not installed on the host
//   - The kernel does not permit unprivileged user namespaces (common on
//     GitHub Actions ubuntu runners and inside the Nix build sandbox)
//
// The probe is a minimal bwrap invocation that mounts /bin and /nix (needed
// to exec /bin/sh from the Nix store) and runs a no-op shell:
//
//	bwrap --ro-bind /bin /bin --ro-bind /nix /nix \
//	      --ro-bind /dev/null /dev/null -- /bin/sh -c "exit 0"
//
// If it exits non-zero or errors (permission denied, executable not found),
// the test is skipped with a clear message.  This mirrors the Darwin
// requireSandboxExec convention in internal/integration/sandbox_exec_helpers_darwin_test.go.

import (
	"os/exec"
	"testing"
)

// requireBwrap skips t when bwrap cannot execute unprivileged user namespaces.
// Call this at the start of any test that exercises runInFileSandbox on Linux.
func requireBwrap(t *testing.T) {
	t.Helper()

	// Check for bwrap in PATH first.
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skipf("bwrap not found in PATH — skipping sandbox test: %v", err)
	}

	// Probe: attempt a minimal bwrap invocation that requires user-namespace
	// support.  We bind /bin (for /bin/sh) and /nix (for the Nix store path
	// that /bin/sh resolves to on NixOS) so the inner command can actually
	// exec.  /dev/null is bound as a write target for the shell's stdout.
	// If the kernel blocks unprivileged user namespaces (as on GitHub Actions
	// or inside the Nix build sandbox), bwrap exits non-zero with
	// "setting up uid map: Permission denied" or similar.
	cmd := exec.Command(bwrapPath,
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/nix", "/nix",
		"--ro-bind", "/dev/null", "/dev/null",
		"--",
		"/bin/sh", "-c", "exit 0",
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("bwrap probe failed (%v) — kernel likely does not permit unprivileged user namespaces; skipping sandbox test", err)
	}
}
