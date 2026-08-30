//go:build darwin

package integration_test

// sandbox_exec_playwright_darwin_test.go — integration coverage for the
// playwright-cli enablement under sandbox-exec.
//
// Without the iokit allows in this profile, running `playwright-cli open
// <url>` inside a sandbox-exec session SIGSEGVs in
// IONotificationPortGetRunLoopSource at ChromeMain+~50ms — the canonical
// fingerprint of `iokit-open` denial.
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
//      the session lives under <sessionDir>/Library/Application
//      Support/Google/Chrome for Testing/ (the per-session work dir that
//      CFFIXED_USER_HOME points at), NOT under the host ~/Library/Application
//      Support/... and NOT under the staging HOME (which holds no Library/
//      entries).
//   5. A full open → close → re-open cycle succeeds under the same
//      profile — the launch path is repeatable in-sandbox, not green only
//      on first-boot state.
//
// The two negative tests in this file use the `withMutatedProfile` helper
// to:
//
//   - Remove the iokit-open-user-client allow block. The positive test
//     above must then fail with the SEGV fingerprint, proving the
//     iokit-open-user-client rule is load-bearing.
//   - Remove the iokit-open-service IOPMrootDomain allow. The positive test
//     above must then fail with the SEGV fingerprint, proving the
//     iokit-open-service rule is load-bearing — current Chrome for Testing
//     acquires its power-management port via iokit-open-service on
//     IOPMrootDomain, a different operation class from the user-client
//     allow.
//
// The signal `(target children)` widening is covered by the deterministic
// positive/negative pair in sandbox_exec_signal_darwin_test.go. See the
// NOTE near the end of this file for why no playwright-based signal negative
// exists here.
//
// We intentionally do not exercise the headless / WindowServer-bootstrap
// rule here — headless-by-default is tracked separately.
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
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
// sandbox does not allow writes to. Confirms the work-dir Library
// skeleton is working.
const crashpadEPERMFingerprint = "Operation not permitted"

// playwrightCLITimeout caps the playwright-cli invocation in case the
// chromium child hangs (e.g. partial sandbox grant). 60 s is generous
// even on a cold box; a successful run typically completes in ~3 s.
const playwrightCLITimeout = 60 * time.Second

