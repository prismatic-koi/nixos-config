//go:build darwin

package integration_test

// sandbox_exec_keychain_darwin_test.go — integration coverage for the
// ~/Library/Keychains/login.keychain-db file-read literal grant (section
// §5i of generateProfile). The grant provides the in-sandbox Keychain
// capability the keyring-crate path needs — Datadog's `pup` CLI is the
// canonical consumer, and any Rust binary built on the keyring crate
// shares the code path on macOS.
//
// Per docs/sandbox-exec-testing.md, the coverage is a positive/negative
// pair on the rule, plus profile-shape assertions for the security
// narrowing the grant relies on:
//
//   - TestSandboxExecProfile_LoginKeychainLiteralPresent verifies the
//     generated profile carries an allow rule on the exact literal path
//     and that the rule is emitted unconditionally (no os.Stat gate).
//     Fresh machines without a user keychain remain unaffected —
//     sandbox-exec silently ignores literal rules for non-existent paths,
//     and the production rule shape pins that behaviour.
//
//   - TestSandboxExecProfile_LoginKeychainGrantIsLiteralNotSubpath
//     enforces the load-bearing narrowing: the profile must contain NO
//     (subpath ...) rule referencing ~/Library/Keychains or any directory
//     under it. The modern UUID-keyed databases (keychain-2.db, user.kb)
//     and the TrustedPeersHelper sibling files MUST remain unreadable from
//     inside the sandbox.
//
//   - TestSandboxExecProfile_KeychainLookupAllowed is the functional
//     positive. It compiles a tiny C probe that calls
//     SecKeychainFindGenericPassword — the same Security-Framework entry
//     point keyring-rs wraps on macOS, NOT /usr/bin/security (which
//     depends on $HOME, a different code path the issue body explicitly
//     calls out). The probe is invoked under the production-shaped
//     profile and must exit 0 with an OSStatus of either errSecSuccess (0)
//     or errSecItemNotFound (-25300). The exit code does not show a
//     missing grant: without the grant the lookup also returns
//     errSecItemNotFound (see keychainProbeSourceC). The check that
//     catches a missing grant is the absence of a probe deny line in the
//     kernel log.
//
//   - TestSandboxExecProfile_KeychainLookupDeniedWithoutGrant is the
//     paired negative. It uses withMutatedProfile per the testing
//     convention to re-target the (literal ...) at a non-existent sibling
//     path, runs the same probe, and asserts the observable result
//     differs from the positive — proving the grant is load-bearing. The
//     discriminator is the kernel sandbox-deny event surfaced via
//     `log show`: a `Sandbox: keychain_probe(NNN) deny(1) file-read-data
//     …/login.keychain-db` line, which the test asserts is absent under
//     the production profile and present under the mutated profile. Only
//     lines of the probe process count (keychainProbeDenyLines). The deny
//     comes from the direct open(2) of the probe, not from the lookup: see
//     keychainProbeSourceC.
//
// The existing deny set on ~/.aws, /private/etc/ssh, /private/etc/wireguard,
// and sops secrets.d is covered by the sandbox_exec_denies_darwin_test.go
// and sandbox_exec_secrets_deny_darwin_test.go suites.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// keychainProbeSourceC is the C source for the in-sandbox Keychain probe.
//
// The probe deliberately uses the legacy SecKeychain* API — the same entry
// point the keyring-rs macOS backend wraps (see keyring-rs / security-framework
// / SecKeychainFindGenericPassword) — so the exercised path matches what
// `pup` and other keyring-crate consumers hit at runtime. We do NOT shell out
// to /usr/bin/security: that CLI uses $HOME to find the keychain search list
// and is a different code path the issue body explicitly calls out.
//
// The deprecation-warning suppression is load-bearing for the build: without
// it clang turns the deprecation into a warning that adds noise to the
// integration test output. The deprecations themselves are surfaced by Apple
// as documentation hints for new code; the legacy API remains the path the
// keyring crate uses on production macOS today.
//
// After the lookup, the probe opens the login keychain file with open(2).
// The Security framework reads that file in the probe process. Before each
// read it calls sandbox_check with SANDBOX_CHECK_NO_REPORT. If the sandbox
// denies the read, the framework skips the keychain without an open(2): the
// lookup returns errSecItemNotFound and the kernel logs no deny. So the
// lookup alone cannot show a missing grant. The direct open(2) is the same
// read without the pre-check, and the kernel logs a deny of the probe for
// it. The negative test depends on that deny line.
//
// Exit codes:
//
//	0 = SecKeychainFindGenericPassword returned errSecSuccess (0) or
//	    errSecItemNotFound (-25300) — lookup completed without an
//	    unexpected error.
//	1 = SecKeychainFindGenericPassword returned a different OSStatus —
//	    the probe surfaced an error the positive test wants to see as
//	    a hard failure.
//	2 = Usage error (missing service or keychain path argument).
//
// The result of the direct open(2) does not change the exit code.
//
// The probe also writes "OSStatus=<n>\n" and "login_keychain_open=<result>\n"
// to stdout so the test can inspect both results for diagnostics,
// regardless of exit code.
const keychainProbeSourceC = `// keychain_probe.c — generated by sandbox_exec_keychain_darwin_test.go.
#define SEC_KEYCHAIN_DEPRECATED_OK 1
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

#include <Security/Security.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#pragma clang diagnostic pop

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: %s <service> <login-keychain-path>\n", argv[0]);
        return 2;
    }
    const char *service = argv[1];
    const char *loginKeychain = argv[2];

    UInt32 passwordLength = 0;
    void *passwordData = NULL;
    SecKeychainItemRef itemRef = NULL;

    OSStatus status = SecKeychainFindGenericPassword(
        NULL,                                  /* default keychain search list */
        (UInt32)strlen(service), service,
        0, NULL,
        &passwordLength, &passwordData,
        &itemRef);

    if (passwordData != NULL) SecKeychainItemFreeContent(NULL, passwordData);
    if (itemRef != NULL) CFRelease(itemRef);

    fprintf(stdout, "OSStatus=%d\n", (int)status);

    /* The same read as the Security framework, without its no-report
       sandbox_check. If the sandbox denies it, the kernel logs a deny of
       this process. */
    int fd = open(loginKeychain, O_RDONLY);
    if (fd >= 0) {
        close(fd);
        fprintf(stdout, "login_keychain_open=ok\n");
    } else {
        int err = errno;
        fprintf(stdout, "login_keychain_open=errno %d (%s)\n", err, strerror(err));
    }
    fflush(stdout);

    /* errSecSuccess is 0 and errSecItemNotFound is -25300. Either one means
       that the lookup ended with no unexpected error. Neither one shows
       that the framework read the login keychain. */
    if (status == 0 || status == -25300) {
        return 0;
    }
    return 1;
}
`

