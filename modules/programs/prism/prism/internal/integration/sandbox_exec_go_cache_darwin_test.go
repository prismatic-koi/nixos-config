//go:build darwin

package integration_test

// sandbox_exec_go_cache_darwin_test.go — integration coverage for the
// section-5k RW grant on the two Go cache directories:
// ~/go/pkg/mod (GOMODCACHE) and ~/Library/Caches/go-build (GOCACHE).
//
// The repo AGENTS.md names `go build ./...` and `go test ./...` (run from
// modules/programs/prism/prism/) as "the first check for any prism code
// change". Without these grants the toolchain fails on its first cache write
// under deny-default, and the repo's primary quality gate cannot run as
// documented inside a Darwin worker.
//
// This file tests:
//
//  1. Positive: PrepareSandboxExec materialises both directories host-side
//     (a (subpath ...) grant on a non-existent path is a silent no-op the
//     sandboxed process cannot repair — its ungranted parents return EPERM),
//     and under the production profile a create→read→remove round-trip of a
//     uniquely-named file succeeds inside each one.
//
//  2. Positive, execution: a binary placed in GOCACHE RUNS under the
//     production profile. This is the half that actually tracks the primary
//     requirement (a Darwin worker can run `go build ./...` and
//     `go test ./...`), because cmd/go can serve a linked test binary
//     straight out of the
//     build cache on a warm build. Note what governs it: section 9's
//     UNQUALIFIED (allow process-exec* ...), not section 5k. Its paired
//     negative therefore mutates the rule that genuinely governs execution.
//
//  3. Negative, execution: a binary placed in GOMODCACHE does NOT run,
//     because of the explicit section-22 deny. Paired with a strip negative
//     proving that deny is load-bearing — without it the same binary runs,
//     since section 9 permits execution profile-wide.
//
//  4. Negative (whole-block strip): removing the ENTIRE section-5k allow
//     block makes the same writes fail — proving the block is load-bearing
//     for both paths. Stripping only one (subpath ...) line leaves the other
//     granted and, in the single-filter case, risks a filter-less
//     allow-everything clause.
//
// Section 9's (allow process-exec* ...) carries no path filter, so it permits
// execution profile-wide — no section-5k grant governs execution. A planted
// binary therefore executes from the module cache under the production
// profile, and from the build cache even with all of section 5k stripped.
// The profile carries an explicit deny for the module cache, and these tests
// assert the rules that actually decide the outcome.
//
// The security half — that no path OUTSIDE the two cache leaves is
// granted (notably ~/go, which contains the PATH-visible ~/go/bin) — is
// asserted at profile-string level by
// TestGenerateProfile_GoCacheGrantsNothingOutsideTheCaches in
// internal/container/sandbox_exec_test.go.
//
// Per docs/sandbox-exec-testing.md. Capability-probe gating via
// requireSandboxExec.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goCacheDirs5k returns the two section-5k directories, keyed by a short
// label. The paths mirror container.goCacheDirs — the Go toolchain's Darwin
// defaults, which are what the in-sandbox toolchain always resolves to
// because the sandbox env forwards no GO* variable.
func goCacheDirs5k(t *testing.T) map[string]string {
	t.Helper()
	home := realUserHome(t)
	return map[string]string{
		"gomodcache": filepath.Join(home, "go", "pkg", "mod"),
		"gocache":    filepath.Join(home, "Library", "Caches", "go-build"),
	}
}

// goCacheGrantBlock5k returns the exact section-5k allow block emitted by
// generateProfile, for the presence assertion in the positive test and the
// whole-block strip in the negative.
// The two clauses carry identical operations. Neither carries
// file-map-executable (it is inert — section 9 governs execution
// profile-wide).
func goCacheGrantBlock5k(t *testing.T) string {
	t.Helper()
	home := realUserHome(t)
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, "go", "pkg", "mod")) + "))\n" +
		"\n" +
		"(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, "Library", "Caches", "go-build")) + "))\n"
}

// goCacheExecDenyBlock returns the exact section-22 deny emitted by
// generateProfile — the rule that stops execution out of the module cache.
func goCacheExecDenyBlock(t *testing.T) string {
	t.Helper()
	home := realUserHome(t)
	return "(deny process-exec* file-map-executable\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, "go", "pkg", "mod")) + "))\n"
}