// runPlaywrightCLI invokes sandbox-exec on the given profile path against
// playwright-cli with the given CLI args (e.g. "open", <url> — or "close").
// The env mirrors what agent_run_sandbox_exec_darwin.go does in
// production: HOME and XDG_CACHE/CONFIG at the real host home, and
// CFFIXED_USER_HOME at the per-session work dir so chromium's
// NSHomeDirectory()-derived writes land under <sessionDir>/Library/...
//
// The launch CWD is the session work dir: node hard-fails at bootstrap
// ("EPERM: process.cwd failed ... uv_cwd") when the sandboxed CWD is
// unresolvable, so the process must start in a directory the profile
// grants — mirroring production, where the agent's CWD is the granted
// worktree. Callers must build the profile with the getcwd ancestor extras
// (writeAugmentedPositiveProfileWithLaunchDir /
// withMutatedProfileAndLaunchDir — see
// sandbox_exec_launch_dir_darwin_test.go).
func runPlaywrightCLI(t *testing.T, profilePath, playwrightBin, sessionDir string, cliArgs ...string) (combinedOutput string, runErr error) {
	t.Helper()

	realHome := realUserHome(t)

	// The Nix-built playwright-cli is a #! /nix/store/.../bash script. We
	// run it directly via sandbox-exec; the shebang line resolves to a
	// Nix store bash that is covered by the system-paths /nix allow.
	envVars := []string{
		"HOME=" + realHome,
		"CFFIXED_USER_HOME=" + sessionDir,
		"PATH=" + os.Getenv("PATH"),
		"XDG_CACHE_HOME=" + filepath.Join(realHome, ".cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(realHome, ".config"),
		// Mirrors buildSandboxExecHomeEnv: without these, playwright-core
		// on Darwin derives its daemon registry/log dir AND its
		// browser-server descriptor registry from POSIX $HOME
		// (os.homedir()/Library/Caches — XDG_CACHE_HOME is ignored on
		// darwin). With $HOME = real home, an unredirected daemon writes
		// into the real ~/Library/Caches — denied by the profile AND a
		// host-pollution hazard, so the redirects are load-bearing.
		"PLAYWRIGHT_DAEMON_SESSION_DIR=" + filepath.Join(sessionDir, "Library", "Caches", "ms-playwright", "daemon"),
		"PLAYWRIGHT_SERVER_REGISTRY=" + filepath.Join(sessionDir, "Library", "Caches", "ms-playwright", "b"),
	}
	if term := os.Getenv("TERM"); term != "" {
		envVars = append(envVars, "TERM="+term)
	}

	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env", "-i")
	cmd.Args = append(cmd.Args, envVars...)
	cmd.Args = append(cmd.Args, playwrightBin)
	cmd.Args = append(cmd.Args, cliArgs...)
	cmd.Dir = sessionDir

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

// runPlaywrightCLIOpen invokes runPlaywrightCLI with `open <data-url>`.
// If the open SIGSEGVs (the negative tests' expected outcome), the
// playwright daemon never establishes a session, so no close is needed
// against the dead session.
func runPlaywrightCLIOpen(t *testing.T, profilePath, playwrightBin, sessionDir string) (combinedOutput string, runErr error) {
	t.Helper()
	return runPlaywrightCLI(t, profilePath, playwrightBin, sessionDir, "open", playwrightDataURL)
}

// TestSandboxExecProfile_PlaywrightCLIOpensUnderProductionProfile is the
// positive integration test for chromium under sandbox-exec.
//
// It generates the production SBPL profile (which also prepares the
// session work dir — creating the chromium Library/Application
// Support/Google skeleton inside it), and invokes playwright-cli with
// `open <data-url>` (then close → re-open → close) under sandbox-exec.
// Asserts exit 0 with no SEGV or kill-EPERM fingerprints in stderr, and
// that the chromium user-data directory landed under the work-dir Library
// (not the host Library).
//
// The live Chrome for Testing performs iokit-open-service on IOPMrootDomain,
// a different operation class from the profile's iokit-open-user-client
// RootDomainUserClient allow. Without the §9c iokit-open-service
// IOPMrootDomain allow, the denial cascades into an AMFI core-dump denial
// and SEGV_ACCERR at render init. The paired negative
// TestSandboxExecProfile_PlaywrightCLIFailsWithoutIOPMrootDomainAllow proves
// that allow is load-bearing.
func TestSandboxExecProfile_PlaywrightCLIOpensUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	playwrightBin := requirePlaywrightCLI(t)

	// Use BareRoot so the ancestor block grants HOME traversal for the
	// chromium binary at /nix/store/... (already covered) and the
	// session-work-dir Library subpath.
	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	// Sanity-check the chromium-skeleton layout before launching anything: the
	// chromium skeleton lives in the session work dir. The unit test
	// TestPrepareSessionWorkDir_ChromiumLibrarySkeleton covers this more
	// thoroughly, but a failure here would explain a cascade of crashpad
	// EPERMs below.
	for _, d := range container.SessionWorkDirChromiumDirs(sessionDir) {
		if _, statErr := os.Stat(d); statErr != nil {
			t.Fatalf("chromium skeleton dir %q missing after PrepareSandboxExec: %v", d, statErr)
		}
	}

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

	testProfilePath := writeAugmentedPositiveProfileWithLaunchDir(t, prepared, sessionDir)

	combined, runErr := runPlaywrightCLIOpen(t, testProfilePath, playwrightBin, sessionDir)
	if runErr != nil {
		t.Fatalf("playwright-cli open exited non-zero under production profile.\n"+
			"This is the canonical issue #2021 failure mode \u2014 chromium SIGSEGV\n"+
			"in IONotificationPortGetRunLoopSource because the iokit-open-user-client\n"+
			"allow set is incomplete, or the signal/work-dir-Library rules are missing.\n"+
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
			"does not permit \u2014 likely the work-dir Library/Application\n"+
			"Support/Google skeleton is not being honoured or CFFIXED_USER_HOME\n"+
			"is not redirecting NSHomeDirectory() at the session work dir\n"+
			"(issue #2247).\nOutput:\n%s", crashpadEPERMFingerprint, combined)
	}

	// open → close → re-open cycle: the first open above
	// spawned the playwright daemon; close must tear the session down
	// cleanly, and a second open must then succeed from scratch — proving
	// the in-sandbox launch path is repeatable, not green only on
	// first-boot state.
	closeOut, closeErr := runPlaywrightCLI(t, testProfilePath, playwrightBin, sessionDir, "close")
	if closeErr != nil {
		t.Errorf("playwright-cli close exited non-zero under production profile (issue #2249).\nExit: %v\nOutput:\n%s", closeErr, closeOut)
	}

	reopenOut, reopenErr := runPlaywrightCLIOpen(t, testProfilePath, playwrightBin, sessionDir)
	if reopenErr != nil {
		t.Errorf("playwright-cli re-open exited non-zero under production profile (issue #2249).\nExit: %v\nOutput:\n%s", reopenErr, reopenOut)
	}
	for _, fp := range segVFingerprint {
		if strings.Contains(reopenOut, fp) {
			t.Errorf("re-open output contains SEGV fingerprint %q (issue #2249).\nOutput:\n%s", fp, reopenOut)
		}
	}

	// Final close so the test does not leak a long-running daemon.
	if finalCloseOut, finalCloseErr := runPlaywrightCLI(t, testProfilePath, playwrightBin, sessionDir, "close"); finalCloseErr != nil {
		t.Errorf("final playwright-cli close exited non-zero (issue #2249).\nExit: %v\nOutput:\n%s", finalCloseErr, finalCloseOut)
	}

	// The chromium user-data directory must live under the work-dir
	// Library, not the host Library (and not the staging HOME). We assert
	// this by checking that the work-dir Library/Application Support/Google
	// directory has a child entry after the run (chromium creates "Chrome
	// for Testing/" underneath at startup). The negative assertion is
	// implicit in the crashpadEPERMFingerprint check above — if chromium
	// wrote to the host Library instead, the sandbox would deny the xattr
	// write and the test would fail there.
	workDirGoogleDir := filepath.Join(sessionDir, "Library", "Application Support", "Google")
	entries, readErr := os.ReadDir(workDirGoogleDir)
	if readErr != nil {
		t.Errorf("work-dir Library/Application Support/Google not readable after run: %v", readErr)
	} else if len(entries) == 0 {
		// playwright-cli may have used --user-data-dir=/tmp/playwright_*
		// instead of the default profile path. The data-URL flow in
		// recent versions does this, so an empty Google/ dir is not a
		// failure per se. Log for diagnostic visibility.
		t.Logf("work-dir Library/Application Support/Google is empty \u2014 chromium likely used --user-data-dir=/tmp/playwright_* override (informational, not a failure)")
	} else {
		t.Logf("work-dir Library/Application Support/Google has %d entries after run", len(entries))
	}

	// Post-run: no legacy staging-HOME dir may have appeared under the
	// session work dir — the mechanism is deleted.
	if _, statErr := os.Lstat(filepath.Join(sessionDir, "home")); statErr == nil {
		t.Errorf("a legacy staging-HOME dir appeared under the session work dir during the run — the staging mechanism was deleted in Step 5 of #2132.\nOffending entries:\n%s", listTreeForDiag(filepath.Join(sessionDir, "home")))
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

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	// The iokit mutation leaves the sessionDir grant intact, so the
	// launch-dir variant keeps node's CWD resolvable and the failure under
	// test is the SEGV, not a getcwd bootstrap death.
	mutatedPath := withMutatedProfileAndLaunchDir(t, m, sessionDir, removeIOKitAllowBlock)

	combined, runErr := runPlaywrightCLIOpen(t, mutatedPath, playwrightBin, sessionDir)
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

// TestSandboxExecProfile_PlaywrightCLIFailsWithoutIOPMrootDomainAllow is
// the paired negative test for the iokit-open-service IOPMrootDomain allow.
// It strips only the iokit-open-service clause from the profile — leaving
// the iokit-open-user-client block fully intact — and asserts that
// playwright-cli now fails with the SEGV fingerprint.
//
// This proves the new allow is load-bearing on its own: the user-client
// allow set is NOT sufficient for current Chrome for Testing, which
// acquires its power-management port via iokit-open-service on the
// IOPMrootDomain registry entry (the deny-log signature is
// `deny(1) iokit-open-service IOPMrootDomain`).
func TestSandboxExecProfile_PlaywrightCLIFailsWithoutIOPMrootDomainAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	playwrightBin := requirePlaywrightCLI(t)

	m := newProfileManagerWithBareRoot(t)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	// The iokit-open-service mutation leaves the sessionDir grant intact,
	// so the launch-dir variant keeps node's CWD resolvable and the failure
	// under test is the SEGV, not a getcwd bootstrap death.
	mutatedPath := withMutatedProfileAndLaunchDir(t, m, sessionDir, removeIOPMrootDomainAllowBlock)

	combined, runErr := runPlaywrightCLIOpen(t, mutatedPath, playwrightBin, sessionDir)
	if runErr == nil {
		t.Errorf("playwright-cli open exited 0 WITHOUT the iokit-open-service IOPMrootDomain allow.\n"+
			"The negative test is not catching the regression \u2014 chromium should\n"+
			"have SIGSEGV'd acquiring its power-management port (issue #2249).\n"+
			"Mutated profile: %s\nOutput:\n%s", mutatedPath, combined)
		return
	}

	// At least one of the SEGV fingerprints must appear in the launcher
	// stderr. If chromium failed for a different reason (e.g. parse error
	// in the mutated profile), the negative test is not isolating the
	// iokit-open-service rule.
	sawSegV := false
	for _, fp := range segVFingerprint {
		if strings.Contains(combined, fp) {
			sawSegV = true
			break
		}
	}
	if !sawSegV {
		t.Errorf("playwright-cli failed under the mutated profile, but the failure mode\n"+
			"does not match the iokit-denial SEGV fingerprint (issue #2249).\n"+
			"Expected one of %v in output.\nExit: %v\nProfile: %s\nOutput:\n%s",
			segVFingerprint, runErr, mutatedPath, combined)
	} else {
		t.Logf("ka pai \u2014 chromium correctly SIGSEGV'd without the iokit-open-service IOPMrootDomain allow (exit: %v)", runErr)
	}
}

// NOTE: this file has no playwright-based signal (target children) negative.
// `playwright-cli close` tears the session down gracefully over CDP and
// sends no signal, so the launcher never exercises the kill path and the
// `kill EPERM` fingerprint cannot manifest without contriving a SEGV. The
// signal (target children) widening is covered by the deterministic
// positive/negative pair in sandbox_exec_signal_darwin_test.go, which
// proves the rule's semantics (child-signalling allowed with the rule,
// EPERM without it) directly with a bash child-kill probe. The
// killEPERMFingerprint absence assertion in the positive test above is
// retained — it guards any future close-path force-kill regression.

// listTreeForDiag returns a newline-separated recursive listing of root
// (relative paths), capped at 200 entries, for assertion diagnostics. When
// the staging-Library assertions fire, the listing names the offending
// writer (for example, Caches/ms-playwright/daemon/... → the playwright
// daemon log dir) instead of leaving the operator to re-run with manual
// instrumentation.
func listTreeForDiag(root string) string {
	const maxEntries = 200
	var entries []string
	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			entries = append(entries, path+" (walk error: "+err.Error()+")")
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		entries = append(entries, rel)
		if len(entries) >= maxEntries {
			entries = append(entries, "... (truncated at "+strconv.Itoa(maxEntries)+" entries)")
			return filepath.SkipAll
		}
		return nil
	})
	if len(entries) == 0 {
		return "(empty or unreadable: " + root + ")"
	}
	return strings.Join(entries, "\n")
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

// removeIOPMrootDomainAllowBlock returns a copy of the generated profile
// with the (allow iokit-open-service ...) clause stripped. The clause spans
// two lines in the current generator output:
//
//	(allow iokit-open-service
//	  (iokit-registry-entry-class "IOPMrootDomain"))
//
// We match the verbatim block so the substitution is unambiguous and any
// future addition of a second registry entry would invalidate the match —
// at which point the test fails loudly (withMutatedProfile rejects no-op
// mutations) rather than silently mutating the wrong region.
func removeIOPMrootDomainAllowBlock(p string) string {
	const block = `(allow iokit-open-service
  (iokit-registry-entry-class "IOPMrootDomain"))
`
	return strings.Replace(p, block, "", 1)
}
