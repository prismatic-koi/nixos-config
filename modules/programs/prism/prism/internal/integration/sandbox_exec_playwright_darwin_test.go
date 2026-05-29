//go:build darwin

package integration_test

// sandbox_exec_playwright_darwin_test.go — integration coverage for the
// playwright-cli enablement under sandbox-exec (issue #2021).
//
// Without the changes in this PR, running `playwright-cli open <url>` inside
// a sandbox-exec session SIGSEGVs in IONotificationPortGetRunLoopSource at
// ChromeMain+~50ms — the canonical fingerprint of `iokit-open` denial.
// Subsequent secondary failures (`Operation not permitted` on the crashpad
// xattr writes, `kill EPERM` on the node-side launcher trying to clean up
// the chromium child) cascade from there.
//
// This file's positive test invokes the production SBPL profile against the
// Nix-built `playwright-cli` binary using a `data:` URL so the test does
// not depend on network egress. It asserts that:
//
//   1. playwright-cli open exits 0.
//   2. The stderr output does not contain the SEGV fingerprint
//      (`SEGV_ACCERR` or `IONotificationPortGetRunLoopSource`).
//   3. The stderr does not contain the secondary failure markers
//      `Operation not permitted` (crashpad xattr) or `kill EPERM`
//      (launcher signal denial).
//   4. The chromium user-data directory created by playwright-cli during
//      the session lives under <stagingHome>/Library/Application
//      Support/Google/Chrome for Testing/, NOT under the host
//      ~/Library/Application Support/...
//
// The two negative tests use the `withMutatedProfile` helper to:
//
//   - Remove the iokit-open-user-client allow block. The positive test
//     above must then fail with the SEGV fingerprint, proving the
//     iokit-open-user-client rule is load-bearing.
//   - Remove the `(target children)` qualifier from the signal allow.
//     The positive test above must then surface a `kill EPERM` warning
//     in the launcher stderr, proving the signal widening is
//     load-bearing.
//
// We intentionally do not exercise the headless / WindowServer-bootstrap
// rule here — that follow-up is out of scope (issue #2021, "Out of scope"
// §4.2 — headless-by-default is tracked separately).
//
// Why playwright-cli end-to-end rather than a minimal IOKit harness:
// the iokit-open-user-client class set was empirically chosen against the
// Chrome for Testing framework's exact init path. A minimal harness that
// just calls IONotificationPortCreate/IOServiceMatching would not exercise
// the IOAudioEngineUserClient / IOFramebufferSharedUserClient probes
// chromium does, and would not surface a kill EPERM signal at all (no
// node launcher → no child PID to signal). The honest end-to-end signal
// is worth the extra start-up time per the issue's testing convention
// guidance.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// playwrightCLIBinaryName is the binary the test resolves via PATH. The
// Nix-built playwright-cli wraps `Google Chrome for Testing` via a
// shell-script launcher (see pkgs/playwright-cli.nix); the wrapper sets
// PLAYWRIGHT_MCP_EXECUTABLE_PATH to a Nix store path.
const playwrightCLIBinaryName = "playwright-cli"

// requirePlaywrightCLI resolves the playwright-cli binary via PATH +
// EvalSymlinks and skips the test if it is not present on a Nix store
// path. The bash wrapper script is intentionally resolved as the entry
// point — sandbox-exec runs the wrapper, the wrapper exec's node, and
// node spawns the chromium grandchild.
func requirePlaywrightCLI(t *testing.T) string {
	t.Helper()

	binPath, err := exec.LookPath(playwrightCLIBinaryName)
	if err != nil {
		t.Skipf("%s not found in PATH: %v", playwrightCLIBinaryName, err)
	}

	resolved, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", binPath, err)
	}
	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("playwright-cli at %q does not resolve to a /nix/store path \u2014 cannot test under sandbox", resolved)
	}
	return resolved
}

// playwrightDataURL is a tiny self-contained `data:` URL the positive
// test loads. It does not rely on network egress, DNS, or any external
// HTTP endpoint, so the test signal is purely about chromium init under
// sandbox-exec rather than network policy.
const playwrightDataURL = "data:text/html;charset=utf-8,<!doctype html><title>prism-2021</title><h1>prism-2021-sentinel</h1>"

