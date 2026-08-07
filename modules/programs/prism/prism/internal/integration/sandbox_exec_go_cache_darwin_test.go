//go:build darwin

package integration_test

// sandbox_exec_go_cache_darwin_test.go — integration coverage for the
// section-5k RW grant on the two Go cache directories (issue #2621):
// ~/go/pkg/mod (GOMODCACHE) and ~/Library/Caches/go-build (GOCACHE).
//
// The repo AGENTS.md names `go build ./...` and `go test ./...` (run from
// modules/programs/prism/prism/) as "the first check for any prism code
// change". Before #2621 neither Go cache path had a grant, so under
// deny-default the toolchain failed on its first cache write and the repo's
// primary quality gate could not run as documented inside a Darwin worker.
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
//     production profile. This is the half that actually tracks AC-1 of
//     #2621 ("a Darwin worker can run `go build ./...` and `go test ./...`").
//     A write round-trip cannot distinguish "the cache dirs are writable"
//     from "the documented commands work": the build cache stores linked
//     executables and cmd/go can serve a test binary straight out of it on a
//     warm build, so a profile that is writable but not
//     executable-mappable passes a write test and still fails the second
//     `go test` run.
//
//  3. Negative, execution: the same binary placed in GOMODCACHE does NOT
//     run. The module cache holds source; withholding file-map-executable
//     there is deliberate, and this pins it.
//
//  4. Negative (whole-block strip, per the #2243 lesson): removing the
//     ENTIRE section-5k allow block makes the same writes fail — proving the
//     block is load-bearing for both paths. Stripping only one
//     (subpath ...) line would leave the other granted and, in the
//     single-filter case, risk a filter-less allow-everything clause.
//
// The security half of the AC — that no path OUTSIDE the two cache leaves is
// granted (notably ~/go, which contains the PATH-visible ~/go/bin) — is
// asserted at profile-string level by
// TestGenerateProfile_GoCacheGrantsNothingOutsideTheCaches in
// internal/container/sandbox_exec_test.go.
//
// Per docs/sandbox-exec-testing.md (issue #1192); #2207 capability-probe
// gating via requireSandboxExec.

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
// The two clauses are separate because the permission sets differ: GOCACHE
// carries file-map-executable and GOMODCACHE deliberately does not.
func goCacheGrantBlock5k(t *testing.T) string {
	t.Helper()
	home := realUserHome(t)
	return "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, "go", "pkg", "mod")) + "))\n" +
		"\n" +
		"(allow file-read* file-write* file-test-existence file-read-metadata file-map-executable\n" +
		"  (subpath " + sbplQuoteForTest(filepath.Join(home, "Library", "Caches", "go-build")) + "))\n"
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

	// Prepare must have materialised both dirs host-side (issue #2621): a
	// grant on a path that does not exist is a silent no-op, and the
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

// TestSandboxExecGoCache_BuildCacheExecutable is the AC-tracking positive
// test for issue #2621: a binary that lives in GOCACHE can be EXECUTED under
// the production profile, not merely written and read.
//
// This is the half a write round-trip cannot cover. The Go build cache stores
// linked executables — `go build ./...` caches the built binary, and cmd/go
// can serve a test binary straight out of the cache on a warm build. In a
// (version 3) profile file-read* does not imply file-map-executable, so a
// grant that is read-write but not executable-mappable passes a write probe
// and still fails the second `go test` run: the cold run links into $WORK
// under the section-3b TMPDIR grant and succeeds, the warm run maps the cache
// entry and gets EPERM. That is AC-1 failing in the same shape as #2621.
//
// The probe copies the Nix-built bash into the cache dir and runs it. Nix
// binaries are ad-hoc signed and are the sanctioned test target per
// docs/sandbox-exec-testing.md (Apple-signed binaries SIGABRT under a
// deny-default profile — see #1190).
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

// TestSandboxExecGoCache_BuildCacheExecDeniedWithoutGrantBlock is the paired
// negative for the exec probe: with the section-5k block stripped, the same
// binary does not run.
func TestSandboxExecGoCache_BuildCacheExecDeniedWithoutGrantBlock(t *testing.T) {
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
	block := goCacheGrantBlock5k(t)
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		if !strings.Contains(p, block) {
			t.Fatalf("profile does not contain the section-5k block to strip:\n%s\nProfile:\n%s", block, p)
		}
		return strings.Replace(p, block, "", 1)
	})

	planted := plantExecutableForTest(t, nixBash, buildCache, ".prism-2621-gocache-exec-denied")

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf denied")
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("a binary in %s executed WITHOUT the section-5k block.\n"+
			"The 5k grant is not the load-bearing rule for execution — investigate.\n"+
			"Output: %s\nMutated profile: %s", buildCache, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — exec from %s correctly denied without the 5k block (exit: %v)", buildCache, runErr)
	}
}

// TestSandboxExecGoCache_ModuleCacheExecDenied is the security-side negative
// that pins the deliberate asymmetry in the section-5k grant (issue #2621):
// GOCACHE carries file-map-executable, GOMODCACHE does not.
//
// The module cache holds module SOURCE. Nothing in the documented gate execs
// out of it, so it stays non-executable under the production profile even
// though it is read-write. This test is what stops a future edit from
// levelling the two grants "for symmetry" and silently handing a sandboxed
// process an executable staging area among the dependency sources.
func TestSandboxExecGoCache_ModuleCacheExecDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	modCache := goCacheDirs5k(t)["gomodcache"]

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	planted := plantExecutableForTest(t, nixBash, modCache, ".prism-2621-gomodcache-exec")

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", "HOME="+realUserHome(t), planted, "-c", "printf executed")
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("a binary in the module cache %s EXECUTED under the production profile.\n"+
			"GOMODCACHE holds source and must not be executable-mappable — "+
			"check whether file-map-executable leaked into its grant.\n"+
			"Output: %s\nProfile: %s", modCache, string(out), testProfilePath)
	} else {
		t.Logf("ka pai — exec from the module cache %s correctly denied (exit: %v)", modCache, runErr)
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
