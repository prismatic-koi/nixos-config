package container

// usage_mount_test.go — unit coverage for the read-only sandbox bind of the
// prism usage snapshot directory (issue #2572).
//
// Background. The bottom-bar usage segment reads
// $XDG_STATE_HOME/prism/usage/current.json
// (pi/extensions/prism.ts::readUsageSnapshot, issue #2540). That directory
// was never bound into the sandbox, and the reader degrades silently on a
// missing file, so the feature rendered nothing in every sandboxed session.
//
// These tests pin the argv/profile shape. The live behaviour — a real read
// through the mount, a denied write, and mid-session visibility of a
// newly-written snapshot — is covered by the bwrap integration tests in
// usage_mount_bwrap_test.go and the sandbox-exec pair in
// internal/integration/sandbox_exec_usage_dir_darwin_test.go.
//
// Isolation: every test sets $XDG_STATE_HOME explicitly (bwrapFixture only
// overrides $HOME), so nothing here depends on the developer's own
// environment and nothing touches the real state directory.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageDirUnderHome is the in-sandbox canonical location of the usage
// directory: $HOME-relative, because bwrap runs with --clearenv and never
// re-adds XDG_STATE_HOME, so both in-sandbox readers fall back to
// $HOME/.local/state.
func usageDirUnderHome(home string) string {
	return filepath.Join(home, ".local", "state", "prism", "usage")
}

// bindArgsForTest renders ONLY the bind triples of a bwrap argv, one per
// element. Failure messages in this file must never dump the whole argv: it
// carries --setenv pairs holding real host credentials (GITHUB_TOKEN,
// OPENROUTER_API_KEY, ...), which a `go test` failure would then print to
// the terminal or a CI log. The bind list is the only part these tests
// diagnose against anyway.
func bindArgsForTest(args []string) []string {
	var out []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" || args[i] == "--ro-bind" {
			out = append(out, args[i]+" "+args[i+1]+" "+args[i+2])
		}
	}
	return out
}

// TestBwrapBuildArgs_UsageStateDirROBound is the functional AC: with the
// usage directory present on the host it is bound into the sandbox, and it
// is bound READ-ONLY.
func TestBwrapBuildArgs_UsageStateDirROBound(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	usageDir := usageDirUnderHome(fakeHome)
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll usage dir: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasROBind(args, usageDir) {
		t.Errorf("usage dir %q not found as --ro-bind SRC SRC in binds: %v", usageDir, bindArgsForTest(args))
	}
	// RO must not silently become RW: every writer goes through the sidecar
	// endpoint POST /usage/snapshot, and a writable mount would let a
	// compromised session forge usage figures on the host.
	if hasBind(args, usageDir) {
		t.Errorf("usage dir %q must be RO (--ro-bind), not RW (--bind): %v", usageDir, bindArgsForTest(args))
	}
}

// TestBwrapBuildArgs_UsageStateDirAbsentNoBind covers the edge-case AC: a
// session started before the host ever captured a snapshot must start
// normally. bwrap ABORTS on a missing bind source, so the entry must be
// omitted entirely rather than emitted against a non-existent path.
//
// In production prepareVolumeDirs pre-creates the directory, so this path
// only fires when that creation failed (it is non-fatal by design — e.g. an
// unwritable HOME in the nix build sandbox).
func TestBwrapBuildArgs_UsageStateDirAbsentNoBind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	usageDir := usageDirUnderHome(fakeHome)
	if _, err := os.Stat(usageDir); err == nil {
		t.Fatalf("fixture unexpectedly created %q — update this test", usageDir)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if hasROBind(args, usageDir) {
		t.Errorf("missing usage dir should be omitted but found as --ro-bind in binds: %v", bindArgsForTest(args))
	}
	if hasBind(args, usageDir) {
		t.Errorf("missing usage dir should be omitted but found as --bind in binds: %v", bindArgsForTest(args))
	}
}