// keychainProbeBuildPath compiles the keychain probe with the host
// /usr/bin/clang (which carries the Xcode CLT SDK paths needed for
// <Security/Security.h>) and returns the absolute path of the produced
// binary. The binary is ad-hoc-signed by clang at link time — see
// `codesign -dv` on the output (Format=Mach-O, Signature=adhoc) — so it
// can run under the v3 deny-default sandbox profile (unlike Apple-signed
// system binaries, which SIGABRT in dyld — see requireNixBash).
//
// The Nix-built `cc` cannot be used: the gcc-wrapper does not expose the
// macOS SDK framework headers, so the build fails with
// "Security/Security.h: No such file or directory". The probe is purely a
// test fixture — using Apple's clang to build a fixture binary is
// independent of the requireNixBash rationale (which is about the
// runtime binary's signature, not the compiler).
func keychainProbeBuildPath(t *testing.T) string {
	t.Helper()

	const clangPath = "/usr/bin/clang"
	if _, err := os.Stat(clangPath); err != nil {
		t.Skipf("%s not present — Xcode Command Line Tools required to build the Keychain probe: %v", clangPath, err)
	}

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "keychain_probe.c")
	if err := os.WriteFile(srcPath, []byte(keychainProbeSourceC), 0o644); err != nil {
		t.Fatalf("write keychain probe source: %v", err)
	}

	binPath := filepath.Join(dir, "keychain_probe")
	cmd := exec.Command(clangPath,
		"-O0", "-g",
		"-o", binPath,
		srcPath,
		"-framework", "Security",
		"-framework", "CoreFoundation")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("clang build of keychain probe failed (this host may lack a usable Xcode CLT install): %v\nclang output:\n%s", err, out)
	}
	if _, statErr := os.Stat(binPath); statErr != nil {
		t.Fatalf("keychain probe binary missing after clang: %v\nclang output:\n%s", statErr, out)
	}
	return binPath
}

