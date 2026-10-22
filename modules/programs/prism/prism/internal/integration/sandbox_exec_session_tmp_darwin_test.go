//go:build darwin

package integration_test

// sandbox_exec_session_tmp_darwin_test.go — the per-session TMPDIR of the
// sandbox-exec profile (sections 3 and 21e of generateProfile, #3150).
//
// Each session has its own TMPDIR, /var/tmp/prism-<hash of the instance ID>.
// The session can write files and bind and connect Unix sockets there. It
// cannot create, rename, or delete a file in /private/tmp, /tmp, or the user
// $TMPDIR, and it cannot read, write, or connect in the TMPDIR of another
// session. It cannot rename or remove the node of its own TMPDIR, and it
// cannot change the mode or the flags of that node. It can still chmod and
// chflags a file inside its TMPDIR.
//
// The tests use the connect probe of
// sandbox_exec_container_socket_darwin_test.go and the probe extras
// (augmentProfileForProbe), which add no /tmp grant. Each positive test has
// a paired negative that mutates the profile and gets the opposite result.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// expectOp runs a probe file operation and checks that it succeeds (wantOK)
// or fails.
func expectOp(t *testing.T, profilePath, probe string, wantOK bool, why string, probeArgs ...string) {
	t.Helper()
	out, err := runProbe(t, profilePath, probe, probeArgs...)
	ok := err == nil && strings.Contains(out, opOKMarker)
	if ok != wantOK {
		t.Errorf("probe %q: ok=%v, want %v: %s.\nExit: %v\nOutput: %s\nProfile: %s", probeArgs, ok, wantOK, why, err, out, profilePath)
	}
}

// longSocketName is a 64-byte socket name. A path of the form
// <TMPDIR>/<longSocketName> is 92 bytes in the /var/tmp form, inside the 104-byte
// Darwin sun_path limit, and has the depth of a go test socket below a
// t.TempDir()-style directory.
var longSocketName = strings.Repeat("s", 59) + ".sock"

// The chflags(2) flags that the tests use, from <sys/stat.h>.
const (
	ufNodump    = 0x1 // nodump
	ufImmutable = 0x2 // uchg
)

// nodeModeAndFlags returns the permission bits and the chflags flags of
// path, read outside the sandbox.
func nodeModeAndFlags(t *testing.T, path string) (os.FileMode, uint32) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm(), info.Sys().(*syscall.Stat_t).Flags
}

// clearFlagsAtCleanup clears every chflags flag of path at cleanup, so that
// the removal of the TMPDIR works when a probe set uchg. Register it after
// the profile helper, so that it runs before the removal.
func clearFlagsAtCleanup(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Chflags(path, 0) })
}

// TestSandboxExecSessionTmp_UsableUnderProductionProfile: under the
// production profile, the session creates, reads, renames, and removes a
// file in its TMPDIR, and binds and connects a Unix socket there. It also
// creates and removes a dir, and changes the mode and the flags of a file
// in that dir.
func TestSandboxExecSessionTmp_UsableUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	tmpDir := sessionTmpDirOf(t, m)

	f := filepath.Join(tmpDir, "f")
	expectOp(t, profilePath, probe, true, "the session cannot create a file in its TMPDIR", "--create", f)
	expectOp(t, profilePath, probe, true, "the session cannot read a file in its TMPDIR", "--read", f)
	expectOp(t, profilePath, probe, true, "the session cannot rename a file in its TMPDIR", "--rename", f, f+".2")
	expectOp(t, profilePath, probe, true, "the session cannot remove a file in its TMPDIR", "--remove", f+".2")

	sub := filepath.Join(tmpDir, "d")
	script := filepath.Join(sub, "script.sh")
	expectOp(t, profilePath, probe, true, "the session cannot create a dir in its TMPDIR", "--mkdir", sub)
	expectOp(t, profilePath, probe, true, "the session cannot write a file in a dir in its TMPDIR", "--create", script)
	expectOp(t, profilePath, probe, true, "the session cannot chmod +x a file in its TMPDIR", "--chmod", script, "700")
	expectOp(t, profilePath, probe, true, "the session cannot set a flag on a file in its TMPDIR", "--chflags", script, strconv.Itoa(ufNodump))
	if mode, flags := nodeModeAndFlags(t, script); mode != 0o700 || flags&ufNodump == 0 {
		t.Errorf("%s has mode %v and flags %#x after the chmod and the chflags, want 0700 and nodump", script, mode, flags)
	}
	expectOp(t, profilePath, probe, true, "the session cannot remove a file in a dir in its TMPDIR", "--remove", script)
	expectOp(t, profilePath, probe, true, "the session cannot remove a dir in its TMPDIR", "--remove", sub)

	sock := filepath.Join(tmpDir, longSocketName)
	if out, err := runProbe(t, profilePath, probe, "--bind", sock); err != nil || !strings.Contains(out, connectedMarker) {
		t.Errorf("the session cannot bind and connect the %d-byte socket path %s in its TMPDIR.\nExit: %v\nOutput: %s\nProfile: %s",
			len(sock), sock, err, out, profilePath)
	}
}

