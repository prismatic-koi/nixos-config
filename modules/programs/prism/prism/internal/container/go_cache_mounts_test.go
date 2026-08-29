package container

// go_cache_mounts_test.go — unit coverage for the Linux bwrap Go cache
// mounts and for the shared, platform-aware path list they
// derive from (go_cache.go).
//
// These are argv-and-spec assertions. The paired behavioural proof — a real
// bwrap sandbox that writes through the mounts and reuses a warm cache —
// lives in go_cache_mount_bwrap_test.go, which is Linux-only.

import (
	"os"
	"path/filepath"
	"testing"
)

// goCacheLinuxLeaves returns the two Linux leaf cache paths for home, spelled
// out as LITERALS rather than derived from goCacheDirsForGOOS. A test that
// derives its expectation from the code under test cannot catch a change to
// that code; the Darwin test makes the same choice for the same reason.
func goCacheLinuxLeaves(home string) (modCache, buildCache string) {
	return filepath.Join(home, "go", "pkg", "mod"), filepath.Join(home, ".cache", "go-build")
}

// findSpec returns the MountSpec with the given HostPath, and whether it was
// found.
func findSpec(specs []MountSpec, hostPath string) (MountSpec, bool) {
	for _, spec := range specs {
		if spec.HostPath == hostPath {
			return spec, true
		}
	}
	return MountSpec{}, false
}

// ── the shared platform-aware list (go_cache.go) ─────────────────────────────

// TestGoCacheDirsForGOOS_PerPlatformPaths pins the one property that makes a
// single shared list correct for two platforms: the module cache path is
// IDENTICAL on both (GOPATH defaults to <home>/go everywhere), while the
// build cache path differs because os.UserCacheDir() is <home>/.cache on
// Linux and <home>/Library/Caches on Darwin.
func TestGoCacheDirsForGOOS_PerPlatformPaths(t *testing.T) {
	const home = "/home/example"

	linux := goCacheDirsForGOOS(home, goosLinux)
	darwin := goCacheDirsForGOOS(home, goosDarwin)

	if len(linux) != 2 || len(darwin) != 2 {
		t.Fatalf("want 2 entries per platform; got linux=%v darwin=%v", linux, darwin)
	}
	if linux[0].path != darwin[0].path {
		t.Errorf("module cache path must be the same on both platforms (GOPATH default): linux %q, darwin %q",
			linux[0].path, darwin[0].path)
	}
	if want := filepath.Join(home, "go", "pkg", "mod"); linux[0].path != want {
		t.Errorf("module cache = %q; want %q", linux[0].path, want)
	}
	if want := filepath.Join(home, ".cache", "go-build"); linux[1].path != want {
		t.Errorf("linux build cache = %q; want %q — os.UserCacheDir() is ~/.cache on Linux, NOT ~/Library/Caches",
			linux[1].path, want)
	}
	if want := filepath.Join(home, "Library", "Caches", "go-build"); darwin[1].path != want {
		t.Errorf("darwin build cache = %q; want %q", darwin[1].path, want)
	}

	// execDenied is a property of WHICH cache it is, not of the platform:
	// the module cache holds source, the build cache serves linked test
	// binaries. Whether the flag can be ENFORCED is per-platform (bwrap
	// cannot), but the classification must not drift between the two.
	for _, tc := range []struct {
		name string
		dirs []goCacheDir
	}{{"linux", linux}, {"darwin", darwin}} {
		if !tc.dirs[0].execDenied {
			t.Errorf("%s: module cache must carry execDenied", tc.name)
		}
		if tc.dirs[1].execDenied {
			t.Errorf("%s: build cache must NOT carry execDenied — cmd/go execs linked test binaries out of it", tc.name)
		}
	}
}

// TestGoCacheDirsForGOOS_FailsClosed covers the two "no grant" inputs: no
// resolvable home, and a platform prism does not support. Both must return
// nil so no caller emits a bind or a grant rooted at "/".
func TestGoCacheDirsForGOOS_FailsClosed(t *testing.T) {
	if dirs := goCacheDirsForGOOS("", goosLinux); dirs != nil {
		t.Errorf("goCacheDirsForGOOS(\"\", linux) = %v; want nil", dirs)
	}
	if dirs := goCacheDirsForGOOS("", goosDarwin); dirs != nil {
		t.Errorf("goCacheDirsForGOOS(\"\", darwin) = %v; want nil", dirs)
	}
	if dirs := goCacheDirsForGOOS("/home/example", "windows"); dirs != nil {
		t.Errorf("goCacheDirsForGOOS(home, \"windows\") = %v; want nil", dirs)
	}
}

