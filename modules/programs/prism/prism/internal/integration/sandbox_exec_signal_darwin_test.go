//go:build darwin

package integration_test

// sandbox_exec_signal_darwin_test.go — deterministic integration coverage
// for the signal (target self) (target children) allow in the production
// SBPL profile.
//
// The (target children) widening lets the playwright-cli node launcher clean
// up its chromium child. A playwright-based negative cannot isolate the rule:
// `playwright-cli close` tears the session down gracefully over CDP and
// sends no signal, so a `kill EPERM` fingerprint cannot manifest without
// contriving a SEGV — and even a contrived SEGV does not isolate the rule,
// because the daemon → launcher → chromium grandchild depth means kill
// EPERM appears on the SEGV path even WITH (target children) present.
//
// This file probes the rule's semantics directly using bash: a parent
// process spawning a child and signalling it. This is
// deterministic (no browser failure modes), fast (no chromium launch), and
// isolates exactly the clause under test:
//
//   - Positive: under the production profile, `bash -c 'sleep & kill $!'`
//     exits 0 — child-signalling is permitted by (target children).
//   - Negative: with the signal clause mutated to (target self) only, the
//     same operation fails with "Operation not permitted" — proving
//     (target children) is the load-bearing qualifier, not a no-op.
//
// The (target children) rule remains load-bearing well beyond playwright:
// shell job control, pi killing a timed-out tool child, and any harness
// subprocess cleanup all signal child processes. The playwright positive in
// sandbox_exec_playwright_darwin_test.go retains its killEPERMFingerprint
// absence assertion as a guard on the close-path force-kill case.
//
// Per docs/sandbox-exec-testing.md: bash is the probe binary
// (requireNixBash skips when bash is not a Nix store binary), and both
// tests launch with cmd.Dir set to the session work dir using the
// launch-dir profile variants — otherwise bash inherits the go-test
// binary's ungranted CWD and emits shell-init getcwd noise
// ("Operation not permitted") that is indistinguishable from a kill
// denial by naive substring matching. Belt and braces, the assertions
// additionally key on the kill-specific denial fingerprint (bash's kill
// builtin reports `kill: (<pid>) - Operation not permitted`), not on any
// bare EPERM substring.

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// signalChildScript spawns a background sleep child (output detached so
// the kernel pipe to the test harness closes when bash exits, not when an
// orphaned sleep does), signals it via the kill builtin (which calls
// kill(2) from the bash process targeting its direct child), and exits
// with the kill's status. Under (target self)-only the kill(2) is denied
// by seatbelt with EPERM; bash reports "Operation not permitted" and the
// script exits non-zero.
const signalChildScript = `sleep 5 >/dev/null 2>&1 & kill -TERM $!`

// killDenialFingerprint is the bash kill-builtin failure prefix — the
// full line is `kill: (<pid>) - Operation not permitted`. Both substrings
// below must co-occur for output to count as a kill denial; shell-init
// getcwd noise contains "Operation not permitted" but never "kill: (".
const killDenialFingerprint = "kill: ("

// epermText is the errno string shared by all seatbelt denials.
const epermText = "Operation not permitted"

// containsKillDenial reports whether out contains the kill-specific
// denial fingerprint (both the kill-builtin prefix and the EPERM text).
func containsKillDenial(out string) bool {
	return strings.Contains(out, killDenialFingerprint) && strings.Contains(out, epermText)
}

// TestSandboxExecProfile_SignalChildAllowed is the positive: under the
// production profile, a sandboxed process can signal its own child.
func TestSandboxExecProfile_SignalChildAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)

	prepared, _ := preparePositiveProfile(t, m)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	// Regression guard at the string level first: a generator change that
	// drops the widening should fail here with a precise message before
	// the behavioural probe below.
	if !strings.Contains(prepared.content, "(allow signal (target self) (target children))") {
		t.Fatalf("generated profile is missing the signal (target self) (target children) widening (issues #2021, #2249).\nProfile:\n%s", prepared.content)
	}

	// Launch-dir variant + cmd.Dir: keep bash's CWD resolvable so the
	// output is free of shell-init getcwd EPERM noise (see file header).
	profilePath := writeAugmentedPositiveProfileWithLaunchDir(t, prepared, sessionDir)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", signalChildScript)
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("signalling a direct child failed under the production profile.\n"+
			"The signal (target children) allow should permit kill(2) on a\n"+
			"child process (issues #2021, #2249).\nExit: %v\nOutput:\n%s", runErr, out)
	}
	if containsKillDenial(string(out)) {
		t.Errorf("child-kill output contains the kill-denial fingerprint under the production profile.\nOutput:\n%s", out)
	}
}

// TestSandboxExecProfile_SignalChildEPERMWithoutTargetChildren is the
// paired negative: with the signal clause narrowed to (target self) only,
// the same child-kill must be denied with EPERM. This proves the positive
// above is not green by accident — (target children) is specifically what
// permits the child signal.
func TestSandboxExecProfile_SignalChildEPERMWithoutTargetChildren(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	// Replace the widened signal clause with self-only. The mutation must
	// produce a syntactically valid SBPL profile so the sandbox still
	// loads — the failure under test is the runtime signal denial, not a
	// profile parse error. withMutatedProfile fatals if the substitution
	// matches nothing, so a generator reshape cannot silently turn this
	// negative into a no-op. The mutation leaves the sessionDir grant
	// intact, so the launch-dir variant keeps bash's CWD resolvable and
	// the only EPERM in the output can be the kill denial itself.
	mutatedPath := withMutatedProfileAndLaunchDir(t, m, sessionDir, func(p string) string {
		return strings.ReplaceAll(p,
			"(allow signal (target self) (target children))",
			"(allow signal (target self))")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath, nixBash, "-c", signalChildScript)
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("signalling a direct child succeeded WITHOUT (target children).\n"+
			"The negative test is not catching the signal-widening regression\n"+
			"(issues #2021, #2249).\nMutated profile: %s\nOutput:\n%s", mutatedPath, out)
		return
	}
	if !containsKillDenial(string(out)) {
		t.Errorf("child-kill failed under the mutated profile, but not with the\n"+
			"kill-specific denial fingerprint (%q + %q) — the failure mode does\n"+
			"not isolate the signal rule (a getcwd or other EPERM would be a\n"+
			"different failure).\nExit: %v\nProfile: %s\nOutput:\n%s",
			killDenialFingerprint, epermText, runErr, mutatedPath, out)
	} else {
		t.Logf("ka pai — child-kill correctly denied EPERM without (target children) (exit: %v)", runErr)
	}
}
