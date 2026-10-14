package container

// session_tmp_dir_test.go — the per-session TMPDIR of a sandbox-exec session
// (#3150): its path, its creation and removal, and the profile rules that
// make it the only temp tree the session can write or connect in.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useProductionSessionTmpRoot points the per-session TMPDIR root at /tmp, the
// production root, for the rest of t. Only tests that do not create the dir
// call it: a content test then checks the production paths.
func useProductionSessionTmpRoot(t *testing.T) {
	t.Helper()
	t.Cleanup(SetSessionTmpRootForTest("/tmp"))
}

// grantedSBPLPaths returns every path term of every allow clause whose
// operation list satisfies opMatch.
func grantedSBPLPaths(profile string, opMatch func(ops string) bool) []sbplPathRule {
	var rules []sbplPathRule
	for _, clause := range topLevelSBPLClauses(profile) {
		if !strings.HasPrefix(clause, "(allow ") {
			continue
		}
		ops := clause
		if idx := strings.IndexByte(clause, '\n'); idx >= 0 {
			ops = clause[:idx]
		}
		if !opMatch(ops) {
			continue
		}
		for _, m := range sbplPathRuleRe.FindAllStringSubmatch(clause, -1) {
			rules = append(rules, sbplPathRule{form: m[1], path: m[2], clause: clause})
		}
	}
	return rules
}

// readsContent reports whether an allow clause grants a read of file
// content. file-read-metadata alone (stat) does not.
func readsContent(ops string) bool {
	return strings.Contains(ops, "file-read*") || strings.Contains(ops, "file-read-data")
}

// connectAllowedSBPLPaths returns the require-not path terms of the
// section-17b allowlist deny: the Unix-socket paths that the session can
// connect to.
func connectAllowedSBPLPaths(t *testing.T, profile string) []sbplPathRule {
	t.Helper()
	clause := extractClause(t, profile, "(deny network-outbound\n  (require-all")
	var rules []sbplPathRule
	for _, line := range strings.Split(clause, "\n") {
		if !strings.Contains(line, "(require-not ") {
			continue
		}
		for _, m := range sbplPathRuleRe.FindAllStringSubmatch(line, -1) {
			rules = append(rules, sbplPathRule{form: m[1], path: m[2], clause: clause})
		}
	}
	return rules
}

func anyCovers(rules []sbplPathRule, path string) (sbplPathRule, bool) {
	for _, r := range rules {
		if r.covers(path) {
			return r, true
		}
	}
	return sbplPathRule{}, false
}

func TestSessionTmpDirPath_ShortStableAndPerInstance(t *testing.T) {
	useProductionSessionTmpRoot(t)

	a1, err := SessionTmpDirPath("instance-a")
	if err != nil {
		t.Fatalf("SessionTmpDirPath: %v", err)
	}
	a2, _ := SessionTmpDirPath("instance-a")
	b, _ := SessionTmpDirPath("instance-b")
	if a1 != a2 {
		t.Errorf("the path is not stable for one instance ID: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("two instance IDs share the TMPDIR %q", a1)
	}
	if !strings.HasPrefix(a1, "/tmp/prism-") || len(filepath.Base(a1)) != len("prism-")+sessionTmpDirHashLen {
		t.Errorf("TMPDIR %q is not /tmp/prism-<%d hex chars>", a1, sessionTmpDirHashLen)
	}

	// The go test suite binds Unix sockets several levels below TMPDIR, and
	// a socket path must fit in sun_path (104 bytes with the NUL on Darwin).
	// At 31 bytes in the resolved form the suite passes in a sandbox-exec
	// session; each extra byte is one byte less for those socket paths.
	resolved := symlinkForms(a1)[0]
	if resolved != "/private"+a1 {
		t.Fatalf("resolved form = %q, want %q", resolved, "/private"+a1)
	}
	if len(resolved) > 31 {
		t.Errorf("resolved TMPDIR %q is %d bytes; keep it at 31 or less so the socket paths of go test fit in sun_path", resolved, len(resolved))
	}

	if _, err := SessionTmpDirPath(""); err == nil {
		t.Errorf("SessionTmpDirPath(\"\") returned no error")
	}
}

func TestSymlinkForms(t *testing.T) {
	cases := map[string][]string{
		"/tmp/prism-x":             {"/private/tmp/prism-x", "/tmp/prism-x"},
		"/private/tmp/prism-x":     {"/private/tmp/prism-x", "/tmp/prism-x"},
		"/var/folders/a/T":         {"/private/var/folders/a/T", "/var/folders/a/T"},
		"/private/var/folders/a/T": {"/private/var/folders/a/T", "/var/folders/a/T"},
		"/tmpfoo/x":                {"/tmpfoo/x"},
		"/Users/u/x":               {"/Users/u/x"},
	}
	for in, want := range cases {
		got := symlinkForms(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("symlinkForms(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionTmpDirEnv(t *testing.T) {
	got := SessionTmpDirEnv("/tmp/prism-abc")
	want := []string{"TMPDIR=/tmp/prism-abc", "MAC_CHROMIUM_TMPDIR=/tmp/prism-abc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SessionTmpDirEnv = %q, want %q", got, want)
	}
}

// newTmpDirTestManager returns a Manager with a unique instance ID and
// registers removal of its TMPDIR.
func newTmpDirTestManager(t *testing.T) *Manager {
	t.Helper()
	id := "tmpdir-" + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() { RemoveSessionTmpDir(id) })
	return newSandboxExecManagerWithInstance(Config{SessionName: "repo@tmpdir", InstanceID: id})
}

func TestPrepareSessionTmpDir_CreatesPrivateDir(t *testing.T) {
	m := newTmpDirTestManager(t)

	dir, err := m.PrepareSessionTmpDir()
	if err != nil {
		t.Fatalf("PrepareSessionTmpDir: %v", err)
	}
	want, _ := m.sessionTmpDirPath()
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("TMPDIR %q is not a dir: %v", dir, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("TMPDIR mode = %o, want 700", perm)
	}

	// A second call (a restored session) keeps the dir and its content.
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", keep, err)
	}
	if _, err := m.PrepareSessionTmpDir(); err != nil {
		t.Fatalf("second PrepareSessionTmpDir: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a second Prepare removed the content of the TMPDIR: %v", err)
	}
}

func TestPrepareSessionTmpDir_TightensMode(t *testing.T) {
	m := newTmpDirTestManager(t)
	dir, _ := m.sessionTmpDirPath()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := m.PrepareSessionTmpDir(); err != nil {
		t.Fatalf("PrepareSessionTmpDir: %v", err)
	}
	info, _ := os.Lstat(dir)
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("TMPDIR mode = %o after Prepare, want 700", perm)
	}
}

