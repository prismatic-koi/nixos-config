//go:build linux

package container

// go_cache_mount_bwrap_test.go — Linux-only behavioural coverage for the Go
// module and build cache binds (issue #2731).
//
// The sibling unit tests in go_cache_mounts_test.go assert the emitted argv
// and the mount specs. Argv assertions are necessary but not sufficient: the
// acceptance criteria are about what a sandboxed `go build` can DO, and a
// bind that emits the right triple can still fail to deliver a warm cache.
// These tests spawn REAL bwrap sandboxes through the production emitter
// (StandardSandboxMounts + AppendBwrapBind) and measure the behaviour the
// ACs name:
//
//   - both caches are read-WRITE inside the sandbox and writes persist to
//     the host (that persistence IS the warm cache);
//   - a second sandbox reuses the cache the first one populated instead of
//     rebuilding cold;
//   - without the binds neither holds — the no-op proof that the positive
//     tests are not green by accident;
//   - a host with no Go caches yet still starts a sandbox (#2243: bwrap
//     ABORTS on a missing bind source), while a hand-written unconditional
//     bind of the same missing path does abort — so the OptionalIfMissing
//     guard is load-bearing, not decorative.
//
// The sandbox is deliberately minimal and does NOT bind the host root: the
// Go cache binds are the only route to those directories, so a passing
// positive test can only be the rule under test.
//
// Skips (never fails) when bwrap is absent or cannot create user namespaces
// — GitHub Actions ubuntu runners (#1510) and the nix build sandbox both
// land there. The bwrap probe, the coreutils resolver and the shell quoter
// are shared with usage_mount_bwrap_test.go rather than duplicated; they are
// generic bwrap helpers despite their usage-test names.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// goCacheHome creates a fake host home with both Go cache directories
// pre-created, exactly as prepareVolumeDirs does on a real host, and returns
// (hostHome, modCache, buildCache).
func goCacheHome(t *testing.T) (hostHome, modCache, buildCache string) {
	t.Helper()
	hostHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	modCache = filepath.Join(hostHome, "go", "pkg", "mod")
	buildCache = filepath.Join(hostHome, ".cache", "go-build")
	for _, dir := range []string{modCache, buildCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", dir, err)
		}
	}
	return hostHome, modCache, buildCache
}

// goCacheMountArgsForTest returns the bwrap bind triples the PRODUCTION
// emitter generates for the two Go caches, and nothing else. Building them
// through StandardSandboxMounts + AppendBwrapBind (rather than hand-writing
// "--bind src dst") is what makes these tests cover the shipped code.
//
// requireEmitted=false is used by the missing-source test, where emitting
// nothing is the correct outcome.
func goCacheMountArgsForTest(t *testing.T, hostHome string, requireEmitted bool) []string {
	t.Helper()
	want := map[string]bool{}
	for _, dir := range goCacheDirsForGOOS(hostHome, goosLinux) {
		want[dir.path] = true
	}
	if len(want) == 0 {
		t.Fatal("goCacheDirsForGOOS returned no entries for a non-empty home")
	}

	var args []string
	for _, spec := range StandardSandboxMounts(Config{}, hostHome, hostHome, isolationBwrap) {
		if want[spec.HostPath] {
			args = AppendBwrapBind(args, spec)
		}
	}
	if requireEmitted && len(args) == 0 {
		t.Fatal("StandardSandboxMounts emitted no Go cache bind — the mount under test is missing")
	}
	return args
}

// runInBwrapWithPath runs script under a minimal bwrap sandbox with the
// supplied extra mount arguments, HOME pointed at hostHome and PATH set to
// pathDirs, and returns the combined output and the run error.
//
// --clearenv comes FIRST, and is load-bearing rather than cosmetic. bwrap
// applies arguments left to right, so it must precede every --setenv or the
// values below are wiped. It also makes the run hermetic: without it the
// developer's environment leaks in, and an exported GOCACHE or GOMODCACHE
// then steers the in-sandbox toolchain away from the HOME-derived paths
// these tests mount — the warm-cache test would fail on a host that exports
// either. Production does the same thing for the same reason
// (standardSandboxEnvArgs re-adds a fixed set after --clearenv).
func runInBwrapWithPath(t *testing.T, bwrapBin, hostHome string, pathDirs []string, mountArgs []string, script string) (string, error) {
	t.Helper()
	args := []string{
		"--clearenv",
		"--ro-bind", "/nix", "/nix",
		"--ro-bind", "/bin", "/bin",
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-pid",
		"--die-with-parent",
		"--setenv", "PATH", strings.Join(pathDirs, ":"),
		"--setenv", "HOME", hostHome,
	}
	args = append(args, mountArgs...)
	args = append(args, "/bin/sh", "-c", script)
	out, err := exec.Command(bwrapBin, args...).CombinedOutput()
	return string(out), err
}

