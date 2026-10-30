package container

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/rundir"
)

// runDirOf returns the run dir of the profile manager of newTmpDirProfile.
func runDirOf(m *Manager) string {
	return filepath.Dir(m.cfg.HostAPISockPath)
}

// TestGenerateProfile_RunDirNodeDeny asserts the section-21m deny on unlink,
// create, mode writes, flag writes, and ACL writes of the run dir node, as a
// literal. A subpath here denies every write in the run dir.
func TestGenerateProfile_RunDirNodeDeny(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")
	runDir := runDirOf(m)

	want := "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n" +
		"  (literal " + quoteSBPL(runDir) + "))\n"
	if !strings.Contains(profile, want) {
		t.Errorf("profile missing the section-21m run dir node deny:\n%s\nprofile:\n%s", want, profile)
	}
	if !strings.Contains(profile, "  (subpath "+quoteSBPL(runDir)+")\n") {
		t.Errorf("profile missing the section-6 grant of the run dir, so the files in it are not writable:\n%s", profile)
	}
}

// TestGenerateProfile_RunDirNodeDenyFollowsEveryAllow pins the order: SBPL
// resolves a conflict in favour of the later rule, so the section-21m deny
// must follow the section-6 grant and every other allow.
func TestGenerateProfile_RunDirNodeDenyFollowsEveryAllow(t *testing.T) {
	m, profile := newTmpDirProfile(t, "instance-a")

	denyIdx := strings.Index(profile, dirNodeDeny([]string{runDirOf(m)}))
	lastAllow := strings.LastIndex(profile, "(allow ")
	if denyIdx < 0 {
		t.Fatalf("profile missing the section-21m run dir node deny:\n%s", profile)
	}
	if denyIdx < lastAllow {
		t.Errorf("section-21m run dir node deny (idx %d) precedes an allow (idx %d); SBPL would outrank the deny", denyIdx, lastAllow)
	}
}

// TestGenerateProfile_RunDirNodeDenyBothVarForms: a run dir under /var gets
// the deny in both /var symlink forms, as section 21e does.
func TestGenerateProfile_RunDirNodeDenyBothVarForms(t *testing.T) {
	runDir := "/var/folders/zz/abcdef/T/state/prism/run/abc"
	m := newSandboxExecManager(Config{
		SessionName:     "repo@main",
		InstanceID:      "instance-a",
		Worktree:        "/Users/prism-test/code/repo/main",
		HostAPISockPath: filepath.Join(runDir, "hostapi.sock"),
	})
	want := dirNodeDeny([]string{"/private" + runDir, runDir})
	if profile := generateProfile(m); !strings.Contains(profile, want) {
		t.Errorf("profile missing the section-21m deny in both /var forms:\n%s\nprofile:\n%s", want, profile)
	}
}

// TestGenerateProfile_NoRunDirNodeDenyWithoutHostAPISock: a profile with no
// host-API socket has no run dir grant, so its only node denies of this shape
// are the section-21e deny of the TMPDIR and the section-21n deny of the
// session work dir.
func TestGenerateProfile_NoRunDirNodeDenyWithoutHostAPISock(t *testing.T) {
	useProductionSessionTmpRoot(t)
	t.Setenv("XDG_STATE_HOME", "/Users/prism-test/.local/state")
	m := newSandboxExecManager(Config{
		SessionName: "repo@main",
		InstanceID:  "instance-a",
		Worktree:    "/Users/prism-test/code/repo/main",
	})
	const head = "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl\n"
	if n := strings.Count(generateProfile(m), head); n != 2 {
		t.Errorf("profile without HostAPISockPath has %d node denies, want 2 (the TMPDIR and the work dir)", n)
	}
}

// TestPrepareVolumeDirs_RefusesSymlinkedRunDir: a run dir that is a symlink,
// for example to the run dir of another session, fails the prepare, and no
// file is made in the other run dir. A missing run dir is made.
func TestPrepareVolumeDirs_RefusesSymlinkedRunDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	runDir := filepath.Join(root, "run", "own")
	m := New(Config{SessionName: "repo@main", Worktree: t.TempDir(), HostAPISockPath: filepath.Join(runDir, "hostapi.sock")})
	if err := m.prepareVolumeDirs(false); err != nil {
		t.Fatalf("prepareVolumeDirs with a missing run dir: %v", err)
	}
	if info, err := os.Lstat(runDir); err != nil || !info.IsDir() {
		t.Fatalf("prepareVolumeDirs did not make the run dir: %v %v", info, err)
	}

	other := filepath.Join(root, "run", "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, runDir); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareVolumeDirs(false); !errors.Is(err, rundir.ErrSymlinkDir) {
		t.Errorf("prepareVolumeDirs with a symlinked run dir: %v, want ErrSymlinkDir", err)
	}
}

// TestPILogResume_RefusesSymlinkedLog: the two resume warnings append to the
// agent-run log, and do not write through a symlink that the sandbox put at
// its path.
func TestPILogResume_RefusesSymlinkedLog(t *testing.T) {
	writers := map[string]func(Config){
		"piLogResumeWarning": func(cfg Config) { piLogResumeWarning(cfg, "/lookup", "detail") },
		"piLogResumeRefusal": func(cfg Config) { piLogResumeRefusal(cfg, errors.New("refused")) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cfg := Config{SessionName: "repo@feature", HarnessSessionID: "sid-1"}
			logPath, err := piAgentRunLogPath(cfg.SessionName)
			if err != nil {
				t.Fatal(err)
			}

			write(cfg)
			b, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(b), "sid-1") {
				t.Fatalf("%s wrote %q (%v) to the agent-run log, want a line about sid-1", name, b, err)
			}

			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(logPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, logPath); err != nil {
				t.Fatal(err)
			}
			write(cfg)
			if b, _ := os.ReadFile(target); string(b) != "keep" {
				t.Errorf("%s wrote through the symlink: the target holds %q", name, b)
			}
		})
	}
}
