// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
//
// This file is the single, platform-aware source of truth for the pair of Go
// cache directories a sandbox shares with the host. BOTH isolators derive
// their paths from goCacheDirsForGOOS here, through mechanisms that are
// structurally different (see the mounts.go package comment):
//
//   - sandbox-exec (Darwin) — SBPL "(subpath ...)" grants
//     emitted by generateProfile section 5k, plus host-side creation in
//     sandboxExecIsolator.Prepare via ensureGoCacheDirs.
//   - bwrap (Linux) — MountSpec entries returned by
//     StandardSandboxMounts and translated to "--bind SRC DST" by
//     AppendBwrapBind, plus host-side creation in prepareVolumeDirs.
//
// One list, four consumers. The grant and the directory creation cannot
// drift apart, and neither can the two platforms. Keep this the single
// source of truth: do not add a second list for either platform.
package container

import (
	"log"
	"os"
	"path/filepath"
)

// The two GOOS values prism supports. They are named constants rather than
// runtime.GOOS reads because each isolator's platform is known statically:
// bwrap is Linux-only and sandbox-exec is Darwin-only
// (config.ValidIsolationModes). Pinning at the call site rather than reading
// runtime.GOOS also keeps both emitters deterministic and testable from
// either host — the Darwin profile assertions run in Linux CI, and the bwrap
// argv assertions run on m4mac.
const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

// goCacheDir describes one Go cache directory the sandbox grants read-write,
// together with whether the sandboxed process must also be able to map code
// out of it.
type goCacheDir struct {
	// path is the absolute host path of the cache directory.
	path string

	// execDenied emits an explicit (deny process-exec* file-map-executable)
	// for this directory in the final deny section of the Darwin SBPL
	// profile.
	//
	// DARWIN ONLY. bwrap has no per-bind exec control — there is no
	// "--bind-noexec" and no noexec remount flag in its argument grammar —
	// so the Linux mount path in StandardSandboxMounts reads this field for
	// documentation value only and cannot enforce it. See the Go-cache block
	// in mounts.go for why that asymmetry is accepted.
	//
	// True for GOMODCACHE. The module cache holds module SOURCE, and with the
	// GOTOOLCHAIN pin below nothing in the documented gate execs out of it, so
	// a sandboxed process must not be able to run anything it plants among the
	// dependency sources.
	//
	// COUPLING — read GoToolchainEnv before touching this. The "nothing execs
	// from the module cache" premise is TRUE ONLY BECAUSE prism injects
	// GOTOOLCHAIN=local. Under Go's default GOTOOLCHAIN=auto, cmd/go downloads
	// a newer toolchain INTO the module cache and execs <dir>/bin/go from
	// there, which this deny would block. The two changes are one mechanism:
	// remove the env pin and this deny breaks `go build` on any repo whose
	// go.mod outgrows the nix-pinned toolchain.
	//
	// The deny is load-bearing rather than decorative, and the ABSENCE of a
	// file-map-executable grant does NOT achieve the same thing. Section 9
	// emits (allow process-exec* ...) with NO path filter, so execution is
	// permitted profile-wide and no section-5k grant governs it either way.
	// Testing on a host proved this empirically: a planted binary ran
	// from BOTH cache dirs, and it ran from GOCACHE even with the whole
	// section-5k block stripped. Withholding a flag from an allow clause
	// cannot narrow a capability that a later unqualified allow hands out.
	// Only an explicit deny does.
	//
	// False for GOCACHE. cmd/go can serve a linked test binary straight out
	// of the build cache on a warm build, so execution there must keep
	// working — denying it risks breaking the very gate this section exists
	// to enable.
	execDenied bool
}

// goCacheDirsForGOOS returns the Go cache directories a sandbox shares with
// the host on the named platform, derived from the given home directory.
// It returns nil when home is empty, and nil for any GOOS other than the two
// prism supports (fail closed: no home or no known platform means no grant).
//
// The entries are the Go toolchain's per-platform DEFAULTS:
//
//	<home>/go/pkg/mod                 GOMODCACHE — GOPATH=<home>/go on both
//	                                  platforms, so this path is shared.
//	<home>/Library/Caches/go-build    GOCACHE on darwin — os.UserCacheDir()
//	                                  is <home>/Library/Caches there.
//	<home>/.cache/go-build            GOCACHE on linux — os.UserCacheDir()
//	                                  is <home>/.cache there.
//
// Hardcoding the defaults is exact rather than approximate, and deliberately
// does NOT read the host's GOPATH / GOMODCACHE / GOCACHE / GOENV:
//
//   - Both sandboxes build their environment explicitly and forward no GO*
//     variable (bwrap runs --clearenv and re-adds a fixed set;
//     buildSandboxExecHomeEnv does the same on Darwin), and go's env file is
//     not granted, so the in-sandbox toolchain cannot resolve anything else.
//   - Reading a host GO* variable here would be a widening vector: a host
//     variable would then steer a sandbox grant.
//
// os.UserCacheDir() is likewise not called: it reads $XDG_CACHE_HOME, which
// is a host environment variable, and it resolves against the CALLING
// process's home rather than the home argument. Both properties are wrong
// here for the same reason.
func goCacheDirsForGOOS(home, goos string) []goCacheDir {
	if home == "" {
		return nil
	}
	var buildCache string
	switch goos {
	case goosDarwin:
		buildCache = filepath.Join(home, "Library", "Caches", "go-build")
	case goosLinux:
		buildCache = filepath.Join(home, ".cache", "go-build")
	default:
		return nil
	}
	return []goCacheDir{
		{path: filepath.Join(home, "go", "pkg", "mod"), execDenied: true},
		{path: buildCache},
	}
}

// createGoCacheDirs creates the given Go cache directories on the host,
// best-effort. Shared by both isolators' pre-creation paths
// (ensureGoCacheDirs on Darwin, prepareVolumeDirs on Linux) so the two agree
// on mode and on failure posture. isolator names the caller in the log line
// ("sandbox-exec" or "bwrap") so a failure says which path hit it.
//
// Best-effort by design: a failure is logged, never fatal. The Go caches are
// a build convenience, unlike the work-dir git/ssh configs whose absence
// hard-fails Prepare, and a session must still start where $HOME is
// unwritable (the homeless-shelter nix build sandbox).
//
// Mode 0o755 matches what the go toolchain itself creates (0o777 & umask) —
// these are ordinary user caches shared with the host shell, not per-session
// private state.
func createGoCacheDirs(isolator string, dirs []goCacheDir) {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir.path, 0o755); err != nil {
			log.Printf("container: %s: create go cache dir %s: %v", isolator, dir.path, err)
		}
	}
}
