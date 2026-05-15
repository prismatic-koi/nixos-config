//go:build linux

package parity_test

// bwrap_guard_linux_test.go — bwrap skip guard for parity tests that
// dispatch bash via the D-5 sandbox.
//
// Mirrors internal/iris/bwrap_test_linux_test.go::requireBwrap. The parity
// suite lives in its own package (parity_test) so it cannot reuse the
// helper from internal/iris/; we replicate the probe verbatim. Any change
// to the probe in the iris package should be reflected here too.
//
// Why a guard is needed:
//
//   - The iris bash sandbox uses bwrap (unprivileged user namespaces) to
//     isolate the bash subprocess.
//   - On the plain GitHub Actions Linux runner, the kernel does not permit
//     unprivileged user namespaces — bwrap fails with
//     "setting up uid map: Permission denied".
//   - Inside the Nix build sandbox (nix-build-prism-checked CI job), bwrap
//     works fine, so the parity contract IS exercised end-to-end there.
//
// Parity tests that dispatch bash via tool_exec must call requireBwrap(t)
// at the top of the test. Tests that do NOT dispatch bash (DB-only checks,
// client-socket round-trips, etc.) do not need the guard.

import (
	"os/exec"
	"testing"
)

// requireBwrap skips t when bwrap cannot execute unprivileged user namespaces.
// Call this at the start of any parity test that exercises the iris bash
// sandbox on Linux. The implementation mirrors internal/iris/bwrap_test_linux_test.go.
func requireBwrap(t *testing.T) {
	t.Helper()

	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skipf("bwrap not found in PATH — skipping sandbox-dependent parity test: %v", err)
	}

	// Probe: minimal bwrap invocation that requires user-namespace support.
	// /bin (for /bin/sh) and /nix (for the Nix store path /bin/sh resolves
	// to on NixOS) are bound so the inner shell can exec. /dev/null is bound
	// as a write target. If the kernel blocks unprivileged user namespaces
	// (plain GitHub Actions runner), bwrap exits non-zero.
	cmd := exec.Command(bwrapPath,
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/nix", "/nix",
		"--ro-bind", "/dev/null", "/dev/null",
		"--",
		"/bin/sh", "-c", "exit 0",
	)
	if err := cmd.Run(); err != nil {
		t.Skipf("bwrap probe failed (%v) — kernel likely does not permit unprivileged user namespaces; skipping sandbox-dependent parity test", err)
	}
}