// segVFingerprint substrings: presence of any of these in the launcher
// stderr indicates the canonical iokit-open denial SEGV from the issue.
// The exact line in the crash log is `Received signal 11 SEGV_ACCERR`.
var segVFingerprint = []string{
	"SEGV_ACCERR",
	"IONotificationPortGetRunLoopSource",
	"<process did exit: exitCode=null, signal=SIGSEGV>",
}

// killEPERMFingerprint is the secondary error surfaced when the node-side
// launcher tries to clean up its chromium grandchild but is denied the
// signal by sandbox-exec. The literal stderr string from the launcher is
// `kill EPERM`.
const killEPERMFingerprint = "kill EPERM"

// crashpadEPERMFingerprint is the crashpad xattr write denial that
// surfaces when chromium's user-data directory falls under a path the
// sandbox does not allow writes to. Confirms the staging Library
// directories are working.
const crashpadEPERMFingerprint = "Operation not permitted"

// playwrightCLITimeout caps the playwright-cli invocation in case the
// chromium child hangs (e.g. partial sandbox grant). 60 s is generous
// even on a cold box; a successful run typically completes in ~3 s.
const playwrightCLITimeout = 60 * time.Second

// runPlaywrightCLIOpen invokes sandbox-exec on the given profile path
// against playwright-cli with `open <data-url>` followed by `close`.
// The env passed in must include HOME, CFFIXED_USER_HOME, PATH, and
// XDG_* set to the staging HOME values \u2014 mirroring what
// agent_run_sandbox_exec_darwin.go does in production.
func runPlaywrightCLIOpen(t *testing.T, profilePath, playwrightBin, stagingHome string) (combinedOutput string, runErr error) {
	t.Helper()

	// The Nix-built playwright-cli is a #! /nix/store/.../bash script. We
	// run it directly via sandbox-exec; the shebang line resolves to a
	// Nix store bash that is covered by the system-paths /nix allow.
	//
	// open <url> then close releases the chromium child so the test does
	// not leak a long-running daemon. If the open SIGSEGVs, the close
	// becomes a no-op against an already-dead session.
	envVars := []string{
		"HOME=" + stagingHome,
		"CFFIXED_USER_HOME=" + stagingHome,
		"PATH=" + os.Getenv("PATH"),
		"XDG_CACHE_HOME=" + filepath.Join(stagingHome, ".cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(stagingHome, ".config"),
	}
	if term := os.Getenv("TERM"); term != "" {
		envVars = append(envVars, "TERM="+term)
	}

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env", "-i")
	cmd.Args = append(cmd.Args, envVars...)
	cmd.Args = append(cmd.Args, playwrightBin, "open", playwrightDataURL)

	// Belt-and-suspenders timeout: spawn a goroutine that kills the
	// process if it overruns playwrightCLITimeout.
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(playwrightCLITimeout):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	defer close(done)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSandboxExecProfile_PlaywrightCLIOpensUnderProductionProfile is the
// positive integration test for the chromium-under-sandbox-exec fix
// (issue #2021).
//
// It generates the production SBPL profile, prepares the staging HOME
// (which now creates the chromium Library/Application Support/Google
// directories), and invokes playwright-cli with `open <data-url>` under
// sandbox-exec. Asserts exit 0 with no SEGV or kill-EPERM fingerprints
// in stderr, and that the chromium user-data directory landed under the
// staging Library (not the host Library).
func TestSandboxExecProfile_PlaywrightCLIOpensUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	playwrightBin := requirePlaywrightCLI(t)

	// Use BareRoot so the ancestor block grants HOME traversal for the
	// chromium binary at /nix/store/... (already covered) and the
	// staging-HOME Library subpath.
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Sanity-check that PrepareSandboxExecHome created the chromium
	// staging directories \u2014 the unit test
	// TestPrepareSandboxExecHome_ChromiumLibraryStagingDirs covers this
	// more thoroughly, but a failure here would explain a cascade of
	// crashpad EPERMs below.
	for _, d := range []string{
		filepath.Join(stagingHome, "Library", "Application Support", "Google"),
		filepath.Join(stagingHome, "Library", "Caches", "Google"),
	} {
		if _, statErr := os.Stat(d); statErr != nil {
			t.Fatalf("chromium staging dir %q missing after PrepareSandboxExecHome: %v", d, statErr)
		}
	}

	prepared, _ := preparePositiveProfile(t, m)

	// Load-bearing regression guard: the iokit-open-user-client block
	// must be present, and the signal widening must include
	// (target children). If either is missing the playwright-cli
	// invocation below would fail with the SEGV / kill-EPERM
	// fingerprint, but a generator regression that drops the rule
	// silently should fail this assertion first.
	if !strings.Contains(prepared.content, "(allow iokit-open-user-client") {
		t.Fatalf("generated profile is missing the iokit-open-user-client allow block (issue #2021).\nProfile:\n%s", prepared.content)
	}
	if !strings.Contains(prepared.content, "(allow signal (target self) (target children))") {
		t.Fatalf("generated profile is missing the signal (target self) (target children) widening (issue #2021).\nProfile:\n%s", prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	combined, runErr := runPlaywrightCLIOpen(t, testProfilePath, playwrightBin, stagingHome)
	if runErr != nil {
		t.Fatalf("playwright-cli open exited non-zero under production profile.\n"+
			"This is the canonical issue #2021 failure mode \u2014 chromium SIGSEGV\n"+
			"in IONotificationPortGetRunLoopSource because the iokit-open-user-client\n"+
			"allow set is incomplete, or the signal/staging-Library rules are missing.\n"+
			"Exit: %v\nProfile: %s\nOutput:\n%s",
			runErr, testProfilePath, combined)
	}

	for _, fp := range segVFingerprint {
		if strings.Contains(combined, fp) {
			t.Errorf("playwright-cli output contains SEGV fingerprint %q (issue #2021).\n"+
				"The iokit-open-user-client allow set is insufficient for the\n"+
				"chromium framework init path.\nOutput:\n%s", fp, combined)
		}
	}

	if strings.Contains(combined, killEPERMFingerprint) {
		t.Errorf("playwright-cli output contains %q (issue #2021).\n"+
			"The signal allow lacks (target children); the node launcher\n"+
			"cannot clean up its chromium grandchild.\nOutput:\n%s", killEPERMFingerprint, combined)
	}

	if strings.Contains(combined, crashpadEPERMFingerprint) {
		t.Errorf("playwright-cli output contains %q (issue #2021).\n"+
			"chromium's crashpad write fell on a host path the sandbox\n"+
			"does not permit \u2014 likely the staging Library/Application\n"+
			"Support/Google dir is not being honoured or CFFIXED_USER_HOME\n"+
			"is not redirecting NSHomeDirectory().\nOutput:\n%s", crashpadEPERMFingerprint, combined)
	}

	// The chromium user-data directory must live under the staging
	// Library, not the host Library. We assert this by checking that
	// the staging Library/Application Support/Google directory has a
	// child entry after the run (chromium creates "Chrome for Testing/"
	// underneath at startup). The negative assertion is implicit in the
	// crashpadEPERMFingerprint check above \u2014 if chromium wrote to
	// the host Library instead, the sandbox would deny the xattr write
	// and the test would fail there.
	stagingGoogleDir := filepath.Join(stagingHome, "Library", "Application Support", "Google")
	entries, readErr := os.ReadDir(stagingGoogleDir)
	if readErr != nil {
		t.Errorf("staging Library/Application Support/Google not readable after run: %v", readErr)
	} else if len(entries) == 0 {
		// playwright-cli may have used --user-data-dir=/tmp/playwright_*
		// instead of the default profile path. The data-URL flow in
		// recent versions does this, so an empty Google/ dir is not a
		// failure per se. Log for diagnostic visibility.
		t.Logf("staging Library/Application Support/Google is empty \u2014 chromium likely used --user-data-dir=/tmp/playwright_* override (informational, not a failure)")
	} else {
		t.Logf("staging Library/Application Support/Google has %d entries after run", len(entries))
	}
}

// TestSandboxExecProfile_PlaywrightCLIFailsWithoutIOKitAllow is the
// paired negative test for the iokit-open-user-client allow block. It
// strips the entire iokit-open-user-client block from the profile via
// withMutatedProfile and asserts that playwright-cli now fails with the
// canonical SEGV fingerprint.
//
// This proves the positive test is not green by accident \u2014 the
// iokit-open-user-client allow rule is specifically what unblocks the
// chromium framework init path.
func TestSandboxExecProfile_PlaywrightCLIFailsWithoutIOKitAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	playwrightBin := requirePlaywrightCLI(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	mutatedPath := withMutatedProfile(t, m, removeIOKitAllowBlock)

	combined, runErr := runPlaywrightCLIOpen(t, mutatedPath, playwrightBin, stagingHome)
	if runErr == nil {
		t.Errorf("playwright-cli open exited 0 WITHOUT the iokit-open-user-client allow block.\n"+
			"The negative test is not catching the regression \u2014 chromium should\n"+
			"have SIGSEGV'd in IONotificationPortGetRunLoopSource.\n"+
			"Mutated profile: %s\nOutput:\n%s", mutatedPath, combined)
		return
	}

	// At least one of the SEGV fingerprints must appear in the launcher
	// stderr. If chromium failed for a different reason (e.g. parse
	// error in the mutated profile, missing dyld lookup), the negative
	// test is not isolating the iokit rule.
	sawSegV := false
	for _, fp := range segVFingerprint {
		if strings.Contains(combined, fp) {
			sawSegV = true
			break
		}
	}
	if !sawSegV {
		t.Errorf("playwright-cli failed under the mutated profile, but the failure mode\n"+
			"does not match the iokit-denial SEGV fingerprint.\n"+
			"Expected one of %v in output.\nExit: %v\nProfile: %s\nOutput:\n%s",
			segVFingerprint, runErr, mutatedPath, combined)
	} else {
		t.Logf("ka pai \u2014 chromium correctly SIGSEGV'd without iokit-open-user-client allow (exit: %v)", runErr)
	}
}

// TestSandboxExecProfile_PlaywrightCLISignalEPERMWithoutTargetChildren
// is the paired negative test for the signal (target children) widening.
// It removes the (target children) qualifier from the signal allow and
// asserts that the playwright-cli launcher now surfaces a `kill EPERM`
// warning when trying to clean up its chromium grandchild.
//
// Note: this negative test does NOT necessarily produce a non-zero exit
// code from playwright-cli \u2014 the open itself may still succeed,
// and the kill EPERM is a warning during cleanup. The assertion is on
// the stderr substring, not the exit code.
func TestSandboxExecProfile_PlaywrightCLISignalEPERMWithoutTargetChildren(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	playwrightBin := requirePlaywrightCLI(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Replace the widened signal clause with self-only. The mutation must
	// produce a syntactically valid SBPL profile so the sandbox still
	// loads \u2014 we want the failure to come from the runtime signal
	// denial, not a profile parse error.
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			"(allow signal (target self) (target children))",
			"(allow signal (target self))")
	})

	combined, _ := runPlaywrightCLIOpen(t, mutatedPath, playwrightBin, stagingHome)

	if !strings.Contains(combined, killEPERMFingerprint) {
		t.Errorf("playwright-cli output does not contain %q after removing\n"+
			"(target children) from the signal allow.\n"+
			"The negative test is not catching the signal-widening regression.\n"+
			"Profile: %s\nOutput:\n%s", killEPERMFingerprint, mutatedPath, combined)
	} else {
		t.Logf("ka pai \u2014 launcher correctly surfaced %q without (target children)", killEPERMFingerprint)
	}
}

// removeIOKitAllowBlock returns a copy of the generated profile with the
// (allow iokit-open-user-client ...) clause stripped. The clause spans
// six lines in the current generator output:
//
//	(allow iokit-open-user-client
//	  (iokit-user-client-class "IOSurfaceRoot")
//	  (iokit-user-client-class "IOHIDLibUserClient")
//	  (iokit-user-client-class "IOAudioEngineUserClient")
//	  (iokit-user-client-class "IOFramebufferSharedUserClient")
//	  (iokit-user-client-class "RootDomainUserClient"))
//
// We match the verbatim header and trailing class line with the
// double-closing paren so the substitution is unambiguous and any future
// addition of a sixth class to the block would invalidate the match \u2014
// at which point the test fails loudly rather than silently mutating the
// wrong region.
func removeIOKitAllowBlock(p string) string {
	const block = `(allow iokit-open-user-client
  (iokit-user-client-class "IOSurfaceRoot")
  (iokit-user-client-class "IOHIDLibUserClient")
  (iokit-user-client-class "IOAudioEngineUserClient")
  (iokit-user-client-class "IOFramebufferSharedUserClient")
  (iokit-user-client-class "RootDomainUserClient"))
`
	return strings.Replace(p, block, "", 1)
}
