package container

// rlimit.go — per-process RLIMIT_NOFILE caps for agent exec paths (issue #2190).
//
// Layer 1 of the FD-isolation work (#2181): every agent process spawned under
// bwrap or sandbox-exec is launched with a bounded RLIMIT_NOFILE so a single
// misbehaving agent cannot exhaust the host's FD pool no matter what commands
// it runs. The hard cap is kernel-enforced — an unprivileged agent cannot
// raise it from inside the sandbox (`ulimit -n <higher>` / setrlimit above
// the hard limit fails with EPERM).
//
// The limits are applied to the *parent* (prism agent-run) immediately before
// cmd.Start() and restored immediately after, because Darwin's
// syscall.SysProcAttr has no Rlimits field — inheritance across fork/exec is
// the only portable way to set a child's rlimits. The restore is best-effort:
// an unprivileged process cannot raise its own hard limit back up, so the
// fallback restores only the soft limit under the new (lower) hard cap. That
// leaves the supervising parent itself capped, which is harmless — its own FD
// bookkeeping (PTY, stderr pipe, log file) is a handful of descriptors — and
// is arguably extra defence-in-depth.
//
// The host-mode agent path is deliberately NOT capped (out of scope per
// #2181's reassessment): host-mode agents inherit the host's RLIMIT_NOFILE.

import (
	"syscall"

	"github.com/prismatic-koi/prism/internal/config"
)

// AgentRlimitNofile is the resolved RLIMIT_NOFILE pair for an agent exec,
// after the #2190 clamping rules have been applied by
// ResolveAgentRlimitNofile.
type AgentRlimitNofile struct {
	// Soft and Hard are the resolved limits to apply.
	Soft uint64
	Hard uint64
	// HardClamped reports that the configured hard limit exceeded the host's
	// hard limit and was clamped down to it. Callers must surface a warning
	// to the agent's log file when set (#2190 edge-case AC).
	HardClamped bool
	// HostHard is the host hard limit observed at resolve time; used for the
	// HardClamped warning message.
	HostHard uint64
}

// ResolveAgentRlimitNofile applies the #2190 clamping rules to the configured
// (soft, hard) pair against the host's hard limit:
//
//  1. Non-positive configured values fall back to the compiled-in defaults
//     (config.DefaultAgentMaxOpenFilesSoft / DefaultAgentMaxOpenFilesHard).
//     The config loader's defaults make this unreachable for an absent key,
//     but a hand-edited config.json must not produce a zero cap that would
//     make every open() in the sandbox fail.
//  2. Configured hard > host hard → clamp hard to the host hard limit and
//     set HardClamped (the caller warns to the agent log).
//  3. Configured soft > resolved hard → clamp soft down to hard (silent
//     normalisation per the #2190 edge-case AC).
//
// Pure function — separated from ApplyAgentRlimitNofile so the clamping
// rules are unit-testable on any platform without touching process state.
func ResolveAgentRlimitNofile(cfgSoft, cfgHard int, hostHard uint64) AgentRlimitNofile {
	if cfgSoft <= 0 {
		cfgSoft = config.DefaultAgentMaxOpenFilesSoft
	}
	if cfgHard <= 0 {
		cfgHard = config.DefaultAgentMaxOpenFilesHard
	}
	res := AgentRlimitNofile{
		Soft:     uint64(cfgSoft),
		Hard:     uint64(cfgHard),
		HostHard: hostHard,
	}
	if res.Hard > hostHard {
		res.Hard = hostHard
		res.HardClamped = true
	}
	if res.Soft > res.Hard {
		res.Soft = res.Hard
	}
	return res
}

// ApplyAgentRlimitNofile resolves the configured RLIMIT_NOFILE caps against
// the host's current limits, applies them to the current process so the
// about-to-be-spawned sandbox child inherits them at fork time, and returns
// a best-effort restore func that the caller must invoke once cmd.Start()
// has returned (on both the success and error paths).
//
// warnf is called (printf-style) for every non-fatal anomaly: clamping the
// configured hard limit to the host hard limit, and Getrlimit/Setrlimit
// failures. Failures never abort the agent spawn — the worst case is the
// child inheriting the host's limits, which is the pre-#2190 status quo.
//
// Calling syscall.Setrlimit here also disables the Go runtime's
// restore-original-RLIMIT_NOFILE-on-exec behaviour (Go ≥ 1.19 raises the
// soft limit to the hard limit at startup and silently undoes that for
// exec'd children), which is exactly what we need: the child must inherit
// the resolved (soft, hard) pair verbatim.
func ApplyAgentRlimitNofile(cfgSoft, cfgHard int, warnf func(format string, args ...any)) (restore func()) {
	noop := func() {}

	var prev syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &prev); err != nil {
		warnf("cannot read host RLIMIT_NOFILE: %v — agent will inherit host limits", err)
		return noop
	}

	res := ResolveAgentRlimitNofile(cfgSoft, cfgHard, prev.Max)
	if res.HardClamped {
		warnf("configured agent_max_open_files_hard %d exceeds host hard limit %d — clamping to host hard limit",
			cfgHard, res.HostHard)
	}

	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: res.Soft, Max: res.Hard}); err != nil {
		warnf("cannot set RLIMIT_NOFILE to (soft %d, hard %d): %v — agent will inherit host limits",
			res.Soft, res.Hard, err)
		return noop
	}

	return func() {
		// Attempt a full restore first. This fails with EPERM whenever
		// prev.Max > res.Hard (an unprivileged process cannot raise its own
		// hard limit), in which case fall back to restoring the soft limit
		// under the now-lowered hard cap so the parent's soft headroom is
		// as close to its original value as the kernel allows.
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &prev); err == nil {
			return
		}
		softOnly := syscall.Rlimit{Cur: prev.Cur, Max: res.Hard}
		if softOnly.Cur > softOnly.Max {
			softOnly.Cur = softOnly.Max
		}
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &softOnly)
	}
}
