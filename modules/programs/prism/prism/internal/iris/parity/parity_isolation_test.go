package parity_test

// parity_isolation_test.go — cross-cutting isolation + tripwire assertions
// for the iris D-10 parity gate.
//
// Covers two D-10 security ACs:
//
//  1. "No parity test invokes any `prism` binary, `prism` Go package, or
//     `~/.local/state/prism/` filesystem path. Verified by a
//     test-suite-level mechanism — either a build-tag-protected init() that
//     panics on import of internal/container, internal/sidecar,
//     internal/session, **or** an IRIS_PARITY_TEST_MODE=1 env var that, when
//     set, makes the prism binary os.Exit(99) on startup with a clear error
//     message."
//
//     This suite uses the **env-var tripwire** approach. The constant
//     iristest.EnvParityTestMode is the env-var name; iristest.NewIsolated
//     sets it for every parity test. The prism binary's main() (see
//     prism/main.go) checks for it on startup and exits 99. The unit test
//     TestPrismBinaryTripwireDocumented asserts that the constant matches
//     the string prism actually checks (a grep over main.go) so a rename
//     on either side surfaces immediately.
//
//  2. "No parity test reads or writes the real host paths
//     ~/.local/state/iris/iris.db, ~/.local/state/iris/iris.sock,
//     ~/.local/state/iris/run/, or ~/code/archives/iris/. The suite
//     redirects iris's storage roots to a t.TempDir() via the equivalent of
//     sidecartest.NewIsolated adapted for iris. Verified by an explicit
//     assertion at suite startup that os.Getenv("XDG_STATE_HOME") resolves
//     under t.TempDir() for every test."

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// TestIsolationContractHolds is the per-test self-check mandated by the
// D-10 security AC. It runs first (alphabetical ordering puts I before any
// other parity test file's lead test) and asserts that every iris path
// resolves under the per-test tempdir.
func TestIsolationContractHolds(t *testing.T) {
	iso := iristest.NewIsolated(t)
	if err := iristest.CheckXDGUnderTempDir(iso.Root); err != nil {
		t.Fatalf("isolation contract: %v", err)
	}
	// The iris.Paths fields must all be rooted under iso.Root EXCEPT for
	// Sock and RunDir which are redirected to a short MkdirTemp prefix to
	// keep per-session sockets within the 108-byte sun_path limit. Both
	// short-prefix paths still live under os.TempDir(), not under any
	// host-owned location.
	for _, p := range []struct {
		name string
		val  string
	}{
		{"DB", iso.Paths.DB},
		{"LogDir", iso.Paths.LogDir},
		{"ConfigFile", iso.Paths.ConfigFile},
		{"ArchiveRoot", iso.Paths.ArchiveRoot},
	} {
		if !strings.HasPrefix(p.val, iso.Root) {
			t.Errorf("iris.Paths.%s = %q does not resolve under tempdir %q", p.name, p.val, iso.Root)
		}
	}
	for _, p := range []struct {
		name string
		val  string
	}{
		{"Sock", iso.Paths.Sock},
		{"RunDir", iso.Paths.RunDir},
	} {
		if !strings.HasPrefix(p.val, os.TempDir()) {
			t.Errorf("iris.Paths.%s = %q does not resolve under a tempdir prefix", p.name, p.val)
		}
	}
	// Sanity: the resolved paths must not contain the substring 'prism'
	// (the iris codename invariant) and must not equal any path that ends
	// in the literal host iris path suffix when anchored at a non-tempdir
	// HOME. We already verified the prefix-under-tempdir invariant above;
	// this is a defence-in-depth grep for any host-anchored path string
	// that might have leaked through a Setenv ordering bug.
	for _, p := range []string{iso.Paths.DB, iso.Paths.Sock, iso.Paths.RunDir, iso.Paths.LogDir, iso.Paths.ConfigFile, iso.Paths.ArchiveRoot} {
		if strings.HasPrefix(p, "/home/") && !strings.HasPrefix(p, iso.Home) {
			t.Errorf("iris path %q is under a non-tempdir home prefix — isolation broken", p)
		}
		if strings.HasPrefix(p, os.Getenv("HOME")) && os.Getenv("HOME") != iso.Home {
			t.Errorf("iris path %q resolves under non-test HOME %q", p, os.Getenv("HOME"))
		}
	}
}

