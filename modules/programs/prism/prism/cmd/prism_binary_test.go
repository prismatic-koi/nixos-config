package cmd

// Helpers for tests that run a real prism binary (issue #3146).
//
// Such a test runs the binary in a pane of a private tmux server. The pane
// command runs under the default-shell of that server. When the shell is the
// zsh of the user, ~/.zshenv can source home-manager's hm-session-vars.sh,
// which sets XDG_STATE_HOME and XDG_CONFIG_HOME back to the real paths. The
// binary then acts on the real prism database and the real sessions. Two
// mechanisms stop this, and each one is sufficient:
//
//  1. The test owns HOME, ZDOTDIR, XDG_STATE_HOME, XDG_CONFIG_HOME, and
//     TMUX_TMPDIR, and sets SHELL to /bin/sh, so the pane reads no startup
//     file of the user.
//  2. The binary is built with stateguard.Root set to the test root. It
//     refuses to run when a path that it resolves is outside that root.
//
// The sidecar that the binary starts detaches with setsid, so the kill of
// the tmux process group does not reach it. The cleanup finds every process
// of the binary by its path, which does not depend on the state dir, and
// stops it.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const stateguardRootFlag = "github.com/prismatic-koi/prism/internal/stateguard.Root"

// isolatedPrismBinary builds the prism binary for a test, and gives the test
// a home, a state dir, a config dir, and a tmux socket dir under one root
// that the test owns. It returns the binary path and the root.
//
// Call it before newCmdTestServer: the tmux server takes its environment
// when it starts. It must not run in a parallel test, because it uses
// t.Setenv.
func isolatedPrismBinary(t *testing.T) (bin, root string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found in PATH — skipping integration test")
	}
	if _, err := processesOf("/nonexistent"); err != nil {
		t.Skipf("pgrep cannot list processes here (%v): the test cannot find and stop the processes of the prism binary that it builds", err)
	}

	root = stateguardtest.ShortTempDir(t, "pbt-")
	bin = filepath.Join(root, "bin", "prism")

	// The build runs before HOME changes, so go keeps its caches.
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	build := exec.Command("go", "build", "-o", bin, "-ldflags", "-X "+stateguardRootFlag+"="+root, ".")
	build.Dir = moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	isolatePrismEnv(t, root)
	writeHostModeConfig(t, root)
	t.Cleanup(func() { stopProcessesOf(t, bin) })
	return bin, root
}

// writeHostModeConfig gives the binary a config.json for host mode in the
// test config dir. newCmdTestServer puts a stub `pi` on the PATH of the
// panes, so a host-mode session gets no real agent.
func writeHostModeConfig(t *testing.T, root string) {
	t.Helper()
	extDir := filepath.Join(root, "pi-extension")
	cfgDir := filepath.Join(root, "config", "prism")
	for _, d := range []string{extDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("create %s: %v", d, err)
		}
	}
	cfg := fmt.Sprintf(`{"default_isolation_mode": "host", "pi_extension_dir": %q}`, extDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// isolatePrismEnv points HOME, ZDOTDIR, XDG_STATE_HOME, XDG_CONFIG_HOME, and
// TMUX_TMPDIR at directories under root, and sets SHELL to /bin/sh.
func isolatePrismEnv(t *testing.T, root string) {
	t.Helper()
	home := filepath.Join(root, "home")
	dirs := map[string]string{
		"HOME":            home,
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"TMUX_TMPDIR":     filepath.Join(root, "tmux"),
	}
	for k, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("create %s dir: %v", k, err)
		}
		t.Setenv(k, d)
	}
	t.Setenv("ZDOTDIR", home)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TMUX", "")
	// The spawned binary must run its host-side path, not proxy to a sidecar.
	t.Setenv("PRISM_HOST_API", "")
	// A host gitconfig with gpgsign=true and no key breaks every commit.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

// rootDBPath returns the path of the prism database under the test root.
func rootDBPath(root string) string {
	return filepath.Join(root, "state", "prism", "prism.db")
}

// seedTestDBRow writes an agent_status row for sessionName to the test
// database. A destructive verb such as `prism cleanup --session` refuses a
// session that has no row.
func seedTestDBRow(t *testing.T, root, sessionName, repo, worktree string) {
	t.Helper()
	d, err := db.Open(rootDBPath(root))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatus(sessionName, repo, worktree, "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", sessionName, err)
	}
}

// testDBStatus returns the agent_status row for sessionName in the test
// database, or nil when there is no row.
func testDBStatus(t *testing.T, root, sessionName string) *db.Status {
	t.Helper()
	d, err := db.Open(rootDBPath(root))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	defer d.Close()
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", sessionName, err)
	}
	return st
}

// requireTestDBRow fails t unless the test database holds a row for
// sessionName. The row shows that the binary wrote to the test state dir.
func requireTestDBRow(t *testing.T, root, sessionName string) {
	t.Helper()
	if testDBStatus(t, root, sessionName) == nil {
		t.Errorf("the test database has no agent_status row for %q: the binary did not use the test state dir", sessionName)
	}
}