// TestPrepareSessionTmpDir_RefusesPlant: /tmp is writable by every local
// user, so a node at the path that Prepare did not make is a possible plant.
// Prepare refuses a symlink, a regular file, and a dir of another user.
func TestPrepareSessionTmpDir_RefusesPlant(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		m := newTmpDirTestManager(t)
		dir, _ := m.sessionTmpDirPath()
		target := t.TempDir()
		if err := os.Symlink(target, dir); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := m.PrepareSessionTmpDir(); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("Prepare accepted a symlink at the TMPDIR path; err = %v", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		m := newTmpDirTestManager(t)
		dir, _ := m.sessionTmpDirPath()
		if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := m.PrepareSessionTmpDir(); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("Prepare accepted a regular file at the TMPDIR path; err = %v", err)
		}
	})
	t.Run("other user", func(t *testing.T) {
		m := newTmpDirTestManager(t)
		if _, err := m.PrepareSessionTmpDir(); err != nil {
			t.Fatalf("PrepareSessionTmpDir: %v", err)
		}
		orig := geteuid
		geteuid = func() int { return orig() + 1 }
		t.Cleanup(func() { geteuid = orig })
		if _, err := m.PrepareSessionTmpDir(); err == nil || !strings.Contains(err.Error(), "another user") {
			t.Errorf("Prepare accepted a TMPDIR that another user owns; err = %v", err)
		}
	})
}

func TestRemoveSessionTmpDir(t *testing.T) {
	m := newTmpDirTestManager(t)
	dir, err := m.PrepareSessionTmpDir()
	if err != nil {
		t.Fatalf("PrepareSessionTmpDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	RemoveSessionTmpDir(m.cfg.InstanceID)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("TMPDIR %q still exists after RemoveSessionTmpDir: %v", dir, err)
	}
	// Idempotent.
	RemoveSessionTmpDir(m.cfg.InstanceID)
	RemoveSessionTmpDir("")
}

// TestRemoveSessionTmpDir_SymlinkKeepsTarget: if a symlink is at the TMPDIR
// path, the removal takes the link and not the content of its target.
func TestRemoveSessionTmpDir_SymlinkKeepsTarget(t *testing.T) {
	m := newTmpDirTestManager(t)
	dir, _ := m.sessionTmpDirPath()
	target := t.TempDir()
	victim := filepath.Join(target, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	RemoveSessionTmpDir(m.cfg.InstanceID)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the symlink at %q still exists: %v", dir, err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("RemoveSessionTmpDir removed the content of the symlink target: %v", err)
	}
}

func TestEnsureRemoved_RemovesSessionTmpDir(t *testing.T) {
	newFakeHome(t)
	m := newTmpDirTestManager(t)
	dir, err := m.PrepareSessionTmpDir()
	if err != nil {
		t.Fatalf("PrepareSessionTmpDir: %v", err)
	}
	m.EnsureRemoved(context.Background())
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("TMPDIR %q still exists after EnsureRemoved: %v", dir, err)
	}
}