// TestSandboxExecSessionTmp_DeniedWithoutGrant is the paired negative. It
// points every TMPDIR rule of the profile at a path that does not exist,
// and the same create and bind then fail.
func TestSandboxExecSessionTmp_DeniedWithoutGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	tmpDir := sessionTmpDirOf(t, m)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return retargetSessionTmp(p, tmpDir, tmpDir+".prism-3150-disabled")
	})

	expectOp(t, mutatedPath, probe, false, "the create still works without the TMPDIR grant, so the positive test proves nothing",
		"--create", filepath.Join(tmpDir, "f"))
	expectOp(t, mutatedPath, probe, false, "the mkdir still works without the TMPDIR grant, so the positive test proves nothing",
		"--mkdir", filepath.Join(tmpDir, "d"))

	existing := filepath.Join(tmpDir, "script.sh")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", existing, err)
	}
	expectOp(t, mutatedPath, probe, false, "the chmod still works without the TMPDIR grant, so the positive test proves nothing",
		"--chmod", existing, "700")
	expectOp(t, mutatedPath, probe, false, "the chflags still works without the TMPDIR grant, so the positive test proves nothing",
		"--chflags", existing, strconv.Itoa(ufNodump))
	if out, err := runProbe(t, mutatedPath, probe, "--bind", filepath.Join(tmpDir, "s.sock")); err == nil {
		t.Errorf("the bind still works without the TMPDIR grant, so the positive test proves nothing.\nOutput: %s\nProfile: %s", out, mutatedPath)
	}
}

// retargetSessionTmp replaces every rule path of the TMPDIR from with to, in
// both /var symlink forms.
func retargetSessionTmp(profile, from, to string) string {
	for _, prefix := range []string{"/private", ""} {
		profile = strings.ReplaceAll(profile, sbplQuoteForTest(prefix+from), sbplQuoteForTest(prefix+to))
	}
	return profile
}

// sharedTmpFixture is one existing file and one new path in a shared temp
// tree.
type sharedTmpFixture struct {
	tree     string
	existing string
	fresh    string
}

// sharedTmpFixtures creates a file in /private/tmp and in the user $TMPDIR,
// and names a new path in /private/tmp, /tmp, and the user $TMPDIR. The test
// process makes them outside the sandbox.
func sharedTmpFixtures(t *testing.T) []sharedTmpFixture {
	t.Helper()
	tag := "prism-3150-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var fx []sharedTmpFixture
	// /private/tmp and /tmp are one dir, so each tree gets its own names.
	for i, tree := range []string{"/private/tmp", "/tmp", filepath.Clean(os.TempDir())} {
		name := tag + "-" + strconv.Itoa(i)
		existing := filepath.Join(tree, name+"-existing")
		if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", existing, err)
		}
		fresh := filepath.Join(tree, name+"-fresh")
		t.Cleanup(func() {
			_ = os.Remove(existing)
			_ = os.Remove(existing + ".renamed")
			_ = os.Remove(fresh)
		})
		fx = append(fx, sharedTmpFixture{tree: tree, existing: existing, fresh: fresh})
	}
	return fx
}

