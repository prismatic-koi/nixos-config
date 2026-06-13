//go:build darwin

package integration_test

// sandbox_exec_rlimit_darwin_test.go — Layer 1 FD isolation (#2190):
// integration coverage for the sandbox-exec exec path's RLIMIT_NOFILE cap.
//
// The production path (cmd/agent_run_sandbox_exec_darwin.go
// runAgentRunSandboxExec) calls container.ApplyAgentRlimitNofile immediately
// before sandboxCmd.Start() and restores immediately after; the spawned
// sandbox inherits the resolved (soft, hard) pair at fork time. These tests
// exercise the same helper against a real /usr/bin/sandbox-exec spawn under
// the production-generated SBPL profile (per docs/sandbox-exec-testing.md:
// profile from Manager.PrepareSandboxExec, Nix-built bash as the test
// binary) and observe the limits from *inside* the sandbox via bash's
// ulimit builtin (getrlimit(RLIMIT_NOFILE) for the sandboxed process).
//
// Negative-test discipline (docs/sandbox-exec-testing.md requires every
// positive test to prove it is not green by accident): the enforcement under
// test here is rlimit inheritance across fork/exec, not an SBPL profile rule
// — RLIMIT_NOFILE is invisible to the sandbox profile, so withMutatedProfile
// cannot exercise it. The no-op proof is instead applying a *different*
// configured pair and asserting the observed limits track it: two runs with
// different configured values must observe different limits, which a vacuous
// observer (e.g. one reporting host limits regardless) would fail. The
// security AC (`ulimit -n <above hard>` fails inside the sandbox) has its
// own subtest.
//
// This file does not modify generateProfile / PrepareSandboxExec —
// the profile is used as generated (plus the
// standard test-harness extras from augmentProfileForTest that let a
// Nix-built binary start under the sandbox).

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// sandboxExecObservedNofile runs Nix bash under sandbox-exec with the given
// profile and reports the RLIMIT_NOFILE (soft, hard) pair observed inside
// the sandbox.
//
// launchDir is the bash CWD inside the sandbox — it MUST be a path the
// profile grants (typically the session work dir). Without an explicit
// granted CWD, bash inherits the go-test binary's CWD (the integration
// package directory, ungranted) and emits a `shell-init: error retrieving
// current directory` line to stderr before the actual ulimit output,
// breaking the two-field parse. See docs/sandbox-exec-testing.md § Launch
// CWD for the full rationale.
func sandboxExecObservedNofile(t *testing.T, profilePath, nixBash, launchDir string) (soft, hard uint64) {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", `echo "$(ulimit -Sn) $(ulimit -Hn)"`)
	cmd.Dir = launchDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox-exec ulimit probe: %v — %s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("sandbox-exec ulimit probe: want two fields, got %q", out)
	}
	soft, sErr := strconv.ParseUint(fields[0], 10, 64)
	hard, hErr := strconv.ParseUint(fields[1], 10, 64)
	if sErr != nil || hErr != nil {
		t.Fatalf("sandbox-exec ulimit probe: cannot parse %q (soft err %v, hard err %v)", out, sErr, hErr)
	}
	return soft, hard
}

// TestSandboxExecAgentRlimitNofile covers the #2190 sandbox-exec-path ACs:
//
//   - the spawned sandbox's RLIMIT_NOFILE matches the resolved (soft, hard)
//     config values;
//   - the observation tracks the configured values (no-op proof);
//   - the sandbox cannot raise its limit above the configured hard cap
//     (`ulimit -n <higher>` fails).
//
// Subtests run sequentially and use descending hard values: an unprivileged
// process cannot raise its hard limit back after the best-effort restore, so
// each subsequent apply must not require raising it.
func TestSandboxExecAgentRlimitNofile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	// Launch the bash probe with cwd = session work dir so getcwd resolves
	// to a profile-granted path. The work dir is created by
	// PrepareSandboxExec (called inside preparePositiveProfile) and is
	// covered by the (subpath <sessionDir>) RW allow in the profile.
	sessionDir, sessionDirErr := m.SessionWorkDir()
	if sessionDirErr != nil || sessionDir == "" {
		t.Fatalf("SessionWorkDir: %v", sessionDirErr)
	}

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
		return sandboxExecObservedNofile(t, profilePath, nixBash, sessionDir)
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
			t.Errorf("inside sandbox-exec: got (soft %d, hard %d), want (soft %d, hard %d)",
				gotSoft, gotHard, want.Soft, want.Hard)
		}
		firstObservedSoft, firstObservedHard = gotSoft, gotHard
	})

	t.Run("different configured limits are tracked (no-op proof)", func(t *testing.T) {
		want := container.ResolveAgentRlimitNofile(secondSoft, secondHard, hostLim.Max)
		gotSoft, gotHard := applyAndObserve(t, secondSoft, secondHard)
		if gotSoft != want.Soft || gotHard != want.Hard {
			t.Errorf("inside sandbox-exec: got (soft %d, hard %d), want (soft %d, hard %d)",
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
		cmd := exec.Command(sandboxExecPath, "-f", profilePath,
			nixBash, "-c", fmt.Sprintf("ulimit -n %d", raiseTo))
		cmd.Dir = sessionDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("ulimit -n %d succeeded inside the sandbox — the hard cap is not kernel-enforced: %s", raiseTo, out)
		}
	})
}