// TestPrismBinaryTripwireDocumented asserts that the env-var the iristest
// package sets matches the env-var the prism binary's main() checks. This
// is a static check on prism/main.go — no subprocess is invoked, no prism
// code runs.
//
// If this test fails, either:
//
//   - the constant iristest.EnvParityTestMode was renamed; or
//   - prism/main.go was edited and no longer checks for the same name.
//
// Either way the tripwire is broken: a parity test could accidentally
// invoke prism without exiting 99. Fix one side or the other and re-run.
func TestPrismBinaryTripwireDocumented(t *testing.T) {
	mainPath := findPrismMainGo(t)
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %q: %v", mainPath, err)
	}
	want := iristest.EnvParityTestMode
	if !strings.Contains(string(src), `"`+want+`"`) {
		t.Errorf("prism main.go (%s) does not mention %q — tripwire is broken; expected an os.Getenv(%q) check", mainPath, want, want)
	}
	// The exit code documented in the AC is 99.
	if !strings.Contains(string(src), "os.Exit(99)") && !strings.Contains(string(src), "irisParityTripwireExitCode") {
		t.Errorf("prism main.go (%s) does not exit 99 on tripwire: AC requires os.Exit(99) with a clear error message", mainPath)
	}
}

// TestPrismPackagesNotImportedByParity asserts that the parity package
// graph does NOT import any of the forbidden prism-owned packages. This
// catches accidental imports at compile time — but a static AST check
// makes the failure mode explicit (the error message names the package
// rather than a cryptic build error).
//
// Forbidden imports per the D-10 security AC:
//
//   internal/container, internal/sidecar, internal/session
//
// The parity tests legitimately use internal/db and internal/git (the
// schema/git wrappers are shared per §10.4). Only the three named
// packages are gated.
func TestPrismPackagesNotImportedByParity(t *testing.T) {
	pkgDir := mustPackageDir(t)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse parity package dir: %v", err)
	}

	forbidden := []string{
		"github.com/prismatic-koi/prism/internal/container",
		"github.com/prismatic-koi/prism/internal/sidecar",
		"github.com/prismatic-koi/prism/internal/session",
	}

	for pkgName, pkg := range pkgs {
		for fname, f := range pkg.Files {
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				for _, bad := range forbidden {
					if path == bad {
						t.Errorf("parity file %s/%s imports forbidden package %q (pkg %s)", pkgDir, fname, bad, pkgName)
					}
				}
			}
			// Also disallow the cmd/ package which is the prism CLI tree.
			for _, imp := range f.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if path == "github.com/prismatic-koi/prism/cmd" {
					t.Errorf("parity file %s/%s imports prism CLI package %q — parity tests must not import the prism CLI surface", pkgDir, fname, path)
				}
			}
		}
	}

	// NB: iris itself legitimately imports internal/container for shared
	// sandbox primitives (bwrap mount arg construction, sandbox-exec SBPL
	// helpers). D-11 removes that dependency by inlining the helpers; until
	// then the parity gate only forbids forbidden imports from the parity
	// test files themselves, not from internal/iris/.
}

// TestNoPrismFilesystemPathTouched asserts that after a full parity run
// (well, the lifetime of this single test), no path under
// ~/.local/state/prism/ was created. This is a structural check; the
// real ~/.local/state/prism/ may exist for non-test reasons, but no new
// inode should have been written by the parity suite. We assert this by
// checking that ResolvePaths does not return any prism-prefixed path.
func TestNoPrismFilesystemPathTouched(t *testing.T) {
	iso := iristest.NewIsolated(t)
	paths := iso.Paths
	for _, p := range []string{paths.DB, paths.Sock, paths.RunDir, paths.LogDir, paths.ConfigFile, paths.ArchiveRoot} {
		if strings.Contains(p, "prism") {
			t.Errorf("resolved iris path %q contains 'prism'", p)
		}
	}
	// Use the imported iris package to silence the unused-import linter on
	// platforms where the helpers test file has no other reference.
	_ = iris.ProtocolVersion
}

// findPrismMainGo locates prism/main.go relative to this test file. We use
// runtime.Caller to anchor the path so the test is robust against future
// Go module reorganisation.
func findPrismMainGo(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("findPrismMainGo: runtime.Caller failed")
	}
	// thisFile is .../prism/internal/iris/parity/parity_isolation_test.go
	// prism module root is .../prism/, walk upward until we find main.go
	// alongside go.mod (anchoring on go.mod avoids depending on a fixed depth).
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			candidate := filepath.Join(dir, "main.go")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			t.Fatalf("findPrismMainGo: go.mod found at %q but no main.go alongside", dir)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("findPrismMainGo: walked %d parents without finding go.mod from %q", 10, thisFile)
	return ""
}

// mustPackageDir returns the absolute path to this test's package directory.
func mustPackageDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("mustPackageDir: runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}
