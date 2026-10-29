package container

// pi_shared_state_test.go — the shared lock dir and Notion store dir
// (#3289, #3290): the paths, the one-time move of the Notion store, the
// bwrap binds, and the sandbox-exec grant. Each test sets HOME or passes
// its own dirs, so no test reads or writes the real home dir.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	notionTokensJSON = `{"accessToken":"a","refreshToken":"r","expiresAt":1,"clientId":"c"}`
	notionClientJSON = `{"clientId":"c","redirectUri":"http://localhost:3738/oauth/callback","registeredAt":1}`
)

// notionMoveFixture makes an old agent dir with the two Notion store files
// and an empty new store dir.
func notionMoveFixture(t *testing.T) (agentDir, notionDir string) {
	t.Helper()
	root := t.TempDir()
	agentDir = filepath.Join(root, ".pi", "agent")
	notionDir = filepath.Join(root, ".local", "state", "prism", "pi-notion")
	for _, d := range []string{agentDir, notionDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(agentDir, "notion-mcp-oauth.json"), notionTokensJSON)
	writeTestFile(t, filepath.Join(agentDir, "notion-mcp-client.json"), notionClientJSON)
	return agentDir, notionDir
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// writeLockOwner makes the lock dir at dir with an owner file in the
// format of notion/auth.ts, ts ms in the past.
func writeLockOwner(t *testing.T, dir, id string, age time.Duration) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(map[string]any{"id": id, "pid": 1, "ts": time.Now().Add(-age).UnixMilli()})
	writeTestFile(t, filepath.Join(dir, "owner"), string(owner))
}

func TestPISharedStateDirs_Paths(t *testing.T) {
	lockDir, notionDir := PISharedStateDirs("/home/u")
	if lockDir != "/home/u/.local/state/prism/pi-locks" || notionDir != "/home/u/.local/state/prism/pi-notion" {
		t.Errorf("PISharedStateDirs(/home/u) = %q, %q", lockDir, notionDir)
	}
	if lockDir, notionDir := PISharedStateDirs(""); lockDir != "" || notionDir != "" {
		t.Errorf("PISharedStateDirs(\"\") = %q, %q, want empty", lockDir, notionDir)
	}
}

// The extensions compute the two paths themselves. They must name the same
// dirs as Go, or a sandbox gets a bind or grant that pi does not use.
func TestPISharedStateDirs_ExtensionsUseSamePaths(t *testing.T) {
	const extDir = "../../../pi/extensions"
	if _, err := os.Stat(extDir); os.IsNotExist(err) {
		t.Skipf("%s not present (expected in the nix build sandbox)", extDir)
	}
	for file, want := range map[string]string{
		extDir + "/anthropic-oauth/refresh-lock.ts": `join(homedir(), ".local", "state", "prism", "pi-locks")`,
		extDir + "/notion/auth.ts":                  `join(homedir(), ".local", "state", "prism", "pi-notion")`,
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not compute %s; keep it in step with PISharedStateDirs", file, want)
		}
	}
}

func TestEnsurePISharedStateDirs_MakesBothDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	lockDir, notionDir := EnsurePISharedStateDirs(filepath.Join(home, ".pi", "agent"))
	wantLock, wantNotion := PISharedStateDirs(home)
	if lockDir != wantLock || notionDir != wantNotion {
		t.Fatalf("EnsurePISharedStateDirs = %q, %q, want %q, %q", lockDir, notionDir, wantLock, wantNotion)
	}
	for _, d := range []string{lockDir, notionDir} {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s = (%v, %v), want a dir with mode 0700", d, fi, err)
		}
	}
}

// EnsurePISharedStateDirs also moves the Notion store of the real layout:
// ~/.pi/agent to ~/.local/state/prism/pi-notion.
func TestEnsurePISharedStateDirs_MovesNotionStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(agentDir, "notion-mcp-oauth.json"), notionTokensJSON)

	_, notionDir := EnsurePISharedStateDirs(agentDir)
	if got, err := os.ReadFile(filepath.Join(notionDir, "notion-mcp-oauth.json")); err != nil || string(got) != notionTokensJSON {
		t.Errorf("moved token file = (%q, %v)", got, err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("old token file still there: %v", err)
	}
}