// TestSandboxExecSessionTmp_SharedTmpWriteDenied: under the production
// profile, the session cannot create, rename, or delete a file in
// /private/tmp, /tmp, or the user $TMPDIR, and cannot move a file from
// there into its own TMPDIR.
func TestSandboxExecSessionTmp_SharedTmpWriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	fx := sharedTmpFixtures(t)

	m := newSocketProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	tmpDir := sessionTmpDirOf(t, m)

	for _, f := range fx {
		expectOp(t, profilePath, probe, false, "the session created a file in "+f.tree, "--create", f.fresh)
		expectOp(t, profilePath, probe, false, "the session renamed a file in "+f.tree, "--rename", f.existing, f.existing+".renamed")
		expectOp(t, profilePath, probe, false, "the session moved a file from "+f.tree+" into its TMPDIR", "--rename", f.existing, filepath.Join(tmpDir, "moved"))
		expectOp(t, profilePath, probe, false, "the session removed a file in "+f.tree, "--remove", f.existing)
		if _, err := os.Stat(f.existing); err != nil {
			t.Errorf("the file %s is gone after the denied operations: %v", f.existing, err)
		}
	}
}

// TestSandboxExecSessionTmp_SharedTmpWritableWithGrant is the paired
// negative: with a write grant on /private/tmp, /tmp, and the user $TMPDIR
// added, the same create, rename, and remove succeed.
func TestSandboxExecSessionTmp_SharedTmpWritableWithGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	fx := sharedTmpFixtures(t)

	m := newSocketProfileManager(t)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return p + "\n" + sharedTmpWriteGrant()
	})

	for _, f := range fx {
		expectOp(t, mutatedPath, probe, true, "the create in "+f.tree+" still fails with a write grant, so the positive test proves nothing", "--create", f.fresh)
		expectOp(t, mutatedPath, probe, true, "the rename in "+f.tree+" still fails with a write grant, so the positive test proves nothing", "--rename", f.existing, f.existing+".renamed")
		expectOp(t, mutatedPath, probe, true, "the remove in "+f.tree+" still fails with a write grant, so the positive test proves nothing", "--remove", f.existing+".renamed")
	}
}

// otherSession prepares a second session, puts a file and a listening socket
// in its TMPDIR, and returns its TMPDIR.
func otherSession(t *testing.T) string {
	t.Helper()
	other := container.New(container.Config{
		SessionName:  "integ-sandbox-exec-other-session",
		InstanceID:   "integ-sbx-other-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:     t.TempDir(),
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	})
	preparePositiveProfile(t, other)
	dir := sessionTmpDirOf(t, other)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write the file of the other session: %v", err)
	}
	startUnixListener(t, filepath.Join(dir, "s.sock"))
	return dir
}

// TestSandboxExecSessionTmp_OtherSessionDenied: under the production
// profile of one session, a read, a create, and a connect in the TMPDIR of
// another session fail.
func TestSandboxExecSessionTmp_OtherSessionDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	otherDir := otherSession(t)

	m := newSocketProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	if own := sessionTmpDirOf(t, m); own == otherDir {
		t.Fatalf("two sessions share the TMPDIR %s", own)
	}

	expectOp(t, profilePath, probe, false, "the session read a file in the TMPDIR of another session", "--read", filepath.Join(otherDir, "f"))
	expectOp(t, profilePath, probe, false, "the session created a file in the TMPDIR of another session", "--create", filepath.Join(otherDir, "new"))
	expectDenied(t, profilePath, probe, filepath.Join(otherDir, "s.sock"),
		"the session connected to a socket in the TMPDIR of another session")
}

