package container

// bwrap_container_cli_test.go — no bwrap mount makes a podman or docker API
// socket path of the host visible in the sandbox (#3065). The sandbox root
// is an empty tmpfs, so a path exists in the sandbox only below a mount
// destination. userns_bwrap_test.go covers the second property: the sandbox
// cannot create a user namespace.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostContainerSocketPaths returns every path where the host user can have
// a podman or docker API socket.
func hostContainerSocketPaths(runtimeDir string) []string {
	uidRuntimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	return []string{
		filepath.Join(runtimeDir, "podman", "podman.sock"),
		filepath.Join(runtimeDir, "docker.sock"),
		filepath.Join(uidRuntimeDir, "podman", "podman.sock"),
		filepath.Join(uidRuntimeDir, "docker.sock"),
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
		"/run/docker.sock",
		"/var/run/docker.sock",
	}
}

// bwrapOptionValueCount gives the number of values that follow each bwrap
// option that takes values. The scan uses it to step over the values, so
// that it never reads a --setenv value as an option.
var bwrapOptionValueCount = map[string]int{
	"--setenv": 2, "--unsetenv": 1, "--chdir": 1,
	"--bind": 2, "--ro-bind": 2, "--dev-bind": 2,
	"--bind-try": 2, "--ro-bind-try": 2, "--dev-bind-try": 2,
	"--symlink": 2, "--tmpfs": 1, "--dir": 1, "--proc": 1, "--dev": 1,
	"--remount-ro": 1, "--mqueue": 1, "--perms": 1, "--size": 1,
}

// bwrapMountsExposing returns each bind mount in args whose destination is
// target or a directory above target. The scan stops at "--", where the
// sandboxed command starts.
func bwrapMountsExposing(args []string, target string) []string {
	var hits []string
	for i := 0; i < len(args); i++ {
		opt := args[i]
		if opt == "--" {
			break
		}
		n := bwrapOptionValueCount[opt]
		switch opt {
		case "--bind", "--ro-bind", "--dev-bind", "--bind-try", "--ro-bind-try", "--dev-bind-try":
			if i+2 < len(args) {
				dst := filepath.Clean(args[i+2])
				if dst == "/" || dst == target || strings.HasPrefix(target, dst+"/") {
					hits = append(hits, opt+" "+args[i+1]+" "+args[i+2])
				}
			}
		}
		i += n
	}
	return hits
}

// TestBwrapMountsExposing_FindsABindAboveTheSocket shows that the scan is
// not a no-op: it finds a bind of the socket, of a directory above it, and
// of /, and it ignores a --setenv value and a bind beside the socket.
func TestBwrapMountsExposing_FindsABindAboveTheSocket(t *testing.T) {
	const sock = "/run/user/1000/podman/podman.sock"
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"--bind", sock, sock}, 1},
		{[]string{"--ro-bind", "/run/user/1000", "/run/user/1000"}, 1},
		{[]string{"--bind-try", "/run", "/run"}, 1},
		{[]string{"--dev-bind", "/", "/"}, 1},
		{[]string{"--ro-bind", "/elsewhere", "/run/user/1000/"}, 1},
		{[]string{"--ro-bind", "/run/user/1000/podman2", "/run/user/1000/podman2"}, 0},
		{[]string{"--ro-bind", "/run/current-system", "/run/current-system"}, 0},
		{[]string{"--setenv", "--bind", "/run", "--chdir", "/run"}, 0},
		{[]string{"--", "--bind", "/run", "/run"}, 0},
	}
	for _, c := range cases {
		if got := bwrapMountsExposing(c.args, sock); len(got) != c.want {
			t.Errorf("bwrapMountsExposing(%q) = %q, want %d hit(s)", c.args, got, c.want)
		}
	}
}

// TestBwrapBuildArgs_NoHostContainerSocketVisible: with a production-shaped
// pi session, no bind mount exposes a podman or docker API socket path of
// the host, and no CONTAINER_HOST or DOCKER_HOST points at one.
func TestBwrapBuildArgs_NoHostContainerSocketVisible(t *testing.T) {
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	m, fakeHome, cleanup := bwrapPIFixture(t)
	defer cleanup()
	runDir := filepath.Join(fakeHome, ".local", "state", "prism", "run", "0123456789ab")
	m.cfg.HostAPISockPath = filepath.Join(runDir, "hostapi.sock")
	m.cfg.HarnessPipeSockPath = filepath.Join(runDir, "pipe.sock")
	m.cfg.AgentRunLogPath = filepath.Join(runDir, "agent-run.log")

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)
	if !hasBind(args, runDir) {
		t.Fatalf("fixture: the per-session run dir %s is not bound, so the scan does not cover the production shape: %v",
			runDir, redactedArgs(args))
	}

	for _, sock := range hostContainerSocketPaths(runtimeDir) {
		if hits := bwrapMountsExposing(args, sock); len(hits) > 0 {
			t.Errorf("a bwrap mount makes %s visible in the sandbox: %q", sock, hits)
		}
	}
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && (args[i+1] == "CONTAINER_HOST" || args[i+1] == "DOCKER_HOST") {
			t.Errorf("--setenv %s %q without ContainersEnabled", args[i+1], args[i+2])
		}
	}
}