// TestSandboxExecPrepare_CreatesSessionTmpDir: Prepare creates the TMPDIR
// that the profile grants.
func TestSandboxExecPrepare_CreatesSessionTmpDir(t *testing.T) {
	newFakeHome(t)
	m := newTmpDirTestManager(t)
	m.cfg.Worktree = t.TempDir()
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, err := m.sessionWorkDirPath(); err == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	if _, err := m.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}
	dir, _ := m.sessionTmpDirPath()
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		t.Fatalf("Prepare did not create the TMPDIR %q: %v", dir, err)
	}
	profile, err := os.ReadFile(m.sandboxExecProfilePath())
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if !strings.Contains(string(profile), "(subpath "+quoteSBPL(dir)+")") {
		t.Errorf("the profile does not grant the TMPDIR %q that Prepare created:\n%s", dir, profile)
	}
}

// TestSandboxExecPrepare_SessionTmpDirFailurePropagated: a plant at the
// TMPDIR path stops the session. Prepare writes no profile and returns no
// args.
func TestSandboxExecPrepare_SessionTmpDirFailurePropagated(t *testing.T) {
	newFakeHome(t)
	m := newTmpDirTestManager(t)
	m.cfg.Worktree = t.TempDir()
	t.Cleanup(func() {
		if sessionDir, err := m.sessionWorkDirPath(); err == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})
	dir, _ := m.sessionTmpDirPath()
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	iso := &sandboxExecIsolator{name: m.name}
	args, err := iso.Prepare(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "session TMPDIR") {
		t.Fatalf("Prepare with a symlink at the TMPDIR path: err = %v, want a session TMPDIR error", err)
	}
	if args != nil {
		t.Errorf("args must be nil on error; got %v", redactedArgs(args))
	}
	if _, statErr := os.Stat(m.sandboxExecProfilePath()); statErr == nil {
		t.Errorf("Prepare wrote a profile despite the TMPDIR failure")
		_ = os.Remove(m.sandboxExecProfilePath())
	}
}

// newTmpDirProfile renders the profile of a production-shaped session (home
// under /Users, user $TMPDIR under /var/folders) and returns it with the
// Manager.
func newTmpDirProfile(t *testing.T, instanceID string) (*Manager, string) {
	t.Helper()
	useProductionSessionTmpRoot(t)
	t.Setenv("TMPDIR", "/var/folders/zz/abcdef/T")
	home := auditLocationTestHome
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	m := newSandboxExecManager(Config{
		SessionName:     "repo@main",
		InstanceID:      instanceID,
		Worktree:        filepath.Join(home, "code", "repo", "main"),
		BareRoot:        filepath.Join(home, "code", "repo"),
		HostAPISockPath: filepath.Join(home, ".local", "state", "prism", "run", "abc", "hostapi.sock"),
	})
	return m, generateProfile(m)
}

// TestGenerateProfile_SessionTmpDirGrant: the profile grants read, write,
// map-executable, and Unix-socket connect on the per-session TMPDIR, in
// both /tmp symlink forms.
func TestGenerateProfile_SessionTmpDirGrant(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	tmpDir, _ := m.sessionTmpDirPath()

	clause := extractClause(t, profile, "(allow file-read* file-write* file-test-existence file-read-metadata file-map-executable\n  (subpath "+quoteSBPL("/private"+tmpDir)+")")
	if !strings.Contains(clause, "(subpath "+quoteSBPL(tmpDir)+")") {
		t.Errorf("the TMPDIR grant lacks the /tmp form %q; clause:\n%s", tmpDir, clause)
	}

	writes := grantedSBPLPaths(profile, func(ops string) bool { return strings.Contains(ops, "file-write") })
	reads := grantedSBPLPaths(profile, readsContent)
	connects := connectAllowedSBPLPaths(t, profile)
	for _, form := range symlinkForms(tmpDir) {
		inside := form + "/go-build1/s.sock"
		if _, ok := anyCovers(writes, inside); !ok {
			t.Errorf("no write grant covers %s", inside)
		}
		if _, ok := anyCovers(reads, inside); !ok {
			t.Errorf("no read grant covers %s", inside)
		}
		if _, ok := anyCovers(connects, inside); !ok {
			t.Errorf("no connect allowance covers %s", inside)
		}
	}
}