// TestSandboxExecGoCache_RoundTrip is the positive integration test for the
// section-5k RW grants: PrepareSandboxExec creates both cache dirs, and a
// create→read→remove round-trip succeeds inside each of them under the
// production profile.
func TestSandboxExecGoCache_RoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	dirs := goCacheDirs5k(t)

	// BareRoot manager: real-path access under HOME requires the
	// BareRoot-ancestor block's metadata allow on (subpath HOME).
	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// Prepare must have materialised both dirs host-side: a grant on a path
	// that does not exist is a silent no-op, and the
	// sandboxed process cannot create it — MkdirAll would first have to
	// mkdir the ungranted parents (~/go, ~/go/pkg, ~/Library/Caches).
	for label, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("PrepareSandboxExec did not create the %s dir %s: %v", label, dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s (%s) exists but is not a directory", label, dir)
		}
	}

	block := goCacheGrantBlock5k(t)
	if !strings.Contains(prepared.content, block) {
		t.Fatalf("generated profile does not contain the section-5k Go cache block:\n%s\nProfile:\n%s", block, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	for label, dir := range dirs {
		t.Run(label, func(t *testing.T) {
			testFile := filepath.Join(dir, ".prism-2621-go-cache-rw-"+label)
			t.Cleanup(func() { _ = os.Remove(testFile) }) // host-side safety net
			sentinel := "prism-2621-" + label + "-sentinel"
			script := "printf '%s' " + shQuote(sentinel) + " > " + shQuote(testFile) +
				" && cat " + shQuote(testFile) +
				" && rm " + shQuote(testFile)
			cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
				"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("in-sandbox RW round-trip in %s failed under the production profile — "+
					"the AGENTS.md go build/test gate cannot run (issue #2621).\n"+
					"Exit: %v\nOutput: %s\nProfile: %s", dir, runErr, string(out), testProfilePath)
			}
			if !strings.Contains(string(out), sentinel) {
				t.Errorf("round-trip read in %s did not return the sentinel.\nGot: %s", dir, string(out))
			}
		})
	}
}

// TestSandboxExecGoCache_DeniedWithoutGrantBlock is the paired negative test:
// with the ENTIRE section-5k block stripped, the same writes fail in both
// cache dirs — proving the block is the load-bearing capability and no other
// rule in the profile already granted these paths.
func TestSandboxExecGoCache_DeniedWithoutGrantBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	dirs := goCacheDirs5k(t)

	m := newProfileManagerWithBareRoot(t)

	block := goCacheGrantBlock5k(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		if !strings.Contains(p, block) {
			t.Fatalf("profile does not contain the section-5k block to strip:\n%s\nProfile:\n%s", block, p)
		}
		return strings.Replace(p, block, "", 1)
	})

	for label, dir := range dirs {
		t.Run(label, func(t *testing.T) {
			if _, err := os.Stat(dir); err != nil {
				t.Skipf("%s does not exist on this host — nothing to probe: %v", dir, err)
			}
			testFile := filepath.Join(dir, ".prism-2621-go-cache-denied-"+label)
			t.Cleanup(func() { _ = os.Remove(testFile) }) // in case the deny fails
			script := "echo prism-2621-denied > " + shQuote(testFile)
			cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
				"/usr/bin/env", "HOME="+realUserHome(t), nixBash, "-c", script)
			out, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Errorf("write into %s succeeded WITHOUT the section-5k block.\n"+
					"The 5k grant is not the load-bearing rule — investigate.\n"+
					"Output: %s\nMutated profile: %s", dir, string(out), mutatedPath)
			} else {
				t.Logf("ka pai — write into %s correctly denied without the 5k block (exit: %v)", dir, runErr)
			}
		})
	}
}

// plantExecutableForTest copies src to <dir>/<name>, marks it executable, and
// registers a cleanup. Returns the planted path.
//
// Copying rather than symlinking is deliberate: SBPL path filters evaluate the
// RESOLVED target for open-class operations, so a symlink into /nix/store
// would be judged against the section-2 /nix grant and the probe would prove
// nothing about the directory under test.
func plantExecutableForTest(t *testing.T, src, dir, name string) string {
	t.Helper()

	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("cannot read %s to plant a test binary: %v", src, err)
	}
	dst := filepath.Join(dir, name)
	t.Cleanup(func() { _ = os.Remove(dst) })
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Skipf("cannot plant a test binary at %s: %v", dst, err)
	}
	return dst
}

// TestSandboxExecGoCache_BuildCacheExecutable is the positive test: a binary
// that lives in GOCACHE can be EXECUTED under the production profile, not
// merely written and read.
//
// This matters because cmd/go can serve a linked test binary straight out of
// the build cache on a warm build, so `go test ./...` on its second run may
// exec from GOCACHE rather than from $WORK.
//
// What governs the outcome is section 9's (allow process-exec* ...), which
// carries NO path filter and therefore permits execution profile-wide — NOT
// the section-5k grant. The same probe succeeds with all of section 5k
// stripped, which confirms section-5k grants never govern execution. The
// paired negative below therefore mutates section 9's effect, which is the
// rule under test.
func TestSandboxExecGoCache_BuildCacheExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	buildCache := goCacheDirs5k(t)["gocache"]

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	planted := plantExecutableForTest(t, nixBash, buildCache, ".prism-2621-gocache-exec")

	const sentinel = "prism-2621-gocache-exec-ok"
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf '%s' "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("executing a binary from GOCACHE (%s) failed under the production profile — "+
			"a warm-cache `go test` will fail the same way (issue #2621 AC-1).\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", buildCache, runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), sentinel) {
		t.Errorf("binary in GOCACHE ran but did not emit the sentinel.\nGot: %s", string(out))
	}
}