// TestSandboxExecSessionTmp_OtherSessionReachableWithItsGrant is the paired
// negative: with the TMPDIR rules of the profile pointed at the TMPDIR of
// the other session, the same read, create, and connect succeed.
func TestSandboxExecSessionTmp_OtherSessionReachableWithItsGrant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	otherDir := otherSession(t)

	m := newSocketProfileManager(t)
	tmpDir := sessionTmpDirOf(t, m)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return retargetSessionTmp(p, tmpDir, otherDir)
	})

	expectOp(t, mutatedPath, probe, true, "the read still fails with the grant on the other TMPDIR, so the positive test proves nothing", "--read", filepath.Join(otherDir, "f"))
	expectOp(t, mutatedPath, probe, true, "the create still fails with the grant on the other TMPDIR, so the positive test proves nothing", "--create", filepath.Join(otherDir, "new"))
	expectConnected(t, mutatedPath, probe, filepath.Join(otherDir, "s.sock"),
		"the connect still fails with the grant on the other TMPDIR, so the positive test proves nothing")
}

// sessionWorkDirOf returns the per-session work dir of m, a destination
// that the profile grants for write (section 6).
func sessionWorkDirOf(t *testing.T, m *container.Manager) string {
	t.Helper()
	dir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	return dir
}

// sessionTmpNodeDenyOps are the operations of the section-21e deny, in the
// order that the generator writes them.
var sessionTmpNodeDenyOps = []string{"file-write-unlink", "file-write-create", "file-write-mode", "file-write-flags"}

// sessionTmpNodeDenyBlock is the section-21e clause for the TMPDIR tmpDir,
// with the operations ops (default: sessionTmpNodeDenyOps).
// withMutatedProbeProfile fails when the text is not in the profile, so a
// copy that drifts from the generator fails the negative tests.
func sessionTmpNodeDenyBlock(tmpDir string, ops ...string) string {
	if len(ops) == 0 {
		ops = sessionTmpNodeDenyOps
	}
	return "(deny " + strings.Join(ops, " ") + "\n" +
		"  (literal " + sbplQuoteForTest("/private"+tmpDir) + ")\n" +
		"  (literal " + sbplQuoteForTest(tmpDir) + "))\n"
}

// withoutSessionTmpNodeDenyOp takes op out of the section-21e deny and
// keeps the other operations of the deny.
func withoutSessionTmpNodeDenyOp(profile, tmpDir, op string) string {
	var rest []string
	for _, o := range sessionTmpNodeDenyOps {
		if o != op {
			rest = append(rest, o)
		}
	}
	return strings.Replace(profile, sessionTmpNodeDenyBlock(tmpDir), sessionTmpNodeDenyBlock(tmpDir, rest...), 1)
}

// TestSandboxExecSessionTmp_NodeCannotBeReplaced: under the production
// profile, the session cannot rename or remove the node of its own TMPDIR
// (section 21e), so it cannot put a symlink at the path.
func TestSandboxExecSessionTmp_NodeCannotBeReplaced(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	tmpDir := sessionTmpDirOf(t, m)

	moved := filepath.Join(sessionWorkDirOf(t, m), "moved-tmpdir")
	expectOp(t, profilePath, probe, false, "the session renamed its TMPDIR node", "--rename", tmpDir, moved)
	expectOp(t, profilePath, probe, false, "the session removed its TMPDIR node", "--remove", tmpDir)
	if info, err := os.Lstat(tmpDir); err != nil || !info.IsDir() {
		t.Errorf("the TMPDIR %s is not a dir after the denied operations: %v", tmpDir, err)
	}
}

// TestSandboxExecSessionTmp_NodeReplaceableWithoutDeny is the paired
// negative: with the section-21e deny removed, the rename of the TMPDIR node
// succeeds.
func TestSandboxExecSessionTmp_NodeReplaceableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	tmpDir := sessionTmpDirOf(t, m)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return strings.Replace(p, sessionTmpNodeDenyBlock(tmpDir), "", 1)
	})

	moved := filepath.Join(sessionWorkDirOf(t, m), "moved-tmpdir")
	expectOp(t, mutatedPath, probe, true, "the TMPDIR rename still fails without section 21e, so the positive test proves nothing", "--rename", tmpDir, moved)
}

