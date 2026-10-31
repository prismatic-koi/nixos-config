package container

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sessionDirScopeFixture is a project location with one bare+worktree
// project, one project root that is not a repo, and a dir outside each
// project. config.json names the project location.
type sessionDirScopeFixture struct {
	base, code, root, worktree, scratch, notes string
}

func newSessionDirScopeFixture(t *testing.T) sessionDirScopeFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := sessionDirScopeFixture{
		base:     base,
		code:     filepath.Join(base, "code"),
		root:     filepath.Join(base, "code", "repo"),
		worktree: filepath.Join(base, "code", "repo", "main"),
		scratch:  filepath.Join(base, "code", "scratch"),
		notes:    filepath.Join(base, "notes"),
	}
	for _, d := range []string{
		filepath.Join(f.root, ".bare", "hooks"),
		filepath.Join(f.root, ".bare", "info"),
		f.worktree, f.scratch, f.notes,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, ".bare", "config"), []byte("[core]\n\tbare = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSessionDirScopeConfig(t, []string{f.code})
	return f
}

// writeSessionDirScopeConfig points PRISM_CONFIG_FILE at a config.json with
// the project locations.
func writeSessionDirScopeConfig(t *testing.T, locations []string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"project_locations": locations, "project_specific": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

// AC (#3381): bwrap and sandbox-exec each refuse a session in a project
// location, in a dir above it, and in a symlink to it, before they write a
// file for the session. The error names the dir and the reason.
func TestPrepare_RefusesSessionDirThatHoldsProjectRoots_3381(t *testing.T) {
	f := newSessionDirScopeFixture(t)
	link := filepath.Join(f.notes, "link-to-code")
	if err := os.Symlink(f.code, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{f.code, f.base, link} {
		t.Run("bwrap "+dir, func(t *testing.T) {
			m, _, cleanup := bwrapFixture(t, Config{
				SessionName:  "code",
				Worktree:     dir,
				GitUserName:  "Test User",
				GitUserEmail: "test@example.com",
			})
			defer cleanup()
			args, err := m.PrepareBwrap()
			if !errors.Is(err, ErrSessionDirHoldsProjectRoots) {
				t.Fatalf("PrepareBwrap(%s) = (%v, %v), want ErrSessionDirHoldsProjectRoots", dir, redactedArgs(args), err)
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), f.code) {
				t.Errorf("error %q does not name the dir %s and the project location %s", err, dir, f.code)
			}
			if got, _ := os.ReadFile(m.SshConfigFilePath()); string(got) != "fake ssh config" {
				t.Errorf("PrepareBwrap wrote the SSH config of a refused session: %q", got)
			}
		})
		t.Run("sandbox-exec "+dir, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			m := newSandboxExecManagerWithInstance(Config{SessionName: "code", Worktree: dir})
			args, err := m.PrepareSandboxExec()
			if !errors.Is(err, ErrSessionDirHoldsProjectRoots) {
				t.Fatalf("PrepareSandboxExec(%s) = (%v, %v), want ErrSessionDirHoldsProjectRoots", dir, redactedArgs(args), err)
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), f.code) {
				t.Errorf("error %q does not name the dir %s and the project location %s", err, dir, f.code)
			}
			if _, statErr := os.Stat(m.sandboxExecProfilePath()); statErr == nil {
				t.Error("PrepareSandboxExec wrote the profile of a refused session")
			}
			if sessionDir, dirErr := m.sessionWorkDirPath(); dirErr == nil {
				if _, statErr := os.Stat(sessionDir); statErr == nil {
					t.Error("PrepareSandboxExec made the work dir of a refused session")
				}
			}
		})
	}
}

// The control of the refusal: when config.json does not name the project
// location, the session in it starts. Its bwrap bind and its profile grant
// a write of the whole dir, and nothing pins the .bare below it.
func TestPrepare_SessionDirAboveARepoCanWriteItsBare_3381(t *testing.T) {
	f := newSessionDirScopeFixture(t)
	writeSessionDirScopeConfig(t, []string{filepath.Join(f.base, "elsewhere")})

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:  "code",
		Worktree:     f.code,
		GitUserName:  "Test User",
		GitUserEmail: "test@example.com",
	})
	defer cleanup()
	args, err := m.PrepareBwrap()
	if err != nil {
		t.Fatalf("PrepareBwrap: %v", err)
	}
	if !hasBind(args, f.code) {
		t.Fatalf("args have no read-write bind of %s: %v", f.code, redactedArgs(args))
	}
	for _, name := range []string{"config", "hooks", "info"} {
		p := filepath.Join(f.root, ".bare", name)
		if slices.Contains(args, p) {
			t.Errorf("args name %s, want no bind of it: %v", p, redactedArgs(args))
		}
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sm := newSandboxExecManagerWithInstance(Config{SessionName: "code", Worktree: f.code})
	if _, err := sm.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sm.sandboxExecProfilePath()) })
	profile := generateProfile(sm)
	if !strings.Contains(profile, "(subpath "+quoteSBPL(f.code)+")") {
		t.Errorf("the profile grants no subpath of %s", f.code)
	}
	if strings.Contains(profile, filepath.Join(f.root, ".bare")) {
		t.Errorf("the profile names %s, want no rule for it", filepath.Join(f.root, ".bare"))
	}
}

// AC (#3381): a session in a worktree, in a project root that is not a
// repo, and in a dir outside each project still prepares.
func TestPrepare_AllowsSessionDirBelowProjectRoots_3381(t *testing.T) {
	f := newSessionDirScopeFixture(t)
	for _, tc := range []struct {
		dir, bareRoot string
	}{
		{f.worktree, f.root},
		{f.scratch, ""},
		{f.notes, ""},
	} {
		t.Run("bwrap "+tc.dir, func(t *testing.T) {
			m, _, cleanup := bwrapFixture(t, Config{
				SessionName:  "repo@main",
				Worktree:     tc.dir,
				BareRoot:     tc.bareRoot,
				GitUserName:  "Test User",
				GitUserEmail: "test@example.com",
			})
			defer cleanup()
			if _, err := m.PrepareBwrap(); err != nil {
				t.Fatalf("PrepareBwrap(%s): %v", tc.dir, err)
			}
		})
		t.Run("sandbox-exec "+tc.dir, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			m := newSandboxExecManagerWithInstance(Config{SessionName: "repo@main", Worktree: tc.dir, BareRoot: tc.bareRoot})
			t.Cleanup(func() { _ = os.Remove(m.sandboxExecProfilePath()) })
			if _, err := m.PrepareSandboxExec(); err != nil {
				t.Fatalf("PrepareSandboxExec(%s): %v", tc.dir, err)
			}
		})
	}
}