// loginKeychainPath returns the absolute path of the user's legacy login
// keychain (the file the §5i grant covers).
func loginKeychainPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db")
}

// expectedKeychainLiteralBlock returns the exact SBPL allow block
// generateProfile emits for the §5i Keychain grant, derived from the
// current user's home. Used both for content assertions and for the
// withMutatedProfile-based negative test.
func expectedKeychainLiteralBlock(t *testing.T) string {
	t.Helper()
	return "(allow file-read* file-test-existence file-read-metadata\n" +
		"  (literal " + sbplQuoteForTest(loginKeychainPath(t)) + "))\n"
}

// TestSandboxExecProfile_LoginKeychainLiteralPresent verifies that the
// production-shaped profile carries the §5i Keychain literal grant.
//
// Edge case: the rule is emitted unconditionally (no os.Stat gate on
// the host file), so a fresh machine without ~/Library/Keychains/login.keychain-db
// gets the same syntactically-valid profile. sandbox-exec silently ignores
// (literal ...) rules for non-existent paths, so the sandbox still launches
// — the production rule shape pins that behaviour.
func TestSandboxExecProfile_LoginKeychainLiteralPresent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	want := expectedKeychainLiteralBlock(t)
	if !strings.Contains(prepared.content, want) {
		t.Errorf("generated profile is missing the §5i Keychain literal allow block (issue #2293).\n"+
			"Expected to find verbatim:\n%s\n"+
			"Full profile:\n%s",
			want, prepared.content)
	}
}

// TestSandboxExecProfile_LoginKeychainGrantIsLiteralNotSubpath enforces the
// load-bearing security narrowing: the profile must contain no
// (subpath ...) rule referencing ~/Library/Keychains or any directory under
// it. The Keychain grant MUST be single-file via (literal ...) so that
// keychain-2.db, user.kb, and other sibling files in the same directory
// remain unreadable from inside the sandbox.
//
// We strip the known-good literal grant out of the profile before scanning,
// so a future widening to a subpath would still be caught even if the
// literal is also present.
func TestSandboxExecProfile_LoginKeychainGrantIsLiteralNotSubpath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	keychainsDir := filepath.Join(home, "Library", "Keychains")

	// Strip the legitimate literal block so we can scan the rest of the
	// profile without false-positives.
	scrubbed := strings.ReplaceAll(prepared.content, expectedKeychainLiteralBlock(t), "<keychain-literal-block-elided>\n")

	// Any subpath quoting ~/Library/Keychains in the scrubbed profile is
	// a security-narrowing regression.
	subpathProbe := "(subpath " + sbplQuoteForTest(keychainsDir)
	if strings.Contains(scrubbed, subpathProbe) {
		t.Errorf("profile contains a (subpath ...) grant on ~/Library/Keychains — AC #2 of issue #2293 prohibits this; only the single-file (literal …/login.keychain-db) is permitted.\nFull profile:\n%s", prepared.content)
	}

	// Tighter check: no path inside ~/Library/Keychains (other than the
	// elided literal block above) may appear in the scrubbed profile.
	if strings.Contains(scrubbed, keychainsDir) {
		t.Errorf("profile references ~/Library/Keychains beyond the single-file login.keychain-db literal — AC #2 of issue #2293 prohibits this.\nScrubbed profile:\n%s", scrubbed)
	}
}

// keychainProbeSentinelService is the service name passed to
// SecKeychainFindGenericPassword by the probe. The expectation is that no
// user keychain carries an entry for this name, so the lookup completes
// with errSecItemNotFound (-25300), with or without the §5i grant. (If a
// host somehow has this exact entry and the grant, the lookup returns
// errSecSuccess instead, and the probe also exits 0.)
const keychainProbeSentinelService = "prism-2293-keychain-probe-no-such-service"

// runKeychainProbe invokes the probe binary under sandbox-exec with the
// given profile path and returns the combined stdout/stderr plus the
// exit code and any run error.
func runKeychainProbe(t *testing.T, profilePath, probeBin string) (combined string, exitCode int, runErr error) {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, probeBin, keychainProbeSentinelService, loginKeychainPath(t))
	out, err := cmd.CombinedOutput()
	exit := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		exit = -1
	}
	return string(out), exit, err
}

