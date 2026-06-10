package integration_test

// rlimit_bwrap_test.go — Layer 1 FD isolation (#2190): integration coverage
// for the bwrap exec path's RLIMIT_NOFILE cap.
//
// The production path (cmd/agent_run.go runAgentRunBwrapHandler) calls
// container.ApplyAgentRlimitNofile immediately before bwrapCmd.Start() and
// restores immediately after; the spawned sandbox inherits the resolved
// (soft, hard) pair at fork time. These tests exercise the same helper
// against a real bwrap spawn and observe the limits from *inside* the
// sandbox via the shell's ulimit builtin (equivalent to reading
// /proc/self/limits — both report getrlimit(RLIMIT_NOFILE) for the sandboxed
// process).
//
// No-op-proofing (the negative-test discipline from
// docs/sandbox-exec-testing.md, applied to the rlimit mechanism): the
// enforcement under test is rlimit inheritance, not a sandbox profile rule,
// so the "mutation" is applying a *different* configured pair and asserting
// the observed limits track it. Two runs with different configured values
// must observe different limits — a vacuous observer (e.g. one that reported
// the host limits regardless) would fail that cross-check. The security AC
// (`ulimit -n <above hard>` fails with EPERM inside the sandbox) is covered
// by its own subtest.
//
// Linux-only in practice: requireBwrap skips when bwrap is unavailable
// (always on Darwin) and on GitHub Actions runners where unprivileged userns
// is blocked (#1510). requireUsableBwrap additionally probes an actual spawn
// so the test skips rather than fails inside environments (e.g. the Nix
// build sandbox) where bwrap is present but cannot create namespaces.

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// requireUsableBwrap extends requireBwrap (stdio_test.go) with a live spawn
// probe. bwrap can be present on PATH but unable to create user namespaces
// (Nix build sandbox, locked-down kernels); in that case every test here
// would fail for environmental reasons unrelated to the rlimit cap, so skip.
func requireUsableBwrap(t *testing.T) string {
	t.Helper()
	bin := requireBwrap(t)
	probe := exec.Command(bin, bwrapSandboxArgs("true")...)
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("bwrap present but not usable in this environment: %v — %s", err, out)
	}
	return bin
}

// bwrapSandboxArgs returns a minimal bwrap argv that binds the host root
// read-only (so /bin/sh and its libraries resolve) and runs script under
// /bin/sh. The production sandbox argv is far richer (see
// internal/container/bwrap.go) but rlimit inheritance is a property of
// fork/exec, not of the mount/namespace layout — a minimal sandbox observes
// the same limits the production one does.
func bwrapSandboxArgs(script string) []string {
	return []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-pid",
		"/bin/sh", "-c", script,
	}
}

// bwrapObservedNofile spawns /bin/sh under bwrap and reports the
// RLIMIT_NOFILE (soft, hard) pair observed inside the sandbox.
func bwrapObservedNofile(t *testing.T, bwrapBin string) (soft, hard uint64) {
	t.Helper()
	cmd := exec.Command(bwrapBin, bwrapSandboxArgs(`echo "$(ulimit -Sn) $(ulimit -Hn)"`)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bwrap ulimit probe: %v — %s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("bwrap ulimit probe: want two fields, got %q", out)
	}
	soft, sErr := strconv.ParseUint(fields[0], 10, 64)
	hard, hErr := strconv.ParseUint(fields[1], 10, 64)
	if sErr != nil || hErr != nil {
		t.Fatalf("bwrap ulimit probe: cannot parse %q (soft err %v, hard err %v)", out, sErr, hErr)
	}
	return soft, hard
}

// TestBwrapAgentRlimitNofile covers the #2190 bwrap-path ACs:
//
//   - the spawned sandbox's RLIMIT_NOFILE matches the resolved (soft, hard)
//     config values;
//   - the observation tracks the configured values (no-op proof);
//   - the sandbox cannot raise its limit above the configured hard cap.
//
// Subtests run sequentially and use descending hard values: an unprivileged
// process cannot raise its hard limit back after the best-effort restore, so
// each subsequent apply must not require raising it.
func TestBwrapAgentRlimitNofile(t *testing.T) {
	bwrapBin := requireUsableBwrap(t)

	var hostLim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &hostLim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}

	// applyAndObserve mirrors the production call shape: apply → spawn →
	// restore, returning what the sandbox observed.
	applyAndObserve := func(t *testing.T, cfgSoft, cfgHard int) (uint64, uint64) {
		t.Helper()
		restore := container.ApplyAgentRlimitNofile(cfgSoft, cfgHard, func(format string, args ...any) {
			t.Logf("rlimit warning: "+format, args...)
		})
		defer restore()
		return bwrapObservedNofile(t, bwrapBin)
	}

	const (
		firstSoft, firstHard   = 4096, 8192
		secondSoft, secondHard = 2048, 4096
	)

	var firstObservedSoft, firstObservedHard uint64

	t.Run("configured limits visible inside sandbox", func(t *testing.T) {
		want := container.ResolveAgentRlimitNofile(firstSoft, firstHard, hostLim.Max)
		gotSoft, gotHard := applyAndObserve(t, firstSoft, firstHard)
		if gotSoft != want.Soft || gotHard != want.Hard {
			t.Errorf("inside bwrap: got (soft %d, hard %d), want (soft %d, hard %d)",
				gotSoft, gotHard, want.Soft, want.Hard)
		}
		firstObservedSoft, firstObservedHard = gotSoft, gotHard
	})

	t.Run("different configured limits are tracked (no-op proof)", func(t *testing.T) {
		want := container.ResolveAgentRlimitNofile(secondSoft, secondHard, hostLim.Max)
		gotSoft, gotHard := applyAndObserve(t, secondSoft, secondHard)
		if gotSoft != want.Soft || gotHard != want.Hard {
			t.Errorf("inside bwrap: got (soft %d, hard %d), want (soft %d, hard %d)",
				gotSoft, gotHard, want.Soft, want.Hard)
		}
		if gotSoft == firstObservedSoft && gotHard == firstObservedHard {
			t.Errorf("observed limits (%d, %d) identical across different configured pairs — the observation is a no-op",
				gotSoft, gotHard)
		}
	})

	t.Run("ulimit cannot raise above the hard cap", func(t *testing.T) {
		restore := container.ApplyAgentRlimitNofile(secondSoft, secondHard, func(format string, args ...any) {
			t.Logf("rlimit warning: "+format, args...)
		})
		defer restore()

		raiseTo := container.ResolveAgentRlimitNofile(secondSoft, secondHard, hostLim.Max).Hard + 1000
		cmd := exec.Command(bwrapBin, bwrapSandboxArgs(fmt.Sprintf("ulimit -n %d", raiseTo))...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("ulimit -n %d succeeded inside the sandbox — the hard cap is not kernel-enforced: %s", raiseTo, out)
		}
	})
}