// TestGoCacheDirs_DarwinDelegatesToSharedList is the single-source-of-truth
// link: the Darwin SBPL generator's goCacheDirs must be the
// same list the shared function returns for darwin, not a second copy. The
// companion assertion that Darwin's PATHS and execDenied FLAGS are unchanged
// is TestGoCacheDirs_DarwinDefaults in sandbox_exec_test.go, which asserts
// them as literals.
func TestGoCacheDirs_DarwinDelegatesToSharedList(t *testing.T) {
	const home = "/Users/example"
	got, want := goCacheDirs(home), goCacheDirsForGOOS(home, goosDarwin)
	if len(got) != len(want) {
		t.Fatalf("goCacheDirs(%q) = %v; want %v", home, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("goCacheDirs[%d] = %+v; want %+v — the Darwin grant must not fork from the shared list", i, got[i], want[i])
		}
	}
}

// ── the mount specs (mounts.go) ──────────────────────────────────────────────

// TestStandardSandboxMounts_GoCacheDirsRW is the functional AC: the bwrap
// mount set carries both Go caches, read-write, at the same paths the host
// uses (Dst==Src, since sandboxHomeDir == hostHome under bwrap).
func TestStandardSandboxMounts_GoCacheDirsRW(t *testing.T) {
	const home = "/home/example"
	specs := StandardSandboxMounts(Config{}, home, home, isolationBwrap)
	modCache, buildCache := goCacheLinuxLeaves(home)

	for _, path := range []string{modCache, buildCache} {
		spec, ok := findSpec(specs, path)
		if !ok {
			t.Fatalf("StandardSandboxMounts emits no entry for %s (issue #2731)", path)
		}
		if spec.SandboxPath != path {
			t.Errorf("%s: SandboxPath = %q; want %q (Dst==Src under bwrap)", path, spec.SandboxPath, path)
		}
		if spec.ReadOnly {
			t.Errorf("%s: must be read-write — go writes module downloads, its lock file, and build outputs", path)
		}
		if !spec.OptionalIfMissing {
			t.Errorf("%s: must be OptionalIfMissing — bwrap ABORTS on a missing bind source (#2243)", path)
		}
		if spec.EvalSymlinks {
			t.Errorf("%s: EvalSymlinks pins an inode; the caches are plain directories", path)
		}
	}
}

// TestStandardSandboxMounts_GoCacheDerivedFromSharedList asserts the mount
// entries are generated from goCacheDirsForGOOS rather than from a second
// hand-written list (no duplication). Changing the shared
// list must move the mounts with it.
func TestStandardSandboxMounts_GoCacheDerivedFromSharedList(t *testing.T) {
	const home = "/home/example"
	specs := StandardSandboxMounts(Config{}, home, home, isolationBwrap)

	for _, dir := range goCacheDirsForGOOS(home, goosLinux) {
		if _, ok := findSpec(specs, dir.path); !ok {
			t.Errorf("shared list entry %s has no matching mount spec — the two sites have drifted", dir.path)
		}
	}
}

// TestStandardSandboxMounts_GoCacheNoParentGrantedWholesale is the [security]
// AC. The grant must be the two LEAF cache directories and nothing above
// them: ~/go also holds ~/go/bin, where `go install` drops binaries that are
// typically on the host PATH, and ~/.cache is the user's whole cache tree.
// (subpath ~/Library/Caches) is rejected on Darwin for exactly this
// reason; the Linux grant is held to the same line.
//
// The assertion sweeps EVERY spec in the walk, not just the Go entries, so it
// also fails if some unrelated future entry widens to one of these parents.
func TestStandardSandboxMounts_GoCacheNoParentGrantedWholesale(t *testing.T) {
	const home = "/home/example"
	specs := StandardSandboxMounts(Config{}, home, home, isolationBwrap)

	forbidden := map[string]string{
		filepath.Join(home, "go"):                "~/go holds ~/go/bin — a sandboxed agent must not be able to plant an executable the user later runs",
		filepath.Join(home, "go", "pkg"):         "~/go/pkg is wider than the module cache leaf",
		filepath.Join(home, ".cache"):            "~/.cache is the user's whole cache tree",
		filepath.Join(home, "Library", "Caches"): "the Darwin cache tree — rejected as too broad in #2621",
		home:                                     "$HOME wholesale",
		"/":                                      "the host root",
	}
	for _, spec := range specs {
		if why, bad := forbidden[spec.HostPath]; bad {
			t.Errorf("mount grants %q wholesale: %s", spec.HostPath, why)
		}
		if why, bad := forbidden[spec.SandboxPath]; bad {
			t.Errorf("mount exposes %q wholesale inside the sandbox: %s", spec.SandboxPath, why)
		}
	}
}