// TestBwrapGoCacheDirs_WritableInsideSandbox is the functional AC: both Go
// caches are available read-write at the same paths the host uses. The write
// must also PERSIST to the host — a cache that dies with the sandbox
// interior is exactly the cold-cache state #2731 exists to end.
func TestBwrapGoCacheDirs_WritableInsideSandbox(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostHome, modCache, buildCache := goCacheHome(t)
	coreutils := nixCoreutilsBinDir(t)

	for _, dir := range []string{modCache, buildCache} {
		planted := filepath.Join(dir, "written-inside")
		out, err := runInBwrapWithPath(t, bwrapBin, hostHome, []string{coreutils},
			goCacheMountArgsForTest(t, hostHome, true),
			"echo from-sandbox > "+shQuoteForTest(planted))
		if err != nil {
			t.Fatalf("writing to %s inside the sandbox failed: %v — %s", planted, err, out)
		}
		got, readErr := os.ReadFile(planted)
		if readErr != nil {
			t.Fatalf("the in-sandbox write to %s did not reach the host: %v", planted, readErr)
		}
		if strings.TrimSpace(string(got)) != "from-sandbox" {
			t.Errorf("host content of %s = %q; want %q", planted, string(got), "from-sandbox")
		}
	}
}

// TestBwrapGoCacheDirs_UnreachableWithoutBind is the paired no-op proof.
// Without the Go cache binds the same paths must be unreachable: a host file
// is not readable, and an in-sandbox write does not reach the host. Without
// this, every positive test above could be green because some other mount
// happened to expose $HOME.
func TestBwrapGoCacheDirs_UnreachableWithoutBind(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	hostHome, modCache, buildCache := goCacheHome(t)
	coreutils := nixCoreutilsBinDir(t)

	for _, dir := range []string{modCache, buildCache} {
		hostFile := filepath.Join(dir, "host-only")
		if err := os.WriteFile(hostFile, []byte("host-secret"), 0o600); err != nil {
			t.Fatalf("WriteFile %q: %v", hostFile, err)
		}

		// No mount args at all — the deliberate mutation.
		out, err := runInBwrapWithPath(t, bwrapBin, hostHome, []string{coreutils}, nil,
			"cat "+shQuoteForTest(hostFile))
		if err == nil {
			t.Errorf("read of %s succeeded WITHOUT the Go cache bind — some other mount exposes the path,\n"+
				"so the positive tests are not isolating the mount under test.\nOutput: %s", hostFile, out)
		}
		if strings.Contains(out, "host-secret") {
			t.Errorf("host cache content leaked into a sandbox with no Go cache bind: %s", out)
		}

		// A write with no bind must stay in the ephemeral sandbox interior.
		stray := filepath.Join(dir, "stray")
		if _, err := runInBwrapWithPath(t, bwrapBin, hostHome, []string{coreutils}, nil,
			"mkdir -p "+shQuoteForTest(dir)+" && echo stray > "+shQuoteForTest(stray)); err != nil {
			// A failure here is fine too — the point is only that nothing
			// reaches the host.
			t.Logf("in-sandbox write without a bind failed (acceptable): %v", err)
		}
		if _, err := os.Stat(stray); err == nil {
			t.Errorf("%s exists on the host — a sandbox with no Go cache bind wrote through to it", stray)
		}
	}
}

