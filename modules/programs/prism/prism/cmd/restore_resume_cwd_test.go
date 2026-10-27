package cmd

// restore_resume_cwd_test.go — `prism restore` does not resume a pi session
// whose session-file header has a cwd that is not the worktree (#3284).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// restoreHeaderLine returns a pi session header line with id and cwd.
func restoreHeaderLine(t *testing.T, id, cwd string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "cwd": cwd})
	if err != nil {
		t.Fatalf("marshal session header: %v", err)
	}
	return append(b, '\n')
}

// writeRestoreSessionFile writes a pi session file for worktree under
// $HOME/.pi/agent/sessions and returns its path.
func writeRestoreSessionFile(t *testing.T, home, worktree, id, headerCwd string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent", "sessions", encodePiCWDForTest(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	path := filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+id+".jsonl")
	if err := os.WriteFile(path, restoreHeaderLine(t, id, headerCwd), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

func TestRestoreResumeSessionID(t *testing.T) {
	const id = "019e3284-aaaa-bbbb-cccc-dddddddddddd"
	const name = "myrepo@resume-cwd"

	t.Run("header cwd is the worktree", func(t *testing.T) {
		clearPICodingAgentDir(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		worktree := filepath.Join(home, "code", "myrepo", "resume-cwd")
		writeRestoreSessionFile(t, home, worktree, id, worktree)

		var got string
		stderr := captureStderrDuringFn(t, func() { got = restoreResumeSessionID(name, worktree, id) })
		if got != id {
			t.Errorf("restoreResumeSessionID = %q, want %q", got, id)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("header cwd is not the worktree", func(t *testing.T) {
		clearPICodingAgentDir(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		worktree := filepath.Join(home, "code", "myrepo", "resume-cwd")
		planted := filepath.Join(home, ".npm", "planted")
		path := writeRestoreSessionFile(t, home, worktree, id, planted)

		var got string
		stderr := captureStderrDuringFn(t, func() { got = restoreResumeSessionID(name, worktree, id) })
		if got != "" {
			t.Errorf("restoreResumeSessionID = %q, want empty", got)
		}
		for _, want := range []string{`restore "` + name + `"`, "not resuming pi session " + id, path, planted, worktree} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want substring %q", stderr, want)
			}
		}
	})

	// Only a missing sessions dir is a silent pass. A dangling symlink in the
	// dir is an error of one file, and restore must print it.
	t.Run("dangling symlink in the sessions dir", func(t *testing.T) {
		clearPICodingAgentDir(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		worktree := filepath.Join(home, "code", "myrepo", "resume-cwd")
		path := writeRestoreSessionFile(t, home, worktree, id, worktree)
		if err := os.Symlink(filepath.Join(home, "absent"), filepath.Join(filepath.Dir(path), "x.jsonl")); err != nil {
			t.Fatal(err)
		}

		var got string
		stderr := captureStderrDuringFn(t, func() { got = restoreResumeSessionID(name, worktree, id) })
		if got != "" {
			t.Errorf("restoreResumeSessionID = %q, want empty", got)
		}
		if !strings.Contains(stderr, "not resuming pi session "+id) || !strings.Contains(stderr, "x.jsonl") {
			t.Errorf("stderr = %q, want the refusal that names x.jsonl", stderr)
		}
	})

	t.Run("no sessions dir", func(t *testing.T) {
		clearPICodingAgentDir(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		worktree := filepath.Join(home, "code", "myrepo", "resume-cwd")

		var got string
		stderr := captureStderrDuringFn(t, func() { got = restoreResumeSessionID(name, worktree, id) })
		if got != id {
			t.Errorf("restoreResumeSessionID = %q, want %q (the launcher handles a missing file)", got, id)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})
}

// TestRestoreSession_HostMode_NoSessionFlag_WhenHeaderCwdIsForeign drives
// the real host-mode restore path. The session file exists, but its header
// names a directory outside the worktree, so the agent pane must start pi
// without --session.
func TestRestoreSession_HostMode_NoSessionFlag_WhenHeaderCwdIsForeign(t *testing.T) {
	clearPICodingAgentDir(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// See TestRestoreSession_HostMode_AppendsSessionFlagWhenFileExists.
	stubNvimOnPath(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)
	withRestoreConfig(t, config.Config{DefaultIsolationMode: config.IsolationHost})

	d := openRestoreTestDB(t)
	worktreeDir := t.TempDir()
	sessionName := "myrepo@host-resume-foreign"
	const harnessSessionID = "019e3284-1234-7890-abcd-ef0123456789"
	hsid := harnessSessionID
	_ = seedStatus(t, d, sessionName, worktreeDir, &hsid)
	if err := d.SetIsolationMode(sessionName, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	var status db.Status
	for _, st := range statuses {
		if st.SessionName == sessionName {
			status = st
			break
		}
	}

	writeRestoreSessionFile(t, fakeHome, worktreeDir, harnessSessionID, filepath.Join(fakeHome, "planted"))

	var restoreErr error
	stderr := captureStderrDuringFn(t, func() { restoreErr = callRestoreSession(d, status) })
	if restoreErr != nil {
		t.Fatalf("restoreSession: %v", restoreErr)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if !s.hasSession(sessionName) {
		t.Fatalf("session %q was not created", sessionName)
	}
	if want := `restore "` + sessionName + `": not resuming pi session ` + harnessSessionID; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want substring %q", stderr, want)
	}
	pane := agentPaneStartCmd(t, s, sessionName)
	if strings.Contains(pane, "--session") {
		t.Errorf("agent pane start command contains --session for a foreign header cwd\ncaptured: %s", pane)
	}
}