// requireTestDBRowEnded fails t unless the row for sessionName in the test
// database is ended within 10 seconds. It shows that a destructive verb
// acted on the test state dir. The cleanup binary writes ended_at after it
// kills the tmux session, so the test waits for the write.
func requireTestDBRowEnded(t *testing.T, root, sessionName string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st := testDBStatus(t, root, sessionName); st != nil && st.EndedAt != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("the %q row of the test database is not ended: the cleanup did not act on the test state dir", sessionName)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopProcessesOf sends SIGKILL to every process whose command line starts
// with bin, and fails t if one of them is still alive after 5 seconds.
func stopProcessesOf(t *testing.T, bin string) {
	t.Helper()
	pids, err := processesOf(bin)
	if err != nil {
		t.Errorf("list the processes of %s: %v", bin, err)
		return
	}
	if len(pids) == 0 {
		return
	}
	t.Logf("stopping %d process(es) of the test-built prism binary: %v", len(pids), pids)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		left, err := processesOf(bin)
		if err == nil && len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("processes of the test-built prism binary %s are alive after SIGKILL: %v (list error: %v)", bin, left, err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processesOf returns the PID of each process whose command line is bin, or
// starts with bin and a space. It uses pgrep, not ps: /bin/ps is setuid on
// macOS, and a sandboxed process cannot run it.
func processesOf(bin string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", "^"+regexp.QuoteMeta(bin)+"( |$)").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil // no process matches
		}
		return nil, err
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// TestStopProcessesOf_StopsDetachedProcess shows that the cleanup stops a
// process of the built binary that detached with setsid and left no PID file,
// as a sidecar does when a shell startup file moves its state dir.
func TestStopProcessesOf_StopsDetachedProcess(t *testing.T) {
	if _, err := processesOf("/nonexistent"); err != nil {
		t.Skipf("pgrep cannot list processes here (%v)", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// A link to this test binary stands in for the built prism binary.
	// PRISM_CMD_TEST_STUB makes TestMain sleep instead of running the suite.
	bin := filepath.Join(stateguardtest.ShortTempDir(t, "pbt-"), "prism")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	c := exec.Command(bin, "sidecar", "--session", "prism-test@leak")
	c.Env = append(os.Environ(), "PRISM_CMD_TEST_STUB=1")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = c.Wait() }()
	t.Cleanup(func() { _ = c.Process.Kill() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		pids, err := processesOf(bin)
		if err == nil && len(pids) == 1 && pids[0] == c.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("processesOf(%s) = %v, %v; want [%d]", bin, pids, err, c.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopProcessesOf(t, bin)

	if pids, err := processesOf(bin); err != nil || len(pids) != 0 {
		t.Errorf("after stopProcessesOf, processesOf(%s) = %v, %v; want none", bin, pids, err)
	}
}

// TestIsolatedPrismBinary_RefusesStateOutsideRoot shows that the binary from
// isolatedPrismBinary does not run when its state dir is outside the test
// root, as when a shell startup file sets XDG_STATE_HOME back to the real
// path.
func TestIsolatedPrismBinary_RefusesStateOutsideRoot(t *testing.T) {
	if os.Getenv("NIX_BUILD_TOP") != "" {
		t.Skip("skipping inside the nix build sandbox (NIX_BUILD_TOP is set): the test builds the prism module with go build")
	}
	bin, root := isolatedPrismBinary(t)

	run := func(stateHome string) (string, error) {
		c := exec.Command(bin, "--help")
		c.Env = append(os.Environ(), "XDG_STATE_HOME="+stateHome)
		var out bytes.Buffer
		c.Stdout, c.Stderr = &out, &out
		err := c.Run()
		return out.String(), err
	}

	if out, err := run(filepath.Join(root, "state")); err != nil {
		t.Fatalf("binary with the state dir inside the root: %v\n%s", err, out)
	}

	outside := t.TempDir()
	out, err := run(outside)
	if err == nil {
		t.Fatalf("binary ran with XDG_STATE_HOME=%s outside the root %s; want a refusal\n%s", outside, root, out)
	}
	if want := fmt.Sprintf("%q is outside that root", outside); !strings.Contains(out, want) {
		t.Errorf("refusal output = %q, want it to contain %s", out, want)
	}
}

// TestCmdTestServer_PaneKeepsTestStateDir shows that a command in a pane of
// the test tmux server sees the XDG_STATE_HOME of the test. If the pane
// shell reads the startup files of the user, they can replace it.
func TestCmdTestServer_PaneKeepsTestStateDir(t *testing.T) {
	root := stateguardtest.ShortTempDir(t, "pbt-")
	isolatePrismEnv(t, root)
	s := newCmdTestServer(t)

	outFile := filepath.Join(root, "pane-state-home")
	if err := s.run("new-window", "-t", "bootstrap:", "printf %s \"$XDG_STATE_HOME\" > "+outFile); err != nil {
		t.Fatalf("new-window: %v", err)
	}
	want := filepath.Join(root, "state")
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(outFile); err == nil && len(b) > 0 {
			got = string(b)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != want {
		t.Errorf("pane XDG_STATE_HOME = %q, want %q", got, want)
	}
}