// A home that prism cannot write gives empty paths, and no panic: the
// session starts with no bind and no grant.
func TestEnsurePISharedStateDirs_UnwritableHomeIsNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the dir mode")
	}
	home := filepath.Join(t.TempDir(), "ro-home")
	if err := os.Mkdir(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	t.Setenv("HOME", home)

	if lockDir, notionDir := EnsurePISharedStateDirs(filepath.Join(home, ".pi", "agent")); lockDir != "" || notionDir != "" {
		t.Errorf("EnsurePISharedStateDirs = %q, %q, want empty", lockDir, notionDir)
	}
}

func TestMigratePINotionStore_MovesBothFiles(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)

	migratePINotionStore(agentDir, notionDir)

	for name, want := range map[string]string{"notion-mcp-oauth.json": notionTokensJSON, "notion-mcp-client.json": notionClientJSON} {
		path := filepath.Join(notionDir, name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = (%q, %v), want %q", path, got, err, want)
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", path, fi.Mode().Perm())
		}
	}
	if got := dirNames(t, agentDir); len(got) != 0 {
		t.Errorf("old agent dir after the move = %v, want empty (no store files, no lock dir)", got)
	}
	if got, want := dirNames(t, notionDir), []string{"notion-mcp-client.json", "notion-mcp-oauth.json"}; !slices.Equal(got, want) {
		t.Errorf("store dir after the move = %v, want %v (no lock dir, no temp file)", got, want)
	}
}