// TestSandboxExecGoCache_BuildCacheExecDeniedWhenProcessExecDenied is the
// paired negative for the positive above, and it names the rule that truly
// governs: section 9's unqualified (allow process-exec* ...).
//
// The mutation appends a deny for the build cache to the END of the profile.
// Appending is what makes it effective — SBPL resolves a conflict in favour
// of the LATER rule, so a deny placed after section 9's allow overrides it
// for that subpath. If execution then still succeeds, the profile does not
// govern execution the way this file claims and the positive above is
// meaningless.
//
// This deliberately does NOT strip the section-5k block. Stripping it does
// not deny execution — section 5k never governed it — so asserting otherwise
// is a false control.
func TestSandboxExecGoCache_BuildCacheExecDeniedWhenProcessExecDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	buildCache := goCacheDirs5k(t)["gocache"]
	if _, err := os.Stat(buildCache); err != nil {
		t.Skipf("%s does not exist on this host — nothing to probe: %v", buildCache, err)
	}

	m := newProfileManagerWithBareRoot(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return p + "\n(deny process-exec* file-map-executable\n" +
			"  (subpath " + sbplQuoteForTest(buildCache) + "))\n"
	})

	planted := plantExecutableForTest(t, nixBash, buildCache, ".prism-2621-gocache-exec-denied")

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf denied")
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("a binary in %s executed even WITH an explicit (deny process-exec* ...) for that subpath.\n"+
			"Execution is not governed the way this file assumes — the positive test above proves nothing.\n"+
			"Output: %s\nMutated profile: %s", buildCache, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — exec from %s correctly denied by an explicit deny (exit: %v)", buildCache, runErr)
	}
}

// TestSandboxExecGoCache_ModuleCacheExecDenied is the security-side positive
// for the section-22 deny: a binary planted in GOMODCACHE does NOT run under
// the production profile.
//
// The module cache holds dependency SOURCE, and section 5k grants it
// read-write so the toolchain can populate it. Without the explicit deny a
// sandboxed process can plant a binary among the dependency sources and
// execute it.
func TestSandboxExecGoCache_ModuleCacheExecDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	modCache := goCacheDirs5k(t)["gomodcache"]

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	deny := goCacheExecDenyBlock(t)
	if !strings.Contains(prepared.content, deny) {
		t.Fatalf("generated profile does not contain the section-22 module-cache exec deny:\n%s\nProfile:\n%s", deny, prepared.content)
	}
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	planted := plantExecutableForTest(t, nixBash, modCache, ".prism-2621-gomodcache-exec")

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf executed")
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("a binary in the module cache %s EXECUTED under the production profile.\n"+
			"GOMODCACHE holds dependency source and must not be executable. Check that the "+
			"section-22 deny is still emitted AFTER section 9's unqualified (allow process-exec* ...) — "+
			"SBPL takes the later rule, so a deny moved above it is silently overridden.\n"+
			"Output: %s\nProfile: %s", modCache, string(out), testProfilePath)
	} else {
		t.Logf("ka pai — exec from the module cache %s correctly denied (exit: %v)", modCache, runErr)
	}
}

// TestSandboxExecGoCache_ModuleCacheExecAllowedWithoutDenyBlock is the paired
// strip negative proving the section-22 deny is load-bearing rather than
// decorative.
//
// With the deny removed, the same binary MUST run — because section 9 allows
// process-exec* with no path filter. If it fails to run without the deny,
// something else is blocking execution and the positive above is green for
// the wrong reason.
//
// If the module cache's non-executability is expressed as the mere absence
// of file-map-executable on its allow clause, it enforces nothing at all.
// This test catches that mistake.
func TestSandboxExecGoCache_ModuleCacheExecAllowedWithoutDenyBlock(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	modCache := goCacheDirs5k(t)["gomodcache"]
	if _, err := os.Stat(modCache); err != nil {
		t.Skipf("%s does not exist on this host — nothing to probe: %v", modCache, err)
	}

	m := newProfileManagerWithBareRoot(t)
	deny := goCacheExecDenyBlock(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		if !strings.Contains(p, deny) {
			t.Fatalf("profile does not contain the section-22 deny to strip:\n%s\nProfile:\n%s", deny, p)
		}
		return strings.Replace(p, deny, "", 1)
	})

	planted := plantExecutableForTest(t, nixBash, modCache, ".prism-2621-gomodcache-exec-nodeny")

	const sentinel = "prism-2621-nodeny-ran"
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf '%s' "+shQuote(sentinel))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Errorf("a binary in %s did NOT run with the section-22 deny stripped.\n"+
			"Execution there is blocked by something other than that deny, so "+
			"TestSandboxExecGoCache_ModuleCacheExecDenied passes for the wrong reason.\n"+
			"Exit: %v\nOutput: %s\nMutated profile: %s", modCache, runErr, string(out), mutatedPath)
	} else if !strings.Contains(string(out), sentinel) {
		t.Errorf("binary ran without the deny but did not emit the sentinel.\nGot: %s", string(out))
	} else {
		t.Logf("ka pai — the section-22 deny is load-bearing: exec succeeds without it")
	}
}