// TestSandboxExecSessionTmp_NodeModeAndFlagsLocked: under the production
// profile, the session cannot chmod or chflags the node of its own TMPDIR
// (section 21e), in either /var form. The mode and the flags of the node
// do not change, so the session cannot open the node to other users or
// lock it against the cleanup removal.
func TestSandboxExecSessionTmp_NodeModeAndFlagsLocked(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	tmpDir := sessionTmpDirOf(t, m)
	clearFlagsAtCleanup(t, tmpDir)

	modeBefore, flagsBefore := nodeModeAndFlags(t, tmpDir)
	ops := []struct {
		why  string
		args func(node string) []string
	}{
		{"the session changed the mode of its TMPDIR node", func(node string) []string { return []string{"--chmod", node, "777"} }},
		{"the session set the uchg flag on its TMPDIR node", func(node string) []string { return []string{"--chflags", node, strconv.Itoa(ufImmutable)} }},
	}
	for _, node := range []string{tmpDir, "/private" + tmpDir} {
		for _, op := range ops {
			expectOp(t, profilePath, probe, false, op.why, op.args(node)...)
			if mode, flags := nodeModeAndFlags(t, tmpDir); mode != modeBefore || flags != flagsBefore {
				t.Errorf("probe %q changed the TMPDIR node: mode %v -> %v, flags %#x -> %#x", op.args(node), modeBefore, mode, flagsBefore, flags)
			}
			// Put the node back after a probe that changed it. Without this,
			// a uchg flag that one probe set makes the next chmod fail with
			// EPERM, and the next check passes whatever the profile holds.
			if err := syscall.Chflags(tmpDir, int(flagsBefore)); err != nil {
				t.Fatalf("reset the flags of %s: %v", tmpDir, err)
			}
			if err := os.Chmod(tmpDir, modeBefore); err != nil {
				t.Fatalf("reset the mode of %s: %v", tmpDir, err)
			}
		}
	}
}

// TestSandboxExecSessionTmp_NodeModeWritableWithoutDeny is the paired
// negative of the chmod check: with file-write-mode taken out of the
// section-21e deny, the chmod of the TMPDIR node succeeds.
func TestSandboxExecSessionTmp_NodeModeWritableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	tmpDir := sessionTmpDirOf(t, m)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return withoutSessionTmpNodeDenyOp(p, tmpDir, "file-write-mode")
	})

	expectOp(t, mutatedPath, probe, true, "the chmod of the TMPDIR node still fails without file-write-mode in section 21e, so the positive test proves nothing", "--chmod", tmpDir, "777")
	if mode, _ := nodeModeAndFlags(t, tmpDir); mode != 0o777 {
		t.Errorf("the TMPDIR node has mode %v after the chmod, want 0777", mode)
	}
}

// TestSandboxExecSessionTmp_NodeFlagsWritableWithoutDeny is the paired
// negative of the chflags check: with file-write-flags taken out of the
// section-21e deny, the chflags uchg of the TMPDIR node succeeds.
func TestSandboxExecSessionTmp_NodeFlagsWritableWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	probe := buildConnectProbe(t)

	m := newSocketProfileManager(t)
	tmpDir := sessionTmpDirOf(t, m)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return withoutSessionTmpNodeDenyOp(p, tmpDir, "file-write-flags")
	})
	clearFlagsAtCleanup(t, tmpDir)

	expectOp(t, mutatedPath, probe, true, "the chflags of the TMPDIR node still fails without file-write-flags in section 21e, so the positive test proves nothing", "--chflags", tmpDir, strconv.Itoa(ufImmutable))
	if _, flags := nodeModeAndFlags(t, tmpDir); flags&ufImmutable == 0 {
		t.Errorf("the TMPDIR node has flags %#x after the chflags, want uchg", flags)
	}
}