// TestGenerateProfile_NoSharedTmpGrant is the class fix of #3150: no write
// grant and no Unix-socket connect allowance covers /tmp, /private/tmp, or
// the user $TMPDIR. A host socket there (tmux, the ssh-agent, AeroSpace,
// podman, nvim, or one that the profile does not name) is then
// unreachable, and the session cannot create, rename, or delete a file
// there. TestGenerateProfile_SessionTmpDirGrant is the control: the same
// parsers find the session's own TMPDIR.
func TestGenerateProfile_NoSharedTmpGrant(t *testing.T) {
	_, profile := newTmpDirProfile(t, "instance-a")

	writes := grantedSBPLPaths(profile, func(ops string) bool { return strings.Contains(ops, "file-write") })
	connects := connectAllowedSBPLPaths(t, profile)
	if len(writes) == 0 || len(connects) == 0 {
		t.Fatalf("parsed no write grants (%d) or no connect allowances (%d); the parser is broken:\n%s", len(writes), len(connects), profile)
	}
	for _, p := range []string{
		"/private/tmp",
		"/private/tmp/macron-type.sock",
		"/private/tmp/tmux-501/default",
		"/tmp",
		"/tmp/macron-type.sock",
		"/var/folders/zz/abcdef/T",
		"/var/folders/zz/abcdef/T/podman/podman-machine-default-api.sock",
		"/private/var/folders/zz/abcdef/T",
		"/private/var/folders/zz/abcdef/T/nvim.u/r/nvim.1.0",
	} {
		if r, ok := anyCovers(writes, p); ok {
			t.Errorf("write grant (%s %q) covers the shared temp path %s.\nclause:\n%s", r.form, r.path, p, r.clause)
		}
		if r, ok := anyCovers(connects, p); ok {
			t.Errorf("connect allowance (%s %q) covers the shared temp path %s", r.form, r.path, p)
		}
	}
}

// TestGenerateProfile_OtherSessionTmpDirNotGranted: the profile of one
// session has no read, write, or connect rule on the TMPDIR of another.
func TestGenerateProfile_OtherSessionTmpDirNotGranted(t *testing.T) {
	_, profile := newTmpDirProfile(t, "instance-a")
	other, err := SessionTmpDirPath("instance-b")
	if err != nil {
		t.Fatalf("SessionTmpDirPath: %v", err)
	}

	writes := grantedSBPLPaths(profile, func(ops string) bool { return strings.Contains(ops, "file-write") })
	reads := grantedSBPLPaths(profile, readsContent)
	connects := connectAllowedSBPLPaths(t, profile)
	for _, form := range symlinkForms(other) {
		for _, p := range []string{form, form + "/f", form + "/s.sock"} {
			if r, ok := anyCovers(writes, p); ok {
				t.Errorf("write grant (%s %q) covers the TMPDIR of another session: %s", r.form, r.path, p)
			}
			if r, ok := anyCovers(reads, p); ok {
				t.Errorf("read grant (%s %q) covers the TMPDIR of another session: %s", r.form, r.path, p)
			}
			if r, ok := anyCovers(connects, p); ok {
				t.Errorf("connect allowance (%s %q) covers the TMPDIR of another session: %s", r.form, r.path, p)
			}
		}
	}
}

// TestGenerateProfile_SessionTmpDirNodeDeny asserts the section-21e deny on
// unlink and create of the TMPDIR node, in both forms, as literals. A
// subpath here denies every write inside the TMPDIR.
func TestGenerateProfile_SessionTmpDirNodeDeny(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	tmpDir, _ := m.sessionTmpDirPath()

	want := "(deny file-write-unlink file-write-create\n" +
		"  (literal " + quoteSBPL("/private"+tmpDir) + ")\n" +
		"  (literal " + quoteSBPL(tmpDir) + "))\n"
	if !strings.Contains(profile, want) {
		t.Errorf("profile missing the section-21e node deny:\n%s\nprofile:\n%s", want, profile)
	}
}

// TestGenerateProfile_SessionTmpDirNodeDenyFollowsEveryAllow pins the order:
// SBPL resolves a conflict in favour of the later rule, so the section-21e
// deny must follow the section-3 grant and every other allow.
func TestGenerateProfile_SessionTmpDirNodeDenyFollowsEveryAllow(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	tmpDir, _ := m.sessionTmpDirPath()

	denyIdx := strings.Index(profile, sessionTmpNodeDeny(symlinkForms(tmpDir)))
	lastAllow := strings.LastIndex(profile, "(allow ")
	if denyIdx < 0 {
		t.Fatalf("profile missing the section-21e node deny:\n%s", profile)
	}
	if denyIdx < lastAllow {
		t.Errorf("section-21e node deny (idx %d) precedes an allow (idx %d); SBPL would outrank the deny", denyIdx, lastAllow)
	}
}
