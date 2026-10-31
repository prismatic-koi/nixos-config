//go:build darwin

package integration_test

// sandbox_exec_session_above_root_darwin_test.go — prism refuses a
// sandbox-exec session in a dir that holds project roots (#3381).
//
// Section 6 of the profile grants a write of the whole dir of the session.
// A session in a project location, or in a dir above it, can then write the
// .bare (config, hooks, info) of each project below it. The control test
// starts such a session with a config.json that does not name the project
// location, and shows that the sandbox writes each of these paths. The
// refusal test names the project location in config.json and expects
// PrepareSandboxExec to refuse the session before it writes a profile, also
// for a symlink and for another letter case of the dir. The worktree test
// shows that a session in a worktree of the same project still starts, and
// still cannot write the .bare.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// sessionAboveRootFixture is a project location under HOME with one
// bare+worktree project. It is under HOME and not under TMPDIR, because the
// profile allows the whole TMPDIR tree.
type sessionAboveRootFixture struct {
	base, code, root, bare, worktree string
}

func newSessionAboveRootFixture(t *testing.T) sessionAboveRootFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	base, err := os.MkdirTemp(home, ".prism-3381-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	f := sessionAboveRootFixture{
		base:     base,
		code:     filepath.Join(base, "code"),
		root:     filepath.Join(base, "code", "repo"),
		bare:     filepath.Join(base, "code", "repo", ".bare"),
		worktree: filepath.Join(base, "code", "repo", "main"),
	}
	for _, d := range []string{filepath.Join(f.bare, "hooks"), filepath.Join(f.bare, "info"), f.worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.bare, "config"), []byte("[core]\n\tbare = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// useProjectLocation points PRISM_CONFIG_FILE at a config.json with the one
// project location.
func useProjectLocation(t *testing.T, location string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"project_locations": []string{location}, "project_specific": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", path)
}

func newSessionAboveRootManager(t *testing.T, worktree, bareRoot string) *container.Manager {
	t.Helper()
	return container.New(container.Config{
		SessionName:  "integ-session-above-root",
		InstanceID:   "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Worktree:     worktree,
		BareRoot:     bareRoot,
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	})
}

// bareWriteScript writes .bare/config, a hook, and info/attributes. It exits
// 10, 11, or 12 at the first write that fails, and 0 when all three work.
const bareWriteScript = `echo '[filter "evil"]' >> "$BARE/config" || exit 10
echo '#!/bin/sh' > "$BARE/hooks/post-checkout" || exit 11
echo '* filter=evil' > "$BARE/info/attributes" || exit 12
exit 0`

func runBareScript(t *testing.T, profilePath, bare, worktree, script string) ([]byte, error) {
	t.Helper()
	nixBash := requireNixBash(t)
	cmd := exec.Command(sandboxExecPath, "-f", profilePath, nixBash, "-c", script)
	cmd.Env = append(os.Environ(), "BARE="+bare, "WT="+worktree)
	return cmd.CombinedOutput()
}

// TestSandboxExecSessionAboveRoot_PrepareRefuses: with the project location
// in config.json, PrepareSandboxExec refuses a session in the project
// location, in a dir above it, in a symlink to it, and in the project
// location with another letter case on a case-insensitive volume. It
// writes no profile.
func TestSandboxExecSessionAboveRoot_PrepareRefuses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	f := newSessionAboveRootFixture(t)
	useProjectLocation(t, f.code)
	link := filepath.Join(f.base, "link-to-code")
	if err := os.Symlink(f.code, link); err != nil {
		t.Fatal(err)
	}
	dirs := []string{f.code, f.base, link}
	if upper := filepath.Join(f.base, "CODE"); func() bool { _, err := os.Stat(upper); return err == nil }() {
		dirs = append(dirs, upper)
	} else {
		t.Logf("the volume of %s is case-sensitive; no letter-case probe", f.base)
	}
	for _, dir := range dirs {
		m := newSessionAboveRootManager(t, dir, "")
		args, err := m.PrepareSandboxExec()
		if !errors.Is(err, container.ErrSessionDirHoldsProjectRoots) {
			if len(args) >= 3 {
				_ = os.Remove(args[2])
			}
			t.Errorf("PrepareSandboxExec(%s) = %v, want ErrSessionDirHoldsProjectRoots", dir, err)
			continue
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("PrepareSandboxExec(%s) error %q does not name the dir", dir, err)
		}
		if len(args) != 0 {
			t.Errorf("PrepareSandboxExec(%s) returned args for a refused session: %d", dir, len(args))
		}
	}
}

// TestSandboxExecSessionAboveRoot_BareWritableWithoutRefusal is the control:
// config.json does not name the project location, so prism starts the
// session in it, and the sandbox writes .bare/config, a hook, and
// info/attributes of the project below it.
func TestSandboxExecSessionAboveRoot_BareWritableWithoutRefusal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	f := newSessionAboveRootFixture(t)
	useProjectLocation(t, filepath.Join(f.base, "elsewhere"))
	m := newSessionAboveRootManager(t, f.code, "")
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if out, err := runBareScript(t, profilePath, f.bare, f.worktree, bareWriteScript); err != nil {
		t.Fatalf("a session in %s could not write the .bare below it, so the refusal test proves nothing (exit 10: config, 11: hook, 12: info/attributes).\nExit: %v\nOutput: %s\nProfile: %s",
			f.code, err, out, profilePath)
	}
	if got, err := os.ReadFile(filepath.Join(f.bare, "config")); err != nil || !strings.Contains(string(got), `[filter "evil"]`) {
		t.Errorf(".bare/config on the host = (%q, %v), want the planted filter", got, err)
	}
}

// TestSandboxExecSessionAboveRoot_WorktreeSessionStarts: with the project
// location in config.json, a session in a worktree of the project still
// prepares. It writes its worktree, and it cannot write .bare/config, a
// hook, or info/attributes.
func TestSandboxExecSessionAboveRoot_WorktreeSessionStarts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	f := newSessionAboveRootFixture(t)
	useProjectLocation(t, f.code)
	m := newSessionAboveRootManager(t, f.worktree, f.root)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	if out, err := runBareScript(t, profilePath, f.bare, f.worktree, `echo ok > "$WT/new.txt"`); err != nil {
		t.Fatalf("a write to the worktree failed: %v\nOutput: %s\nProfile: %s", err, out, profilePath)
	}
	for _, script := range []string{
		`echo '[filter "evil"]' >> "$BARE/config"`,
		`echo '#!/bin/sh' > "$BARE/hooks/post-checkout"`,
		`echo '* filter=evil' > "$BARE/info/attributes"`,
	} {
		out, err := runBareScript(t, profilePath, f.bare, f.worktree, script)
		if err == nil {
			t.Errorf("%q succeeded in a worktree session.\nOutput: %s\nProfile: %s", script, out, profilePath)
			continue
		}
		if !strings.Contains(string(out), "Operation not permitted") {
			t.Errorf("%q failed for a reason other than the sandbox: %v\nOutput: %s", script, err, out)
		}
	}
}