// TestStandardSandboxMounts_GoCacheSkippedWithoutSandboxHome covers the
// degenerate call shape: with no in-sandbox HOME there is nowhere to mount
// the caches, so no entry may be emitted. Without the guard the entries
// would land at "/go/pkg/mod" and "/.cache/go-build" — a host cache exposed
// at the sandbox root. Mirrors the usage-dir guard.
func TestStandardSandboxMounts_GoCacheSkippedWithoutSandboxHome(t *testing.T) {
	hostHome := t.TempDir()
	for _, spec := range StandardSandboxMounts(Config{}, "", hostHome, isolationBwrap) {
		if spec.HostPath == filepath.Join(hostHome, "go", "pkg", "mod") ||
			spec.HostPath == filepath.Join(hostHome, ".cache", "go-build") {
			t.Errorf("Go cache entry emitted with no in-sandbox HOME: %+v", spec)
		}
	}
}

// ── emitted bwrap argv (mounts.go + bwrap.go) ────────────────────────────────

// TestBwrapBuildArgs_GoCacheDirsBound is the argv-level functional AC: with
// both caches present on the host, BuildArgs emits an RW bind for each at
// Dst==Src. This is the mount that makes `go build ./...` reuse the host
// cache instead of rebuilding cold in the sandbox interior.
func TestBwrapBuildArgs_GoCacheDirsBound(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	// The fixture does not pre-create the Go caches — create them so the
	// conditional mounts fire. (In production prepareVolumeDirs does this.)
	modCache, buildCache := goCacheLinuxLeaves(fakeHome)
	for _, dir := range []string{modCache, buildCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", dir, err)
		}
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, dir := range []string{modCache, buildCache} {
		if !hasBind(args, dir) {
			t.Errorf("%q not found as --bind SRC SRC in args: %v", dir, redactedArgs(args))
		}
		if hasROBind(args, dir) {
			t.Errorf("%q must be RW (--bind), not RO (--ro-bind): %v", dir, redactedArgs(args))
		}
	}
}

// TestBwrapBuildArgs_GoCacheDirsAbsentNoBind is the [edge-case] AC: bwrap
// ABORTS at startup on a missing --bind source, so a host
// that has never run Go must produce no Go cache bind at all. Without the
// OptionalIfMissing guard this change would break EVERY sandbox on such a
// machine, not just Go builds.
func TestBwrapBuildArgs_GoCacheDirsAbsentNoBind(t *testing.T) {
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	modCache, buildCache := goCacheLinuxLeaves(fakeHome)
	for _, dir := range []string{modCache, buildCache} {
		if _, err := os.Stat(dir); err == nil {
			t.Fatalf("fixture unexpectedly created %q — update this test", dir)
		}
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	for _, dir := range []string{modCache, buildCache} {
		if hasBind(args, dir) {
			t.Errorf("missing %q must be omitted but was found as --bind: %v", dir, redactedArgs(args))
		}
		if hasROBind(args, dir) {
			t.Errorf("missing %q must be omitted but was found as --ro-bind: %v", dir, redactedArgs(args))
		}
	}
}

// ── host-side pre-creation (container.go) ────────────────────────────────────

// TestPrepareVolumeDirs_CreatesGoCacheDirs pins the other half of the
// edge-case AC. OptionalIfMissing alone would leave a fresh machine cold
// FOREVER: the mount is skipped, go builds into the sandbox interior, the
// host directories are never created, and the next session skips the mount
// too. Pre-creating them host-side is what makes the first session warm the
// cache for the second.
func TestPrepareVolumeDirs_CreatesGoCacheDirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs(false): %v", err)
	}

	modCache, buildCache := goCacheLinuxLeaves(fakeHome)
	for _, dir := range []string{modCache, buildCache} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("expected Go cache dir to exist: %s (err: %v)", dir, err)
		}
	}
}

// TestPrepareVolumeDirs_GoCacheCreationFailureIsNotFatal keeps the caches in
// the "build convenience" class rather than the "session cannot start" class.
// The nix build sandbox runs with HOME=/homeless-shelter (unwritable), and a
// session must still start there — without the mounts.
func TestPrepareVolumeDirs_GoCacheCreationFailureIsNotFatal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: cannot make an unwritable directory")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	if err := os.Chmod(fakeHome, 0o555); err != nil {
		t.Fatalf("setup: chmod %q: %v", fakeHome, err)
	}
	t.Cleanup(func() { _ = os.Chmod(fakeHome, 0o755) })

	m := New(Config{SessionName: "repo@feat", AllocatedPort: 14000})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Errorf("prepareVolumeDirs must not fail when the Go caches cannot be created: %v", err)
	}

	modCache, _ := goCacheLinuxLeaves(fakeHome)
	if _, err := os.Stat(modCache); err == nil {
		t.Fatalf("setup did not make %q unwritable — the test proves nothing", fakeHome)
	}
}