// logShowAvailable reports whether `log show` can be invoked on the host.
// On a normal Darwin developer machine it works for any user; inside a
// nested sandbox-exec session it fails with "Cannot run while sandboxed".
// The nested-sandbox check is already gated by requireSandboxExec so this
// is primarily for environments without `/usr/bin/log` at all.
func logShowAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := os.Stat("/usr/bin/log"); err != nil {
		return false
	}
	cmd := exec.Command("/usr/bin/log", "show", "--last", "1s", "--style", "compact")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if strings.Contains(string(out), "Cannot run while sandboxed") {
		t.Logf("log show unavailable (nested sandbox): %s", strings.TrimSpace(string(out)))
		return false
	}
	t.Logf("log show probe failed: %v\n%s", err, string(out))
	return false
}

// keychainProbeDenyLines returns the kernel deny lines of the form
// "Sandbox: keychain_probe(<pid>) deny(...) ... login.keychain-db" from the
// `log show` output since since, or "" if there are none. The raw output can
// also hold the command line of `log` itself, which holds every word of the
// predicate, and a deny of another sandboxed process on the host. Neither one
// is a deny of the probe.
func keychainProbeDenyLines(t *testing.T, since time.Time) string {
	var lines []string
	for _, line := range strings.Split(queryLogShowForKeychainDeny(t, since), "\n") {
		if strings.Contains(line, "Sandbox: keychain_probe(") && strings.Contains(line, "deny(") &&
			strings.Contains(line, "login.keychain-db") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// queryLogShowForKeychainDeny invokes `log show` from since and returns its
// raw output, or "" if the query fails. The output is not a list of denies:
// it also holds the command line of `log` itself, which matches the
// predicate. Do not test this output directly. Filter it through
// keychainProbeDenyLines.
//
// The log does not hold every deny. The Security framework checks its read
// of a keychain file with SANDBOX_CHECK_NO_REPORT, so the kernel logs no
// deny for that check (see keychainProbeSourceC).
func queryLogShowForKeychainDeny(t *testing.T, since time.Time) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/log", "show",
		"--predicate", `eventMessage CONTAINS "Sandbox" AND eventMessage CONTAINS "login.keychain-db" AND eventMessage CONTAINS "deny"`,
		"--start", since.Format("2006-01-02 15:04:05"),
		"--info",
		"--style", "compact")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("log show query failed (treating as no signal): %v\n%s", err, string(out))
		return ""
	}
	return string(out)
}

// TestSandboxExecProfile_KeychainLookupAllowed is the positive test: a
// keyring-crate-shaped Keychain probe under the production-shaped SBPL
// profile completes, and the kernel logs no deny of the probe on
// login.keychain-db. The probe exercises
// SecKeychainFindGenericPassword — the same Security-Framework function
// keyring-rs's macOS backend calls into.
//
// We skip when the user's login keychain does not exist on the host
// (e.g. a fresh CI VM with no user account configured), since the
// positive signal is only meaningful when there is a real keychain for
// the sandbox to allow or deny.
func TestSandboxExecProfile_KeychainLookupAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	keychain := loginKeychainPath(t)
	if _, err := os.Stat(keychain); err != nil {
		t.Skipf("login keychain %s not present on host — positive Keychain probe requires a real keychain to discriminate sandbox-EPERM from missing-file: %v", keychain, err)
	}

	probeBin := keychainProbeBuildPath(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	since := time.Now().Add(-1 * time.Second)
	out, exit, runErr := runKeychainProbe(t, profilePath, probeBin)

	if exit != 0 {
		t.Errorf("Keychain probe exited %d under the production profile.\n"+
			"AC #3 of issue #2293: SecKeychainFindGenericPassword must complete\n"+
			"under the production profile. The probe surfaces OSStatus inline below.\n"+
			"A non-zero exit means that the OSStatus was neither errSecSuccess (0)\n"+
			"nor errSecItemNotFound (-25300): an unexpected Security-Framework error.\n"+
			"A missing §5i grant does not cause this exit, because without the grant\n"+
			"the lookup returns errSecItemNotFound. The kernel-log check catches a\n"+
			"missing grant.\n"+
			"Exit: %v\nOutput:\n%s\nProfile: %s",
			exit, runErr, out, profilePath)
	}

	// The kernel log must hold no deny of the probe on login.keychain-db.
	// This is the only check in this test that fails when the §5i grant
	// is missing: the probe exits 0 with or without the grant. If `log show`
	// is not usable (a nested sandbox, or no /usr/bin/log), this test cannot
	// see a missing grant. The paired negative test skips in that case.
	if logShowAvailable(t) {
		// `log show` ingestion has some latency; give the subsystem
		// a moment to flush so a real denial would show up by the time
		// we query.
		time.Sleep(500 * time.Millisecond)
		denies := keychainProbeDenyLines(t, since)
		if strings.Contains(denies, "login.keychain-db") {
			t.Errorf("kernel log carries a sandbox deny on ~/Library/Keychains/login.keychain-db over the probe window — the §5i grant is not effective.\nDeny lines:\n%s\nProbe output:\n%s\nProfile: %s",
				denies, out, profilePath)
		}
	}

	t.Logf("ka pai — Keychain probe under production profile produced %s (exit 0)", strings.TrimSpace(out))
}

