//go:build linux

package container_test

// service_port_bwrap_test.go — a process in the bwrap sandbox of a session
// reaches the port that `prism container start` publishes on 127.0.0.1.
// The test needs a real podman and a usable bwrap:
//
//	PRISM_CONTAINER_INTEGRATION=1 go test ./internal/container -run ServicePort -v

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

const serviceBanner = "prism-service-ok"

// startBannerService starts a service container that writes serviceBanner
// to each TCP connection on container port 8080, and returns the host port.
func startBannerService(t *testing.T) int {
	t.Helper()
	if os.Getenv("PRISM_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set PRISM_CONTAINER_INTEGRATION=1 to run against the real podman")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := prismcontainer.Caller{SessionName: "prism-test@service-port", InstanceID: uuid.New().String(), Worktree: prismcontainertest.RealTempDir(t)}
	res := prismcontainer.Start(context.Background(), prismcontainer.Deps{}, c, prismcontainer.StartRequest{
		Image: "alpine", Mount: prismcontainer.MountNone, Ports: []string{"8080"},
		Command: []string{"sh", "-c", "while true; do echo " + serviceBanner + " | nc -l -p 8080; done"},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := prismcontainer.SweepInstances(ctx, prismcontainer.ExecRunner{}, []string{c.InstanceID}); err != nil {
			t.Errorf("remove the service container: %v", err)
		}
	})
	if res.ExitCode != 0 || len(res.Ports) != 1 {
		t.Fatalf("start = %+v (stderr %s)", res, res.Stderr)
	}
	return res.Ports[0].HostPort
}

// nixBash returns the /nix/store path of bash. The test sandbox binds only
// /nix.
func nixBash(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !strings.HasPrefix(real, "/nix/store/") {
		t.Skipf("bash resolves to %q (%v), not a /nix/store path", real, err)
	}
	return real
}

func usableBwrap(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not found in PATH")
	}
	if out, err := exec.Command(bin, "--ro-bind", "/nix", "/nix", "--proc", "/proc", "--dev", "/dev", "--unshare-pid", nixBash(t), "-c", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap present but not usable in this environment: %v — %s", err, out)
	}
	return bin
}

// probeInBwrap connects to 127.0.0.1:port from bash in a bwrap sandbox with
// the given flags, and returns the output.
func probeInBwrap(t *testing.T, flags []string, port int) (string, error) {
	t.Helper()
	bwrapBin, bash := usableBwrap(t), nixBash(t)
	// Builtins only: the sandbox has no PATH. "read -t 0.2 < /dev/zero" is
	// a sleep.
	script := fmt.Sprintf("for ((i = 0; i < 50; i++)); do if read -r line < /dev/tcp/127.0.0.1/%d; then echo \"$line\"; exit 0; fi; read -t 0.2 < /dev/zero; done; exit 1", port)
	args := append(append([]string{}, flags...), "--ro-bind", "/nix", "/nix", bash, "-c", script)
	out, err := exec.Command(bwrapBin, args...).CombinedOutput()
	return string(out), err
}

// TestServicePortBwrap_ReachableFromSandbox runs the production namespace
// flags of the session sandbox under a real bwrap.
func TestServicePortBwrap_ReachableFromSandbox(t *testing.T) {
	port := startBannerService(t)
	out, err := probeInBwrap(t, container.BwrapBaselineArgsForTest(), port)
	if err != nil || !strings.Contains(out, serviceBanner) {
		t.Fatalf("the sandbox did not reach 127.0.0.1:%d: %v\nOutput: %s", port, err, out)
	}
}

// TestServicePortBwrap_NotReachableWithOwnNetwork is the paired negative:
// with --unshare-net the same probe fails at the connect, so the positive
// test reads the banner through the host network.
func TestServicePortBwrap_NotReachableWithOwnNetwork(t *testing.T) {
	port := startBannerService(t)
	flags := append(container.BwrapBaselineArgsForTest(), "--unshare-net")
	out, err := probeInBwrap(t, flags, port)
	if err == nil || strings.Contains(out, serviceBanner) {
		t.Fatalf("the probe reached the service with --unshare-net, so it proves nothing: %v\nOutput: %s", err, out)
	}
	if want := fmt.Sprintf("/dev/tcp/127.0.0.1/%d", port); !strings.Contains(out, want) {
		t.Fatalf("the probe failed for a reason other than the connect to %s: %v\nOutput: %s", want, err, out)
	}
}