// TestBwrapBuildArgs_UsageStateDirHonoursXDGStateHome is the AC that the
// bind resolves $XDG_STATE_HOME the same way pi/extensions/prism.ts does
// rather than assuming ~/.local/state.
//
// The SOURCE follows the host's $XDG_STATE_HOME (that is where the sidecar
// actually writes the snapshots). The DESTINATION stays $HOME-relative
// because bwrap's --clearenv leaves XDG_STATE_HOME unset inside the sandbox,
// so the in-sandbox reader looks under $HOME/.local/state whatever the host
// exported. Src != Dst is therefore load-bearing here.
func TestBwrapBuildArgs_UsageStateDirHonoursXDGStateHome(t *testing.T) {
	stateHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)

	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	xdgUsageDir := filepath.Join(stateHome, "prism", "usage")
	if err := os.MkdirAll(xdgUsageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll xdg usage dir: %v", err)
	}
	// Plant the hardcoded ~/.local/state location too, so the test fails if
	// the implementation ignores XDG_STATE_HOME and picks the home-relative
	// path as the SOURCE by accident rather than because it does not exist.
	homeUsageDir := usageDirUnderHome(fakeHome)
	if err := os.MkdirAll(homeUsageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll home usage dir: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	if !hasROBindSrcDst(args, xdgUsageDir, homeUsageDir) {
		t.Errorf("want --ro-bind %q %q (XDG source remapped to the canonical in-sandbox path); binds: %v",
			xdgUsageDir, homeUsageDir, bindArgsForTest(args))
	}
	if hasROBind(args, homeUsageDir) {
		t.Errorf("with $XDG_STATE_HOME set the SOURCE must be %q, not the hardcoded %q; binds: %v",
			xdgUsageDir, homeUsageDir, bindArgsForTest(args))
	}
	if hasBind(args, xdgUsageDir) || hasBindSrcDstForTest(args, xdgUsageDir, homeUsageDir) {
		t.Errorf("usage dir must be RO (--ro-bind), not RW (--bind): %v", bindArgsForTest(args))
	}
}

// hasBindSrcDstForTest reports whether args contains --bind src dst (the RW
// flag) for a possibly-remapped pair. hasBind only matches Dst==Src.
func hasBindSrcDstForTest(args []string, src, dst string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == src && args[i+2] == dst {
			return true
		}
	}
	return false
}

// TestBwrapBuildArgs_UsageStateDirNoAncestorExposed is the security AC: no
// path outside the usage directory becomes readable because of this bind.
//
// The dangerous shape is a bind of a PARENT. $XDG_STATE_HOME/prism holds
// prism.db (the whole session database) and run/ (every session's host-API
// socket dir, isolated per session by security fix #960); $XDG_STATE_HOME
// itself holds unrelated application state. This test walks every emitted
// bind triple and fails if any source or destination is an ancestor of the
// usage directory.
func TestBwrapBuildArgs_UsageStateDirNoAncestorExposed(t *testing.T) {
	stateHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)

	m, fakeHome, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	xdgUsageDir := filepath.Join(stateHome, "prism", "usage")
	if err := os.MkdirAll(xdgUsageDir, 0o700); err != nil {
		t.Fatalf("MkdirAll xdg usage dir: %v", err)
	}
	// Plant a sibling secret under the parent. If a future change widens the
	// bind to the parent, the ancestor assertion below catches it.
	if err := os.WriteFile(filepath.Join(stateHome, "prism", "prism.db"), []byte("fake"), 0o600); err != nil {
		t.Fatalf("WriteFile fake prism.db: %v", err)
	}

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	// Every strict ancestor of the usage dir, on both the host side and the
	// in-sandbox side.
	forbidden := map[string]bool{}
	for _, leaf := range []string{xdgUsageDir, usageDirUnderHome(fakeHome)} {
		for p := filepath.Dir(leaf); ; p = filepath.Dir(p) {
			forbidden[p] = true
			if p == filepath.Dir(p) {
				break
			}
		}
	}

	for _, flag := range []string{"--bind", "--ro-bind"} {
		for _, triple := range findTriples(args, flag) {
			for _, p := range []string{triple[0], triple[1]} {
				if forbidden[p] {
					t.Errorf("%s exposes %q, an ancestor of the usage dir — bind the LEAF only; binds: %v",
						flag, p, bindArgsForTest(args))
				}
			}
		}
	}
}