// TestSandboxExecProfile_KeychainLookupDeniedWithoutGrant is the paired
// negative: in a profile identical to production EXCEPT with the
// §5i Keychain literal re-targeted at a non-existent sibling path, the
// same probe MUST surface a different observable than the positive — the
// kernel logs a sandbox deny of the probe process on login.keychain-db.
// The OSStatus does not discriminate: without the grant the lookup also
// returns errSecItemNotFound (see keychainProbeSourceC). This proves the
// grant is load-bearing and the positive test is not a no-op.
//
// Count only deny lines of the probe (keychainProbeDenyLines). The raw
// `log show` output matches even when no deny occurs.
//
// We use withMutatedProfile per the testing convention: rather than
// deleting the rule, we re-target the literal at a deliberately
// non-existent sibling path. This keeps the rest of the SBPL block
// structure unchanged and keeps the profile syntactically valid.
func TestSandboxExecProfile_KeychainLookupDeniedWithoutGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)

	keychain := loginKeychainPath(t)
	if _, err := os.Stat(keychain); err != nil {
		t.Skipf("login keychain %s not present on host — negative Keychain probe requires a real keychain to be denied: %v", keychain, err)
	}

	if !logShowAvailable(t) {
		// The kernel-log signal is the only reliable discriminator —
		// without it we cannot prove the grant is load-bearing.
		t.Skip("log show unavailable on this host — the negative AC requires the kernel-log discriminator to detect a sandbox deny on login.keychain-db, which we cannot read here. Run this test on the same host that runs the §5i CI gate.")
	}

	probeBin := keychainProbeBuildPath(t)

	m := newProfileManagerWithBareRoot(t)

	// Re-target the §5i literal at a non-existent sibling path. The
	// withMutatedProfile helper will fail the test if the substitution
	// does not match, so a format-drift regression in the generator is
	// caught here before we even reach the sandbox-exec run.
	denyTarget := keychain + ".prism-2293-disabled"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			sbplQuoteForTest(keychain),
			sbplQuoteForTest(denyTarget))
	})

	since := time.Now().Add(-1 * time.Second)
	out, exit, runErr := runKeychainProbe(t, mutatedPath, probeBin)

	// Sandbox-exec must still accept the mutated profile (re-targeting at
	// a non-existent sibling path is the documented withMutatedProfile
	// idiom — sandbox-exec silently ignores literal rules for
	// non-existent paths, so the profile remains loadable).
	if strings.Contains(out, "sandbox-exec: profile parser:") {
		t.Fatalf("mutated profile failed to parse — the negative test setup is wrong.\nExit: %v\nOutput:\n%s\nProfile: %s",
			runErr, out, mutatedPath)
	}

	// Give `log show` a moment to ingest the deny.
	time.Sleep(1 * time.Second)
	denies := keychainProbeDenyLines(t, since)
	if denies == "" {
		t.Errorf("kernel log does NOT carry a `Sandbox: keychain_probe(<pid>) deny(...)` line on ~/Library/Keychains/login.keychain-db after running the probe under the mutated profile.\n"+
			"AC #4 of issue #2293 requires the same probe to fail WITHOUT the §5i grant — proving the grant is load-bearing and the positive test is not a no-op.\n"+
			"If your host has tightened `log show` access (e.g. a downstream MDM policy), re-run this test on a host that surfaces unified-log events to the invoking user.\n"+
			"Probe exit: %d\nProbe output:\n%s\nMutated profile: %s\nDeny lines (empty = no deny found):\n%q",
			exit, out, mutatedPath, denies)
		return
	}

	t.Logf("ka pai — without the §5i grant the kernel emitted a sandbox deny on login.keychain-db (probe exit %d):\n%s", exit, strings.TrimSpace(denies))
}
