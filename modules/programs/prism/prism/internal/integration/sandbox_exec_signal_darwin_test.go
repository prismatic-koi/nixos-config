//go:build darwin

package integration_test

// sandbox_exec_signal_darwin_test.go — deterministic integration coverage
// for the signal (target self) (target children) allow in the production
// SBPL profile (issues #2021, #2249).
//
// History: the (target children) widening was added in #2021 so the
// playwright-cli node launcher could clean up its chromium child, and was
// originally covered by a playwright-based negative asserting a
// `kill EPERM` launcher warning. That negative's premise broke with the
// #2249 iokit-open-service fix: pre-fix, the SEGVd browser forced the
// launcher down the force-kill path; post-fix the browser runs healthily
// and `playwright-cli close` tears the session down gracefully over CDP —
// no signal is ever sent, so the EPERM fingerprint cannot manifest without
// contriving a SEGV (and the 2026-06-12 host capture suggests a contrived
// SEGV would not isolate the rule either: kill EPERM was observed on the
// SEGV path even WITH (target children) present — the daemon → launcher →
// chromium grandchild depth observation in #2249).
//
// This file replaces it with a direct probe of the rule's semantics using
// bash: a parent process spawning a child and signalling it. This is
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
// Per docs/sandbox-exec-testing.md: bash is the right probe binary here —
// the strip negative leaves the launch CWD ungranted-tolerant (bash only
// warns on an unresolvable CWD), and requireNixBash skips when bash is not
// a Nix store binary.

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

	// Regression guard at the string level first: a generator change that
	// drops the widening should fail here with a precise message before
	// the behavioural probe below.
	if !strings.Contains(prepared.content, "(allow signal (target self) (target children))") {
		t.Fatalf("generated profile is missing the signal (target self) (target children) widening (issues #2021, #2249).\nProfile:\n%s", prepared.content)
	}

	profilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", signalChildScript)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("signalling a direct child failed under the production profile.\n"+
			"The signal (target children) allow should permit kill(2) on a\n"+
			"child process (issues #2021, #2249).\nExit: %v\nOutput:\n%s", runErr, out)
	}
	if strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("child-kill output contains an EPERM under the production profile.\nOutput:\n%s", out)
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

	// Replace the widened signal clause with self-only. The mutation must
	// produce a syntactically valid SBPL profile so the sandbox still
	// loads — the failure under test is the runtime signal denial, not a
	// profile parse error. withMutatedProfile fatals if the substitution
	// matches nothing, so a generator reshape cannot silently turn this
	// negative into a no-op.
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			"(allow signal (target self) (target children))",
			"(allow signal (target self))")
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath, nixBash, "-c", signalChildScript)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("signalling a direct child succeeded WITHOUT (target children).\n"+
			"The negative test is not catching the signal-widening regression\n"+
			"(issues #2021, #2249).\nMutated profile: %s\nOutput:\n%s", mutatedPath, out)
		return
	}
	if !strings.Contains(string(out), "Operation not permitted") {
		t.Errorf("child-kill failed under the mutated profile, but not with the\n"+
			"expected EPERM fingerprint — the failure mode does not isolate the\n"+
			"signal rule.\nExit: %v\nProfile: %s\nOutput:\n%s", runErr, mutatedPath, out)
	} else {
		t.Logf("ka pai — child-kill correctly denied EPERM without (target children) (exit: %v)", runErr)
	}
}