// TestStandardSandboxMounts_UsageStateDirSpecShape asserts the mode-agnostic
// spec itself, independently of the bwrap appender: read-only, optional, and
// scoped to the leaf directory on both sides.
func TestStandardSandboxMounts_UsageStateDirSpecShape(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	hostHome := "/tmp/host-home-fixture"

	var found *MountSpec
	specs := StandardSandboxMounts(Config{}, hostHome, hostHome, isolationBwrap)
	for i := range specs {
		if strings.HasSuffix(specs[i].HostPath, filepath.Join("prism", "usage")) {
			found = &specs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("StandardSandboxMounts emits no entry for the prism usage dir (issue #2572)")
	}
	want := filepath.Join(hostHome, ".local", "state", "prism", "usage")
	if found.HostPath != want {
		t.Errorf("HostPath = %q, want %q", found.HostPath, want)
	}
	if found.SandboxPath != want {
		t.Errorf("SandboxPath = %q, want %q", found.SandboxPath, want)
	}
	if !found.ReadOnly {
		t.Error("usage dir spec must be ReadOnly — writes route through the sidecar endpoint")
	}
	if !found.OptionalIfMissing {
		t.Error("usage dir spec must be OptionalIfMissing — bwrap aborts on missing bind sources")
	}
	if found.EvalSymlinks {
		t.Error("usage dir spec must not set EvalSymlinks — the path is a plain host directory, not a sops symlink")
	}
}

// TestStandardSandboxMounts_UsageStateDirSkippedWithoutHome covers the
// caller contract of usage.DirForHome: with neither $XDG_STATE_HOME nor a
// resolvable home there is nothing to mount, and the entry must be dropped
// rather than emitted rooted at "/".
func TestStandardSandboxMounts_UsageStateDirSkippedWithoutHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	for _, spec := range StandardSandboxMounts(Config{}, "", "", isolationBwrap) {
		if strings.Contains(spec.HostPath, filepath.Join("prism", "usage")) ||
			strings.Contains(spec.SandboxPath, filepath.Join("prism", "usage")) {
			t.Errorf("usage dir entry must be skipped when no home is resolvable, got %+v", spec)
		}
	}
}

// TestPrepareVolumeDirs_CreatesUsageDir pins the host-side pre-creation
// (issue #2572). Without it a session spawned before the first snapshot
// capture gets no mount at all (OptionalIfMissing), and the bottom bar stays
// blank for the whole life of that session even after the host writes a
// snapshot.
func TestPrepareVolumeDirs_CreatesUsageDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@main", Worktree: t.TempDir()})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs: %v", err)
	}

	usageDir := usageDirUnderHome(fakeHome)
	info, err := os.Stat(usageDir)
	if err != nil {
		t.Fatalf("usage dir %q was not pre-created: %v", usageDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", usageDir)
	}
	// 0700 matches internal/usage's own dirMode — the snapshots are the
	// user's account rate-limit figures; no other host user needs them.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("usage dir mode = %04o, want 0700", perm)
	}
}

// TestPrepareVolumeDirs_UsageDirCreationFailureIsNotFatal covers the
// edge-case AC: a session must start normally when the usage directory
// cannot be created. This is also the homeless-shelter case — the nix build
// sandbox runs with HOME=/homeless-shelter, which is deliberately
// unwritable.
func TestPrepareVolumeDirs_UsageDirCreationFailureIsNotFatal(t *testing.T) {
	// A read-only parent makes MkdirAll fail with EACCES.
	root := t.TempDir()
	fakeHome := filepath.Join(root, "home")
	if err := os.MkdirAll(fakeHome, 0o500); err != nil {
		t.Fatalf("MkdirAll read-only home: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(fakeHome, 0o700) })

	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", fakeHome)

	m := New(Config{SessionName: "repo@main", Worktree: t.TempDir()})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs must not fail when the usage dir cannot be created: %v", err)
	}
	if _, err := os.Stat(usageDirUnderHome(fakeHome)); err == nil {
		t.Skip("usage dir was created despite the read-only parent (running as root?) — nothing to assert")
	}
}