// When the new store already has a file, that file is in use. The old file
// is removed, because it can hold a refresh token that Notion rotated away.
func TestMigratePINotionStore_NewStoreWins(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	const newer = `{"accessToken":"a2","refreshToken":"r2","expiresAt":2,"clientId":"c"}`
	writeTestFile(t, filepath.Join(notionDir, "notion-mcp-oauth.json"), newer)

	migratePINotionStore(agentDir, notionDir)

	if got, _ := os.ReadFile(filepath.Join(notionDir, "notion-mcp-oauth.json")); string(got) != newer {
		t.Errorf("the move changed the new token file: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("old token file still there: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(notionDir, "notion-mcp-client.json")); string(got) != notionClientJSON {
		t.Errorf("client file not moved: %q", got)
	}
}

// A symlink at an old store name is not followed: a sandbox with the old
// profile could put one there, and a copy of its target would land in a dir
// that every sandbox can read.
func TestMigratePINotionStore_SymlinkNotFollowed(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	secret := filepath.Join(t.TempDir(), "secret.json")
	writeTestFile(t, secret, `{"private":"key"}`)
	oldToken := filepath.Join(agentDir, "notion-mcp-oauth.json")
	if err := os.Remove(oldToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, oldToken); err != nil {
		t.Fatal(err)
	}

	migratePINotionStore(agentDir, notionDir)

	if _, err := os.Lstat(filepath.Join(notionDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("the target of a symlink was copied into the store dir: %v", err)
	}
	if fi, err := os.Lstat(oldToken); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was changed: (%v, %v)", fi, err)
	}
}

func TestMigratePINotionStore_NotOneJSONObjectStays(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	for _, content := range []string{"not json", "[]", "null", `{"a":1} {"b":2}`} {
		writeTestFile(t, filepath.Join(agentDir, "notion-mcp-oauth.json"), content)
		migratePINotionStore(agentDir, notionDir)
		if _, err := os.Lstat(filepath.Join(notionDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
			t.Errorf("content %q was moved: %v", content, err)
		}
		if got, _ := os.ReadFile(filepath.Join(agentDir, "notion-mcp-oauth.json")); string(got) != content {
			t.Errorf("content %q: old file = %q, want it unchanged", content, got)
		}
	}
}

// A live holder of the old lock (a pi with the old extension code, in a
// refresh) makes the move wait for the next session start.
func TestMigratePINotionStore_HeldOldLockDefers(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	oldLock := filepath.Join(agentDir, "notion-mcp-oauth.json.lock")
	writeLockOwner(t, oldLock, "live-peer", time.Second)

	migratePINotionStore(agentDir, notionDir)

	if got := dirNames(t, notionDir); len(got) != 0 {
		t.Errorf("store dir = %v, want empty: the move must wait for the lock", got)
	}
	if id := readPINotionLockOwnerID(oldLock); id != "live-peer" {
		t.Errorf("old lock owner = %q, want the live peer kept", id)
	}
}

func TestMigratePINotionStore_HeldNewLockDefers(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	writeLockOwner(t, filepath.Join(notionDir, "notion-mcp-oauth.json.lock"), "live-peer", time.Second)

	migratePINotionStore(agentDir, notionDir)

	if _, err := os.Lstat(filepath.Join(notionDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("the token file moved while the new lock was held: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "notion-mcp-oauth.json.lock")); !os.IsNotExist(err) {
		t.Errorf("the old lock was not released after the deferral: %v", err)
	}
}

// A stale lock (its holder died) does not stop the move. The age comes from
// the owner file, or from the dir mtime when there is no owner file.
func TestMigratePINotionStore_StaleLockIsBroken(t *testing.T) {
	t.Run("owner ts", func(t *testing.T) {
		agentDir, notionDir := notionMoveFixture(t)
		writeLockOwner(t, filepath.Join(agentDir, "notion-mcp-oauth.json.lock"), "dead-peer", time.Minute)
		migratePINotionStore(agentDir, notionDir)
		if _, err := os.Stat(filepath.Join(notionDir, "notion-mcp-oauth.json")); err != nil {
			t.Errorf("token file not moved past a stale lock: %v", err)
		}
	})
	t.Run("dir mtime", func(t *testing.T) {
		agentDir, notionDir := notionMoveFixture(t)
		lock := filepath.Join(agentDir, "notion-mcp-oauth.json.lock")
		if err := os.Mkdir(lock, 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		migratePINotionStore(agentDir, notionDir)
		if _, err := os.Stat(filepath.Join(notionDir, "notion-mcp-oauth.json")); err != nil {
			t.Errorf("token file not moved past a stale lock with no owner: %v", err)
		}
	})
	t.Run("fresh dir mtime is held", func(t *testing.T) {
		agentDir, notionDir := notionMoveFixture(t)
		if err := os.Mkdir(filepath.Join(agentDir, "notion-mcp-oauth.json.lock"), 0o700); err != nil {
			t.Fatal(err)
		}
		migratePINotionStore(agentDir, notionDir)
		if _, err := os.Lstat(filepath.Join(notionDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
			t.Errorf("token file moved past a fresh lock with no owner: %v", err)
		}
	})
}

// The owner write does not follow a symlink. A sandbox can swap in a dir
// with an "owner" symlink after the mkdir of prism; the target must not
// change.
func TestWritePINotionLockOwner_DoesNotFollowSymlink(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "notion-mcp-oauth.json.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "host-file")
	writeTestFile(t, target, "host content")
	if err := os.Symlink(target, filepath.Join(lock, "owner")); err != nil {
		t.Fatal(err)
	}

	if err := writePINotionLockOwner(lock, "prism"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("writePINotionLockOwner = %v, want EEXIST", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "host content" {
		t.Errorf("symlink target = %q, want it unchanged", got)
	}
}

// An owner FIFO in a lock dir that a sandbox made does not block the move:
// the read fails at once, the age comes from the dir mtime, and a fresh
// lock defers the move.
func TestMigratePINotionStore_OwnerFIFODoesNotBlock(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	lock := filepath.Join(notionDir, "notion-mcp-oauth.json.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(lock, "owner"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan struct{})
	go func() {
		migratePINotionStore(agentDir, notionDir)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the move blocked on the owner FIFO")
	}
	if _, err := os.Lstat(filepath.Join(notionDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("the token file moved past a fresh lock: %v", err)
	}
}

// With nothing to move, the move makes no lock dir and changes nothing.
func TestMigratePINotionStore_NothingToMove(t *testing.T) {
	agentDir, notionDir := notionMoveFixture(t)
	for _, name := range piNotionStoreFiles {
		if err := os.Remove(filepath.Join(agentDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	migratePINotionStore(agentDir, notionDir)
	if got := dirNames(t, agentDir); len(got) != 0 {
		t.Errorf("old agent dir = %v, want empty", got)
	}
	if got := dirNames(t, notionDir); len(got) != 0 {
		t.Errorf("store dir = %v, want empty", got)
	}
}

// piBwrapMountsCfg returns a Config that appendPIBwrapMounts accepts.
func piBwrapMountsCfg(t *testing.T) Config {
	t.Helper()
	extDir := t.TempDir()
	writeTestFile(t, filepath.Join(extDir, piExtensionFilename), "// ext")
	fakePI := filepath.Join(t.TempDir(), "pi")
	writeTestFile(t, fakePI, "#!/bin/sh")
	return Config{
		PIBinaryPath:            fakePI,
		PIAgentConfigHostDir:    t.TempDir(),
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
		PIExtensionHostDir:      extDir,
	}
}

// bwrap binds each shared dir read-write at its host path, so that the
// path that the extensions compute from HOME is the host dir.
func TestAppendPIBwrapMounts_BindsSharedStateDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := piBwrapMountsCfg(t)
	cfg.PILockHostDir, cfg.PINotionHostDir = PISharedStateDirs(t.TempDir())
	for _, d := range []string{cfg.PILockHostDir, cfg.PINotionHostDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}
	for _, d := range []string{cfg.PILockHostDir, cfg.PINotionHostDir} {
		if !hasTriple(args, "--bind", d, d) {
			t.Errorf("no --bind %s %s in %v", d, d, redactedArgs(args))
		}
	}
}

// An empty field, a missing dir, or a symlink gives no bind.
func TestAppendPIBwrapMounts_NoSharedStateBindWithoutDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := piBwrapMountsCfg(t)
	root := t.TempDir()
	cfg.PILockHostDir = filepath.Join(root, "missing")
	cfg.PINotionHostDir = filepath.Join(root, "link")
	if err := os.Symlink(t.TempDir(), cfg.PINotionHostDir); err != nil {
		t.Fatal(err)
	}

	args, err := appendPIBwrapMounts(nil, cfg)
	if err != nil {
		t.Fatalf("appendPIBwrapMounts: %v", err)
	}
	for _, d := range []string{cfg.PILockHostDir, cfg.PINotionHostDir} {
		if slices.Contains(args, d) {
			t.Errorf("args name %s, want no bind: %v", d, redactedArgs(args))
		}
	}
}

// The sandbox-exec profile of a pi session grants a read and a write of the
// two shared dirs (section 6c), and the agent dir write clause does not
// name the Notion store.
func TestGenerateProfile_PISharedStateDirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	lockDir, notionDir := PISharedStateDirs(fakeHome)
	m := newSandboxExecManager(Config{
		SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"),
		PILockHostDir: lockDir, PINotionHostDir: notionDir,
	})
	profile := generateProfile(m)

	want := "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + quoteSBPL(lockDir) + ")\n" +
		"  (subpath " + quoteSBPL(notionDir) + "))\n"
	if !strings.Contains(profile, want) {
		t.Errorf("profile has no section-6c clause.\nwant:\n%s\nprofile:\n%s", want, profile)
	}
	agentDir := filepath.Join(fakeHome, ".pi", "agent")
	for _, p := range []string{filepath.Join(agentDir, "notion"), filepath.Join(agentDir, ".notion")} {
		if strings.Contains(profile, p) {
			t.Errorf("profile still names a Notion path in the agent dir (%s):\n%s", p, profile)
		}
	}
}

// Section 21g denies a replace of the two dir nodes, and must follow every
// allow, or SBPL lets the section-6c subpath grant outrank it.
func TestGenerateProfile_PISharedStateDenyFollowsEveryAllow(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	lockDir, notionDir := PISharedStateDirs(fakeHome)
	m := newSandboxExecManager(Config{
		SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"),
		PILockHostDir: lockDir, PINotionHostDir: notionDir,
	})
	profile := generateProfile(m)

	deny := "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n" +
		"  (literal " + quoteSBPL(lockDir) + ")\n" +
		"  (literal " + quoteSBPL(notionDir) + "))\n"
	denyIdx := strings.Index(profile, deny)
	if denyIdx < 0 {
		t.Fatalf("profile has no section-21g deny.\nwant:\n%s\nprofile:\n%s", deny, profile)
	}
	if last := strings.LastIndex(profile, "(allow "); denyIdx < last {
		t.Errorf("section-21g deny (idx %d) precedes an allow (idx %d)", denyIdx, last)
	}
}

func TestGenerateProfile_NoSharedStateRuleWhenEmptyOrNotPi(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	lockDir, notionDir := PISharedStateDirs(fakeHome)

	t.Run("pi, empty fields", func(t *testing.T) {
		m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w")})
		if profile := generateProfile(m); strings.Contains(profile, "pi-locks") || strings.Contains(profile, "pi-notion") {
			t.Errorf("profile names a shared dir with empty fields:\n%s", profile)
		}
	})
	t.Run("pi, lock dir only", func(t *testing.T) {
		m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"), PILockHostDir: lockDir})
		profile := generateProfile(m)
		if !strings.Contains(profile, "(subpath "+quoteSBPL(lockDir)+"))\n") || strings.Contains(profile, "pi-notion") {
			t.Errorf("profile with the lock dir only:\n%s", profile)
		}
	})
	t.Run("not pi", func(t *testing.T) {
		m := newSandboxExecManager(Config{SessionName: "repo@feature", Worktree: filepath.Join(fakeHome, "w"), PILockHostDir: lockDir, PINotionHostDir: notionDir})
		if profile := generateProfile(m); strings.Contains(profile, "pi-locks") || strings.Contains(profile, "pi-notion") {
			t.Errorf("profile of a non-pi session names a shared dir:\n%s", profile)
		}
	})
}

func TestPISharedStateSBPLAllow_Empty(t *testing.T) {
	if got := piSharedStateSBPLAllow("", ""); got != "" {
		t.Errorf("piSharedStateSBPLAllow(\"\", \"\") = %q, want empty", got)
	}
}

// InPINotionRepos matches dir with the NOTION_MCP_REPOS entries as the
// extension does (scope.ts): dir is an entry or is below one (#3319).
func TestInPINotionRepos(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	vault := filepath.Join(home, "documents", "obsidian")
	code := filepath.Join(home, "code", "repo")
	worktree := filepath.Join(code, "main")
	other := filepath.Join(home, "code", "other")
	prefix := filepath.Join(home, "documents", "obsidian-old")
	for _, d := range []string{vault, worktree, other, prefix} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(home, "vault-link")
	if err := os.Symlink(vault, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		repos string
		dir   string
		want  bool
	}{
		{"tilde entry", "~/documents/obsidian", vault, true},
		{"$HOME entry", "$HOME/documents/obsidian", vault, true},
		{"${HOME} entry", "${HOME}/documents/obsidian", vault, true},
		{"absolute entry", vault, vault, true},
		{"second entry", "~/x:~/documents/obsidian", vault, true},
		{"spaces around an entry", " ~/documents/obsidian ", vault, true},
		{"trailing slash on the entry", "~/documents/obsidian/", vault, true},
		{"worktree below an entry", "~/code/repo", worktree, true},
		{"symlink entry", "~/vault-link", vault, true},
		{"symlink dir", "~/documents/obsidian", link, true},

		{"empty list", "", vault, false},
		{"only separators", "::", vault, false},
		{"other repo", "~/documents/obsidian", other, false},
		{"name prefix is not a parent", "~/documents/obsidian", prefix, false},
		{"entry below the dir", "~/code/repo/main", code, false},
		{"relative entry", "documents/obsidian", vault, false},
		{"tilde user entry", "~user/documents/obsidian", vault, false},
		{"missing entry", "~/gone", vault, false},
		{"relative dir", "~/documents/obsidian", "documents/obsidian", false},
		{"empty dir", "~/documents/obsidian", "", false},
		{"missing dir", "~/documents", filepath.Join(home, "documents", "gone"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InPINotionRepos(tc.repos, tc.dir); got != tc.want {
				t.Errorf("InPINotionRepos(%q, %q) = %v, want %v", tc.repos, tc.dir, got, tc.want)
			}
		})
	}
}

// A home entry with no home dir matches nothing.
func TestInPINotionRepos_NoHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", "")
	for _, repos := range []string{"~", "~/x", "$HOME", "${HOME}/x"} {
		if InPINotionRepos(repos, dir) {
			t.Errorf("InPINotionRepos(%q, %q) = true with no HOME, want false", repos, dir)
		}
	}
}

// bwrap binds the Notion store dir and the old store files in the agent dir
// only when PINotionHostDir is set (#3319). The lock dir is bound in both
// cases.
func TestAppendPIBwrapMounts_NotionStoreOnlyWithNotionDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lockDir, notionDir := PISharedStateDirs(t.TempDir())
	for _, d := range []string{lockDir, notionDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, granted := range []bool{true, false} {
		cfg := piBwrapMountsCfg(t)
		legacy := filepath.Join(cfg.PIAgentConfigHostDir, "notion-mcp-oauth.json")
		writeTestFile(t, legacy, "{}")
		cfg.PILockHostDir = lockDir
		if granted {
			cfg.PINotionHostDir = notionDir
		}
		args, err := appendPIBwrapMounts(nil, cfg)
		if err != nil {
			t.Fatalf("appendPIBwrapMounts: %v", err)
		}
		if !hasTriple(args, "--bind", lockDir, lockDir) {
			t.Errorf("granted=%v: no --bind of the lock dir in %v", granted, redactedArgs(args))
		}
		if got := slices.Contains(args, notionDir); got != granted {
			t.Errorf("granted=%v: args name the Notion store dir = %v, want %v: %v", granted, got, granted, redactedArgs(args))
		}
		if got := slices.Contains(args, legacy); got != granted {
			t.Errorf("granted=%v: args name the old store file = %v, want %v: %v", granted, got, granted, redactedArgs(args))
		}
	}
}

// Section 21l denies a read of the old Notion store files in the agent dir
// for a pi session with no Notion store dir (#3319), and must follow every
// allow, or SBPL lets the section-6a read grant outrank it. A session with
// the store dir, and a session that is not pi, get no such deny.
func TestGenerateProfile_PINotionLegacyDenyFollowsEveryAllow(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	lockDir, notionDir := PISharedStateDirs(fakeHome)
	agentDir := filepath.Join(fakeHome, ".pi", "agent")
	deny := piNotionLegacySBPLDeny(agentDir)
	for _, name := range piNotionStoreFiles {
		if !strings.Contains(deny, "(literal "+quoteSBPL(filepath.Join(agentDir, name))+")") {
			t.Errorf("section-21l deny does not name %s: %s", name, deny)
		}
	}
	if !strings.HasPrefix(deny, "(deny file-read*\n") {
		t.Errorf("section-21l deny is not a read deny: %s", deny)
	}

	m := newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"), PILockHostDir: lockDir})
	profile := generateProfile(m)
	denyIdx := strings.Index(profile, deny)
	if denyIdx < 0 {
		t.Fatalf("profile of a session with no Notion store dir has no section-21l deny:\n%s", profile)
	}
	if last := strings.LastIndex(profile, "(allow "); denyIdx < last {
		t.Errorf("section-21l deny (idx %d) precedes an allow (idx %d)", denyIdx, last)
	}
	if strings.Contains(profile, "pi-notion") {
		t.Errorf("profile of a session with no Notion store dir names the store dir:\n%s", profile)
	}

	m = newSandboxExecManager(Config{SessionName: "repo@feature", Harness: "pi", Worktree: filepath.Join(fakeHome, "w"), PILockHostDir: lockDir, PINotionHostDir: notionDir})
	if profile := generateProfile(m); strings.Contains(profile, deny) {
		t.Errorf("profile of a session with the Notion store dir has the section-21l deny:\n%s", profile)
	}
	m = newSandboxExecManager(Config{SessionName: "repo@feature", Worktree: filepath.Join(fakeHome, "w")})
	if profile := generateProfile(m); strings.Contains(profile, "notion-mcp") {
		t.Errorf("profile of a non-pi session names a Notion store file:\n%s", profile)
	}
}