// TestBwrapGoCacheDirs_MissingSourceStartsAndBindsAborts is the [edge-case]
// AC (#2243). Two halves, and both are needed:
//
//  1. On a host where neither cache exists, the production emitter produces
//     NO Go cache bind and the sandbox starts normally. This is the case
//     that would otherwise break EVERY session on a machine that has never
//     run Go — not just Go builds.
//  2. An unconditional bind of the same missing path DOES abort bwrap. This
//     is the mutation proof: it shows half 1 passes because of the
//     OptionalIfMissing guard, not because bwrap tolerates missing sources.
func TestBwrapGoCacheDirs_MissingSourceStartsAndBindsAborts(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	coreutils := nixCoreutilsBinDir(t)

	// A fresh host: home exists, neither Go cache does.
	hostHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	modCache := filepath.Join(hostHome, "go", "pkg", "mod")

	mountArgs := goCacheMountArgsForTest(t, hostHome, false)
	if len(mountArgs) != 0 {
		t.Fatalf("Go cache binds emitted for a host with no caches: %v — bwrap would abort at startup (#2243)", mountArgs)
	}

	out, err := runInBwrapWithPath(t, bwrapBin, hostHome, []string{coreutils}, mountArgs, "echo started")
	if err != nil {
		t.Fatalf("the sandbox failed to start on a host with no Go caches: %v — %s", err, out)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("sandbox produced no output on a host with no Go caches: %q", out)
	}

	// The mutation: bind the missing path unconditionally, as an entry
	// WITHOUT OptionalIfMissing would.
	out, err = runInBwrapWithPath(t, bwrapBin, hostHome, []string{coreutils},
		[]string{"--bind", modCache, modCache}, "echo started")
	if err == nil {
		t.Errorf("bwrap started with an unconditional --bind of the missing %s (output %q) —\n"+
			"the OptionalIfMissing guard would then be untested by the half above", modCache, out)
	}
}

// TestBwrapGoCacheDirs_SecondSandboxReusesWarmCache is the second functional
// AC: a `go build` in a NEW sandbox reuses the cache the previous one
// populated instead of rebuilding from cold.
//
// The measurement is a compile COUNT, not a wall-clock time: `go build -x`
// prints every tool invocation it runs, so a cold build shows dozens of
// `compile` lines and a fully cached build shows none. That signal is
// deterministic under load, where a timing threshold is not. The wall-clock
// figures are in the PR description.
func TestBwrapGoCacheDirs_SecondSandboxReusesWarmCache(t *testing.T) {
	bwrapBin := requireUsableBwrapForUsageTest(t)
	coreutils := nixCoreutilsBinDir(t)
	goBin := nixGoBinDirForTest(t)
	hostHome, _, _ := goCacheHome(t)

	// A module with no external dependencies: the reuse under test is the
	// build cache, and no network is needed. The go directive stays below
	// the toolchain in the nix store so nothing triggers a toolchain
	// download.
	srcDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", name, err)
		}
	}
	write("go.mod", "module example.test/warm\n\ngo 1.21\n")
	write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n")

	build := func(label string) int {
		t.Helper()
		mountArgs := goCacheMountArgsForTest(t, hostHome, true)
		mountArgs = append(mountArgs, "--bind", srcDir, srcDir)
		out, err := runInBwrapWithPath(t, bwrapBin, hostHome, []string{goBin, coreutils}, mountArgs,
			"cd "+shQuoteForTest(srcDir)+" && go build -x -o /dev/null .")
		if err != nil {
			t.Fatalf("%s build failed inside the sandbox: %v — %s", label, err, out)
		}
		return strings.Count(out, "compile")
	}

	cold := build("cold")
	if cold < 5 {
		t.Fatalf("the cold build ran only %d compile step(s) — the cache was not cold, so the comparison below proves nothing", cold)
	}

	warm := build("warm")
	if warm >= cold {
		t.Errorf("a second sandbox rebuilt from cold: %d compile step(s) after a warm run of %d.\n"+
			"The Go build cache is not being shared with the host (#2731).", warm, cold)
	}
	// A genuinely warm build recompiles nothing; allow a small margin so an
	// unrelated toolchain detail cannot make this flaky.
	if warm > cold/10 {
		t.Errorf("the second sandbox reused little of the cache: %d compile step(s) versus %d cold", warm, cold)
	}
	t.Logf("compile steps: cold=%d warm=%d", cold, warm)
}

// nixGoBinDirForTest returns the /nix/store bin directory holding the go
// toolchain, so the in-sandbox PATH can reach it with only /nix bound.
// Skips when go is absent or is not a Nix store deployment.
func nixGoBinDirForTest(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not found in PATH")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", p, err)
	}
	if !strings.HasPrefix(real, "/nix/store/") {
		t.Skipf("go resolves to %q, not a /nix/store path — skipping", real)
	}
	return filepath.Dir(real)
}
